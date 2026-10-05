#!/usr/bin/env bash
# Real container-yard lifecycle acceptance for the managed AI Observer integration.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
STATE=''
YARD_NAME=''
PROJECT=''
INSTANCE=''
MARKER=''
TAILSCALE_FIXTURE=0
YARD_RUNTIME_ROOT=''
tail_address=100.100.100.42

die() { printf 'aiobserver-acceptance: %s\n' "$*" >&2; exit 2; }
info() { printf '  [ .. ] %s\n' "$*"; }
ok() { printf '  [ ok ] %s\n' "$*"; }

[ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'run on VM1 through dev/agent-e2e.sh'
for command in curl go incus jq python3 ss sudo sg; do
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
yard_engine() {
  local engine="$ROOT/.build/yard"
  if [ -x "$YARD_RUNTIME_ROOT/current/bin/yard" ]; then
    engine="$YARD_RUNTIME_ROOT/current/bin/yard"
  fi
  # Init can enroll the user in incus-admin without changing this shell's groups.
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    local command
    printf -v command '%q ' "$engine" "$@"
    sg incus-admin -c "exec $command"
  else
    "$engine" "$@"
  fi
}
yard() { yard_engine -Y "$YARD_NAME" "$@"; }
guest() { incus exec "$INSTANCE" --project "$PROJECT" -- "$@"; }

assert_websocket() {
  python3 - "$1" <<'PY'
import socket
import sys
from urllib.parse import urlsplit

origin = sys.argv[1]
url = urlsplit(origin)
for allowed, expected in [(origin, 101), ("http://unrelated.invalid:8080", 403)]:
    with socket.create_connection((url.hostname, url.port), timeout=5) as connection:
        request = "\r\n".join([
            "GET /ws HTTP/1.1", f"Host: {url.netloc}",
            "Connection: Upgrade", "Upgrade: websocket",
            "Sec-WebSocket-Version: 13", "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==",
            f"Origin: {allowed}", "", "",
        ])
        connection.sendall(request.encode())
        with connection.makefile("rb") as response:
            status = int(response.readline(8192).split()[1])
        if status != expected:
            sys.exit(f"WebSocket returned {status}; expected {expected}")
PY
}

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  # The lease broker deletes the disposable VM, including its nested Incus state.
  if [ "$TAILSCALE_FIXTURE" = 1 ]; then
    sudo -n ip address del "$tail_address/32" dev lo
    if sudo -n grep -Fqx "# $MARKER" /usr/local/bin/tailscale; then
      sudo -n rm -- /usr/local/bin/tailscale
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
export YARD_RUNTIME_ROOT="$SUBYARD_HOME/runtime"
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
ENVIRONMENT_PROFILES=
HOST_BASE=$STATE/host
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
EOF
chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env"

backfill_marker="backfill-$token"
large_backfill_marker="large-backfill-$token"
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

# Optional large synthetic history exercises synchronous backfill before HTTP starts.
backfill_records="${SUBYARD_E2E_OBSERVER_BACKFILL_RECORDS:-0}"
[[ "$backfill_records" =~ ^(0|[1-9][0-9]*)$ ]] || die 'invalid synthetic backfill record count'
if [ "$backfill_records" -gt 0 ]; then
  info "seeding $backfill_records synthetic history records"
  python3 - "$(dirname "$claude_file")" "$backfill_records" "$now" "$large_backfill_marker" <<'PY'
import json
import sys
from pathlib import Path

root, count, timestamp, marker = Path(sys.argv[1]), int(sys.argv[2]), sys.argv[3], sys.argv[4]
for batch in range((count + 999) // 1000):
    with (root / f"backlog-{batch}.jsonl").open("w") as output:
        for index in range(batch * 1000, min(count, (batch + 1) * 1000)):
            record = {
                "type": "user", "timestamp": timestamp,
                "sessionId": f"backlog-{batch}", "uuid": f"backlog-{index}",
                "cwd": "/synthetic/aiobserver-e2e",
                "message": {
                    "id": f"backlog-{index}", "role": "user", "type": "message",
                    "content": [{"type": "text", "text": marker if index == count - 1 else "Synthetic startup backlog"}],
                },
            }
            output.write(json.dumps(record) + "\n")
PY
fi

base_version="0.16.4-e2e.$token"
candidate_version="0.16.5-e2e.$token"
base_release="$STATE/release-base"
candidate_release="$STATE/release-candidate"
runtime_arch="$(go env GOARCH)"
base_bundle="$base_release/subyard-$base_version-linux-$runtime_arch.tar.gz"
info 'building native base and update candidates before starting observer backfill'
YARD_BUILD_VERSION="$base_version" "$ROOT/dev/build-engine.sh" --force
"$ROOT/dev/package-engine.sh" --output-dir "$base_release" --version "$base_version" >/dev/null
"$ROOT/dev/package-engine.sh" --output-dir "$candidate_release" --version "$candidate_version" >/dev/null
chmod -R a+rX "$base_release" "$candidate_release"
"$ROOT/scripts/install-runtime-release.sh" \
  --runtime-root "$YARD_RUNTIME_ROOT" \
  --bundle "$base_bundle" --checksum "$base_bundle.sha256" \
  --manifest "$base_bundle.manifest.json" \
  --provenance "$base_bundle.provenance.json" >/dev/null
info 'initializing a container yard with the native base runtime'
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
container_id="$(guest docker inspect -f '{{.Id}}' subyard-ai-observer)"
update_runtime() {
  info 'activating the verified local runtime update'
  local old_release_target
  old_release_target="$(readlink "$YARD_RUNTIME_ROOT/current")"
  YARD_RELEASE_BASE_URL="file://$candidate_release" \
    yard update --version "$candidate_version" --yes
  [ "$(readlink "$YARD_RUNTIME_ROOT/current")" != "$old_release_target" ] \
    || die 'runtime update did not activate the candidate release'
  [ "$(yard --version)" = "yard $candidate_version" ] \
    || die 'candidate runtime version is not active'
  [ "$(guest docker inspect -f '{{.Id}}' subyard-ai-observer)" = "$container_id" ] \
    || die 'release activation replaced the observer container'
}
if [ "$backfill_records" -gt 0 ]; then
  health_state="$(guest /usr/local/bin/ai-observer-health | jq -er '.state')"
  [ "$health_state" = starting ] || die "large backfill did not hold health in starting state (got $health_state)"
  yard integration status aiobserver --json | jq -e \
    '.observed == "ready" and .health.aiobserver == "starting"' >/dev/null \
    || die 'native integration status did not separate installation from startup health'
  yard status | grep -Eq '^[[:space:]]+aiobserver[[:space:]]+starting[[:space:]]' \
    || die 'native yard status did not report pending observer readiness'
  guest /usr/local/bin/ai-observer-installed >/dev/null \
    || die 'observer installation checks failed during backfill'
  if guest /usr/local/bin/ai-observer-check >/dev/null 2>&1; then
    die 'strict HTTP readiness passed before the large backfill completed'
  fi
  info 'repeating native init while the observer is still backfilling'
  yard init --yes
  [ "$(guest docker inspect -f '{{.Id}}' subyard-ai-observer)" = "$container_id" ] \
    || die 'repeat init replaced the observer container during backfill'
  guest systemctl is-active --quiet subyard-ai-observer.service \
    || die 'repeat init stopped the observer service during backfill'
  [ "$(guest docker inspect -f '{{.State.Running}}' subyard-ai-observer)" = true ] \
    || die 'repeat init killed the observer container during backfill'
  [ "$(guest /usr/local/bin/ai-observer-health | jq -er '.state')" = starting ] \
    || die 'repeat init did not leave the observer in its backfill state'
  guest /usr/local/bin/ai-observer-installed >/dev/null \
    || die 'observer installation checks failed after repeat init'
  if guest /usr/local/bin/ai-observer-check >/dev/null 2>&1; then
    die 'strict HTTP readiness passed while the large backfill was still running'
  fi
  info 'updating the yard through the verified local runtime release during backfill'
  update_runtime
  [ "$(guest /usr/local/bin/ai-observer-health | jq -er '.state')" = starting ] \
    || die 'release activation did not tolerate the observer backfill state'
  if guest /usr/local/bin/ai-observer-check >/dev/null 2>&1; then
    die 'strict HTTP readiness passed before release activation completed the backfill'
  fi

  info 'waiting up to 20 minutes for the synthetic observer backfill to finish'
  ready=0
  deadline=$((SECONDS + 1200))
  while [ "$SECONDS" -lt "$deadline" ]; do
    health_state="$(guest /usr/local/bin/ai-observer-health | jq -er '.state')"
    case "$health_state" in
      ready)
        if guest /usr/local/bin/ai-observer-check >/dev/null 2>&1 \
          && curl -fsS --max-time 10 "http://127.0.0.1:$observer_port/health" >/dev/null; then
          ready=1
          break
        fi
        ;;
      starting) ;;
      failed) die 'observer reported failed during large backfill' ;;
      unknown) ;;
      *) die "observer returned invalid health state during backfill: $health_state" ;;
    esac
    sleep 5
  done
  [ "$ready" = 1 ] || die 'observer did not become strictly ready within 20 minutes'
else
  update_runtime
  guest /usr/local/bin/ai-observer-check >/dev/null || die 'observer readiness check failed'
  curl -fsS --max-time 10 "http://127.0.0.1:$observer_port/health" >/dev/null \
    || die 'owner dashboard route is unavailable'
fi
ok 'pinned container, service, read-only session mounts, and owner proxy converged'
assert_websocket "http://127.0.0.1:$observer_port"

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
if [ "$backfill_records" -gt 0 ]; then
  api_contains owner "$large_backfill_marker" \
    || die 'the last synthetic backfill record is absent through owner HTTP'
fi
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

info 'removing Claude as an ingestion source while keeping AI Observer selected'
container_before_source_change="$(guest docker inspect -f '{{.Id}}' subyard-ai-observer)"
yard integration disable claude --yes
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
yard integration enable claude --yes
[ "$(guest docker inspect -f '{{.Id}}' subyard-ai-observer)" != "$container_without_claude" ] \
  || die 'restoring Claude did not replace the observer container'
# Enabling an integration appends it to the requested order, which also changes
# the derived HOST_LINKS bytes. Bind subsequent checks to that canonical intent.
initial_provision_marker="$(incus config get \
  "$INSTANCE" user.subyard.ai_observer_provision --project "$PROJECT")"
[[ "$initial_provision_marker" =~ ^[0-9a-f]{64}$ ]] \
  || die 'restoring Claude did not publish a valid convergence identity'
[ "$initial_provision_marker" != "$without_claude_marker" ] \
  || die 'restoring Claude did not update the observer convergence identity'
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
yard integration disable aiobserver --yes
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
yard integration enable aiobserver --yes
[ "$(incus config get "$INSTANCE" user.subyard.ai_observer_provision --project "$PROJECT")" = \
    "$initial_provision_marker" ] || die 'reselection did not restore the original convergence identity'
[ "$(guest stat -c '%d:%i' /srv/agents/ai-observer/data/ai-observer.duckdb)" = \
    "$database_inode" ] || die 'reselection replaced the observer database'
api_contains owner "$backfill_marker" || die 'reselection lost the original backfill record'
api_contains owner "$live_marker" || die 'reselection lost the original live record'
api_contains owner "$resume_marker" || die 'reselection did not ingest the record written while stopped'
ok 'reselection preserved history and resumed ingestion'

status_output="$(yard status)"
grep -Eq '^[[:space:]]+profiles[[:space:]]+all$' <<<"$status_output" \
  || die 'detailed status did not render the empty profile selection'
grep -Eq "^[[:space:]]+aiobserver[[:space:]]+up[[:space:]]+\\(http://127\\.0\\.0\\.1:$observer_port/\\)$" \
  <<<"$status_output" || die 'detailed status omitted the healthy observer owner URL'
ok 'detailed status reports the selected profiles and healthy dashboard URL'

# Exercise the real Incus bind using a synthetic owner Tailscale address, without
# joining a real tailnet or using account credentials on the disposable VM.
command -v tailscale >/dev/null 2>&1 && die 'Tailscale fixture requires a host without an existing tailscale CLI'
[ ! -e /usr/local/bin/tailscale ] && [ ! -L /usr/local/bin/tailscale ] \
  || die 'refusing to replace an existing tailscale path'
info 'migrating the legacy loopback dashboard to an active owner Tailscale address'
cat >"$STATE/tailscale" <<EOF
#!/bin/sh
# $MARKER
[ "\$*" = 'ip -4' ] || exit 1
printf '%s\\n' '$tail_address'
EOF
sudo -n install -m 0755 "$STATE/tailscale" /usr/local/bin/tailscale
TAILSCALE_FIXTURE=1
sudo -n ip address add "$tail_address/32" dev lo
yard_engine migrate --yes
[ "$(incus config get "$INSTANCE" user.subyard.ai_observer_proxy --project "$PROJECT")" = \
    "v2:$tail_address:$observer_port" ] || die 'Tailscale proxy receipt did not converge'
[ "$(incus config device get "$INSTANCE" ai-observer listen --project "$PROJECT")" = \
    "tcp:$tail_address:$observer_port" ] || die 'dashboard did not bind the exact Tailscale address'
guest /usr/local/bin/ai-observer-check >/dev/null || die 'observer readiness after origin migration failed'
assert_websocket "http://$tail_address:$observer_port"
curl --noproxy '*' -fsS --max-time 10 "http://$tail_address:$observer_port/api/logs?limit=1" \
  | jq -e . >/dev/null || die 'dashboard HTTP is unavailable through the Tailscale address'
yard status >"$STATE/tailscale-status"
grep -Fq "(http://$tail_address:$observer_port/)" "$STATE/tailscale-status" \
  || die 'detailed status omitted the Tailscale dashboard URL'
yard security >/dev/null
container_id="$(guest docker inspect -f '{{.Id}}' subyard-ai-observer)"
yard init --yes
[ "$(guest docker inspect -f '{{.Id}}' subyard-ai-observer)" = "$container_id" ] \
  || die 'Tailscale repeat init replaced the observer container'
[ "$(guest stat -c '%d:%i' /srv/agents/ai-observer/data/ai-observer.duckdb)" = "$database_inode" ] \
  || die 'Tailscale migration replaced the observer database'
ok 'Tailscale route migration, HTTP/WebSocket access, status, security and repeat init passed'

printf 'ok: AI Observer real container-yard lifecycle\n'
