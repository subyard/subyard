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
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import safefs

NO_REPLACE_REFUSAL = (errno.EOPNOTSUPP, 'atomic no-replace rename requires renameat2(RENAME_NOREPLACE)')


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
                path = self.root/str(index)
                want = vector.get('python_outcome', vector['outcome'])
                operation = vector.get('operation', 'write-file')
                if operation == 'finish-directory':
                    path.mkdir()
                    path.chmod(vector['initial_mode'])
                    before = path.stat()
                    self.assertFalse(safefs.ensure_dir(path, mode=vector['mode']))
                    after = path.stat()
                    self.assertEqual(f'{stat.S_IMODE(after.st_mode):04o}', want)
                    self.assertEqual((after.st_uid, after.st_gid), (before.st_uid, before.st_gid))
                    continue
                if operation == 'resolve-file':
                    path.symlink_to('missing')
                    # Exercise the user-file policy even in the root permission run.
                    with patch.object(safefs.os, 'geteuid', return_value=1):
                        with self.assertRaises(FileNotFoundError):
                            safefs.resolve_file(path)
                        with self.assertRaises(FileNotFoundError):
                            safefs.write_file(path, b'new', follow_symlink=True)
                    self.assertEqual(want, 'dangling target refused')
                    self.assertEqual(os.readlink(path), 'missing')
                    self.assertFalse((self.root/'missing').exists())
                    continue
                self.assertEqual(operation, 'write-file')
                mode, kind = vector['mode'], vector['target']
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
                self.assertEqual(actual, want)
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

    def test_setgid_parent_and_restrictive_umask_converge(self):
        self.path.mkdir()
        self.path.chmod(0o2755)
        self.mask(0o077)
        fresh = self.path/'fresh'
        self.assertTrue(safefs.ensure_dir(fresh, mode=0o755))
        self.assertEqual(stat.S_IMODE(fresh.stat().st_mode), 0o2755)
        interrupted = self.path/'interrupted'
        interrupted.mkdir(mode=0o755)
        self.assertEqual(stat.S_IMODE(interrupted.stat().st_mode), 0o2700)
        safefs.write_file(interrupted/'value', b'data', parent_mode=0o755)
        self.assertEqual(stat.S_IMODE(interrupted.stat().st_mode), 0o2755)

    def test_finish_preserves_setuid_and_setgid_without_group_membership(self):
        self.path.mkdir()
        info = self.path.stat()
        uid = info.st_uid or 1
        for initial, target, wanted in ((0o2700, 0o755, 0o2700), (0o4500, 0o700, 0o4700)):
            with self.subTest(initial=oct(initial)):
                self.path.chmod(initial)
                with patch.object(safefs.os, 'geteuid', return_value=uid), \
                        patch.object(safefs.os, 'getegid', return_value=info.st_gid+1), \
                        patch.object(safefs.os, 'getgroups', return_value=[]):
                    self.assertFalse(safefs.ensure_dir(self.path, mode=target, owner=(info.st_uid, -1)))
                self.assertEqual(stat.S_IMODE(self.path.stat().st_mode), wanted)

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

    def test_concurrent_writers_can_replace(self):
        barrier = threading.Barrier(2, timeout=5)

        def fault(point):
            if point == safefs.BEFORE_PUBLISH:
                barrier.wait()

        with patch.object(safefs, 'FAULT', fault), concurrent.futures.ThreadPoolExecutor() as pool:
            list(pool.map(lambda data: safefs.write_file(self.path, data), (b'one', b'two')))
        self.assertIn(self.path.read_bytes(), (b'one', b'two'))

    def test_failed_no_replace_publish_leaves_no_target_or_staging_entry(self):
        stage = self.root/'stage'
        stage.mkdir()
        for code in (None, errno.ENOSYS, errno.EINVAL, errno.EOPNOTSUPP):
            libc = object() if code is None else SimpleNamespace(renameat2=lambda *args: -1)
            with self.subTest(code=code), patch.object(safefs.ctypes, 'CDLL', return_value=libc), \
                    patch.object(safefs.ctypes, 'get_errno', return_value=code), patch.object(safefs.os, 'link') as link:
                with self.assertRaises(OSError) as error:
                    safefs.write_file(self.path, b'new', stage_dir=stage, no_replace=True)
                self.assertEqual(error.exception.args, NO_REPLACE_REFUSAL)
                link.assert_not_called()
                self.assertFalse(self.path.exists())
                self.assertEqual(list(stage.iterdir()), [])

    def test_rename_noreplace_preserves_missing_source_errno(self):
        self.path.mkdir()
        target = self.root/'destination'
        with self.assertRaises(FileNotFoundError) as error:
            safefs.rename_noreplace(self.path/'missing', target)
        self.assertEqual(error.exception.errno, errno.ENOENT)
        self.assertFalse(target.exists())

    def test_no_replace_directory_without_renameat2_refuses_without_linking(self):
        self.path.mkdir()
        target = self.root/'destination'
        with patch.object(safefs.ctypes, 'CDLL', return_value=object()), patch.object(safefs.os, 'link') as link:
            with self.assertRaises(OSError) as error:
                safefs.rename_noreplace(self.path, target)
        self.assertEqual(error.exception.args, NO_REPLACE_REFUSAL)
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

    def test_repair_mode_narrows_owned_entries(self):
        def made(name, mode, directory=False):
            path = self.root/name
            path.mkdir() if directory else path.write_bytes(b'')
            os.chmod(path, mode)
            return path
        for path, deny, want in (
                (made('broad', 0o644), 0o077, 0o600),
                (made('readonly', 0o400), 0o077, 0o400),
                (made('readable', 0o644), 0o022, 0o644),
                (made('setuid', 0o4600), 0o077, 0o600),
                (made('exec', 0o755), 0o077, 0o700),
                (made('dir', 0o755, True), 0o077, 0o700),
                (made('sticky', 0o1777, True), 0o077, 0o1700)):
            safefs.repair_mode(path, deny)
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), want, path.name)
        fd = os.open(made('fd', 0o640), os.O_RDONLY)
        try:
            safefs.repair_mode(fd, 0o077)
        finally:
            os.close(fd)
        self.assertEqual(stat.S_IMODE((self.root/'fd').stat().st_mode), 0o600)

    def test_repair_mode_refuses_unsafe_entries(self):
        target = self.root/'target-file'
        target.write_bytes(b'')
        os.chmod(target, 0o644)
        (self.root/'link').symlink_to(target)
        os.link(target, self.root/'hard')
        os.mkfifo(self.root/'fifo', 0o644)
        for name, reason in (('link', safefs.SYMLINK), ('hard', safefs.MULTIPLE_LINKS),
                             ('fifo', safefs.NOT_REGULAR)):
            with self.assertRaises(safefs.Unsafe) as raised:
                safefs.repair_mode(self.root/name, 0o077)
            self.assertEqual(raised.exception.reason, reason)
            self.assertIn(name, str(raised.exception))
        self.assertEqual(stat.S_IMODE(target.stat().st_mode), 0o644)
        if os.geteuid() != 0:
            with self.assertRaises(safefs.Unsafe) as raised:
                safefs.repair_mode('/etc/passwd', 0o077)
            self.assertEqual(raised.exception.reason, safefs.OWNER)

if __name__ == '__main__':
    unittest.main()
