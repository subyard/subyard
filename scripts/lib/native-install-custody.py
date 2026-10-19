#!/usr/bin/env python3
"""Generation CAS for the two fixed host installers, under their retained flock."""
import fcntl
import hashlib
import json
import os
import pwd
from pathlib import Path
import re
import secrets
import stat
import sys
import tempfile



def generation(value, absent=False):
    if (absent and value == '0') or re.fullmatch('[a-f0-9]{64}', value):
        return value
    raise ValueError('invalid native install generation')


def safe_parent(path):
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o022:
        raise ValueError('unsafe native install parent')


def validate_lock(path, fd):
    actual, named = os.fstat(fd), path.lstat()
    if (actual.st_dev, actual.st_ino) != (named.st_dev, named.st_ino) or not stat.S_ISREG(actual.st_mode) or actual.st_uid != os.geteuid() or actual.st_nlink != 1 or stat.S_IMODE(actual.st_mode) != 0o600:
        raise ValueError('native install lock identity changed')
    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)


def observe(path, owner):
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    except FileNotFoundError:
        return '0'
    with os.fdopen(fd, 'rb') as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o644 or info.st_size > 4096:
            raise ValueError('unsafe native install generation')
        raw = stream.read(4097)
    value = json.loads(raw)
    if value != {'version': 1, 'owner': owner, 'generation': value.get('generation')} or raw != (json.dumps(value, sort_keys=True, separators=(',', ':'))+'\n').encode():
        raise ValueError('invalid native install generation record')
    return generation(value['generation'])


def claim(path, owner, allowed, desired):
    desired = generation(desired)
    allowed = tuple(generation(value, True) for value in allowed)
    actual = observe(path, owner)
    if actual != desired and actual not in allowed:
        raise ValueError('native install generation changed before mutation')
    if actual != desired:
        fd, temporary = tempfile.mkstemp(prefix='.'+path.name+'.', dir=path.parent)
        try:
            with os.fdopen(fd, 'wb') as stream:
                os.fchmod(stream.fileno(), 0o644)
                stream.write((json.dumps({'version': 1, 'owner': owner, 'generation': desired}, sort_keys=True, separators=(',', ':'))+'\n').encode())
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(temporary, path)
        finally:
            if os.path.exists(temporary): os.unlink(temporary)
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    try: os.fsync(fd)
    finally: os.close(fd)
    fd = os.open(path.parent, os.O_DIRECTORY)
    try: os.fsync(fd)
    finally: os.close(fd)
    if observe(path, owner) != desired:
        raise ValueError('native install generation readback changed')


def installed_files(paths):
    facts = {}
    for value in paths:
        path = Path(value)
        try: info = path.lstat()
        except FileNotFoundError:
            facts[value] = 'absent'
            continue
        if not stat.S_ISREG(info.st_mode): raise ValueError('unsafe native installed file')
        facts[value] = [info.st_dev, info.st_ino, info.st_uid, info.st_gid, info.st_mode, info.st_size,
                        {'Sec': info.st_mtime_ns//1000000000, 'Nsec': info.st_mtime_ns%1000000000},
                        {'Sec': info.st_ctime_ns//1000000000, 'Nsec': info.st_ctime_ns%1000000000},
                        hashlib.sha256(path.read_bytes()).hexdigest()]
    raw = json.dumps(dict(sorted(facts.items())), separators=(',', ':'), ensure_ascii=False)
    for character, escaped in (('&', '\\u0026'), ('<', '\\u003c'), ('>', '\\u003e'), ('\u2028', '\\u2028'), ('\u2029', '\\u2029')):
        raw = raw.replace(character, escaped)
    return hashlib.sha256(raw.encode()).hexdigest()


def claim_vector(owner, resources, descriptors, expected, selected, successor='', ordinary=False):
    roles = ('engine', 'service') if owner == 'host-power' else ('engine', 'service', 'timer')
    if len(resources) != len(roles) or len(set(map(str, resources))) != len(roles) or len(descriptors) != len(roles):
        raise ValueError('invalid native installed resource vector')
    records = []
    for resource, fd in zip(resources, descriptors):
        resource = Path(resource)
        if not resource.is_absolute() or any(value in str(resource) for value in ('\n', '\r', '\x00')):
            raise ValueError('invalid installed resource path')
        safe_parent(resource.parent)
        validate_lock(resource.parent/('.'+resource.name+'.native-install.lock'), fd)
        records.append(resource.parent/('.'+resource.name+'.native-custody.json'))
    current = [observe(record, owner) for record in records]
    if ordinary:
        before = current
        desired = secrets.token_hex(32)
        allowed = [(value,) for value in current]
    else:
        before = json.loads(expected)
        if before != [{'role': role, 'value': value.get('value')} for role, value in zip(roles, before)] or len(before) != len(roles) or json.dumps(before, separators=(',', ':')) != expected:
            raise ValueError('invalid native expected resource vector')
        before = [generation(value['value'], True) for value in before]
        selected = generation(selected)
        desired = generation(successor) if successor else selected
        if successor and (desired == selected or desired in before):
            raise ValueError('native install successor is not fresh')
        allowed = [tuple((value, selected)) for value in before]
    # Validate the WHOLE vector before publishing any entry. A partial prior
    # publication can be finished only with this exact original recipe.
    if any(actual != desired and actual not in permitted for actual, permitted in zip(current, allowed)):
        raise ValueError('native install resource generation changed before mutation')
    for record, permitted in zip(records, allowed):
        claim(record, owner, permitted, desired)
    for resource, fd, record in zip(resources, descriptors, records):
        validate_lock(Path(resource).parent/('.'+Path(resource).name+'.native-install.lock'), fd)
        if observe(record, owner) != desired:
            raise ValueError('native install vector readback changed')


def main():
    action, owner, resource, *arguments = sys.argv[1:]
    if owner not in ('host-power', 'test-vms-host-sink'):
        raise ValueError('unsupported native installer')
    if os.environ.get('SUBYARD_NATIVE_OWNER_STDIN') == '1':
        # fd0 is the original account OFD, never a /proc reopen. This must run
        # after the actual privilege boundary and before creating any files.
        uid = pwd.getpwnam(os.environ['SUBYARD_USER']).pw_uid
        home = os.environ['SUBYARD_HOME']
        if not home.startswith('/') or os.path.normpath(home) != home:
            raise ValueError('invalid native owner scope')
        directory = Path('/tmp')/('subyard-owner-locks-'+str(uid))
        parent = directory.lstat()
        if not stat.S_ISDIR(parent.st_mode) or parent.st_uid != uid or stat.S_IMODE(parent.st_mode) != 0o700:
            raise ValueError('unsafe native owner barrier parent')
        named = (directory/(hashlib.sha256(os.fsencode(home)).hexdigest()+'.lock')).lstat()
        actual = os.fstat(0)
        if not stat.S_ISREG(actual.st_mode) or actual.st_uid != uid or actual.st_nlink != 1 or stat.S_IMODE(actual.st_mode) != 0o600 or (named.st_dev, named.st_ino, named.st_mode, named.st_uid, named.st_nlink) != (actual.st_dev, actual.st_ino, actual.st_mode, actual.st_uid, actual.st_nlink):
            raise ValueError('native owner barrier did not survive privilege entry')
    if action == 'owner-barrier': return
    resource = Path(resource)
    if not resource.is_absolute() or any(value in str(resource) for value in ('\n', '\r', '\x00')):
        raise ValueError('invalid installed resource path')
    root = resource.parent
    safe_parent(root)
    lock = root/('.'+resource.name+'.native-install.lock')
    record = root/('.'+resource.name+'.native-custody.json')
    if action == 'prepare-lock':
        try:
            fd = os.open(lock, os.O_RDWR | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
            created = True
        except FileExistsError:
            fd = os.open(lock, os.O_RDWR | os.O_NOFOLLOW)
            created = False
        try:
            info = os.fstat(fd)
            if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_nlink != 1:
                raise ValueError('unsafe native install lock')
            if created: os.fchmod(fd, 0o600)
            elif stat.S_IMODE(info.st_mode) != 0o600: raise ValueError('unsafe native install lock mode')
        finally: os.close(fd)
        return
    fd = int(arguments.pop(0))
    validate_lock(lock, fd)
    safe_parent(root)
    if action == 'validate': return
    if action == 'vector':
        mode, expected, selected, successor, *pairs = arguments
        if len(pairs) % 2 or mode not in ('claim', 'invalidate', 'fence'):
            raise ValueError('invalid native resource vector action')
        claim_vector(owner, [Path(value) for value in pairs[::2]], [int(value) for value in pairs[1::2]], expected, selected, successor, mode == 'invalidate')
    elif action == 'before':
        expected, *paths = arguments
        generation(expected)
        if len(paths) != {'host-power': 2, 'test-vms-host-sink': 3}[owner] or installed_files(paths) != expected:
            raise ValueError('native installed files changed before mutation')
    elif action == 'claim':
        expected, selected = arguments
        claim(record, owner, (expected,), selected)
    elif action == 'invalidate':
        claim(record, owner, (observe(record, owner),), secrets.token_hex(32))
    elif action == 'fence':
        expected, selected, successor = arguments
        if successor in (expected, selected): raise ValueError('native install successor is not fresh')
        claim(record, owner, (expected, selected), successor)
    else:
        raise ValueError('invalid native installer action')
    validate_lock(lock, fd)


if __name__ == '__main__': main()
