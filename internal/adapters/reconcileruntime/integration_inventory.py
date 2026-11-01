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
import fcntl

ROOT = '@STATE_ROOT@'
OWNER = int('@STATE_UID@')

class OwnershipConflict(Exception):
    def __init__(self, reason, path, detail=''):
        self.reason = reason
        self.path = path
        self.detail = detail

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

def inventory_identity(entry, current):
    # Structured writers bind protected metadata and owned fields separately.
    # Other owners may change their fields without changing this inventory.
    if entry['kind'] == 'structured' and current not in (None, 'foreign'):
        return 'structured'
    return current

def expected_metadata(entry, home, uid):
    owner = uid if entry['path'].startswith(home + '/') else OWNER
    mode = entry.get('mode', 0o644) if entry['kind'] == 'file' else None
    group = entry.get('gid', owner) if entry['kind'] == 'link' else owner
    return [owner, group, mode]

def preview_migration_plan(records, home, uid):
    paths = {home + '/' + relative for relative in (
        '.claude/CLAUDE.md', '.codex/AGENTS.md', '.config/opencode/AGENTS.md')}
    accepted = {'9697d51bbecba09a3865819daf186615bea26a5291013143e1993394603053d0',
                '2de48bed4c984591e976f396432ff7758f4df3ca24e0b55fc90bf1fb0a5aeb48'}
    start, end = b'<!-- subyard-preview -->', b'<!-- /subyard-preview -->'
    result = []
    for record in records:
        if record['kind'] != 'file' or record.get('path') not in paths:
            continue
        current = actual(record)
        if current is None:
            continue
        if current == 'foreign':
            raise OwnershipConflict('owned artifact drift', record['path'])
        with open(record['path'], 'rb') as stream:
            payload = stream.read()
        if start not in payload and end not in payload:
            continue
        begin, finish = payload.find(start), payload.find(end)
        if (payload.count(start) != 1 or payload.count(end) != 1 or finish < begin
                or digest(payload[begin:finish + len(end)]) not in accepted):
            raise OwnershipConflict('owned artifact drift', record['path'],
                                    'preview instruction block requires inspection')
        if (current not in (record['digest'], record.get('pending_digest'), record.get('config_pending_digest'))
                or actual_metadata(record) != expected_metadata(record, home, uid)):
            raise OwnershipConflict('owned artifact drift', record['path'])
        result.append((record, current, payload[:begin] + payload[finish + len(end):]))
    return result


def apply_preview_migrations(migrations, inventory, inventory_path, uid):
    if not migrations:
        return
    # Publish retry identities before replacing any file. This is the same narrow
    # preview-only operation as guest static refresh, without skill installation.
    for record, before, payload in migrations:
        record['config_pending_digest'] = digest(payload)
        record['preview_migrated'] = True
    atomic(inventory_path, encode(inventory))
    for record, before, payload in migrations:
        if actual(record) != before:
            raise OwnershipConflict('owned artifact drift', record['path'])
        atomic(record['path'], payload, 0o644, uid)
        record['digest'] = digest(payload)
        record.pop('config_pending_digest', None)
    atomic(inventory_path, encode(inventory))


def adoptable_selected(entry, current, home, uid):
    accepted = {entry['digest']}
    # Instruction composition accepts the exact original host bytes only through
    # the existing explicit initial-adoption gate. This is input, not ownership.
    if entry['kind'] == 'file' and entry.get('source_digest'):
        accepted.add(entry['source_digest'])
    return (entry['kind'] in ('file', 'link') and current in accepted
            and actual_metadata(entry) == expected_metadata(entry, home, uid))

def clean(entry):
    return {k: v for k, v in entry.items() if k not in ('content', 'source_digest')}

def core_adoption_problem(entry, current):
    # These shared core files predate the integration inventory. Recognize only
    # their exact desired bytes and protected system metadata, never agent files.
    modes = {'/usr/local/libexec/subyard/projects-changed': 0o755,
             '/etc/subyard/agent-project-hooks': 0o644}
    mode = modes.get(entry.get('path'))
    predecessor_digests = {
        '/usr/local/libexec/subyard/projects-changed': {
            # Original installer heredoc, before shared resource hooks.
            'b632dd04a13ba11abff0b7502785ae09d8a6119c0c1261f338861796de82df35',
            'cefded0322e335042ff9a0e74f2cba187fb1a2a6aa8a2ffbcdf249ecc8e12588',
            '11af99ae9813d9ba1ee71c3d19eeb1457b4e6dae9c45e6f25b781b5ad69211d7'
        }
    }
    accepted_digests = {entry['digest']} | predecessor_digests.get(entry.get('path'), set())
    if (entry['id'] != '_projects' or entry['kind'] != 'file' or mode is None
            or entry.get('mode', 0o644) != mode):
        return 'not a shared core artifact'
    if current not in accepted_digests:
        return 'unrecognized core content'
    st = os.lstat(entry['path'])
    if (not stat.S_ISREG(st.st_mode) or st.st_uid != OWNER or st.st_gid != OWNER
            or stat.S_IMODE(st.st_mode) != mode):
        return 'unsafe core metadata'
    parent = os.path.dirname(entry['path'])
    while parent != '/':
        st = os.lstat(parent)
        if (not stat.S_ISDIR(st.st_mode) or st.st_uid != OWNER or st.st_gid != OWNER
                or st.st_mode & 0o022):
            return 'unsafe core ancestor'
        parent = os.path.dirname(parent)
    return ''

def adoptable_core(entry, current):
    return core_adoption_problem(entry, current) == ''

def unowned_selected(entry, current):
    return OwnershipConflict('unowned selected artifact', entry['path'],
                             core_adoption_problem(entry, current))

def open_parent(path, create=False, uid=OWNER):
    descriptor = os.open('/', os.O_RDONLY | os.O_DIRECTORY)
    try:
        for part in os.path.dirname(path).strip('/').split('/'):
            if not part:
                continue
            try:
                child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=descriptor)
            except FileNotFoundError:
                if not create:
                    raise
                os.mkdir(part, 0o755, dir_fd=descriptor)
                child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=descriptor)
                os.fchown(child, uid, uid)
                os.fchmod(child, 0o755)
            os.close(descriptor)
            descriptor = child
        return descriptor
    except Exception:
        os.close(descriptor)
        raise

def atomic(path, payload, mode=0o600, uid=OWNER, parent=None, expected=None, validate=None):
    owned_parent = parent is None
    if owned_parent:
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
        if expected is not None and config_state_at(parent, os.path.basename(path)) != expected:
            raise ValueError('concurrent config change')
        if validate is not None:
            validate()
        os.replace(temporary, os.path.basename(path), src_dir_fd=parent, dst_dir_fd=parent)
        os.fsync(parent)
    finally:
        try:
            os.unlink(temporary, dir_fd=parent)
        except FileNotFoundError:
            pass
        if owned_parent:
            os.close(parent)

def config_state_at(parent, name):
    try:
        fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=parent)
    except FileNotFoundError:
        return {'exists': False, 'digest': '', 'uid': 0, 'gid': 0, 'mode': 0}
    with os.fdopen(fd, 'rb') as stream:
        before = os.fstat(stream.fileno())
        if not stat.S_ISREG(before.st_mode):
            raise ValueError('invalid config destination')
        payload = stream.read((32 << 20) + 1)
        after = os.fstat(stream.fileno())
        identity = lambda st: (st.st_dev, st.st_ino, st.st_mode, st.st_uid, st.st_gid, st.st_size, st.st_mtime_ns, st.st_ctime_ns)
        if len(payload) > 32 << 20 or identity(before) != identity(after):
            raise ValueError('config observation changed or exceeds bound')
    return {'exists': True, 'digest': digest(payload), 'uid': before.st_uid,
            'gid': before.st_gid, 'mode': stat.S_IMODE(before.st_mode)}

def config_state(path):
    try:
        parent = open_parent(path)
    except FileNotFoundError:
        return {'exists': False, 'digest': '', 'uid': 0, 'gid': 0, 'mode': 0}
    try:
        return config_state_at(parent, os.path.basename(path))
    finally:
        os.close(parent)

def config_files(mode, request, home, uid, entries):
    if len(entries) > 2048 or any(e['kind'] not in ('file', 'structured') or not e['id']
            or not e['path'].startswith(home + '/') or os.path.normpath(e['path']) != e['path'] for e in entries):
        raise ValueError('invalid config file scope')
    if mode == 'configs-observe':
        print(json.dumps([config_state(e['path']) for e in entries]))
        return
    if len(entries) != 1 or entries[0]['kind'] != 'file':
        raise ValueError('invalid config write scope')
    entry = entries[0]
    payload = base64.b64decode(entry['content'], validate=True)
    if len(payload) > 32 << 20 or digest(payload) != entry['digest']:
        raise ValueError('invalid config payload')
    expected = request['before']
    desired = {'exists': True, 'digest': entry['digest'], 'uid': uid, 'gid': uid, 'mode': 0o644}
    # Check before creating a missing parent; foreign state is never repair work.
    current = config_state(entry['path'])
    if current not in (expected, desired):
        raise ValueError('config original state changed')
    parent = open_parent(entry['path'], True, uid)
    try:
        current = config_state_at(parent, os.path.basename(entry['path']))
        if current == desired:
            return
        if current != expected:
            raise ValueError('concurrent config change')
        atomic(entry['path'], payload, 0o644, uid, parent, current)
    finally:
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

def artifact_state_at(parent, name, kind):
    if kind != 'link':
        return config_state_at(parent, name)
    try:
        before = os.stat(name, dir_fd=parent, follow_symlinks=False)
    except FileNotFoundError:
        return {'exists': False, 'digest': '', 'uid': 0, 'gid': 0, 'mode': 0}
    if not stat.S_ISLNK(before.st_mode):
        raise ValueError('invalid integration link')
    value = os.readlink(name, dir_fd=parent)
    after = os.stat(name, dir_fd=parent, follow_symlinks=False)
    if (before.st_dev, before.st_ino, before.st_uid, before.st_gid, before.st_ctime_ns) != (after.st_dev, after.st_ino, after.st_uid, after.st_gid, after.st_ctime_ns):
        raise ValueError('concurrent integration link change')
    return {'exists': True, 'digest': digest(value.encode()), 'uid': before.st_uid, 'gid': before.st_gid, 'mode': 0}

def artifact_state(entry):
    if entry['kind'] == 'package':
        return {'exists': False, 'digest': '', 'uid': 0, 'gid': 0, 'mode': 0}
    try:
        parent = open_parent(entry['path'])
    except FileNotFoundError:
        return {'exists': False, 'digest': '', 'uid': 0, 'gid': 0, 'mode': 0}
    try:
        return artifact_state_at(parent, os.path.basename(entry['path']), entry['kind'])
    finally:
        os.close(parent)

def wanted_state(entry, home, uid):
    owner, group, mode = expected_metadata(entry, home, uid)
    return {'exists': True, 'digest': entry['digest'], 'uid': owner, 'gid': group,
            'mode': 0o644 if entry['kind'] == 'structured' else mode or 0}

def pending_records(previous, wanted, observed):
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
    return list(pending.values())

def committed_records(previous, desired):
    records = [clean(entry) for entry in desired]
    for entry in records:
        if previous.get(entry['kind'] + ':' + entry.get('path', entry['id']), {}).get('preview_migrated'):
            entry['preview_migrated'] = True
    return records

def receipt_bytes(entries, released):
    return encode({'schema': 1, 'entries': entries, 'released': sorted(set(released))})

def main(request=None, locked=False):
    mode = sys.argv[1]
    if request is None:
        request = json.load(sys.stdin)
    home = request['home']
    uid = request['uid']
    desired = request['entries'] or []
    guarded = request.get('after_owner')
    if mode in ('configs-observe', 'configs-write'):
        config_files(mode, request, home, uid, desired)
        return
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
    receipt = None
    released = []
    established = False
    if os.path.lexists(ROOT):
        ancestors(ROOT)
        protected(ROOT, True)
    if os.path.lexists(path):
        protected(path)
        if guarded is not None:
            descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
            with os.fdopen(descriptor, 'rb') as stream:
                info = os.fstat(stream.fileno())
                receipt = stream.read((32 << 20) + 1)
                if info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600 or len(receipt) > 32 << 20:
                    raise ValueError('unsafe original inventory receipt')
        else:
            with open(path, 'rb') as stream:
                receipt = stream.read()
        inventory = json.loads(receipt)
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
        source_digest = entry.get('source_digest')
        if source_digest is not None and (entry['kind'] != 'file'
                or not isinstance(source_digest, str) or len(source_digest) != 64
                or any(c not in '0123456789abcdef' for c in source_digest)):
            raise ValueError('invalid source evidence')
        if entry['kind'] != 'package' and (not entry['path'].startswith(home + '/') and entry['path'] not in ('/usr/local/libexec/subyard/projects-changed', '/etc/subyard/agent-project-hooks') or os.path.normpath(entry['path']) != entry['path']):
            raise ValueError('invalid artifact destination')
    def key(entry):
        return entry['kind'] + ':' + entry.get('path', entry['id'])
    previous = {key(e): e for e in records}
    wanted = {key(e): e for e in desired}
    if len(previous) != len(records) or len(wanted) != len(desired):
        raise ValueError('duplicate inventory entry')
    cutover = request.get('preview_skill_cutover') is True
    migrations = preview_migration_plan(records, home, uid) if cutover else []
    if mode in ('configs-prepare', 'configs-commit'):
        for entry in desired:
            if entry['kind'] not in ('file', 'structured') or not entry['path'].startswith(home + '/'):
                raise ValueError('invalid config inventory entry')
        # Config refresh acknowledges existing ownership only. It cannot adopt,
        # retire or finish a pending package/link/hook reconciliation.
        by_path = {e.get('path'): e for e in records if e.get('path')}
        # Refuse all unsafe config ownership before the preview-only writes.
        if migrations and mode == 'configs-prepare':
            for entry in desired:
                record = by_path.get(entry.get('path'))
                current = actual(entry)
                if record is None:
                    if current is not None:
                        raise unowned_selected(entry, current)
                elif (any(record.get(k) != entry.get(k) for k in ('id','kind','path','format'))
                      or current not in (None,record['digest'],record.get('pending_digest'),record.get('config_pending_digest'))):
                    raise OwnershipConflict('owned artifact drift', entry['path'])
            if not locked:
                return locked_main(request, create=True)
            apply_preview_migrations(migrations, inventory, path, uid)
        elif migrations:
            raise ValueError('preview migration requires prepared apply')

        for entry in desired:
            record = by_path.get(entry['path'])
            if record is None:
                if mode == 'configs-prepare' and entry['kind'] == 'file' and actual(entry) is not None:
                    raise unowned_selected(entry, actual(entry))
                continue
            if any(record.get(k) != entry.get(k) for k in ('id', 'kind', 'path', 'format')):
                raise OwnershipConflict('owned artifact drift', entry['path'])
            current = actual(entry)
            if mode == 'configs-prepare':
                if entry['kind'] == 'file':
                    if current not in (None, record['digest'], record.get('pending_digest'), record.get('config_pending_digest')):
                        raise OwnershipConflict('owned artifact drift', entry['path'])
                    # A previous refresh may have written its bytes before an
                    # interruption. Preserve that evidence when intent changes.
                    if current is not None and current == record.get('config_pending_digest'):
                        record['digest'] = current
                if record['digest'] != entry['digest'] or 'config_pending_digest' in record:
                    record['config_pending_digest'] = entry['digest']
            else:
                if record.get('config_pending_digest', record['digest']) != entry['digest']:
                    raise ValueError('config inventory intent is stale')
                if entry['kind'] == 'file' and (current != entry['digest'] or actual_metadata(entry) != expected_metadata(entry, home, uid)):
                    raise OwnershipConflict('owned artifact drift', entry['path'])
                record['digest'] = entry['digest']
                record.pop('config_pending_digest', None)
        if not established:
            return
        updated = encode(dict(inventory, entries=records))
        with open(path, 'rb') as stream:
            unchanged = stream.read() == updated
        if not unchanged:
            if not locked:
                return locked_main(request, create=True)
            atomic(path, updated)
        return
    observed = {}
    metadata = {}
    comparable = [{k: v for k, v in entry.items() if k != 'preview_migrated'} for entry in records]
    changed = comparable != [clean(e) for e in desired] or bool(migrations)
    retired = []
    adopted = []
    for name, entry in previous.items():
        current = actual(entry)
        observed[name] = inventory_identity(entry, current)
        current_metadata = actual_metadata(entry)
        if current_metadata is not None:
            metadata[name] = current_metadata
        if entry['kind'] in ('file', 'link') and current not in (None, entry['digest'], entry.get('pending_digest'), entry.get('config_pending_digest')):
            raise OwnershipConflict('owned artifact drift', entry['path'])
        if name not in wanted:
            if (entry['kind'] in ('file', 'link') and current is not None
                    and current_metadata != expected_metadata(entry, home, uid)):
                raise OwnershipConflict('owned artifact drift', entry['path'])
            retired.append(entry)
    for name, entry in wanted.items():
        current = actual(entry)
        observed[name] = inventory_identity(entry, current)
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
                    raise unowned_selected(entry, current)
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
    identity = [home, uid, records, released, observed, metadata,
                [clean(e) for e in desired], not established, adopt, adoption,
                [e.get('source_digest') for e in desired]]
    if cutover:
        identity.append('preview-skill-cutover-v1')
    fingerprint = digest(encode(identity))
    guarded = request.get('after_owner')
    if guarded is not None and receipt is not None:
        info = os.lstat(path)
        if info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600:
            raise ValueError('unsafe original inventory receipt')
    receipt_state = config_state(path) if guarded is not None else None
    states = [artifact_state(entry) for entry in desired] if guarded is not None else None
    projected_previous = dict(previous)
    for record, _, payload in migrations:
        projected_previous[key(record)] = dict(record, preview_migrated=True)
        if key(record) not in wanted:
            projected_previous[key(record)]['config_pending_digest'] = digest(payload)
    next_pending = receipt_bytes(pending_records(projected_previous, wanted, observed), released)
    committed_released = released + [e['path'] for e in retired
                                    if e['kind'] == 'structured' or projected_previous[key(e)].get('preview_migrated')]
    committed_released = [p for p in committed_released if p not in [e.get('path') for e in desired]]
    next_committed = receipt_bytes(committed_records(projected_previous, desired), committed_released)
    witness = None
    already_committed = False
    if guarded is not None:
        current_receipt = '' if receipt is None else digest(receipt)
        if guarded.get('capture') is True:
            if any(previous.get(name, {}).get('config_pending_digest') not in (None, '', entry['digest']) for name, entry in wanted.items()):
                raise ValueError('foreign original config pending intent')
            if mode != 'observe':
                raise ValueError('invalid integration capture')
            witness = {'before': current_receipt, 'pending': digest(next_pending),
                       'committed': digest(next_committed), 'states': states}
        else:
            if set(guarded) != {'before', 'pending', 'committed', 'states'} or len(guarded['states']) != len(desired):
                raise ValueError('missing original integration witness')
            if current_receipt not in (guarded['before'], guarded['pending'], guarded['committed']) or digest(next_committed) != guarded['committed']:
                raise ValueError('original integration receipt changed')
            for index, entry in enumerate(desired):
                if entry['kind'] == 'package':
                    continue
                original, current = guarded['states'][index], states[index]
                target = wanted_state(entry, home, uid)
                if entry['kind'] == 'structured':
                    current = dict(current, digest='')
                    original = dict(original, digest='')
                    target = dict(target, digest='')
                if current not in (original, target):
                    raise ValueError('original integration artifact changed')
            already_committed = current_receipt == guarded['committed'] and all(
                e['kind'] in ('structured', 'package') or states[i] == wanted_state(e, home, uid)
                for i, e in enumerate(desired))
            if not already_committed and digest(next_pending) != guarded['pending']:
                raise ValueError('original integration pending intent changed')
            witness = guarded
    expected = request.get('expected')
    if expected and fingerprint != expected:
        raise ValueError('integration plan is stale')
    if mode == 'observe':
        facts = []
        for name in sorted(set(previous) | set(wanted)):
            entry = wanted.get(name) or previous[name]
            value = observed.get(name)
            fact_name = 'inventory-' + name if name.startswith('structured:') else name
            facts.append({'key': fact_name, 'desired': digest(encode(clean(wanted[name]))) if name in wanted else 'absent',
                          'observed': digest(encode([previous.get(name), value, metadata.get(name)])),
                          'at_desired': (value is None if name not in wanted else
                              {k: v for k, v in (previous.get(name) or {}).items() if k != 'preview_migrated'} == clean(entry) and (
                                  entry['kind'] in ('structured', 'package') or
                                  value == entry['digest'] and metadata.get(name) == expected_metadata(entry, home, uid)))})
        print(json.dumps({'fingerprint': fingerprint, 'changed': changed, 'retired': retired,
                          'adopted': [clean(e) for e in adopted], 'initial': not established,
                          'facts': facts, **({'after_owner': witness, 'states': states} if guarded is not None else {})}))
        return
    if mode not in ('apply', 'commit'):
        raise ValueError('invalid inventory operation')
    for entry in adopted:
        current = actual(entry)
        if not (adoptable_core(entry, current)
                or adopt and not established and adoptable_selected(entry, current, home, uid)):
            raise unowned_selected(entry, current)
    if not locked:
        # The request has passed its original guards. Create mutation custody
        # only now, then validate that same request again under the stable lock.
        return locked_main(request, create=True)
    if migrations and mode != 'apply':
        raise ValueError('preview migration requires prepared apply')
    if migrations and guarded is None:
        apply_preview_migrations(migrations, inventory, path, uid)
        for record, _, _ in migrations:
            observed[key(record)] = actual(record)
    protected(ROOT, True)
    def validate_commit():
        with open(path, 'rb') as stream:
            if digest(stream.read()) != current_receipt:
                raise ValueError('concurrent inventory receipt change')
        for entry in desired:
            if entry['kind'] == 'package':
                continue
            current = artifact_state(entry)
            if entry['kind'] == 'structured':
                row = request.get('commit_states', {}).get(entry['path'])
                if row is None or current != row['State']:
                    raise ValueError('structured destination changed before commit')
                proof = row.get('materialization')
                if not isinstance(proof, dict):
                    raise ValueError('missing structured commit witness')
                identity = digest((request['developer'] + '\0' + entry['path']).encode())
                for suffix, key in (('.json','baseline'),('.ownership','receipt')):
                    protected_path = '/var/lib/subyard/config-materialization/' + identity + suffix
                    protected(protected_path)
                    descriptor = os.open(protected_path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
                    with os.fdopen(descriptor, 'rb') as source:
                        info = os.fstat(source.fileno())
                        raw = source.read((32 << 20) + 1)
                        if info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600 or len(raw) > 32 << 20 or digest(raw) != proof[key]:
                            raise ValueError('structured receipt changed before commit')
            elif current != wanted_state(entry, home, uid):
                raise ValueError('incomplete approved integration set')
    def save(entries):
        atomic(path, receipt_bytes(entries, released), expected=receipt_state, validate=validate_commit if mode == 'commit' and guarded is not None else None)
    if mode == 'commit':
        # Structured retirement releases fields, preserving the user's document.
        # Tombstones prove why these known paths are no longer owned.
        released += [e['path'] for e in retired if e['kind'] == 'structured' or e.get('preview_migrated')]
        released = [p for p in released if p not in [e.get('path') for e in desired]]
        locks = []
        try:
            if guarded is not None:
                # Retain the publisher's existing locks through the final tuple
                # checks and inventory commit; these readonly opens create no state.
                for identity in sorted({digest((request['developer'] + '\0' + entry['path']).encode())
                                        for entry in desired if entry['kind'] == 'structured'}):
                    lock_path = '/var/lib/subyard/config-materialization/' + identity + '.lock'
                    parent = open_parent(lock_path)
                    try:
                        descriptor = os.open(os.path.basename(lock_path), os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=parent)
                    finally:
                        os.close(parent)
                    locks.append(descriptor)
                    info = os.fstat(descriptor)
                    if not stat.S_ISREG(info.st_mode) or info.st_uid != OWNER or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600:
                        raise ValueError('unsafe structured publisher lock')
                    fcntl.flock(descriptor, fcntl.LOCK_SH | fcntl.LOCK_NB)
            save(committed_records(previous, desired))
        finally:
            for descriptor in locks:
                os.close(descriptor)
        return
    # Publish intent before file replacement; both old and candidate digests permit retry.
    if already_committed:
        return
    if guarded is None or current_receipt != guarded['pending']:
        save(pending_records(projected_previous if guarded is not None else previous, wanted, observed))
    if guarded is not None:
        # Keep the exact pending receipt through every retired preview write.
        # Selected instructions use the ordinary original-to-desired publisher.
        for record, before, payload in migrations:
            if key(record) in wanted:
                continue
            current = artifact_state(record)
            if (current['digest'] != before or digest(payload) != projected_previous[key(record)]['config_pending_digest']
                    or actual_metadata(record) != expected_metadata(record, home, uid)):
                raise OwnershipConflict('owned artifact drift', record['path'])
            atomic(record['path'], payload, 0o644, uid, expected=current)
    for entry in retired:
        # Preview cutover retains unmarked instruction text when its host source
        # disappears or its client is disabled. The tombstone releases ownership.
        if projected_previous[key(entry)].get('preview_migrated'):
            continue
        if entry['kind'] in ('file', 'link') and os.path.lexists(entry['path']):
            parent = open_parent(entry['path'])
            try:
                if (actual(entry) not in (entry['digest'], entry.get('pending_digest'), entry.get('config_pending_digest'))
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
            parent_fd = open_parent(destination)
            try:
                current = artifact_state_at(parent_fd, os.path.basename(destination), 'file')
                expected_state = states[list(wanted).index(name)] if guarded is not None else current
                if current != expected_state or current['digest'] != (observed[name] or '') or actual_metadata(entry) != metadata.get(name):
                    raise ValueError('concurrent artifact metadata change')
                atomic(destination, payload, entry.get('mode', 0o644), owner, parent_fd, current)
            finally:
                os.close(parent_fd)
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
                before_link = artifact_state_at(parent_fd, os.path.basename(destination), 'link')
                before_metadata = [before_link['uid'], before_link['gid'], None] if before_link['exists'] else None
                if (guarded is not None and before_link != states[list(wanted).index(name)]
                        or before_link['digest'] != (observed[name] or '') or before_metadata != metadata.get(name)):
                    raise OwnershipConflict('concurrent artifact change', destination)
                os.symlink(target, temporary, dir_fd=parent_fd)
                os.chown(temporary, uid, entry['gid'], dir_fd=parent_fd, follow_symlinks=False)
                if artifact_state_at(parent_fd, os.path.basename(destination), 'link') != before_link:
                    raise OwnershipConflict('concurrent artifact change', destination)
                os.replace(temporary, os.path.basename(destination), src_dir_fd=parent_fd, dst_dir_fd=parent_fd)
                os.fsync(parent_fd)
            finally:
                try:
                    os.unlink(temporary, dir_fd=parent_fd)
                except FileNotFoundError:
                    pass
                os.close(parent_fd)

def locked_main(request=None, create=False):
    # Read-only/config-only absence never creates inventory evidence. A new
    # lock is created only after main validates an actual inventory mutation.
    if request is None:
        request = json.load(sys.stdin)
    writing = sys.argv[1] not in ('observe', 'configs-observe')
    if sys.argv[1] in ('configs-observe', 'configs-write', 'configs-prepare', 'configs-commit') and not os.path.lexists(ROOT + '/inventory.json'):
        return main(request)
    lock_path = ROOT + '/inventory.lock'
    if not create and not os.path.lexists(lock_path):
        return main(request)
    if not os.path.lexists(ROOT):
        os.makedirs(ROOT, mode=0o700)
        os.chmod(ROOT, 0o700)
    ancestors(ROOT)
    protected(ROOT, True)
    if create:
        try:
            descriptor = os.open(lock_path, os.O_NOFOLLOW | os.O_RDWR | os.O_CREAT | os.O_EXCL, 0o600)
            os.fchmod(descriptor, 0o600)
        except FileExistsError:
            descriptor = os.open(lock_path, os.O_NOFOLLOW | os.O_RDWR)
    else:
        descriptor = os.open(lock_path, os.O_NOFOLLOW | os.O_NONBLOCK | (os.O_RDWR if writing else os.O_RDONLY))
    with os.fdopen(descriptor, 'r+b' if writing else 'rb') as lock:
        info = os.fstat(lock.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != OWNER or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600:
            raise ValueError('unsafe inventory lock')
        fcntl.flock(lock.fileno(), fcntl.LOCK_EX if writing else fcntl.LOCK_SH)
        main(request, locked=True)

try:
    locked_main()
except OwnershipConflict as error:
    # Only a fixed category and destination are emitted, never document contents.
    print(json.dumps({'reason': error.reason, 'path': error.path, 'detail': error.detail}), file=sys.stderr)
    sys.exit(1)
except Exception:
    print('integration ownership conflict or unavailable evidence', file=sys.stderr)
    sys.exit(1)
