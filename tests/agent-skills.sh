#!/usr/bin/env bash
# Shared delivery and preview migration contracts; no optional-profile content.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bash "$ROOT/tests/helpers/permission-umasks.sh" "$ROOT/config/skills/tests/delivery-test.py" "$ROOT/config/skills/tests/guest-facts-test.py"
python3 -B "$ROOT/config/skills/tests/agent-bridge-test.py"
