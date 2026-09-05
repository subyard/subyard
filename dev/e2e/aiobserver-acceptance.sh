#!/usr/bin/env bash
# Real container-yard lifecycle acceptance for the managed AI Observer integration.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
STATE=''
YARD_NAME=''
PROJECT=''
INSTANCE=''
MARKER=''

die() { printf 'aiobserver-acceptance: %s\n' "$*" >&2; exit 2; }
info() { printf '  [ .. ] %s\n' "$*"; }
ok() { printf '  [ ok ] %s\n' "$*"; }

[ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'run on VM1 through dev/agent-e2e.sh'
for command in curl go incus jq ss sudo; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
sudo -n true || die 'passwordless sudo is required on the disposable VM'

incus() {
  if [ -S /var/lib/incus/unix.socket ] && [ ! -w /var/lib/incus/unix.socket ]; then
    sudo -n /usr/bin/incus "$@"
  else
    /usr/bin/incus "$@"
  fi
}
yard() { "$ROOT/.build/yard" -Y "$YARD_NAME" "$@"; }
guest() { incus exec "$INSTANCE" --project "$PROJECT" -- "$@"; }

cleanup() {
  local rc=$? managed=''
  trap - EXIT INT TERM
  set +e
  if [ -n "$PROJECT" ] && [ -n "$INSTANCE" ] \
    && incus project show "$PROJECT" >/dev/null 2>&1 \
    && incus config show "$INSTANCE" --project "$PROJECT" >/dev/null 2>&1; then
    managed="$(incus config get "$INSTANCE" user.subyard.managed --project "$PROJECT" 2>/dev/null)"
  fi
  if [ -n "$YARD_NAME" ] && [ -n "${SUBYARD_CONFIG_HOME:-}" ] \
    && [ -f "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env" ] \
    && grep -Fqx "# $MARKER" "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env"; then
    if [ -z "$managed" ] || [ "$managed" = true ]; then
      install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel"
      printf 'SSH_PORT=64998\n' > "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
      chmod 0600 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
      yard teardown --yes >/dev/null 2>&1 || rc=3
    else
      printf 'aiobserver-acceptance: refusing to teardown unmanaged instance %s/%s\n' \
        "$PROJECT" "$INSTANCE" >&2
      rc=3
    fi
  fi
  if [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-aiobserver.* ]] \
    && [ -f "$STATE/.marker" ] && [ "$(<"$STATE/.marker")" = "$MARKER" ]; then
    sudo -n find "$STATE" -depth -delete || rc=3
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

STATE="$(mktemp -d /var/tmp/subyard-aiobserver.XXXXXX)"
token="$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
MARKER="subyard-aiobserver-e2e-v1-$token"
printf '%s\n' "$MARKER" > "$STATE/.marker"
YARD_NAME="observer-e2e-$token"
PROJECT="subyard-$YARD_NAME"
INSTANCE="yard-$YARD_NAME"

export SUBYARD_OPERATOR_HOME="$HOME"
export SUBYARD_CONFIG_HOME="$STATE/config"
export SUBYARD_HOME="$STATE/data"
export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export SUBYARD_NO_AUDIT=1
export SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1
export MIN_DISK_GIB=1

install -d -m 0700 \
  "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME" \
  "$STATE/host/host-agent-sessions/claude/projects/synthetic" \
  "$STATE/host/host-agent-sessions/codex/sessions/$(date -u +%Y/%m/%d)"

ssh_port=$((32000 + ($$ % 1000)))
observer_port=$((ssh_port + 20000))
for _ in $(seq 1 100); do
  if ! ss -Hln "sport = :$ssh_port" 2>/dev/null | grep -q . \
    && ! ss -Hln "sport = :$observer_port" 2>/dev/null | grep -q .; then
    break
  fi
  ssh_port=$((ssh_port + 1))
  observer_port=$((ssh_port + 20000))
done
if ss -Hln "sport = :$ssh_port" 2>/dev/null | grep -q . \
  || ss -Hln "sport = :$observer_port" 2>/dev/null | grep -q .; then
  die 'could not reserve unused loopback test ports'
fi

cat > "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env" <<EOF
# $MARKER
SSH_PORT=$ssh_port
CODING_TOOL_INTEGRATIONS=claude codex aiobserver
ENVIRONMENT_PROFILES=orca
HOST_BASE=$STATE/host
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
EOF
chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env"

backfill_marker="backfill-$token"
live_marker="live-$token"
resume_marker="resume-$token"
source_resume_marker="source-resume-$token"
now="$(date -u +'%Y-%m-%dT%H:%M:%S.000Z')"
claude_file="$STATE/host/host-agent-sessions/claude/projects/synthetic/session-$token.jsonl"
codex_file="$STATE/host/host-agent-sessions/codex/sessions/$(date -u +%Y/%m/%d)/rollout-$token.jsonl"
printf '%s\n' \
  "{\"type\":\"user\",\"timestamp\":\"$now\",\"sessionId\":\"claude-$token\",\"cwd\":\"/synthetic/aiobserver-e2e\",\"message\":{\"id\":\"claude-message-$token\",\"role\":\"user\",\"type\":\"message\",\"content\":[{\"type\":\"text\",\"text\":\"$backfill_marker\"}]}}" \
  > "$claude_file"
printf '%s\n' \
  "{\"timestamp\":\"$now\",\"type\":\"session_meta\",\"payload\":{\"id\":\"codex-$token\",\"timestamp\":\"$now\",\"cwd\":\"/synthetic/aiobserver-e2e\",\"originator\":\"aiobserver-e2e\",\"cli_version\":\"0.0.0-test\",\"model_provider\":\"openai\",\"model\":\"gpt-5\"}}" \
  > "$codex_file"

info 'building the current candidate and initializing a container yard'
YARD_BUILD_VERSION=0.11.3 "$ROOT/dev/build-engine.sh" --force
yard init --yes
yard start --yes

if [ ! -f "$HOME/.cache/subyard-e2e-platform/.subyard-e2e-platform-marker" ]; then
  incus info >/dev/null
  incus storage show default --project default >/dev/null
  incus network show incusbr0 --project default >/dev/null
  printf '%s\n' subyard-e2e-platform-v1 \
    > "$HOME/.cache/subyard-e2e-platform/.subyard-e2e-platform-marker"
  chmod 0600 "$HOME/.cache/subyard-e2e-platform/.subyard-e2e-platform-marker"
fi
[ "$(<"$HOME/.cache/subyard-e2e-platform/.subyard-e2e-platform-marker")" = \
    subyard-e2e-platform-v1 ] || die 'unexpected shared E2E platform marker'

expected_image='tobilg/ai-observer:0.5.0@sha256:e2d8f8fdf5e0b55b2cbb1f0db84288eca4d8c3727f9ec569a92704c3a2ecc30f'
initial_provision_marker="$(incus config get \
  "$INSTANCE" user.subyard.ai_observer_provision --project "$PROJECT")"
[[ "$initial_provision_marker" =~ ^[0-9a-f]{64}$ ]] \
  || die 'instance provision marker is not a SHA-256 convergence identity'
[ "$(incus config get "$INSTANCE" user.subyard.ai_observer_proxy --project "$PROJECT")" = \
    "v1:$observer_port" ] || die 'instance proxy marker is wrong'
[ "$(incus config device get "$INSTANCE" ai-observer type --project "$PROJECT")" = proxy ] \
  || die 'dashboard device is not a proxy'
[ "$(incus config device get "$INSTANCE" ai-observer bind --project "$PROJECT")" = host ] \
  || die 'dashboard proxy is not owner-bound'
[ "$(incus config device get "$INSTANCE" ai-observer listen --project "$PROJECT")" = \
    "tcp:127.0.0.1:$observer_port" ] || die 'dashboard proxy listener is wrong'
[ "$(incus config device get "$INSTANCE" ai-observer connect --project "$PROJECT")" = \
    'tcp:127.0.0.1:8080' ] || die 'dashboard proxy target is wrong'

[ "$(guest docker inspect -f '{{.Config.Image}}' subyard-ai-observer)" = "$expected_image" ] \
  || die 'observer image is not the pinned artifact'
[ "$(guest docker inspect -f '{{json .Config.Cmd}}' subyard-ai-observer)" = \
    '["watch","all","--backfill"]' ] || die 'observer command is wrong'
binds="$(guest docker inspect -f '{{json .HostConfig.Binds}}' subyard-ai-observer)"
jq -e --arg claude '/mnt/host/agent-sessions/claude/projects:/sessions/claude:ro' \
  --arg codex '/mnt/host/agent-sessions/codex/sessions:/sessions/codex:ro' \
  --arg data '/srv/agents/ai-observer/data:/app/data:rw' \
  'index($claude) != null and index($codex) != null and index($data) != null' \
  <<<"$binds" >/dev/null || die 'observer bind mounts are wrong'
guest systemctl is-enabled --quiet subyard-ai-observer.service \
  || die 'observer service is not enabled'
guest /usr/local/bin/ai-observer-check >/dev/null || die 'observer readiness check failed'
curl -fsS --max-time 10 "http://127.0.0.1:$observer_port/health" >/dev/null \
  || die 'owner dashboard route is unavailable'
ok 'pinned container, service, read-only session mounts, and owner proxy converged'

api_contains() {
  local location="$1" marker="$2" response=''
  for _ in $(seq 1 60); do
    if [ "$location" = guest ]; then
      response="$(guest curl -fsS --max-time 5 -G --data-urlencode "search=$marker" \
        'http://127.0.0.1:8080/api/logs?limit=100' 2>/dev/null || true)"
    else
      response="$(curl -fsS --max-time 5 -G --data-urlencode "search=$marker" \
        "http://127.0.0.1:$observer_port/api/logs?limit=100" 2>/dev/null || true)"
    fi
    if jq -e --arg marker "$marker" '.. | strings | select(contains($marker))' \
      <<<"$response" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  return 1
}

api_contains guest "$backfill_marker" || die 'pre-existing Claude session was not backfilled'
api_contains owner "$backfill_marker" || die 'backfilled Claude session is absent through owner HTTP'
ok 'pre-existing Claude session was backfilled and is queryable through both API routes'

live_time="$(date -u +'%Y-%m-%dT%H:%M:%S.000Z')"
printf '%s\n' \
  "{\"timestamp\":\"$live_time\",\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"$live_marker\"}]}}" \
  >> "$codex_file"
api_contains guest "$live_marker" || die 'appended Codex session record was not ingested live'
api_contains owner "$live_marker" || die 'live Codex record is absent through owner HTTP'
ok 'appended Codex session record was ingested live'

container_id="$(guest docker inspect -f '{{.Id}}' subyard-ai-observer)"
database_inode="$(guest stat -c '%d:%i' /srv/agents/ai-observer/data/ai-observer.duckdb)"
info 'repeating init to prove convergence is idempotent'
yard init --yes
[ "$(guest docker inspect -f '{{.Id}}' subyard-ai-observer)" = "$container_id" ] \
  || die 'repeat init replaced the converged observer container'
[ "$(incus config get "$INSTANCE" user.subyard.ai_observer_provision --project "$PROJECT")" = \
    "$initial_provision_marker" ] || die 'repeat init changed the observer convergence identity'
[ "$(guest stat -c '%d:%i' /srv/agents/ai-observer/data/ai-observer.duckdb)" = \
    "$database_inode" ] || die 'repeat init replaced the observer database'
api_contains owner "$live_marker" || die 'repeat init lost collected data'
ok 'repeat init preserved the container and database'

restarts_before="$(guest systemctl show subyard-ai-observer.service --property=NRestarts --value)"
guest docker kill subyard-ai-observer >/dev/null
recovered=0
for _ in $(seq 1 60); do
  if guest /usr/local/bin/ai-observer-check >/dev/null 2>&1; then
    recovered=1
    break
  fi
  sleep 1
done
[ "$recovered" = 1 ] || die 'systemd did not recover the killed observer container'
restarts_after="$(guest systemctl show subyard-ai-observer.service --property=NRestarts --value)"
[[ "$restarts_before" =~ ^[0-9]+$ && "$restarts_after" =~ ^[0-9]+$ ]] \
  || die 'systemd restart counters are invalid'
[ "$restarts_after" -gt "$restarts_before" ] || die 'service recovery did not increment NRestarts'
api_contains owner "$backfill_marker" || die 'service recovery lost backfilled data'
ok 'systemd recovered a killed observer container with its data intact'

write_integrations() {
  local integrations="$1" temporary="$STATE/config.env.new"
  sed "s/^CODING_TOOL_INTEGRATIONS=.*/CODING_TOOL_INTEGRATIONS=$integrations/" \
    "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env" > "$temporary"
  chmod 0600 "$temporary"
  mv -fT "$temporary" "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env"
}

info 'removing Claude as an ingestion source while keeping AI Observer selected'
container_before_source_change="$(guest docker inspect -f '{{.Id}}' subyard-ai-observer)"
write_integrations 'codex aiobserver'
yard init --yes
container_without_claude="$(guest docker inspect -f '{{.Id}}' subyard-ai-observer)"
[ "$container_without_claude" != "$container_before_source_change" ] \
  || die 'removing Claude did not replace the observer container'
without_claude_marker="$(incus config get \
  "$INSTANCE" user.subyard.ai_observer_provision --project "$PROJECT")"
[[ "$without_claude_marker" =~ ^[0-9a-f]{64}$ ]] \
  || die 'source change produced an invalid observer convergence identity'
[ "$without_claude_marker" != "$initial_provision_marker" ] \
  || die 'removing Claude did not change the observer convergence identity'
[ "$(guest stat -c '%d:%i' /srv/agents/ai-observer/data/ai-observer.duckdb)" = \
    "$database_inode" ] || die 'removing Claude replaced the observer database'
binds="$(guest docker inspect -f '{{json .HostConfig.Binds}}' subyard-ai-observer)"
jq -e --arg claude '/srv/agents/ai-observer/empty/claude:/sessions/claude:ro' \
  --arg codex '/mnt/host/agent-sessions/codex/sessions:/sessions/codex:ro' \
  'index($claude) != null and index($codex) != null' <<<"$binds" >/dev/null \
  || die 'removing Claude did not switch its observer mount to the managed empty tree'
api_contains owner "$backfill_marker" || die 'source removal lost existing observer data'

source_resume_time="$(date -u +'%Y-%m-%dT%H:%M:%S.000Z')"
printf '%s\n' \
  "{\"type\":\"user\",\"timestamp\":\"$source_resume_time\",\"sessionId\":\"claude-$token\",\"cwd\":\"/synthetic/aiobserver-e2e\",\"message\":{\"id\":\"claude-source-message-$token\",\"role\":\"user\",\"type\":\"message\",\"content\":[{\"type\":\"text\",\"text\":\"$source_resume_marker\"}]}}" \
  >> "$claude_file"
info 'restoring Claude as an ingestion source'
write_integrations 'claude codex aiobserver'
yard init --yes
[ "$(guest docker inspect -f '{{.Id}}' subyard-ai-observer)" != "$container_without_claude" ] \
  || die 'restoring Claude did not replace the observer container'
[ "$(incus config get "$INSTANCE" user.subyard.ai_observer_provision --project "$PROJECT")" = \
    "$initial_provision_marker" ] || die 'restoring Claude did not restore its convergence identity'
[ "$(guest stat -c '%d:%i' /srv/agents/ai-observer/data/ai-observer.duckdb)" = \
    "$database_inode" ] || die 'restoring Claude replaced the observer database'
binds="$(guest docker inspect -f '{{json .HostConfig.Binds}}' subyard-ai-observer)"
jq -e --arg claude '/mnt/host/agent-sessions/claude/projects:/sessions/claude:ro' \
  --arg codex '/mnt/host/agent-sessions/codex/sessions:/sessions/codex:ro' \
  'index($claude) != null and index($codex) != null' <<<"$binds" >/dev/null \
  || die 'restoring Claude did not restore its real read-only observer mount'
api_contains owner "$source_resume_marker" \
  || die 'restoring Claude did not ingest the record written while its source was absent'
ok 'selected ingestion source changes converged without losing the database'

info 'deselecting AI Observer while retaining its database'
write_integrations 'claude codex'
yard init --yes
guest systemctl is-active --quiet subyard-ai-observer.service \
  && die 'deselection left the observer service active'
[ "$(guest docker inspect -f '{{.State.Running}}' subyard-ai-observer)" = false ] \
  || die 'deselection left the observer container running'
guest test -f /srv/agents/ai-observer/data/ai-observer.duckdb \
  || die 'deselection removed the observer database'
[ "$(guest stat -c '%d:%i' /srv/agents/ai-observer/data/ai-observer.duckdb)" = \
    "$database_inode" ] || die 'deselection replaced the observer database'
[ -z "$(incus config get "$INSTANCE" user.subyard.ai_observer_provision --project "$PROJECT")" ] \
  || die 'deselection left the observer provision marker'
[ -z "$(incus config get "$INSTANCE" user.subyard.ai_observer_proxy --project "$PROJECT")" ] \
  || die 'deselection left the observer proxy marker'
if incus config device get "$INSTANCE" ai-observer type --project "$PROJECT" >/dev/null 2>&1; then
  die 'deselection left the owner proxy device'
fi
if curl -fsS --max-time 2 "http://127.0.0.1:$observer_port/health" >/dev/null 2>&1; then
  die 'deselection left the owner dashboard route reachable'
fi
ok 'deselection stopped publication and preserved the database'

resume_time="$(date -u +'%Y-%m-%dT%H:%M:%S.000Z')"
printf '%s\n' \
  "{\"timestamp\":\"$resume_time\",\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"$resume_marker\"}]}}" \
  >> "$codex_file"
info 'reselecting AI Observer and checking resume from persistent state'
write_integrations 'claude codex aiobserver'
yard init --yes
[ "$(incus config get "$INSTANCE" user.subyard.ai_observer_provision --project "$PROJECT")" = \
    "$initial_provision_marker" ] || die 'reselection did not restore the original convergence identity'
[ "$(guest stat -c '%d:%i' /srv/agents/ai-observer/data/ai-observer.duckdb)" = \
    "$database_inode" ] || die 'reselection replaced the observer database'
api_contains owner "$backfill_marker" || die 'reselection lost the original backfill record'
api_contains owner "$live_marker" || die 'reselection lost the original live record'
api_contains owner "$resume_marker" || die 'reselection did not ingest the record written while stopped'
ok 'reselection preserved history and resumed ingestion'

status_output="$(yard status)"
grep -Eq '^[[:space:]]+profiles[[:space:]]+orca$' <<<"$status_output" \
  || die 'detailed status did not render the selected resource-only profile'
grep -Eq "^[[:space:]]+aiobserver[[:space:]]+up[[:space:]]+\\(http://127\\.0\\.0\\.1:$observer_port/\\)$" \
  <<<"$status_output" || die 'detailed status omitted the healthy observer owner URL'
ok 'detailed status reports the selected profiles and healthy dashboard URL'

printf 'ok: AI Observer real container-yard lifecycle\n'
