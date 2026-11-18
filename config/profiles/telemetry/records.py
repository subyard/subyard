#!/usr/bin/env python3
"""Private, bounded producer spool. Collection failures never fail workloads."""
from contextlib import contextmanager
import ctypes
import datetime
import fcntl
import hashlib
import hmac
import http.client
import itertools
import json
import os
from pathlib import Path
import re
import socket
import sys
import stat
import tempfile
import time
import uuid

SCHEMA = 'subyard.telemetry.record/v1'
POLICY_VERSION = 'subyard.telemetry.capture/v1'
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
UNKNOWN_SOURCE_TIME = '0001-01-01T00:00:00.000000000Z'
USE_CURRENT_TIME = object()
MAX_RECORDS = 4096
MAX_BYTES = 64*1024*1024
FRAME_BYTES = 64*1024
SEGMENT_BYTES = 256*1024
MARKER_SLOT = 4096
SPOOL_VERSION = b'subyard-spool/1'
LOCK_WAIT = 0.05


def timestamp(value):
    if not isinstance(value, str) or not PATTERNS['timestamp'].fullmatch(value):
        return None
    try:
        utc = datetime.datetime.fromisoformat(value.replace('Z', '+00:00')).astimezone(datetime.timezone.utc)
    except (ValueError, OverflowError):
        return None
    fraction = re.match(r'[.,]([0-9]+)', value[19:])
    digits = fraction[1].ljust(9, '0') if fraction else '000000000'
    return utc.isoformat(timespec='seconds')[:-6]+'.'+digits+'Z'


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


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
        value = timestamp(value)
        return value is not None, value
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


def prepared_record(key, kind, body, source_id, event_id, source_time):
    source_id, event_id = opaque_id(key, source_id), opaque_id(key, event_id)
    if kind not in KINDS or not source_id or not event_id:
        raise ValueError('invalid record identity or kind')
    body, removed = clean(body, key)
    required = BODY_ID.get(kind)
    if required and not body.get(required):
        raise ValueError('missing record identity')
    source_time = timestamp(source_time)
    if source_time is None:
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


def private_directory(path):
    """Create/check the private leaf, never repair existing objects.

    Parents must already satisfy their owning contract: mkdir(parents=True) does
    not make intermediate parents private. Initialize the data root first before
    creating sources, runs or spool beneath it.
    """
    path = Path(path)
    for parent in (path, *path.parents):
        if parent.is_symlink():
            raise ValueError('unsafe directory')
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    info = path.stat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700:
        raise ValueError('private directory required')
    return path


def atomic_json(path, value):
    path = Path(path)
    private_directory(path.parent)
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
        sync_directory(path.parent)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def checked_file(fd, maximum):
    info = os.fstat(fd)
    if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1
            or stat.S_IMODE(info.st_mode) != 0o600 or info.st_size > maximum):
        raise ValueError('private file required')
    return info


def open_private(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        checked_file(fd, FRAME_BYTES)
        return os.fdopen(fd, 'rb')
    except (OSError, ValueError):
        os.close(fd)
        raise


def read_private(path):
    with open_private(path) as source:
        return json.load(source)


def identity_key(directory):
    with open_private(directory/'.identity-key') as source:
        key = source.read(33)
    if len(key) != 32:
        raise ValueError('invalid telemetry identity key')
    return key


def lock_until(fd, mode, deadline):
    while True:
        try:
            fcntl.flock(fd, mode | fcntl.LOCK_NB)
            return
        except BlockingIOError:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise
            time.sleep(min(0.005, remaining))

def sync_directory(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


@contextmanager
def directory_lock(path, mode, deadline=None):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        if deadline is None:
            fcntl.flock(fd, mode)
        else:
            lock_until(fd, mode, deadline)
        yield fd
    finally:
        os.close(fd)


def capture_policy(directory, key):
    value = read_private(directory/'spool-policy.json')
    settings = value['settings']
    proof = hmac.digest(key, b'spool-policy/v1\0'+compact_json(settings), 'sha256').hex()
    verifier = hmac.digest(key, b'subyard.telemetry/identity-key/v1', 'sha256').hex()
    attributes, policy = settings['attributes'], settings['policy_version']
    allowed = {*BODY_RULES['session_attributes'][1], 'subyard.agent_run.id', 'subyard.agent_run.evidence'}
    if (not hmac.compare_digest(value['signature'], proof)
            or not hmac.compare_digest(settings['key_verifier'], verifier)
            or type(attributes) is not list or not set(attributes) <= allowed
            or not re.fullmatch(re.escape(POLICY_VERSION)+r':[0-9a-f]{64}', policy)):
        raise ValueError('invalid telemetry capture policy')
    return attributes, policy


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


def append_frame(spool, stream, frame, deadline=None):
    if deadline is None:
        deadline = time.monotonic()+LOCK_WAIT
    target = spool/(stream+'.open')
    for attempt in itertools.count():
        # Always try the uncontended path once. Bound waits and retries.
        if attempt and time.monotonic() >= deadline:
            return False
        try:
            fd = os.open(target, os.O_RDWR | os.O_APPEND | os.O_NOFOLLOW | os.O_NONBLOCK)
        except FileNotFoundError:
            with directory_lock(spool, fcntl.LOCK_EX, deadline) as directory:
                if target.exists() or target.is_symlink():
                    continue
                limit = min(MAX_RECORDS, MAX_BYTES//SEGMENT_BYTES)
                with os.scandir(directory) as entries:
                    names = [entry.name for entry in itertools.islice(entries, limit)]
                if len(names) >= limit:
                    return False
                sealed = max((name for name in names if re.fullmatch(
                    re.escape(stream)+r'\.[0-9a-f]{32}\.[0-9]{16}\.sealed', name)), default='')
                generation, number = (sealed.split('.')[1:3] if sealed else (uuid.uuid4().hex, '0'))
                fd = os.open(spool, os.O_TMPFILE | os.O_RDWR | os.O_APPEND, 0o600)
                try:
                    os.fchmod(fd, 0o600)
                    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    # Linux fallocate(KEEP_SIZE) reserves bytes without hiding EOF.
                    fallocate = ctypes.CDLL(None, use_errno=True).fallocate
                    if fallocate(fd, 1, ctypes.c_longlong(0), ctypes.c_longlong(SEGMENT_BYTES)):
                        raise OSError(ctypes.get_errno(), 'spool segment allocation failed')
                    header = b' '.join((SPOOL_VERSION, stream.encode(), generation.encode(),
                                        f'{int(number)+1:016d}'.encode()))+b'\n'
                    write_frame(fd, header+frame)
                    os.link(f'/proc/self/fd/{fd}', target.name, dst_dir_fd=directory)
                    os.fsync(directory)
                finally:
                    os.close(fd)
                return True
        try:
            lock_until(fd, fcntl.LOCK_EX, deadline)
            info = checked_file(fd, SEGMENT_BYTES)
            try:
                current = target.lstat()
            except FileNotFoundError:
                continue
            if not os.path.samestat(info, current):
                continue
            fields = os.pread(fd, 160, 0).split(b'\n', 1)[0].split(b' ')
            if (len(fields) != 4 or fields[:2] != [SPOOL_VERSION, stream.encode()]
                    or not re.fullmatch(rb'[0-9a-f]{32}', fields[2]) or not re.fullmatch(rb'[0-9]{16}', fields[3])):
                raise ValueError('unsupported spool segment')
            if os.pread(fd, 1, info.st_size-1) == b'\n' and info.st_size+len(frame) <= SEGMENT_BYTES:
                # A previous appender may have died just after linking this inode.
                sync_directory(spool)
                write_frame(fd, frame)
                return True
            # The sealed reader treats any bytes after the last newline as a gap.
            with directory_lock(spool, fcntl.LOCK_EX, deadline) as directory:
                sealed = spool/('.'.join(part.decode() for part in fields[1:])+'.sealed')
                if sealed.exists() or sealed.is_symlink():
                    raise ValueError('spool segment collision')
                os.rename(target, sealed)
                os.fsync(directory)
        finally:
            os.close(fd)


def write_frame(fd, data):
    if os.write(fd, data) != len(data):
        raise OSError('short spool write')
    os.fsync(fd)


def note_loss(directory, key, stream):
    path = directory/'spool-loss'
    fd = os.open(path, os.O_RDWR | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        checked_file(fd, 2*MARKER_SLOT)
        def proof(data):
            return hmac.digest(key, b'spool-loss/v1\0'+data, 'sha256').hex().encode()

        lock_until(fd, fcntl.LOCK_EX, time.monotonic()+LOCK_WAIT)
        slots = []
        for offset in (0, MARKER_SLOT):
            digest, _, data = os.pread(fd, MARKER_SLOT, offset).partition(b' ')
            data = data.rstrip(b'\0')
            if hmac.compare_digest(digest, proof(data)):
                slots.append(json.loads(data))
            elif digest == hashlib.sha256(data).hexdigest().encode():
                slots.append(dict(json.loads(data), stream='broker'))
        previous = max(slots, key=lambda item: item['revision'], default={'revision': 0, 'count': 0})
        now_at = timestamp(now())
        if previous['count']:
            previous['stream'] = previous['stream'] if previous['stream'] == stream else 'broker'
        else:
            previous.update(id=uuid.uuid4().hex, first=now_at, stream=stream)
        previous.update(revision=previous['revision']+1, last=now_at, count=previous['count']+1)
        data = compact_json(previous)
        slot = (proof(data)+b' '+data).ljust(MARKER_SLOT, b'\0')
        if os.pwrite(fd, slot, (previous['revision']%2)*MARKER_SLOT) != MARKER_SLOT:
            raise OSError('short loss marker write')
        os.fsync(fd)
    finally:
        os.close(fd)


def emit(data_dir, kind, body, source_id=None, source_time=USE_CURRENT_TIME, event_id=None, *, broker=False):
    deadline = time.monotonic()+LOCK_WAIT
    try:
        directory = private_directory(data_dir)
        with directory_lock(directory, fcntl.LOCK_SH, deadline):
            key = identity_key(directory)
            attributes, policy = capture_policy(directory, key)
            stream = 'broker'
            try:
                source_id = source_id or read_private(directory/'producer.json')['source_id']
                if source_time is USE_CURRENT_TIME:
                    source_time = now() if not event_id else None
                record = prepared_record(key, kind, body, source_id, event_id or str(uuid.uuid4()), source_time)
                identity = [record[field] for field in ('kind', 'source_version', 'source_time', 'body')]
                record['input_id'] = hmac.digest(key, b'producer-input/v1\0'+compact_json(identity), 'sha256').hex()
                record['policy_version'] = policy
                if session_metadata(record) and not filter_session(record, attributes):
                    record.update(filtered=True, body={})
                if not broker:
                    stream = record['body'].get('agent_run_id') or 'broker'
                frame = compact_json(signed_record(key, record))+b'\n'
                if len(frame) <= FRAME_BYTES and append_frame(private_directory(directory/'spool'), stream, frame, deadline):
                    return True
            except (OSError, ValueError, KeyError, TypeError):
                pass
            note_loss(directory, key, stream)
    except (OSError, ValueError, KeyError, TypeError):
        print('Telemetry coverage gap could not be saved', file=sys.stderr, flush=True)
    return False
