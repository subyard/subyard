#!/usr/bin/env python3
"""Exercise the real Orca producer against the strict release transport."""
import json
import os
from pathlib import Path
import runpy
import shutil
import subprocess
import tempfile
import unittest

PROFILE = Path(__file__).resolve().parents[1]
ROOT = PROFILE.parents[2]


class ReleaseObservationTest(unittest.TestCase):
    def test_modern_identity_requires_trusted_split_files(self):
        provider = runpy.run_path(str(PROFILE/'tests/release-observation.py'))
        with tempfile.TemporaryDirectory(prefix='orca-modern-', dir='/tmp') as temp:
            package = Path(temp)/'orca'
            resource = package/'resources/orca'
            resource.mkdir(parents=True)
            for name in ('handler.sh', 'registration-contract.sh', 'registration-observer.bash', 'fixed-paths', 'fixed-paths.bash'):
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


    def test_fixed_path_parsers_agree(self):
        provider = runpy.run_path(str(PROFILE/'tests/release-observation.py'))
        parse = provider['parse_fixed_paths']
        shipped = (PROFILE/'resources/orca/fixed-paths').read_text()
        cases = {'shipped': shipped,
                 'indented comment and blanks': '\n  # indented comment\n\t\nunit /etc/a.service\n   path   /usr/b  \n#c\n',
                 'no final newline': 'path /usr/last'}
        invalid = {'unknown kind': 'other /usr/x\n', 'relative': 'path usr/x\n', 'extra field': 'path /usr/x y\n',
                   'parent step': 'path /usr/../x\n', 'crlf': 'path /usr/last\r\n', 'lone cr': 'path /usr/a\rpath /usr/b\n'}
        with tempfile.TemporaryDirectory(prefix='orca-fixed-paths-', dir='/tmp') as temp:
            for name, text in {**cases, **invalid}.items():
                with self.subTest(name=name):
                    listing = Path(temp)/'fixed-paths'
                    listing.write_bytes(text.encode())
                    shell = subprocess.run(['bash', '-c', '. "$1"; orca_fixed_paths_parse "$2"', 'parity',
                                            str(PROFILE/'resources/orca/fixed-paths.bash'), str(listing)],
                                           capture_output=True, text=True, timeout=10)
                    if name in invalid:
                        self.assertNotEqual(shell.returncode, 0)
                        with self.assertRaises(ValueError):
                            parse(text)
                        continue
                    self.assertEqual(shell.returncode, 0, shell.stderr)
                    self.assertEqual([tuple(line.split(' ', 1)) for line in shell.stdout.splitlines()], parse(text))

if __name__ == '__main__':
    unittest.main()
