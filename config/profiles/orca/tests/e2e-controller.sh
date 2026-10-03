#!/usr/bin/env bash
# Check controller dispatch without acquiring a VM lease.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
controller=config/profiles/orca/tests/e2e/acceptance.sh
mkdir -p "$tmp/$(dirname "$controller")" "$tmp/dev"
cp "$ROOT/$controller" "$tmp/$controller"
export PROFILE_CONTROLLER_CALLS="$tmp/broker-calls"
cat > "$tmp/dev/agent-e2e.sh" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$PROFILE_CONTROLLER_CALLS"
exit "${PROFILE_CONTROLLER_EXIT:-0}"
SH
reject_before_broker() {
  local rc=0
  : > "$PROFILE_CONTROLLER_CALLS"
  bash "$tmp/$controller" "$@" > "$tmp/rejected" 2>&1 || rc=$?
  [ "$rc" -eq 2 ] && [ ! -s "$PROFILE_CONTROLLER_CALLS" ] || {
    printf 'FAIL: expected argument rejection before broker: %s\n' "$*" >&2; exit 1
  }
}
reject_before_broker
reject_before_broker --slot
reject_before_broker --slot 0
reject_before_broker --slot abc
reject_before_broker --slot 7 --lane
reject_before_broker --slot 7 --lane ''
reject_before_broker --slot 7 --lane invalid
reject_before_broker --slot 7 --lane --slot 8
reject_before_broker --slot 7 --lane bootstrap --unknown

cat > "$tmp/expected" <<'CALLS'
--slot 7 --vm-count 1 --purpose orca-bootstrap --vm 1 -- env SUBYARD_E2E_ORCA_BOOTSTRAP=1 bash config/profiles/orca/tests/e2e/orca-bootstrap.sh
--slot 7 --vm-count 1 --purpose orca-bootstrap --vm 1 -- env SUBYARD_E2E_ORCA_BOOTSTRAP=1 SUBYARD_E2E_ORCA_EXISTING_YARD=1 bash config/profiles/orca/tests/e2e/orca-bootstrap.sh
--slot 7 --vm-count 1 --purpose orca-resource --vm 1 -- env SUBYARD_E2E_ORCA_RESOURCE=1 bash config/profiles/orca/tests/e2e/orca-resource.sh
--slot 7 --vm-count 1 --purpose orca-projects --vm 1 -- env SUBYARD_E2E_ORCA_PROJECTS=1 bash config/profiles/orca/tests/e2e/orca-projects.sh
--slot 7 --vm-count 1 --purpose orca-ssh-agent --vm 1 -- env SUBYARD_E2E_ORCA_BOOTSTRAP=1 SUBYARD_E2E_ORCA_SSH_AGENT=1 bash config/profiles/orca/tests/e2e/orca-bootstrap.sh
--slot 7 --vm-count 1 --purpose codex-permissions --vm 1 -- env SUBYARD_E2E_ORCA_BOOTSTRAP=1 SUBYARD_E2E_ORCA_CODEX_PERMISSIONS=1 bash config/profiles/orca/tests/e2e/orca-bootstrap.sh
CALLS
: > "$PROFILE_CONTROLLER_CALLS"
bash "$tmp/$controller" --slot 7
diff -u "$tmp/expected" "$PROFILE_CONTROLLER_CALLS"
: > "$PROFILE_CONTROLLER_CALLS"
bash "$tmp/$controller" --slot 7 --lane full
diff -u "$tmp/expected" "$PROFILE_CONTROLLER_CALLS"

manifest="$ROOT/config/profiles/orca/tests/e2e/acceptance-lanes.json"
jq -e '.schema_version == 1 and [.lanes[].id] ==
  ["bootstrap", "existing-yard", "resource", "projects", "ssh-agent", "codex-permissions"] and
  all(.lanes[]; .arguments == ["--lane", .id])' "$manifest" > /dev/null
jq -r '.lanes[].id' "$manifest" > "$tmp/lanes"
index=0
while IFS= read -r lane; do
  index=$((index + 1))
  : > "$PROFILE_CONTROLLER_CALLS"
  bash "$tmp/$controller" --lane "$lane" --slot 7
  sed -n "${index}p" "$tmp/expected" > "$tmp/expected-lane"
  diff -u "$tmp/expected-lane" "$PROFILE_CONTROLLER_CALLS"
done < "$tmp/lanes"

rc=0
: > "$PROFILE_CONTROLLER_CALLS"
PROFILE_CONTROLLER_EXIT=23 bash "$tmp/$controller" --slot 7 > "$tmp/failure" 2>&1 || rc=$?
[ "$rc" -eq 23 ]
[ "$(wc -l < "$PROFILE_CONTROLLER_CALLS")" -eq 1 ]
rc=0
: > "$PROFILE_CONTROLLER_CALLS"
PROFILE_CONTROLLER_EXIT=23 bash "$tmp/$controller" --slot 7 --lane projects > "$tmp/failure" 2>&1 || rc=$?
[ "$rc" -eq 23 ]
sed -n '4p' "$tmp/expected" > "$tmp/expected-lane"
diff -u "$tmp/expected-lane" "$PROFILE_CONTROLLER_CALLS"
python3 -B - "$ROOT/config/profiles/orca/tests/e2e/orca-projects-helper.py" <<'PY'
import importlib.util
import shlex
import sys

spec = importlib.util.spec_from_file_location("orca_projects_helper", sys.argv[1])
helper = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helper)
for name, argv, accepted in (
    ("selected yard", ["-Y", "fixture", "_project-state", "check-role"], True),
    ("default yard", ["_project-state", "check-role"], True),
    ("extra argument", ["-Y", "fixture", "_project-state", "check-role", "extra"], False),
    ("foreign yard", ["-Y", "foreign", "_project-state", "check-role"], False),
):
    inner = "SUBYARD_OPERATION_ID=fixture:role exec " + shlex.join(["yard", *argv])
    command, environment = helper.parse_yard_command(shlex.join(["bash", "-lc", inner]))
    assert command == argv and environment == {"SUBYARD_OPERATION_ID": "fixture:role"}, name
    assert helper.valid_yard_action(command, "fixture") is accepted, name
print("ok: owner role check preserves exact arity and selected yard")
PY
printf 'ok: complete and independent lane dispatch, argument rejection and failure propagation\n'
