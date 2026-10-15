#!/usr/bin/env python3
"""Conservatively retire a proven temporary native trace receiver after adoption.

Progress is deliberately separate from service readiness. This module never reads
process arguments or environments and never restarts native coding sessions.
"""
import argparse
import fcntl
import hashlib
import json
import math
import os
from pathlib import Path
import re
import secrets
import stat
import subprocess
import sys
import time
import tomllib

import native

TEMP = {'otlp-http': {'endpoint': 'http://127.0.0.1:4319/v1/traces', 'protocol': 'binary'}}
IMAGE = 'otel/opentelemetry-collector-contrib:0.162.0@sha256:39923a8e431bd1f57be82411999d389fcfe40857492e4365456d97a4c1f74be6'
ATTRIBUTE = 'subyard.openlit.trace_cutover'
LEGACY = Path('/var/lib/subyard/openlit-traces-workaround')
LOCK = Path('/run/lock/subyard-openlit.lock')
LIMIT = 128 * 1024


class ProbeUnavailable(ValueError):
    """A bounded native probe could not complete; this is not ownership proof."""


def lock_identity(fd, path=LOCK, owner=0):
    info = os.fstat(fd)
    current = path.lstat()
    uid, gid = (owner, owner) if isinstance(owner, int) else owner
    if (info.st_dev, info.st_ino) != (current.st_dev, current.st_ino) or not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or (info.st_uid, info.st_gid, info.st_mode & 0o777) != (uid, gid, 0o600):
        raise ValueError('unsafe lifecycle lock')


def acquire(path=LOCK, wait=5, owner=0):
    try:
        fd = os.open(path, os.O_NOFOLLOW | os.O_RDWR | os.O_CREAT | os.O_EXCL, 0o600)
        os.fchmod(fd, 0o600)
    except FileExistsError:
        fd = os.open(path, os.O_NOFOLLOW | os.O_RDWR)
    try:
        lock_identity(fd, path, owner)
        deadline = time.monotonic()+wait
        while True:
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if time.monotonic() >= deadline:
                    raise ValueError('lifecycle lock busy') from None
                time.sleep(0.05)
        lock_identity(fd, path, owner)
        return fd
    except BaseException:
        os.close(fd)
        raise


def run(arguments, timeout=5, data=None):
    result = subprocess.run(arguments, input=data, capture_output=True, timeout=timeout, check=False)
    if result.returncode or len(result.stdout) > LIMIT:
        raise ProbeUnavailable('cutover probe unavailable')
    return result.stdout


def private(path, directory=False):
    native.safe(path)
    info = path.stat()
    if (info.st_uid, info.st_gid, info.st_mode & 0o777) != (os.getuid(), os.getgid(), 0o700 if directory else 0o600):
        raise ValueError('unsafe cutover state')
    if directory != path.is_dir() or (not directory and info.st_size > LIMIT):
        raise ValueError('unsafe cutover state')


def owned(state):
    # safe() checks files, so validate the directory separately.
    if state.is_symlink() or not state.is_dir():
        raise ValueError('unsafe runtime root')
    for parent in state.parents:
        if parent.is_symlink():
            raise ValueError('unsafe runtime parent')
    info = state.stat()
    if (info.st_uid, info.st_gid, info.st_mode & 0o777) != (os.getuid(), os.getgid(), 0o700):
        raise ValueError('unsafe runtime root')
    private(state/'marker')
    if (state/'marker').read_text().strip() != 'subyard-openlit-v1':
        raise ValueError('foreign runtime root')


def save(state, record):
    native.write(state/'trace-cutover.json', json.dumps(record, sort_keys=True)+'\n')


def record(state):
    owned(state)
    path = state/'trace-cutover.json'
    native.safe(path)
    if not path.exists():
        return None
    private(path)
    value = json.loads(path.read_text())
    required = {'schema', 'phase', 'home', 'uid', 'token', 'legacy', 'files', 'container'}
    if not isinstance(value, dict) or not required.issubset(value) or value['schema'] != 1 or value['phase'] not in ('prepared', 'adopted', 'retired'):
        raise ValueError('invalid cutover state')
    if not isinstance(value['home'], str) or not Path(value['home']).is_absolute() or not isinstance(value['uid'], int) or value['uid'] < 0 or not re.fullmatch('[0-9a-f]{32}', value['token']):
        raise ValueError('invalid cutover identity')
    if value['legacy'] != str(LEGACY) or not isinstance(value['files'], dict) or not isinstance(value['container'], dict):
        raise ValueError('invalid cutover ownership')
    if not re.fullmatch('[a-f0-9]{64}', value['container'].get('id', '')) or not re.fullmatch('sha256:[a-f0-9]{64}', value['container'].get('image', '')):
        raise ValueError('invalid cutover container')
    if value['phase'] != 'prepared' and (not isinstance(value.get('barrier'), int) or value['barrier'] < 0 or not re.fullmatch('[0-9a-f-]{36}', value.get('boot', '')) or not isinstance(value.get('cutover_ns'), int)):
        raise ValueError('invalid process fence')
    return value


def marked(state):
    # Standalone native fixtures and other callers do not activate migration policy.
    if state.name != 'native.json' or not (state.parent/'marker').exists():
        return False
    owned(state.parent)
    return True


def leave_unowned(state, old, value):
    return old is None and value == TEMP and marked(state)


def admission(state):
    owned(state)
    path = state/'trace-admission.json'
    native.safe(path)
    if not path.exists():
        return None
    private(path)
    value = json.loads(path.read_text())
    if not isinstance(value, dict) or set(value) != {'schema', 'home', 'uid', 'status'} or value['schema'] != 1 or value['status'] not in ('unavailable', 'mismatch', 'ready'):
        raise ValueError('invalid deferred trace admission')
    if not isinstance(value['home'], str) or not Path(value['home']).is_absolute() or not isinstance(value['uid'], int) or value['uid'] < 0:
        raise ValueError('invalid deferred trace identity')
    return value


def save_admission(state, value):
    path = state/'trace-admission.json'
    content = json.dumps(value, sort_keys=True)+'\n'
    if not path.exists() or path.read_text() != content:
        native.write(path, content)


def cancel_admission(state):
    if admission(state) is not None:
        (state/'trace-admission.json').unlink()


def eligible(home, state, uid):
    if home.is_symlink() or not home.is_dir() or home.stat().st_uid != uid:
        return False
    private(state/'native.json')
    baseline = native.load(state/'native.json', 'claude')
    if baseline.get('schema') != 1 or not isinstance(baseline.get('agents'), dict):
        raise ValueError('invalid native ownership state')
    codex = baseline['agents'].get('codex')
    return (isinstance(codex, dict) and 'trace_exporter' not in codex
            and native.load(home/'.codex/config.toml', 'codex').get('otel', {}).get('trace_exporter') == TEMP)


def admission_ready(home, state, selected, agents):
    if not selected or 'codex' not in agents:
        return False
    value = admission(state)
    return (value is not None and value['status'] == 'ready' and value['home'] == str(home)
            and eligible(home, state, value['uid']))


def probe_recognition(runner):
    try:
        proof = recognize(runner=runner)
        return ('ready', proof) if proof is not None else ('mismatch', None)
    except (ProbeUnavailable, OSError, subprocess.TimeoutExpired):
        return 'unavailable', None
    except (ValueError, KeyError, TypeError):
        # Invalid recipes/identities are not transient admission authorization.
        return 'mismatch', None


def matches_prepared(proof, value):
    return proof is not None and all(proof[key] == value[key] for key in ('legacy', 'files', 'container'))


def retry_admission(state, runner=run):
    value = admission(state)
    if value is None:
        return 'idle'
    home = Path(value['home'])
    if not eligible(home, state, value['uid']):
        cancel_admission(state)
        return 'ignored-admission'
    if runner(['systemctl', 'is-active', 'subyard-openlit.service']).strip() != b'active':
        return 'pending-service'
    path = home/'.codex/config.toml'
    _, before, original = config_snapshot(path)
    status, proof = probe_recognition(runner)
    prepared = record(state)
    if status == 'ready' and prepared is not None and not matches_prepared(proof, prepared):
        status = 'mismatch'
    unchanged(path, before, original)
    # Only a request for a fresh owning reconciliation is persisted. No config,
    # exporter, container, ownership or receiver environment is changed here.
    save_admission(state, {**value, 'status': status})
    return 'admission-'+status


def file_identity(path, mode):
    native.safe(path)
    info = path.stat()
    if (info.st_uid, info.st_gid, info.st_mode & 0o777) != (os.getuid(), os.getgid(), mode) or info.st_size > LIMIT:
        raise ValueError('unsafe compatibility recipe')
    return [info.st_dev, info.st_ino, info.st_mtime_ns, info.st_ctime_ns,
            hashlib.sha256(path.read_bytes()).hexdigest()]


def config_identity(info):
    return (info.st_dev, info.st_ino, info.st_uid, info.st_gid, info.st_mode,
            info.st_mtime_ns, info.st_ctime_ns, info.st_size)


def unchanged(path, before, original):
    native.safe(path)
    if config_identity(path.stat()) != config_identity(before) or path.read_bytes() != original:
        raise ValueError('native configuration changed during cutover')


def config_snapshot(path):
    native.safe(path)
    fd = os.open(path, os.O_NOFOLLOW | os.O_RDONLY)
    with os.fdopen(fd, 'rb') as source:
        before = os.fstat(source.fileno())
        if not stat.S_ISREG(before.st_mode) or before.st_size > 4 * 1024 * 1024:
            raise ValueError('unsafe native configuration')
        original = source.read(4 * 1024 * 1024+1)
    unchanged(path, before, original)
    return tomllib.loads(original.decode()), before, original


def yaml_object(path, runner):
    # Compose is already required by this profile; use its native YAML parser.
    # An extension node permits parsing Collector YAML without a second package.
    content = b'x-recipe:\n'+b''.join(b'  '+line+b'\n' for line in path.read_bytes().splitlines())
    content += b'services:\n  parser:\n    image: unused\n'
    parsed = json.loads(runner(['docker', 'compose', '--project-name', 'openlit-cutover-parser',
                               '--env-file', '/dev/null', '-f', '-', 'config', '--format', 'json', '--no-interpolate'], data=content))
    return parsed['x-recipe']


def projection(row):
    # Keep only identity needed for exact stop; never persist env/arguments.
    return {'id': row['Id'], 'image': row['Image'], 'labels': {key: row['Config']['Labels'].get(key) for key in (
        'com.docker.compose.project', 'com.docker.compose.service', 'com.docker.compose.project.working_dir',
        'com.docker.compose.project.config_files', 'com.docker.compose.config-hash')},
        'mounts': row.get('Mounts'), 'ports': row['HostConfig'].get('PortBindings'),
        'networks': sorted(row['NetworkSettings']['Networks'])}


def recognize(legacy=None, runner=run):
    """Recognize only a protected, pinned, trace-only compatibility recipe.

    Names come from the real recipe/labels, never from an invented marker. Any
    unsupported deployed layout remains untouched and requires no registration.
    """
    legacy = LEGACY if legacy is None else legacy
    if legacy != LEGACY or legacy.is_symlink() or not legacy.is_dir():
        return None
    for parent in legacy.parents:
        if parent.is_symlink():
            return None
    info = legacy.stat()
    if (info.st_uid, info.st_gid, info.st_mode & 0o777) != (os.getuid(), os.getgid(), 0o700):
        return None
    recipes = [legacy/name for name in ('compose.yaml', 'compose.yml') if (legacy/name).exists() or (legacy/name).is_symlink()]
    if len(recipes) != 1:
        return None
    recipe = recipes[0]
    files = {recipe.name: file_identity(recipe, 0o600)}
    # An explicit `compose -p` overrides both recipe name and directory default.
    # Discover that effective project only from the unique protected-path resource,
    # then bind both its recipe and the entire project inventory before adoption.
    ids = runner(['docker', 'ps', '-aq', '--no-trunc', '--filter',
                  'label=com.docker.compose.project.working_dir='+str(legacy)]).decode().split()
    if len(ids) != 1 or not re.fullmatch('[a-f0-9]{64}', ids[0]):
        return None
    rows = json.loads(runner(['docker', 'inspect', ids[0]]))
    if len(rows) != 1 or rows[0].get('Id') != ids[0]:
        return None
    row = rows[0]
    labels = row['Config'].get('Labels') or {}
    project = labels.get('com.docker.compose.project')
    if not isinstance(project, str) or not re.fullmatch('[a-z0-9][a-z0-9_-]{0,62}', project) or project == 'subyard-openlit':
        return None
    if labels.get('com.docker.compose.project.working_dir') != str(legacy) or labels.get('com.docker.compose.project.config_files') != str(recipe) or row['Config'].get('Image') != IMAGE:
        return None
    project_ids = runner(['docker', 'ps', '-aq', '--no-trunc', '--filter',
                         'label=com.docker.compose.project='+project]).decode().split()
    if project_ids != ids:
        return None
    compose_args = ['docker', 'compose', '--project-name', project, '--env-file', '/dev/null', '-f', str(recipe)]
    config = json.loads(runner([*compose_args, 'config', '--format', 'json', '--no-interpolate']))
    services = config.get('services', {})
    if config.get('name') != project or len(services) != 1:
        return None
    service, definition = next(iter(services.items()))
    allowed = {'image', 'command', 'volumes', 'ports', 'networks', 'restart', 'logging', 'name', 'mem_limit', 'container_name'}
    if set(definition)-allowed or definition.get('image') != IMAGE or definition.get('command') != ['--config=/etc/otelcol-contrib/config.yaml']:
        return None
    ports = definition.get('ports', [])
    if len(ports) != 1 or ports[0].get('host_ip') != '127.0.0.1' or str(ports[0].get('published')) != '4319' or ports[0].get('target') != 4318 or ports[0].get('protocol', 'tcp') != 'tcp':
        return None
    networks = config.get('networks', {})
    if len(networks) != 1 or len(definition.get('networks', {})) != 1 or set(definition['networks']) != set(networks):
        return None
    if next(iter(networks.values())) != {'name': 'subyard-openlit_default', 'external': True}:
        return None
    mounts = definition.get('volumes', [])
    if len(mounts) != 1:
        return None
    mount = mounts[0]
    source = Path(mount.get('source', ''))
    if source.parent != legacy or mount.get('type') != 'bind' or mount.get('target') != '/etc/otelcol-contrib/config.yaml' or mount.get('read_only') is not True:
        return None
    permissions = source.stat().st_mode & 0o777
    if permissions not in (0o600, 0o644):
        return None
    files[source.name] = file_identity(source, permissions)
    collector = yaml_object(source, runner)
    pipelines = collector.get('service', {}).get('pipelines', {})
    if set(collector)-{'receivers', 'processors', 'exporters', 'service'} or set(collector.get('service', {})) != {'pipelines'} or set(pipelines) != {'traces'}:
        return None
    pipeline = pipelines['traces']
    receivers = collector.get('receivers', {})
    exporters = collector.get('exporters', {})
    # Restrict the compatibility route to this stack, not arbitrary remote data.
    if receivers != {'otlp': {'protocols': {'http': {'endpoint': '0.0.0.0:4318'}}}} or pipeline.get('receivers') != ['otlp']:
        return None
    forwarding = (pipeline.get('exporters') == ['otlphttp/openlit'] and set(exporters) == {'otlphttp/openlit'}
                  and exporters['otlphttp/openlit'].get('endpoint') == 'http://openlit:4318'
                  and not set(exporters['otlphttp/openlit'])-{'endpoint', 'timeout', 'sending_queue', 'retry_on_failure'})
    direct = (pipeline.get('exporters') == ['clickhouse'] and set(exporters) == {'clickhouse'}
              and exporters['clickhouse'].get('endpoint') == 'tcp://clickhouse:9000'
              and exporters['clickhouse'].get('database') == 'openlit'
              and exporters['clickhouse'].get('username') == 'openlit'
              and exporters['clickhouse'].get('traces_table_name', 'otel_traces') == 'otel_traces'
              and not set(exporters['clickhouse'])-{'endpoint', 'database', 'username', 'password', 'traces_table_name', 'timeout', 'sending_queue', 'retry_on_failure', 'create_schema', 'async_insert', 'ttl'})
    if not (forwarding or direct):
        return None
    if set(collector.get('processors', {}))-{'batch', 'memory_limiter'} or set(pipeline)-{'receivers', 'processors', 'exporters'}:
        return None
    image = json.loads(runner(['docker', 'image', 'inspect', IMAGE]))
    expected_hash = runner([*compose_args, 'config', '--hash', service]).decode().split()
    value = projection(row)
    labels = value['labels']
    if (labels['com.docker.compose.project'] != project or labels['com.docker.compose.service'] != service or labels['com.docker.compose.project.working_dir'] != str(legacy) or labels['com.docker.compose.project.config_files'] != str(recipe) or expected_hash != [service, labels['com.docker.compose.config-hash']]):
        return None
    if len(image) != 1 or row['Image'] != image[0]['Id'] or row['Config'].get('Image') != IMAGE or row['Config'].get('Cmd') != definition['command']:
        return None
    host = row['HostConfig']
    if host.get('Privileged') or host.get('CapAdd') or host.get('Devices') or host.get('NetworkMode') == 'host':
        return None
    live_mounts = row.get('Mounts', [])
    if len(live_mounts) != 1 or live_mounts[0].get('Type') != 'bind' or live_mounts[0].get('Source') != str(source) or live_mounts[0].get('Destination') != mount['target'] or live_mounts[0].get('RW') is not False:
        return None
    if value['ports'] != {'4318/tcp': [{'HostIp': '127.0.0.1', 'HostPort': '4319'}]} or value['networks'] != ['subyard-openlit_default'] or row['State'].get('Status') not in ('running', 'exited'):
        return None
    return {'legacy': str(legacy), 'files': files, 'container': value}


def prepare(home, state, agents, writer, runner=run, uid=None):
    owned(state)
    uid = home.stat().st_uid if uid is None else uid
    if home.is_symlink() or not home.is_dir() or home.stat().st_uid != uid:
        raise ValueError('unsafe developer home')
    value = record(state)
    intent = admission(state)
    if intent is not None and (intent['home'] != str(home) or intent['uid'] != uid):
        raise ValueError('deferred trace user changed')
    if 'codex' not in agents:
        cancel_admission(state)
    backup = state/'trace-cutover-backup.json'
    env = state/'trace-cutover.env'
    for path in (backup, env, state/'native.json'):
        native.safe(path)
        if path.exists():
            private(path)
    if backup.exists() and json.loads(backup.read_text()) != {'schema': 1, 'trace_exporter': TEMP}:
        raise ValueError('invalid temporary leaf backup')
    if value is not None and not backup.exists():
        raise ValueError('temporary leaf backup missing')
    if env.exists() and not re.fullmatch('OPENLIT_TRACE_CUTOVER_ID=(none|[a-f0-9]{32})\n', env.read_text()):
        raise ValueError('invalid trace environment')
    if value is not None and value['home'] != str(home):
        raise ValueError('cutover user changed')
    if 'codex' in agents and value is None:
        ownership = native.load(state/'native.json', 'claude').get('agents', {}).get('codex', {})
        path = home/'.codex/config.toml'
        native.safe(path)
        config, before, original = config_snapshot(path) if path.exists() else ({}, None, None)
        if 'trace_exporter' not in ownership and config.get('otel', {}).get('trace_exporter') == TEMP:
            status, proof = probe_recognition(runner)
            unchanged(path, before, original)
            if status == 'unavailable' or intent is not None:
                save_admission(state, {'schema': 1, 'home': str(home), 'uid': uid, 'status': status})
            if proof is not None:
                value = {'schema': 1, 'phase': 'prepared', 'home': str(home), 'uid': uid, 'token': secrets.token_hex(16), **proof}
                # The only retained user data is this exact leaf, not a config copy.
                native.write(backup, json.dumps({'schema': 1, 'trace_exporter': TEMP})+'\n')
                save(state, value)
                cancel_admission(state)
        elif intent is not None:
            cancel_admission(state)
    if value is not None and value['phase'] == 'prepared' and 'codex' in agents:
        path = home/'.codex/config.toml'
        native.safe(path)
        obj, before, original = config_snapshot(path) if path.exists() else ({}, None, None)
        if obj.get('otel', {}).get('trace_exporter') == TEMP:
            # Revalidate ownership before deletion, including retries after interruption.
            status, proof = probe_recognition(runner)
            unchanged(path, before, original)
            if status == 'unavailable':
                save_admission(state, {'schema': 1, 'home': str(home), 'uid': uid, 'status': status})
            elif not matches_prepared(proof, value):
                raise ValueError('compatibility receiver ownership changed')
            else:
                del obj['otel']['trace_exporter']
                native.write(path, writer.dumps(obj), before.st_uid, before.st_gid,
                             lambda: unchanged(path, before, original))
                cancel_admission(state)
        elif 'trace_exporter' in obj.get('otel', {}):
            current = obj['otel']['trace_exporter']
            ownership = native.load(state/'native.json', 'claude').get('agents', {}).get('codex', {}).get('trace_exporter')
            if ownership is None or ownership.get('present') or current != native.CODEX['trace_exporter'] or ownership.get('managed') != current:
                raise ValueError('native trace settings changed before adoption')
            cancel_admission(state)
    token = value['token'] if value is not None else 'none'
    content = 'OPENLIT_TRACE_CUTOVER_ID='+token+'\n'
    if not env.exists() or env.read_text() != content:
        native.write(env, content)


def scan(uid, proc=Path('/proc')):
    """Read safe identities only, fail closed when an owning process is unreadable."""
    identities = {}
    entries = list(proc.iterdir())
    if len(entries) > 65536:
        raise ValueError('process inventory too large')
    for path in entries:
        if not path.name.isdecimal():
            continue
        try:
            if path.stat().st_uid != uid:
                continue
            comm = (path/'comm').read_text().strip()
            exe = os.readlink(path/'exe')
            if comm != 'codex' and Path(exe.removesuffix(' (deleted)')).name != 'codex':
                continue
            fields = (path/'stat').read_text().rsplit(')', 1)[1].split()
            identities[path.name] = int(fields[19])
        except FileNotFoundError:
            continue  # Process exited during inventory.
        except (PermissionError, IndexError):
            raise ValueError('process fence unavailable') from None
    return identities


def fence(home, state, uid, scanner=scan, proc=Path('/proc')):
    value = record(state)
    if value is None or value['phase'] != 'prepared':
        return
    if value['home'] != str(home) or value['uid'] != uid or home.stat().st_uid != uid:
        raise ValueError('cutover user changed')
    ownership = native.load(state/'native.json', 'claude').get('agents', {}).get('codex', {}).get('trace_exporter')
    current = native.load(home/'.codex/config.toml', 'codex').get('otel', {}).get('trace_exporter')
    if ownership is None or ownership['managed'] != native.CODEX['trace_exporter'] or ownership['present'] or current != ownership['managed']:
        return
    try:
        processes = scanner(uid)
        # Capture AFTER native write and scan: a backend racing through adoption
        # is held conservatively, including retries after unavailable metadata.
        barrier = math.ceil(float((proc/'uptime').read_text().split()[0])*os.sysconf('SC_CLK_TCK'))
        boot = (proc/'sys/kernel/random/boot_id').read_text().strip()
        if barrier < 0 or not re.fullmatch('[a-f0-9-]{36}', boot):
            raise ValueError('process clock unavailable')
    except (ValueError, OSError, IndexError):
        return 'pending-process-metadata'
    value.update(phase='adopted', barrier=barrier, boot=boot, processes=processes, cutover_ns=time.time_ns())
    save(state, value)


def new_flow(state, value, runner=run):
    token = value['token']
    ns = value['cutover_ns']
    if not re.fullmatch('[a-f0-9]{32}', token) or not isinstance(ns, int) or ns < 0:
        raise ValueError('invalid trace evidence identity')
    query = ("SELECT count() FROM otel_traces WHERE ServiceName IN ('codex_cli_rs', 'codex_exec', 'codex-app-server') AND TraceId != '' "
             f"AND ResourceAttributes['{ATTRIBUTE}'] = '{token}' AND Timestamp > fromUnixTimestamp64Nano({ns})")
    result = runner(['docker', 'compose', '--project-directory', str(state), '--env-file', str(state/'database.env'),
                     '-f', str(state/'compose.yaml'), 'exec', '-T', 'clickhouse', 'bash', '-c',
                     'exec clickhouse-client --user openlit --password "$CLICKHOUSE_PASSWORD" --database openlit --query "$1"',
                     'openlit-cutover', query])
    return re.fullmatch(rb'[0-9]+\s*', result) is not None and int(result) > 0


def finish(home, state, uid, runner=run, scanner=scan, proc=Path('/proc')):
    value = record(state)
    if value is None:
        return retry_admission(state, runner)
    if value['phase'] == 'retired':
        return 'idle'
    if value['home'] != str(home) or value['uid'] != uid or home.stat().st_uid != uid:
        return 'pending-user'
    if value['phase'] == 'prepared' and admission(state) is not None:
        return retry_admission(state, runner)
    # A crash after config adoption but before fencing must retain every currently
    # running backend by building a later barrier, never infer a prior snapshot.
    fence(home, state, uid, scanner, proc)
    value = record(state)
    if value['phase'] != 'adopted':
        return 'pending-adoption'
    if runner(['systemctl', 'is-active', 'subyard-openlit.service']).strip() != b'active':
        return 'pending-service'
    current = native.load(home/'.codex/config.toml', 'codex').get('otel', {}).get('trace_exporter')
    ownership = native.load(state/'native.json', 'claude').get('agents', {}).get('codex', {}).get('trace_exporter')
    if current != native.CODEX['trace_exporter'] or ownership is None or ownership.get('managed') != current or ownership.get('present'):
        return 'pending-settings'
    boot = (proc/'sys/kernel/random/boot_id').read_text().strip()
    processes = scanner(uid)
    if boot == value['boot'] and any(ticks <= value['barrier'] or value.get('processes', {}).get(pid) == ticks for pid, ticks in processes.items()):
        return 'pending-sessions'
    if not new_flow(state, value, runner):
        return 'pending-traces'
    proof = recognize(runner=runner)
    if proof is None or any(proof[key] != value[key] for key in ('legacy', 'files', 'container')):
        return 'pending-ownership'
    # Only the original exact container is stopped. Keep its files, network and data.
    runner(['docker', 'stop', '--time', '20', value['container']['id']], timeout=25)
    rows = json.loads(runner(['docker', 'inspect', value['container']['id']]))
    if len(rows) != 1 or projection(rows[0]) != value['container'] or rows[0]['State'].get('Status') != 'exited':
        return 'pending-stop'
    value['phase'] = 'retired'
    save(state, value)
    return 'retired'


def main():
    if len(sys.argv) > 1 and sys.argv[1] in ('locked-guest', 'verify-lock'):
        try:
            if sys.argv[1] == 'locked-guest':
                fd = acquire()
                try:
                    lock_identity(fd)
                    result = subprocess.run(['/bin/bash', sys.argv[2], '--runtime-lock-fd', str(fd), *sys.argv[3:]], pass_fds=(fd,), check=False)
                    sys.exit(result.returncode)
                finally:
                    os.close(fd)
            else:
                fd = int(sys.argv[2])
                lock_identity(fd)
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                lock_identity(fd)
            return
        except (ValueError, OSError, IndexError):
            print('openlit: unsafe or busy lifecycle lock', file=sys.stderr)
            sys.exit(1)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('mode', choices=('prepare', 'fence', 'finish', 'cancel'))
    parser.add_argument('--home', type=Path)
    parser.add_argument('--state', type=Path, required=True)
    parser.add_argument('--uid', type=int)
    parser.add_argument('--agents', default='')
    args = parser.parse_args()
    try:
        if args.mode == 'finish':
            # Finish owns its lock. prepare/fence are invoked by locked guest.sh.
            fd = acquire(wait=0)
            try:
                value = record(args.state) or admission(args.state)
                if value is not None:
                    home = Path(value['home'])
                    if home.is_symlink() or not home.is_dir():
                        raise ValueError('unsafe developer home')
                    finish(home, args.state, home.stat().st_uid)
            finally:
                os.close(fd)
        elif args.mode == 'fence':
            if args.home is None or args.uid is None:
                raise ValueError('missing cutover identity')
            fence(args.home, args.state, args.uid)
        elif args.mode == 'cancel':
            cancel_admission(args.state)
        else:
            if args.home is None:
                raise ValueError('missing cutover identity')
            import tomli_w
            prepare(args.home, args.state, args.agents.split(), tomli_w, uid=args.uid)
    except (ValueError, OSError, KeyError, TypeError, subprocess.TimeoutExpired, json.JSONDecodeError):
        # Pending migration is never a service failure; fail closed for retirement.
        if args.mode != 'finish':
            print('openlit: protected trace migration state conflict', file=sys.stderr)
            sys.exit(1)


if __name__ == '__main__':
    main()
