#!/usr/bin/env python3
"""Profile-owned clone/link contracts against disposable local repositories."""
import contextlib
import importlib.util
import io
import os
from pathlib import Path
import signal
import subprocess
import tempfile
import time
import unittest
from unittest import mock

PROFILE = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('clone_skills', PROFILE / 'clone.py')
clone = importlib.util.module_from_spec(spec)
spec.loader.exec_module(clone)


def git(*arguments, cwd=None):
    subprocess.run(['git', *arguments], cwd=cwd, check=True, capture_output=True,
                   env={'HOME': '/nonexistent', 'PATH': os.environ['PATH'],
                        'GIT_CONFIG_NOSYSTEM': '1', 'GIT_AUTHOR_NAME': 't',
                        'GIT_AUTHOR_EMAIL': 't@example.org', 'GIT_COMMITTER_NAME': 't',
                        'GIT_COMMITTER_EMAIL': 't@example.org'})


class CloneLinks(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='subyard-user-skills-', dir='/tmp')
        self.addCleanup(self.temporary.cleanup)
        self.base = Path(self.temporary.name)
        self.home = self.base / 'home'
        self.home.mkdir()
        self.root = self.home / clone.ROOT
        for name, value in {'CANONICAL': self.base / 'canonical',
                            'GITHUB_WRAPPER': self.base / 'missing-wrapper',
                            'SAFE_PATH': os.environ['PATH']}.items():
            patcher = mock.patch.object(clone, name, value)
            patcher.start()
            self.addCleanup(patcher.stop)

    def repository(self, name, skills=('alpha',)):
        work = self.base / 'work' / name
        work.mkdir(parents=True)
        for skill in skills:
            path = work / 'skills' / skill / 'SKILL.md'
            path.parent.mkdir(parents=True)
            path.write_text('---\nname: ' + skill + '\ndescription: a\n---\n')
        git('init', '-q', '-b', 'main', str(work))
        git('add', '-A', cwd=work)
        git('commit', '-q', '-m', 'skills', cwd=work)
        bare = self.base / 'remotes' / (name + '.git')
        git('clone', '-q', '--bare', str(work), str(bare))
        return str(bare)

    def run_hook(self, mode, *urls, post_apply=False):
        errors = io.StringIO()
        with contextlib.redirect_stderr(errors):
            code = clone.run(mode, urls, self.home, post_apply)
        return code, errors.getvalue()

    def target(self, url, skill='alpha'):
        return self.root / clone.source_name(url) / 'skills' / skill

    def test_clone_once_link_and_plain_git_update(self):
        url = self.repository('one')
        self.assertEqual(self.run_hook('check', url)[0], 1)
        self.assertEqual(self.run_hook('apply', url), (0, ''))
        for harness in clone.ADAPTERS:
            self.assertEqual(os.readlink(self.home / harness / 'alpha'), str(self.target(url)))
        self.assertEqual(self.run_hook('check', url)[0], 0)
        target = self.target(url)
        (target / 'local').write_text('keep my edits')
        work = self.base / 'work/one'
        (work / 'skills/alpha/new').write_text('from upstream')
        git('add', '-A', cwd=work)
        git('commit', '-q', '-m', 'update', cwd=work)
        git('push', '-q', url, 'main', cwd=work)
        self.assertEqual(self.run_hook('apply', url), (0, ''))
        self.assertFalse((target / 'new').exists(), 'init updated the clone')
        git('pull', '--ff-only', cwd=target.parents[1])
        self.assertEqual((target / 'new').read_text(), 'from upstream')
        self.assertEqual((target / 'local').read_text(), 'keep my edits')
        self.assertEqual(self.run_hook('apply'), (0, ''))
        self.assertFalse(os.path.lexists(self.home / '.claude/skills/alpha'))
        self.assertTrue((target / 'local').is_file(), 'deselection removed clone content')

    def test_unavailable_source_warns_and_next_init_retries_statelessly(self):
        url = str(self.base / 'remotes/later.git')
        code, warning = self.run_hook('apply', url)
        self.assertEqual(code, 0)
        self.assertIn(clone.source_name(url), warning)
        self.assertIn('authorize Git access or correct USER_SKILL_SOURCES, then run yard init', warning)
        self.assertNotIn(url, warning)
        self.assertFalse(self.target(url).parents[1].exists())
        self.assertEqual(self.run_hook('check', url)[0], 1)
        self.assertEqual(self.run_hook('check', url, post_apply=True)[0], 0)
        self.repository('later')
        self.assertEqual(self.run_hook('apply', url), (0, ''))
        self.assertEqual(self.run_hook('check', url)[0], 0)
        self.assertTrue((self.home / '.claude/skills/alpha').is_symlink())

    def test_first_source_wins_and_foreign_paths_and_reserved_names_stay(self):
        first = self.repository('first', ('alpha', 'beta', 'subyard-own'))
        second = self.repository('second', ('alpha', 'gamma'))
        occupied = self.home / '.claude/skills/beta'
        occupied.parent.mkdir(parents=True)
        occupied.write_text('mine')
        self.assertEqual(self.run_hook('apply', first, second)[0], 0)
        self.assertEqual(occupied.read_text(), 'mine')
        self.assertEqual(os.readlink(self.home / '.agents/skills/alpha'), str(self.target(first)))
        self.assertFalse(os.path.lexists(self.home / '.agents/skills/subyard-own'))
        foreign = self.home / '.agents/skills/foreign'
        foreign.symlink_to('/elsewhere')
        self.assertEqual(self.run_hook('apply', second)[0], 0)
        self.assertEqual(os.readlink(self.home / '.agents/skills/alpha'), str(self.target(second)))
        self.assertEqual(os.readlink(foreign), '/elsewhere')
        self.assertTrue(self.target(first).is_dir())
        self.assertEqual(self.run_hook('check', second)[0], 0)

    def test_symlinked_parents_root_and_skill_sources_are_not_followed(self):
        url = self.repository('one')
        elsewhere = self.base / 'elsewhere'
        elsewhere.mkdir()
        (self.home / '.claude').symlink_to(elsewhere)
        self.assertEqual(self.run_hook('apply', url)[0], 0)
        self.assertEqual(list(elsewhere.iterdir()), [])
        target = self.target(url)
        (target.parent / 'foreign').symlink_to(target, target_is_directory=True)
        self.assertEqual(self.run_hook('apply', url)[0], 0)
        self.assertFalse(os.path.lexists(self.home / '.agents/skills/foreign'))
        other = self.base / 'home2'
        other.mkdir()
        root = other / clone.ROOT
        root.parent.mkdir(parents=True)
        root.symlink_to(self.root)
        for mode in ('check', 'apply'):
            with self.assertRaises(OSError):
                clone.run(mode, [url], other)

    def test_timeout_cleans_partial_clone_and_kills_git_group(self):
        fake = self.base / 'bin'
        fake.mkdir()
        marker = self.base / 'child.pid'
        (fake / 'git').write_text(f'#!/bin/bash\nsleep 300 &\necho $! > {marker}\nwait\n')
        (fake / 'git').chmod(0o755)
        url = str(self.base / 'missing.git')
        with mock.patch.object(clone, 'SAFE_PATH', f'{fake}:/usr/bin:/bin'), \
                mock.patch.object(clone, 'ATTEMPT_TIMEOUT', .2):
            self.assertEqual(self.run_hook('apply', url)[0], 0)
        self.assertFalse(self.target(url).parents[1].exists())
        self.assertEqual(list(self.root.glob('.tmp-*')), [])
        pid = int(marker.read_text())
        for _ in range(50):
            try:
                os.kill(pid, 0)
            except ProcessLookupError:
                break
            time.sleep(.1)
        else:
            os.kill(pid, signal.SIGKILL)
            self.fail('git child survived timeout')

    def test_github_wrapper_retries_after_plain_git(self):
        url = 'https://github.com/example/skills'
        local = self.repository('github')
        fake = self.base / 'bin'
        fake.mkdir()
        (fake / 'git').write_text('#!/bin/bash\nexit 128\n')
        (fake / 'git').chmod(0o755)
        wrapper = self.base / 'subyard-github'
        wrapper.write_text(f'#!/bin/bash\nexec /usr/bin/git clone -q -- {local} "$7"\n')
        wrapper.chmod(0o755)
        with mock.patch.object(clone, 'SAFE_PATH', f'{fake}:/usr/bin:/bin'), \
                mock.patch.object(clone, 'GITHUB_WRAPPER', wrapper):
            self.assertEqual(self.run_hook('apply', url)[0], 0)
        self.assertEqual(os.readlink(self.home / '.claude/skills/alpha'), str(self.target(url)))


if __name__ == '__main__':
    unittest.main()
