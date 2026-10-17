"""Exact absent-runtime observation for the released-updater fixture."""
import hashlib
import shlex


def read_only_commands(package):
    owner = (package / 'owner.sh').read_text()
    invocation = owner.split('| incus exec ', 1)[1].split(')" || rc=$?', 1)[0]
    arguments = shlex.split(invocation)
    assert arguments[:8] == ['$YARD_INSTANCE_NAME', '--project', '$INCUS_PROJECT',
                             '--', 'bash', '-euo', 'pipefail', '-c']
    assert arguments[9:] == ['subyard', '$mode', '$selected', '$DEV_USER',
                             '${SUBYARD_PROFILE_STOPPED:-0}', '${extra[@]}']
    files = ('profile.json', 'runtime.sh', 'owner.sh', 'guest.py', 'fetch.py',
             'subyard-git-fetch.service.in', 'subyard-git-fetch.timer')
    payload = ''.join(hashlib.sha256((package / name).read_bytes()).hexdigest() + '\n'
                      for name in files).encode() + b'default\0subyard\0yard\0dev\0'
    binding = hashlib.sha256(payload).hexdigest()
    command = ['exec', 'yard', '--project', 'subyard', '--', 'bash', '-euo',
               'pipefail', '-c', arguments[8], 'subyard', 'observe', '0', 'dev', '0', binding]
    return [(command, '{"state":"absent"}\n')]
