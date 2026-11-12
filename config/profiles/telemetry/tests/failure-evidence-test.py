#!/usr/bin/env python3
"""Known secrets are masked; useful failure tails survive fixture disposal."""
from contextlib import redirect_stdout
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('evidence', Path(__file__).parent/'e2e/failure-evidence.py')
evidence = importlib.util.module_from_spec(spec)
spec.loader.exec_module(evidence)


class FailureEvidence(unittest.TestCase):
    def test_tail_masks_known_secrets_preserves_diagnostics_and_refuses_symlinks(self):
        secret = 'fixture-credential'
        detail = b'Error: /tmp/fixture/source.py:42 url=http://localhost:1234 retries=2 id=abc123\n'
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            path = Path(name)/'broker-command.stderr'
            path.write_bytes(b'x'*9000+secret.encode()+detail)
            link = Path(name)/'link'
            link.symlink_to(path)
            with redirect_stdout(io.StringIO()) as controller:
                evidence.emit('storage', {'stderr': path, 'link': link, 'stdout': secret.encode()+detail},
                              secrets=(secret,), return_code=7)
        saved = json.loads(controller.getvalue().removeprefix(evidence.PREFIX))
        self.assertEqual(saved['return_code'], 7)
        self.assertTrue(saved['files']['stderr']['truncated'])
        self.assertLessEqual(len(saved['files']['stderr']['text']), evidence.LIMIT)
        self.assertIn(detail.decode(), saved['files']['stderr']['text'])
        self.assertNotIn(secret, controller.getvalue())
        self.assertEqual(saved['files']['link']['error'], 'OSError')
        with redirect_stdout(io.StringIO()) as forwarded:
            evidence.relay(controller.getvalue().encode())
        self.assertEqual(forwarded.getvalue(), controller.getvalue())


if __name__ == '__main__':
    unittest.main()
