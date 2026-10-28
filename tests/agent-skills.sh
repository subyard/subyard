#!/usr/bin/env bash
# Shared delivery and preview migration contracts; no optional-profile content.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bash "$ROOT/tests/helpers/permission-umasks.sh" "$ROOT/config/skills/tests/delivery-test.py"
