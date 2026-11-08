# Declared persist-directory adoption ("destroy nothing until success").
# One run copies the source directory S into a flat stage G beside the persist
# target T, publishes the copies into T without replacing anything (the target
# wins), then exchanges a prepared link for S in one RENAME_EXCHANGE, so the old
# directory lands at the backup name B. Only after that commit point does finish
# remove the stage copies, the backup and the transfer descriptor D.
#
# Until the exchange the program only opens, reads, creates with O_EXCL, makes
# directories, renames with RENAME_NOREPLACE, exchanges, makes symlinks and
# fsyncs. Destructive calls exist only in discard_stage, delete_backup and
# remove_own_records; a source-level test enforces that.
#
# Started as root only to drop to the developer; every access below is
# descriptor-relative and never follows links. Configured roots and their
# ancestors are trusted: a mount placed onto them by root inside the yard during
# an operation is outside the contract, since root can change that data directly.
import ctypes
import errno
import grp
import hashlib
import json
import os
import pwd
import re
import stat
import sys

SCHEMA = 3
RESERVE = 1 << 30
NOREPLACE, EXCHANGE = 1, 2
DIRECTORY = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC
CHUNK = 1 << 20
LISTED = 20
# An existing target entry wins. A target that refuses an entry would lose its
# bytes with the backup, so the transfer is refused before the exchange.
TARGET_WINS = (errno.EEXIST, errno.ENOTEMPTY)
TARGET_REFUSES = (errno.EACCES, errno.EPERM, errno.ENOSPC, errno.EDQUOT, errno.EROFS)
ID = re.compile(r'^[0-9a-f]{16}$')


class Refusal(Exception):
    def __init__(self, reason, path, detail='', integration=''):
        self.reason, self.path, self.detail, self.integration = reason, path, detail, integration


class Refusals(Exception):
    def __init__(self, items):
        self.items = items


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def renameat2(old_fd, old, new_fd, new, flags):
    function = ctypes.CDLL(None, use_errno=True).renameat2
    function.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
    function.restype = ctypes.c_int
    if function(old_fd, os.fsencode(old), new_fd, os.fsencode(new), flags) != 0:
        code = ctypes.get_errno()
        raise OSError(code, os.strerror(code))


# ---------------------------------------------------------------- descriptors

def open_path(path):
    """Open an absolute directory without following any component."""
    fd = os.open('/', DIRECTORY)
    try:
        for part in [p for p in path.split('/') if p]:
            child = os.open(part, DIRECTORY, dir_fd=fd)
            os.close(fd)
            fd = child
        return fd
    except BaseException:
        os.close(fd)
        raise


def fd_label(parent, name):
    try:
        return os.readlink('/proc/self/fd/%d' % parent) + '/' + name
    except OSError:
        return name


def mount_id(fd):
    """Mount identity of an opened descriptor; a bind mount of the same filesystem differs."""
    with open('/proc/self/fdinfo/%d' % fd) as stream:
        for line in stream:
            if line.startswith('mnt_id:'):
                return int(line.split()[1])
    raise OSError(errno.ENOTSUP, 'mount identity is unavailable')


def open_entry(parent, name, flags, expected=None):
    """Open one entry below a verified parent, then verify the descriptor
    actually used: same filesystem, same mount and, if given, the expected inode."""
    fd = os.open(name, flags | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=parent)
    try:
        st = os.fstat(fd)
        if (st.st_dev != os.fstat(parent).st_dev or mount_id(fd) != mount_id(parent)
                or expected is not None and (st.st_dev, st.st_ino) != (expected.st_dev, expected.st_ino)):
            raise Refusal('persist mount boundary', fd_label(parent, name))
    except BaseException:
        os.close(fd)
        raise
    return fd


def open_child(fd, name, expected=None):
    return open_entry(fd, name, DIRECTORY, expected)


def crosses_mount(parent, name, st):
    """True when the entry name below the opened parent is on another mount."""
    if st.st_dev != os.fstat(parent).st_dev:
        return True
    fd = os.open(name, os.O_PATH | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=parent)
    try:
        return mount_id(fd) != mount_id(parent) or os.fstat(fd).st_ino != st.st_ino
    finally:
        os.close(fd)


def lstat(fd, name):
    try:
        return os.stat(name, dir_fd=fd, follow_symlinks=False)
    except FileNotFoundError:
        return None


def kind(st):
    if st is None:
        return None
    if stat.S_ISDIR(st.st_mode):
        return 'dir'
    if stat.S_ISREG(st.st_mode):
        return 'file'
    if stat.S_ISLNK(st.st_mode):
        return 'link'
    return 'special'


def identity(st):
    return [st.st_dev, st.st_ino] if st is not None else None


def key(st):
    return [st.st_ino, st.st_size, st.st_mtime_ns, st.st_ctime_ns]


def readlink(fd, name):
    return os.readlink(name, dir_fd=fd)


def lexists_path(path):
    try:
        parent = open_path(os.path.dirname(path))
    except OSError:
        return None
    try:
        return lstat(parent, os.path.basename(path))
    finally:
        os.close(parent)


def filesystem_refusal(error, fallback):
    """An operating-system error on declared data is a path-bearing refusal."""
    path = error.filename if isinstance(error.filename, str) and os.path.isabs(error.filename) else fallback
    detail = os.strerror(error.errno) if error.errno else type(error).__name__
    if isinstance(error.filename, str) and not os.path.isabs(error.filename):
        detail += ': ' + error.filename
    return Refusal('persist filesystem error', path, detail)


def unsafe_root(st):
    """The same owner and write-bit rule as the artifact inventory guards."""
    return st is None or kind(st) != 'dir' or st.st_uid != UID or st.st_mode & 0o022


# ---------------------------------------------------------------- names

def names(mapping, transfer_id, yard):
    """Every name of one transfer derives from the source, target, yard and id."""
    source, target = mapping['source'], mapping['target']
    base = os.path.basename(source)
    return {
        'descriptor': base + '.subyard-transfer.json',
        'temporary': base + '.subyard-transfer.json.' + transfer_id + '.tmp',
        'backup': base + '.subyard-backup-' + transfer_id,
        'stage': os.path.dirname(target) + '/.subyard-stage-' + yard + '-' + transfer_id,
        'copy': '.subyard-copy-' + transfer_id + '-',
    }


def stale_temporaries(parent, base):
    prefix = base + '.subyard-transfer.json.'
    return sorted(n for n in os.listdir(parent)
                  if n.startswith(prefix) and n.endswith('.tmp') and ID.match(n[len(prefix):-4]))


def read_descriptor(parent, mapping, yard):
    """The published descriptor beside S, or None. A descriptor that is not our
    own complete record is refused, never interpreted."""
    name = os.path.basename(mapping['source']) + '.subyard-transfer.json'
    path = os.path.dirname(mapping['source']) + '/' + name
    try:
        fd = open_entry(parent, name, os.O_RDONLY)
    except FileNotFoundError:
        return None
    except (OSError, Refusal):
        raise Refusal('persist transfer identity unavailable', path)
    try:
        st = os.fstat(fd)
        if not stat.S_ISREG(st.st_mode) or st.st_uid != UID or st.st_nlink != 1 or st.st_size > 1 << 16:
            raise Refusal('persist transfer identity unavailable', path)
        with os.fdopen(os.dup(fd), 'rb') as stream:
            value = json.load(stream)
    except (OSError, ValueError):
        raise Refusal('persist transfer identity unavailable', path)
    finally:
        os.close(fd)
    target = value.get('target') if isinstance(value, dict) else None
    if (not isinstance(value, dict) or value.get('schema') != SCHEMA or value.get('yard') != yard
            or value.get('source') != mapping['source'] or not isinstance(value.get('id'), str)
            or not ID.match(value['id']) or not isinstance(target, str) or not os.path.isabs(target)
            or os.path.normpath(target) != target or not isinstance(value.get('root'), list)):
        raise Refusal('persist transfer identity unavailable', path)
    value['names'] = names(dict(mapping, target=target), value['id'], yard)
    return value


# ---------------------------------------------------------------- destructive sites

def remove_own_records(parent, own):
    """Unlink our own record names (descriptor, its temporary, a prepared link,
    a probe) below parent. Only regular files and symlinks owned by the
    developer are removed; anything else under such a name stays."""
    for name in own:
        st = lstat(parent, name)
        if st is not None and kind(st) in ('file', 'link') and st.st_uid == UID:
            os.unlink(name, dir_fd=parent)
    os.fsync(parent)


def discard_stage(stage, prefix):
    """Unlink this transfer's own copies (regular files and symlinks with the
    exclusive prefix) in the flat stage, then remove the stage. It never
    descends. Returns True when the stage is retained (something else is left)."""
    try:
        parent = open_path(os.path.dirname(stage))
    except FileNotFoundError:
        return False
    try:
        name = os.path.basename(stage)
        st = lstat(parent, name)
        if st is None:
            return False
        if kind(st) != 'dir':
            return True
        try:
            fd = open_child(parent, name, st)
        except Refusal:
            return True
        try:
            for entry in sorted(os.listdir(fd)):
                child = lstat(fd, entry)
                if (entry.startswith(prefix) and kind(child) in ('file', 'link')
                        and child.st_uid == UID and not crosses_mount(fd, entry, child)):
                    os.unlink(entry, dir_fd=fd)
            os.fsync(fd)
        finally:
            os.close(fd)
        try:
            os.rmdir(name, dir_fd=parent)
        except OSError as error:
            if error.errno not in (errno.ENOTEMPTY, errno.EEXIST):
                raise
            return True
        finally:
            os.fsync(parent)
        return False
    finally:
        os.close(parent)


def delete_backup(parent, name, recorded):
    """Delete the committed backup only if the opened directory is the recorded
    source identity on its parent's mount. The walk never follows a link and
    never enters another mount. Returns True when anything is retained."""
    st = lstat(parent, name)
    if st is None:
        return False
    if kind(st) != 'dir' or identity(st) != recorded:
        return True
    def clear_tree(fd):
        """Empty one opened backup directory."""
        retained = False
        try:
            mode = stat.S_IMODE(os.fstat(fd).st_mode)
            if mode & 0o700 != 0o700:
                os.fchmod(fd, mode | 0o700)
            entries = sorted(os.listdir(fd))
        except OSError:
            return True
        for entry in entries:
            try:
                child = lstat(fd, entry)
                if child is None:
                    continue
                if kind(child) == 'dir':
                    try:
                        nested = open_child(fd, entry, child)
                    except Refusal:
                        retained = True
                        continue
                    try:
                        if clear_tree(nested):
                            retained = True
                            continue
                    finally:
                        os.close(nested)
                    os.rmdir(entry, dir_fd=fd)
                elif crosses_mount(fd, entry, child):
                    retained = True
                else:
                    os.unlink(entry, dir_fd=fd)
            except OSError:
                retained = True
        try:
            os.fsync(fd)
        except OSError:
            pass
        return retained

    try:
        fd = open_child(parent, name, st)
    except Refusal:
        return True
    try:
        retained = clear_tree(fd)
    finally:
        os.close(fd)
    if retained:
        return True
    try:
        os.rmdir(name, dir_fd=parent)
    except OSError:
        return True
    os.fsync(parent)
    return False


# ---------------------------------------------------------------- trees

def scan(fd, root, problems, rel=''):
    """List a source tree: rel -> (kind, stat, link target). Never crosses a mount.
    A failure on one entry is reported with its path; the other entries are kept."""
    result = {}
    for name in sorted(os.listdir(fd)):
        path = rel + name
        try:
            st = lstat(fd, name)
            if st is None:
                continue
            entry = kind(st)
            if entry != 'dir' and crosses_mount(fd, name, st):
                problems.append(Refusal('persist mount boundary', root + '/' + path))
                continue
            if entry == 'special':
                problems.append(Refusal('unsupported persist entry', root + '/' + path))
                continue
            if st.st_uid != UID or entry != 'link' and st.st_mode & 0o022:
                problems.append(Refusal('unsafe persist data', root + '/' + path))
                continue
            wanted = os.R_OK | os.X_OK if entry == 'dir' else os.R_OK
            if entry != 'link' and not os.access(name, wanted, dir_fd=fd, follow_symlinks=False):
                problems.append(Refusal('persist filesystem error', root + '/' + path, 'not readable by the developer'))
                continue
            result[path] = (entry, st, readlink(fd, name) if entry == 'link' else None)
            if entry == 'dir':
                child = open_child(fd, name, st)
                try:
                    result.update(scan(child, root, problems, path + '/'))
                finally:
                    os.close(child)
        except Refusal:
            problems.append(Refusal('persist mount boundary', root + '/' + path))
        except OSError as error:
            problems.append(filesystem_refusal(error, root + '/' + path))
    return result


def chain(fd):
    """(dev, ino) of an opened directory and each of its ancestors."""
    result = []
    current = os.dup(fd)
    try:
        while True:
            st = os.fstat(current)
            if result and result[-1] == identity(st):
                return result
            result.append(identity(st))
            parent = os.open('..', DIRECTORY, dir_fd=current)
            os.close(current)
            current = parent
    finally:
        os.close(current)


def nearest(path):
    """The nearest existing ancestor directory of path (path itself included)."""
    while True:
        try:
            return path, open_path(path)
        except FileNotFoundError:
            path = os.path.dirname(path)


def existing(source_tree, target):
    """Source entries whose name already exists in the target: they are not
    transferred. A target entry where S has a directory but T has none excludes
    the whole subtree. Raises a mount refusal for a mount inside T on the way."""
    found = []
    try:
        root = open_path(target)
    except FileNotFoundError:
        return found
    fds, blocked = {'': root}, []
    try:
        for rel in sorted(source_tree, key=lambda r: r.split('/')):
            if any(rel.startswith(b + '/') for b in blocked):
                continue
            parent = fds.get(os.path.dirname(rel))
            if parent is None:
                continue
            name = os.path.basename(rel)
            st = lstat(parent, name)
            if st is None:
                continue
            if source_tree[rel][0] == 'dir' and kind(st) == 'dir':
                try:
                    fds[rel] = open_child(parent, name, st)
                except Refusal:
                    raise Refusal('persist mount boundary', target + '/' + rel, 'a mount inside the persist target')
                continue
            found.append(rel)
            if source_tree[rel][0] == 'dir':
                blocked.append(rel)
    finally:
        for fd in fds.values():
            os.close(fd)
    return found


# ---------------------------------------------------------------- observe

def target_checks(mapping, source_fd, problems):
    source, target = mapping['source'], mapping['target']
    mount = '/' + '/'.join(target.strip('/').split('/')[:3])
    st = lexists_path(mount)
    if kind(st) != 'dir':
        problems.append(Refusal('session mount is unavailable', target))
        return None
    try:
        ancestor, fd = nearest(target)
    except OSError:
        problems.append(Refusal('unsafe persist target', os.path.dirname(target)))
        return None
    try:
        # Aliases are found by identity, not by path strings.
        target_chain, source_chain = chain(fd), chain(source_fd)
        # T (or the directory it would be created in) inside S, or S inside an existing T.
        if source_chain[0] in target_chain or ancestor == target and target_chain[0] in source_chain:
            problems.append(Refusal('persist paths overlap', source, target))
            return None
        target_st = os.fstat(fd) if ancestor == target else None
    finally:
        os.close(fd)
    # The stage is created beside T and the copies are published into T.
    for writable in [os.path.dirname(target), target] if ancestor == target else [ancestor]:
        if not os.access(writable, os.W_OK | os.X_OK, follow_symlinks=False):
            problems.append(Refusal('unsafe persist target', writable, 'not writable by the developer'))
            return None
    if target_st is None and lexists_path(target) is not None:
        problems.append(Refusal('unsafe persist target', target))
        return None
    if target_st is not None:
        parent = open_path(os.path.dirname(target))
        try:
            mounted = crosses_mount(parent, os.path.basename(target), lstat(parent, os.path.basename(target)))
        finally:
            os.close(parent)
        if mounted:
            problems.append(Refusal('persist target filesystem', target, 'the target is a mount point; its stage would be on another filesystem'))
            return None
        if unsafe_root(target_st):
            problems.append(Refusal('unsafe persist target', target, 'must be owned by the developer and not writable by group or others'))
            return None
    vfs = os.statvfs(ancestor)
    return {'identity': ['dir'] + identity(target_st) if target_st else [None] + identity(os.stat(ancestor)),
            'available': vfs.f_bavail * vfs.f_frsize}


def classify(parent, mapping, yard, source_st):
    """Derive the transfer state from the filesystem and the descriptor:
    - no descriptor: 'none' (N, Done or R; retained items are listed by name only);
    - B is the recorded directory: 'exchanged' (X, committed; S may even have
      been recreated, which is then a fresh adoption after finish);
    - S is still the recorded directory: 'active' (A, before the exchange);
    - neither name holds the recorded directory (remnant): 'exchanged' too.
      Finish then discards only own stage copies, deletes no backup (none has
      the recorded identity), removes the descriptor and leaves anything else
      under the retained names for status to list."""
    record = read_descriptor(parent, mapping, yard)
    if record is None:
        return 'none', None
    backup = lstat(parent, record['names']['backup'])
    if kind(backup) == 'dir' and identity(backup) == record['root']:
        return 'exchanged', record
    if kind(source_st) == 'dir' and identity(source_st) == record['root']:
        return 'active', record
    # Neither name holds the recorded directory: only own remnants are left.
    return 'exchanged', record


def observe_mapping(mapping, yard, start, problems):
    source, target, selected = mapping['source'], mapping['target'], mapping['selected']
    result = {'integration': mapping['integration'], 'source': source, 'target': target, 'selected': selected,
              'ops': [], 'id': '', 'state': 'none', 'persist': '', 'files': 0, 'bytes': 0,
              'existing': [], 'existing_count': 0, 'identity': None, 'target_identity': None}
    try:
        parent = open_path(os.path.dirname(source))
    except FileNotFoundError:
        return result
    try:
        base = os.path.basename(source)
        source_st = lstat(parent, base)
        result['identity'] = [kind(source_st)] + (identity(source_st) or [])
        state, record = classify(parent, mapping, yard, source_st)
        result['state'] = state
        if record:
            result['id'] = record['id']
        exact_link = (kind(source_st) == 'link' and source_st.st_uid == UID
                      and readlink(parent, base) == target)
        ops = []
        if state == 'exchanged':
            # Completing a committed switch is allowed everywhere, activation included.
            ops.append('finish')
        elif state == 'active' and start:
            ops.append('cancel')
        fresh = selected and kind(source_st) == 'dir' and (state != 'active' or start)
        if fresh and not start:
            problems.append(Refusal('persist adoption pending', source, '', mapping['integration']))
            result['persist'] = 'deferred'
        elif state == 'active' and selected and not start:
            problems.append(Refusal('persist adoption pending', source, '', mapping['integration']))
            result['persist'] = 'deferred'
        elif fresh:
            ops.append('adopt')
            preflight(parent, mapping, source_st, result, problems)
        if selected and (exact_link or 'adopt' in ops) and result['persist'] != 'deferred':
            result['persist'] = 'projected'
        result['ops'] = ops
        return result
    finally:
        os.close(parent)


def preflight(parent, mapping, source_st, result, problems):
    """Every independent refusal of a new transfer, collected together."""
    source = mapping['source']
    if unsafe_root(source_st):
        problems.append(Refusal('unsafe persist data', source, 'must be owned by the developer and not writable by group or others'))
    if crosses_mount(parent, os.path.basename(source), source_st):
        problems.append(Refusal('persist mount boundary', source, 'the persist source is a mount point'))
        return
    source_fd = open_child(parent, os.path.basename(source), source_st)
    try:
        tree = scan(source_fd, source, problems)
        checked = target_checks(mapping, source_fd, problems)
    finally:
        os.close(source_fd)
    result['files'] = sum(1 for e in tree.values() if e[0] != 'dir')
    result['bytes'] = sum(e[1].st_size for e in tree.values() if e[0] == 'file')
    if checked is None:
        return
    result['target_identity'] = checked['identity']
    try:
        found = existing(tree, mapping['target'])
    except Refusal as error:
        problems.append(error)
        return
    result['existing'], result['existing_count'] = found[:LISTED], len(found)
    if checked['available'] < result['bytes'] + RESERVE:
        problems.append(Refusal('persist space unavailable', mapping['target'],
                                'needs %d bytes plus a 1 GiB reserve; %d available' % (result['bytes'], checked['available'])))


def observe(request, start):
    yard = request['yard']
    entries, problems = [], []
    for mapping in sorted(request['mappings'], key=lambda m: m['source']):
        before = len(problems)
        try:
            entry = observe_mapping(mapping, yard, start, problems)
        except Refusal as error:
            problems.append(error)
            continue
        except OSError as error:
            problems.append(filesystem_refusal(error, mapping['source']))
            continue
        finally:
            for problem in problems[before:]:
                problem.integration = problem.integration or mapping['integration']
        if len(problems) != before and entry['persist'] == 'projected':
            # The refusal explains this path; the inventory must not report it again.
            entry['persist'] = 'deferred'
        entries.append(entry)
    return entries, problems


# ---------------------------------------------------------------- apply

def probe(directory_fd, exchange):
    token = '.subyard-probe-' + os.urandom(6).hex()
    first, second, third = token + '-a', token + '-b', token + '-c'
    try:
        os.symlink('a', first, dir_fd=directory_fd)
        os.symlink('b', second, dir_fd=directory_fd)
        try:
            if exchange:
                renameat2(directory_fd, first, directory_fd, second, EXCHANGE)
                if readlink(directory_fd, first) != 'b':
                    raise OSError(errno.EINVAL, 'exchange did not swap')
            else:
                try:
                    renameat2(directory_fd, first, directory_fd, second, NOREPLACE)
                    raise OSError(errno.EINVAL, 'no-replace rename replaced')
                except OSError as error:
                    if error.errno != errno.EEXIST:
                        raise
                renameat2(directory_fd, first, directory_fd, third, NOREPLACE)
        except OSError as error:
            raise Refusal('atomic exchange unsupported' if exchange else 'no-replace rename unsupported',
                          '', os.strerror(error.errno or errno.EINVAL))
    finally:
        remove_own_records(directory_fd, (first, second, third))


def ensure_directory(path):
    """Open path, creating missing directories as the developer (mode 0700).
    Returns the descriptor and whether path itself was created."""
    ancestor, fd = nearest(path)
    try:
        missing = [p for p in path[len(ancestor):].split('/') if p]
        for name in missing:
            os.mkdir(name, 0o700, dir_fd=fd)
            os.fsync(fd)
            child = open_child(fd, name)
            os.close(fd)
            fd = child
        return fd, bool(missing)
    except BaseException:
        os.close(fd)
        raise


def copy_file(sparent, name, stage, staged):
    """Copy one file into the stage under a new exclusive own name; returns the key copied."""
    source = open_entry(sparent, name, os.O_RDONLY)
    try:
        before = os.fstat(source)
        copy = os.open(staged, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600, dir_fd=stage)
        try:
            while True:
                chunk = os.read(source, CHUNK)
                if not chunk:
                    break
                view = memoryview(chunk)
                while view:
                    view = view[os.write(copy, view):]
            os.fchmod(copy, stat.S_IMODE(before.st_mode))
            os.utime(copy, ns=(before.st_atime_ns, before.st_mtime_ns))
            os.fsync(copy)
        finally:
            os.close(copy)
        return key(before)
    finally:
        os.close(source)


def stage_pass(source_fd, label, stage, prefix, copies, counter):
    """Copy every new or changed entry into the stage; returns the scanned tree."""
    problems = []
    tree = scan(source_fd, label, problems)
    if problems:
        raise Refusals(problems)
    fds = {'': source_fd}
    try:
        for rel in sorted(tree, key=lambda r: r.split('/')):
            entry, st, link = tree[rel]
            parent = fds.get(os.path.dirname(rel))
            if parent is None:
                continue
            name = os.path.basename(rel)
            if entry == 'dir':
                try:
                    fds[rel] = open_child(parent, name, st)
                except FileNotFoundError:
                    pass
                continue
            if rel in copies and copies[rel][1] == (key(st) if entry == 'file' else link):
                continue
            staged = prefix + str(next(counter))
            try:
                if entry == 'link':
                    os.symlink(link, staged, dir_fd=stage)
                    copies[rel] = (staged, link)
                else:
                    copies[rel] = (staged, copy_file(parent, name, stage, staged))
            except FileNotFoundError:
                continue  # removed from S meanwhile
    finally:
        for rel, fd in fds.items():
            if rel:
                os.close(fd)
    return tree


def publish(tree, copies, stage, target, root_mode, skipped):
    """Rename the copies into T without replacing anything. Existing entries win.
    Only directories this run creates get modes; existing T directories are
    never changed. Entries the target refuses are all reported together, and
    the transfer stops before its exchange."""
    root, created = ensure_directory(target)
    fds, blocked, made, refused = {'': root}, [], {}, []
    if created:
        made[''] = root_mode
    try:
        for rel in sorted(tree, key=lambda r: r.split('/')):
            if any(rel.startswith(b + '/') for b in blocked):
                continue
            entry, st, _ = tree[rel]
            parent = fds.get(os.path.dirname(rel))
            if parent is None:
                continue
            name = os.path.basename(rel)
            current = lstat(parent, name)
            if entry == 'dir':
                try:
                    if current is None:
                        os.mkdir(name, stat.S_IMODE(st.st_mode) | 0o700, dir_fd=parent)
                        fds[rel] = open_child(parent, name)
                        made[rel] = stat.S_IMODE(st.st_mode)
                    elif kind(current) == 'dir':
                        fds[rel] = open_child(parent, name, current)
                    else:
                        skipped.append(rel)
                        blocked.append(rel)
                except Refusal:
                    skipped.append(rel)
                    blocked.append(rel)
                except OSError as error:
                    if error.errno in TARGET_REFUSES:
                        refused.append(Refusal('persist target entry refused', target + '/' + rel, os.strerror(error.errno)))
                    elif error.errno in TARGET_WINS:
                        skipped.append(rel)
                    else:
                        raise
                    blocked.append(rel)
                continue
            if current is not None or rel not in copies:
                skipped.append(rel)
                continue
            try:
                renameat2(stage, copies[rel][0], parent, name, NOREPLACE)
            except OSError as error:
                if error.errno in TARGET_REFUSES:
                    refused.append(Refusal('persist target entry refused', target + '/' + rel, os.strerror(error.errno)))
                elif error.errno in TARGET_WINS:
                    skipped.append(rel)
                else:
                    raise
        for fd in fds.values():
            os.fsync(fd)
        # Created directories get their captured modes last, deepest first.
        for rel in sorted(made, key=lambda r: -len(r.split('/')) if r else 0):
            os.fchmod(fds[rel], made[rel])
        if refused:
            raise Refusals(refused)
    finally:
        for fd in fds.values():
            os.close(fd)
        parent = open_path(os.path.dirname(target))
        try:
            os.fsync(parent)
        finally:
            os.close(parent)


def adopt(mapping, entry, yard):
    """One transfer from preflight to the commit point and finish."""
    source, target = mapping['source'], mapping['target']
    parent = open_path(os.path.dirname(source))
    try:
        base = os.path.basename(source)
        source_st = lstat(parent, base)
        if kind(source_st) != 'dir' or [kind(source_st)] + identity(source_st) != entry['identity']:
            raise Refusal('integration plan is stale', source)
        remove_own_records(parent, stale_temporaries(parent, base))
        transfer_id = os.urandom(8).hex()
        own = names(mapping, transfer_id, yard)
        stage_parent, _ = ensure_directory(os.path.dirname(target))
        try:
            problems = []
            for fd, exchange in ((parent, True), (stage_parent, False)):
                try:
                    probe(fd, exchange)
                except Refusal as error:
                    problems.append(error)
            if problems:
                raise Refusals(problems)
            # The published descriptor is complete: temporary, fsync, no-replace rename.
            temporary = os.open(own['temporary'], os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600, dir_fd=parent)
            with os.fdopen(temporary, 'wb') as stream:
                stream.write(json.dumps({'schema': SCHEMA, 'yard': yard, 'id': transfer_id, 'source': source,
                                         'target': target, 'stage': own['stage'], 'backup': own['backup'],
                                         'root': identity(source_st)}, sort_keys=True).encode())
                stream.flush()
                os.fsync(stream.fileno())
            try:
                renameat2(parent, own['temporary'], parent, own['descriptor'], NOREPLACE)
            except OSError as error:
                remove_own_records(parent, [own['temporary']])
                if error.errno == errno.EEXIST:
                    raise Refusal('integration plan is stale', source)
                raise
            os.fsync(parent)
            os.mkdir(os.path.basename(own['stage']), 0o700, dir_fd=stage_parent)
            os.fsync(stage_parent)
            stage = open_child(stage_parent, os.path.basename(own['stage']))
        finally:
            os.close(stage_parent)
        skipped = []
        try:
            source_fd = open_child(parent, base, source_st)
            try:
                copies, counter = {}, iter(range(1 << 62))
                stage_pass(source_fd, source, stage, own['copy'], copies, counter)
                # The one catch-up pass recopies what changed, under new names.
                tree = stage_pass(source_fd, source, stage, own['copy'], copies, counter)
            finally:
                os.close(source_fd)
            os.fsync(stage)
            publish(tree, copies, stage, target, stat.S_IMODE(source_st.st_mode), skipped)
        finally:
            os.close(stage)
        # Commit point: the prepared link takes S's name, the old directory B's.
        os.symlink(target, own['backup'], dir_fd=parent)
        os.fsync(parent)
        now = lstat(parent, base)
        if kind(now) != 'dir' or identity(now) != identity(source_st):
            raise Refusal('integration plan is stale', source)
        renameat2(parent, own['backup'], parent, base, EXCHANGE)
        os.fsync(parent)
        record = {'id': transfer_id, 'source': source, 'target': target, 'root': identity(source_st), 'names': own}
        return skipped, finish(parent, record)
    finally:
        os.close(parent)


def finish(parent, record, retire=False):
    """Idempotent, best-effort completion of a committed switch: the stage and
    the backup are cleaned independently, then the descriptor is removed even
    when remnants stay. With retire (the integration is deselected), our own
    link S -> recorded T is removed while the descriptor still names it; the
    artifact inventory never recorded a link whose transfer was interrupted.
    Returns the retained paths."""
    own, retained = record['names'], []
    if attempt(discard_stage, own['stage'], own['copy']):
        retained.append(own['stage'])
    prepared = lstat(parent, own['backup'])
    if kind(prepared) == 'link' and readlink(parent, own['backup']) == record['target']:
        # A link prepared before an exchange that never happened.
        remove_own_records(parent, [own['backup']])
    if attempt(delete_backup, parent, own['backup'], record['root']):
        retained.append(os.path.dirname(record['source']) + '/' + own['backup'])
    base = os.path.basename(record['source'])
    link = lstat(parent, base)
    own_link = retire and kind(link) == 'link' and readlink(parent, base) == record['target']
    remove_own_records(parent, ([base] if own_link else []) + [own['descriptor']])
    return retained


def attempt(cleanup, *arguments):
    """Run one cleanup; an OS error retains (and reports) what it could not remove."""
    try:
        return cleanup(*arguments)
    except OSError:
        return True


def cancel(parent, record):
    """Abandon an interrupted transfer before its exchange: S is untouched and
    copies already published to T stay there."""
    own = record['names']
    retained = [own['stage']] if attempt(discard_stage, own['stage'], own['copy']) else []
    prepared = lstat(parent, own['backup'])
    stale = [own['temporary'], own['descriptor']]
    if kind(prepared) == 'link' and readlink(parent, own['backup']) == record['target']:
        stale.insert(0, own['backup'])
    remove_own_records(parent, stale)
    return retained


def apply(request, entries):
    yard, problems, results = request['yard'], [], []
    by_source = {m['source']: m for m in request['mappings']}
    for entry in entries:
        mapping = by_source[entry['source']]
        outcome = {'source': entry['source'], 'not_transferred': [], 'retained': []}
        try:
            for op in entry['ops']:
                parent = open_path(os.path.dirname(mapping['source']))
                try:
                    source_st = lstat(parent, os.path.basename(mapping['source']))
                    state, record = classify(parent, mapping, yard, source_st)
                    if op == 'finish' and state == 'exchanged':
                        outcome['retained'] += finish(parent, record, retire=not mapping['selected'])
                    elif op == 'cancel' and state == 'active':
                        outcome['retained'] += cancel(parent, record)
                    elif op == 'adopt':
                        if state != 'none':
                            raise Refusal('integration plan is stale', mapping['source'])
                        skipped, retained = adopt(mapping, entry, yard)
                        outcome['not_transferred'] += skipped
                        outcome['retained'] += retained
                finally:
                    os.close(parent)
        except Refusals as error:
            problems.extend(error.items)
        except Refusal as error:
            problems.append(error)
        except OSError as error:
            problems.append(filesystem_refusal(error, mapping['source']))
        for problem in problems:
            problem.integration = problem.integration or mapping['integration']
        if outcome['not_transferred'] or outcome['retained']:
            results.append(outcome)
    if problems:
        raise Refusals(problems)
    return results


# ---------------------------------------------------------------- retained

def tree_size(parent, name):
    """Bytes below one entry without following links or crossing mounts; None if unknown."""
    st = lstat(parent, name)
    if kind(st) != 'dir':
        return st.st_size if st is not None else None
    try:
        fd = open_child(parent, name, st)
    except (OSError, Refusal):
        return None
    try:
        total = 0
        for child in os.listdir(fd):
            size = tree_size(fd, child)
            if size is None:
                return None
            total += size
        return total
    except OSError:
        return None
    finally:
        os.close(fd)


def retained(request):
    """Read-only listing of backups and stages that no current transfer names.
    Mappings can share a target parent, so the names of every mapping's current
    transfer are collected before anything is listed."""
    yard, found, named = request['yard'], {}, set()
    mappings = sorted(request['mappings'], key=lambda m: m['source'])
    for mapping in mappings:
        try:
            parent = open_path(os.path.dirname(mapping['source']))
            try:
                record = read_descriptor(parent, mapping, yard)
            finally:
                os.close(parent)
            if record:
                named |= {os.path.dirname(mapping['source']) + '/' + record['names']['backup'], record['names']['stage']}
        except (OSError, Refusal):
            pass
    for mapping in mappings:
        directories = [(os.path.dirname(mapping['source']), os.path.basename(mapping['source']) + '.subyard-backup-', 'backup'),
                       (os.path.dirname(mapping['target']), '.subyard-stage-' + yard + '-', 'stage')]
        for directory, prefix, label in directories:
            try:
                parent = open_path(directory)
            except OSError:
                continue
            try:
                for name in sorted(os.listdir(parent)):
                    path = directory + '/' + name
                    if not name.startswith(prefix) or path in named or path in found:
                        continue
                    found[path] = {'integration': mapping['integration'], 'kind': label, 'path': path,
                                   'bytes': tree_size(parent, name)}
            except OSError:
                pass
            finally:
                os.close(parent)
    return [found[p] for p in sorted(found)]


# ---------------------------------------------------------------- main

def fingerprint(request, entries, start):
    return digest([request['yard'], UID, GID, start, [
        [e['integration'], e['source'], e['target'], e['selected'], e['ops'], e['id'], e['state'], e['persist'],
         e['identity'], e['target_identity'], e['existing'], e['existing_count']] for e in entries]])


def drop_privileges(request):
    global UID, GID
    account = pwd.getpwnam(request['developer'])
    if account.pw_uid != request['uid']:
        raise ValueError('developer account does not match configured uid')
    UID, GID = account.pw_uid, grp.getgrnam(request['developer']).gr_gid
    if os.getuid() == 0:
        os.setgroups([GID])
        os.setgid(GID)
        os.setuid(UID)
    elif os.getuid() != UID:
        raise ValueError('persist transfer must run as the developer')
    os.umask(0o077)


def main():
    mode = sys.argv[1]
    request = json.load(sys.stdin)
    drop_privileges(request)
    for mapping in request['mappings']:
        for field in ('source', 'target'):
            if not os.path.isabs(mapping[field]) or os.path.normpath(mapping[field]) != mapping[field]:
                raise ValueError('invalid persist mapping')
    start = request.get('start') is True
    if mode in ('observe', 'apply'):
        entries, problems = observe(request, start)
        if problems:
            report(problems, entries)
        value = fingerprint(request, entries, start)
        result = {'fingerprint': value, 'entries': entries}
        if mode == 'apply':
            if value != request.get('expected'):
                raise Refusal('integration plan is stale', '')
            result['results'] = apply(request, [e for e in entries if e['ops']])
        print(json.dumps(result))
    elif mode == 'retained':
        print(json.dumps({'retained': retained(request)}))
    else:
        raise ValueError('invalid persist operation')


def report(items, entries=None):
    print(json.dumps({'conflicts': [{'reason': i.reason, 'path': i.path, 'detail': i.detail,
                                     'integration': i.integration} for i in items],
                      'entries': entries or []}), file=sys.stderr)
    sys.exit(1)


UID = GID = -1
if __name__ == '__main__':
    try:
        main()
    except Refusals as error:
        report(error.items)
    except Refusal as error:
        report([error])
    except OSError as error:
        report([filesystem_refusal(error, '')])
    except Exception as error:
        print(json.dumps({'error': type(error).__name__}), file=sys.stderr)
        sys.exit(1)
