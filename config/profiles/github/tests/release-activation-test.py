#!/usr/bin/env python3
"""Bind the activation fixture to current frozen metadata and authentic predecessors."""
import json
from pathlib import Path
import subprocess
import tempfile
import unittest


GUEST = Path(__file__).parent/'e2e/release-activation-guest.sh'
INSTALLER_SHA = 'eeacda9ded1cc4c095ecc1e0569cf6fd96a737b3af6ab30ce7804833e3d8f1d1'


class ActivationIdentityTest(unittest.TestCase):
    def identity(self, version='0.21.0', baseline='0.20.0', **changes):
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            metadata = Path(temporary)/'candidate.json'
            value = dict(version=version,
                runtime_file=f'subyard-{version}-linux-amd64.tar.gz',
                runtime_sha256='a'*64)
            value.update(changes)
            metadata.write_text(json.dumps(value))
            return subprocess.run(['bash', str(GUEST), '--candidate-identity', str(metadata), baseline],
                capture_output=True, text=True, check=False)

    def test_current_and_original_candidate_select_greater_synthetic_successor(self):
        for candidate, baseline, successor in (('0.21.0', '0.20.0', '0.21.1-activation-fixture'),
                ('0.19.1-activation-e2e', '0.19.0', '0.19.2-activation-fixture')):
            with self.subTest(candidate=candidate, baseline=baseline):
                result = self.identity(candidate, baseline)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout, f'{candidate}\t{successor}\t{INSTALLER_SHA}\n')

    def test_mismatched_archive_checksum_or_predecessor_is_refused(self):
        for changes in ({'runtime_file': 'subyard-0.19.1-activation-e2e-linux-amd64.tar.gz'},
                {'runtime_sha256': 'invalid'}, {'baseline': '0.21.0'}, {'version': '0.20.0'}):
            with self.subTest(changes=changes):
                self.assertNotEqual(self.identity(**changes).returncode, 0)


if __name__ == '__main__':
    unittest.main()
