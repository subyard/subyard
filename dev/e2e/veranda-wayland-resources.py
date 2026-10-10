#!/usr/bin/env python3
"""Measure one frozen Veranda on an owned disposable Debian VM with composed Wayland."""
import argparse
import contextlib
import importlib.util
import json
import os
from pathlib import Path
import pwd
import re
import signal
import stat
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parents[2]
sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location('veranda_probe', ROOT / 'dev/measure-veranda.py')
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)


def output_geometry(text):
    """Require one current 1080p/60Hz output; reject a merely advertised alternate mode."""
    if (text.count("interface: 'wl_output'") != 1
            or not re.search(r'\bscale: 1,', text)
            or not re.search(r'width: 1920 px, height: 1080 px, refresh: 60\.000 Hz,\s+flags: current(?: preferred)?\s*(?:\n|$)', text)):
        raise RuntimeError('Wayland output does not match 1920x1080 at 60 Hz and scale 1')
    return {'width': 1920, 'height': 1080, 'refresh_millihertz': 60000, 'scale': 1}


def private_log(path):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    os.fchmod(fd, 0o600)
    return os.fdopen(fd, 'w')


@contextlib.contextmanager
def cleanup_on_exit(action, label, observer=None):
    failed = False
    notification_error = None
    def notify(phase, rc, status):
        nonlocal notification_error
        if observer is not None:
            try:
                observer(phase, label, rc, status)
            except BaseException as error:
                notification_error = error
    try:
        yield
    except BaseException:
        failed = True
        raise
    finally:
        try:
            # A reporting failure must never prevent the owned cleanup action.
            notify('start', None, 'started')
            action()
        except BaseException as error:
            notify('end', error.returncode if isinstance(error, subprocess.CalledProcessError) else None, 'failed')
            if not failed:
                raise
            with contextlib.suppress(BaseException):
                print('veranda-wayland-resources: ' + label + ' cleanup failed; original error preserved', file=sys.stderr)
        else:
            notify('end', 0, 'passed')
            if notification_error is not None and not failed:
                raise notification_error


def create_state():
    # Keys must stay outside the checkout; do not inherit an ambient TMPDIR.
    state = Path(tempfile.mkdtemp(prefix='veranda-wayland-', dir='/var/tmp'))
    state.chmod(0o700)
    probe.private_file(state / '.marker', 'subyard-veranda-wayland-v1:' + state.name + '\n')
    info = state.lstat()
    return state, (info.st_uid, info.st_dev, info.st_ino)


def cleanup_state(state, identity):
    # Repeat the ownership checks inside the privileged helper, before deletion.
    code = '''import os, pathlib, re, stat, subprocess, sys
root = pathlib.Path(sys.argv[1])
uid, device, inode = map(int, sys.argv[2:])
assert root.parent == pathlib.Path('/var/tmp') and root.resolve() == root
assert re.fullmatch(r'veranda-wayland-[a-zA-Z0-9_]+', root.name)
info = root.lstat()
assert stat.S_ISDIR(info.st_mode) and stat.S_IMODE(info.st_mode) == 0o700
assert (info.st_uid, info.st_dev, info.st_ino) == (uid, device, inode)
fd = os.open(root / '.marker', os.O_RDONLY | os.O_NOFOLLOW)
with os.fdopen(fd) as marker:
    info = os.fstat(marker.fileno())
    assert stat.S_ISREG(info.st_mode) and info.st_uid == uid
    assert stat.S_IMODE(info.st_mode) == 0o600 and info.st_nlink == 1
    assert marker.read(256) == 'subyard-veranda-wayland-v1:' + root.name + '\\n'
subprocess.run(['find', '-P', str(root), '-xdev', '-depth', '-delete'],
               stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
               check=True, timeout=8)
assert not os.path.lexists(root)
'''
    subprocess.run(['sudo', '-n', 'timeout', '--signal=TERM', '--kill-after=2s', '10s',
                    sys.executable, '-c', code, str(state), *(str(value) for value in identity)],
                   stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                   check=True, timeout=15)


@contextlib.contextmanager
def recorded_state(observer=None):
    state, identity = create_state()
    def export_logs():
        # The observer redirects complete evidence privately; never echo it in chat.
        logs = {name: (state / name).read_text(errors='replace')
                for name in ('apt-update.log', 'apt-install.log', 'platform-stop-check.log', 'platform-prepare.log', 'platform-remove.log',
                             'platform-cleanup.log', 'weston.log', 'probe.log', 'probe-gdk.log')
                if (state / name).is_file()}
        print('VERANDA_WAYLAND_LOGS ' + json.dumps(logs), flush=True)
    with cleanup_on_exit(lambda: cleanup_state(state, identity), 'fixture', observer):
        with cleanup_on_exit(export_logs, 'logs'):
            yield state


def signal_product(sig, members):
    if not members:
        return
    # Root helpers belong only to our recorded command tree. PIDfds prevent
    # signalling a recycled PID between the identity check and the signal.
    code = '''import json, os, signal, sys
for pid, start in json.load(sys.stdin):
    fd = None
    try:
        fd = os.pidfd_open(pid)
        with open('/proc/%d/stat' % pid) as source:
            current = int(source.read().rsplit(')', 1)[1].split()[19])
        if current == start:
            signal.pidfd_send_signal(fd, int(sys.argv[1]))
    except (ProcessLookupError, FileNotFoundError):
        pass
    finally:
        if fd is not None: os.close(fd)
'''
    subprocess.run(['sudo', '-n', sys.executable, '-c', code, str(int(sig))],
                   input=json.dumps([[pid, item['start']] for pid, item in members.items()]),
                   text=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True, timeout=5)


def run_probe(command, env, log, *, privileged=False, timeout_seconds=600):
    process = subprocess.Popen(command, env=env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
    tree = probe.Tree(process.pid)
    try:
        deadline = time.monotonic() + timeout_seconds
        while True:
            tree.sample()
            rc = process.poll()
            if rc is not None:
                return rc
            if time.monotonic() >= deadline:
                raise RuntimeError('owned guest command timed out')
            time.sleep(0.25)
    finally:
        tree.stop(process, signal_processes=signal_product if privileged else None)


def product_stop_check(state, env):
    # Exercise the actual timeout path with owned root children before init.
    code = '''import signal, subprocess, sys, time
child = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(60)'], start_new_session=True)
def stop(*_):
    child.wait(timeout=3)
    sys.exit(0)
signal.signal(signal.SIGTERM, stop)
print('VERANDA_ROOT_STOP_READY', flush=True)
time.sleep(60)
'''
    with private_log(state / 'platform-stop-check.log') as log:
        try:
            run_probe(['sudo', '-n', sys.executable, '-c', code], env, log,
                      privileged=True, timeout_seconds=1)
        except RuntimeError as error:
            if str(error) != 'owned guest command timed out':
                raise
        else:
            raise RuntimeError('root command timeout fixture did not time out')
    if 'VERANDA_ROOT_STOP_READY\n' not in (state / 'platform-stop-check.log').read_text():
        raise RuntimeError('root command timeout fixture was not ready')
    check, identity = create_state()
    with cleanup_on_exit(lambda: cleanup_state(check, identity), 'root-file fixture'):
        subprocess.run(['sudo', '-n', 'mkdir', '-m', '0700', str(check / 'root-owned')], check=True, timeout=5)
        subprocess.run(['sudo', '-n', 'install', '-m', '0600', '/dev/null', str(check / 'root-owned/file')], check=True, timeout=5)
    print('VERANDA_ROOT_STATE_CLEANED', flush=True)


@contextlib.contextmanager
def prepared_platform(state, engine, observer=None):
    """Use the product reconciler; keep the temporary yard outside the measured fleet."""
    home = Path(pwd.getpwuid(os.getuid()).pw_dir)
    platform = home / '.cache/subyard-e2e-platform'
    if any(path.is_symlink() for path in (home / '.cache', platform)):
        raise RuntimeError('platform cache must be plain')
    subprocess.run(['sudo', '-n', 'install', '-d', '-o', str(os.getuid()), '-g', str(os.getgid()),
                    '-m', '0711', str(platform)], check=True, timeout=5)
    config, data = state / 'prepare-config', state / 'prepare-data'
    for directory in (config, data, config / 'yards'):
        probe.private_dir(directory)
    name = 'vp-' + state.name.rsplit('-', 1)[-1].replace('_', '-') + '-0'
    yard = config / 'yards' / name
    probe.private_dir(yard)
    probe.private_file(config / 'config.env', '')
    probe.private_file(yard / 'config.env',
                       'YARD_KIND=container\nSSH_PORT=64996\nLIMITS_MEMORY=512MiB\n'
                       f'HOST_BASE={state}/prepare-host\nRESTRICTED_DISK_PATHS={state}/prepare-host\n'
                       'FORWARD_SSH_AGENT=0\nENVIRONMENT_PROFILES=\nCODING_TOOL_INTEGRATIONS=\n'
                       'HOST_CLAUDE_MD=\nHOST_CODEX_AGENTS_MD=\nHOST_OPENCODE_AGENTS_MD=\n')
    env = {'PATH': '/usr/local/bin:/usr/bin:/bin', 'LANG': 'C.UTF-8', 'HOME': str(home),
           'SUBYARD_OPERATOR_HOME': str(home), 'SUBYARD_CONFIG_HOME': str(config), 'SUBYARD_HOME': str(data),
           'SUBYARD_REPOSITORY_ROOT': str(ROOT), 'STORAGE_PATH': str(platform / 'incus/incus/storage'),
           'SUBYARD_NO_AUDIT': '1', 'SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE': '1', 'MIN_DISK_GIB': '1'}
    probe.isolated_defaults(state, env)
    product_stop_check(state, env)

    def product(command, log_name):
        with private_log(state / log_name) as log:
            rc = run_probe([str(engine), '-Y', name, *command, '--yes'], env, log, privileged=True)
        if rc:
            raise RuntimeError(log_name.removesuffix('.log') + ' failed')

    with cleanup_on_exit(lambda: product(['teardown'], 'platform-cleanup.log'), 'platform', observer):
        product(['init'], 'platform-prepare.log')
        product(['teardown', '--keep-data'], 'platform-remove.log')
        substrate = platform / 'incus'
        if substrate.is_symlink() or not substrate.is_dir():
            raise RuntimeError('product preparation did not create the platform substrate')
        marker = state / 'platform-marker'
        probe.private_file(marker, 'subyard-e2e-platform-v1\n')
        pending = platform / ('.subyard-e2e-platform-marker.' + name)
        subprocess.run(['sudo', '-n', 'install', '-o', str(os.getuid()), '-g', str(os.getgid()),
                        '-m', '0600', str(marker), str(pending)], check=True, timeout=5)
        subprocess.run(['sudo', '-n', 'mv', '-fT', str(pending),
                        str(platform / '.subyard-e2e-platform-marker')], check=True, timeout=5)
        yield


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('deb', type=Path)
    parser.add_argument('engine', type=Path)
    parser.add_argument('--diagnostic-gdk-gl', action='store_true',
                        help='short paired normal/early GTK GL diagnostic; not budget acceptance')
    parser.add_argument('--configured-fleet', action='store_true',
                        help='verify 20 uncreated local yards/200 project names per fresh launch')
    args = parser.parse_args()
    if args.diagnostic_gdk_gl and args.configured_fleet:
        parser.error('the paired empty-owner diagnostic and configured fleet are separate checks')
    if os.environ.get('SUBYARD_E2E_VM') != '1' or os.environ.get('SUBYARD_E2E_TYPE') != 'android-test':
        parser.error('requires the allocated android-test singleton VM1')
    if os.getuid() == 0:
        parser.error('run the GUI as the allocated unprivileged operator')
    subprocess.run(['sudo', '-n', 'test', '-f', '/run/subyard-e2e-lease.json'], check=True, timeout=5)
    for path in (args.deb, args.engine):
        if not stat.S_ISREG(path.lstat().st_mode):
            parser.error('staged inputs must be plain files')
    if probe.command_output(['dpkg-deb', '-f', str(args.deb), 'Package', 'Version', 'Architecture']).splitlines() != ['Package: subyard-veranda', 'Version: 0.1.1', 'Architecture: amd64']:
        parser.error('requires the frozen 0.1.1 amd64 desktop package')
    args.deb, args.engine = args.deb.resolve(), args.engine.resolve()
    started = time.monotonic()
    with recorded_state() as state:
        for name in ('home', 'runtime', 'config', 'cache', 'data'):
            (state / name).mkdir(mode=0o700)
            (state / name).chmod(0o700)
        # Only the owned disposable guest changes. Preserve Incus, SSH and the base services.
        for name, command, timeout in (
                ('apt-update', ['apt-get', 'update', '-qq'], 120),
                ('apt-install', ['apt-get', 'install', '-y', '--no-install-recommends',
                                 'weston', 'wayland-utils', 'libegl-mesa0', 'libgl1-mesa-dri', str(args.deb)], 300)):
            with private_log(state / (name + '.log')) as log:
                result = subprocess.run(['sudo', '-n', 'env', 'DEBIAN_FRONTEND=noninteractive', *command],
                                        stdout=log, stderr=subprocess.STDOUT, timeout=timeout)
            if result.returncode:
                raise RuntimeError(name + ' failed')
        probe.webkit_version()  # Fail before spending the measurement window on missing metadata.
        env = {'PATH': '/usr/bin:/bin', 'LANG': 'C.UTF-8', 'HOME': str(state / 'home'),
               'XDG_RUNTIME_DIR': str(state / 'runtime'), 'XDG_CONFIG_HOME': str(state / 'config'),
               'XDG_CACHE_HOME': str(state / 'cache'), 'XDG_DATA_HOME': str(state / 'data'),
               'WAYLAND_DISPLAY': 'veranda-measure'}
        with contextlib.ExitStack() as preparation:
            if args.configured_fleet:
                preparation.enter_context(prepared_platform(state, args.engine))
            payload = measure(state, args, env)
        # Report only after compositor and product preparation cleanup complete.
        payload.update(platform_cleanup='passed' if args.configured_fleet else 'not_required',
                       privileged_command_cleanup='passed' if args.configured_fleet else 'not_required')
    payload.update(duration_seconds=time.monotonic() - started, fixture_cleanup='passed')
    print('VERANDA_WAYLAND_RESULT ' + json.dumps(payload), flush=True)


def measure(state, args, env):
    compositor = None
    compositor_tree = None
    try:
        with private_log(state / 'weston.log') as log:
            # Weston 14's headless auto renderer is noop; select actual GL composition.
            compositor = subprocess.Popen(['weston', '--no-config', '--backend=headless', '--renderer=gl',
                                            '--width=1920', '--height=1080', '--refresh-rate=60000',
                                            '--scale=1', '--idle-time=0', '--socket=veranda-measure'],
                                           env=env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
            compositor_tree = probe.Tree(compositor.pid)
            deadline = time.monotonic() + 10
            while True:
                compositor_tree.sample()
                if compositor.poll() is not None:
                    raise RuntimeError('Weston GL compositor exited before readiness')
                if time.monotonic() >= deadline:
                    raise RuntimeError('Weston GL compositor readiness timed out')
                try:
                    probe.wayland_display(env)
                except RuntimeError:
                    time.sleep(0.1)
                    continue
                break
            info = subprocess.check_output(['wayland-info', '--interface', 'wl_output'], env=env,
                                           text=True, stderr=subprocess.DEVNULL, timeout=3)
            geometry = output_geometry(info)
            renderer = re.search(r'GL renderer: ([ -~]{1,200})', (state / 'weston.log').read_text())
            if renderer is None:
                raise RuntimeError('Weston did not report its GL renderer')
            versions = {}
            for package in ('weston', 'wayland-utils', 'libwebkit2gtk-4.1-0', 'libegl-mesa0', 'libgl1-mesa-dri'):
                versions[package] = probe.command_output(['dpkg-query', '-W', '-f=${Version}', package])
            mem_total = next(int(line.split()[1]) * 1024 for line in Path('/proc/meminfo').read_text().splitlines()
                             if line.startswith('MemTotal:'))
            measurements = {}
            cases = (('normal', []), ('gdk', ['--disable-gdk-gl'])) if args.diagnostic_gdk_gl else (('normal', []),)
            for name, flags in cases:
                output = state / ('resources-' + name + '.json')
                with private_log(state / ('probe-gdk.log' if name == 'gdk' else 'probe.log')) as probe_log:
                    rc = run_probe([sys.executable, str(ROOT / 'dev/measure-veranda.py'), '--wayland',
                                    '--binary', '/usr/bin/subyard-veranda', '--engine', str(args.engine),
                                    '--output', str(output), '--starts', '3' if args.diagnostic_gdk_gl else '30',
                                    '--idle-seconds', '10' if args.diagnostic_gdk_gl else '120',
                                    '--idle-settle-seconds', '10' if args.diagnostic_gdk_gl else '30',
                                    *(['--configured-fleet'] if args.configured_fleet else []), *flags], env, probe_log)
                if rc:
                    raise RuntimeError('Wayland resource probe failed')
                measurements[name] = json.loads(output.read_text())
            if compositor.poll() is not None:
                raise RuntimeError('Weston exited during resource measurement')
            metrics = measurements['normal']
            setup = {'environment_type': 'android-test', 'nominal_memory_bytes': 8 * 1024**3,
                     'nominal_disk_bytes': 40 * 1024**3, 'observed_memory_bytes': mem_total,
                     'logical_cpus': os.cpu_count(), 'compositor': 'Weston headless GL',
                     'gl_renderer': renderer.group(1), 'screen': geometry, 'packages': versions,
                     'compositor_included_in_app_metrics': False,
                     'scope': ('Wayland VM configured/uncreated fleet control, 20 yards/200 names; initialization/import/runtime and hardware acceleration unverified'
                               if args.configured_fleet else 'Wayland VM control, fresh empty owner state; hardware acceleration and prepared fleet unverified')}
    finally:
        if compositor is not None:
            compositor_tree.stop(compositor)
    return {'schema_version': 1, 'setup': setup, 'metrics': metrics,
            'measurement_kind': 'short paired GDK diagnostic' if args.diagnostic_gdk_gl else 'normal control',
            'diagnostics': measurements if args.diagnostic_gdk_gl else {}, 'compositor_cleanup': 'passed'}


if __name__ == '__main__':
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(143))
    try:
        main()
    except Exception as error:
        # Do not echo raw subprocess payloads, inherited state or transport diagnostics.
        print('veranda-wayland-resources: ' + (str(error) if isinstance(error, RuntimeError) else type(error).__name__), file=sys.stderr)
        sys.exit(1)
