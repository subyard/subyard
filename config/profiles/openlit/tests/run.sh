#!/usr/bin/env bash
# OpenLit identity recovery is supported even while new activation is disabled.
set -euo pipefail
export TMPDIR=/tmp
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
python3 -B "$ROOT/config/profiles/openlit/tests/identity-test.py"
python3 -B "$ROOT/config/profiles/openlit/tests/runtime-test.py"
