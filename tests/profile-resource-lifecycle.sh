#!/usr/bin/env bash
# Profile descriptors own their handlers; dispatch and representative stop/probe paths stay generic.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$TMP"
export HOME="$TMP/home" SUBYARD_NO_AUDIT=1 PATH="$TMP/bin:$PATH"
export SUBYARD_CONFIG_HOST_DIR="$SUBYARD_CONFIG_HOME/overrides/host"
export SUBYARD_CONFIG_GENERATED_DIR="$SUBYARD_CONFIG_HOME/generated"
mkdir -p "$HOME" "$TMP/bin" "$SUBYARD_CONFIG_HOST_DIR" "$SUBYARD_CONFIG_GENERATED_DIR"

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
      'device list') [ -e "$state_root/up" ] && printf 'adb-emu\n' ;;
    esac ;;
  exec)
    [ -e "$state_root/up" ] || exit 1
    case " $* " in
      *' sh -s -- /srv/staging/_lease bot canonical '*)
        script="$(cat)"
        case "$script" in
          *'echo "OWNED $kind '*) printf 'OWNED canonical 3600\n' ;;
        esac
        ;;
      *' ss -Hltn '*) [ -e "$state_root/listening" ] ;;
      *' test -x /tmp/subyard-android/emulator-control.sh '*) [ -e "$state_root/control-available" ] ;;
      *'emulator-control.sh is-running '*) [ -e "$state_root/emulator-proc" ] ;;
      *'emulator-control.sh stop '*) printf 'stopped\n' ;;
      *' pgrep -u dev -f -- '*)
        [ ! -e "$state_root/legacy-stopped" ] && [ -e "$state_root/emulator-proc" ] ;;
      *' pkill -TERM -u dev -f -- '* | *' pkill -KILL -u dev -f -- '*)
        : > "$state_root/legacy-stopped" ;;
      *' dpkg --print-architecture '*) printf 'amd64\n' ;;
      *' dpkg-query -W '*orca-ide*) printf '1.4.159\n' ;;
      *' docker inspect -f '*) printf 'true\n' ;;
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
touch "$TMP/up" "$TMP/listening" "$TMP/emulator-proc" "$TMP/control-available"

qa_handler="$ROOT/config/profiles/openclaw/resources/qa-bot-broker/handler.sh"
staging_handler="$ROOT/config/profiles/openclaw/resources/staging-gateway/handler.sh"
orca_handler="$ROOT/config/profiles/orca/resources/orca/handler.sh"

# Host-side generated credential fixtures fail visibly if a prepare path sources them.
mkdir -p "$SUBYARD_CONFIG_GENERATED_DIR/qa-pool" "$SUBYARD_CONFIG_GENERATED_DIR/staging"
cat > "$SUBYARD_CONFIG_GENERATED_DIR/qa-pool/secrets.env" <<EOF
OPENCLAW_QA_CONVEX_SECRET_MAINTAINER=test-only
OPENCLAW_QA_CONVEX_SECRET_CI=test-only
touch '$TMP/qa-secret-sourced'
EOF
cat > "$SUBYARD_CONFIG_GENERATED_DIR/staging/canonical.env" <<EOF
SUBYARD_STAGING=1
touch '$TMP/staging-secret-sourced'
EOF

# Remaining shipped profile handlers participate in the same typed prepare/apply protocol.
: > "$RESOURCE_TEST_LOG"
SUBYARD_RESOURCE_MODE=prepare "$qa_handler" status >"$TMP/qa-status-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$staging_handler" status >"$TMP/staging-status-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$orca_handler" status >"$TMP/orca-status-plan.json" </dev/null
grep -Fq '"action":"status","changed":false' "$TMP/qa-status-plan.json" \
  || fail 'QA status prepare did not emit a read-only assessment'
grep -Fq '"action":"status","changed":false' "$TMP/staging-status-plan.json" \
  || fail 'staging status prepare did not emit a read-only assessment'
grep -Fq '"action":"status","changed":false' "$TMP/orca-status-plan.json" \
  || fail 'Orca status prepare did not emit a read-only assessment'

# Every descriptor action variant is reachable from a handler prepare without mutation.
BROKER_SRC=/srv/source SUBYARD_RESOURCE_MODE=prepare \
  "$qa_handler" up --source /srv/source >"$TMP/qa-up-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$qa_handler" seed >"$TMP/qa-seed-plan.json" </dev/null
printf '{"kind":"telegram","payload":{},"note":"fixture"}\n' \
  >"$SUBYARD_CONFIG_GENERATED_DIR/qa-pool/pool.jsonl"
SUBYARD_RESOURCE_MODE=prepare "$qa_handler" expose >"$TMP/qa-expose-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$qa_handler" logs >"$TMP/qa-logs-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$qa_handler" smoke >"$TMP/qa-smoke-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$qa_handler" down >"$TMP/qa-down-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$qa_handler" destroy >"$TMP/qa-destroy-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$qa_handler" destroy --purge >"$TMP/qa-purge-plan.json" </dev/null
grep -Fq '"action":"up","changed":true' "$TMP/qa-up-plan.json" || fail 'QA up action is unreachable'
grep -Fq '"action":"seed","changed":false' "$TMP/qa-seed-plan.json" || fail 'QA empty seed is not a no-op'
grep -Fq '"action":"expose","changed":true' "$TMP/qa-expose-plan.json" || fail 'QA expose action is unreachable'
grep -Fq '"action":"logs","changed":false' "$TMP/qa-logs-plan.json" || fail 'QA logs action is unreachable'
grep -Fq '"action":"smoke","changed":true' "$TMP/qa-smoke-plan.json" || fail 'QA smoke action is unreachable'
grep -Fq '"action":"down","changed":true' "$TMP/qa-down-plan.json" || fail 'QA down action is unreachable'
grep -Fq '"action":"destroy","changed":true' "$TMP/qa-destroy-plan.json" || fail 'QA destroy action is unreachable'
grep -Fq '"action":"destroy-purge","changed":true' "$TMP/qa-purge-plan.json" || fail 'QA purge action is unreachable'

SUBYARD_RESOURCE_MODE=prepare "$staging_handler" up >"$TMP/staging-up-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$staging_handler" start >"$TMP/staging-start-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$staging_handler" stop >"$TMP/staging-stop-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$staging_handler" logs >"$TMP/staging-logs-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$staging_handler" shell >"$TMP/staging-shell-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$staging_handler" down >"$TMP/staging-down-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$staging_handler" destroy >"$TMP/staging-destroy-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$staging_handler" destroy --purge >"$TMP/staging-purge-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$staging_handler" list >"$TMP/staging-list-plan.json" </dev/null
grep -Fq '"action":"up","changed":true' "$TMP/staging-up-plan.json" || fail 'staging up action is unreachable'
grep -Fq '"action":"start","changed":false' "$TMP/staging-start-plan.json" || fail 'running staging start is not a no-op'
grep -Fq '"action":"stop","changed":true' "$TMP/staging-stop-plan.json" || fail 'staging stop action is unreachable'
grep -Fq '"action":"logs","changed":false' "$TMP/staging-logs-plan.json" || fail 'staging logs action is unreachable'
grep -Fq '"action":"shell","changed":false' "$TMP/staging-shell-plan.json" || fail 'staging shell session is unreachable'
grep -Fq '"action":"down","changed":true' "$TMP/staging-down-plan.json" || fail 'staging down action is unreachable'
grep -Fq '"action":"destroy","changed":true' "$TMP/staging-destroy-plan.json" || fail 'staging destroy action is unreachable'
grep -Fq '"action":"destroy-purge","changed":true' "$TMP/staging-purge-plan.json" || fail 'staging purge action is unreachable'
grep -Fq '"action":"list","changed":false' "$TMP/staging-list-plan.json" || fail 'staging list action is unreachable'

ORCA_ADVERTISE_HOST=127.0.0.1 ORCA_HOST_PORT=17678 SUBYARD_RESOURCE_MODE=prepare \
  "$orca_handler" up >"$TMP/orca-up-plan.json" </dev/null
ORCA_ADVERTISE_HOST=127.0.0.1 ORCA_HOST_PORT=17678 SUBYARD_RESOURCE_MODE=prepare \
  "$orca_handler" pair >"$TMP/orca-pair-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$orca_handler" sync >"$TMP/orca-sync-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$orca_handler" down >"$TMP/orca-down-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$orca_handler" is-up >"$TMP/orca-is-up-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$orca_handler" logs >"$TMP/orca-logs-plan.json" </dev/null
grep -Fq '"action":"up","changed":true' "$TMP/orca-up-plan.json" || fail 'Orca up action is unreachable'
grep -Fq '"action":"pair","changed":true' "$TMP/orca-pair-plan.json" || fail 'Orca pair action is unreachable'
grep -Fq '"action":"sync","changed":false' "$TMP/orca-sync-plan.json" || fail 'converged Orca sync is not a no-op'
grep -Fq '"action":"down","changed":true' "$TMP/orca-down-plan.json" || fail 'Orca down action is unreachable'
grep -Fq '"action":"is-up","changed":false' "$TMP/orca-is-up-plan.json" || fail 'Orca is-up read is unreachable'
grep -Fq '"action":"logs","changed":false' "$TMP/orca-logs-plan.json" || fail 'Orca logs action is unreachable'

[ ! -e "$TMP/qa-secret-sourced" ] || fail 'QA prepare sourced generated credentials'
[ ! -e "$TMP/staging-secret-sourced" ] || fail 'staging prepare sourced generated credentials'
if grep -Eq 'file push|docker (run|start|stop|rm|build)|systemctl (enable|start|restart|disable)|config device (add|remove)' \
  "$RESOURCE_TEST_LOG"; then
  fail 'read-only prepare mutated profile resource state'
fi

# Apply rejects a stale/mismatched action before resource mutation.
: > "$RESOURCE_TEST_LOG"
if SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=up SUBYARD_OPERATION_ID=op-mismatch \
  "$qa_handler" down >"$TMP/qa-mismatch.out" 2>&1; then
  fail 'QA apply accepted a mismatched prepared action'
fi
if SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=down SUBYARD_OPERATION_ID=op-mismatch \
  "$staging_handler" stop >"$TMP/staging-mismatch.out" 2>&1; then
  fail 'staging apply accepted a mismatched prepared action'
fi
if SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=up SUBYARD_OPERATION_ID=op-mismatch \
  "$orca_handler" down >"$TMP/orca-mismatch.out" 2>&1; then
  fail 'Orca apply accepted a mismatched prepared action'
fi
if grep -Eq 'docker (start|stop|rm)|systemctl (start|restart|disable)|config device (add|remove)' \
  "$RESOURCE_TEST_LOG"; then
  fail 'mismatched resource apply mutated profile state'
fi

# Central refusal (no interactive terminal) leaves the irreversible purge untouched.
: > "$RESOURCE_TEST_LOG"
if YARD_ENGINE_PATH="$ROOT/.build/yard" "$ROOT/bin/yard" qa-pool destroy --purge \
  </dev/null >"$TMP/qa-purge-decline.out" 2>&1; then
  fail 'non-interactive QA purge bypassed central confirmation'
fi
if grep -Eq 'docker rm|/srv/env-secrets/qa-pool|rm -rf /srv/qa-pool' "$RESOURCE_TEST_LOG"; then
  fail 'declined QA purge mutated runtime, credentials or persistent data'
fi

# Owner-dispatched QA reads must not inspect staged credentials or call bearer endpoints.
: > "$RESOURCE_TEST_LOG"
YARD_ENGINE_PATH="$ROOT/.build/yard" "$ROOT/bin/yard" qa-pool status >"$TMP/qa-status.out"
YARD_ENGINE_PATH="$ROOT/.build/yard" "$ROOT/bin/yard" qa-pool logs >"$TMP/qa-logs.out"
if grep -Eq '/srv/env-secrets/qa-pool|qa-credentials/v1|authorization: Bearer|file push' \
  "$RESOURCE_TEST_LOG" "$TMP/qa-status.out" "$TMP/qa-logs.out"; then
  fail 'owner-dispatched QA status/logs touched staged credentials or the bearer endpoint'
fi
for handler in "$qa_handler" "$staging_handler" "$orca_handler"; do
  if grep -Eq 'proceed_or_die|announce_confirm' "$handler"; then
    fail "${handler##*/} retains action-local confirmation"
  fi
done
if grep -Eq '(^|[[:space:]|])e2e([[:space:]|)]|$)' "$staging_handler"; then
  fail 'staging handler retains the undeclared deferred e2e route'
fi

probe_handlers=(
  "$ROOT/config/profiles/android/resources/emulator/handler.sh"
  "$staging_handler"
  "$qa_handler"
  "$orca_handler"
)
for handler in "${probe_handlers[@]}"; do
  output="$("$handler" is-up)"
  [ -z "$output" ] || fail "${handler##*/} is-up probe was not silent"
done

[ ! -e "$ROOT/scripts/yard-emu.sh" ] && [ ! -e "$ROOT/scripts/project-staging.sh" ] \
  && [ ! -e "$ROOT/scripts/qa-pool.sh" ] || fail 'legacy core-owned profile handlers remain'

rm -f "$TMP/up"
for handler in "${probe_handlers[@]}"; do
  name="$(basename "$(dirname "$handler")")"
  if "$handler" is-up >"$TMP/$name.out" 2>&1; then
    fail "$name probe accepted a down resource"
  fi
  [ ! -s "$TMP/$name.out" ] || fail "$name down probe emitted output"
done
touch "$TMP/up"

# Controller-owned resources use their state probe and never scan the shared process table.
rm -f "$TMP/listening"
"$ROOT/bin/yard" emu status >"$TMP/emu-status.out"
grep -Fq 'still booting' "$TMP/emu-status.out" \
  || fail 'emulator status did not use its process probe while adb was down'
grep -Fq 'emulator-control.sh is-running' "$RESOURCE_TEST_LOG" \
  || fail 'emulator status did not use controller-owned state'
if grep -Fq 'pgrep -u dev -f --' "$RESOURCE_TEST_LOG"; then
  fail 'controller-owned status scanned the shared process table'
fi

# Representative reverse lifecycle paths execute through the generic dispatcher and fake Incus.
if ! "$ROOT/bin/yard" emu down --yes >"$TMP/emu-down.out" 2>&1; then
  tail -n 20 "$RESOURCE_TEST_LOG" >&2
  fail 'controller-owned emulator down failed'
fi
"$ROOT/bin/yard" staging stop --yes >/dev/null
"$ROOT/bin/yard" qa-pool down --yes >/dev/null
"$ROOT/bin/yard" orca down --yes >/dev/null
grep -Fq 'config device remove' "$RESOURCE_TEST_LOG" || fail 'emulator down did not remove its bridge'
grep -Fq 'emulator-control.sh stop' "$RESOURCE_TEST_LOG" \
  || fail 'emulator stop did not target its owned process group'
if grep -Fq 'pkill -TERM -u dev -f --' "$RESOURCE_TEST_LOG"; then
  fail 'controller-owned stop used the legacy process-table fallback'
fi
grep -Fq 'docker exec subyard-staging-canonical' "$RESOURCE_TEST_LOG" \
  || fail 'staging stop did not reach its profile mechanic'
grep -Fq 'docker stop subyard-qa-broker' "$RESOURCE_TEST_LOG" \
  || fail 'qa-pool down did not reach its profile mechanic'
grep -Fq 'systemctl disable --now subyard-orca.service' "$RESOURCE_TEST_LOG" \
  || fail 'Orca down did not reach its profile-owned service'

# Before the controller's first launch, an already-running pre-upgrade emulator remains manageable.
: > "$RESOURCE_TEST_LOG"
rm -f "$TMP/control-available" "$TMP/legacy-stopped"
"$ROOT/bin/yard" emu down --yes >/dev/null
grep -Fq 'pkill -TERM -u dev -f -- ^(' "$RESOURCE_TEST_LOG" \
  || fail 'legacy emulator stop did not use the strict migration identity'

printf 'ok: profile-owned resources dispatch and reverse lifecycle paths remain generic\n'
