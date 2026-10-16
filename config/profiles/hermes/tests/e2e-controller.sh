#!/usr/bin/env bash
# Host-free controller, original diagnostic retention and cleanup checks.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
python3 -B "$ROOT/config/profiles/hermes/tests/e2e-capture-test.py"
