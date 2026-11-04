#!/usr/bin/env python3
"""Exercise the real Orca producer against the strict release transport."""
import hashlib
import json
import os
from pathlib import Path
import runpy
import shutil
import subprocess
import tempfile
import unittest
from unittest import mock

PROFILE = Path(__file__).resolve().parents[1]
ROOT = PROFILE.parents[2]


class ReleaseObservationTest(unittest.TestCase):
    def test_modern_identity_requires_trusted_split_files(self):
        provider = runpy.run_path(str(PROFILE/'tests/release-observation.py'))
        with tempfile.TemporaryDirectory(prefix='orca-modern-', dir='/tmp') as temp:
            package = Path(temp)/'orca'
            resource = package/'resources/orca'
            resource.mkdir(parents=True)
            for name in ('handler.sh', 'registration-contract.sh', 'registration-observer.bash'):
                shutil.copyfile(PROFILE/'resources/orca'/name, resource/name)
            self.assertEqual(len(provider['read_only_stdin_commands'](package)), 1)
            for name in ('registration-contract.sh', 'registration-observer.bash'):
                path = resource/name
                original = path.read_bytes()
                with self.subTest(name=name, shape='foreign bytes'):
                    path.write_bytes(original+b'\n# foreign package bytes\n')
                    self.assertEqual(provider['read_only_stdin_commands'](package), [])
                path.unlink()
                with self.subTest(name=name, shape='symlink'):
                    path.symlink_to(PROFILE/'resources/orca'/name)
                    self.assertEqual(provider['read_only_stdin_commands'](package), [])
                path.unlink()
                path.write_bytes(original)

    def test_real_producer_and_rejected_mutations(self):
        harness = runpy.run_path(str(ROOT / 'dev/verify-release-upgrades.py'))
        with tempfile.TemporaryDirectory(prefix='orca-release-', dir='/tmp') as temp:
            root = Path(temp)
            shim = root / 'incus'
            shim.write_text('''#!/usr/bin/python3
import json, os, pathlib, sys
args = sys.argv[1:]
if args[:1] == ['list']:
    sys.exit(0)
if args[:1] != ['exec']: sys.exit(99)
pathlib.Path(os.environ['CAPTURE']).write_text(json.dumps(args))
pathlib.Path(os.environ['PAYLOAD']).write_bytes(sys.stdin.buffer.read())
print('{"state":"absent","actual":"","desired":""}')
''')
            shim.chmod(0o700)
            env = dict(os.environ, PATH=f'{root}:/usr/bin:/bin', CAPTURE=str(root/'argv'),
                       PAYLOAD=str(root/'stdin'), SUBYARD_CONFIG_HOME=str(root/'config'),
                       SUBYARD_ENGINE_CONTEXT='1', SUBYARD_ENGINE_CONTEXT_SCHEMA='1')
            for name in ('SUBYARD_OPERATOR_HOME SUBYARD_CONFIG_DIR SUBYARD_HOME STORAGE_PATH HOST_BASE '
                         'RESTRICTED_DISK_PATHS ACCESS_KIND YARD_KIND INCUS_BRIDGE SSH_HOST DEV_UID '
                         'DEV_SUDO FORWARD_SSH_AGENT NESTED_E2E_VMS').split():
                env[name] = ''
            env.update(YARD_INSTANCE_NAME='yard', INCUS_PROJECT='subyard', DEV_USER='dev')
            producer = subprocess.run([str(PROFILE/'runtime.sh'), 'observe'], input='', text=True,
                                      capture_output=True, env=env, timeout=10)
            self.assertEqual(producer.returncode, 0, producer.stderr)
            self.assertEqual(json.loads(producer.stdout)['state'], 'absent')
            args = json.loads((root/'argv').read_text())
            payload = (root/'stdin').read_bytes()
            shims = root/'shims'; shims.mkdir()
            control = root/'control'; control.write_text('{"instances": []}')
            marker = root/'rejected'
            harness['write_observation_incus'](shims, control, marker)
            def run(argv, data):
                marker.unlink(missing_ok=True)
                return subprocess.run([str(shims/'incus'), *argv], input=data, capture_output=True, env=env, timeout=10)
            good = run(args, payload)
            self.assertEqual(good.returncode, 0, good.stderr)
            self.assertEqual(json.loads(good.stdout)['state'], 'absent')
            effect = root/'effect'
            cases = [([*args[:1], 'foreign-instance', *args[2:]], payload),
                     ([*args[:3], 'foreign-project', *args[4:]], payload),
                     (args[:-1], payload), (args+['extra'], payload),
                     ([*args[:9], 'changed', *args[10:]], payload),
                     (args, b'X'+payload[1:]), (args, payload[:-1]), (args, payload+b'X'),
                     (args, f'touch {effect}; # "state":"absent"\n'.encode())]
            for argv, data in cases:
                with self.subTest(argv=argv, length=len(data)):
                    result = run(argv, data)
                    self.assertEqual(result.returncode, 99, result.stderr)
                    self.assertTrue(marker.exists())
                    self.assertFalse(effect.exists())


    def test_historical_identity_and_transport_fail_closed(self):
        harness = runpy.run_path(str(ROOT / 'dev/verify-release-upgrades.py'))
        provider_path = PROFILE / 'tests/release-observation.py'
        provider = runpy.run_path(str(provider_path))
        original_fixtures = provider['read_only_stdin_commands'].__globals__['FIXTURES']
        with tempfile.TemporaryDirectory(prefix='orca-history-', dir='/tmp') as temp:
            root = Path(temp)
            package = root/'runtime/config/profiles/orca'
            resource = package/'resources/orca'
            (resource/'registration').mkdir(parents=True)
            handler = resource/'handler.sh'; handler.write_bytes(b'frozen handler\n')
            member = resource/'registration/fixture.py'; member.write_bytes(b'frozen registration\n')
            fixtures = root/'contracts'; fixtures.mkdir()
            for name in ('contract-0.18.1.txt', 'observer.txt'):
                shutil.copyfile(original_fixtures/name, fixtures/name)
            digest = lambda path: hashlib.sha256(path.read_bytes()).hexdigest()
            identity = {'handler': digest(handler), 'registration': {'fixture.py': digest(member)},
                        'contract': '0.18.1'}
            # Identical handler identities can have different registration inputs.
            other = dict(identity, registration={'fixture.py': '0'*64})
            (fixtures/'identities.json').write_text(json.dumps({'other': other, 'match': identity}))
            provider['read_only_stdin_commands'].__globals__['FIXTURES'] = fixtures
            entries = provider['read_only_stdin_commands'](package)
            self.assertEqual(len(entries), 1)
            argv, payload, _, _, digest_command = entries[0]
            self.assertNotIn('/etc/profile.d/subyard-orca-codex.sh', argv)
            self.assertNotIn('/usr/local/libexec/subyard/orca-codex/codex', argv)
            env = dict(os.environ, SUBYARD_CONFIG_HOME=str(root/'config'), DEV_USER='dev')
            rendered = subprocess.run(digest_command, env=env, capture_output=True, text=True, timeout=5)
            self.assertEqual(rendered.returncode, 0, rendered.stderr)
            argv[argv.index(None)] = rendered.stdout.strip()
            shims = root/'shims'; shims.mkdir()
            control = root/'control'; control.write_text('{"instances": []}')
            marker = root/'rejected'
            run_path = runpy.run_path
            def load(path, *args, **kwargs):
                return provider if Path(path) == provider_path else run_path(path, *args, **kwargs)
            with mock.patch.object(runpy, 'run_path', side_effect=load):
                harness['write_observation_incus'](shims, control, marker, [root/'runtime'])
            def run(arguments, data):
                marker.unlink(missing_ok=True)
                return subprocess.run([str(shims/'incus'), *arguments], input=data,
                                      capture_output=True, env=env, timeout=5)
            self.assertEqual(run(argv, payload).returncode, 0)
            for arguments, data in ((argv[:-1], payload), (argv+['extra'], payload),
                                    (argv, payload[:-1]), (argv, payload+b'X'),
                                    (argv, b'not a program; "state":"absent"\n')):
                self.assertEqual(run(arguments, data).returncode, 99)
                self.assertTrue(marker.exists())
            member.write_bytes(b'changed\n')
            self.assertEqual(provider['read_only_stdin_commands'](package), [])
            self.assertEqual(run(argv, payload).returncode, 99)
            self.assertTrue(marker.exists())

    def test_go022_shape_and_hybrid_refusal(self):
        harness = runpy.run_path(str(ROOT / 'dev/verify-release-upgrades.py'))
        provider_path = PROFILE / 'tests/release-observation.py'
        provider = runpy.run_path(str(provider_path))
        originals = provider['read_only_stdin_commands'].__globals__['FIXTURES']
        with tempfile.TemporaryDirectory(prefix='orca-022-', dir='/tmp') as temp:
            root = Path(temp)
            package = root / 'runtime/config/profiles/orca'
            resource = package / 'resources/orca'
            (resource / 'registration').mkdir(parents=True)
            handler = resource / 'handler.sh'; handler.write_bytes(b'Go 0.22 handler')
            published = json.loads((originals/'identities.json').read_text())['0.22.0']
            for name in published['registration']:
                (resource/'registration'/name).write_bytes(b'Go 0.22 registration')
            member = resource / 'registration/codex_launch.py'
            fixtures = root / 'contracts'; fixtures.mkdir()
            for name in ('contract-0.22.0.txt', 'observer-0.22.0.txt'):
                shutil.copyfile(originals / name, fixtures / name)
            digest = lambda path: hashlib.sha256(path.read_bytes()).hexdigest()
            identity = dict(published, handler=digest(handler),
                            registration={name: digest(resource/'registration'/name)
                                          for name in published['registration']})
            (fixtures / 'identities.json').write_text(json.dumps({'0.22.0': identity}))
            provider['read_only_stdin_commands'].__globals__['FIXTURES'] = fixtures
            argv, payload, _, _, digest_command = provider['read_only_stdin_commands'](package)[0]
            self.assertEqual(argv[-11:], ['/etc/profile.d/subyard-orca-codex.sh',
                                         '/usr/local/libexec/subyard/orca-codex/codex',
                                         'codex_appserver.py', 'codex_launch.py', 'discovery.py',
                                         'inventory.py', 'main.py', 'reconcile.py', 'settings.py',
                                         'state.py', 'transport.py'])
            self.assertEqual(digest(fixtures / 'observer-0.22.0.txt'),
                             '14f969ad9809d4801aa9a622672b23ee1e9bed3bb57105e626239ac4b7a6bb47')
            env = dict(os.environ, SUBYARD_CONFIG_HOME=str(root / 'config'), DEV_USER='dev')
            rendered = subprocess.run(digest_command, env=env, capture_output=True, text=True, timeout=5)
            self.assertEqual(rendered.returncode, 0, rendered.stderr)
            argv[argv.index(None)] = rendered.stdout.strip()
            shims = root / 'shims'; shims.mkdir()
            control = root / 'control'; control.write_text('{"instances": []}')
            marker = root / 'rejected'
            load_original = runpy.run_path
            def load(path, *args, **kwargs):
                return provider if Path(path) == provider_path else load_original(path, *args, **kwargs)
            with mock.patch.object(runpy, 'run_path', side_effect=load):
                harness['write_observation_incus'](shims, control, marker, [root / 'runtime'])
            def run(arguments, data):
                marker.unlink(missing_ok=True)
                return subprocess.run([str(shims / 'incus'), *arguments], input=data,
                                      capture_output=True, env=env, timeout=5)
            self.assertEqual(run(argv, payload).returncode, 0)
            for arguments, data in ((argv[:-1], payload), (argv + ['extra'], payload),
                                    (argv[:8] + ['0' * 64] + argv[9:], payload),
                                    (argv, payload + b'X'), (argv, b'foreign')):
                self.assertEqual(run(arguments, data).returncode, 99)
                self.assertTrue(marker.exists())
            member.write_bytes(b'changed')
            self.assertEqual(provider['read_only_stdin_commands'](package), [])
            self.assertEqual(run(argv, payload).returncode, 99)
            member.write_bytes(b'Go 0.22 registration')
            for partial in ('registration-contract.sh', 'registration-observer.bash'):
                for dangling in (False, True):
                    with self.subTest(partial=partial, dangling=dangling):
                        path = resource / partial
                        if dangling:
                            path.symlink_to('missing-contract-input')
                        else:
                            path.write_text('foreign')
                        self.assertEqual(provider['read_only_stdin_commands'](package), [])
                        path.unlink()
            for partial in ('registration-contract.sh', 'registration-observer.bash'):
                (resource/partial).write_text('foreign')
            self.assertEqual(provider['read_only_stdin_commands'](package), [])
            for partial in ('registration-contract.sh', 'registration-observer.bash'):
                (resource/partial).unlink()
            (resource / 'registration/foreign.py').write_text('foreign')
            self.assertEqual(provider['read_only_stdin_commands'](package), [])
            (resource / 'registration/foreign.py').unlink()
            handler.write_bytes(b'foreign handler')
            self.assertEqual(provider['read_only_stdin_commands'](package), [])


if __name__ == '__main__':
    unittest.main()
