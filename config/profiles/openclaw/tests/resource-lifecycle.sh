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
export SUBYARD_CONFIG_HOST_DIR="$SUBYARD_CONFIG_HOME/overrides/host"
export SUBYARD_CONFIG_GENERATED_DIR="$SUBYARD_CONFIG_HOME/generated"
mkdir -p "$HOME" "$TMP/bin" "$SUBYARD_CONFIG_HOST_DIR" "$SUBYARD_CONFIG_GENERATED_DIR"
printf '%s' 'synthetic-production-token' | sha256sum | cut -d ' ' -f 1 \
  > "$SUBYARD_CONFIG_HOST_DIR/prod-fingerprints"

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
      *' sh -s -- /srv/staging/_lease bot canonical '*)
        script="$(cat)"
        case "$script" in
          *'echo "OWNED $kind '*) [ -e "$state_root/lease-owned" ] && printf 'OWNED canonical 3600\n' || printf 'MISSING\n' ;;
          *'rm -f "$st"'*) rm -f "$state_root/lease-owned" ;;
        esac
        ;;
      *'.holder //'*) [ -e "$state_root/lease-owned" ] ;;
      *' cat "$1/$2.json" '* ) [ ! -e "$state_root/lease-owned" ] || printf '{"holder":"canonical","kind":"canonical","epoch":1}\n' ;;
      *' docker exec '*' kill -0 '*) [ -e "$state_root/gateway-running" ] ;;
      *' docker exec '*' kill "$pid" '*) rm -f "$state_root/gateway-running" ;;
      *' docker stop subyard-qa-broker '*) touch "$state_root/qa-stopped" ;;
      *' docker inspect -f {{.State.Running}} subyard-qa-broker '*) [ -e "$state_root/qa-stopped" ] && printf 'false\n' || printf 'true\n' ;;
      *' docker inspect -f {{.Id}} '*) printf '%064d\n' 1 ;;
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
touch "$TMP/up" "$TMP/listening" "$TMP/lease-owned" "$TMP/gateway-running"
qa_handler="$ROOT/config/profiles/openclaw/resources/qa-bot-broker/handler.sh"
staging_handler="$ROOT/config/profiles/openclaw/resources/staging-gateway/handler.sh"
# shellcheck source=tests/helpers/resource-lifecycle.sh
. "$ROOT/tests/helpers/resource-lifecycle.sh"

check_resource_help "$qa_handler"
check_resource_help "$staging_handler" list
check_resource_usage "$qa_handler" up --source
check_resource_usage "$qa_handler" logs unexpected
check_resource_usage "$qa_handler" destroy --unknown
check_resource_usage "$staging_handler" start invalid/zone
check_resource_usage "$staging_handler" up --source
check_resource_usage "$staging_handler" logs --purge

check_resource_stopped "$qa_handler"
check_resource_stopped "$staging_handler"
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
jq -e '.action == "status" and .changed == false' "$TMP/qa-status-plan.json" >/dev/null \
  || fail 'QA status prepare did not emit a read-only assessment'
jq -e '.action == "status" and .changed == false' "$TMP/staging-status-plan.json" >/dev/null \
  || fail 'staging status prepare did not emit a read-only assessment'
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
jq -e '.action == "up" and .changed == true' "$TMP/qa-up-plan.json" >/dev/null || fail 'QA up action is unreachable'
jq -e '.action == "seed" and .changed == false' "$TMP/qa-seed-plan.json" >/dev/null || fail 'QA empty seed is not a no-op'
jq -e '.action == "expose" and .changed == true' "$TMP/qa-expose-plan.json" >/dev/null || fail 'QA expose action is unreachable'
jq -e '.action == "logs" and .changed == false' "$TMP/qa-logs-plan.json" >/dev/null || fail 'QA logs action is unreachable'
jq -e '.action == "smoke" and .changed == true' "$TMP/qa-smoke-plan.json" >/dev/null || fail 'QA smoke action is unreachable'
jq -e '.action == "down" and .changed == true' "$TMP/qa-down-plan.json" >/dev/null || fail 'QA down action is unreachable'
jq -e '.action == "destroy" and .changed == true' "$TMP/qa-destroy-plan.json" >/dev/null || fail 'QA destroy action is unreachable'
jq -e '.action == "destroy-purge" and .changed == true' "$TMP/qa-purge-plan.json" >/dev/null || fail 'QA purge action is unreachable'

SUBYARD_RESOURCE_MODE=prepare "$staging_handler" up >"$TMP/staging-up-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$staging_handler" start >"$TMP/staging-start-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$staging_handler" stop >"$TMP/staging-stop-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$staging_handler" logs >"$TMP/staging-logs-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$staging_handler" shell >"$TMP/staging-shell-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$staging_handler" down >"$TMP/staging-down-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$staging_handler" destroy >"$TMP/staging-destroy-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$staging_handler" destroy --purge >"$TMP/staging-purge-plan.json" </dev/null
SUBYARD_RESOURCE_MODE=prepare "$staging_handler" list >"$TMP/staging-list-plan.json" </dev/null
jq -e '.action == "up" and .changed == true' "$TMP/staging-up-plan.json" >/dev/null || fail 'staging up action is unreachable'
jq -e '.action == "start" and .changed == false' "$TMP/staging-start-plan.json" >/dev/null || fail 'running staging start is not a no-op'
jq -e '.action == "stop" and .changed == true' "$TMP/staging-stop-plan.json" >/dev/null || fail 'staging stop action is unreachable'
jq -e '.action == "logs" and .changed == false' "$TMP/staging-logs-plan.json" >/dev/null || fail 'staging logs action is unreachable'
jq -e '.action == "shell" and .changed == false' "$TMP/staging-shell-plan.json" >/dev/null || fail 'staging shell session is unreachable'
jq -e '.action == "down" and .changed == true' "$TMP/staging-down-plan.json" >/dev/null || fail 'staging down action is unreachable'
jq -e '.action == "destroy" and .changed == true' "$TMP/staging-destroy-plan.json" >/dev/null || fail 'staging destroy action is unreachable'
jq -e '.action == "destroy-purge" and .changed == true' "$TMP/staging-purge-plan.json" >/dev/null || fail 'staging purge action is unreachable'
jq -e '.action == "list" and .changed == false' "$TMP/staging-list-plan.json" >/dev/null || fail 'staging list action is unreachable'
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
if grep -Eq 'docker (start|stop|rm)|systemctl (start|restart|disable)|config device (add|remove)' \
  "$RESOURCE_TEST_LOG"; then
  fail 'mismatched resource apply mutated profile state'
fi

# Central refusal (no interactive terminal) leaves the irreversible purge untouched.
: > "$RESOURCE_TEST_LOG"
if "$ROOT/bin/yard" qa-pool destroy --purge \
  </dev/null >"$TMP/qa-purge-decline.out" 2>&1; then
  fail 'non-interactive QA purge bypassed central confirmation'
fi
if grep -Eq 'docker rm|file push|rm -rf /srv/env-secrets/qa-pool|rm -rf /srv/qa-pool' "$RESOURCE_TEST_LOG"; then
  fail 'declined QA purge mutated runtime, credentials or persistent data'
fi

# Owner-dispatched QA reads must not inspect staged credentials or call bearer endpoints.
: > "$RESOURCE_TEST_LOG"
"$ROOT/bin/yard" qa-pool status >"$TMP/qa-status.out"
"$ROOT/bin/yard" qa-pool logs >"$TMP/qa-logs.out"
if grep -Eq '/srv/env-secrets/qa-pool|qa-credentials/v1|authorization: Bearer|file push' \
  "$RESOURCE_TEST_LOG" "$TMP/qa-status.out" "$TMP/qa-logs.out"; then
  fail 'owner-dispatched QA status/logs touched staged credentials or the bearer endpoint'
fi
for handler in "$qa_handler" "$staging_handler"; do
  if grep -Eq 'proceed_or_die|announce_confirm' "$handler"; then
    fail "${handler##*/} retains action-local confirmation"
  fi
done
if grep -Eq '(^|[[:space:]|])e2e([[:space:]|)]|$)' "$staging_handler"; then
  fail 'staging handler retains the undeclared deferred e2e route'
fi

check_resource_probe "$staging_handler"
check_resource_probe "$qa_handler"
[ ! -e "$ROOT/scripts/project-staging.sh" ] && [ ! -e "$ROOT/scripts/qa-pool.sh" ] \
  || fail 'legacy core-owned OpenClaw handlers remain'

# Reverse lifecycle paths must execute through the real generic dispatcher.
"$ROOT/bin/yard" staging stop --yes >/dev/null
"$ROOT/bin/yard" qa-pool down --yes >/dev/null
grep -Fq 'docker exec subyard-staging-canonical' "$RESOURCE_TEST_LOG" \
  || fail 'staging stop did not reach its profile mechanic'
grep -Fq 'docker stop subyard-qa-broker' "$RESOURCE_TEST_LOG" \
  || fail 'qa-pool down did not reach its profile mechanic'
printf 'ok: OpenClaw prepare, reads, refusal and dispatcher reverse lifecycle\n'
