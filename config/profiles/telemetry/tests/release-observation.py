"""Exact absent local-collector observation for generic released-updater checks."""
import ast


def read_only_commands(package):
    tree = ast.parse((package/'owner.py').read_text())
    script = next(ast.literal_eval(node.value) for node in tree.body
                  if isinstance(node, ast.Assign) and any(isinstance(target, ast.Name) and target.id == 'GUEST_SCRIPT' for target in node.targets))
    return [(['exec', 'yard', '--project', 'subyard', '--', 'bash', '-euo', 'pipefail', '-c',
              script, 'subyard', 'observe', '0', 'dev', agents], '{"state": "absent", "actual": "", "desired": ""}\n')
            for agents in ('', 'claude', 'codex', 'claude codex')]
