#!/usr/bin/env python3
"""Private, bounded producer spool. Collection failures never fail workloads."""
from contextlib import contextmanager
import ctypes
import datetime
import fcntl
import hashlib
import http.client
import itertools
import json
import os
from pathlib import Path
import re
import socket
import subprocess
import sys
import stat
import tempfile
import time
import uuid

SCHEMA = 'subyard.telemetry.record/v1'
POLICY_VERSION = 'subyard.telemetry.capture/v1'
TIMESTAMP = re.compile(r'[0-9]{4}-[0-9]{2}-[0-9]{2}T(?:[01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](?:[.,][0-9]{1,9})?(?:Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])')
MAX_RECORDS = 4096
MAX_BYTES = 64*1024*1024
FRAME_BYTES = 64*1024
SEGMENT_BYTES = 256*1024
MARKER_SLOT = 4096
SPOOL_VERSION = b'subyard-spool/1'
LOCK_WAIT = 0.05


def timestamp(value):
    if not isinstance(value, str) or not TIMESTAMP.fullmatch(value):
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


def compact_json(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':')).encode()


def collector_binary():
    installed = Path(__file__).with_name('subyard-telemetry')
    if installed.is_file():
        return installed
    return Path(os.environ.get('SUBYARD_TELEMETRY_COLLECTOR', installed))


def sanitize_state(family, value, binary=None):
    try:
        result = subprocess.run([binary or collector_binary(), 'sanitize', '--family', family],
                                input=compact_json(value), capture_output=True, timeout=2, check=False)
        if result.returncode == 0:
            return json.loads(result.stdout)
    except (OSError, subprocess.TimeoutExpired, ValueError, TypeError):
        pass
    raise ValueError('invalid or unavailable telemetry sanitizer')


def operational_directory(data_dir):
    path = Path(data_dir)
    return private_directory(path.with_name(path.name+'-private'))


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


def atomic_json(path, value, *, family=None):
    if family is not None:
        value = sanitize_state(family, value)
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


def load_producer_state(directory, binary=None):
    """Read and validate the same state used by emit, readiness and repair."""
    directory = Path(directory)
    for path in (directory, directory/'spool'):
        info = path.lstat()
        if not stat.S_ISDIR(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o700 or info.st_uid != os.getuid():
            raise ValueError('invalid telemetry producer directory')
    with open_private(directory/'spool-loss') as slot:
        if os.fstat(slot.fileno()).st_size != 2*MARKER_SLOT:
            raise ValueError('invalid telemetry loss slot')
    producer = read_private(directory/'producer.json')
    policy = read_private(directory/'spool-policy.json')
    state = {'producer': producer, 'policy': policy}
    if (sanitize_state('producer_state', state, binary) != state
            or producer['origin_id'] != policy['origin_id']):
        raise ValueError('invalid telemetry producer state')
    return {**producer, **policy}


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


def append_frame(spool, stream, frame, deadline=None, *, run=None):
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
                    fields = [SPOOL_VERSION, stream.encode(), generation.encode(), f'{int(number)+1:016d}'.encode()]
                    if run:
                        fields.append(run.encode())
                    header = b' '.join(fields)+b'\n'
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
            fields = os.pread(fd, 1024, 0).split(b'\n', 1)[0].split(b' ')
            if (len(fields) != (5 if run else 4) or fields[4:] != ([run.encode()] if run else []) or fields[:2] != [SPOOL_VERSION, stream.encode()]
                    or not re.fullmatch(rb'[0-9a-f]{32}', fields[2]) or not re.fullmatch(rb'[0-9]{16}', fields[3])):
                raise ValueError('unsupported spool segment')
            if os.pread(fd, 1, info.st_size-1) == b'\n' and info.st_size+len(frame) <= SEGMENT_BYTES:
                # A previous appender may have died just after linking this inode.
                sync_directory(spool)
                write_frame(fd, frame)
                return True
            # The sealed reader treats any bytes after the last newline as a gap.
            with directory_lock(spool, fcntl.LOCK_EX, deadline) as directory:
                sealed = spool/('.'.join(part.decode() for part in fields[1:4])+'.sealed')
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


def note_loss(directory, run):
    path = directory/'spool-loss'
    fd = os.open(path, os.O_RDWR | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        checked_file(fd, 2*MARKER_SLOT)
        lock_until(fd, fcntl.LOCK_EX, time.monotonic()+LOCK_WAIT)
        slots = []
        for offset in (0, MARKER_SLOT):
            digest, _, data = os.pread(fd, MARKER_SLOT, offset).partition(b' ')
            data = data.rstrip(b'\0')
            if digest == hashlib.sha256(data).hexdigest().encode():
                slots.append(json.loads(data))
        previous = max(slots, key=lambda item: item['revision'], default={'revision': 0, 'count': 0})
        now_at = timestamp(now())
        count = previous.get('count', 0)
        value = dict(revision=previous['revision']+1, count=count+1, last=now_at,
                     first=previous.get('first', now_at) if count else now_at,
                     id=previous.get('id') if count else uuid.uuid4().hex)
        if run and (not count or previous.get('run') == run):
            value['run'] = run
        data = compact_json(sanitize_state('loss_slot', value))
        slot = (hashlib.sha256(data).hexdigest().encode()+b' '+data).ljust(MARKER_SLOT, b'\0')
        if os.pwrite(fd, slot, (value['revision']%2)*MARKER_SLOT) != MARKER_SLOT:
            raise OSError('short loss marker write')
        os.fsync(fd)
    finally:
        os.close(fd)


def emit(data_dir, kind, body, source_id=None, source_time=None, event_id=None, *, broker=False):
    directory = Path(data_dir)
    run = None
    try:
        with directory_lock(directory, fcntl.LOCK_SH, time.monotonic()+LOCK_WAIT):
            settings = load_producer_state(directory)
            source_time = source_time or now()
            record = dict(schema=SCHEMA, origin_id=settings['origin_id'], source_id=source_id or settings['source_id'],
                          event_id=event_id or str(uuid.uuid4()), source_version='1', source_time=source_time,
                          kind=kind, body=body)
            policy = {field: settings[field] for field in ('attributes', 'span_names', 'event_names')}
            policy['schema'] = settings['policy_version']
            record = sanitize_state('record', {'record': record, 'policy': policy})
            if not broker:
                run = record['body'].get('agent_run_id')
            stream = hashlib.sha256(run.encode()).hexdigest()[:32] if run else 'broker'
            frame = compact_json(record)+b'\n'
            if len(frame) <= FRAME_BYTES and append_frame(directory/'spool', stream, frame, run=run):
                return True
    except (OSError, ValueError, KeyError, TypeError):
        pass
    try:
        note_loss(directory, run)
    except (OSError, ValueError, KeyError, TypeError):
        print('Telemetry coverage gap could not be saved', file=sys.stderr, flush=True)
    return False
