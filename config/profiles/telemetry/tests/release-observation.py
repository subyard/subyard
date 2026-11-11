"""Exact absent local-collector observation for generic released-updater checks."""
import ast
from itertools import combinations


def read_only_commands(package):
    tree = ast.parse((package/'owner.py').read_text())
    script = next(ast.literal_eval(node.value) for node in tree.body
                  if isinstance(node, ast.Assign) and any(isinstance(target, ast.Name) and target.id == 'GUEST_SCRIPT' for target in node.targets))
    managed_binding = any(isinstance(node, ast.Constant) and node.value == 'managed=' for node in ast.walk(tree))
    commands = []
    for agents in ('', 'claude', 'codex', 'claude codex'):
        names = agents.split()
        selections = [subset for count in range(len(names)+1) for subset in combinations(names, count)] if managed_binding else [None]
        for managed in selections:
            arguments = ['exec', 'yard', '--project', 'subyard', '--', 'bash', '-euo', 'pipefail', '-c',
                         script, 'subyard', 'observe', '0', 'dev', agents]
            if managed is not None:
                arguments.append('managed='+','.join(managed))
            commands.append((arguments, '{"state": "absent", "actual": "", "desired": ""}\n'))
    return commands
