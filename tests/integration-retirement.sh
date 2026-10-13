#!/usr/bin/env bash
# Retirement compatibility remains separate from active integration provisioning.
set -euo pipefail
export TMPDIR=/tmp
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
python3 -B "$root/config/retired-integrations/aiobserver/tests/retire-test.py"
python3 -B "$root/config/retired-integrations/aiobserver/tests/rollback-refusal-test.py"
