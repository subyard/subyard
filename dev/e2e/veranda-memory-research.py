#!/usr/bin/env python3
"""Compare short Veranda idle/minimized measurements on an owned Debian Wayland VM."""
import argparse
import contextlib
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import signal
import stat
import subprocess
import sys
import time

sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('wayland_fixture', ROOT / 'dev/e2e/veranda-wayland-resources.py')
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)
probe = fixture.probe


def require(value):
    if not value:
        raise ValueError('fixture guard failed')


def guest_resources():
    """Record only the resource descriptor forwarded from the validated grant."""
    cpu = os.environ.get('SUBYARD_E2E_CPU_PER_VM', '')
    memory = os.environ.get('SUBYARD_E2E_MEMORY_PER_VM', '')
    disk = os.environ.get('SUBYARD_E2E_DISK_PER_VM', '')
    base = os.environ.get('SUBYARD_E2E_BASE_FINGERPRINT', '')
    require(re.fullmatch(r'[1-9][0-9]{0,8}', cpu)
            and re.fullmatch(r'[1-9][0-9]{0,8}(MiB|GiB|MB|GB)', memory)
            and re.fullmatch(r'[1-9][0-9]{0,8}(MiB|GiB|MB|GB)', disk)
            and re.fullmatch(r'[0-9a-f]{64}', base)
            and os.environ.get('SUBYARD_E2E_VM_COUNT') == '1'
            and os.environ.get('SUBYARD_E2E_TYPE') in {'subyard-pair', 'android-test'})
    return {'environment_type': os.environ['SUBYARD_E2E_TYPE'], 'vm_count': 1,
            'cpu_per_vm': int(cpu), 'memory_per_vm': memory, 'disk_per_vm': disk,
            'base_fingerprint': base}


def geometry(text):
    result = fixture.output_geometry(text)
    require(result == {'width': 1920, 'height': 1080, 'refresh_millihertz': 60000, 'scale': 1})
    return result


def installed_binary(deb):
    import tarfile
    process = subprocess.Popen(['dpkg-deb', '--fsys-tarfile', str(deb)], stdout=subprocess.PIPE,
                               stderr=subprocess.DEVNULL)
    digest = None
    try:
        with tarfile.open(fileobj=process.stdout, mode='r|') as archive:
            for member in archive:
                if member.name.lstrip('./') == 'usr/bin/subyard-veranda':
                    require(member.isfile() and 0 < member.size < 100 * 1024**2 and digest is None)
                    digest = hashlib.sha256(archive.extractfile(member).read()).hexdigest()
        require(process.wait(timeout=10) == 0 and digest is not None)
    finally:
        if process.poll() is None:
            process.kill()
        process.wait(timeout=3)
        process.stdout.close()
    return digest


def install_candidate(deb, installed_sha256, install_log):
    """Return an installation identity only after its payload and executable agree."""
    deb_sha256 = hashlib.sha256(deb.read_bytes()).hexdigest()
    binary_hash = installed_binary(deb)
    if deb_sha256 != installed_sha256:
        subprocess.run(['sudo', '-n', 'env', 'DEBIAN_FRONTEND=noninteractive',
                        'apt-get', 'install', '--reinstall', '-y', '--no-install-recommends', str(deb)],
                       stdout=install_log, stderr=subprocess.STDOUT, check=True, timeout=180)
    else:
        print('Candidate already installed; verifying package and executable before measurement.', file=install_log)
    require(hashlib.sha256(Path('/usr/bin/subyard-veranda').read_bytes()).hexdigest() == binary_hash)
    return deb_sha256, binary_hash


def group_command(action, state, group, identity=None):
    # Root changes only this exclusive leaf, bound to the lease and private fixture.
    code = '''import json, os, pathlib, re, stat, sys
action, state_value, group_name, uid_value, run, slot, *identity = sys.argv[1:]
uid = int(uid_value)
assert os.getuid() == 0 and uid > 0 and action in {'validate', 'create', 'remove'}
assert re.fullmatch(r'[0-9a-f]{8}', run) and re.fullmatch(r'slot-[0-9]{3}', slot) and slot != 'slot-000'
lease_path = pathlib.Path('/run/subyard-e2e-lease.json')
info = lease_path.lstat()
assert stat.S_ISREG(info.st_mode) and info.st_uid == 0 and stat.S_IMODE(info.st_mode) == 0o444 and info.st_size <= 4096
lease = json.loads(lease_path.read_bytes())
assert lease.get('schema_version') == 2 and lease.get('run') == run and lease.get('slot') == slot
assert lease.get('purpose') == 'veranda-memory-research'
state = pathlib.Path(state_value)
assert state.parent == pathlib.Path('/var/tmp') and state.resolve() == state
assert re.fullmatch(r'veranda-wayland-[a-zA-Z0-9_]+', state.name)
info = state.lstat()
assert stat.S_ISDIR(info.st_mode) and info.st_uid == uid and stat.S_IMODE(info.st_mode) == 0o700
fd = os.open(state / '.marker', os.O_RDONLY | os.O_NOFOLLOW)
with os.fdopen(fd) as marker:
    info = os.fstat(marker.fileno())
    assert stat.S_ISREG(info.st_mode) and info.st_uid == uid and stat.S_IMODE(info.st_mode) == 0o600 and info.st_nlink == 1
    assert marker.read(256) == 'subyard-veranda-wayland-v1:' + state.name + '\\n'
assert group_name == 'subyard-veranda-memory-' + run + '-' + state.name.rsplit('-', 1)[1]
if action == 'validate':
    sys.exit(0)
parent = pathlib.Path('/sys/fs/cgroup')
assert parent.resolve() == parent and (parent / 'cgroup.controllers').is_file()
group = parent / group_name
if action == 'create':
    group.mkdir(mode=0o755)
    try:
        os.chmod(group, 0o755)
        assert not (group / 'cgroup.procs').read_text().strip()
        os.chown(group / 'cgroup.procs', uid, -1)
        os.chmod(group / 'cgroup.procs', 0o600)
        info = group.lstat()
        print(json.dumps([info.st_dev, info.st_ino]))
    except BaseException:
        group.rmdir()
        raise
else:
    assert group.resolve() == group and not group.is_symlink()
    info = group.lstat()
    assert stat.S_ISDIR(info.st_mode) and info.st_uid == 0
    assert [info.st_dev, info.st_ino] == list(map(int, identity))
    assert not (group / 'cgroup.procs').read_text().strip()
    group.rmdir()
'''
    result = subprocess.run(['sudo', '-n', sys.executable, '-c', code, action, str(state), group.name,
                             str(os.getuid()), os.environ['SUBYARD_E2E_RUN_ID'], os.environ['SUBYARD_E2E_SLOT'],
                             *(str(item) for item in (identity or []))],
                            text=True, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=8, check=True)
    return json.loads(result.stdout) if action == 'create' else None


def attach_group(arguments):
    """Migrate one still-gated owned child without granting writes to ancestor cgroups."""
    require(os.getuid() == 0 and len(arguments) == 7)
    value, pid, start, parent, parent_start, device, inode = arguments
    pid, start, parent, parent_start, device, inode = map(int, (pid, start, parent, parent_start, device, inode))
    require(all(item > 0 for item in (pid, start, parent, parent_start, inode)) and device >= 0)
    group = Path(value)
    match = re.fullmatch(r'subyard-veranda-memory-([0-9a-f]{8})-([a-zA-Z0-9_]+)', group.name)
    require(match and group.parent == Path('/sys/fs/cgroup') and group.resolve() == group)
    state = Path('/var/tmp') / ('veranda-wayland-' + match[2])
    info = state.lstat()
    require(stat.S_ISDIR(info.st_mode) and info.st_uid > 0 and stat.S_IMODE(info.st_mode) == 0o700
            and state.resolve() == state)
    uid = info.st_uid
    marker_fd = os.open(state / '.marker', os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(marker_fd) as marker:
        info = os.fstat(marker.fileno())
        require(stat.S_ISREG(info.st_mode) and info.st_uid == uid and info.st_nlink == 1
                and stat.S_IMODE(info.st_mode) == 0o600)
        require(marker.read(256) == 'subyard-veranda-wayland-v1:' + state.name + '\n')
    lease_path = Path('/run/subyard-e2e-lease.json')
    info = lease_path.lstat()
    require(stat.S_ISREG(info.st_mode) and info.st_uid == 0 and stat.S_IMODE(info.st_mode) == 0o444
            and info.st_size <= 4096)
    lease = json.loads(lease_path.read_bytes())
    require(lease.get('schema_version') == 2 and lease.get('run') == match[1]
            and lease.get('purpose') == 'veranda-memory-research'
            and re.fullmatch(r'slot-[0-9]{3}', lease.get('slot', '')) and lease['slot'] != 'slot-000')
    info = group.lstat()
    require(stat.S_ISDIR(info.st_mode) and info.st_uid == 0 and (info.st_dev, info.st_ino) == (device, inode))
    info = (group / 'cgroup.procs').lstat()
    require(stat.S_ISREG(info.st_mode) and info.st_uid == uid and stat.S_IMODE(info.st_mode) == 0o600
            and not (group / 'cgroup.procs').read_text().strip())
    parent_info = (Path('/proc') / str(parent)).lstat()
    child_info = (Path('/proc') / str(pid)).lstat()
    require(parent_info.st_uid == child_info.st_uid == uid)
    parent_fields = (Path('/proc') / str(parent) / 'stat').read_text().rsplit(')', 1)[1].split()
    child_fields = (Path('/proc') / str(pid) / 'stat').read_text().rsplit(')', 1)[1].split()
    require(int(parent_fields[19]) == parent_start and int(child_fields[19]) == start
            and int(child_fields[1]) == parent)
    fd = os.pidfd_open(pid)
    try:
        signal.pidfd_send_signal(fd, 0)
        current = (Path('/proc') / str(pid) / 'stat').read_text().rsplit(')', 1)[1].split()
        require(int(current[19]) == start and int(current[1]) == parent)
        with (group / 'cgroup.procs').open('w') as destination:
            destination.write(str(pid) + '\n')
        require((group / 'cgroup.procs').read_text().split() == [str(pid)])
        signal.pidfd_send_signal(fd, 0)
    finally:
        os.close(fd)


@contextlib.contextmanager
def app_group(state, cleanup, observer=None):
    group = Path('/sys/fs/cgroup') / ('subyard-veranda-memory-' + os.environ['SUBYARD_E2E_RUN_ID']
                                    + '-' + state.name.rsplit('-', 1)[1])
    identity = group_command('create', state, group)
    def remove():
        group_command('remove', state, group, identity)
        cleanup['cgroup'] = 'passed'
    with fixture.cleanup_on_exit(remove, 'app cgroup', observer):
        yield group


def cleanup_observer(args, cleanup):
    def observe(phase, label, rc, status):
        label = {'app cgroup': 'cgroup'}.get(label, label)
        require(label in cleanup and phase in ('start', 'end'))
        args.phase = label + '-cleanup'
        frame = {'phase': phase, 'label': label, 'exit_code': rc, 'status': status}
        args.cleanup_events.append(frame)
        if status == 'failed':
            cleanup[label] = 'failed'
        print('VERANDA_MEMORY_CLEANUP ' + json.dumps(frame), flush=True)
    return observe


@contextlib.contextmanager
def case_phase(args, stage):
    require(stage in ('install', 'probe', 'verification'))
    identity = {'schema_version': 1, 'case': args.active_case,
                'repeat': args.active_repeat, 'stage': stage}
    started = time.monotonic()
    outcome = {'exit_code': None, 'status': 'failed'}
    def finish():
        print('VERANDA_MEMORY_PHASE ' + json.dumps(dict(identity, state='end',
              duration_seconds=time.monotonic() - started, **outcome)), flush=True)
    print('VERANDA_MEMORY_PHASE ' + json.dumps(dict(identity, state='start',
          duration_seconds=0, exit_code=None, status='started')), flush=True)
    with fixture.cleanup_on_exit(finish, 'case timing evidence'):
        try:
            yield
        except BaseException as error:
            if stage == 'probe':
                outcome['exit_code'] = args.child_exit if type(args.child_exit) is int else None
            elif isinstance(error, subprocess.CalledProcessError):
                outcome['exit_code'] = error.returncode
            raise
        else:
            outcome.update(exit_code=0, status='passed')


def record_case(args, case, setup):
    args.results.append(case)
    # This is completed-case evidence, never whole-run acceptance. Later owned
    # cleanup can still fail before the final RESULT is emitted.
    print('VERANDA_MEMORY_CASE ' + json.dumps({'schema_version': 1, 'case': case, 'setup': setup}), flush=True)


def packages():
    return {name: probe.command_output(['dpkg-query', '-W', '-f=${Version}', name])
            for name in ('labwc', 'wlrctl', 'wlr-randr', 'libwlroots-0.18',
                         'libwebkit2gtk-4.1-0', 'libgtk-3-0t64', 'libgl1-mesa-dri')}


def measure(state, args, cleanup):
    args.phase = 'compositor'
    config = state / 'compositor-config'
    probe.private_dir(config)
    probe.private_file(config / 'rc.xml', '<labwc_config><core><xwayland>no</xwayland></core></labwc_config>\n')
    for name in ('home', 'runtime', 'config', 'cache', 'data'):
        probe.private_dir(state / name)
    env = {'PATH': '/usr/bin:/bin', 'LANG': 'C.UTF-8', 'HOME': str(state / 'home'),
           'XDG_RUNTIME_DIR': str(state / 'runtime'), 'XDG_CONFIG_HOME': str(state / 'config'),
           'XDG_CACHE_HOME': str(state / 'cache'), 'XDG_DATA_HOME': str(state / 'data'),
           'XDG_CONFIG_DIRS': str(config), 'LABWC_UPDATE_ACTIVATION_ENV': '0',
           'WLR_BACKENDS': 'headless', 'WLR_HEADLESS_OUTPUTS': '1', 'WLR_RENDERER': 'pixman'}
    with fixture.private_log(state / 'labwc.log') as log:
        compositor = subprocess.Popen(['labwc', '-V', '-C', str(config)], env=env,
                                      stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        tree = probe.Tree(compositor.pid)
        def stop():
            tree.stop(compositor)
            cleanup['compositor'] = 'passed'
        with fixture.cleanup_on_exit(stop, 'compositor', args.cleanup_observer):
            deadline = time.monotonic() + 15
            while True:
                tree.sample()
                require(compositor.poll() is None)
                sockets = [path for path in (state / 'runtime').glob('wayland-*')
                           if stat.S_ISSOCK(path.lstat().st_mode)]
                if len(sockets) == 1:
                    env['WAYLAND_DISPLAY'] = str(sockets[0])
                    probe.wayland_display(env)
                    break
                require(time.monotonic() < deadline)
                time.sleep(0.1)
            subprocess.run(['wlr-randr', '--output', 'HEADLESS-1', '--custom-mode', '1920x1080@60Hz',
                            '--scale', '1'], env=env, stdout=subprocess.DEVNULL,
                           stderr=subprocess.DEVNULL, check=True, timeout=5)
            info = subprocess.check_output(['wayland-info', '--interface', 'wl_output'], env=env,
                                           text=True, stderr=subprocess.DEVNULL, timeout=5)
            screen = geometry(info)
            compositor_log = (state / 'labwc.log').read_text(errors='replace')
            require(re.search(r'(?i)pixman', compositor_log) is not None)
            version = packages()
            args.preparation_witnesses.append(probe.resource_witness())
            setup = {'screen': screen, 'packages': version, 'compositor': 'labwc headless',
                     'renderer': 'pixman', 'logical_cpus': os.cpu_count(),
                     'guest_resources': args.guest_resources,
                     'preparation_resources': {'witnesses': args.preparation_witnesses,
                                              'assessment': probe.assess_resource_witnesses(args.preparation_witnesses)},
                     'compositor_in_app_metrics': False, 'probe_in_app_metrics': False,
                     'workload': 'one owner, 20 configured NOT_CREATED yards, 200 project names',
                     'window_binding': 'empty compositor before launch, then singleton fixed app ID; protocol has no PID',
                     'comparison_limit': 'compositor differs from previous Weston GL control; hardware rendering unmeasured'}
            if args.allocation_controls:
                cases = [('F', args.deb, [], 'full'), ('M', args.deb, [], 'minimal'),
                         ('N', args.deb, [], 'native-only')]
                setup['comparison_limit'] += '; diagnostic Rust1.90/tauri-unstable controls; no exact allocation subtraction'
                setup['allocation_controls'] = {
                    'F': 'full UI', 'M': 'minimal HTML, actual snapshot/details/events',
                    'N': 'native window, retained initial snapshot/details and RPC subscription; no frontend frame',
                    'confounders': ['uniform tauri-unstable changes WebView embedding',
                                    'native-only retains the initial snapshot without a UI event consumer',
                                    'different object representations, layout and font use']}
            else:
                cases = [('A', args.deb, [], None),
                         ('D-image', args.deb, ['--gdk-rendering-image'], None)]
                if args.candidate_deb:
                    cases.append(('C-on-A', args.candidate_deb, [], None))
            # Setup already installed this DEB; every case still verifies its payload and executable.
            installed_deb_sha256 = hashlib.sha256(args.deb.read_bytes()).hexdigest()
            for repeat in range(1, 4):
                for name, deb, flags, allocation_mode in (cases if repeat % 2 else list(reversed(cases))):
                    checkpoint = name + '-' + str(repeat)
                    args.active_case, args.active_repeat, args.phase = name, repeat, 'case-install'
                    args.child_exit = None
                    print('VERANDA_MEMORY_CHECK ' + json.dumps({'case': name, 'repeat': repeat,
                                                             'state': 'start'}), flush=True)
                    with case_phase(args, 'install'), fixture.private_log(state / (checkpoint + '-install.log')) as install_log:
                        installed_deb_sha256, binary_hash = install_candidate(deb, installed_deb_sha256, install_log)
                    application = Path('/usr/bin/subyard-veranda')
                    if allocation_mode:
                        # The probe deliberately strips inherited app variables. This
                        # fixed adapter execs the verified ELF with the same PID/cgroup.
                        adapter_root = state / (checkpoint + '-application')
                        probe.private_dir(adapter_root)
                        application = adapter_root / 'subyard-veranda'
                        probe.private_file(application, '#!/bin/sh\nVERANDA_ALLOCATION_MODE=' + allocation_mode
                                           + ' exec /usr/bin/subyard-veranda\n')
                        application.chmod(0o700)
                    output = state / (checkpoint + '.json')
                    args.phase = 'case-probe'
                    with case_phase(args, 'probe'):
                        require(packages() == version)
                        with app_group(state, cleanup, args.cleanup_observer) as group, fixture.private_log(state / (checkpoint + '-probe.log')) as probe_log:
                            rc = fixture.run_probe([sys.executable, str(ROOT / 'dev/measure-veranda.py'),
                                                    '--wayland', '--configured-fleet', '--binary', str(application),
                                                    '--engine', str(args.engine), '--app-cgroup', str(group),
                                                    '--privileged-cgroup-attach',
                                                    '--toplevel-control-wlrctl', '--wlrctl-app-id', args.app_id,
                                                    '--window-state', 'visible', '--window-state', 'minimized',
                                                    '--starts', '1', '--idle-settle-seconds', '30', '--idle-seconds', '120',
                                                    '--output', str(output), *flags], env, probe_log, timeout_seconds=360)
                            args.child_exit = rc
                        require(rc == 0)
                    args.phase = 'case-metrics'
                    with case_phase(args, 'verification'):
                        metrics = json.loads(output.read_text())
                        require(metrics['inputs']['binary_sha256'] == hashlib.sha256(application.read_bytes()).hexdigest())
                        require(metrics['inputs']['engine_sha256'] == hashlib.sha256(args.engine.read_bytes()).hexdigest())
                        if allocation_mode == 'native-only':
                            require(metrics['startup']['launches'][0]['readiness_kind']
                                    == 'native-workload-complete; no frontend frame')
                            history = metrics['startup']['launches'][0]['process_history']
                            require(history and all('webkit-web' not in process['observed_roles'] for process in history))
                        require(set(metrics['window_states']) == {'visible', 'minimized'})
                        for mode in metrics['window_states'].values():
                            require(mode['duration_seconds'] >= 120 and mode['settle_seconds'] == 30)
                            require(not mode['statistics_partial'] and len(mode['samples']) >= 120)
                        require(packages() == version)
                        require(compositor.poll() is None)
                        record_case(args, {'case': name, 'repeat': repeat,
                                           'deb_sha256': hashlib.sha256(deb.read_bytes()).hexdigest(),
                                           'installed_binary_sha256': binary_hash,
                                           'allocation_mode': allocation_mode, 'metrics': metrics}, setup)
                    print('VERANDA_MEMORY_CHECK ' + json.dumps({'case': name, 'repeat': repeat,
                                                             'state': 'passed',
                                                             'rss_median_bytes': {state: value['statistics']['rss_bytes']['median']
                                                                                  for state, value in metrics['window_states'].items()},
                                                             'pss_median_bytes': {state: value['statistics']['pss_bytes']['median']
                                                                                  for state, value in metrics['window_states'].items()}}), flush=True)
            return setup


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('deb', type=Path)
    parser.add_argument('engine', type=Path)
    parser.add_argument('--candidate-deb', type=Path)
    parser.add_argument('--allocation-controls', action='store_true',
                        help='isolated diagnostic full/minimal/native-only candidate, with one fixed ELF')
    parser.add_argument('--app-id', choices=['com.subyard.veranda', 'subyard-veranda'], default='subyard-veranda')
    args = parser.parse_args()
    require(not (args.allocation_controls and args.candidate_deb))
    require(os.getuid() != 0 and os.environ.get('SUBYARD_E2E_VM') == '1'
            and os.environ.get('SUBYARD_E2E_TYPE') in {'subyard-pair', 'android-test'}
            and os.environ.get('SUBYARD_E2E_VM_COUNT') == '1'
            and os.environ.get('SUBYARD_E2E_PURPOSE') == 'veranda-memory-research')
    require(re.fullmatch(r'[0-9a-f]{8}', os.environ.get('SUBYARD_E2E_RUN_ID', '')))
    require(re.fullmatch(r'slot-[0-9]{3}', os.environ.get('SUBYARD_E2E_SLOT', '')))
    args.guest_resources = guest_resources()
    for path in (args.deb, args.engine, *([args.candidate_deb] if args.candidate_deb else [])):
        require(stat.S_ISREG(path.lstat().st_mode) and path.parent.resolve() == path.parent.absolute())
    args.deb, args.engine = args.deb.resolve(), args.engine.resolve()
    if args.candidate_deb:
        args.candidate_deb = args.candidate_deb.resolve()
    for deb in (args.deb, *([args.candidate_deb] if args.candidate_deb else [])):
        require(probe.command_output(['dpkg-deb', '-f', str(deb), 'Package', 'Version', 'Architecture']).splitlines()
                == ['Package: subyard-veranda', 'Version: 0.1.1', 'Architecture: amd64'])
    require(probe.command_output([str(args.engine), '--version']) == args.engine.name + ' 0.1.1')
    initial = subprocess.run(['dpkg-query', '-W', '-f=${Status}', 'subyard-veranda'],
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=5)
    require(initial.returncode != 0)
    args.results = []
    args.preparation_witnesses = []
    args.phase, args.active_case, args.active_repeat, args.child_exit = 'authentication', None, 0, None
    cleanup = {name: 'unverified' for name in ('cgroup', 'compositor', 'platform', 'fixture', 'package')}
    args.cleanup_events = []
    args.cleanup_observer = cleanup_observer(args, cleanup)
    receipt = {'schema_version': 1, 'result': 'failed', 'exception': 'none', 'setup': None,
               'guest_resources': args.guest_resources, 'preparation_witnesses': args.preparation_witnesses,
               'cases': args.results, 'cleanup': cleanup, 'unmeasured': ['explicit-hidden', 'occluded', 'hardware-rendering'],
               'scope': 'short memory comparison; no startup, security, full recovery or release acceptance'}
    started = time.monotonic()
    rc = 1
    try:
        with fixture.recorded_state(args.cleanup_observer) as state:
            group = Path('/sys/fs/cgroup') / ('subyard-veranda-memory-' + os.environ['SUBYARD_E2E_RUN_ID']
                                            + '-' + state.name.rsplit('-', 1)[1])
            group_command('validate', state, group)
            args.preparation_witnesses.append(probe.resource_witness())
            def remove_package():
                subprocess.run(['sudo', '-n', 'timeout', '--signal=TERM', '--kill-after=2s', '60s',
                                'apt-get', 'remove', '-y', 'subyard-veranda'],
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True, timeout=65)
                cleanup['package'] = 'passed'
            def export_logs():
                # Full logs are carried only in the observer's private output file.
                logs = {path.name: path.read_text(errors='replace') for path in sorted(state.glob('*-probe.log'))}
                logs.update({path.name: path.read_text(errors='replace') for path in sorted(state.glob('*-install.log'))})
                if (state / 'labwc.log').is_file():
                    logs['labwc.log'] = (state / 'labwc.log').read_text(errors='replace')
                print('VERANDA_MEMORY_LOGS ' + json.dumps(logs), flush=True)
            with fixture.cleanup_on_exit(remove_package, 'package', args.cleanup_observer), fixture.cleanup_on_exit(export_logs, 'evidence'):
                args.phase = 'setup'
                for name, command, timeout in (
                        ('apt-update', ['apt-get', 'update', '-qq'], 120),
                        ('apt-install', ['apt-get', 'install', '-y', '--no-install-recommends',
                                         'labwc', 'wlrctl', 'wlr-randr', 'wayland-utils', 'libgl1-mesa-dri', str(args.deb)], 300)):
                    with fixture.private_log(state / (name + '.log')) as log:
                        subprocess.run(['sudo', '-n', 'env', 'DEBIAN_FRONTEND=noninteractive', *command],
                                       stdout=log, stderr=subprocess.STDOUT, check=True, timeout=timeout)
                args.phase = 'platform'
                with fixture.prepared_platform(state, args.engine, args.cleanup_observer):
                    receipt['setup'] = measure(state, args, cleanup)
                    args.phase = 'platform-cleanup'
                cleanup['platform'] = 'passed'
        cleanup['fixture'] = 'passed'
        require(all(value == 'passed' for value in cleanup.values()))
        receipt['result'], rc = 'measured', 0
        args.phase = 'complete'
    except BaseException as error:
        receipt['exception'] = ('interrupted' if isinstance(error, (KeyboardInterrupt, SystemExit))
                                else 'guard' if isinstance(error, (ValueError, KeyError))
                                else 'child' if isinstance(error, (subprocess.SubprocessError,)) else 'io')
        if isinstance(error, KeyboardInterrupt):
            rc = 130
        elif isinstance(error, SystemExit):
            rc = error.code if error.code in (130, 143) else 1
        elif isinstance(error, subprocess.CalledProcessError):
            args.child_exit = error.returncode
    receipt.update(exit_code=rc, duration_seconds=time.monotonic() - started,
                   phase=args.phase, active_case=args.active_case, active_repeat=args.active_repeat,
                   child_exit=args.child_exit, cleanup_events=args.cleanup_events)
    print('VERANDA_MEMORY_RESULT ' + json.dumps(receipt), flush=True)
    return rc


if __name__ == '__main__':
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(143))
    if sys.argv[1:2] == ['--attach-cgroup']:
        try:
            attach_group(sys.argv[2:])
        except Exception:
            sys.exit(1)
    else:
        sys.exit(main())
