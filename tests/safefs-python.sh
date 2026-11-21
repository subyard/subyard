#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bash "$ROOT/tests/helpers/permission-umasks.sh" "$ROOT/internal/safefs/safefs_test.py"
