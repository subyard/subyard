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
! grep -q -- '--purpose' "$tmp/missing"
bash "$tmp/$controller" --slot 7 > "$tmp/calls"
grep -Fxq -- '--slot 7 --type android-test --vm-count 1 --purpose android-pool-runtime --vm 1 -- bash config/profiles/android/tests/e2e/android-pool-runtime.sh --lane full' "$tmp/calls"
rc=0
PROFILE_CONTROLLER_EXIT=23 bash "$tmp/$controller" --slot 7 > "$tmp/failure" 2>&1 || rc=$?
[ "$rc" -eq 23 ]
[ "$(wc -l < "$tmp/failure")" -eq 1 ]
printf 'ok: acceptance dispatch, slot requirement and failure propagation\n'
