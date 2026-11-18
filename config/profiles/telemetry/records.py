#!/usr/bin/env python3
"""Private, bounded producer outbox. Collection failures never fail workloads."""
from contextlib import closing
import datetime
import fcntl
import hmac
import http.client
import json
import os
from pathlib import Path
import re
import socket
import sqlite3
import stat
import tempfile
import uuid

SCHEMA = 'subyard.telemetry.record/v1'
POLICY_VERSION = 'subyard.telemetry.capture/v1'
STORE_VERSION = 4
KINDS = {'run', 'session', 'relation', 'scope', 'process', 'sample', 'allocation', 'association', 'gap'}
BODY_ID = {'run': 'agent_run_id', **dict.fromkeys(('scope', 'process', 'sample', 'allocation', 'association'), 'object_id')}
BODY_RULES = {}
PATTERNS = {}
policy_path = Path(__file__).with_name('record-policy.txt')
for line in policy_path.read_text().splitlines():
    rule, *fields = line.split()
    if rule == 'pattern':
        PATTERNS[fields[0]] = re.compile(fields[1])
    elif rule in ('enum', 'names'):
        BODY_RULES[fields[0]] = (rule, fields[1:])
    else:
        BODY_RULES.update((field, (rule, ())) for field in fields)

IDENTITY = re.compile(r'[A-Za-z0-9_.:-]{1,128}')
UNKNOWN_SOURCE_TIME = '0001-01-01T00:00:00Z'
USE_CURRENT_TIME = object()
MAX_RECORDS = 4096
MAX_BYTES = 64*1024*1024


def valid_timestamp(value):
    if not isinstance(value, str) or not PATTERNS['timestamp'].fullmatch(value):
        return False
    try:
        datetime.datetime.fromisoformat(value.replace('Z', '+00:00')).astimezone(datetime.timezone.utc)
    except (ValueError, OverflowError):
        return False
    return True


def legacy_identity(key, value):
    if PATTERNS['legacy_opaque'].fullmatch(value):
        return value
    return 'id:' + hmac.digest(key, value.encode(), 'sha256').hex()


def opaque_id(key, value):
    if not isinstance(value, str) or not PATTERNS['legacy_id'].fullmatch(value):
        return None
    # Accept the old spelling as an input alias, but digest every input.
    return 'h1:' + hmac.digest(key, legacy_identity(key, value).encode(), 'sha256').hex()


def clean_field(key, field, value):
    rule, choices = BODY_RULES[field]
    if value is None:
        # Explicit nulls preserve unavailable gauges and clear old projections.
        return True, None
    if rule == 'names':
        return (True, [item for item in value if item in choices]) if type(value) is list and len(value) <= 128 else (False, None)
    if rule == 'missing':
        if type(value) is list and len(value) <= 128:
            return True, [item for item in value if isinstance(item, str) and item in BODY_RULES]
        if value in ('broker_attribution', 'final_tail'):
            return True, value
        rule = 'uint'
    if rule == 'bool':
        return type(value) is bool, value
    if rule in ('uint', 'int'):
        if isinstance(value, str) and PATTERNS['decimal'].fullmatch(value) and (rule == 'int' or not value.startswith('-')):
            digits = value.lstrip('+-').lstrip('0')
            if len(digits) > 20:
                return False, None
            value = int(('-' if value.startswith('-') else '') + (digits or '0'), 10)
        if rule == 'uint':
            return type(value) is int and 0 <= value < 2**64, value
        return type(value) is int and -(2**63) <= value < 2**63, value
    if not isinstance(value, str):
        return False, None
    if rule == 'enum':
        return value in choices, value
    if rule == 'time':
        return valid_timestamp(value), value
    if rule == 'id':
        value = opaque_id(key, value)
        return value is not None, value
    return bool(PATTERNS[rule].fullmatch(value)), value


def clean(body, key):
    if not isinstance(body, dict):
        raise ValueError('unsupported metadata')
    result, removed = {}, 0
    for field, original in body.items():
        if field not in BODY_RULES:
            removed += 1
            continue
        accepted, value = clean_field(key, field, original)
        if accepted:
            result[field] = value
            if type(original) is list:
                removed += len(original) - len(value)
        else:
            removed += 1
    return result, removed


def compact_json(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':')).encode()


def signed_record(key, record):
    signature = hmac.digest(key, b'producer-record/v1\0' + compact_json(record), 'sha256').hex()
    return {'record': record, 'signature': signature}


def outbox_record(key, kind, body, source_id, event_id, source_time):
    source_id, event_id = opaque_id(key, source_id), opaque_id(key, event_id)
    if kind not in KINDS or not source_id or not event_id:
        raise ValueError('invalid record identity or kind')
    body, removed = clean(body, key)
    required = BODY_ID.get(kind)
    if required and not body.get(required):
        raise ValueError('missing record identity')
    if not valid_timestamp(source_time):
        source_time = UNKNOWN_SOURCE_TIME
        removed += 1
    return {'schema': SCHEMA, 'source_id': source_id, 'event_id': event_id, 'source_version': '1',
            'source_time': source_time, 'kind': kind, 'body': body, 'redactions': removed}


SESSION_FIELDS = ('session_id', 'native_session_id', 'session_attributes')
NATIVE_SOURCE = 'native-otlp-attribute'


def session_metadata(record):
    body = record['body']
    return (record['kind'] in ('session', 'relation') or any(field in body for field in SESSION_FIELDS)
            or body.get('relation') == 'native_session' or (body.get('source') or '').startswith(NATIVE_SOURCE))


def filter_session(record, attributes):
    body = record['body']
    native_relation = body.get('relation') == 'native_session'
    native_source = (body.get('source') or '').startswith(NATIVE_SOURCE)
    relation_kind = record['kind'] in ('session', 'relation')
    names = body.get('session_attributes')
    retained = [name for name in names if name in attributes] if isinstance(names, list) else None
    allowed = bool(retained) if retained is not None else set(BODY_RULES['session_attributes'][1]).issubset(attributes)
    keep_run = 'subyard.agent_run.id' in attributes
    keep_evidence = 'subyard.agent_run.evidence' in attributes

    def remove(*fields):
        for field in fields:
            if field in body:
                del body[field]
                record['redactions'] += 1

    session = body.get('session_id') or body.get('native_session_id')
    ambiguous = body.get('session_id') and body.get('native_session_id') and body['session_id'] != body['native_session_id']
    if not relation_kind and (not allowed or not keep_run or ambiguous or (not session and 'session_attributes' in body)):
        remove(*SESSION_FIELDS)
        if native_relation:
            remove('relation')
        if native_source:
            remove('source')
        return True
    if relation_kind and not session:
        return record['kind'] != 'session' and not native_relation and not native_source and 'session_attributes' not in body
    if not allowed or ambiguous:
        return False

    if 'native_session_id' in body:
        if not body.get('session_id') and session:
            body['session_id'] = body.pop('native_session_id')
        else:
            remove('native_session_id')
    if retained is not None:
        body['session_attributes'] = retained
        record['redactions'] += len(names) - len(retained)
    if not keep_run:
        remove('agent_run_id', 'parent_run_id', 'relation')
        record['kind'] = 'session'
    if (relation_kind or native_source) and (not keep_run or not keep_evidence):
        record['redactions'] += 'source' in body and body['source'] != NATIVE_SOURCE
        body['source'] = NATIVE_SOURCE
    return True


def private_directory(path, *, outbox=False):
    """Create/check the private leaf, never repair existing objects.

    Parents must already satisfy their owning contract: mkdir(parents=True) does
    not make intermediate parents private. Initialize the data root first before
    creating sources/runs/outbox beneath it (or prepare that root in a fixture).
    """
    path = Path(path)
    for parent in (path, *path.parents):
        if parent.is_symlink():
            raise ValueError('unsafe directory')
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    info = path.stat()
    modes = (0o700, 0o1700) if outbox else (0o700,)
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) not in modes:
        raise ValueError('private directory required')
    return path


def atomic_json(path, value, *, outbox=False):
    path = Path(path)
    private_directory(path.parent, outbox=outbox)
    if path.is_symlink():
        raise ValueError('unsafe file')
    if path.exists():
        info = path.stat()
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o600:
            raise ValueError('private file required')
    content = compact_json(value)+b'\n'
    if len(content) > 65536:
        raise ValueError('record too large')
    fd, temporary = tempfile.mkstemp(prefix='.', suffix='.tmp', dir=path.parent)
    try:
        with os.fdopen(fd, 'wb') as out:
            os.fchmod(out.fileno(), 0o600)
            out.write(content)
            out.flush()
            os.fsync(out.fileno())
        os.replace(temporary, path)
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def open_private(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    source = os.fdopen(fd, 'rb')
    info = os.fstat(fd)
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600 or info.st_size > 65536:
        source.close()
        raise ValueError('private file required')
    return source


def read_private(path):
    with open_private(path) as source:
        return json.load(source)


def read_outbox(path):
    value = read_private(path)
    return value.get('record', value)


def store_settings(directory):
    path = directory/'telemetry.sqlite'
    try:
        info = path.lstat()
    except FileNotFoundError:
        return {}
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600:
        raise ValueError('private telemetry store required')
    with closing(sqlite3.connect(path.absolute().as_uri()+'?mode=ro', uri=True, timeout=0.05)) as db:
        version, = db.execute('PRAGMA user_version').fetchone()
        if 0 <= version < STORE_VERSION:
            return {}
        if version != STORE_VERSION:
            raise ValueError('unsupported telemetry schema')
        settings = dict(db.execute("""SELECT key,value FROM metadata WHERE key IN
                        ('identity_key_verifier','session_capture_attributes','session_capture_policy')"""))
        if not settings.get('identity_key_verifier'):
            raise ValueError('telemetry identity key verifier is unavailable')
        return settings


def capture_policy(directory):
    settings = store_settings(directory)
    selected = settings.get('session_capture_attributes')
    if selected is None:
        attributes = {*BODY_RULES['session_attributes'][1], 'subyard.agent_run.id', 'subyard.agent_run.evidence'}
    else:
        attributes = set(json.loads(selected))
    return attributes, settings.get('session_capture_policy', POLICY_VERSION)


def identity_key(directory):
    verifier = store_settings(directory).get('identity_key_verifier')
    path = directory/'.identity-key'
    try:
        with open_private(path) as source:
            key = source.read(33)
            if len(key) != 32:
                raise ValueError('invalid telemetry identity key')
            if verifier is not None and not hmac.compare_digest(
                    verifier, hmac.digest(key, b'subyard.telemetry/identity-key/v1', 'sha256').hex()):
                raise ValueError('telemetry identity key does not match the store')
            return key
    except FileNotFoundError:
        if verifier is not None:
            raise ValueError('telemetry identity key is missing from an existing store') from None
        # Serialize only initialization with the collector. Existing keys are
        # read without a lock and are never replaced.
        parent = os.open(directory, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            fcntl.flock(parent, fcntl.LOCK_EX | fcntl.LOCK_NB)
            if path.exists() or path.is_symlink():
                return identity_key(directory)
            if store_settings(directory).get('identity_key_verifier') is not None:
                raise ValueError('telemetry identity key is missing from an existing store')
            key = os.urandom(32)
            with tempfile.NamedTemporaryFile(dir=directory, prefix='.key-', delete=False) as temporary:
                try:
                    os.fchmod(temporary.fileno(), 0o600)
                    temporary.write(key)
                    temporary.flush()
                    os.fsync(temporary.fileno())
                    os.replace(temporary.name, path)
                    os.fsync(parent)
                finally:
                    if os.path.exists(temporary.name):
                        os.unlink(temporary.name)
            return key
        finally:
            os.close(parent)


class LocalConnection(http.client.HTTPConnection):
    def __enter__(self):
        return self

    def __exit__(self, *_exception):
        self.close()

    def __init__(self, path):
        super().__init__('localhost', timeout=0.5)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(str(self.path))


def deliver(data_dir, record):
    path = Path(data_dir)/'control.sock'
    try:
        info = path.lstat()
        if not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
            return False
        with LocalConnection(path) as connection:
            connection.request('POST', '/v1/records', compact_json(record), {'Content-Type': 'application/json'})
            response = connection.getresponse()
            response.read(1024)
            return response.status == 200
    except (OSError, ValueError, http.client.HTTPException):
        return False


def emit(data_dir, kind, body, source_id=None, source_time=USE_CURRENT_TIME, event_id=None):
    try:
        if kind not in KINDS:
            return False
        directory = private_directory(data_dir)
        key = identity_key(directory)
        outbox = private_directory(directory/'outbox', outbox=True)
        fd = os.open(outbox/'.lock', os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, 'rb') as lock:
            info = os.fstat(lock.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600:
                return False
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            identity = directory/'producer.json'
            if not identity.exists():
                atomic_json(identity, {'source_id': str(uuid.uuid4())})
            source_id = source_id or read_private(identity)['source_id']
            generated_event_id = not event_id
            event_id = event_id or str(uuid.uuid4())
            encoded_event = opaque_id(key, event_id)
            if encoded_event is None:
                return False
            path = outbox/(encoded_event+'.json')
            existing = None
            candidates = [path]
            # Pending files from either old producer spelling can still drain.
            candidates.append(outbox/(legacy_identity(key, event_id)+'.json'))
            if len(event_id) <= 224 and '/' not in event_id:
                candidates.append(outbox/(event_id+'.json'))
            for candidate in candidates:
                try:
                    existing = read_private(candidate)
                    if 'record' in existing:
                        if signed_record(key, existing['record']) != existing:
                            return False
                        existing, authenticated = existing['record'], True
                    else:
                        authenticated = False
                    break
                except FileNotFoundError:
                    continue
            if source_time is USE_CURRENT_TIME:
                source_time = (existing or {}).get('source_time')
                if source_time is None and generated_event_id:
                    source_time = datetime.datetime.now(datetime.timezone.utc).isoformat()
            record = outbox_record(key, kind, body, source_id, event_id, source_time)
            attributes = ()
            if session_metadata(record):
                attributes, policy = capture_policy(directory)
                if not filter_session(record, attributes):
                    return True
                record['policy_version'] = policy
            if existing is not None:
                if existing.get('schema') != SCHEMA and not (
                        not existing.get('schema') and existing.get('schema_version') == 1):
                    return False
                old = dict(existing, body=existing['body'].copy()) if authenticated else outbox_record(
                    key, existing['kind'], existing['body'], existing['source_id'],
                    existing['event_id'], existing.get('source_time'))
                if session_metadata(old) and not filter_session(old, attributes):
                    return False
                metadata = ('redactions', 'policy_version')
                if {k: v for k, v in old.items() if k not in metadata} != {
                        k: v for k, v in record.items() if k not in metadata}:
                    return False
                record = existing if authenticated else record
            else:
                entries = []
                for pending in outbox.glob('*.json'):
                    if pending.is_symlink():
                        continue
                    try:
                        entries.append(pending.stat().st_size)
                    except FileNotFoundError:
                        # The collector may commit and remove an older record.
                        continue
                health_path = directory/'producer-health.json'
                health = read_private(health_path) if health_path.exists() else None
                gap = None
                if health:
                    gap = outbox_record(key, 'gap', {
                        'reason': 'producer_outbox_capacity', 'quality': 'partial', 'missing': health['count'],
                        'coverage_start': health['first_time'], 'coverage_end': health['last_time'],
                    }, source_id, health['event_id'], health['first_time'])
                required = len(compact_json(signed_record(key, record)))+1
                if gap:
                    required += len(compact_json(signed_record(key, gap)))+1
                if len(entries)+1+bool(gap) > MAX_RECORDS or sum(entries)+required >= MAX_BYTES:
                    now = datetime.datetime.now(datetime.timezone.utc).isoformat()
                    health = {'event_id': health['event_id'] if gap else str(uuid.uuid4()),
                              'first_time': gap['source_time'] if gap else now, 'last_time': now,
                              'count': min(2**63-1, (gap['body'].get('missing') or 0)+1) if gap else 1}
                    atomic_json(health_path, health)
                    return False
                if gap:
                    atomic_json(outbox/(gap['event_id']+'.json'), signed_record(key, gap), outbox=True)
                    health_path.unlink()
                    parent_fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
                    try:
                        os.fsync(parent_fd)
                    finally:
                        os.close(parent_fd)
                atomic_json(path, signed_record(key, record), outbox=True)
        # Only initial run registration needs a committed acknowledgement before
        # native SDK startup. Broker and sampling paths stop at the durable outbox.
        if kind == 'run' and record['body'].get('state') == 'starting' and record['body'].get('agent'):
            deliver(directory, signed_record(key, record))
        return True
    except (OSError, ValueError, TypeError, KeyError, sqlite3.Error):
        return False
