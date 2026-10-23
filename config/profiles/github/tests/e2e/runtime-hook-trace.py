#!/usr/bin/python3
"""Fixture Bash wrapper: measure one authenticated observer without changing it."""
import hashlib
import json
import os
from pathlib import Path
import signal
import stat
import subprocess
import sys
import time


REASONS = {
    'unsafe managed parent', 'unsafe managed path', 'unsafe managed file',
    'invalid developer account', 'unsupported developer home', 'unsupported developer group',
    'unsafe runtime directory', 'foreign runtime directory', 'foreign service unit',
    'foreign loaded service unit', 'runtime directory appeared during publication',
    'runtime verification failed', 'system manager operation failed',
    'unsafe lifecycle lock', 'unsafe or replaced lifecycle lock',
    'native custody generation changed', 'native custody changed before effect',
    'native operation timed out', 'native filesystem operation failed',
    'profile request could not be verified',
}
PUBLIC_LINES = {('git-fetch: '+reason+'; inspect owned timer state').encode(): reason
                for reason in REASONS}


def record(value):
    """Capture failures are separate from the original command's result."""
    try:
        data = (json.dumps(dict(pid=os.getpid(), **value), sort_keys=True)+'\n').encode()
        fd = os.open(os.environ['SUBYARD_TRACE_HOOK_LOG'],
                     os.O_WRONLY | os.O_APPEND | os.O_CREAT | os.O_CLOEXEC | os.O_NOFOLLOW, 0o600)
        try:
            info = os.fstat(fd)
            if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                    or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600):
                return
            os.write(fd, data)
        finally:
            os.close(fd)
    except (OSError, KeyError, ValueError):
        pass


def main():
    args = sys.argv[1:]
    if len(args) == 2 and args[0] == '--verify':
        try:
            records = [json.loads(line) for line in Path(args[1]).read_text().splitlines()]
        except FileNotFoundError:
            records = []
        starts = {item['pid'] for item in records if item['kind'] == 'start'}
        ends = {item['pid'] for item in records if item['kind'] == 'end'}
        if (ends-starts or any(item['kind'] == 'authentication-failed' for item in records)
                or any(item['kind'] not in ('start', 'end') for item in records)):
            raise ValueError('missing authenticated native hook observations')
        # SIGKILL at the existing outer deadline cannot emit an end record.
        # No event proves only that this diagnostic did not observe the hook.
        # The native command owns readiness/status, and may not launch a hook.
        print(json.dumps({'status': 'observed' if starts else 'not-observed',
                          'observations': len(starts), 'completed': len(ends),
                          'incomplete': len(starts-ends)}, sort_keys=True))
        return
    real = os.environ['SUBYARD_TRACE_REAL_BASH']
    source = Path(os.environ['SUBYARD_TRACE_HOOK_SOURCE']).resolve()
    if len(args) != 2 or args[1] != 'observe' or Path(args[0]).resolve() != source:
        os.execv(real, [real, *args])
    expected = os.environ['SUBYARD_TRACE_HOOK_SHA256']
    if hashlib.sha256(source.read_bytes()).hexdigest() != expected:
        record({'kind': 'authentication-failed'})
        os.execv(real, [real, *args])
    start = time.monotonic()
    record({'kind': 'start', 'monotonic_ns': time.monotonic_ns()})
    child = subprocess.Popen([real, *args], stderr=subprocess.PIPE)
    # Forward every byte unchanged. Retain only a bounded current line in memory;
    # output, arguments, paths, environment and credential payloads never enter the log.
    pending = b''
    dropped = False
    byte_count = 0
    reasons = set()
    while True:
        block = child.stderr.read1(4096)
        if not block:
            break
        os.write(2, block)
        byte_count += len(block)
        for index, part in enumerate(block.split(b'\n')):
            if index:
                if not dropped and pending in PUBLIC_LINES:
                    reasons.add(PUBLIC_LINES[pending])
                pending, dropped = b'', False
            if len(pending)+len(part) <= 256 and not dropped:
                pending += part
            else:
                pending, dropped = b'', True
    if not dropped and pending in PUBLIC_LINES:
        reasons.add(PUBLIC_LINES[pending])
    code = child.wait()
    record({'kind': 'end', 'exit_code': code,
            'duration_ms': round((time.monotonic()-start)*1000),
            'stderr_bytes': byte_count, 'reasons': sorted(reasons)})
    if code < 0:
        if -code not in (signal.SIGKILL, signal.SIGSTOP):
            signal.signal(-code, signal.SIG_DFL)
        os.kill(os.getpid(), -code)
    raise SystemExit(code)


if __name__ == '__main__':
    main()
