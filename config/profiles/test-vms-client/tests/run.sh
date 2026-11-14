#!/usr/bin/env bash
# Host-free contracts of the opt-in test-VM client profile.
set -euo pipefail
PROFILE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ROOT="$(cd "$PROFILE/../../.." && pwd)"
export PYTHONDONTWRITEBYTECODE=1 TMPDIR=/tmp
[ "$#" -eq 0 ] || { printf 'Usage: tests/run.sh\n' >&2; exit 2; }
go -C "$ROOT" test -count=1 ./config/profiles/test-vms-client/tests
python3 -B "$PROFILE/tests/skill-delivery-test.py"
bash "$PROFILE/tests/client-handles.sh"
