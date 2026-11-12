#!/usr/bin/env python3
"""Known secrets are masked; useful failure tails survive fixture disposal."""
from contextlib import redirect_stdout
import ast
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import stat
import sys
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('evidence', Path(__file__).parent/'e2e/failure-evidence.py')
evidence = importlib.util.module_from_spec(spec)
spec.loader.exec_module(evidence)


class FailureEvidence(unittest.TestCase):
    def test_delegated_child_failure_reaches_controller_before_cleanup(self):
        spec = importlib.util.spec_from_file_location('profile_fixture', Path(__file__).parent/'e2e/profile-lifecycle.py')
        fixture = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(fixture)
        payload = next(ast.literal_eval(node.value) for node in ast.walk(ast.parse(Path(spec.origin).read_text()))
                       if isinstance(node, ast.Assign) and any(isinstance(target, ast.Name) and target.id == 'payload'
                                                               for target in node.targets))
        for timed_out in (False, True):
            with self.subTest(timed_out=timed_out), tempfile.TemporaryDirectory(
                    prefix='subyard-telemetry-delegated.', dir='/tmp') as name:
                root = Path(name)
                root.chmod(0o700)
                (root/'marker').write_bytes(b'subyard-telemetry-delegated-v1')
                (root/'marker').chmod(0o600)
                helper = root/'source/config/profiles/telemetry/tests/e2e/failure-evidence.py'
                helper.parent.mkdir(parents=True)
                shutil.copyfile(evidence.__file__, helper)
                child = "import sys,time;print('x'*20000+' fixture-token stdout-cause',flush=True);print('fixture-token stderr-cause',file=sys.stderr,flush=True);" + (
                    'time.sleep(10)' if timed_out else 'sys.exit(7)')
                body = 'failure_evidence.run('+repr([sys.executable, '-c', child])+', evidence_root, secrets=secrets, check=True, timeout=.2)'
                read_text = Path.read_text
                def guest_text(path, *args, **kwargs):
                    values = {'/var/lib/subyard/telemetry/auth.token': 'fixture-token',
                              '/proc/self/cgroup': '0::/fixture/caller', '/sys/fs/cgroup/fixture/memory.max': str(2*1024**3),
                              '/sys/fs/cgroup/fixture/pids.max': '128'}
                    return values[str(path)] if str(path) in values else read_text(path, *args, **kwargs)
                with patch.object(Path, 'read_text', guest_text), self.assertRaises(SystemExit) as error:
                    exec(compile(payload, 'delegated-payload', 'exec'), {'__file__': str(root/'payload.py'), 'BODY': body})
                self.assertEqual(error.exception.code, 1)
                record = json.loads((root/'native-command').read_text().removeprefix(evidence.PREFIX))
                self.assertEqual(record['return_code'], None if timed_out else 7)
                self.assertEqual(record['error_class'], 'TimeoutExpired' if timed_out else 'CalledProcessError')
                self.assertTrue(record['files']['stdout']['truncated'])
                for path in (root/'native-command', root/'failure'):
                    self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
                    self.assertNotIn('fixture-token', path.read_text())
                self.assertEqual(sorted(path.name for path in root.iterdir()), ['failure', 'marker', 'native-command', 'source'])
                binary = root/'bin'
                binary.mkdir()
                (binary/'systemd-run').write_text('#!/bin/sh\nexit 0\n')
                (binary/'systemd-run').chmod(0o755)
                with redirect_stdout(io.StringIO()) as controller, self.assertRaises(fixture.CommandFailure):
                    fixture.command([sys.executable, '-B', '-', json.dumps({'root': name, 'unit': 'synthetic'}), '1000', name],
                                    fixture.START.encode(), env=dict(os.environ, PATH=str(binary)+':'+os.environ['PATH']))
                forwarded = controller.getvalue()
                self.assertIn('stdout-cause', forwarded)
                self.assertIn('stderr-cause', forwarded)
                self.assertIn('TimeoutExpired' if timed_out else 'CalledProcessError', forwarded)
                self.assertNotIn('fixture-token', forwarded)

    def test_retention_refuses_foreign_marker_and_symlink_temporary_file(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            (root/'marker').write_bytes(b'foreign')
            with self.assertRaises(AssertionError):
                evidence.retain(root, 'failure', {})
            (root/'marker').write_bytes(b'subyard-telemetry-delegated-v1')
            target = root/'foreign'
            target.write_bytes(b'unchanged')
            (root/'failure.tmp').symlink_to(target)
            with self.assertRaises(FileExistsError):
                evidence.retain(root, 'failure', {})
            self.assertEqual(target.read_bytes(), b'unchanged')
            self.assertFalse((root/'failure').exists())

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
