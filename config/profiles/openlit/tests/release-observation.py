"""Exact absent-package observation for the released-updater fixture."""
import ast


def read_only_commands(package):
    tree = ast.parse((package / 'dashboard.py').read_text())
    script = next(ast.literal_eval(node.value) for node in tree.body
                  if isinstance(node, ast.Assign) and any(isinstance(target, ast.Name) and target.id == 'GUEST_SCRIPT' for target in node.targets))
    command = ["exec", "yard", "--project", "subyard", "--",
               "bash", "-euo", "pipefail", "-c", script, "subyard", "observe", "0", "dev", "codex"]
    return [(command, '{"state":"absent","actual":"","desired":""}\n')]
