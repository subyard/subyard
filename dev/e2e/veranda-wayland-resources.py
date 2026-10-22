#!/usr/bin/env python3
"""Measure one frozen Veranda on an owned disposable Debian VM with composed Wayland."""
import argparse
import contextlib
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import pwd
import re
import selectors
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
spec = importlib.util.spec_from_file_location('veranda_prepared', ROOT / 'dev/e2e/veranda-prepared-assets.py')
prepared = importlib.util.module_from_spec(spec)
spec.loader.exec_module(prepared)


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
                             'platform-cleanup.log', 'platform-release-package.log', 'platform-release-install.log',
                             'platform-release-config.log', 'platform-release-migrate.log', 'platform-release-check.log',
                             'platform-prepared-image.log', 'weston.log', 'compositor.log', 'xvfb.log', 'window-manager.log',
                             'x11-display-ready.log', 'x11-wm-ready.log', 'session-bus.log', 'accessibility.log',
                             'geometry.log', 'gtk-stderr.log', 'probe.log', 'probe-gdk.log')
                if (state / name).is_file()}
        for path in state.glob('builder-*.log'):
            assert path.is_file() and not path.is_symlink() and path.stat().st_size <= 2 * 1024 * 1024
            logs[path.name] = path.read_text(errors='replace')
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


def prepare_release(state, engine, env, prepared_assets=None):
    """Commit fresh capabilities through the same-candidate installed release owner."""
    version = subprocess.check_output([str(engine), '--version'], env=env, text=True, timeout=5).strip()
    if not re.fullmatch(r'yard [A-Za-z0-9][A-Za-z0-9._+-]*', version):
        raise RuntimeError('platform engine version is invalid')
    release = state / 'prepare-release'
    runtime = Path(env['SUBYARD_HOME']) / 'runtime'
    env['YARD_RUNTIME_ROOT'] = str(runtime)
    def command(argv, name):
        with private_log(state / ('platform-release-' + name + '.log')) as log:
            if run_probe(argv, env, log, privileged=True):
                raise RuntimeError('platform release ' + name + ' failed')
    if prepared_assets is None:
        command(['bash', str(ROOT / 'dev/package-engine.sh'), '--version', version.removeprefix('yard '),
                 '--output-dir', str(release)], 'package')
        bundles = list(release.glob('subyard-*-linux-*.tar.gz'))
        if len(bundles) != 1:
            raise RuntimeError('platform release bundle is ambiguous')
        bundle = bundles[0]
    else:
        assets = prepared.validate(prepared_assets, engine, version.removeprefix('yard '), ROOT)
        bundle = Path(prepared_assets) / assets['runtime']['bundle']
    command(['bash', str(ROOT / 'scripts/install-runtime-release.sh'), '--runtime-root', str(runtime),
             '--bundle', str(bundle), '--checksum', str(bundle) + '.sha256',
             '--manifest', str(bundle) + '.manifest.json', '--provenance', str(bundle) + '.provenance.json'], 'install')
    installed = runtime / 'current/bin/yard-engine'
    if hashlib.sha256(installed.read_bytes()).digest() != hashlib.sha256(engine.read_bytes()).digest():
        raise RuntimeError('platform installed engine differs from the candidate')
    launcher = runtime / 'current/bin/yard'
    # Keep the concrete source diagnostic before migration renders its public
    # dependency-unavailable summary. This is a read-only prerequisite.
    command([str(launcher), 'config', 'status'], 'config')
    command([str(launcher), 'migrate', '--yes'], 'migrate')
    command([str(launcher), 'migrate', '--check', '--json'], 'check')
    if json.loads((state / 'platform-release-check.log').read_text())['outcome']['status'] != 'ready':
        raise RuntimeError('platform release is not ready')


def platform_environment(state):
    """One isolated configuration for platform preparation and its preflight."""
    home = Path(pwd.getpwuid(os.getuid()).pw_dir)
    platform = home / '.cache/subyard-e2e-platform'
    if any(path.is_symlink() for path in (home / '.cache', platform)):
        raise RuntimeError('platform cache must be plain')
    subprocess.run(['sudo', '-n', 'install', '-d', '-o', str(os.getuid()), '-g', str(os.getgid()),
                    '-m', '0711', str(platform)], check=True, timeout=5)
    config, data, units = state / 'prepare-config', state / 'prepare-data', state / 'prepare-key-units'
    for directory in (config, data, config / 'yards', units):
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
           'SUBYARD_NO_AUDIT': '1', 'SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE': '1',
           'SUBYARD_KEYS_SYSTEMD_DIR': str(units), 'MIN_DISK_GIB': '1'}
    probe.isolated_defaults(state, env)
    return platform, name, env


@contextlib.contextmanager
def prepared_platform(state, engine, observer=None, prepared_assets=None):
    """Use the product reconciler; keep the temporary yard outside the measured fleet."""
    platform, name, env = platform_environment(state)
    product_stop_check(state, env)
    prepare_release(state, engine, env, prepared_assets) if prepared_assets is not None else prepare_release(state, engine, env)

    def product(command, log_name):
        with private_log(state / log_name) as log:
            rc = run_probe([str(engine), '-Y', name, *command, '--yes'], env, log, privileged=True)
        if rc:
            raise RuntimeError(log_name.removesuffix('.log') + ' failed')

    with contextlib.ExitStack() as assets_context:
        if prepared_assets is not None:
            assets = prepared.validate(prepared_assets, engine, subprocess.check_output(
                [str(engine), '--version'], env=env, text=True, timeout=5).strip().removeprefix('yard '), ROOT)
            image_log = assets_context.enter_context(private_log(state / 'platform-prepared-image.log'))
            def incus_command(argv, timeout):
                result = subprocess.run(argv, env=env, stdin=subprocess.DEVNULL, capture_output=True,
                                        text=True, timeout=timeout)
                # Never persist unrelated remote/client configuration or certificates.
                image_log.write(json.dumps({'command': argv[1:3], 'original_exit': result.returncode}) + '\n')
                if '--help' in argv or argv[1:] == ['version']:
                    image_log.write(result.stdout)
                if result.returncode:
                    image_log.write(result.stderr)
                image_log.flush()
                if result.returncode:
                    raise subprocess.CalledProcessError(result.returncode, argv)
                return result.stdout
            reference = assets_context.enter_context(prepared.image_remote(
                prepared_assets, assets, state, env, incus_command))
            with (Path(env['SUBYARD_CONFIG_HOME']) / 'yards' / name / 'config.env').open('a') as config:
                config.write('YARD_IMAGE=' + reference + '\nYARD_IMAGE_FALLBACK=' + reference + '\n')
        assets_context.enter_context(cleanup_on_exit(lambda: product(['teardown'], 'platform-cleanup.log'), 'platform', observer))
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
    parser.add_argument('--gtk', action='store_true', help='current GTK ordinary baseline; historical marker/WebKit path otherwise')
    parser.add_argument('--x11', action='store_true', help='GTK only: private Xvfb/Openbox at 1920x1080')
    parser.add_argument('--short', action='store_true', help='GTK only: one launch/one second idle prerequisite, not budget acceptance')
    parser.add_argument('--child-usage', action='store_true', help='GTK only: retain requested per-launch raw child and app observations')
    parser.add_argument('--correlation-config', type=Path, help='optional private template; requires --child-usage; no producer')
    parser.add_argument('--diagnostic-gdk-gl', action='store_true',
                        help='short paired normal/early GTK GL diagnostic; not budget acceptance')
    parser.add_argument('--configured-fleet', action='store_true',
                        help='verify 20 uncreated local yards/200 project names per fresh launch')
    parser.add_argument('--prepared-assets', type=Path,
                        help='GTK only: validated staged runtime and exact Debian container image assets')
    args = parser.parse_args()
    if args.child_usage and not args.gtk: parser.error('--child-usage requires --gtk')
    if args.correlation_config and not args.child_usage: parser.error('--correlation-config requires --child-usage')
    if args.prepared_assets is not None and not args.gtk:
        parser.error('--prepared-assets requires --gtk')
    if (args.x11 or args.short) and not args.gtk:
        parser.error('--x11/--short require --gtk')
    if args.gtk and args.diagnostic_gdk_gl:
        parser.error('legacy GL diagnostic is separate from GTK')
    if args.gtk:
        args.configured_fleet = True
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
    metadata = probe.command_output(['dpkg-deb', '-f', str(args.deb), 'Package', 'Version', 'Architecture']).splitlines()
    if (len(metadata) != 3 or metadata[0] != 'Package: subyard-veranda' or metadata[2] != 'Architecture: amd64'
            or not re.fullmatch(r'Version: [A-Za-z0-9][A-Za-z0-9.+~:-]*', metadata[1])):
        parser.error('requires an explicit amd64 subyard-veranda package')
    args.product_version = metadata[1].removeprefix('Version: ')
    if probe.command_output([str(args.engine), '--version']) != 'yard ' + args.product_version:
        parser.error('package and supplied engine versions must match exactly')
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
                                 *(['libgtk-4-1', 'libegl-mesa0', 'libgl1-mesa-dri', 'dbus', 'at-spi2-core', 'python3-gi', 'gir1.2-atspi-2.0',
                                     *(['xvfb', 'openbox', 'xdotool', 'x11-utils', 'libglx-mesa0'] if args.x11 else ['labwc', 'wlrctl', 'wlr-randr', 'wayland-utils'])]
                                   if args.gtk else ['weston', 'wayland-utils', 'libegl-mesa0', 'libgl1-mesa-dri']), str(args.deb)], 300)):
            with private_log(state / (name + '.log')) as log:
                result = subprocess.run(['sudo', '-n', 'env', 'DEBIAN_FRONTEND=noninteractive', *command],
                                        stdout=log, stderr=subprocess.STDOUT, timeout=timeout)
            if result.returncode:
                raise RuntimeError(name + ' failed')
        if not args.gtk:
            probe.webkit_version()  # Historical WebKit metadata only.
        elif probe.command_output(['/usr/bin/subyard-veranda', '--version']) != 'Subyard Veranda ' + args.product_version:
            raise RuntimeError('installed GUI version differs from the supplied package/engine')
        env = {'PATH': '/usr/bin:/bin', 'LANG': 'C.UTF-8', 'HOME': str(state / 'home'),
               'XDG_RUNTIME_DIR': str(state / 'runtime'), 'XDG_CONFIG_HOME': str(state / 'config'),
               'XDG_CACHE_HOME': str(state / 'cache'), 'XDG_DATA_HOME': str(state / 'data'),
               'WAYLAND_DISPLAY': 'veranda-measure'}
        with contextlib.ExitStack() as preparation:
            if args.configured_fleet:
                preparation.enter_context(prepared_platform(state, args.engine, prepared_assets=args.prepared_assets)
                                          if args.prepared_assets is not None else prepared_platform(state, args.engine))
            payload = measure_gtk(state, args, env) if args.gtk else measure(state, args, env)
        # Report only after compositor and product preparation cleanup complete.
        payload.update(platform_cleanup='passed' if args.configured_fleet else 'not_required',
                       privileged_command_cleanup='passed' if args.configured_fleet else 'not_required')
    payload.update(duration_seconds=time.monotonic() - started, fixture_cleanup='passed')
    print('VERANDA_WAYLAND_RESULT ' + json.dumps(payload), flush=True)


def measure_gtk(state, args, env):
    """Owned controllable Linux displays; probes/compositor stay outside app sums."""
    env.pop('WAYLAND_DISPLAY', None)
    env.update(GTK_A11Y='atspi',
               XDG_SESSION_TYPE='x11' if args.x11 else 'wayland',
               GDK_BACKEND='x11' if args.x11 else 'wayland',
               DBUS_SESSION_BUS_ADDRESS='unix:path=' + str(state / 'session-bus'),
               STORAGE_PATH=str(Path(pwd.getpwuid(os.getuid()).pw_dir) / '.cache/subyard-e2e-platform/incus/incus/storage'))
    with contextlib.ExitStack() as stack:
        def service(name, command, **options):
            log = stack.enter_context(private_log(state / (name + '.log')))
            process = subprocess.Popen(command, env=env, stdout=log, stderr=subprocess.STDOUT,
                                       start_new_session=True, **options)
            tree = probe.Tree(process.pid)
            stack.enter_context(cleanup_on_exit(lambda: tree.stop(process), name))
            return process
        service('session-bus', ['dbus-daemon', '--session', '--nofork', '--nopidfile', '--address=' + env['DBUS_SESSION_BUS_ADDRESS']])
        deadline = time.monotonic() + 10
        while not (state / 'session-bus').is_socket():
            if time.monotonic() >= deadline: raise RuntimeError('private session bus readiness timed out')
            time.sleep(.05)
        launchers = [path for path in subprocess.check_output(['dpkg-query', '-L', 'at-spi2-core'], text=True, timeout=5).splitlines()
                     if path.endswith('/at-spi-bus-launcher')]
        if len(launchers) != 1: raise RuntimeError('accessibility launcher is ambiguous')
        service('accessibility', [launchers[0], '--launch-immediately'])
        flags = []
        if args.x11:
            # Reuse the existing SHORT's real display/EWMH readiness contract.
            spec = importlib.util.spec_from_file_location('veranda_display_ready', ROOT / 'veranda/desktop/wayland-short.py')
            short = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(short)
            deadline = time.monotonic() + 10
            read_fd, write_fd = os.pipe()
            try:
                try:
                    display = service('xvfb', ['Xvfb', '-noreset', '-displayfd', str(write_fd), '-screen', '0',
                                               '1920x1080x24', '-nolisten', 'tcp'], pass_fds=(write_fd,))
                finally:
                    os.close(write_fd)
                number = probe.read_display_number(read_fd, deadline)
                env['DISPLAY'] = ':' + number
            finally:
                os.close(read_fd)
            short.wait_x11_ready(display, None, env, deadline, state / 'x11-display-ready.log')
            manager = service('window-manager', ['openbox'])
            short.wait_x11_ready(display, manager, env, deadline, state / 'x11-wm-ready.log')
            dimensions = subprocess.check_output(['xdotool', 'getdisplaygeometry'], env=env, text=True, timeout=3).strip()
            if dimensions != '1920 1080': raise RuntimeError('X11 geometry differs from 1920x1080')
            geometry = {'width': 1920, 'height': 1080, 'scale': 1, 'refresh_millihertz': None}
            flags = ['--x11-display', env['DISPLAY']]
            compositor_name = 'Xvfb/Openbox'
            packages = ['xvfb', 'openbox', 'xdotool', 'libgtk-4-1', 'libegl-mesa0', 'libgl1-mesa-dri', 'libglx-mesa0']
        else:
            probe.private_dir(state / 'compositor')
            probe.private_file(state / 'compositor/rc.xml', '<labwc_config><core><xwayland>no</xwayland></core></labwc_config>\n')
            env.update(WLR_BACKENDS='headless', WLR_HEADLESS_OUTPUTS='1', WLR_RENDERER='pixman',
                       WLR_LIBINPUT_NO_DEVICES='1', LABWC_UPDATE_ACTIVATION_ENV='0',
                       XDG_CONFIG_DIRS=str(state / 'compositor'))
            compositor = service('compositor', ['labwc', '-V', '-C', str(state / 'compositor')])
            deadline = time.monotonic() + 15
            while True:
                if compositor.poll() is not None: raise RuntimeError('owned labwc exited before readiness')
                if time.monotonic() >= deadline: raise RuntimeError('owned labwc readiness timed out')
                sockets = [path for path in (state / 'runtime').glob('wayland-*') if stat.S_ISSOCK(path.lstat().st_mode)]
                if len(sockets) == 1:
                    env['WAYLAND_DISPLAY'] = str(sockets[0])
                    break
                time.sleep(.05)
            if 'pixman' not in (state / 'compositor.log').read_text().lower():
                raise RuntimeError('labwc renderer was not confirmed as Pixman')
            with private_log(state / 'geometry.log') as log:
                subprocess.run(['wlr-randr', '--output', 'HEADLESS-1', '--custom-mode', '1920x1080@60Hz', '--scale', '1'],
                               env=env, stdout=log, stderr=subprocess.STDOUT, check=True, timeout=5)
            geometry = output_geometry(subprocess.check_output(['wayland-info', '--interface', 'wl_output'],
                                       env=env, text=True, stderr=subprocess.DEVNULL, timeout=5))
            flags = ['--wayland', '--toplevel-control-wlrctl', '--wlrctl-app-id', 'org.subyard.Veranda',
                     '--window-state', 'visible', '--window-state', 'minimized']
            compositor_name = 'labwc headless/Pixman'
            packages = ['labwc', 'wlrctl', 'wlr-randr', 'libgtk-4-1', 'libegl-mesa0', 'libgl1-mesa-dri']
        output = state / 'resources-gtk.json'
        with private_log(state / 'probe.log') as log:
            rc = run_probe([sys.executable, str(ROOT / 'dev/measure-veranda.py'), '--gtk', '--configured-fleet',
                            '--binary', '/usr/bin/subyard-veranda', '--engine', str(args.engine), '--output', str(output),
                            '--gtk-stderr-log', str(state / 'gtk-stderr.log'), '--starts', '1' if args.short else '30', '--idle-seconds', '1' if args.short else '120',
                            '--idle-settle-seconds', '0' if args.short else '30', *(['--child-usage'] if args.child_usage else []),
                            *(['--correlation-config', str(args.correlation_config)] if args.correlation_config else []), *flags], env, log,
                           timeout_seconds=180 if args.short else 1200)
        if not output.is_file(): raise RuntimeError('GTK resource probe failed without metrics')
        metrics = json.loads(output.read_text())
        # Emit measured over-limit results before surfacing the native failure.
        if rc: print('VERANDA_GTK_RESOURCE_FAILURE ' + json.dumps(metrics), flush=True)
        if rc: raise RuntimeError('GTK resource probe failed')
        if args.child_usage and metrics.get('observation_status') != 'passed':
            print('VERANDA_GTK_RESOURCE_FAILURE ' + json.dumps(metrics), flush=True)
            raise RuntimeError('requested raw resource observations unavailable')
        return {'schema_version': 1, 'measurement_kind': 'GTK one-launch prerequisite; not budget acceptance' if args.short else 'GTK ordinary sampled baseline',
                'setup': {'backend': 'x11' if args.x11 else 'wayland', 'compositor': compositor_name,
                          'screen': geometry, 'renderer': 'shipping GTK default/auto; GSK_RENDERER unset; successful GSK class evidence per launch (GSK_DEBUG=renderer); hardware acceleration unmeasured',
                          'compositor_renderer': 'Pixman' if not args.x11 else 'Xvfb software display',
                          'packages': {package: probe.command_output(['dpkg-query', '-W', '-f=${Version}', package]) for package in packages},
                          'product_version': args.product_version,
                          'installed_gui_sha256': hashlib.sha256(Path('/usr/bin/subyard-veranda').read_bytes()).hexdigest(),
                          'input_deb_sha256': hashlib.sha256(args.deb.read_bytes()).hexdigest(),
                          'observed_memory_bytes': next(int(line.split()[1]) * 1024 for line in Path('/proc/meminfo').read_text().splitlines() if line.startswith('MemTotal:')),
                          'logical_cpus': os.cpu_count(), 'compositor_and_observer_in_app_metrics': False},
                'metrics': metrics, 'compositor_cleanup': 'passed'}


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
            'measurement_kind': 'historical marker/WebKit paired GDK diagnostic' if args.diagnostic_gdk_gl else 'historical marker/WebKit normal control',
            'diagnostics': measurements if args.diagnostic_gdk_gl else {}, 'compositor_cleanup': 'passed'}


if __name__ == '__main__':
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(143))
    try:
        main()
    except Exception as error:
        # Do not echo raw subprocess payloads, inherited state or transport diagnostics.
        print('veranda-wayland-resources: ' + (str(error) if isinstance(error, RuntimeError) else type(error).__name__), file=sys.stderr)
        sys.exit(1)
