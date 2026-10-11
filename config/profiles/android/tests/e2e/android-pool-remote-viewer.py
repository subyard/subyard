#!/usr/bin/env python3
"""Public remote-owner viewer checks using the retained Android fixture."""
import contextlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shlex
import signal
import subprocess
import sys
import threading
import time


GUEST_CHECK = '''
import contextlib, hashlib, io, json, os, pathlib, re, runpy, shutil, stat, sys, time
sys.path.insert(0, '/usr/local/lib/subyard-android')
import client
mode, path = sys.argv[1:]
if mode == 'wires':
    count = 0
    for entry in pathlib.Path('/proc').iterdir():
        if not entry.name.isdigit():
            continue
        try:
            argv = (entry / 'cmdline').read_bytes().split(b'\\0')
            count += argv[1:3] == [b'/usr/local/lib/subyard-android/client.py', b'_wire']
        except (FileNotFoundError, PermissionError, ProcessLookupError):
            pass
    result = count
elif mode in ('status', 'heartbeat'):
    result = client.rpc('status')['slots' if mode == 'status' else 'heartbeat_seconds']
elif mode == 'runtimes':
    import subprocess
    units = subprocess.run(['systemctl', 'list-units', '--all', '--no-legend', '--plain',
                            'subyard-android-slot-*.service'], capture_output=True, text=True, timeout=10)
    if units.returncode:
        raise RuntimeError('runtime unit observation failed')
    lines = units.stdout.splitlines()
    primary = sum(bool(re.fullmatch(r'subyard-android-slot-[0-9]{3}-[1-9][0-9]*[.]service', line.split()[0]))
                  for line in lines if line.split())
    result = dict(primary=primary, units=len(lines))
elif mode == 'capacity':
    status = client.rpc('status')
    memory = status.get('memory', {})
    if type(memory) is not dict:
        memory = {}
    result = {}
    for key in ('visible_available_bytes', 'physical_available_bytes', 'cgroup_headroom_bytes',
                'available_bytes', 'total_bytes', 'reserve_bytes', 'pending_bytes', 'admission_headroom_bytes'):
        value = memory.get(key)
        result[key] = value if type(value) is int and 0 <= value <= 1 << 60 else None
    source = memory.get('limiting_source')
    result['limiting_source'] = source if source in ('visible', 'physical', 'cgroup') else 'unknown'
    broker = status.get('broker_memory', {})
    result['broker_memory'] = {}
    for key in ('ram_current_bytes', 'swap_current_bytes'):
        value = broker.get(key) if type(broker) is dict else None
        result['broker_memory'][key] = value if type(value) is int and 0 <= value <= 1 << 60 else None
    try:
        config = json.loads(pathlib.Path('/etc/subyard-android.json').read_text())
        size = config['size']
        result['disk_reserve_bytes'] = (2 * size + 1) * 1024**3 if type(size) is int and 1 <= size <= 64 else None
        for name in ('images', 'runtimes'):
            try:
                free = shutil.disk_usage(pathlib.Path(config['state_root']) / name).free
                result[name + '_free_bytes'] = free if 0 <= free <= 1 << 60 else None
            except OSError:
                result[name + '_free_bytes'] = None
    except (OSError, ValueError, KeyError, TypeError):
        pass
else:
    lease = client.read_lease(path)
    if mode in ('idle-display', 'wake-display'):
        with contextlib.redirect_stdout(io.StringIO()):
            helper = runpy.run_path('/opt/subyard-e2e-lifecycle.py')
            def adb(*arguments, timeout=10):
                return helper['adb'](lease, 'shell', *arguments, timeout=timeout)
            def cpuinfo():
                unknown = dict(observation='unknown', total_percent=None,
                               process_percent=dict.fromkeys(('framework', 'composer', 'optimizer', 'other')))
                try:
                    raw = adb('dumpsys', 'cpuinfo', timeout=5)
                except Exception:
                    return unknown
                if len(raw.encode()) > 65536:
                    return unknown
                number = r'[0-9]{1,6}(?:\\.[0-9])?'
                usage = number + r'% user \\+ ' + number + r'% kernel'
                usage += r'(?: \\+ ' + number + r'% (?:iowait|irq|softirq))*'
                usage += r'(?: / faults:(?: [0-9]{1,12} (?:minor|major)){1,2})?'
                categories = dict.fromkeys(unknown['process_percent'], 0)
                total = None
                for line in raw.splitlines():
                    # ProcessCpuTracker indents processes by two, threads by four.
                    if line.startswith(('    ', '   +', '   -')):
                        continue
                    match = re.fullmatch('(' + number + r')% TOTAL: ' + usage, line)
                    if match:
                        if total is not None:
                            return unknown
                        total = float(match[1])
                    elif line.startswith(('  ', ' +', ' -')):
                        match = re.fullmatch(r'(?:  | \\+| -)(' + number + r')% [1-9][0-9]{0,9}/([^:\\r\\n]{1,512}): ' + usage, line)
                        if not match:
                            return unknown
                        name = match[2]
                        category = ('framework' if name == 'system_server' else
                                    'composer' if name == 'surfaceflinger' else
                                    'optimizer' if name in ('dex2oat', 'dex2oat64', 'artd', 'installd') else 'other')
                        categories[category] += float(match[1])
                    elif '% TOTAL:' in line:
                        return unknown
                if total is None or total > 100:
                    return unknown
                return dict(observation='ok', total_percent=total,
                            process_percent={key: round(value, 1) for key, value in categories.items()})
            attempted = False
            try:
                if mode == 'idle-display':
                    helper['idle_display'](lease)
                    before = cpuinfo()
                    attempted = True
                    adb('cmd', 'deviceidle', 'force-idle', 'deep')
                    deep = adb('cmd', 'deviceidle', 'get', 'deep').strip()
                    forced = adb('cmd', 'deviceidle', 'get', 'force').strip()
                    if deep != 'IDLE' or forced != 'true':
                        raise RuntimeError('fixture did not confirm forced deep idle')
                    time.sleep(6)  # Allow asynchronous job restrictions to propagate.
                    result = dict(deep=deep, forced=True, cpu_before=before, cpu_after=cpuinfo())
                else:
                    adb('input', 'keyevent', 'KEYCODE_WAKEUP', timeout=30)
                    adb('cmd', 'deviceidle', 'unforce')
                    if (adb('cmd', 'deviceidle', 'get', 'force').strip() != 'false' or
                            adb('cmd', 'deviceidle', 'get', 'deep').strip() != 'ACTIVE'):
                        raise RuntimeError('fixture did not confirm restored active state')
                    result = True
            except BaseException:
                if attempted or mode == 'wake-display':
                    with contextlib.suppress(Exception):
                        adb('cmd', 'deviceidle', 'unforce')
                raise
        held = client.rpc('allocation', token=lease['token'])['state'] == 'held'
        if mode == 'idle-display':
            result['held'] = held
        else:
            result = held
    elif mode == 'renew':
        client.rpc('renew', token=lease['token'])
        result = True
    elif mode == 'request':
        result = dict(operation='tunnel', token=lease['token'])
    elif mode == 'allocation':
        value = client.rpc('allocation', token=lease['token'])
        info = os.stat(path)
        result = dict(state=value['state'], slot=value['slot_id'], generation=value['generation'],
                      expiry=value['expires_at'], uid=info.st_uid, mode=stat.S_IMODE(info.st_mode),
                      digest=hashlib.sha256(pathlib.Path(path).read_bytes()).hexdigest())
    else:
        raise AssertionError('unknown fixture check')
print(json.dumps(result))
'''


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def process_identity(pid):
    try:
        fields = Path(f'/proc/{pid}/stat').read_text().rsplit(') ', 1)[1].split()
        return dict(pid=pid, start=fields[19], parent=int(fields[1]), group=int(fields[2]), state=fields[0])
    except (FileNotFoundError, PermissionError, ProcessLookupError):
        return None


def live_identity(identity):
    current = process_identity(identity['pid'])
    return current is not None and current['start'] == identity['start'] and current['state'] != 'Z'


def process_arguments(pid):
    try:
        return Path(f'/proc/{pid}/cmdline').read_bytes().split(b'\0')
    except (FileNotFoundError, PermissionError, ProcessLookupError):
        return []


def process_executable(pid):
    try:
        return Path(f'/proc/{pid}/exe').resolve(strict=True)
    except (FileNotFoundError, PermissionError, ProcessLookupError):
        return None


def signal_owned(identity, number):
    if not live_identity(identity):
        return
    with contextlib.suppress(ProcessLookupError):
        # A pidfd plus a second identity check fences PID reuse during failure cleanup.
        fd = os.pidfd_open(identity['pid'])
        try:
            if live_identity(identity):
                signal.pidfd_send_signal(fd, number)
        finally:
            os.close(fd)


def observe_descendants(owned, capture=None, executables=()):
    # Capture identities while ancestry still proves fixture ownership, including
    # children which start their own session and may later be reparented.
    parents = {pid for pid, identity in owned.items() if live_identity(identity)}
    pending = {}
    for entry in Path('/proc').iterdir():
        if entry.name.isdigit() and (identity := process_identity(int(entry.name))) is not None:
            pending[identity['pid']] = identity
    if capture is not None:
        marker = b'SUBYARD_E2E_VIEWER_CAPTURE=' + os.fsencode(capture)
        for pid, identity in pending.items():
            if process_executable(pid) not in executables:
                continue
            try:
                with Path(f'/proc/{pid}/environ').open('rb') as environment:
                    captured = marker in environment.read(262144).split(b'\0')
            except (FileNotFoundError, PermissionError, ProcessLookupError):
                continue
            # Detached native children may lose their parent before any scan.
            # Their exact fixture executable and inherited capture marker retain
            # ownership; verify the start identity again before adopting them.
            if captured and live_identity(identity):
                owned[pid] = identity
                parents.add(pid)
    while True:
        children = {pid: identity for pid, identity in pending.items()
                    if pid not in owned and identity['parent'] in parents}
        if not children:
            return
        owned.update(children)
        parents.update(children)


def main():
    def interrupted(number, frame):
        raise SystemExit(128 + number)
    for number in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
        signal.signal(number, interrupted)
    require(len(sys.argv) == 8, 'invalid remote viewer fixture arguments')
    yard_bin, work, tools, owner_yard, project, instance, lease = sys.argv[1:]
    work, tools = Path(work), Path(tools)
    require(not lease or not Path(lease).exists(), 'attached lease path must exist only inside the selected yard')
    incus = ['/usr/bin/incus']
    if not os.access('/var/lib/incus/unix.socket', os.W_OK):
        incus = ['sudo', '-n', *incus]

    def guest(mode):
        result = subprocess.run(
            [*incus, '--project', project, 'exec', instance, '--user', '1000', '--group', '1000',
             '--env', 'HOME=/home/dev', '--', 'python3', '-c', GUEST_CHECK, mode, lease],
            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            timeout=180 if mode == 'idle-display' else 75 if mode == 'wake-display' else 35)
        require(result.returncode == 0, 'private guest lease check failed')
        return json.loads(result.stdout)

    def host_wires():
        count = 0
        for entry in Path('/proc').iterdir():
            if not entry.name.isdigit():
                continue
            try:
                argv = (entry / 'cmdline').read_bytes().split(b'\0')
                owner = b'--session-wire' in argv and owner_yard.encode() in argv
                wire = b'/usr/local/lib/subyard-android/client.py' in argv and b'_wire' in argv
                target = instance.encode() in argv and project.encode() in argv
                controller = str(work / 'ssh_config').encode() in argv and any(b'--session-wire' in arg for arg in argv)
                count += owner or wire and target or controller
            except (FileNotFoundError, PermissionError, ProcessLookupError):
                pass
        return count

    def no_wires():
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            if host_wires() == 0 and guest('wires') == 0:
                return
            time.sleep(0.5)
        raise RuntimeError('remote viewer left an owner or yard wire process')

    spec = importlib.util.spec_from_file_location('viewer_evidence',
                                                  Path(__file__).with_name('android-pool-viewer.py'))
    evidence = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(evidence)
    viewer_bin = work / 'viewer-bin'
    viewer_bin.mkdir(mode=0o700)
    wrapper = viewer_bin / 'scrcpy'
    window = Path(__file__).parent.parent / 'helpers/scrcpy-view-window.py'
    environment = dict(os.environ, PATH=f'{viewer_bin}:{tools / "platform-tools"}:' + os.environ['PATH'],
                       ADB=str(tools / 'platform-tools/adb'), SDL_RENDER_DRIVER='software',
                       SUBYARD_HOST_ID='android-e2e-controller',
                       PYTHONDONTWRITEBYTECODE='1')

    registration = subprocess.run([yard_bin, 'host', 'add', 'android-e2e-owner', '--yes'],
                                  env=environment, stdin=subprocess.DEVNULL,
                                  capture_output=True, timeout=60)
    require(registration.returncode == 0, 'canonical owner registration failed')
    hosts = subprocess.run([yard_bin, 'host', 'list'], env=environment,
                           capture_output=True, timeout=30)
    require(hosts.returncode == 0, 'canonical host listing failed')
    registered = [line.split()[0] for line in hosts.stdout.decode().splitlines()[1:]
                  if line.split()[1:] == ['android-e2e-owner']]
    require(len(registered) == 1, 'canonical owner identity was not unique')
    selector = registered[0] + '/' + owner_yard

    def view(arguments, phase, seconds=30, cancel=False, observe=None, capacity_refusal=False,
             cancel_when=None):
        capture = work / f'viewer-{phase}.capture'
        executables = {(tools / path).resolve() for path in
                       ('subyard-e2e-scrcpy/scrcpy', 'platform-tools/adb')}
        # Resource sessions filter ambient variables; embed the observation window.
        wrapper.write_text('#!/usr/bin/python3\nimport importlib.util, os, sys\n'
                           f'os.environ["SUBYARD_E2E_VIEWER_CAPTURE"] = {str(capture)!r}\n'
                           f'spec = importlib.util.spec_from_file_location("window", {str(window)!r})\n'
                           'window = importlib.util.module_from_spec(spec)\nspec.loader.exec_module(window)\n'
                           f'raise SystemExit(window.run([{str(tools / "subyard-e2e-scrcpy/scrcpy")!r}, *sys.argv[1:]], '
                           f'seconds={seconds}))\n')
        wrapper.chmod(0o700)
        started = time.monotonic()
        print(f'E2E_PHASE phase=fixture/viewer-remote-{phase} state=start duration_seconds=0 exit_code=0', flush=True)
        log = work / f'viewer-{phase}.log'
        process = None
        owned = {}
        viewer_identity = None
        scrcpy_seen = adb_seen = False
        try:
            with log.open('wb') as output:
                process = subprocess.Popen(
                    ['xvfb-run', '-a', '-s', '-screen 0 1280x800x24 -nolisten tcp', yard_bin,
                     '-Y', selector, 'emu', 'view', *arguments, '--',
                     '--max-size=640', '--no-audio', '--verbosity=debug'],
                    env=environment,
                    stdin=subprocess.DEVNULL, stdout=output, stderr=subprocess.STDOUT,
                    start_new_session=True)
                identity = process_identity(process.pid)
                require(identity is not None, 'fixture session identity is unavailable')
                owned[process.pid] = identity
                deadline = started + 1440
                cancelled = False
                completed = None
                next_observation = started
                while time.monotonic() < deadline:
                    observe_descendants(owned, capture, executables)
                    for pid, identity in owned.items():
                        if not live_identity(identity):
                            continue
                        argv = process_arguments(pid)
                        if argv[:5] == [yard_bin.encode(), b'-Y', selector.encode(), b'emu', b'view']:
                            viewer_identity = identity
                        scrcpy_seen |= str(tools / 'subyard-e2e-scrcpy/scrcpy').encode() in argv
                        adb_seen |= process_executable(pid) == (tools / 'platform-tools/adb').resolve()
                    completed = os.waitid(os.P_PID, process.pid, os.WEXITED | os.WNOHANG | os.WNOWAIT)
                    if completed is not None:
                        break
                    if observe and time.monotonic() >= next_observation:
                        observe()
                        next_observation = time.monotonic() + 1
                    if (cancel and not cancelled and (cancel_when is None or cancel_when())
                            and re.search(rb'^INFO: Texture: [0-9]{1,4}x[0-9]{1,4}$',
                                          log.read_bytes()[:262144], re.MULTILINE)):
                        # Cancel the public CLI; it owns its local SSH and viewer children.
                        require(viewer_identity is not None and live_identity(viewer_identity),
                                'could not identify the public viewer for cancellation')
                        signal_owned(viewer_identity, signal.SIGTERM)
                        cancelled = True
                        # Product session cleanup allows 60s, then bounded pipe drainage.
                        deadline = min(deadline, time.monotonic() + 70)
                    time.sleep(0.1)
                require(completed is not None, 'public remote viewer timed out')
                code = completed.si_status if completed.si_code == os.CLD_EXITED else 128 + completed.si_status
            summary = evidence.capture_summary(log.read_bytes()[:262145], code)
            print('android viewer evidence: ' + json.dumps(summary, sort_keys=True), flush=True)
            if summary.get('pool_error') == 'capacity':
                report_capacity('refused')
            if capacity_refusal:
                require(evidence.safe_memory_refusal(summary), 'second viewer did not safely refuse memory capacity')
            else:
                require(summary['rendered'] is True, 'public remote viewer did not render a frame')
                require(scrcpy_seen, 'fixture did not observe its local scrcpy identity')
                if cancel_when is not None:
                    require(cancelled, 'remote standalone viewer did not acquire and renew its own lease')
                require(cancelled and code != 0 if cancel else code == 0 and summary['result'] == 'passed',
                        'public remote viewer returned an unexpected exit')
            no_wires()
            observe_descendants(owned, capture, executables)
            # xvfb-run owns the display until its leader is reaped in finally;
            # public CLI descendants must already be gone before fixture cleanup.
            leftovers = [identity for identity in owned.values()
                         if identity['pid'] != process.pid and live_identity(identity)]
            require(not leftovers, 'remote viewer left a fixture-owned local descendant')
            print('android-pool-remote local-processes=absent scrcpy=observed adb=' +
                  ('observed' if adb_seen else 'unobserved') + ' selector=canonical', flush=True)
            print(f'E2E_PHASE phase=fixture/viewer-remote-{phase} state=end duration_seconds={int(time.monotonic() - started)} exit_code=0', flush=True)
        finally:
            if process is not None:
                observe_descendants(owned, capture, executables)
                # Fence cleanup to our dedicated session before reaping its leader.
                with contextlib.suppress(ProcessLookupError):
                    os.killpg(process.pid, signal.SIGTERM)
                try:
                    process.wait(timeout=70 if cancel else 10)
                except subprocess.TimeoutExpired:
                    with contextlib.suppress(ProcessLookupError):
                        os.killpg(process.pid, signal.SIGKILL)
                    process.wait(timeout=5)
                observe_descendants(owned, capture, executables)
                for identity in owned.values():
                    if identity['pid'] != process.pid:
                        signal_owned(identity, signal.SIGKILL)

    def report_capacity(observation):
        try:
            value = guest('capacity')
        except Exception:
            value = dict(observation='unavailable')
        print('android-pool-remote capacity-' + observation + '=' + json.dumps(value, sort_keys=True), flush=True)

    if not lease:
        idle = {slot['slot_id']: slot for slot in guest('status')}
        require(idle and all(slot['state'] == 'available' for slot in idle.values()),
                'standalone viewer requires an idle pool')
        heartbeat = guest('heartbeat')
        require(type(heartbeat) is int and 1 <= heartbeat <= 1200,
                'invalid public heartbeat interval')
        observed = {}
        def observe():
            for slot in guest('status'):
                if slot['state'] == 'held':
                    prior = observed.setdefault(slot['slot_id'], dict(slot))
                    if slot['expires_at'] > prior['expires_at']:
                        observed['renewed'] = True
        view(['--device', 'phone', '--api', '36', '--purpose', 'remote-viewer-acceptance'],
             'standalone', seconds=heartbeat + 30, observe=observe, cancel=True,
             cancel_when=lambda: observed.get('renewed', False))
        slots = guest('status')
        require(observed.get('renewed') and any(slot['generation'] > idle[slot['slot_id']]['generation']
                                               for slot in slots),
                'remote standalone viewer did not acquire and renew its own lease')
        require(all(slot['state'] == 'available' for slot in slots) and guest('runtimes')['units'] == 0,
                'remote standalone viewer leaked a lease or runtime')
        print('android-pool-remote standalone-view=PASS lease=renewed-released slots=available', flush=True)
        print('android-pool-remote standalone-cancellation=PASS slots=available runtimes=0', flush=True)
        return

    guest('renew')
    before = guest('allocation')
    require(before['state'] == 'held' and before['uid'] == 1000 and before['mode'] == 0o600,
            'attached lease ownership or permissions differ')
    no_wires()
    view(['--lease-file', lease], 'attached')
    require(guest('allocation') == before, 'remote attached viewer changed its borrowed lease')
    print('android-pool-remote attached-view=PASS lease=unchanged yard-path=PASS', flush=True)

    # The fixture owns the borrowed lease; renew it independently during another boot.
    stopped, failed = threading.Event(), threading.Event()
    def renew():
        while not stopped.wait(45):
            try:
                guest('renew')
            except Exception:
                failed.set()
                return
    guest('renew')
    worker = threading.Thread(target=renew, daemon=True)
    worker.start()
    slots = guest('status')
    idle = {slot['slot_id']: slot for slot in slots if slot['state'] == 'available'}
    runtimes = guest('runtimes')
    require(idle and runtimes['primary'] == 1, 'expected one borrowed runtime and a free slot')
    try:
        sleeping = guest('idle-display')
        require(sleeping['held'] is True and sleeping['forced'] is True and sleeping['deep'] == 'IDLE',
                'idle display did not confirm Doze and retain its borrowed lease')
        print('android-pool-remote idle-display=asleep deep=IDLE forced=true borrowed=held cpuinfo=' +
              json.dumps(dict(before=sleeping['cpu_before'], after=sleeping['cpu_after']), sort_keys=True), flush=True)
        report_capacity('before-standalone')
        view(['--device', 'phone', '--api', '36', '--purpose', 'remote-viewer-acceptance'],
             'capacity-refusal', capacity_refusal=True)
    finally:
        original_error = sys.exc_info()[0] is not None
        try:
            # Restore even on failure, before any read-only attached viewer.
            require(guest('wake-display') is True, 'display restoration did not retain its borrowed lease')
            print('android-pool-remote display-restoration=PASS deep=ACTIVE forced=false borrowed=held', flush=True)
        except Exception:
            if not original_error:
                raise
            print('android-pool-remote display-restoration=failed', file=sys.stderr, flush=True)
        finally:
            stopped.set()
            worker.join(timeout=40)
    require(not worker.is_alive() and not failed.is_set(), 'fixture lease renewal failed')
    after = guest('allocation')
    require(all(after[key] == before[key] for key in ('state', 'slot', 'generation', 'uid', 'mode', 'digest')),
            'remote standalone viewer changed borrowed lease ownership')
    slots = guest('status')
    require({slot['slot_id']: slot for slot in slots if slot['slot_id'] in idle} == idle
            and sum(slot['state'] == 'held' for slot in slots) == 1 and guest('runtimes') == runtimes,
            'refused second viewer changed a free slot or leaked a runtime')
    print('android-pool-remote capacity-refusal=PASS borrowed=held free-slots=unchanged runtimes=1', flush=True)

    guest('renew')
    before = guest('allocation')
    view(['--lease-file', lease], 'cancelled', seconds=120, cancel=True)
    require(guest('allocation') == before, 'cancelled attached viewer changed its borrowed lease')

    # Send a real authenticated tunnel, then close controller SSH stdin at EOF.
    # The credential crosses only anonymous pipes and is never saved in evidence.
    request = guest('request')
    command = ' '.join(shlex.quote(arg) for arg in ['yard', '-Y', owner_yard, 'emu', '--session-wire', 'view'])
    with (work / 'viewer-eof.err').open('wb') as error:
        process = subprocess.Popen(['ssh', '-T', '-o', 'ForwardAgent=no', '-o', 'ForwardX11=no',
                                    '-o', 'ClearAllForwardings=yes', 'android-e2e-owner', '--',
                                    'bash', '-lc', shlex.quote(command)],
                                   stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=error)
        try:
            response, _ = process.communicate(json.dumps(request).encode() + b'\n', timeout=30)
            require(len(response) <= 16384 and json.loads(response).get('ok') is True,
                    'SSH tunnel request failed')
            require(process.returncode == 0, 'SSH EOF did not close the owner wire')
        finally:
            if process.poll() is None:
                process.kill()
                process.wait(timeout=5)
    no_wires()
    require(guest('allocation') == before, 'SSH EOF changed its borrowed lease')
    print('android-pool-remote wire-eof=PASS cancellation=PASS processes=absent', flush=True)


if __name__ == '__main__':
    try:
        main()
    except Exception as exc:
        # Keep credentials and arbitrary native output out of controller evidence.
        detail = str(exc) if isinstance(exc, RuntimeError) else type(exc).__name__
        print(f'android-pool-remote viewer=FAIL ({detail})', file=sys.stderr)
        raise SystemExit(1)
