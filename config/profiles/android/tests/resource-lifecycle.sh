#!/usr/bin/env bash
# Profile-owned resource lifecycle and real dispatcher coverage.
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
  exec)
    [ -e "$state_root/up" ] || exit 1
    case " $* " in
      *' python3 /usr/local/lib/subyard-android/client.py _wire '*)
        IFS= read -r request
        case "$request" in
          *'"operation": "status"'*)
            if [ -e "$state_root/android-lease" ]; then
              printf '%s\n' '{"ok":true,"result":{"slots":[{"slot_id":"fixture-0","state":"held"}]}}'
            else
              printf '%s\n' '{"ok":true,"result":{"slots":[]}}'
            fi
            ;;
          *'"operation": "drain"'*)
            rm -f "$state_root/android-lease"
            printf '%s\n' '{"ok":true,"result":{"stopped":true}}'
            ;;
          *) printf '%s\n' '{"ok":false,"error":"request","message":"unexpected Android fixture request"}' ;;
        esac
        ;;
      *' systemctl is-active --quiet subyard-android-pool.service '*) : ;;
    esac ;;
  file) : ;;
esac
MOCK
chmod 755 "$TMP/bin/incus"
export RESOURCE_TEST_LOG="$TMP/incus.log"
touch "$TMP/up"
# shellcheck source=tests/helpers/resource-lifecycle.sh
. "$ROOT/tests/helpers/resource-lifecycle.sh"

handler="$ROOT/config/profiles/android/resources/emulator/handler.sh"
check_resource_help "$handler"
check_resource_usage "$handler" run --unknown
check_resource_stopped "$handler"
check_resource_probe "$handler"
[ ! -e "$ROOT/scripts/yard-emu.sh" ] || fail 'legacy core-owned Android handler remains'

# Android status is pool-service readiness plus a broker read; it owns no L1 runtime.
: > "$RESOURCE_TEST_LOG"
"$ROOT/bin/yard" emu status >"$TMP/emu-status.out"
grep -Fq '"slots": []' "$TMP/emu-status.out" \
  || fail 'emulator status did not report the empty broker pool'
grep -Fq 'client.py _wire' "$RESOURCE_TEST_LOG" \
  || fail 'emulator status did not reach the owner broker transport'
if grep -Eq 'emulator-control|pgrep -u dev -f --|config device' "$RESOURCE_TEST_LOG"; then
  fail 'emulator status retained an L1 runtime probe'
fi

# Representative reverse lifecycle paths execute through the generic dispatcher and fake Incus.
touch "$TMP/android-lease"
SUBYARD_RESOURCE_MODE=prepare "$ROOT/config/profiles/android/resources/emulator/handler.sh" \
  down >"$TMP/emu-down-plan.json"
grep -Fq 'close selected Android leases and stop their runtimes' "$TMP/emu-down-plan.json" \
  || fail 'emulator down did not assess the held broker lease'
if ! "$ROOT/bin/yard" emu down --yes >"$TMP/emu-down.out" 2>&1; then
  cat "$TMP/emu-down.out" >&2
  tail -n 20 "$RESOURCE_TEST_LOG" >&2
  fail 'broker-owned emulator down failed'
fi
grep -Fq '"stopped": true' "$TMP/emu-down.out" || fail 'emulator down did not receive the broker drain acknowledgement'
[ ! -e "$TMP/android-lease" ] || fail 'emulator down did not drain the fixture lease'
if grep -Eq 'config device|emulator-control|pkill -TERM -u dev -f --' "$RESOURCE_TEST_LOG"; then
  fail 'emulator down retained an L1 runtime mutation'
fi
printf 'ok: Android broker reads and dispatcher lease drain\n'
