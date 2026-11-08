# Bounded exact-root metadata publication. Invoked only by the native init plan.
import errno, hashlib, json, os, stat, sys

request = json.load(sys.stdin)
uid = request['uid']
gid = os.getgid()
base = '/srv/workspaces'
active_target = -1

class Refusal(Exception):
    def __init__(self, reason):
        self.reason = reason

def indexed_inspect(index):
    global active_target
    active_target = index
    return inspect(request['targets'][index])

def directory(path):
    fd = os.open('/', os.O_RDONLY | os.O_DIRECTORY)
    try:
        for part in path.strip('/').split('/'):
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = child
        return fd
    except BaseException:
        os.close(fd)
        raise

def fact(s):
    return [s.st_mode, s.st_uid, s.st_gid, s.st_dev, s.st_ino, s.st_size, s.st_mtime_ns, s.st_ctime_ns]

def project_facts():
    paths = {}
    fd = directory(base)
    try:
        for name in os.listdir(fd):
            path = base + '/' + name
            s = os.stat(name, dir_fd=fd, follow_symlinks=False)
            paths[path] = fact(s)
            if stat.S_ISLNK(s.st_mode):
                paths[path].append(os.readlink(name, dir_fd=fd))
            if stat.S_ISDIR(s.st_mode):
                child = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
                try:
                    for leaf in os.listdir(child):
                        p = path + '/' + leaf
                        ls = os.stat(leaf, dir_fd=child, follow_symlinks=False)
                        paths[p] = fact(ls)
                        if stat.S_ISLNK(ls.st_mode):
                            paths[p].append(os.readlink(leaf, dir_fd=child))
                        if len(paths) > 4096:
                            raise Refusal('scope_limit')
                finally:
                    os.close(child)
    finally:
        os.close(fd)
    return paths

def project_digest(paths):
    h = hashlib.sha256()
    for p, f in sorted(paths.items()):
        h.update(p.encode() + b'\0')
        h.update(str(tuple(f[:8])).encode() + b'\0')
        if len(f) > 8: h.update(f[8].encode())
    return h.hexdigest()

def inspect(target):
    root, desired = target['root'], json.loads(target['payload'])
    if root != base + '/' + desired['projectId']:
        raise Refusal('unsafe_path')
    try: fd = directory(root)
    except FileNotFoundError: raise Refusal('missing_root')
    except OSError as error:
        if error.errno in (errno.ELOOP, errno.ENOTDIR): raise Refusal('unsafe_root_path')
        raise
    try:
        rs = os.fstat(fd)
        # Historical bind attachment let Incus create a root-owned wrapper
        # and can expose a root-owned source with the developer's mapped group.
        # Only authoritative bind targets admit this layout; never normalize it.
        def legacy_bind(s):
            return desired['mode'] == 'bind' and s.st_uid == 0 and s.st_gid == gid and not s.st_mode & 0o002
        if rs.st_uid != uid and not legacy_bind(rs):
            raise Refusal('unsafe_root')
        repair_root = rs.st_uid == uid and not legacy_bind(rs) and bool(rs.st_mode & 0o022)
        try: src = os.open('src', os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
        except FileNotFoundError: raise Refusal('missing_source')
        except OSError as error:
            if error.errno in (errno.ELOOP, errno.ENOTDIR): raise Refusal('unsafe_source_path')
            raise
        try:
            ss = os.fstat(src)
            if (ss.st_uid != uid and not legacy_bind(ss)) or (legacy_bind(rs) and (ss.st_gid != gid or ss.st_mode & 0o002)):
                raise Refusal('unsafe_source')
        finally: os.close(src)
        try: meta = os.open('.subyard-meta.json', os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=fd)
        except FileNotFoundError:
            return {'root': fact(rs), 'src': fact(ss), 'metadata': None, 'repairRoot': repair_root, 'repairMetadata': ''}
        except OSError as error:
            if error.errno == errno.ELOOP: raise Refusal('unsafe_metadata_path')
            raise
        try:
            ms = os.fstat(meta)
            if not stat.S_ISREG(ms.st_mode): raise Refusal('metadata_type')
            if ms.st_uid != uid: raise Refusal('metadata_owner')
            if ms.st_nlink != 1: raise Refusal('metadata_links')
            if ms.st_size > 65536: raise Refusal('metadata_size')
            with os.fdopen(os.dup(meta), 'rb') as stream: payload = stream.read(65537)
            try: existing = json.loads(payload)
            except (ValueError, UnicodeError, RecursionError): raise Refusal('invalid_metadata')
            if not isinstance(existing, dict) or type(existing.get('schema')) is not int:
                raise Refusal('invalid_metadata')
            if 'identityVersion' in existing and (type(existing['identityVersion']) is not int or existing['identityVersion'] not in (0, 2)):
                raise Refusal('invalid_metadata')
            for key in ('schema', 'projectId', 'name', 'mode'):
                if existing.get(key) != desired.get(key): raise Refusal('conflicting_metadata')
            # Absent legacy soft fields stay preserved; only present stale values are rewritten.
            rewrite = any(existing.get(key) not in (None, '', 0) and existing.get(key) != desired.get(key)
                          for key in ('yard', 'identityVersion', 'target', 'importedAt'))
            repair = 'rewrite' if rewrite else 'mode' if ms.st_mode & 0o022 else ''
            return {'root': fact(rs), 'src': fact(ss), 'metadata': [fact(ms), hashlib.sha256(payload).hexdigest()], 'repairRoot': repair_root, 'repairMetadata': repair}
        finally: os.close(meta)
    finally: os.close(fd)

def main():
    global active_target
    before = project_facts()
    observed, refusals = [], []
    for i in range(len(request['targets'])):
        try:
            observed.append(indexed_inspect(i))
        except (Refusal, OSError) as error:
            reason = failure_reason(error)
            if request['mode'] != 'inspect' or reason in ('io_failure', 'scope_limit'):
                raise
            refusals.append({'reason': reason, 'target': i, 'stale': False})
    if refusals:
        print(json.dumps({'errors': refusals}, separators=(',', ':')))
        sys.exit(1)
    if request['mode'] != 'inspect':
        for i, (actual, expected) in enumerate(zip(observed, request['expected'])):
            if actual != expected:
                active_target = i
                raise Refusal('stale_inputs')
        active_target = -1
        if len(observed) != len(request['expected']) or project_digest(before) != request['projects']:
            raise Refusal('scope_changed')
    if request['mode'] == 'apply':
        changed = set()
        for i, (target, expected) in enumerate(zip(request['targets'], observed)):
            active_target = i
            if expected['metadata'] is not None and not expected['repairRoot'] and not expected['repairMetadata']: continue
            root = target['root']
            fd = directory(root)
            tmp = None
            meta = None
            try:
                if fact(os.fstat(fd)) != expected['root']: raise Refusal('stale_inputs')
                if inspect(target) != expected: raise Refusal('stale_inputs')
                if expected['metadata'] is not None:
                    meta = os.open('.subyard-meta.json', os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=fd)
                if expected['repairRoot']:
                    if fact(os.fstat(fd)) != expected['root']: raise Refusal('stale_inputs')
                    os.fchmod(fd, stat.S_IMODE(expected['root'][0]) & ~0o022)
                    os.fsync(fd)
                if expected['metadata'] is not None and expected['repairMetadata'] != 'rewrite':
                    if expected['repairMetadata'] == 'mode':
                        if fact(os.fstat(meta)) != expected['metadata'][0] or os.fstat(meta).st_nlink != 1 or fact(os.stat('.subyard-meta.json', dir_fd=fd, follow_symlinks=False)) != expected['metadata'][0]:
                            raise Refusal('stale_inputs')
                        os.fchmod(meta, 0o600)
                        os.fsync(meta)
                    verified = inspect(target)
                    if verified['repairRoot'] or verified['repairMetadata'] or verified['metadata'] is None or verified['metadata'][1] != expected['metadata'][1]:
                        raise Refusal('verification_failed')
                    changed.add(root)
                    continue
                # Create in the held directory; replace only the captured stale soft fields.
                tmp = '.subyard-meta-' + os.urandom(12).hex()
                out = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=fd)
                try:
                    os.fchmod(out, 0o600)
                    if os.getuid() != uid:
                        os.fchown(out, uid, gid)
                    with os.fdopen(os.dup(out), 'wb') as stream:
                        stream.write(target['payload'].encode()); stream.flush()
                    os.fsync(out)
                finally: os.close(out)
                if meta is None:
                    os.link(tmp, '.subyard-meta.json', src_dir_fd=fd, dst_dir_fd=fd, follow_symlinks=False)
                    os.unlink(tmp, dir_fd=fd)
                else:
                    if fact(os.fstat(meta)) != expected['metadata'][0] or os.fstat(meta).st_nlink != 1 or fact(os.stat('.subyard-meta.json', dir_fd=fd, follow_symlinks=False)) != expected['metadata'][0]:
                        raise Refusal('stale_inputs')
                    os.rename(tmp, '.subyard-meta.json', src_dir_fd=fd, dst_dir_fd=fd)
                tmp = None
                os.fsync(fd)
                verified = inspect(target)['metadata']
                if verified is None or verified[1] != hashlib.sha256(target['payload'].encode()).hexdigest():
                    raise Refusal('verification_failed')
                changed.add(root)
            finally:
                if tmp is not None: os.unlink(tmp, dir_fd=fd)
                if meta is not None: os.close(meta)
                os.close(fd)
        active_target = -1
        after = project_facts()
        # Publication changes only the known parent directory and its metadata entry.
        old_unmodified = {p:f for p,f in before.items() if p not in changed and p not in {r+'/.subyard-meta.json' for r in changed}}
        new_unmodified = {p:f for p,f in after.items() if p not in changed and p not in {r+'/.subyard-meta.json' for r in changed}}
        if old_unmodified != new_unmodified: raise Refusal('scope_changed')
        for root in changed:
            original = before[root][:5]
            if next(e['repairRoot'] for t, e in zip(request['targets'], observed) if t['root'] == root):
                original = [original[0] & ~0o022] + original[1:]
            if original != after[root][:5]: raise Refusal('stale_inputs')
        before = after
        observed = [indexed_inspect(i) for i in range(len(request['targets']))]
    print(json.dumps({'expected': observed, 'projects': project_digest(before)}, separators=(',', ':')))

def failure_reason(error):
    if isinstance(error, Refusal):
        return error.reason
    elif error.errno in (errno.ELOOP, errno.ENOTDIR):
        return 'unsafe_path'
    elif error.errno == errno.ENOENT and request['mode'] == 'apply':
        return 'stale_inputs'
    elif error.errno == errno.EEXIST:
        return 'stale_inputs'
    elif error.errno in (errno.EACCES, errno.EPERM):
        return 'access_denied'
    return 'io_failure'

try:
    main()
except (Refusal, OSError) as error:
    reason = failure_reason(error)
    stale = reason in ('stale_inputs', 'scope_changed') or (request['mode'] == 'apply' and reason in ('missing_root', 'missing_source', 'unsafe_path', 'unsafe_root_path', 'unsafe_source_path', 'unsafe_metadata_path', 'unsafe_root', 'unsafe_source', 'access_denied', 'metadata_type', 'metadata_owner', 'metadata_links', 'metadata_size', 'invalid_metadata', 'conflicting_metadata'))
    print(json.dumps({'error': {'reason': reason, 'target': active_target, 'stale': stale}}, separators=(',', ':')))
    sys.exit(1)
