#!/usr/bin/env python3
"""Local subordinate-label CAS and shadow-compatible per-file lock contracts."""
from contextlib import contextmanager
import importlib.util
import os
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('identity_subids', Path(__file__).resolve().parents[1]/'subids.py')
subids = importlib.util.module_from_spec(spec)
spec.loader.exec_module(subids)


class SubordinateIDsTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='subyard-subids-', dir='/tmp')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        (self.root/'etc').mkdir(mode=0o700)
        self.before = b'dev:100000:65536\n1001:200000:1000\nother:300000:65536\n'
        self.events = []
        @contextmanager
        def account_lock():
            self.events.append('password-lock')
            try:
                yield
            finally:
                self.events.append('password-unlock')
        self.helper = subids.SubordinateIDs(self.root, os.getuid(), account_lock)
        for name in ('subuid', 'subgid'):
            self.write(name, self.before)

    def write(self, name, data):
        path = self.root/'etc'/name
        path.write_bytes(data)
        path.chmod(0o644)

    def test_rebind_preserves_ranges_numeric_labels_foreign_bytes_and_metadata(self):
        recipe = self.helper.observe('dev', 'next', 1001)
        for _ in range(2):
            self.helper.rebind(recipe, 'dev', 'next', 1001)
            for name in ('subuid', 'subgid'):
                path = self.root/'etc'/name
                self.assertEqual(path.read_bytes(), self.before.replace(b'dev:', b'next:', 1))
                self.assertEqual(path.stat().st_mode & 0o777, 0o644)
                self.assertEqual(path.stat().st_uid, os.getuid())
                self.assertEqual(path.stat().st_nlink, 1)
            self.assertFalse(list((self.root/'etc').glob('*.lock')))
        self.assertEqual(self.events, ['password-lock', 'password-unlock']*2)

    def test_interrupted_between_databases_resumes_without_deallocating_ranges(self):
        recipe = self.helper.observe('dev', 'next', 1001)
        def cut(name):
            if name == 'subuid':
                raise InterruptedError('fixture interruption')
        with self.assertRaises(InterruptedError):
            self.helper.rebind(recipe, 'dev', 'next', 1001, cut)
        self.assertEqual((self.root/'etc/subgid').read_bytes(), self.before)
        self.helper.rebind(recipe, 'dev', 'next', 1001)
        self.assertEqual((self.root/'etc/subgid').read_bytes(), self.before.replace(b'dev:', b'next:', 1))

    def test_foreign_change_refuses_without_rewriting_either_file(self):
        recipe = self.helper.observe('dev', 'next', 1001)
        altered = self.before+b'external:500000:1000\n'
        self.write('subuid', altered)
        with self.assertRaises(ValueError):
            self.helper.rebind(recipe, 'dev', 'next', 1001)
        self.assertEqual((self.root/'etc/subuid').read_bytes(), altered)
        self.assertEqual((self.root/'etc/subgid').read_bytes(), self.before)

    def test_external_provider_and_overlapping_owner_refuse_during_assessment(self):
        self.write('nsswitch.conf', b'subid: sss\n')
        with self.assertRaises(ValueError):
            self.helper.observe('dev', 'next', 1001)
        self.write('nsswitch.conf', b'subid: files\n')
        self.write('subuid', self.before+b'foreign:100001:100\n')
        with self.assertRaises(ValueError):
            self.helper.observe('dev', 'next', 1001)
        self.assertFalse(list((self.root/'etc').glob('*.lock')))

    def test_killed_writer_after_link_recovers_only_exact_dead_pid_pair(self):
        recipe = self.helper.observe('dev', 'next', 1001)
        pid = os.fork()
        if pid == 0:
            original = os.link
            def cut(*args, **kwargs):
                original(*args, **kwargs)
                os._exit(75)
            os.link = cut
            self.helper.rebind(recipe, 'dev', 'next', 1001)
            os._exit(99)
        waited, status = os.waitpid(pid, 0)
        self.assertEqual(waited, pid)
        self.assertEqual(os.waitstatus_to_exitcode(status), 75)
        lock = self.root/'etc/subuid.lock'
        abandoned = self.root/f'etc/subuid.{pid}'
        self.assertEqual(lock.stat().st_nlink, 2)
        self.assertEqual(lock.stat().st_ino, abandoned.stat().st_ino)
        foreign = self.root/'etc/foreign-link'
        os.link(lock, foreign)
        with self.assertRaises(ValueError): self.helper.rebind(recipe, 'dev', 'next', 1001)
        self.assertEqual(lock.stat().st_nlink, 3)
        self.assertEqual((self.root/'etc/subuid').read_bytes(), self.before)
        foreign.unlink()
        self.helper.rebind(recipe, 'dev', 'next', 1001)
        self.assertFalse(lock.exists())
        self.assertFalse(abandoned.exists())
        for name in ('subuid', 'subgid'):
            self.assertEqual((self.root/'etc'/name).read_bytes(), self.before.replace(b'dev:', b'next:', 1))

    def test_live_shadow_lock_is_preserved_and_own_temporary_is_removed(self):
        recipe = self.helper.observe('dev', 'next', 1001)
        lock = self.root/'etc/subuid.lock'
        lock.write_bytes(str(os.getpid()).encode()+b'\0')
        lock.chmod(0o600)
        before = lock.stat()
        with self.assertRaises(ValueError):
            self.helper.rebind(recipe, 'dev', 'next', 1001)
        self.assertEqual(lock.stat().st_ino, before.st_ino)
        self.assertFalse((self.root/f'etc/subuid.{os.getpid()}').exists())
        self.assertEqual((self.root/'etc/subuid').read_bytes(), self.before)


if __name__ == '__main__':
    unittest.main()
