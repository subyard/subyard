#!/usr/bin/env bash
# Check controller dispatch without acquiring a VM lease.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
# shellcheck source=tests/helpers/profile-controller.sh
. "$ROOT/tests/helpers/profile-controller.sh"
setup_profile_controller "$ROOT" config/profiles/openlit/tests/e2e/acceptance.sh
assert_controller_missing_slot
bash "$tmp/$controller" --slot 7 > "$tmp/calls"
grep -Fxq -- '--slot 7 --type android-test --vm-count 1 --purpose openlit-native-telemetry --vm 1 -- timeout --signal=TERM --kill-after=30 3600 bash config/profiles/openlit/tests/e2e/owner.sh' "$tmp/calls"
bash "$tmp/$controller" --slot 7 --kind vm > "$tmp/vm-calls"
grep -Fxq -- '--slot 7 --type android-test --vm-count 1 --purpose openlit-native-telemetry --vm 1 -- timeout --signal=TERM --kill-after=30 3600 bash config/profiles/openlit/tests/e2e/owner.sh vm' "$tmp/vm-calls"
assert_controller_failure --slot 7
printf 'ok: acceptance dispatch, slot requirement and failure propagation\n'

bash "$tmp/$controller" --slot 7 --kind container --auth-only > "$tmp/auth-calls"
grep -Fxq -- '--slot 7 --type android-test --vm-count 1 --purpose openlit-native-telemetry --vm 1 -- timeout --signal=TERM --kill-after=30 3600 bash config/profiles/openlit/tests/e2e/owner.sh container auth-only' "$tmp/auth-calls"

bash "$tmp/$controller" --slot 7 --teardown-only > "$tmp/teardown-calls"
grep -Fxq -- '--slot 7 --type android-test --vm-count 1 --purpose openlit-native-telemetry --vm 1 -- timeout --signal=TERM --kill-after=30 3600 bash config/profiles/openlit/tests/e2e/owner.sh container teardown-only' "$tmp/teardown-calls"

bash "$tmp/$controller" --slot 7 --dependency-only > "$tmp/dependency-calls"
grep -Fxq -- '--slot 7 --type android-test --vm-count 1 --purpose openlit-native-telemetry --vm 1 -- timeout --signal=TERM --kill-after=30 3600 bash config/profiles/openlit/tests/e2e/owner.sh container dependency-only' "$tmp/dependency-calls"

bash "$tmp/$controller" --slot 7 --network-only > "$tmp/network-calls"
grep -Fxq -- '--slot 7 --type android-test --vm-count 1 --purpose openlit-native-telemetry --vm 1 -- timeout --signal=TERM --kill-after=30 3600 bash config/profiles/openlit/tests/e2e/owner.sh container network-only' "$tmp/network-calls"
assert_controller_rejects 2 --slot 7 --kind vm --network-only
assert_controller_rejects 2 --slot 7 --network-only --dependency-only

bash "$tmp/$controller" --slot 7 --pull-diagnostic > "$tmp/pull-calls"
grep -Fxq -- '--slot 7 --type android-test --vm-count 1 --purpose openlit-native-telemetry --vm 1 -- timeout --signal=TERM --kill-after=30 3600 bash config/profiles/openlit/tests/e2e/owner.sh container pull-diagnostic' "$tmp/pull-calls"
for incompatible in '--kind vm' '--dependency-only' '--network-only' '--migration'; do
  read -r -a incompatible_arguments <<< "$incompatible"
  assert_controller_rejects 2 --slot 7 --pull-diagnostic "${incompatible_arguments[@]}"
done

bash "$tmp/$controller" --slot 7 --migration > "$tmp/migration-calls"
grep -Fxq -- '--slot 7 --type android-test --vm-count 1 --purpose openlit-trace-migration --vm 1 -- timeout --signal=TERM --kill-after=30 3600 bash config/profiles/openlit/tests/e2e/migration.sh' "$tmp/migration-calls"
for incompatible in '--kind container' '--kind vm' '--network-only' '--auth-only'; do
  read -r -a incompatible_arguments <<< "$incompatible"
  assert_controller_rejects 2 --slot 7 --migration "${incompatible_arguments[@]}"
done
assert_controller_failure --slot 7 --migration
python3 -B "$ROOT/config/profiles/openlit/tests/migration-process-test.py"
