#!/usr/bin/env bash
# Existing record and correlation contracts are small enough for the early matrix.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
if [ -z "${SUBYARD_TELEMETRY_COLLECTOR:-}" ]; then
  cd "$ROOT"
  mkdir -p .build/telemetry-tests
  export SUBYARD_TELEMETRY_COLLECTOR="$ROOT/.build/telemetry-tests/subyard-telemetry"
  go build -o "$SUBYARD_TELEMETRY_COLLECTOR" ./config/profiles/telemetry/cmd/telemetry
fi
bash "$ROOT/tests/helpers/permission-umasks.sh" \
  "$ROOT/config/profiles/telemetry/tests/records-test.py" \
  "$ROOT/config/profiles/telemetry/tests/resources-test.py"
