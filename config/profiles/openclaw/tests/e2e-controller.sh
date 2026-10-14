#!/usr/bin/env bash
# Check OpenClaw acceptance dispatch without acquiring a VM lease.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
manifest="$ROOT/config/profiles/openclaw/tests/e2e/external-obligations.json"
jq -e '.schema_version == 1 and (.obligations | length >= 1) and all(.obligations[]; (.id | type == "string" and length > 0) and (.description | type == "string" and length > 0) and (.required_inputs | type == "array" and length > 0))' \
  "$manifest" >/dev/null
# shellcheck source=tests/helpers/profile-controller.sh
. "$ROOT/tests/helpers/profile-controller.sh"
setup_profile_controller "$ROOT" config/profiles/openclaw/tests/e2e/acceptance.sh
assert_controller_missing_slot
assert_controller_rejects 2 --slot 0
bash "$tmp/$controller" --slot 7 > "$tmp/calls"
grep -Fxq -- '--slot 7 --vm-count 1 --purpose openclaw-provision --vm 1 -- bash config/profiles/openclaw/tests/e2e/owner.sh' "$tmp/calls"
assert_controller_failure --slot 7
printf 'ok: OpenClaw acceptance dispatch, slot requirement and failure propagation\n'
