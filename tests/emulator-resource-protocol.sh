#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
python3 "$ROOT/tests/helpers/android-pool-test.py"
tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$tmp"
export HOME="$tmp/home" SUBYARD_NO_AUDIT=1 PATH="$tmp/bin:$PATH"
mkdir -p "$tmp/bin"
cat > "$tmp/bin/incus" <<'MOCK'
#!/usr/bin/env bash
case "$1" in info) exit 0 ;; list) printf 'RUNNING\n' ;; *) exit 91 ;; esac
MOCK
chmod +x "$tmp/bin/incus"
handler="$ROOT/config/profiles/android/resources/emulator/handler.sh"
SUBYARD_RESOURCE_MODE=prepare "$handler" run -- true > "$tmp/plan"
grep -Fq '"action":"run","changed":true' "$tmp/plan"
SUBYARD_RESOURCE_MODE=prepare "$handler" cache prune --dry-run > "$tmp/plan"
grep -Fq '"action":"cache","changed":false' "$tmp/plan"
if SUBYARD_RESOURCE_MODE=prepare "$handler" run > /dev/null 2>&1; then
  printf 'FAIL: run accepted missing command\n' >&2; exit 1
fi
if SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=down SUBYARD_OPERATION_ID=test \
  "$handler" run -- true > /dev/null 2>&1; then
  printf 'FAIL: mismatched prepared action reached pool\n' >&2; exit 1
fi
printf 'ok: Android lease resource prepare/apply protocol\n'
