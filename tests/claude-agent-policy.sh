#!/usr/bin/env bash
# Compatibility entrypoint; optional integration assertions live in their package.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec bash "$ROOT/config/agents/claude/tests/provision.sh" "$@"
