"""Filesystem contracts shared with Go and interruption/retry coverage."""

import concurrent.futures
import errno
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

import safefs


class SafeFiles(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory(dir='/tmp')
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        self.path = self.root/'target'

    def mask(self, bits):
        self.addCleanup(os.umask, os.umask(bits))

    def test_parity_vectors(self):
        vectors = json.loads(Path(__file__).with_name('testdata').joinpath('python-parity.json').read_bytes())
        for index, vector in enumerate(vectors):
            with self.subTest(vector=vector):
                mode, kind = vector['mode'], vector['target']
                path = self.root/str(index)
                other = self.root/'untouched'
                other.write_bytes(b'untouched')
                if kind == 'regular':
                    path.write_bytes(b'old')
                elif kind == 'symlink':
                    path.symlink_to(other)
                elif kind == 'directory':
                    path.mkdir()
                    path.chmod(0o700)
                elif kind == 'fifo':
                    os.mkfifo(path)
                before = os.umask(vector['umask'])
                try:
                    safefs.write_file(path, b'new', mode=mode, no_replace=vector.get('no_replace', False))
                    actual = 'published'
                except safefs.Unsafe as error:
                    actual = {safefs.SYMLINK: 'symlink', safefs.NOT_REGULAR: 'not regular'}[error.reason]
                except FileExistsError:
                    actual = 'exists'
                finally:
                    os.umask(before)
                self.assertEqual(actual, vector['outcome'])
                if actual == 'published':
                    self.assertEqual(path.read_bytes(), b'new')
                    self.assertEqual(stat.S_IMODE(path.stat().st_mode), mode)
                elif kind == 'regular':
                    self.assertEqual(path.read_bytes(), b'old')
                elif kind == 'symlink':
                    self.assertTrue(path.is_symlink())
                    self.assertEqual(other.read_bytes(), b'untouched')

    def test_interruption_visibility_cleanup_and_retry(self):
        for point in (safefs.AFTER_STAGE, safefs.AFTER_WRITE, safefs.BEFORE_PUBLISH, safefs.AFTER_PUBLISH):
            for error in (InterruptedError, safefs.SimulatedCrash):
                with self.subTest(point=point, error=error):
                    safefs.write_file(self.path, b'old')

                    def fault(at):
                        if at == point:
                            raise error(at)

                    with patch.object(safefs, 'FAULT', fault), self.assertRaises(error):
                        safefs.write_file(self.path, b'new', prefix='.', suffix='.tmp')
                    published = point == safefs.AFTER_PUBLISH
                    self.assertEqual(self.path.read_bytes(), b'new' if published else b'old')
                    stages = list(self.root.glob('.*.tmp'))
                    self.assertEqual(len(stages), int(not published and error is safefs.SimulatedCrash))
                    safefs.write_file(self.path, b'new', prefix='.', suffix='.tmp')
                    self.assertEqual(self.path.read_bytes(), b'new')
                    for stage in stages:
                        stage.unlink()

    def test_process_kill_at_publication(self):
        self.path.write_bytes(b'old')
        program = '''import os, sys
sys.path.insert(0, sys.argv[1])
import safefs
def interrupt(point):
    if point == sys.argv[2]:
        os._exit(137)
safefs.FAULT = interrupt
safefs.write_file(sys.argv[3], b'new')
'''
        for point, content, stages in ((safefs.BEFORE_PUBLISH, b'old', 1), (safefs.AFTER_PUBLISH, b'new', 1)):
            result = subprocess.run([sys.executable, '-IBc', program, str(Path(safefs.__file__).parent),
                                     point, str(self.path)], timeout=5)
            self.assertEqual(result.returncode, 137)
            self.assertEqual(self.path.read_bytes(), content)
            self.assertEqual(len(list(self.root.glob('.target.tmp-*'))), stages)
        safefs.write_file(self.path, b'final')
        self.assertEqual(self.path.read_bytes(), b'final')

    def test_directory_interruption_and_modes(self):
        self.mask(0o277)
        for point, mode in ((safefs.AFTER_DIR_CREATE, 0o500), (safefs.AFTER_DIR_CHMOD, 0o700)):
            def fault(at):
                if at == point:
                    raise safefs.SimulatedCrash(at)

            with patch.object(safefs, 'FAULT', fault), self.assertRaises(safefs.SimulatedCrash):
                safefs.ensure_private_dir(self.path)
            self.assertEqual(stat.S_IMODE(self.path.stat().st_mode), mode)
            self.assertFalse(safefs.ensure_private_dir(self.path))
            self.assertEqual(stat.S_IMODE(self.path.stat().st_mode), 0o700)
            self.path.rmdir()

        self.assertTrue(safefs.ensure_dir(self.path/'child', parent_mode=0o700))
        self.assertEqual(stat.S_IMODE(self.path.stat().st_mode), 0o700)
        self.path.chmod(0o750)
        with self.assertRaisesRegex(safefs.DirState, safefs.MODE):
            safefs.ensure_dir(self.path)
        self.assertFalse(safefs.ensure_dir(self.path, existing=safefs.ACCEPT))
        self.assertEqual(stat.S_IMODE(self.path.stat().st_mode), 0o750)
        with self.assertRaisesRegex(safefs.DirState, safefs.MODE):
            safefs.ensure_dir(self.path, existing=safefs.EXACT)
        safefs.ensure_dir(self.path, existing=safefs.REPAIR)
        self.assertEqual(stat.S_IMODE(self.path.stat().st_mode), 0o700)
        foreign = os.geteuid() + 1
        with self.assertRaisesRegex(safefs.DirState, safefs.OWNER):
            safefs.ensure_dir(self.path, owner=(foreign, -1))
        with self.assertRaisesRegex(safefs.Unsafe, safefs.OWNER):
            safefs.ensure_dir(self.path/'foreign', owner=(foreign, -1))
        self.assertFalse((self.path/'foreign').exists())

    def test_parity_finish_keeps_sticky_and_leaves_set_id_directories_untouched(self):
        self.path.mkdir()
        for before, after in ((0o1500, 0o1700), (0o2500, 0o2500), (0o4500, 0o4500), (0o7500, 0o7500)):
            with self.subTest(mode=oct(before)):
                self.path.chmod(before)
                identity = self.path.stat().st_uid, self.path.stat().st_gid
                self.assertFalse(safefs.ensure_dir(self.path))
                info = self.path.stat()
                self.assertEqual(stat.S_IMODE(info.st_mode), after)
                self.assertEqual((info.st_uid, info.st_gid), identity)

    def test_parity_dangling_user_link_is_refused(self):
        self.path.symlink_to('missing')
        # Exercise the user-file policy even in the root permission run.
        with patch.object(safefs.os, 'geteuid', return_value=1):
            with self.assertRaises(FileNotFoundError):
                safefs.resolve_file(self.path)
            with self.assertRaises(FileNotFoundError):
                safefs.write_file(self.path, b'new', follow_symlink=True)
        self.assertEqual(os.readlink(self.path), 'missing')
        self.assertFalse((self.root/'missing').exists())

    def test_anchored_publication_and_root_checks(self):
        before = self.root/'before'
        before.mkdir(mode=0o700)
        after = before.with_name('after')
        with safefs.create(before/'value') as pending:
            pending.file.write(b'data')
            before.rename(after)
            before.mkdir(mode=0o700)
            pending.commit()
        self.assertEqual((after/'value').read_bytes(), b'data')
        self.assertFalse((before/'value').exists())
        (before/'link').symlink_to(after)
        with self.assertRaisesRegex(safefs.Unsafe, safefs.SYMLINK):
            safefs.write_file(before/'link/value', b'unsafe', root=self.root)
        with self.assertRaisesRegex(safefs.Unsafe, safefs.ESCAPES_ROOT):
            safefs.write_file(before/'../outside', b'unsafe', root=before)

    def test_interrupted_parent_creation_is_finished_before_creating_children(self):
        self.mask(0o777)
        leaf = self.path/'nested'/'leaf'

        def fault(point):
            if point == safefs.AFTER_DIR_CREATE:
                raise safefs.SimulatedCrash(point)

        with patch.object(safefs, 'FAULT', fault), self.assertRaises(safefs.SimulatedCrash):
            safefs.ensure_dir(leaf, parent_mode=0o700)
        self.assertTrue(self.path.is_dir())
        self.assertEqual(stat.S_IMODE(self.path.stat().st_mode), 0)
        self.assertTrue(safefs.ensure_dir(leaf, parent_mode=0o700))
        self.assertEqual(stat.S_IMODE(self.path.stat().st_mode), 0o700)
        self.assertEqual(stat.S_IMODE(leaf.stat().st_mode), 0o700)

    def test_user_file_symlink_and_preserved_target_mode(self):
        other = self.root/'other'
        other.write_bytes(b'old')
        other.chmod(0o640)
        self.path.symlink_to('other')
        if os.geteuid() == 0:
            with self.assertRaisesRegex(safefs.Unsafe, safefs.SYMLINK):
                safefs.write_file(self.path, b'new', follow_symlink=True)
            self.assertEqual(other.read_bytes(), b'old')
        else:
            safefs.write_file(self.path, b'new', follow_symlink=True, preserve_mode=True)
            self.assertEqual(other.read_bytes(), b'new')
            other.unlink()
            with self.assertRaises(FileNotFoundError):
                safefs.write_file(self.path, b'new', follow_symlink=True)
        self.assertTrue(self.path.is_symlink())
        if other.exists():
            self.assertEqual(stat.S_IMODE(other.stat().st_mode), 0o640)

    def test_metadata_before_publish(self):
        self.mask(0o777)
        owner = (65534, 65534) if os.geteuid() == 0 else (os.geteuid(), os.getegid())

        def check(pending):
            info = os.fstat(pending.file.fileno())
            self.assertEqual((info.st_uid, info.st_gid, stat.S_IMODE(info.st_mode)), (*owner, 0o2640))
            self.assertFalse(self.path.exists())

        safefs.write_file(self.path, b'new', mode=0o2640, owner=owner, check=check)
        info = self.path.stat()
        self.assertEqual((info.st_uid, info.st_gid, stat.S_IMODE(info.st_mode)), (*owner, 0o2640))

    def test_concurrent_writers_and_no_replace_fallback(self):
        barrier = threading.Barrier(2, timeout=5)

        def fault(point):
            if point == safefs.BEFORE_PUBLISH:
                barrier.wait()

        with patch.object(safefs, 'FAULT', fault), concurrent.futures.ThreadPoolExecutor() as pool:
            list(pool.map(lambda data: safefs.write_file(self.path, data), (b'one', b'two')))
        self.assertIn(self.path.read_bytes(), (b'one', b'two'))
        stage = self.root/'stage'
        stage.mkdir()
        with patch.object(safefs.ctypes, 'CDLL', return_value=object()):
            with self.assertRaises(FileExistsError):
                safefs.write_file(self.path, b'new', stage_dir=stage, no_replace=True)
            self.path.unlink()
            safefs.write_file(self.path, b'new', stage_dir=stage, no_replace=True)
        self.assertEqual(self.path.read_bytes(), b'new')
        self.assertEqual(list(stage.iterdir()), [])

    def test_no_replace_directory_without_renameat2_refuses_without_linking(self):
        self.path.mkdir()
        target = self.root/'destination'
        with patch.object(safefs.ctypes, 'CDLL', return_value=object()), patch.object(safefs.os, 'link') as link:
            with self.assertRaisesRegex(OSError, 'atomic no-replace directory rename requires renameat2') as error:
                safefs.rename_noreplace(self.path, target)
        self.assertEqual(error.exception.errno, errno.EOPNOTSUPP)
        link.assert_not_called()
        self.assertTrue(self.path.is_dir())
        self.assertFalse(target.exists())

    def test_rename_noreplace_rejects_nul_before_calling_libc(self):
        self.path.write_bytes(b'original')
        target = self.root/'destination'
        for src, dst in ((str(self.path)+'\0suffix', target), (self.path, os.fsencode(target)+b'\0suffix')):
            with self.subTest(src=src, dst=dst), patch.object(safefs.ctypes, 'CDLL') as libc:
                with self.assertRaisesRegex(ValueError, 'embedded null byte'):
                    safefs.rename_noreplace(src, dst)
                libc.assert_not_called()
        self.assertEqual(self.path.read_bytes(), b'original')
        self.assertFalse(target.exists())

    def test_lock_releases_flock_while_duplicate_remains_open(self):
        with safefs.lock(self.path) as fd:
            duplicate = os.dup(fd)
            self.addCleanup(os.close, duplicate)
            with self.assertRaises(BlockingIOError), safefs.lock(self.path, deadline=time.monotonic()):
                self.fail('locked an already locked file')
        with safefs.lock(self.path, deadline=time.monotonic()):
            self.assertTrue(stat.S_ISREG(os.fstat(duplicate).st_mode))

    def test_read_and_lock_refuse_unsafe_entries(self):
        self.assertIsNone(safefs.read_regular(self.path))
        os.mkfifo(self.path)
        with self.assertRaisesRegex(safefs.Unsafe, safefs.NOT_REGULAR):
            safefs.read_regular(self.path)
        self.path.unlink()
        self.path.symlink_to('missing')
        with self.assertRaisesRegex(safefs.Unsafe, safefs.SYMLINK):
            safefs.read_regular(self.path)
        with self.assertRaises(OSError) as error, safefs.lock(self.path):
            self.fail('locked a symlink')
        self.assertEqual(error.exception.errno, errno.ELOOP)
        self.path.unlink()
        with safefs.lock(self.path) as fd:
            self.assertTrue(stat.S_ISREG(os.fstat(fd).st_mode))
            with self.assertRaises(BlockingIOError), safefs.lock(self.path, deadline=time.monotonic()):
                self.fail('locked an already locked file')
        safefs.write_file(self.path, b'hello')
        self.assertEqual(safefs.read_regular(self.path, max_bytes=5, single_link=True), b'hello')
        with self.assertRaisesRegex(safefs.Unsafe, safefs.TOO_LARGE):
            safefs.read_regular(self.path, max_bytes=4)


if __name__ == '__main__':
    unittest.main()
