#!/usr/bin/env bash
# Owning host-free checks for the clone/link hook.
set -euo pipefail
[ "$#" -eq 0 ] || { printf 'Usage: tests/run.sh\n' >&2; exit 2; }
profile="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bash "$profile/tests/guest-test.sh"
shellcheck -S warning "$profile/"*.sh "$profile/tests/"*.sh "$profile/tests/e2e/"*.sh
bash "$profile/tests/e2e/acceptance.sh" --preflight
