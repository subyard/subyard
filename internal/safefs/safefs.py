"""Linux file primitives for Python 3.11+.

Publication is atomic. The writer never opens the destination through a final
symlink and sets the staging inode's owner and mode before publishing it.
Destination lookups and publication use one pinned parent descriptor. Ancestors
are trusted unless root is specified. A writer of the parent controls its entries.
"""

from contextlib import contextmanager
import ctypes
import errno
import fcntl
import os
from pathlib import Path
import secrets
import stat
import time

DURABLE, BEST_EFFORT = 'durable', 'best-effort'
FINISH, REPAIR, EXACT, ACCEPT = 'finish', 'repair', 'exact', 'accept'
AFTER_STAGE, AFTER_WRITE = 'after-stage', 'after-write'
BEFORE_PUBLISH, AFTER_PUBLISH = 'before-publish', 'after-publish'
AFTER_DIR_CREATE, AFTER_DIR_CHMOD = 'after-dir-create', 'after-dir-chmod'
SYMLINK, NOT_REGULAR, NOT_DIRECTORY = 'symbolic link', 'not a regular file', 'not a directory'
OWNER, MODE, ESCAPES_ROOT = 'unsafe ownership', 'unsafe permissions', 'path escapes root'
MULTIPLE_LINKS, TOO_LARGE = 'multiple hard links', 'file exceeds size limit'
FAULT = None

_DIR = os.O_PATH | os.O_DIRECTORY
_NOFOLLOW = os.O_NOFOLLOW | os.O_CLOEXEC
_SET_ID = stat.S_ISUID | stat.S_ISGID


class Unsafe(ValueError):
    def __init__(self, reason, path):
        self.reason, self.path = reason, os.fspath(path)
        super().__init__(f'safefs: {reason}: {self.path}')


class DirState(Unsafe):
    def __init__(self, reason, path, info):
        super().__init__(reason, path)
        self.mode, self.uid = stat.S_IMODE(info.st_mode), info.st_uid


class SimulatedCrash(BaseException):
    """Leave staging entries on disk while releasing process-local resources."""


def _fault(point):
    if FAULT is not None:
        FAULT(point)


def _owner_ids(owner):
    uid, gid = owner if owner is not None else (-1, -1)
    if any(not -1 <= value < 2**32 - 1 for value in (uid, gid)):
        raise OSError(errno.EINVAL, os.strerror(errno.EINVAL))
    return uid, gid


def _name(path):
    name = os.fspath(path)
    if name in ('', '.', '..') or '/' in name:
        raise OSError(errno.EINVAL, os.strerror(errno.EINVAL), name)
    return name


def _path_parts(path):
    directory, name = os.path.split(path)
    return directory or '.', _name(name)


def _lstat(path, fd):
    try:
        return os.stat(path, dir_fd=fd, follow_symlinks=False)
    except FileNotFoundError:
        return None


def _regular(info, path):
    if stat.S_ISLNK(info.st_mode):
        raise Unsafe(SYMLINK, path)
    if not stat.S_ISREG(info.st_mode):
        raise Unsafe(NOT_REGULAR, path)


def _open_directory_at(parent, name, path):
    fd = os.open(name, os.O_PATH | _NOFOLLOW, dir_fd=parent)
    try:
        info = os.fstat(fd)
        if stat.S_ISLNK(info.st_mode):
            raise Unsafe(SYMLINK, path)
        if not stat.S_ISDIR(info.st_mode):
            raise Unsafe(NOT_DIRECTORY, path)
        return fd, info
    except BaseException:
        os.close(fd)
        raise


def sync_dir(path_or_fd):
    """Sync a directory path or descriptor, including an O_PATH descriptor."""
    if isinstance(path_or_fd, int):
        fd = os.open('.', os.O_RDONLY | os.O_DIRECTORY, dir_fd=path_or_fd)
    else:
        fd = os.open(path_or_fd, os.O_RDONLY | os.O_DIRECTORY | _NOFOLLOW)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def _finish_directory(fd, info, mode, gid):
    actual = stat.S_IMODE(info.st_mode)
    if actual & _SET_ID:
        return
    descriptor = f'/proc/self/fd/{fd}'
    if gid != -1 and info.st_gid != gid:
        os.chown(descriptor, -1, gid)
    wanted = mode | (actual & stat.S_ISVTX)
    if actual != wanted:
        # Procfs addresses the open inode even when its mode is 0000.
        os.chmod(descriptor, wanted)
        _fault(AFTER_DIR_CHMOD)


def _ensure_at(parent, name, path, mode, uid, gid, existing, durability):
    created = False
    try:
        fd, info = _open_directory_at(parent, name, path)
    except FileNotFoundError:
        if uid != os.geteuid():
            raise Unsafe(OWNER, path) from None
        initial = mode if gid == -1 else mode & 0o700
        try:
            os.mkdir(name, initial, dir_fd=parent)
        except FileExistsError:
            pass
        else:
            created = True
            _fault(AFTER_DIR_CREATE)
        fd, info = _open_directory_at(parent, name, path)
    try:
        if created or existing != ACCEPT:
            actual = stat.S_IMODE(info.st_mode)
            wanted = mode | (actual & stat.S_ISVTX)
            if info.st_uid != uid:
                raise DirState(OWNER, path, info)
            change_group = gid != -1 and info.st_gid != gid
            if existing == EXACT and not created:
                if change_group:
                    raise DirState(OWNER, path, info)
                if actual != wanted:
                    raise DirState(MODE, path, info)
            elif existing == FINISH and actual & 0o777 & ~mode:
                raise DirState(MODE, path, info)
            else:
                _finish_directory(fd, info, mode, gid)
        if durability == DURABLE:
            sync_dir(fd)
            sync_dir(parent)
        return fd, created
    except BaseException:
        os.close(fd)
        raise


def _finish_parent(fd, mode, durability):
    # The parent may have been created under a restrictive umask just before
    # an interruption. Existing broader and foreign ancestors are trusted.
    info = os.fstat(fd)
    actual = info.st_mode & 0o777
    wanted = mode & 0o777
    if info.st_uid == os.geteuid() and actual != wanted and not (actual & ~wanted or info.st_mode & _SET_ID):
        _finish_directory(fd, info, mode, -1)
        if durability == DURABLE:
            sync_dir(fd)


def _open_parent_dir(path, mode, durability):
    try:
        fd = os.open(path, _DIR)
    except (FileNotFoundError, PermissionError):
        if mode is None or path in ('.', '/'):
            raise
    else:
        try:
            if mode is not None and path not in ('.', '/'):
                _finish_parent(fd, mode, durability)
            return fd
        except BaseException:
            os.close(fd)
            raise
    parent, name = _path_parts(path)
    fd = _open_parent_dir(parent, mode, durability)
    try:
        child, _ = _ensure_at(fd, name, path, mode, os.geteuid(), -1, FINISH, durability)
        return child
    finally:
        os.close(fd)


def _rooted_parent(directory, root, mode, durability):
    directory, root = Path(directory), Path(root)
    try:
        parts = directory.relative_to(root).parts
    except ValueError:
        raise Unsafe(ESCAPES_ROOT, directory) from None
    if '..' in parts or '..' in root.parts:
        raise Unsafe(ESCAPES_ROOT, directory)
    fd, info = _open_directory_at(None, root, root)
    current = root
    try:
        for component in (*parts, None):
            if info.st_uid != os.geteuid():
                raise DirState(OWNER, current, info)
            if info.st_mode & 0o022:
                raise DirState(MODE, current, info)
            if component is None:
                return fd
            current /= component
            if mode is None:
                child, info = _open_directory_at(fd, component, current)
            else:
                try:
                    child, info = _open_directory_at(fd, component, current)
                except FileNotFoundError:
                    child, _ = _ensure_at(fd, component, current, mode, os.geteuid(), -1, FINISH, durability)
            os.close(fd)
            fd = child
            if mode is not None:
                _finish_parent(fd, mode, durability)
                info = os.fstat(fd)
    except BaseException:
        os.close(fd)
        raise


def _parent(path, dir_fd, parent_mode, root, durability):
    if dir_fd is not None:
        if root is not None or parent_mode is not None:
            raise ValueError('safefs: dir_fd already selects the parent')
        name = _name(path)
        return os.dup(dir_fd), name
    directory, name = _path_parts(path)
    if root is None:
        return _open_parent_dir(directory, parent_mode, durability), name
    if parent_mode is not None and parent_mode & 0o022:
        raise Unsafe(MODE, directory)
    return _rooted_parent(directory, root, parent_mode, durability), name


def open_dir(path, *, mode=0o700, owner=None, existing=FINISH, parent_mode=None, root=None,
             dir_fd=None, durability=DURABLE):
    """Return the caller-owned O_PATH descriptor and whether the leaf was created."""
    if existing not in (FINISH, REPAIR, EXACT, ACCEPT):
        raise ValueError('safefs: invalid existing directory policy')
    uid, gid = _owner_ids(owner)
    if uid == -1:
        uid = os.geteuid()
    parent, name = _parent(path, dir_fd, parent_mode, root, durability)
    try:
        return _ensure_at(parent, name, path, mode, uid, gid, existing, durability)
    finally:
        os.close(parent)


def ensure_dir(path, **options):
    fd, created = open_dir(path, **options)
    os.close(fd)
    return created


def ensure_private_dir(path):
    return ensure_dir(path)


def resolve_file(path):
    """Resolve a user file's final link chain. An actual link must have a target."""
    info = _lstat(path, None)
    if info is None or not stat.S_ISLNK(info.st_mode):
        return path
    if os.geteuid() == 0:
        raise Unsafe(SYMLINK, path)
    return os.path.realpath(path, strict=True)


def _rename_noreplace(src, dst, src_fd, dst_fd):
    src_bytes, dst_bytes = os.fsencode(src), os.fsencode(dst)
    if b'\0' in src_bytes or b'\0' in dst_bytes:
        raise ValueError('embedded null byte')
    rename = getattr(ctypes.CDLL(None, use_errno=True), 'renameat2', None)
    if rename is not None:
        if rename(src_fd, src_bytes, dst_fd, dst_bytes, 1) == 0:
            return True
        error = ctypes.get_errno()
        if error not in (errno.EINVAL, errno.ENOSYS, errno.EOPNOTSUPP):
            raise OSError(error, os.strerror(error), src, None, dst)
    source_parent = None if src_fd == -100 else src_fd
    if stat.S_ISDIR(os.stat(src, dir_fd=source_parent, follow_symlinks=False).st_mode):
        raise OSError(errno.EOPNOTSUPP, 'atomic no-replace directory rename requires renameat2', src, None, dst)
    os.link(src, dst, src_dir_fd=source_parent,
            dst_dir_fd=None if dst_fd == -100 else dst_fd, follow_symlinks=False)
    return False


def rename_noreplace(src, dst, *, src_dir_fd=-100, dst_dir_fd=-100):
    if not _rename_noreplace(src, dst, src_dir_fd, dst_dir_fd):
        os.unlink(src, dir_fd=None if src_dir_fd == -100 else src_dir_fd)


class Pending:
    """Own the staging file and parent descriptors until commit or abort."""

    def __init__(self, path, name, parent, stage, mode, no_replace, durability, check):
        self.path, self._target = path, name
        self._parent, self.dir_fd = parent, stage
        self._mode, self._no_replace = mode, no_replace
        self._durability, self._check = durability, check
        self.name = self.file = self._identity = None
        self.published = self._crashed = False

    def __enter__(self):
        return self

    @property
    def target(self):
        """Destination basename for descriptor-relative checks."""
        return self._target

    @property
    def parent_fd(self):
        """Borrow the pinned destination parent while the pending file is open."""
        return self._parent

    def __exit__(self, kind, _value, _trace):
        if kind is not None and issubclass(kind, SimulatedCrash):
            self._crashed = True
        self.abort()

    def _stage(self, prefix, suffix, owner):
        _name(prefix + '00000000' + suffix)
        alphabet = 'abcdefghijklmnopqrstuvwxyz0123456789_'
        while True:
            name = prefix + ''.join(secrets.choice(alphabet) for _ in range(8)) + suffix
            if name == self._target:
                continue
            try:
                fd = os.open(name, os.O_RDWR | os.O_CREAT | os.O_EXCL | _NOFOLLOW, 0o600, dir_fd=self.dir_fd)
            except FileExistsError:
                continue
            break
        try:
            self.name, self._identity = name, os.fstat(fd)
            self.file = os.fdopen(fd, 'w+b')
        except BaseException:
            os.close(fd)
            raise
        if owner != (-1, -1):
            os.fchown(fd, *owner)
        os.fchmod(fd, self._mode)
        self._fault(AFTER_STAGE)

    def _fault(self, point):
        try:
            _fault(point)
        except SimulatedCrash:
            self._crashed = True
            raise

    def publish(self):
        if self.published:
            return
        if self._parent is None or self._crashed:
            raise ValueError('safefs: pending file is closed')
        if not self.file.closed:
            self.file.flush()
        self._fault(AFTER_WRITE)
        if not self.file.closed:
            if self._mode & _SET_ID:
                os.fchmod(self.file.fileno(), self._mode)
            if self._durability == DURABLE:
                os.fsync(self.file.fileno())
        if self._check is not None:
            self._check(self)
        self.file.close()
        self._fault(BEFORE_PUBLISH)
        if self._no_replace:
            renamed = _rename_noreplace(self.name, self._target, self.dir_fd, self._parent)
            self.published = True
            if not renamed:
                os.unlink(self.name, dir_fd=self.dir_fd)
        else:
            target = _lstat(self._target, self._parent)
            if target is not None:
                _regular(target, self.path)
            os.replace(self.name, self._target, src_dir_fd=self.dir_fd, dst_dir_fd=self._parent)
            self.published = True
        self.name = None
        self._fault(AFTER_PUBLISH)

    def sync_parent(self):
        if self._parent is None or self._crashed:
            raise ValueError('safefs: pending file is closed')
        if self._durability == DURABLE:
            sync_dir(self._parent)
            if self.dir_fd != self._parent:
                sync_dir(self.dir_fd)

    def commit(self):
        self.publish()
        self.sync_parent()
        self.abort()

    def abort(self):
        if self._parent is None:
            return
        try:
            if self.name is not None and not self._crashed:
                try:
                    info = _lstat(self.name, self.dir_fd)
                    if info is not None and os.path.samestat(info, self._identity):
                        os.unlink(self.name, dir_fd=self.dir_fd)
                except OSError:
                    pass
        finally:
            if self.file is not None:
                try:
                    self.file.close()
                except OSError:
                    pass
            if self.dir_fd != self._parent:
                os.close(self.dir_fd)
            os.close(self._parent)
            self._parent = self.dir_fd = None


def create(path, *, mode=0o600, owner=None, preserve_mode=False, prefix=None, suffix='',
           dir_fd=None, stage_dir=None, parent_mode=None, root=None, follow_symlink=False,
           no_replace=False, durability=DURABLE, check=None):
    owner = _owner_ids(owner)
    parent, name = _parent(path, dir_fd, parent_mode, root, durability)
    try:
        info = _lstat(name, parent)
        if info is not None and stat.S_ISLNK(info.st_mode) and follow_symlink and os.geteuid() != 0:
            # Resolve from the pinned directory. The resolved leaf is checked
            # again by the writer and can never be opened through a symlink.
            resolved = os.path.realpath(f'/proc/self/fd/{parent}/{name}', strict=True)
            return create(resolved, mode=mode, owner=owner, preserve_mode=preserve_mode,
                          prefix=prefix, suffix=suffix, stage_dir=stage_dir,
                          root=os.path.abspath(root) if root is not None else None,
                          no_replace=no_replace, durability=durability, check=check)
        if info is not None and (preserve_mode or not no_replace):
            _regular(info, path)
            if preserve_mode:
                mode = stat.S_IMODE(info.st_mode)
        stage = parent
        if stage_dir is not None:
            stage = os.dup(stage_dir) if isinstance(stage_dir, int) else os.open(stage_dir, _DIR | _NOFOLLOW)
        pending = Pending(path, name, parent, stage, mode, no_replace, durability, check)
        parent = None  # Pending owns both descriptors.
        try:
            pending._stage(prefix if prefix is not None else f'.{name}.tmp-', suffix, owner)
        except BaseException:
            pending.abort()
            raise
        return pending
    finally:
        if parent is not None:
            os.close(parent)


def write_from(path, fill, **options):
    with create(path, **options) as pending:
        fill(pending.file)
        pending.commit()


def write_file(path, data, **options):
    write_from(path, lambda file: file.write(data), **options)


def read_regular(path, *, max_bytes=None, owner_uid=None, deny_perm=0, single_link=False, dir_fd=None):
    if dir_fd is not None:
        _name(path)
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NONBLOCK | _NOFOLLOW, dir_fd=dir_fd)
    except FileNotFoundError:
        return None
    except OSError as error:
        if error.errno == errno.ELOOP:
            raise Unsafe(SYMLINK, path) from None
        raise
    try:
        info = os.fstat(fd)
        _regular(info, path)
        if owner_uid is not None and info.st_uid != owner_uid:
            raise Unsafe(OWNER, path)
        if single_link and info.st_nlink != 1:
            raise Unsafe(MULTIPLE_LINKS, path)
        if info.st_mode & deny_perm:
            raise Unsafe(MODE, path)
        source = os.fdopen(fd, 'rb')
    except BaseException:
        os.close(fd)
        raise
    with source:
        if max_bytes is None:
            return source.read()
        if info.st_size > max_bytes or len(data := source.read(max_bytes + 1)) > max_bytes:
            raise Unsafe(TOO_LARGE, path)
        return data


@contextmanager
def lock(path, *, shared=False, deadline=None, mode=0o600):
    fd = os.open(path, os.O_RDWR | os.O_CREAT | os.O_NONBLOCK | _NOFOLLOW, mode)
    try:
        _regular(os.fstat(fd), path)
        operation = fcntl.LOCK_SH if shared else fcntl.LOCK_EX
        if deadline is None:
            fcntl.flock(fd, operation)
        else:
            while True:
                try:
                    fcntl.flock(fd, operation | fcntl.LOCK_NB)
                    break
                except BlockingIOError:
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        raise
                    time.sleep(min(0.005, remaining))
        try:
            yield fd
        finally:
            fcntl.flock(fd, fcntl.LOCK_UN)
    finally:
        os.close(fd)
