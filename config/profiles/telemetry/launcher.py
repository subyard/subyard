#!/usr/bin/env python3
"""Explicit managed native runs; native commands and their exit status stay native."""
import argparse
import contextlib
import datetime
import fcntl
import hashlib
import json
import os
import re
from pathlib import Path
import signal
import stat
import subprocess
import sys
import threading
import time
import uuid

import processes
import records


MAX_RUNS = 4096
MAX_RUN_BYTES = 16*1024*1024
MAX_RUN_PAYLOAD = 16384


@contextlib.contextmanager
def manifest_lock(root):
    fd = os.open(root/'.lock', os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'rb') as lock:
        info = os.fstat(lock.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600:
            raise ValueError('private file required')
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


def manifest_health(data_dir, root, failed=False):
    path = root/'health'
    health = records.read_private(path) if path.exists() else None
    if failed:
        now = datetime.datetime.now(datetime.timezone.utc).isoformat()
        health = health or dict(event_id=str(uuid.uuid4()), first_time=now, count=0)
        health.update(last_time=now, count=min(2**63-1, health['count']+1))
        records.atomic_json(path, health)
    elif health and records.emit(data_dir, 'gap', dict(reason='run_manifest_capacity', quality='partial',
            missing=health['count'], coverage_start=health['first_time'], coverage_end=health['last_time']),
            event_id=health['event_id'], source_time=health['first_time']):
        path.unlink()


def admit_manifest(data_dir, run, reserve=False):
    """Reserve a complete bounded snapshot before any managed group is created."""
    root = None
    try:
        directory = records.private_directory(data_dir)
        root = records.private_directory(directory/'runs')
        identity = run['agent_run_id']
        if not records.IDENTITY.fullmatch(identity):
            raise ValueError('invalid run identity')
        value = dict(run)
        if reserve:
            value.update(manifest_state='reserved', launcher_pid=os.getpid(),
                         launcher_start_ticks=processes.start_ticks(os.getpid()))
        size = len((json.dumps(value, sort_keys=True, separators=(',', ':'))+'\n').encode())
        with manifest_lock(root):
            path = root/(identity+'.json')
            if size > MAX_RUN_PAYLOAD or path.exists() or path.is_symlink():
                manifest_health(data_dir, root, True)
                return False
            count = total = 0
            deadline = time.monotonic()+.1
            with os.scandir(root) as entries:
                for scanned, item in enumerate(entries, 1):
                    if scanned > MAX_RUNS+16 or time.monotonic() > deadline:
                        manifest_health(data_dir, root, True)
                        return False
                    if not item.name.endswith('.json'):
                        continue
                    pending = Path(item.path)
                    try:
                        entry = records.read_private(pending)
                        length = pending.stat().st_size
                    except FileNotFoundError:
                        continue  # The observer may finish an admitted run.
                    if not isinstance(entry, dict) or length > MAX_RUN_PAYLOAD:
                        raise ValueError('run payload limit')
                    total += MAX_RUN_PAYLOAD if entry.get('manifest_state') == 'reserved' else length
                    count += 1
                    if count >= MAX_RUNS or total+(MAX_RUN_PAYLOAD if reserve else size) > MAX_RUN_BYTES:
                        manifest_health(data_dir, root, True)
                        return False
            if count+1 > MAX_RUNS or (MAX_RUN_PAYLOAD if reserve else size)+total > MAX_RUN_BYTES:
                manifest_health(data_dir, root, True)
                return False
            records.atomic_json(path, value)
            try:
                manifest_health(data_dir, root)
            except (OSError, ValueError, KeyError):
                pass  # The admitted snapshot remains durable if health replay fails.
        return True
    except (OSError, ValueError, KeyError, TypeError):
        return False


def finish_manifest(data_dir, run):
    root = records.private_directory(Path(data_dir)/'runs')
    with manifest_lock(root):
        path = root/(run['agent_run_id']+'.json')
        previous = records.read_private(path)
        if not isinstance(previous, dict) or previous.get('manifest_state') != 'reserved' or previous.get('launcher_pid') != os.getpid() or previous.get('launcher_start_ticks') != processes.start_ticks(os.getpid()):
            raise ValueError('run reservation identity')
        if len((json.dumps(run, sort_keys=True, separators=(',', ':'))+'\n').encode()) > MAX_RUN_PAYLOAD:
            raise ValueError('run payload limit')
        records.atomic_json(path, run)


def discard_reservation(data_dir, run):
    try:
        root = records.private_directory(Path(data_dir)/'runs')
        with manifest_lock(root):
            path = root/(run['agent_run_id']+'.json')
            previous = records.read_private(path)
            if isinstance(previous, dict) and previous.get('manifest_state') == 'reserved' and previous.get('launcher_pid') == os.getpid() and previous.get('launcher_start_ticks') == processes.start_ticks(os.getpid()):
                path.unlink()
    except (OSError, ValueError, KeyError):
        pass  # A failed filesystem retains the reservation for observer recovery.


def boot_id(proc=Path('/proc')):
    return (proc/'sys/kernel/random/boot_id').read_text().strip()


def host_id():
    try:
        return hashlib.sha256(Path('/etc/machine-id').read_bytes().strip()).hexdigest()
    except OSError:
        return 'boot-'+boot_id()


def context(args, environ):
    run_id = str(uuid.uuid4())
    result = {'agent_run_id': run_id, 'agent': args.agent, 'agent_version': args.agent_version,
              'parent_run_id': environ.get('SUBYARD_AGENT_RUN_ID'), 'task_id': args.task_id or environ.get('SUBYARD_TASK_ID'),
              'project_id': args.project_id or environ.get('SUBYARD_PROJECT_ID'),
              'worktree_id': args.worktree_id or environ.get('SUBYARD_WORKTREE_ID'),
              'yard_id': args.yard_id or environ.get('SUBYARD_YARD_NAME'),
              'host_id': args.host_id or environ.get('SUBYARD_HOST_ID') or host_id(), 'boot_id': boot_id()}
    for name, value in result.items():
        if value is not None and (len(value) > 128 or not records.IDENTITY.fullmatch(value)):
            raise ValueError('invalid '+name)
    return result


def native_arguments(agent, arguments, run_id):
    if agent != 'codex':
        return arguments
    overrides = []
    for signal_name in ('exporter', 'metrics_exporter', 'trace_exporter'):
        overrides += ['-c', 'otel.'+signal_name+'.otlp-http.headers.x-subyard-agent-run-id='+json.dumps(run_id)]
    return [arguments[0], *overrides, *arguments[1:]]


def environment(data_dir, run, environ):
    result = dict(environ)
    for field, name in [('agent_run_id', 'SUBYARD_AGENT_RUN_ID'), ('task_id', 'SUBYARD_TASK_ID'),
                        ('project_id', 'SUBYARD_PROJECT_ID'), ('worktree_id', 'SUBYARD_WORKTREE_ID'),
                        ('yard_id', 'SUBYARD_YARD_NAME'), ('host_id', 'SUBYARD_HOST_ID')]:
        if run.get(field) is not None:
            result[name] = run[field]
    result['SUBYARD_TELEMETRY_DATA_DIR'] = str(data_dir)
    if run['agent'] == 'claude':
        fields = [entry for entry in result.get('OTEL_RESOURCE_ATTRIBUTES', '').split(',')
                  if entry and not entry.startswith('subyard.agent_run.id=')]
        result['OTEL_RESOURCE_ATTRIBUTES'] = ','.join([*fields, 'subyard.agent_run.id='+run['agent_run_id']])
        for name in ('OTEL_EXPORTER_OTLP_HEADERS', 'OTEL_EXPORTER_OTLP_LOGS_HEADERS',
                     'OTEL_EXPORTER_OTLP_METRICS_HEADERS', 'OTEL_EXPORTER_OTLP_TRACES_HEADERS'):
            if name == 'OTEL_EXPORTER_OTLP_HEADERS' or name in result:
                headers = [entry for entry in result.get(name, '').split(',')
                           if entry and entry.partition('=')[0].strip().lower() != 'x-subyard-agent-run-id']
                result[name] = ','.join([*headers, 'x-subyard-agent-run-id='+run['agent_run_id']])
    return result


def scope_parent(data_dir, run, root, proc=Path('/proc'), cgroups=Path('/sys/fs/cgroup')):
    candidates = processes.delegated_parents(proc, cgroups)
    if run.get('parent_run_id'):
        path = Path(data_dir)/'runs'/(run['parent_run_id']+'.json')
        try:
            previous = records.read_private(path)
            if previous.get('cgroup') and previous['boot_id'] == run['boot_id']:
                candidate = Path(previous['cgroup'])
                info = candidate.stat()
                if [info.st_dev, info.st_ino] != previous['cgroup_identity']:
                    raise ValueError('parent scope generation changed')
                source = cgroups/processes.membership(proc).relative_to('/')
                if not source.is_relative_to(candidate):
                    raise ValueError('parent scope outside caller hierarchy')
                if candidate not in candidates:
                    if not {'cpu', 'memory', 'io'}.issubset((candidate/'cgroup.controllers').read_text().split()):
                        raise ValueError('parent scope controllers unavailable')
                    if not os.access(candidate, os.W_OK) or not os.access(candidate/'cgroup.procs', os.W_OK):
                        raise ValueError('parent scope permissions unavailable')
                    raise ValueError('parent scope not delegated')
                candidates.remove(candidate)
                candidates.insert(0, candidate)
                run['parent_scope_id'] = run['parent_run_id']+':scope'
        except FileNotFoundError:
            pass
    if root not in candidates:
        candidates.append(root)
    for parent in candidates:
        try:
            processes.safe_placement(parent, proc, cgroups)
        except (OSError, ValueError):
            continue
        return parent
    raise ValueError('safe caller delegation unavailable')


def scope_failure(error):
    reasons = {text: text.replace(' ', '_') for text in (
        'stale delegation', 'delegation identity', 'delegation manager unavailable', 'delegation manager membership',
        'parent scope generation changed', 'parent scope outside caller hierarchy', 'parent scope controllers unavailable',
        'parent scope permissions unavailable', 'parent scope not delegated', 'safe caller delegation unavailable',
        'accounting controllers unavailable', 'scope placement failed', 'inherited ceiling', 'inherited locality ceiling',
        'migration delegation unavailable', 'unsafe delegated group', 'unified cgroup unavailable',
        'private directory required', 'private file required', 'unsafe directory', 'unsafe file')}
    reason = reasons.get(error.args[0], 'unclassified') if error.args and isinstance(error.args[0], str) else 'unclassified'
    source, line = 'python', 0
    frame = error.__traceback__
    while frame:
        name = Path(frame.tb_frame.f_code.co_filename).name
        if name in ('launcher.py', 'processes.py', 'records.py'):
            source, line = name, frame.tb_lineno
        frame = frame.tb_next
    kind = type(error).__name__ if type(error).__name__ in ('ValueError', 'KeyError', 'OSError', 'PermissionError', 'FileNotFoundError', 'ProcessLookupError') else 'Error'
    number = error.errno if isinstance(error, OSError) and isinstance(error.errno, int) and 0 <= error.errno <= 4095 else 0
    return f'reason={reason} error={kind} errno={number} source={source}:{line}'


def create_scope(data_dir, run, proc=Path('/proc'), cgroups=Path('/sys/fs/cgroup')):
    delegation = records.read_private(Path(data_dir)/'delegation.json')
    if delegation['boot_id'] != boot_id(proc):
        raise ValueError('stale delegation')
    root = Path(delegation['path'])
    info = root.stat()
    if [info.st_dev, info.st_ino] != delegation['identity'] or info.st_uid != os.getuid() or root.is_symlink():
        raise ValueError('delegation identity')
    manager = root/'manager'
    manager_pid = delegation['pid']
    if processes.start_ticks(manager_pid, proc) != delegation['start_ticks']:
        raise ValueError('delegation manager unavailable')
    if str(manager_pid) not in (manager/'cgroup.procs').read_text().split():
        raise ValueError('delegation manager membership')
    parent = scope_parent(data_dir, run, root, proc, cgroups)
    group = parent/('run-'+run['agent_run_id'])
    group.mkdir(mode=0o700)
    try:
        available = set((group/'cgroup.controllers').read_text().split())
        if not {'cpu', 'memory', 'io'}.issubset(available):
            raise ValueError('accounting controllers unavailable')
        (group/'cgroup.subtree_control').write_text('+cpu +memory +io'+(' +pids' if 'pids' in available else ''))
        (group/'workload').mkdir(mode=0o700)
        info = group.stat()
        parent_info = parent.stat()
        run.update(cgroup=str(group), cgroup_identity=[info.st_dev, info.st_ino],
                   scope_root=str(parent), scope_root_identity=[parent_info.st_dev, parent_info.st_ino])
    except Exception:
        group.rmdir()
        raise
    return group/'workload'


def enable_delegation(data_dir):
    root = Path('/sys/fs/cgroup')/processes.membership().relative_to('/')
    # Only the service explicitly installed with Delegate=yes may call this.
    if root.stat().st_uid != os.getuid() or root == Path('/sys/fs/cgroup'):
        raise ValueError('delegation unavailable')
    members = (root/'cgroup.procs').read_text().split()
    if members != [str(os.getpid())]:
        raise ValueError('delegation is not exclusive')
    manager = root/'manager'
    manager.mkdir(mode=0o700, exist_ok=True)
    (manager/'cgroup.procs').write_text(str(os.getpid()))
    available = set((root/'cgroup.controllers').read_text().split())
    if not {'cpu', 'memory', 'io'}.issubset(available):
        raise ValueError('accounting controllers unavailable')
    (root/'cgroup.subtree_control').write_text('+cpu +memory +io'+(' +pids' if 'pids' in available else ''))
    # Limit only the sampler leaf. Agent workloads remain sibling descendants
    # with their caller's validated ancestor ceilings.
    (manager/'memory.max').write_text(str(256*1024*1024))
    (manager/'cpu.max').write_text('25000 100000')
    if (manager/'pids.max').exists():
        (manager/'pids.max').write_text('64')
    identity = processes.process(os.getpid())
    info = root.stat()
    records.atomic_json(Path(data_dir)/'delegation.json', {'path': str(root), 'identity': [info.st_dev, info.st_ino],
                        'pid': os.getpid(), 'start_ticks': identity['start_ticks'], 'boot_id': boot_id()})


def removed_scope(run):
    for name, identity in (('scope_root', 'scope_root_identity'), ('cgroup', 'cgroup_identity')):
        if not run.get(name):
            continue
        try:
            info = Path(run[name]).lstat()
        except FileNotFoundError:
            return name+'_missing'
        except OSError:
            return None  # Permission and transient filesystem failures prove no removal.
        if [info.st_dev, info.st_ino] != run[identity] or info.st_uid != os.getuid() or not stat.S_ISDIR(info.st_mode):
            return name+'_generation_changed'
    return None


def observe(data_dir, once=False, until_empty=False, interval=5):
    samplers = {}
    sampled_sources = 0
    source_thread = None
    scan = None
    deferred = []
    while True:
        root = records.private_directory(Path(data_dir)/'runs')
        pending = []
        total = 0
        batch, deferred = deferred, []
        if not batch:
            if scan is None:
                scan = os.scandir(root)
            # Keep the directory iterator across ticks: a retained foreign
            # prefix cannot starve later snapshots in an oversized directory.
            for _ in range(MAX_RUNS+16):
                try:
                    entry = next(scan)
                except StopIteration:
                    scan.close()
                    scan = None
                    break
                if entry.name.endswith('.json'):
                    batch.append(Path(entry.path))
                if len(batch) >= MAX_RUNS:
                    break
        for index, path in enumerate(batch):
            try:
                run = records.read_private(path)
                if not isinstance(run, dict) or run.get('agent_run_id') != path.stem or not isinstance(run.get('boot_id'), str) or not isinstance(run.get('cgroup', ''), str):
                    continue
                length = path.stat().st_size
                if length > MAX_RUN_PAYLOAD:
                    continue
                if total+length > MAX_RUN_BYTES:
                    deferred = batch[index:]
                    break
                total += length
                pending.append((path, run))
            except (OSError, ValueError):
                continue
        try:
            with manifest_lock(root):
                manifest_health(data_dir, root)
        except (OSError, ValueError, KeyError, TypeError):
            pass
        if until_empty and not pending and not batch and scan is None:
            return 0
        if not pending and not batch and scan is None and (Path(data_dir)/'drain.json').exists():
            try:
                delegation = records.read_private(Path(data_dir)/'delegation.json')
                if list(Path(delegation['path']).glob('run-*')):
                    records.emit(data_dir, 'gap', {'reason': 'scope_manifest_missing', 'quality': 'partial'})
                else:
                    return 0
            except (OSError, ValueError, KeyError):
                return 0
        for path, run in sorted(pending, key=lambda pair: len(pair[1].get('cgroup', '')), reverse=True):
            run_id = run['agent_run_id']
            try:
                if run.get('manifest_state') == 'reserved':
                    try:
                        alive = run['boot_id'] == boot_id() and processes.start_ticks(run['launcher_pid']) == run['launcher_start_ticks']
                    except (FileNotFoundError, ProcessLookupError):
                        alive = False
                    if not alive and records.emit(data_dir, 'gap', {'agent_run_id': run_id, 'reason': 'run_reservation_interrupted', 'quality': 'partial'}):
                        path.unlink()
                    continue
                if run['boot_id'] != boot_id():
                    if records.emit(data_dir, 'gap', {'agent_run_id': run_id, 'reason': 'boot_changed_before_final', 'quality': 'partial'}):
                        path.unlink()
                        samplers.pop(run_id, None)
                    continue
                missing = removed_scope(run)
                if missing:
                    body = {'agent_run_id': run_id, 'object_id': run_id+':scope', 'quality': 'partial',
                            'scope_quality': 'partial', 'reason': 'scope_removed_before_final',
                            'missing': 'final_tail', 'source': missing}
                    if records.emit(data_dir, 'gap', body) and records.emit(data_dir, 'run',
                            {**body, 'state': 'resources_unknown', 'resource_completed': False}):
                        path.unlink()
                        samplers.pop(run_id, None)
                    continue
                if run_id not in samplers and len(samplers) >= MAX_RUNS:
                    continue
                sampler = samplers.setdefault(run_id, processes.Sampler(data_dir, run)) if run_id not in samplers else samplers[run_id]
                if sampler.poll():
                    path.unlink()
                    samplers.pop(run_id, None)
            except (OSError, ValueError, KeyError):
                records.emit(data_dir, 'gap', {'agent_run_id': run_id, 'reason': 'scope_observation_unavailable', 'quality': 'partial'})
        if once:
            if scan is not None:
                scan.close()
            return 0
        if time.monotonic()-sampled_sources >= 30:
            if source_thread is None or not source_thread.is_alive():
                def poll_sources():
                    try:
                        import sources
                        sources.poll(data_dir)
                    except (ImportError, OSError, ValueError):
                        pass
                source_thread = threading.Thread(target=poll_sources, daemon=True)
                source_thread.start()
            sampled_sources = time.monotonic()
        time.sleep(interval)


def detached_observer(data_dir):
    # Standalone fallback has no service lifetime guarantee; its scope is partial.
    pid = os.fork()
    if pid:
        os.waitpid(pid, 0)
        return
    os.setsid()
    if os.fork():
        os._exit(0)
    null = os.open('/dev/null', os.O_RDWR)
    for descriptor in (0, 1, 2):
        os.dup2(null, descriptor)
    if null > 2:
        os.close(null)
    try:
        lock_fd = os.open(Path(data_dir)/'manager.lock', os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
        fcntl.flock(lock_fd, fcntl.LOCK_EX)
        observe(data_dir, until_empty=True)
    finally:
        os._exit(0)



def gated_child(command, env):
    read_fd, write_fd = os.pipe()
    try:
        pid = os.fork()
    except OSError:
        os.close(read_fd)
        os.close(write_fd)
        raise
    if pid == 0:
        os.close(write_fd)
        gate = os.read(read_fd, 1)
        os.close(read_fd)
        if gate != b'1':
            os._exit(125)
        try:
            os.execvpe(command[0], command, env)
        except OSError:
            os._exit(127)
    os.close(read_fd)
    return pid, write_fd


def launch(args):
    command = args.command[1:] if args.command[:1] == ['--'] else args.command
    if not command:
        raise ValueError('native command required')
    data_dir = Path(args.data_dir).absolute()
    run = context(args, os.environ)
    run['shared'] = getattr(args, 'shared_backend', False) or any(value in ('app-server', '--remote') for value in command[1:])
    if run['agent_version'] == 'unknown' and args.agent in ('codex', 'claude'):
        try:
            version = subprocess.run([command[0], '--version'], stdin=subprocess.DEVNULL,
                                     stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=2)
            match = re.search(rb'\b[0-9][A-Za-z0-9._+-]{0,63}', version.stdout[:4096])
            if version.returncode == 0 and match:
                run['agent_version'] = match[0].decode('ascii')
        except (OSError, subprocess.SubprocessError):
            pass
    tracked = admit_manifest(data_dir, run, reserve=True)
    if not tracked:
        print('Telemetry tracking unavailable; reason=run_manifest_admission', file=sys.stderr)
        records.emit(data_dir, 'gap', {'agent_run_id': run['agent_run_id'], 'reason': 'run_manifest_unavailable', 'quality': 'partial'})
        if args.scope_mode == 'require':
            return 2
    group = None
    try:
        records.private_directory(data_dir)
        records.private_directory(data_dir/'runs')
        if tracked and args.scope_mode != 'partial':
            group = create_scope(data_dir, run)
    except (OSError, ValueError, KeyError) as error:
        if args.scope_mode == 'require':
            discard_reservation(data_dir, run)
            print('Managed telemetry scope unavailable; native command was not started; '+scope_failure(error), file=sys.stderr)
            return 2
        records.emit(data_dir, 'gap', {'agent_run_id': run['agent_run_id'], 'reason': 'managed_scope_unavailable', 'quality': 'partial'})
    record = {**run, 'state': 'starting', 'scope_quality': 'managed' if group else 'partial'}
    records.emit(data_dir, 'run', record)
    print('agent_run_id='+run['agent_run_id'], file=sys.stderr, flush=True)
    if group:
        try:
            run['scope_start_time'] = datetime.datetime.now(datetime.timezone.utc).isoformat(timespec='microseconds')
            processes.Sampler(data_dir, run).capture_baseline()
        except (OSError, ValueError, KeyError):
            records.emit(data_dir, 'gap', {'agent_run_id': run['agent_run_id'], 'reason': 'scope_baseline_unavailable', 'quality': 'partial'})
    env = environment(data_dir, run, os.environ)
    command = native_arguments(args.agent, command, run['agent_run_id'])
    try:
        pid, write_fd = gated_child(command, env)
    except OSError:
        discard_reservation(data_dir, run)
        if group:
            try:
                group.rmdir()
                group.parent.rmdir()
            except OSError:
                pass
        return 127
    try:
        identity = processes.process(pid)
        run.update(pid=pid, start_ticks=identity['start_ticks'])
        try:
            if group:
                (group/'cgroup.procs').write_text(str(pid))
                if str(pid) not in (group/'cgroup.procs').read_text().split():
                    raise ValueError('scope placement failed')
        except (OSError, ValueError) as error:
            if args.scope_mode == 'require':
                records.emit(data_dir, 'run', {'agent_run_id': run['agent_run_id'], 'state': 'not_started', 'scope_quality': 'unavailable'})
                os.close(write_fd)
                write_fd = -1
                os.waitpid(pid, 0)
                try:
                    group.rmdir()
                    group.parent.rmdir()
                except OSError:
                    pass
                discard_reservation(data_dir, run)
                print('Managed telemetry placement unavailable; native command was not started; '+scope_failure(error), file=sys.stderr)
                return 2
            group = None
            run.pop('cgroup', None)
            run.pop('cgroup_identity', None)
            records.emit(data_dir, 'gap', {'agent_run_id': run['agent_run_id'], 'reason': 'scope_placement_unavailable', 'quality': 'partial'})
            records.emit(data_dir, 'run', {'agent_run_id': run['agent_run_id'], 'state': 'starting', 'scope_quality': 'partial'})
        try:
            if tracked:
                finish_manifest(data_dir, run)
        except (OSError, ValueError):
            records.emit(data_dir, 'gap', {'agent_run_id': run['agent_run_id'], 'reason': 'run_manifest_unavailable', 'quality': 'partial'})
            # Nothing native has executed behind the gate. Reap this child,
            # then default mode may start once in the unchanged caller leaf.
            os.close(write_fd)
            write_fd = -1
            os.waitpid(pid, 0)
            if group:
                try:
                    group.rmdir()
                    group.parent.rmdir()
                except OSError:
                    pass
            discard_reservation(data_dir, run)
            tracked = False
            group = None
            refused = args.scope_mode == 'require'
            records.emit(data_dir, 'run', {'agent_run_id': run['agent_run_id'], 'state': 'not_started' if refused else 'starting', 'scope_quality': 'unavailable' if refused else 'partial'})
            if refused:
                return 2
            try:
                pid, write_fd = gated_child(command, env)
            except OSError:
                return 127
        if tracked and not group:
            try:
                detached_observer(data_dir)
            except OSError:
                pass
        os.write(write_fd, b'1')
    except (OSError, ValueError, KeyError):
        if write_fd >= 0:
            os.close(write_fd)
            write_fd = -1
        os.waitpid(pid, 0)
        if group:
            try:
                group.rmdir()
                group.parent.rmdir()
            except OSError:
                pass
        discard_reservation(data_dir, run)
        records.emit(data_dir, 'gap', {'agent_run_id': run['agent_run_id'], 'reason': 'run_startup_unavailable', 'quality': 'partial'})
        return 2 if args.scope_mode == 'require' else 127
    finally:
        if write_fd >= 0:
            os.close(write_fd)
    previous = {}
    def forward(signum, frame):
        try:
            os.kill(pid, signum)
        except ProcessLookupError:
            pass
    for signum in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
        previous[signum] = signal.signal(signum, forward)
    try:
        _, status = os.waitpid(pid, 0)
    finally:
        for signum, handler in previous.items():
            signal.signal(signum, handler)
    code = os.waitstatus_to_exitcode(status)
    records.emit(data_dir, 'run', {'agent_run_id': run['agent_run_id'], 'state': 'main_exited', 'main_exited': True, 'agent_exited': True, 'exit_code': code})
    return code if code >= 0 else 128-code


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='verb', required=True)
    run = sub.add_parser('run')
    run.add_argument('--data-dir', default=os.environ.get('SUBYARD_TELEMETRY_DATA_DIR', '/var/lib/subyard/telemetry'))
    run.add_argument('--agent', required=True, choices=['codex', 'claude', 'other'])
    run.add_argument('--agent-version', default='unknown')
    run.add_argument('--scope-mode', choices=['auto', 'require', 'partial'], default='auto')
    run.add_argument('--shared-backend', action='store_true', help='report backend usage as shared across its native sessions')
    for name in ('task-id', 'project-id', 'worktree-id', 'yard-id', 'host-id'):
        run.add_argument('--'+name)
    run.add_argument('command', nargs=argparse.REMAINDER)
    manager = sub.add_parser('manager')
    manager.add_argument('--data-dir', default='/var/lib/subyard/telemetry')
    manager.add_argument('--delegate', action='store_true')
    manager.add_argument('--sample-interval', type=float, default=5)
    args = parser.parse_args()
    if args.verb == 'run':
        return launch(args)
    records.private_directory(args.data_dir)
    if args.delegate:
        try:
            enable_delegation(args.data_dir)
        except (OSError, ValueError):
            records.emit(args.data_dir, 'gap', {'reason': 'delegation_unavailable', 'quality': 'partial'})
    lock_fd = os.open(Path(args.data_dir)/'manager.lock', os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    fcntl.flock(lock_fd, fcntl.LOCK_EX)
    if not 0.1 <= args.sample_interval <= 300:
        parser.error('sample interval must be between 0.1 and 300 seconds')
    return observe(args.data_dir, interval=args.sample_interval)


if __name__ == '__main__':
    raise SystemExit(main())
