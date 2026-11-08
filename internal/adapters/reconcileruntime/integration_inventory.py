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

def diagnostic_binding(settings, entries):
    # Bind optional evidence to committed state. Older writers may preserve
    # unknown fields while replacing settings/entries; those bytes are stale.
    committed = [{k: v for k, v in entry.items()
                  if k not in ('pending_digest', 'config_pending_digest')}
                 for entry in entries]
    return digest(encode([settings, committed]))

def valid_diagnostics(value):
    if not isinstance(value, dict) or not {'key', 'digests'} <= set(value) or set(value) - {'key', 'digests', 'files'}:
        return False
    key, digests, files = value['key'], value['digests'], value.get('files', {})
    if not isinstance(key, str) or len(key) != 64 or any(c not in '0123456789abcdef' for c in key):
        return False
    if not isinstance(digests, dict) or len(digests) > 4096 or any(not isinstance(k, str) or len(k) > 256 or not isinstance(v, str) or len(v) != 64 or any(c not in '0123456789abcdef' for c in v) for k, v in digests.items()):
        return False
    if not isinstance(files, dict) or len(files) > 4096:
        return False
    return all(isinstance(k, str) and len(k) <= 256 and isinstance(v, dict) and set(v) == {'path', 'digest'} and isinstance(v['path'], str) and len(v['path']) <= 16384 and isinstance(v['digest'], str) and len(v['digest']) == 64 and all(c in '0123456789abcdef' for c in v['digest']) for k, v in files.items())

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
    applied_settings = None
    diagnostics = None
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
        applied_settings = inventory.get('applied_settings')
        diagnostics = inventory.get('diagnostics')
        if applied_settings is not None and (stat.S_IMODE(os.lstat(path).st_mode) != 0o600 or os.lstat(path).st_nlink != 1):
            raise ValueError('unsafe applied integration settings inventory')
        if applied_settings is not None and (not isinstance(applied_settings, dict) or len(applied_settings) > 4096 or any(not isinstance(k, str) or not isinstance(v, str) or len(k) > 256 or len(v) > 16384 for k, v in applied_settings.items())):
            raise ValueError('invalid applied integration settings')
        if diagnostics is not None:
            if stat.S_IMODE(os.lstat(path).st_mode) != 0o600 or os.lstat(path).st_nlink != 1:
                raise ValueError('unsafe applied diagnostics inventory')
            if (not valid_diagnostics(diagnostics)
                    or inventory.get('diagnostic_binding') != diagnostic_binding(applied_settings, records)):
                diagnostics = None
        released = inventory.get('released', [])
        established = True
    if mode == 'settings':
        # Read only protected baseline metadata first. The caller validates its
        # developer identity before a second selection read validates every
        # installed/released destination against that applied owner's home.
        print(json.dumps({'installed': established, 'applied_settings': applied_settings, 'diagnostics': diagnostics}))
        return
    if not isinstance(released, list) or any(not isinstance(p, str) or not p.startswith(home + '/') or os.path.normpath(p) != p for p in released):
        raise ValueError('invalid released artifact evidence')
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
    if mode == 'selection':
        # Only committed package entries prove successful selection. Apply keeps
        # new package intent out of this inventory until all hooks succeed.
        selected = [e['id'] for e in records if e['kind'] == 'package']
        print(json.dumps({'installed': established, 'selected': selected, 'applied_settings': applied_settings, 'diagnostics': diagnostics}))
        return
    cutover = request.get('preview_skill_cutover') is True
    migrations = preview_migration_plan(records, home, uid) if cutover else []
    if mode in ('configs-prepare', 'configs-commit'):
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
            apply_preview_migrations(migrations, inventory, path, uid)
        elif migrations:
            raise ValueError('preview migration requires prepared apply')

        committed_paths = set()
        for entry in desired:
            if entry['kind'] not in ('file', 'structured') or not entry['path'].startswith(home + '/'):
                raise ValueError('invalid config inventory entry')
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
                committed_paths.add(entry['path'])
        if not established:
            return
        capture = request.get('config_diagnostics')
        if mode == 'configs-commit' and capture is not None:
            if not isinstance(capture, dict) or set(capture) != {'key', 'files'} or not isinstance(capture['key'], str) or len(capture['key']) != 64 or any(c not in '0123456789abcdef' for c in capture['key']) or (diagnostics is not None and capture['key'] != diagnostics['key']) or not isinstance(capture['files'], dict) or len(capture['files']) > 4096:
                raise ValueError('config diagnostic baseline changed')
            if diagnostics is None:
                diagnostics = {'key': capture['key'], 'digests': {}}
            files = dict(diagnostics.get('files', {}))
            for name, item in capture['files'].items():
                if not isinstance(name, str) or len(name) > 256 or not isinstance(item, dict) or set(item) != {'path', 'digest', 'destination'} or not isinstance(item['path'], str) or len(item['path']) > 16384 or not isinstance(item['digest'], str) or len(item['digest']) != 64 or any(c not in '0123456789abcdef' for c in item['digest']) or not isinstance(item['destination'], str):
                    raise ValueError('invalid config diagnostics')
                if item['destination'] in committed_paths:
                    files[name] = {'path': item['path'], 'digest': item['digest']}
            diagnostics = dict(diagnostics, files=files)
            inventory['diagnostics'] = diagnostics
            inventory['diagnostic_binding'] = diagnostic_binding(applied_settings, records)
        updated = encode(dict(inventory, entries=records))
        with open(path, 'rb') as stream:
            unchanged = stream.read() == updated
        if not unchanged:
            atomic(path, updated)
        return
    observed = {}
    metadata = {}
    # Installed-view release inspection and repair own only the committed
    # installed scope. An incomplete apply marks the records it introduced as
    # uncommitted; outside that scope they belong to the pending init, which
    # completes or retires them. Release repair neither judges, retires nor
    # forgets them. Committed records keep ordinary retirement even when a
    # failed apply left intent labels on them.
    retain = request.get('retain_pending_intent') is True or mode == 'observe-applied'
    intent = {name for name, entry in previous.items()
              if retain and name not in wanted and entry.get('uncommitted') is True}
    def comparable_entry(entry):
        value = {k: v for k, v in entry.items() if k != 'preview_migrated'}
        if mode == 'observe-applied':
            # Apply records intent before writes, including unchanged files.
            # A later hook failure leaves those exact redundant labels behind.
            # Inside the installed scope, passive release inspection may ignore
            # only intent identical to the committed artifact; actual
            # bytes/metadata and source-derived desired records are still
            # compared below. Init retains the labels.
            for field in ('pending_digest', 'config_pending_digest'):
                if value.get(field) == value.get('digest'):
                    value.pop(field, None)
        return value
    comparable = [comparable_entry(entry) for entry in records if key(entry) not in intent]
    changed = comparable != [clean(e) for e in desired] or bool(migrations)
    retired = []
    adopted = []
    for name, entry in previous.items():
        current = actual(entry)
        observed[name] = inventory_identity(entry, current)
        current_metadata = actual_metadata(entry)
        if current_metadata is not None:
            metadata[name] = current_metadata
        if name in intent:
            continue
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
    if applied_settings is not None:
        identity.append(applied_settings)
    if diagnostics is not None:
        identity.append(diagnostics)
    if cutover:
        identity.append('preview-skill-cutover-v1')
    fingerprint = digest(encode(identity))
    expected = request.get('expected')
    if expected and fingerprint != expected:
        raise ValueError('integration plan is stale')
    if mode in ('observe', 'observe-applied'):
        facts = []
        for name in sorted((set(previous) - intent) | set(wanted)):
            entry = wanted.get(name) or previous[name]
            value = observed.get(name)
            facts.append({'key': name, 'desired': digest(encode(clean(wanted[name]))) if name in wanted else 'absent',
                          'observed': digest(encode([previous.get(name), value, metadata.get(name)])),
                          'at_desired': (value is None if name not in wanted else
                              comparable_entry(previous.get(name) or {}) == clean(entry) and (
                                  entry['kind'] in ('structured', 'package') or
                                  value == entry['digest'] and metadata.get(name) == expected_metadata(entry, home, uid)))})
        print(json.dumps({'fingerprint': fingerprint, 'changed': changed, 'retired': retired, 'diagnostics': diagnostics,
                          'adopted': [clean(e) for e in adopted], 'initial': not established,
                          'facts': facts}))
        return
    if mode == 'capture-settings':
        settings = request.get('applied_settings')
        new_diagnostics = request.get('diagnostics')
        if new_diagnostics is None and settings == applied_settings:
            new_diagnostics = diagnostics
        if not expected or not established or changed or not isinstance(settings, dict) or len(settings) > 4096 or any(not isinstance(k, str) or not isinstance(v, str) or len(k) > 256 or len(v) > 16384 for k,v in settings.items()):
            raise ValueError('applied settings require a converged installed inventory')
        if new_diagnostics is not None and not valid_diagnostics(new_diagnostics):
            raise ValueError('invalid applied diagnostics')
        if applied_settings == settings and diagnostics == new_diagnostics:
            return
        atomic(path, encode(dict(inventory, applied_settings=settings, diagnostics=new_diagnostics,
                                 diagnostic_binding=diagnostic_binding(settings, records))))
        return
    if mode not in ('apply', 'commit'):
        raise ValueError('invalid inventory operation')
    for entry in adopted:
        current = actual(entry)
        if not (adoptable_core(entry, current)
                or adopt and not established and adoptable_selected(entry, current, home, uid)):
            raise unowned_selected(entry, current)
    if migrations:
        if mode != 'apply':
            raise ValueError('preview migration requires prepared apply')
        apply_preview_migrations(migrations, inventory, path, uid)
        for record, _, _ in migrations:
            observed[key(record)] = actual(record)
    os.makedirs(ROOT, mode=0o700, exist_ok=True)
    protected(ROOT, True)
    def save(entries):
        value = {'schema': 1, 'entries': entries, 'released': sorted(set(released))}
        if applied_settings is not None:
            value['applied_settings'] = applied_settings
        if diagnostics is not None:
            value['diagnostics'] = diagnostics
            value['diagnostic_binding'] = inventory.get('diagnostic_binding')
        atomic(path, encode(value))
    if mode == 'commit':
        # Structured retirement releases fields, preserving the user's document.
        # Tombstones prove why these known paths are no longer owned.
        released += [e['path'] for e in retired if e['kind'] == 'structured' or e.get('preview_migrated')]
        released = [p for p in released if p not in [e.get('path') for e in desired]]
        committed = [clean(e) for e in desired]
        for entry in committed:
            if previous.get(key(entry), {}).get('preview_migrated'):
                entry['preview_migrated'] = True
        save(committed + [previous[name] for name in sorted(intent)])
        return
    # Publish intent before file replacement; both old and candidate digests permit retry.
    pending = dict(previous)
    for name, entry in wanted.items():
        if entry['kind'] == 'package':
            continue
        # Commit saves clean desired records, which drops this marker.
        candidate = dict(clean(entry), pending_digest=entry['digest'], uncommitted=True)
        if name in previous:
            candidate = dict(previous[name], pending_digest=entry['digest'])
        elif observed[name] is not None:
            candidate['digest'] = observed[name]
        pending[name] = candidate
    save(list(pending.values()))
    for entry in retired:
        # Preview cutover retains unmarked instruction text when its host source
        # disappears or its client is disabled. The tombstone releases ownership.
        if entry.get('preview_migrated'):
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
    print(json.dumps({'reason': error.reason, 'path': error.path, 'detail': error.detail}), file=sys.stderr)
    sys.exit(1)
except Exception:
    print('integration ownership conflict or unavailable evidence', file=sys.stderr)
    sys.exit(1)
