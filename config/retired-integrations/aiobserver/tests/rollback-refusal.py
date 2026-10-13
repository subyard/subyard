#!/usr/bin/env python3
"""Assert the actual candidate's irreversible reader boundary without mutation."""
import hashlib
import os
from pathlib import Path
import stat
import subprocess
import sys


def snapshot(config, runtime):
    facts = []
    for path in sorted(config.rglob('*')):
        info = path.lstat()
        value = os.readlink(path) if stat.S_ISLNK(info.st_mode) else (
            hashlib.sha256(path.read_bytes()).hexdigest() if stat.S_ISREG(info.st_mode) else '')
        if not (stat.S_ISLNK(info.st_mode) or stat.S_ISREG(info.st_mode) or stat.S_ISDIR(info.st_mode)):
            raise ValueError('unsupported protected config entry')
        facts.append((str(path.relative_to(config)), info.st_mode, info.st_uid, info.st_gid, value))
    return facts, tuple(os.readlink(runtime/name) for name in ('current', 'previous'))


def verify(config, runtime, command):
    before = snapshot(config, runtime)
    result = subprocess.run(command, capture_output=True, timeout=180)
    if result.returncode == 0 or b'code=rollback-incompatible' not in result.stdout+result.stderr:
        raise ValueError('missing typed incompatible rollback refusal')
    if snapshot(config, runtime) != before:
        raise ValueError('incompatible rollback changed protected state')


if __name__ == '__main__':
    try:
        config, runtime, *command = sys.argv[1:]
        if not command: raise ValueError('rollback command required')
        verify(Path(config), Path(runtime), command)
        print('ok: actual candidate refuses epoch-1 rollback without mutation')
    except (OSError, ValueError, subprocess.SubprocessError):
        print('retirement: incompatible rollback verification failed', file=sys.stderr)
        sys.exit(1)
