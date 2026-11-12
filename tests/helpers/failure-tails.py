#!/usr/bin/env python3
"""Tail top-level fixture diagnostics; never descend into config/home/runtime trees."""
import os
from pathlib import Path
import stat
import sys

root = Path(sys.argv[1])
flags = os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK
try:
    assert not root.is_symlink()
    with os.fdopen(os.open(root/'.marker', flags), 'rb') as marker:
        assert stat.S_ISREG(os.fstat(marker.fileno()).st_mode) and marker.read(4096) == (sys.argv[2]+'\n').encode()
except (OSError, AssertionError):
    sys.exit('failure-tails: refusing unowned state directory')
remaining, total_marker = 32768-18, b'[total truncated]\n'
entries = []
for entry in os.scandir(root):
    try:
        info = entry.stat(follow_symlinks=False)
        if entry.name.endswith(('.log', '.stderr', '.json')) and stat.S_ISREG(info.st_mode):
            entries.append((-info.st_mtime_ns, entry.name, entry.path))
    except OSError: continue
for _, name, path in sorted(entries):
    try:
        with os.fdopen(os.open(path, flags), 'rb') as stream:
            info = os.fstat(stream.fileno())
            if not stat.S_ISREG(info.st_mode): continue
            header = f'--- {name!r} ---\n'.encode()
            limit = min(4096, remaining-len(header)-13)
            if limit <= 0:
                sys.stderr.buffer.write(total_marker)
                break
            stream.seek(max(0, info.st_size-limit))
            output = header+stream.read(limit)+(b'\n[truncated]\n' if info.st_size > limit else b'\n')
            sys.stderr.buffer.write(output)
            remaining -= len(output)
    except OSError:
        continue
