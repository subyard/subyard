#!/usr/bin/env bash
# Check controller dispatch without acquiring a VM lease.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
# shellcheck source=tests/helpers/profile-controller.sh
. "$ROOT/tests/helpers/profile-controller.sh"
setup_profile_controller "$ROOT" config/profiles/android/tests/e2e/acceptance.sh
assert_controller_missing_slot
for lane in full diagnostics recovery viewer viewer-native-debug sdk-images memory-lease; do
  arguments=(--slot 7)
  [[ "$lane" = full ]] || arguments+=(--lane "$lane")
  bash "$tmp/$controller" "${arguments[@]}" > "$tmp/calls"
  grep -Fxq -- "--slot 7 --type android-test --vm-count 1 --purpose android-pool-runtime --vm 1 -- bash config/profiles/android/tests/e2e/android-pool-runtime.sh --lane $lane" "$tmp/calls"
  assert_controller_failure "${arguments[@]}"
done
for arguments in '--slot' '--slot 0' '--slot 7 --lane' '--slot 7 --lane unknown' '--slot 7 --unknown'; do
  read -r -a argv <<< "$arguments"
  assert_controller_rejects 2 "${argv[@]}"
done
python3 -B "$ROOT/config/profiles/android/tests/helpers/android-e2e-evidence-test.py"
python3 -B "$ROOT/config/profiles/android/tests/helpers/scrcpy-view-window-test.py"
printf 'ok: Android lane dispatch, arity, failure and cleanup contracts\n'
