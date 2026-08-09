#!/usr/bin/env bash
# Codex package wiring and immutable release-pin checks.
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
[ "$AGENT_codex_CHECK" = codex-check ] || fail "Codex package check drifted"
[ -x "$AGENT_codex_PROVISION" ] || fail "Codex provision hook is not executable"
[ "$CODEX_VERSION" = 0.147.0 ] || fail "Codex release pin drifted"
[ "$CODEX_SHA256_AMD64" = 0246e2e773834e07f0fb5249ed6ebad12e4591e608f8c7bb97dd6a9690544c36 ] \
  || fail "Codex amd64 checksum pin drifted"
[ "$CODEX_SHA256_ARM64" = eb677c80f666b1ab8b4b1d083b66e8d614b1281d960bb6f9fd8ca98f58b38b90 ] \
  || fail "Codex arm64 checksum pin drifted"

case "$AGENT_codex_PERSIST" in
  *auth.json* | *credentials* | *tokens*) fail "Codex authorization is host-persisted" ;;
esac
grep -Fq -- '--env CODEX_VERSION="$CODEX_VERSION"' "$ROOT/scripts/04-provision-subyard.sh" \
  || fail "Codex version pin is not passed to agent provision hooks"
grep -Fq -- '--env CODEX_SHA256_AMD64="$CODEX_SHA256_AMD64"' \
  "$ROOT/scripts/04-provision-subyard.sh" \
  || fail "Codex amd64 checksum is not passed to agent provision hooks"
grep -Fq -- '--env CODEX_SHA256_ARM64="$CODEX_SHA256_ARM64"' \
  "$ROOT/scripts/04-provision-subyard.sh" \
  || fail "Codex arm64 checksum is not passed to agent provision hooks"
grep -Fq 'CODEX_VERSION CODEX_SHA256_AMD64 CODEX_SHA256_ARM64' \
  "$ROOT/scripts/lib/engine-context.sh" \
  || fail "Codex pins do not survive elevated adapter execution"

printf 'ok: Codex agent defaults\n'
