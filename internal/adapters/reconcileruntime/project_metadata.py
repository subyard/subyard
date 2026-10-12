# Bounded exact-root metadata publication. Invoked only by the native init plan.
import errno, hashlib, json, os, stat, sys

request = json.load(sys.stdin)
uid = os.getuid()
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
    try:
        rs = os.fstat(fd)
        if rs.st_uid != uid or rs.st_mode & 0o022:
            raise Refusal('unsafe_root')
        try: src = os.open('src', os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
        except FileNotFoundError: raise Refusal('missing_source')
        try:
            ss = os.fstat(src)
            if ss.st_uid != uid:
                raise Refusal('unsafe_source')
        finally: os.close(src)
        try: meta = os.open('.subyard-meta.json', os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=fd)
        except FileNotFoundError:
            return {'root': fact(rs), 'src': fact(ss), 'metadata': None}
        try:
            ms = os.fstat(meta)
            if not stat.S_ISREG(ms.st_mode) or ms.st_uid != uid or ms.st_nlink != 1 or ms.st_mode & 0o022 or ms.st_size > 65536:
                raise Refusal('unsafe_metadata')
            with os.fdopen(os.dup(meta), 'rb') as stream: payload = stream.read(65537)
            try: existing = json.loads(payload)
            except (ValueError, UnicodeError): raise Refusal('invalid_metadata')
            if not isinstance(existing, dict) or type(existing.get('schema')) is not int:
                raise Refusal('invalid_metadata')
            if 'identityVersion' in existing and (type(existing['identityVersion']) is not int or existing['identityVersion'] not in (0, 2)):
                raise Refusal('invalid_metadata')
            for key in ('schema', 'projectId', 'name', 'mode'):
                if existing.get(key) != desired.get(key): raise Refusal('conflicting_metadata')
            for key in ('yard', 'identityVersion', 'target', 'importedAt'):
                if existing.get(key) not in (None, '', 0) and existing.get(key) != desired.get(key):
                    raise Refusal('conflicting_metadata')
            return {'root': fact(rs), 'src': fact(ss), 'metadata': [fact(ms), hashlib.sha256(payload).hexdigest()]}
        finally: os.close(meta)
    finally: os.close(fd)

def main():
    global active_target
    before = project_facts()
    observed = [indexed_inspect(i) for i in range(len(request['targets']))]
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
            if expected['metadata'] is not None: continue
            root = target['root']
            fd = directory(root)
            tmp = None
            try:
                if fact(os.fstat(fd)) != expected['root']: raise Refusal('stale_inputs')
                if inspect(target) != expected: raise Refusal('stale_inputs')
                # Create in the held directory, then link without replacing any existing path.
                tmp = '.subyard-meta-' + os.urandom(12).hex()
                out = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=fd)
                try:
                    os.fchmod(out, 0o600)
                    with os.fdopen(os.dup(out), 'wb') as stream:
                        stream.write(target['payload'].encode()); stream.flush()
                    os.fsync(out)
                finally: os.close(out)
                os.link(tmp, '.subyard-meta.json', src_dir_fd=fd, dst_dir_fd=fd, follow_symlinks=False)
                os.unlink(tmp, dir_fd=fd); tmp = None
                os.fsync(fd)
                verified = inspect(target)['metadata']
                if verified is None or verified[1] != hashlib.sha256(target['payload'].encode()).hexdigest():
                    raise Refusal('verification_failed')
                changed.add(root)
            finally:
                if tmp is not None: os.unlink(tmp, dir_fd=fd)
                os.close(fd)
        active_target = -1
        after = project_facts()
        # Publication changes only the known parent directory and its metadata entry.
        old_unmodified = {p:f for p,f in before.items() if p not in changed and p not in {r+'/.subyard-meta.json' for r in changed}}
        new_unmodified = {p:f for p,f in after.items() if p not in changed and p not in {r+'/.subyard-meta.json' for r in changed}}
        if old_unmodified != new_unmodified: raise Refusal('scope_changed')
        for root in changed:
            if before[root][:5] != after[root][:5]: raise Refusal('stale_inputs')
        before = after
        observed = [indexed_inspect(i) for i in range(len(request['targets']))]
    print(json.dumps({'expected': observed, 'projects': project_digest(before)}, separators=(',', ':')))

try:
    main()
except (Refusal, OSError) as error:
    if isinstance(error, Refusal):
        reason = error.reason
    elif error.errno in (errno.ELOOP, errno.ENOTDIR):
        reason = 'unsafe_path'
    elif error.errno == errno.ENOENT and request['mode'] == 'apply':
        reason = 'stale_inputs'
    elif error.errno == errno.EEXIST:
        reason = 'stale_inputs'
    else:
        reason = 'io_failure'
    stale = reason in ('stale_inputs', 'scope_changed') or (request['mode'] == 'apply' and reason in ('missing_root', 'missing_source', 'unsafe_path', 'unsafe_root', 'unsafe_source', 'unsafe_metadata', 'invalid_metadata', 'conflicting_metadata'))
    print(json.dumps({'error': {'reason': reason, 'target': active_target, 'stale': stale}}, separators=(',', ':')))
    sys.exit(1)
