"""Exact absent-runtime observation for the released-updater fixture."""
import hashlib
import shlex


def read_only_commands(package):
    owner = (package / 'owner.sh').read_text()
    script, = shlex.split(owner.split('-- bash -euo pipefail -c ', 1)[1]
                         .split(' subyard "$mode"', 1)[0])
    files = ('profile.json', 'runtime.sh', 'owner.sh', 'guest.py', 'fetch.py',
             'subyard-git-fetch.service.in', 'subyard-git-fetch.timer')
    source = ''.join(hashlib.sha256((package / name).read_bytes()).hexdigest() + '\n'
                     for name in files).encode()
    binding = hashlib.sha256(source + b'default\0subyard\0yard\0dev\0').hexdigest()
    command = ['exec', 'yard', '--project', 'subyard', '--', 'bash', '-euo', 'pipefail',
               '-c', script, 'subyard', 'observe', '0', 'dev', '0', binding]
    return [(command, '{"state":"absent"}\n')]
