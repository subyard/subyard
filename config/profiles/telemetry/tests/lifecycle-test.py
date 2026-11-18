#!/usr/bin/env python3
import json
import hashlib
import importlib.util
import io
from contextlib import ExitStack, closing, redirect_stderr
import os
from pathlib import Path
import select
import shutil
import signal
import sqlite3
import stat
import subprocess
import sys
import tempfile
import types
import unittest
from unittest.mock import patch
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
sys.path.insert(0, str(Path(__file__).resolve().parents[4]/'internal/adapters/configmaterial'))
import guest
import records
spec = importlib.util.spec_from_file_location('profile_lifecycle', Path(__file__).parent/'e2e/profile-lifecycle.py')
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)


class LifecycleTest(unittest.TestCase):
    def test_upgrade_resumes_after_each_publication(self):
        profile = Path(guest.__file__).parent
        legacy_observer = b'''
import fcntl, pathlib, sys
with (pathlib.Path(sys.argv[1])/'manager.lock').open('rb') as lock:
    print('opened', flush=True)
    fcntl.flock(lock, fcntl.LOCK_EX)
    print('locked', flush=True)
    sys.stdin.read()
'''
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            workspace = Path(name)
            workspace.chmod(0o711)
            source = workspace/'source'
            shutil.copytree(profile, source, ignore=shutil.ignore_patterns('tests', 'collector', 'cmd', 'bin'))
            shutil.copyfile(guest.tomli_w.__file__, source/'tomli_w.py')
            binary = source/'subyard-telemetry'
            subprocess.run(['go', 'build', '-o', binary, './config/profiles/telemetry/cmd/telemetry'],
                           cwd=profile.parents[2], check=True, timeout=90)

            for step in ('stopped', 'fenced', 'asset', 'archived', 'started'):
                with self.subTest(step=step), tempfile.TemporaryDirectory(dir=workspace) as case, ExitStack() as children:
                    root = Path(case)
                    root.chmod(0o711)
                    account = types.SimpleNamespace(pw_uid=os.getuid() or 1000, pw_gid=os.getgid(), pw_dir=str(root/'home'))
                    runtime = guest.Runtime(source, 'dev', True, [], root/'control', root/'data',
                                            root/'library', root/'units', root/'bin', account)
                    control, data = runtime.control, runtime.data
                    for directory in (control, data, runtime.home, runtime.library, runtime.units, runtime.commands):
                        directory.mkdir(mode=0o700 if directory == data else 0o755)
                    os.chown(data, runtime.uid, runtime.gid)
                    (control/'marker').write_bytes(guest.MARKER)
                    installed_launcher = runtime.library/'launcher.py'
                    for path, (_, mode) in runtime.assets.items():
                        if path.name != 'record-policy.txt':
                            path.write_bytes(legacy_observer if path == installed_launcher else b'previous '+path.name.encode())
                            path.chmod(mode)
                    (control/'assets.json').write_text(json.dumps({
                        str(path): hashlib.sha256(path.read_bytes()).hexdigest() for path in runtime.assets if path.exists()}))

                    def developer(*command):
                        if os.getuid() != runtime.uid:
                            return ['/usr/bin/setpriv', '--reuid', str(runtime.uid), '--regid', str(runtime.gid), '--clear-groups', *command]
                        return command

                    def child(*command):
                        process = children.enter_context(subprocess.Popen(developer(*command), stdin=subprocess.PIPE, stdout=subprocess.PIPE))
                        children.callback(lambda: process.kill() if process.poll() is None else None)
                        return process

                    # Use real SQLite and flock state. Only systemd operations
                    # are substituted so the fixture runs without a system manager.
                    initialization = '''
from contextlib import closing
import json, os, pathlib, sqlite3, sys
root = pathlib.Path(sys.argv[1])
(root/'runs').mkdir(mode=0o700)
(root/'runs/legacy-run.json').write_text(json.dumps({'agent_run_id': 'legacy-run'}))
(root/'runs/legacy-run.json').chmod(0o600)
(root/'manager.lock').touch(mode=0o600)
(root/'outbox').mkdir(mode=0o700)
path = root/'telemetry.sqlite'
with closing(sqlite3.connect(path)) as db:
    db.executescript("""
create table metadata(key text primary key, value text);
insert into metadata values('origin_id', 'released');
create table runs(agent_run_id text, body text, source_time text);
insert into runs values('legacy-run', '{"state":"starting"}', '');
create table associations(object_id text, body text, start_time text, end_time text, shared integer, quality text, agent_run_id text);
create table objects(object_id text, parent_object_id text, accounting text, body text, kind text);
create table records(origin_id text, event_id text, kind text);
insert into records values('released', 'last-measurement', 'sample');
create table samples(object_id text);
create table relations(agent_run_id text, session_id text, origin_id text, event_id text, body text);
create table gaps(agent_run_id text, object_id text, source_time text, body text);
pragma user_version=2;
""")
os.chmod(path, 0o600)
'''
                    subprocess.run(developer(sys.executable, '-Bc', initialization, data), check=True, timeout=5)
                    children.callback((data/'runs').chmod, 0o700)
                    workload = child(sys.executable, '-Bc', 'import sys; sys.stdin.read()')
                    observer = child(sys.executable, '-B', installed_launcher, data)
                    self.assertTrue(select.select([observer.stdout], [], [], 5)[0])
                    self.assertEqual(observer.stdout.readline(), b'opened\n')
                    self.assertEqual(observer.stdout.readline(), b'locked\n')
                    waiting = child(sys.executable, '-B', installed_launcher, data)
                    self.assertTrue(select.select([waiting.stdout], [], [], 5)[0])
                    self.assertEqual(waiting.stdout.readline(), b'opened\n')

                    state_path = root/'services.json'
                    units = (guest.COLLECTOR, guest.MANAGER)
                    state_path.write_text(json.dumps({unit: dict(LoadState='loaded', ActiveState='active', UnitFileState='enabled')
                                                      for unit in units}))
                    runtime.manager = lambda unit: json.loads(state_path.read_text())[unit]
                    runtime.running_no_new_privs = lambda fields: True
                    runtime.skill_delivery = lambda mode: True
                    stops = []

                    def cut(boundary):
                        if interrupted and step == boundary:
                            os.kill(os.getpid(), signal.SIGKILL)

                    def systemctl(operation, *arguments):
                        if operation == 'daemon-reload':
                            return
                        states = json.loads(state_path.read_text())
                        for unit in arguments:
                            if unit == '--now':
                                continue
                            if operation in ('enable', 'disable'):
                                states[unit]['UnitFileState'] = operation + 'd'
                            if operation == 'stop':
                                stops.append(unit)
                                states[unit]['ActiveState'] = 'inactive'
                            if operation == 'enable' and '--now' in arguments:
                                states[unit]['ActiveState'] = 'active'
                        state_path.write_text(json.dumps(states))
                        if operation == 'enable':
                            cut('started')

                    def after(operation, boundary):
                        def run(*args, **kwargs):
                            operation(*args, **kwargs)
                            cut(boundary)
                        return run

                    def publication(path, *args, **kwargs):
                        publish(path, *args, **kwargs)
                        if path == runtime.library/'records.py':
                            cut('asset')

                    publish = guest.native.publish
                    runtime.prepare_store = after(runtime.prepare_store, 'archived')
                    as_developer = runtime.as_developer

                    def developer_step(operation):
                        as_developer(operation)
                        cut('stopped' if operation == runtime.quiesce_legacy_observers else 'fenced')

                    runtime.as_developer = developer_step

                    with patch.object(guest, 'systemctl', side_effect=systemctl), \
                         patch.object(guest.native, 'reconcile', return_value=True), \
                         patch.object(guest.native, 'publish', side_effect=publication):
                        interrupted = True
                        pid = os.fork()
                        if pid == 0:
                            try:
                                runtime.apply()
                            finally:
                                os._exit(1)
                        _, status = os.waitpid(pid, 0)
                        self.assertEqual(os.waitstatus_to_exitcode(status), -signal.SIGKILL)
                        self.assertEqual(runtime.observe()['state'], 'stale')
                        self.assertEqual(observer.wait(timeout=5), -signal.SIGKILL)
                        self.assertEqual(waiting.wait(timeout=5), -signal.SIGKILL)
                        self.assertIsNone(workload.poll())
                        archive = data/'archive/telemetry.sqlite'
                        outbox = data/'outbox'
                        self.assertEqual(stat.S_IMODE(outbox.stat().st_mode), 0o700 if step == 'stopped' else 0o1700)
                        self.assertEqual(archive.exists(), step in ('archived', 'started'))
                        for unit in units:
                            self.assertEqual(runtime.manager(unit)['ActiveState'], 'active' if step == 'started' else 'inactive')
                        interrupted = False
                        runtime.apply()
                        self.assertEqual(runtime.observe()['state'], 'current')
                        self.assertEqual(stops, [] if step == 'started' else list(units))
                    self.assertIsNone(workload.poll())
                    self.assertEqual(stat.S_IMODE(archive.stat().st_mode), 0o400)
                    with closing(sqlite3.connect(data/'telemetry.sqlite')) as db:
                        self.assertEqual(db.execute('PRAGMA user_version').fetchone(), (4,))
                        self.assertEqual(db.execute('SELECT count(*) FROM records').fetchone(), (0,))
                    with closing(sqlite3.connect(archive.as_uri()+'?mode=ro', uri=True)) as db:
                        self.assertEqual(db.execute('SELECT event_id FROM records').fetchone(), ('last-measurement',))
                    self.assertTrue((data/'runs/legacy-run.json').exists())
                    subprocess.run(developer(sys.executable, '-Bc',
                        'import sys; sys.path.insert(0, sys.argv[1]); import launcher; launcher.observe(sys.argv[2], once=True)',
                        runtime.library, data), check=True, timeout=5)
                    self.assertFalse(list((data/'runs-v4').glob('*.json')))

    def test_running_legacy_producer_cannot_publish_after_upgrade(self):
        # Keep the pre-upgrade lock open across publication, as a producer that
        # passed its first directory check just before the upgrade could do.
        code = """
import fcntl, os, pathlib, stat, sys
outbox = pathlib.Path(sys.argv[1])
with (outbox/'.lock').open('r+b') as lock:
    assert stat.S_IMODE(outbox.stat().st_mode) == 0o700
    print('ready', flush=True)
    sys.stdin.read()
    fcntl.flock(lock, fcntl.LOCK_EX)
    if stat.S_IMODE(outbox.stat().st_mode) == 0o700:
        (outbox/'late.json').write_text('{"agent_run_id":"raw-legacy-run"}')
"""
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            data = Path(name)
            self.assertTrue(records.emit(data, 'gap', {}, event_id='pending'))
            outbox = data/'outbox'
            pending = list(outbox.glob('*.json'))
            with subprocess.Popen([sys.executable, '-B', '-c', code, str(outbox)],
                                  stdin=subprocess.PIPE, stdout=subprocess.PIPE) as old:
                self.assertEqual(old.stdout.readline().strip(), b'ready')
                runtime = object.__new__(guest.Runtime)
                runtime.data, runtime.uid, runtime.gid = data, os.getuid(), os.getgid()
                runtime.as_developer(runtime.fence_outbox)
                old.communicate(timeout=3)
                self.assertEqual(old.returncode, 0)
            self.assertCountEqual(outbox.glob('*.json'), pending)
            self.assertTrue(records.emit(data, 'run', {'agent_run_id': 'new-run'}, event_id='new'))
            self.assertEqual(len(list(outbox.glob('*.json'))), 2)

    def test_missing_managed_document_waits_before_first_telemetry_install(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            runtime = object.__new__(guest.Runtime)
            runtime.selected, runtime.agents, runtime.managed_agents = True, ['claude', 'codex'], {'claude', 'codex'}
            runtime.home, runtime.control, runtime.library, runtime.data = root, root/'control', root/'library', root/'data'
            runtime.assets, runtime.retained = {}, {}
            runtime.owned = lambda: False
            runtime.desired = lambda: 'a'*64
            runtime.manager = lambda unit: {}
            observed = runtime.observe()
            self.assertEqual(observed['state'], 'pending')
            self.assertEqual([item['parameter'] for item in observed['diagnostics']],
                             ['telemetry.agent.claude.config.missing', 'telemetry.agent.codex.config.missing'])
            with self.assertRaisesRegex(ValueError, 'config apply'):
                runtime.apply()
            self.assertEqual(list(root.iterdir()), [], 'pending managed inputs allowed partial installation')

    def test_invalid_agent_document_is_pending_until_config_repair(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            runtime = object.__new__(guest.Runtime)
            asset = root/'collector'
            asset.write_bytes(b'stale collector')
            runtime.assets, runtime.retained = {asset: (b'current collector', 0o755)}, {}
            runtime.selected, runtime.agents = True, ['claude']
            runtime.managed_agents = {'claude'}
            runtime.home, runtime.control = root, root/'control'
            runtime.library = root/'library'
            runtime.endpoint, runtime.uid, runtime.gid = 'http://127.0.0.1:14318', os.getuid(), os.getgid()
            runtime.owned = lambda: True
            runtime.saved_inventory = lambda: {}
            runtime.manager = lambda unit: {'ActiveState': 'active', 'UnitFileState': 'enabled'}
            runtime.running_no_new_privs = lambda state: True
            runtime.skill_delivery = lambda mode: True
            runtime.desired = lambda: 'a'*64
            runtime.token = lambda: 'x'*64
            (root/'.claude').mkdir()
            (root/'.claude/settings.json').write_bytes(b'private invalid content')
            observation = runtime.observe()
            self.assertEqual(observation['state'], 'pending')
            self.assertEqual(len(observation['actual']), 64)
            self.assertEqual(observation['desired'], 'a'*64)
            self.assertIn('telemetry.agent.claude.config.invalid', str(observation['diagnostics']))
            self.assertNotIn('private invalid content', str(observation))
            (root/'.claude/settings.json').write_text('{}')
            self.assertEqual(runtime.observe()['state'], 'stale', 'repairing the document must reveal pending collector refresh')

    def test_runtime_diagnostics_name_changed_asset_without_contents(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            path = Path(name)/'collector.py'
            path.write_bytes(b'private observed content')
            runtime = object.__new__(guest.Runtime)
            runtime.assets = {path: (b'desired content', 0o644)}
            runtime.retained = {}
            runtime.selected = True
            runtime.home, runtime.agents = Path(name), []
            runtime.control = Path(name)
            runtime.owned = lambda: True
            runtime.asset_snapshot = lambda path: path.read_bytes()
            runtime.saved_inventory = lambda: {str(path): hashlib.sha256(path.read_bytes()).hexdigest()}
            runtime.manager = lambda unit: {'ActiveState': 'active', 'UnitFileState': 'enabled'}
            runtime.running_no_new_privs = lambda state: True
            runtime.skill_delivery = lambda mode: True
            runtime.desired = lambda: 'a'*64
            observation = runtime.observe()
            self.assertIn({'parameter': 'telemetry.asset.collector.py', 'installed': 'changed', 'desired': 'current'}, observation['diagnostics'])
            self.assertNotIn('private observed content', str(observation))

    def test_running_child_no_new_privs_uses_actual_kernel_status_and_uid(self):
        process=subprocess.Popen(['/usr/bin/setpriv','--no-new-privs',sys.executable,'-c',
            "import sys;print('ready',flush=True);sys.stdin.read()"],stdin=subprocess.PIPE,stdout=subprocess.PIPE)
        try:
            self.assertEqual(process.stdout.readline(),b'ready\n')
            runtime=types.SimpleNamespace(uid=os.getuid())
            fields=dict(ActiveState='active',MainPID=str(process.pid))
            self.assertTrue(guest.Runtime.running_no_new_privs(runtime,fields))
            runtime.uid+=1
            self.assertFalse(guest.Runtime.running_no_new_privs(runtime,fields))
            self.assertFalse(guest.Runtime.running_no_new_privs(runtime,dict(ActiveState='active',MainPID='0')))
        finally:
            process.communicate(timeout=3)

    def test_type_wide_incuseffective_envelope_admits_only_fixed_startup(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            units=Path(name)
            command='/usr/bin/setpriv --no-new-privs /usr/local/lib/subyard-telemetry/subyard-telemetry serve'
            runtime=types.SimpleNamespace(units=units,user='dev',gid=1000,starts={guest.COLLECTOR:command},
                assets={units/guest.COLLECTOR:(b'known unit',0o644)})
            fields=dict(LoadState='loaded',FragmentPath=str(units/guest.COLLECTOR),User='dev',Group='1000',
                Type='simple',KillMode='control-group',UMask='0077',LimitCORE='0',NoNewPrivileges='no',
                DropInPaths='/run/systemd/system/service.d/zzz-lxc-service.conf',MemoryMax='268435456',TasksMax='64',
                CPUQuotaPerSecUSec='250ms',ExecStart='{ path=/usr/bin/setpriv ; argv[]='+command+' ; ignore_errors=no ; start_time=x ; }')
            with patch.object(guest,'systemctl',return_value='\n'.join(key+'='+value for key,value in fields.items())):
                observed=guest.Runtime.manager(runtime,guest.COLLECTOR)
            self.assertEqual(observed['NoNewPrivileges'],'no')
            fields.update(ActiveState='activating',SubState='auto-restart',NoNewPrivileges='yes')
            with patch.object(guest,'systemctl',return_value='\n'.join(key+'='+value for key,value in fields.items())):
                self.assertEqual(guest.Runtime.manager(runtime,guest.COLLECTOR)['ActiveState'],'failed')

    def test_unit_failure_projection_reports_predicates_without_private_native_fields(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            binary=Path(name)/'systemctl'
            binary.write_text('''#!/usr/bin/env python3
import sys
unit=sys.argv[2]
fields=dict(LoadState='loaded',FragmentPath='/etc/systemd/system/'+unit,User='dev',Group='1000',
    DropInPaths='/run/systemd/system/service.d/PRIVATE_FILE.conf',NoNewPrivileges='no',Type='simple',
    KillMode='process' if unit.endswith('-runs.service') else 'control-group',UMask='0077',LimitCORE='0',
    ExecStart='PRIVATE_COMMAND',Environment='PRIVATE_ENV',EnvironmentFiles='/private/config')
print('\\n'.join(key+'='+value for key,value in fields.items()))
''')
            binary.chmod(0o755)
            result=subprocess.run([sys.executable,'-B','-','dev','1000'],input=fixture.UNIT_FACTS.encode(),
                capture_output=True,check=True,timeout=5,env=dict(os.environ,PATH=name+os.pathsep+os.environ['PATH']))
            document=json.loads(result.stdout)
            self.assertEqual(len(document['units']),2)
            for unit in document['units']:
                self.assertEqual(unit['dropins'],{'type-wide':1,'unit-specific':0,'other':0})
                self.assertTrue(unit['expected_matches']['FragmentPath'])
                self.assertTrue(unit['expected_matches']['User'])
                self.assertFalse(unit['expected_matches']['NoNewPrivileges'])
                self.assertFalse(unit['expected_matches']['ExecStart'])
                self.assertFalse(unit['expected_matches']['Environment'])
                self.assertTrue(all(type(value) is bool for value in unit['expected_matches'].values()))
            self.assertNotIn(b'PRIVATE',result.stdout)
            self.assertNotIn(b'/private',result.stdout)

    def test_profile_cli_stderr_summary_preserves_error_words_and_redacts_values(self):
        private = ['SENSITIVE_HEADER','SENSITIVE_CAP','SENSITIVE_TOKEN','SENSITIVE_ENV','SENSITIVE_ARGV',
                   'SENSITIVE_CONFIG','SENSITIVE_USER','SENSITIVE_PASSWORD','SENSITIVE_QUERY','SENSITIVE_PATH',
                   '0123456789abcdef0123456789abcdef']
        output = ('yard: cannot enable profile because required setting is missing\n'
                  'Authorization: Bearer SENSITIVE_HEADER\nX-Private-Capability: SENSITIVE_CAP\n'
                  'token=SENSITIVE_TOKEN CUSTOM_ENV=SENSITIVE_ENV\nargv: [SENSITIVE_ARGV]\n'
                  'config: {private: SENSITIVE_CONFIG}\n'
                  'https://SENSITIVE_USER:SENSITIVE_PASSWORD@example.invalid/path?key=SENSITIVE_QUERY\n'
                  '/private/SENSITIVE_PATH 0123456789abcdef0123456789abcdef\n')
        argv = [sys.executable,'-c','import sys;sys.stdout.write(sys.argv[1]);sys.stderr.write(sys.argv[1]);sys.exit(7)',output]
        for operation in ('profile-enable','profile-disable','guest-command','installed-owner-observe'):
            with self.subTest(operation=operation), self.assertRaises(fixture.CommandFailure) as caught:
                fixture.command(argv,operation=operation)
            facts = caught.exception.facts
            if operation in ('profile-enable','profile-disable'):
                self.assertIn('cannot enable profile because required setting is missing',facts['stderr_summary'])
                self.assertLessEqual(len(facts['stderr_summary']),1024)
            else:
                self.assertNotIn('stderr_summary',facts)
            for value in private:
                self.assertNotIn(value,str(caught.exception))
        self.assertLessEqual(len(fixture.profile_stderr_summary(b'ordinary error '*1000)),1024)
        self.assertEqual(fixture.profile_stderr_summary(b'cannot enable profile\nconfig:\nshort-private-value\nmore-private-data'),
                         'cannot enable profile <details>')

    def test_failed_enable_observes_same_installed_owner_once_without_engine_guard(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            engine = root/'bin/yard'
            owner = root/'config/profiles/telemetry/owner.py'
            engine.parent.mkdir()
            engine.write_text('synthetic candidate')
            owner.parent.mkdir(parents=True)
            owner.write_text("import os,sys\nassert sys.argv[1:]==['--runtime','observe']\nassert os.environ['INCUS_PROJECT']=='fixture-project'\nassert os.environ['YARD_INSTANCE_NAME']=='fixture-instance'\nsys.stderr.write('PRIVATE_CREDENTIAL /private/config\\ntelemetry: local reconciliation failed (PermissionError at native.py:28)\\n');sys.exit(7)\n")
            context = dict(INCUS_PROJECT='fixture-project',YARD_INSTANCE_NAME='fixture-instance',DEV_USER='dev',
                           ENVIRONMENT_PROFILES='android',CODING_TOOL_INTEGRATIONS='')
            args = types.SimpleNamespace(engine=engine,project='fixture-project',instance='fixture-instance')
            stderr = io.StringIO()
            with patch.object(fixture,'command',wraps=fixture.command) as called, redirect_stderr(stderr):
                fixture.observe_enable_failure(args, context.__getitem__)
            called.assert_called_once()
            self.assertEqual(called.call_args.args[0], [sys.executable,'-B',str(owner),'--runtime','observe'])
            self.assertNotIn('SUBYARD_ENGINE_CONTEXT', {key:value for key,value in called.call_args.kwargs['env'].items()
                                                      if key not in os.environ})
            result = json.loads(stderr.getvalue())
            self.assertEqual(result['command']['stderr_classes'], ['PermissionError'])
            self.assertEqual(result['command']['stderr_locations'], ['native.py:28'])
            self.assertNotIn('PRIVATE', stderr.getvalue())

    def test_real_command_failure_is_redacted_and_cleanup_preserves_primary(self):
        secret = 'Bearer PRIVATE_CREDENTIAL /private/config PRIVATE_ARGV'
        argv = [sys.executable, '-c', 'import sys;sys.stderr.write(sys.argv[1]);sys.exit(7)',
                'permission denied '+secret]
        with self.assertRaises(fixture.CommandFailure) as caught:
            fixture.command(argv, operation='profile-enable')
        primary = caught.exception
        self.assertEqual(primary.facts['exit_code'], 7)
        self.assertEqual(primary.facts['stderr_signals'], ['permission denied'])
        self.assertNotIn(secret, str(primary))
        self.assertNotIn('PRIVATE', str(primary))
        stderr = io.StringIO()
        with redirect_stderr(stderr):
            fixture.cleanup([('disable', lambda: fixture.command(argv, operation='profile-disable'))], primary)
        diagnostic = json.loads(stderr.getvalue())
        self.assertEqual(diagnostic['operation'], 'disable')
        self.assertEqual(diagnostic['command']['exit_code'], 7)
        self.assertNotIn('PRIVATE', stderr.getvalue())
        self.assertEqual(primary.facts['operation'], 'profile-enable')
        with redirect_stderr(io.StringIO()), self.assertRaises(fixture.CommandFailure):
            fixture.cleanup([('disable', lambda: fixture.command(argv, operation='profile-disable'))], None)

    def test_apply_repeat_pause_disable_preserves_data_and_never_stops_workloads(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            if os.getuid() == 0:
                root.chmod(0o711)
            source = root/'source'
            source.mkdir()
            for path in Path(guest.__file__).parent.glob('*.py'):
                shutil.copyfile(path, source/path.name)
            shutil.copyfile(Path(guest.__file__).with_name('record-policy.txt'), source/'record-policy.txt')
            shutil.copyfile(Path(guest.tomli_w.__file__), source/'tomli_w.py')
            # Match the compiled collector's observed 25 MiB size, not a tiny stub.
            (source/'subyard-telemetry').write_bytes(b'\x7fELF'+b'\0'*(25_333_922-4))
            for directory in ('home', 'units', 'bin'):
                (root/directory).mkdir()
            account = types.SimpleNamespace(pw_uid=os.getuid() or 1000, pw_gid=os.getgid(), pw_dir=str(root/'home'))
            states, calls = {}, []
            registered = False
            def skill_delivery(mode):
                nonlocal registered
                if mode == 'apply':
                    registered = True
                elif mode == 'remove':
                    registered = False
                return registered
            def manager(*args):
                calls.append(args)
                if args[0] == 'show':
                    unit = args[1]
                    if unit not in states:
                        return 'LoadState=not-found'
                    command=next(line.removeprefix('ExecStart=') for line in (root/'units'/unit).read_text().splitlines() if line.startswith('ExecStart='))
                    unit_user=next(line.removeprefix('User=') for line in (root/'units'/unit).read_text().splitlines() if line.startswith('User='))
                    fields=dict(LoadState='loaded',FragmentPath=str(root/'units'/unit),User=unit_user,Group=str(account.pw_gid),
                        Type='simple',UMask='0077',LimitCORE='0',NoNewPrivileges='yes',DropInPaths='',
                        ExecStart='{ path='+command.split()[0]+' ; argv[]='+command+' ; ignore_errors=no ; start_time=x ; }',
                        KillMode='process' if unit==guest.MANAGER else 'control-group',Delegate='yes',
                        MemoryMax=str(256*1024*1024),TasksMax='64',CPUQuotaPerSecUSec='250ms')
                    fields.update(states[unit])
                    return '\n'.join(f'{key}={value}' for key,value in fields.items())
                if args[0] == 'enable':
                    for unit in args[2:]:
                        if states.get(unit, {}).get('ActiveState') != 'active':
                            states[unit] = dict(ActiveState='active', UnitFileState='enabled')
                if args[0] == 'disable':
                    for unit in args[1:]:
                        if unit.startswith('--'):
                            continue
                        states[unit]['UnitFileState'] = 'disabled'
                        if '--now' in args:
                            states[unit]['ActiveState'] = 'inactive'
                if args[0] == 'daemon-reload':
                    for state in states.values():
                        state.pop('ExecStart', None)
                if args[0] in ('stop','start'):
                    for unit in args[1:]:states[unit]['ActiveState']='active' if args[0]=='start' else 'inactive'
                return ''
            with patch.object(guest.pwd, 'getpwnam', return_value=account), patch.object(guest, 'systemctl', side_effect=manager), \
                 patch.object(guest.Runtime, 'prepare_store'), \
                 patch.object(guest.Runtime, 'skill_delivery', side_effect=skill_delivery), \
                 patch.object(guest.Runtime,'running_no_new_privs',side_effect=lambda fields: fields.get('ProcessNNP','yes')=='yes'):
                runtime = guest.Runtime(source, 'dev', True, ['codex'], root/'control', root/'data', root/'library', root/'units', root/'bin')
                for command in runtime.starts.values():
                    self.assertEqual(command.split()[:2],['/usr/bin/setpriv','--no-new-privs'])
                    actual=subprocess.check_output(command.split()[:2]+[sys.executable,'-c',
                        "from pathlib import Path;print(next(line.split(':',1)[1].strip() for line in Path('/proc/self/status').read_text().splitlines() if line.startswith('NoNewPrivs:')))"])
                    self.assertEqual(actual.strip(),b'1')
                self.assertEqual(runtime.observe()['state'], 'stale')
                runtime.apply()
                self.assertTrue(registered)
                observed=runtime.observe()
                self.assertEqual(observed['state'], 'current')
                self.assertEqual(observed['actual'],observed['desired'],'current runtime must satisfy the generic activation digest contract')
                self.assertEqual(runtime.startup(), {'state': 'ready'})
                registered = False
                self.assertIn({'parameter': 'telemetry.skill', 'installed': 'absent', 'desired': 'registered'},
                              runtime.observe()['diagnostics'])
                runtime.apply()
                self.assertTrue(registered)
                for unit in (guest.COLLECTOR, guest.MANAGER):
                    states[unit].update(ActiveState='inactive', Job='17')
                    self.assertEqual(runtime.startup()['state'], 'starting')
                    states[unit].update(ActiveState='active', Job='')
                states[guest.COLLECTOR]['ActiveState'] = 'inactive'
                self.assertEqual(runtime.startup(), {'state': 'drift'})
                states[guest.COLLECTOR]['ActiveState'] = 'failed'
                states[guest.MANAGER].update(ActiveState='inactive', Job='23')
                self.assertEqual(runtime.startup()['state'], 'starting')
                states[guest.MANAGER].update(ActiveState='active', Job='')
                self.assertEqual(runtime.startup(), {'state': 'drift'})
                states[guest.COLLECTOR].update(ActiveState='activating', SubState='auto-restart')
                self.assertEqual(runtime.startup(), {'state': 'drift'})
                for budget in ('30000', '0'):
                    with patch.dict(os.environ, {'SUBYARD_STARTUP_REMAINING_MS':budget}):
                        self.assertEqual(runtime.startup()['state'], 'starting')
                states[guest.COLLECTOR]['SubState'] = 'running'
                states[guest.COLLECTOR]['ActiveState'] = 'active'
                marker = root/'data/retained.db'
                marker.write_bytes(b'history')
                # Recover an active interrupted previous install. Unit text and
                # manifest are exactly the known pre-setpriv version.
                saved=json.loads((root/'control/assets.json').read_text())
                for unit in (guest.COLLECTOR,guest.MANAGER):
                    path=root/'units'/unit
                    legacy=runtime.assets[path][0].replace(b'/usr/bin/setpriv --no-new-privs ',b'',1)
                    path.write_bytes(legacy)
                    saved[str(path)]=hashlib.sha256(legacy).hexdigest()
                    states[unit].update(DropInPaths='/run/systemd/system/service.d/zzz-lxc-service.conf',NoNewPrivileges='no')
                    command = runtime.starts[unit].removeprefix('/usr/bin/setpriv --no-new-privs ')
                    states[unit]['ExecStart'] = '{ path='+command.split()[0]+' ; argv[]='+command+' ; ignore_errors=no ; }'
                (root/'control/assets.json').write_text(json.dumps(saved))
                self.assertEqual(runtime.observe()['state'],'stale')
                self.assertEqual(runtime.startup(), {'state': 'ready'})
                publish = guest.native.publish

                def interrupt_legacy_unit(path, *args, **kwargs):
                    publish(path, *args, **kwargs)
                    if path == runtime.units/guest.COLLECTOR:
                        raise InterruptedError

                with patch.object(guest.native, 'publish', side_effect=interrupt_legacy_unit), self.assertRaises(InterruptedError):
                    runtime.apply()
                self.assertEqual(runtime.observe()['state'], 'stale')
                runtime.apply()
                self.assertIn(('stop',guest.COLLECTOR,guest.MANAGER),calls)
                self.assertEqual(runtime.observe()['state'],'current')
                stops=sum(call[0]=='stop' for call in calls)
                runtime.apply()
                self.assertEqual(sum(call[0]=='stop' for call in calls),stops)
                # A restart interrupted after new files/manifest were committed
                # must still retry the old active NNP0 process on unchanged files.
                states[guest.COLLECTOR]['ProcessNNP']='no'
                stale=runtime.observe()
                self.assertEqual(stale['state'],'stale')
                self.assertNotEqual(stale['actual'],stale['desired'])
                runtime.apply()
                self.assertEqual(calls[-3:], [('stop',guest.COLLECTOR), ('daemon-reload',),
                                             ('enable','--now',guest.COLLECTOR,guest.MANAGER)])
                self.assertEqual(runtime.observe()['state'],'current')
                for changed in ({'User':'root'},{'DropInPaths':'/etc/systemd/system/subyard-.service.d/foreign.conf'},
                                {'DropInPaths':'/etc/systemd/system/'+guest.COLLECTOR+'.d/foreign.conf'},
                                {'KillMode':'process'},{'MemoryMax':'infinity'},{'Environment':'PRIVATE=unsafe'},
                                {'ExecStart':'{ path=/bin/false ; argv[]=/bin/false ; ignore_errors=no ; }'}):
                    original=dict(states[guest.COLLECTOR])
                    states[guest.COLLECTOR].update(changed)
                    with self.assertRaises(ValueError):runtime.apply()
                    states[guest.COLLECTOR]=original
                # Replacing files used by running services must start new processes
                # even when the service definitions are unchanged.
                for asset in ('records.py', 'subyard-telemetry'):
                    installed = runtime.library/asset
                    before, mode = runtime.assets[installed]
                    (source/asset).write_bytes(before + b'\n')
                    runtime.assets[installed] = before + b'\n', mode
                start = len(calls)
                runtime.apply()
                upgrade_calls = calls[start:]
                self.assertLess(upgrade_calls.index(('stop', guest.COLLECTOR, guest.MANAGER)),
                                upgrade_calls.index(('enable', '--now', guest.COLLECTOR, guest.MANAGER)))
                self.assertEqual(runtime.observe()['state'], 'current')

                start = len(calls)
                runtime.selected = False
                runtime.apply()
                self.assertFalse(registered)
                observed=runtime.observe()
                self.assertEqual(observed['state'], 'current')
                self.assertEqual(observed['actual'],observed['desired'])
                self.assertEqual(marker.read_bytes(), b'history')
                self.assertTrue((root/'data/drain.json').exists())
                # A fresh unselected run, packaged or from plain source, needs no
                # collector binary and keeps the installed one unchanged.
                bare = root/'bare-source'
                shutil.copytree(source, bare)
                (bare/'subyard-telemetry').unlink()
                installed_binary = root/'library/subyard-telemetry'
                kept = installed_binary.read_bytes()
                runs = {name: guest.Runtime(path, 'dev', False, ['codex'], root/'control', root/'data', root/'library', root/'units', root/'bin')
                        for name, path in (('source', bare), ('packaged', source))}
                for unselected in runs.values():
                    self.assertNotIn(installed_binary, unselected.assets)
                self.assertEqual(runs['source'].observe(), runs['packaged'].observe())
                inventory_path = root/'control/assets.json'
                saved = json.loads(inventory_path.read_text())
                stale = {**saved, str(root/'library/records.py'): '0'*64}
                inventory_path.write_text(json.dumps(stale))
                for unselected in runs.values():
                    observation = unselected.observe()
                    self.assertEqual(observation['state'], 'stale')
                    self.assertIn('telemetry.inventory', [item['parameter'] for item in observation['diagnostics']])
                installed_records = root/'library/records.py'
                candidate = installed_records.read_bytes()
                installed_records.write_bytes(b'unmanaged edit')
                with self.assertRaisesRegex(ValueError, 'managed asset changed'):
                    runs['source'].apply()
                installed_records.write_bytes(candidate)
                runs['source'].apply()
                observed = runs['source'].observe()
                self.assertEqual(observed['state'], 'current')
                self.assertEqual(observed['actual'], observed['desired'])
                self.assertEqual(observed, runs['packaged'].observe())
                self.assertEqual(installed_binary.read_bytes(), kept)
                self.assertEqual(json.loads((root/'control/assets.json').read_text())[str(installed_binary)],
                                 hashlib.sha256(kept).hexdigest())
                self.assertEqual(states[guest.MANAGER]['ActiveState'], 'active')
                self.assertFalse(any(call[0] == 'stop' and guest.MANAGER in call for call in calls[start:]))
                self.assertTrue((root/'bin/subyard-telemetry-run').read_text().startswith('#!/bin/sh\n'))
                self.assertNotIn('analytics', (root/'home/.codex/config.toml').read_text())
                token=(root/'data/auth.token').read_bytes()
                native_receipt=(root/'control/native.json').read_bytes()
                target=dict(user='next',uid=runtime.uid,gid=runtime.gid,home=str(runtime.home),group='next')
                recipe=runtime.identity_observe(target,'a'*64)
                runtime.identity_apply(recipe,'quiesce')
                self.assertEqual(states[guest.COLLECTOR]['ActiveState'],'inactive')
                self.assertEqual(states[guest.MANAGER]['ActiveState'],'inactive')
                original_replace=guest.native.os.replace
                def cut_after_unit_replace(source,path):
                    original_replace(source,path)
                    if Path(path)==runtime.units/guest.COLLECTOR:
                        raise InterruptedError('after first identity unit publication')
                with patch.object(guest.native.os,'replace',side_effect=cut_after_unit_replace):
                    with self.assertRaises(InterruptedError):runtime.identity_apply(recipe,'rebind')
                self.assertEqual((runtime.units/guest.COLLECTOR).stat().st_mode & 0o777,0o644)
                for _ in range(2):runtime.identity_apply(recipe,'rebind')
                self.assertEqual(states[guest.COLLECTOR]['ActiveState'],'inactive')
                self.assertEqual(states[guest.MANAGER]['ActiveState'],'active')
                self.assertEqual((root/'data/auth.token').read_bytes(),token)
                self.assertEqual((root/'control/native.json').read_bytes(),native_receipt)
                self.assertEqual(marker.read_bytes(),b'history')
                runtime=runtime.identity_runtime(target)
                # The binary exception must not relax configuration admission.
                with self.assertRaisesRegex(ValueError, 'unsafe native file'):
                    guest.native.snapshot(source/'subyard-telemetry')
                installed = root/'library/subyard-telemetry'
                with installed.open('r+b') as output:
                    output.write(b'X')
                with self.assertRaisesRegex(ValueError, 'managed asset changed'):
                    runtime.apply()
                self.assertEqual(installed.read_bytes()[:1], b'X')
                with (source/'subyard-telemetry').open('wb') as output:
                    output.truncate(guest.BINARY_LIMIT+1)
                with self.assertRaisesRegex(ValueError, 'unsafe native file'):
                    guest.Runtime(source, 'dev', True, [], root/'control', root/'data', root/'library', root/'units', root/'bin')

    def test_unselected_guest_needs_only_the_collector_path(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            source = root/'source'
            source.mkdir()
            for path in Path(guest.__file__).parent.glob('*.py'):
                shutil.copyfile(path, source/path.name)
            shutil.copyfile(Path(guest.__file__).with_name('record-policy.txt'), source/'record-policy.txt')
            shutil.copyfile(Path(guest.tomli_w.__file__), source/'tomli_w.py')
            account = types.SimpleNamespace(pw_uid=os.getuid() or 1000, pw_gid=os.getgid(), pw_dir=str(root/'home'))
            def runtime(selected, origin=source):
                return guest.Runtime(origin, 'dev', selected, [], root/'control', root/'data', root/'library',
                                     root/'units', root/'bin')
            with patch.object(guest.pwd, 'getpwnam', return_value=account):
                with self.assertRaisesRegex(ValueError, 'missing profile asset'):
                    runtime(True)
                source_run = runtime(False)
                self.assertEqual(source_run.retained, {root/'library/subyard-telemetry': 0o755})
                (source/'subyard-telemetry').write_bytes(b'\x7fELF synthetic collector')
                packaged_run = runtime(False)
                self.assertEqual(source_run.desired(), packaged_run.desired())
                self.assertEqual(source_run.observe(), {'state': 'absent', 'actual': '', 'desired': ''})
                self.assertEqual(packaged_run.observe(), source_run.observe())
                selected_run = runtime(True)
                self.assertNotEqual(selected_run.desired(), source_run.desired())
                self.assertEqual(selected_run.retained, {})
                # A leftover collector at the retained path is still detected.
                (root/'library').mkdir()
                (root/'library/subyard-telemetry').write_bytes(b'leftover')
                with self.assertRaisesRegex(ValueError, 'unmanaged collector assets'):
                    source_run.observe()

    def test_failure_diagnostic_omits_private_exception_payload(self):
        try:
            raise ValueError('Authorization=Bearer PRIVATE /private/config.json\nPRIVATE_ARGV')
        except ValueError as error:
            message = guest.failure_diagnostic(error)
        self.assertEqual(message, 'telemetry: local reconciliation failed (ValueError at runtime)')
        self.assertNotIn('PRIVATE', message)


if __name__ == '__main__':
    unittest.main()
