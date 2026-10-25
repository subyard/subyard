#!/usr/bin/env python3
import json
import hashlib
import importlib.util
import io
from contextlib import redirect_stderr
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import types
import unittest
from unittest.mock import patch
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
sys.path.insert(0, str(Path(__file__).resolve().parents[4]/'internal/adapters/configmaterial'))
import guest
spec = importlib.util.spec_from_file_location('profile_lifecycle', Path(__file__).parent/'e2e/profile-lifecycle.py')
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)


class LifecycleTest(unittest.TestCase):
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
            source = root/'source'
            source.mkdir()
            for path in Path(guest.__file__).parent.glob('*.py'):
                shutil.copyfile(path, source/path.name)
            shutil.copyfile(Path(guest.tomli_w.__file__), source/'tomli_w.py')
            # Match the compiled collector's observed 25 MiB size, not a tiny stub.
            (source/'subyard-telemetry').write_bytes(b'\x7fELF'+b'\0'*(25_333_922-4))
            for directory in ('home', 'units', 'bin'):
                (root/directory).mkdir()
            account = types.SimpleNamespace(pw_uid=os.getuid() or 1000, pw_gid=os.getgid(), pw_dir=str(root/'home'))
            states, calls = {}, []
            def manager(*args):
                calls.append(args)
                if args[0] == 'show':
                    unit = args[1]
                    if unit not in states:
                        return 'LoadState=not-found'
                    command=next(line.removeprefix('ExecStart=') for line in (root/'units'/unit).read_text().splitlines() if line.startswith('ExecStart='))
                    fields=dict(LoadState='loaded',FragmentPath=str(root/'units'/unit),User='dev',Group=str(account.pw_gid),
                        Type='simple',UMask='0077',LimitCORE='0',NoNewPrivileges='yes',DropInPaths='',
                        ExecStart='{ path='+command.split()[0]+' ; argv[]='+command+' ; ignore_errors=no ; start_time=x ; }',
                        KillMode='process' if unit==guest.MANAGER else 'control-group',Delegate='yes',
                        MemoryMax=str(256*1024*1024),TasksMax='64',CPUQuotaPerSecUSec='250ms')
                    fields.update(states[unit])
                    return '\n'.join(f'{key}={value}' for key,value in fields.items())
                if args[0] == 'enable':
                    for unit in args[2:]:
                        states[unit] = dict(ActiveState='active', UnitFileState='enabled')
                if args[0] == 'disable':
                    for unit in args[1:]:
                        if unit.startswith('--'):
                            continue
                        states[unit]['UnitFileState'] = 'disabled'
                        if '--now' in args:
                            states[unit]['ActiveState'] = 'inactive'
                if args[0] == 'restart':
                    for unit in args[1:]: states[unit]['ProcessNNP']='yes'
                return ''
            with patch.object(guest.pwd, 'getpwnam', return_value=account), patch.object(guest, 'systemctl', side_effect=manager), \
                 patch.object(guest.Runtime,'running_no_new_privs',side_effect=lambda fields: fields.get('ProcessNNP','yes')=='yes'):
                runtime = guest.Runtime(source, 'dev', True, ['codex'], root/'control', root/'data', root/'library', root/'units', root/'bin', root/'legacy/native.json')
                for command in runtime.starts.values():
                    self.assertEqual(command.split()[:2],['/usr/bin/setpriv','--no-new-privs'])
                    actual=subprocess.check_output(command.split()[:2]+[sys.executable,'-c',
                        "from pathlib import Path;print(next(line.split(':',1)[1].strip() for line in Path('/proc/self/status').read_text().splitlines() if line.startswith('NoNewPrivs:')))"])
                    self.assertEqual(actual.strip(),b'1')
                self.assertEqual(runtime.observe()['state'], 'stale')
                runtime.apply()
                observed=runtime.observe()
                self.assertEqual(observed['state'], 'current')
                self.assertEqual(observed['actual'],observed['desired'],'current runtime must satisfy the generic activation digest contract')
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
                (root/'control/assets.json').write_text(json.dumps(saved))
                self.assertEqual(runtime.observe()['state'],'stale')
                runtime.apply()
                self.assertIn(('restart',guest.COLLECTOR,guest.MANAGER),calls)
                self.assertEqual(runtime.observe()['state'],'current')
                restarts=sum(call[0]=='restart' for call in calls)
                runtime.apply()
                self.assertEqual(sum(call[0]=='restart' for call in calls),restarts)
                # A restart interrupted after new files/manifest were committed
                # must still retry the old active NNP0 process on unchanged files.
                states[guest.COLLECTOR]['ProcessNNP']='no'
                stale=runtime.observe()
                self.assertEqual(stale['state'],'stale')
                self.assertNotEqual(stale['actual'],stale['desired'])
                runtime.apply()
                self.assertEqual(calls[-1],('restart',guest.COLLECTOR))
                self.assertEqual(runtime.observe()['state'],'current')
                for changed in ({'User':'root'},{'DropInPaths':'/etc/systemd/system/subyard-.service.d/foreign.conf'},
                                {'DropInPaths':'/etc/systemd/system/'+guest.COLLECTOR+'.d/foreign.conf'},
                                {'KillMode':'process'},{'MemoryMax':'infinity'},{'Environment':'PRIVATE=unsafe'},
                                {'ExecStart':'{ path=/bin/false ; argv[]=/bin/false ; ignore_errors=no ; }'}):
                    original=dict(states[guest.COLLECTOR])
                    states[guest.COLLECTOR].update(changed)
                    with self.assertRaises(ValueError):runtime.apply()
                    states[guest.COLLECTOR]=original
                runtime.selected = False
                runtime.apply()
                observed=runtime.observe()
                self.assertEqual(observed['state'], 'current')
                self.assertEqual(observed['actual'],observed['desired'])
                self.assertEqual(marker.read_bytes(), b'history')
                self.assertTrue((root/'data/drain.json').exists())
                self.assertEqual(states[guest.MANAGER]['ActiveState'], 'active')
                self.assertFalse(any(call[0] == 'stop' and guest.MANAGER in call for call in calls))
                self.assertTrue((root/'bin/subyard-telemetry-run').read_text().startswith('#!/bin/sh\n'))
                self.assertNotIn('analytics', (root/'home/.codex/config.toml').read_text())
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
                    guest.Runtime(source, 'dev', True, [], root/'control', root/'data', root/'library', root/'units', root/'bin', root/'legacy/native.json')

    def test_failure_diagnostic_omits_private_exception_payload(self):
        try:
            raise ValueError('Authorization=Bearer PRIVATE /private/config.json\nPRIVATE_ARGV')
        except ValueError as error:
            message = guest.failure_diagnostic(error)
        self.assertEqual(message, 'telemetry: local reconciliation failed (ValueError at runtime)')
        self.assertNotIn('PRIVATE', message)


if __name__ == '__main__':
    unittest.main()
