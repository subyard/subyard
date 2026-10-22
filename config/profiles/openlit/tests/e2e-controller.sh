#!/usr/bin/env bash
# Check controller dispatch without acquiring a VM lease.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
# shellcheck source=tests/helpers/profile-controller.sh
. "$ROOT/tests/helpers/profile-controller.sh"
setup_profile_controller "$ROOT" config/profiles/openlit/tests/e2e/acceptance.sh
assert_controller_missing_slot
bash "$tmp/$controller" --slot 7 > "$tmp/calls"
grep -Fxq -- '--slot 7 --type android-test --vm-count 1 --purpose openlit-native-telemetry --vm 1 -- timeout --signal=TERM --kill-after=180 4500 bash config/profiles/openlit/tests/e2e/owner.sh' "$tmp/calls"
bash "$tmp/$controller" --slot 7 --kind container > "$tmp/container-calls"
grep -Fxq -- '--slot 7 --type android-test --vm-count 1 --purpose openlit-native-telemetry --vm 1 -- timeout --signal=TERM --kill-after=180 4500 bash config/profiles/openlit/tests/e2e/owner.sh container' "$tmp/container-calls"
bash "$tmp/$controller" --slot 7 --kind vm > "$tmp/vm-calls"
grep -Fxq -- '--slot 7 --type android-test --vm-count 1 --purpose openlit-native-telemetry --vm 1 -- timeout --signal=TERM --kill-after=180 4500 bash config/profiles/openlit/tests/e2e/owner.sh vm' "$tmp/vm-calls"
assert_controller_failure --slot 7
printf 'ok: acceptance dispatch, slot requirement and failure propagation\n'

for mode in auth-only teardown-only dependency-only network-only; do
  bash "$tmp/$controller" --slot 7 --"$mode" > "$tmp/$mode-calls"
  grep -Fxq -- "--slot 7 --type android-test --vm-count 1 --purpose openlit-native-telemetry --vm 1 -- timeout --signal=TERM --kill-after=30 3600 bash config/profiles/openlit/tests/e2e/owner.sh container $mode" "$tmp/$mode-calls"
done
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
assert_controller_rejects 2 --slot 7 --native-loss invalid
assert_controller_rejects 2 --slot 7 --native-loss group --kind vm
assert_controller_rejects 2 --slot 7 --native-loss helper --auth-only
assert_controller_rejects 2 --slot 7 --native-loss group --native-loss owner
# Dispatch grammar uses a stub at the separately tested frozen-bundle boundary.
mkdir -p "$tmp/tests/helpers"
cat > "$tmp/tests/helpers/profile-native-loss.py" <<'PY'
import os, pathlib, sys
assert sys.argv[1:] == ['--preflight', '--bundle', '/frozen/bundle', '--bundle-sha256', 'a'*64]
pathlib.Path(os.environ['PROFILE_CONTROLLER_LOG']).with_name('preflight-checked').touch()
PY
SUBYARD_E2E_CANDIDATE_BUNDLE=/frozen/bundle SUBYARD_E2E_CANDIDATE_SHA256="$(printf 'a%.0s' {1..64})" \
  bash "$tmp/$controller" --slot 7 --native-loss group > "$tmp/loss-calls"
[ -f "$tmp/preflight-checked" ]
grep -Fxq -- '--slot 7 --type android-test --vm-count 1 --purpose openlit-native-telemetry --vm 1 -- timeout --signal=TERM --kill-after=180 4500 bash config/profiles/openlit/tests/e2e/owner.sh container  group' "$tmp/loss-calls"
