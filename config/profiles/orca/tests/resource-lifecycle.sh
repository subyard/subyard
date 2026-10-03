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

# shellcheck source=config/profiles/orca/release.env
. "$ROOT/config/profiles/orca/release.env"
export ORCA_TEST_VERSION="$ORCA_VERSION"
cat > "$TMP/bin/incus" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
state_root="$(cd "$(dirname "$0")/.." && pwd)"
log="$state_root/incus.log"
printf '%s\n' "$*" >> "$log"
case "${1:-}" in
  info) [ -e "$state_root/up" ] ;;
  list) [ -e "$state_root/up" ] && printf 'RUNNING\n' ;;
  config)
    case "${2:-} ${3:-}" in
      'device list')
        if [ -e "$state_root/up" ]; then
          printf 'unrelated-device\n'
          # Device lists can arrive in chunks; consumers must not close the pipe early.
          sleep 0.02
          [ -e "$state_root/missing-orca-route" ] || printf 'orca-server\n'
        fi
        ;;
      'device get')
        [ "${5:-}" = orca-server ] || exit 1
        case "${6:-}" in
          listen) printf 'tcp:127.0.0.1:17678\n' ;;
          connect) printf 'tcp:127.0.0.1:6768\n' ;;
          *) exit 1 ;;
        esac
        ;;
    esac ;;
  exec)
    [ -e "$state_root/up" ] || exit 1
    case " $* " in
      *' ss -Hltn '*) [ -e "$state_root/listening" ] ;;
      *' dpkg --print-architecture '*) printf 'amd64\n' ;;
      *' dpkg-query -W '*orca-ide*) printf '%s\n' "$ORCA_TEST_VERSION" ;;
      *' bash -se -- dev /usr/bin/orca-ide /srv/agents/orca ') printf '0 0\n' ;;
      *' bash -se -- '*'orca-registration.sha256'*)
        cat >/dev/null
        printf '{"state":"current","actual":"%064d","desired":"%064d"}\n' 0 0 ;;
    esac ;;
  file) : ;;
esac
MOCK
cat > "$TMP/bin/curl" <<'MOCK'
#!/usr/bin/env bash
exit 0
MOCK
cat > "$TMP/bin/ss" <<'MOCK'
#!/usr/bin/env bash
exit 0
MOCK
chmod 755 "$TMP/bin/incus" "$TMP/bin/curl" "$TMP/bin/ss"
export RESOURCE_TEST_LOG="$TMP/incus.log"
touch "$TMP/up" "$TMP/listening"
orca_handler="$ROOT/config/profiles/orca/resources/orca/handler.sh"
# shellcheck source=tests/helpers/resource-lifecycle.sh
. "$ROOT/tests/helpers/resource-lifecycle.sh"

check_resource_help "$orca_handler"
check_resource_usage "$orca_handler" logs --unknown

check_resource_stopped "$orca_handler"
: > "$RESOURCE_TEST_LOG"
SUBYARD_RESOURCE_MODE=prepare "$orca_handler" status >"$TMP/orca-status-plan.json" </dev/null
grep -Fq '"action":"status","changed":false' "$TMP/orca-status-plan.json" \
  || fail 'Orca status prepare did not emit a read-only assessment'
ORCA_ADVERTISE_HOST=127.0.0.1 ORCA_HOST_PORT=17678 SUBYARD_RESOURCE_MODE=prepare \
  "$orca_handler" up >"$TMP/orca-up-plan.json" </dev/null
touch "$TMP/missing-orca-route"
if ORCA_ADVERTISE_HOST=127.0.0.1 ORCA_HOST_PORT=17678 SUBYARD_RESOURCE_MODE=prepare \
  "$orca_handler" pair >"$TMP/orca-pair-missing-route.out" 2>&1 </dev/null; then
  fail 'Orca pair prepare accepted a missing owner route'
fi
grep -Fq 'endpoint settings are not applied' "$TMP/orca-pair-missing-route.out" \
  || fail 'Orca pair missing-route rejection was not actionable'
rm -f "$TMP/missing-orca-route"
ORCA_ADVERTISE_HOST=127.0.0.1 ORCA_HOST_PORT=17678 SUBYARD_RESOURCE_MODE=prepare \
  "$orca_handler" pair >"$TMP/orca-pair-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare \
  "$orca_handler" restart >"$TMP/orca-restart-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$orca_handler" sync >"$TMP/orca-sync-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$orca_handler" down >"$TMP/orca-down-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$orca_handler" is-up >"$TMP/orca-is-up-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$orca_handler" logs >"$TMP/orca-logs-plan.json" </dev/null
grep -Fq '"action":"up","changed":true' "$TMP/orca-up-plan.json" || fail 'Orca up action is unreachable'
grep -Fq '"action":"pair","changed":true' "$TMP/orca-pair-plan.json" || fail 'Orca pair action is unreachable'
grep -Fq '"action":"restart","changed":true' "$TMP/orca-restart-plan.json" \
  || fail 'Orca restart action is unreachable'
grep -Fq '"action":"sync","changed":true' "$TMP/orca-sync-plan.json" || fail 'explicit Orca sync must run its bounded reconciliation'
grep -Fq '"action":"down","changed":true' "$TMP/orca-down-plan.json" || fail 'Orca down action is unreachable'
grep -Fq '"action":"is-up","changed":false' "$TMP/orca-is-up-plan.json" || fail 'Orca is-up read is unreachable'
grep -Fq '"action":"logs","changed":false' "$TMP/orca-logs-plan.json" || fail 'Orca logs action is unreachable'
if grep -Eq 'file push|docker (run|start|stop|rm|build)|systemctl (enable|start|restart|disable)|config device (add|remove)' \
  "$RESOURCE_TEST_LOG"; then
  fail 'read-only prepare mutated profile resource state'
fi

# Apply rejects a stale/mismatched action before resource mutation.
: > "$RESOURCE_TEST_LOG"
if SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=up SUBYARD_OPERATION_ID=op-mismatch \
  "$orca_handler" down >"$TMP/orca-mismatch.out" 2>&1; then
  fail 'Orca apply accepted a mismatched prepared action'
fi
if grep -Eq 'docker (start|stop|rm)|systemctl (start|restart|disable)|config device (add|remove)' \
  "$RESOURCE_TEST_LOG"; then
  fail 'mismatched resource apply mutated profile state'
fi
if grep -Eq 'proceed_or_die|announce_confirm' "$orca_handler"; then
  fail 'Orca handler retains action-local confirmation'
fi

check_resource_probe "$orca_handler"
"$ROOT/bin/yard" orca down --yes >/dev/null
grep -Fq 'systemctl disable --now subyard-orca.service' "$RESOURCE_TEST_LOG" \
  || fail 'Orca down did not reach its profile-owned service'
printf 'ok: Orca prepare, silent probes and dispatcher reverse lifecycle\n'
