#!/usr/bin/env python3
"""Read cumulative counters and explicit process generations; never inspect argv/env."""
import datetime
import hashlib
import os
from pathlib import Path
import time

import records


def namespace(proc, name):
    return os.readlink(proc/'ns'/name)


def stat_fields(pid, proc=Path('/proc')):
    fields = (proc/str(pid)/'stat').read_text().rsplit(') ', 1)[1].split()
    if len(fields) < 22 or fields[0] == 'Z':
        return None
    return fields


def start_ticks(pid, proc=Path('/proc')):
    fields = stat_fields(pid, proc)
    return int(fields[19]) if fields is not None else None


def process(pid, proc=Path('/proc')):
    root = proc/str(pid)
    fields = stat_fields(pid, proc)
    if fields is None:
        return None
    ticks = os.sysconf('SC_CLK_TCK')
    return {'pid': int(pid), 'parent': int(fields[1]), 'start_ticks': int(fields[19]),
            'cpu_user_ns': int(fields[11])*1000000000//ticks,
            'cpu_system_ns': int(fields[12])*1000000000//ticks,
            'rss_bytes': int(fields[21])*os.sysconf('SC_PAGE_SIZE'),
            'process_namespace_id': namespace(root, 'pid'),
            'cgroup_namespace_id': namespace(root, 'cgroup')}


def inventory(proc=Path('/proc')):
    result = {}
    paths = [path for path in proc.iterdir() if path.name.isdecimal()]
    if len(paths) > 65536:
        raise ValueError('process inventory limit')
    for path in paths:
        try:
            value = process(path.name, proc)
            if value is not None:
                result[value['pid']] = value
        except (OSError, ValueError, IndexError):
            continue
    return result


def key(value):
    return (value['process_namespace_id'], value['pid'], value['start_ticks'])


def values(path):
    return dict(line.split(None, 1) for line in path.read_text().splitlines() if line.strip())


def number(path):
    try:
        return int(path.read_text().strip())
    except (OSError, ValueError):
        return None


def cgroup_sample(path):
    cpu = values(path/'cpu.stat')
    result = {name: int(cpu[field])*1000 if field in cpu else None
              for name, field in [('cpu_ns', 'usage_usec'), ('cpu_user_ns', 'user_usec'), ('cpu_system_ns', 'system_usec')]}
    result.update(memory_bytes=number(path/'memory.current'), memory_peak_bytes=number(path/'memory.peak'),
                  swap_bytes=number(path/'memory.swap.current'))
    io = {'rbytes': 0, 'wbytes': 0, 'rios': 0, 'wios': 0}
    try:
        for line in (path/'io.stat').read_text().splitlines():
            for pair in line.split()[1:]:
                name, value = pair.split('=', 1)
                if name in io:
                    io[name] += int(value)
    except (OSError, ValueError):
        io = dict.fromkeys(io)
    result.update(zip(('io_read_bytes', 'io_write_bytes', 'io_read_ops', 'io_write_ops'), io.values()))
    return result


def membership(proc=Path('/proc'), pid='self'):
    lines = (proc/str(pid)/'cgroup').read_text().splitlines()
    if len(lines) != 1 or not lines[0].startswith('0::/'):
        raise ValueError('unified cgroup unavailable')
    path = Path(lines[0][3:])
    if '..' in path.parts:
        raise ValueError('invalid cgroup')
    return path


def safe_placement(target, proc=Path('/proc'), cgroups=Path('/sys/fs/cgroup')):
    source = cgroups/membership(proc).relative_to('/')
    target = Path(target)
    if target.is_symlink() or not target.is_relative_to(cgroups):
        raise ValueError('unsafe delegated group')
    for name in ('cpuset.cpus.effective', 'cpuset.mems.effective'):
        allowed = source/name
        destination = target/name
        if allowed.exists() and allowed.read_text().strip():
            if not destination.exists() or allowed.read_text().strip() != destination.read_text().strip():
                raise ValueError('inherited locality ceiling')
    # Moving a child between siblings must not escape any source-only ceiling.
    for parent in (source, *source.parents):
        if parent == cgroups or target.is_relative_to(parent):
            break
        for name in ('memory.max', 'memory.high', 'memory.swap.max', 'pids.max'):
            path = parent/name
            if path.exists() and path.read_text().strip() != 'max':
                raise ValueError('inherited ceiling')
        path = parent/'cpu.max'
        if path.exists() and path.read_text().split()[0] != 'max':
            raise ValueError('inherited ceiling')
        path = parent/'io.max'
        if path.exists() and path.read_text().strip():
            raise ValueError('inherited ceiling')
    # The kernel checks migration permission at the nearest common ancestor,
    # even when the destination leaf belongs to this UID.
    common = next(parent for parent in (source, *source.parents) if target.is_relative_to(parent))
    if not os.access(common/'cgroup.procs', os.W_OK):
        raise ValueError('migration delegation unavailable')


def delegated_parents(proc=Path('/proc'), cgroups=Path('/sys/fs/cgroup')):
    source = cgroups/membership(proc).relative_to('/')
    result = []
    for parent in (source, *source.parents):
        if parent == cgroups:
            break
        try:
            if parent.is_symlink() or parent.stat().st_uid != os.getuid():
                continue
            required = {'cpu', 'memory', 'io'}
            if not required.issubset((parent/'cgroup.controllers').read_text().split()):
                continue
            if not os.access(parent, os.W_OK) or not os.access(parent/'cgroup.procs', os.W_OK):
                continue
            enabled = set((parent/'cgroup.subtree_control').read_text().split())
            if not required.issubset(enabled):
                if (parent/'cgroup.procs').read_text().split() or not os.access(parent/'cgroup.subtree_control', os.W_OK):
                    continue
                # This UID already owns the empty delegated ancestor; enable
                # accounting below it without moving callers or changing caps.
                missing = required-enabled
                (parent/'cgroup.subtree_control').write_text(' '.join('+'+name for name in sorted(missing)))
            result.append(parent)
        except OSError:
            continue
    return result


class Sampler:
    def __init__(self, data_dir, run, proc=Path('/proc')):
        self.data_dir, self.run, self.proc = Path(data_dir), run, proc
        self.known, self.last, self.direct_last, self.ended = {}, {}, {}, set()
        self.group = Path(run['cgroup']) if run.get('cgroup') else None
        self.workload = self.group/'workload' if self.group else None
        self.run_body = {**run, 'state': 'resources_observing', 'scope_quality': 'managed' if self.group else 'partial'}
        self.run_announced = records.emit(self.data_dir, 'run', self.run_body)
        self.object_id = run['agent_run_id']+':scope'
        self.base = {name: run[name] for name in ('host_id', 'boot_id', 'agent_run_id')}
        self.base.update(object_id=self.object_id, generation=run['agent_run_id'],
                         accounting='inclusive', shared=bool(run.get('shared')), scope_quality='managed' if self.group else 'partial')
        self.declarations = [{**self.base, 'object_kind': 'cgroup' if self.group else 'process_tree',
                     'quality': 'explicit' if self.group else 'partial', 'parent_object_id': run.get('parent_scope_id')}]
        if self.group:
            self.declarations.append({**self.base, 'object_id': run['agent_run_id']+':workload',
                         'object_kind': 'cgroup', 'parent_object_id': self.object_id, 'accounting': 'direct', 'quality': 'explicit'})
        self.declarations = [body for body in self.declarations if not self.declare(body)]
        self.pending_baselines = list(run.get('scope_baselines', []))

    def declare(self, body):
        options = {}
        if self.run.get('scope_start_time'):
            options = {'source_time': self.run['scope_start_time'], 'event_id': body['object_id']+':declaration'}
        return records.emit(self.data_dir, 'scope', body, **options)

    def publish_baselines(self):
        if self.declarations:
            return False
        while self.pending_baselines:
            entry = self.pending_baselines[0]
            if not records.emit(self.data_dir, 'sample', entry['body'], source_time=entry['source_time'], event_id=entry['event_id']):
                return False
            self.pending_baselines.pop(0)
        return True

    def capture_baseline(self):
        if not self.group:
            return False
        self.group_identity()
        entries = []
        for path, suffix, accounting in ((self.group, ':scope', 'inclusive'), (self.workload, ':workload', 'direct')):
            if values(path/'cgroup.events').get('populated') != '0':
                raise ValueError('baseline group is not empty')
            body = {**self.base, **cgroup_sample(path), 'object_id': self.run['agent_run_id']+suffix,
                    'accounting': accounting, 'monotonic_ns': time.monotonic_ns(), 'counter_reset': False,
                    'observed_process_count': 0, 'populated': False, 'quality': 'complete', 'final': False}
            entries.append({'body': body, 'source_time': datetime.datetime.now(datetime.timezone.utc).isoformat(timespec='microseconds'),
                            'event_id': self.run['agent_run_id']+suffix+':baseline'})
        self.run['scope_baselines'] = entries
        self.pending_baselines = list(entries)
        return self.publish_baselines()

    def group_identity(self):
        info = self.group.stat()
        if [info.st_dev, info.st_ino] != self.run['cgroup_identity'] or self.group.is_symlink() or info.st_uid != os.getuid():
            raise ValueError('cgroup generation changed')
        if self.run.get('scope_root'):
            root = Path(self.run['scope_root'])
            info = root.stat()
            if root.is_symlink() or info.st_uid != os.getuid() or [info.st_dev, info.st_ino] != self.run['scope_root_identity'] or not self.group.is_relative_to(root):
                raise ValueError('scope delegation generation changed')

    def poll(self):
        if not self.run_announced:
            self.run_announced = records.emit(self.data_dir, 'run', self.run_body)
            if not self.run_announced:
                return False
        self.declarations = [body for body in self.declarations if not self.declare(body)]
        if self.declarations or not self.publish_baselines():
            return False
        direct_saved = True
        current = inventory(self.proc)
        if self.group:
            self.group_identity()
            members = set()
            # Nested managed runs have their own sampler. Process details belong
            # only to this run's direct workload; parent counters remain inclusive.
            groups = [self.workload, *self.workload.rglob('*')]
            if len(groups) > 65536:
                raise ValueError('scope inventory limit')
            for group in groups:
                if group.is_dir() and not group.is_symlink():
                    try:
                        members.update(map(int, (group/'cgroup.procs').read_text().split()))
                    except FileNotFoundError:
                        pass
            chosen = {pid: value for pid, value in current.items() if pid in members}
            counters = cgroup_sample(self.group)
            populated = values(self.group/'cgroup.events').get('populated') != '0'
            direct = cgroup_sample(self.workload)
            direct_reset = any(value is not None and self.direct_last.get(name) is not None and value < self.direct_last[name]
                               for name, value in direct.items() if name.startswith(('cpu_', 'io_')))
            self.direct_last = direct
            direct_saved = records.emit(self.data_dir, 'sample', {**self.base, **direct,
                         'object_id': self.run['agent_run_id']+':workload', 'accounting': 'direct',
                         'monotonic_ns': time.monotonic_ns(), 'counter_reset': direct_reset, 'quality': 'complete', 'final': not populated})
        else:
            chosen = {pid: value for pid, value in current.items()
                      if key(value) in self.known or (pid == self.run['pid'] and value['start_ticks'] == self.run['start_ticks'])}
            while True:
                more = {pid: value for pid, value in current.items() if value['parent'] in chosen and pid not in chosen}
                if not more:
                    break
                chosen.update(more)
            counters = dict.fromkeys(('cpu_ns', 'cpu_user_ns', 'cpu_system_ns', 'memory_bytes', 'memory_peak_bytes',
                                      'swap_bytes', 'io_read_bytes', 'io_write_bytes', 'io_read_ops', 'io_write_ops'))
            populated = bool(chosen)
        for value in chosen.values():
            identity = key(value)
            object_id = self.run['host_id']+':'+self.run['boot_id']+':'+hashlib.sha256(value['process_namespace_id'].encode()).hexdigest()[:12]+':'+str(value['pid'])+':'+str(value['start_ticks'])
            if identity not in self.known:
                records.emit(self.data_dir, 'process', {**self.base, **{k: v for k, v in value.items() if k != 'parent'},
                             'object_id': object_id, 'object_kind': 'process', 'parent_object_id': self.object_id,
                             'accounting': 'direct', 'state': 'observed'})
                self.known[identity] = object_id
                records.emit(self.data_dir, 'association', {'object_id': object_id, 'agent_run_id': self.run['agent_run_id'],
                             'parent_object_id': self.object_id,
                             'start_time': datetime.datetime.now(datetime.timezone.utc).isoformat(), 'end_time': None,
                             'source': 'managed_scope_membership' if self.group else 'process_tree_discovery',
                             'quality': 'explicit' if self.group else 'partial', 'shared': bool(self.run.get('shared'))})
            records.emit(self.data_dir, 'sample', {**self.base, 'object_id': object_id, 'accounting': 'direct',
                         'monotonic_ns': time.monotonic_ns(), 'cpu_user_ns': value['cpu_user_ns'],
                         'cpu_system_ns': value['cpu_system_ns'], 'cpu_ns': value['cpu_user_ns']+value['cpu_system_ns'],
                         'rss_bytes': value['rss_bytes'], 'pss_bytes': None, 'quality': 'observed'})
        living = {key(value) for value in chosen.values()}
        for identity in self.known.keys()-living-self.ended:
            still_alive = any(key(value) == identity for value in current.values())
            records.emit(self.data_dir, 'process', {**self.base, 'object_id': self.known[identity],
                         'state': 'scope_left' if still_alive else 'ended', 'quality': 'observed'})
            records.emit(self.data_dir, 'association', {'object_id': self.known[identity], 'agent_run_id': self.run['agent_run_id'],
                         'parent_object_id': self.object_id,
                         'end_time': datetime.datetime.now(datetime.timezone.utc).isoformat(),
                         'source': 'managed_scope_membership' if self.group else 'process_tree_discovery',
                         'quality': 'explicit' if self.group else 'partial', 'shared': bool(self.run.get('shared'))})
            records.emit(self.data_dir, 'gap', {**self.base, 'object_id': self.known[identity],
                         'reason': 'process_left_managed_scope' if still_alive else 'process_final_counter_unavailable', 'quality': 'partial'})
            self.ended.add(identity)
        reset = any(value is not None and self.last.get(name) is not None and value < self.last[name]
                    for name, value in counters.items() if name.startswith(('cpu_', 'io_')))
        self.last = counters
        final = not populated
        saved = records.emit(self.data_dir, 'sample', {**self.base, **counters,
                             'monotonic_ns': time.monotonic_ns(), 'counter_reset': reset,
                             'observed_process_count': len(chosen), 'populated': populated,
                             'quality': 'complete' if self.group else 'partial', 'final': final})
        if final and saved and direct_saved:
            if not records.emit(self.data_dir, 'run', {'agent_run_id': self.run['agent_run_id'], 'state': 'resources_complete', 'resource_completed': True,
                         'quality': 'complete' if self.group else 'partial'}):
                return False
            if self.group:
                # Never remove an unknown or populated group; final counters were durable first.
                self.group_identity()
                if values(self.group/'cgroup.events').get('populated') == '0':
                    self.workload.rmdir()
                    self.group.rmdir()
            return True
        return False
