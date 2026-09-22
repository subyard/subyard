#!/usr/bin/env bash
# Codex package wiring without a shipped agent release pin.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

SUBYARD_CONFIG_DIR="$ROOT/config"
# shellcheck source=config/agents.env
set -a
. "$ROOT/config/agents.env"
set +a

[ "$AGENT_codex_PROVISION" = "$ROOT/config/agents/codex/provision.sh" ] \
  || fail "Codex provision hook is not wired"
[ "$AGENT_codex_COMMAND" = codex ] || fail "Codex convergence command drifted"
[ "$AGENT_codex_CHECK" = codex-policy-check ] || fail "Codex package check drifted"
[ -x "$AGENT_codex_PROVISION" ] || fail "Codex provision hook is not executable"

case "$AGENT_codex_PERSIST" in
  *auth.json* | *credentials* | *tokens*) fail "Codex authorization is host-persisted" ;;
esac

printf 'ok: Codex agent defaults\n'
