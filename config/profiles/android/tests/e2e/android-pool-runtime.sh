#!/usr/bin/env bash
# Real Android profile acceptance in one marker-owned E2E VM.
set -euo pipefail

lane=full
if [ "$#" -eq 2 ] && [ "$1" = --lane ]; then
  lane="$2"
elif [ "$#" -ne 0 ]; then
  printf 'usage: android-pool-runtime.sh [--lane full|recovery|viewer]\n' >&2
  exit 2
fi
case "$lane" in full|recovery|viewer) ;; *) printf 'invalid Android test lane\n' >&2; exit 2 ;; esac

[ "${SUBYARD_E2E_VM:-}" = 1 ] || { printf 'android-pool-runtime: requires VM1\n' >&2; exit 1; }
[ -r /run/subyard-e2e-lease.json ] || { printf 'android-pool-runtime: missing E2E lease guard\n' >&2; exit 1; }
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd)"
# shellcheck source=tests/helpers/release-candidate.sh
. "$root/tests/helpers/release-candidate.sh"
if YARD_BIN="$(release_candidate_prepare "$root")"; then
  unset YARD_ENGINE_PATH
else
  candidate_rc=$?
  [ "$candidate_rc" = 1 ] || exit "$candidate_rc"
  YARD_BIN="$root/.build/yard"
fi
for command in go incus sudo timeout; do command -v "$command" >/dev/null || exit 1; done
sudo -n true
[ -x "$YARD_BIN" ] || { "$root/dev/build-engine.sh"; YARD_BIN="$root/.build/yard"; }
hold_seconds="${ANDROID_DIAGNOSTIC_HOLD_SECONDS:-0}"
[[ "$hold_seconds" =~ ^[0-9]+$ ]] || { printf 'ANDROID_DIAGNOSTIC_HOLD_SECONDS must be an integer\n' >&2; exit 1; }
hold_seconds=$((10#$hold_seconds))
[ "$hold_seconds" -le 1200 ] || { printf 'ANDROID_DIAGNOSTIC_HOLD_SECONDS must be <= 1200\n' >&2; exit 1; }
state="$(mktemp -d /var/tmp/subyard-android-pool-runtime.XXXXXX)"
printf '%s\n' subyard-android-pool-runtime-v1 > "$state/.marker"
token="$(printf '%s' "${state##*.}" | tr '[:upper:]' '[:lower:]')"; YARD_NAME="android-e2e-$token"
export SUBYARD_OPERATOR_HOME="$HOME" SUBYARD_CONFIG_HOME="$state/config" SUBYARD_HOME="$state/data"
export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage" SUBYARD_NO_AUDIT=1 SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
incus() {
  if [ -S /var/lib/incus/unix.socket ] && [ ! -w /var/lib/incus/unix.socket ]; then
    sudo -n /usr/bin/incus "$@"
  else
    /usr/bin/incus "$@"
  fi
}
yard() {
  # First init can enroll this user, but cannot update its parent shell's groups.
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    local command
    printf -v command '%q ' "$YARD_BIN" -Y "$YARD_NAME" "$@"
    sg incus-admin -c "exec $command"
  else
    "$YARD_BIN" -Y "$YARD_NAME" "$@"
  fi
}
project=''
instance=''
monitor_pid=''
monitor_stop=''
incus_binary=(/usr/bin/incus)
cleanup() {
  status=$?
  trap - EXIT INT TERM
  if [ -n "$monitor_pid" ] && [ -n "$project" ] && [ -n "$instance" ]; then
    timeout 20 "${incus_binary[@]}" --project "$project" exec "$instance" -- \
      touch "$monitor_stop" >/dev/null 2>&1 || true
    for ((attempt = 0; attempt < 40; attempt++)); do
      [ -r "/proc/$monitor_pid/stat" ] || break
      [ "$(awk '{print $3}' "/proc/$monitor_pid/stat")" = Z ] && break
      sleep 0.5
    done
    kill -TERM "$monitor_pid" 2>/dev/null || true
    wait "$monitor_pid" 2>/dev/null || true
  fi
  if [ "$status" -ne 0 ] && [ -n "$project" ] && [ -n "$instance" ]; then
    timeout 30 sudo -n /usr/bin/incus --project "$project" exec "$instance" -- journalctl \
      -u subyard-android-pool.service -u 'subyard-android-slot-*.service' -n 200 --no-pager \
      | sed -E 's/(token|credential|secret)[^ ]*/[redacted]/Ig' >&2 || true
    timeout 15 sudo -n /usr/bin/incus --project "$project" exec "$instance" -- namei -l \
      /srv/cache/android-sdk/platform-tools/adb /srv/cache/android-sdk/emulator/emulator >&2 || true
    if [ "$hold_seconds" -gt 0 ]; then
      hold="$state/diagnostic-ready"
      : > "$hold"
      printf 'android diagnostic retained: state=%s project=%s instance=%s release_marker=%s\n' \
        "$state" "$project" "$instance" "$hold" >&2
      timeout "$hold_seconds" bash -c 'while [ -f "$1" ]; do sleep 2; done' _ "$hold" || true
    fi
  fi
  if yard teardown --yes > "$state/teardown.log" 2>&1; then
    printf 'android cleanup=passed\n'
  else
    sed -n '1,100p' "$state/teardown.log" >&2
    status=3
  fi
  sudo -n find "$state" -depth -delete >/dev/null 2>&1 || true
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM
[ "$(sudo -n cat "$HOME/.cache/subyard-e2e-platform/.subyard-e2e-platform-marker")" = subyard-e2e-platform-v1 ] || exit 1
mkdir -p "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME"
ssh_port=$((30000 + ($$ % 10000)))
while ss -H -ltn "sport = :$ssh_port" 2>/dev/null | grep -q .; do ssh_port=$((ssh_port + 1)); done
cat > "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env" <<EOF
ENVIRONMENT_PROFILES=android
CODING_TOOL_INTEGRATIONS=
YARD_DEVICES="kvm"
FORWARD_SSH_AGENT=0
SSH_PORT=$ssh_port
EOF
chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env"
yard init --yes
project="$(yard config show INCUS_PROJECT | sed -n 's/^effective: //p')"
instance="$(yard config show YARD_INSTANCE_NAME | sed -n 's/^effective: //p')"
yard start --yes
# Verify the required physical namespace boundary before downloading the Android SDK.
incus --project "$project" exec "$instance" -- bash -ceu '
  name="$1"
  test ! -e "/run/netns/$name"
  ip netns add "$name"
  trap '\''ip netns delete "$name"'\'' EXIT
  owner="$(readlink /proc/self/ns/net)"
  child="$(ip netns exec "$name" readlink /proc/self/ns/net)"
  test "$owner" != "$child"
  printf "android namespace-preflight=PASS\n"
' _ "subyard-android-probe-$token"
yard provision --yes
incus --project "$project" exec "$instance" --user 1000 --group 1000 --env HOME=/home/dev -- bash -ceu '
  mkdir -p "$HOME/.android/avd/personal-acceptance.avd"
  printf preserved > "$HOME/.android/avd/personal-acceptance.avd/userdata-marker"
  printf preserved > /srv/cache/gradle/.android-acceptance-marker
'
yard init --yes
printf 'android phase=provision\n' >&2
yard provision --yes
yard start --yes
# Yard agent readiness can precede the profile service's control socket after boot.
incus --project "$project" exec "$instance" -- timeout 30 sh -c '
  until runuser -u dev -- /usr/bin/python3 /usr/local/lib/subyard-android/client.py status >/dev/null 2>&1; do
    sleep 0.2
  done
'
direct_catalog="$state/direct-catalog.log"
wire_catalog="$state/wire-catalog.log"
host_catalog="$state/host-catalog.log"
direct_status=0
incus --project "$project" exec "$instance" --user 1000 --group 1000 -- \
  /usr/bin/python3 /usr/local/lib/subyard-android/client.py catalog >"$direct_catalog" 2>&1 || direct_status=$?
wire_status=0
printf '%s\n' '{"operation":"catalog"}' | incus --project "$project" exec "$instance" -- \
  /usr/bin/python3 /usr/local/lib/subyard-android/client.py _wire >"$wire_catalog" 2>&1 || wire_status=$?
host_status=0
yard emu catalog >"$host_catalog" 2>&1 || host_status=$?
catalog_failed=0
[ "$direct_status" -eq 0 ] || catalog_failed=1
[ "$wire_status" -eq 0 ] || catalog_failed=1
[ "$host_status" -eq 0 ] || catalog_failed=1
grep -Eq 'Android (pool operation failed|[[:lower:]_]+:|pool unavailable)|"ok"[[:space:]]*:[[:space:]]*false' \
  "$direct_catalog" "$wire_catalog" "$host_catalog" && catalog_failed=1 || true
if [ "$catalog_failed" -ne 0 ]; then
  printf 'android direct catalog status=%s\n' "$direct_status" >&2
  sed -n '1,80p' "$direct_catalog" >&2
  printf 'android wire catalog status=%s\n' "$wire_status" >&2
  sed -n '1,80p' "$wire_catalog" >&2
  printf 'android host catalog status=%s\n' "$host_status" >&2
  sed -n '1,80p' "$host_catalog" >&2
  exit 1
fi
printf 'android phase=remote-owner-route\n'
timeout --foreground --kill-after=10 150 bash "$root/config/profiles/android/tests/e2e/android-pool-remote.sh" \
  "$root" "$state" "$YARD_NAME" "$project" "$instance"
printf 'android phase=prepare-images\n'
# Install both SDK packages while the pool is idle, before starting any emulator.
# These public calls exercise the real installer and cache, never a diagnostic seed.
yard emu cache prepare --api 35
yard emu cache prepare --api 36
yard emu cache prepare --api 35
if [ "$lane" = full ]; then
  monitor_stop="/run/subyard-e2e-android-monitor-$token.stop"
  monitor_log="$state/first-boot-monitor.log"
  incus_binary=(/usr/bin/incus)
  if [ -S /var/lib/incus/unix.socket ] && [ ! -w /var/lib/incus/unix.socket ]; then
    incus_binary=(sudo -n /usr/bin/incus)
  fi
  (
    set -o pipefail
    timeout --foreground --kill-after=10 9000 "${incus_binary[@]}" --project "$project" exec "$instance" -- \
      python3 -u - "$monitor_stop" < "$root/config/profiles/android/tests/e2e/android-pool-monitor.py" 2>&1 \
      | tee "$monitor_log"
  ) &
  monitor_pid=$!
  for ((attempt = 0; attempt < 100; attempt++)); do
    grep -Fxq 'android-boot-monitor ready' "$monitor_log" 2>/dev/null && break
    kill -0 "$monitor_pid" 2>/dev/null || break
    sleep 0.1
  done
  grep -Fxq 'android-boot-monitor ready' "$monitor_log" 2>/dev/null \
    || { printf 'android boot monitor did not start\n' >&2; exit 1; }
  adb_log="$state/adb-getprop.log"
  adb_status=0
  timeout --foreground --kill-after=15 1320 "${incus_binary[@]}" --project "$project" exec "$instance" \
    --user 1000 --group 1000 --env HOME=/home/dev -- env \
    PATH=/srv/cache/android-sdk/.subyard/bin:/srv/cache/android-sdk/platform-tools:/opt/jdk-17/bin:/usr/bin:/bin \
    android-broker run --device tablet --api 36 --purpose android-pool-runtime -- \
    sh -c 'adb shell getprop ro.build.version.sdk && exit 23' >"$adb_log" 2>&1 || adb_status=$?
  if [ "$adb_status" -ne 23 ]; then
    printf 'android adb status=%s\n' "$adb_status" >&2
    sed -n '1,120p' "$adb_log" >&2
    exit 1
  fi
  mapfile -t adb_lines < <(tr -d '\r' < "$adb_log")
  [ "${#adb_lines[@]}" -eq 1 ] && [ "${adb_lines[0]}" = 36 ] || {
    printf 'android adb expected SDK 36\n' >&2
    sed -n '1,20p' "$adb_log" >&2
    exit 1
  }
  # The L2 fixture exercises concurrent leases and clean reuse through its normal agent facade.
  bash "$root/config/profiles/android/tests/e2e/android-pool-projects.sh" "$root" "$state" "$YARD_NAME" "$project" "$instance"
  timeout 20 "${incus_binary[@]}" --project "$project" exec "$instance" -- touch "$monitor_stop" \
    || { printf 'android boot monitor stop marker failed\n' >&2; exit 1; }
  monitor_status=0
  wait "$monitor_pid" || monitor_status=$?
  monitor_pid=''
  timeout 20 "${incus_binary[@]}" --project "$project" exec "$instance" -- rm -f -- "$monitor_stop" || true
  [ "$monitor_status" -eq 0 ] || { printf 'android boot monitor failed: %s\n' "$monitor_status" >&2; exit 1; }
fi
printf 'android phase=viewer-dependencies\n'
# Test-only virtual display and checksum-pinned official scrcpy portable release.
# https://github.com/Genymobile/scrcpy/blob/master/doc/linux.md
incus --project "$project" exec "$instance" -- bash -ceu '
  export DEBIAN_FRONTEND=noninteractive
  timeout 180 apt-get update -qq
  timeout 180 apt-get install -y -qq xvfb xauth
  tools=/opt/subyard-e2e-scrcpy
  install -d -m 0755 "$tools"
  curl -fLsS --connect-timeout 15 --max-time 120 \
    https://github.com/Genymobile/scrcpy/releases/download/v4.1/scrcpy-linux-x86_64-v4.1.tar.gz \
    -o "$tools/release.tar.gz"
  printf "%s  %s\n" ad56ae8bfeedf41e824945c11dbf55fcb092b3e615b9b486f48a50e30d389635 \
    "$tools/release.tar.gz" | sha256sum -c -
  tar -xzf "$tools/release.tar.gz" --strip-components=1 -C "$tools"
  rm "$tools/release.tar.gz"
'
viewer_args=()
[ "$lane" != viewer ] || viewer_args=(--viewer-only)
timeout --foreground --kill-after=60 7200 bash "$root/config/profiles/android/tests/e2e/android-pool-recovery.sh" \
  "$root" "$state" "$YARD_NAME" "$project" "$instance" "${viewer_args[@]}"
[ "$lane" != viewer ] || exit 0
printf 'android phase=prune-redownload\n'
yard emu cache prune --dry-run > "$state/prune-dry.json"
yard emu cache prune > "$state/prune-apply.json"
yard emu cache prune > "$state/prune-noop.json"
python3 - "$state" <<'PY'
import json
from pathlib import Path
import sys
root = Path(sys.argv[1])
dry, apply, noop = (json.loads((root / f'prune-{name}.json').read_text()) for name in ('dry', 'apply', 'noop'))
assert len(dry['candidates']) == 2 and not dry['removed']
assert len(apply['removed']) == 2 and not apply['skipped']
assert not noop['removed'] and noop['bytes'] == 0
PY
yard emu cache prepare --api 35
incus --project "$project" exec "$instance" --user 1000 --group 1000 --env HOME=/home/dev -- bash -ceu '
  test "$(cat "$HOME/.android/avd/personal-acceptance.avd/userdata-marker")" = preserved
  test "$(cat /srv/cache/gradle/.android-acceptance-marker)" = preserved
  test -x /srv/cache/android-sdk/platform-tools/adb
  test -x /opt/jdk-17/bin/java
'
printf 'android cache: prune, no-op, redownload and personal data preservation passed\n'
status_log="$state/pool-status.json"
incus --project "$project" exec "$instance" --user 1000 --group 1000 -- \
  /usr/bin/python3 /usr/local/lib/subyard-android/client.py status >"$status_log"
python3 - "$status_log" <<'PY'
import json
import sys
status = json.load(open(sys.argv[1]))
if not status.get('slots') or any(slot.get('state') != 'available' for slot in status['slots']):
    raise SystemExit('android pool slots were not all available after run')
PY
runtime_units="$(incus --project "$project" exec "$instance" -- \
  systemctl list-units --all --no-legend --plain 'subyard-android-slot-*.service')"
[ -z "$runtime_units" ] || { printf 'android runtime units remain after run\n%s\n' "$runtime_units" >&2; exit 1; }
namespaces="$(incus --project "$project" exec "$instance" -- ip netns list)"
if grep -Eq '^subyard-android-slot-[0-9]{3}-[1-9][0-9]*([[:space:]]|$)' <<<"$namespaces"; then
  printf 'android runtime namespaces remain after run\n' >&2
  exit 1
fi
printf '35\n'
printf 'android_pool_runtime=passed\n'
