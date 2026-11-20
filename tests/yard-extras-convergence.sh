#!/usr/bin/env bash
# Removing the final declared extra reconciles stale devices and extras-owned capability keys.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=tests/helpers/test-lib.sh
. "$ROOT/tests/helpers/test-lib.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$TMP"
export HOME="$TMP/home"
export SUBYARD_NO_AUDIT=1
export SUBYARD_EXTRAS_MOUNTS=''
export SUBYARD_EXTRAS_CAPABILITIES=''
export SUBYARD_EXTRAS_DEVICES=''
export PATH="$TMP/bin:$PATH"
export MOCK_DEVICES="$TMP/devices"
export MOCK_CONFIG="$TMP/config-state"
export MOCK_LOG="$TMP/incus.log"
mkdir -p "$HOME" "$TMP/bin"
printf 'yx-last\n' > "$MOCK_DEVICES"
printf '%s\n' \
  'security.idmap.size=1000000' \
  'security.syscalls.intercept.mknod=true' \
  'security.syscalls.intercept.setxattr=true' > "$MOCK_CONFIG"

cat > "$TMP/bin/incus" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-} ${2:-} ${3:-}" in
  'info  ') exit 0 ;;
  'info yard --project') exit 0 ;;
  'config device list') cat "$MOCK_DEVICES" ;;
  'config device remove')
    device="${5:-}"
    sed -i "/^${device}$/d" "$MOCK_DEVICES"
    printf 'remove-device %s\n' "$device" >> "$MOCK_LOG"
    ;;
  'config get yard')
    key="${4:-}"
    sed -n "s/^${key}=//p" "$MOCK_CONFIG"
    ;;
  'config unset yard')
    key="${4:-}"
    sed -i "/^${key}=/d" "$MOCK_CONFIG"
    printf 'unset-config %s\n' "$key" >> "$MOCK_LOG"
    ;;
  *) printf 'unexpected incus call: %s\n' "$*" >&2; exit 90 ;;
esac
MOCK
chmod +x "$TMP/bin/incus"

if "$ROOT/scripts/09-yard-extras.sh" --check; then
  fail "stale final extra was reported as converged"
fi
"$ROOT/scripts/09-yard-extras.sh" --yes >"$TMP/output" 2>&1
[ ! -s "$MOCK_DEVICES" ] || fail "last stale yx-* device was not removed"
[ ! -s "$MOCK_CONFIG" ] || fail "stale extras-owned capability keys were not cleared"
grep -Fq 'remove-device yx-last' "$MOCK_LOG" || fail "device cleanup was not applied"
[ "$(grep -c '^unset-config ' "$MOCK_LOG")" -eq 3 ] || fail "not every extras-owned key was cleared"
"$ROOT/scripts/09-yard-extras.sh" --check || fail "reconciled empty extras state did not converge"

printf 'ok: yard extras reconciler removes the final extra\n'
