#!/usr/bin/env bash
# Codex package wiring without a shipped agent release pin.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=tests/helpers/test-lib.sh
. "$ROOT/tests/helpers/test-lib.sh"

manifest="$ROOT/config/agents/codex/integration.json"
jq -e '.schema_version == 2 and .provision == "provision.sh" and
  .command == "codex" and .check == "codex-policy-check" and
  (.artifacts | length) == 2 and
  (.artifacts[0] | .source == "config.toml" and .destination == ".codex/config.toml" and .format == "destination") and
  (.artifacts[1] | .source == "rules/repo.rules" and .destination == ".codex/rules/repo.rules" and .format == "raw")
' "$manifest" >/dev/null || fail "Codex package declaration drifted"
[ -x "$ROOT/config/agents/codex/$(jq -r '.provision' "$manifest")" ] \
  || fail "Codex provision hook is not executable"
AGENT_codex_PERSIST="$(jq -r '.persist' "$manifest")"
if grep -Eq '^AGENT_codex_' "$ROOT/config/agents.env"; then
  fail "Codex shipped defaults have two authorities"
fi

case "$AGENT_codex_PERSIST" in
  *auth.json* | *credentials* | *tokens*) fail "Codex authorization is host-persisted" ;;
esac

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

printf 'ok: Codex agent defaults\n'
