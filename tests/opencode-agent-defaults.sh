#!/usr/bin/env bash
# OpenCode default-config checks.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "missing test dependency: $1"; }
need jq

# The public policy contains no provider, model, or credentials.
policy="$ROOT/config/agents/opencode/opencode.jsonc"
jq -e '
  .permission["*"] == "allow" and
  .permission.bash["*"] == "allow" and
  .permission.bash["git commit"] == "ask" and
  .permission.bash["git commit *"] == "ask" and
  .permission.bash["git push"] == "ask" and
  .permission.bash["git push *"] == "ask" and
  (has("autoupdate") | not) and
  (has("provider") | not) and
  (has("model") | not)
' "$policy" >/dev/null || fail "OpenCode yard policy contract is invalid"

manifest="$ROOT/config/agents/opencode/integration.json"
jq -e '
  .schema_version == 2 and .command == "opencode" and .provision == "provision.sh" and
  (.artifacts | length) == 2 and
  (.artifacts[0] | .id == "config" and .source == "opencode.jsonc" and
    .destination == ".config/opencode/opencode.jsonc" and .format == "destination") and
  (.artifacts[1] | .kind == "instructions" and .source_setting == "HOST_OPENCODE_AGENTS_MD" and
    .destination == ".config/opencode/AGENTS.md")
' "$manifest" >/dev/null || fail "OpenCode declaration drifted"
AGENT_opencode_PERSIST="$(jq -r '.persist' "$manifest")"
[ -x "$ROOT/config/agents/opencode/$(jq -r '.provision' "$manifest")" ] \
  || fail "OpenCode provision hook is not executable"
if grep -Eq '^AGENT_opencode_' "$ROOT/config/agents.env"; then
  fail "OpenCode shipped defaults have two authorities"
fi
grep -Fq '_provision_var="AGENT_' "$ROOT/scripts/reconcile-integrations.sh" \
  || fail "Bounded integration adapter does not discover agent provision hooks"
case "$AGENT_opencode_PERSIST" in
  *auth.json* | *'/log'* | *'/storage'*) fail "OpenCode secrets/logs/legacy storage are persisted" ;;
esac
case "$AGENT_opencode_PERSIST" in
  *'.local/share/opencode/opencode.db:/mnt/host/agent-sessions/opencode/opencode.db:file'*) ;;
  *) fail "OpenCode SQLite session database is not persisted" ;;
esac

# Shipped templates retain commit/push rules; runtime enforcement needs separate acceptance.
jq -e '
  (.permissions.ask | index("Bash(git commit)")) != null and
  (.permissions.ask | index("Bash(git commit:*)")) != null and
  (.permissions.ask | index("Bash(git push)")) != null and
  (.permissions.ask | index("Bash(git push:*)")) != null
' "$ROOT/config/agents/claude/settings.json" >/dev/null || fail "Claude commit/push ask entries drifted"
for command in commit push; do
  grep -Fq "pattern = [\"git\", \"$command\"]" "$ROOT/config/agents/codex/rules/repo.rules" \
    || fail "Codex $command gate missing"
done
grep -Fq 'approval_policy = "on-request"' \
  "$ROOT/config/agents/codex/config.toml" \
  || fail "Codex yard approvals are not interactive"
grep -Fq 'approvals_reviewer = "user"' \
  "$ROOT/config/agents/codex/config.toml" \
  || fail "Codex yard approvals are not routed to the operator"
grep -Fq 'sandbox_mode = "danger-full-access"' \
  "$ROOT/config/agents/codex/config.toml" \
  || fail "Codex yard permissions are not unrestricted"
if grep -Eq '^[[:space:]]*(default_permissions|\[permissions\.|\[sandbox_workspace_write\])' \
  "$ROOT/config/agents/codex/config.toml"; then
  fail "Codex yard config mixes legacy sandbox settings with permission profiles"
fi
grep -Fq 'HOST_OPENCODE_AGENTS_MD=' "$ROOT/config/host.env" \
  || fail "OpenCode host instructions are not configurable"

printf 'ok: OpenCode agent defaults\n'
