#!/usr/bin/env python3
"""Host-free checks for the real-boot observer's projection and cleanup."""
import json
import hashlib
import pathlib
import re
import shlex
import subprocess
import tempfile

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

cleanup = re.search(r'^cleanup\(\) \{\n.*?^\}', fixture, re.MULTILINE | re.DOTALL)[0]
for failed in ['unmask', 'start', 'evidence']:
    script = '''set +e
HOLDER_PID=''; SECOND_BOOT_ACTIVE=0; REAPER_MASKED=1; REAPER_TIMER_STOPPED=1
stop_sampler() { :; }
retire_minimal_owner() { :; }
restore_inventory_fault() { :; }
collect_evidence() { [ "$FAILED" != evidence ]; }
outer_exec() { case "$*" in *"$FAILED"*) return 42;; *) return 0;; esac; }
owner_cleanup() { exit "$?"; }
''' + f'FAILED={failed}\n' + cleanup + '\ntrue\ncleanup\n'
    result = subprocess.run(['bash', '-c', script], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=5)
    assert result.returncode == 3, (failed, result.returncode, result.stdout, result.stderr)
print('staging observer projection and cleanup checks passed')

# A failed diagnostic/export must not replace the original bootstrap failure.
script = '''set +e
HOLDER_PID=''; SECOND_BOOT_ACTIVE=0; REAPER_MASKED=0; REAPER_TIMER_STOPPED=0
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
    for failed, expected in [('prerequisites', 42), ('backend', 7), ('', 0)]:
        checkout = root/(failed or 'passed'); (checkout/'.build').mkdir(parents=True)
        log = checkout/'calls'
        script = f'''set -euo pipefail
ROOT={str(checkout)!r}; SUBYARD_HOME=/installed; PROJECT=subyard-test-yard; INSTANCE=yard-test-yard
YARD=test-yard; OWNER_BASE_IMAGE=marked-image; MARKER=subyard-p0-123; OWNER_DIAGNOSTIC_DEV_UID=1001
diagnostic_cpu=4; MINIMAL_OWNER=0; BROKER_INSTALLED=0; LOG={str(log)!r}; FAILED={failed!r}
die() {{ exit 2; }}
go() {{
  printf 'compile\\n' >> "$LOG"
  printf '#!/bin/sh\\nprintf "backend\\n" >> "$LOG"\\n[ "$FAILED" != backend ] || exit 7\\n' > "$ROOT/.build/p0-staging-owner/owner"
  chmod 0700 "$ROOT/.build/p0-staging-owner/owner"
}}
owner_adapter() {{ printf '%s\\n' "$1" >> "$LOG"; }}
incus() {{
  case "$1" in
    project) printf '%s\\n' "$MARKER";;
    list) :;;
    init) printf 'init\\n' >> "$LOG";;
    exec) printf 'cloud-init\\n' >> "$LOG";;
    *) return 42;;
  esac
}}
timeout() {{ shift 3; "$@"; }}
outer_exec() {{ printf 'prerequisites\\n' >> "$LOG"; [ "$FAILED" != prerequisites ] || return 42; }}
verify_broker_installation() {{ printf 'verify\\n' >> "$LOG"; }}
export LOG FAILED
''' + prepare_owner + '\nprepare_minimal_owner\n'
        result = subprocess.run(['bash', '-c', script], capture_output=True, text=True, timeout=5)
        assert result.returncode == expected, result
        calls = log.read_text().splitlines()
        assert calls[:6] == ['compile','02-create-project.sh','init','03-create-subyard.sh','cloud-init','prerequisites'], calls
        assert calls[6:] == ({'prerequisites': [], 'backend': ['backend','verify'], '': ['backend','verify','install-test-vms-host-sink.sh']}[failed]), calls
        assert (checkout/'.build/p0-staging-owner').exists() == bool(failed), calls
print('minimal owner prerequisites precede product backend and host sink')

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
HOLDER_PID=''; SECOND_BOOT_ACTIVE=0; REAPER_MASKED=0; REAPER_TIMER_STOPPED=0
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
    leaf.write_text(f'. {str(context)!r}\nsubyard_require_engine_context\n' + '''
test "$YARD_KIND:$NESTED_E2E_VMS:$DEV_UID:$DEV_SUDO" = container:1:1001:0
test "$INCUS_PROJECT:$YARD_INSTANCE_NAME" = subyard-test-yard:yard-test-yard
test "$SRV_VOLUME_TYPE:$VM_FREE_PAGE_REPORTING:$VM_PIN_IPV4" = filesystem:0:0
test "$SUBYARD_POWER_DESIRED:$SUBYARD_SUDO_PREAUTHORIZED" = running:1
printf 'complete typed owner context verified\\n'
''')
    script = f'''set -euo pipefail
SUBYARD_HOME={directory!r}; OWNER_CONFIG_HOME={directory!r}; OWNER_DIAGNOSTIC_DEV_UID=1001
PROJECT=subyard-test-yard; INSTANCE=yard-test-yard; YARD=test-yard; OWNER_BASE_IMAGE=marked-base
''' + adapter + '\nowner_adapter context-check.sh\n'
    result = subprocess.run(['bash', '-c', script], capture_output=True, text=True, timeout=5)
    assert result.returncode == 0 and result.stdout == 'complete typed owner context verified\n', result
print('minimal owner adapters receive the complete product context contract')

apt_indexes = fixture.split("<<'PYAPTINDEX'\n", 1)[1].split('\nPYAPTINDEX', 1)[0]
with tempfile.TemporaryDirectory(prefix='subyard-apt-index-', dir='/tmp') as directory:
    root = pathlib.Path(directory)
    for failed in ['', 'symlink', 'busy', 'linked']:
        case = root/(failed or 'success'); lists = case/'var/lib/apt/lists'; lists.mkdir(parents=True)
        locks = [case/name for name in ['var/lib/dpkg/lock-frontend','var/lib/dpkg/lock','var/lib/apt/lists/lock']]
        for lock in locks: lock.parent.mkdir(parents=True, exist_ok=True); lock.touch()
        inodes = [lock.stat().st_ino for lock in locks]
        index = lists/'repository_Packages'; index.write_text('reproducible index')
        foreign = case/'keep'; foreign.write_text('foreign bytes')
        if failed == 'symlink': (lists/'late-link').symlink_to(foreign)
        if failed == 'linked':
            import os
            os.link(foreign, lists/'late-link')
        # Fixed path literals are redirected only into this test-owned namespace.
        payload = apt_indexes.replace('/var/lib/',str(case/'var/lib')+'/')
        payload = payload.replace("root=pathlib.Path", "pwd.getpwnam=lambda name: type('APTUser',(),{'pw_uid':os.geteuid()})()\nroot=pathlib.Path",1)
        held = None
        if failed == 'busy':
            import fcntl
            held = locks[0].open('r+'); fcntl.lockf(held, fcntl.LOCK_EX|fcntl.LOCK_NB)
        result = subprocess.run(['python3', '-c', payload], capture_output=True, text=True, timeout=5)
        if held: held.close()
        assert (result.returncode != 0) == bool(failed), result
        assert index.exists() == bool(failed) and foreign.read_text() == 'foreign bytes', result
        assert [lock.stat().st_ino for lock in locks] == inodes, 'APT lock inode changed'
    reclaim_apt = re.search(r'^reclaim_owner_apt_indexes\(\) \{\n.*?^\}', fixture, re.MULTILINE | re.DOTALL)[0]
    for owned, installed in [(0,1),(1,0)]:
        script = f'''set -euo pipefail
MINIMAL_OWNER={owned}
verify_broker_installation() {{ return {0 if installed else 1}; }}
outer_exec() {{ echo unsafe_deletion; }}
''' + reclaim_apt + '\nreclaim_owner_apt_indexes\n'
        result = subprocess.run(['bash', '-c', script], capture_output=True, text=True, timeout=5)
        assert result.returncode == 1 and not result.stdout, result
print('APT indexes require native locks, canonical ownership and complete preflight; lock inodes survive')

fault_payload = fixture.split("<<'PYINVENTORYFAULT'\n", 1)[1].split('\nPYINVENTORYFAULT', 1)[0]
with tempfile.TemporaryDirectory(prefix='subyard-inventory-fault-', dir='/tmp') as directory:
    root = pathlib.Path(directory)
    config = root/'etc/subyard/test-vms.env'; config.parent.mkdir(parents=True)
    fault_root = root/'run/subyard-staging-incus-fault'; fault_root.parent.mkdir()
    native = root/'real-incus'; native.write_text('#!/bin/sh\nprintf "native:%s\\n" "$*"\n'); native.chmod(0o700)
    payload = fault_payload.replace('/run/subyard-staging-incus-fault',str(fault_root)).replace('/etc/subyard/test-vms.env',str(config))
    def fault(action, marker='subyard-p0-123'):
        return subprocess.run(['python3','-c',payload,action,marker,str(native)],capture_output=True,text=True,timeout=5)
    original = b'E2E_VM_MEMORY=2GiB\nSUBYARD_INNER_INCUS=incus\n'
    config.write_bytes(original); config.chmod(0o640)
    assert fault('install').returncode == 0
    wrapper = fault_root/'incus'
    delegated = subprocess.run([str(wrapper),'version'],capture_output=True,text=True,timeout=5)
    assert delegated.returncode == 0 and delegated.stdout == 'native:version\n'
    command = [str(wrapper),'project','list','--format','csv','-c','n']
    failed = subprocess.run(command,capture_output=True,text=True,timeout=5)
    assert failed.returncode == 1 and 'p0 targeted inventory fault' in failed.stderr
    assert subprocess.run(command,capture_output=True,text=True,timeout=5).returncode == 0
    assert fault('consumed').returncode == 0
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
HOLDER_PID=''; SECOND_BOOT_ACTIVE=0; REAPER_MASKED=1; REAPER_TIMER_STOPPED=1; FAULT_INSTALLED=1
stop_sampler() {{ :; }}
inventory_fault_config() {{ echo restore; return {failed}; }}
collect_evidence() {{ echo capture; }}
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
        assert any('unmask' in call for call in calls), calls
        assert any(' start ' in ' '+call+' ' for call in calls) == (not failed), calls
print('exit cleanup restores fault before capture/reaper/retirement and preserves failure on restoration refusal')

with tempfile.TemporaryDirectory(prefix='subyard-recovery-holder-cleanup-', dir='/tmp') as directory:
    script = f'''set -euo pipefail
STATE_PARENT={directory!r}; HOLDER_CLIENT=recovery; HOLDER_PID=42
SECOND_BOOT_ACTIVE=0; REAPER_MASKED=0; REAPER_TIMER_STOPPED=0; FAULT_INSTALLED=0
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
    for failed in ['', 'setup', 'release', 'consumed', 'quarantine', 'fence', 'incident', 'host', 'replay', 'restore', 'available', 'instances', 'volumes', 'events']:
        case = root/(failed or 'success'); case.mkdir(); log = case/'calls'
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
REAPER_MASKED=1; REAPER_TIMER_STOPPED=1; FAULT_INSTALLED=0; FAULT_ROOT=/run/subyard-staging-incus-fault
LOG={str(log)!r}; FAILED={failed!r}
die() {{ echo "$*" >&2; exit 2; }}
verify_broker_installation() {{ :; }}
stop_sampler() {{ echo observer_stopped >> "$LOG"; }}
start_holder_child() {{ printf 'slot-001\tconfig\tproject\trun\tpurpose\t4\t%s\n' "$base" > "$STATE_PARENT/recovery.ready"; HOLDER_STARTED_PID=42; }}
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
        result = subprocess.run(['bash','-c',script],capture_output=True,text=True,timeout=5)
        assert result.returncode == (2 if failed else 0), (failed,result)
        calls = log.read_text().splitlines()
        if not failed:
            receipt = json.loads(result.stdout.split('STAGING_RECOVERY=',1)[1])
            assert receipt.pop('original_release_exit_code') == 1 and all(value is True for value in receipt.values()), receipt
            assert calls.index('release') < calls.index('consumed') < calls.index('capture') < calls.index('restore') < calls.index('systemctl'), calls
        else:
            assert 'STAGING_RECOVERY=' not in result.stdout, (failed,result.stdout)
            if failed in ['setup','release','consumed','quarantine','fence','incident','host','replay','restore']:
                assert 'systemctl' not in calls, (failed,calls)
print('actual singleton recovery phase requires original failure, capture, immutable replay, restoration, fencing and native availability proofs')
