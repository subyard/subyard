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
diagnostic=config/profiles/orca/tests/e2e/load-diagnostic.sh
cp "$ROOT/$diagnostic" "$tmp/$diagnostic"
: > "$PROFILE_CONTROLLER_CALLS"
bash "$tmp/$diagnostic" --help > "$tmp/help"
[ ! -s "$PROFILE_CONTROLLER_CALLS" ]
for roots in 1000 1400; do
  : > "$PROFILE_CONTROLLER_CALLS"
  bash "$tmp/$diagnostic" --slot 7 --roots "$roots"
  printf '%s\n' "--slot 7 --vm-count 1 --purpose orca-load-diagnostic --vm 1 -- env SUBYARD_E2E_ORCA_PROJECTS=1 SUBYARD_E2E_ORCA_LOAD_DIAGNOSTIC=1 SUBYARD_E2E_ORCA_LOAD_ROOTS=$roots SUBYARD_E2E_ORCA_LOAD_LANE=load timeout --signal=TERM --kill-after=30s 45m bash config/profiles/orca/tests/e2e/orca-projects.sh" > "$tmp/expected-load"
  diff -u "$tmp/expected-load" "$PROFILE_CONTROLLER_CALLS"
done
: > "$PROFILE_CONTROLLER_CALLS"
bash "$tmp/$diagnostic" --slot 7 --roots 1400 --lane cleanup
sed 's/SUBYARD_E2E_ORCA_LOAD_LANE=load/SUBYARD_E2E_ORCA_LOAD_LANE=cleanup/' "$tmp/expected-load" > "$tmp/expected-cleanup"
diff -u "$tmp/expected-cleanup" "$PROFILE_CONTROLLER_CALLS"
controller="$diagnostic"
reject_before_broker
reject_before_broker --slot 0
reject_before_broker --slot 7 --roots 999
reject_before_broker --slot 7 --roots
reject_before_broker --slot 7 --unknown
reject_before_broker --slot 7 --lane invalid
reject_before_broker --slot 7 --lane
# Environment flags cannot authorize local fixture creation without a matching lease.
rc=0
env SUBYARD_E2E_ORCA_PROJECTS=1 SUBYARD_E2E_ORCA_LOAD_DIAGNOSTIC=1 \
  SUBYARD_E2E_VM=1 SUBYARD_E2E_RUN_ID=invalid SUBYARD_E2E_SLOT=invalid \
  bash "$ROOT/config/profiles/orca/tests/e2e/orca-projects.sh" > "$tmp/local-guard" 2>&1 || rc=$?
[ "$rc" -eq 1 ]
grep -Fq 'load diagnostic requires the matching disposable VM lease' "$tmp/local-guard"
# Exercise bounded convergence without creating repositories or contacting a runtime.
sed -n '/^load_sync() {$/,/^}$/p' "$ROOT/config/profiles/orca/tests/e2e/load-discovery.sh" > "$tmp/load-sync-function"
cat > "$tmp/load-sync-check" <<'SH'
set -euo pipefail
die() { printf '%s\n' "$*" >&2; exit 1; }
stage() { printf '%s\n' "$*" >&2; }
sleep() { :; }
guest_dev() {
  local count=0
  [ ! -f "$CHECK_DIR/progress" ] || count="$(<"$CHECK_DIR/progress")"
  printf '%s' "$((count + 1))" > "$CHECK_DIR/progress"
  if [ "$count" -eq 0 ] || [ "$CHECK_CASE" = no-progress ]; then printf '4\n'; else printf '0\n'; fi
}
yard() {
  local count=0
  [ ! -f "$CHECK_DIR/calls" ] || count="$(<"$CHECK_DIR/calls")"
  printf '%s' "$((count + 1))" > "$CHECK_DIR/calls"
  [ "$count" -eq 0 ] || return 0
  case "$CHECK_CASE" in
    lock) printf '%s\n' 'Orca registration error: Another Orca registration invocation holds the lock' ;;
    *) printf '%s\n' 'Orca registration error: Orca registration time budget exhausted' ;;
  esac
  if [ "$CHECK_CASE" = mixed ]; then
    printf '%s\n' 'Orca registration error: private fixture: Orca runtime request timed out: repo.rm (5.0s)'
  fi
  return 1
}
load_root=/unused
. "$CHECK_FUNCTION"
load_sync
SH
for check_case in progress lock no-progress mixed; do
  mkdir "$tmp/$check_case"
  rc=0
  ROOT="$ROOT" CHECK_CASE="$check_case" CHECK_DIR="$tmp/$check_case" \
    CHECK_FUNCTION="$tmp/load-sync-function" bash "$tmp/load-sync-check" \
    > "$tmp/$check_case/output" 2>&1 || rc=$?
  case "$check_case" in
    progress|lock) [ "$rc" -eq 0 ] && [ "$(<"$tmp/$check_case/calls")" -eq 2 ] ;;
    no-progress|mixed) [ "$rc" -eq 1 ] && [ "$(<"$tmp/$check_case/calls")" -eq 1 ] ;;
  esac
  ! grep -Fq 'private fixture' "$tmp/$check_case/output"
done
printf 'ok: complete and independent lane dispatch, argument rejection and failure propagation\n'
