#!/usr/bin/env bash
# Owning host-free behavior checks; installed systemd evidence belongs to E2E.
set -euo pipefail
export TMPDIR=/tmp
profile="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
root="$(cd "$profile/../../.." && pwd)"
install -d "$root/.build/git-fetch"
go -C "$root" build -o "$root/.build/git-fetch/github-client" ./config/profiles/github/cmd/broker
export GIT_FETCH_TEST_GITHUB_CLIENT="$root/.build/git-fetch/github-client"
python3 -B "$profile/tests/fetch-test.py"
python3 -B "$profile/tests/guest-test.py"
python3 -B "$profile/tests/release-observation-test.py"
bash "$profile/tests/owner-lifecycle.sh"
bash -n "$profile/owner.sh" "$profile/runtime.sh" "$profile/tests/"*.sh "$profile/tests/e2e/"*.sh
shellcheck -S warning "$profile/owner.sh" "$profile/runtime.sh" "$profile/tests/"*.sh "$profile/tests/e2e/"*.sh
bash "$profile/tests/e2e/acceptance.sh" --help >/dev/null
bash "$profile/tests/e2e/acceptance.sh" --preflight
