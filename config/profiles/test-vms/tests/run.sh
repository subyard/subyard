#!/usr/bin/env bash
set -euo pipefail
PROFILE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export PYTHONDONTWRITEBYTECODE=1
python3 -B "$PROFILE/tests/test_full_cleanup.py"

(cd "$PROFILE/../../.." && go test ./config/profiles/test-vms)
