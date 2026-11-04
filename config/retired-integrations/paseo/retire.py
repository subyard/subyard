#!/usr/bin/env python3
"""Remove Paseo, retaining only non-empty worktrees."""
import hashlib, json, os, subprocess, sys

PROJECT = os.environ['INCUS_PROJECT']
INSTANCE = os.environ['YARD_INSTANCE_NAME']
GUEST = r'''
import contextlib, fcntl, hashlib, json, os, shutil, stat, subprocess, sys
ROOT = ''
UNIT_PATHS = ('/etc/systemd/system/paseo.service', '/run/systemd/system/paseo.service',
              '/usr/lib/systemd/system/paseo.service', '/lib/systemd/system/paseo.service')
STATE = '/srv/agents/paseo'
HELPERS = ('paseo', 'paseo-check', 'paseo-pair', 'paseo-rotate-identity', 'paseo-sync-projects')
PATHS = (*UNIT_PATHS, *(path + '.d' for path in UNIT_PATHS),
         *('/usr/local/bin/' + name for name in HELPERS), '/opt/subyard/paseo', '/var/lib/subyard/paseo-ownership')

def canonical(path):
    # Debian's /lib alias is the same fixed unit directory, never an arbitrary link.
    if path.startswith('/lib/') and os.path.islink(ROOT + '/lib'):
        if os.readlink(ROOT + '/lib') not in ('usr/lib', '/usr/lib'):
            raise OSError('unsafe unit directory')
        return '/usr/lib/' + path[5:]
    return path

@contextlib.contextmanager
def parent(path):
    parts = canonical(path).strip('/').split('/')
    descriptor = os.open(ROOT or '/', os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        for part in parts[:-1]:
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=descriptor)
            os.close(descriptor)
            descriptor = child
        yield descriptor, parts[-1]
    finally:
        os.close(descriptor)

def present(path):
    try:
        with parent(path) as (descriptor, name):
            return os.stat(name, dir_fd=descriptor, follow_symlinks=False)
    except FileNotFoundError:
        return None

def state_targets():
    info = present(STATE)
    if info is None:
        return [], False
    if not stat.S_ISDIR(info.st_mode):
        return [STATE], False
    with parent(STATE) as (descriptor, name):
        state = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=descriptor)
    try:
        keep = False
        if 'worktrees' in os.listdir(state):
            info = os.stat('worktrees', dir_fd=state, follow_symlinks=False)
            if stat.S_ISDIR(info.st_mode):
                worktrees = os.open('worktrees', os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=state)
                try:
                    keep = bool(os.listdir(worktrees))
                finally:
                    os.close(worktrees)
        return ([STATE + '/' + name for name in sorted(os.listdir(state)) if name != 'worktrees'] if keep else [STATE]), keep
    finally:
        os.close(state)

def loaded_unit():
    result = subprocess.run(['systemctl', 'show', 'paseo.service', '--property=ActiveState,LoadState'],
                            stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=30, check=True)
    return dict(line.split('=', 1) for line in result.stdout.splitlines() if '=' in line)

def inspect():
    paths = {canonical(path) for path in PATHS if present(path) is not None}
    state, keep = state_targets()
    unit = loaded_unit()
    return {'remove': sorted(paths.union(state)), 'worktrees': keep, 'service': unit.get('LoadState') != 'not-found' or unit.get('ActiveState', 'inactive') not in ('inactive', 'failed')}

def fingerprint(facts):
    return hashlib.sha256(json.dumps(facts, sort_keys=True, separators=(',', ':')).encode()).hexdigest()

def systemctl(*arguments):
    subprocess.run(['systemctl', *arguments], stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                   stderr=subprocess.DEVNULL, timeout=90, check=True)

def remove(path):
    try:
        with parent(path) as (descriptor, name):
            info = os.stat(name, dir_fd=descriptor, follow_symlinks=False)
            if stat.S_ISDIR(info.st_mode):
                if not shutil.rmtree.avoids_symlink_attacks:
                    raise OSError('safe recursive removal is unavailable')
                shutil.rmtree(name, dir_fd=descriptor)
            else:
                os.unlink(name, dir_fd=descriptor)
    except FileNotFoundError:
        pass

def apply_locked(expected):
    facts = inspect()
    if fingerprint(facts) != expected:
        sys.exit(3)
    # Never recurse into a guest mount that could expose host data. A retained
    # worktree mount is untouched, but a mount at STATE itself still refuses.
    with open('/proc/self/mountinfo', 'rb') as stream:
        mounts = [line.split()[4].decode() for line in stream]
    roots = facts['remove'] + ([STATE] if present(STATE) else [])
    for mount in mounts:
        if facts['worktrees'] and (mount == ROOT + STATE + '/worktrees' or mount.startswith(ROOT + STATE + '/worktrees/')):
            continue
        if any(mount == ROOT + path or mount.startswith(ROOT + path + '/') for path in roots):
            raise OSError('refusing mounted retirement path')
    unit = loaded_unit()
    if unit.get('ActiveState', 'inactive') not in ('inactive', 'failed'):
        systemctl('stop', 'paseo.service')
    if any(present(path) is not None for path in UNIT_PATHS):
        systemctl('disable', 'paseo.service')
    # The stopped daemon can no longer create a worktree. Decide preservation again.
    state, _ = state_targets()
    for path in sorted({canonical(path) for path in PATHS if present(path) is not None}):
        remove(path)
    for path in state:
        remove(path)
    systemctl('daemon-reload')

def apply(expected):
    with parent(UNIT_PATHS[0]) as (lock, _):
        fcntl.flock(lock, fcntl.LOCK_EX)
        apply_locked(expected)

if sys.argv[1] == 'observe':
    print(json.dumps(inspect(), sort_keys=True, separators=(',', ':')))
else:
    apply(sys.argv[2])
'''


def guest(*arguments, timeout=30):
    return subprocess.run(['incus', 'exec', INSTANCE, '--project', PROJECT, '--', 'python3', '-B', '-c', GUEST, *arguments],
                          stdin=subprocess.DEVNULL, capture_output=True, timeout=timeout)


def observe():
    result = guest('observe')
    if result.returncode:
        raise ValueError('native-unavailable')
    facts = json.loads(result.stdout)
    observation = {'state': 'absent', 'actual': '', 'desired': ''}
    if facts['remove'] or facts['service']:
        observation = {'state': 'stale', 'actual': hashlib.sha256(result.stdout.strip()).hexdigest(),
                       'desired': hashlib.sha256(os.environ.get('SUBYARD_INTEGRATION_SOURCE_BINDING', '').encode()).hexdigest(),
                       'notice': 'retire integration paseo: remove its service, helpers, runtime and state, including chats, schedules, loops and uploads.'}
    if facts['worktrees']:
        observation['notice'] = observation.get('notice', '') + ' Keep /srv/agents/paseo/worktrees: non-empty worktrees remain.'
        observation['notice'] = observation['notice'].strip()
    return observation


def main():
    observed = observe()
    if sys.argv[1:] == ['observe']:
        print(json.dumps(observed))
        return
    if len(sys.argv) != 5 or sys.argv[1] != 'apply' or observed['state'] != 'stale' or [observed['actual'], observed['desired']] != sys.argv[3:]:
        raise ValueError('observation-changed')
    result = guest('apply', sys.argv[3], timeout=900)
    if result.returncode:
        raise ValueError('observation-changed' if result.returncode == 3 else 'operation-failed')
    verified = observe()
    if verified['state'] != 'absent':
        raise ValueError('convergence-failed')
    print(json.dumps(verified))


if __name__ == '__main__':
    phase = 'apply' if sys.argv[1:2] == ['apply'] else 'inspect'
    try:
        main()
    except (ValueError, KeyError, TypeError, OSError, subprocess.SubprocessError) as error:
        reasons = ('native-unavailable', 'observation-changed', 'convergence-failed', 'operation-failed')
        print(f'subyard-lifecycle-diagnostic-v1 {phase} {str(error) if str(error) in reasons else "operation-failed"}', file=sys.stderr)
        sys.exit(1)
