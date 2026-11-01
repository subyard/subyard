#!/usr/bin/env bash
# Host-free failure and signal cleanup of the physical reboot controller.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d /tmp/subyard-proxy-controller.XXXXXXXX)"
trap 'find "$TMP" -depth -delete' EXIT
mkdir -p "$TMP/dev/e2e"
cp "$ROOT/dev/e2e/proxy-address-boot.sh" "$TMP/dev/e2e/proxy-address-boot.sh"
cp "$ROOT/dev/e2e/publication-capture.py" "$TMP/dev/e2e/publication-capture.py"
python3 - "$TMP/dev/e2e/publication-capture.py" "$TMP/capture.frame" <<'FRAME'
import importlib.util,pathlib,sys
spec=importlib.util.spec_from_file_location("capture",sys.argv[1])
capture=importlib.util.module_from_spec(spec);spec.loader.exec_module(capture)
records={name:({"status":"absent"},None) for name in capture.LABELS}
marker=b"subyard-p0-power-systemd-1\n"
records["state-0"]=({"status":"present","size":len(marker),"sha256":capture.digest(marker)},marker)
body,complete=capture.make_archive(records)
with pathlib.Path(sys.argv[2]).open("wb") as stream:
    capture.emit(body,complete,stream)
pathlib.Path(sys.argv[2]).chmod(0o600)
FRAME
cat > "$TMP/dev/agent-e2e.sh" <<'CLIENT'
set_requested_slot() { :; }
build_bundle() { tar -czf "$2" --files-from /dev/null; }
acquire_lease() { LEASE_GENERATION=1; printf 'acquire\n' >> "$FIXTURE_LOG"; }
start_lease_keeper() { :; }
run_guest() {
  [ "${1:-}" = --raw-stdout ] || return 98
  shift
  case "$*" in
    *' prepare '*)
      printf 'prepare\n' >> "$FIXTURE_LOG"
      case "$FIXTURE_SCENARIO" in
        failure|cleanup-failure) cat "$FIXTURE_FRAME"; return 23 ;;
        term|int)
          printf '%s\n' "$BASHPID" > "$FIXTURE_PID.tmp"
          mv -- "$FIXTURE_PID.tmp" "$FIXTURE_PID"
          # The separate parent signals the launched script's PID while this
          # builtin waits; no unowned background process is needed.
          read -r -t 20 fixture_input ;;
        term-external|int-external|term-group|int-group)
          printf '%s\n' "$BASHPID" > "$FIXTURE_PID.tmp"
          mv -- "$FIXTURE_PID.tmp" "$FIXTURE_PID"
          # A real foreground external process models Bash waiting for SSH.
          python3 -c 'import os,pathlib,time; pid=pathlib.Path(os.environ["FIXTURE_TRANSPORT_PID"]); scratch=pid.with_name(pid.name+".tmp"); scratch.write_text(str(os.getpid())+"\n"); scratch.replace(pid); time.sleep(float(os.environ["FIXTURE_TRANSPORT_SECONDS"]))' ;;
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
  FIXTURE_LOG="$TMP/$scenario.log" FIXTURE_SCENARIO="$scenario" FIXTURE_FRAME="$TMP/capture.frame" SUBYARD_E2E_VM='' \
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
        assert log.read_text() == f"acquire\nprepare\nguest-cleanup\nrelease:{expected}\n", (scenario, log.read_text(), output)
        assert "fixture_cleanup=skipped" in output, "interrupted capture permitted unacknowledged fixture cleanup"
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

# Guest cleanup accepts proven absence after ordinary teardown, never unknown or foreign ownership.
python3 - "$ROOT" <<'PROJECT_CLEANUP'
from pathlib import Path
import subprocess,sys
root=Path(sys.argv[1])
def function(path,name):
 text=path.read_text();start=text.index(name+'() {');return text[start:text.index('\n}\n',start)+3]
source=root/'dev/e2e/proxy-address-boot.sh'
script='\n'.join([function(source,'proxy_cleanup'),function(root/'dev/e2e/lib-power-upgrade-contracts.sh','project_presence'),function(root/'dev/e2e/power-reconciler-upgrade.sh','assert_project_owned'),function(root/'dev/e2e/power-reconciler-upgrade.sh','capture_owned_project_images')])+r'''
MODE=clean CLEANUP_ARMED=1 PRESERVE_FIXTURE=0 PROJECT=subyard MARKER=fixture INSTANCE=yard READY=ready WAITING=waiting FOREIGN_PROJECT=foreign FOREIGN_INSTANCE=control ADDRESS_UNIT_PATH=/fixture/absent INTERFACE=absent
assert_state_root(){ :; }
drain_publication_worker(){ :; }
sudo(){ return 1; }
ip(){ return 1; }
die(){ printf 'REFUSED\n'; exit 2; }
cleanup(){ exit "$?"; }
incus(){
 case "$*" in
 'project list --format csv -c n') case "$SCENARIO" in absent) return 0;; unknown) return 2;; *) printf 'subyard\n'; return 0;; esac;;
 'project get subyard user.subyard.p0-power-systemd') [ "$SCENARIO" = owned ] && printf 'fixture\n';;
 'project get subyard features.images') printf 'false\n';;
 *) return 1;;
 esac
}
proxy_cleanup
'''
for scenario,expected in [('absent',0),('unknown',3),('unmarked',2),('owned',0)]:
 p=subprocess.run(['bash','-c',script],env={'PATH':'/usr/bin:/bin','SCENARIO':scenario},capture_output=True)
 print(scenario,p.returncode,'expected',expected)
 if p.returncode!=expected:raise SystemExit(1)
PROJECT_CLEANUP
