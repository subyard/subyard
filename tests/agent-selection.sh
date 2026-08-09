#!/usr/bin/env bash
# Exact AGENTS selection controls guest home creation; resolver tests own derived persistence.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

grep -Fq -- '--env AGENTS="${AGENTS:-}"' "$ROOT/scripts/04-provision-subyard.sh" \
  || fail "selected agents are not passed to core guest provisioning"
if grep -Fq 'for d in .claude .codex' "$ROOT/scripts/04-provision-subyard.sh"; then
  fail "core provisioning still creates unselected agent homes"
fi

printf 'ok: exact agent selection\n'
