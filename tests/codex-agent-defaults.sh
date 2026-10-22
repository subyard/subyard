#!/usr/bin/env bash
# Codex package wiring without a shipped agent release pin.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

manifest="$ROOT/config/agents/codex/integration.json"
jq -e '.schema_version == 2 and .provision == "provision.sh" and
  .command == "codex" and .check == "codex-policy-check" and
  (.artifacts | length) == 3 and
  (.artifacts[0] | .source == "config.toml" and .destination == ".codex/config.toml" and .format == "destination") and
  (.artifacts[1] | .source == "rules/repo.rules" and .destination == ".codex/rules/repo.rules" and .format == "raw") and
  (.artifacts[2] | .source_setting == "HOST_CODEX_AGENTS_MD" and .destination == ".codex/AGENTS.md")
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

printf 'ok: Codex agent defaults\n'
