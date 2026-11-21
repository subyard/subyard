#!/usr/bin/env bash
# Exact CODING_TOOL_INTEGRATIONS selection controls guest home creation; resolver tests own derived persistence.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=tests/helpers/test-lib.sh
. "$ROOT/tests/helpers/test-lib.sh"

grep -Fq -- '--env CODING_TOOL_INTEGRATIONS="${CODING_TOOL_INTEGRATIONS:-}"' "$ROOT/scripts/04-provision-subyard.sh" \
  || fail "selected agents are not passed to core guest provisioning"
if grep -Fq 'for d in .claude .codex' "$ROOT/scripts/04-provision-subyard.sh"; then
  fail "core provisioning still creates unselected agent homes"
fi

printf 'ok: exact agent selection\n'
