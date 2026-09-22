# Version 1 records observed ownership only. Desired selection remains in yard config.
import base64
import hashlib
import json
import os
import pwd
import grp
import stat
import sys
import secrets

ROOT = '@STATE_ROOT@'
OWNER = int('@STATE_UID@')

class OwnershipConflict(Exception):
    def __init__(self, reason, path):
        self.reason = reason
        self.path = path

def digest(value):
    return hashlib.sha256(value).hexdigest()

def encode(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':')).encode()

def protected(path, directory=False):
    st = os.lstat(path)
    if st.st_uid != OWNER or st.st_mode & 0o022 or (not stat.S_ISDIR(st.st_mode) if directory else not stat.S_ISREG(st.st_mode)):
        raise ValueError('unsafe inventory')

def ancestors(path):
    parent = os.path.dirname(path)
    while parent != '/':
        if os.path.lexists(parent) and (os.path.islink(parent) or not os.path.isdir(parent)):
            raise ValueError('unsafe artifact ancestor')
        parent = os.path.dirname(parent)

def actual(entry):
    if entry['kind'] == 'package':
        return None
    path = entry['path']
    ancestors(path)
    if not os.path.lexists(path):
        return None
    st = os.lstat(path)
    if entry['kind'] == 'link':
        return digest(os.readlink(path).encode()) if stat.S_ISLNK(st.st_mode) else 'foreign'
    if not stat.S_ISREG(st.st_mode):
        return 'foreign'
    with open(path, 'rb') as stream:
        return digest(stream.read())

def actual_metadata(entry):
    if entry['kind'] not in ('file', 'link') or not os.path.lexists(entry['path']):
        return None
    st = os.lstat(entry['path'])
    mode = stat.S_IMODE(st.st_mode) if entry['kind'] == 'file' else None
    return [st.st_uid, st.st_gid, mode]

def expected_metadata(entry, home, uid):
    owner = uid if entry['path'].startswith(home + '/') else OWNER
    mode = entry.get('mode', 0o644) if entry['kind'] == 'file' else None
    group = entry.get('gid', owner) if entry['kind'] == 'link' else owner
    return [owner, group, mode]

def adoptable_selected(entry, current, home, uid):
    return (entry['kind'] in ('file', 'link') and current == entry['digest']
            and actual_metadata(entry) == expected_metadata(entry, home, uid))

def clean(entry):
    return {k: v for k, v in entry.items() if k != 'content'}

def adoptable_core(entry, current):
    # These shared core files predate the integration inventory. Recognize only
    # their exact desired bytes and protected system metadata, never agent files.
    modes = {'/usr/local/libexec/subyard/projects-changed': 0o755,
             '/etc/subyard/agent-project-hooks': 0o644}
    mode = modes.get(entry.get('path'))
    predecessor_digests = {
        '/usr/local/libexec/subyard/projects-changed': {
            'cefded0322e335042ff9a0e74f2cba187fb1a2a6aa8a2ffbcdf249ecc8e12588'
        }
    }
    accepted_digests = {entry['digest']} | predecessor_digests.get(entry.get('path'), set())
    if (entry['id'] != '_projects' or entry['kind'] != 'file' or mode is None
            or entry.get('mode', 0o644) != mode or current not in accepted_digests):
        return False
    st = os.lstat(entry['path'])
    if (not stat.S_ISREG(st.st_mode) or st.st_uid != OWNER or st.st_gid != OWNER
            or stat.S_IMODE(st.st_mode) != mode):
        return False
    parent = os.path.dirname(entry['path'])
    while parent != '/':
        st = os.lstat(parent)
        if (not stat.S_ISDIR(st.st_mode) or st.st_uid != OWNER or st.st_gid != OWNER
                or st.st_mode & 0o022):
            return False
        parent = os.path.dirname(parent)
    return True

def open_parent(path):
    descriptor = os.open('/', os.O_RDONLY | os.O_DIRECTORY)
    try:
        for part in os.path.dirname(path).strip('/').split('/'):
            if not part:
                continue
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=descriptor)
            os.close(descriptor)
            descriptor = child
        return descriptor
    except Exception:
        os.close(descriptor)
        raise

def atomic(path, payload, mode=0o600, uid=OWNER):
    parent = open_parent(path)
    temporary = '.integration-' + secrets.token_hex(12)
    try:
        fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=parent)
        with os.fdopen(fd, 'wb') as stream:
            os.fchmod(stream.fileno(), mode)
            os.fchown(stream.fileno(), uid, uid)
            stream.write(payload)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, os.path.basename(path), src_dir_fd=parent, dst_dir_fd=parent)
        os.fsync(parent)
    finally:
        try:
            os.unlink(temporary, dir_fd=parent)
        except FileNotFoundError:
            pass
        os.close(parent)

def mkdir_owned(path, uid, gid=None):
    missing = []
    current = path
    while not os.path.exists(current):
        missing.append(current)
        current = os.path.dirname(current)
    for directory in reversed(missing):
        os.mkdir(directory, 0o755)
        os.chown(directory, uid, uid if gid is None else gid)

def main():
    mode = sys.argv[1]
    request = json.load(sys.stdin)
    home = request['home']
    uid = request['uid']
    desired = request['entries'] or []
    if any(entry['kind'] == 'link' for entry in desired):
        developer = request['developer']
        if pwd.getpwnam(developer).pw_uid != uid:
            raise ValueError('developer account does not match configured uid')
        # Historical provisioning uses chown DEV_USER:DEV_USER for links.
        # A named group's numeric ID need not equal its user's pinned UID.
        link_gid = grp.getgrnam(developer).gr_gid
        for entry in desired:
            if entry['kind'] == 'link':
                entry['gid'] = link_gid
    adopt = request.get('adopt') is True
    path = ROOT + '/inventory.json'
    records = []
    released = []
    established = False
    if os.path.lexists(ROOT):
        ancestors(ROOT)
        protected(ROOT, True)
    if os.path.lexists(path):
        protected(path)
        with open(path, 'rb') as stream:
            inventory = json.load(stream)
        if inventory.get('schema') != 1 or not isinstance(inventory.get('entries'), list):
            raise ValueError('unsupported inventory')
        records = inventory['entries']
        released = inventory.get('released', [])
        if not isinstance(released, list) or any(not isinstance(p, str) or not p.startswith(home + '/') or os.path.normpath(p) != p for p in released):
            raise ValueError('invalid released artifact evidence')
        established = True
    for entry in records + desired:
        if entry['kind'] not in ('file', 'link', 'structured', 'package') or not entry['id']:
            raise ValueError('invalid inventory entry')
        if entry['kind'] != 'package' and (not entry['path'].startswith(home + '/') and entry['path'] not in ('/usr/local/libexec/subyard/projects-changed', '/etc/subyard/agent-project-hooks') or os.path.normpath(entry['path']) != entry['path']):
            raise ValueError('invalid artifact destination')
    def key(entry):
        return entry['kind'] + ':' + entry.get('path', entry['id'])
    previous = {key(e): e for e in records}
    wanted = {key(e): e for e in desired}
    if len(previous) != len(records) or len(wanted) != len(desired):
        raise ValueError('duplicate inventory entry')
    observed = {}
    metadata = {}
    changed = records != [clean(e) for e in desired]
    retired = []
    adopted = []
    for name, entry in previous.items():
        current = actual(entry)
        observed[name] = current
        current_metadata = actual_metadata(entry)
        if current_metadata is not None:
            metadata[name] = current_metadata
        if entry['kind'] in ('file', 'link') and current not in (None, entry['digest'], entry.get('pending_digest')):
            raise OwnershipConflict('owned artifact drift', entry['path'])
        if name not in wanted:
            if (entry['kind'] in ('file', 'link') and current is not None
                    and current_metadata != expected_metadata(entry, home, uid)):
                raise OwnershipConflict('owned artifact drift', entry['path'])
            retired.append(entry)
    for name, entry in wanted.items():
        current = actual(entry)
        observed[name] = current
        if entry['kind'] == 'link':
            mount = '/' + '/'.join(entry['target'].strip('/').split('/')[:3])
            if not os.path.isdir(mount):
                raise ValueError('session mount is unavailable')
        if entry['kind'] in ('file', 'link'):
            if name not in previous and current is not None:
                if adoptable_core(entry, current):
                    adopted.append(entry)
                elif adopt and not established and adoptable_selected(entry, current, home, uid):
                    adopted.append(entry)
                else:
                    raise OwnershipConflict('unowned selected artifact', entry['path'])
            changed |= current != entry['digest']
            if current is not None:
                metadata[name] = actual_metadata(entry)
                changed |= metadata[name] != expected_metadata(entry, home, uid)
    # Initial adoption cannot silently claim legacy wiring. Once protected evidence
    # exists, new unrecorded paths belong to their creators, not this inventory.
    for candidate in request.get('legacy', []) if not established else []:
        if candidate not in released and candidate not in [e.get('path') for e in records + desired] and os.path.lexists(candidate):
            raise OwnershipConflict('legacy integration artifact has no ownership evidence', candidate)
    adoption = [[clean(entry), metadata[key(entry)]] for entry in adopted]
    fingerprint = digest(encode([home, uid, records, released, observed, metadata,
                                 [clean(e) for e in desired], not established, adopt, adoption]))
    expected = request.get('expected')
    if expected and fingerprint != expected:
        raise ValueError('integration plan is stale')
    if mode == 'observe':
        print(json.dumps({'fingerprint': fingerprint, 'changed': changed, 'retired': retired,
                          'adopted': [clean(e) for e in adopted], 'initial': not established}))
        return
    if mode not in ('apply', 'commit'):
        raise ValueError('invalid inventory operation')
    for entry in adopted:
        current = actual(entry)
        if not (adoptable_core(entry, current)
                or adopt and not established and adoptable_selected(entry, current, home, uid)):
            raise OwnershipConflict('unowned selected artifact', entry['path'])
    os.makedirs(ROOT, mode=0o700, exist_ok=True)
    protected(ROOT, True)
    def save(entries):
        atomic(path, encode({'schema': 1, 'entries': entries, 'released': sorted(set(released))}))
    if mode == 'commit':
        # Structured retirement releases fields, preserving the user's document.
        # Tombstones prove why these known paths are no longer owned.
        released += [e['path'] for e in retired if e['kind'] == 'structured']
        released = [p for p in released if p not in [e.get('path') for e in desired]]
        save([clean(e) for e in desired])
        return
    # Publish intent before file replacement; both old and candidate digests permit retry.
    pending = dict(previous)
    for name, entry in wanted.items():
        if entry['kind'] == 'package':
            continue
        candidate = dict(clean(entry), pending_digest=entry['digest'])
        if name in previous:
            candidate = dict(previous[name], pending_digest=entry['digest'])
        elif observed[name] is not None:
            candidate['digest'] = observed[name]
        pending[name] = candidate
    save(list(pending.values()))
    for entry in retired:
        if entry['kind'] in ('file', 'link') and os.path.lexists(entry['path']):
            parent = open_parent(entry['path'])
            try:
                if (actual(entry) not in (entry['digest'], entry.get('pending_digest'))
                        or actual_metadata(entry) != expected_metadata(entry, home, uid)):
                    raise OwnershipConflict('owned artifact drift', entry['path'])
                os.unlink(os.path.basename(entry['path']), dir_fd=parent)
            finally:
                os.close(parent)
    for name, entry in wanted.items():
        if entry.get('path') == '/usr/local/libexec/subyard/projects-changed':
            mkdir_owned(entry['path'] + '.d', OWNER)
        if entry['kind'] not in ('file', 'link'):
            continue
        if observed[name] == entry['digest'] and metadata.get(name) == expected_metadata(entry, home, uid):
            continue
        destination = entry['path']
        parent = os.path.dirname(destination)
        owner = uid if destination.startswith(home + '/') else OWNER
        mkdir_owned(parent, owner, entry.get('gid') if entry['kind'] == 'link' else None)
        if destination == '/usr/local/libexec/subyard/projects-changed':
            mkdir_owned(destination + '.d', OWNER)
        if entry['kind'] == 'file':
            payload = base64.b64decode(entry['content'], validate=True)
            if digest(payload) != entry['digest']:
                raise ValueError('invalid artifact payload')
            if actual(entry) != observed[name]:
                raise ValueError('concurrent artifact change')
            atomic(destination, payload, entry.get('mode', 0o644), owner)
        else:
            target = entry['target']
            mount = '/' + '/'.join(target.strip('/').split('/')[:3])
            if not os.path.isdir(mount):
                raise ValueError('session mount is unavailable')
            target_dir = os.path.dirname(target) if entry.get('file_target') else target
            ancestors(target_dir)
            if os.path.islink(target_dir):
                raise ValueError('session target is a symlink')
            mkdir_owned(target_dir, uid, entry['gid'])
            parent_fd = open_parent(destination)
            temporary = '.integration-' + secrets.token_hex(12)
            try:
                os.symlink(target, temporary, dir_fd=parent_fd)
                os.chown(temporary, uid, entry['gid'], dir_fd=parent_fd, follow_symlinks=False)
                if actual(entry) != observed[name] or actual_metadata(entry) != metadata.get(name):
                    raise OwnershipConflict('concurrent artifact change', destination)
                os.replace(temporary, os.path.basename(destination), src_dir_fd=parent_fd, dst_dir_fd=parent_fd)
                os.fsync(parent_fd)
            finally:
                try:
                    os.unlink(temporary, dir_fd=parent_fd)
                except FileNotFoundError:
                    pass
                os.close(parent_fd)

try:
    main()
except OwnershipConflict as error:
    # Only a fixed category and destination are emitted, never document contents.
    print(json.dumps({'reason': error.reason, 'path': error.path}), file=sys.stderr)
    sys.exit(1)
except Exception:
    print('integration ownership conflict or unavailable evidence', file=sys.stderr)
    sys.exit(1)
