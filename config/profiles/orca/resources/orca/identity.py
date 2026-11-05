"""Bounded Orca unit rebind under the core's protected identity authority."""
import fcntl
import grp
import hashlib
import json
import os
from pathlib import Path
import pwd
import stat
import subprocess
import sys
import tempfile
import authority

UNITS = ('subyard-orca.service', 'subyard-orca-discovery.service', 'subyard-orca-discovery.timer')
DIRECTORY = Path('/etc/systemd/system')
MARKER = Path('/usr/local/libexec/subyard/orca-contract.sha256')
LOCK = Path('/usr/local/libexec/subyard/orca-registration.lock')
CGROUP = Path('/sys/fs/cgroup')
# Container images generate type-wide service policy; a unit-specific drop-in stays foreign.
TYPE_DROPINS = tuple(Path(root)/'service.d' for root in ('/run/systemd/system', '/etc/systemd/system', '/usr/lib/systemd/system'))
# These end with the user manager without ending anybody's work.
STALE_RUNTIME = 'Orca runtime differs from this release; restore the previous DEV_USER, run yard orca up, then rename again'
RUNNING_WORK = 'agent terminals are running; close them or run yard orca down, then retry yard init'


def run(*args):
    result = subprocess.run(args, capture_output=True, timeout=125)
    if result.returncode: raise ValueError('Orca identity manager operation failed')
    return result.stdout.decode()


def checked(path, mode=None):
    for parent in path.parents:
        info = parent.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid not in (0, os.getuid()) or info.st_mode & 0o022:
            if parent == Path('/tmp') and info.st_uid == 0 and info.st_mode & stat.S_ISVTX: continue
            raise ValueError('unsafe Orca identity parent')
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_gid != os.getgid() or info.st_nlink != 1 or info.st_mode & 0o022 or info.st_size > 2**20 or (mode is not None and stat.S_IMODE(info.st_mode) != mode):
        raise ValueError('unsafe Orca identity artifact')
    return path.read_bytes()


def sha(data): return hashlib.sha256(data).hexdigest()


def full_runtime():
    paths = (DIRECTORY/UNITS[0], MARKER)
    present = [path.exists() or path.is_symlink() for path in paths]
    if any(present) and not all(present): raise ValueError('partial Orca application ownership')
    if all(present):
        for path in paths: checked(path, 0o644)
    return all(present)


def manager(name):
    values = dict(line.split('=', 1) for line in run('systemctl', 'show', name, '--property=LoadState,ActiveState,SubState,UnitFileState,FragmentPath,DropInPaths,User,Group,Environment').splitlines() if '=' in line)
    # A failed service waiting for Restart= has failed; its next start cannot succeed either.
    if values.get('ActiveState') == 'activating' and values.get('SubState') == 'auto-restart': values['ActiveState'] = 'failed'
    # Discovery is a timer-driven oneshot: a run in progress says nothing about intent.
    if name == UNITS[1] and values.get('ActiveState') in ('activating', 'deactivating'): values['ActiveState'] = 'inactive'
    if values.get('LoadState') != 'loaded' or values.get('FragmentPath') != str(DIRECTORY/name) or any(Path(path).parent not in TYPE_DROPINS for path in values.get('DropInPaths', '').split()) or values.get('ActiveState') not in ('active', 'inactive', 'failed') or values.get('UnitFileState') not in ('enabled', 'disabled', 'static'):
        raise ValueError('unsafe Orca identity manager envelope')
    return values


def cgroup_files(unit):
    group = run('systemctl', 'show', unit, '--property=ControlGroup', '--value').strip()
    return (CGROUP/group.lstrip('/')).rglob('cgroup.procs') if group else ()


def members(path):
    # A unit or process can exit during this scan.
    try: return path.read_text().split()
    except (FileNotFoundError, ProcessLookupError): return []


def hosts_agent(pid):
    try:
        command = (Path('/proc')/pid/'cmdline').read_bytes()
        stat_fields = (Path('/proc')/pid/'stat').read_text().rpartition(') ')[2].split()
    except (FileNotFoundError, ProcessLookupError):
        return False
    return b'daemon-entry' in command or stat_fields[4] != '0'


def refuse_running_work(full):
    """Quiesce stops the service, which would end agent terminals hosted inside it.

    Work in the user manager is core's: it moves lingering only from an idle manager."""
    if full:
        for path in cgroup_files(UNITS[0]):
            if any(hosts_agent(pid) for pid in members(path)): raise ValueError(RUNNING_WORK)


def stop(name):
    run('systemctl', 'stop', name)
    # Stopping a failed unit, e.g. discovery run as the old login after a reboot, keeps it failed.
    if manager(name)['ActiveState'] == 'failed': run('systemctl', 'reset-failed', name)


def publish(path, before, after):
    current = checked(path, 0o644)
    if current == after: return
    if current != before: raise ValueError('Orca identity artifact changed')
    fd, temporary = tempfile.mkstemp(prefix='.identity-', dir=path.parent)
    try:
        with os.fdopen(fd, 'wb') as stream:
            os.fchmod(stream.fileno(), 0o644); stream.write(after); stream.flush(); os.fsync(stream.fileno())
        if checked(path, 0o644) != before: raise ValueError('Orca identity artifact changed')
        os.replace(temporary, path)
        fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try: os.fsync(fd)
        finally: os.close(fd)
    finally:
        if os.path.exists(temporary): os.unlink(temporary)


def contract_digest(manifest, data):
    rows = ''.join(sha(value)+'  '+name+'\n' for name, value in zip(manifest['files'], data))
    return (manifest['version']+':'+sha(rows.encode())+'\n').encode()


def execute(mode, source, user, target_user, manifest):
    try:
        journal, recipe = authority.load(source, 'orca'); phase = journal['phase']
        restarted=authority.rebooted(journal)
        original, target = recipe['original'], recipe['target']
    except FileNotFoundError:
        if mode != 'identity-observe': raise
        account = pwd.getpwnam(user)
        original = dict(user=user, uid=account.pw_uid, gid=account.pw_gid, home=account.pw_dir, group=grp.getgrgid(account.pw_gid).gr_name)
        target = dict(original, user=target_user, group=target_user, home='/home/'+target_user)
        phase = None
        restarted=False
    if user != original['user'] or target_user != target['user']: raise ValueError('Orca identity account scope changed')
    observed = manifest['observation']
    installed = observed['state'] != 'absent'
    if phase is None:
        if observed['state'] not in ('current', 'absent'): raise ValueError('Orca identity requires current owner registration')
        recipe = dict(schema=1, source=source, original=original, target=target, installed=installed)
    elif installed != recipe['installed']: raise ValueError('Orca identity installation changed')
    if not installed:
        if any(path.exists() or path.is_symlink() for path in (MARKER, *(DIRECTORY/name for name in UNITS))):
            raise ValueError('unowned Orca identity artifacts')
        return recipe if mode == 'identity-observe' else None
    full = full_runtime()
    if full != manifest['full']: raise ValueError('Orca application scope changed')
    if phase is None: recipe.update(full=full)
    elif recipe['full'] != full: raise ValueError('Orca application installation changed')
    if phase in (None, 'prepared', 'quiesced'): refuse_running_work(full)
    services = UNITS if full else UNITS[1:]
    rebound = UNITS[:2] if full else UNITS[1:2]
    before, after = ([value.encode() for value in manifest[key]] for key in ('before', 'after'))
    if not full: before, after = before[1:], after[1:]
    units = [checked(DIRECTORY/name, 0o644) for name in rebound]
    if any(value not in ((old,) if phase is None else (old, new)) for value, old, new in zip(units, before, after)):
        raise ValueError(STALE_RUNTIME if phase is None else 'Orca identity service source changed')
    if observed['actual'] not in (manifest['oldDigest'], manifest['newDigest']): raise ValueError('Orca identity registration changed')
    data = [checked(Path(name)) for name in manifest['files']]
    markers = []
    if full:
        index = manifest['files'].index(str(DIRECTORY/UNITS[0]))
        old_data, new_data = list(data), list(data); old_data[index] = before[0]; new_data[index] = after[0]
        old_marker, new_marker = contract_digest(manifest, old_data), contract_digest(manifest, new_data)
        marker = checked(MARKER, 0o644)
        if marker not in ((old_marker,) if phase is None else (old_marker, new_marker)): raise ValueError(STALE_RUNTIME if phase is None else 'Orca runtime identity receipt changed')
        markers = [sha(old_marker), sha(new_marker)]
    immutable = {name: sha(value) for name, value in zip(manifest['files'], data) if name != str(DIRECTORY/UNITS[0])}
    states = [manager(name) for name in services]
    if phase is None and any(row['ActiveState']=='failed' for row in states):raise ValueError('Orca identity requires stable owner services')
    for row in states[:len(rebound)]:
        allowed = (original,) if phase is None else (original, target)
        if not any(row.get('User') == account['user'] and row.get('Group') == account['group'] and 'HOME='+account['home'] in row.get('Environment', '').split() for account in allowed):
            raise ValueError('Orca loaded account envelope changed')
    if phase is None:
        recipe.update(immutable=immutable, manager=states, before=[sha(value) for value in before], after=[sha(value) for value in after], marker=markers)
    else:
        if immutable != recipe['immutable'] or [sha(value) for value in before] != recipe['before'] or [sha(value) for value in after] != recipe['after'] or markers != recipe['marker']:
            raise ValueError('Orca identity source changed')
        for row, expected in zip(states, recipe['manager']):
            if row['UnitFileState'] != expected['UnitFileState'] or not authority.active_allowed(row['ActiveState'],expected['ActiveState'],expected['UnitFileState'],restarted): raise ValueError('Orca identity service intent changed')
    if mode == 'identity-observe': return recipe
    if phase in ('prepared', 'quiesced', 'home', 'account'):
        for name in reversed(services): stop(name)
        if any(manager(name)['ActiveState'] != 'inactive' for name in services): raise ValueError('Orca identity services did not quiesce')
        return None
    if phase != 'account-ready': raise ValueError('invalid Orca identity phase')
    account = pwd.getpwnam(target['user'])
    if (account.pw_uid, account.pw_gid, account.pw_dir) != (target['uid'], target['gid'], target['home']): raise ValueError('Orca target identity changed')
    for name, old, new in zip(rebound, before, after): publish(DIRECTORY/name, old, new)
    if full: publish(MARKER, old_marker, new_marker)
    run('systemctl', 'daemon-reload')
    for name, expected in zip(services, recipe['manager']):
        if expected['ActiveState'] == 'active': run('systemctl', 'start', name)
        elif manager(name)['ActiveState']!='inactive': stop(name)
    for name, expected in zip(services, recipe['manager']):
        row = manager(name)
        if row['ActiveState'] != expected['ActiveState'] or row['UnitFileState'] != expected['UnitFileState']: raise ValueError('Orca identity service restoration failed')
    return None


def main():
    mode, source, *arguments = sys.argv[1:]
    if mode == 'main-state':
        print(json.dumps(full_runtime())); return
    if mode == 'target':
        print(authority.load(source, 'orca')[1]['target']['user']); return
    user, target, encoded = arguments
    descriptor = None
    if mode == 'identity-apply' and LOCK.exists():
        checked(LOCK, 0o644)
        descriptor = os.open(LOCK, os.O_RDONLY | os.O_NOFOLLOW); fcntl.flock(descriptor, fcntl.LOCK_EX)
    try: result = execute(mode, source, user, target, json.loads(encoded))
    finally:
        if descriptor is not None: os.close(descriptor)
    if result is not None: print(json.dumps(result))


if __name__ == '__main__':
    try: main()
    except ValueError as error:
        # These owner checks use fixed diagnostic strings, never file contents.
        print('Orca identity: '+str(error), file=sys.stderr)
        raise SystemExit(1)
    except (OSError, KeyError, TypeError, subprocess.SubprocessError) as error:
        print('Orca identity native read failed: '+type(error).__name__, file=sys.stderr)
        print('Orca identity state could not be verified; retry plain init after resolving owned runtime drift', file=sys.stderr)
        raise SystemExit(1)
