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
if args[0]=='list': print('RUNNING');sys.exit()
assert args[0]=='exec'
arguments=args[args.index('subyard')+1:]
with tempfile.TemporaryDirectory(dir='/tmp',prefix='telemetry-guest-contract-') as temporary:
    stage=pathlib.Path(temporary)
    with tarfile.open(fileobj=sys.stdin.buffer,mode='r|') as archive:
        for member in archive:
            assert member.isfile() and pathlib.Path(member.name).name==member.name
            (stage/member.name).write_bytes(archive.extractfile(member).read())
    sys.path.insert(0,str(stage))
    import guest
    if os.environ.get('TELEMETRY_FIXTURE_BAD_OUTPUT')=='1': print('unexpected-check-output')
    marker=pathlib.Path(os.environ['TELEMETRY_FIXTURE_MARKER'])
    class Runtime:
        def __init__(self,source,user,selected,agents): self.selected=selected
        def observe(self):
            if os.environ.get('TELEMETRY_FIXTURE_STATE')=='unsafe': raise ValueError('foreign control state')
            return dict(state='current' if marker.exists() else ('stale' if self.selected else 'absent'),actual='',desired='')
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
            def invoke(action, **overrides):
                return subprocess.run(['bash',str(profile/'profile.sh'),action],env=dict(environment,**overrides),
                                      capture_output=True,timeout=5)
            self.assertEqual(invoke('--check').returncode,1,'fresh selected profile must be measured drift, not an unavailable adapter')
            self.assertEqual(invoke('--check',SUBYARD_PROFILE_SELECTED='0').returncode,0)
            self.assertEqual(invoke('--check',TELEMETRY_FIXTURE_STATE='unsafe').returncode,2)
            self.assertEqual(invoke('--check',TELEMETRY_FIXTURE_BAD_OUTPUT='1').returncode,2)
            self.assertEqual(invoke('--check',TELEMETRY_FIXTURE_TRANSPORT_FAIL='1').returncode,2)
            self.assertFalse((root/'applied').exists(),'read-only checks must not apply the profile')
            self.assertEqual(invoke('--yes').returncode,0)
            self.assertEqual((root/'applied').read_text(),'1')
            self.assertEqual(invoke('--check').returncode,0)
            self.assertEqual((root/'applied').read_text(),'1')
            self.assertEqual(invoke('--remove').returncode,0)
            self.assertEqual((root/'applied').read_text(),'0')

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
