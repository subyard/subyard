#!/usr/bin/env python3
"""Keep the released-updater fixture bound to the actual read-only transport."""
import json
from pathlib import Path
import runpy
import subprocess
import tempfile
import unittest


PROFILE = Path(__file__).resolve().parents[1]
ROOT = PROFILE.parents[2]


class ReleaseObservationTest(unittest.TestCase):
    def test_actual_transport_absence_archive_drain_and_rejections(self):
        harness = runpy.run_path(str(ROOT / 'dev/verify-release-upgrades.py'))
        provider = runpy.run_path(str(PROFILE / 'tests/release-observation.py'))
        with tempfile.TemporaryDirectory(prefix='subyard-git-fetch-release-', dir='/tmp') as temporary:
            root = Path(temporary)
            control, rejected = root / 'control.json', root / 'rejected'
            control.write_text(json.dumps({'instances': [{'info': {'status': 'Running'}}]}))
            harness['write_observation_incus'](root, control, rejected)
            environment = {
                'PATH': str(root) + ':/usr/bin:/bin', 'HOME': str(root),
                'SUBYARD_ENGINE_CONTEXT': '1', 'SUBYARD_ENGINE_CONTEXT_SCHEMA': '1',
                'SUBYARD_OPERATOR_HOME': str(root), 'SUBYARD_CONFIG_DIR': str(ROOT / 'config'),
                'SUBYARD_CONFIG_HOME': str(root), 'SUBYARD_HOME': str(root),
                'STORAGE_PATH': str(root), 'HOST_BASE': str(root), 'RESTRICTED_DISK_PATHS': str(root),
                'ACCESS_KIND': 'local', 'YARD_KIND': 'container', 'YARD_INSTANCE_NAME': 'yard',
                'INCUS_PROJECT': 'subyard', 'INCUS_BRIDGE': 'fixture', 'SSH_HOST': '127.0.0.1',
                'DEV_USER': 'dev', 'DEV_UID': '1000', 'DEV_SUDO': '0',
                'FORWARD_SSH_AGENT': '0', 'NESTED_E2E_VMS': '0', 'ENVIRONMENT_PROFILES': '',
            }
            result = subprocess.run(['bash', str(PROFILE / 'runtime.sh'), 'observe'],
                                    env=environment, capture_output=True, text=True, timeout=30)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(json.loads(result.stdout), {'state': 'absent'})
            self.assertFalse(rejected.exists())
            arguments, response = provider['read_only_commands'](PROFILE)[0]
            result = subprocess.run([str(root / 'incus'), *arguments], input=b'\xff' * 131072,
                                    capture_output=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout.decode(), response)
            for changed in (
                arguments[:11] + ['refresh', *arguments[12:]],
                arguments[:11] + ['apply', *arguments[12:]],
                arguments[:9] + [arguments[9] + '\n'] + arguments[10:],
                arguments[:-1] + ['a' * 64],
                arguments[:1] + ['foreign'] + arguments[2:],
                arguments + ['unexpected'],
            ):
                with self.subTest(arguments=changed[:2]):
                    result = subprocess.run([str(root / 'incus'), *changed],
                                            capture_output=True, timeout=10)
                    self.assertEqual(result.returncode, 99)
                    self.assertEqual(rejected.read_text(), 'incus operation rejected')
                    rejected.unlink()
            # A failed observation transport must never become runtime absence.
            (root / 'incus').write_text('#!/bin/sh\ncat >/dev/null\nexit 1\n')
            result = subprocess.run(['bash', str(PROFILE / 'runtime.sh'), 'observe'],
                                    env=environment, capture_output=True, timeout=10)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(result.stdout, b'')


if __name__ == '__main__':
    unittest.main()
