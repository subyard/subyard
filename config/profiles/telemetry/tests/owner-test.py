#!/usr/bin/env python3
"""Exact generic release observations use the declared owning transport."""
import json
import io
import os
import shutil
from contextlib import redirect_stderr
from pathlib import Path
import runpy
import subprocess
import sys
import tempfile
import unittest


class OwnerTest(unittest.TestCase):
    def test_exact_owner_check_distinguishes_fresh_drift_from_failure_and_applies(self):
        source = Path(__file__).resolve().parents[1]
        repository = source.parents[2]
        with tempfile.TemporaryDirectory(prefix='telemetry-owner-contract-',dir='/tmp') as name:
            root = Path(name)
            profile = root/'config/profiles/telemetry'
            profile.mkdir(parents=True)
            for path in source.glob('*.py'):
                shutil.copyfile(path,profile/path.name)
            shutil.copyfile(source/'profile.sh',profile/'profile.sh')
            (root/'scripts/lib').mkdir(parents=True)
            shutil.copyfile(repository/'scripts/lib/engine-context.sh',root/'scripts/lib/engine-context.sh')
            (root/'internal/adapters/configmaterial').mkdir(parents=True)
            shutil.copyfile(repository/'internal/adapters/configmaterial/tomli_w.py',root/'internal/adapters/configmaterial/tomli_w.py')
            (profile/'bin').mkdir()
            (profile/'bin/subyard-telemetry').write_bytes(b'synthetic native asset')
            (profile/'bin/subyard-telemetry').chmod(0o755)
            binary = root/'bin'
            binary.mkdir()
            incus = binary/'incus'
            incus.write_text('''#!/usr/bin/env python3
import json,os,pathlib,sys,tarfile,tempfile
args=sys.argv[1:]
if os.environ.get('TELEMETRY_FIXTURE_TRANSPORT_FAIL')=='1': sys.exit(1)
if args[0]=='list': assert args[-2:]==['-c','ns']; print(args[1]+'-neighbour,STOPPED'); print(args[1]+',RUNNING'); sys.exit()
assert args[0]=='exec'
arguments=args[args.index('subyard')+1:]
with tempfile.TemporaryDirectory(dir='/tmp',prefix='telemetry-guest-contract-') as temporary:
    stage=pathlib.Path(temporary)
    with tarfile.open(fileobj=sys.stdin.buffer,mode='r|') as archive:
        for member in archive:
            assert member.isfile() and pathlib.Path(member.name).name==member.name
            (stage/member.name).write_bytes(archive.extractfile(member).read())
    if arguments[0]=='startup':
        assert not (stage/'subyard-telemetry').exists()
        assert '--env' in args and 'SUBYARD_STARTUP_REMAINING_MS=1234' in args
    sys.path.insert(0,str(stage))
    import guest
    if os.environ.get('TELEMETRY_FIXTURE_BAD_OUTPUT')=='1': print('unexpected-check-output')
    marker=pathlib.Path(os.environ['TELEMETRY_FIXTURE_MARKER'])
    class Runtime:
        def __init__(self,source,user,selected,agents): self.selected=selected
        def observe(self):
            if os.environ.get('TELEMETRY_FIXTURE_STATE')=='unsafe': raise ValueError('foreign control state')
            observation=dict(state='current' if marker.exists() else ('stale' if self.selected else 'absent'),actual='',desired='')
            if os.environ.get('TELEMETRY_FIXTURE_DIAGNOSTICS')=='1':
                observation['diagnostics']=[dict(parameter='telemetry.asset.collector.py',installed='changed',desired='current')]
            return observation
        def startup(self): return {'state':'ready' if marker.exists() else 'drift'}
        def apply(self): marker.write_text(str(int(self.selected)))
    guest.Runtime=Runtime
    guest.os.getuid=lambda:0
    guest.sys.argv=['guest.py',*arguments]
    try: guest.main()
    except (OSError,ValueError,KeyError,guest.subprocess.SubprocessError) as error:
        print(guest.failure_diagnostic(error),file=sys.stderr);sys.exit(2)
''')
            incus.chmod(0o755)
            environment = dict(os.environ,PATH=str(binary)+os.pathsep+os.environ['PATH'],
                               SUBYARD_ENGINE_CONTEXT='1',SUBYARD_ENGINE_CONTEXT_SCHEMA='1',
                               SUBYARD_PROFILE_SELECTED='1',CODING_TOOL_INTEGRATIONS='',TELEMETRY_FIXTURE_MARKER=str(root/'applied'))
            for key in ('SUBYARD_OPERATOR_HOME','SUBYARD_CONFIG_DIR','SUBYARD_CONFIG_HOME','SUBYARD_HOME',
                        'STORAGE_PATH','HOST_BASE','RESTRICTED_DISK_PATHS','ACCESS_KIND','YARD_KIND','YARD_INSTANCE_NAME',
                        'INCUS_PROJECT','INCUS_BRIDGE','SSH_HOST','DEV_USER','DEV_UID','DEV_SUDO','FORWARD_SSH_AGENT','NESTED_E2E_VMS'):
                environment[key] = 'dev' if key=='DEV_USER' else 'fixture'
            def invoke(action, *arguments, **overrides):
                return subprocess.run(['bash',str(profile/'profile.sh'),action,*arguments],env=dict(environment,**overrides),
                                      capture_output=True,timeout=5)
            startup = invoke('--startup-check', SUBYARD_STARTUP_REMAINING_MS='1234')
            self.assertEqual(startup.returncode, 0, startup.stderr)
            self.assertEqual(json.loads(startup.stdout), {'state':'drift'})
            self.assertEqual(invoke('--check').returncode,1,'fresh selected profile must be measured drift, not an unavailable adapter')
            self.assertEqual(invoke('--check',SUBYARD_PROFILE_SELECTED='0').returncode,0)
            self.assertEqual(invoke('--check',TELEMETRY_FIXTURE_STATE='unsafe').returncode,2)
            self.assertEqual(invoke('--check',TELEMETRY_FIXTURE_BAD_OUTPUT='1').returncode,2)
            self.assertEqual(invoke('--check',TELEMETRY_FIXTURE_TRANSPORT_FAIL='1').returncode,2)
            self.assertFalse((root/'applied').exists(),'read-only checks must not apply the profile')
            observations = []
            for enabled in ('0', '1'):
                result = invoke('--runtime', 'observe', ENVIRONMENT_PROFILES='telemetry',
                                TELEMETRY_FIXTURE_DIAGNOSTICS='1', SUBYARD_PROFILE_DIAGNOSTICS=enabled)
                self.assertEqual(result.returncode, 0, result.stderr)
                observations.append(json.loads(result.stdout))
            self.assertNotIn('diagnostics', observations[0])
            self.assertEqual(observations[1].pop('diagnostics'),
                             [dict(parameter='telemetry.asset.collector.py', installed='changed', desired='current')])
            self.assertEqual(observations[0], observations[1])
            self.assertFalse((root/'applied').exists(), 'diagnostic observation applied the profile')
            self.assertEqual(invoke('--yes').returncode,0)
            self.assertEqual((root/'applied').read_text(),'1')
            self.assertEqual(invoke('--check').returncode,0)
            self.assertEqual((root/'applied').read_text(),'1')
            self.assertEqual(invoke('--remove').returncode,0)
            self.assertEqual((root/'applied').read_text(),'0')

    def runtime_fixture(self, root):
        """A copied profile whose fake Incus records each guest archive's members."""
        source = Path(__file__).resolve().parents[1]
        repository = source.parents[2]
        profile = root/'config/profiles/telemetry'
        profile.mkdir(parents=True)
        for path in source.glob('*.py'):
            shutil.copyfile(path, profile/path.name)
        for name in ('profile.sh', 'runtime.sh'):
            shutil.copyfile(source/name, profile/name)
            (profile/name).chmod(0o755)
        (root/'scripts/lib').mkdir(parents=True)
        shutil.copyfile(repository/'scripts/lib/engine-context.sh', root/'scripts/lib/engine-context.sh')
        (root/'internal/adapters/configmaterial').mkdir(parents=True)
        shutil.copyfile(repository/'internal/adapters/configmaterial/tomli_w.py', root/'internal/adapters/configmaterial/tomli_w.py')
        commands = root/'commands'
        commands.mkdir()
        (commands/'incus').write_text("""#!/usr/bin/env python3
import json,os,sys,tarfile
args=sys.argv[1:]
if args[0]=='list': assert args[-2:]==['-c','ns']; print(args[1]+'-neighbour,STOPPED'); print(args[1]+',RUNNING'); sys.exit()
with tarfile.open(fileobj=sys.stdin.buffer,mode='r|') as archive:
    members=sorted(member.name for member in archive)
with open(os.environ['TELEMETRY_FIXTURE_EXEC'],'a') as log: log.write(json.dumps(members)+'\\n')
print(json.dumps({'state':'absent','actual':'','desired':''}))
""")
        (commands/'incus').chmod(0o755)
        environment = dict(os.environ, PATH=str(commands)+os.pathsep+os.environ['PATH'],
                           SUBYARD_ENGINE_CONTEXT='1', SUBYARD_ENGINE_CONTEXT_SCHEMA='1', CODING_TOOL_INTEGRATIONS='',
                           ENVIRONMENT_PROFILES='', TELEMETRY_FIXTURE_EXEC=str(root/'exec.log'))
        for key in ('SUBYARD_OPERATOR_HOME','SUBYARD_CONFIG_DIR','SUBYARD_CONFIG_HOME','SUBYARD_HOME',
                    'STORAGE_PATH','HOST_BASE','RESTRICTED_DISK_PATHS','ACCESS_KIND','YARD_KIND','YARD_INSTANCE_NAME',
                    'INCUS_PROJECT','INCUS_BRIDGE','SSH_HOST','DEV_USER','DEV_UID','DEV_SUDO','FORWARD_SSH_AGENT','NESTED_E2E_VMS'):
            environment[key] = 'dev' if key == 'DEV_USER' else 'fixture'
        def observe(**overrides):
            log = root/'exec.log'
            if log.exists():
                log.unlink()
            result = subprocess.run(['bash', str(profile/'runtime.sh'), 'observe'], env=dict(environment, **overrides),
                                    capture_output=True, text=True, timeout=10)
            archives = [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []
            return result, archives
        return profile, observe

    def test_unselected_runtime_needs_no_collector_binary(self):
        with tempfile.TemporaryDirectory(prefix='telemetry-owner-unselected-', dir='/tmp') as name:
            root = Path(name)
            profile, observe = self.runtime_fixture(root)
            source_run, source_archives = observe()
            self.assertEqual(source_run.returncode, 0, source_run.stderr)
            (profile/'bin').mkdir()
            (profile/'bin/subyard-telemetry').write_bytes(b'synthetic native asset')
            (profile/'bin/subyard-telemetry').chmod(0o755)
            packaged_run, packaged_archives = observe()
            self.assertEqual(packaged_run.returncode, 0, packaged_run.stderr)
            self.assertEqual((source_run.stdout, source_archives), (packaged_run.stdout, packaged_archives))
            self.assertEqual(len(source_archives), 1)
            self.assertNotIn('subyard-telemetry', source_archives[0])
            selected_run, selected_archives = observe(ENVIRONMENT_PROFILES='telemetry')
            self.assertEqual(selected_run.returncode, 0, selected_run.stderr)
            self.assertIn('subyard-telemetry', selected_archives[0])

    def test_unsafe_or_missing_collector_refuses_before_any_guest_effect(self):
        with tempfile.TemporaryDirectory(prefix='telemetry-owner-refusal-', dir='/tmp') as name:
            root = Path(name)
            profile, observe = self.runtime_fixture(root)
            packaged = profile/'bin/subyard-telemetry'
            built = root/'.build/profiles/telemetry/bin/subyard-telemetry'
            packaged.parent.mkdir()
            built.parent.mkdir(parents=True)
            missing = (f'native telemetry collector is missing: selected profile telemetry needs {packaged} or {built}; '
                       'build it with make build')
            def unexecutable():
                packaged.write_bytes(b'collector')
                packaged.chmod(0o644)
            def both_symlinked():
                packaged.symlink_to(root/'elsewhere')
                built.symlink_to(root/'elsewhere')
            cases = (
                ('missing', lambda: None, 'telemetry', [missing]),
                ('not executable', unexecutable, 'telemetry', [f'native telemetry collector is not executable: {packaged}']),
                ('symlinked packaged', lambda: packaged.symlink_to(root/'elsewhere'), 'telemetry',
                 [f'unsafe native telemetry source: symlinked collector {packaged}']),
                ('symlinked unselected', lambda: built.symlink_to(root/'elsewhere'), '',
                 [f'unsafe native telemetry source: symlinked collector {built}']),
                ('both symlinked', both_symlinked, 'telemetry',
                 [f'unsafe native telemetry source: symlinked collector {packaged}, {built}']),
                ('every refusal', lambda: None, 'telemetry',
                 [missing, 'standard native destination required: AGENT_codex_CONFIG_DEST must be .codex/config.toml']),
            )
            for case, arrange, selected, refusals in cases:
                with self.subTest(case=case):
                    for path in (packaged, built):
                        if path.is_symlink() or path.exists():
                            path.unlink()
                    arrange()
                    overrides = dict(ENVIRONMENT_PROFILES=selected)
                    if case == 'every refusal':
                        overrides.update(CODING_TOOL_INTEGRATIONS='codex', AGENT_codex_CONFIG_DEST='.config/other.toml')
                    result, archives = observe(**overrides)
                    self.assertEqual(result.returncode, 2)
                    self.assertEqual(archives, [], 'a refusal must precede the guest transport')
                    self.assertIn('telemetry: refused: '+'; '.join(refusals)+'\n', result.stderr)

    def test_failed_guest_forwards_only_closed_public_diagnostic(self):
        owner = runpy.run_path(str(Path(__file__).resolve().parents[1]/'owner.py'))
        safe = 'telemetry: local reconciliation failed (PermissionError at native.py:28)'
        for message, expected in [
                ('PRIVATE_CREDENTIAL /private/config\n'+safe+'\nPRIVATE_ARGV', safe+'\n'),
                (safe+' PRIVATE_CREDENTIAL', ''),
                (safe.replace('native.py', '/private/native.py'), ''),
                (safe.replace(':28', ':0'), ''),
                (safe.replace('PermissionError','PRIVATE_CREDENTIAL'), '')]:
            with self.subTest(message_class=bool(expected)):
                stderr = io.StringIO()
                with redirect_stderr(stderr), self.assertRaisesRegex(ValueError,'telemetry owner transport failed'):
                    owner['run']([sys.executable,'-c','import sys;sys.stderr.write(sys.argv[1]);sys.exit(7)', message])
                self.assertEqual(stderr.getvalue(), expected)
                self.assertNotIn('PRIVATE', stderr.getvalue())

    def test_all_exact_absent_release_observations_are_declared(self):
        source = Path(__file__).resolve().parents[4]
        provider = runpy.run_path(str(Path(__file__).with_name('release-observation.py')))
        verifier = runpy.run_path(str(source/'dev/verify-release-upgrades.py'))
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            control, unexpected = root/'control.json', root/'unexpected'
            control.write_text('{"instances": []}')
            verifier['write_observation_incus'](root, control, unexpected)
            commands = provider['read_only_commands'](Path(__file__).resolve().parents[1])
            self.assertEqual(len(commands), 4)
            for arguments, response in commands:
                result = subprocess.run([str(root/'incus'), *arguments], input=b'fixture', capture_output=True, timeout=2)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(json.loads(result.stdout), json.loads(response))
            self.assertFalse(unexpected.exists())


if __name__ == '__main__':
    unittest.main()
