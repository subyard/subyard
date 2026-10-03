#!/usr/bin/env bash
# Check controller dispatch without acquiring a VM lease.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
controller=config/profiles/android/tests/e2e/acceptance.sh
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
if grep -q -- '--purpose' "$tmp/missing"; then exit 1; fi
for lane in full recovery viewer viewer-native-debug sdk-images; do
  arguments=(--slot 7)
  [[ "$lane" = full ]] || arguments+=(--lane "$lane")
  bash "$tmp/$controller" "${arguments[@]}" > "$tmp/calls"
  grep -Fxq -- "--slot 7 --type android-test --vm-count 1 --purpose android-pool-runtime --vm 1 -- bash config/profiles/android/tests/e2e/android-pool-runtime.sh --lane $lane" "$tmp/calls"
  rc=0
  PROFILE_CONTROLLER_EXIT=23 bash "$tmp/$controller" "${arguments[@]}" > "$tmp/failure" 2>&1 || rc=$?
  [ "$rc" -eq 23 ]
  [ "$(wc -l < "$tmp/failure")" -eq 1 ]
done
for arguments in '--slot' '--slot 0' '--slot 7 --lane' '--slot 7 --lane unknown' '--slot 7 --unknown'; do
  read -r -a argv <<< "$arguments"
  rc=0
  bash "$tmp/$controller" "${argv[@]}" > "$tmp/invalid" 2>&1 || rc=$?
  [ "$rc" -eq 2 ]
  if grep -q -- '--purpose' "$tmp/invalid"; then exit 1; fi
done
python3 -B "$ROOT/config/profiles/android/tests/helpers/android-e2e-evidence-test.py"
python3 -B "$ROOT/config/profiles/android/tests/helpers/scrcpy-view-window-test.py"
printf 'ok: Android lane dispatch, arity, failure and cleanup contracts\n'
