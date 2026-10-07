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
import struct
import subprocess
import tempfile
import time
import zlib

ROOT = Path(__file__).resolve().parents[1]
PAGE = os.sysconf('SC_PAGE_SIZE')
TICKS = os.sysconf('SC_CLK_TCK')
FLEET_COUNTS = {'owners': 1, 'yards': 20, 'projects': 200}


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
    def __init__(self, pid):
        self.pid = pid
        self.known = {}
        self.previous = {}
        self.cpu_ticks = 0
        self.peak_rss = 0
        self.peak_pss = 0
        self.names = set()
        self.max_count = 0
        self.peak_processes = []

    def sample(self, pss=False, attribution=False):
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
        rss = total_pss = 0
        pss_missing = 0
        breakdown = []
        for pid in chosen:
            item = all_processes[pid]
            key = (pid, item['start'])
            self.known[pid] = item['start']
            self.cpu_ticks += max(0, item['ticks'] - self.previous.get(key, item['ticks']))
            self.previous[key] = item['ticks']
            rss += item['rss']
            self.names.add(item['name'])
            process_pss = None
            if pss:
                try:
                    values = (Path('/proc') / str(pid) / 'smaps_rollup').read_text().splitlines()
                    process_pss = next(int(line.split()[1]) * 1024 for line in values if line.startswith('Pss:'))
                    total_pss += process_pss
                except (OSError, ValueError, StopIteration):
                    pss_missing += 1
            entry = {'pid': pid, 'name': item['name'], 'rss_bytes': item['rss'],
                     'pss_bytes': process_pss, 'threads': item['threads']}
            if attribution:
                entry.update(memory_attribution(pid))
            breakdown.append(entry)
        if rss > self.peak_rss:
            self.peak_processes = breakdown
        self.peak_rss = max(self.peak_rss, rss)
        if pss:
            self.peak_pss = max(self.peak_pss, total_pss)
        self.max_count = max(self.max_count, len(chosen))
        return {'rss_bytes': rss, 'pss_bytes': total_pss if pss else None,
                'pss_missing_processes': pss_missing, 'process_count': len(chosen),
                'processes': breakdown}

    def stop(self, process, *, signal_processes=None):
        self.sample()
        # RPC children use independent groups. Kill only groups discovered in this tree.
        def remaining():
            return {pid: item for pid, item in processes().items()
                    if self.known.get(pid) == item['start']}

        def signal_owned(sig):
            if signal_processes is not None:
                signal_processes(sig, remaining())
                return
            for group in {item['group'] for item in remaining().values()} - {os.getpgrp()}:
                try:
                    os.killpg(group, sig)
                except ProcessLookupError:
                    pass

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


def launch(binary, env, timeout, *, expected_fleet=None, stderr=subprocess.DEVNULL):
    started = time.monotonic()
    process = subprocess.Popen([str(binary)], cwd=ROOT, env=env, start_new_session=True,
                               stdout=subprocess.PIPE, stderr=stderr)
    tree = Tree(process.pid)
    selector = selectors.DefaultSelector()
    selector.register(process.stdout, selectors.EVENT_READ)
    buffered = b''
    try:
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
                    ready = time.monotonic() - started
                    tree.sample(pss=True)
                    return process, tree, ready
                buffered = buffered[-4096:]
            if process.poll() is not None:
                raise RuntimeError(f'app exited before ready (status {process.returncode})')
        raise RuntimeError('first-screen readiness timeout')
    except BaseException:
        tree.stop(process)
        raise
    finally:
        selector.close()


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
    parser.add_argument('--binary', type=Path, default=ROOT / 'veranda/src-tauri/target/release/subyard-veranda')
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
    parser.add_argument('--starts', type=int, default=30)
    parser.add_argument('--idle-seconds', type=float, default=120)
    parser.add_argument('--idle-settle-seconds', type=float, default=30,
                        help='settle the final launch before the idle measurement (default: 30)')
    parser.add_argument('--timeout', type=float, default=30)
    args = parser.parse_args()
    if (args.starts < 1 or args.idle_seconds <= 0 or args.idle_settle_seconds < 0 or args.timeout <= 0
            or not all(math.isfinite(value) for value in (args.idle_seconds, args.idle_settle_seconds, args.timeout))):
        parser.error('starts, idle-seconds and timeout must be positive; idle-settle-seconds must be nonnegative; durations must be finite')
    display = None
    if args.wayland:
        try:
            display = wayland_display()
        except RuntimeError as error:
            parser.error(str(error))
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
                process, tree, ready = launch(args.binary.resolve(), env, args.timeout,
                                             expected_fleet=FLEET_COUNTS if args.configured_fleet else None)
                try:
                    # Continue sampling through async details/renderer initialization;
                    # readiness timing remains the first usable fleet/error marker.
                    settle_until = time.monotonic() + 2
                    while time.monotonic() < settle_until:
                        time.sleep(0.05)
                        if process.poll() is not None:
                            raise RuntimeError('app exited during startup sampling')
                        tree.sample(pss=True)
                    launches.append({'ready_seconds': ready, 'peak_sampled_rss_bytes': tree.peak_rss,
                                     'peak_sampled_pss_bytes': tree.peak_pss, 'max_process_count': tree.max_count,
                                     'peak_processes': tree.peak_processes,
                                     'process_names': sorted(tree.names)})
                    print(f'Launch {index + 1}/{args.starts}: {ready:.3f}s', flush=True)
                    if index == args.starts - 1:
                        settle_until = time.monotonic() + args.idle_settle_seconds
                        while time.monotonic() < settle_until:
                            time.sleep(0.25)
                            if process.poll() is not None:
                                raise RuntimeError('app exited before idle measurement')
                            tree.sample(pss=True)
                        screenshot_captured = False if args.wayland else screenshot(display, args.screenshot)
                        baseline = tree.sample(pss=True, attribution=True)
                        initial_cpu = tree.cpu_ticks
                        idle_started = time.monotonic()
                        idle_samples = []
                        while time.monotonic() - idle_started < args.idle_seconds:
                            time.sleep(0.25)
                            if process.poll() is not None:
                                raise RuntimeError('app exited during idle measurement')
                            idle_samples.append(tree.sample(pss=True))
                        elapsed = time.monotonic() - idle_started
                        idle = {'duration_seconds': elapsed,
                                'cpu_seconds': (tree.cpu_ticks - initial_cpu) / TICKS,
                                'cpu_percent_one_core': 100 * (tree.cpu_ticks - initial_cpu) / TICKS / elapsed,
                                'baseline': baseline, 'last': tree.sample(pss=True, attribution=True),
                                'peak_rss_bytes': max(sample['rss_bytes'] for sample in idle_samples),
                                'peak_pss_bytes': max(sample['pss_bytes'] for sample in idle_samples),
                                'pss_missing_process_samples': sum(sample['pss_missing_processes'] for sample in idle_samples)}
                finally:
                    tree.stop(process)
            timings = sorted(item['ready_seconds'] for item in launches)
            os_release = dict(line.split('=', 1) for line in Path('/etc/os-release').read_text().splitlines() if '=' in line)
            cpu = next(line.split(':', 1)[1].strip() for line in Path('/proc/cpuinfo').read_text().splitlines() if line.startswith('model name'))
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
                'screenshot_captured': screenshot_captured,
                'system': {'os': os_release.get('PRETTY_NAME', '').strip('"'), 'kernel': os.uname().release,
                           'arch': os.uname().machine, 'cpu': cpu, 'logical_cpus': os.cpu_count(),
                           'webkit2gtk': webkit, 'webkit2gtk_version_source': webkit_source,
                           'desktop_packages': desktop_packages()},
                'method': {'startup': 'cold processes and fresh isolated app data; warm OS/library cache; first-screen marker after initial fleet/error and two animation frames',
                           'startup_sampling_seconds': 0.05, 'idle_sampling_seconds': 0.25,
                           'idle_settle_seconds': args.idle_settle_seconds,
                           'launch_peak_window': 'process start through two seconds after initial readiness, including asynchronous details',
                           'memory': 'aggregate process-tree and discovered process-group RSS (shared pages double counted); PSS measured separately',
                           'cpu': 'sum sampled /proc CPU deltas; one core normalized; short-lived children between samples may be missed',
                           'mutations_measured': False, 'xvfb_included': False},
                'startup': {'count': len(launches), 'median_seconds': (timings[(len(timings)-1)//2] + timings[len(timings)//2]) / 2,
                            'p95_seconds': timings[math.ceil(0.95 * len(timings))-1],
                            'peak_sampled_rss_bytes': max(item['peak_sampled_rss_bytes'] for item in launches),
                            'peak_sampled_pss_bytes': max(item['peak_sampled_pss_bytes'] for item in launches), 'launches': launches},
                'idle': idle,
            }
            args.output.write_text(json.dumps(result, indent=2) + '\n')
            print(f'Metrics: {args.output}', flush=True)
    finally:
        if xvfb is not None:
            xvfb.terminate()
            xvfb.wait(timeout=3)


if __name__ == '__main__':
    main()
