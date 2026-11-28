#!/usr/bin/env python3
"""Read-only yard facts; private writer entrypoints serve the prepared adapter."""
import json
import os
from pathlib import Path
import re
import stat
import sys
import tempfile

# The guest root and the owner trusted for the facts and their system ancestry.
ROOT = Path('/')
TRUSTED_OWNER = (0, 0)
FACTS = ROOT / 'etc/subyard/guest-runtime.json'
# The owner's value-free failure snapshot for this yard (host-errors).
HOST_ERRORS = ROOT / 'etc/subyard/host-errors.json'
MACHINE = ROOT / 'etc/machine-id'
# check-only result: the saved helper refresh is valid, but selection awaits init.
PROFILES_PENDING = 11


def identity(machine=MACHINE):
    value = machine.read_text().strip()
    if not re.fullmatch('[0-9a-f]{32}', value):
        raise ValueError('yard machine identity unavailable')
    return value


def snapshot(value):
    if not isinstance(value, dict) or set(value) != {'version', 'yard', 'selected_profiles'}:
        raise ValueError('invalid runtime snapshot')
    if not isinstance(value['version'], str) or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9.+_-]{0,127}', value['version']):
        raise ValueError('runtime version unavailable')
    if not isinstance(value['yard'], dict) or set(value['yard']) != {'name', 'project', 'instance'}:
        raise ValueError('invalid yard identity')
    if any(not isinstance(x, str) or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,127}', x) for x in value['yard'].values()):
        raise ValueError('invalid yard identity')
    profiles = value['selected_profiles']
    if not isinstance(profiles, list) or any(not isinstance(x, str) or not re.fullmatch(r'[a-z0-9][a-z0-9_-]*', x) for x in profiles) or len(set(profiles)) != len(profiles):
        raise ValueError('invalid selected profiles')
    return value


def safe(path):
    owner = TRUSTED_OWNER
    root = ROOT.lstat()
    if root.st_uid != TRUSTED_OWNER[0]:
        # Codex maps only the invoking user/group; all other owners, including
        # root, collapse to overflow IDs. Those IDs alone are not root proof.
        # Anchor this exception at / and the fixed system path, and require the
        # sole mapped identity to remain visible so its files cannot pass even
        # when a read-only mount makes access(W_OK) return false. Other unmapped
        # accounts remain indistinguishable: this trusts the administrator's
        # protected system ancestry, not arbitrary overflow-owned paths.
        if path not in (FACTS, HOST_ERRORS):
            raise ValueError('unsafe yard facts directory')
        for name, current, unmapped, trusted in (('uid', os.geteuid(), root.st_uid, TRUSTED_OWNER[0]),
                                                  ('gid', os.getegid(), root.st_gid, TRUSTED_OWNER[1])):
            try:
                mapping = Path('/proc/self/' + name + '_map').read_text().split()
                valid = list(map(int, mapping)) == [current, 0, 1]
            except (OSError, ValueError):
                valid = False
            if not valid or current == 0 or unmapped == current or unmapped == trusted:
                raise ValueError('unsafe yard facts directory')
        owner = (root.st_uid, root.st_gid)
    for parent in (path.parent, *path.parent.parents):
        info = parent.lstat()
        if (not stat.S_ISDIR(info.st_mode) or info.st_uid != owner[0] or info.st_mode & 0o022 or
                (owner != TRUSTED_OWNER and (info.st_gid != owner[1] or os.access(parent, os.W_OK, effective_ids=True)))):
            raise ValueError('unsafe yard facts directory')
        if parent == ROOT:
            break
    return owner


def unique_fields(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise ValueError('duplicate yard facts field')
        value[key] = item
    return value


def load(path, limit):
    owner = safe(path)
    info = path.lstat()
    if (not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o644 or
            (info.st_uid, info.st_gid) != owner or info.st_nlink != 1 or
            (owner != TRUSTED_OWNER and os.access(path, os.W_OK, effective_ids=True))):
        raise ValueError('unsafe yard facts file')
    if info.st_size > limit:
        raise ValueError('invalid yard facts size')
    return json.loads(path.read_bytes(), object_pairs_hook=unique_fields)


def read(path=FACTS, machine=MACHINE):
    value = load(path, 16384)
    if not isinstance(value, dict) or set(value) != {'schema', 'machine_id', 'status', 'target', 'last_success', 'capabilities', 'readiness'} or type(value['schema']) is not int or value['schema'] != 1:
        raise ValueError('unsupported yard facts schema')
    if value['machine_id'] != identity(machine):
        raise ValueError('yard facts belong to another guest')
    snapshot(value['target'])
    if value['last_success'] is not None:
        snapshot(value['last_success'])
        if value['last_success']['yard'] != value['target']['yard']:
            raise ValueError('yard facts identity mismatch')
    if value['status'] not in ('current', 'incomplete') or value['capabilities'] != 'unknown' or value['readiness'] != 'unknown':
        raise ValueError('invalid yard facts state')
    if value['status'] == 'current' and value['last_success'] != value['target']:
        raise ValueError('invalid successful refresh')
    return value


def write(value, path=FACTS):
    safe(path)
    fd, temporary = tempfile.mkstemp(prefix='.' + path.stem + '-', dir=path.parent)
    try:
        with os.fdopen(fd, 'wb') as stream:
            os.fchmod(stream.fileno(), 0o644)
            os.fchown(stream.fileno(), *TRUSTED_OWNER)
            stream.write((json.dumps(value, sort_keys=True) + '\n').encode())
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        descriptor = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def apply(operation, target, path=FACTS, machine=MACHINE):
    snapshot(target)
    # Readers diagnose corrupt/foreign facts. A refresh repairs invalid content
    # without adopting its purported successful version or following links.
    if path.is_symlink():
        raise ValueError('symlinked yard facts file')
    try:
        old = read(path, machine)
    except (OSError, ValueError, TypeError, KeyError):
        old = None
    if operation == 'begin':
        previous = old['last_success'] if old and old['target']['yard'] == target['yard'] else None
        value = dict(schema=1, machine_id=identity(machine), status='incomplete', target=target,
                     last_success=previous, capabilities='unknown', readiness='unknown')
    elif operation == 'commit':
        if old is None or old['status'] != 'incomplete' or old['target'] != target:
            raise ValueError('refresh target changed before completion')
        value = dict(old, status='current', last_success=target)
    else:
        raise ValueError('invalid facts operation')
    write(value, path)


# Host errors: every string is a token or a timestamp, so no error text,
# value or path can pass, whatever the owner sends.
TOKEN = re.compile(r'[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}')
TIME = re.compile(r'[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z')
GENERATED = re.compile(r'[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}[.][0-9]{9}Z')
HOST_ERRORS_LIMIT = 32768 + 128
SECTIONS = ('rpc', 'updates', 'setup')


def entries(value, fields, limit):
    if not isinstance(value, list) or len(value) > limit:
        raise ValueError('invalid host errors')
    for entry in value:
        if not isinstance(entry, dict) or set(entry) != set(fields):
            raise ValueError('invalid host errors')
        for key, kind in fields.items():
            item = entry[key]
            if kind == 'count' and (type(item) is not int or item < 1):
                raise ValueError('invalid host errors')
            if kind == 'number' and (type(item) is not int or item < 0):
                raise ValueError('invalid host errors')
            if kind == 'flag' and type(item) is not bool:
                raise ValueError('invalid host errors')
            if kind in ('token', 'optional', 'time') and not isinstance(item, str):
                raise ValueError('invalid host errors')
            if (kind == 'token' and not TOKEN.fullmatch(item) or
                    kind == 'optional' and item and not TOKEN.fullmatch(item) or
                    kind == 'time' and not TIME.fullmatch(item)):
                raise ValueError('invalid host errors')
            if kind == 'refusals':
                entries(item, {'source': 'token', 'code': 'token', 'names': 'names'}, 8)
            if kind == 'names' and (not isinstance(item, list) or len(item) > 16 or
                                    any(not isinstance(x, str) or not TOKEN.fullmatch(x) for x in item)):
                raise ValueError('invalid host errors')


def host_errors(value, stored=True, machine=MACHINE):
    keys = {'schema', 'yard', 'generated_at', 'omitted', 'unavailable', *SECTIONS}
    if not isinstance(value, dict) or set(value) != (keys | {'machine_id'} if stored else keys):
        raise ValueError('unsupported host errors schema')
    if type(value['schema']) is not int or value['schema'] != 1:
        raise ValueError('unsupported host errors schema')
    if stored and value['machine_id'] != identity(machine):
        raise ValueError('host errors belong to another guest')
    if (not isinstance(value['yard'], str) or not re.fullmatch('[a-z0-9][a-z0-9_-]{0,62}', value['yard']) or
            not isinstance(value['generated_at'], str) or not GENERATED.fullmatch(value['generated_at'])):
        raise ValueError('invalid host errors')
    entries(value['rpc'], {'at': 'time', 'last': 'time', 'count': 'count', 'method': 'token', 'code': 'token'}, 10)
    entries(value['updates'], {'attempt': 'token', 'at': 'time', 'direction': 'token', 'phase': 'token',
                               'status': 'token', 'code': 'optional', 'source': 'optional', 'target': 'optional'}, 10)
    entries(value['setup'], {'attempt': 'token', 'at': 'time', 'phase': 'token', 'status': 'token', 'code': 'optional',
                             'refusals': 'refusals', 'omitted_refusals': 'number', 'incomplete': 'flag'}, 10)
    entries([value['omitted']], {name: 'number' for name in SECTIONS}, 1)
    unavailable = value['unavailable']
    if (not isinstance(unavailable, list) or len(set(unavailable)) != len(unavailable) or
            any(x not in SECTIONS for x in unavailable)):
        raise ValueError('invalid host errors')
    return value


def put_host_errors(value, path=HOST_ERRORS, machine=MACHINE):
    host_errors(value, stored=False)
    if path.is_symlink():
        raise ValueError('symlinked host errors file')
    try:
        old = host_errors(load(path, HOST_ERRORS_LIMIT), machine=machine)
    except (OSError, ValueError, TypeError, KeyError):
        old = None
    # Concurrent owners publish whole snapshots: never replace a newer one.
    if old and old['generated_at'] > value['generated_at']:
        return
    write(dict(value, machine_id=identity(machine)), path)


def main():
    if len(sys.argv) == 1:
        value = read()
        result = dict(value, selected_profiles=value['target']['selected_profiles'])
        print(json.dumps(result, indent=2, sort_keys=True))
        if value['status'] != 'current':
            print('subyard environment: guest helper refresh incomplete; use last_success only', file=sys.stderr)
            return 1
        return 0
    if sys.argv[1:] == ['check']:
        target = snapshot(json.load(sys.stdin))
        value = read()
        if value['status'] != 'current':
            return 10
        if value['target'] == target:
            return 0
        if (value['target']['version'] == target['version'] and
                value['target']['yard'] == target['yard']):
            return PROFILES_PENDING
        return 10
    if sys.argv[1:] == ['host-errors']:
        try:
            value = host_errors(load(HOST_ERRORS, HOST_ERRORS_LIMIT), machine=MACHINE)
        except FileNotFoundError:
            print('subyard environment: no host errors snapshot yet; owner failures are unknown', file=sys.stderr)
            return 1
        print(json.dumps(value, indent=2, sort_keys=True))
        return 0
    if sys.argv[1:] == ['host-errors-put'] and os.geteuid() == 0:
        put_host_errors(json.load(sys.stdin, object_pairs_hook=unique_fields))
        return 0
    if len(sys.argv) == 2 and sys.argv[1] in ('begin', 'commit') and os.geteuid() == 0:
        apply(sys.argv[1], json.load(sys.stdin))
        return 0
    raise ValueError('unsupported yard facts command')


if __name__ == '__main__':
    try:
        sys.exit(main())
    except FileNotFoundError:
        print('subyard environment: facts missing; request a normal owner refresh', file=sys.stderr)
        sys.exit(1)
    except json.JSONDecodeError:
        print('subyard environment: facts corrupt; request a normal owner refresh', file=sys.stderr)
        sys.exit(1)
    except ValueError as error:
        print('subyard environment: ' + str(error) + '; request a normal owner refresh', file=sys.stderr)
        sys.exit(1)
    except (OSError, TypeError, KeyError):
        print('subyard environment: facts unreadable or invalid; request a normal owner refresh', file=sys.stderr)
        sys.exit(1)
