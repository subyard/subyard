#!/usr/bin/env python3
"""Signal one pre-armed fixture requester on its native creation event."""
import argparse
import json
import os
from pathlib import Path
import select
import signal
import subprocess
import time
from urllib.parse import parse_qs, urlsplit


def process_start(pid):
    try:
        fields = Path('/proc', str(pid), 'stat').read_text().rsplit(') ', 1)[1].split()
        return fields[19] if fields[0] not in ('Z', 'X') else None
    except (OSError, IndexError):
        return None


def emit(stage, **fields):
    print(json.dumps(dict(stage=stage, **fields)), flush=True)


def observe(command, project, binding_path, timeout):
    try:
        monitor = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, bufsize=0)
    except OSError:
        emit('terminal', signalled=False, error_class='monitor_start_failed', native_exit_code=None)
        return 2
    counters = dict(events=0, blank_lines=0, project_mismatch=0, resource_mismatch=0, creating=0)
    binding = None
    ready = False
    signalled = False
    error = 'event_timeout'
    pending = b''
    deadline = time.monotonic() + timeout
    try:
        while time.monotonic() < deadline:
            if ready and binding is None and binding_path.exists():
                try:
                    candidate = json.loads(binding_path.read_text())
                    assert candidate['project'] == project and candidate['state'] == 'provisioning'
                    assert type(candidate['epoch']) is int and candidate['epoch'] > 0
                    assert type(candidate['generation']) is int and candidate['generation'] > 0
                    assert isinstance(candidate['worker_start'], str) and candidate['worker_start'].isdigit()
                    assert candidate['pid'] > 1 and candidate['worker_pid'] > 1
                    assert process_start(candidate['pid']) == candidate['start']
                    binding = candidate
                except (OSError, ValueError, KeyError, TypeError, AssertionError):
                    error = 'invalid_binding'
                    break
                emit('armed', epoch=binding['epoch'], generation=binding['generation'])
            readable, _, _ = select.select([monitor.stdout], [], [], .05)
            if not readable:
                continue
            chunk = os.read(monitor.stdout.fileno(), 4096)
            if not chunk:
                error = 'event_stream_ended'
                break
            pending += chunk
            if len(pending) > 131072:
                error = 'event_too_large'
                break
            while b'\n' in pending:
                raw, pending = pending.split(b'\n', 1)
                if not raw.strip():
                    counters['blank_lines'] += 1
                    continue
                try:
                    event = json.loads(raw)
                    operation = event.get('metadata', {})
                    assert isinstance(operation, dict)
                except (ValueError, AttributeError, AssertionError):
                    error = 'invalid_event'
                    return 2
                counters['events'] += 1
                if event.get('type') != 'operation':
                    continue
                if event.get('project') != project:
                    counters['project_mismatch'] += 1
                    continue
                if not ready and operation.get('class') == 'websocket':
                    ready = True
                    emit('ready')
                if operation.get('class') != 'task' or operation.get('description') != 'Creating instance' or not 100 <= operation.get('status_code', 999) < 200:
                    continue
                counters['creating'] += 1
                resources = operation.get('resources', {}).get('instances', [])
                names = [urlsplit(value).path for value in resources]
                if not names or any(name not in ('/1.0/instances/e2e-vm-1', '/1.0/instances/e2e-vm-2') for name in names):
                    counters['resource_mismatch'] += 1
                    continue
                if any(parse_qs(urlsplit(value).query).get('project', [project]) != [project] for value in resources):
                    counters['resource_mismatch'] += 1
                    continue
                if binding is None:
                    error = 'creation_before_arm'
                    return 2
                # The exact local process and server allocation were captured while
                # the lifecycle lock prevented creation. No RPC or live-slot read
                # belongs between this native event and the signal.
                if process_start(binding['pid']) != binding['start']:
                    error = 'requester_identity_lost'
                    return 2
                os.kill(binding['pid'], signal.SIGKILL)
                signalled = True
                emit('creating', epoch=binding['epoch'], generation=binding['generation'],
                     worker_pid=binding['worker_pid'], worker_start=binding['worker_start'],
                     native_status=operation['status_code'])
                return 0
        return 2
    except InterruptedError:
        error = 'observer_interrupted'
        return 2
    except (OSError, TypeError, KeyError, AttributeError, ValueError):
        error = 'observer_error'
        return 2
    finally:
        native_exit = monitor.poll()
        if native_exit is None and error == 'event_stream_ended':
            try:
                native_exit = monitor.wait(timeout=.2)
            except subprocess.TimeoutExpired:
                pass
        if native_exit is None:
            monitor.terminate()
            try:
                native_exit = monitor.wait(timeout=5)
            except subprocess.TimeoutExpired:
                monitor.kill()
                native_exit = monitor.wait()
        emit('terminal', signalled=signalled, error_class=None if signalled else error,
             native_exit_code=native_exit, **counters)


def interrupted(*_):
    raise InterruptedError()


def main():
    signal.signal(signal.SIGTERM, interrupted)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--project', required=True)
    parser.add_argument('--binding', type=Path, required=True)
    parser.add_argument('--timeout', type=float, default=180)
    parser.add_argument('command', nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command[1:] if args.command[:1] == ['--'] else args.command
    if not command or args.timeout <= 0:
        parser.error('native monitor command and positive timeout required')
    return observe(command, args.project, args.binding, args.timeout)


if __name__ == '__main__':
    raise SystemExit(main())
