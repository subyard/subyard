#!/usr/bin/env python3
"""Read-only yard facts; private writer entrypoints serve the prepared adapter."""
import json
import os
from pathlib import Path
import re
import stat
import sys
import tempfile

FACTS = Path('/etc/subyard/guest-runtime.json')
MACHINE = Path('/etc/machine-id')
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
    owner = (0, 0)
    root = Path('/').lstat()
    if root.st_uid != 0:
        # Codex maps only the invoking user/group; all other owners, including
        # root, collapse to overflow IDs. Those IDs alone are not root proof.
        # Anchor this exception at / and the fixed system path, and require the
        # sole mapped identity to remain visible so its files cannot pass even
        # when a read-only mount makes access(W_OK) return false. Other unmapped
        # accounts remain indistinguishable: this trusts the administrator's
        # protected system ancestry, not arbitrary overflow-owned paths.
        if path != FACTS:
            raise ValueError('unsafe yard facts directory')
        for name, current, unmapped in (('uid', os.geteuid(), root.st_uid),
                                         ('gid', os.getegid(), root.st_gid)):
            try:
                mapping = Path('/proc/self/' + name + '_map').read_text().split()
                valid = list(map(int, mapping)) == [current, 0, 1]
            except (OSError, ValueError):
                valid = False
            if not valid or current == 0 or unmapped == current or unmapped == 0:
                raise ValueError('unsafe yard facts directory')
        owner = (root.st_uid, root.st_gid)
    for parent in (path.parent, *path.parent.parents):
        info = parent.lstat()
        if (not stat.S_ISDIR(info.st_mode) or info.st_uid != owner[0] or info.st_mode & 0o022 or
                (owner[0] != 0 and (info.st_gid != owner[1] or os.access(parent, os.W_OK, effective_ids=True)))):
            raise ValueError('unsafe yard facts directory')
    return owner


def unique_fields(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise ValueError('duplicate yard facts field')
        value[key] = item
    return value


def read(path=FACTS, machine=MACHINE):
    owner = safe(path)
    info = path.lstat()
    if (not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o644 or
            (info.st_uid, info.st_gid) != owner or info.st_nlink != 1 or
            (owner[0] != 0 and os.access(path, os.W_OK, effective_ids=True))):
        raise ValueError('unsafe yard facts file')
    if info.st_size > 16384:
        raise ValueError('invalid yard facts size')
    value = json.loads(path.read_bytes(), object_pairs_hook=unique_fields)
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
    fd, temporary = tempfile.mkstemp(prefix='.guest-runtime-', dir=path.parent)
    try:
        with os.fdopen(fd, 'wb') as stream:
            os.fchmod(stream.fileno(), 0o644)
            os.fchown(stream.fileno(), 0, 0)
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
