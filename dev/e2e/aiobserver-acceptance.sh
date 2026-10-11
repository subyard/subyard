#!/usr/bin/env bash
# Compatibility entrypoint for existing VM and release coverage.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
exec bash "$ROOT/config/agents/aiobserver/tests/e2e/acceptance.sh" "$@"
