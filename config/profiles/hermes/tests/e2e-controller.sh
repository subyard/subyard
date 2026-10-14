#!/usr/bin/env bash
# Check controller dispatch without acquiring a VM lease.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
# shellcheck source=tests/helpers/profile-controller.sh
. "$ROOT/tests/helpers/profile-controller.sh"
setup_profile_controller "$ROOT" config/profiles/hermes/tests/e2e/acceptance.sh
assert_controller_missing_slot
bash "$tmp/$controller" --slot 7 > "$tmp/calls"
grep -Fxq -- '--slot 7 --vm-count 1 --purpose hermes-profile --vm 1 -- bash config/profiles/hermes/tests/e2e/hermes-profile.sh' "$tmp/calls"
assert_controller_failure --slot 7
printf 'ok: acceptance dispatch, slot requirement and failure propagation\n'
