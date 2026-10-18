"""Exact absent GitHub runtime observations for released-updater fixtures."""
import ast
import shlex


def read_only_commands(package, *, project="subyard", instance="yard", user="dev"):
    if not (package / "runtime.py").is_file():
        return []
    tree = ast.parse((package / 'runtime.py').read_text())
    probe = next(ast.literal_eval(node.value) for node in ast.walk(tree)
                 if isinstance(node, ast.Assign) and any(isinstance(target, ast.Name)
                     and target.id == 'probe' for target in node.targets))
    prefix = ['exec', instance, '--project', project, '--']
    owner = (package / 'owner.sh').read_text()
    invocation = owner.split('| incus exec ', 1)[1].split('\n}', 1)[0].replace('\\\n', '')
    substitutions = {'$YARD_INSTANCE_NAME': instance, '$INCUS_PROJECT': project,
                     'DEV_USER=$DEV_USER': 'DEV_USER='+user,
                     'GITHUB_GIT_MODE=${GITHUB_GIT_MODE:-off}': 'GITHUB_GIT_MODE=off', '$@': '--check-removed'}
    removed = ['exec'] + [substitutions.get(word, word) for word in shlex.split(invocation)]
    assert removed[:13] == ['exec', instance, '--project', project, '--env',
                            'DEV_USER='+user, '--env', 'GITHUB_GIT_MODE=off', '--', 'bash', '-euo', 'pipefail', '-c']
    assert removed[14:] == ['subyard', '--check-removed']
    return [(prefix + ['getent', 'passwd', user], f'{user}:x:1000:1000::/home/{user}:/bin/bash\n'),
            (removed, ''),
            (prefix + ['sh', '-c', probe, 'subyard', user], ''),
            (prefix + ['timeout', '--kill-after=1', '2', 'runuser', '-u', user, '--',
                       '/usr/local/bin/subyard-github', 'status', '--runtime'], '', 127)]


def read_only_host_commands(package, *, yard="default"):
    if not (package / "runtime.py").is_file():
        return []
    tree = ast.parse((package / 'runtime.py').read_text())
    unit = next(node.value.right for node in ast.walk(tree) if isinstance(node, ast.Assign)
                and any(isinstance(target, ast.Name) and target.id == 'unit' for target in node.targets))
    assert isinstance(unit, ast.JoinedStr)
    name = ''.join(value.value if isinstance(value, ast.Constant) else yard for value in unit.values)
    manager = next(node.args[0] for node in ast.walk(tree) if isinstance(node, ast.Call)
                   and node.args and isinstance(node.args[0], ast.List)
                   and isinstance(node.args[0].elts[0], ast.Constant)
                   and node.args[0].elts[0].value == 'systemctl')
    arguments = [name if isinstance(value, ast.Attribute) else ast.literal_eval(value)
                 for value in manager.elts[1:]]
    return [('systemctl', arguments,
             'ActiveState=inactive\nUnitFileState=\nMainPID=0\nLoadState=not-found\n', 4)]
