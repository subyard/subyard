#!/usr/bin/env bash
# Protected developer rename, account-tool interruption and retained-data contracts.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
python3 -B "$ROOT/config/identity/tests/transition-test.py"
python3 -B "$ROOT/config/identity/tests/subids-test.py"
