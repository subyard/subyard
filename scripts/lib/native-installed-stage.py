#!/usr/bin/env python3
"""Close only the exact fixed installed-runtime stage after its wrapper drains."""
import json
import os
from pathlib import Path
import re
import shutil
import stat
import sys


def finish(stage, binding, recipe, owner):
    if owner not in ('core-guest-runtime', 'key-tools-runtime', 'host-power', 'test-vms-host-sink', 'materialized-integrations') or not all(re.fullmatch('[a-f0-9]{64}', value) for value in (binding, recipe)):
        raise ValueError('invalid installed stage binding')
    parent = Path('/tmp')/('subyard-native-installed-'+str(os.getuid()))
    path = parent/(binding+'-'+owner)
    if str(path) != stage:
        raise ValueError('installed stage path does not match binding')
    parent_info, info = parent.lstat(), path.lstat()
    if not stat.S_ISDIR(parent_info.st_mode) or parent_info.st_uid != os.getuid() or stat.S_IMODE(parent_info.st_mode) != 0o700 or not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o500:
        raise ValueError('unsafe installed stage directory')
    fd = os.open(path/'.stage.json', os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, 'rb') as stream:
        marker_info = os.fstat(stream.fileno())
        if not stat.S_ISREG(marker_info.st_mode) or marker_info.st_uid != os.getuid() or marker_info.st_nlink != 1 or stat.S_IMODE(marker_info.st_mode) != 0o600 or marker_info.st_size > 1024:
            raise ValueError('unsafe installed stage marker')
        raw = stream.read(1025)
    marker = json.loads(raw)
    expected = {'version': 1, 'binding': binding, 'recipe': recipe, 'owner': owner,
                'device': info.st_dev, 'inode': info.st_ino, 'parent': marker.get('parent')}
    if marker != expected or not isinstance(marker['parent'], int) or marker['parent'] <= 0 or raw != (json.dumps(expected, separators=(',', ':'))+'\n').encode():
        raise ValueError('installed stage marker changed')
    # A live helper still needs the tree for its separate final verification.
    # PID reuse conservatively retains it; absence can only select cleanup.
    try:
        os.kill(marker['parent'], 0)
        return
    except ProcessLookupError:
        pass
    count, size = 0, 0
    for root, directories, files in os.walk(path, followlinks=False):
        for name in directories+files:
            child = Path(root)/name
            child_info = child.lstat()
            if child_info.st_uid != os.getuid() or stat.S_ISLNK(child_info.st_mode) or not (stat.S_ISDIR(child_info.st_mode) or stat.S_ISREG(child_info.st_mode)):
                raise ValueError('unsafe installed stage member')
            count += 1
            size += child_info.st_size if stat.S_ISREG(child_info.st_mode) else 0
            if count > 8192 or size > (64 << 20)+1024:
                raise ValueError('installed stage bound')
    for root, directories, files in os.walk(path, followlinks=False):
        os.chmod(root, 0o700)
    current = path.lstat()
    if (current.st_dev, current.st_ino) != (info.st_dev, info.st_ino):
        raise ValueError('installed stage identity changed')
    shutil.rmtree(path)


if __name__ == '__main__':
    if len(sys.argv) != 6 or sys.argv[1] != 'finish':
        raise ValueError('invalid installed stage action')
    finish(*sys.argv[2:])
