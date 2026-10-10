#!/usr/bin/python3
"""Bounded prerequisites and real sleep acceptance on a marked disposable pair."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import pwd
import re
import select
import shlex
import signal
import stat
import struct
import subprocess
import sys
import tempfile
import threading
import time


def selected(value, allowed):
    words = value.split()
    choices = [word[1:-1] for word in words if word.startswith('[') and word.endswith(']')]
    if (len(choices) != 1 or len(words) != len({word.strip('[]') for word in words})
            or any(word.strip('[]') not in allowed for word in words)
            or any('[' in word or ']' in word for word in words if word != '[' + choices[0] + ']')):
        raise ValueError()
    return choices[0]


def classify(observed):
    result = {'schema_version': 1, 'result': 'blocked', 'reason': 'observation_invalid',
              'sleep_mode': 'unknown', 'supports_mem': False, 'pm_test_none': False,
              'suspend_success': None, 'suspend_fail': None, 'rtc_wakeup_enabled': False,
              'wakealarm_unused': False, 'root_access': False, 'clocks_available': False,
              's2idle_callback_capable': False,
              'sleep_acceptance': 'unmeasured'}
    fields = {'linux', 'root', 'state', 'mem_sleep', 'pm_test', 'success', 'fail',
              'wakeup', 'wakealarm', 'write_access', 'clocks', 's2idle_callback'}
    try:
        if type(observed) is not dict or set(observed) != fields:
            return result
        if any(type(observed[name]) is not bool for name in ['linux', 'root', 'write_access', 'clocks']):
            return result
        if observed['s2idle_callback'] is not None and type(observed['s2idle_callback']) is not bool:
            return result
        for name in ['state', 'mem_sleep', 'pm_test', 'success', 'fail', 'wakeup', 'wakealarm']:
            if observed[name] is not None and (type(observed[name]) is not str or len(observed[name]) > 256):
                return result
        reason = 'unsupported_platform'
        if not observed['linux']:
            raise ValueError()
        reason = 'root_required'
        if not observed['root']:
            raise ValueError()
        reason = 'power_state_unavailable'
        states = observed['state'].split()
        if not states or len(states) != len(set(states)) or not set(states) <= {'freeze', 'standby', 'mem', 'disk'}:
            raise ValueError()
        reason = 'mem_unsupported'
        result['supports_mem'] = 'mem' in states
        if not result['supports_mem']:
            raise ValueError()
        reason = 'mem_sleep_unavailable'
        result['sleep_mode'] = selected(observed['mem_sleep'], {'s2idle', 'shallow', 'deep'})
        reason = 'pm_test_unavailable'
        mode = selected(observed['pm_test'], {'none', 'core', 'processors', 'platform', 'devices', 'freezer'})
        reason = 'pm_test_active'
        result['pm_test_none'] = mode == 'none'
        if not result['pm_test_none']:
            raise ValueError()
        reason = 'suspend_counters_unavailable'
        for name in ['success', 'fail']:
            value = observed[name]
            if not value or not value.isascii() or not value.isdecimal() or int(value) > 2**64 - 1 or str(int(value)) != value:
                raise ValueError()
            result['suspend_' + name] = int(value)
        reason = 'rtc_wakeup_unavailable'
        if observed['wakeup'] not in {'enabled', 'disabled'}:
            raise ValueError()
        reason = 'rtc_wakeup_disabled'
        result['rtc_wakeup_enabled'] = observed['wakeup'] == 'enabled'
        if not result['rtc_wakeup_enabled']:
            raise ValueError()
        reason = 'wakealarm_unavailable'
        if observed['wakealarm'] is None:
            raise ValueError()
        reason = 'wakealarm_busy'
        result['wakealarm_unused'] = observed['wakealarm'] in {'', '0'}
        if not result['wakealarm_unused']:
            raise ValueError()
        reason = 'root_access_unavailable'
        result['root_access'] = observed['write_access']
        if not result['root_access']:
            raise ValueError()
        reason = 'clocks_unavailable'
        result['clocks_available'] = observed['clocks']
        if not result['clocks_available']:
            raise ValueError()
        # The fixture selects s2idle even when another mode is currently selected.
        # Exposed enabled callbacks are necessary, not proof of selection or sleep.
        reason = 's2idle_callback_observation_invalid'
        if observed['s2idle_callback'] is None:
            raise ValueError()
        reason = 's2idle_callback_unavailable'
        result['s2idle_callback_capable'] = observed['s2idle_callback']
        if not result['s2idle_callback_capable']:
            raise ValueError()
        result.update(result='ready', reason='ready')
    except (ValueError, TypeError, AttributeError):
        result['reason'] = reason
    return result


def s2idle_callback_capability():
    """Read only: every online CPU needs an enabled index>0 enter_s2idle state."""
    root = '/sys/devices/system/cpu'

    def read(path, limit=256):
        try:
            with open(path, 'rb') as source:
                wire = source.read(limit + 1)
        except FileNotFoundError:
            return None
        if len(wire) > limit or not wire.endswith(b'\n'):
            raise ValueError()
        return wire[:-1].decode('ascii')

    def online():
        value = read(root + '/online', 4096)
        if value is None or not re.fullmatch(r'(?:0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*))?(?:,(?:0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*))?)*', value):
            raise ValueError()
        cpus = []
        for part in value.split(','):
            bounds = part.split('-')
            first, last = int(bounds[0]), int(bounds[-1])
            if not 0 <= first <= last <= 65535 or len(cpus) + last - first + 1 > 256:
                raise ValueError()
            cpus.extend(range(first, last + 1))
        if cpus != sorted(set(cpus)):
            raise ValueError()
        return cpus

    try:
        before = online()
        capable = True
        for cpu in before:
            directory = root + '/cpu' + str(cpu) + '/cpuidle'
            try:
                with os.scandir(directory) as entries:
                    names = []
                    for count, entry in enumerate(entries):
                        if count >= 64:
                            raise ValueError()
                        if entry.name.startswith('state'):
                            if not re.fullmatch(r'state(?:0|[1-9][0-9]*)', entry.name) or int(entry.name[5:]) >= 32:
                                raise ValueError()
                            names.append(entry.name)
                if len(names) > 32:
                    raise ValueError()
            except FileNotFoundError:
                names = []
            enabled = False
            for name in names:
                if name == 'state0':
                    continue
                state = directory + '/' + name
                disable = read(state + '/disable')
                if disable not in {'0', '1'}:
                    raise ValueError()
                counters = [read(state + '/s2idle/' + key) for key in ['usage', 'time']]
                if counters == [None, None]:
                    continue
                if any(value is None or not re.fullmatch(r'0|[1-9][0-9]*', value)
                       or len(value) > 20 or int(value) > 2**64 - 1 for value in counters):
                    raise ValueError()
                enabled |= disable == '0'
            capable &= enabled
        if before != online():
            raise ValueError()
        return capable
    except InterruptedError:
        raise
    except (OSError, UnicodeError, ValueError):
        return None


def prerequisites():
    def read(path):
        try:
            with open(path, 'rb') as source:
                value = source.read(257)
            return value.decode('ascii').strip() if len(value) <= 256 else None
        except (OSError, UnicodeError):
            return None

    def accessible(path, flags):
        try:
            descriptor = os.open(path, flags | os.O_CLOEXEC | os.O_NOFOLLOW)
            os.close(descriptor)
            return True
        except OSError:
            return False

    observed = {'linux': sys.platform == 'linux', 'root': hasattr(os, 'geteuid') and os.geteuid() == 0,
                'state': None, 'mem_sleep': None, 'pm_test': None, 'success': None, 'fail': None,
                'wakeup': None, 'wakealarm': None, 'write_access': False, 'clocks': False,
                's2idle_callback': None}
    if not observed['linux'] or not observed['root']:
        return classify(observed)
    nodes = {'state': '/sys/power/state', 'mem_sleep': '/sys/power/mem_sleep', 'pm_test': '/sys/power/pm_test',
             'success': '/sys/power/suspend_stats/success', 'fail': '/sys/power/suspend_stats/fail',
             'wakeup': '/sys/class/rtc/rtc0/device/power/wakeup', 'wakealarm': '/sys/class/rtc/rtc0/wakealarm'}
    observed.update({name: read(path) for name, path in nodes.items()})
    # Opening checks actual access; no write syscall, alarm ioctl or suspend is performed.
    observed['write_access'] = all(accessible(nodes[name], os.O_WRONLY) for name in ['state', 'wakealarm'])
    try:
        monotonic = time.clock_gettime_ns(time.CLOCK_MONOTONIC)
        boottime = time.clock_gettime_ns(time.CLOCK_BOOTTIME)
        observed['clocks'] = 0 <= monotonic <= boottime <= 2**64 - 1
    except (OSError, AttributeError, ValueError):
        pass
    observed['s2idle_callback'] = s2idle_callback_capability()
    return classify(observed)


def self_test():
    sample = {'linux': True, 'root': True, 'state': 'freeze mem disk', 'mem_sleep': '[s2idle] deep',
              'pm_test': '[none] core processors platform devices freezer', 'success': '3', 'fail': '0',
              'wakeup': 'enabled', 'wakealarm': '', 'write_access': True, 'clocks': True,
              's2idle_callback': True}
    ready = classify(sample)
    assert ready['result'] == 'ready' and ready['sleep_mode'] == 's2idle' and ready['sleep_acceptance'] == 'unmeasured'
    for mode in ['shallow', 'deep']:
        assert classify({**sample, 'mem_sleep': '[' + mode + ']'})['sleep_mode'] == mode
    for field, value, reason in [
        ('linux', False, 'unsupported_platform'), ('root', False, 'root_required'),
        ('state', None, 'power_state_unavailable'), ('state', 'freeze', 'mem_unsupported'),
        ('mem_sleep', None, 'mem_sleep_unavailable'), ('mem_sleep', '[s2idle] [deep]', 'mem_sleep_unavailable'),
        ('pm_test', '[devices] none', 'pm_test_active'), ('success', '-1', 'suspend_counters_unavailable'),
        ('fail', str(2**64), 'suspend_counters_unavailable'), ('wakeup', 'disabled', 'rtc_wakeup_disabled'),
        ('wakealarm', None, 'wakealarm_unavailable'), ('wakealarm', '123', 'wakealarm_busy'),
        ('write_access', False, 'root_access_unavailable'),
        ('clocks', False, 'clocks_unavailable'), ('root', 1, 'observation_invalid'),
        ('s2idle_callback', False, 's2idle_callback_unavailable'),
        ('s2idle_callback', None, 's2idle_callback_observation_invalid'),
        ('s2idle_callback', 1, 'observation_invalid'),
    ]:
        result = classify({**sample, field: value})
        assert result['result'] == 'blocked' and result['reason'] == reason
        assert set(result) == set(ready) and result['sleep_acceptance'] == 'unmeasured'
    assert classify({})['reason'] == 'observation_invalid'
    assert classify({**sample, 'private-value': 'private-value'})['reason'] == 'observation_invalid'
    s2idle_callback_self_test()
    acceptance_self_test()
    print('ok: bounded sleep prerequisite classification; kernel capabilities unmeasured')


def s2idle_callback_self_test():
    from contextlib import redirect_stdout
    from io import StringIO
    from unittest.mock import patch
    with tempfile.TemporaryDirectory(prefix='veranda-s2idle-capability-', dir='/tmp') as directory:
        root = Path(directory)
        root.chmod(0o700)
        nodes = {'/sys/power/state': 'freeze mem disk', '/sys/power/mem_sleep': '[s2idle] deep',
                 '/sys/power/pm_test': '[none] devices', '/sys/power/suspend_stats/success': '3',
                 '/sys/power/suspend_stats/fail': '0', '/sys/class/rtc/rtc0/device/power/wakeup': 'enabled',
                 '/sys/class/rtc/rtc0/wakealarm': '', '/sys/devices/system/cpu/online': '0-1'}
        def fixture(path, value):
            target = root / path.lstrip('/')
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(value + '\n')
            target.chmod(0o600)
            return target
        for path, value in nodes.items():
            fixture(path, value)
        original_open, original_descriptor, original_scan = open, os.open, os.scandir
        scenario, online_reads = ['missing'], [0]
        def mapped(path):
            return root / str(path).lstrip('/') if str(path).startswith('/sys/') else path
        def read(path, *args, **kwargs):
            if str(path).endswith('/state1/s2idle/usage') and scenario[0] == 'unreadable':
                raise PermissionError()
            if str(path) == '/sys/devices/system/cpu/online':
                online_reads[0] += 1
                if scenario[0] == 'online-change' and online_reads[0] == 2:
                    return original_open(fixture('/sys/devices/system/cpu/online-after', '0'), *args, **kwargs)
            return original_open(mapped(path), *args, **kwargs)
        def no_write(*_args):
            raise AssertionError('blocked readiness reached a PM or alarm write')
        with patch('builtins.open', read), patch.object(os, 'open', lambda path, *args: original_descriptor(mapped(path), *args)), \
                patch.object(os, 'scandir', lambda path: original_scan(mapped(path))), patch.object(os, 'geteuid', lambda: 0), \
                patch.object(time, 'clock_gettime_ns', lambda _clock: 100), patch.dict(globals(), {'node_write': no_write}), \
                redirect_stdout(StringIO()):
            blocked = prerequisites()
            assert blocked['reason'] == 's2idle_callback_unavailable' and not blocked['s2idle_callback_capable']
            cleanup = ['unmeasured']
            try:
                suspend_cycle(root, 1, 2, 1, cleanup)
            except ValueError:
                pass
            else:
                raise AssertionError()
            assert cleanup == ['unmeasured']
            # Index zero cannot satisfy the kernel's index>0 s2idle search.
            for cpu in [0, 1]:
                state = '/sys/devices/system/cpu/cpu' + str(cpu) + '/cpuidle/state0'
                for name, value in [('disable', '0'), ('s2idle/usage', '0'), ('s2idle/time', '0')]:
                    fixture(state + '/' + name, value)
            assert prerequisites()['reason'] == 's2idle_callback_unavailable'
            for cpu in [0, 1]:
                state = '/sys/devices/system/cpu/cpu' + str(cpu) + '/cpuidle/state1'
                for name, value in [('disable', '0'), ('s2idle/usage', '0'), ('s2idle/time', str(2**64 - 1))]:
                    fixture(state + '/' + name, value)
            scenario[0] = 'capable'
            assert prerequisites()['result'] == 'ready'
            path = '/sys/devices/system/cpu/cpu1/cpuidle/state1'
            for name, value, reason in [('disable', '1', 's2idle_callback_unavailable'),
                                        ('disable', '2', 's2idle_callback_observation_invalid'),
                                        ('s2idle/usage', '01', 's2idle_callback_observation_invalid'),
                                        ('s2idle/time', str(2**64), 's2idle_callback_observation_invalid')]:
                fixture(path + '/' + name, value)
                assert prerequisites()['reason'] == reason
                fixture(path + '/' + name, str(2**64 - 1) if name.endswith('time') else '0')
            for scenario[0] in ['unreadable', 'online-change']:
                online_reads[0] = 0
                assert prerequisites()['reason'] == 's2idle_callback_observation_invalid'
            scenario[0] = 'malformed-online'
            fixture('/sys/devices/system/cpu/online', '0-256')
            assert prerequisites()['reason'] == 's2idle_callback_observation_invalid'
        record = json.loads(private_read(root / 'progress-1.json', os.geteuid()))
        assert record['prerequisite_reason'] == 's2idle_callback_unavailable'
        assert not record['rtc_programmed'] and not record['power_write_prepared']


def private_read(path, uid=0, limit=4096):
    fd = os.open(path, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW)
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != uid or info.st_nlink != 1
                or stat.S_IMODE(info.st_mode) != 0o600 or info.st_size > limit):
            raise ValueError()
        data = os.read(fd, limit + 1)
        if len(data) > limit:
            raise ValueError()
        return data
    finally:
        os.close(fd)


def put(root, name, value):
    data = value if isinstance(value, bytes) else json.dumps(value, sort_keys=True).encode()
    fd = os.open(root / (name + '.tmp'), os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    try:
        os.fchmod(fd, 0o600)
        if os.write(fd, data) != len(data):
            raise ValueError()
        os.fsync(fd)
    finally:
        os.close(fd)
    os.replace(root / (name + '.tmp'), root / name)


def sleep_progress(root, cycle, phase, measurements):
    """Durable diagnostic boundaries, never an acceptance proof or raw guest dump."""
    phases = {'prerequisites', 'rtc-programmed', 'power-write-prepared', 'power-write-returned',
              'restoration-started', 'restoration-returned'}
    choices = {'prerequisite_result': {'ready', 'blocked'},
               'prerequisite_reason': {'ready', 'observation_invalid', 'unsupported_platform', 'root_required',
                                       'power_state_unavailable', 'mem_unsupported', 'mem_sleep_unavailable',
                                       'pm_test_unavailable', 'pm_test_active', 'suspend_counters_unavailable',
                                       'rtc_wakeup_unavailable', 'rtc_wakeup_disabled', 'wakealarm_unavailable',
                                       'wakealarm_busy', 'root_access_unavailable', 'clocks_unavailable',
                                       's2idle_callback_unavailable', 's2idle_callback_observation_invalid'},
               'sleep_mode': {'unknown', 's2idle', 'shallow', 'deep'},
               'console_visibility': {'unmeasured', 'unavailable', 'raised', 'already-visible'},
               'console_suspend': {'unmeasured', 'unavailable', 'enabled', 'already-disabled'},
               'kernel_cleanup': {'unmeasured', 'unverified', 'verified', 'failed'}}
    booleans = {'rtc_programmed', 'power_write_prepared', 'power_write_returned'}
    numbers = {'suspend_success_before', 'suspend_fail_before', 'suspend_success_after', 'suspend_fail_after',
               'monotonic_before_ns', 'boottime_before_ns', 'monotonic_after_ns', 'boottime_after_ns'}
    if (type(cycle) is not int or cycle not in {1, 2} or phase not in phases
            or type(measurements) is not dict or not set(measurements) <= choices.keys() | booleans | numbers
            or any(type(value) is not str or value not in choices[name]
                   for name, value in measurements.items() if name in choices)
            or any(type(value) is not bool for name, value in measurements.items() if name in booleans)
            or any(value is not None and (type(value) is not int or not 0 <= value <= 2**64 - 1)
                   for name, value in measurements.items() if name in numbers)):
        raise ValueError()
    record = {'schema_version': 1, 'cycle': cycle, 'phase': phase, **measurements}
    if len(json.dumps(record).encode()) > 4096:
        raise ValueError()
    # The ready path has one durable record before the guarded power write.
    # Earlier stage lines retain visibility without spending its one-second window on fsyncs.
    if phase not in {'prerequisites', 'rtc-programmed'} or measurements.get('prerequisite_result') != 'ready':
        directory = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW)
        try:
            put(root, 'progress-' + str(cycle) + '.json', record)
            os.fsync(directory)
        finally:
            os.close(directory)
    print('sleep-stage: cycle=' + str(cycle) + ' phase=' + phase, flush=True)


def valid_context_ids(run, slot, vm, purpose, token, environment_type):
    return (bool(re.fullmatch(r'[0-9a-f]{8,64}', run))
            and bool(re.fullmatch(r'slot-[0-9]{3}', slot)) and 1 <= int(slot[5:]) <= 999
            and vm in {'1', '2'} and purpose == 'veranda-sleep'
            and bool(re.fullmatch(r'[0-9a-f]{32}', token)) and environment_type == 'subyard-pair')


def context(create=False, evidence=False):
    """Every mutating mode is fenced by the broker marker and its own exclusive root."""
    if sys.platform != 'linux' or os.geteuid() != 0:
        raise ValueError()
    run, slot, vm, purpose = [os.environ.get('SUBYARD_E2E_' + name, '')
                              for name in ['RUN_ID', 'SLOT', 'VM', 'PURPOSE']]
    token = os.environ.get('SUBYARD_E2E_SLEEP_TOKEN', '')
    if not valid_context_ids(run, slot, vm, purpose, token, os.environ.get('SUBYARD_E2E_TYPE')):
        raise ValueError()
    lease_path = Path('/run/subyard-e2e-lease.json')
    info = lease_path.lstat()
    if (not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_nlink != 1
            or stat.S_IMODE(info.st_mode) != 0o444 or info.st_size > 4096):
        raise ValueError()
    lease = json.loads(lease_path.read_bytes())
    expected = {'schema_version': 2, 'run': run, 'slot': slot, 'purpose': purpose}
    if any(lease.get(key) != value for key, value in expected.items()):
        raise ValueError()
    source = Path(__file__).resolve().parents[2]
    if (source.name != 'src' or source.parent.parent != Path('/tmp')
            or not source.parent.name.startswith('subyard-worktree.')):
        raise ValueError()
    root = Path('/var/tmp/subyard-veranda-sleep-' + run + '-' + vm)
    marker = ('subyard-veranda-sleep-v1:' + run + ':' + slot + ':' + vm + ':' + token + '\n').encode()
    if create:
        root.mkdir(mode=0o700)
        root.chmod(0o700)
        put(root, '.marker', marker)
    if evidence and not root.exists() and not root.is_symlink():
        return root, source, run, vm
    info = root.lstat()
    if (not stat.S_ISDIR(info.st_mode) or info.st_uid != 0
            or stat.S_IMODE(info.st_mode) != 0o700 or root.resolve() != root
            or private_read(root / '.marker') != marker):
        raise ValueError()
    return root, source, run, vm


def evidence_log(path, uid=0):
    """Copy a fixed private artifact; oversized or changing logs stay incomplete."""
    limit = 8 * 1024 * 1024
    fd = os.open(path, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW)
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != uid or info.st_nlink != 1
                or stat.S_IMODE(info.st_mode) != 0o600):
            raise ValueError()
        with os.fdopen(os.dup(fd), 'rb') as stream:
            data = stream.read(limit)
        after = os.fstat(fd)
        stable = info.st_size == after.st_size and info.st_mtime_ns == after.st_mtime_ns
        return {'name': path.name, 'original_bytes': info.st_size, 'retained_bytes': len(data),
                'truncated': len(data) < info.st_size, 'stable': stable,
                'sha256': hashlib.sha256(data).hexdigest(), 'data_base64': base64.b64encode(data).decode('ascii')}
    finally:
        os.close(fd)


def export_evidence():
    root, source, _run, vm = context(evidence=True)
    paths = [source.parent / 'owner-setup.log'] if vm == '2' else [root / 'receipt.json']
    if vm == '1':
        if (root / 'native.log').exists() or (root / 'native.log').is_symlink():
            paths.append(root / 'native.log')
        elif (root / 'native-process').exists():
            raise ValueError()
    artifacts = [evidence_log(path) for path in paths]
    complete = all(not artifact['truncated'] and artifact['stable'] for artifact in artifacts)
    print(json.dumps({'schema_version': 1, 'vm': int(vm),
                      'result': 'complete' if complete else 'incomplete', 'artifacts': artifacts}, sort_keys=True))
    return 0 if complete else 1


def process_identity(pid):
    return Path('/proc', str(pid), 'stat').read_text().rsplit(')', 1)[1].split()[19]


def native_ssh_children(pid):
    records = []
    entries = list(Path('/proc').glob('[0-9]*'))
    if len(entries) > 4096:
        raise ValueError()
    for entry in entries:
        try:
            fields = (entry / 'stat').read_text().rsplit(')', 1)[1].split()
            if int(fields[1]) != pid:
                continue
            if (entry / 'exe').resolve() != Path('/usr/bin/ssh') or entry.stat().st_uid != 0:
                raise ValueError()
            records.append({'pid': int(entry.name), 'start': fields[19]})
        except FileNotFoundError:
            continue
    if len(records) > 8:
        raise ValueError()
    return records


def stop_owned(record):
    """Pin the recorded generation before signaling; never kill by name or process group."""
    if type(record) is not dict or set(record) != {'pid', 'start'}:
        raise ValueError()
    pid, start = record['pid'], record['start']
    if type(pid) is not int or not 1 < pid < 2**31 or not isinstance(start, str) or not start.isdecimal():
        raise ValueError()
    try:
        fd = os.pidfd_open(pid)
    except ProcessLookupError:
        return
    try:
        if process_identity(pid) != start:
            raise ValueError()
        signal.pidfd_send_signal(fd, signal.SIGTERM)
        wait = select.poll()
        wait.register(fd, select.POLLIN)
        if not wait.poll(5000):
            raise ValueError()
    except FileNotFoundError:
        pass
    finally:
        os.close(fd)


def owner_environment(run, source):
    state = '/var/tmp/subyard-preview-' + run
    return {**os.environ, 'HOME': '/home/dev', 'USER': 'dev', 'LOGNAME': 'dev',
            'SUBYARD_OPERATOR_HOME': '/home/dev', 'SUBYARD_REPOSITORY_ROOT': str(source),
            'SUBYARD_CONFIG_HOME': state + '/config', 'SUBYARD_HOME': state + '/data',
            'SUBYARD_NO_AUDIT': '1', 'SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE': '1', 'MIN_DISK_GIB': '1',
            'STORAGE_PATH': '/home/dev/.cache/subyard-e2e-platform/incus/incus/storage'}


def owner_command(source, run, *arguments):
    # Preview setup owns the yard as dev; changing HOME alone does not change UID.
    return ['/usr/sbin/runuser', '-u', 'dev', '--', str(source / '.build/yard'),
            '-Y', 'pv-' + run, *arguments]


def owner_start():
    root, source, run, vm = context(create=True)
    if vm != '2':
        raise ValueError()
    # The existing preview setup owns this actual product yard and its teardown.
    marker = Path('/var/tmp/subyard-preview-' + run, '.marker')
    if marker.read_text() != 'subyard-preview-acceptance-v1:' + run + ':2\n':
        raise ValueError()
    environment = {key: os.environ[key] for key in ['SUBYARD_E2E_RUN_ID', 'SUBYARD_E2E_SLOT',
                   'SUBYARD_E2E_VM', 'SUBYARD_E2E_TYPE', 'SUBYARD_E2E_PURPOSE', 'SUBYARD_E2E_SLEEP_TOKEN']}
    wrapper = ('#!/usr/bin/python3\nimport os,sys\n'
               'if sys.argv[1:] != ["rpc", "--stdio"]: sys.exit(65)\n'
               'os.environ.update(' + repr(environment) + ')\n'
               'os.execv("/usr/bin/python3", ["/usr/bin/python3", ' + repr(str(Path(__file__).resolve()))
               + ', "--rpc-proxy"])\n').encode()
    target = Path('/usr/local/bin/yard')
    fd = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o755)
    try:
        os.fchmod(fd, 0o755)
        os.write(fd, wrapper)
    finally:
        os.close(fd)
    info = target.lstat()
    put(root, 'wrapper.json', {'inode': info.st_ino, 'device': info.st_dev,
                             'sha256': hashlib.sha256(wrapper).hexdigest()})
    print('sleep-stage: owner-ready')


def frame(stream):
    header = stream.read(4)
    if not header:
        return None
    if len(header) != 4:
        raise ValueError()
    size = struct.unpack('>I', header)[0]
    if not 0 < size <= 1024 * 1024:
        raise ValueError()
    data = stream.read(size)
    if len(data) != size:
        raise ValueError()
    value = json.loads(data)
    if type(value) is not dict:
        raise ValueError()
    return value, header + data


def retain_owner_fault(source, fault):
    # Export fixed diagnostics through the existing private setup log, never payload text.
    code = 'owner_inventory_failed' if fault.get('code') == 'owner_inventory_failed' else 'other'
    message = fault.get('message', '')
    reason = next((name for suffix, name in [
        ('configuration root is not operator-owned', 'config_owner'),
        ('state directory is not owned by the operator', 'state_owner')]
        if type(message) is str and message.endswith(suffix)), 'redacted')
    fd = os.open(source.parent / 'owner-setup.log', os.O_WRONLY | os.O_APPEND | os.O_NOFOLLOW)
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_nlink != 1
                or stat.S_IMODE(info.st_mode) != 0o600 or info.st_size > 8 * 1024 * 1024 - 128):
            raise ValueError()
        data = ('sleep-owner-rpc: code=' + code + ' reason=' + reason + '\n').encode('ascii')
        if os.write(fd, data) != len(data):
            raise ValueError()
    finally:
        os.close(fd)


def rpc_proxy():
    root, source, run, vm = context()
    if vm != '2' or sys.argv[1:] != ['--rpc-proxy']:
        raise ValueError()
    def terminated(_signal, _frame):
        raise SystemExit(143)
    signal.signal(signal.SIGTERM, terminated)
    child = subprocess.Popen(owner_command(source, run, 'rpc', '--stdio'),
                             env=owner_environment(run, source), cwd=source,
                             stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                             stderr=subprocess.DEVNULL)
    token = str(os.getpid()) + '-' + process_identity(os.getpid())
    put(root, 'proxy-' + token + '.json', {'pid': os.getpid(), 'start': process_identity(os.getpid())})
    held_id, held, lock = [None], [None], threading.Lock()
    def incoming():
        while True:
            item = frame(sys.stdin.buffer)
            if item is None:
                child.stdin.close()
                return
            value, data = item
            if value.get('operationId') == 'sleep-query' and value.get('method') == 'settings.list':
                with lock:
                    if held_id[0] is not None:
                        raise ValueError()
                    held_id[0] = value['id']
            child.stdin.write(data)
            child.stdin.flush()
    def release():
        while child.poll() is None:
            if (root / 'release').exists():
                with lock:
                    if held[0] is not None:
                        sys.stdout.buffer.write(held[0])
                        sys.stdout.buffer.flush()
                        held[0] = None
                        put(root, 'released', {'responses': 1})
                        return
            time.sleep(.02)
    threading.Thread(target=incoming, daemon=True).start()
    threading.Thread(target=release, daemon=True).start()
    try:
        while True:
            item = frame(child.stdout)
            if item is None:
                break
            value, data = item
            if type(value.get('error')) is dict:
                retain_owner_fault(source, value['error'])
            with lock:
                if value.get('id') == held_id[0] and value.get('type') in {'response', 'result', 'error'}:
                    if held[0] is not None:
                        raise ValueError()
                    held[0] = data
                    put(root, 'held', {'responses': 1, 'held_at_ns': time.monotonic_ns()})
                    continue
                sys.stdout.buffer.write(data)
                sys.stdout.buffer.flush()
    finally:
        if child.poll() is None:
            child.terminate()
        try:
            child.wait(timeout=5)
        except subprocess.TimeoutExpired:
            child.kill()
            child.wait(timeout=5)
        (root / ('proxy-' + token + '.json')).unlink()


def owner_action(action):
    root, source, run, vm = context()
    if vm != '2':
        raise ValueError()
    if action in {'arm-1', 'arm-2'}:
        cycle = int(action[-1])
        if (root / ('owner-' + str(cycle) + '.json')).exists():
            raise ValueError()
        child = subprocess.Popen([sys.executable, __file__, '--owner-cycle', str(cycle)],
                                 stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        put(root, 'cycle-process-' + str(cycle), {'pid': child.pid, 'start': process_identity(child.pid)})
        until = time.monotonic() + 2
        while not (root / ('owner-' + str(cycle) + '.json')).exists():
            if child.poll() is not None or time.monotonic() > until:
                raise ValueError()
            time.sleep(.01)
    elif action == 'held':
        held = False
        if (root / 'held').exists():
            value = json.loads(private_read(root / 'held'))
            held = (set(value) == {'responses', 'held_at_ns'} and type(value['responses']) is int
                    and value['responses'] == 1 and type(value['held_at_ns']) is int
                    and 0 <= value['held_at_ns'] <= 2**64 - 1)
        print(json.dumps({'held': held}))
    elif action == 'release':
        put(root, 'release', b'1\n')
        until = time.monotonic() + 3
        while not (root / 'released').exists():
            if time.monotonic() > until:
                raise ValueError()
            time.sleep(.01)
        value = json.loads(private_read(root / 'released'))
        if type(value) is not dict or set(value) != {'responses'} or type(value['responses']) is not int or value['responses'] != 1:
            raise ValueError()
    elif action == 'event':
        subprocess.run(owner_command(source, run, 'start', '--yes'),
                       env=owner_environment(run, source), cwd=source,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                       check=True, timeout=45)
    elif action in {'observe-1', 'observe-2'}:
        path = root / ('owner-' + action[-1] + '.json')
        initial = json.loads(private_read(path))['samples']
        until = time.monotonic() + 2
        while True:
            value = json.loads(private_read(path))
            if value['samples'] > initial:
                print(json.dumps(value, sort_keys=True))
                break
            if time.monotonic() > until:
                raise ValueError()
            time.sleep(.02)
    else:
        raise ValueError()


def owner_cycle(cycle):
    root, source, run, vm = context()
    if vm != '2' or cycle not in {1, 2}:
        raise ValueError()
    started = time.monotonic_ns()
    samples, maximum_gap, previous, changed, closed = 0, 0, started, False, 0
    changed_at, closed_at = None, None
    held_at = json.loads(private_read(root / 'held'))['held_at_ns'] if cycle == 1 else None
    if held_at is not None and (type(held_at) is not int or not 0 <= held_at <= started):
        raise ValueError()
    def change():
        nonlocal changed, closed, changed_at, closed_at
        subprocess.run(owner_command(source, run, 'stop', '--force', '--yes'),
                       env=owner_environment(run, source), cwd=source,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                       check=True, timeout=8)
        changed = True
        changed_at = (time.monotonic_ns() - started) // 1000000
        for record in root.glob('proxy-*.json'):
            stop_owned(json.loads(private_read(record)))
            closed += 1
        closed_at = (time.monotonic_ns() - started) // 1000000
    for index in range(31):
        now = time.monotonic_ns()
        maximum_gap = max(maximum_gap, now - previous)
        previous = now
        samples += 1
        if cycle == 2 and index == 3:
            threading.Thread(target=change, daemon=True).start()
        put(root, 'owner-' + str(cycle) + '.json', {'schema_version': 1, 'cycle': cycle,
            'elapsed_ms': (now - started) // 1000000, 'samples': samples,
            'maximum_gap_ms': maximum_gap // 1000000, 'state_changed': changed,
            'transports_closed': closed, 'changed_at_ms': changed_at, 'closed_at_ms': closed_at,
            'query_deadline_expired': cycle == 1 and now - held_at >= 5000000000})
        time.sleep(1)


def node_read(path):
    with open(path, 'rb') as stream:
        data = stream.read(257)
    if len(data) > 256:
        raise ValueError()
    return data.decode('ascii').strip()


def node_write(path, value):
    fd = os.open(path, os.O_WRONLY | os.O_CLOEXEC | os.O_NOFOLLOW)
    try:
        if os.write(fd, value.encode()) != len(value):
            raise ValueError()
    finally:
        os.close(fd)


def pending_query(value, now_ms):
    if (type(value) is not dict or set(value) != {'started_boottime_ms', 'timeout_ms'}
            or type(value['started_boottime_ms']) is not int
            or not 0 <= value['started_boottime_ms'] <= 2**64 - 1
            or type(value['timeout_ms']) is not int or value['timeout_ms'] != 5000
            or not 0 <= now_ms - value['started_boottime_ms'] <= 2000):
        raise ValueError()
    return True


def printk_values(value):
    words = value.split()
    if len(words) != 4 or any(not word.isascii() or not word.isdecimal() or not 0 <= int(word) <= 15 for word in words):
        raise ValueError()
    return tuple(map(int, words))


def restore_sleep(mode, programmed, mode_changed, console_original=None, console_programmed=None):
    restored = True
    alarm, sleep = '/sys/class/rtc/rtc0/wakealarm', '/sys/power/mem_sleep'
    try:
        # An expired owned alarm is empty. A replaced alarm is ambiguous and is retained.
        if node_read(alarm) not in {'', '0', programmed}:
            raise ValueError()
        if programmed is not None:
            node_write(alarm, '0')
            if node_read(alarm) not in {'', '0'}:
                raise ValueError()
    except (OSError, ValueError):
        restored = False
    try:
        if mode_changed:
            if selected(node_read(sleep), {'s2idle', 'shallow', 'deep'}) != 's2idle':
                raise ValueError()
            node_write(sleep, mode)
            if selected(node_read(sleep), {'s2idle', 'shallow', 'deep'}) != mode:
                raise ValueError()
    except (OSError, ValueError):
        restored = False
    try:
        if console_programmed is not None:
            current = printk_values(node_read('/proc/sys/kernel/printk'))
            original = printk_values(console_original)
            if current not in {console_programmed, original}:
                raise ValueError()
            if current != original:
                node_write('/proc/sys/kernel/printk', console_original)
                if printk_values(node_read('/proc/sys/kernel/printk')) != original:
                    raise ValueError()
    except (OSError, ValueError):
        restored = False
    return restored


def suspend_cycle(root, cycle, pid, arm_deadline, cleanup_status, query_clock=None):
    ready = prerequisites()
    progress = {'prerequisite_result': ready['result'], 'sleep_mode': ready['sleep_mode'],
                'prerequisite_reason': ready['reason'],
                'suspend_success_before': ready['suspend_success'], 'suspend_fail_before': ready['suspend_fail'],
                'rtc_programmed': False, 'power_write_prepared': False, 'power_write_returned': False,
                'kernel_cleanup': 'unmeasured', 'console_visibility': 'unmeasured', 'console_suspend': 'unmeasured'}
    sleep_progress(root, cycle, 'prerequisites', progress)
    if ready['result'] != 'ready':
        raise ValueError()
    sleep = '/sys/power/mem_sleep'
    alarm = '/sys/class/rtc/rtc0/wakealarm'
    if 's2idle' not in {word.strip('[]') for word in node_read(sleep).split()}:
        raise ValueError()
    boot = node_read('/proc/sys/kernel/random/boot_id')
    identity = process_identity(pid)
    mono = time.clock_gettime_ns(time.CLOCK_MONOTONIC)
    before = time.clock_gettime_ns(time.CLOCK_BOOTTIME)
    progress.update(monotonic_before_ns=mono, boottime_before_ns=before, kernel_cleanup='unverified')
    programmed, restore_failed, mode_changed = None, False, False
    console_original, console_programmed = None, None
    cleanup_status[0] = 'unverified'
    proof = None
    try:
        # Preserve useful PM info on the serial console without dumping a journal.
        # Missing optional visibility is explicit; ambiguous writes still fail closed.
        try:
            console_original = node_read('/proc/sys/kernel/printk')
            levels = printk_values(console_original)
        except (OSError, ValueError):
            progress['console_visibility'] = 'unavailable'
        else:
            if levels[0] < 7:
                console_programmed = (7, *levels[1:])
                node_write('/proc/sys/kernel/printk', ' '.join(map(str, console_programmed)))
                if printk_values(node_read('/proc/sys/kernel/printk')) != console_programmed:
                    raise ValueError()
                progress['console_visibility'] = 'raised'
            else:
                progress['console_visibility'] = 'already-visible'
        # Observe the console policy without changing the kernel's PM console path.
        try:
            console_suspend = node_read('/sys/module/printk/parameters/console_suspend')
            if console_suspend not in {'Y', 'N'}:
                raise ValueError()
        except (OSError, ValueError):
            progress['console_suspend'] = 'unavailable'
        else:
            progress['console_suspend'] = 'enabled' if console_suspend == 'Y' else 'already-disabled'
        node_write(sleep, 's2idle')
        mode_changed = True
        node_write(alarm, '+12')
        candidate = node_read(alarm)
        if (not candidate.isascii() or not candidate.isdecimal()
                or not 1 <= int(candidate) <= 2**64 - 1 or candidate != str(int(candidate))):
            raise ValueError()
        programmed = candidate
        progress['rtc_programmed'] = True
        sleep_progress(root, cycle, 'rtc-programmed', progress)
        progress['power_write_prepared'] = True
        sleep_progress(root, cycle, 'power-write-prepared', progress)
        # Diagnostic fsync/output may consume time; retain the original timing
        # guards immediately before the actual sleep syscall.
        if time.monotonic() > arm_deadline:
            raise ValueError()
        if query_clock is not None:
            pending_query(query_clock, time.clock_gettime_ns(time.CLOCK_BOOTTIME) // 1000000)
        node_write('/sys/power/state', 'mem')
        after = time.clock_gettime_ns(time.CLOCK_BOOTTIME)
        awake = time.clock_gettime_ns(time.CLOCK_MONOTONIC)
        progress.update(power_write_returned=True, boottime_after_ns=after, monotonic_after_ns=awake)
        sleep_progress(root, cycle, 'power-write-returned', progress)
        success = int(node_read('/sys/power/suspend_stats/success'))
        fail = int(node_read('/sys/power/suspend_stats/fail'))
        if not 0 <= success <= 2**64 - 1 or not 0 <= fail <= 2**64 - 1:
            raise ValueError()
        progress.update(suspend_success_after=success, suspend_fail_after=fail)
        proof = {'sleep_mode': 's2idle', 'boot_unchanged': boot == node_read('/proc/sys/kernel/random/boot_id'),
                 'process_unchanged': identity == process_identity(pid),
                 'suspend_success_delta': success - ready['suspend_success'],
                 'suspend_fail_delta': fail - ready['suspend_fail'],
                 'boottime_delta_ms': (after - before) // 1000000,
                 'suspended_delta_ms': ((after - before) - (awake - mono)) // 1000000}
    finally:
        handlers = {number: signal.signal(number, signal.SIG_IGN) for number in [signal.SIGTERM, signal.SIGINT]}
        try:
            try:
                sleep_progress(root, cycle, 'restoration-started', progress)
            finally:
                # A diagnostic failure must never prevent restoration.
                restore_failed = not restore_sleep(ready['sleep_mode'], programmed, mode_changed,
                                                   console_original, console_programmed)
                cleanup_status[0] = 'failed' if restore_failed else 'verified'
                progress['kernel_cleanup'] = cleanup_status[0]
                sleep_progress(root, cycle, 'restoration-returned', progress)
        finally:
            for number, handler in handlers.items():
                signal.signal(number, handler)
        cleanup_status[0] = 'failed' if restore_failed else 'verified'
    if restore_failed:
        raise ValueError()
    if (not proof['boot_unchanged'] or not proof['process_unchanged']
            or proof['suspend_success_delta'] != 1 or proof['suspend_fail_delta'] != 0
            or not 9000 <= proof['suspended_delta_ms'] <= 20000
            or not 9000 <= proof['boottime_delta_ms'] <= 30000):
        raise ValueError()
    proof['settings_restored'] = True
    if query_clock is not None:
        # Leave at least two seconds on a suspend-excluding timer, so an
        # ordinary expiry during kernel preparation cannot masquerade as recovery.
        if (awake - mono) // 1000000 > 1000:
            raise ValueError()
        proof['query_pending_at_entry'] = True
        proof['query_timer_window_valid'] = True
    return proof


def checked_owner(value, cycle):
    fields = {'schema_version', 'cycle', 'elapsed_ms', 'samples', 'maximum_gap_ms',
              'state_changed', 'transports_closed', 'changed_at_ms', 'closed_at_ms', 'query_deadline_expired'}
    numbers = {'schema_version': (1, 1), 'cycle': (cycle, cycle), 'elapsed_ms': (9000, 30000),
               'samples': (10, 31), 'maximum_gap_ms': (0, 2000), 'transports_closed': (0, 8)}
    if (type(value) is not dict or set(value) != fields
            or any(type(value[key]) is not int or not low <= value[key] <= high
                   for key, (low, high) in numbers.items())
            or type(value['state_changed']) is not bool or value['state_changed'] != (cycle == 2)):
        raise ValueError()
    if type(value['query_deadline_expired']) is not bool or value['query_deadline_expired'] != (cycle == 1):
        raise ValueError()
    if cycle == 1:
        if value['changed_at_ms'] is not None or value['closed_at_ms'] is not None or value['transports_closed'] != 0:
            raise ValueError()
    elif (type(value['changed_at_ms']) is not int or type(value['closed_at_ms']) is not int
          or not 3000 <= value['changed_at_ms'] <= value['closed_at_ms'] <= 9000
          or value['transports_closed'] < 1):
        raise ValueError()
    return value


NATIVE_BOOLEANS = {'expired_promptly', 'expired_failed', 'no_late_success', 'reconnected_snapshot',
                   'owner_event', 'store_unchanged', 'pin_unchanged', 'no_mutations'}


def checked_native(value):
    if (type(value) is not dict or set(value) != NATIVE_BOOLEANS | {'query_after_wake_ms'}
            or any(type(value[key]) is not bool for key in NATIVE_BOOLEANS)
            or type(value['query_after_wake_ms']) is not int or not 0 <= value['query_after_wake_ms'] <= 8000):
        raise ValueError()
    return value


def sleep_progress_self_test():
    from contextlib import redirect_stdout
    from io import StringIO
    from unittest.mock import patch
    invalid_alarms = {'alarm-empty': '', 'alarm-zero': '0', 'alarm-negative': '-1', 'alarm-plus': '+12',
                      'alarm-leading-zero': '01', 'alarm-nonascii': '\u0661', 'alarm-overflow': str(2**64)}
    for scenario in ['success', 'write-error', 'late-arm', 'late-query', 'foreign-console',
                     'console-unavailable', 'restoration-log-error', 'alarm-max', 'foreign-alarm', *invalid_alarms]:
        with tempfile.TemporaryDirectory(prefix='veranda-sleep-progress-', dir='/tmp') as directory:
            root = Path(directory)
            root.chmod(0o700)
            nodes = {'/sys/power/mem_sleep': 's2idle [deep]', '/sys/class/rtc/rtc0/wakealarm': '',
                     '/proc/sys/kernel/random/boot_id': 'synthetic-private-boot-id',
                     '/sys/power/suspend_stats/success': '3', '/sys/power/suspend_stats/fail': '0',
                     '/proc/sys/kernel/printk': '4\t4\t1\t7',
                     '/sys/module/printk/parameters/console_suspend': 'Y'}
            stages, wrote, snapshot_types, alarm_reads = [], [], [], []
            original_progress, original_fsync = sleep_progress, os.fsync
            def report(root, cycle, phase, measurements):
                stages.append(phase)
                if phase == 'restoration-started':
                    snapshots = 2 if 'power-write-returned' in stages else 1 if 'power-write-prepared' in stages else 0
                    assert snapshot_types == [stat.S_IFREG, stat.S_IFDIR] * snapshots
                if phase == 'restoration-started' and scenario == 'restoration-log-error':
                    raise OSError('synthetic diagnostic failure')
                original_progress(root, cycle, phase, measurements)
            def read(path):
                if path == '/sys/class/rtc/rtc0/wakealarm':
                    alarm_reads.append(nodes[path])
                if path == '/proc/sys/kernel/printk' and scenario == 'console-unavailable':
                    raise OSError('synthetic missing console setting')
                return nodes[path]
            def write(path, value):
                wrote.append((path, value))
                if path == '/sys/power/state':
                    assert snapshot_types == [stat.S_IFREG, stat.S_IFDIR]
                    if scenario == 'write-error':
                        raise OSError('synthetic suspend failure')
                    nodes['/sys/power/suspend_stats/success'] = '4'
                    nodes['/sys/class/rtc/rtc0/wakealarm'] = ''
                    if scenario == 'foreign-console':
                        nodes['/proc/sys/kernel/printk'] = '8 4 1 7'
                    if scenario == 'foreign-alarm':
                        nodes['/sys/class/rtc/rtc0/wakealarm'] = 'foreign'
                else:
                    if path == '/sys/class/rtc/rtc0/wakealarm' and value == '+12':
                        nodes[path] = invalid_alarms.get(scenario, str(2**64 - 1) if scenario == 'alarm-max' else '1234')
                    else:
                        nodes[path] = ('[s2idle] deep' if value == 's2idle' else 's2idle [deep]') if path == '/sys/power/mem_sleep' else value
            def clock(number):
                resumed = ('/sys/power/state', 'mem') in wrote
                if number == time.CLOCK_MONOTONIC:
                    return 1100000000 if resumed else 1000000000
                return 14100000000 if resumed else 5001000000 if scenario == 'late-query' and 'power-write-prepared' in stages else 2000000000
            def fsync(fd):
                snapshot_types.append(stat.S_IFMT(os.fstat(fd).st_mode))
                original_fsync(fd)
            ready = {'result': 'ready', 'reason': 'ready', 'sleep_mode': 'deep', 'suspend_success': 3, 'suspend_fail': 0}
            output, cleanup = StringIO(), ['unmeasured']
            with patch.dict(globals(), {'node_read': read, 'node_write': write, 'prerequisites': lambda: ready,
                                       'process_identity': lambda _pid: 'synthetic-private-process-id', 'sleep_progress': report}), \
                    patch.object(time, 'clock_gettime_ns', clock), \
                    patch.object(time, 'monotonic', lambda: 2 if scenario == 'late-arm' and 'power-write-prepared' in stages else 0), \
                    patch.object(os, 'fsync', fsync), redirect_stdout(output):
                failed = False
                try:
                    proof = suspend_cycle(root, 1, 2, 1, cleanup, {'started_boottime_ms': 2000, 'timeout_ms': 5000})
                    assert proof['settings_restored'] and proof['suspend_success_delta'] == 1
                except (OSError, ValueError):
                    failed = True
            assert failed == (scenario not in {'success', 'console-unavailable', 'alarm-max'})
            assert stages[:1 if scenario in invalid_alarms else 3] == (['prerequisites'] if scenario in invalid_alarms else
                                                                      ['prerequisites', 'rtc-programmed', 'power-write-prepared'])
            assert stages[-2:] == ['restoration-started', 'restoration-returned']
            assert ('power-write-returned' in stages) == (scenario not in {'write-error', 'late-arm', 'late-query'} | invalid_alarms.keys())
            assert (('/sys/power/state', 'mem') in wrote) == (scenario not in {'late-arm', 'late-query'} | invalid_alarms.keys())
            assert nodes['/sys/power/mem_sleep'] == 's2idle [deep]'
            assert nodes['/sys/class/rtc/rtc0/wakealarm'] == invalid_alarms.get(scenario, 'foreign' if scenario == 'foreign-alarm' else '0')
            ambiguous_alarm = scenario in invalid_alarms and invalid_alarms[scenario] not in {'', '0'} or scenario == 'foreign-alarm'
            assert cleanup[0] == ('failed' if scenario == 'foreign-console' or ambiguous_alarm else 'verified')
            assert len(alarm_reads) == (2 if scenario in invalid_alarms or scenario == 'foreign-alarm' else 3)
            assert (('/sys/class/rtc/rtc0/wakealarm', '0') in wrote) == (scenario not in invalid_alarms and scenario != 'foreign-alarm')
            assert nodes['/proc/sys/kernel/printk'] == ('8 4 1 7' if scenario == 'foreign-console' else '4\t4\t1\t7')
            assert nodes['/sys/module/printk/parameters/console_suspend'] == 'Y'
            data = private_read(root / 'progress-1.json', os.geteuid())
            record = json.loads(data)
            assert record['phase'] == 'restoration-returned' and record['kernel_cleanup'] == cleanup[0]
            assert record['rtc_programmed'] == (scenario not in invalid_alarms)
            assert record['power_write_prepared'] == (scenario not in invalid_alarms)
            assert record['console_visibility'] == ('unavailable' if scenario == 'console-unavailable' else 'raised')
            assert record['suspend_success_before'] == 3 and record['boottime_before_ns'] == 2000000000
            if record['power_write_returned']:
                assert record['boottime_after_ns'] == 14100000000 and record['suspend_success_after'] == 4
            assert len(data) <= 4096 and stat.S_IFREG in snapshot_types and stat.S_IFDIR in snapshot_types
            assert 'synthetic-private' not in data.decode() + output.getvalue()
            for bad in [{'token': 'synthetic-private'}, {'boottime_before_ns': 2**64},
                        {'sleep_mode': 'synthetic-private'}, {'rtc_programmed': 1},
                        {'console_suspend': 'synthetic-private'}]:
                try:
                    original_progress(root, 1, 'prerequisites', bad)
                except ValueError:
                    pass
                else:
                    raise AssertionError()
            assert private_read(root / 'progress-1.json', os.geteuid()) == data
    for scenario in ['enabled', 'already-disabled', 'missing', 'invalid']:
        with tempfile.TemporaryDirectory(prefix='veranda-sleep-console-', dir='/tmp') as directory:
            root = Path(directory)
            root.chmod(0o700)
            path = '/sys/module/printk/parameters/console_suspend'
            original = 'N' if scenario == 'already-disabled' else 'invalid' if scenario == 'invalid' else 'Y'
            nodes = {'/sys/power/mem_sleep': 's2idle [deep]', '/sys/class/rtc/rtc0/wakealarm': '',
                     '/proc/sys/kernel/random/boot_id': 'synthetic-private-boot-id',
                     '/sys/power/suspend_stats/success': '3', '/sys/power/suspend_stats/fail': '0',
                     '/proc/sys/kernel/printk': '4 4 1 7', path: original}
            writes = []
            def read(node):
                if node == path:
                    if scenario == 'missing':
                        raise OSError('synthetic missing setting')
                return nodes[node]
            def write(node, value):
                writes.append((node, value))
                if node == '/sys/power/state':
                    nodes['/sys/power/suspend_stats/success'] = '4'
                    nodes['/sys/class/rtc/rtc0/wakealarm'] = ''
                elif node == '/sys/class/rtc/rtc0/wakealarm':
                    nodes[node] = '1234' if value == '+12' else value
                else:
                    nodes[node] = ('[s2idle] deep' if value == 's2idle' else 's2idle [deep]') if node == '/sys/power/mem_sleep' else value
            def clock(number):
                resumed = ('/sys/power/state', 'mem') in writes
                return (1100000000 if resumed else 1000000000) if number == time.CLOCK_MONOTONIC else (14100000000 if resumed else 2000000000)
            ready = {'result': 'ready', 'reason': 'ready', 'sleep_mode': 'deep', 'suspend_success': 3, 'suspend_fail': 0}
            output, cleanup, failed = StringIO(), ['unmeasured'], False
            with patch.dict(globals(), {'node_read': read, 'node_write': write, 'prerequisites': lambda: ready,
                                       'process_identity': lambda _pid: 'synthetic-private-process-id'}), \
                    patch.object(time, 'clock_gettime_ns', clock), patch.object(time, 'monotonic', lambda: 0), redirect_stdout(output):
                try:
                    proof = suspend_cycle(root, 1, 2, 1, cleanup)
                    assert proof['settings_restored'] and proof['suspend_success_delta'] == 1
                except (OSError, ValueError):
                    failed = True
            assert not failed and ('/sys/power/state', 'mem') in writes
            assert cleanup[0] == 'verified' and nodes[path] == original
            assert not any(node == path for node, _value in writes)
            assert nodes['/sys/power/mem_sleep'] == 's2idle [deep]' and nodes['/proc/sys/kernel/printk'] == '4 4 1 7'
            data = private_read(root / 'progress-1.json', os.geteuid())
            record = json.loads(data)
            assert record['phase'] == 'restoration-returned' and record['kernel_cleanup'] == cleanup[0]
            assert record['console_suspend'] == ('unavailable' if scenario in {'missing', 'invalid'} else 'already-disabled' if scenario == 'already-disabled' else 'enabled')
            assert len(data) <= 4096 and 'synthetic-private' not in data.decode() + output.getvalue()
    with tempfile.TemporaryDirectory(prefix='veranda-sleep-blocked-', dir='/tmp') as directory:
        root = Path(directory)
        root.chmod(0o700)
        with patch.dict(globals(), {'prerequisites': lambda: {'result': 'blocked', 'reason': 'rtc_wakeup_disabled', 'sleep_mode': 'unknown',
                                                            'suspend_success': None, 'suspend_fail': None}}), \
                redirect_stdout(StringIO()):
            try:
                suspend_cycle(root, 1, 2, 1, ['unmeasured'])
            except ValueError:
                pass
            else:
                raise AssertionError()
        assert json.loads(private_read(root / 'progress-1.json', os.geteuid()))['prerequisite_result'] == 'blocked'


def owner_identity_self_test():
    from io import BytesIO
    from types import SimpleNamespace
    from unittest.mock import call, patch
    with tempfile.TemporaryDirectory(prefix='veranda-sleep-owner-', dir='/tmp') as directory:
        root = Path(directory)
        root.chmod(0o700)
        source = root / 'src'
        source.mkdir(mode=0o700)
        source.chmod(0o700)
        run, uid = 'a' * 8, os.getuid()
        child = SimpleNamespace(stdin=BytesIO(), stdout=BytesIO(), poll=lambda: 0, wait=lambda **_: 0)
        with patch.dict(globals(), {'context': lambda: (root, source, run, '2'),
                                    'process_identity': lambda _pid: 'owned-start'}), \
                patch.object(sys, 'argv', [__file__, '--rpc-proxy']), \
                patch.object(signal, 'signal'), patch.object(time, 'sleep'), \
                patch.object(threading, 'Thread') as thread, \
                patch.object(subprocess, 'Popen', return_value=child) as popen, \
                patch.object(subprocess, 'run') as execute, \
                patch.object(pwd, 'getpwnam', return_value=SimpleNamespace(pw_uid=1234, pw_gid=4321)), \
                patch.object(os, 'getgrouplist', return_value=[4321, 54]) as groups:
            thread.return_value = SimpleNamespace(start=lambda: None)
            rpc_proxy()
            owner_action('event')
            thread.side_effect = lambda target, **_: SimpleNamespace(start=target)
            owner_cycle(2)
        calls = [popen.call_args, *execute.call_args_list]
        for call, arguments, timeout in zip(calls, [('rpc', '--stdio'), ('start', '--yes'), ('stop', '--force', '--yes')], [None, 45, 8]):
            assert call.args[0] == [str(source / '.build/yard'), '-Y', 'pv-' + run, *arguments]
            assert call.kwargs['cwd'] == source
            assert (call.kwargs['user'], call.kwargs['group'], call.kwargs['extra_groups']) == (1234, 4321, [4321, 54])
            env = call.kwargs['env']
            assert (env['HOME'], env['USER'], env['LOGNAME'], env['SUBYARD_OPERATOR_HOME']) == ('/home/dev', 'dev', 'dev', '/home/dev')
            assert env['SUBYARD_CONFIG_HOME'] == '/var/tmp/subyard-preview-' + run + '/config'
            assert call.kwargs.get('timeout') == timeout
        assert len(calls) == 3 and os.getuid() == uid
        assert groups.call_args_list == [call('dev', 4321)] * 3
        assert not list(root.glob('proxy-*.json'))
        record = root / 'owner-2.json'
        assert record.stat().st_uid == uid and stat.S_IMODE(record.stat().st_mode) == 0o600
        value = json.loads(private_read(record, uid))
        assert value['samples'] == 31 and value['state_changed'] and value['cycle'] == 2


def acceptance_self_test():
    global node_read, node_write
    owner_identity_self_test()
    sleep_progress_self_test()
    ids = ['a' * 8, 'slot-001', '1', 'veranda-sleep', 'b' * 32, 'subyard-pair']
    assert valid_context_ids(*ids)
    for slot in ['001', 'slot-000', 'slot-1000', 'private-value']:
        assert not valid_context_ids(ids[0], slot, *ids[2:])
    assert pending_query({'started_boottime_ms': 1000, 'timeout_ms': 5000}, 3000)
    for value in [{'started_boottime_ms': 1000, 'timeout_ms': 5000},
                  {'started_boottime_ms': True, 'timeout_ms': 5000},
                  {'started_boottime_ms': 1000, 'timeout_ms': 10000},
                  {'started_boottime_ms': 1000, 'timeout_ms': 5000, 'private': 'private-value'}]:
        try:
            pending_query(value, 3001)
        except ValueError:
            pass
        else:
            raise AssertionError()
    original_read, original_write = node_read, node_write
    nodes = {'/sys/class/rtc/rtc0/wakealarm': 'foreign', '/sys/power/mem_sleep': '[s2idle] deep'}
    writes = []
    def fake_write(path, value):
        writes.append((path, value))
        nodes[path] = '[' + value + '] s2idle' if path == '/sys/power/mem_sleep' else value
    node_read, node_write = nodes.__getitem__, fake_write
    try:
        assert not restore_sleep('deep', 'owned', True)
        assert writes == [('/sys/power/mem_sleep', 'deep')]
        assert nodes['/sys/class/rtc/rtc0/wakealarm'] == 'foreign'
    finally:
        node_read, node_write = original_read, original_write
    owner = {'schema_version': 1, 'cycle': 2, 'elapsed_ms': 12000, 'samples': 13,
             'maximum_gap_ms': 1001, 'state_changed': True, 'transports_closed': 1,
             'changed_at_ms': 4000, 'closed_at_ms': 4100, 'query_deadline_expired': False}
    native = {**dict.fromkeys(NATIVE_BOOLEANS, True), 'query_after_wake_ms': 20}
    assert checked_owner(owner, 2) == owner and checked_native(native) == native
    first = {**owner, 'cycle': 1, 'state_changed': False, 'transports_closed': 0,
             'changed_at_ms': None, 'closed_at_ms': None, 'query_deadline_expired': True}
    assert checked_owner(first, 1) == first
    assert not all(checked_native({**native, 'expired_promptly': False})[key] for key in NATIVE_BOOLEANS)
    from io import BytesIO
    data = json.dumps({'type': 'response', 'id': 'request-1', 'result': {}}).encode()
    assert frame(BytesIO(struct.pack('>I', len(data)) + data))[0]['id'] == 'request-1'
    for data in [b'[]', b'private-value']:
        try:
            frame(BytesIO(struct.pack('>I', len(data)) + data))
        except ValueError:
            pass
        else:
            raise AssertionError()
    for parser, value in [(lambda value: checked_owner(value, 2), owner), (checked_native, native)]:
        for bad in [{**value, 'private': 'private-value'}, {**value, next(iter(value)): 'private-value'}, {}]:
            try:
                parser(bad)
            except ValueError:
                pass
            else:
                raise AssertionError()
    for key, value in [('samples', True), ('samples', 32), ('transports_closed', 9), ('closed_at_ms', 10000)]:
        try:
            checked_owner({**owner, key: value}, 2)
        except ValueError:
            pass
        else:
            raise AssertionError()
    for value in [True, -1, 8001]:
        try:
            checked_native({**native, 'query_after_wake_ms': value})
        except ValueError:
            pass
        else:
            raise AssertionError()
    with tempfile.TemporaryDirectory(prefix='veranda-sleep-self-test-') as directory:
        root = Path(directory)
        put(root, 'receipt', b'original')
        assert private_read(root / 'receipt', os.geteuid()) == b'original'
        (root / 'receipt.tmp').symlink_to(root / 'receipt')
        try:
            put(root, 'receipt', b'replacement')
        except FileExistsError:
            pass
        else:
            raise AssertionError()
        assert private_read(root / 'receipt', os.geteuid()) == b'original'
        (root / 'receipt').chmod(0o644)
        try:
            private_read(root / 'receipt', os.geteuid())
        except ValueError:
            pass
        else:
            raise AssertionError()


def acceptance_client(native, peer_config, destination):
    root, source, run, vm = context(create=True)
    if vm != '1' or not re.fullmatch(r'root@[0-9.]+:22', destination):
        raise ValueError()
    native = (source / native).resolve()
    if not native.is_relative_to(source / '.build') or not native.is_file():
        raise ValueError()
    peer_config = Path(peer_config)
    if peer_config != source.parent / 'peer-config':
        raise ValueError()
    private_read(peer_config)
    def peer(action):
        command = [sys.executable, str(source / 'dev/e2e/veranda-sleep.py'), '--owner-action', action]
        # VM2 has a different transport directory; the protected config's fixed command uses its recorded source.
        peer_source = private_read(source.parent / 'peer-source').decode().strip()
        if not re.fullmatch(r'/tmp/subyard-worktree\.[A-Za-z0-9]+/src', peer_source):
            raise ValueError()
        command[1] = peer_source + '/dev/e2e/veranda-sleep.py'
        environment = {key: os.environ[key] for key in ['SUBYARD_E2E_RUN_ID', 'SUBYARD_E2E_SLOT',
                       'SUBYARD_E2E_VM', 'SUBYARD_E2E_TYPE', 'SUBYARD_E2E_PURPOSE', 'SUBYARD_E2E_SLEEP_TOKEN']}
        environment['SUBYARD_E2E_VM'] = '2'
        remote = ['env'] + [key + '=' + value for key, value in environment.items()] + command
        return subprocess.run(['ssh', '-F', str(peer_config), 'peer-admin', '--', shlex.join(remote)],
                              check=True, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=50).stdout
    agent = None
    child = None
    kernel_cleanup = ['unmeasured']
    def interrupted(number, _frame):
        raise InterruptedError(number)
    handlers = {number: signal.signal(number, interrupted) for number in [signal.SIGTERM, signal.SIGINT]}
    original_rc = 1
    receipt = {'schema_version': 1, 'result': 'failed', 'phase': 'authentication', 'exception': 'none',
               'reason': 'operation_failed', 'cleanup': 'unverified', 'kernel_cleanup': 'unmeasured',
               'native_exit_code': None, 'cycles': []}
    try:
        agent = subprocess.Popen(['ssh-agent', '-D', '-a', str(root / 'agent.sock')],
                                 stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        put(root, 'agent-process', {'pid': agent.pid, 'start': process_identity(agent.pid)})
        until = time.monotonic() + 5
        while not (root / 'agent.sock').exists():
            if time.monotonic() >= until:
                raise ValueError()
            time.sleep(.02)
        env = {**os.environ, 'SSH_AUTH_SOCK': str(root / 'agent.sock'),
               'VERANDA_TEST_DISPOSABLE_OWNER': '1', 'VERANDA_TEST_SLEEP_CONTROL_ROOT': str(root),
               'VERANDA_TEST_SSH_DESTINATION': destination, 'VERANDA_TEST_SLEEP_YARD': 'pv-' + run}
        subprocess.run(['ssh-add', str(source.parent / 'peer-key')], env=env, check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=5)
        log_fd = os.open(root / 'native.log', os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        os.fchmod(log_fd, 0o600)
        try:
            child = subprocess.Popen([str(native), 'client::client_tests::native_sleep_recovery', '--exact',
                                      '--ignored', '--nocapture', '--test-threads=1'], env=env,
                                     stdin=subprocess.DEVNULL, stdout=log_fd, stderr=subprocess.STDOUT)
            put(root, 'native-process', {'pid': child.pid, 'start': process_identity(child.pid)})
        finally:
            os.close(log_fd)
        overall = time.clock_gettime(time.CLOCK_BOOTTIME) + 180
        def wait_file(name):
            while not (root / name).exists():
                if child.poll() is not None or time.clock_gettime(time.CLOCK_BOOTTIME) >= overall:
                    raise ValueError()
                time.sleep(.02)
            return private_read(root / name)
        for cycle in [1, 2]:
            receipt['phase'] = 'query-suspend' if cycle == 1 else 'reconnect-suspend'
            if wait_file('cycle-' + str(cycle) + '.request') != b'sleep-cycle-v1\n':
                raise ValueError()
            if cycle == 1:
                until = time.monotonic() + 4
                while json.loads(peer('held')) != {'held': True}:
                    if time.monotonic() > until:
                        raise ValueError()
                    time.sleep(.02)
            arm_started = time.monotonic()
            peer('arm-' + str(cycle))
            if time.monotonic() - arm_started > 1:
                raise ValueError()
            query_clock = json.loads(private_read(root / 'query-clock.json')) if cycle == 1 else None
            proof = suspend_cycle(root, cycle, child.pid, time.monotonic() + 1, kernel_cleanup, query_clock)
            receipt['cycles'].append(proof)
            put(root, 'cycle-' + str(cycle) + '.wake', b'sleep-cycle-v1\n')
            owner = checked_owner(json.loads(peer('observe-' + str(cycle))), cycle)
            proof.update(owner_awake=True, owner_elapsed_ms=owner['elapsed_ms'], owner_samples=owner['samples'],
                         state_changed=owner['state_changed'], transports_closed=owner['transports_closed'])
            proof.update(changed_at_ms=owner['changed_at_ms'], closed_at_ms=owner['closed_at_ms'])
            proof['external_deadline_expired'] = owner['query_deadline_expired']
            if cycle == 1:
                wait_file('query-observed')
                peer('release')
                proof.update(withheld_responses=1, released_responses=1)
                put(root, 'query-released', b'sleep-cycle-v1\n')
        receipt['phase'] = 'owner-event'
        wait_file('event.request')
        peer('event')
        receipt['phase'] = 'native-proof'
        native_proof = checked_native(json.loads(wait_file('native-proof.json')))
        receipt['native'] = native_proof
        receipt['native_exit_code'] = child.wait(timeout=10)
        if receipt['native_exit_code'] == 0 and all(native_proof[key] for key in NATIVE_BOOLEANS):
            receipt.update(result='passed', reason='verified', phase='complete')
            original_rc = 0
    except (OSError, ValueError, subprocess.SubprocessError, KeyError, TypeError) as failure:
        if isinstance(failure, InterruptedError):
            original_rc = 128 + failure.args[0]
        receipt['exception'] = ('interrupted' if isinstance(failure, InterruptedError) else 'io' if isinstance(failure, OSError) else 'child'
                                if isinstance(failure, subprocess.SubprocessError) else 'guard')
    finally:
        for number in handlers:
            signal.signal(number, signal.SIG_IGN)
        cleanup_failed, owned_ssh = False, []
        if child is not None and child.poll() is None:
            try:
                owned_ssh = native_ssh_children(child.pid)
                put(root, 'ssh-processes', owned_ssh)
            except (OSError, ValueError):
                cleanup_failed = True
            try:
                child.terminate()
                child.wait(timeout=5)
            except (OSError, subprocess.SubprocessError):
                cleanup_failed = True
            for record in owned_ssh:
                try:
                    stop_owned(record)
                except (OSError, ValueError):
                    cleanup_failed = True
        if child is not None and receipt['native_exit_code'] is None:
            receipt['native_exit_code'] = child.returncode
        if agent is not None:
            try:
                agent.terminate()
                agent.wait(timeout=5)
            except (OSError, subprocess.SubprocessError):
                cleanup_failed = True
        receipt['cleanup'] = 'failed' if cleanup_failed else 'verified'
        if receipt['cleanup'] != 'verified':
            if receipt['result'] == 'passed':
                receipt.update(reason='cleanup_failed', phase='cleanup')
            receipt['result'] = 'failed'
        receipt['kernel_cleanup'] = kernel_cleanup[0]
        if receipt['kernel_cleanup'] not in {'verified', 'unmeasured'}:
            receipt['cleanup'] = 'failed'
            receipt['result'] = 'failed'
        if original_rc == 0 and receipt['result'] != 'passed':
            original_rc = 3
        receipt['operation_exit_code'] = original_rc
        put(root, 'receipt.json', receipt)
        print('sleep-acceptance: ' + json.dumps(receipt, sort_keys=True))
        for number, handler in handlers.items():
            signal.signal(number, handler)
    return original_rc


def cleanup_sleep():
    root, source, run, vm = context()
    if vm == '1':
        for name in ['native-process', 'agent-process']:
            if (root / name).exists():
                stop_owned(json.loads(private_read(root / name)))
        if (root / 'ssh-processes').exists():
            records = json.loads(private_read(root / 'ssh-processes'))
            if not isinstance(records, list) or len(records) > 8:
                raise ValueError()
            for record in records:
                stop_owned(record)
    if vm == '2':
        for record in list(root.glob('proxy-*.json')) + list(root.glob('cycle-process-*')):
            stop_owned(json.loads(private_read(record)))
        wrapper_record = root / 'wrapper.json'
        if wrapper_record.exists():
            value = json.loads(private_read(wrapper_record))
            target = Path('/usr/local/bin/yard')
            info = target.lstat()
            if (not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_nlink != 1
                    or stat.S_IMODE(info.st_mode) != 0o755 or info.st_ino != value['inode']
                    or info.st_dev != value['device'] or hashlib.sha256(target.read_bytes()).hexdigest() != value['sha256']):
                raise ValueError()
            target.unlink()
    # Retain private native logs and failed receipts until the parent copies bounded evidence.
    print('sleep-stage: cleanup-verified')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument('--prerequisites-only', action='store_true', help='inspect capabilities on an owned disposable guest without entering sleep')
    mode.add_argument('--self-test', action='store_true', help='check synthetic classification without reading host capabilities')
    mode.add_argument('--owner-start', action='store_true')
    mode.add_argument('--rpc-proxy', action='store_true')
    mode.add_argument('--owner-action', choices=['arm-1', 'arm-2', 'held', 'release', 'event', 'observe-1', 'observe-2'])
    mode.add_argument('--owner-cycle', type=int, choices=[1, 2])
    mode.add_argument('--acceptance-client', action='store_true')
    mode.add_argument('--cleanup', action='store_true')
    mode.add_argument('--evidence', action='store_true', help='export only fixed private acceptance logs before cleanup')
    parser.add_argument('--native')
    parser.add_argument('--peer-config')
    parser.add_argument('--destination')
    args = parser.parse_args()
    if args.self_test:
        self_test()
        return 0
    if args.owner_start:
        owner_start()
        return 0
    if args.rpc_proxy:
        rpc_proxy()
        return 0
    if args.owner_action:
        owner_action(args.owner_action)
        return 0
    if args.owner_cycle:
        owner_cycle(args.owner_cycle)
        return 0
    if args.cleanup:
        cleanup_sleep()
        return 0
    if args.evidence:
        return export_evidence()
    if args.acceptance_client:
        return acceptance_client(args.native, args.peer_config, args.destination)
    result = prerequisites()
    print(json.dumps(result, sort_keys=True))
    return 0 if result['result'] == 'ready' else 1


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError):
        print('sleep-stage: guarded-operation-failed', file=sys.stderr)
        sys.exit(1)
