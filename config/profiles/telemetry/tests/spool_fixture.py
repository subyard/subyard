import json
import subprocess

import records


def prepare(root):
    root = records.private_directory(root)
    subprocess.run([records.collector_binary(), 'prepare', '--data-dir', root], check=True)
    return root


def spooled(root):
    for path in sorted((root/'spool').glob('*')):
        if path.suffix not in ('.open', '.sealed'):
            continue
        frames = path.read_bytes().split(b'\n')[1:-1]
        yield from (json.loads(frame) for frame in frames)
