#!/usr/bin/env bash
# Converge the profile-owned pool only after active leases release their SDK read locks.
set -euo pipefail
PROFILE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SDK_ROOT="${ANDROID_SDK_ROOT:-/srv/cache/android-sdk}"
PUBLIC_ROOT="${ANDROID_PUBLIC_ROOT:-$SDK_ROOT/.subyard}"
STATE_ROOT="${ANDROID_STATE_ROOT:-/var/lib/subyard-android}"
DEV_USER="${DEV_USER:-dev}"
INSTALL_ROOT=/usr/local/lib/subyard-android
UNIT=/etc/systemd/system/subyard-android-pool.service
[ "${EMULATOR_SHARING:-shared}" = shared ] || { echo 'Android pool supports EMULATOR_SHARING=shared only' >&2; exit 1; }
if [ "${SUBYARD_ANDROID_SDK_LOCKED:-}" != 1 ]; then
  install -d -o root -g root -m 0700 /run/lock/subyard-android
  exec 9>/run/lock/subyard-android/sdk.lock
  flock -w 30 9 || { echo 'Android SDK is in use; retry after leases finish' >&2; exit 1; }
fi
stage="$(mktemp -d)"
trap 'rm -rf -- "$stage"' EXIT
python3 - "$stage/config.json" "$SDK_ROOT" "$STATE_ROOT" "$PUBLIC_ROOT" "${EMULATOR_POOL_SIZE:-2}" \
  "${ANDROID_API:-36}" "${EMULATOR_DEVICE:-phone}" "${ANDROID_VARIANT:-google_apis}" \
  "${ANDROID_ABI:-x86_64}" "${EMULATOR_GPU:-host}" "$DEV_USER" "${JAVA_HOME:-/opt/jdk-17}" <<'PY'
import json, pathlib, sys
output, sdk, state, public, size, api, device, variant, abi, gpu, user, java = sys.argv[1:]
if not (1 <= int(size) <= 32 and int(api) in (34, 35, 36) and device in ('phone', 'tablet')
        and variant in ('google_apis', 'google_apis_playstore') and abi == 'x86_64' and gpu in ('host', 'software', 'software-gles')):
    raise SystemExit('invalid Android pool configuration')
for path in (sdk, state, public, java):
    if not pathlib.Path(path).is_absolute() or any(c.isspace() for c in path):
        raise SystemExit('Android paths must be absolute without whitespace')
pathlib.Path(output).write_text(json.dumps(dict(sdk_root=sdk, state_root=state, public_root=public,
    size=int(size), api=int(api), device=device, variant=variant, abi=abi, gpu=gpu,
    dev_user=user, runtime_user='subyard-android', java_home=java), separators=(',', ':')) + '\n')
PY
cat > "$stage/unit" <<'EOF'
[Unit]
Description=Subyard Android emulator pool
After=network-online.target

[Service]
Type=simple
User=root
ExecStart=/usr/bin/python3 /usr/local/lib/subyard-android/pool.py serve
Restart=on-failure
KillMode=control-group
TimeoutStopSec=120

[Install]
WantedBy=multi-user.target
EOF
update=0
for source in pool.py client.py runtime.sh; do
  cmp -s "$PROFILE_DIR/$source" "$INSTALL_ROOT/$source" || update=1
done
cmp -s "$stage/config.json" /etc/subyard-android.json || update=1
cmp -s "$stage/unit" "$UNIT" || update=1
if [ "$update" = 1 ] && systemctl is-active --quiet subyard-android-pool.service; then
  # Exclusive SDK lock prevents a concurrent acquire between this check and service replacement.
  python3 - "$STATE_ROOT/pool.json" <<'PY'
import json, pathlib, sys
try:
    state = json.loads(pathlib.Path(sys.argv[1]).read_text())
    assert state['schema'] == 'subyard.android-pool.v1'
    assert all(s['state'] == 'available' for s in state['slots'])
except (OSError, ValueError, KeyError, AssertionError):
    raise SystemExit('Android pool update requires every slot to be available')
PY
  systemctl stop subyard-android-pool.service
fi
for group in kvm render video; do
  getent group "$group" >/dev/null || groupadd --system "$group"
done
for ((slot=1; slot<=${EMULATOR_POOL_SIZE:-2}; slot++)); do
  printf -v runtime_user 'subyard-android-%03d' "$slot"
  if ! getent passwd "$runtime_user" >/dev/null; then
    useradd --system --user-group --home-dir /nonexistent --no-create-home --shell /usr/sbin/nologin "$runtime_user"
  fi
  usermod -a -G kvm,render,video "$runtime_user"
done
install -d -o root -g root -m 0711 "$STATE_ROOT"
install -d -o root -g root -m 0700 "$STATE_ROOT/images"
install -d -o root -g root -m 0755 "$STATE_ROOT/runtimes" "$PUBLIC_ROOT" "$PUBLIC_ROOT/bin" "$INSTALL_ROOT"
for source in pool.py client.py runtime.sh; do
  install -m 0644 -o root -g root "$PROFILE_DIR/$source" "$INSTALL_ROOT/$source"
done
chmod 0755 "$INSTALL_ROOT/runtime.sh"
install -m 0644 "$stage/config.json" /etc/subyard-android.json
install -m 0644 "$stage/unit" "$UNIT"
install -m 0755 /dev/stdin "$PUBLIC_ROOT/bin/yard-emu" <<'EOF'
#!/usr/bin/env bash
exec /usr/bin/python3 /usr/local/lib/subyard-android/client.py "$@"
EOF
install -m 0755 /dev/stdin "$PUBLIC_ROOT/bin/yard" <<'EOF'
#!/usr/bin/env bash
if [ "${1:-}" = emu ]; then
  shift
  exec "$(dirname "$0")/yard-emu" "$@"
fi
while IFS= read -r yard; do
  [ "$yard" -ef "$0" ] || exec "$yard" "$@"
done < <(type -aP yard)
printf 'yard: this environment exposes yard emu; use the owner CLI for other commands\n' >&2
exit 127
EOF
# Retire the old staged entrypoints without touching any user AVD or unknown process.
if [ -d /tmp/subyard-android ] && [ ! -L /tmp/subyard-android ]; then
  install -m 0755 "$PROFILE_DIR/emulator-run.sh" /tmp/subyard-android/emulator-run.sh
  install -m 0755 "$PROFILE_DIR/emulator-control.sh" /tmp/subyard-android/emulator-control.sh
fi
systemctl daemon-reload
systemctl enable subyard-android-pool.service >/dev/null
systemctl start subyard-android-pool.service
for _ in {1..50}; do
  if [ -S "$PUBLIC_ROOT/control.sock" ] && \
      ANDROID_PUBLIC_ROOT="$PUBLIC_ROOT" timeout 2 python3 "$INSTALL_ROOT/client.py" status >/dev/null 2>&1; then
    exit 0
  fi
  sleep 0.1
done
echo 'Android pool did not become ready' >&2
exit 1
