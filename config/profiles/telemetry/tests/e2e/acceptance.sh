#!/usr/bin/env bash
# One fresh disposable android-test stage; no operator deployment is used.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
if [ "${1:-}" = --preflight ] && [ "$#" = 1 ]; then
  for script in "$ROOT/config/profiles/telemetry/tests/e2e/"*.sh "$ROOT/dev/e2e/broker-memory-boundary.sh"; do
    bash -n "$script"
  done
  shellcheck -S warning "$ROOT/config/profiles/telemetry/tests/e2e/"*.sh "$ROOT/dev/e2e/broker-memory-boundary.sh"
  python3 -B - "$ROOT/config/profiles/telemetry/tests/e2e" <<'PY'
import ast, json, os, pathlib, sqlite3, subprocess, sys, tempfile, time
from unittest import mock
owner = (pathlib.Path(sys.argv[1])/'owner.sh').read_text()
compile(owner.split("<<'PY_UNITS'\n", 1)[1].split('\nPY_UNITS', 1)[0], 'owner.sh:OTLP-unit-probe', 'exec')
# Execute only the real closed mode-dispatch blocks, with fixture operations
# replaced by assertions. No lease, installation or workload is launched.
held = 'if [ "$mode" = --held-diagnostic ]; then\n' + owner.split('if [ "$mode" = --held-diagnostic ]; then\n', 1)[1].split('\nfi', 1)[0] + '\nfi'
assert owner.index(held) < owner.index('python3 -B config/profiles/telemetry/tests/e2e/native-signals.py')
probe = '''set -eu
mode=--held-diagnostic; ROOT=/fixture; state=/fixture-state
subyard-telemetry-run() { [ "$*" = '--agent other --agent-version fixture --scope-mode require --project-id telemetry-fixture -- bash dev/e2e/broker-memory-boundary.sh --telemetry-held-diagnostic' ]; printf 'managed\\n'; }
wait_for_finals() { printf 'finals\\n'; }
sudo() { [ "$*" = "-n python3 -B /fixture-state/source/guest.py apply 0 dev " ]; printf 'disabled\\n'; }
test() { [ "$*" = '-f /var/lib/subyard/telemetry/telemetry.sqlite' ]; printf 'history\\n'; }
'''
result = subprocess.run(['bash', '-c', probe+held], text=True, capture_output=True, check=True)
assert result.stdout.splitlines() == ['managed', 'finals', 'disabled', 'history', 'ok: telemetry held-pair diagnostic, final counters and retained history']
broker = (pathlib.Path(sys.argv[1]).parents[4]/'dev/e2e/broker-memory-boundary.sh').read_text()
dispatch = broker.split('TELEMETRY_ONLY=0\n',1)[1].split('\nTOKEN=',1)[0]
for flag, expected in [('--telemetry-resources','1 0 --telemetry-resources'), ('--telemetry-held-diagnostic','1 1 --telemetry-held-diagnostic')]:
    script = 'TELEMETRY_ONLY=0; SUBYARD_E2E_RESOURCE_OBSERVER=fixture; SUBYARD_AGENT_RUN_ID=fixture; SUBYARD_TELEMETRY_DATA_DIR=/fixture\n'+dispatch+'\nprintf "%s %s %s\\n" "$TELEMETRY_ONLY" "$HELD_DIAGNOSTIC" "$TELEMETRY_FLAG"'
    result = subprocess.run(['bash','-eu','-c',script,'probe',flag], text=True, capture_output=True, check=True)
    assert result.stdout.strip() == expected
assert '"$TELEMETRY_FLAG" "$TOKEN"' in broker
conditions = [line for line in broker.splitlines() if line.startswith('if [ "$HELD_DIAGNOSTIC" = 0 ]')]
assert len(conditions) == 2
for condition in conditions:
    result = subprocess.run(['bash','-eu','-c',
        'HELD_DIAGNOSTIC=1; SUBYARD_E2E_NATIVE_EXERCISE=fixture; SUBYARD_E2E_RESOURCE_OBSERVER=fixture\n'+condition+'\nexit 1\nfi'], check=False)
    assert result.returncode == 0, 'held diagnostic must skip native clients and storage faults'
assert broker.index('if [ "$HELD_DIAGNOSTIC" = 1 ]; then\n  printf') < broker.index('HEARTBEAT_BEFORE="$(held_heartbeat)"')
assert 'stop_holder_child "$HOLDER_PID"' in broker and '--kind vm --stage final' in broker
print('ok: held diagnostic selects the managed pair only, drains finals and disables with history retained; no workloads launched')
android_mode = 'android_only=0\n'+owner.split('android_only=0\n',1)[1].split('\n',1)[0]+'\n'
vm = 'if [ "$android_only" = 0 ]; then\n'+owner.split('if [ "$android_only" = 0 ]; then\n', 1)[1].split('\nfi', 1)[0]+'\nfi'
android = owner.split(vm, 1)[1].split('\nwait_for_finals', 1)[0]
for mode, expected in (('--android', ['android']), ('--android-resources', ['android']), ('all', ['vm','android'])):
    probe = '''set -eu
mode="$1"
subyard-telemetry-run() {
  case "$*" in
    *'bash dev/e2e/broker-memory-boundary.sh --telemetry-resources') printf 'vm\\n' ;;
    *'bash config/profiles/android/tests/e2e/android-pool-runtime.sh --lane memory-lease') printf 'android\\n' ;;
    *) exit 2 ;;
  esac
}
'''
    result = subprocess.run(['bash','-c',probe+android_mode+vm+android,'probe',mode], text=True, capture_output=True, check=True)
    assert result.stdout.splitlines() == expected
for command, calls in (('storage-failure.py --binary', 1), ('measure.py --stack --assets', 2)):
    position = owner.rindex('python3 -B config/profiles/telemetry/tests/e2e/'+command)
    start = owner.rfind('\nif ', 0, position)+1
    block = owner[start:owner.index('\nfi', position)+3]
    for mode, expected in (('--android', 0), ('--android-resources', 0), ('all', calls)):
        result = subprocess.run(['bash','-c', 'set -eu; mode="$1"; asset=fixture; state=fixture\npython3() { printf "called\\n"; }\n'+android_mode+block,'probe',mode], text=True, capture_output=True, check=True)
        assert len(result.stdout.splitlines()) == expected
profile = 'if [ "$mode" != --android-resources ]; then\n'+owner.split('if [ "$mode" != --android-resources ]; then\n',1)[1].split('\nfi',1)[0]+'\nfi'
for mode in ('all','--android','--android-resources'):
    result = subprocess.run(['bash','-c','set -eu; mode="$1"; ROOT=/fixture; SUBYARD_E2E_PROFILE_EXERCISE=inherited\n'+profile+'\nprintf "%s\\n" "${SUBYARD_E2E_PROFILE_EXERCISE-unset}"','probe',mode],text=True,capture_output=True,check=True)
    if mode == '--android-resources':
        assert result.stdout.splitlines() == ['scope: Android resources continuation; supported profile lifecycle requires separately reviewed evidence','unset']
    else:
        assert result.stdout.strip() == '/fixture/config/profiles/telemetry/tests/e2e/profile-lifecycle.py'
print('ok: both Android-only modes skip local measurements and VM broker; only resources continuation omits profile callback; default keeps full dispatch')
# Run the actual example selection and assertions against tiny SQLite fixtures.
examples = owner.split('python3 -B - "$asset" "$mode" <<\'PY\'\n', 1)[1].split('\nPY', 1)[0]
selection = examples.split('    hashes = []', 1)[0]
for mode, pairs, accepted in (
    ('--android', [('codex','android'),('claude','android')], True),
    ('--android', [('codex','vm'),('claude','vm')], False),
    ('--android', [('codex','android'),('codex','android')], False),
    ('--android-resources', [('codex','android'),('claude','android')], True),
    ('--android-resources', [('codex','vm'),('claude','vm')], False),
    ('--android-resources', [('codex','android'),('codex','android')], False),
    ('all', [('codex','android'),('claude','android')], False),
    ('all', [(agent, kind) for agent in ('codex','claude') for kind in ('vm','android')], True)):
    db = sqlite3.connect(':memory:')
    db.executescript('CREATE TABLE runs(agent_run_id TEXT,body TEXT); CREATE TABLE relations(agent_run_id TEXT); CREATE TABLE associations(agent_run_id TEXT,object_id TEXT); CREATE TABLE allocations(object_id TEXT,body TEXT);')
    for index, (agent, kind) in enumerate(pairs):
        run, allocation = 'run-'+str(index), 'allocation-'+str(index)
        db.execute('INSERT INTO runs VALUES(?,?)', (run, json.dumps(dict(agent=agent,project_id='telemetry-fixture'))))
        db.execute('INSERT INTO relations VALUES(?)', (run,))
        db.execute('INSERT INTO associations VALUES(?,?)', (run,allocation))
        db.execute('INSERT INTO allocations VALUES(?,?)', (allocation,json.dumps(dict(object_kind=kind))))
    db.commit()
    previous = os.getcwd()
    try:
        with tempfile.TemporaryDirectory(dir='/tmp') as directory, mock.patch.object(sqlite3, 'connect', return_value=db), \
             mock.patch.object(sys, 'argv', ['examples', 'fixture', mode]), mock.patch.object(time, 'monotonic', side_effect=[0,100]):
            os.chdir(directory)
            try:
                exec(compile(selection, 'owner.sh:example-selection', 'exec'), {})
            except AssertionError:
                assert not accepted, 'valid native example selection was rejected'
            else:
                assert accepted, 'incomplete native example selection was accepted'
    finally:
        os.chdir(previous)
        db.close()
print('ok: both Android-only modes require actual Codex and Claude Android rows; full examples require both resource kinds')
for path in pathlib.Path(sys.argv[1]).glob('*.py'):
    tree = ast.parse(path.read_text(), filename=str(path))
    if path.name == 'process-scope.py':
        outer_count = child_count = 0
        commands = [ast.literal_eval(node.value) for node in ast.walk(tree) if isinstance(node, ast.Assign)
                    and any(isinstance(target, ast.Name) and target.id == 'code' for target in node.targets)]
        for code in commands:
            compile(code, str(path)+':outer-command', 'exec')
        for code in commands:
            outer = ast.parse(code)
            literals = {target.id: item.value.value for item in ast.walk(outer) if isinstance(item, ast.Assign)
                        and isinstance(item.value, ast.Constant) and isinstance(item.value.value, str)
                        for target in item.targets if isinstance(target, ast.Name)}
            for call in ast.walk(outer):
                if not isinstance(call, ast.Call) or not isinstance(call.func, ast.Attribute) or call.func.attr not in ('Popen', 'check_output', 'run'):
                    continue
                arguments = call.args[0].elts
                index = next(index for index, item in enumerate(arguments) if isinstance(item, ast.Constant) and item.value == '-c')
                child = arguments[index+1]
                compile(literals[child.id] if isinstance(child, ast.Name) else ast.literal_eval(child), str(path)+':child-command', 'exec')
                child_count += 1
            outer_count += 1
        assert outer_count == child_count == 3, 'all three process scope commands and their children must compile'
        print('ok: compiled all three scope commands and their nested child commands; no workloads launched')
PY
  python3 -B "$ROOT/config/profiles/telemetry/tests/e2e/storage-failure.py" --preflight
  printf 'ok: telemetry acceptance syntax; checksum-frozen candidate assets and native prerequisites checked on disposable stage\n'
  exit 0
fi
mode=all
case "${1:-}" in --held-diagnostic|--resources|--local|--native-only|--android|--android-resources) mode="$1"; shift ;; esac
[ "$#" = 2 ] && [ "$1" = --slot ] && [[ "$2" =~ ^[1-9][0-9]*$ ]] || {
  printf 'Usage: config/profiles/telemetry/tests/e2e/acceptance.sh [--held-diagnostic | --resources | --local | --native-only | --android | --android-resources] --slot N | --preflight\n' >&2; exit 2;
}
[ -f "${SUBYARD_E2E_CANDIDATE_BUNDLE:-}" ] && [[ "${SUBYARD_E2E_CANDIDATE_SHA256:-}" =~ ^[a-f0-9]{64}$ ]] || {
  printf 'telemetry acceptance requires a frozen candidate bundle and SHA256\n' >&2; exit 2;
}
fixture_timeout=70m
[ "$mode" != --held-diagnostic ] || fixture_timeout=20m
[ "$mode" != --native-only ] || fixture_timeout=10m
bash "$ROOT/dev/agent-e2e.sh" --slot "$2" --type android-test --vm-count 1 --purpose telemetry-acceptance --vm 1 -- \
  timeout --signal=TERM --kill-after=120s "$fixture_timeout" bash config/profiles/telemetry/tests/e2e/owner.sh "$mode"
