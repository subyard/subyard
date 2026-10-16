"""Exact absent GitHub runtime observations for released-updater fixtures."""
import ast
import shlex


def read_only_commands(package):
    tree = ast.parse((package / 'runtime.py').read_text())
    probe = next(ast.literal_eval(node.value) for node in ast.walk(tree)
                 if isinstance(node, ast.Assign) and any(isinstance(target, ast.Name)
                     and target.id == 'probe' for target in node.targets))
    prefix = ['exec', 'yard', '--project', 'subyard', '--']
    owner = (package / 'owner.sh').read_text()
    invocation = owner.split('| incus exec ', 1)[1].split('\n}', 1)[0].replace('\\\n', '')
    substitutions = {'$YARD_INSTANCE_NAME': 'yard', '$INCUS_PROJECT': 'subyard',
                     'DEV_USER=$DEV_USER': 'DEV_USER=dev', '$@': '--check-removed'}
    removed = ['exec'] + [substitutions.get(word, word) for word in shlex.split(invocation)]
    assert removed[:11] == ['exec', 'yard', '--project', 'subyard', '--env',
                            'DEV_USER=dev', '--', 'bash', '-euo', 'pipefail', '-c']
    assert removed[12:] == ['subyard', '--check-removed']
    return [(prefix + ['getent', 'passwd', 'dev'], 'dev:x:1000:1000::/home/dev:/bin/bash\n'),
            (removed, ''),
            (prefix + ['sh', '-c', probe, 'subyard', 'dev'], ''),
            (prefix + ['timeout', '--kill-after=1', '2', 'runuser', '-u', 'dev', '--',
                       '/usr/local/bin/subyard-github', 'status', '--runtime'], '', 127)]


def read_only_host_commands(package):
    tree = ast.parse((package / 'runtime.py').read_text())
    unit = next(node.value.right for node in ast.walk(tree) if isinstance(node, ast.Assign)
                and any(isinstance(target, ast.Name) and target.id == 'unit' for target in node.targets))
    assert isinstance(unit, ast.JoinedStr)
    name = ''.join(value.value if isinstance(value, ast.Constant) else 'default' for value in unit.values)
    manager = next(node.args[0] for node in ast.walk(tree) if isinstance(node, ast.Call)
                   and node.args and isinstance(node.args[0], ast.List)
                   and isinstance(node.args[0].elts[0], ast.Constant)
                   and node.args[0].elts[0].value == 'systemctl')
    arguments = [name if isinstance(value, ast.Attribute) else ast.literal_eval(value)
                 for value in manager.elts[1:]]
    return [('systemctl', arguments,
             'ActiveState=inactive\nUnitFileState=\nMainPID=0\nLoadState=not-found\n', 4)]
