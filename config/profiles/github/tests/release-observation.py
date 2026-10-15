"""Exact absent guest/client and host-service observations for updater fixtures."""
import ast
import shlex


def read_only_commands(package):
    owner = (package / 'owner.sh').read_text()
    script, = shlex.split(owner.split('-- bash -euo pipefail -c ', 1)[1]
                         .split(' subyard "$@"', 1)[0])
    tree = ast.parse((package / 'runtime.py').read_text())
    probe = next(ast.literal_eval(node.value) for node in ast.walk(tree)
                 if isinstance(node, ast.Assign)
                 and any(isinstance(target, ast.Name) and target.id == 'probe' for target in node.targets))
    guest = ['exec', 'yard', '--project', 'subyard']
    return [
        (guest + ['--', 'getent', 'passwd', 'dev'], 'dev:x:1000:1000::/home/dev:/bin/bash\n'),
        (guest + ['--env', 'DEV_USER=dev', '--', 'bash', '-euo', 'pipefail', '-c', script,
                  'subyard', '--check-removed'], ''),
        (guest + ['--', 'sh', '-c', probe, 'subyard', 'dev'], ''),
        (guest + ['--', 'timeout', '--kill-after=1', '2', 'runuser', '-u', 'dev', '--',
                  '/usr/local/bin/subyard-github', 'status', '--runtime'], ('', 127)),
    ]


def read_only_systemctl_commands(package):
    arguments = ['--user', 'show', 'subyard-github-default.service',
                 '--property=ActiveState,UnitFileState,MainPID,LoadState']
    return [(arguments, ('ActiveState=inactive\nUnitFileState=\nMainPID=0\nLoadState=not-found\n', 4))]
