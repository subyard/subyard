#!/usr/bin/env python3
"""Read-only diagnostics for Android E2E allocations."""
import contextlib
import json
import os
from pathlib import Path
import re
import select
import signal
import subprocess
import sys
import time

def initialize():
    global PoolError, Runtime, stop, state, runtime, host_netns_inode
    sys.path.insert(0, '/usr/local/lib/subyard-android')
    from pool import PoolError, Runtime
    config = json.loads(Path('/etc/subyard-android.json').read_text())
    state = Path(config['state_root']) / 'pool.json'
    runtime = Runtime.__new__(Runtime)  # __init__ would create a directory.
    runtime.root = Path(config['state_root']) / 'runtimes'
    host_netns_inode = os.stat('/proc/1/ns/net').st_ino


def display_values(status, output):
    # Arbitrary dumpsys output never leaves this helper. An incomplete or failed
    # query cannot establish an OFF display, including on transport EOF.
    if status not in ('ok', 'timeout', 'limit', 'transport_unavailable', 'shell_unavailable'):
        status = 'unknown'
    unknown = {'query': status, 'screen_state': 'unknown'}
    if status != 'ok':
        return unknown
    try:
        text = bytes(output).decode('ascii')
    except UnicodeDecodeError:
        return {'query': 'malformed', 'screen_state': 'unknown'}
    if len(output) > 65536:
        return {'query': 'limit', 'screen_state': 'unknown'}
    exits = re.findall(r'^SUBYARD_QUERY_EXIT=([0-9]{1,3})$', text, re.M)
    if exits != ['0'] or not text.endswith('SUBYARD_QUERY_EXIT=0\n'):
        return {'query': 'failed', 'screen_state': 'unknown'}
    states = []
    for line in text.splitlines():
        if 'mScreenState=' in line:
            match = re.fullmatch(r'\s*mScreenState=(ON|OFF|DOZE|DOZE_SUSPEND)\s*', line)
            if not match:
                return {'query': 'malformed', 'screen_state': 'unknown'}
            states.append(match[1])
    if not states or len(set(states)) != 1:
        return {'query': 'unknown', 'screen_state': 'unknown'}
    return {'query': 'ok', 'screen_state': states[0]}


def display_observation(slot):
    status, output = shell_read(slot, 'dumpsys display; printf "\\nSUBYARD_QUERY_EXIT=%s\\n" "$?"', 3, 65536)
    return display_values(status, output)


def slot_states():
    slots = json.loads(state.read_text())['slots']
    active = [slot for slot in slots if slot['state'] != 'available']
    result = []
    for slot in active:
        if (not re.fullmatch(r'[0-9]{3}', slot['slot_id']) or
                type(slot['generation']) is not int or slot['generation'] < 1
                or slot['state'] not in ('provisioning', 'held', 'draining', 'quarantined')):
            raise ValueError('invalid slot identity')
        result.append(({'slot_id': slot['slot_id'], 'generation': slot['generation']}, slot['state']))
    return result


def pairs(path):
    try:
        lines = path.read_text().splitlines()
    except OSError:
        return None
    return {key: int(value) for line in lines if re.fullmatch(r'[a-z_]+ [0-9]+', line)
            for key, value in [line.split()]}


def metrics(slot):
    unit = Runtime.unit(slot)
    result = subprocess.run(['systemctl', 'show', unit,
                             '--property=ControlGroup,MainPID,ActiveState'],
                            capture_output=True, text=True, timeout=2, check=False)
    if result.returncode:
        return None
    props = dict(line.split('=', 1) for line in result.stdout.splitlines() if '=' in line)
    group_name = '/system.slice/' + unit
    pid = props.get('MainPID', '')
    if props.get('ControlGroup') != group_name or not pid.isdecimal() or int(pid) < 1:
        return None
    pid = int(pid)
    try:
        if Path(f'/proc/{pid}/cgroup').read_text().split('0::')[-1].strip() != group_name:
            return None
        netns_inode = os.stat(f'/proc/{pid}/ns/net').st_ino
    except OSError:
        return None
    group = Path('/sys/fs/cgroup/system.slice') / unit
    try:
        values = {name: (group / name).read_text().strip() for name in
                  ('memory.current', 'memory.peak', 'memory.max')}
    except OSError:
        return None
    if not all(value == 'max' or value.isdecimal() for value in values.values()):
        return None
    active_state = props.get('ActiveState', '')
    values.update(unit=unit, pid=pid, netns_inode=netns_inode,
                  host_netns_inode=host_netns_inode,
                  active_state=active_state if active_state in ('active', 'activating', 'failed', 'inactive')
                  else 'other')
    values['memory.events'] = pairs(group / 'memory.events')
    values['cpu.stat'] = pairs(group / 'cpu.stat')
    try:
        pressure = (group / 'memory.pressure').read_text().splitlines()
        values['memory.pressure'] = [line for line in pressure if re.fullmatch(
            r'(some|full) avg10=[0-9.]+ avg60=[0-9.]+ avg300=[0-9.]+ total=[0-9]+', line)]
    except OSError:
        values['memory.pressure'] = None
    return values


def shell_read(slot, command, seconds, limit, first_line=False):
    process = None
    output = bytearray()
    previous = signal.getsignal(signal.SIGALRM)
    def expired(_signum, _frame):
        raise TimeoutError
    signal.signal(signal.SIGALRM, expired)
    signal.setitimer(signal.ITIMER_REAL, seconds)
    try:
        process = runtime.connect(slot)
        deadline = time.monotonic() + seconds
        buffered = bytearray()

        def read(size):
            while len(buffered) < size:
                remaining = deadline - time.monotonic()
                if remaining <= 0 or not select.select([process.stdout], [], [], remaining)[0]:
                    return None
                data = os.read(process.stdout.fileno(), 128)
                if not data:
                    return None
                buffered.extend(data)
            result = bytes(buffered[:size])
            del buffered[:size]
            return result

        for index, command in enumerate(('host:transport:emulator-5554', 'shell:' + command)):
            data = command.encode('ascii')
            process.stdin.write(f'{len(data):04x}'.encode('ascii') + data)
            process.stdin.flush()
            if read(4) != b'OKAY':
                return ('transport_unavailable' if index == 0 else 'shell_unavailable'), output
        while len(output) < limit:
            byte = read(1)
            if byte is None:
                if time.monotonic() >= deadline:
                    return 'timeout', output
                return ('shell_unavailable' if first_line else 'ok'), output
            output.extend(byte)
            if first_line and byte == b'\n':
                return 'ok', output
        return 'limit', output
    except TimeoutError:
        return 'timeout', output
    except (OSError, PoolError, subprocess.SubprocessError, ValueError):
        return 'transport_unavailable', output
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, previous)
        if process is not None:
            with contextlib.suppress(OSError):
                process.stdin.close()
            if process.poll() is None:
                with contextlib.suppress(OSError):
                    process.kill()
            with contextlib.suppress(OSError, subprocess.SubprocessError):
                process.wait(timeout=1)
            with contextlib.suppress(OSError):
                process.stdout.close()


def property_value(slot, name):
    status, output = shell_read(slot, 'getprop ' + name, 3, 128, first_line=True)
    if status != 'ok':
        return {'timeout': 'property_timeout', 'limit': 'other'}.get(status, status)
    value = output.strip().decode('ascii', 'replace')
    if name == 'ro.build.version.sdk':
        return value if re.fullmatch(r'[0-9]{1,3}', value) else 'other'
    allowed = ('', '0', '1') if name == 'sys.boot_completed' else \
        ('', 'running', 'stopped', 'restarting')
    return value if value in allowed else 'other'



def main():
    initialize()
    if sys.argv[1:] == ['--once']:
        active = slot_states()
        print('android-display-observation active_slots=' + str(len(active)), flush=True)
        for slot, _ in active:
            print('android-display-observation ' + json.dumps(dict(slot=slot['slot_id'], generation=slot['generation'], **display_observation(slot)), sort_keys=True), flush=True)
        return
    global stop
    stop = Path(sys.argv[1])
    if not re.fullmatch(r'/run/subyard-e2e-android-monitor-[a-z0-9]+\.stop', str(stop)):
        raise SystemExit('android-boot-monitor: invalid stop marker')
    print('android-boot-monitor ready', flush=True)
    last_state, last_metrics = None, {}
    provisioning_since, diagnosed = {}, set()
    network_wait_since, network_diagnosed = {}, set()
    next_report = 0
    while True:
        stopping = stop.exists()
        try:
            active = slot_states()
            samples = []
            for slot, status in active:
                key = (slot['slot_id'], slot['generation'])
                if status == 'provisioning':
                    provisioning_since.setdefault(key, time.monotonic())
                current = metrics(slot)
                if current is not None:
                    last_metrics[key] = current
                samples.append((slot, status, current or last_metrics.get(key)))
            identity = tuple((slot['slot_id'], slot['generation'], status) for slot, status in active)
            now = time.monotonic()
            if now >= next_report or identity != last_state or stopping:
                if not samples:
                    report = {'sampled_at': time.time(), 'state': 'idle',
                              'last_cgroups': [last_metrics[key] for key in sorted(last_metrics)]}
                    print('android-boot-monitor ' + json.dumps(report, sort_keys=True), flush=True)
                for slot, status, cgroup in samples:
                    report = {'sampled_at': time.time(), 'slot': slot['slot_id'], 'generation': slot['generation'],
                              'state': status, 'cgroup': cgroup}
                    if status in ('provisioning', 'held'):
                        report['display'] = display_observation(slot)
                        for name in ('sys.boot_completed', 'init.svc.bootanim', 'ro.build.version.sdk'):
                            report[name] = property_value(slot, name)
                        if status == 'provisioning' and report.get('sys.boot_completed') == '1':
                            outcome, output = shell_read(slot,
                                'echo wifi=$(settings get global wifi_on); '
                                'if ip -4 addr show scope global | grep -q " inet "; '
                                'then echo ipv4=1; else echo ipv4=0; fi', 5, 128)
                            report['network_probe'] = outcome
                            report['network'] = [line for line in output.decode('ascii', 'replace').splitlines()
                                                 if re.fullmatch(r'(wifi|ipv4)=[0-9]*', line)]
                    print('android-boot-monitor ' + json.dumps(report, sort_keys=True), flush=True)
                    key = (slot['slot_id'], slot['generation'])
                    if (not stopping and status == 'provisioning' and key not in diagnosed
                            and time.monotonic() - provisioning_since[key] >= 450
                            and re.fullmatch(r'[0-9]{1,3}', report.get('ro.build.version.sdk', ''))
                            and report.get('sys.boot_completed') in ('', '0')):
                        diagnosed.add(key)
                        print('android-display-observation ' + json.dumps(dict(slot=slot['slot_id'], generation=slot['generation'], **display_observation(slot)), sort_keys=True), flush=True)
                    if not stopping and status == 'provisioning' and report.get('sys.boot_completed') == '1':
                        network_wait_since.setdefault(key, time.monotonic())
                        if key not in network_diagnosed and time.monotonic() - network_wait_since[key] >= 120:
                            network_diagnosed.add(key)
                            print('android-display-observation ' + json.dumps(dict(slot=slot['slot_id'], generation=slot['generation'], **display_observation(slot)), sort_keys=True), flush=True)
                next_report, last_state = now + 30, identity
        except (OSError, ValueError, KeyError, subprocess.SubprocessError) as exc:
            print('android-boot-monitor error=' + type(exc).__name__, flush=True)
        if stopping:
            break
        time.sleep(5)


if __name__ == '__main__':
    try:
        main()
    except Exception:
        print('android-display-observation query=unavailable screen_state=unknown', flush=True)
        raise SystemExit(1)
