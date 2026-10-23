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
reject_before_broker --slot 7 --native-loss invalid
reject_before_broker --slot 7 --native-loss group
reject_before_broker --slot 7 --lane projects --native-loss helper
reject_before_broker --slot 7 --lane bootstrap --native-loss owner --native-loss group

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

# Focused approval repair does not change the full acceptance manifest/default dispatch.
: > "$PROFILE_CONTROLLER_CALLS"
bash "$tmp/$controller" --lane codex-approvals --slot 7
printf '%s\n' '--slot 7 --vm-count 1 --purpose codex-approvals --vm 1 -- env SUBYARD_E2E_ORCA_BOOTSTRAP=1 SUBYARD_E2E_ORCA_CODEX_PERMISSIONS=1 SUBYARD_E2E_ORCA_CODEX_APPROVALS_ONLY=1 bash config/profiles/orca/tests/e2e/orca-bootstrap.sh' > "$tmp/expected-approvals"
diff -u "$tmp/expected-approvals" "$PROFILE_CONTROLLER_CALLS"

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
mkdir -p "$tmp/tests/helpers"
cat > "$tmp/tests/helpers/profile-native-loss.py" <<'PY'
import os, pathlib, sys
assert sys.argv[1:] == ['--preflight', '--bundle', '/frozen/bundle', '--bundle-sha256', 'a'*64]
pathlib.Path(os.environ['PROFILE_CONTROLLER_CALLS']).with_name('preflight-checked').touch()
PY
: > "$PROFILE_CONTROLLER_CALLS"
SUBYARD_E2E_CANDIDATE_BUNDLE=/frozen/bundle SUBYARD_E2E_CANDIDATE_SHA256="$(printf 'a%.0s' {1..64})" \
  bash "$tmp/$controller" --slot 7 --lane bootstrap --native-loss group
[ -f "$tmp/preflight-checked" ]
printf '%s\n' '--slot 7 --vm-count 1 --purpose orca-bootstrap --vm 1 -- env SUBYARD_E2E_ORCA_BOOTSTRAP=1 SUBYARD_E2E_ORCA_NATIVE_LOSS=group bash config/profiles/orca/tests/e2e/orca-bootstrap.sh' > "$tmp/loss-expected"
diff -u "$tmp/loss-expected" "$PROFILE_CONTROLLER_CALLS"
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
# Native chat journals key prompts by tool-call identity across turns.
python3 -B - "$ROOT/config/profiles/orca/tests/e2e/codex-permissions.py" <<'PYTEST'
import http.server
import importlib.util
import json
from pathlib import Path
import sys
import threading
import urllib.request

spec = importlib.util.spec_from_file_location('provider_fixture', sys.argv[1])
helper = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helper)
provider = http.server.ThreadingHTTPServer(('127.0.0.1', 0), helper.Provider)
provider.workspace, provider.failure = Path('/tmp'), None
thread = threading.Thread(target=provider.serve_forever, daemon=True)
thread.start()
try:
    responses = []
    for _ in range(2):
        provider.command = 'true'
        request = urllib.request.Request(
            'http://127.0.0.1:' + str(provider.server_port) + '/v1/responses',
            data=json.dumps({'tools': [{'name': 'shell'}]}).encode(),
            headers={'Content-Type': 'application/json'})
        with urllib.request.urlopen(request, timeout=5) as response:
            events = [json.loads(line[6:]) for line in response.read().decode().splitlines()
                      if line.startswith('data: ')]
        assert events[0]['response']['id'] == events[-1]['response']['id']
        responses.append((events[0]['response']['id'], events[1]['item']['id'],
                          events[1]['item']['call_id']))
    assert all(responses[0][index] != responses[1][index] for index in range(3)), responses
    assert provider.failure is None
finally:
    provider.shutdown()
    provider.server_close()
    thread.join(timeout=5)
print('ok: synthetic native-chat responses keep distinct response/tool/call identities')
PYTEST
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
# Reach the lease check independently of the host-free runner's username.
mkdir "$tmp/bin"
cat > "$tmp/bin/id" <<'SH'
#!/usr/bin/env bash
[ "$#" -eq 1 ] && [ "$1" = -un ] || exit 2
printf 'dev\n'
SH
chmod 0755 "$tmp/bin/id"
rc=0
env PATH="$tmp/bin:$PATH" SUBYARD_E2E_ORCA_PROJECTS=1 SUBYARD_E2E_ORCA_LOAD_DIAGNOSTIC=1 \
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
# Execute the public unlock PTY driver with synthetic stalled and stopped children.
python3 -B - "$ROOT/config/profiles/orca/tests/e2e/ssh-agent.sh" <<'PY'
import ast
import json
from pathlib import Path
import subprocess
import sys
import tempfile

source = Path(sys.argv[1]).read_text().split("<<'PY'\n", 1)[1].split('\nPY\n', 1)[0]

class HostFreeDeadline(ast.NodeTransformer):
    def visit_Assign(self, node):
        # Shorten only this test's waits; native group enrollment is not under test.
        names = {target.id for target in node.targets if isinstance(target, ast.Name)}
        if names & {'deadline', 'cleanup_deadline'}:
            node.value.right = ast.Constant(0.7 if 'deadline' in names else 0.25)
        elif 'group' in names:
            node.value = ast.parse('os.getgid()', mode='eval').body
        return node

driver = ast.unparse(ast.fix_missing_locations(HostFreeDeadline().visit(ast.parse(source))))
with tempfile.TemporaryDirectory(prefix='subyard-orca-pty-', dir='/tmp') as temporary:
    root = Path(temporary)
    for scenario in ('stalled', 'stopped'):
        child = root / scenario
        child.write_text('''#!/usr/bin/env python3
import os
import signal
import subprocess
import time
print('Operation plan: fixture', flush=True)
print('  [apply] ssh-agent.grant', flush=True)
print('yard: PRIVATE-TRANSCRIPT-SENTINEL', flush=True)
worker = None
def stop(signum, frame):
    if worker is not None:
        worker.terminate()
        worker.wait()
    raise SystemExit(0)
signal.signal(signal.SIGTERM, stop)
if os.path.basename(__file__) == 'stopped':
    os.kill(os.getpid(), signal.SIGSTOP)
else:
    worker = subprocess.Popen(['/bin/sleep', '60'])
time.sleep(60)
''')
        child.chmod(0o700)
        result = subprocess.run([sys.executable, '-c', driver, str(child),
                                 'PRIVATE-KEY-PATH-SENTINEL', '2m', 'PRIVATE-PASSPHRASE-SENTINEL'],
                                capture_output=True, text=True, timeout=5)
        assert result.returncode == 1, (scenario, result.returncode)
        assert 'public terminal unlock timed out; passphrase prompts=0' in result.stderr
        assert 'PRIVATE-' not in result.stdout + result.stderr
        prefix = 'ssh-agent-e2e: wait='
        records = [json.loads(line[len(prefix):]) for line in result.stderr.splitlines()
                   if line.startswith(prefix)]
        assert len(records) == 1, (scenario, result.stderr)
        record = records[0]
        assert record['progress'] == dict(operation_plan=True, grant_step=True,
                                         passphrase_sent=False, native_rejection=False), record
        process = record['processes'][0]
        assert process == dict(role='child', state='T' if scenario == 'stopped' else 'S',
                               executable='python3'), record
        assert not record['process_limit'], record
        if scenario == 'stalled':
            assert dict(role='descendant', state='S', executable='sleep') in record['processes'], record
print('ok: Orca unlock timeout preserves bounded safe progress and process states')
PY
printf 'ok: complete and independent lane dispatch, argument rejection and failure propagation\n'
