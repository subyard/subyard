#!/usr/bin/env bash
# Check controller dispatch without acquiring a VM lease.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
tmp="$(mktemp -d /tmp/subyard-openlit-controller.XXXXXX)"
trap 'rm -rf -- "$tmp"' EXIT
controller=config/profiles/openlit/tests/e2e/acceptance.sh
mkdir -p "$tmp/$(dirname "$controller")" "$tmp/dev"
cp "$ROOT/$controller" "$tmp/$controller"
cat > "$tmp/dev/agent-e2e.sh" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$*"
exit "${PROFILE_CONTROLLER_EXIT:-0}"
SH
if bash "$tmp/$controller" > "$tmp/missing" 2>&1; then
  printf 'FAIL: controller accepted a missing slot\n' >&2; exit 1
fi
! grep -q -- '--purpose' "$tmp/missing"
bash "$tmp/$controller" --slot 7 > "$tmp/calls"
grep -Fxq -- '--slot 7 --type android-test --vm-count 1 --purpose openlit-native-telemetry --vm 1 -- timeout --signal=TERM --kill-after=30 3600 bash config/profiles/openlit/tests/e2e/owner.sh' "$tmp/calls"
bash "$tmp/$controller" --slot 7 --kind vm > "$tmp/vm-calls"
grep -Fxq -- '--slot 7 --type android-test --vm-count 1 --purpose openlit-native-telemetry --vm 1 -- timeout --signal=TERM --kill-after=30 3600 bash config/profiles/openlit/tests/e2e/owner.sh vm' "$tmp/vm-calls"
rc=0
PROFILE_CONTROLLER_EXIT=23 bash "$tmp/$controller" --slot 7 > "$tmp/failure" 2>&1 || rc=$?
[ "$rc" -eq 23 ]
[ "$(wc -l < "$tmp/failure")" -eq 1 ]
printf 'ok: acceptance dispatch, slot requirement and failure propagation\n'

bash "$tmp/$controller" --slot 7 --kind container --auth-only > "$tmp/auth-calls"
grep -Fxq -- '--slot 7 --type android-test --vm-count 1 --purpose openlit-native-telemetry --vm 1 -- timeout --signal=TERM --kill-after=30 3600 bash config/profiles/openlit/tests/e2e/owner.sh container auth-only' "$tmp/auth-calls"

bash "$tmp/$controller" --slot 7 --teardown-only > "$tmp/teardown-calls"
grep -Fxq -- '--slot 7 --type android-test --vm-count 1 --purpose openlit-native-telemetry --vm 1 -- timeout --signal=TERM --kill-after=30 3600 bash config/profiles/openlit/tests/e2e/owner.sh container teardown-only' "$tmp/teardown-calls"

bash "$tmp/$controller" --slot 7 --dependency-only > "$tmp/dependency-calls"
grep -Fxq -- '--slot 7 --type android-test --vm-count 1 --purpose openlit-native-telemetry --vm 1 -- timeout --signal=TERM --kill-after=30 3600 bash config/profiles/openlit/tests/e2e/owner.sh container dependency-only' "$tmp/dependency-calls"

bash "$tmp/$controller" --slot 7 --network-only > "$tmp/network-calls"
grep -Fxq -- '--slot 7 --type android-test --vm-count 1 --purpose openlit-native-telemetry --vm 1 -- timeout --signal=TERM --kill-after=30 3600 bash config/profiles/openlit/tests/e2e/owner.sh container network-only' "$tmp/network-calls"
if bash "$tmp/$controller" --slot 7 --kind vm --network-only > "$tmp/network-vm" 2>&1; then
  printf 'FAIL: network-only accepted VM mode\n' >&2; exit 1
fi
! grep -q -- '--purpose' "$tmp/network-vm"
if bash "$tmp/$controller" --slot 7 --network-only --dependency-only > "$tmp/network-conflict" 2>&1; then
  printf 'FAIL: network-only accepted a second diagnostic mode\n' >&2; exit 1
fi
! grep -q -- '--purpose' "$tmp/network-conflict"

bash "$tmp/$controller" --slot 7 --pull-diagnostic > "$tmp/pull-calls"
grep -Fxq -- '--slot 7 --type android-test --vm-count 1 --purpose openlit-native-telemetry --vm 1 -- timeout --signal=TERM --kill-after=30 3600 bash config/profiles/openlit/tests/e2e/owner.sh container pull-diagnostic' "$tmp/pull-calls"
for incompatible in '--kind vm' '--dependency-only' '--network-only' '--migration'; do
  read -r -a incompatible_arguments <<< "$incompatible"
  if bash "$tmp/$controller" --slot 7 --pull-diagnostic "${incompatible_arguments[@]}" > "$tmp/pull-conflict" 2>&1; then
    printf 'FAIL: pull-diagnostic accepted conflicting mode\n' >&2; exit 1
  fi
  ! grep -q -- '--purpose' "$tmp/pull-conflict"
done

bash "$tmp/$controller" --slot 7 --migration > "$tmp/migration-calls"
grep -Fxq -- '--slot 7 --type android-test --vm-count 1 --purpose openlit-trace-migration --vm 1 -- timeout --signal=TERM --kill-after=30 3600 bash config/profiles/openlit/tests/e2e/migration.sh' "$tmp/migration-calls"
for incompatible in '--kind container' '--kind vm' '--network-only' '--auth-only'; do
  read -r -a incompatible_arguments <<< "$incompatible"
  if bash "$tmp/$controller" --slot 7 --migration "${incompatible_arguments[@]}" > "$tmp/migration-conflict" 2>&1; then
    printf 'FAIL: migration accepted conflicting mode\n' >&2; exit 1
  fi
  if grep -q -- '--purpose' "$tmp/migration-conflict"; then
    printf 'FAIL: conflicting migration allocated a runner\n' >&2; exit 1
  fi
done
rc=0
PROFILE_CONTROLLER_EXIT=23 bash "$tmp/$controller" --slot 7 --migration > "$tmp/migration-failure" 2>&1 || rc=$?
[ "$rc" -eq 23 ]
python3 -B "$ROOT/config/profiles/openlit/tests/migration-process-test.py"
