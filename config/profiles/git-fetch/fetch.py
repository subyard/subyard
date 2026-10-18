#!/usr/bin/env python3
"""Fetch registered yard Git roots serially without publishing remote diagnostics."""
import json
import os
from pathlib import Path
import signal
import stat
import subprocess
import sys
import tempfile

WORKSPACES = Path('/srv/workspaces')
SKIP = {'.git', '.build', '.cache', 'node_modules', 'vendor', '.venv', 'venv', '__pycache__'}
QUERY_SECONDS = 5
FETCH_SECONDS = 60
GITHUB_CLIENT = Path('/usr/local/bin/subyard-github')


def command(arguments, cwd, timeout, capture=False):
    environment = dict(os.environ, GIT_TERMINAL_PROMPT='0', GCM_INTERACTIVE='never',
                       GIT_SSH_COMMAND='ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -o ConnectTimeout=5 -o ServerAliveInterval=5 -o ServerAliveCountMax=2')
    # Queries use a disk-backed bounded read. Fetch output is discarded entirely:
    # even a local remote URL or Git error can contain credentials.
    with tempfile.TemporaryFile() as output:
        process = subprocess.Popen(arguments, cwd=cwd, env=environment,
                                   stdin=subprocess.DEVNULL,
                                   stdout=output if capture else subprocess.DEVNULL,
                                   stderr=subprocess.DEVNULL, start_new_session=True)
        try:
            status = process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            try:
                os.killpg(process.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                pass
            # Reap descendants even if their immediate Git parent exited on TERM.
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait()
            return None
        if status != 0:
            return None
        output.seek(0)
        value = output.read(65537)
        return value.decode('utf-8') if len(value) <= 65536 else None


def git(path, *args):
    return command(['git', '-c', 'core.hooksPath=/dev/null', *args], path, QUERY_SECONDS, True)


def managed_roots(workspaces):
    if workspaces.is_symlink() or not workspaces.is_dir():
        return []
    roots = []
    for project in sorted(workspaces.iterdir()):
        try:
            if project.is_symlink() or not project.is_dir():
                continue
            descriptor = project/'.subyard-meta.json'
            fd = os.open(descriptor, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
            with os.fdopen(fd, 'rb') as source:
                info = os.fstat(source.fileno())
                if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o022:
                    continue
                raw = source.read(65537)
            metadata = json.loads(raw) if len(raw) <= 65536 else None
            if not isinstance(metadata, dict) or type(metadata.get('schema')) is not int or metadata['schema'] != 1 or metadata.get('projectId') != project.name or not isinstance(metadata.get('name'), str) or not metadata['name'].strip():
                continue
            root = project/'src'
            if not root.is_symlink() and root.is_dir() and root.resolve() == root:
                roots.append(root)
        except (OSError, ValueError, UnicodeError):
            continue
    return roots


def repository_paths(roots):
    # Walk ignored nested repositories too; Git ignore rules do not define scope.
    for root in roots:
        for directory, children, _ in os.walk(root, followlinks=False):
            path = Path(directory)
            if path.resolve() != path:
                children[:] = []; continue
            children[:] = [name for name in children if name not in SKIP
                           and not (path/name).is_symlink()]
            marker = path/'.git'
            if not marker.is_symlink() and marker.exists():
                yield path


def fetch_repositories(paths):
    seen = set()
    fetched = failed = 0
    for path in paths:
        try:
            common = git(path, 'rev-parse', '--path-format=absolute', '--git-common-dir')
            if common is None:
                failed += 1
                continue
            info = Path(common.strip()).stat()
            identity = (info.st_dev, info.st_ino)
            if identity in seen:
                continue
            seen.add(identity)
            remotes = git(path, 'remote')
            if remotes is None:
                failed += 1
                continue
            for remote in remotes.splitlines():
                arguments = ['git', '-c', 'core.hooksPath=/dev/null', '-c', 'fetch.parallel=1',
                             '-c', 'submodule.fetchJobs=1', 'fetch', '--quiet', '--jobs=1',
                             '--no-recurse-submodules', '--', remote]
                # The existing client selects auth by each actual HTTPS URL, including
                # remotes from different GitHub installations. SSH remains unchanged.
                wrapper = GITHUB_CLIENT
                if git(path, 'config', '--get', 'subyard.githubGitMode') != 'broker-first\n' and wrapper.is_file() and os.access(wrapper, os.X_OK):
                    arguments = [str(wrapper), 'run', '--', *arguments]
                if command(arguments, path, FETCH_SECONDS) is None:
                    failed += 1
                else:
                    fetched += 1
        except (OSError, ValueError, UnicodeError, subprocess.SubprocessError):
            failed += 1
    return fetched, failed


def cycle(workspaces=WORKSPACES):
    # systemd serializes this oneshot. No aggregate cutoff: every discovered repo
    # gets its turn, even when earlier remotes consume their individual timeout.
    fetched, failed = fetch_repositories(repository_paths(managed_roots(workspaces)))
    print(f'git-fetch: remotes fetched={fetched} failed={failed}', flush=True)


if __name__ == '__main__':
    try:
        cycle()
    except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError):
        print('git-fetch: cycle could not inspect managed repositories', file=sys.stderr)
        sys.exit(1)
