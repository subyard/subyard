#!/usr/bin/env python3
"""Host-free cache deletion and exact project backup/restore contracts."""
import importlib.util
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[1]


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, ROOT / path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


soft = load('cleanup', 'scripts/lib/yard-cleanup.py')
hard = load('preservation', 'scripts/lib/yard-cleanup-hard.py')


class Cleanup(unittest.TestCase):
    def setUp(self):
        self.fixture = tempfile.TemporaryDirectory(prefix='yard-cleanup-', dir='/tmp')
        self.addCleanup(self.fixture.cleanup)
        self.root = Path(self.fixture.name)

    def assess(self, paths):
        return soft.assess([str(p) for p in paths], allowed_mounts=('/', '/srv', '/tmp'))

    def project(self, name='one', mode='copy'):
        srv = self.root / 'backup'
        source = srv / 'workspaces' / name / 'src'
        source.mkdir(parents=True)
        request = dict(projects=[dict(projectId=name, mode=mode, yardPath='/srv/workspaces/' + name + '/src')])
        return srv, source, request

    def test_soft_preserves_root_modes_and_symlink_targets(self):
        cache, outside = self.root / 'cache', self.root / 'outside'
        cache.mkdir(mode=0o750)
        outside.mkdir()
        (outside / 'keep').write_text('retained')
        (cache / 'link').symlink_to(outside)
        (cache / 'nested').mkdir()
        (cache / 'nested' / 'remove').write_text('cache')
        before = cache.stat()
        soft.apply(self.assess([cache]))
        self.assertEqual(list(cache.iterdir()), [])
        after = cache.stat()
        self.assertEqual((after.st_ino, after.st_mode, after.st_uid, after.st_gid),
                         (before.st_ino, before.st_mode, before.st_uid, before.st_gid))
        self.assertEqual((outside / 'keep').read_text(), 'retained')

    def test_missing_cache_is_noop_and_replaced_root_refuses(self):
        soft.apply(self.assess([self.root / 'absent']))
        cache = self.root / 'cache'
        cache.mkdir()
        approved = self.assess([cache])
        cache.rename(self.root / 'old')
        cache.mkdir()
        (cache / 'keep').write_text('new root')
        with self.assertRaises(ValueError):
            soft.apply(approved)
        self.assertTrue((cache / 'keep').exists())

    def test_symlink_root_refuses_before_deletion(self):
        (self.root / 'link').symlink_to(self.root)
        with self.assertRaises(OSError):
            self.assess([self.root / 'link'])

    def test_nested_mount_is_preserved(self):
        child = self.root / 'mounted'
        child.mkdir()
        (child / 'keep').write_text('other filesystem')
        approved = self.assess([self.root])
        real = soft.mount_id
        with mock.patch.object(soft, 'mount_id', side_effect=lambda fd: real(fd) + (os.fstat(fd).st_ino == child.stat().st_ino)):
            result = soft.apply(approved)
        self.assertEqual(result['mounts_preserved'], 1)
        self.assertTrue((child / 'keep').exists())

    def test_copy_preserves_git_dirty_untracked_modes_symlinks_and_hardlinks(self):
        srv, source, request = self.project()
        subprocess.run(['git', 'init', '-q', str(source)], check=True)
        (source / 'tracked').write_text('committed')
        subprocess.run(['git', '-C', str(source), 'add', 'tracked'], check=True)
        subprocess.run(['git', '-C', str(source), '-c', 'user.name=Fixture', '-c', 'user.email=fixture@invalid', 'commit', '-qm', 'baseline'], check=True)
        (source / 'tracked').write_text('dirty')
        (source / 'untracked').write_text('private task data')
        (source / 'untracked').chmod(0o600)
        (source / 'link').symlink_to('tracked')
        os.link(source / 'tracked', source.parent / 'linked')
        request['preserved'] = hard.inspect(request, srv)
        destination = self.root / 'restored'
        hard.restore(request, str(srv), str(destination))
        hard.restore(request, str(srv), str(destination))  # interruption retry
        self.assertEqual(hard.inspect(request, destination), request['preserved'])
        result = destination / 'workspaces/one/src'
        self.assertNotEqual((result / 'tracked').stat().st_ino, (source / 'tracked').stat().st_ino)
        self.assertEqual((result / 'tracked').stat().st_ino, (result.parent / 'linked').stat().st_ino)
        self.assertIn(' M tracked', subprocess.check_output(['git', '-C', str(result), 'status', '--porcelain'], text=True))

    def test_external_relative_and_absolute_alternates_refuse(self):
        for absolute in (False, True):
            with self.subTest(absolute=absolute):
                srv, source, request = self.project(str(absolute))
                # Include both registered wrappers as the fixture grows.
                request['projects'] = [dict(projectId=p.name, mode='copy') for p in (srv / 'workspaces').iterdir()]
                objects = source / '.git/objects'
                (objects / 'info').mkdir(parents=True)
                external = self.root / 'external-objects'
                external.mkdir(exist_ok=True)
                alternate = str(external) if absolute else os.path.relpath(external, objects)
                (objects / 'info/alternates').write_text(alternate + '\n')
                with self.assertRaisesRegex(ValueError, 'alternates'):
                    hard.inspect(request, srv)
                (objects / 'info/alternates').unlink()

    def test_external_git_indirection_refuses(self):
        srv, source, request = self.project()
        (source / '.git').write_text('gitdir: /outside/git\n')
        with self.assertRaisesRegex(ValueError, 'Git metadata'):
            hard.inspect(request, srv)

    def test_external_git_object_symlink_refuses(self):
        srv, source, request = self.project()
        (source / '.git').mkdir()
        external = self.root / 'external-objects'
        external.mkdir()
        (source / '.git/objects').symlink_to(external)
        with self.assertRaisesRegex(ValueError, 'Git metadata symlink'):
            hard.inspect(request, srv)

    def test_bind_source_is_not_copied_or_overwritten(self):
        srv, source, request = self.project(mode='bind')
        (source / 'not-backed-up').write_text('hidden underlying mountpoint data')
        (source.parent / 'task').write_text('wrapper data')
        request['preserved'] = hard.inspect(request, srv)
        destination = self.root / 'restored'
        target = destination / 'workspaces/one/src'
        target.mkdir(parents=True)
        (target / 'host').write_text('external host edits')
        hard.restore(request, str(srv), str(destination))
        self.assertEqual((target / 'host').read_text(), 'external host edits')
        self.assertFalse((target / 'not-backed-up').exists())
        self.assertEqual((target.parent / 'task').read_text(), 'wrapper data')

    def test_environment_directory_presence_is_not_in_witness(self):
        srv, _, request = self.project()
        before = hard.inspect(request, srv)
        (srv / 'cache').mkdir()
        (srv / 'unclassified-runtime').mkdir()
        self.assertEqual(hard.inspect(request, srv), before)

    def test_unknown_workspace_and_changed_backup_refuse(self):
        srv, source, request = self.project()
        request['preserved'] = hard.inspect(request, srv)
        (source / 'change').write_text('after verification')
        with self.assertRaisesRegex(ValueError, 'backup project content changed'):
            hard.restore(request, str(srv), str(self.root / 'restored'))
        (srv / 'workspaces/unregistered').mkdir()
        with self.assertRaisesRegex(ValueError, 'unregistered'):
            hard.inspect(request, srv)


if __name__ == '__main__':
    unittest.main()
