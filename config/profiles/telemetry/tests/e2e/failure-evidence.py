#!/usr/bin/env python3
"""Send fixture-owned failure tails to the controller before disposable cleanup."""
from contextlib import redirect_stdout
import io
import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile

LIMIT = 8192
PREFIX = 'telemetry-failure-evidence: '


def emit(phase, files, secrets=(), **status):
    masks = [secret.encode() for secret in secrets if secret]
    bound = LIMIT + max(map(len, masks), default=0)
    tails = {}
    for name, value in files.items():
        try:
            if isinstance(value, Path):
                fd = os.open(value, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
                with os.fdopen(fd, 'rb') as stream:
                    info = os.fstat(stream.fileno())
                    if not stat.S_ISREG(info.st_mode):
                        raise ValueError('regular fixture file required')
                    size = info.st_size
                    stream.seek(max(0, size-bound))
                    raw = stream.read(bound)
            else:
                raw = value or b''
                size = len(raw)
                raw = raw[-bound:]
            for secret in masks:
                raw = raw.replace(secret, b'<redacted>')
            tails[name] = dict(text=raw[-LIMIT:].decode('utf-8', errors='replace'), truncated=size > LIMIT)
        except (OSError, ValueError) as error:
            tails[name] = dict(error=type(error).__name__)
    print(PREFIX+json.dumps(dict(phase=phase, files=tails, **status)), flush=True)


def relay(output):
    for line in (output or b'').splitlines():
        if line.startswith(PREFIX.encode()) and len(line) <= 262144:
            print(line.decode('utf-8', errors='replace'), flush=True)


def retain(root, phase, files, secrets=(), error=None):
    assert not root.is_symlink() and (root/'marker').read_bytes() == b'subyard-telemetry-delegated-v1'
    if error is not None:
        files = dict(files, exception=str(error).encode())
    with redirect_stdout(io.StringIO()) as output:
        emit(phase, files, secrets=secrets, error_class=type(error).__name__ if error else None,
             return_code=getattr(error, 'returncode', None))
    path = root/(phase+'.tmp')
    with os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600), 'w') as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(output.getvalue())
    os.replace(path, root/phase)


def run(command, root, secrets=(), **kwargs):
    # Spool child streams outside Python's redirect_stdout; retain only masked tails.
    with tempfile.TemporaryDirectory(dir=root) as temporary:
        paths = {name: Path(temporary)/name for name in ('stdout', 'stderr')}
        with paths['stdout'].open('xb') as stdout, paths['stderr'].open('xb') as stderr:
            try:
                return subprocess.run(command, stdout=stdout, stderr=stderr, **kwargs)
            except (subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
                retain(root, 'native-command', dict(paths, command=json.dumps(command).encode()), secrets, error)
                raise
