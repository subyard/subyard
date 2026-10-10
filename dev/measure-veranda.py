#!/usr/bin/env python3
"""Bounded Linux resource probe using isolated Xvfb or explicit Wayland; no fleet mutations."""
import argparse
import ctypes
import ctypes.util
import hashlib
import importlib.util
import json
import math
import os
import re
from pathlib import Path
import selectors
import signal
import stat
import statistics
import struct
import subprocess
import sys
import tempfile
import time
import zlib

ROOT = Path(__file__).resolve().parents[1]
PAGE = os.sysconf('SC_PAGE_SIZE')
TICKS = os.sysconf('SC_CLK_TCK')
FLEET_COUNTS = {'owners': 1, 'yards': 20, 'projects': 200}


def resource_witness(*, proc=Path('/proc'), cgroup_root=Path('/sys/fs/cgroup'),
                     app_cgroup=None, disk=ROOT):
    """Numeric guest pressure witnesses; missing/malformed counters remain unknown."""
    started = time.monotonic()
    def counters(path, names, kind='pairs'):
        result = dict.fromkeys(names)
        try:
            with path.open('rb') as stream:
                raw = stream.read(65537)
            if len(raw) > 65536:
                raise ValueError()
            seen = set()
            for line in raw.decode('ascii').splitlines():
                fields = line.split()
                if not fields:
                    continue
                key = fields[0].rstrip(':')
                if key not in result:
                    continue
                if key in seen:
                    raise ValueError()
                seen.add(key)
                if kind == 'psi':
                    values = [value[6:] for value in fields[1:] if value.startswith('total=')]
                    if len(values) != 1:
                        raise ValueError()
                    value = values[0]
                else:
                    if len(fields) != (3 if kind == 'kib' else 2) or kind == 'kib' and fields[2] != 'kB':
                        raise ValueError()
                    value = fields[1]
                if not value.isascii() or not value.isdecimal():
                    raise ValueError()
                number = int(value) * (1024 if kind == 'kib' else 1)
                if not 0 <= number <= 2**64 - 1:
                    raise ValueError()
                result[key] = number
            return {'status': 'ok' if seen == set(names) else 'incomplete', 'values': result}
        except FileNotFoundError:
            status = 'missing'
        except OSError:
            status = 'unavailable'
        except (ValueError, UnicodeError):
            status = 'malformed'
        return {'status': status, 'values': dict.fromkeys(names)}
    events = ('low', 'high', 'max', 'oom', 'oom_kill', 'oom_group_kill')
    app = {'status': 'unavailable', 'values': dict.fromkeys(events)}
    if app_cgroup is not None:
        # Reuse the owning leaf's inode/path guard before reading its counters.
        app_cgroup.members()
        app = counters(app_cgroup.path / 'memory.events', events)
    storage = {'status': 'unavailable', 'available_bytes': None, 'available_inodes': None}
    try:
        info = os.statvfs(disk)
        free, inodes = info.f_bavail * info.f_frsize, info.f_favail
        if not 0 <= free <= 2**64 - 1 or not 0 <= inodes <= 2**64 - 1:
            raise ValueError()
        storage.update(status='ok', available_bytes=free, available_inodes=inodes)
    except (OSError, ValueError):
        pass
    return {'schema_version': 1,
            'guest_memory_bytes': counters(proc / 'meminfo', ('MemTotal', 'MemAvailable', 'SwapTotal', 'SwapFree'), 'kib'),
            'guest_vm_counters': counters(proc / 'vmstat', ('pswpin', 'pswpout', 'pgscan_direct', 'pgscan_kswapd',
                                                         'pgsteal_direct', 'pgsteal_kswapd', 'oom_kill')),
            'guest_memory_psi_microseconds': counters(proc / 'pressure/memory', ('some', 'full'), 'psi'),
            'guest_root_cgroup_events': counters(cgroup_root / 'memory.events', events),
            'app_cgroup_events': app, 'disk': storage,
            'collection_seconds': time.monotonic() - started}


def assess_resource_witnesses(witnesses, *, require_app=False):
    """Assess recorded counter changes separately from native/RSS acceptance."""
    result = {'schema_version': 1, 'status': 'unknown', 'interval_count': 0,
              'unknown_counters': [], 'increased_counters': [],
              'scope': 'recorded cumulative changes between supplied endpoints; not continuous observation'}
    if type(witnesses) not in (list, tuple) or not 2 <= len(witnesses) <= 1024 or type(require_app) is not bool:
        result['unknown_counters'] = ['witness_sequence']
        return result
    result['interval_count'] = len(witnesses) - 1
    required = [('guest_memory_bytes', key, False) for key in ('MemTotal', 'MemAvailable', 'SwapTotal', 'SwapFree')]
    required += [('guest_vm_counters', key, True) for key in
                 ('pswpin', 'pswpout', 'pgscan_direct', 'pgscan_kswapd', 'pgsteal_direct', 'pgsteal_kswapd', 'oom_kill')]
    required += [('guest_memory_psi_microseconds', key, True) for key in ('some', 'full')]
    if require_app:
        required += [('app_cgroup_events', key, True) for key in ('high', 'max', 'oom', 'oom_kill')]
    known = {}
    for block, key, cumulative in required:
        name, values = block + '.' + key, []
        for witness in witnesses:
            item = witness.get(block) if type(witness) is dict and type(witness.get('schema_version')) is int and witness['schema_version'] == 1 else None
            value = item.get('values', {}).get(key) if type(item) is dict and item.get('status') in ('ok', 'incomplete') and type(item.get('values')) is dict else None
            values.append(value)
        if any(type(value) is not int or not 0 <= value <= 2**64 - 1 for value in values):
            result['unknown_counters'].append(name)
            continue
        if block == 'guest_memory_bytes':
            known[key] = values
        if cumulative:
            if any(after < before for before, after in zip(values, values[1:])):
                result['unknown_counters'].append(name)
            if any(after > before for before, after in zip(values, values[1:])):
                result['increased_counters'].append(name)
        elif key in ('MemTotal', 'SwapTotal') and len(set(values)) != 1:
            result['unknown_counters'].append(name)
    for total, free in (('MemTotal', 'MemAvailable'), ('SwapTotal', 'SwapFree')):
        if total in known and free in known and any(available > capacity for capacity, available in zip(known[total], known[free])):
            result['unknown_counters'].append('guest_memory_bytes.' + free)
    if 'SwapFree' in known and any(after < before for before, after in zip(known['SwapFree'], known['SwapFree'][1:])):
        result['increased_counters'].append('guest_memory_bytes.swap_used')
    result['unknown_counters'] = sorted(set(result['unknown_counters']))
    result['increased_counters'] = sorted(set(result['increased_counters']))
    result['status'] = ('unknown' if result['unknown_counters'] else
                        'pressure_observed' if result['increased_counters'] else 'no_recorded_pressure')
    return result


class AppCgroup:
    """A caller-created empty cgroup-v2 leaf; the probe never changes controllers."""
    def __init__(self, path, *, privileged_attach=False):
        root = Path('/sys/fs/cgroup')
        if not path.is_absolute() or path == root or not path.is_relative_to(root) or path.resolve() != path:
            raise RuntimeError('app cgroup must be a dedicated leaf under /sys/fs/cgroup')
        for component in (path, *path.parents):
            info = component.lstat()
            if (not stat.S_ISDIR(info.st_mode) or info.st_uid not in (0, os.getuid())
                    or info.st_mode & 0o022):
                raise RuntimeError('app cgroup path must contain only plain owned directories')
            if component == root:
                break
        self.path = path
        self.privileged_attach = privileged_attach
        info = path.lstat()
        self.identity = (info.st_dev, info.st_ino)
        info = (path / 'cgroup.procs').lstat()
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                or stat.S_IMODE(info.st_mode) != 0o600):
            raise RuntimeError('app cgroup.procs must be delegated to the current user with mode 0600')
        if not (path / 'cgroup.controllers').is_file():
            raise RuntimeError('app cgroup requires cgroup v2')
        self.require_empty()

    def members(self):
        info = self.path.lstat()
        if (not stat.S_ISDIR(info.st_mode) or (info.st_dev, info.st_ino) != self.identity
                or any(child.is_dir() for child in self.path.iterdir())):
            raise RuntimeError('app cgroup identity changed or is no longer a leaf')
        return {int(pid) for pid in (self.path / 'cgroup.procs').read_text().split()}

    def require_empty(self):
        if self.members():
            raise RuntimeError('app cgroup must be empty; probe and compositor must remain outside')

    def attach(self, pid):
        self.require_empty()
        if self.privileged_attach:
            # Destination delegation alone cannot authorize migration from a
            # sibling cgroup. The owned-VM helper validates the actual lease,
            # marker, cgroup inode and still-gated parent/child identities.
            current = processes()
            parent = os.getpid()
            if pid not in current or parent not in current or current[pid]['parent'] != parent:
                raise RuntimeError('privileged cgroup attach requires the still-gated direct child')
            subprocess.run(['sudo', '-n', sys.executable, str(ROOT / 'dev/e2e/veranda-memory-research.py'),
                            '--attach-cgroup', str(self.path), str(pid), str(current[pid]['start']),
                            str(parent), str(current[parent]['start']),
                            str(self.identity[0]), str(self.identity[1])],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=5, check=True)
        else:
            fd = os.open(self.path / 'cgroup.procs', os.O_WRONLY | os.O_NOFOLLOW)
            with os.fdopen(fd, 'w') as stream:
                stream.write(str(pid) + '\n')
        if self.members() != {pid}:
            raise RuntimeError('gated app cgroup migration was not confirmed')

    def memory_current(self):
        try:
            return int((self.path / 'memory.current').read_text())
        except OSError:
            return None


def process_role(pid, name, root_pid, engine):
    if pid == root_pid:
        return 'native-shell'
    if name.startswith('WebKitWeb'):
        return 'webkit-web'
    if name.startswith('WebKitNetwork'):
        return 'webkit-network'
    try:
        executable = (Path('/proc') / str(pid) / 'exe').resolve(strict=True)
        if engine is not None and executable == engine:
            return 'local-core'
        if executable.name in ('ssh', 'ssh-agent'):
            return 'app-ssh-helper'
    except OSError:
        pass
    return 'other-app-helper'


def processes():
    result = {}
    for path in Path('/proc').iterdir():
        if not path.name.isdigit():
            continue
        try:
            raw = (path / 'stat').read_text()
            fields = raw[raw.rfind(')') + 2:].split()
            result[int(path.name)] = {
                'parent': int(fields[1]), 'group': int(fields[2]),
                'ticks': int(fields[11]) + int(fields[12]),
                'start': int(fields[19]), 'rss': int(fields[21]) * PAGE,
                'threads': int(fields[17]),
                'name': raw[raw.find('(') + 1:raw.rfind(')')],
            }
        except (OSError, ValueError, IndexError):
            pass
    return result


class Tree:
    def __init__(self, pid, *, cgroup=None, engine=None):
        self.pid = pid
        self.cgroup = cgroup
        self.engine = engine
        self.started = time.monotonic()
        self.history = {}
        self.known = {}
        self.previous = {}
        self.cpu_ticks = 0
        self.peak_rss = 0
        self.peak_pss = 0
        self.names = set()
        self.max_count = 0
        self.peak_processes = []

    def sample(self, pss=False, attribution=False, resources=False):
        all_processes = processes()
        # A recycled process-group ID must not import unrelated processes after exit.
        active_groups = {item['group'] for pid, item in all_processes.items()
                         if self.known.get(pid) == item['start']}
        if self.pid not in self.known:
            active_groups.add(self.pid)
        chosen = {pid for pid, item in all_processes.items()
                  if self.known.get(pid) == item['start'] or item['group'] in active_groups}
        while True:
            descendants = {pid for pid, item in all_processes.items()
                           if item['parent'] in chosen}
            if descendants <= chosen:
                break
            chosen |= descendants
        if self.cgroup is not None:
            # Cgroup membership catches children even after a double fork/reparent.
            # Keep already-observed identities too, in case a helper migrates out.
            chosen = (self.cgroup.members() & all_processes.keys()) | {
                pid for pid, item in all_processes.items() if self.known.get(pid) == item['start']}
        rss = total_pss = total_private = total_swap = total_fds = total_threads = 0
        pss_missing = 0
        fd_missing = 0
        identity_changed = 0
        now = time.monotonic() - self.started
        breakdown = []
        for pid in sorted(chosen):
            item = all_processes[pid]
            key = (pid, item['start'])
            self.known[pid] = item['start']
            self.cpu_ticks += max(0, item['ticks'] - self.previous.get(key, 0))
            self.previous[key] = item['ticks']
            self.names.add(item['name'])
            process_rss = item['rss']
            process_pss = private = swap = fds = None
            if pss:
                try:
                    values = {line.split(':', 1)[0]: int(line.split()[1]) * 1024
                              for line in (Path('/proc') / str(pid) / 'smaps_rollup').read_text().splitlines()[1:]}
                    current = (Path('/proc') / str(pid) / 'stat').read_text().rsplit(')', 1)[1].split()
                    if int(current[19]) != item['start']:
                        # Do not attach a recycled PID's memory to an old identity.
                        identity_changed += 1
                        continue
                    process_rss, process_pss = values['Rss'], values['Pss']
                    private = values['Private_Clean'] + values['Private_Dirty'] + values.get('Private_Hugetlb', 0)
                    swap = values['Swap']
                    total_pss += process_pss
                    total_private += private
                    total_swap += swap
                except (OSError, ValueError, KeyError, IndexError):
                    pss_missing += 1
                try:
                    fds = sum(1 for _ in (Path('/proc') / str(pid) / 'fd').iterdir())
                    total_fds += fds
                except OSError:
                    fd_missing += 1
            rss += process_rss
            total_threads += item['threads']
            entry = {'pid': pid, 'start_ticks': item['start'], 'parent_pid': item['parent'],
                     'name': item['name'], 'role': process_role(pid, item['name'], self.pid, self.engine),
                     'rss_bytes': process_rss, 'pss_bytes': process_pss, 'private_bytes': private,
                     'swap_bytes': swap, 'fds': fds, 'threads': item['threads']}
            history = self.history.setdefault(key, {
                'pid': pid, 'start_ticks': item['start'], 'name': item['name'], 'role': entry['role'],
                'first_parent_pid': item['parent'], 'first_seen_seconds': now, 'sample_count': 0})
            history['observed_roles'] = sorted({*history.get('observed_roles', []), entry['role']})
            history.update(last_seen_seconds=now, last_parent_pid=item['parent'],
                           sample_count=history['sample_count'] + 1)
            if attribution:
                entry.update(memory_attribution(pid))
            breakdown.append(entry)
        if rss > self.peak_rss:
            self.peak_processes = breakdown
        self.peak_rss = max(self.peak_rss, rss)
        if pss:
            self.peak_pss = max(self.peak_pss, total_pss)
        self.max_count = max(self.max_count, len(chosen))
        roles = {}
        for entry in breakdown:
            totals = roles.setdefault(entry['role'], {'process_count': 0})
            totals['process_count'] += 1
            for metric in ('rss_bytes', 'pss_bytes', 'private_bytes', 'swap_bytes', 'fds', 'threads'):
                if entry[metric] is not None:
                    totals[metric] = totals.get(metric, 0) + entry[metric]
        result = {'rss_bytes': rss, 'pss_bytes': total_pss if pss else None,
                'private_bytes': total_private if pss else None, 'swap_bytes': total_swap if pss else None,
                'fds': total_fds if pss else None, 'threads': total_threads, 'fd_missing_processes': fd_missing,
                'identity_changed_processes': identity_changed,
                'cgroup_memory_current_bytes': self.cgroup.memory_current() if self.cgroup is not None else None,
                'sample_seconds': now,
                'pss_missing_processes': pss_missing, 'process_count': len(chosen),
                'roles': roles, 'processes': breakdown}
        if resources:
            result['resource_witness'] = resource_witness(app_cgroup=self.cgroup)
        return result

    def process_history(self):
        live = {(pid, item['start']) for pid, item in processes().items()}
        return [{**entry, 'alive_at_end': key in live} for key, entry in sorted(self.history.items())]

    def stop(self, process, *, signal_processes=None):
        self.sample()
        # RPC children use independent groups; signal only recorded PID/starttime
        # identities through PIDfds, never a possibly recycled group wholesale.
        def remaining():
            if self.cgroup is not None:
                self.sample()
            return {pid: item for pid, item in processes().items()
                    if self.known.get(pid) == item['start']}

        def signal_owned(sig):
            if signal_processes is not None:
                signal_processes(sig, remaining())
                return
            for pid, item in remaining().items():
                fd = None
                try:
                    fd = os.pidfd_open(pid)
                    current = processes().get(pid)
                    if current is not None and current['start'] == item['start']:
                        signal.pidfd_send_signal(fd, sig)
                except (ProcessLookupError, FileNotFoundError):
                    pass
                finally:
                    if fd is not None:
                        os.close(fd)

        signal_owned(signal.SIGTERM)
        try:
            process.wait(timeout=3)
        except subprocess.TimeoutExpired:
            signal_owned(signal.SIGKILL)
            process.wait(timeout=3)
        deadline = time.monotonic() + 3
        while remaining() and time.monotonic() < deadline:
            time.sleep(0.05)
        if remaining():
            signal_owned(signal.SIGKILL)
            deadline = time.monotonic() + 3
            while remaining() and time.monotonic() < deadline:
                time.sleep(0.05)
            if remaining():
                raise RuntimeError('owned process descendants remained after shutdown')


def wayland_display(env=None):
    """Validate only the caller's compositor endpoint, without importing its environment."""
    env = os.environ if env is None else env
    display = env.get('WAYLAND_DISPLAY', '')
    runtime_value = env.get('XDG_RUNTIME_DIR', '')
    if not display or not runtime_value:
        raise RuntimeError('--wayland requires WAYLAND_DISPLAY and XDG_RUNTIME_DIR from a Wayland session')
    if any(len(value) > 4096 or any(ord(char) < 32 or ord(char) == 127 for char in value)
           for value in (display, runtime_value)):
        raise RuntimeError('--wayland requires valid compositor endpoint names')
    runtime = Path(runtime_value)
    try:
        info = runtime.lstat()
        if not runtime.is_absolute() or not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700:
            raise RuntimeError('--wayland requires an owned plain XDG_RUNTIME_DIR with mode 0700')
        endpoint = Path(display) if Path(display).is_absolute() else runtime / display
        if endpoint.parent != runtime:
            raise RuntimeError('--wayland requires a compositor socket directly inside XDG_RUNTIME_DIR')
        info = endpoint.lstat()
        if not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.getuid():
            raise RuntimeError('--wayland requires a plain compositor socket owned by the current user')
    except OSError:
        raise RuntimeError('--wayland compositor socket or runtime directory is unavailable; run from a Wayland session') from None
    # libwayland-client accepts absolute WAYLAND_DISPLAY since 1.15, independently
    # of XDG_RUNTIME_DIR: https://wayland.freedesktop.org/docs/html/apb.html
    return str(endpoint)


def environment(directory, display, engine, *, wayland=False):
    paths = {name: directory / name for name in ('home', 'data', 'cache', 'config', 'runtime', 'operator', 'owner-config', 'owner-data')}
    for path in paths.values():
        path.mkdir(mode=0o700)
        path.chmod(0o700)
    # An explicit empty host layer prevents the repository's private fallback.
    host_layer = paths['owner-config'] / 'config.env'
    host_layer.write_text('')
    host_layer.chmod(0o600)
    env = {
        'PATH': f"{ROOT / 'bin'}:/usr/local/bin:/usr/bin:/bin",
        'LANG': 'C.UTF-8',
        'HOME': str(paths['home']),
        'XDG_DATA_HOME': str(paths['data']), 'XDG_CACHE_HOME': str(paths['cache']),
        'XDG_CONFIG_HOME': str(paths['config']), 'XDG_RUNTIME_DIR': str(paths['runtime']),
        'SUBYARD_OPERATOR_HOME': str(paths['operator']),
        'SUBYARD_CONFIG_HOME': str(paths['owner-config']), 'SUBYARD_HOME': str(paths['owner-data']),
        'SUBYARD_REPOSITORY_ROOT': str(ROOT), 'YARD_ENGINE_PATH': str(engine),
        'VERANDA_RESOURCE_PROBE': '1',
    }
    env.update({'GDK_BACKEND': 'wayland', 'WAYLAND_DISPLAY': display} if wayland else {'DISPLAY': display})
    return env


def private_dir(path):
    path.mkdir(mode=0o700)
    path.chmod(0o700)


def private_file(path, text):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    os.fchmod(fd, 0o600)
    with os.fdopen(fd, 'w') as stream:
        stream.write(text)


def write_metrics(path, text):
    """Preserve CLI replacement, but never follow links or rely on umask."""
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1:
            raise RuntimeError('metrics output must be an owned plain file with one link')
        os.fchmod(fd, 0o600)
        os.ftruncate(fd, 0)
        with os.fdopen(fd, 'w', closefd=False) as stream:
            stream.write(text)
    finally:
        os.close(fd)


def isolated_defaults(directory, env):
    # Keep installed defaults, but make private-yard discovery point only into
    # our fresh fixture even when the caller has a private repository overlay.
    shipped = directory / 'shipped-config'
    private_dir(shipped)
    for name in ('incus.project.env', 'subyard.env', 'host.env', 'agents.env', 'ports.env'):
        source = ROOT / 'config' / name
        if source.is_file():
            private_file(shipped / name, source.read_text())
    # Public immutable assets retain their real paths and profile contracts.
    # App/owner state and records stay in plain private directories above.
    for name in ('agents', 'profiles', 'yards'):
        (shipped / name).symlink_to(ROOT / 'config' / name, target_is_directory=True)
    env['SUBYARD_CONFIG_DIR'] = str(shipped)


def configured_fleet(directory, env):
    """Seed only this fresh fixture; production inventory validates the stored records."""
    config = Path(env['SUBYARD_CONFIG_HOME'])
    isolated_defaults(directory, env)
    private_dir(config / 'yards')
    private_dir(directory / 'projects')
    names = ['default', *('fleet-' + str(index).zfill(2) for index in range(1, 20))]
    for index, name in enumerate(names):
        yard = config / 'yards' / name
        private_dir(yard)
        private_file(yard / 'config.env', f'YARD_KIND=container\nSSH_PORT={64000 + index}\n'
                     f'HOST_BASE={directory}/host-{name}\nRESTRICTED_DISK_PATHS={directory}/host-{name}\n'
                     'ENVIRONMENT_PROFILES=\nCODING_TOOL_INTEGRATIONS=\n')
        records = config / 'projects' if name == 'default' else yard / 'projects'
        private_dir(records)
        for project in range(10):
            identity = f'project-{index:02}-{project:02}'
            host = directory / 'projects' / identity
            private_dir(host)
            private_file(records / (identity + '.json'), json.dumps({
                'schema': 1, 'identityVersion': 2, 'projectId': identity, 'name': identity,
                'hostPath': str(host), 'yardPath': f'/srv/workspaces/{identity}/src',
                'mode': 'sync', 'sshHost': 'yard' if name == 'default' else 'yard-' + name,
            }))
    spec = importlib.util.spec_from_file_location('veranda_owner', ROOT / 'dev/e2e/veranda-owner.py')
    owner_module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(owner_module)
    owner = owner_module.Owner([env['YARD_ENGINE_PATH']], env=env, timeout=10)
    try:
        negotiated = owner.result('rpc.negotiate')
        inventory = owner.result('owner.inventory')
        yards = inventory.get('yards', [])
        states = {state: sum(item.get('state') == state for item in yards)
                  for state in ('NOT_CREATED', 'UNKNOWN')}
        # Record only bounded counts/booleans, never owner identity or paths.
        print('VERANDA_FLEET_PREFLIGHT ' + json.dumps({
            'yards': len(yards), 'projects': sum(len(item.get('projects', [])) for item in yards),
            'not_created': states['NOT_CREATED'], 'unknown': states['UNKNOWN'],
            'other_state': len(yards) - sum(states.values()),
            'schema_matches': inventory.get('schema') == 1,
            'names_match': sorted(item['name'] for item in yards) == sorted(names),
            'projects_per_yard_match': all(len(item.get('projects', [])) == 10 for item in yards),
            'capability_matches': 'owner-inventory-v1' in negotiated.get('capabilities', []),
        }), flush=True)
        if ('owner-inventory-v1' not in negotiated.get('capabilities', [])
                or inventory.get('schema') != 1
                or sorted(item['name'] for item in yards) != sorted(names)
                or any(len(item.get('projects', [])) != 10 or item.get('state') != 'NOT_CREATED' for item in yards)
                or sum(len(item['projects']) for item in yards) != 200):
            raise RuntimeError('configured fleet inventory does not match 20 uncreated yards and 200 projects')
        return {**FLEET_COUNTS, 'yard_state': 'NOT_CREATED', 'engine_version': negotiated.get('engineVersion')}
    finally:
        owner.close()


def ready_fleet(buffer, expected):
    marker = re.search(rb'(?:^|\n)VERANDA_FLEET ([^\n]{1,512})\n', buffer)
    try:
        counts = json.loads(marker[1]) if marker else None
    except (ValueError, UnicodeDecodeError):
        counts = None
    if (counts != expected or not isinstance(counts, dict)
            or any(type(value) is not int for value in counts.values())):
        raise RuntimeError('first rendered fleet does not match the configured workload')


def memory_attribution(pid):
    """Summarize only this probe's mappings; never expose mapped paths or argv."""
    categories = {}
    category = 'anonymous'
    try:
        for line in (Path('/proc') / str(pid) / 'smaps').read_text().splitlines():
            if re.match(r'^[0-9a-f]+-[0-9a-f]+ ', line):
                fields = line.split(maxsplit=5)
                mapped = fields[5].lower() if len(fields) == 6 else ''
                category = ('mesa' if any(name in mapped for name in ('mesa', 'swrast', 'llvmpipe', '/dri/', 'libllvm'))
                            else 'webkit' if 'webkit' in mapped or 'javascriptcore' in mapped
                            else 'shared-memory' if any(name in mapped for name in ('memfd:', '/dev/shm/', 'sysv'))
                            else 'anonymous' if not mapped or mapped.startswith('[')
                            else 'other-file')
            elif line.startswith(('Rss:', 'Pss:')):
                key = 'rss_bytes' if line.startswith('Rss:') else 'pss_bytes'
                totals = categories.setdefault(category, {'rss_bytes': 0, 'pss_bytes': 0})
                totals[key] += int(line.split()[1]) * 1024
        thread_names = sorted({path.joinpath('comm').read_text().strip()
                               for path in (Path('/proc') / str(pid) / 'task').iterdir()})
        return {'mapping_categories': categories, 'thread_names': thread_names}
    except (OSError, ValueError):
        return {'mapping_categories': categories, 'attribution_incomplete': True}


def readiness_kind(buffer):
    markers = [line[len(b'VERANDA_ALLOCATION '):] for line in buffer.splitlines()
               if line.startswith(b'VERANDA_ALLOCATION ')]
    if not markers:
        return 'frontend-two-frame-marker'
    try:
        if len(markers) != 1 or json.loads(markers[0]) != {
                'mode': 'native-only', 'readiness': 'native-workload-complete; no frontend frame'}:
            raise ValueError()
    except (ValueError, TypeError, UnicodeDecodeError):
        raise RuntimeError('invalid allocation readiness marker') from None
    return 'native-workload-complete; no frontend frame'


def launch(binary, env, timeout, *, expected_fleet=None, stderr=subprocess.DEVNULL, cgroup=None):
    started = time.monotonic()
    gate_read = gate_write = None
    command = [str(binary)]
    options = {}
    if cgroup is not None:
        cgroup.require_empty()
        gate_read, gate_write = os.pipe()
        command = [sys.executable, '-c',
                   'import os,sys; fd=int(sys.argv[1]); token=os.read(fd,1); os.close(fd); '
                   'sys.exit(1) if token != b"1" else os.execv(sys.argv[2],[sys.argv[2]])',
                   str(gate_read), str(binary)]
        options['pass_fds'] = (gate_read,)
    try:
        process = subprocess.Popen(command, cwd=ROOT, env=env, start_new_session=True,
                                   stdout=subprocess.PIPE, stderr=stderr, **options)
    except BaseException:
        if gate_write is not None:
            os.close(gate_write)
        raise
    finally:
        if gate_read is not None:
            os.close(gate_read)
    tree = Tree(process.pid, cgroup=cgroup, engine=Path(env['YARD_ENGINE_PATH']).resolve() if 'YARD_ENGINE_PATH' in env else None)
    selector = selectors.DefaultSelector()
    selector.register(process.stdout, selectors.EVENT_READ)
    buffered = b''
    try:
        if cgroup is not None:
            identity = processes()[process.pid]['start']
            tree.known[process.pid] = identity
            cgroup.attach(process.pid)
            if processes().get(process.pid, {}).get('start') != identity:
                raise RuntimeError('gated app process identity changed before exec')
            os.write(gate_write, b'1')
            os.close(gate_write)
            gate_write = None
        while time.monotonic() - started < timeout:
            tree.sample(pss=True)
            for key, _ in selector.select(0.05):
                block = os.read(key.fd, 4096)
                if not block:
                    raise RuntimeError(f'app exited before ready (status {process.poll()})')
                buffered += block
                if b'VERANDA_READY\n' in buffered:
                    if expected_fleet is not None:
                        ready_fleet(buffered, expected_fleet)
                    tree.readiness_kind = readiness_kind(buffered)
                    ready = time.monotonic() - started
                    tree.sample(pss=True)
                    return process, tree, ready
                buffered = buffered[-4096:]
            if process.poll() is not None:
                raise RuntimeError(f'app exited before ready (status {process.returncode})')
        raise RuntimeError('first-screen readiness timeout')
    except BaseException:
        try:
            tree.stop(process)
        except BaseException:
            print('veranda-probe: launch cleanup failed; original error preserved', file=sys.stderr)
        raise
    finally:
        if gate_write is not None:
            os.close(gate_write)
        selector.close()


class WlrToplevel:
    """Actual foreign-toplevel states on a caller-owned initially empty compositor.

    wlrctl has no PID or geometry query: binding is temporal singleton + fixed
    app-ID. Output geometry and absence of covering layer-shell surfaces are
    caller gates, and unminimized/active alone does not prove painted pixels.
    """
    def __init__(self, display, app_id):
        self.app_id = app_id
        self.env = {'PATH': '/usr/bin:/bin', 'LANG': 'C.UTF-8',
                    'WAYLAND_DISPLAY': display, 'XDG_RUNTIME_DIR': str(Path(display).parent)}

    def command(self, action, *matches):
        result = subprocess.run(['wlrctl', 'toplevel', action, *matches], env=self.env,
                                text=True, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=3)
        if result.returncode:
            raise RuntimeError('wlrctl compositor command or state query failed')
        return result.stdout

    def require_empty(self):
        if self.command('list').strip():
            raise RuntimeError('toplevel control requires an initially empty owned compositor')

    def singleton(self):
        lines = self.command('list').splitlines()
        if len(lines) != 1 or lines[0].partition(': ')[0] != self.app_id:
            raise RuntimeError('owned compositor does not contain exactly the expected app toplevel')

    def check(self, state):
        self.singleton()
        minimized = state == 'minimized'
        matches = [f'app_id:{self.app_id}', 'state:minimized' if minimized else 'state:unminimized',
                   'state:inactive' if minimized else 'state:active']
        self.command('find', *matches)
        return {'state': state, 'app_id': self.app_id, 'toplevel_count': 1,
                'minimized_acknowledged': minimized, 'activated_acknowledged': not minimized,
                'binding': 'empty-before-owned-launch then singleton fixed app-ID; protocol has no PID',
                'proof': 'compositor foreign-toplevel state acknowledgement; painted pixels and client geometry unmeasured'}

    def set(self, state):
        self.singleton()
        self.command('minimize' if state == 'minimized' else 'focus', f'app_id:{self.app_id}')
        deadline = time.monotonic() + 5
        while True:
            try:
                return self.check(state)
            except RuntimeError:
                if time.monotonic() >= deadline:
                    raise RuntimeError('compositor did not acknowledge the requested window state') from None
                time.sleep(0.05)


def measure_idle(process, tree, seconds, settle_seconds, *, control=None, state='visible'):
    proof = control.set(state) if control is not None else {'state': 'unverified-visible', 'proof': 'no compositor state query'}
    settle_until = time.monotonic() + settle_seconds
    while time.monotonic() < settle_until:
        time.sleep(0.25)
        if process.poll() is not None:
            raise RuntimeError('app exited before idle measurement')
        tree.sample(pss=True)
        if control is not None:
            control.check(state)
    baseline = tree.sample(pss=True, attribution=True, resources=True)
    initial_cpu = previous_cpu = tree.cpu_ticks
    idle_started = previous_time = time.monotonic()
    samples = []
    while time.monotonic() - idle_started < seconds:
        next_sample = idle_started + min(seconds, len(samples) + 1)
        time.sleep(max(0, next_sample - time.monotonic()))
        if process.poll() is not None:
            raise RuntimeError('app exited during idle measurement')
        sample_started = time.monotonic()
        sample = tree.sample(pss=True, resources=True)
        now = time.monotonic()
        sample.update(elapsed_seconds=now - idle_started, collection_seconds=now - sample_started,
                      cpu_percent_one_core=100 * (tree.cpu_ticks - previous_cpu) / TICKS / (now - previous_time))
        previous_cpu, previous_time = tree.cpu_ticks, now
        if control is not None:
            control.check(state)
        samples.append(sample)
    elapsed = time.monotonic() - idle_started
    stats = {}
    for metric in ('rss_bytes', 'pss_bytes', 'private_bytes', 'swap_bytes', 'fds', 'threads', 'process_count',
                   'cgroup_memory_current_bytes', 'cpu_percent_one_core'):
        values = sorted(sample[metric] for sample in samples if sample.get(metric) is not None)
        stats[metric] = ({'median': statistics.median(values), 'p95': values[math.ceil(0.95 * len(values)) - 1],
                          'observed_max': max(values), 'sample_count': len(values)} if values else None)
    result = {'duration_seconds': elapsed, 'settle_seconds': settle_seconds, 'window_state': proof,
            'cpu_seconds': (tree.cpu_ticks - initial_cpu) / TICKS,
            'cpu_percent_one_core': 100 * (tree.cpu_ticks - initial_cpu) / TICKS / elapsed,
            'baseline': baseline, 'last': tree.sample(pss=True, attribution=True, resources=True),
            'peak_rss_bytes': stats['rss_bytes']['observed_max'], 'peak_pss_bytes': stats['pss_bytes']['observed_max'],
            'pss_missing_process_samples': sum(sample['pss_missing_processes'] for sample in samples),
            'statistics_partial': any(sample['pss_missing_processes'] or sample.get('fd_missing_processes', 0)
                                      or sample.get('identity_changed_processes', 0)
                                      for sample in samples),
            'statistics': stats, 'samples': samples}
    result['resource_assessment'] = assess_resource_witnesses(
        [item.get('resource_witness') for item in (baseline, *samples, result['last'])],
        require_app=getattr(tree, 'cgroup', None) is not None)
    return result


def command_output(args):
    return subprocess.check_output(args, text=True, stderr=subprocess.DEVNULL, timeout=10).strip()


def webkit_version():
    """Runtime-only desktop installations need no development headers for metadata."""
    try:
        return command_output(['pkg-config', '--modversion', 'webkit2gtk-4.1']), 'pkg-config'
    except (OSError, subprocess.CalledProcessError):
        try:
            return command_output(['dpkg-query', '-W', '-f=${Version}', 'libwebkit2gtk-4.1-0']), 'Debian runtime package'
        except (OSError, subprocess.CalledProcessError):
            raise RuntimeError('WebKit runtime version metadata is unavailable') from None


def desktop_packages():
    try:
        return command_output(['dpkg-query', '-W', 'xvfb', 'libegl-mesa0', 'libgl1-mesa-dri', 'libglx-mesa0'])
    except (OSError, subprocess.CalledProcessError):
        return 'package versions unavailable'


def screenshot(display, output):
    """Capture only our isolated 24-bit Xvfb display using libX11 and PNG stdlib encoding."""
    library = ctypes.util.find_library('X11')
    if not library:
        return False
    x11 = ctypes.CDLL(library)
    x11.XOpenDisplay.argtypes = [ctypes.c_char_p]
    x11.XOpenDisplay.restype = ctypes.c_void_p
    x11.XDefaultRootWindow.argtypes = [ctypes.c_void_p]
    x11.XDefaultRootWindow.restype = ctypes.c_ulong
    x11.XGetImage.argtypes = [ctypes.c_void_p, ctypes.c_ulong, ctypes.c_int, ctypes.c_int,
                             ctypes.c_uint, ctypes.c_uint, ctypes.c_ulong, ctypes.c_int]
    x11.XGetImage.restype = ctypes.c_void_p
    x11.XGetPixel.argtypes = [ctypes.c_void_p, ctypes.c_int, ctypes.c_int]
    x11.XGetPixel.restype = ctypes.c_ulong
    x11.XDestroyImage.argtypes = [ctypes.c_void_p]
    x11.XCloseDisplay.argtypes = [ctypes.c_void_p]
    connection = x11.XOpenDisplay(display.encode())
    if not connection:
        return False
    image = x11.XGetImage(connection, x11.XDefaultRootWindow(connection), 0, 0, 1280, 800,
                         ctypes.c_ulong(-1).value, 2)
    try:
        if not image:
            return False
        data = bytearray()
        for y in range(800):
            data.append(0)
            for x in range(1280):
                pixel = x11.XGetPixel(image, x, y)
                data.extend(((pixel >> 16) & 255, (pixel >> 8) & 255, pixel & 255))
        def chunk(kind, payload):
            return struct.pack('>I', len(payload)) + kind + payload + struct.pack('>I', zlib.crc32(kind + payload))
        output.write_bytes(b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('>IIBBBBB', 1280, 800, 8, 2, 0, 0, 0))
                           + chunk(b'IDAT', zlib.compress(data)) + chunk(b'IEND', b''))
        return True
    finally:
        if image:
            x11.XDestroyImage(image)
        x11.XCloseDisplay(connection)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True,
                        help='explicit historical diagnostic binary; no desktop candidate is selected by default')
    parser.add_argument('--engine', type=Path, default=ROOT / '.build/veranda-probe-engine')
    parser.add_argument('--output', type=Path, default=ROOT / '.build/veranda-resources.json')
    parser.add_argument('--screenshot', type=Path, default=ROOT / '.build/veranda-resources.png')
    parser.add_argument('--wayland', action='store_true',
                        help='use the current owned Wayland compositor; keep app data isolated and omit screenshots')
    parser.add_argument('--configured-fleet', action='store_true',
                        help='seed and verify 20 uncreated local yards/200 project names in fresh isolated state')
    parser.add_argument('--disable-compositing', action='store_true',
                        help='diagnostic only: force WebKit compositing off for this isolated child')
    parser.add_argument('--disable-dmabuf', action='store_true',
                        help='diagnostic only: disable WebKit DMABuf renderer for this isolated child')
    parser.add_argument('--disable-gdk-gl', action='store_true',
                        help='diagnostic only: disable early GTK GL context probing for this isolated child')
    graphics = parser.add_mutually_exclusive_group()
    graphics.add_argument('--gdk-debug-nogl', action='store_true',
                          help='diagnostic only: set GDK_DEBUG=nogl (ignored if GTK3 does not recognize the key)')
    graphics.add_argument('--gdk-rendering-image', action='store_true',
                          help='diagnostic only: select GTK3 GDK_RENDERING=image for this isolated child')
    parser.add_argument('--app-cgroup', type=Path,
                        help='caller-created empty cgroup-v2 leaf with current-user cgroup.procs mode 0600')
    parser.add_argument('--privileged-cgroup-attach', action='store_true',
                        help='explicit owned-VM lease-guarded root helper for migration; requires --app-cgroup')
    parser.add_argument('--toplevel-control-wlrctl', action='store_true',
                        help='measure acknowledged states on an initially empty caller-owned Wayland compositor')
    parser.add_argument('--wlrctl-app-id',
                        help='expected exact app-ID on the owned compositor; defaults to the binary basename')
    parser.add_argument('--window-state', action='append', choices=('visible', 'minimized'),
                        help='repeat for separate settle/idle windows on the final launch; requires wlrctl control')
    parser.add_argument('--starts', type=int, default=30)
    parser.add_argument('--idle-seconds', type=float, default=120)
    parser.add_argument('--idle-settle-seconds', type=float, default=30,
                        help='settle the final launch before the idle measurement (default: 30)')
    parser.add_argument('--timeout', type=float, default=30)
    args = parser.parse_args()
    if args.privileged_cgroup_attach and args.app_cgroup is None:
        parser.error('--privileged-cgroup-attach requires --app-cgroup')
    if (args.gdk_debug_nogl or args.gdk_rendering_image) and (args.disable_gdk_gl or args.disable_dmabuf or args.disable_compositing):
        parser.error('new GTK3 graphics diagnostics must change one axis without other graphics flags')
    if ((args.toplevel_control_wlrctl and not args.wayland)
            or ((args.window_state or args.wlrctl_app_id) and not args.toplevel_control_wlrctl)):
        parser.error('window-state/app-ID selection requires --wayland and --toplevel-control-wlrctl')
    states = args.window_state or ['visible']
    if len(states) != len(set(states)):
        parser.error('each window-state can be measured only once per launch')
    if (args.starts < 1 or args.idle_seconds <= 0 or args.idle_settle_seconds < 0 or args.timeout <= 0
            or not all(math.isfinite(value) for value in (args.idle_seconds, args.idle_settle_seconds, args.timeout))):
        parser.error('starts, idle-seconds and timeout must be positive; idle-settle-seconds must be nonnegative; durations must be finite')
    display = None
    if args.wayland:
        try:
            display = wayland_display()
        except RuntimeError as error:
            parser.error(str(error))
    cgroup = None
    if args.app_cgroup is not None:
        try:
            cgroup = AppCgroup(args.app_cgroup, privileged_attach=args.privileged_cgroup_attach)
        except (RuntimeError, OSError, ValueError) as error:
            parser.error(str(error) if isinstance(error, RuntimeError) else 'app cgroup is unavailable or invalid')
    control = None
    if args.toplevel_control_wlrctl:
        app_id = args.wlrctl_app_id or args.binary.name
        if not app_id or len(app_id) > 256 or any(character.isspace() for character in app_id):
            parser.error('wlrctl app-ID must be a nonempty bounded identifier without whitespace')
        control = WlrToplevel(display, app_id)
        control.require_empty()
    webkit, webkit_source = webkit_version()
    inputs = {'binary_sha256': hashlib.sha256(args.binary.read_bytes()).hexdigest(),
              'engine_sha256': hashlib.sha256(args.engine.read_bytes()).hexdigest()}
    xvfb = None
    if not args.wayland:
        read_fd, write_fd = os.pipe()
        xvfb = subprocess.Popen(['Xvfb', '-displayfd', str(write_fd), '-screen', '0', '1280x800x24', '-nolisten', 'tcp'],
                                pass_fds=(write_fd,), stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        os.close(write_fd)
        display = ':' + os.read(read_fd, 32).decode().strip()
        os.close(read_fd)
    launches = []
    try:
        with tempfile.TemporaryDirectory(prefix='veranda-probe-', dir='/var/tmp') as temporary:
            for index in range(args.starts):
                directory = Path(temporary) / str(index)
                directory.mkdir(mode=0o700)
                env = environment(directory, display, args.engine.resolve(), wayland=args.wayland)
                fleet = configured_fleet(directory, env) if args.configured_fleet else None
                if args.disable_compositing:
                    env['WEBKIT_DISABLE_COMPOSITING_MODE'] = '1'
                if args.disable_dmabuf:
                    env['WEBKIT_DISABLE_DMABUF_RENDERER'] = '1'
                if args.disable_gdk_gl:
                    env['GDK_GL'] = 'disable'
                if args.gdk_debug_nogl:
                    env['GDK_DEBUG'] = 'nogl'
                if args.gdk_rendering_image:
                    env['GDK_RENDERING'] = 'image'
                if control is not None:
                    control.require_empty()
                launch_options = {'cgroup': cgroup} if cgroup is not None else {}
                process, tree, ready = launch(args.binary.resolve(), env, args.timeout,
                                             expected_fleet=FLEET_COUNTS if args.configured_fleet else None, **launch_options)
                try:
                    # Continue sampling through async details/renderer initialization;
                    # readiness timing remains the first usable fleet/error marker.
                    settle_until = time.monotonic() + 2
                    while time.monotonic() < settle_until:
                        time.sleep(0.05)
                        if process.poll() is not None:
                            raise RuntimeError('app exited during startup sampling')
                        tree.sample(pss=True)
                    launches.append({'ready_seconds': ready,
                                     'readiness_kind': getattr(tree, 'readiness_kind', 'frontend-two-frame-marker'),
                                     'peak_sampled_rss_bytes': tree.peak_rss,
                                     'peak_sampled_pss_bytes': tree.peak_pss, 'max_process_count': tree.max_count,
                                     'peak_processes': tree.peak_processes,
                                     'process_names': sorted(tree.names)})
                    print(f'Launch {index + 1}/{args.starts}: {ready:.3f}s', flush=True)
                    if index == args.starts - 1:
                        screenshot_captured = False if args.wayland else screenshot(display, args.screenshot)
                        window_states = {state: measure_idle(process, tree, args.idle_seconds, args.idle_settle_seconds,
                                                             control=control, state=state) for state in states}
                        idle = window_states[states[0]]
                        shown_again = control.set('visible') if control is not None else None
                        history = tree.process_history() if hasattr(tree, 'process_history') else []
                finally:
                    failed = sys.exc_info()[0] is not None
                    try:
                        tree.stop(process)
                        if cgroup is not None:
                            cgroup.require_empty()
                        if control is not None:
                            control.require_empty()
                    except BaseException:
                        if not failed:
                            raise
                        print('veranda-probe: app cleanup failed; original error preserved', file=sys.stderr)
                    if len(launches) == index + 1 and hasattr(tree, 'process_history'):
                        launches[-1]['process_history'] = tree.process_history()
                    if getattr(process, 'stdout', None) is not None:
                        process.stdout.close()
            timings = sorted(item['ready_seconds'] for item in launches)
            os_release = dict(line.split('=', 1) for line in Path('/etc/os-release').read_text().splitlines() if '=' in line)
            cpu = next((line.split(':', 1)[1].strip() for line in Path('/proc/cpuinfo').read_text().splitlines() if line.startswith('model name')), 'not measured')
            result = {
                'scope': ('Linux Wayland isolated local owner; screen, GPU, compositor and workload pinning remain caller gates; no prepared physical fleet or mutations'
                          if args.wayland else 'Linux Xvfb isolated local owner; screenshot defines rendered state; no prepared physical fleet or mutations'),
                'inputs': inputs,
                'configured_fleet': fleet,
                'display': ({'server': 'Wayland', 'screen': 'not measured', 'gpu': 'not measured', 'compositor': 'not measured',
                             'rendering': 'hardware acceleration unverified', 'screenshot_limitation': 'omitted to avoid capturing unrelated desktop content'}
                            if args.wayland else {'server': 'Xvfb', 'screen': '1280x800x24', 'rendering': 'hardware acceleration unverified'}),
                'diagnostic_disable_compositing': args.disable_compositing,
                'diagnostic_disable_dmabuf': args.disable_dmabuf,
                'diagnostic_disable_gdk_gl': args.disable_gdk_gl,
                'diagnostic_gdk_debug_nogl': args.gdk_debug_nogl,
                'diagnostic_gdk_rendering_image': args.gdk_rendering_image,
                'screenshot_captured': screenshot_captured,
                'system': {'os': os_release.get('PRETTY_NAME', '').strip('"'), 'kernel': os.uname().release,
                           'arch': os.uname().machine, 'cpu': cpu, 'logical_cpus': os.cpu_count(),
                           'webkit2gtk': webkit, 'webkit2gtk_version_source': webkit_source,
                           'desktop_packages': desktop_packages()},
                'method': {'startup': ('cold processes and fresh isolated app data; warm OS/library cache; '
                                       + ('native workload complete; no frontend frame'
                                          if launches[-1]['readiness_kind'].startswith('native-workload')
                                          else 'first-screen marker after initial fleet/error and two animation frames')),
                           'startup_sampling_seconds': 0.05, 'idle_sampling_seconds': 1,
                           'idle_settle_seconds': args.idle_settle_seconds,
                           'launch_peak_window': 'process start through two seconds after initial readiness, including asynchronous details',
                           'memory': 'simultaneous sum of per-process smaps_rollup RSS (shared pages double counted), PSS, private clean+dirty+hugetlb and swap; missing rollups use stat RSS and flag incomplete sums',
                           'process_scope': 'dedicated cgroup-v2 leaf plus previously observed identities' if cgroup is not None else 'process tree and discovered groups; unseen detached children may be missed',
                           'process_history': 'PID/starttime observed history; children shorter than sampling interval may be missed',
                           'peak': 'observed maximum simultaneous sum; 1 Hz may miss short peaks; never sum VmHWM',
                           'cgroup_memory': 'memory.current includes charged memory beyond process RSS/PSS; unavailable controllers yield null',
                           'resource_witness': 'numeric guest pressure/disk witnesses at idle endpoints and 1 Hz samples; overhead included in collection_seconds; unknown counters remain null; no pressure acceptance claim',
                           'cpu': 'sum sampled /proc CPU deltas; one core normalized; short-lived children between samples may be missed',
                           'mutations_measured': False, 'xvfb_included': False},
                'startup': {'count': len(launches), 'median_seconds': (timings[(len(timings)-1)//2] + timings[len(timings)//2]) / 2,
                            'p95_seconds': timings[math.ceil(0.95 * len(timings))-1],
                            'peak_sampled_rss_bytes': max(item['peak_sampled_rss_bytes'] for item in launches),
                            'peak_sampled_pss_bytes': max(item['peak_sampled_pss_bytes'] for item in launches), 'launches': launches},
                'idle': idle,
                'window_states': window_states,
                'shown_again': shown_again,
                'unmeasured': ['occluded', 'explicit-native-hidden', 'authoritative-state-after-show', 'frontend-focus/input',
                               'wakeups', 'redraws', 'subscriptions', 'pending-tasks', 'accessibility-state']
                              + ([] if control is not None and 'minimized' in states else ['minimized']),
                'process_history': history,
            }
            write_metrics(args.output, json.dumps(result, indent=2) + '\n')
            print(f'Metrics: {args.output}', flush=True)
    finally:
        if xvfb is not None:
            xvfb.terminate()
            xvfb.wait(timeout=3)


if __name__ == '__main__':
    main()
