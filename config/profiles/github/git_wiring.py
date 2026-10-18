#!/usr/bin/env python3
"""Own one XDG include and fragment; never replace the user's Git config."""
import os
import pwd
from pathlib import Path
import subprocess
import sys

MARKER = '# subyard-managed-github-git-v1\n'
PREFIXES = ('https://github.com/', 'git@github.com:', 'ssh://git@github.com/')


def fragment():
    text = MARKER + '[subyard]\n\tgithubGitMode = broker-first\n'
    text += '[protocol "subyard-github"]\n\tallow = always\n'
    for prefix in PREFIXES:
        text += f'[url "subyard-github::{prefix}"]\n\tinsteadOf = {prefix}\n'
    return text


def safe(path, home):
    current = path
    while current != home:
        if current.is_symlink():
            raise ValueError('symlinked Git wiring path')
        current = current.parent
    if path.exists() and not path.is_file():
        raise ValueError('non-file Git wiring path')


def reconcile(home, mode, action):
    if mode not in ('off', 'broker-first') or action not in ('apply', 'check', 'remove'):
        raise ValueError('invalid Git wiring action or mode')
    config = home/'.config/git/config'
    owned = home/'.config/subyard/github-git.conf'
    if mode == 'off' or action == 'remove':
        # An unowned/symlinked XDG configuration is unrelated to default-off mode.
        # Only a regular marked fragment establishes ownership to clean up.
        if owned.is_symlink() or not owned.exists() or not owned.is_file() or not owned.read_text().startswith(MARKER):
            if config.is_symlink() or any(parent.is_symlink() for parent in config.parents if parent != home and home in parent.parents):
                return True
            if owned.exists() or owned.is_symlink():
                return True
    for path in (config, owned):
        safe(path, home)
    enabled = mode == 'broker-first' and action != 'remove'
    includes = subprocess.run(['git', 'config', '--file', str(config), '--get-all', 'include.path'],
                              capture_output=True, text=True, check=False)
    if includes.returncode not in (0, 1):
        raise ValueError('cannot read XDG Git configuration')
    count = includes.stdout.splitlines().count(str(owned))
    exists = owned.exists()
    if exists and not owned.read_text().startswith(MARKER):
        raise ValueError('unmanaged Git fragment exists')
    if enabled:
        environment = dict(os.environ, HOME=str(home), XDG_CONFIG_HOME=str(home/'.config'))
        effective = subprocess.run(['git', 'config', '--global', '--includes', '--get-regexp', r'^url\..*\.insteadof$'], env=environment, capture_output=True, text=True, check=False)
        if effective.returncode not in (0, 1):
            raise ValueError('cannot inspect effective Git rewrites')
        for line in effective.stdout.splitlines():
            key, value = line.split(None, 1)
            if any(prefix.startswith(value) or value.startswith(prefix) for prefix in PREFIXES):
                if (key, value) not in {(f'url.subyard-github::{prefix}.insteadof', prefix) for prefix in PREFIXES}:
                    raise ValueError('conflicting GitHub rewrite')
    current = (count == 1 and exists and owned.read_text() == fragment() and
               owned.stat().st_mode & 0o777 == 0o644 and owned.stat().st_uid == os.geteuid() and owned.stat().st_nlink == 1) if enabled else (count == 0 and not exists)
    if action == 'check':
        return current
    if current:
        return True
    if count:
        subprocess.run(['git', 'config', '--file', str(config), '--fixed-value', '--unset-all',
                        'include.path', str(owned)], check=True)
    if enabled:
        config.parent.mkdir(parents=True, exist_ok=True)
        owned.parent.mkdir(parents=True, exist_ok=True)
        temp = owned.with_suffix('.tmp')
        safe(temp, home)
        with temp.open('w') as output:
            output.write(fragment())
        temp.chmod(0o644)
        temp.replace(owned)
        subprocess.run(['git', 'config', '--file', str(config), '--add', 'include.path', str(owned)], check=True)
    elif exists:
        owned.unlink()
    return True


if __name__ == '__main__':
    try:
        if len(sys.argv) != 4:
            raise ValueError('invalid Git wiring arguments')
        if os.geteuid() == 0:
            user = pwd.getpwnam(os.environ.get('DEV_USER', 'dev'))
            os.setgroups([])
            os.setgid(user.pw_gid)
            os.setuid(user.pw_uid)
            os.chdir(user.pw_dir)
        sys.exit(0 if reconcile(Path(sys.argv[1]), sys.argv[2], sys.argv[3]) else 10)
    except (ValueError, OSError, subprocess.SubprocessError):
        print('github provision: Git wiring is unsafe or could not converge', file=sys.stderr)
        sys.exit(1)
