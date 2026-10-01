#!/usr/bin/env bash
# Check controller dispatch without acquiring a VM lease.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
controller=config/profiles/orca/tests/e2e/acceptance.sh
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
[ "$(wc -l < "$tmp/calls")" -eq 6 ]
grep -Fxq -- '--slot 7 --type android-test --purpose orca-bootstrap --vm 1 -- env SUBYARD_E2E_ORCA_BOOTSTRAP=1 bash config/profiles/orca/tests/e2e/orca-bootstrap.sh' "$tmp/calls"
grep -Fxq -- '--slot 7 --type android-test --purpose orca-bootstrap --vm 1 -- env SUBYARD_E2E_ORCA_BOOTSTRAP=1 SUBYARD_E2E_ORCA_EXISTING_YARD=1 bash config/profiles/orca/tests/e2e/orca-bootstrap.sh' "$tmp/calls"
grep -Fxq -- '--slot 7 --type android-test --purpose orca-resource --vm 1 -- env SUBYARD_E2E_ORCA_RESOURCE=1 bash config/profiles/orca/tests/e2e/orca-resource.sh' "$tmp/calls"
grep -Fxq -- '--slot 7 --type android-test --purpose orca-projects --vm 1 -- env SUBYARD_E2E_ORCA_PROJECTS=1 bash config/profiles/orca/tests/e2e/orca-projects.sh' "$tmp/calls"
grep -Fxq -- '--slot 7 --type android-test --purpose orca-ssh-agent --vm 1 -- env SUBYARD_E2E_ORCA_BOOTSTRAP=1 SUBYARD_E2E_ORCA_SSH_AGENT=1 bash config/profiles/orca/tests/e2e/orca-bootstrap.sh' "$tmp/calls"
grep -Fxq -- '--slot 7 --type android-test --purpose codex-permissions --vm 1 -- env SUBYARD_E2E_ORCA_BOOTSTRAP=1 SUBYARD_E2E_ORCA_CODEX_PERMISSIONS=1 bash config/profiles/orca/tests/e2e/orca-bootstrap.sh' "$tmp/calls"
rc=0
PROFILE_CONTROLLER_EXIT=23 bash "$tmp/$controller" --slot 7 > "$tmp/failure" 2>&1 || rc=$?
[ "$rc" -eq 23 ]
[ "$(wc -l < "$tmp/failure")" -eq 1 ]
printf 'ok: acceptance dispatch, slot requirement and failure propagation\n'
