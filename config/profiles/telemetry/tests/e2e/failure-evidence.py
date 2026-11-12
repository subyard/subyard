#!/usr/bin/env python3
"""Send fixture-owned failure tails to the controller before disposable cleanup."""
import json
import os
from pathlib import Path
import stat

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
