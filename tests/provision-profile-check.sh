#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
mkdir -p "$tmp/runtime/scripts/lib" "$tmp/runtime/config/profiles/synthetic" "$tmp/bin" "$tmp/home" "$tmp/config-home" "$tmp/storage" "$tmp/host"

cp "$ROOT/scripts/provision-profile.sh" "$tmp/runtime/scripts/"
cp "$ROOT/scripts/lib/engine-context.sh" "$tmp/runtime/scripts/lib/"
: > "$tmp/runtime/config/profiles/synthetic/profile.conf"
printf '#!/bin/sh\n# subyard-provision-check-v1\nprintf "hook output\\n"\nprintf "hook error\\n" >&2\nexit "${PROVISION_CHECK_STATUS:?}"\n' > "$tmp/runtime/config/profiles/synthetic/provision.sh"

cat > "$tmp/bin/incus" <<'INCUS'
#!/usr/bin/env bash
while [ "$1" != -- ]; do shift; done
shift
if [ -n "${REMOTE_PID_FILE:-}" ]; then printf '%s\n' "$$" > "$REMOTE_PID_FILE"; fi
exec "$@"
INCUS
chmod +x "$tmp/bin/incus"

engine_env=(
  PATH="$tmp/bin:$PATH"
  SUBYARD_ENGINE_CONTEXT=1
  SUBYARD_ENGINE_CONTEXT_SCHEMA=1
  SUBYARD_OPERATOR_HOME="$tmp/home"
  SUBYARD_CONFIG_DIR="$ROOT/config"
  SUBYARD_CONFIG_HOME="$tmp/config-home"
  SUBYARD_HOME="$ROOT"
  STORAGE_PATH="$tmp/storage"
  HOST_BASE="$tmp/host"
  RESTRICTED_DISK_PATHS=""
  ACCESS_KIND=local
  YARD_KIND=container
  YARD_INSTANCE_NAME=yard-check
  INCUS_PROJECT=subyard-check
  INCUS_BRIDGE=incusbr0
  SSH_HOST=yard-check
  DEV_USER=dev
  DEV_UID=1000
  DEV_SUDO=1
  FORWARD_SSH_AGENT=0
  NESTED_E2E_VMS=0
)

output="$(env "${engine_env[@]}" PROVISION_CHECK_STATUS=0 \
  bash "$tmp/runtime/scripts/provision-profile.sh" --check synthetic)"
[ "$output" = converged ] || { printf 'FAIL: converged output=%q\n' "$output" >&2; exit 1; }

output="$(env "${engine_env[@]}" PROVISION_CHECK_STATUS=10 \
  bash "$tmp/runtime/scripts/provision-profile.sh" --check synthetic)"
[ "$output" = changed ] || { printf 'FAIL: changed output=%q\n' "$output" >&2; exit 1; }

status=0
env "${engine_env[@]}" PROVISION_CHECK_STATUS=7 \
  bash "$tmp/runtime/scripts/provision-profile.sh" --check synthetic >"$tmp/out" 2>"$tmp/err" || status=$?
[ "$status" = 7 ] && [ ! -s "$tmp/out" ] && [ "$(cat "$tmp/err")" = 'hook error' ] \
  || { printf 'FAIL: failed check status/output was not preserved\n' >&2; exit 1; }
for expected in 0 7; do
  status=0
  env "${engine_env[@]}" PROVISION_CHECK_STATUS="$expected" \
    bash "$tmp/runtime/scripts/provision-profile.sh" synthetic >"$tmp/out" 2>"$tmp/err" || status=$?
  [ "$status" = "$expected" ] && [ "$(cat "$tmp/out")" = 'hook output' ] && [ "$(cat "$tmp/err")" = 'hook error' ] \
    || { printf 'FAIL: apply status/output was not preserved\n' >&2; exit 1; }
done

env "${engine_env[@]}" python3 - "$tmp" <<'PY'
import ctypes
import os
import pathlib
import signal
import subprocess
import sys
import time

root = pathlib.Path(sys.argv[1])
hook = root / "runtime/config/profiles/synthetic/provision.sh"
adapter = root / "runtime/scripts/provision-profile.sh"
# Reap only this test's orphaned descendants; the real-container check separately
# verifies the guest's PID 1. This makes local execution independent of host init.
assert ctypes.CDLL(None, use_errno=True).prctl(36, 1, 0, 0, 0) == 0

def identity(pid):
    try:
        fields = pathlib.Path(f"/proc/{pid}/stat").read_text().rsplit(") ", 1)[1].split()
        return fields[19], fields[0]
    except FileNotFoundError:
        return None

def reap():
    while True:
        try:
            if os.waitpid(-1, os.WNOHANG)[0] == 0:
                return
        except ChildProcessError:
            return

def wait_for(predicate):
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.02)
    raise AssertionError("profile execution did not reach the required state")

for wrapper_zombie in (False, True):
    marker = root / ("zombie" if wrapper_zombie else "missing")
    marker.mkdir(mode=0o700)
    hook.write_text('''#!/usr/bin/env bash
# subyard-provision-check-v1
sleep 300 &
child=$!
printf '%s %s %s\\n' "$$" "$child" "${BASH_SOURCE[0]%/*}" > "$CANCEL_MARKER/started"
wait "$child"
''')
    env = dict(os.environ, CANCEL_MARKER=str(marker), REMOTE_PID_FILE=str(marker / "wrapper"))
    runner = subprocess.Popen(["bash", str(adapter), "synthetic"], env=env,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
    owned = []
    try:
        wait_for(lambda: (marker / "started").exists() and len((marker / "started").read_text().split()) == 3)
        parent, child, bundle = (marker / "started").read_text().split()
        wrapper = int((marker / "wrapper").read_text())
        witness = {int(pid): identity(int(pid))[0] for pid in (parent, child)}
        # Pin the actual attached wrapper and all its then-live descendants for
        # failure cleanup, rather than signaling a potentially reused numeric PID.
        def pin(pid):
            before = identity(pid)
            fd = os.pidfd_open(pid)
            after = identity(pid)
            assert before and after and after[0] == before[0]
            owned.append(fd)
            children = pathlib.Path(f"/proc/{pid}/task/{pid}/children").read_text().split()
            for child_pid in children:
                pin(int(child_pid))
        pin(wrapper)
        if wrapper_zombie:
            runner.send_signal(signal.SIGSTOP)
            wait_for(lambda: identity(runner.pid)[1] == "T")
        signal.pidfd_send_signal(owned[0], signal.SIGKILL)
        if not wrapper_zombie:
            assert runner.wait(timeout=5) == 137
        def gone():
            reap()
            return all((current := identity(pid)) is None or current[0] != ticks
                       for pid, ticks in witness.items())
        wait_for(gone)
        assert not pathlib.Path(bundle).exists(), "cancelled bundle was retained"
        if wrapper_zombie:
            runner.send_signal(signal.SIGCONT)
            assert runner.wait(timeout=5) == 137
        stdout, _ = runner.communicate(timeout=5)
        assert stdout == b""
    finally:
        for fd in reversed(owned):
            try:
                signal.pidfd_send_signal(fd, signal.SIGKILL)
            except ProcessLookupError:
                pass
            os.close(fd)
        if runner.poll() is None:
            runner.send_signal(signal.SIGCONT)
            runner.kill()
            runner.wait(timeout=5)
        reap()
print("ok: attached-wrapper loss reaps profile hook and child")
PY

printf 'ok: provision profile check protocol\n'
