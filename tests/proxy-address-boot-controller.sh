#!/usr/bin/env bash
# Host-free failure and signal cleanup of the physical reboot controller.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d /tmp/subyard-proxy-controller.XXXXXXXX)"
trap 'find "$TMP" -depth -delete' EXIT
mkdir -p "$TMP/dev/e2e"
cp "$ROOT/dev/e2e/proxy-address-boot.sh" "$TMP/dev/e2e/proxy-address-boot.sh"
cat > "$TMP/dev/agent-e2e.sh" <<'CLIENT'
set_requested_slot() { :; }
build_bundle() { printf fixture > "$2"; }
acquire_lease() { LEASE_GENERATION=1; printf 'acquire\n' >> "$FIXTURE_LOG"; }
start_lease_keeper() { :; }
run_guest() {
  case "$*" in
    *' prepare '*)
      printf 'prepare\n' >> "$FIXTURE_LOG"
      case "$FIXTURE_SCENARIO" in
        failure|cleanup-failure) return 23 ;;
        term|int)
          printf '%s\n' "$BASHPID" > "$FIXTURE_PID"
          # The separate parent signals the launched script's PID while this
          # builtin waits; no unowned background process is needed.
          read -r -t 20 fixture_input ;;
        term-external|int-external|term-group|int-group)
          printf '%s\n' "$BASHPID" > "$FIXTURE_PID"
          # A real foreground external process models Bash waiting for SSH.
          python3 -c 'import os,pathlib,time; pathlib.Path(os.environ["FIXTURE_TRANSPORT_PID"]).write_text(str(os.getpid())); time.sleep(float(os.environ["FIXTURE_TRANSPORT_SECONDS"]))' ;;
      esac ;;
    *' clean '*) printf 'clean\n' >> "$FIXTURE_LOG" ;;
    *) return 99 ;;
  esac
}
cleanup_guest() {
  printf 'guest-cleanup\n' >> "$FIXTURE_LOG"
  [ "$FIXTURE_SCENARIO" != cleanup-failure ]
}
cleanup_on_exit() {
  local rc=${2:-$?}
  printf 'release:%s\n' "$rc" >> "$FIXTURE_LOG"
  find "$LOCAL_TEMP" -depth -delete
  exit "$rc"
}
CLIENT
for scenario in failure cleanup-failure; do
  # Cleanup failure cannot replace the native preparation failure.
  expected=23
  rc=0
  FIXTURE_LOG="$TMP/$scenario.log" FIXTURE_SCENARIO="$scenario" SUBYARD_E2E_VM='' \
    bash "$TMP/dev/e2e/proxy-address-boot.sh" --slot 2 \
    > "$TMP/$scenario.output" 2>&1 || rc=$?
  [ "$rc" = "$expected" ] && [ "$(cat "$TMP/$scenario.log")" = "$(printf 'acquire\nprepare\nguest-cleanup\nclean\nrelease:%s' "$expected")" ] || {
    printf 'FAIL: reboot controller %s cleanup (exit %s, expected %s)\n' "$scenario" "$rc" "$expected" >&2
    cat "$TMP/$scenario.output" "$TMP/$scenario.log" >&2
    exit 1
  }
done
python3 - "$TMP" <<'PY'
import os
import pathlib
import signal
import subprocess
import sys
import time

root = pathlib.Path(sys.argv[1])
for scenario, signum, expected in (
    ("term", signal.SIGTERM, 143), ("int", signal.SIGINT, 130),
    ("term-external", signal.SIGTERM, 143), ("int-external", signal.SIGINT, 130),
    ("term-group", signal.SIGTERM, 143), ("int-group", signal.SIGINT, 130),
):
    log = root / (scenario + ".log")
    pid_file = root / (scenario + ".pid")
    transport_pid_file = root / (scenario + ".transport.pid")
    external = scenario.endswith(("-external", "-group"))
    group_signal = scenario.endswith("-group")
    environment = dict(os.environ, FIXTURE_LOG=str(log), FIXTURE_SCENARIO=scenario, FIXTURE_PID=str(pid_file),
                       FIXTURE_TRANSPORT_PID=str(transport_pid_file), FIXTURE_TRANSPORT_SECONDS="20" if group_signal else "1",
                       SUBYARD_E2E_VM="")
    process = subprocess.Popen(
        ["bash", str(root / "dev/e2e/proxy-address-boot.sh"), "--slot", "2"],
        env=environment, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
        start_new_session=True,
    )
    controller_pid = None
    try:
        deadline = time.monotonic() + 5
        while not pid_file.exists() or (external and not transport_pid_file.exists()):
            assert process.poll() is None, "controller exited before prepare"
            assert time.monotonic() < deadline, "controller did not reach prepare"
            time.sleep(0.01)
        controller_pid = int(pid_file.read_text())
        transport_pid = int(transport_pid_file.read_text()) if external else None
        if group_signal:
            os.killpg(process.pid, signum)
        else:
            os.kill(process.pid, signum)
            if external:
                # The controller remains its transport's owner while Bash
                # defers the trap, then reaps it before releasing the lease.
                time.sleep(0.03)
                assert process.poll() is None, "PID-only signal orphaned the transport"
        process.wait(timeout=5)
        assert process.returncode == expected, (scenario, process.returncode)
        output = process.stdout.read().decode()
        assert log.read_text() == f"acquire\nprepare\nguest-cleanup\nclean\nrelease:{expected}\n", (scenario, log.read_text(), output)
        for pid in (controller_pid, transport_pid):
            if pid is None:
                continue
            try:
                os.kill(pid, 0)
            except ProcessLookupError:
                pass
            else:
                raise AssertionError(f"orphaned controller/transport {pid}")
        target = "process-group" if group_signal else "launched-pid"
        timing = "deferred-until-transport-return" if external and not group_signal else "prompt"
        print(f"evidence: scenario={scenario} signal_target={target} exit={expected} cleanup=complete controller_transport=absent trap={timing}")
    finally:
        # Limit failed-test cleanup to this fixture's newly created session.
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.wait()
        process.stdin.close()
        process.stdout.close()
PY
printf 'ok: reboot controller releases after prepare failure, INT, TERM and cleanup failure\n'
