#!/usr/bin/env bash
# Profile-owned resource help, usage and stopped-yard contract.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$TMP"
export HOME="$TMP/home" SUBYARD_NO_AUDIT=1 PATH="$TMP/bin:$PATH"
mkdir -p "$HOME" "$TMP/bin"

cat > "$TMP/bin/incus" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
state_root="$(cd "$(dirname "$0")/.." && pwd)"
log="$state_root/incus.log"
printf '%s\n' "$*" >> "$log"
case "${1:-}" in
  info) [ -e "$state_root/up" ] ;;
  list) [ -e "$state_root/up" ] && printf 'RUNNING\n' ;;
esac
MOCK
chmod 755 "$TMP/bin/incus"
export RESOURCE_TEST_LOG="$TMP/incus.log"
touch "$TMP/up"
# shellcheck source=tests/helpers/resource-lifecycle.sh
. "$ROOT/tests/helpers/resource-lifecycle.sh"

handler="$ROOT/config/profiles/hermes/resources/dashboard/handler.sh"
check_resource_help "$handler"
check_resource_stopped "$handler"
printf 'ok: Hermes public help, usage and stopped-yard preconditions\n'
