#!/usr/bin/env bash
# Install the exact candidate only inside the lease-guarded disposable stage.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
[ "${SUBYARD_E2E_VM:-}" = 1 ] && sudo -n test -f /run/subyard-e2e-lease.json || {
  printf 'telemetry owner fixture requires a leased disposable stage\n' >&2; exit 2;
}
cd "$ROOT"
delegated=0
if [ "${1:-}" = --delegated-fixture ]; then delegated=1; shift; fi
mode="${1:-all}"
[ "$#" -le 1 ] && { [ "$mode" = all ] || [ "$mode" = --held-diagnostic ] || [ "$mode" = --scope-diagnostic ] || [ "$mode" = --resources ] || [ "$mode" = --local ] || [ "$mode" = --native-only ] || [ "$mode" = --android ] || [ "$mode" = --android-resources ]; } || {
  printf 'Usage: owner.sh [--held-diagnostic | --scope-diagnostic | --resources | --local | --native-only | --android | --android-resources]\n' >&2; exit 2;
}
android_only=0
case "$mode" in --android|--android-resources) android_only=1 ;; esac
if [ "$delegated" = 0 ]; then
  # Start a new bounded fixture before its workloads exist. Moving an SSH
  # workload to an unrelated delegation would escape ssh.service's pids budget.
  case "${SUBYARD_E2E_MEMORY_PER_VM:-}" in
    4GiB) fixture_memory=3G ;;
    8GiB) fixture_memory=6G ;;
    *) printf 'unsupported telemetry fixture memory grant\n' >&2; exit 2 ;;
  esac
  [[ "${SUBYARD_E2E_RUN_ID:-}" =~ ^[a-f0-9]{8}$ ]] || exit 2
  fixture_seconds=4200
  [ "$mode" != all ] || fixture_seconds=7200
  [ "$mode" != --held-diagnostic ] || fixture_seconds=1140
  [ "$mode" != --scope-diagnostic ] || fixture_seconds=240
  [ "$mode" != --native-only ] || fixture_seconds=600
  fixture_env=(env -i HOME=/home/dev USER=dev LOGNAME=dev LANG=C.UTF-8 TMPDIR=/tmp
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin)
  for name in YARD PROJECT RUN_ID PURPOSE SLOT GENERATION TYPE VM_COUNT CPU_PER_VM MEMORY_PER_VM DISK_PER_VM BASE_FINGERPRINT VM; do
    variable="SUBYARD_E2E_$name"
    fixture_env+=("$variable=${!variable}")
  done
  exec sudo -n systemd-run --quiet --unit="subyard-telemetry-acceptance-$SUBYARD_E2E_RUN_ID.service" \
    --uid=dev --service-type=exec --property='Delegate=cpu memory io pids' --property=DelegateSubgroup=caller \
    --property=TasksMax=1024 --property="MemoryMax=$fixture_memory" --property=ExitType=cgroup \
    --property="RuntimeMaxSec=$fixture_seconds" --wait --pipe --collect --expand-environment=no \
    --working-directory="$ROOT" "${fixture_env[@]}" bash config/profiles/telemetry/tests/e2e/owner.sh --delegated-fixture "$mode"
fi
for command in python3 curl jq tar sha256sum systemctl incus go; do command -v "$command" >/dev/null; done
state="$(mktemp -d /tmp/subyard-telemetry-acceptance.XXXXXX)"
chmod 0700 "$state"
trap 'printf "telemetry disposable evidence: %s\n" "$state"' EXIT
# shellcheck source=config/profiles/telemetry/tests/e2e/assets.sh
. "$ROOT/config/profiles/telemetry/tests/e2e/assets.sh"
fixture_assets="$state/assets"
telemetry_prepare_assets "$ROOT" "$fixture_assets"
export SUBYARD_TELEMETRY_FIXTURE_ASSETS="$fixture_assets"
asset="$fixture_assets/subyard-telemetry"
mkdir "$state/source"
cp config/profiles/telemetry/*.py "$state/source/"
cp internal/adapters/configmaterial/tomli_w.py "$state/source/"
cp "$asset" "$state/source/subyard-telemetry"
chmod 0755 "$state/source/subyard-telemetry"
if [ "$mode" != --held-diagnostic ] && [ "$mode" != --resources ] && [ "$mode" != --native-only ] && [ "$android_only" = 0 ]; then
  python3 -B config/profiles/telemetry/tests/e2e/storage-failure.py --binary "$asset" --evidence "$state/storage-failure"
fi
# Native signal homes are separate below; no real authorization is read.
sudo -n python3 -B "$state/source/guest.py" apply 1 dev ''
timeout 30 bash -c 'until test -S /var/lib/subyard/telemetry/control.sock && test -f /var/lib/subyard/telemetry/delegation.json; do sleep 0.2; done'
python3 -B - /var/lib/subyard/telemetry http://127.0.0.1:14318 <<'PY_UNITS'
import json, pathlib, sqlite3, sys, time, urllib.error, urllib.request
data = pathlib.Path(sys.argv[1])
metrics = [dict(name=name, unit=unit, gauge=dict(dataPoints=[dict(asInt='1')]))
           for name, unit in (('fixture.unit.percent', '%'), ('fixture.unit.tokens', '{token}'))]
request = urllib.request.Request(sys.argv[2]+'/v1/metrics',
    data=json.dumps(dict(resourceMetrics=[dict(scopeMetrics=[dict(metrics=metrics)])])).encode(),
    headers={'Content-Type': 'application/json', 'Authorization': 'Bearer '+(data/'auth.token').read_text().strip()})
deadline = time.monotonic()+10
while True:
    try:
        with urllib.request.urlopen(request, timeout=5) as response:
            assert response.status == 200
            response.read()
        break
    except urllib.error.URLError as error:
        if isinstance(error, urllib.error.HTTPError) or time.monotonic() >= deadline:
            raise
        time.sleep(0.1)
with sqlite3.connect((data/'telemetry.sqlite').as_uri()+'?mode=ro', uri=True, timeout=2) as db:
    db.execute('PRAGMA query_only=ON')
    row = db.execute("SELECT protobuf FROM records WHERE encoding='otlp/metrics' ORDER BY sequence DESC LIMIT 1").fetchone()
assert row and all(value in row[0] for value in (b'fixture.unit.percent', b'fixture.unit.tokens', b'%', b'{token}'))
print(json.dumps(dict(schema='subyard.telemetry.unit-acceptance/v1', percent_retained=True,
                      token_retained=True, sqlite_commit=True)), flush=True)
PY_UNITS
finals_diagnostic() {
  timeout --signal=TERM --kill-after=1 5 python3 -B - "$ROOT/config/profiles/telemetry" /var/lib/subyard/telemetry <<'PY_FINALS_DIAGNOSTIC' || true
import contextlib, hashlib, json, os, pathlib, sqlite3, stat, sys
sys.path.insert(0, sys.argv[1])
import records

def diagnose(data, cgroups=pathlib.Path('/sys/fs/cgroup'), proc=pathlib.Path('/proc')):
    def text(path, limit=8192):
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(fd, 'rb') as stream:
            return stream.read(limit).decode('utf-8', errors='replace')
    def process(pid):
        if type(pid) is not int or not 1 < pid < 2**31:
            raise ValueError('invalid process identity')
        value = text(proc/str(pid)/'stat', 4096)
        name, fields = value.partition('(')[2].rsplit(')', 1)
        fields = fields.split()
        kind = next((label for label, names in (
            ('ssh', ('ssh',)), ('shell', ('bash', 'sh', 'dash')), ('python', ('python', 'python3')),
            ('incus', ('incus',)), ('yard', ('yard', 'yard-engine')), ('sleep', ('sleep',))) if name in names), 'other')
        return dict(pid=pid, start_ticks=int(fields[19]), state=fields[0] if fields[0] in 'RSDZTWXIPt' else 'other', comm_class=kind)
    result = dict(schema='subyard.telemetry.finals-diagnostic/v1', runs=[], outbox_count=0,
                  outbox_bytes=0, runs_truncated=False, outbox_truncated=False)
    for index, path in enumerate((data/'outbox').glob('*.json')):
        if index >= 4096:
            result['outbox_truncated'] = True
            break
        result['outbox_count'] += 1
        try:
            info = path.lstat()
            if stat.S_ISREG(info.st_mode):
                result['outbox_bytes'] += info.st_size
        except FileNotFoundError:
            pass
    for index, path in enumerate((data/'runs').glob('*.json')):
        if index >= 16:
            result['runs_truncated'] = True
            break
        item = dict(run_ref=hashlib.sha256(path.name.encode()).hexdigest()[:16], manifest='invalid')
        result['runs'].append(item)
        try:
            run = records.read_private(path)
            if not isinstance(run, dict) or run.get('agent_run_id') != path.stem or not records.IDENTITY.fullmatch(path.stem):
                continue
            item['manifest'] = 'reserved' if run.get('manifest_state') == 'reserved' else 'ready'
            try:
                identity = process(run['pid'] if item['manifest'] == 'ready' else run['launcher_pid'])
                item['main_identity_present'] = identity['start_ticks'] == run.get('start_ticks', run.get('launcher_start_ticks'))
                if item['main_identity_present']:
                    item['main_process'] = identity
            except (OSError, ValueError, KeyError, IndexError):
                item['main_identity_present'] = False
            try:
                with contextlib.closing(sqlite3.connect((data/'telemetry.sqlite').as_uri()+'?mode=ro', uri=True, timeout=.2)) as db:
                    row = db.execute('SELECT body FROM runs WHERE agent_run_id=?', (path.stem,)).fetchone()
                body = json.loads(row[0]) if row else {}
                state = body.get('state')
                item['run_state'] = state if state in ('starting', 'resources_observing', 'main_exited', 'resources_complete', 'resources_unknown', 'not_started') else 'other'
                for field in ('main_exited', 'agent_exited', 'resource_completed'):
                    item[field] = body.get(field) if type(body.get(field)) is bool else None
            except (OSError, ValueError, TypeError, sqlite3.Error):
                item['run_state'] = 'unavailable'
            if not run.get('cgroup'):
                item['scope'] = 'partial'
                continue
            group = pathlib.Path(run['cgroup'])
            if not group.is_relative_to(cgroups) or '..' in group.parts or group.name != 'run-'+path.stem:
                item['scope'] = 'identity_mismatch'
                continue
            info = group.lstat()
            if (not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid()
                    or [info.st_dev, info.st_ino] != run.get('cgroup_identity')):
                item['scope'] = 'identity_mismatch'
                continue
            item['scope'] = 'identified'
            events = dict(line.split(None, 1) for line in text(group/'cgroup.events').splitlines())
            item['populated'] = {'0': False, '1': True}.get(events.get('populated'))
            members = []
            for leaf in (group, group/'workload'):
                members += [int(value) for value in text(leaf/'cgroup.procs').split()[:17] if value.isdecimal()]
            item['processes_truncated'] = len(members) > 16
            item['processes'] = []
            for pid in sorted(set(members))[:16]:
                try:
                    item['processes'].append(process(pid))
                except (OSError, ValueError, IndexError):
                    item['processes'].append(dict(pid=pid, identity='unavailable'))
        except (OSError, ValueError, KeyError, TypeError, IndexError) as error:
            item['error_class'] = type(error).__name__ if type(error).__module__ in ('builtins', 'json.decoder') else 'Error'
    return result

if __name__ == '__main__':
    try:
        print(json.dumps(diagnose(pathlib.Path(sys.argv[2])), sort_keys=True), flush=True)
    except (OSError, ValueError, TypeError):
        print('{"schema":"subyard.telemetry.finals-diagnostic/v1","diagnostic":"unavailable"}', flush=True)
PY_FINALS_DIAGNOSTIC
}
wait_for_finals() {
  # The fixture controller must outlive the outside observer's durable final.
  local original=0
  timeout 45 bash -c 'while compgen -G "/var/lib/subyard/telemetry/runs/*.json" >/dev/null || compgen -G "/var/lib/subyard/telemetry/outbox/*.json" >/dev/null; do sleep 0.2; done' || original=$?
  printf '{"schema":"subyard.telemetry.fixture-stage/v1","stage":"finals-wait","exit_code":%s}\n' "$original" >&2
  if [ "$original" != 0 ]; then finals_diagnostic || true; fi
  return "$original"
}
if [ "$mode" = --scope-diagnostic ]; then
  python3 -B config/profiles/telemetry/tests/e2e/process-scope.py --nested-only --state "$state" --data-dir /var/lib/subyard/telemetry
  python3 -B config/profiles/telemetry/tests/e2e/measure.py --stack --assets "$fixture_assets" --output "$state/overhead" --idle 1 --load 1
  wait_for_finals
  exit
fi
if [ "$mode" = --held-diagnostic ]; then
  export SUBYARD_E2E_RESOURCE_OBSERVER="$ROOT/config/profiles/telemetry/tests/e2e/resource-observer.py"
  managed_exit=0
  subyard-telemetry-run --agent other --agent-version fixture --scope-mode require --project-id telemetry-fixture -- \
    bash dev/e2e/broker-memory-boundary.sh --telemetry-held-diagnostic || managed_exit=$?
  printf '{"schema":"subyard.telemetry.fixture-stage/v1","stage":"managed-held","exit_code":%s}\n' "$managed_exit" >&2
  if [ "$managed_exit" != 0 ]; then
    finals_diagnostic || true
    exit "$managed_exit"
  fi
  wait_for_finals
  sudo -n python3 -B "$state/source/guest.py" apply 0 dev ''
  test -f /var/lib/subyard/telemetry/telemetry.sqlite
  printf 'ok: telemetry held-pair diagnostic, final counters and retained history\n'
  exit
fi
if [ "$mode" = --resources ]; then
  python3 -B config/profiles/telemetry/tests/e2e/process-scope.py --nested-only --different-gid --state "$state" --data-dir /var/lib/subyard/telemetry
fi
if [ "$mode" != --resources ] && [ "$mode" != --native-only ] && [ "$android_only" = 0 ]; then
  python3 -B config/profiles/telemetry/tests/e2e/measure.py --stack --assets "$fixture_assets" --output "$state/overhead" --idle 60 --load 90
  python3 -B config/profiles/telemetry/tests/e2e/process-scope.py --state "$state" --data-dir /var/lib/subyard/telemetry
fi
python3 -B config/profiles/telemetry/tests/e2e/native-signals.py --state "$state" --data-dir /var/lib/subyard/telemetry --source "$ROOT"
if [ "$mode" = --native-only ]; then
  wait_for_finals
  sudo -n python3 -B "$state/source/guest.py" apply 0 dev ''
  test -f /var/lib/subyard/telemetry/telemetry.sqlite
  printf 'ok: telemetry native signals and command diagnostic only\n'
  exit
fi
if [ "$mode" = --local ]; then
  wait_for_finals
  sudo -n python3 -B "$state/source/guest.py" apply 0 dev ''
  test -f /var/lib/subyard/telemetry/telemetry.sqlite
  printf 'ok: telemetry local storage, OTLP units, stack overhead, process scopes and native signals\n'
  exit
fi
export SUBYARD_E2E_RESOURCE_OBSERVER="$ROOT/config/profiles/telemetry/tests/e2e/resource-observer.py"
export SUBYARD_E2E_NATIVE_EXERCISE="$ROOT/config/profiles/telemetry/tests/e2e/native-signals.py"
export SUBYARD_TELEMETRY_NATIVE_STATE="$state/native"
if [ "$mode" != --android-resources ]; then
  export SUBYARD_E2E_PROFILE_EXERCISE="$ROOT/config/profiles/telemetry/tests/e2e/profile-lifecycle.py"
else
  unset SUBYARD_E2E_PROFILE_EXERCISE
  printf 'scope: Android resources continuation; supported profile lifecycle requires separately reviewed evidence\n'
fi
if [ "$android_only" = 0 ]; then
  subyard-telemetry-run --agent other --agent-version fixture --scope-mode require --project-id telemetry-fixture -- \
    bash dev/e2e/broker-memory-boundary.sh --telemetry-resources
fi
subyard-telemetry-run --agent other --agent-version fixture --scope-mode require --project-id telemetry-fixture -- \
  bash config/profiles/android/tests/e2e/android-pool-runtime.sh --lane memory-lease
wait_for_finals
python3 -B - "$asset" "$mode" <<'PY'
import hashlib, json, os, pathlib, sqlite3, subprocess, sys, time
data = pathlib.Path('/var/lib/subyard/telemetry')
kinds = {'android'} if sys.argv[2] in ('--android', '--android-resources') else {'vm', 'android'}
expected = {(agent, kind) for agent in ('codex', 'claude') for kind in kinds}
output = pathlib.Path('.build/agent-telemetry/physical-examples')
output.mkdir(mode=0o700, parents=True, exist_ok=False)
os.chmod(output, 0o700)
deadline = time.monotonic()+40
with sqlite3.connect('file:'+str(data/'telemetry.sqlite')+'?mode=ro', uri=True) as db:
    while True:
        rows = db.execute('''SELECT DISTINCT runs.agent_run_id, json_extract(runs.body,'$.agent'),
              json_extract(allocations.body,'$.object_kind') FROM runs
            JOIN relations USING(agent_run_id) JOIN associations USING(agent_run_id)
            JOIN allocations USING(object_id)
            WHERE json_extract(runs.body,'$.project_id')='telemetry-fixture'
              AND json_extract(runs.body,'$.agent') IN ('codex','claude')''').fetchall()
        rows = [row for row in rows if row[2] in kinds]
        if expected <= {(row[1], row[2]) for row in rows} or time.monotonic() >= deadline:
            break
        time.sleep(0.2)
    assert expected <= {(row[1], row[2]) for row in rows}, 'missing actual native session/run/resource examples'
    assert len({row[0] for row in rows}) >= len(expected), 'native examples must come from distinct managed runs'
    hashes = []
    for index, (run, _agent, _kind) in enumerate(rows, 1):
        assert db.execute('SELECT count(*) FROM objects WHERE object_id=?', (run+':scope',)).fetchone()[0] == 1
        result = subprocess.run([sys.argv[1], 'show-run', '--data-dir', str(data), '--run', run],
                                capture_output=True, check=True, timeout=8)
        document = json.loads(result.stdout)
        assert document.get('sessions'), 'example lacks native exported session evidence'
        if index == 1:
            print(json.dumps({'schema': 'subyard.telemetry.acceptance-example/v1', 'document': document}, separators=(',', ':')), flush=True)
        content = (json.dumps(document, indent=2)+'\n').encode()
        fd = os.open(output/('run-'+str(index)+'.json'), os.O_CREAT|os.O_EXCL|os.O_WRONLY, 0o600)
        with os.fdopen(fd, 'wb') as stream:
            os.fchmod(stream.fileno(), 0o600)
            stream.write(content)
        hashes.append(hashlib.sha256(content).hexdigest())
print(json.dumps({'native_run_examples': len(hashes), 'sha256': hashes}))
PY
sudo -n python3 -B "$state/source/guest.py" apply 0 dev ''
test -f /var/lib/subyard/telemetry/telemetry.sqlite
python3 -B - "$asset" "$state" <<'PY'
import hashlib, json, pathlib, subprocess, sys
binary, state = sys.argv[1], pathlib.Path(sys.argv[2])
source = pathlib.Path('/var/lib/subyard/telemetry')
target = state/'replayed'
archive = state/'telemetry-export.jsonl'
def command(action, directory, *args):
    return subprocess.run([binary, action, '--data-dir', str(directory), *args],
                          capture_output=True, check=True, timeout=120).stdout
before = json.loads(command('stats', source))
cursor = count = 0
with archive.open('wb') as output:
    while cursor < before['last_sequence']:
        page = command('export', source, '--after', str(cursor), '--limit', '1000')
        rows = [json.loads(line) for line in page.splitlines()]
        assert rows and all(row['schema'] == 'subyard.telemetry.export/v1' for row in rows)
        assert rows[0]['sequence'] > cursor and rows[-1]['sequence'] <= before['last_sequence']
        cursor = rows[-1]['sequence']
        count += len(rows)
        output.write(page)
assert count == before['records'], 'incomplete stable export'
for _ in range(2):
    command('replay', target, '--input', str(archive))
    replayed = json.loads(command('stats', target))
    assert replayed['records'] == count and replayed['kinds'] == before['kinds'], 'replay duplicated or lost records'
sql = 'SELECT origin_id,event_id,object_id,source_time,cpu_ns,io_read_bytes,io_write_bytes FROM sample_values ORDER BY origin_id,event_id LIMIT 100'
assert json.loads(command('query', source, '--sql', sql)) == json.loads(command('query', target, '--sql', sql))
assert any(key.startswith('otlp/') for key in before['kinds']) and before['kinds'].get('allocation')
print(json.dumps({'schema': 'subyard.telemetry.export-acceptance/v1', 'records': count,
                  'kinds': before['kinds'], 'replay_passes': 2, 'sample_sql_equal': True,
                  'sha256': hashlib.file_digest(archive.open('rb'), 'sha256').hexdigest()}), flush=True)
PY
if [ "$mode" = --android-resources ]; then
  printf 'ok: telemetry Android native resources, memory leases and export replay; profile lifecycle evidence is a separate prerequisite\n'
elif [ "$mode" = --android ]; then
  printf 'ok: telemetry Android native resources, memory leases, profile lifecycle and export replay\n'
else
  printf 'ok: telemetry native signals, managed descendants and both native resource owners\n'
fi
