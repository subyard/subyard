#!/usr/bin/env bash
# Root-side probe: a lease's loopback ADB port answers its own user and refuses another account.
set -euo pipefail
[ "$#" -eq 0 ] || { printf 'usage: android-relay-owner.sh\n' >&2; exit 2; }
[ "$(id -u)" = 0 ] || { printf 'android-relay-owner: requires root in the yard\n' >&2; exit 1; }
owner=dev
other=nobody
id "$other" >/dev/null
work="$(mktemp -d /tmp/subyard-android-relay-owner.XXXXXX)"
printf '%s\n' subyard-android-relay-owner-v1 > "$work/.marker"
chown "$owner" "$work"
runner=''
cleanup() {
  local result=$? status=0
  trap - EXIT
  touch "$work/done" 2>/dev/null || true
  if [ -n "$runner" ]; then
    wait "$runner" || status=$?
    [ "$result" -ne 0 ] || result="$status"
  fi
  if [ "$result" -ne 0 ] && [ -f "$work/run.log" ]; then
    printf 'android-relay-owner: run log (last 40 lines)\n' >&2
    tail -n 40 "$work/run.log" >&2 || true
  fi
  if [ "$(cat "$work/.marker")" = subyard-android-relay-owner-v1 ]; then
    rm -rf -- "$work"
  fi
  exit "$result"
}
trap cleanup EXIT
# The leased command publishes its port, then holds the lease until the probe finishes.
# shellcheck disable=SC2016 # The leased shell expands its own variables.
# Start in the owner's work directory with its HOME: root's cwd (/root) is not searchable by the owner.
(cd "$work" && HOME="$(getent passwd "$owner" | cut -d: -f6)" runuser -u "$owner" -- bash -lc 'exec android-broker run --device phone --api 36 --wait 900 \
  --purpose android-relay-owner -- sh -c '\''printf %s "$ANDROID_ADB_SERVER_PORT" > "$1/port.new" &&
  mv "$1/port.new" "$1/port" && while [ ! -e "$1/done" ]; do sleep 1; done'\'' _ "$1"' _ "$work") \
  < /dev/null > "$work/run.log" 2>&1 &
runner=$!
for _ in $(seq 1500); do
  [ ! -s "$work/port" ] || break
  kill -0 "$runner" 2>/dev/null || break
  sleep 1
done
if [ ! -s "$work/port" ]; then
  printf 'android-relay-owner: the leased command exited or timed out before publishing its port\n' >&2
  exit 1
fi
port="$(cat "$work/port")"
[[ "$port" =~ ^[1-9][0-9]*$ ]]
probe() {
  runuser -u "$1" -- python3 - "$port" <<'PY'
import socket, sys
with socket.create_connection(('127.0.0.1', int(sys.argv[1])), timeout=10) as stream:
    stream.settimeout(10)
    stream.sendall(b'000chost:version')
    try:
        reply = stream.recv(4)
    except (ConnectionResetError, socket.timeout):
        reply = b''
print('answered' if reply == b'OKAY' else 'refused')
PY
}
[ "$(probe "$other")" = refused ]
[ "$(probe "$owner")" = answered ]
grep -Fq 'refused a loopback connection owned by another account' "$work/run.log"
printf 'android relay: loopback port refuses another account and answers its owner=PASS\n'
