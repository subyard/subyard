#!/usr/bin/env bash
# OpenCode default-config checks.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "missing test dependency: $1"; }
need jq

# The public policies contain no provider, model, or credentials.
dir="$ROOT/config/agents/opencode"
jq -e '
  .permission == {"*": "allow", "bash": {"*": "allow"}} and
  (has("autoupdate") | not) and (has("provider") | not) and (has("model") | not)
' "$dir/opencode.jsonc" >/dev/null || fail "OpenCode yard policy contract is invalid"
git_ask='["git commit", "git commit *", "git push", "git push *"]'
sudo_ask='["sudo", "sudo *", "su", "su *", "doas", "doas *", "pkexec", "pkexec *", "sudoedit", "sudoedit *", "run0", "run0 *"]'
for entry in 50-subyard-git-approval:"$git_ask" 51-subyard-sudo-approval:"$sudo_ask"; do
  name="${entry%%:*}"
  jq -e --argjson ask "${entry#*:}" '
    . == {permission: {bash: ($ask | map({(.): "ask"}) | add)}}
  ' "$dir/$name.jsonc" >/dev/null || fail "OpenCode $name policy has unexpected rules"
done

manifest="$dir/integration.json"
jq -e '
  .schema_version == 2 and .command == "opencode" and .provision == "provision.sh" and
  (.artifacts | length) == 1 and
  (.artifacts[0] | .id == "config" and .source == "opencode.jsonc" and
    .approval_sources == {"git": "50-subyard-git-approval.jsonc", "sudo": "51-subyard-sudo-approval.jsonc"} and
    .destination == ".config/opencode/opencode.jsonc" and .format == "destination")
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

printf 'ok: OpenCode agent defaults\n'
