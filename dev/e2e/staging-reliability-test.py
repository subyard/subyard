#!/usr/bin/env python3
"""Host-free checks for the real-boot observer's projection and cleanup."""
import json
import hashlib
import copy
import pathlib
import re
import shlex
import subprocess
import tempfile
import os
import sys
from unittest import mock

fixture = pathlib.Path(__file__).with_name('staging-reliability.sh').read_text()
source = fixture.split("<<'PYTHON'", 1)[1].split('\n', 1)[1].split('\nPYTHON', 1)[0]
compile(source, 'boot-observer', 'exec')
namespace = {}
exec(source.split('running=True', 1)[0], namespace)
with tempfile.TemporaryDirectory(prefix='subyard-boot-observer-', dir='/tmp') as directory:
    root = pathlib.Path(directory)
    pressure = root / 'pressure'
    pressure.write_text('some avg10=1.5 avg60=0 avg300=2 total=10\n')
    assert namespace['pressure'](pressure)['some']['total'] == 10
    for invalid in ['', 'some avg10=nan avg60=0 avg300=2 total=10\n', 'some avg10=101 avg60=0 avg300=0 total=10\n']:
        pressure.write_text(invalid)
        assert namespace['pressure'](pressure) == {'unavailable': True}
    assert namespace['pressure'](root / 'absent') == {'unavailable': True}
    counters = root / 'cpu.stat'
    counters.write_text('usage_usec 7\nnr_throttled 2\nprivate_field 99\n')
    assert namespace['counters'](counters) == {'usage_usec': 7, 'nr_throttled': 2}
    # Map only the logfile roots; the actual parser still opens bounded files.
    namespace['P'] = lambda value: root if value == '/var/log/incus' else pathlib.Path(value)
    logs = root / 'project_vm'
    logs.mkdir()
    (logs / 'qemu.qmp.log.old').write_text('[2026-10-11T00:00:00Z] Event: '+json.dumps({'event':'RESET','data':{'reason':'guest-reset','guest':True,'private':'secret'},'timestamp':{'seconds':1,'microseconds':2,'private':'secret'}})+'\n')
    (logs / 'qemu.qmp.log').write_text('[2026-10-11T00:00:01Z] QUERY: '+json.dumps({'execute':'cont','arguments':{'private':'secret'}})+'\n[2026-10-11T00:00:02Z] Event: '+json.dumps({'event':'PRIVATE_EVENT','data':{'private':'secret'}})+'\n')
    evidence = namespace['qmp_events']('project', 'vm')
    rows = evidence['records']
    assert [row['event'] for row in rows] == ['RESET', 'cont']
    assert rows[0]['reason'] == 'guest-reset' and rows[0]['guest'] is True
    assert 'secret' not in json.dumps(evidence) and 'private' not in json.dumps(evidence)

p0_fixture = pathlib.Path(__file__).with_name('p0-broker-recovery.sh').read_text()
pause_source = p0_fixture.split("<<'PYREAPERPAUSE'\n",1)[1].split('\nPYREAPERPAUSE',1)[0]
with tempfile.TemporaryDirectory(prefix='subyard-reaper-pause-', dir='/tmp') as directory:
    root=pathlib.Path(directory)/'run/systemd/system'; root.mkdir(parents=True)
    dropin=root/'subyard-test-vms-lease-reaper.service.d/subyard-e2e-pause.conf'
    effective={'RefuseManualStart':'no','ActiveState':'inactive'}
    def systemctl(argv,**kwargs):
        if argv[1]=='daemon-reload': effective['RefuseManualStart']='yes' if dropin.exists() else 'no'
        if argv[1]=='show': return effective[argv[3].split('=',1)[1]]+'\n'
        return ''
    def pause(action,marker='subyard-p0-123'):
        with mock.patch.object(sys,'argv',['pause',action,marker]), mock.patch.object(subprocess,'check_output',systemctl):
            exec(pause_source.replace("'/run/systemd/system'",repr(str(root))),{})
    def refused(action):
        try: pause(action)
        except RuntimeError: return
        raise AssertionError('unsafe pause accepted')
    pause('install'); pause('verify')
    assert dropin.stat().st_mode & 0o777 == 0o600 and dropin.parent.stat().st_mode & 0o777 == 0o755
    original=dropin.read_bytes()
    refused('install')
    for drift in ['mode','marker','link','extra','effective','active']:
        if drift=='mode': dropin.chmod(0o644)
        elif drift=='marker': dropin.write_bytes(original.replace(b'123',b'456'))
        elif drift=='link': os.link(dropin,root/'other')
        elif drift=='extra': (dropin.parent/'foreign').write_bytes(b'foreign')
        elif drift=='effective': effective['RefuseManualStart']='no'
        else: effective['ActiveState']='active'
        refused('verify')
        if drift in ['mode','marker','link','extra']:
            refused('restore'); assert dropin.exists()
        dropin.chmod(0o600); dropin.write_bytes(original)
        if drift=='link': (root/'other').unlink()
        if drift=='extra': (dropin.parent/'foreign').unlink()
        effective.update(RefuseManualStart='yes',ActiveState='inactive')
    pause('restore'); assert not dropin.parent.exists() and effective['RefuseManualStart']=='no'
    dropin.parent.symlink_to(root,target_is_directory=True); refused('install'); dropin.parent.unlink()
    root.chmod(0o777); refused('install'); root.chmod(0o755)
print('shared reaper pause preserves owned drop-in and verifies effective/inactive state before recovery')

cleanup = re.search(r'^cleanup\(\) \{\n.*?^\}', fixture, re.MULTILINE | re.DOTALL)[0]
for failed in ['pause', 'start', 'evidence']:
    script = '''set +e
HOLDER_PID=''; SECOND_BOOT_ACTIVE=0; REAPER_PAUSED=1; MARKER=subyard-p0-123; REAPER_TIMER_STOPPED=1
stop_sampler() { :; }
retire_minimal_owner() { echo retired; }
restore_inventory_fault() { :; }
collect_evidence() { [ "$FAILED" != evidence ]; }
reaper_pause() { [ "$FAILED" != pause ]; }
outer_exec() { case "$*" in *"$FAILED"*) return 42;; *) return 0;; esac; }
owner_cleanup() { local rc=$?; echo shared; exit "$rc"; }
''' + f'FAILED={failed}\n' + cleanup + '\ntrue\ncleanup\n'
    result = subprocess.run(['bash', '-c', script], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=5)
    assert result.returncode == 3, (failed, result.returncode, result.stdout, result.stderr)
    if failed == 'pause': assert 'retired' not in result.stdout and 'shared' not in result.stdout, result
print('staging observer projection and cleanup checks passed')

p0_cleanup = re.search(r'^cleanup\(\) \{\n.*?^\}',p0_fixture,re.MULTILINE|re.DOTALL)[0]
for failed in ['fault','pause']:
    script = '''set +e
NEIGHBOR_CONFIG=''; RECLAIM_MARKER=''; STATE_PARENT=''
VICTIM_PID=''; NEIGHBOR_PID=''; REUSE_PID=''
REAPER_PAUSED=1; MARKER=subyard-p0-123; REAPER_TIMER_STOPPED=1; REAPER_PAUSE_MARKER=subyard-p0-123
restore_targeted_incus_failure() { [ "$FAILED" != fault ]; }
reaper_pause() { echo pause_restore; [ "$FAILED" != pause ]; }
outer_root() { echo unexpected_start; }
''' + f'FAILED={failed}\n' + p0_cleanup + '\ntrue\ncleanup\n'
    result=subprocess.run(['bash','-c',script],capture_output=True,text=True,timeout=5)
    assert result.returncode==3 and 'unexpected_start' not in result.stdout, result

# A failed diagnostic/export must not replace the original bootstrap failure.
script = '''set +e
HOLDER_PID=''; SECOND_BOOT_ACTIVE=0; REAPER_PAUSED=0; REAPER_TIMER_STOPPED=0
stop_sampler() { :; }
retire_minimal_owner() { :; }
restore_inventory_fault() { :; }
collect_evidence() { return 42; }
owner_cleanup() { exit "$?"; }
''' + cleanup + '\n(exit 7)\ncleanup\n'
result = subprocess.run(['bash', '-c', script], capture_output=True, text=True, timeout=5)
assert result.returncode == 7, result
receipt = json.loads(result.stdout.split('STAGING_CLEANUP=', 1)[1])
assert receipt == {'fixture_exit_code': 7, 'cleanup_exit_code': 3}, receipt

# Before verified worker installation, no Incus export or host sink is invoked.
collect = re.search(r'^collect_evidence\(\) \{\n.*?^\}', fixture, re.MULTILINE | re.DOTALL)[0]
with tempfile.TemporaryDirectory(prefix='subyard-bootstrap-evidence-', dir='/tmp') as directory:
    script = f'''set -euo pipefail
STATE_PARENT={directory!r}; BROKER_INSTALLED=0
stop_sampler() {{ :; }}
outer_exec() {{ echo unexpected_export; return 42; }}
sudo() {{ echo unexpected_sink; return 42; }}
''' + collect + '\ncollect_evidence\n'
    result = subprocess.run(['bash', '-c', script], capture_output=True, text=True, timeout=5)
    assert result.returncode == 0 and result.stdout == 'STAGING_EVIDENCE_UNAVAILABLE=broker_not_verified\n', result
    script = f'''set -euo pipefail
STATE_PARENT={directory!r}; BROKER_INSTALLED=1
stop_sampler() {{ :; }}
verify_broker_installation() {{ return 1; }}
outer_exec() {{ echo unsafe_export; }}
sudo() {{ echo unsafe_sink; }}
''' + collect + '\ncollect_evidence\n'
    result = subprocess.run(['bash', '-c', script], capture_output=True, text=True, timeout=5)
    assert result.returncode == 1 and not result.stdout, result

print('bootstrap exit and uninstalled-worker guard checks passed')

# The native sink acknowledges pending spool entries after durable host capture.
# Exercise the actual collector with an empty spool and a persisted builder ID.
# Mock native root filesystem ownership/Incus only; retain real paths and modes.
builder_receipt = re.search(r'^builder_incident_receipt\(\) \{\n.*?^\}', fixture, re.MULTILINE | re.DOTALL)[0]
builder_capture = re.search(r'^collect_builder_incident\(\) \{\n.*?^\}', fixture, re.MULTILINE | re.DOTALL)[0]
with tempfile.TemporaryDirectory(prefix='subyard-acked-builder-', dir='/tmp') as directory:
    root = pathlib.Path(directory)
    state = root/'native-state'
    state.mkdir(mode=0o700)
    config = root/'native-config'
    config.write_text('E2E_VM_STATE_DIR=/var/lib/subyard/test-vms\nE2E_BROKER_SOURCE=test-yard\n')
    config.chmod(0o644)
    incident_id = '00000000000000000001-'+'a'*16
    fallback_slot = {'slot_id': 'slot-000', 'resource_generation': 0, 'lease_epoch': 0}
    receipt = {'incident_id': incident_id, 'owner': 'b'*32, 'build_key': 'c'*64,
        'incident_slot': fallback_slot}
    registry = {'schema_version': 1, 'owner': receipt['owner'], 'build': {
        'key': receipt['build_key'], 'environment': 'subyard-pair', 'project': 'subyard-e2e-vms-builder',
        'memory_bytes': 2684354560, 'disk_peak_bytes': 32212254720,
        'incident_id': incident_id, 'incident_slot': fallback_slot,
        # Persisted readinessFailure has exported Go field names, unlike diagnostics.
        'readiness_failure': {'Label': 'waiting for e2e-base-1 Incus agent',
            'Project': 'subyard-e2e-vms-builder', 'VM': 'e2e-base-1', 'Phase': 'incus-agent',
            'Budget': 300000000000, 'StartedAt': '2026-01-01T00:00:00Z',
            'ObservedAt': '2026-01-01T00:05:00Z', 'LastProbe': 'error_kind=command_error exit_code=255'}}}
    registry_path = state/'images.json'
    registry_path.write_text(json.dumps(registry))
    registry_path.chmod(0o600)
    host_root = root/'logs/test-vms-broker-incidents'
    host_root.mkdir(parents=True, mode=0o750)
    (host_root/'host').mkdir(mode=0o750)
    host_root.chmod(0o750)
    (host_root/'host').chmod(0o750)
    incident = {'schema_version': 1, 'incident_id': incident_id, 'source': 'test-yard',
        **fallback_slot, 'created_at': '2026-10-12T00:00:00Z', 'diagnostics': {
            'resource': json.dumps({'resource_kind': 'base_builder', 'project': 'subyard-e2e-vms-builder',
                'vm': 'e2e-base-1', 'build_key': receipt['build_key']})}}
    memory = {'outer_host_evidence': 'collected by physical-owner host sink',
        'available_bytes': 123, 'limit_available': False}
    host = {'observed_at': '2026-10-12T00:00:01Z',
        'timing': 'after incident, at host spool collection; kernel window is the preceding 15 minutes',
        'host_memory': memory, 'allocation_memory': memory}
    incident_path = host_root/(incident_id+'.json')
    host_path = host_root/'host'/(incident_id+'.json')
    for path, payload in ((incident_path, incident), (host_path, host)):
        path.write_text(json.dumps(payload))
        path.chmod(0o640)
    shim = root/'native-python.py'
    shim.write_text('''import pathlib,os,stat,sys
native=pathlib.PosixPath
directory=native(sys.argv.pop(1))
paths={'/var/lib/subyard/test-vms':directory/'native-state','/etc/subyard/test-vms.env':directory/'native-config'}
pathlib.Path=lambda *args: paths.get(str(native(*args)),native(*args))
original=native.lstat
def lstat(path):
    info=list(original(path))
    if path.is_relative_to(directory): info[4]=0
    foreign=directory/'foreign-owner'
    if foreign.exists() and str(path)==foreign.read_text(): info[4]=1001
    return os.stat_result(info)
native.lstat=lstat
sys.argv=sys.argv[1:]
exec(compile(sys.stdin.read(),'native-evidence-leaf','exec'))
''')
    native_python = shim.read_text()
    def run_capture(spool=None, project_marker='test-vms-v1', preflight=False):
        spool = {'schema_version': 1} if spool is None else spool
        script = f'''set -euo pipefail
STATE_PARENT={str(root)!r}; SUBYARD_HOME={str(root)!r}; BROKER_INSTALLED=1
stop_sampler() {{ :; }}
verify_broker_installation() {{ :; }}
sudo() {{ [ "$*" = '-n systemctl start subyard-test-vms-host-sink.service' ]; }}
python3() {{ command python3 {str(shim)!r} {str(root)!r} "$@"; }}
outer_exec() {{
  case "$*" in
    '/usr/local/libexec/subyard/test-vms-inner _test-vms-worker spool-export') printf '%s\\n' {shlex.quote(json.dumps(spool))};;
    'incus project get subyard-e2e-vms-builder user.subyard.managed') printf '%s\\n' {project_marker!r};;
    'incus project get subyard-e2e-vms-builder user.subyard.base_owner') echo {'b'*32};;
    'python3 -') python3 -;;
    *) return 42;;
  esac
}}
''' + builder_receipt + '\n' + builder_capture + '\n' + collect + ('\nbuilder_incident_receipt\n' if preflight else '\ncollect_evidence\n')
        return subprocess.run(['bash', '-c', script], capture_output=True, text=True, timeout=5)
    # Supported provisioning creates a private root, not a worker-key marker.
    # Both an absent image registry and a persisted empty registry are native.
    registry_path.unlink()
    for payload in (None, {'schema_version': 1, 'owner': receipt['owner'], 'bases': []}):
        if payload is not None:
            registry_path.write_text(json.dumps(payload)); registry_path.chmod(0o600)
        result = run_capture(preflight=True)
        assert result.returncode == 0 and json.loads(result.stdout) == {}, (result.returncode, result.stdout, result.stderr)
    registry_path.write_text(json.dumps(registry)); registry_path.chmod(0o600)
    result = run_capture(preflight=True)
    assert result.returncode == 0 and json.loads(result.stdout) == receipt, result
    for failure in ({'project': 'subyard-e2e-vms-builder', 'vm': 'e2e-base-1'},
        {'Project': 'foreign', 'VM': 'e2e-base-1'}, {'Project': 'subyard-e2e-vms-builder', 'VM': 'foreign'},
        {'Project': 'subyard-e2e-vms-builder'}, 'malformed'):
        registry_path.write_text(json.dumps({**registry, 'build': {**registry['build'], 'readiness_failure': failure}}))
        result = run_capture(preflight=True)
        assert result.returncode == 1 and not result.stdout, (failure,result)
    registry_path.write_text(json.dumps(registry))
    for spool in ({'schema_version': 1}, {'schema_version': 1, 'incidents': [incident]}):
        result = run_capture(spool)
        assert result.returncode == 0, (result.returncode, result.stdout, result.stderr)
        lines = result.stdout.splitlines()
        assert json.loads(next(line.split('=', 1)[1] for line in lines if line.startswith('STAGING_SPOOL='))) == spool
        assert [json.loads(line.split('=', 1)[1]) for line in lines if line.startswith('STAGING_BUILDER_INCIDENT=')] == [incident]
        assert [json.loads(line.split('=', 1)[1]) for line in lines if line.startswith('STAGING_HOST_OBSERVATION=')] == [host]
    # An acquire-driven builder retains its triggering allocation identity even
    # after the host sink acknowledges the pending spool entry.
    acquiring_slot = {'slot_id': 'slot-001', 'resource_generation': 7, 'lease_epoch': 11}
    acquiring_registry = {**registry, 'build': {**registry['build'], 'incident_slot': acquiring_slot}}
    acquiring_incident = {**incident, **acquiring_slot}
    registry_path.write_text(json.dumps(acquiring_registry))
    incident_path.write_text(json.dumps(acquiring_incident))
    result = run_capture()
    assert result.returncode == 0, (result.returncode, result.stdout, result.stderr)
    assert [json.loads(line.split('=', 1)[1]) for line in result.stdout.splitlines()
        if line.startswith('STAGING_BUILDER_INCIDENT=')] == [acquiring_incident]
    result = run_capture(preflight=True)
    assert result.returncode == 0 and json.loads(result.stdout) == {**receipt, 'incident_slot': acquiring_slot}, result
    for key, bad in (('slot_id', 'slot-002'), ('resource_generation', 8), ('lease_epoch', 12),
        ('resource_generation', True), ('lease_epoch', 11.0)):
        incident_path.write_text(json.dumps({**acquiring_incident, key: bad}))
        result = run_capture()
        assert result.returncode == 1 and 'STAGING_BUILDER_INCIDENT=' not in result.stdout, (key, bad, result)
    incident_path.write_text(json.dumps(acquiring_incident))
    for slot in (fallback_slot, acquiring_slot):
        for key, bad in (('slot_id', 'slot-0000'), ('slot_id', '../foreign'),
            ('resource_generation', True), ('resource_generation', -1), ('resource_generation', 2**64),
            ('lease_epoch', False), ('lease_epoch', 1.0), ('lease_epoch', -1), ('lease_epoch', 2**64)):
            registry_path.write_text(json.dumps({**registry, 'build': {**registry['build'],
                'incident_slot': {**slot, key: bad}}}))
            result = run_capture(preflight=True)
            assert result.returncode == 1 and not result.stdout, (slot, key, bad, result)
        for key in ('slot_id', 'resource_generation', 'lease_epoch'):
            registry_path.write_text(json.dumps({**registry, 'build': {**registry['build'],
                'incident_slot': {name: value for name, value in slot.items() if name != key}}}))
            result = run_capture(preflight=True)
            assert result.returncode == 1 and not result.stdout, (slot, key, result)
    for key in ('resource_generation', 'lease_epoch'):
        registry_path.write_text(json.dumps({**registry, 'build': {**registry['build'],
            'incident_slot': {**fallback_slot, key: 1}}}))
        assert run_capture(preflight=True).returncode == 1
    registry_path.write_text(json.dumps(registry))
    incident_path.write_text(json.dumps(incident))
    assert run_capture(project_marker='foreign').returncode == 1
    for path, bad, original in ((config, 'E2E_VM_STATE_DIR=/foreign\nE2E_BROKER_SOURCE=test-yard\n', config.read_text()),
        (registry_path, json.dumps({**registry, 'build': {**registry['build'], 'incident_id': '../foreign'}}), registry_path.read_text()),
        (incident_path, json.dumps({**incident, 'source': 'foreign'}), incident_path.read_text()),
        (host_path, json.dumps({**host, 'observed_at': '2026-10-11T00:00:00Z'}), host_path.read_text())):
        path.write_text(bad)
        result = run_capture()
        assert result.returncode == 1 and 'STAGING_BUILDER_INCIDENT=' not in result.stdout, result
        path.write_text(original)
    host_path.chmod(0o600)
    assert run_capture().returncode == 1
    host_path.chmod(0o640)
    for path in (registry_path, host_path):
        saved = path.with_suffix('.saved')
        path.rename(saved)
        path.symlink_to(saved)
        assert run_capture().returncode == 1
        path.unlink()
        saved.rename(path)
        (root/'foreign-owner').write_text(str(path))
        assert run_capture().returncode == 1
        (root/'foreign-owner').unlink()
    saved = host_path.with_suffix('.saved')
    host_path.rename(saved)
    assert run_capture().returncode == 1
    saved.rename(host_path)
print('marker-free preflight, fallback/acquire builder capture, exact slot identity and provenance refusal checks passed')

verify_worker = re.search(r'^verify_broker_installation\(\) \{\n.*?^\}', fixture, re.MULTILINE | re.DOTALL)[0]
with tempfile.TemporaryDirectory(prefix='subyard-bootstrap-worker-', dir='/tmp') as directory:
    root = pathlib.Path(directory)
    engine = root/'runtime/current/bin/yard-engine'
    engine.parent.mkdir(parents=True)
    engine.write_bytes(b'fixture candidate engine')
    expected = hashlib.sha256(engine.read_bytes()).hexdigest()
    for actual, result_code in [(expected, 0), ('b'*64, 1), ('', 42)]:
        script = f'''set -euo pipefail
SUBYARD_HOME={directory!r}; PROJECT=subyard-test-yard; INSTANCE=yard-test-yard
YARD=test-yard; MARKER=owned-fixture; WORKER_DIGEST={actual!r}
timeout() {{
  case "$*" in
    *user.subyard.p0-image-cache*) echo owned-fixture;;
    *user.subyard.managed*) echo true;;
    *user.subyard.name*) echo test-yard;;
    *sha256sum*) [ -n "$WORKER_DIGEST" ] || return 42; printf '%s  worker\\n' "$WORKER_DIGEST";;
    *) return 42;;
  esac
}}
''' + verify_worker + '\nverify_broker_installation\n'
        result = subprocess.run(['bash', '-c', script], capture_output=True, text=True, timeout=5)
        assert result.returncode == result_code, result
print('broker evidence requires matching installed candidate identity')

# Capacity receipts survive refusal; only known headroom satisfies the unchanged guard.
sample = re.search(r'^sample_cold_capacity\(\) \{\n.*?^\}', fixture, re.MULTILINE | re.DOTALL)[0]
guard = fixture.split('# A cold builder reserves 30GiB plus the normal 5GiB physical reserve.\n', 1)[1].split('\njq -e ', 1)[0]
for available, broker, expected in [
        ('37580963840', '{"resources":{}}', 0),
        ('37580963839', '{"resources":{}}', 2),
        ('unknown-private-value', '{"resources":{}}', 2),
        ('37580963840', 'unknown-private-value', 2)]:
    script = f'''set -euo pipefail
outer_exec() {{ printf 'Size Used Avail\\n42949672960 100 {available}\\n'; }}
status() {{ printf '%s\\n' '{broker}'; }}
die() {{ exit 2; }}
''' + sample + '\nsample_cold_capacity after_reclaim\n' + guard
    result = subprocess.run(['bash', '-c', script], capture_output=True, text=True, timeout=5)
    assert result.returncode == expected and 'unknown-private-value' not in result.stdout, result
    rows = {line.split('=', 1)[0]: json.loads(line.split('=', 1)[1]) for line in result.stdout.splitlines()}
    assert rows['STAGING_COLD_CAPACITY']['phase'] == 'after_reclaim', rows
    disk = rows['STAGING_STORAGE_CAPACITY']
    assert disk['required_bytes'] == 35 * 1024**3 and disk['phase'] == 'after_reclaim', disk
    if available.isdecimal():
        assert disk['available_bytes'] == int(available) and disk['reserve_bytes'] == 5 * 1024**3, disk
    else:
        assert disk['error_kind'] == 'telemetry_unavailable', disk
print('cold backing capacity receipts and unchanged refusal boundary checks passed')

measure = fixture.split("<<'PYDISPOSABLE'", 1)[1].split('\n', 1)[1].split('\nPYDISPOSABLE', 1)[0]
with tempfile.TemporaryDirectory(prefix='subyard-disposable-sizes-', dir='/tmp') as directory:
    root = pathlib.Path(directory)
    cached = root/'cache'; cached.mkdir(); (cached/'entry').write_bytes(b'cache')
    linked = root/'link'; linked.symlink_to(cached, target_is_directory=True)
    paths = [str(cached), str(root/'absent'), str(linked), str(root/'missing-stage'), str(cached)]
    result = subprocess.run(['python3', '-c', measure, 'before_reclaim', 'outer_vm', *paths], capture_output=True, text=True, timeout=5)
    row = json.loads(result.stdout.split('STAGING_DISPOSABLE_CAPACITY=', 1)[1])
    assert row['default_go_build_bytes'] > 0 and row['marked_go_build_bytes'] == 0 and row['go_modules_bytes'] is None, row
    assert 'apt_lists_bytes' not in row, 'allocated VM lists require the privileged probe'
    assert directory not in result.stdout, 'private paths escaped category projection'

reclaim = re.search(r'^reclaim_release_staging\(\) \{\n.*?^\}', fixture, re.MULTILINE | re.DOTALL)[0]
with tempfile.TemporaryDirectory(prefix='subyard-worktree.', dir='/tmp') as directory:
    root = pathlib.Path(directory)/'src'; stage = root/'.build/p0-owner-release'; stage.mkdir(parents=True)
    (stage/'unused-package').write_text('candidate package')
    installed = root/'installed-worker'; installed.write_text('verified engine')
    for staged, verified, expected in [(0, 1, 1), (1, 0, 1), (1, 1, 0)]:
        script = f'set -euo pipefail\nROOT={str(root)!r}; RELEASE_STAGED={staged}; BROKER_INSTALLED={verified}\n' + reclaim + '\nreclaim_release_staging\n'
        result = subprocess.run(['bash', '-c', script], capture_output=True, text=True, timeout=5)
        assert result.returncode == expected and installed.read_text() == 'verified engine', result
        assert stage.exists() == (expected != 0), 'release staging deletion crossed ownership gate'
    foreign = root/'foreign'; foreign.mkdir(); (foreign/'keep').write_text('unowned')
    stage.symlink_to(foreign, target_is_directory=True)
    script = f'set -euo pipefail\nROOT={str(root)!r}; RELEASE_STAGED=1; BROKER_INSTALLED=1\n' + reclaim + '\nreclaim_release_staging\n'
    result = subprocess.run(['bash', '-c', script], capture_output=True, text=True, timeout=5)
    assert result.returncode == 1 and (foreign/'keep').read_text() == 'unowned', result
print('disposable byte projection and verified run-staging cleanup checks passed')

footprint = fixture.split("<<'PYFOOTPRINT'", 1)[1].split('\n', 1)[1].split('\nPYFOOTPRINT', 1)[0]
with tempfile.TemporaryDirectory(prefix='subyard-disk-footprint-', dir='/tmp') as directory:
    root = pathlib.Path(directory)
    (root/'usr/lib').mkdir(parents=True)
    (root/'usr/lib/installed').write_bytes(b'installed runtime')
    (root/'fixture/source').mkdir(parents=True)
    (root/'fixture/source/file').write_bytes(b'source')
    (root/'foreign').mkdir()
    (root/'foreign/keep').write_bytes(b'private foreign content')
    (root/'var').symlink_to(root/'foreign', target_is_directory=True)
    (root/'opt').mkdir()
    # Map allowlisted paths into this fixture; no system footprint is read locally.
    setup = f'''import pathlib,subprocess
original_path=pathlib.Path
pathlib.Path=lambda name: original_path({directory!r}) / str(name).lstrip('/')
original_du=subprocess.check_output
def du(args,**kwargs):
    if args[-1].endswith('/opt'): raise subprocess.TimeoutExpired(args,2)
    return original_du(args,**kwargs)
subprocess.check_output=du
'''
    for scope, extra in [('outer_vm', ['/fixture/source','/fixture/missing','/fixture/owner']), ('owner_container', [])]:
        result = subprocess.run(['python3', '-c', setup+footprint, scope, *extra], capture_output=True, text=True, timeout=5)
        assert result.returncode == 0, result
        row = json.loads(result.stdout.split('STAGING_DISK_FOOTPRINT=', 1)[1])
        assert row['phase'] == 'after_reclaim' and row['scope'] == scope, row
        assert row['usr_bytes'] >= row['usr_lib_bytes'] > 0, row
        assert row['var_bytes'] is None and row['var_lib_bytes'] is None and row['opt_bytes'] is None, row
        assert row['incus_images_bytes'] is None and row['boot_bytes'] == 0, row
        assert all(value is None or type(value) is int and value >= 0 for key,value in row.items() if key.endswith('_bytes')), row
        assert directory not in result.stdout and 'private' not in result.stdout and '/usr' not in result.stdout, result
        if scope == 'outer_vm':
            assert row['source_bytes'] > 0 and row['platform_bytes'] == row['owner_state_bytes'] == 0, row
            assert 'nested_storage_bytes' not in row, row
        else:
            assert row['nested_storage_bytes'] == 0 and 'source_bytes' not in row, row
    assert (root/'foreign/keep').read_bytes() == b'private foreign content'
print('numeric disk footprint privacy, missing, symlink and timeout checks passed')

prepare_owner = 'prepare_minimal_owner() {' + fixture.split('prepare_minimal_owner() {', 1)[1].split('\n\nprepare_owner_go_cache', 1)[0]
with tempfile.TemporaryDirectory(prefix='subyard-minimal-owner-', dir='/tmp') as directory:
    root = pathlib.Path(directory)
    for failed, expected in [('foreign-project', 2), ('inventory', 2), ('post-inventory', 2), ('missing-project', 2), ('instances', 2), ('nonempty', 2), ('legacy', 0), ('prerequisites', 42), ('backend', 7), ('evidence-baseline', 2), ('audit', 2), ('', 0)]:
        checkout = root/(failed or 'passed'); (checkout/'.build').mkdir(parents=True)
        (checkout/'native-state').mkdir(mode=0o700)
        native_config = checkout/'native-config'
        native_config.write_text('E2E_VM_STATE_DIR='+('/foreign' if failed == 'evidence-baseline' else '/var/lib/subyard/test-vms')+'\nE2E_BROKER_SOURCE=test-yard\n')
        native_config.chmod(0o644)
        native_shim = checkout/'native-python.py'; native_shim.write_text(native_python)
        log = checkout/'calls'
        project_state = checkout/'project-state'
        metadata = checkout/'product-metadata'
        if failed == 'foreign-project': project_state.write_text('foreign')
        script = f'''set -euo pipefail
ROOT={str(checkout)!r}; SUBYARD_HOME=/installed; PROJECT=subyard-test-yard; INSTANCE=yard-test-yard
OWNER_CONFIG_HOME={str(checkout/'config')!r}; PROJECT_STATE={str(project_state)!r}; METADATA={str(metadata)!r}
YARD=test-yard; OWNER_BASE_IMAGE=marked-image; MARKER=subyard-p0-123; OWNER_DIAGNOSTIC_DEV_UID=1001
diagnostic_cpu=4; MINIMAL_OWNER=0; BROKER_INSTALLED=0; LOG={str(log)!r}; FAILED={failed!r}
die() {{ exit 2; }}
go() {{
  printf 'compile\\n' >> "$LOG"
  printf '#!/bin/sh\\n[ "$5" = "$1" ] || exit 42\\nprintf "backend\\n" >> "$LOG"\\n[ "$FAILED" != backend ] || exit 7\\n' > "$ROOT/.build/p0-staging-owner/owner"
  chmod 0700 "$ROOT/.build/p0-staging-owner/owner"
}}
owner_adapter() {{
  printf '%s\\n' "$1" >> "$LOG"
  if [ "$1" = 02-create-project.sh ]; then
    [ ! -e "$PROJECT_STATE" ] || return 42
    [ "$FAILED" = missing-project ] || printf 'owned' > "$PROJECT_STATE"
    if [ "$FAILED" != legacy ]; then
      printf 'installation=1\\noperator_uid=%s\\ndata_home=/installed\\nconfig_home=%s\\n' "$(id -u)" "$OWNER_CONFIG_HOME" > "$METADATA"
    fi
  fi
}}
incus() {{
  case "$1" in
    project)
      case "$2" in
        list)
          [ "$FAILED" != inventory ] || return 42
          if [ -e "$PROJECT_STATE" ]; then
            [ "$FAILED" != post-inventory ] || return 42
            echo "$PROJECT"
          fi;;
        show) test -f "$PROJECT_STATE";;
        get)
          case "$4" in
            user.subyard.p0-image-cache) [ "$(cat "$PROJECT_STATE")" = cache ] && echo "$MARKER";;
            *) return 42;;
          esac;;
        set)
          [ "$(cat "$PROJECT_STATE")" = owned ] || return 42
          [ "$4" = features.images=false ] && [ "$5" = "user.subyard.p0-image-cache=$MARKER" ] || return 42
          printf 'cache-after-create\\n' >> "$LOG"
          printf 'cache' > "$PROJECT_STATE";;
        *) return 42;;
      esac;;
    list)
      [ "$FAILED" != instances ] || return 42
      if [ "$FAILED" = nonempty ]; then echo foreign-instance; fi;;
    init)
      [ "$(cat "$PROJECT_STATE")" = cache ] || return 42
      printf 'instance' > "$PROJECT_STATE"
      [[ " $* " = *" -c user.subyard.initialized=false "* ]] || return 42
      printf 'init\\n' >> "$LOG";;
    exec) printf 'cloud-init\\n' >> "$LOG";;
    *) return 42;;
  esac
}}
timeout() {{ shift 3; "$@"; }}
outer_exec() {{
  if [ "$*" = 'python3 -' ]; then
    printf 'evidence-baseline\\n' >> "$LOG"
    command python3 {str(native_shim)!r} {str(checkout)!r} -
  else
    printf 'prerequisites\\n' >> "$LOG"; [ "$FAILED" != prerequisites ] || return 42
  fi
}}
verify_broker_installation() {{ printf 'verify\\n' >> "$LOG"; }}
prepare_owner_audit() {{ printf 'audit\\n' >> "$LOG"; [ "$FAILED" != audit ]; }}
export LOG FAILED
''' + builder_receipt + '\n' + prepare_owner + '\nprepare_minimal_owner\nprintf "cold\\n" >> "$LOG"\n'
        result = subprocess.run(['bash', '-c', script], capture_output=True, text=True, timeout=5)
        assert result.returncode == expected, (result.returncode, result.stdout, result.stderr)
        calls = log.read_text().splitlines()
        if failed in ('foreign-project', 'inventory', 'post-inventory', 'missing-project', 'instances', 'nonempty'):
            assert calls == (['compile'] if failed in ('foreign-project', 'inventory') else ['compile','02-create-project.sh']), calls
            assert project_state.exists() == (failed not in ('inventory', 'missing-project'))
            if project_state.exists():
                assert project_state.read_text() == ('foreign' if failed == 'foreign-project' else 'owned')
            continue
        assert metadata.exists() == (failed != 'legacy')
        if metadata.exists():
            assert metadata.read_text() == f'installation=1\noperator_uid={os.getuid()}\ndata_home=/installed\nconfig_home={checkout / "config"}\n'
        assert calls[:7] == ['compile','02-create-project.sh','cache-after-create','init','03-create-subyard.sh','cloud-init','prerequisites'], calls
        assert calls[7:] == ({'prerequisites': [], 'backend': ['backend','verify'], 'evidence-baseline': ['backend','verify','evidence-baseline'], 'audit': ['backend','verify','evidence-baseline','audit'], 'legacy': ['backend','verify','evidence-baseline','audit','install-test-vms-host-sink.sh','cold'], '': ['backend','verify','evidence-baseline','audit','install-test-vms-host-sink.sh','cold']}[failed]), calls
        assert (checkout/'.build/p0-staging-owner').exists() == (failed not in ('', 'legacy')), calls
print('minimal owner prerequisites precede product backend and host sink')

audit_prepare = re.search(r'^prepare_owner_audit\(\) \{\n.*?^\}', fixture, re.MULTILINE | re.DOTALL)[0]
with tempfile.TemporaryDirectory(prefix='subyard-owner-audit-', dir='/tmp') as directory:
    root = pathlib.Path(directory)
    foreign = root/'foreign'; foreign.mkdir(); keep = foreign/'keep'; keep.write_bytes(b'foreign evidence')
    for failed in ['', 'command', 'absent', 'mode', 'symlink', 'file-symlink', 'home-symlink']:
        home = root/(failed or 'success'); home.mkdir(); home.chmod(0o700)
        logs = home/'logs'
        if failed == 'symlink':
            logs.symlink_to(foreign)
        elif failed != 'absent':
            logs.mkdir(); logs.chmod(0o700)
            for name in ['yard.log', '.yard.lock']:
                path = logs/name; path.write_bytes(b''); path.chmod(0o600)
            if failed == 'mode': logs.chmod(0o770)
            if failed == 'file-symlink':
                (logs/'yard.log').unlink(); (logs/'yard.log').symlink_to(keep)
        if failed == 'home-symlink':
            alias = root/'home-alias'; alias.symlink_to(home); home = alias
        calls = root/((failed or 'success')+'.calls')
        script = f'''set -euo pipefail
SUBYARD_HOME={str(home)!r}; CALLS={str(calls)!r}; FAILED={failed!r}
timeout() {{ printf '%s\\n' "$*" >> "$CALLS"; [ "$FAILED" != command ]; }}
''' + audit_prepare + '\nprepare_owner_audit\n'
        result = subprocess.run(['bash', '-c', script], capture_output=True, text=True, timeout=5)
        assert result.returncode == (0 if not failed else 1), (failed, result)
        assert calls.exists() == (failed not in ['symlink', 'file-symlink', 'home-symlink']), failed
        if calls.exists():
            assert 'env SUBYARD_NO_AUDIT=' in calls.read_text() and calls.read_text().rstrip().endswith('/bin/yard help')
    assert keep.read_bytes() == b'foreign evidence' and foreign.stat().st_uid == keep.stat().st_uid
print('owner audit setup requires normal installed CLI success and exact user-owned private paths before the sink')

retire = re.search(r'^retire_minimal_owner\(\) \{\n.*?^\}', fixture, re.MULTILINE | re.DOTALL)[0]
for owned, installed, drained, expected, reclaims in [(0,1,1,1,0),(1,1,0,1,0),(1,1,1,0,1),(1,0,1,0,1)]:
    script = f'''set -euo pipefail
MINIMAL_OWNER=1; BROKER_INSTALLED={installed}; PROJECT=fixture; MARKER=owned
incus() {{ echo {'owned' if owned else 'foreign'}; }}
verify_broker_installation() {{ :; }}
outer_exec() {{ return {0 if drained else 1}; }}
reclaim_owner_project_residue() {{ echo reclaimed; }}
''' + retire + '\nretire_minimal_owner\n'
    result = subprocess.run(['bash', '-c', script], capture_output=True, text=True, timeout=5)
    assert result.returncode == expected and result.stdout.count('reclaimed') == reclaims, result
print('minimal owner cleanup requires ownership and native drain before disposal')

# Exercise the actual EXIT trap and shared cleanup, which can also reclaim owners.
shared_fixture = pathlib.Path(__file__).with_name('p0-guest.sh').read_text()
shared_cleanup = re.search(r'^owner_cleanup\(\) \{\n.*?^\}', shared_fixture, re.MULTILINE | re.DOTALL)[0]
with tempfile.TemporaryDirectory(prefix='subyard-owner-cleanup-', dir='/tmp') as directory:
    root = pathlib.Path(directory)
    for failed in ['ownership', 'identity', 'drain', 'reclaim', '']:
        for original_rc in [0, 7]:
            case = root/((failed or 'success')+str(original_rc)); case.mkdir()
            registration = case/'yards/test-yard/config.env'; registration.parent.mkdir(parents=True)
            registration.write_text('# owned\n')
            project = case/'project'; project.touch()
            log = case/'calls'
            yard = case/'bin/yard'; yard.parent.mkdir()
            yard.write_text('#!/bin/sh\nprintf "teardown\\n" >> "$LOG"\n'); yard.chmod(0o700)
            script = f'''set -euo pipefail
HOLDER_PID=''; SECOND_BOOT_ACTIVE=0; REAPER_PAUSED=0; REAPER_TIMER_STOPPED=0
MINIMAL_OWNER=1; BROKER_INSTALLED=1; PROJECT=subyard-test-yard; MARKER=owned; TOKEN=123
OWNER_YARD_DIR={str(case/'yards')!r}; OWNER_ROOT={str(case/'absent-owner')!r}
SUBYARD_HOME={str(case/'absent-data')!r}; OWNER_BASELINE_CAPTURED=0
LOG={str(log)!r}; PROJECT_STATE={str(project)!r}; FAILED={failed!r}; export LOG
stop_sampler() {{ :; }}
restore_inventory_fault() {{ :; }}
collect_evidence() {{ printf 'evidence\\n' >> "$LOG"; }}
verify_broker_installation() {{ printf 'identity\\n' >> "$LOG"; [ "$FAILED" != identity ]; }}
outer_exec() {{ printf 'drain\\n' >> "$LOG"; [ "$FAILED" != drain ]; }}
incus() {{
  case "$*" in
    'project get subyard-test-yard user.subyard.p0-image-cache')
      if [ "$FAILED" = ownership ]; then echo foreign; else echo owned; fi;;
    'project show subyard-test-yard') test -f "$PROJECT_STATE";;
    'project show subyard-e2e-yard') return 1;;
    *) return 42;;
  esac
}}
reclaim_owner_project_residue() {{ printf 'retire-reclaim\\n' >> "$LOG"; [ "$FAILED" != reclaim ] || return 42; find "$PROJECT_STATE" -delete; }}
reclaim_owner_project_if_present() {{ printf 'shared-reclaim\\n' >> "$LOG"; }}
clean_tree() {{ printf 'shared-clean-tree\\n' >> "$LOG"; }}
cleanup_owner_capacity_state() {{ printf 'shared-capacity-cleanup\\n' >> "$LOG"; }}
''' + cleanup + '\n' + retire + '\n' + shared_cleanup + f'\ntrap cleanup EXIT\nexit {original_rc}\n'
            result = subprocess.run(['bash', '-c', script], cwd=case, capture_output=True, text=True, timeout=5)
            receipt = json.loads(result.stdout.split('STAGING_CLEANUP=', 1)[1])
            expected_rc = original_rc or (3 if failed else 0)
            assert result.returncode == expected_rc, result
            assert receipt == {'fixture_exit_code': expected_rc, 'cleanup_exit_code': 3 if failed else 0}, receipt
            calls = log.read_text().splitlines()
            assert calls[0] == 'evidence', calls
            assert project.exists() == registration.exists() == bool(failed), calls
            if failed:
                assert not any(call.startswith('shared-') or call == 'teardown' or (call == 'retire-reclaim' and failed != 'reclaim') for call in calls), calls
            else:
                assert calls[:4] == ['evidence', 'identity', 'drain', 'retire-reclaim'], calls
                assert calls.count('shared-reclaim') == 2 and calls[-1] == 'shared-capacity-cleanup', calls
print('full exit cleanup preserves retirement refusal and original failure before shared reclamation')

adapter = re.search(r'^owner_adapter\(\) \{\n.*?^\}', fixture, re.MULTILINE | re.DOTALL)[0]
with tempfile.TemporaryDirectory(prefix='subyard-owner-context-', dir='/tmp') as directory:
    root = pathlib.Path(directory)
    leaf = root/'runtime/current/scripts/context-check.sh'; leaf.parent.mkdir(parents=True)
    context = pathlib.Path(__file__).resolve().parents[2]/'scripts/lib/engine-context.sh'
    leaf.write_text(f'set -euo pipefail\n. {str(context)!r}\nsubyard_require_engine_context\n' + '''
test "$YARD_KIND:$NESTED_E2E_VMS:$DEV_UID:$DEV_SUDO" = container:1:1001:0
test "$INCUS_PROJECT:$YARD_INSTANCE_NAME" = subyard-test-yard:yard-test-yard
test "$SRV_VOLUME_TYPE:$VM_FREE_PAGE_REPORTING:$VM_PIN_IPV4" = filesystem:0:0
test "$SUBYARD_POWER_DESIRED:$SUBYARD_SUDO_PREAUTHORIZED" = running:1
python3 - <<'PYCONTEXT'
import json, os
v = os.environ
c = json.loads(v['SUBYARD_INSTALLATION_CONTEXT'])
assert (c['yardName'], c['incusProject'], c['yardInstanceName']) == ('test-yard', 'subyard-test-yard', 'yard-test-yard')
assert (c['accessKind'], c['yardKind'], c['sshHost'], c['sshPort'], c['devUid'], c['shiftMode']) == ('local', 'container', '', 2224, 1001, 'shift')
assert c['paths'] == dict(repositoryRoot=v['SUBYARD_CONFIG_DIR'], configDir=v['SUBYARD_CONFIG_DIR'],
    operatorHome=v['SUBYARD_OPERATOR_HOME'], dataHome=v['SUBYARD_HOME'], configHome=v['SUBYARD_CONFIG_HOME'],
    hostBase=v['HOST_BASE'], storagePath=v['STORAGE_PATH'], stateDir=v['SUBYARD_STATE_DIR'])
assert c['paths']['stateDir'] == v['SUBYARD_HOME'] + '/state'
PYCONTEXT
touch "$SUBYARD_HOME/project-created"
printf 'complete typed owner context verified\\n'
''')
    script = f'''set -euo pipefail
ROOT={str(pathlib.Path(__file__).resolve().parents[2])!r}
SUBYARD_HOME={directory!r}; OWNER_CONFIG_HOME={directory!r}; OWNER_DIAGNOSTIC_DEV_UID=1001
PROJECT=subyard-test-yard; INSTANCE=yard-test-yard; YARD=test-yard; OWNER_BASE_IMAGE=marked-base
''' + adapter + '\nowner_adapter context-check.sh\n'
    result = subprocess.run(['bash', '-c', script], capture_output=True, text=True, timeout=5)
    assert result.returncode == 0 and result.stdout == 'complete typed owner context verified\n', (result.returncode, result.stdout, result.stderr)
    assert (root/'project-created').is_file()
print('minimal owner adapters receive the complete product context contract')

apt_indexes = fixture.split("<<'PYAPTINDEX'\n", 1)[1].split('\nPYAPTINDEX', 1)[0]
# Use the producer's real public exports; its broker selector is distinct from
# the workspace attribution stored in the lease's yard field.
worker = pathlib.Path('dev/agent-e2e.sh').read_text()
write_guest = re.search(r'^write_guest_command\(\) \{\n.*?^\}', worker, re.MULTILINE | re.DOTALL)[0]
generated = subprocess.run(['bash','--noprofile','--norc','-c', '''set -euo pipefail
E2E_YARD=broker-selector; LEASE_YARD=workspace-attribution
LEASE_PROJECT=sample-project; LEASE_RUN=sample-run; LEASE_SLOT=slot-001; LEASE_PURPOSE=staging-reliability
LEASE_GENERATION=1; ENVIRONMENT_TYPE=android-test; VM_COUNT=1; GRANT_CPU_PER_VM=4
GRANT_MEMORY_PER_VM=8GiB; GRANT_DISK_PER_VM=40GiB; BASE_FINGERPRINT=sample-base
''' + write_guest + '\nwrite_guest_command 1 /tmp/fixture true\n'], capture_output=True, text=True, timeout=5)
assert generated.returncode == 0, generated
guest_exports = '\n'.join(line for line in generated.stdout.splitlines() if line.startswith('export SUBYARD_E2E_'))
exported = subprocess.run(['bash','--noprofile','--norc','-c', guest_exports + '''
printf '%s\\n' "$SUBYARD_E2E_PROJECT" "$SUBYARD_E2E_RUN_ID" "$SUBYARD_E2E_SLOT" "$SUBYARD_E2E_PURPOSE"
test "$SUBYARD_E2E_YARD" = broker-selector
'''], env={'PATH':'/usr/bin:/bin'}, capture_output=True, text=True, timeout=5)
assert exported.returncode == 0, exported
actor_context = exported.stdout.splitlines(); assert len(actor_context) == 4
with tempfile.TemporaryDirectory(prefix='subyard-apt-index-', dir='/tmp') as directory:
    root = pathlib.Path(directory)
    for scope,failed in [('owner_container',case) for case in ('','symlink','busy','linked','linked-lock','special','config')]+[('outer_vm',case) for case in ('','lease','project','run','slot','purpose','marker','marker-mode','lease-link','lease-symlink','du-timeout','du-invalid','supported-purpose','foreign-matching-purpose',
        'archive-config','archive-symlink','archive-owner','archive-mode','archive-link','archive-hardlink','archive-special','archive-entry-owner','archive-linked-lock','archive-busy','clean-failure','clean-timeout','clean-lock-replaced')]:
        case = root/(scope+'-'+(failed or 'success')); lists = case/'var/lib/apt/lists'; lists.mkdir(parents=True)
        locks = [case/name for name in ['var/lib/dpkg/lock-frontend','var/lib/dpkg/lock','var/lib/apt/lists/lock']]
        archives=case/'var/cache/apt/archives'; archives.mkdir(parents=True); archives.chmod(0o755)
        archive_partial=archives/'partial'; archive_partial.mkdir(mode=0o700); archive_partial.chmod(0o700)
        archive_files=[archives/'fixture.deb',archive_partial/'download.deb']
        for path in archive_files: path.write_bytes(b'private downloaded package'); path.chmod(0o600)
        if scope=='outer_vm': locks.append(archives/'lock')
        for lock in locks: lock.parent.mkdir(parents=True, exist_ok=True); lock.touch()
        inodes = [lock.stat().st_ino for lock in locks]
        index = lists/'repository_Packages'; index.write_text('reproducible index')
        partial=lists/'partial';partial.mkdir(mode=0o700);(partial/'index').write_text('private APT index bytes')
        foreign = case/'keep'; foreign.write_text('foreign bytes')
        if failed == 'symlink': (lists/'late-link').symlink_to(foreign)
        if failed == 'linked': os.link(foreign, lists/'late-link')
        if failed == 'linked-lock': os.link(locks[0],case/'linked-lock')
        if failed == 'special': os.mkfifo(lists/'late-fifo')
        if failed == 'archive-symlink': archives.rename(archives.with_name('original-archives')); archives.symlink_to(archives.with_name('original-archives'))
        if failed == 'archive-mode': archives.chmod(0o777)
        if failed == 'archive-link': (archives/'late-link').symlink_to(foreign)
        if failed == 'archive-hardlink': os.link(foreign,archives/'late-link')
        if failed == 'archive-special': os.mkfifo(archives/'late-fifo')
        if failed == 'archive-linked-lock': os.link(archives/'lock',case/'linked-lock')
        state=case/'state';state.mkdir(mode=0o711);state.chmod(0o711)
        marker=state/'.subyard-p0-marker';marker.write_text(('foreign' if failed=='marker' else 'fixture')+'\n');marker.chmod(0o600)
        current_context=list(actor_context)
        if failed=='supported-purpose': current_context[-1]='broker-cold-upgrade'
        if failed=='foreign-matching-purpose': current_context[-1]='foreign-purpose'
        lease=case/'lease';lease.write_text(json.dumps({'schema_version':1 if failed=='lease' else 2,'yard':'workspace-attribution',
            **dict(zip(('project','run','slot','purpose'),current_context))}));lease.chmod(0o444)
        if failed=='marker-mode':marker.chmod(0o644)
        if failed=='lease-link':os.link(lease,case/'lease-copy')
        if failed=='lease-symlink':lease.rename(case/'lease-original');lease.symlink_to(case/'lease-original')
        native_calls=case/'native-calls'
        # Real paths/locks/index deletion; native APT is always intercepted.
        # Never let this host-free actor call host apt-get, sudo or another leaf.
        setup=f"""import os,pathlib,pwd,subprocess
actual_lstat=pathlib.PosixPath.lstat;actual_fstat=os.fstat
caller_uid=os.getuid()
def owner(item,marker=False,apt=False,foreign=False):
 values=list(item);values[4]=77 if foreign else 42 if apt else caller_uid if marker else 0;return os.stat_result(values)
pathlib.PosixPath.lstat=lambda path:owner(actual_lstat(path),path.is_relative_to(pathlib.Path({str(state)!r})),path.is_relative_to(pathlib.Path({str(partial)!r})) or path.is_relative_to(pathlib.Path({str(archive_partial)!r})),({failed!r}=='archive-owner' and path==pathlib.Path({str(archives)!r})) or ({failed!r}=='archive-entry-owner' and path==pathlib.Path({str(archive_files[0])!r})))
os.fstat=lambda fd:owner(actual_fstat(fd))
os.geteuid=lambda:0
pwd.getpwnam=lambda name:type('APTUser',(),{{'pw_uid':42}})()
actual_output=subprocess.check_output
actual_run=subprocess.run
def run(args,**kwargs):
 if args[0]=='apt-get':
  assert args==['apt-get','clean'] and kwargs==dict(check=True,timeout=20,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL),(args,kwargs)
  path=pathlib.Path({str(native_calls)!r});path.write_text('apt-get clean\\n');path.chmod(0o600)
  if {failed!r}=='clean-failure':raise subprocess.CalledProcessError(100,args,stderr=b'private APT archive bytes')
  if {failed!r}=='clean-timeout':raise subprocess.TimeoutExpired(args,20,stderr=b'private APT archive bytes')
  for name in {list(map(str,archive_files))!r}:pathlib.Path(name).unlink()
  if {failed!r}=='clean-lock-replaced':
   replacement=pathlib.Path({str(archives/'replacement')!r});replacement.write_bytes(b'');replacement.replace({str(archives/'lock')!r})
  return subprocess.CompletedProcess(args,0)
 assert args[:4]==['du','-sx','-B1','--'] and args[4] in {list(map(str,[lists,archives]))!r},args
 return actual_run(args,**kwargs)
subprocess.run=run
def command(args,**kwargs):
 if args==['apt-config','shell','LISTS','Dir::State::lists/d']:return b"LISTS='/foreign/lists/'\\n" if {failed!r}=='config' else ('LISTS='+{str(lists)!r}+'/').encode()
 if args==['apt-config','shell','ARCHIVES','Dir::Cache::archives/d']:return b"ARCHIVES='/foreign/archives/'\\n" if {failed!r}=='archive-config' else ('ARCHIVES='+{str(archives)!r}+'/').encode()
 assert args[:4]==['du','-sx','-B1','--'],args
 if {failed!r}=='du-timeout':raise subprocess.TimeoutExpired(args,2,output=b'private APT index bytes')
 return b'private APT index bytes' if {failed!r}=='du-invalid' else actual_output(args,**kwargs)
subprocess.check_output=command
"""
        payload=apt_indexes.replace('/var/lib/',str(case/'var/lib')+'/').replace('/var/cache/',str(case/'var/cache')+'/').replace('/run/subyard-e2e-lease.json',str(lease))
        held = None
        if failed in ('busy','archive-busy'):
            import fcntl
            held = locks[-1 if failed=='archive-busy' else 0].open('r+'); fcntl.lockf(held, fcntl.LOCK_EX|fcntl.LOCK_NB)
        context = ['foreign-'+key if failed == key else value
            for key,value in zip(('project','run','slot','purpose'),current_context)]
        result = subprocess.run(['python3', '-c', setup+payload,scope,str(state),'fixture',str(os.getuid()),
            *context], capture_output=True, text=True, timeout=5)
        if held: held.close()
        native_failure=failed in ('clean-failure','clean-timeout')
        post_native_failure=native_failure or failed=='clean-lock-replaced'
        refused=bool(failed) and not failed.startswith('du-') and failed!='supported-purpose' and not post_native_failure
        assert (result.returncode != 0) == (refused or post_native_failure), (scope,failed,result)
        assert index.exists() == refused and (partial/'index').exists() == refused and foreign.read_text() == 'foreign bytes', result
        assert native_calls.exists()==(scope=='outer_vm' and not refused),(scope,failed,result)
        assert all(path.exists()==(scope!='outer_vm' or refused or native_failure) for path in archive_files),(scope,failed,result)
        rows=[json.loads(line.split('=',1)[1]) for line in result.stdout.splitlines()]
        assert len(rows)==(0 if refused else 1 if post_native_failure else 2) and not any(value in result.stdout+result.stderr for value in ('private APT index bytes','private APT archive bytes')),result
        if failed=='clean-failure': assert 'exit 100' in result.stderr,result
        if failed=='clean-timeout': assert 'deadline' in result.stderr,result
        if rows:
            keys={'phase','scope','apt_lists_bytes'} | ({'apt_archives_bytes'} if scope=='outer_vm' else set())
            assert [row['phase'] for row in rows]==(['before_reclaim'] if post_native_failure else ['before_reclaim','after_reclaim']) and all(row['scope']==scope and set(row)==keys for row in rows),rows
            assert all(row['apt_lists_bytes'] is None for row in rows) if failed.startswith('du-') else post_native_failure or rows[0]['apt_lists_bytes']>rows[1]['apt_lists_bytes']>=0,rows
            if scope=='outer_vm':
                assert all(row['apt_archives_bytes'] is None for row in rows) if failed.startswith('du-') else post_native_failure or rows[0]['apt_archives_bytes']>rows[1]['apt_archives_bytes']>=0,rows
        assert ([lock.stat().st_ino for lock in locks] != inodes)==(failed=='clean-lock-replaced'), 'APT lock inode changed'
    reclaim_apt = re.search(r'^reclaim_owner_apt_indexes\(\) \{\n.*?^\}', fixture, re.MULTILINE | re.DOTALL)[0]
    for owned, installed in [(0,1),(1,0)]:
        script = f'''set -euo pipefail
MINIMAL_OWNER={owned}; SUPPORTED_UPGRADE=0
verify_broker_installation() {{ return {0 if installed else 1}; }}
p0_capacity_assert_root_marker() {{ :; }}
outer_exec() {{ echo unsafe_deletion; }}
''' + reclaim_apt + '\nreclaim_owner_apt_indexes owner_container\n'
        result = subprocess.run(['bash', '-c', script], capture_output=True, text=True, timeout=5)
        assert result.returncode == 1 and not result.stdout, result
    context_start=fixture.index('readonly -a OUTER_E2E_CONTEXT=')
    outer_context=fixture[context_start:fixture.index('\nTOKEN=',context_start)]
    script='set -euo pipefail\n'+guest_exports+'\n'+outer_context+"""
SUBYARD_E2E_YARD=nested-yard; MINIMAL_OWNER=1; SUPPORTED_UPGRADE=0; P0_CAPACITY_STATE_ROOT=/tmp/fixture; MARKER=fixture
p0_capacity_assert_root_marker() { :; }
verify_broker_installation() { :; }
outer_exec() { printf 'owner:%s\\n' "$*"; cat >/dev/null; }
sudo() { printf 'l1:%s\\n' "$*"; cat >/dev/null; }
"""+reclaim_apt+'\nreclaim_owner_apt_indexes owner_container\nreclaim_owner_apt_indexes outer_vm\n'
    result=subprocess.run(['bash','-c',script],capture_output=True,text=True,timeout=5)
    assert result.returncode==0 and len(result.stdout.splitlines())==2,result
    assert all(' '.join(actor_context) in line and 'broker-selector' not in line and 'nested-yard' not in line
        for line in result.stdout.splitlines()),result
    # The supported owner is created by the published runtime's normal init,
    # rather than the minimal diagnostic driver. Both keep identical proof gates.
    supported_script=script.replace('MINIMAL_OWNER=1; SUPPORTED_UPGRADE=0', 'MINIMAL_OWNER=0; SUPPORTED_UPGRADE=1')
    result=subprocess.run(['bash','-c',supported_script],capture_output=True,text=True,timeout=5)
    assert result.returncode==0 and len(result.stdout.splitlines())==2,result
    for guard in ['p0_capacity_assert_root_marker', 'verify_broker_installation']:
        refused=supported_script.replace(guard+'() { :; }',guard+'() { return 1; }')
        result=subprocess.run(['bash','-c',refused],capture_output=True,text=True,timeout=5)
        assert result.returncode==1 and not result.stdout,result
    release_gate=fixture.split('if [ "$RELEASE_STAGED" = 1 ]; then\n  reclaim_release_staging',1)[1].split('\nfi',1)[0]
    release_gate='if [ "$RELEASE_STAGED" = 1 ]; then\n  reclaim_release_staging'+release_gate+'\nfi'
    for staged in [0,1]:
        for native in [0,23]:
            script=f"""set -euo pipefail
RELEASE_STAGED={staged}
reclaim_release_staging() {{ echo staged; return {native}; }}
die() {{ exit 2; }}
"""+release_gate
            result=subprocess.run(['bash','-c',script],capture_output=True,text=True,timeout=5)
            assert result.stdout==('staged\n' if staged else ''),result
            assert result.returncode==(2 if staged and native else 0),result
print('APT indexes require native locks, canonical ownership and complete preflight; lock inodes survive')

fault_payload = fixture.split("<<'PYINVENTORYFAULT'\n", 1)[1].split('\nPYINVENTORYFAULT', 1)[0]
with tempfile.TemporaryDirectory(prefix='subyard-inventory-fault-', dir='/tmp') as directory, \
        tempfile.TemporaryDirectory(prefix='subyard-inventory-fault-', dir='/dev/shm') as fault_directory:
    root = pathlib.Path(directory)
    config = root/'etc/subyard/test-vms.env'; config.parent.mkdir(parents=True)
    fault_root = pathlib.Path(fault_directory)/'subyard-staging-incus-fault'
    assert root.stat().st_dev != fault_root.parent.stat().st_dev
    native = root/'real-incus'; native.write_text('#!/bin/sh\nprintf "native:%s\\n" "$*"\n'); native.chmod(0o700)
    payload = fault_payload.replace('/run/subyard-staging-incus-fault',str(fault_root)).replace('/etc/subyard/test-vms.env',str(config))
    def fault(action, marker='subyard-p0-123'):
        return subprocess.run(['python3','-c',payload,action,marker,str(native)],capture_output=True,text=True,timeout=5)
    original = b'E2E_VM_MEMORY=2GiB\nSUBYARD_INNER_INCUS=incus\n'
    config.write_bytes(original); config.chmod(0o640)
    assert fault('install').returncode == 0
    wrapper = fault_root/'incus'
    delegated = subprocess.run(['sh',str(wrapper),'version'],capture_output=True,text=True,timeout=5)
    assert delegated.returncode == 0 and delegated.stdout == 'native:version\n'
    command = ['sh',str(wrapper),'project','list','--format','csv','-c','n']
    failed = subprocess.run(command,capture_output=True,text=True,timeout=5)
    assert failed.returncode == 1 and 'p0 targeted inventory fault' in failed.stderr
    assert subprocess.run(command,capture_output=True,text=True,timeout=5).returncode == 0
    assert fault('consumed').returncode == 0
    candidate = config.with_name('.'+config.name+'.subyard-p0-123.candidate')
    candidate.write_bytes(b'foreign staging'); candidate.chmod(0o600)
    assert fault('restore').returncode != 0 and candidate.read_bytes() == b'foreign staging'
    candidate.unlink()
    for drift in ['marker','backup','config','mode','symlink']:
        path = {'marker':fault_root/'.marker','backup':fault_root/'backup','config':config,'mode':config,'symlink':fault_root/'backup'}[drift]
        before = path.read_bytes(); mode = path.stat().st_mode & 0o777
        if drift == 'mode': path.chmod(0o600)
        elif drift == 'symlink': path.unlink(); path.symlink_to(config)
        else: path.write_bytes(b'foreign change\n')
        assert fault('restore').returncode != 0 and fault_root.exists(), drift
        if drift == 'symlink': path.unlink()
        path.write_bytes(before); path.chmod(mode)
    assert fault('restore').returncode == 0 and not fault_root.exists()
    assert config.read_bytes() == original and config.stat().st_mode & 0o777 == 0o640
    fault_root.symlink_to(config.parent, target_is_directory=True)
    assert fault('install').returncode != 0 and config.read_bytes() == original
print('actual one-shot inventory fault delegates native commands and refuses foreign restoration before exact restore')

restore_fault = re.search(r'^restore_inventory_fault\(\) \{\n.*?^\}', fixture, re.MULTILINE | re.DOTALL)[0]
for failed in [0,1]:
    for original_rc in [0,7]:
        script = f'''set -euo pipefail
HOLDER_PID=''; SECOND_BOOT_ACTIVE=0; REAPER_PAUSED=1; MARKER=subyard-p0-123; REAPER_TIMER_STOPPED=1; FAULT_INSTALLED=1
stop_sampler() {{ :; }}
inventory_fault_config() {{ echo restore; return {failed}; }}
collect_evidence() {{ echo capture; }}
reaper_pause() {{ echo pause_restore; }}
outer_exec() {{ echo "$*"; }}
retire_minimal_owner() {{ echo retire; }}
owner_cleanup() {{ local rc=$?; echo shared; exit "$rc"; }}
''' + cleanup + '\n' + restore_fault + f'\ntrap cleanup EXIT\nexit {original_rc}\n'
        result = subprocess.run(['bash','-c',script],capture_output=True,text=True,timeout=5)
        expected = original_rc or (3 if failed else 0)
        assert result.returncode == expected, result
        calls = result.stdout.splitlines()
        assert calls[:2] == ['restore','capture'], calls
        assert ('retire' in calls) == ('shared' in calls) == (not failed), calls
        assert ('pause_restore' in calls) == (not failed), calls
        assert any(' start ' in ' '+call+' ' for call in calls) == (not failed), calls
print('exit cleanup restores fault before capture/reaper/retirement and preserves failure on restoration refusal')

with tempfile.TemporaryDirectory(prefix='subyard-recovery-holder-cleanup-', dir='/tmp') as directory:
    script = f'''set -euo pipefail
STATE_PARENT={directory!r}; HOLDER_CLIENT=recovery; HOLDER_PID=42
SECOND_BOOT_ACTIVE=0; REAPER_PAUSED=0; REAPER_TIMER_STOPPED=0; FAULT_INSTALLED=0
stop_sampler() {{ :; }}
stop_holder_child() {{ test "$1" = 42 && test -f "$STATE_PARENT/recovery.release" && test ! -e "$STATE_PARENT/holder.release"; }}
collect_evidence() {{ :; }}
retire_minimal_owner() {{ :; }}
owner_cleanup() {{ exit "$?"; }}
''' + cleanup + '\n' + restore_fault + '\ntrap cleanup EXIT\nexit 7\n'
    result = subprocess.run(['bash','-c',script],capture_output=True,text=True,timeout=5)
    assert result.returncode == 7 and '"cleanup_exit_code":0' in result.stdout, result

recovery_phase = re.search(r'^verify_singleton_recovery\(\) \{\n.*?^\}', fixture, re.MULTILINE | re.DOTALL)[0]
owned_vm = re.search(r'^assert_owned_vm\(\) \{\n.*?^\}', fixture, re.MULTILINE | re.DOTALL)[0]
with tempfile.TemporaryDirectory(prefix='subyard-recovery-phase-', dir='/tmp') as directory:
    root = pathlib.Path(directory)
    identifier = '00000000000000000001-abcdef0123456789'
    events = [{'kind':kind,'incident_id':identifier,'event_id':f'{index:020d}-abcdef0123456789','resource_generation':generation,'lease_epoch':9,'recovery_attempt':1}
              for index,(kind,generation) in enumerate([('slot.quarantined',4),('recovery.start',4),('recovery.available',5)],1)]
    for failed in ['', 'pause', 'setup', 'release', 'consumed', 'quarantine', 'fence', 'incident', 'host', 'replay', 'restore', 'available', 'instances', 'volumes', 'events']:
        case = root/(failed or 'success'); case.mkdir(); log = case/'calls'
        runner = str(pathlib.Path(__file__).resolve().parents[1]/'agent-e2e.sh') if not failed else 'true'
        key_environment = dict(os.environ)
        for name in ['SUBYARD_E2E_IDENTITY', 'SUBYARD_E2E_YARD_STATE_DIR']:
            key_environment.pop(name, None)
        if not failed:
            prepared = subprocess.run([runner, '--yard', 'test-yard', '--prepare'],
                env=dict(key_environment, SUBYARD_E2E_STATE_DIR=str(case/'holder')),
                capture_output=True, text=True, timeout=5)
            assert prepared.returncode == 0, prepared
            first_key = (case/'holder/id_ed25519').read_bytes()
        slot = {'state':'held','purpose':'staging-inventory-failure','project':'subyard-test-yard','resource_generation':4,'lease_epoch':9}
        quarantined = dict(slot,state='quarantined',incident_id=identifier,last_failure_event_id=identifier)
        if failed == 'quarantine': quarantined['resource_generation'] = 5
        available = dict(quarantined,state='available',resource_generation=6 if failed == 'available' else 5)
        incident = {'incident_id':identifier,'slot_id':'slot-001','resource_generation':4,'lease_epoch':9,
                    'created_at':'2026-10-12T00:00:00Z','failure_reason':'p0 targeted inventory fault',
                    'context':{'schema_version':2,'yard':'test-yard','project':'subyard-test-yard'},'diagnostics':{},
                    'command':{'command':('/wrong' if failed == 'incident' else '/run/subyard-staging-incus-fault/incus project list --format csv -c n'),'exit_code':1}}
        host = {'outer_state':'running','observed_at':'2026-10-12T00:00:01Z','host_memory':{'available_bytes':100,'outer_host_evidence':'collected by physical-owner host sink'},
                'allocation_memory':{'limit_available':False,'available_bytes':100,
                              'outer_host_evidence':'unavailable' if failed == 'host' else 'collected by physical-owner host sink'}}
        incident_dir = case/'logs/test-vms-broker-incidents'; (incident_dir/'host').mkdir(parents=True)
        incident_path = incident_dir/(identifier+'.json'); incident_path.write_text(json.dumps(incident))
        (incident_dir/'host'/(identifier+'.json')).write_text(json.dumps(host))
        held_body = json.dumps({'pool':{'slots':[slot]}})
        quarantine_body = json.dumps({'pool':{'slots':[quarantined]}})
        available_body = json.dumps({'pool':{'slots':[available]}})
        event_body = '\n'.join(json.dumps(event) for event in (events if failed != 'events' else events[:-1]))
        script = f'''set -euo pipefail
STATE_PARENT={str(case)!r}; SUBYARD_HOME={str(case)!r}; YARD=test-yard; base={'a'*64!r}
RUNNER={runner!r}
REAPER_PAUSED=1; MARKER=subyard-p0-123; REAPER_TIMER_STOPPED=1; FAULT_INSTALLED=0; FAULT_ROOT=/run/subyard-staging-incus-fault
LOG={str(log)!r}; FAILED={failed!r}
die() {{ echo "$*" >&2; exit 2; }}
verify_broker_installation() {{ :; }}
reaper_pause() {{ echo pause_$2 >> "$LOG"; [ "$FAILED" != pause ]; }}
stop_sampler() {{ echo observer_stopped >> "$LOG"; }}
start_holder_child() {{ [ -n "$FAILED" ] || [ -s "$STATE_PARENT/recovery/id_ed25519.pub" ]; printf 'slot-001\tconfig\tproject\trun\tpurpose\t4\t%s\n' "$base" > "$STATE_PARENT/recovery.ready"; HOLDER_STARTED_PID=42; }}
recovery_monotonic_seconds() {{ echo 100; }}
status() {{ printf '%s\n' {held_body!r}; }}
wait_state() {{ if [ "$1" = quarantined ]; then printf '%s\n' {quarantine_body!r}; else printf '%s\n' {available_body!r}; fi; }}
wait() {{ echo release >> "$LOG"; [ "$FAILED" = release ]; }}
inventory_fault_config() {{ echo "$1" >> "$LOG"; [ "$FAILED" != "$1" ] && [ "$FAILED:$1" != setup:install ]; }}
collect_evidence() {{ echo capture >> "$LOG"; }}
sudo() {{
  echo sink >> "$LOG"
  if [ "$FAILED" = replay ]; then echo changed >> {str(incident_path)!r}; fi
}}
outer_exec() {{
  echo "$1" >> "$LOG"
  case "$*" in
    *volatile.uuid*) echo aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa;;
    *user.subyard.managed*) echo test-vms-v1;;
    *user.subyard.generation*) echo 4;;
    *user.subyard.lease-epoch*) echo 9;;
    'sh -eu -c '*) [ "$FAILED" != fence ];;
    'incus list '*) if [ "$FAILED" = instances ]; then echo '[{{"name":"retained"}}]'; else echo '[]'; fi;;
    'incus storage '*) if [ "$FAILED" = volumes ]; then echo '[{{"type":"virtual-machine"}}]'; else echo '[]'; fi;;
    systemctl*) :;;
    *) return 42;;
  esac
}}
runtime_yard() {{ printf '%s\n' {shlex.quote(event_body)}; }}
''' + restore_fault + '\n' + owned_vm + '\n' + recovery_phase + '\nverify_singleton_recovery\n'
        result = subprocess.run(['bash','-c',script],env=key_environment,capture_output=True,text=True,timeout=5)
        assert result.returncode == (2 if failed else 0), (failed,result)
        calls = log.read_text().splitlines()
        if not failed:
            assert (case/'holder/id_ed25519').read_bytes() == first_key
            assert (case/'recovery/id_ed25519').read_bytes() != first_key
            for key, mode in [('id_ed25519', 0o600), ('id_ed25519.pub', 0o644)]:
                assert (case/'recovery'/key).stat().st_mode & 0o777 == mode
            verified = subprocess.run(['ssh-keygen', '-lf', str(case/'recovery/id_ed25519.pub')],
                capture_output=True, text=True, timeout=5)
            assert verified.returncode == 0 and '(ED25519)' in verified.stdout, verified
            receipt = json.loads(result.stdout.split('STAGING_RECOVERY=',1)[1])
            assert receipt.pop('original_release_exit_code') == 1 and all(value is True for value in receipt.values()), receipt
            assert calls.index('release') < calls.index('consumed') < calls.index('capture') < calls.index('restore') < calls.index('systemctl'), calls
        else:
            assert 'STAGING_RECOVERY=' not in result.stdout, (failed,result.stdout)
            if failed in ['pause','setup','release','consumed','quarantine','fence','incident','host','replay','restore']:
                assert 'systemctl' not in calls, (failed,calls)
print('actual singleton recovery phase requires original failure, capture, immutable replay, restoration, fencing and native availability proofs')

# Execute the shared pre-acquire boundary: supported history must build only via
# acquire, while the established reliability diagnostic retains explicit refresh.
cold = fixture.split('if [ "$SUPPORTED_UPGRADE" = 0 ]; then\n  printf \'  [ .. ] cold post-upgrade base validation', 1)[1]
cold = 'if [ "$SUPPORTED_UPGRADE" = 0 ]; then\n  printf \'  [ .. ] cold post-upgrade base validation' + cold.split('\nstart_holder_child hold_lease holder', 1)[0]
resource_observer = re.search(r'^observe_cold_resources\(\) \{\n.*?^\}',fixture,re.MULTILINE|re.DOTALL)[0]
native_idle = {'status':'ok','pool':{'slots':[{'slot_id':'slot-001','state':'available'}]},
               'resources':{'bases':[],'memory_admission':{'physical_available_known':True,
                 'pending_builder_bytes':0,'pending_vm_bytes':0},'reserved_vm_memory_bytes':0}}
for supported, cached, success in [(1, False, True), (1, True, False), (0, False, True)]:
    with tempfile.TemporaryDirectory(dir='/tmp') as directory:
        marker = pathlib.Path(directory) / 'refresh'
        sample=copy.deepcopy(native_idle)
        sample['resources']['bases']=[{'type':'subyard-pair','fingerprint':'a'*64,'current':True}] if cached or not supported else []
        body=json.dumps(sample)
        script = f"""set -euo pipefail
SUPPORTED_UPGRADE={supported}
SUBYARD_HOME=/unused
status() {{ printf '%s' '{body}'; }}
timeout() {{ : > '{marker}'; }}
outer_exec() {{ printf '%s' '{{"properties":{{"user.subyard.base_validation":"cidata-seed-clean-v2"}},"type":"virtual-machine"}}'; }}
die() {{ exit 2; }}
""" + resource_observer + '\n' + cold
        result = subprocess.run(['bash', '-c', script], capture_output=True, text=True, timeout=5)
        assert (result.returncode == 0) == success, result
        assert marker.exists() == (not supported), result
print('supported upgrade cold boundary refuses cached bases and never refreshes before acquire')

# Run the same production observer and terminal boundary against native-shaped
# snapshots. Absent idle builder/errors are normal; unknown evidence never is.
terminal = fixture.rsplit('if [ "$SUPPORTED_UPGRADE" = 1 ]; then\n  observe_cold_resources released',1)[1]
terminal = 'if [ "$SUPPORTED_UPGRADE" = 1 ]; then\n  observe_cold_resources released' + terminal.split('\nverify_singleton_recovery',1)[0]
for phase in ['before_acquire','released']:
    valid=copy.deepcopy(native_idle)
    if phase=='released': valid['resources']['bases']=[{'type':'subyard-pair','fingerprint':'a'*64,'current':True}]
    samples=[('omitted-idle-builder-and-diagnostics',valid,True)]
    explicit=copy.deepcopy(valid); explicit['resources']['errors']=[]
    samples.append(('empty-diagnostics',explicit,True))
    for path,values in [
        (['resources'],[None,[], 'unknown']),
        (['resources','bases'],[None,{},'unknown',[None],[{'type':'subyard-pair','fingerprint':'invalid','current':True}]]),
        (['resources','memory_admission'],[None,{},'unknown']),
        (['resources','memory_admission','physical_available_known'],[None,False,'true']),
        (['resources','memory_admission','pending_builder_bytes'],[None,'0',False,-1,0.5,1]),
        (['resources','memory_admission','pending_vm_bytes'],[None,'0',False,-1,0.5,1]),
        (['resources','reserved_vm_memory_bytes'],[None,'0',False,-1,0.5,1]),
        (['resources','builder'],[None,{}, {'type':'subyard-pair','reserved_memory_bytes':2684354560}]),
        (['resources','errors'],[None,{},'unknown',['image_registry_unavailable'],['memory_telemetry_unavailable'],['future_unknown_error']]),
        (['pool','slots'],[None,[],[{'slot_id':'slot-002','state':'available'}],[{'slot_id':'slot-001','state':'provisioning'}]])]:
        for value in values:
            sample=copy.deepcopy(valid); target=sample
            for key in path[:-1]: target=target[key]
            target[path[-1]]=value
            samples.append(('.'.join(path)+'='+repr(value),sample,False))
        sample=copy.deepcopy(valid); target=sample
        for key in path[:-1]: target=target[key]
        target.pop(path[-1],None)
        # Builder and empty diagnostics are legitimate omitted native fields.
        samples.append(('missing '+'.'.join(path),sample,path[-1] in ['builder','errors']))
    if phase=='before_acquire':
        sample=copy.deepcopy(valid); sample['resources']['bases']=[{'type':'subyard-pair','fingerprint':'a'*64,'current':True}]
        samples.append(('cached-base',sample,False))
    else:
        for bases in [[],[{'type':'subyard-pair','fingerprint':'b'*64,'current':True}],
                      [{'type':'subyard-pair','fingerprint':'a'*64,'current':False}]]:
            sample=copy.deepcopy(valid); sample['resources']['bases']=bases
            samples.append(('missing-bound-current-base',sample,False))
    for label,sample,success in samples:
        script='set -euo pipefail\nSUPPORTED_UPGRADE=1; base='+('a'*64)+'\n'
        script+='status() { printf %s '+shlex.quote(json.dumps(sample))+'; }\n'
        script+='outer_exec() { :; }\ndie() { exit 2; }\n'+resource_observer+'\n'
        script+=('observe_cold_resources before_acquire\n' if phase=='before_acquire' else terminal)
        result=subprocess.run(['bash','-c',script],capture_output=True,text=True,timeout=5)
        assert (result.returncode==0)==success,(phase,label,result)
        assert ('STAGING_SUPPORTED_COLD_TERMINAL=' in result.stdout)==(phase=='released' and success),(phase,label,result)
        if success:
            observed=json.loads(result.stdout.split('STAGING_RESOURCE_OBSERVATION=',1)[1].splitlines()[0])
            assert observed['phase']==phase and observed['diagnostics_complete'] is True and observed['builder_present'] is False,observed
            assert all(type(observed[key]) is int and observed[key]==0 for key in ['pending_builder_bytes','pending_vm_bytes','reserved_vm_memory_bytes']),observed
            assert set(observed)=={'phase','slot_state','bases','builder_present','pending_builder_bytes','pending_vm_bytes','reserved_vm_memory_bytes','diagnostics_complete'},observed
            if phase=='released': assert observed['bases']==valid['resources']['bases'],observed
print('native cold/final resource observations require typed complete evidence; unknown registry/memory/counts refuse and omitted idle builder passes')

# Execute the predecessor's preparation boundary with physical peers replaced.
# The real preparation function remains shared; it runs while 0.19 is installed,
# and the native predecessor must publish an available broker before the update.
predecessor = fixture.split('  baseline_owner="$SUBYARD_HOME/runtime/current/bin/yard"\n', 1)[1].split('  # Stage only verified frozen assets', 1)[0]
predecessor = 'baseline_owner="$SUBYARD_HOME/runtime/current/bin/yard"\n' + predecessor
with tempfile.TemporaryDirectory(prefix='subyard-predecessor-broker-', dir='/tmp') as directory:
    root = pathlib.Path(directory)
    launcher = root / 'runtime/current/bin/yard'
    launcher.parent.mkdir(parents=True)
    launcher.write_text('#!/bin/bash\nprintf "yard 0.19.0\\n"\n')
    launcher.chmod(0o700)
    leaf = root / '.subyard-acceptance/runtime/config/profiles/github/git_wiring.py'
    leaf.parent.mkdir(parents=True)
    leaf.write_bytes(pathlib.Path('config/profiles/github/git_wiring.py').read_bytes())
    for failure in ['', 'version', 'worker', 'native-state', 'git-operation']:
        log = root / ('calls-' + (failure or 'success'))
        script = f"""set -euo pipefail
ROOT={str(root)!r}; SUBYARD_HOME={str(root)!r}; baseline=0.19.0; PROJECT=fixture; INSTANCE=owner
prepare_minimal_owner() {{ printf 'minimal\n' >> '{log}'; }}
runtime_yard() {{ if [ "$*" = --version ]; then printf '%s\n' 'yard {"0.22.0" if failure == "version" else "0.19.0"}'; else printf '%s\n' '{{"resources":{{"bases":[]}}}}'; fi; }}
verify_broker_installation() {{ printf 'verified\n' >> '{log}'; return {1 if failure == 'worker' else 0}; }}
outer_exec() {{
  case "$*" in
    'python3 - /home/dev off check') printf 'git-operation\n' >> '{log}'; cat >/dev/null; return {1 if failure == 'git-operation' else 0} ;;
    *'_test-vms-worker status') printf 'native-state\n' >> '{log}'; printf '%s\n' '{{"status":"ok","pool":{{"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{{"state":"{"held" if failure == "native-state" else "available"}"}}]}}}}' ;;
    *sha256sum*) printf '%s  leases.json\n' '{"a"*64}' ;;
    *) return 99 ;;
  esac
}}
incus() {{ printf '%s\n' '01234567-89ab-cdef-0123-456789abcdef'; }}
die() {{ exit 2; }}
""" + predecessor
        result = subprocess.run(['bash', '-c', script], capture_output=True, text=True, timeout=5)
        assert (result.returncode == 0) == (not failure), result
        calls = log.read_text().splitlines()
        assert calls == {'version':['minimal'], 'worker':['minimal','verified'],
                          'native-state':['minimal','verified','native-state'],
                          'git-operation':['minimal','verified','native-state','git-operation'],
                          '':['minimal','verified','native-state','git-operation']}[failure], calls
        assert ('STAGING_BASELINE_BROKER=' in result.stdout) == (failure in ['', 'git-operation']), result
print('shared minimal preparation retains authentic predecessor and requires native broker ownership before upgrade')

# Execute the candidate's real Git configuration function as the process user.
# Physical preflight uses the same leaf's CLI, which drops to native guest dev.
with tempfile.TemporaryDirectory(prefix='subyard-updater-git-', dir='/tmp') as directory:
    root = pathlib.Path(directory)
    home = root / 'home'; home.mkdir(mode=0o700)
    config = home / '.config/git/config'; config.parent.mkdir(parents=True)
    config.write_text('[user]\n\tname = Fixture\n'); config.chmod(0o600)
    original = config.read_bytes()
    import shutil
    native_git = shutil.which('git'); assert native_git
    for installed in [False, True]:
        tools = root / ('installed' if installed else 'missing'); tools.mkdir(mode=0o700)
        if installed: (tools / 'git').symlink_to(native_git)
        operation = "import pathlib,runpy,sys; leaf=runpy.run_path(sys.argv[1]); assert leaf['reconcile'](pathlib.Path(sys.argv[2]),'off','check')"
        result = subprocess.run([sys.executable, '-c', operation,
                                 str(pathlib.Path('config/profiles/github/git_wiring.py').resolve()), str(home)],
                                env={'PATH':str(tools),'HOME':str(home),'LC_ALL':'C'},
                                capture_output=True,text=True,timeout=5)
        assert (result.returncode==0)==installed, result
        if not installed: assert 'FileNotFoundError' in result.stderr and "'git'" in result.stderr,result
        assert config.read_bytes()==original and not (home/'.config/subyard/github-git.conf').exists()
print('actual updater Git configuration operation rejects missing Git and preserves native user config')

# Exercise consumed-input cleanup on real private files, including all-or-nothing
# refusal before deletion. Installed history and controller/source bytes survive.
consumed = fixture.split("<<'PYCONSUMED'",1)[1].split('\n',1)[1].split('\nPYCONSUMED',1)[0]
for fault in ['', 'symlink', 'hardlink', 'mount', 'foreign-owner', 'candidate-digest', 'baseline-digest']:
    with tempfile.TemporaryDirectory(prefix='subyard-worktree.',dir='/tmp') as directory:
        parent=pathlib.Path(directory); root=parent/'src'; owner=parent/'owner'
        runtime=root/'.subyard-acceptance/runtime'; release=root/'.subyard-acceptance/release'; baseline=owner/'baseline-0.19.0'
        for target in [runtime,release,baseline,root/'dev',owner/'installed/current',owner/'installed/previous']:
            target.mkdir(parents=True,exist_ok=True)
        for target in [runtime/'engine',release/'runtime.tar.gz',baseline/'archive',owner/'installed/current/engine',owner/'installed/previous/engine',parent/'worktree.tar.gz']:
            target.write_bytes(b'verified fixture bytes')
        digest=hashlib.sha256(b'verified fixture bytes').hexdigest()
        (root/'.subyard-acceptance/candidate.json').write_text(json.dumps({'runtime_file':'runtime.tar.gz','runtime_sha256':digest}))
        (root/'dev/verify-release-upgrades.py').write_text("import hashlib\nDIRECT_BASELINE='0.19.0'\nDIRECT_BASELINE_SHA256={'amd64':"+repr(digest)+"}\ndef baseline_assets(owner,path,version,hashes,arch):\n    assert hashlib.sha256((path/'archive').read_bytes()).hexdigest()==hashes[arch]\n")
        if fault=='symlink': (runtime/'foreign').symlink_to(owner/'installed/current')
        if fault=='hardlink': os.link(owner/'installed/current/engine',runtime/'foreign')
        if fault=='candidate-digest': (release/'runtime.tar.gz').write_bytes(b'changed')
        if fault=='baseline-digest': (baseline/'archive').write_bytes(b'changed')
        prefix=''
        if fault=='mount':
            prefix="from unittest import mock\n_original_read=pathlib.Path.read_text\ndef read(path,*a,**kw):\n    if str(path)=='/proc/self/mountinfo': return '1 0 0:1 / '+"+repr(str(runtime))+"+' rw - ext4 none rw\\n'\n    return _original_read(path,*a,**kw)\nmock.patch.object(pathlib.Path,'read_text',read).start()\n"
        if fault=='foreign-owner': prefix='from unittest import mock\nmock.patch.object(os,\'geteuid\',return_value=os.geteuid()+1).start()\n'
        program=consumed.replace('root,owner=map(pathlib.Path,sys.argv[1:])',prefix+'root,owner=map(pathlib.Path,sys.argv[1:])')
        result=subprocess.run([sys.executable,'-c',program,str(root),str(owner)],capture_output=True,text=True,timeout=5)
        assert (result.returncode==0)==(fault==''),(fault,result)
        for target in [runtime,release,baseline,parent/'worktree.tar.gz']:
            assert target.exists()==bool(fault),(fault,target,result)
        assert (root/'dev/verify-release-upgrades.py').exists()
        assert all((owner/f'installed/{history}/engine').read_bytes()==b'verified fixture bytes' for history in ['current','previous'])
        if not fault:
            receipt=json.loads(result.stdout.split('STAGING_CONSUMED_INPUTS=',1)[1])
            assert receipt['total_allocated_bytes']==sum(receipt['allocated_bytes'].values())>0,receipt

reclaim = re.search(r'^reclaim_supported_upgrade_inputs\(\) \{\n.*?^\}',fixture,re.MULTILINE|re.DOTALL)[0]
fingerprint='a'*64
for fault in ['', 'unbound','marker','identity','alias','baseline-image']:
    script=f"""set -euo pipefail
ROOT=/tmp/subyard-worktree.fixture/src; OWNER_ROOT=/tmp/owner
SUPPORTED_UPGRADE=1; SUPPORTED_UPGRADE_BOUND={0 if fault=='unbound' else 1}
OWNER_BASE_IMAGE_CREATED=1; OWNER_BASE_IMAGE_FINGERPRINT={fingerprint}
OWNER_BASELINE_IMAGES={'a'*64 if fault=='baseline-image' else ''}
p0_capacity_assert_root_marker() {{ return {1 if fault=='marker' else 0}; }}
verify_broker_installation() {{ return {1 if fault=='identity' else 0}; }}
incus() {{ printf '%s\\n' '[{{"aliases":[{{"name":"subyard-e2e-debian-13-cloud-container"}}],"fingerprint":"{'b'*64 if fault=='alias' else fingerprint}"}}]'; }}
python3() {{ cat >/dev/null; echo consumed; }}
reclaim_broker_recovery_capacity() {{ [ "$OWNER_BASE_IMAGE" = {fingerprint} ]; echo image; OWNER_BASE_IMAGE_CREATED=0; }}
"""+reclaim+'\nreclaim_supported_upgrade_inputs\n'
    result=subprocess.run(['bash','-c',script],capture_output=True,text=True,timeout=5)
    assert (result.returncode==0)==(fault==''),(fault,result)
    assert ('consumed' in result.stdout)==(fault==''),(fault,result)
    assert ('STAGING_CONSUMED_OWNER_IMAGE=' in result.stdout)==(fault==''),(fault,result)
print('verified consumed upgrade inputs reclaim allocated bytes; foreign/link/mount/digest/identity/image drift refuses before deletion')
