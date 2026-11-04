"""Exact read-only empty predecessor retirement fixture."""
import ast
import json


def read_only_commands(package):
    tree = ast.parse((package / 'retire.py').read_text())
    guest = next(ast.literal_eval(node.value) for node in tree.body
                 if isinstance(node, ast.Assign) and any(isinstance(target, ast.Name) and target.id == 'GUEST' for target in node.targets))
    facts = {'remove': [], 'worktrees': False, 'service': False}
    prefix = ['exec', 'yard', '--project', 'subyard', '--', 'python3', '-B', '-c', guest]
    return [(prefix + ['observe', 'dev'], json.dumps(facts, sort_keys=True, separators=(',', ':')) + '\n')]
