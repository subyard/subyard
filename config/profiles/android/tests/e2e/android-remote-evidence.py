#!/usr/bin/env python3
"""Bounded remote-fixture diagnostics containing only closed labels and exit codes."""
import json
from pathlib import Path
import re
import sys

PREFIX = 'android-remote-evidence '
STAGES = {'arguments', 'preflight', 'owner-config', 'controller-bootstrap', 'payload', 'ssh', 'owner-catalog',
          'remote-catalog', 'owner-status', 'remote-status', 'compare', 'viewer', 'run',
          'run-output', 'after-status', 'after-compare', 'owner-unchanged', 'complete', 'unknown'}
MODES = {'read', 'run', 'view', 'unknown'}
ERRORS = {
    'ssh-command-refused': rb'android-pool-remote: rejected SSH command',
    'owner-environment': rb'remote payload did not execute with owner configuration',
    'owner-adb': rb'remote owner ADB did not report SDK 36',
    'shared-library': rb'error while loading shared libraries',
    'release-readiness': rb'release transition|dependency-unavailable',
    'ssh-transport': rb'Connection refused|Permission denied \(publickey\)|Host key verification failed',
    'confirmation': rb'confirmation required|non-interactive',
}


def tail(path):
    try:
        with Path(path).open('rb') as stream:
            stream.seek(0, 2)
            stream.seek(max(0, stream.tell() - 16384))
            return stream.read(16384)
    except OSError:
        return b''


def code(value):
    try:
        number = int(value)
        return number if 0 <= number <= 255 else None
    except (TypeError, ValueError):
        return None


def record(stage, mode, native_exit, command_exit, work):
    data = b''
    if work:
        data = b'\n'.join(tail(Path(work) / name) for name in
            ('controller-bootstrap.err', 'remote-catalog.err', 'remote-status.err', 'remote-run.err', 'remote-after.err', 'sshd.log'))
    return dict(stage=stage if stage in STAGES else 'unknown', mode=mode if mode in MODES else 'unknown',
                native_exit=code(native_exit), command_exit=code(command_exit),
                error=next((name for name, pattern in ERRORS.items() if re.search(pattern, data)), 'unknown'))


def summary(path, outer_exit):
    result = record('unknown', 'unknown', None, None, '')
    cleanup_exit = None
    for line in tail(path).decode('utf-8', 'replace').splitlines():
        if line.startswith(PREFIX):
            try:
                value = json.loads(line[len(PREFIX):])
                if (set(value) == set(result) and value['stage'] in STAGES and value['mode'] in MODES
                        and value['error'] in {*ERRORS, 'unknown'}
                        and all(value[key] is None or type(value[key]) is int and 0 <= value[key] <= 255
                                for key in ('native_exit', 'command_exit'))):
                    result = value
            except (ValueError, TypeError, KeyError):
                pass
        match = re.fullmatch(r'android-remote-cleanup exit_code=(\d{1,3})', line)
        if match:
            cleanup_exit = code(match[1])
    return dict(result, outer_exit=code(outer_exit), cleanup_exit=cleanup_exit)


if __name__ == '__main__':
    action, *arguments = sys.argv[1:]
    value = record(*arguments) if action == 'record' else summary(*arguments)
    print(PREFIX + json.dumps(value, sort_keys=True))
