#!/usr/bin/env bash
# Owning host-free checks: the sync helper, the owner transport and skill delivery.
set -euo pipefail
[ "$#" -eq 0 ] || { printf 'Usage: tests/run.sh\n' >&2; exit 2; }
profile="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
root="$(cd "$profile/../../.." && pwd)"
python3 -B "$profile/tests/sync-test.py"
bash "$profile/tests/owner-lifecycle.sh"
go -C "$root" test ./config/profiles/user-skills/...
bash -n "$profile/owner.sh" "$profile/tests/"*.sh "$profile/tests/e2e/"*.sh
shellcheck -S warning "$profile/owner.sh" "$profile/tests/"*.sh
bash "$profile/tests/e2e/acceptance.sh" --help >/dev/null
bash "$profile/tests/e2e/acceptance.sh" --preflight
