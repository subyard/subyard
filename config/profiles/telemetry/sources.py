#!/usr/bin/env python3
"""Independent owner journal observer. No source acknowledgements or lifecycle writes."""
import contextlib
import datetime
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import selectors
import signal
import socket
import stat
import subprocess
import time
import uuid

import records
import resources

LIMIT = 1024 * 1024 + 65536
MAX_INTENTS = 128
MAX_INTENT_BYTES = 1024 * 1024


def registration_gap(data_dir, reason):
    if records.emit(data_dir, 'gap', dict(reason=reason, quality='partial', missing='broker_attribution')):
        return True
    try:
        # One content-free recovery marker outside the intent quota. A full
        # analytics outbox must not turn registration rejection into quiet loss.
        root = records.private_directory(Path(data_dir)/'sources/intents')
        records.atomic_json(root/'registration-health', dict(reason=reason,
            source_time=datetime.datetime.now(datetime.timezone.utc).isoformat(), event_id='registration-'+uuid.uuid4().hex))
    except (OSError, ValueError, TypeError):
        pass  # A failed filesystem cannot promise persistent coverage.
    return False


def flush_registration_gap(data_dir, root):
    pending = root/'intents'
    marker, reading = pending/'registration-health', pending/'registration-health-reading'
    if not reading.exists() and marker.exists():
        os.replace(marker, reading)
    if reading.exists():
        value = records.read_private(reading)
        if records.emit(data_dir, 'gap', dict(reason=value['reason'], quality='partial', missing='broker_attribution'),
                        source_time=value['source_time'], event_id=value['event_id']):
            reading.unlink()


def registration_request(request):
    allowed = {'kind', 'phase', 'owner_id', 'yard_id', 'project_id', 'correlation_id',
               'slot_id', 'resource_generation', 'lease_epoch', 'transport'}
    if not isinstance(request, dict) or set(request)-allowed or request.get('kind') not in ('vm', 'android') or request.get('phase') not in ('request', 'held'):
        raise ValueError('invalid registration')
    value = dict(request)
    for name in ('owner_id', 'correlation_id'):
        if not isinstance(value.get(name), str) or not records.IDENTITY.fullmatch(value[name]):
            raise ValueError('invalid registration identity')
    for name in ('yard_id', 'project_id'):
        if value.get(name) is not None and (not isinstance(value[name], str) or not records.IDENTITY.fullmatch(value[name])):
            raise ValueError('invalid registration context')
    if value['phase'] == 'request':
        return value
    if not isinstance(value.get('slot_id'), str) or not records.IDENTITY.fullmatch(value['slot_id']):
        raise ValueError('invalid registration slot')
    for name in ('resource_generation', 'lease_epoch'):
        number = value.get(name)
        if name == 'lease_epoch' and value['kind'] == 'android' and number is None:
            value[name] = None
            continue
        if isinstance(number, str) and re.fullmatch('[0-9]{1,20}', number):
            number = int(number)
        if type(number) is not int or not 0 < number < 2**64:
            raise ValueError('invalid registration generation')
        value[name] = number
    transport = value.get('transport')
    keys = {'script', 'yard', 'state_dir', 'yard_state_dir', 'identity', 'route', 'route_config'} if value['kind'] == 'vm' else {'control', 'capability', 'instance', 'project'}
    if not isinstance(transport, dict) or set(transport)-keys or any(not isinstance(item, str) or len(item)>4096 or '\x00' in item or '\n' in item for item in transport.values()):
        raise ValueError('invalid registration transport')
    if value['kind'] == 'vm':
        script = Path(transport.get('script', ''))
        if not script.is_absolute() or script.name != 'agent-e2e.sh' or script.parent.name != 'dev' or not re.fullmatch('[a-z0-9][a-z0-9_-]{0,31}', transport.get('yard', '')):
            raise ValueError('invalid registration route')
    else:
        control = Path(transport.get('control', ''))
        if not control.is_absolute() or control.name != 'control.sock' or not re.fullmatch('[a-f0-9]{64}', transport.get('capability', '')):
            raise ValueError('invalid registration route')
        if ('instance' in transport or 'project' in transport) and not all(re.fullmatch('[a-zA-Z0-9][a-zA-Z0-9_.-]{0,99}', transport.get(name, '')) for name in ('instance', 'project')):
            raise ValueError('invalid registration route')
    if len(json.dumps(value).encode()) > 16384:
        raise ValueError('registration byte limit')
    return value


def registration_paths(root, request):
    reader = hashlib.sha256(json.dumps(request['transport'], sort_keys=True, separators=(',', ':')).encode()).hexdigest()
    identity = hashlib.sha256(json.dumps([request['kind'], request['owner_id'], request['correlation_id']], separators=(',', ':')).encode()).hexdigest()
    return root/('observer-'+reader+'.json'), root/'intents'/('intent-'+identity+'.json')


def registration_binding(intent):
    request = intent['request']
    return dict(agent_run_id=intent['agent_run_id'], start_time=intent['registered_at'], slot_id=request['slot_id'],
                resource_generation=request['resource_generation'], lease_epoch=request.get('lease_epoch'))


def binding_conflict(config, correlation, binding):
    identity = ('agent_run_id', 'slot_id', 'resource_generation', 'lease_epoch')
    previous = config.get('associations', {}).get(correlation)
    if previous and any(previous.get(name) != binding.get(name) for name in identity):
        return True
    grant = ('slot_id', 'resource_generation', 'lease_epoch')
    return any(key != correlation and all(value.get(name) == binding.get(name) for name in grant)
               for key, value in config.get('associations', {}).items())


def reader_conflict(data_dir, root, request, binding):
    producer_path = Path(data_dir)/'producer.json'
    source = None
    if producer_path.exists():
        producer = records.read_private(producer_path)['source_id']
        source = 'resource-'+hashlib.sha256((producer+':'+request['kind']+':'+request['owner_id']).encode()).hexdigest()
    for path in list(root.glob('*.json'))[:32]:
        config = records.read_private(path)
        if config.get('kind') == request['kind'] and (config.get('registration_owner',config.get('owner_id')) == request['owner_id'] or config.get('source_id') == source):
            if binding_conflict(config,request['correlation_id'],binding):
                return True
    return False


@contextlib.contextmanager
def intent_lock(root):
    path = root/'.lock'
    try:
        fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_RDWR | os.O_NOFOLLOW, 0o600)
        os.fchmod(fd, 0o600)
    except FileExistsError:
        fd = os.open(path, os.O_RDWR | os.O_NOFOLLOW)
    with os.fdopen(fd, 'rb') as lock:
        info = os.fstat(lock.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600:
            raise ValueError('private registration lock required')
        deadline = time.monotonic()+.1
        while True:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if time.monotonic() >= deadline:
                    raise
                time.sleep(.005)
        yield


def store_intent(data_dir, agent_run, request):
    root = records.private_directory(Path(data_dir)/'sources')
    pending = records.private_directory(root/'intents')
    path, intent_path = registration_paths(root, request)
    intent = dict(schema_version=1, agent_run_id=agent_run, request=request,
                  registered_at=datetime.datetime.now(datetime.timezone.utc).isoformat())
    with intent_lock(pending):
        if intent_path.exists():
            previous = records.read_private(intent_path)
            if previous['agent_run_id'] == agent_run and previous['request'] == request:
                return True
            registration_gap(data_dir, 'correlation_registration_conflict')
            return False
        if reader_conflict(data_dir,root,request,registration_binding(intent)):
            registration_gap(data_dir, 'correlation_registration_conflict')
            return False
        entries = list(pending.glob('intent-*.json'))
        if len(entries) >= MAX_INTENTS or sum(item.lstat().st_size for item in entries)+len(json.dumps(intent).encode()) > MAX_INTENT_BYTES:
            registration_gap(data_dir, 'correlation_registration_capacity')
            return False
        records.atomic_json(intent_path, intent)
    return True


def merge_intents(data_dir, root):
    """Called under the reader lock; hook admission never takes that lock."""
    changed = set()
    flush_registration_gap(data_dir, root)
    for intent_path in sorted((root/'intents').glob('intent-*.json'))[:MAX_INTENTS]:
        try:
            intent = records.read_private(intent_path)
            if set(intent) != {'schema_version','agent_run_id','request','registered_at'} or not isinstance(intent['registered_at'],str) or datetime.datetime.fromisoformat(intent['registered_at']).tzinfo is None:
                raise ValueError('invalid registration intent shape')
            request = registration_request(intent['request'])
            if intent.get('schema_version') != 1 or request['phase'] != 'held' or not records.IDENTITY.fullmatch(intent['agent_run_id']):
                raise ValueError('invalid registration intent')
            path, expected = registration_paths(root, request)
            if expected != intent_path:
                raise ValueError('registration intent identity')
            binding = registration_binding(intent)
            if reader_conflict(data_dir,root,request,binding):
                if registration_gap(data_dir,'correlation_registration_conflict'):
                    intent_path.unlink()
                continue
            if path.exists():
                config = records.read_private(path)
            else:
                existing = list(root.glob('*.json'))
                if len(existing) >= 32:
                    completed = [(old, records.read_private(old)) for old in existing]
                    completed = [item for item in completed if item[1].get('completed')]
                    if not completed:
                        registration_gap(data_dir, 'observer_capacity')
                        continue
                    min(completed, key=lambda item: item[1].get('registered_at', ''))[0].unlink()
                if not (Path(data_dir)/'producer.json').exists():
                    public = {key: request.get(key) for key in ('owner_id', 'yard_id', 'project_id', 'correlation_id')}
                    public.update(agent_run_id=intent['agent_run_id'], source='acquire-correlation', quality='explicit')
                    records.emit(data_dir, 'relation', public, source_time=intent['registered_at'], event_id=intent_path.stem+'-relation')
                producer = records.read_private(Path(data_dir)/'producer.json')['source_id']
                source = 'resource-'+hashlib.sha256((producer+':'+request['kind']+':'+request['owner_id']).encode()).hexdigest()
                config = dict(source_id=source, kind=request['kind'], owner_id=request['owner_id'], cursor='-',
                              registration_owner=request['owner_id'],registered_at=intent['registered_at'], associations={}, transport=request['transport'])
            correlation = request['correlation_id']
            if binding_conflict(config, correlation, binding):
                if not registration_gap(data_dir, 'correlation_registration_conflict'):
                    continue
            elif correlation not in config['associations']:
                if len(config['associations']) >= 128:
                    registration_gap(data_dir, 'correlation_registration_capacity')
                    continue
                config['associations'][correlation] = binding
                config.update(completed=False, cursor='-')
                records.atomic_json(path, config)
                changed.add(path)
            intent_path.unlink()
            directory = os.open(intent_path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
        except (ValueError, KeyError, TypeError):
            if registration_gap(data_dir, 'correlation_registration_invalid'):
                intent_path.unlink()
        except OSError:
            # The pending durable intent remains recoverable after transient IO.
            continue
    return changed


def command_page(command, request=None, environment=None):
    process = subprocess.Popen(command, stdin=subprocess.PIPE if request is not None else subprocess.DEVNULL, env=environment, stdout=subprocess.PIPE,
                               stderr=subprocess.DEVNULL, start_new_session=True)
    data = bytearray()
    deadline = time.monotonic() + 10
    try:
        if request is not None:
            process.stdin.write(json.dumps(request).encode() + b'\n')
            process.stdin.close()
        with selectors.DefaultSelector() as ready:
            ready.register(process.stdout, selectors.EVENT_READ)
            while True:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise ValueError('source deadline')
                if not ready.select(remaining):
                    raise ValueError('source deadline')
                part = os.read(process.stdout.fileno(), min(65536, LIMIT + 1 - len(data)))
                if not part:
                    break
                data.extend(part)
                if len(data) > LIMIT:
                    raise ValueError('source byte limit')
        if process.wait(timeout=max(0.01, deadline - time.monotonic())) != 0:
            raise ValueError('source failed')
        return json.loads(data)
    finally:
        if process.poll() is None:
            with contextlib.suppress(ProcessLookupError):
                os.killpg(process.pid, signal.SIGKILL)
            process.wait()
        process.stdout.close()


def fetch(config):
    cursor = config.get('cursor', '-')
    transport = config['transport']
    if config['kind'] == 'vm':
        script = Path(transport['script'])
        if not script.is_absolute() or script.name != 'agent-e2e.sh' or script.parent.name != 'dev':
            raise ValueError('unsupported source helper')
        yard = transport['yard']
        if not re.fullmatch('[a-z0-9][a-z0-9_-]{0,31}', yard):
            raise ValueError('invalid source yard')
        environment = dict(os.environ)
        for key, name in [('state_dir', 'SUBYARD_E2E_STATE_DIR'), ('yard_state_dir', 'SUBYARD_E2E_YARD_STATE_DIR'),
                          ('identity', 'SUBYARD_E2E_IDENTITY'), ('route', 'SUBYARD_E2E_BASTION_ROUTE'),
                          ('route_config', 'SUBYARD_E2E_ROUTE_CONFIG')]:
            if key in transport:
                environment[name] = transport[key]
        response = command_page(['bash', str(script), '--yard', yard, '--observe', cursor], environment=environment)
        if response.get('schema_version') != 1 or response.get('status') != 'ok':
            raise ValueError('unsupported owner observation')
        return response['observation']
    if config['kind'] != 'android':
        raise ValueError('unsupported resource kind')
    capability = transport['capability']
    if not re.fullmatch('[0-9a-f]{64}', capability):
        raise ValueError('invalid observer capability')
    control = Path(transport['control'])
    if not control.is_absolute() or control.name != 'control.sock':
        raise ValueError('invalid owner endpoint')
    request = dict(operation='observe-v1', capability=capability, cursor=cursor, count=100)
    if 'instance' in transport:
        if not all(re.fullmatch('[a-zA-Z0-9][a-zA-Z0-9_.-]{0,99}', transport.get(key, ''))
                   for key in ('instance', 'project')):
            raise ValueError('invalid owner route')
        response = command_page(['incus', 'exec', transport['instance'], '--project', transport['project'],
                                 '--', 'python3', '/usr/local/lib/subyard-android/client.py', '_wire'], request)
        if response.get('ok') is not True:
            raise ValueError('owner observation unavailable')
        return response['result']
    with socket.socket(socket.AF_UNIX) as channel:
        channel.settimeout(3)
        channel.connect(str(control))
        channel.sendall(json.dumps(request).encode() + b'\n')
        data = bytearray()
        while not data.endswith(b'\n'):
            part = channel.recv(min(65536, LIMIT + 1 - len(data)))
            if not part or len(data) + len(part) > LIMIT:
                raise ValueError('owner response interrupted or oversized')
            data.extend(part)
    response = json.loads(data)
    if response.get('ok') is not True:
        raise ValueError('owner observation unavailable')
    return response['result']


def consume(data_dir, config, page):
    if not isinstance(page, dict) or type(page.get('gap')) is not bool or type(page.get('more')) is not bool:
        raise ValueError('invalid owner page')
    owner_id = page.get('owner_id')
    if not isinstance(owner_id, str) or not re.fullmatch('[0-9a-f]{64}', owner_id):
        raise ValueError('missing native owner identity')
    config['owner_id'] = owner_id
    events, samples = page['events'], page['observations']
    if not isinstance(events, list) or len(events) > 200 or not isinstance(samples, list) or len(samples) > 64:
        raise ValueError('owner page bound')
    cursor = page['next_cursor']
    if cursor != '-' and not re.fullmatch('[0-9]{20}-[0-9a-f]{16}', cursor):
        raise ValueError('invalid owner cursor')
    if page['gap']:
        marker = hashlib.sha256((config.get('cursor', '-') + ':' + cursor).encode()).hexdigest()
        if not records.emit(data_dir, 'gap', dict(owner_id=config['owner_id'], reason='cursor_retention_or_reset',
                            quality='unknown'), source_id=config['source_id'], source_time=config['registered_at'], event_id='cursor-' + marker):
            return False
    for event in events:
        # Global events without allocation identity do not manufacture an object.
        identity = event.get('allocation', event)
        if identity.get('resource_generation', identity.get('generation')):
            if not resources.emit_event(data_dir, config, event):
                return False
    allocations = page.get('allocations', [])
    if not isinstance(allocations, list) or len(allocations) > 32:
        raise ValueError('owner allocation bound')
    for allocation in allocations:
        at = page['observed_at']
        identity = 'current-' + hashlib.sha256(json.dumps([allocation.get('slot_id'),
            allocation.get('resource_generation', allocation.get('generation')), allocation.get('lease_epoch'), at],
            separators=(',', ':')).encode()).hexdigest()
        if not resources.emit_event(data_dir, config, dict(event_id=identity, source_time=at, allocation=allocation,
                                                         kind='allocation', phase='observed')):
            return False
    for sample in samples:
        # The owner descriptor binds the interval; the later held callback does not.
        allocation = next((item for item in allocations if item.get('slot_id') == sample.get('slot_id')
                           and item.get('resource_generation', item.get('generation')) == sample.get('resource_generation')
                           and item.get('lease_epoch') == sample.get('lease_epoch')), {})
        sample = dict(sample, acquired_at=allocation.get('acquired_at'))
        if not resources.emit_sample(data_dir, config, sample):
            return False
    if config['kind'] == 'android':
        config['stop_seen'] = config.get('stop_seen', False) or any(event.get('phase') == 'stopped' for event in events)
        config['completed'] = config['stop_seen'] and not page['more'] and not allocations and not samples
    config['cursor'] = cursor
    config['last_seen'] = datetime.datetime.now(datetime.timezone.utc).isoformat()
    config['offline'] = False
    return True


def poll(data_dir):
    """Advance each private cursor only after every record is durably admitted."""
    try:
        root = records.private_directory(Path(data_dir) / 'sources')
        fd = os.open(root / '.lock', os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, 'rb') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            merge_intents(data_dir, root)
            pending = [(path, records.read_private(path)) for path in sorted(root.glob('*.json'))[:32]]
            pending.sort(key=lambda item: item[1].get('polled_at', ''))
            deadline = time.monotonic() + 10
            for path, config in pending:
                if time.monotonic() >= deadline:
                    break
                try:
                    config = records.read_private(path)
                except FileNotFoundError:
                    continue  # A completed reader may have been retired by a merge.
                config['polled_at'] = datetime.datetime.now(datetime.timezone.utc).isoformat()
                try:
                    page = fetch(config)
                    if path in merge_intents(data_dir, root):
                        continue  # A new binding reset the durable cursor; fetch its retained history next.
                    if consume(data_dir, config, page):
                        if path not in merge_intents(data_dir, root):
                            records.atomic_json(path, config)
                except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError):
                    if not config.get('offline'):
                        admitted = records.emit(data_dir, 'gap', dict(owner_id=config['owner_id'], reason='source_offline',
                                     start_time=config.get('last_seen'), quality='unknown', stale=True),
                                     source_id=config['source_id'], source_time=config.get('last_seen', config['registered_at']),
                                     event_id='offline-' + hashlib.sha256(config.get('last_seen', config['registered_at']).encode()).hexdigest())
                        if admitted:
                            config['offline'] = True
                    records.atomic_json(path, config)
        return True
    except (OSError, ValueError, KeyError, TypeError):
        return False
