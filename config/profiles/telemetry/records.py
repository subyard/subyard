#!/usr/bin/env python3
"""Private, bounded producer outbox. Collection failures never fail workloads."""
import datetime
import fcntl
import http.client
import json
import os
from pathlib import Path
import re
import socket
import stat
import tempfile
import uuid

SCHEMA = 'subyard.telemetry.record/v1'
KINDS = {'run', 'session', 'relation', 'scope', 'process', 'sample', 'allocation', 'association', 'gap'}
# Mirror of collector/record.go, checked by TestRecordFieldParity.
BODY_RULES = {}
BODY_RULE_TABLE = '''
id agent_run_id agent_version session_id parent_run_id task_id project_id worktree_id yard_id host_id owner_id boot_id generation counter_generation resource_id slot_id correlation_id allocation_id association_id native_session_id executable_name acquire_id device_id sample_id source_event_id cursor attribution_id
object object_id parent_object_id
short_id purpose resource_kind boundary lifecycle type unit
namespace cgroup_namespace_id process_namespace_id
uint monotonic_ns cpu_ns cpu_user_ns cpu_system_ns memory_bytes memory_peak_bytes swap_bytes rss_bytes rss_peak_bytes pss_bytes io_read_bytes io_write_bytes io_read_ops io_write_ops observed_process_count pid start_ticks resource_generation lease_epoch requested_cpu reserved_cpu reserved_memory_bytes allocated_cpu allocated_memory_bytes allocated_disk_bytes sampled_peak_bytes reserved_disk_bytes actual_cpu_ns actual_memory_bytes actual_disk_bytes cpu_count memory_limit_bytes disk_limit_bytes interval_ns device_count host_monotonic_ns runtime_generation process_count allocation_generation requested_memory_bytes requested_disk_bytes reserved_vcpu reserved_ram_bytes measured_cpu_ns measured_memory_bytes vcpus disk_bytes cpu_usage_ns memory_current_bytes memory_limit swap_current_bytes read_bytes write_bytes read_ops write_ops source_sequence cpu memory disk resources
int exit_code value
uint signal
bool counter_reset shared final_complete resource_completed agent_exited available stale requested reserved actual present final main_exited supported populated
time coverage_start coverage_end acquired_at ready_at expires_at stopped_at start_time end_time observed_at observed_time last_seen source_time
missing missing
drop status
enum agent codex claude other
enum state starting running not_started main_exited resources_observing resources_complete resources_unknown observed scope_left ended available provisioning held draining quarantined recovering unavailable
enum object_kind cgroup process_tree process scope vm android
enum accounting direct inclusive inclusive-owner-resource qemu-process-and-threads
enum quality explicit measured observed complete partial unknown
enum scope_quality managed partial unavailable
enum selector 1 2 emulator
enum reservation_semantics allocated_virtual_capacity_held
enum relation native_session
enum source acquire-correlation validated-grant managed_scope_membership process_tree_discovery scope_root_missing scope_root_generation_changed cgroup_missing cgroup_generation_changed native-otlp-attribute native-otlp-attribute/authenticated-header native-otlp-attribute/registered-resource-attribute
enum phase request held allocation acquired baseline draining observed periodic pre_stop stopped quarantined lease.acquire lease.admission_rejected lease.available lease.expired lease.fence lease.held lease.release lease.renew_failed lease.stop_failed lease.stop_succeeded lease.transition
enum reason boot_changed_before_final correlation_generation_mismatch correlation_registration_capacity correlation_registration_conflict correlation_registration_invalid correlation_registration_unavailable cursor_retention_or_reset delegation_unavailable final_tail_missing managed_scope_unavailable observer_capacity process_final_counter_unavailable process_left_managed_scope producer_outbox_capacity run_manifest_capacity run_manifest_unavailable run_reservation_interrupted run_startup_unavailable scope_baseline_unavailable scope_manifest_missing scope_observation_unavailable scope_placement_unavailable scope_removed_before_final source_offline
'''
for line in BODY_RULE_TABLE.strip().splitlines():
    rule, *fields = line.split()
    if rule == 'enum':
        BODY_RULES[fields[0]] = (rule, fields[1:])
    else:
        BODY_RULES.update((field, (rule, ())) for field in fields)

IDENTITY = re.compile(r'[A-Za-z0-9_.:-]{1,128}')
OBJECT_ID = re.compile(r'[A-Za-z0-9_.:-]{1,224}')
SHORT_ID = re.compile(r'[A-Za-z0-9_.:-]{1,64}')
NAMESPACE_ID = re.compile(r'(?:pid|cgroup):\[[0-9]{1,20}\]')
TIMESTAMP = re.compile(r'[0-9]{4}-[0-9]{2}-[0-9]{2}T(?:[01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](?:[.,][0-9]{1,9})?(?:Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])')
UNKNOWN_SOURCE_TIME = '0001-01-01T00:00:00Z'
USE_CURRENT_TIME = object()
MAX_RECORDS = 4096
MAX_BYTES = 64*1024*1024


def valid_timestamp(value):
    if not isinstance(value, str) or not TIMESTAMP.fullmatch(value):
        return False
    try:
        datetime.datetime.fromisoformat(value.replace('Z', '+00:00')).astimezone(datetime.timezone.utc)
    except (ValueError, OverflowError):
        return False
    return True


def clean_field(key, value):
    rule, choices = BODY_RULES.get(key, ('drop', ()))
    if rule == 'drop':
        return False, None
    if value is None:
        # Explicit nulls preserve unavailable gauges and clear old projections.
        return True, None
    if rule == 'missing':
        if type(value) is list and len(value) <= 128:
            return True, [item for item in value if isinstance(item, str) and item in BODY_RULES and item != 'status']
        return ((type(value) is int and 0 <= value < 2**64)
                or (isinstance(value, str) and value in ('broker_attribution', 'final_tail'))), value
    if rule == 'bool':
        return type(value) is bool, value
    if rule == 'uint':
        return type(value) is int and 0 <= value < 2**64, value
    if rule == 'int':
        return type(value) is int and -(2**63) <= value < 2**63, value
    if not isinstance(value, str):
        return False, None
    if rule == 'enum':
        return value in choices, value
    if rule == 'time':
        return valid_timestamp(value), value
    pattern = {'id': IDENTITY, 'object': OBJECT_ID, 'short_id': SHORT_ID, 'namespace': NAMESPACE_ID}[rule]
    return bool(pattern.fullmatch(value)), value


def clean(body, kind=None):
    if not isinstance(body, dict) or len(body) > 128:
        raise ValueError('unsupported metadata')
    result = {}
    for key, value in body.items():
        accepted, value = clean_field(key, value)
        if accepted:
            result[key] = value
        elif kind == 'sample' and BODY_RULES.get(key, (None,))[0] == 'uint':
            raise ValueError('invalid sample counter')
    return result


def private_directory(path):
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


def emit(data_dir, kind, body, source_id=None, source_time=USE_CURRENT_TIME, event_id=None):
    try:
        if kind not in KINDS:
            return False
        body = clean(body, kind)
        key = 'agent_run_id' if kind == 'run' else 'object_id' if kind in ('scope', 'process', 'sample', 'allocation', 'association') else None
        if key and not body.get(key):
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
            generated_event_id = not event_id
            event_id = event_id or str(uuid.uuid4())
            if not IDENTITY.fullmatch(source_id) or not IDENTITY.fullmatch(event_id):
                return False
            path = outbox/(event_id+'.json')
            existing = read_private(path) if path.exists() else None
            saved_time = (existing or {}).get('source_time')
            if source_time is USE_CURRENT_TIME:
                source_time = saved_time
                if source_time is None and generated_event_id:
                    source_time = datetime.datetime.now(datetime.timezone.utc).isoformat()
            if not valid_timestamp(source_time):
                # Stable across retries after the collector removes the spool file.
                source_time = UNKNOWN_SOURCE_TIME
            record = {'schema': SCHEMA, 'source_id': source_id, 'event_id': event_id,
                      'source_version': '1', 'source_time': source_time,
                      'kind': kind, 'body': body}
            if existing:
                existing['body'] = clean(existing['body'], kind)
                if not valid_timestamp(saved_time):
                    existing['source_time'] = UNKNOWN_SOURCE_TIME
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
