#!/usr/bin/env bash
# Check the public remote-owner Android route against the retained E2E yard.
set -euo pipefail

fail() { printf 'android-pool-remote: %s\n' "$*" >&2; exit 1; }
[ "$#" -eq 5 ] || [ "$#" -eq 6 ] || [ "$#" -eq 9 ] \
  || fail 'usage: script ROOT STATE YARD PROJECT INSTANCE [OWNER_ADB [--viewer TOOLS YARD_LEASE]]'
root="$1" state="$2" yard_name="$3" project="$4" instance="$5"
. "$root/tests/helpers/release-candidate.sh"
if YARD_BIN="$(release_candidate_prepare "$root")"; then unset YARD_ENGINE_PATH; else candidate_rc=$?; [ "$candidate_rc" = 1 ] || exit "$candidate_rc"; YARD_BIN="$root/.build/yard"; fi
owner_adb="${6:-}"
viewer_tools="${8:-}" viewer_lease="${9:-}"
if [ "$#" -eq 9 ]; then
  [ "$7" = --viewer ] && [[ "$viewer_tools" = "$state"/android-recovery.* ]] \
    && [ "$(cat "$viewer_tools/.marker" 2>/dev/null)" = subyard-android-pool-recovery-v1 ] \
    && [[ "$viewer_lease" =~ ^/home/dev/\.cache/subyard-android-recovery\.[a-zA-Z0-9]+/first.json$ ]] \
    || fail 'viewer tools or lease are not the retained recovery fixture'
fi
[[ "$root" = /* && "$state" = /* ]] || fail 'root and state must be absolute paths'
[[ "$yard_name" =~ ^[a-zA-Z0-9][a-zA-Z0-9-]*$ \
  && "$project" =~ ^[a-zA-Z0-9][a-zA-Z0-9-]*$ \
  && "$instance" =~ ^[a-zA-Z0-9][a-zA-Z0-9-]*$ ]] || fail 'invalid yard identity'
[ "$(cat "$state/.marker" 2>/dev/null)" = subyard-android-pool-runtime-v1 ] \
  || fail 'state is not the retained Android fixture'
[ "${SUBYARD_E2E_VM:-}" = 1 ] && [ -r /run/subyard-e2e-lease.json ] \
  || fail 'requires the allocated Android E2E VM'
[ -x "$YARD_BIN" ] && [ -x /usr/sbin/sshd ] || fail 'candidate yard or sshd is missing'
if [ -n "$owner_adb" ]; then
  [[ "$owner_adb" = "$state"/android-recovery.*/platform-tools/adb ]] && [ -x "$owner_adb" ] \
    || fail 'owner ADB is not the copied fixture tool'
fi
for command in python3 sg sudo ss ssh ssh-keygen timeout; do
  command -v "$command" >/dev/null || fail "missing $command"
done
sudo -n true || fail 'passwordless sudo is required in the allocated VM'

export SUBYARD_OPERATOR_HOME="$HOME" SUBYARD_CONFIG_HOME="$state/config" SUBYARD_HOME="$state/data"
export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export SUBYARD_NO_AUDIT=1 SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
owner() {
  local command
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    printf -v command '%q ' "$YARD_BIN" -Y "$yard_name" "$@"
    timeout --foreground 45 sg incus-admin -c "exec $command"
  else
    timeout --foreground 45 "$YARD_BIN" -Y "$yard_name" "$@"
  fi
}
[ "$(owner config show INCUS_PROJECT | sed -n 's/^effective: //p')" = "$project" ] \
  || fail 'yard Incus project differs from retained fixture'
[ "$(owner config show YARD_INSTANCE_NAME | sed -n 's/^effective: //p')" = "$instance" ] \
  || fail 'yard instance differs from retained fixture'

work="$(mktemp -d "$state/android-remote.XXXXXX")"
printf '%s\n' subyard-android-remote-v1 > "$work/.marker"
sshd_job=''
cleanup() {
  local result=$? pid='' watchdog=''
  trap - EXIT INT TERM
  set +e
  if [ "$result" -ne 0 ]; then
    for log in remote-catalog.err remote-status.err remote-run.err remote-after.err sshd.log; do
      if [ -f "$work/$log" ]; then
        printf 'android-pool-remote diagnostic: %s\n' "$log" >&2
        sed -n '1,30p' "$work/$log" \
          | sed -E 's/(token|credential|secret|identityfile)[^[:space:]]*/[redacted]/Ig; s/[[:xdigit:]]{64}/[redacted]/g' >&2
      fi
    done
  fi
  if [ -f "$work/sshd.pid" ]; then
    pid="$(cat "$work/sshd.pid")"
    if [[ "$pid" =~ ^[0-9]+$ ]] && [ -f "/proc/$pid/cmdline" ]; then
      if sudo -n python3 -c 'import sys; assert sys.argv[2].encode() in open("/proc/"+sys.argv[1]+"/cmdline","rb").read()' \
        "$pid" "$work/sshd_config"; then
        sudo -n kill -TERM "$pid" || result=3
      else
        fail 'refusing to stop an unrelated sshd process'
      fi
    fi
  fi
  # A failed daemon can exit before writing its PID file; stop its own job too.
  if [ -n "$sshd_job" ]; then
    if [ -z "$pid" ] && kill -0 "$sshd_job" 2>/dev/null; then
      sudo -n kill -TERM "$sshd_job" 2>/dev/null || result=3
    fi
    ( sleep 5; sudo -n kill -KILL "$sshd_job" 2>/dev/null || true ) &
    watchdog=$!
    wait "$sshd_job" 2>/dev/null || true
    kill "$watchdog" 2>/dev/null || true
    wait "$watchdog" 2>/dev/null || true
  fi
  if [ "$(cat "$work/.marker" 2>/dev/null)" = subyard-android-remote-v1 ]; then
    sudo -n find "$work" -depth -delete || result=3
  else
    result=3
  fi
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

install -d -m 0700 "$work/controller-config/yards/remote" "$work/controller-data" "$work/client-bin"
owner_config="$state/config/yards/$yard_name/config.env"
[ -f "$owner_config" ] || fail 'owner registration is missing'
owner_hash="$(sha256sum "$owner_config" | cut -d' ' -f1)"
owner_port="$(owner config show SSH_PORT | sed -n 's/^effective: //p')"
cat > "$work/controller-config/yards/remote/config.env" <<CONFIG
ACCESS_KIND=remote
OWNER_ENDPOINT=android-e2e-owner
OWNER_YARD_NAME=$yard_name
SSH_PORT=$owner_port
CODING_TOOL_INTEGRATIONS=
CONFIG
chmod 0600 "$work/controller-config/yards/remote/config.env"
run_payload=''
if [ -n "$owner_adb" ]; then
  run_payload="$work/payload.py"
  python3 - "$run_payload" "$state/config" "$owner_adb" <<'PY'
import pathlib, sys
path, owner_config, adb = sys.argv[1:]
source = '''import os, subprocess, sys
owner_config, adb = VALUES
if os.environ.get("SUBYARD_CONFIG_HOME") != owner_config:
    sys.exit("remote payload did not execute with owner configuration")
result = subprocess.run([adb, "shell", "getprop", "ro.build.version.sdk"],
                        stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                        stderr=subprocess.PIPE, timeout=120)
if result.returncode != 0 or result.stdout.strip() != b"36":
    sys.exit("remote owner ADB did not report SDK 36")
print("36", flush=True)
sys.exit(23)
'''.replace('VALUES', repr((owner_config, adb)), 1)
pathlib.Path(path).write_text(source)
PY
  chmod 0600 "$run_payload"
fi

# The dedicated key can reach only this temporary daemon. It never touches ~/.ssh.
ssh-keygen -q -t ed25519 -N '' -f "$work/host-key"
ssh-keygen -q -t ed25519 -N '' -f "$work/client-key"
cp "$work/client-key.pub" "$work/authorized_keys"
chmod 0600 "$work/authorized_keys"
# Generic remote commands use SSH shell forwarding, not the owner RPC transport.
# Parse the exact public CLI request without evaluating SSH_ORIGINAL_COMMAND.
python3 - "$work/owner-command" "$root" "$state" "$yard_name" "$HOME" "$STORAGE_PATH" "$run_payload" "$YARD_BIN" <<'PY'
import pathlib, sys
path, root, state, yard, home, storage, payload, yard_bin = sys.argv[1:]
source = '''#!/usr/bin/python3
import os, re, shlex, sys
root, state, yard, home, storage, payload, yard_bin = VALUES
try:
    outer = shlex.split(os.environ.get("SSH_ORIGINAL_COMMAND", ""))
    if outer[:1] == ["--"]:
        outer.pop(0)
    assert len(outer) == 3 and outer[:2] == ["bash", "-lc"]
    inner = shlex.split(outer[2])
    operation = None
    if inner == ["yard", "-Y", yard, "emu", "--session-wire", "view"]:
        arguments = ["--session-wire", "view"]
    else:
        assert len(inner) >= 6 and inner[1:5] == ["yard", "-Y", yard, "emu"]
        arguments = inner[5:]
        assert arguments in (["catalog"], ["status"]) or (payload and arguments ==
            ["run", "--", "/usr/bin/python3", payload])
        assert re.fullmatch(r"SUBYARD_OPERATION_ID=[A-Za-z0-9_-]{1,128}", inner[0])
        operation = inner[0].split("=", 1)[1]
except (AssertionError, ValueError):
    sys.exit("android-pool-remote: rejected SSH command")
env = os.environ.copy()
for key in ("CODING_TOOL_INTEGRATIONS", "AGENTS", "SUBYARD_ENGINE_CONTEXT", "SUBYARD_CONFIG_LOADED", "SUBYARD_OPERATION_ID"):
    env.pop(key, None)
env.update(SUBYARD_OPERATOR_HOME=home, SUBYARD_CONFIG_HOME=state + "/config",
           SUBYARD_HOME=state + "/data", SUBYARD_REPOSITORY_ROOT=root,
           STORAGE_PATH=storage, SUBYARD_NO_AUDIT="1", SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE="1")
if operation is not None:
    env["SUBYARD_OPERATION_ID"] = operation
os.execve(yard_bin, [yard_bin, "-Y", yard, "emu", *arguments], env)
'''.replace('VALUES', repr((root, state, yard, home, storage, payload, yard_bin)), 1)
pathlib.Path(path).write_text(source)
PY
chmod 0700 "$work/owner-command"

port=$((40000 + ($$ % 10000)))
while ss -Hln "sport = :$port" 2>/dev/null | grep -q .; do port=$((port + 1)); done
cat > "$work/sshd_config" <<CONFIG
Port $port
ListenAddress 127.0.0.1
HostKey $work/host-key
PidFile $work/sshd.pid
AuthorizedKeysFile $work/authorized_keys
StrictModes no
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
PermitRootLogin no
AllowUsers $(id -un)
AllowTcpForwarding no
AllowAgentForwarding no
X11Forwarding no
PermitTTY yes
ForceCommand $work/owner-command
LogLevel ERROR
CONFIG
sudo -n /usr/sbin/sshd -t -f "$work/sshd_config"
# shellcheck disable=SC2024
sudo -n /usr/sbin/sshd -D -e -f "$work/sshd_config" > "$work/sshd.log" 2>&1 &
sshd_job=$!
printf '[127.0.0.1]:%s ' "$port" > "$work/known_hosts"
cat "$work/host-key.pub" >> "$work/known_hosts"
cat > "$work/ssh_config" <<CONFIG
Host android-e2e-owner
  HostName 127.0.0.1
  User $(id -un)
  Port $port
  IdentityFile $work/client-key
  IdentitiesOnly yes
  UserKnownHostsFile $work/known_hosts
  StrictHostKeyChecking yes
  ConnectTimeout 5
  ServerAliveInterval 5
  ServerAliveCountMax 2
  LogLevel ERROR
CONFIG
printf '#!/bin/sh\nexec /usr/bin/ssh -F %s "$@"\n' "$work/ssh_config" > "$work/client-bin/ssh"
chmod 0700 "$work/client-bin/ssh"
if [ -n "$viewer_tools" ]; then
  # The Linux controller must not need Incus; owner SSH gets its own login PATH.
  printf '#!/bin/sh\n: > %s\nexit 89\n' "$work/controller-incus-called" > "$work/client-bin/incus"
  chmod 0700 "$work/client-bin/incus"
fi
for _ in $(seq 1 50); do
  kill -0 "$sshd_job" 2>/dev/null || fail 'temporary sshd exited'
  if ss -Hln "sport = :$port" | grep -q .; then break; fi
  sleep 0.1
done

controller() {
  local seconds=60
  if [ "$1" = run ]; then seconds=1320; fi
  PATH="$work/client-bin:$PATH" SUBYARD_CONFIG_HOME="$work/controller-config" \
    SUBYARD_HOME="$work/controller-data" timeout --foreground "$seconds" \
    "$YARD_BIN" -Y remote emu "$@" </dev/null
}
owner emu catalog > "$work/local-catalog.json"
controller catalog > "$work/remote-catalog.json" 2> "$work/remote-catalog.err" \
  || fail 'remote catalog failed'
owner emu status > "$work/local-status.json"
controller status > "$work/remote-status.json" 2> "$work/remote-status.err" \
  || fail 'remote status failed'
python3 - "$work" "$viewer_lease" <<'PY'
import json, pathlib, sys
root = pathlib.Path(sys.argv[1])
def load(name):
    return json.loads((root / name).read_text())
local_catalog, remote_catalog = load('local-catalog.json'), load('remote-catalog.json')
local_status, remote_status = load('local-status.json'), load('remote-status.json')
assert local_catalog == remote_catalog, 'remote catalog differs from owner catalog'
assert {35, 36} <= {image['api'] for image in remote_catalog['images']}, 'Android APIs missing'
fields = ('schema', 'resource_type', 'resource_id', 'defaults', 'graphics_mode',
          'heartbeat_seconds', 'ttl_seconds', 'slots')
assert {key: local_status[key] for key in fields} == {key: remote_status[key] for key in fields}, \
    'remote status differs from owner status'
assert len(remote_status['slots']) == 2
if not sys.argv[2]:
    assert all(slot['state'] == 'available' for slot in remote_status['slots']), \
        'Android slots were not idle during remote route check'
PY
if [ -n "$viewer_tools" ]; then
  PATH="$work/client-bin:$PATH" SUBYARD_CONFIG_HOME="$work/controller-config" \
    SUBYARD_HOME="$work/controller-data" python3 -B \
    "$root/config/profiles/android/tests/e2e/android-pool-remote-viewer.py" \
    "$YARD_BIN" "$work" "$viewer_tools" "$yard_name" "$project" "$instance" "$viewer_lease"
  [ ! -e "$work/controller-incus-called" ] || fail 'remote controller invoked local Incus'
  printf 'android-pool-remote controller-incus=unused\n'
fi
if [ -n "$run_payload" ] && [ -z "$viewer_tools" ]; then
  run_status=0
  controller run -- /usr/bin/python3 "$run_payload" \
    > "$work/remote-run.out" 2> "$work/remote-run.err" || run_status=$?
  [ "$run_status" -eq 23 ] || fail "remote run returned $run_status instead of payload exit 23"
  [ "$(tr -d '\r' < "$work/remote-run.out")" = 36 ] || fail 'remote run changed payload stdout'
  controller status > "$work/remote-after.json" 2> "$work/remote-after.err" \
    || fail 'remote status after run failed'
  python3 - "$work/remote-after.json" <<'PY'
import json, sys
slots = json.load(open(sys.argv[1]))['slots']
assert len(slots) == 2 and all(slot['state'] == 'available' for slot in slots), \
    'remote run did not release its lease'
PY
  printf 'android-pool-remote run=PASS sdk=36 exit=23 slots=available\n'
fi
[ "$(sha256sum "$owner_config" | cut -d' ' -f1)" = "$owner_hash" ] \
  || fail 'remote reads changed owner registration'
printf 'android-pool-remote catalog=PASS status=PASS owner-config=unchanged\n'
