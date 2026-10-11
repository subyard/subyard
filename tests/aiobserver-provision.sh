#!/usr/bin/env bash
# Compatibility entrypoint; behavior belongs to the integration package.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec bash "$ROOT/config/agents/aiobserver/tests/run.sh" "$@"
