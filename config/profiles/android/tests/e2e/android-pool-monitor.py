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


def restart_values(status, output, previous_pid=None):
    # Count only the prior framework PID or explicitly scoped core processes.
    # Guest kernel/lmkd kills and records outside this window remain unknown.
    if status not in ('ok', 'timeout', 'limit', 'transport_unavailable', 'shell_unavailable'):
        status = 'unknown'
    result = {'query': status, 'evidence': 'unknown', 'counts': None,
              'java_exceptions': None, 'native_signals': None,
              'stack_subsystems': None, 'stack_components': None, 'public_frames': None,
              'watchdog_handlers': None, 'stack_capped': None}
    guest = dict(mem_total_kb=None, mem_available_kb=None, oom_kill=None, data_available_kib=None)
    if previous_pid is None:
        result['guest'] = dict(guest)
    if status != 'ok':
        return result
    if len(output) > 65536:
        return dict(result, query='limit')
    text = bytes(output).decode('utf-8', 'replace')
    exits = re.findall(r'^SUBYARD_QUERY_EXIT=([0-9]{1,3})$', text, re.M)
    if exits != ['0'] or not text.endswith('SUBYARD_QUERY_EXIT=0\n'):
        return dict(result, query='failed')
    counts = dict(watchdog=0, java_fatal=0, java_oom=0, native_fatal=0)
    exceptions, signals = set(), set()
    stack, components, public_frames, handlers, traces, capped = [], set(), [], set(), set(), False
    public = ('android.os.BinderProxy.transact', 'android.os.BinderProxy.transactNative',
              'android.os.Binder.blockUntilThreadAvailable', 'android.os.ServiceManager.waitForService',
              'android.os.ServiceManager.waitForDeclaredService',
              'com.android.server.Watchdog$BinderThreadMonitor.monitor')
    handler_names = {'monitor thread': 'monitor', 'foreground thread': 'foreground',
                     'main thread': 'main', 'ui thread': 'ui', 'i/o thread': 'io',
                     'display thread': 'display', 'animation thread': 'animation',
                     'surface animation thread': 'surface_animation'}
    subsystems = ((('com.android.server.wm.', 'android.view.Window'), 'window'),
                  (('com.android.server.display.', 'android.hardware.display.'), 'display'),
                  (('android.view.Surface', 'android.hardware.graphics.composer'), 'composer'),
                  (('android.os.Binder', 'android.os.Parcel', 'android.os.ServiceManager',
                    'android.os.IServiceManager', 'com.android.server.Watchdog$BinderThreadMonitor'), 'binder'),
                  (('com.android.server.pm.', 'android.content.pm.'), 'package'),
                  (('com.android.server.wifi.', 'android.net.wifi.'), 'wifi'),
                  (('com.android.server.power.', 'android.os.PowerManager'), 'power'),
                  (('com.android.internal.os.', 'android.os.Looper', 'android.os.Handler', 'java.lang.'), 'runtime'),
                  (('com.android.server.', 'com.android.internal.', 'android.'), 'core'))
    scoped = {previous_pid: 'system_server'} if previous_pid is not None else {}
    allowed_exceptions = ('java.lang.RuntimeException', 'java.lang.IllegalStateException',
                          'java.lang.IllegalArgumentException', 'java.lang.NullPointerException',
                          'java.lang.OutOfMemoryError', 'android.os.DeadObjectException',
                          'java.lang.SecurityException', 'android.os.ServiceSpecificException')
    records = []
    for line in text.splitlines():
        if not line or line == 'SUBYARD_QUERY_EXIT=0' or re.fullmatch(
                r'--------- beginning of (crash|system|events)', line):
            continue
        numeric = re.fullmatch(r'SUBYARD_GUEST_(MEM_TOTAL_KB|MEM_AVAILABLE_KB|OOM_KILL|DATA_AVAILABLE_KIB)=(.*)', line)
        if numeric and previous_pid is None:
            name = numeric[1].lower()
            value = numeric[2]
            matches = re.findall(r'^SUBYARD_GUEST_' + numeric[1] + r'=(.*)$', text, re.M)
            if (len(matches) == 1 and re.fullmatch(r'[0-9]{1,20}', value) and int(value) <= 2**64 - 1
                    and (name != 'mem_total_kb' or int(value) > 0)):
                guest[name] = int(value)
            continue
        core = re.fullmatch(r'SUBYARD_CORE_PID_(system_server|zygote|zygote64|surfaceflinger)=([0-9 ]*)', line)
        if core and previous_pid is None:
            for value in core[2].split():
                if not re.fullmatch(r'[1-9][0-9]{0,9}', value) or int(value) > 2147483647:
                    return dict(result, query='malformed')
                scoped[int(value)] = 'zygote' if core[1] == 'zygote64' else core[1]
            continue
        match = re.fullmatch(r'[VDIWEFS]/(.+?)\(\s*([0-9]{1,10})\): (.*)', line)
        if not match:
            return dict(result, query='malformed')
        records.append((match[1].rstrip(), int(match[2]), match[3]))
        if len(records) > 1024:
            return dict(result, query='limit')
    if previous_pid is None:
        for tag, pid, message in records:
            if not 1 <= pid <= 2147483647:
                continue
            if ((tag == 'AndroidRuntime' and re.match(r'(?:\*\*\* )?FATAL EXCEPTION IN SYSTEM PROCESS:', message))
                    or (tag == 'Watchdog' and message.startswith('*** WATCHDOG KILLING SYSTEM PROCESS:'))):
                scoped[pid] = 'system_server'
            if tag == 'libc' and re.match(r'Fatal signal [0-9]{1,2}\b', message):
                core = re.search(r'\bpid ([1-9][0-9]{0,9}) \((system_server|zygote|zygote64|surfaceflinger)\)$', message)
                if core and int(core[1]) == pid and pid <= 2147483647:
                    scoped[pid] = 'zygote' if core[2] == 'zygote64' else core[2]
    roles = set()
    for tag, pid, message in records:
        if pid not in scoped:
            continue
        roles.add(scoped[pid])
        counts['watchdog'] += int(tag == 'watchdog' or (tag == 'Watchdog' and
                                 message.startswith('*** WATCHDOG KILLING SYSTEM PROCESS:')))
        counts['java_fatal'] += int(tag == 'AndroidRuntime' and bool(re.match(
            r'(?:\*\*\* )?FATAL EXCEPTION(?: IN SYSTEM PROCESS)?:', message)))
        counts['java_oom'] += int(bool(re.search(r'\bjava\.lang\.OutOfMemoryError\b', message)))
        if tag == 'AndroidRuntime':
            for name in allowed_exceptions:
                if re.match(r'(?:Caused by: )?' + re.escape(name) + r'(?::|$)', message.lstrip()):
                    exceptions.add(name.rsplit('.', 1)[1])
        native = re.match(r'Fatal signal ([0-9]{1,2})\b', message) if tag == 'libc' else None
        if native and 1 <= int(native[1]) <= 64:
            counts['native_fatal'] += 1
            signals.add(int(native[1]))
        trace = (tag, pid)
        fatal = tag == 'AndroidRuntime' and bool(re.match(
            r'(?:\*\*\* )?FATAL EXCEPTION(?: IN SYSTEM PROCESS)?:', message))
        watchdog = tag == 'Watchdog' and message.startswith('*** WATCHDOG KILLING SYSTEM PROCESS:')
        if fatal or watchdog:
            traces.add(trace)
        if watchdog:
            for name, category in handler_names.items():
                if re.search(r'\bBlocked in (?:handler|monitor [A-Za-z0-9_.$]{1,160}) on ' + re.escape(name) +
                             r' \([^()\r\n]{0,128}\) for [0-9]{1,6}s(?:,|$)', message):
                    handlers.add(category)
            if 'Blocked in monitor com.android.server.Watchdog$BinderThreadMonitor on ' in message:
                handlers.add('binder')
        if trace not in traces:
            continue
        frame = re.fullmatch(r'\s*at ([A-Za-z_$][A-Za-z0-9_$]{0,63}(?:\.[A-Za-z_$][A-Za-z0-9_$]{0,63}){1,12})'
                             r'\.([A-Za-z_$][A-Za-z0-9_$]{0,79}|<init>|<clinit>)'
                             r'\((?:Native Method|Unknown Source|[A-Za-z_$][A-Za-z0-9_$]{0,79}\.java:[0-9]{1,6})\)\s*', message)
        if frame and len(frame[1]) <= 192:
            category = next((value for prefixes, value in subsystems if frame[1].startswith(prefixes)), None)
            if category:
                components.add(category)
                if len(stack) < 16:
                    stack.append(category)
                else:
                    capped = True
                canonical = frame[1] + '.' + frame[2]
                if canonical in public and canonical not in public_frames:
                    public_frames.append(canonical)
        elif not (fatal or watchdog or
                  (tag == 'AndroidRuntime' and re.fullmatch(
                      r'\s*(?:(?:Caused by|Suppressed): )?[A-Za-z_$][A-Za-z0-9_.$]{0,191}(?:Exception|Error)(?::.*)?|\s*\.\.\. [0-9]{1,6} more', message)) or
                  (tag == 'Watchdog' and (re.fullmatch(r'.{1,128}(?: annotated)? stack trace:', message) or
                   re.fullmatch(r'\s*- (?:waiting to lock|locked) <0x[0-9a-fA-F]{1,16}> \(a [A-Za-z0-9_.$]{1,192}\)', message)))):
            traces.discard(trace)
    values = dict(result, counts=counts, lines=len(records), record_limit=256, line_limit=1024,
                java_exceptions=sorted(exceptions), native_signals=sorted(signals),
                stack_subsystems=stack, stack_components=sorted(components), public_frames=public_frames,
                watchdog_handlers=sorted(handlers), stack_capped=capped,
                evidence='matching_records' if any(counts.values()) else 'unknown')
    if previous_pid is None:
        values['core_roles'] = sorted(roles)
        values['guest'] = guest
    return values


def framework_restart(slot, history, status, output):
    if status != 'ok':
        return None
    pids = re.findall(rb'^framework=([1-9][0-9]{0,9})$', bytes(output), re.M)
    if len(pids) != 1 or int(pids[0]) > 2147483647:
        return None
    pid = int(pids[0])
    key = (slot['slot_id'], slot['generation'])
    previous, count = history.get(key, (pid, 0))
    if previous == pid:
        history[key] = (pid, count)
        return None
    history[key] = (pid, count + 1)
    # A changed validated PID proves a restart; blank/failed probes do not.
    outcome, raw = shell_read(slot,
        f'logcat -b crash -b system -b events -d -t 256 -v brief --pid={previous}; '
        'printf "\\nSUBYARD_QUERY_EXIT=%s\\n" "$?"', 5, 65536)
    return dict(previous_pid=previous, current_pid=pid, restart=count + 1,
                **restart_values(outcome, raw, previous))


def startup_crash(slot, elapsed, diagnosed):
    key = (slot['slot_id'], slot['generation'])
    threshold = next((value for value in (450, 1100) if elapsed >= value and
                      (key, value) not in diagnosed), None)
    if threshold is None:
        return None
    diagnosed.add((key, threshold))
    outcome, raw = shell_read(slot,
        'while read key value unit rest; do case "$key:$unit" in '
        'MemTotal::kB) echo SUBYARD_GUEST_MEM_TOTAL_KB=$value;; '
        'MemAvailable::kB) echo SUBYARD_GUEST_MEM_AVAILABLE_KB=$value;; esac; done '
        '2>/dev/null </proc/meminfo; '
        'while read key value rest; do case "$key" in oom_kill) echo SUBYARD_GUEST_OOM_KILL=$value;; '
        'esac; done 2>/dev/null </proc/vmstat; '
        'df -k /data 2>/dev/null | { read fs total used available rest; '
        'if [ "$available" = Available ] && read fs total used available rest; '
        'then echo SUBYARD_GUEST_DATA_AVAILABLE_KIB=$available; fi; }; '
        'for role in system_server zygote zygote64 surfaceflinger; do '
        'echo SUBYARD_CORE_PID_$role=$(pidof "$role"); done; '
        'logcat -b crash -b system -b events -d -t 256 -v brief; '
        'printf "\\nSUBYARD_QUERY_EXIT=%s\\n" "$?"', 5, 65536)
    return dict(threshold_seconds=threshold, **restart_values(outcome, raw))


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


def yard_observation(proc=Path('/proc')):
    # These are yard-visible /proc counters; LXCFS may virtualize them.
    result = dict(cpu_ticks=None, ticks_per_second=None, mem_total_kb=None, mem_available_kb=None)
    try:
        ticks = os.sysconf('SC_CLK_TCK')
        if type(ticks) is int and 0 < ticks <= 2147483647:
            result['ticks_per_second'] = ticks
    except (OSError, ValueError):
        pass
    try:
        with (proc / 'stat').open('rb') as source:
            line = source.readline(513)
        if (len(line) <= 512 and line.endswith(b'\n') and
                re.fullmatch(rb'cpu(?: +[0-9]{1,20}){8,10}\n', line)):
            values = [int(value) for value in line.split()[1:]]
            if all(value <= 2**64 - 1 for value in values):
                result['cpu_ticks'] = dict(zip(
                    ('user', 'nice', 'system', 'idle', 'iowait', 'irq', 'softirq', 'steal'), values[:8]))
    except OSError:
        pass
    try:
        with (proc / 'meminfo').open('rb') as source:
            raw = source.read(8193)
        if len(raw) <= 8192 and raw.endswith(b'\n'):
            for key, field in ((b'MemTotal', 'mem_total_kb'), (b'MemAvailable', 'mem_available_kb')):
                lines = re.findall(rb'^' + key + rb':[^\n]*$', raw, re.M)
                if len(lines) != 1:
                    continue
                match = re.fullmatch(key + rb': +([0-9]{1,20}) kB', lines[0])
                if match and int(match[1]) <= 2**64 - 1 and (field != 'mem_total_kb' or int(match[1]) > 0):
                    result[field] = int(match[1])
    except OSError:
        pass
    return result


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
    memory_stat = pairs(group / 'memory.stat')
    values['memory.stat'] = None if memory_stat is None else {
        key: memory_stat.get(key) for key in ('anon', 'file', 'shmem', 'kernel', 'active_file', 'inactive_file')
        if type(memory_stat.get(key)) is int and 0 <= memory_stat[key] <= 1 << 60}
    values['main_process_memory_kb'] = dict(rss=None, pss=None)
    try:
        with Path(f'/proc/{pid}/smaps_rollup').open('rb') as source:
            raw = source.read(8193)
        if len(raw) <= 8192:
            for key, field in ((b'Rss', 'rss'), (b'Pss', 'pss')):
                lines = re.findall(rb'^' + key + rb':[^\n]*$', raw, re.M)
                match = re.fullmatch(key + rb': +([0-9]{1,20}) kB', lines[0]) if len(lines) == 1 else None
                if match and int(match[1]) <= 1 << 60:
                    values['main_process_memory_kb'][field] = int(match[1])
    except OSError:
        pass
    values['cpu.stat'] = pairs(group / 'cpu.stat')
    for name in ('memory.pressure', 'cpu.pressure'):
        try:
            pressure = (group / name).read_text().splitlines()
            values[name] = [line for line in pressure if re.fullmatch(
                r'(some|full) avg10=[0-9.]+ avg60=[0-9.]+ avg300=[0-9.]+ total=[0-9]+', line)]
        except OSError:
            values[name] = None
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
    framework_history = {}
    startup_diagnosed = set()
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
                              'yard_visible': yard_observation(),
                              'last_cgroups': [last_metrics[key] for key in sorted(last_metrics)]}
                    print('android-boot-monitor ' + json.dumps(report, sort_keys=True), flush=True)
                for slot, status, cgroup in samples:
                    key = (slot['slot_id'], slot['generation'])
                    report = {'sampled_at': time.time(), 'slot': slot['slot_id'], 'generation': slot['generation'],
                              'yard_visible': yard_observation(),
                              'state': status, 'cgroup': cgroup}
                    if status in ('provisioning', 'held'):
                        report['display'] = display_observation(slot)
                        for name in ('sys.boot_completed', 'init.svc.bootanim', 'ro.build.version.sdk'):
                            report[name] = property_value(slot, name)
                        if status == 'provisioning':
                            if report.get('sys.boot_completed') == '1':
                                outcome, output = shell_read(slot,
                                    'echo framework=$(pidof system_server); '
                                    'case "$(service check wifi)" in '
                                    '"Service wifi: found") echo wifi_service=1;; '
                                    '"Service wifi: not found") echo wifi_service=0;; esac; '
                                    'echo wifi=$(settings get global wifi_on); '
                                    'case "$(cmd wifi status)" in '
                                    '"Wifi is enabled"*) echo wifi_radio=enabled;; '
                                    '"Wifi is disabled"*) echo wifi_radio=disabled;; '
                                    '*) echo wifi_radio=unknown;; esac; '
                                    'if ip -4 addr show scope global | grep -q " inet "; '
                                    'then echo ipv4=1; else echo ipv4=0; fi', 5, 256)
                                report['network_probe'] = outcome
                                report['network'] = [line for line in output.decode('ascii', 'replace').splitlines()
                                                     if re.fullmatch(r'(framework|wifi_service|wifi|ipv4)=[0-9]{0,10}|wifi_radio=(enabled|disabled|unknown)', line)]
                            else:
                                outcome, output = shell_read(slot, 'echo framework=$(pidof system_server)',
                                                             3, 128, first_line=True)
                                report['framework_probe'] = outcome
                                report['framework'] = [line for line in output.decode('ascii', 'replace').splitlines()
                                                       if re.fullmatch(r'framework=[0-9]{0,10}', line)]
                            restart = framework_restart(slot, framework_history, outcome, output)
                            if restart is not None:
                                report['framework_restart'] = restart
                            elif not stopping:
                                startup = startup_crash(slot, time.monotonic() - provisioning_since[key], startup_diagnosed)
                                if startup is not None:
                                    report['startup_crash'] = startup
                    print('android-boot-monitor ' + json.dumps(report, sort_keys=True), flush=True)
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
