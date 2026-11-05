"""Preserve subordinate ranges while rebinding one local owner label."""
from contextlib import contextmanager
import ctypes
import hashlib
import os
from pathlib import Path
import re
import stat
import tempfile


def digest(payload):
    return hashlib.sha256(payload).hexdigest()


class IdentityRefusal(ValueError):
    """A fixed nonsecret ownership diagnostic safe for the operation boundary."""


class SubordinateIDs:
    def __init__(self, root=Path('/'), owner=0, password_lock=None):
        self.root, self.owner = root, owner
        self.password_lock = password_lock or self.libc_password_lock

    @contextmanager
    def libc_password_lock(self):
        if self.root != Path('/'):
            raise IdentityRefusal('fixture account lock must be explicitly injected')
        library = ctypes.CDLL(None, use_errno=True)
        if library.lckpwdf() != 0:
            raise IdentityRefusal('account database is locked')
        try:
            yield
        finally:
            if library.ulckpwdf() != 0:
                raise IdentityRefusal('account database lock release failed')

    def directory_safe(self):
        directory = self.root/'etc'
        for path in (directory, *directory.parents):
            if path == self.root.parent:
                break
            info = path.lstat()
            if not stat.S_ISDIR(info.st_mode) or info.st_uid != self.owner or info.st_mode & 0o022:
                raise IdentityRefusal('unsafe subordinate identity directory')

    def attributes(self, path):
        return {name: digest(os.getxattr(path,name,follow_symlinks=False)) for name in sorted(os.listxattr(path,follow_symlinks=False))}

    def regular(self, path, mode):
        info = path.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_uid != self.owner or info.st_gid != os.getgid() or stat.S_IMODE(info.st_mode) != mode or info.st_nlink != 1 or info.st_size > 1024*1024:
            raise IdentityRefusal('unsafe subordinate identity metadata')
        return info

    def provider(self):
        self.directory_safe()
        path = self.root/'etc/nsswitch.conf'
        if not path.exists() and not path.is_symlink():
            return 'files'
        self.regular(path, 0o644)
        entries = []
        for line in path.read_text().splitlines():
            line = line.split('#', 1)[0].strip()
            if line.startswith('subid:'):
                entries.append(line.split(':', 1)[1].strip())
        if entries not in ([], ['files']):
            raise IdentityRefusal('external subordinate identity provider cannot be rebound locally')
        return 'files'

    def payload(self, name):
        self.directory_safe()
        directory = self.root/'etc'
        info = directory.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != self.owner or info.st_mode & 0o022:
            raise IdentityRefusal('unsafe subordinate identity directory')
        path = directory/name
        if not path.exists() and not path.is_symlink():
            return None
        self.regular(path, 0o644)
        return path.read_bytes()

    @staticmethod
    def rows(payload):
        result = []
        for line in (payload or b'').splitlines(keepends=True):
            fields = line.rstrip(b'\r\n').split(b':')
            if len(fields) != 3 or not re.fullmatch(rb'[A-Za-z0-9_.-]+', fields[0]) or any(not re.fullmatch(rb'[0-9]+', field) for field in fields[1:]):
                raise IdentityRefusal('invalid subordinate identity row')
            start, count = int(fields[1]), int(fields[2])
            if count < 1 or start+count > 2**32:
                raise IdentityRefusal('invalid subordinate identity range')
            result.append((fields[0].decode('ascii'), start, count, line))
        return result

    def observe(self, old, new, uid):
        self.provider()
        result = {}
        for name in ('subuid', 'subgid'):
            payload = self.payload(name)
            rows = self.rows(payload)
            if any(owner == new for owner, *_ in rows):
                raise IdentityRefusal('destination subordinate identity already exists')
            ranges = [[start, count] for owner, start, count, _ in rows if owner == old]
            if len({tuple(value) for value in ranges}) != len(ranges):
                raise IdentityRefusal('duplicate subordinate identity range')
            for owner, start, count, _ in rows:
                if owner not in (old, str(uid)) and any(start < first+length and first < start+count for first, length in ranges):
                    raise IdentityRefusal('subordinate identity range overlaps another owner')
            after = None if payload is None else b''.join(new.encode()+line[len(old):] if owner == old else line for owner, _, _, line in rows)
            foreign = b''.join(line for owner, _, _, line in rows if owner != old)
            result[name] = dict(present=payload is not None, ranges=ranges,
                                before=None if payload is None else digest(payload),
                                after=None if after is None else digest(after), foreign=digest(foreign), attributes={} if payload is None else self.attributes(self.root/'etc'/name))
        return result

    @contextmanager
    def file_lock(self, name):
        path = self.root/'etc'/name
        temporary = Path(str(path)+'.'+str(os.getpid()))
        lock = Path(str(path)+'.lock')
        fd = os.open(temporary, os.O_CREAT | os.O_EXCL | os.O_WRONLY | os.O_NOFOLLOW, 0o600)
        created = os.fstat(fd)
        owned = None
        try:
            os.fchmod(fd, 0o600)
            os.write(fd, str(os.getpid()).encode()+b'\0')
            os.fdatasync(fd)
            try:
                os.link(temporary, lock, follow_symlinks=False)
            except FileExistsError:
                before = lock.lstat()
                if not stat.S_ISREG(before.st_mode) or before.st_uid != self.owner or before.st_gid != os.getgid() or stat.S_IMODE(before.st_mode) != 0o600 or before.st_nlink not in (1,2) or before.st_size > 11:
                    raise IdentityRefusal('unsafe subordinate database lock')
                payload = lock.read_bytes()
                if not re.fullmatch(rb'[1-9][0-9]{0,9}\0', payload):
                    raise IdentityRefusal('unrecognized subordinate database lock')
                pid = int(payload[:-1])
                try:
                    os.kill(pid, 0)
                except ProcessLookupError:
                    def same_lock(candidate, links):
                        actual = candidate.lstat()
                        return (actual.st_dev, actual.st_ino, actual.st_uid, actual.st_gid, actual.st_mode, actual.st_nlink, actual.st_size) == (before.st_dev, before.st_ino, before.st_uid, before.st_gid, before.st_mode, links, before.st_size) and candidate.read_bytes() == payload
                    # A killed shadow-compatible writer can leave its exact PID
                    # temporary linked to the lock. Only that dead writer's
                    # protected two-name inode is recoverable under lckpwdf.
                    if before.st_nlink == 2:
                        abandoned = Path(str(path)+'.'+str(pid))
                        if not same_lock(abandoned,2) or not same_lock(lock,2):
                            raise IdentityRefusal('subordinate database lock counterpart changed')
                        abandoned.unlink()
                    if not same_lock(lock,1):
                        raise IdentityRefusal('subordinate database lock changed')
                    lock.unlink()
                    os.link(temporary, lock, follow_symlinks=False)
                except PermissionError:
                    raise IdentityRefusal('subordinate database lock is owned by a live process')
                else:
                    raise IdentityRefusal('subordinate database is locked')
            opened = os.fstat(fd)
            named = lock.lstat()
            if opened.st_nlink != 2 or (opened.st_dev, opened.st_ino) != (named.st_dev, named.st_ino):
                raise IdentityRefusal('subordinate database lock identity changed')
            owned = (opened.st_dev, opened.st_ino)
            temporary.unlink()
            yield
        finally:
            os.close(fd)
            if temporary.exists() and not temporary.is_symlink():
                info = temporary.lstat()
                if (info.st_dev, info.st_ino) == (created.st_dev, created.st_ino):
                    temporary.unlink()
            if owned is not None and lock.exists() and not lock.is_symlink():
                info = lock.lstat()
                if (info.st_dev, info.st_ino) == owned:
                    lock.unlink()

    def publish(self, name, content, expected):
        path = self.root/'etc'/name
        info = self.regular(path, 0o644)
        attributes = {name: os.getxattr(path,name,follow_symlinks=False) for name in os.listxattr(path,follow_symlinks=False)}
        fd, temporary = tempfile.mkstemp(prefix='.'+name+'-identity-', dir=path.parent)
        try:
            with os.fdopen(fd, 'wb') as stream:
                os.fchmod(stream.fileno(), stat.S_IMODE(info.st_mode))
                os.fchown(stream.fileno(), info.st_uid, info.st_gid)
                for key,value in attributes.items():
                    os.setxattr(stream.fileno(),key,value)
                stream.write(content)
                stream.flush()
                os.fsync(stream.fileno())
            if self.payload(name) != expected:
                raise IdentityRefusal('subordinate identity database changed')
            os.replace(temporary, path)
            directory = os.open(path.parent, os.O_DIRECTORY | os.O_NOFOLLOW)
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
        finally:
            if os.path.exists(temporary):
                os.unlink(temporary)

    def rebind(self, recipe, old, new, uid, checkpoint=lambda _: None):
        self.provider()
        if not any(value['ranges'] for value in recipe.values()):
            if self.observe(old, new, uid) != recipe:
                raise IdentityRefusal('subordinate identity baseline changed')
            return
        with self.password_lock(), self.file_lock('subuid'), self.file_lock('subgid'):
            self.provider()
            for name in ('subuid', 'subgid'):
                payload = self.payload(name)
                expected = recipe[name]
                actual = None if payload is None else digest(payload)
                if payload is not None and self.attributes(self.root/'etc'/name) != expected['attributes']:
                    raise IdentityRefusal('subordinate identity metadata changed')
                if actual not in (expected['before'], expected['after']):
                    raise IdentityRefusal('subordinate identity state changed')
                if actual == expected['after']:
                    continue
                rows = self.rows(payload)
                owned = [[start, count] for owner, start, count, _ in rows if owner == old]
                foreign = b''.join(line for owner, _, _, line in rows if owner != old)
                if owned != expected['ranges'] or digest(foreign) != expected['foreign'] or any(owner == new for owner, *_ in rows):
                    raise IdentityRefusal('subordinate identity owner scope changed')
                updated = b''.join(new.encode()+line[len(old):] if owner == old else line for owner, _, _, line in rows)
                if digest(updated) != expected['after']:
                    raise IdentityRefusal('subordinate identity result changed')
                self.publish(name, updated, payload)
                checkpoint(name)
