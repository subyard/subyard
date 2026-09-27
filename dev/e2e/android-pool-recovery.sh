#!/usr/bin/env bash
# Restart the retained Android yard and pool while fencing private dev leases.
set -euo pipefail

fail() { printf 'android-pool-recovery: %s\n' "$*" >&2; exit 1; }
viewer_only=0
if [ "$#" -eq 6 ] && [ "$6" = --viewer-only ]; then
  viewer_only=1
elif [ "$#" -ne 5 ]; then
  fail 'usage: script ROOT STATE YARD PROJECT INSTANCE [--viewer-only]'
fi
root="$1" state="$2" yard_name="$3" project="$4" instance="$5"
[[ "$root" = /* && "$state" = /* ]] || fail 'root and state must be absolute paths'
for name in "$yard_name" "$project" "$instance"; do
  [[ "$name" =~ ^[a-zA-Z0-9][a-zA-Z0-9-]*$ ]] || fail 'invalid fixture identifier'
done
[ "$(cat "$state/.marker" 2>/dev/null)" = subyard-android-pool-runtime-v1 ] \
  || fail 'state is not the retained Android fixture'
[ "${SUBYARD_E2E_VM:-}" = 1 ] && [ -r /run/subyard-e2e-lease.json ] \
  || fail 'requires the allocated Android E2E VM'
[ -x "$root/.build/yard" ] || fail 'candidate yard binary is missing'
[ -r "$root/dev/e2e/android-pool-viewer.py" ] || fail 'viewer helper is missing'
export SUBYARD_OPERATOR_HOME="$HOME" SUBYARD_CONFIG_HOME="$state/config" SUBYARD_HOME="$state/data"
export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export SUBYARD_NO_AUDIT=1 SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
RECOVERY_TIMEOUT=7200
uptime_seconds() { local up rest; read -r up rest < /proc/uptime; : "$rest"; printf '%s\n' "${up%%.*}"; }
deadline=$(( $(uptime_seconds) + RECOVERY_TIMEOUT ))
remaining() {
  local left=$((deadline - $(uptime_seconds)))
  if [ "${recovery_cleanup:-0}" = 1 ] && [ "$left" -le 0 ]; then
    printf '%s\n' "$1"
    return
  fi
  [ "$left" -gt 0 ] || fail "${RECOVERY_TIMEOUT}-second work deadline expired"
  if [ "$left" -lt "$1" ]; then printf '%s\n' "$left"; else printf '%s\n' "$1"; fi
}
yard() {
  local seconds="$1" command
  shift
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    printf -v command '%q ' "$root/.build/yard" -Y "$yard_name" "$@"
    timeout --foreground "$(remaining "$seconds")" sg incus-admin -c "exec $command"
  else
    timeout --foreground "$(remaining "$seconds")" "$root/.build/yard" -Y "$yard_name" "$@"
  fi
}
incus_exec() {
  local seconds="$1"
  shift
  local -a binary=(/usr/bin/incus)
  if [ -S /var/lib/incus/unix.socket ] && [ ! -w /var/lib/incus/unix.socket ]; then
    binary=(sudo -n /usr/bin/incus)
  fi
  timeout --foreground "$(remaining "$seconds")" "${binary[@]}" \
    --project "$project" exec "$instance" "$@"
}
guest() {
  local seconds="$1"
  shift
  incus_exec "$seconds" --user 1000 --group 1000 --env HOME=/home/dev -- \
    env PATH=/srv/cache/android-sdk/.subyard/bin:/srv/cache/android-sdk/platform-tools:/opt/jdk-17/bin:/usr/bin:/bin \
    "$@"
}
[ "$(yard 30 config show INCUS_PROJECT | sed -n 's/^effective: //p')" = "$project" ] \
  || fail 'yard Incus project differs from fixture'
[ "$(yard 30 config show YARD_INSTANCE_NAME | sed -n 's/^effective: //p')" = "$instance" ] \
  || fail 'yard instance differs from fixture'
work="$(mktemp -d "$state/android-recovery.XXXXXX")"
printf '%s\n' subyard-android-pool-recovery-v1 > "$work/.marker"
lease_dir=''
phase_log=''
cleanup() {
  local result=$?
  trap - EXIT INT TERM
  if [ "$result" -ne 0 ] && [ -s "$phase_log" ]; then
    printf 'android-pool-recovery: failure log (last 20 lines)\n' >&2
    tail -n 20 "$phase_log" | sed -E \
      's/(token|credential|secret|password|authorization|api[_-]?key)[=: ][^[:space:]]+/[redacted]/Ig' >&2
  fi
  if [ -n "$lease_dir" ]; then
    recovery_cleanup=1
    guest 60 bash -c '
      [ "$(cat "$1/.marker" 2>/dev/null)" = subyard-android-pool-recovery-v1 ] || exit 2
      for lease in "$1"/first.json "$1"/second.json; do
        [ ! -f "$lease" ] || yard-emu release --lease-file "$lease" >/dev/null 2>&1 || true
      done
      find "$1" -depth -delete
    ' _ "$lease_dir" </dev/null >/dev/null 2>&1 || {
      printf 'android-pool-recovery: private lease cleanup could not complete\n' >&2
      [ "$result" -ne 0 ] || result=3
    }
  fi
  printf 'android-pool-recovery: evidence=%s\n' "$work" >&2
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

cat > "$work/check.py" <<'PY'
import json
import os
import socket
import stat
import subprocess
import sys
import time

mode = sys.argv[1]
def call(*args, timeout=30, env=None):
    return subprocess.run(args, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                          stderr=subprocess.PIPE, timeout=timeout, env=env)
def lease(path):
    info = os.stat(path)
    assert info.st_uid == os.getuid() and stat.S_IMODE(info.st_mode) == 0o600
    value = json.load(open(path))
    assert value['allocation']['request']['device'] == 'phone'
    assert value['allocation']['request']['api'] == 35
    return value
if mode == 'allocation':
    sys.path.insert(0, '/usr/local/lib/subyard-android')
    import client
    value = client.rpc('allocation', token=lease(sys.argv[2])['token'])
    assert value['state'] == 'held'
    print(json.dumps([value['slot_id'], value['generation'], value['expires_at']]))
elif mode == 'adb':
    value = lease(sys.argv[2])
    if len(sys.argv) > 3:
        prior = lease(sys.argv[3])
        if value['allocation']['slot_id'] == prior['allocation']['slot_id']:
            assert value['allocation']['generation'] > prior['allocation']['generation']
    env = dict(os.environ, ANDROID_SERIAL=value['allocation']['android_serial'],
               ADB_SERVER_SOCKET='localfilesystem:' + value['endpoint'])
    result = call('adb', 'shell', 'getprop', 'ro.build.version.sdk', timeout=120, env=env)
    assert result.returncode == 0 and result.stdout.strip() == b'35'
    print('api=35 generation=' + str(value['allocation']['generation']))
elif mode == 'stale':
    value = lease(sys.argv[2])
    result = call('yard-emu', 'renew', '--lease-file', sys.argv[2])
    assert result.returncode == 1 and result.stderr.startswith(b'Android stale:')
    endpoint = value['endpoint']
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        try:
            with socket.socket(socket.AF_UNIX) as stream:
                stream.settimeout(3)
                stream.connect(endpoint)
                command = b'host:transport:emulator-5554'
                stream.sendall(f'{len(command):04x}'.encode() + command)
                assert not stream.recv(4), 'old endpoint returned ADB data'
                break
        except (FileNotFoundError, ConnectionRefusedError, ConnectionResetError, BrokenPipeError):
            break
        time.sleep(0.5)
    else:
        raise AssertionError('old endpoint stayed open')
    result = call('yard-emu', 'status')
    assert result.returncode == 0
    slot = next(s for s in json.loads(result.stdout)['slots']
                if s['slot_id'] == value['allocation']['slot_id'])
    assert slot['state'] == 'available' and slot['generation'] >= value['allocation']['generation']
    print('stale=PASS endpoint=closed generation=' + str(slot['generation']))
elif mode == 'advanced':
    value = lease(sys.argv[2])
    result = call('yard-emu', 'status')
    assert result.returncode == 0
    slot = next(s for s in json.loads(result.stdout)['slots']
                if s['slot_id'] == value['allocation']['slot_id'])
    assert slot['state'] == 'available' and slot['generation'] > value['allocation']['generation']
    print('generation-advanced=PASS')
elif mode == 'idle':
    status = call('yard-emu', 'status')
    catalog = call('yard-emu', 'catalog')
    assert status.returncode == 0 and catalog.returncode == 0
    slots = json.loads(status.stdout)['slots']
    assert slots and all(slot['state'] == 'available' for slot in slots)
    images = json.loads(catalog.stdout)['images']
    revisions = []
    for api in (35, 36):
        matches = [image for image in images if image['api'] == api and
                   image['variant'] == 'google_apis' and image['abi'] == 'x86_64' and image['cached']]
        assert len(matches) == 1 and matches[0]['revision']
        revisions.append(str(matches[0]['revision']))
    print('api35=' + revisions[0] + ' api36=' + revisions[1])
else:
    raise AssertionError('unknown recovery check')
PY
check_guest() { local seconds="$1"; shift; guest "$seconds" python3 - "$@" < "$work/check.py"; }
unit_list() {
  incus_exec 30 -- systemctl list-units --all --no-legend --plain \
    'subyard-android-slot-*.service' </dev/null
}
baseline="$(check_guest 45 idle)" || fail 'cached images or initial idle state missing'
[ -z "$(unit_list)" ] || fail 'runtime unit exists before recovery'
lease_dir="$(guest 30 bash -c '
  umask 077
  install -d -m 0700 /home/dev/.cache
  mktemp -d /home/dev/.cache/subyard-android-recovery.XXXXXX
' </dev/null)"
[[ "$lease_dir" =~ ^/home/dev/\.cache/subyard-android-recovery\.[a-zA-Z0-9]+$ ]] \
  || fail 'invalid private lease directory'
guest 30 bash -c 'printf "%s\n" subyard-android-pool-recovery-v1 > "$1/.marker"' \
  _ "$lease_dir" </dev/null
first="$lease_dir/first.json"
second="$lease_dir/second.json"

# Owner run executes its payload on this VM; reuse the yard's verified SDK tools.
incus_exec 45 -- tar -C /srv/cache/android-sdk -cf - platform-tools \
  | tar -C "$work" -xf -
owner_adb="$work/platform-tools/adb"
[ -x "$owner_adb" ] || fail 'owner ADB is missing'
timeout 15 "$owner_adb" version >/dev/null
# The owner's viewer must cross the runtime network namespace just like remote ADB.
incus_exec 45 -- tar -C /opt -cf - subyard-e2e-scrcpy | tar -C "$work" -xf -
if ! command -v xvfb-run >/dev/null; then
  timeout 120 sudo -n env DEBIAN_FRONTEND=noninteractive apt-get update -qq
  timeout 180 sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq xvfb xauth
fi

printf 'android-pool-recovery phase=pool-service\n'
guest 1320 yard-emu acquire --device phone --api 35 --lease-file "$first" \
  </dev/null >/dev/null
check_guest 150 adb "$first"
printf 'android-pool-recovery phase=owner-viewer\n'
owner_lease="$work/owner-view.json"
(umask 077; guest 30 cat "$first" > "$owner_lease")
before_view="$(check_guest 30 allocation "$first")"
phase_log="$work/owner-viewer.log"
printf -v viewer_command '%q ' "$root/.build/yard" -Y "$yard_name" emu view \
  --lease-file "$owner_lease" -- --time-limit=30 --max-size=640 --no-audio
timeout --foreground "$(remaining 210)" env ADB="$owner_adb" \
  PATH="$work/subyard-e2e-scrcpy:$work/platform-tools:$PATH" SDL_RENDER_DRIVER=software \
  xvfb-run -a -s '-screen 0 1280x800x24 -nolisten tcp' \
  sg incus-admin -c "exec $viewer_command" > "$phase_log" 2>&1
grep -Fq 'Texture:' "$phase_log" || fail 'owner viewer did not render a video frame'
[ "$(check_guest 30 allocation "$first")" = "$before_view" ] \
  || fail 'owner viewer released or renewed the attached lease'
printf 'android-pool-recovery owner-viewer=PASS lease=unchanged\n'
printf 'android-pool-recovery phase=viewer\n'
phase_log="$work/viewer.log"
guest 1440 python3 - "$first" < "$root/dev/e2e/android-pool-viewer.py" \
  > "$phase_log" 2>&1
printf 'android-pool-recovery viewer=PASS server-start-delay=20s\n'
phase_log=''
[ "$viewer_only" = 0 ] || exit 0
incus_exec 120 -- systemctl restart subyard-android-pool.service </dev/null
await_idle() {
  local observed units attempt
  for ((attempt = 0; attempt < 45; attempt++)); do
    if observed="$(check_guest 30 idle 2>/dev/null)" && [ "$observed" = "$baseline" ] \
      && units="$(unit_list 2>/dev/null)" && [ -z "$units" ]; then
      printf 'android-pool-recovery cache=%s slots=available runtime=absent\n' "$observed"
      return 0
    fi
    sleep 2
  done
  fail 'pool did not return to cached idle state'
}
await_idle
check_guest 60 stale "$first"
printf 'android-pool-recovery pool-restart=PASS\n'

printf 'android-pool-recovery phase=yard-stop-start\n'
guest 1320 yard-emu acquire --device phone --api 35 --lease-file "$second" \
  </dev/null >/dev/null
check_guest 150 adb "$second" "$first"
yard 180 stop --yes >/dev/null
yard 300 start --yes >/dev/null
await_idle
check_guest 60 stale "$second"
printf 'android-pool-recovery yard-restart=PASS\n'

phase_log="$work/final-adb.log"
timeout --foreground "$(remaining 1440)" bash "$root/dev/e2e/android-pool-remote.sh" \
  "$root" "$state" "$yard_name" "$project" "$instance" "$owner_adb" \
  > "$phase_log" 2>&1
grep -Fxq 'android-pool-remote run=PASS sdk=35 exit=23 slots=available' "$phase_log" \
  || fail 'final remote owner Android run did not report PASS'
check_guest 45 advanced "$second"
printf 'android-pool-recovery public-remote-run-api=35 result=PASS\n'
printf 'android-pool-recovery=PASS\n'
