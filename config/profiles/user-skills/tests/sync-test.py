#!/usr/bin/env python3
"""Host-free contracts of the user-skills sync helper against local bare repositories."""
import contextlib
import fcntl
import importlib.util
import io
import json
import os
import signal
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

sys.dont_write_bytecode = True
PROFILE = Path(__file__).resolve().parents[1]
ROOT = PROFILE.parents[2]
SKILL = PROFILE / 'skills/subyard-user-skills'


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


sync = load('user_skills_sync', SKILL / 'sync.py')
deliver = load('deliver', ROOT / 'config/skills/deliver.py')
HARNESSES = sync.ADAPTERS + sync.EXTRA
GIT_ENV = {'HOME': '/nonexistent', 'PATH': os.environ['PATH'], 'GIT_CONFIG_NOSYSTEM': '1',
           'GIT_AUTHOR_NAME': 't', 'GIT_AUTHOR_EMAIL': 't@example.org',
           'GIT_COMMITTER_NAME': 't', 'GIT_COMMITTER_EMAIL': 't@example.org'}


def git(*arguments, cwd=None):
    subprocess.run(['git', *arguments], cwd=cwd, env=GIT_ENV, check=True, capture_output=True)


class Fixture(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='subyard-user-skills-', dir='/tmp')
        self.addCleanup(self.temporary.cleanup)
        self.base = Path(self.temporary.name)
        self.home = self.base / 'home'
        self.home.mkdir()
        self.root = self.home / sync.ROOT
        patches = {'ALLOW_LOCAL': True, 'CANONICAL': self.base / 'canonical', 'LOCK_WAIT': 0.5,
                   'GITHUB_WRAPPER': self.base / 'missing-wrapper', 'SAFE_PATH': os.environ['PATH']}
        for name, value in patches.items():
            patcher = mock.patch.object(sync, name, value)
            patcher.start()
            self.addCleanup(patcher.stop)
        environment = mock.patch.dict(os.environ, {'HOME': str(self.home)})
        environment.start()
        self.addCleanup(environment.stop)

    def repository(self, name, skills):
        work = self.base / 'work' / name
        work.mkdir(parents=True)
        for skill, files in skills.items():
            for relative, content in files.items():
                path = work / 'skills' / skill / relative
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(content)
        git('init', '-q', '-b', 'main', str(work))
        git('add', '-A', cwd=work)
        git('commit', '-q', '-m', 'skills', cwd=work)
        bare = self.base / 'remotes' / (name + '.git')
        git('clone', '-q', '--bare', str(work), str(bare))
        return str(bare)

    def run_helper(self, *arguments):
        output, errors = io.StringIO(), io.StringIO()
        with contextlib.redirect_stderr(errors):
            code = sync.main(list(arguments), out=output)
        return code, output.getvalue()

    def target(self, url, skill):
        return str(self.root / sync.source_name(sync.normalize(url)) / 'skills' / skill)

    def links(self, skill):
        return {harness: os.readlink(self.home / harness / skill) if os.path.islink(self.home / harness / skill) else None
                for harness in HARNESSES}


class Naming(unittest.TestCase):
    def test_normalization_matches_the_setting_type(self):
        # domain.NormalizeRemoteURL reads the same vectors.
        vectors = json.loads((ROOT / 'internal/domain/testdata/remote-urls.json').read_text())
        self.assertTrue(vectors['normalized'] and vectors['refused'])
        for url, normalized in vectors['normalized'].items():
            self.assertEqual(sync.normalize(url), normalized, url)
        for url, reason in vectors['refused'].items():
            with self.assertRaises(ValueError) as refused:
                sync.normalize(url)
            self.assertEqual(str(refused.exception), reason, url)
            self.assertNotIn('secret', str(refused.exception))

    def test_names_are_sanitized_and_distinct_by_url(self):
        first = sync.source_name('https://github.com/a/Dev_Flow')
        second = sync.source_name('https://example.org/b/dev-flow')
        self.assertRegex(first, r'^dev-flow-[0-9a-f]{8}$')
        self.assertNotEqual(first, second)
        self.assertRegex(sync.source_name('git@host:___'), r'^source-[0-9a-f]{8}$')
        self.assertEqual(len(sync.source_name('https://h/' + 'x' * 90)), 49)
        with self.assertRaises(sync.Unsafe) as refused:
            sync.sources_for(['https://a/b', 'https://A/b.git', 'http://c/d'], Path('/r'))
        self.assertIn('source 2 repeats source 1', str(refused.exception))
        self.assertIn('source 3 uses an unsupported scheme', str(refused.exception))

    def test_harnesses_and_skill_names_follow_core_delivery(self):
        self.assertEqual((sync.ADAPTERS, sync.EXTRA), (deliver.ADAPTERS, deliver.EXTRA))
        self.assertIn(sync.SKILL_NAME.pattern, (ROOT / 'config/skills/deliver.py').read_text())


class Sync(Fixture):
    def test_apply_clones_links_skips_and_is_idempotent(self):
        valid = '---\nname: x\ndescription: y\n---\n'
        first = self.repository('first', {
            'alpha': {'SKILL.md': valid}, 'beta': {'SKILL.md': valid, 'ref.md': 'r'},
            'subyard-own': {'SKILL.md': valid}, 'Bad_Name': {'SKILL.md': valid}, 'empty': {'notes.md': 'n'},
            'reserved': {'SKILL.md': valid}})
        second = self.repository('second', {'alpha': {'SKILL.md': valid}, 'delta': {'SKILL.md': valid}})
        (self.base / 'canonical' / 'reserved').mkdir(parents=True)
        code, output = self.run_helper('apply', first, second)
        self.assertEqual(code, 0, output)
        self.assertEqual(stat.S_IMODE(os.lstat(self.root).st_mode), 0o755)
        sources = json.loads((self.root / 'sources.json').read_text())
        self.assertEqual([source['url'] for source in sources], [first, second])
        self.assertEqual(stat.S_IMODE(os.lstat(self.root / 'sources.json').st_mode), 0o644)
        for skill, url in (('alpha', first), ('beta', first), ('delta', second)):
            self.assertEqual(set(self.links(skill).values()), {self.target(url, skill)}, skill)
        for skill in ('subyard-own', 'Bad_Name', 'empty', 'reserved'):
            self.assertEqual(set(self.links(skill).values()), {None}, skill)
        for reason in ('reserved skill name', 'not a valid skill directory', 'already provided by an earlier source'):
            self.assertIn(reason, output)
        self.assertEqual(self.run_helper('check', first, second), (0, 'current\n'))
        code, output = self.run_helper('apply', first, second)
        self.assertEqual(code, 0)
        self.assertNotIn('linked', output)
        self.assertEqual(list(self.root.glob('.tmp-*')), [])

    def test_occupied_paths_and_symlinked_harnesses_are_never_replaced(self):
        url = self.repository('one', {'alpha': {'SKILL.md': 's'}, 'beta': {'SKILL.md': 's'}})
        (self.home / '.claude/skills').mkdir(parents=True)
        (self.home / '.claude/skills/alpha').write_text('mine')
        (self.home / '.agents/skills').mkdir(parents=True)
        os.symlink('/elsewhere', self.home / '.agents/skills/beta')
        (self.base / 'hermes').mkdir()
        os.symlink(self.base / 'hermes', self.home / '.hermes')
        code, output = self.run_helper('apply', url)
        self.assertEqual(code, 0, output)
        self.assertEqual((self.home / '.claude/skills/alpha').read_text(), 'mine')
        self.assertEqual(os.readlink(self.home / '.agents/skills/beta'), '/elsewhere')
        self.assertEqual(list((self.base / 'hermes').iterdir()), [])
        self.assertIn('path is occupied', output)
        self.assertIn('.hermes/skills: not a real directory', output)
        self.assertEqual(self.run_helper('check', url), (0, 'current\n'))
        self.assertEqual(self.run_helper('unlink')[0], 0)
        self.assertEqual((self.home / '.claude/skills/alpha').read_text(), 'mine')
        self.assertEqual(os.readlink(self.home / '.agents/skills/beta'), '/elsewhere')

    def test_ownership_is_an_exact_target_match(self):
        url = self.repository('one', {'alpha': {'SKILL.md': 's'}})
        self.assertEqual(self.run_helper('apply', url)[0], 0)
        directory = self.home / '.claude/skills'
        name = sync.source_name(sync.normalize(url))
        foreign = {'renamed': f'{self.root}/{name}/skills/alpha', 'outside': f'{self.root}/../x/skills/outside',
                   'nested': f'{self.root}/{name}/skills/nested/deeper', 'Upper': f'{self.root}/BAD-00000000/skills/Upper'}
        for link, target in foreign.items():
            os.symlink(target, directory / link)
        self.assertEqual(self.run_helper('unlink')[0], 0)
        self.assertFalse(os.path.lexists(directory / 'alpha'))
        for link, target in foreign.items():
            self.assertEqual(os.readlink(directory / link), target)
        self.assertFalse(os.path.lexists(self.root / 'sources.json'))
        self.assertTrue((self.root / name / '.git').is_dir(), 'unlink must keep the clone')

    def test_changes_drift_unavailable_and_deselection(self):
        first = self.repository('first', {'alpha': {'SKILL.md': 's'}})
        second = self.repository('second', {'alpha': {'SKILL.md': 's'}, 'beta': {'SKILL.md': 's'}})
        self.assertEqual(self.run_helper('apply', first)[0], 0)
        (self.home / '.config/opencode/skills/alpha').unlink()
        self.assertEqual(self.run_helper('check', first), (0, 'work\n'))
        self.assertEqual(self.run_helper('check', second), (0, 'work\n'))
        self.assertEqual(self.run_helper('apply', second)[0], 0)
        self.assertEqual(set(self.links('alpha').values()), {self.target(second, 'alpha')})
        self.assertTrue((self.root / sync.source_name(sync.normalize(first))).is_dir(), 'old clone stays')
        missing = str(self.base / 'remotes/missing.git')
        code, output = self.run_helper('apply', second, missing)
        self.assertEqual(code, 3, output)
        self.assertIn('unavailable', output)
        self.assertEqual(self.run_helper('check', second, missing), (0, 'unavailable\n'))
        self.assertEqual(set(self.links('beta').values()), {self.target(second, 'beta')})
        # Deselected: sources.json and owned links are work; unlink leaves clones.
        self.assertEqual(self.run_helper('check'), (0, 'work\n'))
        self.assertEqual(self.run_helper('unlink')[0], 0)
        self.assertEqual(self.run_helper('check'), (0, 'current\n'))
        self.assertEqual(set(self.links('beta').values()), {None})
        self.assertTrue((self.root / sync.source_name(sync.normalize(second)) / '.git').is_dir())

    def test_selected_empty_list_is_exactly_unlink(self):
        self.assertEqual(self.run_helper('apply')[0], 0)
        self.assertFalse(os.path.lexists(self.root), 'an empty list creates nothing')
        self.assertEqual(self.run_helper('check'), (0, 'current\n'))
        url = self.repository('one', {'alpha': {'SKILL.md': 's'}})
        self.assertEqual(self.run_helper('apply', url)[0], 0)
        self.assertEqual(self.run_helper('check'), (0, 'work\n'))
        with mock.patch.object(sync, 'unlink_owned', wraps=sync.unlink_owned) as unlink, \
                mock.patch.object(sync, 'write_sources', side_effect=AssertionError('no sources.json')):
            code, output = self.run_helper('apply')
        self.assertEqual((code, unlink.call_count), (0, 1))
        self.assertIn('removed link', output)
        self.assertFalse(os.path.lexists(self.root / 'sources.json'))
        self.assertEqual(set(self.links('alpha').values()), {None})
        self.assertEqual(self.run_helper('check'), (0, 'current\n'))
        self.assertTrue((self.root / sync.source_name(sync.normalize(url)) / '.git').is_dir(), 'clones stay')

    def test_sources_json_urls_must_be_strings(self):
        self.assertEqual(self.run_helper('apply', self.repository('one', {'alpha': {'SKILL.md': 's'}}))[0], 0)
        for content in ('[{"url": 7}]', '[{"url": ["https://h/r"]}]', '{"url": "https://h/r"}', '[7]'):
            (self.root / 'sources.json').write_text(content)
            self.assertEqual(self.run_helper('sync')[0], 2, content)
            self.assertEqual(self.run_helper('list')[0], 2, content)

    def test_sync_removes_interrupted_sources_writes(self):
        url = self.repository('one', {'alpha': {'SKILL.md': 's'}})
        self.assertEqual(self.run_helper('apply', url)[0], 0)
        leftover = self.root / '.sources-interrupted'
        leftover.write_text('[]')
        self.assertEqual(self.run_helper('sync')[0], 0)
        self.assertEqual(list(self.root.glob('.sources-*')), [])

    def test_terminated_helper_kills_its_git_process_group(self):
        fake = self.base / 'fake-bin'
        fake.mkdir()
        marker = self.base / 'grandchild.pid'
        direct = self.base / 'git.pid'
        (fake / 'git').write_text(f'#!/bin/bash\necho $$ > {direct}\nsleep 300 &\necho $! > {marker}\nwait\n')
        (fake / 'git').chmod(0o755)
        driver = (
            'import importlib.util, sys\n'
            'spec = importlib.util.spec_from_file_location("sync", sys.argv[1])\n'
            'sync = importlib.util.module_from_spec(spec)\n'
            'spec.loader.exec_module(sync)\n'
            'sync.ALLOW_LOCAL, sync.SAFE_PATH = True, sys.argv[2]\n'
            'sync.entry(["apply", sys.argv[3]])\n')
        def gone(pid):
            for _ in range(50):
                try:
                    os.kill(pid, 0)
                except ProcessLookupError:
                    return True
                time.sleep(0.1)
            return False

        for signum in (signal.SIGTERM, signal.SIGHUP, signal.SIGKILL):
            marker.unlink(missing_ok=True)
            helper = subprocess.Popen(
                [sys.executable, '-B', '-c', driver, str(SKILL / 'sync.py'), f'{fake}:/usr/bin:/bin',
                 str(self.base / 'remotes/any.git')],
                env={'HOME': str(self.home), 'PATH': '/usr/bin:/bin'}, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            for _ in range(100):
                if marker.exists() and marker.read_text().strip():
                    break
                time.sleep(0.05)
            else:
                helper.kill()
                self.fail('fake git never started')
            helper.send_signal(signum)
            grandchild = int(marker.read_text())
            if signum == signal.SIGKILL:
                # PR_SET_PDEATHSIG: git itself dies with an unhandled kill of the helper.
                self.assertEqual(helper.wait(timeout=10), -signal.SIGKILL)
                self.addCleanup(lambda pid=grandchild: os.path.exists(f'/proc/{pid}') and os.kill(pid, signal.SIGKILL))
                self.assertTrue(gone(int(direct.read_text())), 'git survived a killed helper')
                continue
            self.assertEqual(helper.wait(timeout=10), 128 + signum)
            self.assertTrue(gone(grandchild), f'git grandchild survived signal {signum}')

    def test_agent_sync_reads_sources_and_repairs_links(self):
        url = self.repository('one', {'alpha': {'SKILL.md': 's'}})
        self.assertEqual(self.run_helper('apply', url)[0], 0)
        clone = self.root / sync.source_name(sync.normalize(url))
        subprocess.run(['rm', '-rf', '--', str(clone)], check=True)
        (self.home / '.claude/skills/alpha').unlink()
        code, output = self.run_helper('sync')
        self.assertEqual(code, 0, output)
        self.assertIn('cloned', output)
        self.assertEqual(set(self.links('alpha').values()), {self.target(url, 'alpha')})
        code, output = self.run_helper('list')
        self.assertEqual(code, 0)
        self.assertIn(url, output)
        self.assertIn(f'dir: {clone} (present)', output)
        self.assertIn('skill: alpha', output)
        (self.root / 'sources.json').write_text('{"not": "a list"}')
        self.assertEqual(self.run_helper('sync')[0], 2)
        self.assertEqual(self.run_helper('list')[0], 2)

    def test_lock_busy_changes_nothing_while_check_and_list_answer(self):
        url = self.repository('one', {'alpha': {'SKILL.md': 's'}})
        self.assertEqual(self.run_helper('apply', url)[0], 0)
        before = (self.root / 'sources.json').read_bytes()
        descriptor = os.open(self.root / '.lock', os.O_RDWR)
        self.addCleanup(os.close, descriptor)
        fcntl.flock(descriptor, fcntl.LOCK_EX)
        other = self.repository('two', {'beta': {'SKILL.md': 's'}})
        started = time.monotonic()
        self.assertEqual(self.run_helper('apply', other)[0], 4)
        self.assertEqual(self.run_helper('unlink')[0], 4)
        self.assertGreaterEqual(time.monotonic() - started, 1.0)
        self.assertEqual((self.root / 'sources.json').read_bytes(), before)
        self.assertEqual(set(self.links('alpha').values()), {self.target(url, 'alpha')})
        started = time.monotonic()
        self.assertEqual(self.run_helper('check', url), (0, 'current\n'))
        self.assertEqual(self.run_helper('list')[0], 0)
        self.assertLess(time.monotonic() - started, 0.4)

    def test_unsafe_home_and_root(self):
        with mock.patch.dict(os.environ, {'HOME': str(self.base / 'absent')}):
            self.assertEqual(self.run_helper('sync')[0], 2)
        self.root.parent.mkdir(parents=True)
        os.symlink(self.base, self.root)
        for command in (('apply',), ('sync',), ('check',), ('list',), ('unlink',)):
            self.assertEqual(self.run_helper(*command)[0], 2, command)
        self.assertEqual(self.run_helper('apply', 'https://user:pw@example.org/r')[0], 2)

    def test_timeout_kills_the_whole_git_process_group(self):
        fake = self.base / 'fake-bin'
        fake.mkdir()
        marker = self.base / 'grandchild.pid'
        (fake / 'git').write_text(f'#!/bin/bash\nsleep 300 &\necho $! > {marker}\nwait\n')
        (fake / 'git').chmod(0o755)
        with mock.patch.object(sync, 'SAFE_PATH', f'{fake}:/usr/bin:/bin'), mock.patch.object(sync, 'ATTEMPT_TIMEOUT', 1):
            started = time.monotonic()
            code, output = self.run_helper('apply', str(self.base / 'remotes/any.git'))
        self.assertEqual(code, 3, output)
        self.assertLess(time.monotonic() - started, 10)
        pid = int(marker.read_text())
        for _ in range(50):
            try:
                os.kill(pid, 0)
            except ProcessLookupError:
                break
            time.sleep(0.1)
        else:
            self.fail('git grandchild survived the timeout')
        self.assertEqual(list(self.root.glob('.tmp-*')), [])

    def test_sync_budget_bounds_all_attempts(self):
        with mock.patch.object(sync, 'SYNC_BUDGET', 0):
            code, output = self.run_helper('apply', self.repository('one', {'alpha': {'SKILL.md': 's'}}))
        self.assertEqual(code, 3, output)

    def test_github_wrapper_retries_after_plain_git(self):
        url = 'https://github.com/example/skills'
        local = self.repository('github', {'alpha': {'SKILL.md': 's'}})
        fake = self.base / 'fake-bin'
        fake.mkdir()
        (fake / 'git').write_text('#!/bin/bash\nexit 128\n')
        (fake / 'git').chmod(0o755)
        wrapper = self.base / 'subyard-github'
        wrapper.write_text(f'#!/bin/bash\n[ "$1 $2 $3 $4 $5 $6" = "run -- git clone -- {url}" ] || exit 9\n'
                           f'exec /usr/bin/git clone -q -- {local} "$7"\n')
        wrapper.chmod(0o755)
        with mock.patch.object(sync, 'SAFE_PATH', f'{fake}:/usr/bin:/bin'), mock.patch.object(sync, 'GITHUB_WRAPPER', wrapper):
            code, output = self.run_helper('apply', url)
        self.assertEqual(code, 0, output)
        self.assertEqual(set(self.links('alpha').values()), {self.target(url, 'alpha')})

    def test_cleanup_errors_are_reported_and_not_fatal(self):
        self.assertEqual(self.run_helper('apply')[0], 0)
        stuck = self.root / '.tmp-old-1234' / 'locked'
        stuck.mkdir(parents=True)
        (stuck / 'file').write_text('x')
        stuck.chmod(0o500)
        self.addCleanup(stuck.chmod, 0o700)
        code, output = self.run_helper('sync')
        self.assertEqual(code, 0)
        self.assertIn('cleanup', output)

    def test_sources_json_is_replaced_atomically(self):
        url = self.repository('one', {'alpha': {'SKILL.md': 's'}})
        self.assertEqual(self.run_helper('apply', url)[0], 0)
        before = (self.root / 'sources.json').read_bytes()
        other = str(self.base / 'remotes/other.git')
        with mock.patch.object(sync.os, 'fsync', side_effect=OSError(5, 'injected')):
            self.assertEqual(self.run_helper('apply', other)[0], 2)
        self.assertEqual((self.root / 'sources.json').read_bytes(), before)
        self.assertEqual(list(self.root.glob('.sources-*')), [])


class ProfileSkillDelivery(unittest.TestCase):
    """The profile skill and helper are delivered and retired by core delivery."""

    def test_delivery_static_refresh_and_retirement(self):
        with tempfile.TemporaryDirectory(prefix='subyard-user-skills-delivery-', dir='/tmp') as temporary:
            root = Path(temporary)
            home = root / 'home'
            home.mkdir()
            canonical = root / 'usr/local/share/subyard/skills/subyard-user-skills'

            def run(mode, registered=False):
                args = type('Args', (), dict(root=root, home=home, uid=os.getuid(), gid=os.getgid(),
                                             provider='profile-user-skills', mode=mode, source=PROFILE / 'skills',
                                             registered=registered, extra=True))()
                return deliver.delivery(args)

            self.assertEqual(run('check-registered'), 10)
            self.assertEqual(run('apply'), 0)
            self.assertEqual(run('check-registered'), 0)
            self.assertEqual(sorted(path.name for path in canonical.iterdir()), ['SKILL.md', 'sync.py'])
            self.assertEqual((canonical / 'sync.py').read_bytes(), (SKILL / 'sync.py').read_bytes())
            for harness in HARNESSES:
                self.assertEqual(os.readlink(home / harness / 'subyard-user-skills'), str(canonical))
            self.assertEqual(run('apply', registered=True), 0)
            self.assertEqual(run('check'), 0)
            self.assertEqual(run('remove'), 0)
            self.assertFalse(canonical.exists())
            self.assertEqual(run('check-removed'), 0)


if __name__ == '__main__':
    unittest.main()
