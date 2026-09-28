#!/usr/bin/env bash
# Extras probe detects missing, stale and property-drifted YARD_* state.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$TMP"
export HOME="$TMP/home"
export SUBYARD_NO_AUDIT=1
export SUBYARD_EXTRAS_MOUNTS='cache:/srv/cache:rw:0755'
export SUBYARD_EXTRAS_CAPABILITIES=''
export SUBYARD_EXTRAS_DEVICES=''
export PATH="$TMP/bin:$PATH"
mkdir -p "$HOME" "$TMP/bin"

export MOCK_DEVICES=yx-cache
export MOCK_SOURCE="$HOST_BASE/cache"
export MOCK_PATH=/srv/cache
export MOCK_READONLY=false
export MOCK_SHIFT=true
export MOCK_IDMAP=''
export MOCK_MKNOD=''
export MOCK_SETXATTR=''
cat > "$TMP/bin/incus" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-} ${2:-} ${3:-}" in
  'info  ' | 'info yard --project') exit 0 ;;
  'config device list') tr ' ' '\n' <<<"$MOCK_DEVICES" ;;
  'config device get')
    case "${6:-}" in
      type) printf 'disk\n' ;;
      source) printf '%s\n' "$MOCK_SOURCE" ;;
      path) printf '%s\n' "$MOCK_PATH" ;;
      readonly) printf '%s\n' "$MOCK_READONLY" ;;
      shift) printf '%s\n' "$MOCK_SHIFT" ;;
    esac ;;
  'config get yard')
    case "${4:-}" in
      security.idmap.size) printf '%s\n' "$MOCK_IDMAP" ;;
      security.syscalls.intercept.mknod) printf '%s\n' "$MOCK_MKNOD" ;;
      security.syscalls.intercept.setxattr) printf '%s\n' "$MOCK_SETXATTR" ;;
    esac ;;
  *) printf 'unexpected incus call: %s\n' "$*" >&2; exit 90 ;;
esac
MOCK
chmod +x "$TMP/bin/incus"

extras_check() { "$ROOT/scripts/09-yard-extras.sh" --check >/dev/null 2>&1; }

extras_check || fail "matching extras rejected"
export ALLOWS_HOST_ACCESS=false
! extras_check || fail "host-free role accepted a profile mount"
ALLOWS_HOST_ACCESS=true
MOCK_DEVICES='yx-cache yx-stale'
! extras_check || fail "stale yx-* device accepted"
MOCK_DEVICES=''
! extras_check || fail "missing desired extra accepted"
MOCK_DEVICES=yx-cache
MOCK_PATH=/wrong
! extras_check || fail "drifted extra mount accepted"

# No declarations is still a desired state: stale devices and extras-owned capability keys must
# keep the stage pending until the reconciler removes the final extra.
SUBYARD_EXTRAS_MOUNTS=''
MOCK_DEVICES=yx-last
MOCK_IDMAP=1000000
MOCK_MKNOD=true
MOCK_SETXATTR=true
! extras_check || fail "stale final extra accepted with an empty desired set"
MOCK_DEVICES=''
MOCK_IDMAP=''
MOCK_MKNOD=''
MOCK_SETXATTR=''
extras_check || fail "empty desired/live extras state rejected"

printf 'ok: init extras probe detects desired/live drift\n'
