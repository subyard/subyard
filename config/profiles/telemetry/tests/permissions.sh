#!/usr/bin/env bash
# Existing record and correlation contracts are small enough for the early matrix.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
bash "$ROOT/tests/helpers/permission-umasks.sh" \
  "$ROOT/config/profiles/telemetry/tests/records-test.py" \
  "$ROOT/config/profiles/telemetry/tests/resources-test.py"
