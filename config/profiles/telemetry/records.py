#!/usr/bin/env python3
"""Private, bounded producer outbox. Collection failures never fail workloads."""
import datetime
import fcntl
import http.client
import json
import math
import os
from pathlib import Path
import re
import socket
import stat
import tempfile
import uuid

SCHEMA = 'subyard.telemetry.record/v1'
KINDS = {'run', 'session', 'relation', 'scope', 'process', 'sample', 'allocation', 'association', 'gap'}
FIELDS = set('''association_id purpose allocated_cpu allocated_memory_bytes allocated_disk_bytes reservation_semantics agent_run_id state agent agent_version session_id parent_run_id task_id project_id
worktree_id yard_id host_id object_id object_kind parent_object_id accounting boot_id generation
monotonic_ns cpu_ns cpu_user_ns cpu_system_ns memory_bytes memory_peak_bytes swap_bytes
io_read_bytes io_write_bytes io_read_ops io_write_ops rss_bytes rss_peak_bytes pss_bytes observed_process_count
quality scope_quality coverage_start coverage_end counter_reset cgroup_namespace_id
process_namespace_id pid start_ticks exit_code signal relation source start_time end_time shared
reason available sampled_peak_bytes owner_id slot_id resource_generation lease_epoch allocation_id
acquire_id correlation_id reserved_cpu reserved_memory_bytes reserved_disk_bytes actual_cpu_ns
actual_memory_bytes actual_disk_bytes cpu_count memory_limit_bytes disk_limit_bytes interval_ns
status resource_kind device_count device_id sample_id source_event_id cursor missing stale
boundary host_monotonic_ns runtime_generation requested reserved actual lifecycle type
cpu memory disk resources present counter_generation final main_exited agent_exited resource_completed supported process_count
value unit observed_time resource_id allocation_generation requested_cpu requested_memory_bytes
selector phase final_complete acquired_at ready_at expires_at stopped_at last_seen source_time
requested_disk_bytes reserved_vcpu reserved_ram_bytes measured_cpu_ns measured_memory_bytes
vcpus disk_bytes cpu_usage_ns memory_current_bytes memory_limit swap_current_bytes
read_bytes write_bytes read_ops write_ops populated attribution_id'''.split())
IDENTITY = re.compile(r'[A-Za-z0-9_.:-]{1,128}\Z')
MAX_RECORDS = 4096
MAX_BYTES = 64*1024*1024


def clean(value):
    if value is None or type(value) is bool:
        return value
    if type(value) is int:
        if -(2**63) <= value < 2**64:
            return value
        raise ValueError('counter range')
    if type(value) is float and math.isfinite(value):
        return value
    if isinstance(value, str) and len(value) <= 256 and all(ord(c) >= 32 and ord(c) != 127 for c in value):
        return value
    if isinstance(value, list) and len(value) <= 128:
        return [clean(item) for item in value]
    if isinstance(value, dict) and len(value) <= 128:
        return {key: clean(item) for key, item in value.items() if key in FIELDS}
    raise ValueError('unsupported metadata')


def private_directory(path):
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
    content = (json.dumps(value, sort_keys=True, separators=(',', ':'))+'\n').encode()
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


def read_private(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, 'rb') as source:
        info = os.fstat(source.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600 or info.st_size > 65536:
            raise ValueError('private file required')
        return json.load(source)


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
            connection.request('POST', '/v1/records', json.dumps(record), {'Content-Type': 'application/json'})
            response = connection.getresponse()
            response.read(1024)
            return response.status == 200
    except (OSError, ValueError, http.client.HTTPException):
        return False


def emit(data_dir, kind, body, source_id=None, source_time=None, event_id=None):
    try:
        if kind not in KINDS:
            return False
        directory = private_directory(data_dir)
        outbox = private_directory(directory/'outbox')
        fd = os.open(outbox/'.lock', os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, 'rb') as lock:
            info = os.fstat(lock.fileno())
            if info.st_uid != os.getuid() or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600:
                return False
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            identity = directory/'producer.json'
            if not identity.exists():
                atomic_json(identity, {'source_id': str(uuid.uuid4())})
            source_id = source_id or read_private(identity)['source_id']
            event_id = event_id or str(uuid.uuid4())
            if not IDENTITY.fullmatch(source_id) or not IDENTITY.fullmatch(event_id):
                return False
            path = outbox/(event_id+'.json')
            existing = read_private(path) if path.exists() else None
            record = {'schema': SCHEMA, 'source_id': source_id, 'event_id': event_id,
                      'source_version': '1', 'source_time': source_time or (existing or {}).get('source_time') or datetime.datetime.now(datetime.timezone.utc).isoformat(),
                      'kind': kind, 'body': clean(body)}
            if existing:
                if existing != record:
                    return False
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
                    gap = {'schema': SCHEMA, 'source_id': source_id, 'event_id': health['event_id'],
                           'source_version': '1', 'source_time': health['first_time'], 'kind': 'gap',
                           'body': {'reason': 'producer_outbox_capacity', 'quality': 'partial',
                                    'missing': health['count'], 'coverage_start': health['first_time'], 'coverage_end': health['last_time']}}
                required = len(json.dumps(record).encode())+(len(json.dumps(gap).encode()) if gap else 0)
                if len(entries)+1+bool(gap) > MAX_RECORDS or sum(entries)+required >= MAX_BYTES:
                    now = datetime.datetime.now(datetime.timezone.utc).isoformat()
                    health = health or {'event_id': str(uuid.uuid4()), 'first_time': now, 'count': 0, 'reason': 'outbox_capacity'}
                    health.update(last_time=now, count=min(2**63-1, health['count']+1))
                    atomic_json(health_path, health)
                    return False
                if gap:
                    atomic_json(outbox/(gap['event_id']+'.json'), gap)
                    health_path.unlink()
                    parent_fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
                    try:
                        os.fsync(parent_fd)
                    finally:
                        os.close(parent_fd)
                atomic_json(path, record)
        # Only initial run registration needs a committed acknowledgement before
        # native SDK startup. Broker and sampling paths stop at the durable outbox.
        if kind == 'run' and record['body'].get('state') == 'starting' and record['body'].get('agent'):
            deliver(directory, record)
        return True
    except (OSError, ValueError, TypeError, KeyError):
        return False
