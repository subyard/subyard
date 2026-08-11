#!/usr/bin/env bash
# Emulator resource prepare is probe-only; authorized view starts runtime/bridge without a prompt.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HANDLER="$ROOT/config/profiles/android/resources/emulator/handler.sh"
TMP="$(mktemp -d)"
cleanup() {
  local status=$?
  if [ "$status" -ne 0 ]; then
    printf 'emulator protocol diagnostics:\n' >&2
    tail -n 80 "$TMP/incus.log" "$TMP/tools.log" 2>/dev/null >&2 || true
  fi
  if [ "${SUBYARD_KEEP_TEST_TMP:-0}" = 1 ]; then
    printf 'kept emulator protocol temp: %s\n' "$TMP" >&2
  else
    rm -rf "$TMP"
  fi
  return "$status"
}
trap cleanup EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$TMP"
export HOME="$TMP/home" SUBYARD_NO_AUDIT=1 PATH="$TMP/bin:$PATH"
mkdir -p "$TMP/bin"

cat >"$TMP/bin/incus" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
state_root="$(cd "$(dirname "$0")/.." && pwd)"
running="$state_root/emulator.running"
log="$state_root/incus.log"
printf '%s\n' "$*" >>"$log"
case "${1:-}" in
  info) exit 0 ;;
  list) printf 'RUNNING\n' ;;
  config) case "${2:-} ${3:-}" in
  'device list')
    for path in "$state_root"/proxy-*; do
      [ ! -e "$path" ] || printf '%s\n' "${path##*/proxy-}"
    done
    ;;
  'device get')
    dev="${5:-}" key="${6:-}"
    [ -e "$state_root/proxy-$dev" ] || exit 1
    case "$dev:$key" in
      adb-emu:listen) printf 'tcp:127.0.0.1:15555\n' ;;
      adb-emu:connect) printf 'tcp:127.0.0.1:5555\n' ;;
      adb-emu:bind) printf 'host\n' ;;
      *) exit 1 ;;
    esac
    ;;
  'device add') touch "$state_root/proxy-${5:?}" ;;
  'device remove') rm -f "$state_root/proxy-${5:?}" ;;
  *) exit 90 ;;
  esac ;;
  file) : ;;
  exec)
    case " $* " in
      *' ss -Hltn '*) [ -e "$running" ] ;;
      *' test -x /tmp/subyard-android/emulator-control.sh '*) exit 0 ;;
      *'emulator-control.sh is-running '*) [ -e "$running" ] ;;
      *'emulator-control.sh start '*) touch "$running"; printf 'started\n' ;;
      *'emulator-control.sh stop '*) rm -f "$running"; printf 'stopped\n' ;;
      *' adb shell getprop sys.boot_completed '*) printf '1\n' ;;
      *' pgrep -u dev -f -- '*) exit 1 ;;
      *) exit 0 ;;
    esac
    ;;
  *) exit 90 ;;
esac
MOCK

cat >"$TMP/bin/adb" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
state_root="$(cd "$(dirname "$0")/.." && pwd)"
printf 'adb %s\n' "$*" >>"$state_root/tools.log"
case " $* " in
  *' get-state '*) printf 'device\n' ;;
esac
MOCK

cat >"$TMP/bin/scrcpy" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
state_root="$(cd "$(dirname "$0")/.." && pwd)"
if [ "${1:-}" = --version ]; then printf 'scrcpy 3.0\n'; exit 0; fi
printf 'scrcpy %s\n' "$*" >>"$state_root/tools.log"
MOCK
chmod 755 "$TMP/bin/incus" "$TMP/bin/adb" "$TMP/bin/scrcpy"

: >"$TMP/incus.log"
SUBYARD_RESOURCE_MODE=prepare "$HANDLER" view >"$TMP/view-plan.json" </dev/null
grep -Fq '"action":"view","changed":true' "$TMP/view-plan.json" \
  || fail 'view prepare did not report required runtime/bridge convergence'
if grep -Eq 'config device (add|remove)|file push|emulator-control\.sh (start|stop)' "$TMP/incus.log"; then
  fail 'view prepare mutated emulator or proxy state'
fi
[ ! -e "$TMP/emulator.running" ] && [ ! -e "$TMP/proxy-adb-emu" ] \
  || fail 'view prepare changed state files'

if SUBYARD_RESOURCE_MODE=prepare "$HANDLER" view --unexpected >"$TMP/invalid-view-plan.out" 2>&1; then
  fail 'view prepare accepted a scrcpy argument without the required separator'
fi
if SUBYARD_RESOURCE_MODE=prepare "$HANDLER" up Pixel_API_35 Another_AVD >"$TMP/invalid-up-plan.out" 2>&1; then
  fail 'up prepare accepted more than one AVD before the argument separator'
fi
[ ! -e "$TMP/emulator.running" ] && [ ! -e "$TMP/proxy-adb-emu" ] \
  || fail 'invalid prepare arguments changed emulator state'

if SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=view SUBYARD_OPERATION_ID=op-invalid-view \
  "$HANDLER" view --unexpected </dev/null >"$TMP/invalid-view-apply.out" 2>&1; then
  fail 'view apply accepted arguments rejected by prepare'
fi
[ ! -e "$TMP/emulator.running" ] && [ ! -e "$TMP/proxy-adb-emu" ] \
  || fail 'invalid apply arguments started the emulator'

if SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=down SUBYARD_OPERATION_ID=op-mismatch \
  "$HANDLER" view </dev/null >"$TMP/mismatch.out" 2>&1; then
  fail 'view apply accepted a mismatched prepared action'
fi
[ ! -e "$TMP/emulator.running" ] || fail 'mismatched apply started the emulator'

SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=view SUBYARD_OPERATION_ID=op-view \
  "$HANDLER" view --no-control -- --max-size=800 </dev/null >"$TMP/view.out"
[ -e "$TMP/emulator.running" ] || fail 'authorized view did not start the emulator runtime'
[ -e "$TMP/proxy-adb-emu" ] || fail 'authorized view did not create the loopback proxy'
grep -Fq 'scrcpy -s 127.0.0.1:15555 --no-control --max-size=800' "$TMP/tools.log" \
  || fail 'authorized view did not preserve validated scrcpy arguments'

SUBYARD_RESOURCE_MODE=prepare "$HANDLER" up >"$TMP/up-plan.json" </dev/null
grep -Fq '"action":"up","changed":false' "$TMP/up-plan.json" \
  || fail 'converged up did not report a no-op'
before="$(wc -l <"$TMP/incus.log")"
SUBYARD_RESOURCE_MODE=prepare "$HANDLER" down >"$TMP/down-plan.json" </dev/null
after="$(wc -l <"$TMP/incus.log")"
grep -Fq '"action":"down","changed":true' "$TMP/down-plan.json" \
  || fail 'down prepare did not report running runtime/bridge impact'
[ "$after" -ge "$before" ] || fail 'invalid probe log accounting'
[ -e "$TMP/emulator.running" ] && [ -e "$TMP/proxy-adb-emu" ] \
  || fail 'down prepare changed state'

if grep -Eq 'proceed_or_die|announce_confirm' "$HANDLER"; then
  fail 'emulator handler retains action-local confirmation'
fi

# Full Go owner dispatch: session intent needs no terminal and starts the missing runtime/bridge.
rm -f "$TMP/emulator.running" "$TMP/proxy-adb-emu" "$TMP/tools.log"
YARD_ENGINE_PATH="$ROOT/.build/yard" "$ROOT/bin/yard" emu view </dev/null >"$TMP/owner-view.out"
[ -e "$TMP/emulator.running" ] && [ -e "$TMP/proxy-adb-emu" ] \
  || fail 'owner-dispatched view did not converge runtime and bridge'
grep -Fq 'scrcpy -s 127.0.0.1:15555' "$TMP/tools.log" \
  || fail 'owner-dispatched view did not open scrcpy'

printf 'ok: emulator resource prepare/apply is typed, probe-only before consent, and view starts immediately\n'
