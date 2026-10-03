#!/usr/bin/env python3
"""Retain sanitized first-boot evidence before Incus discards stopped-VM logs."""
import grp
import json
import os
from pathlib import Path
import pwd
import re
import select
import shlex
import signal
import subprocess
import sys
import time


PROJECT = 'subyard-vpn-e2e'
INSTANCE = 'yard-vpn-e2e'
REASONS = ('disconnect', 'guest-shutdown', 'guest-reset', 'guest-panic',
           'host-qmp-quit', 'host-signal', 'host-error', 'subsystem-reset')
ACTIONS = ('instance-started', 'instance-stopped', 'instance-shutdown', 'instance-restarted')
STATES = ('Running', 'Stopped', 'Starting', 'Stopping', 'Error', 'Frozen')


def event_evidence(event):
    if not isinstance(event, dict) or not isinstance(event.get('metadata'), dict):
        return None
    metadata = event['metadata']
    if event.get('type') == 'lifecycle' and event.get('project') == PROJECT:
        if metadata.get('source') not in (f'/1.0/instances/{INSTANCE}',
                                         f'/1.0/instances/{INSTANCE}?project={PROJECT}'):
            return None
        if metadata.get('action') in ACTIONS:
            return {'action': metadata['action']}
    context = metadata.get('context', {})
    if (event.get('type') == 'logging' and metadata.get('message') == 'Instance stopped'
            and isinstance(context, dict) and context.get('project') == PROJECT
            and context.get('instance') == INSTANCE):
        return {'shutdown_reason': context.get('reason') if context.get('reason') in REASONS else 'unknown',
                'shutdown_target': context.get('target') if context.get('target') in ('stop', 'reboot') else 'unknown'}
    return None


def incus_arguments(*arguments):
    argv = ['incus', *arguments]
    try:
        group = grp.getgrnam('incus-admin').gr_gid
        user = pwd.getpwuid(os.getuid()).pw_name
        if group not in [os.getegid(), *os.getgroups()] and group in os.getgrouplist(user, os.getgid()):
            return ['/usr/bin/sg', 'incus-admin', '-c', 'exec ' + shlex.join(argv)]
    except KeyError:
        pass  # Bootstrap may not have created the group yet.
    return argv


def stop_child(child):
    for sig in (signal.SIGTERM, signal.SIGKILL):
        try:
            os.killpg(child.pid, sig)
        except ProcessLookupError:
            pass
        if sig == signal.SIGTERM:
            try:
                child.wait(timeout=1)
            except subprocess.TimeoutExpired:
                pass
    child.wait()


def probe(*arguments):
    """Limit both probe duration and raw in-memory output; never retain stderr."""
    try:
        child = subprocess.Popen(incus_arguments(*arguments), stdout=subprocess.PIPE,
                                 stderr=subprocess.DEVNULL, stdin=subprocess.DEVNULL,
                                 start_new_session=True)
    except OSError:
        return None
    output = bytearray()
    deadline = time.monotonic() + 2
    try:
        while time.monotonic() < deadline:
            if not select.select([child.stdout], [], [], max(0, deadline - time.monotonic()))[0]:
                break
            chunk = os.read(child.stdout.fileno(), 4096)
            if not chunk:
                child.wait(timeout=max(0.001, deadline - time.monotonic()))
                return bytes(output) if child.returncode == 0 else None
            output.extend(chunk)
            if len(output) > 65536:
                break
    except subprocess.TimeoutExpired:
        pass
    finally:
        stop_child(child)
        child.stdout.close()
    return None


def run(command, duration=900):
    # This deadline bounds observation only; it never changes init's deadline.
    started = time.monotonic()
    monitor = None
    child = None
    previous = {}
    buffer = b''
    dropping = False
    rows = 0
    reason = 'unavailable'
    subscribed = False
    start_seen = False
    hints = set()
    gaps = set()
    next_sample = 0

    def emit(**values):
        nonlocal rows
        if 'gap' in values:
            if values['gap'] in gaps:
                return
            gaps.add(values['gap'])
        if rows < 512:
            values['elapsed_ms'] = int((time.monotonic() - started) * 1000)
            print('amnezia_first_boot=' + json.dumps(values, sort_keys=True), flush=True)
            rows += 1

    def forward(signum, _frame):
        if child is not None:
            try:
                child.send_signal(signum)
            except ProcessLookupError:
                pass

    emit(observer='started', subscription='unconfirmed')
    try:
        while time.monotonic() - started < duration:
            if monitor is None:
                try:
                    monitor = subprocess.Popen(incus_arguments('monitor', '--project', PROJECT,
                                                '--type=lifecycle', '--type=logging', '--format=json'),
                                               stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                               stdin=subprocess.DEVNULL, start_new_session=True)
                    os.set_blocking(monitor.stdout.fileno(), False)
                except OSError:
                    emit(gap='monitor_unavailable')
                buffer = b''
                dropping = False
            if child is None:
                child = subprocess.Popen(command)
                for sig in (signal.SIGINT, signal.SIGTERM):
                    previous[sig] = signal.signal(sig, forward)
            if monitor is None:
                if child.poll() is not None:
                    break
                time.sleep(1)
                continue
            for _ in range(16):
                try:
                    chunk = os.read(monitor.stdout.fileno(), 65536)
                except BlockingIOError:
                    break
                if not chunk:
                    break
                lines = (buffer + chunk).split(b'\n')
                buffer = lines.pop()
                if dropping:
                    if not lines:
                        buffer = b''
                        continue
                    lines.pop(0)
                    dropping = False
                for line in lines:
                    if not line:
                        continue
                    try:
                        evidence = event_evidence(json.loads(line)) if len(line) <= 65536 else None
                    except (ValueError, TypeError):
                        evidence = None
                        emit(gap='invalid_event')
                    if evidence:
                        subscribed = True
                        start_seen |= evidence.get('action') == 'instance-started'
                        reason = evidence.get('shutdown_reason', reason)
                        emit(subscription='ready', **evidence)
                if len(buffer) > 65536:
                    buffer = b''
                    dropping = True
                    emit(gap='oversized_event')
            if child.poll() is not None:
                break  # Drain queued shutdown events before closing the monitor.
            if monitor.poll() is not None:
                stop_child(monitor)
                monitor.stdout.close()
                monitor = None
                emit(gap='monitor_exit')
                time.sleep(1)
                continue
            if time.monotonic() < next_sample:
                select.select([monitor.stdout], [], [], 1)
                continue
            next_sample = time.monotonic() + 5
            raw = probe('query', f'/1.0/instances/{INSTANCE}/state?project={PROJECT}')
            if raw:
                state = json.loads(raw)
                status = state.get('status')
                values = {'state': status if status in STATES else 'unknown'}
                for key, value in (('qemu_pid', state.get('pid')),
                                   ('memory_bytes', state.get('memory', {}).get('usage'))):
                    if type(value) is int and 0 <= value <= 2**63 - 1:
                        values[key] = value
                if status == 'Running' and not start_seen:
                    values['first_start_gap'] = 1
                emit(**values)
                console = probe('console', INSTANCE, '--project', PROJECT, '--show-log')
                if console:
                    for hint, marker in (('firmware', b'BdsDxe'), ('kernel', b'Linux version'),
                                         ('panic', b'Kernel panic'), ('oom', b'Out of memory'),
                                         ('agent', b'incus-agent')):
                        if marker in console and hint not in hints:
                            hints.add(hint)
                            emit(boot_hint=hint)
                elif console is None:
                    emit(gap='console_unavailable')
            else:
                emit(gap='instance_state_unavailable')
            memory = {}
            for line in Path('/proc/meminfo').read_text().splitlines():
                match = re.fullmatch(r'(MemAvailable|MemFree):\s+([0-9]+) kB', line)
                if match:
                    memory[match[1].lower() + '_bytes'] = int(match[2]) * 1024
            emit(**memory)
            select.select([monitor.stdout], [], [], 1)
    except (OSError, ValueError, TypeError, AttributeError):
        emit(gap='collection_unavailable')
    finally:
        if monitor is not None:
            stop_child(monitor)
            monitor.stdout.close()
        # Summary remains visible even if a noisy daemon exhausted the sample cap.
        rows = 0
        emit(observer='stopped' if child is not None and child.poll() is not None else 'deadline',
             subscription='ready' if subscribed else 'unconfirmed',
             shutdown_reason=reason)

        # Observation failure never substitutes for the real init result.
        try:
            if child is None:
                child = subprocess.Popen(command)
                for sig in (signal.SIGINT, signal.SIGTERM):
                    previous[sig] = signal.signal(sig, forward)
            code = child.wait()
            return code if code >= 0 else 128 - code
        finally:
            if child is not None and child.poll() is None:
                child.terminate()
                try:
                    child.wait(timeout=1)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait()
            for sig, handler in previous.items():
                signal.signal(sig, handler)


if __name__ == '__main__':
    if len(sys.argv) < 3 or sys.argv[1] != '--':
        sys.exit(2)
    sys.exit(run(sys.argv[2:]))
