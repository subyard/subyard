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
                       PYTHONDONTWRITEBYTECODE='1')

    def view(arguments, phase, seconds=30, cancel=False, observe=None, capacity_refusal=False):
        # Resource sessions filter ambient variables; embed the observation window.
        wrapper.write_text('#!/usr/bin/python3\nimport importlib.util, sys\n'
                           f'spec = importlib.util.spec_from_file_location("window", {str(window)!r})\n'
                           'window = importlib.util.module_from_spec(spec)\nspec.loader.exec_module(window)\n'
                           f'raise SystemExit(window.run([{str(tools / "subyard-e2e-scrcpy/scrcpy")!r}, *sys.argv[1:]], '
                           f'seconds={seconds}))\n')
        wrapper.chmod(0o700)
        started = time.monotonic()
        print(f'E2E_PHASE phase=fixture/viewer-remote-{phase} state=start duration_seconds=0 exit_code=0', flush=True)
        log = work / f'viewer-{phase}.log'
        process = None
        try:
            with log.open('wb') as output:
                process = subprocess.Popen(
                    ['xvfb-run', '-a', '-s', '-screen 0 1280x800x24 -nolisten tcp', yard_bin,
                     '-Y', 'remote', 'emu', 'view', *arguments, '--',
                     '--max-size=640', '--no-audio', '--verbosity=debug'],
                    env=environment,
                    stdin=subprocess.DEVNULL, stdout=output, stderr=subprocess.STDOUT,
                    start_new_session=True)
                deadline = started + 1440
                cancelled = False
                completed = None
                while time.monotonic() < deadline:
                    completed = os.waitid(os.P_PID, process.pid, os.WEXITED | os.WNOHANG | os.WNOWAIT)
                    if completed is not None:
                        break
                    if observe:
                        observe()
                    if cancel and not cancelled and re.search(rb'^INFO: Texture: [0-9]{1,4}x[0-9]{1,4}$',
                                                              log.read_bytes()[:262144], re.MULTILINE):
                        # Cancel the public CLI; it owns its local SSH and viewer children.
                        # xvfb-run is a shell wrapper, so find its exact candidate child.
                        children = []
                        for entry in Path('/proc').iterdir():
                            if not entry.name.isdigit():
                                continue
                            try:
                                argv = (entry / 'cmdline').read_bytes().split(b'\0')
                                if argv[:7] == [yard_bin.encode(), b'-Y', b'remote', b'emu', b'view',
                                                b'--lease-file', lease.encode()]:
                                    children.append(int(entry.name))
                            except (FileNotFoundError, PermissionError, ProcessLookupError):
                                pass
                        require(len(children) == 1, 'could not identify the public viewer for cancellation')
                        os.kill(children[0], signal.SIGTERM)
                        cancelled = True
                        deadline = min(deadline, time.monotonic() + 30)
                    time.sleep(1)
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
                require(cancelled and code != 0 if cancel else code == 0 and summary['result'] == 'passed',
                        'public remote viewer returned an unexpected exit')
            no_wires()
            print(f'E2E_PHASE phase=fixture/viewer-remote-{phase} state=end duration_seconds={int(time.monotonic() - started)} exit_code=0', flush=True)
        finally:
            if process is not None:
                # Fence cleanup to our dedicated session before reaping its leader.
                with contextlib.suppress(ProcessLookupError):
                    os.killpg(process.pid, signal.SIGTERM)
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    with contextlib.suppress(ProcessLookupError):
                        os.killpg(process.pid, signal.SIGKILL)
                    process.wait(timeout=5)

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
             'standalone', seconds=heartbeat + 30, observe=observe)
        slots = guest('status')
        require(observed.get('renewed') and any(slot['generation'] > idle[slot['slot_id']]['generation']
                                               for slot in slots),
                'remote standalone viewer did not acquire and renew its own lease')
        require(all(slot['state'] == 'available' for slot in slots) and guest('runtimes')['units'] == 0,
                'remote standalone viewer leaked a lease or runtime')
        print('android-pool-remote standalone-view=PASS lease=renewed-released slots=available', flush=True)
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
