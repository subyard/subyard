#!/usr/bin/env python3
"""Exercise native observation against the released-updater's strict shims."""
import json
from pathlib import Path
import runpy
import shutil
import subprocess
import tempfile
import unittest


PROFILE = Path(__file__).resolve().parents[1]
ROOT = PROFILE.parents[2]


class ReleaseObservationTest(unittest.TestCase):
    def test_actual_absence_transport_and_manager_fail_closed(self):
        harness = runpy.run_path(str(ROOT / 'dev/verify-release-upgrades.py'))
        provider = runpy.run_path(str(PROFILE / 'tests/release-observation.py'))
        with tempfile.TemporaryDirectory(prefix='subyard-github-release-', dir='/tmp') as temporary:
            root = Path(temporary)
            runtime = root / 'runtime'
            package = runtime / 'config/profiles/github'
            (package / 'bin').mkdir(parents=True)
            # The broker is fingerprinted but never executed by absence inspection.
            for name in ('profile.json', 'owner.sh', 'runtime.sh', 'runtime.py', 'provision.sh',
                         'SKILL.md', 'bin/subyard-github'):
                shutil.copy2(PROFILE / name, package / name)
            (package / 'bin/broker').write_text('unexecuted synthetic broker input\n')
            (runtime / 'scripts/lib').mkdir(parents=True)
            for name in ('engine-context.sh', 'ui.sh', 'host.sh', 'download.sh'):
                shutil.copy2(ROOT / 'scripts/lib' / name, runtime / 'scripts/lib' / name)
            shutil.copy2(ROOT / 'scripts/lib-power.sh', runtime / 'scripts/lib-power.sh')
            shims = root / 'commands'
            shims.mkdir()
            control, rejected = root / 'control.json', root / 'rejected'
            control.write_text(json.dumps({'instances': [{'info': {'status': 'Running'}}]}))
            harness['write_observation_incus'](shims, control, rejected)
            environment = {
                'PATH': str(shims) + ':/usr/bin:/bin', 'HOME': str(root),
                'SUBYARD_ENGINE_CONTEXT': '1', 'SUBYARD_ENGINE_CONTEXT_SCHEMA': '1',
                'SUBYARD_OPERATOR_HOME': str(root), 'SUBYARD_CONFIG_DIR': str(runtime / 'config'),
                'SUBYARD_CONFIG_HOME': str(root / 'config'), 'SUBYARD_HOME': str(root / 'data'),
                'STORAGE_PATH': str(root), 'HOST_BASE': str(root), 'RESTRICTED_DISK_PATHS': str(root),
                'ACCESS_KIND': 'local', 'YARD_KIND': 'container', 'YARD_INSTANCE_NAME': 'yard',
                'INCUS_PROJECT': 'subyard', 'INCUS_BRIDGE': 'fixture', 'SSH_HOST': '127.0.0.1',
                'DEV_USER': 'dev', 'DEV_UID': '1000', 'DEV_SUDO': '0',
                'FORWARD_SSH_AGENT': '0', 'NESTED_E2E_VMS': '0', 'ENVIRONMENT_PROFILES': '',
            }

            def observe():
                result = subprocess.run(['bash', str(package / 'runtime.sh'), 'observe'],
                                        env=environment, capture_output=True, text=True, timeout=30)
                self.assertEqual(result.returncode, 0, result.stderr)
                return json.loads(result.stdout)

            observed = observe()
            self.assertEqual(observed['state'], 'current')
            self.assertTrue(observed['ownership_absent'])
            self.assertEqual(observed['actual'], observed['desired'])
            self.assertFalse(rejected.exists())
            for entry in provider['read_only_commands'](PROFILE):
                arguments, output, *status = entry
                status = status[0] if status else 0
                result = subprocess.run([str(shims / 'incus'), *arguments], input=b'\xff' * 131072,
                                        capture_output=True, timeout=10)
                self.assertEqual(result.returncode, status)
                self.assertEqual(result.stdout.decode(), output)
                for changed in (arguments + ['unexpected'], ['exec', 'foreign', *arguments[2:]]):
                    result = subprocess.run([str(shims / 'incus'), *changed],
                                            capture_output=True, timeout=10)
                    self.assertEqual(result.returncode, 99)
                    self.assertTrue(rejected.exists())
                    rejected.unlink()
            archive = provider['read_only_commands'](PROFILE)[1][0]
            for changed in (archive[:-1] + ['--yes'],
                            archive[:12] + [archive[12] + '\n'] + archive[13:]):
                result = subprocess.run([str(shims / 'incus'), *changed], capture_output=True, timeout=10)
                self.assertEqual(result.returncode, 99)
                rejected.unlink()
            program, arguments, output, status = provider['read_only_host_commands'](PROFILE)[0]
            self.assertEqual(program, 'systemctl')
            result = subprocess.run([str(shims / program), *arguments], capture_output=True, timeout=10)
            self.assertEqual((result.stdout.decode(), result.returncode), (output, status))
            for changed in (
                ['--user', 'start', arguments[2]], ['--user', 'stop', arguments[2]],
                ['--user', 'restart', arguments[2]], arguments + ['unexpected'],
                arguments[:2] + ['foreign.service'] + arguments[3:],
                arguments[:-1] + ['--property=ActiveState'],
            ):
                result = subprocess.run([str(shims / 'systemctl'), *changed], capture_output=True, timeout=10)
                self.assertEqual(result.returncode, 99)
                self.assertTrue(rejected.exists())
                rejected.unlink()
            for command in ('incus', 'systemctl'):
                path = shims / command
                original = path.read_text()
                path.write_text('#!/bin/sh\ncat >/dev/null\nexit 1\n')
                observed = observe()
                self.assertFalse(observed.get('ownership_absent', False))
                path.write_text(original)


if __name__ == '__main__':
    unittest.main()
