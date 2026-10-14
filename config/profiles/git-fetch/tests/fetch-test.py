#!/usr/bin/env python3
"""Profile-owned real Git fixtures and bounded process/discovery contracts."""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import unittest
from unittest.mock import patch

PROFILE = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('fetch', PROFILE/'fetch.py')
fetch = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fetch)


def git(*args):
    return subprocess.check_output(['git', *map(str, args)], stderr=subprocess.DEVNULL).decode().strip()


class FetchTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='subyard-git-fetch-', dir='/tmp')
        self.addCleanup(self.temporary.cleanup)
        self.base = Path(self.temporary.name)
        self.workspaces = self.base/'workspaces'
        self.workspaces.mkdir()

    def project(self, name):
        project = self.workspaces/name
        project.mkdir()
        metadata = project/'.subyard-meta.json'
        metadata.write_text(json.dumps({'schema': 1, 'projectId': name, 'name': name}))
        metadata.chmod(0o600)
        root = project/'src'
        root.mkdir()
        return root

    def source(self):
        source = self.base/'source'
        git('init', '-q', source)
        git('-C', source, 'config', 'user.name', 'Fixture')
        git('-C', source, 'config', 'user.email', 'fixture@example.invalid')
        git('-C', source, 'commit', '-qm', 'first', '--allow-empty')
        return source

    def test_actual_fetch_deduplicates_worktrees_and_preserves_checkout(self):
        source = self.source()
        main = self.project('main')
        git('clone', '-q', source, main)
        original = git('-C', main, 'rev-parse', 'HEAD')
        nested = main/'private'
        git('clone', '-q', source, nested)
        (main/'.gitignore').write_text('private/\n')
        git('-C', main, 'check-ignore', 'private')
        linked = self.project('linked')
        linked.rmdir()
        git('-C', main, 'worktree', 'add', '-q', '-b', 'linked', linked)
        git('-C', source, 'commit', '-qm', 'second', '--allow-empty')
        with contextlib.redirect_stdout(io.StringIO()) as output:
            fetch.cycle(self.workspaces)
        self.assertEqual(output.getvalue(), 'git-fetch: remotes fetched=2 failed=0\n')
        self.assertEqual(git('-C', main, 'rev-parse', 'refs/remotes/origin/HEAD'), git('-C', source, 'rev-parse', 'HEAD'))
        self.assertEqual(git('-C', main, 'rev-parse', 'HEAD'), original)
        self.assertEqual(git('-C', linked, 'rev-parse', 'HEAD'), original)
        self.assertEqual(git('-C', nested, 'rev-parse', 'origin/HEAD'), git('-C', source, 'rev-parse', 'HEAD'))
        self.assertEqual(git('-C', nested, 'rev-parse', 'HEAD'), original)

    def test_native_github_wrapper_fetches_local_remote_without_broker(self):
        client = os.environ.get('GIT_FETCH_TEST_GITHUB_CLIENT')
        self.assertTrue(client, 'runner must supply the current native GitHub client')
        wrapper = self.base/'subyard-github'
        wrapper.write_text('#!/bin/sh\nexec "'+client+'" _github-client "$@"\n')
        wrapper.chmod(0o755)
        source = self.source()
        main = self.project('wrapped')
        git('clone', '-q', source, main)
        git('-C', source, 'commit', '-qm', 'second', '--allow-empty')
        with patch.object(fetch, 'GITHUB_CLIENT', wrapper):
            self.assertEqual(fetch.fetch_repositories([main]), (1, 0))
        self.assertEqual(git('-C', main, 'rev-parse', 'origin/HEAD'), git('-C', source, 'rev-parse', 'HEAD'))

    def test_folder_scope_pruning_and_metadata_permissions(self):
        main = self.project('main')
        git('init', '-q', main)
        git('init', '-q', main/'nested')
        folder = self.project('folder')
        git('init', '-q', folder/'repo')
        git('init', '-q', folder/'.build'/'ignored')
        unknown = self.workspaces/'unregistered'/'src'
        git('init', '-q', unknown)
        (folder/'symlink').symlink_to(unknown, target_is_directory=True)
        self.assertEqual(set(fetch.repository_paths(fetch.managed_roots(self.workspaces))),
                         {main, main/'nested', folder/'repo'})
        (main.parent/'.subyard-meta.json').chmod(0o666)
        self.assertEqual(fetch.managed_roots(self.workspaces), [folder])

    def test_late_healthy_remote_is_reached_without_aggregate_cutoff(self):
        paths = [self.project(f'repo-{number}') for number in range(3)]
        calls = []
        def command(arguments, cwd, timeout, capture=False):
            if 'rev-parse' in arguments:
                return str(cwd)+'\n'
            if arguments[-1] == 'remote':
                return '\n'.join([f'slow-{number}' for number in range(31)] + ['healthy'])+'\n'
            calls.append((cwd, arguments[-1]))
            return '' if arguments[-1] == 'healthy' else None
        with patch.object(fetch, 'command', command):
            self.assertEqual(fetch.fetch_repositories(paths), (3, 93))
        self.assertEqual([cwd for cwd, remote in calls if remote == 'healthy'], paths)

    def test_failure_isolation_auth_and_serial_flags(self):
        one, two = self.project('one'), self.project('two')
        calls = []
        def command(arguments, cwd, timeout, capture=False):
            calls.append((arguments, cwd, timeout))
            if 'rev-parse' in arguments:
                return str(cwd)+'\n'
            if arguments[-1] == 'remote':
                return 'origin\nsecondary\n'
            return None if cwd == one and arguments[-1] == 'origin' else ''
        with patch.object(fetch, 'command', command), patch.object(fetch.os, 'access', return_value=True), patch.object(fetch.Path, 'is_file', return_value=True):
            self.assertEqual(fetch.fetch_repositories([one, two]), (3, 1))
        fetches = [c for c in calls if 'fetch' in c[0]]
        self.assertEqual([cwd for _, cwd, _ in fetches], [one, one, two, two])
        for arguments, _, timeout in fetches:
            self.assertEqual(arguments[:3], ['/usr/local/bin/subyard-github', 'run', '--'])
            self.assertIn('--jobs=1', arguments)
            self.assertIn('--no-recurse-submodules', arguments)
            self.assertIn('fetch.parallel=1', arguments)
            self.assertEqual(timeout, 60)

    def test_timeout_terminates_descendants_and_suppresses_credentials(self):
        program = self.base/'slow.py'
        child_pid = self.base/'child.pid'
        program.write_text('import pathlib,subprocess,time\nchild=subprocess.Popen(["sleep","30"])\npathlib.Path('+repr(str(child_pid))+').write_text(str(child.pid))\nprint("https://token:secret@example.invalid",flush=True)\ntime.sleep(30)\n')
        started = time.monotonic()
        with contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertIsNone(fetch.command(['python3', str(program)], self.base, 1, True))
        self.assertLess(time.monotonic()-started, 3)
        self.assertEqual(output.getvalue(), '')
        child = Path('/proc')/child_pid.read_text()/'stat'
        if child.exists():
            self.assertIn(child.read_text().rsplit(')', 1)[1].split()[0], ('Z', 'X'))


if __name__ == '__main__':
    unittest.main()
