#!/usr/bin/env bash
# OpenCode default-config checks.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "missing test dependency: $1"; }
need jq

# The public policies contain no provider, model, or credentials. The base policy asks for
# nothing; each approval variant adds only its gate's ask patterns.
dir="$ROOT/config/agents/opencode"
jq -e '
  .permission == {"*": "allow", "bash": {"*": "allow"}} and
  (has("autoupdate") | not) and (has("provider") | not) and (has("model") | not)
' "$dir/opencode.jsonc" >/dev/null || fail "OpenCode yard policy contract is invalid"
git_ask='["git commit", "git commit *", "git push", "git push *"]'
sudo_ask='["sudo", "sudo *", "su", "su *", "doas", "doas *", "pkexec", "pkexec *", "sudoedit", "sudoedit *", "run0", "run0 *"]'
for variant in git:"$git_ask" sudo:"$sudo_ask" git-sudo:"$(jq -cn --argjson a "$git_ask" --argjson b "$sudo_ask" '$a + $b')"; do
  name="${variant%%:*}"
  diff -u <(jq --argjson ask "${variant#*:}" '.permission.bash += ($ask | map({(.): "ask"}) | add)' "$dir/opencode.jsonc") \
    <(jq . "$dir/opencode-$name.jsonc") \
    || fail "OpenCode $name policy must add only its approval prompts"
done

manifest="$dir/integration.json"
jq -e '
  .schema_version == 2 and .command == "opencode" and .provision == "provision.sh" and
  (.artifacts | length) == 2 and
  (.artifacts[0] | .id == "config" and .source == "opencode.jsonc" and
    .approval_sources == {"git": "opencode-git.jsonc", "sudo": "opencode-sudo.jsonc", "git sudo": "opencode-git-sudo.jsonc"} and
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

grep -Fq 'HOST_OPENCODE_AGENTS_MD=' "$ROOT/config/host.env" \
  || fail "OpenCode host instructions are not configurable"

printf 'ok: OpenCode agent defaults\n'
