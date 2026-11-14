#!/usr/bin/env python3
"""Run the retirement payloads' real guest removal checks against stubbed guests."""
import pathlib
import subprocess
import tempfile

ROOT = pathlib.Path(__file__).resolve().parents[4]
CASES = {
    'aiobserver': ('/etc/subyard/ai-observer/managed', 'subyard-ai-observer.service'),
    'paseo': ('/usr/local/bin/paseo-check', 'paseo.service'),
}


def guest_check(name):
    source = (ROOT / f'config/retired-integrations/{name}/tests/e2e/owner.sh').read_text()
    body = source.split('guest_yard "$name" sh -eu -c \'\n', 2)
    # The removal check is the first per-yard guest script after seeding.
    script = [part for part in body[1:] if 'systemctl is-active --quiet' in part.split("'", 1)[0]][0]
    return script.split("'", 1)[0]


for name, (path, unit) in CASES.items():
    check = guest_check(name)
    for fault in ('', 'active', 'file'):
        with tempfile.TemporaryDirectory(prefix='subyard-retirement-payload-', dir='/tmp') as directory:
            tools = pathlib.Path(directory)
            (tools / 'systemctl').write_text('#!/bin/sh\n' + ('exit 0\n' if fault == 'active' else 'exit 3\n'))
            (tools / 'docker').write_text('#!/bin/sh\nprintf "other\\n"\n')
            for tool in ('systemctl', 'docker'):
                (tools / tool).chmod(0o755)
            # Map absolute guest paths under the stub root; only the chosen path exists.
            guest = check.replace(' /', f' {directory}/root/')
            if fault == 'file':
                target = pathlib.Path(f'{directory}/root{path}')
                target.parent.mkdir(parents=True)
                target.write_text('left behind\n')
            result = subprocess.run(['sh', '-eu', '-c', guest], capture_output=True, text=True, timeout=10,
                                    env={'PATH': f'{tools}:/usr/bin:/bin'})
            assert (result.returncode == 0) == (fault == ''), (name, fault, unit, result)
print('retirement payload guest checks fail on an active unit or any remaining control')
