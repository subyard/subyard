#!/usr/bin/env python3
"""Disposable integration diagnostics survive cleanup without following links."""
import io
import os
from pathlib import Path
import runpy
import subprocess
import sys
import tempfile
import unittest
from unittest import mock


class FailureTailsTest(unittest.TestCase):
    def test_failure_tails_are_owned_bounded_newest_first_and_never_follow_links(self):
        helper = Path(__file__).with_name('failure-tails.py')
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            root = Path(temporary)/'state'
            root.mkdir(mode=0o700)
            (root/'.marker').write_text('fixture-owned\n')
            for index in range(12):
                path = root/f'{index:02}.log'
                path.write_bytes(b'head-must-disappear'+b'x'*5000+f'tail-{index:02}'.encode())
                os.utime(path, ns=(index+1, index+1))
            outside = Path(temporary)/'outside'
            outside.write_text('symlink-target-must-not-appear')
            nested = root/'home'
            nested.mkdir()
            (nested/'ignored.json').write_text('nested-must-not-appear')
            (root/'link.log').symlink_to(outside)
            (root/'linked-home').symlink_to(nested, target_is_directory=True)
            os.mkfifo(root/'pipe.log')
            (root/'small.stderr').write_text('complete diagnostic')
            (root/'small.json').write_text('{"fixture":true}')
            def invoke(state=root, marker='fixture-owned'):
                return subprocess.run([sys.executable, '-B', str(helper), str(state), marker],
                                      capture_output=True, timeout=3)
            result = invoke()
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout, b'')
            self.assertLessEqual(len(result.stderr), 32768)
            self.assertIn(b'complete diagnostic', result.stderr)
            self.assertIn(b'{"fixture":true}', result.stderr)
            self.assertIn(b'tail-11', result.stderr)
            self.assertLess(result.stderr.index(b"'11.log'"), result.stderr.index(b"'10.log'"))
            self.assertNotIn(b'head-must-disappear', result.stderr)
            self.assertNotIn(b'symlink-target-must-not-appear', result.stderr)
            self.assertNotIn(b'nested-must-not-appear', result.stderr)
            self.assertNotIn(b'link.log', result.stderr)
            self.assertIn(b'[truncated]', result.stderr)
            self.assertIn(b'[total truncated]', result.stderr)
            self.assertNotEqual(invoke(marker='foreign').returncode, 0)
            linked = Path(temporary)/'linked-state'
            linked.symlink_to(root, target_is_directory=True)
            self.assertNotEqual(invoke(state=linked).returncode, 0)
            (root/'.marker').unlink()
            (root/'.marker').symlink_to(root/'small.json')
            self.assertNotEqual(invoke().returncode, 0)

    def test_unreadable_entry_does_not_suppress_other_tails(self):
        helper = Path(__file__).with_name('failure-tails.py')
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            root = Path(temporary)
            (root/'.marker').write_text('fixture-owned\n')
            (root/'readable.log').write_text('retained diagnostic')
            bad = mock.Mock()
            bad.stat.side_effect = OSError('unreadable')
            with os.scandir(root) as scan:
                entries = [bad, *scan]
            stderr = mock.Mock(buffer=io.BytesIO())
            with mock.patch.object(sys, 'argv', [str(helper), temporary, 'fixture-owned']), \
                 mock.patch.object(os, 'scandir', return_value=entries), mock.patch.object(sys, 'stderr', stderr):
                runpy.run_path(str(helper))
            self.assertIn(b'retained diagnostic', stderr.buffer.getvalue())


if __name__ == '__main__':
    unittest.main()
