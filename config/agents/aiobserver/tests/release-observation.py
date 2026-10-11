"""Exact absent-package observations for the released-updater fixture."""
import ast


def read_only_commands(package):
    # Bind the canned response to the actual owning assessment, not any shell.
    source = ast.parse((package / "lifecycle.py").read_text())
    assessment = next(ast.literal_eval(node.value) for node in source.body
                      if isinstance(node, ast.Assign)
                      and any(isinstance(target, ast.Name) and target.id == "CONTROL_ASSESSMENT"
                              for target in node.targets))
    guest = ["exec", "yard", "--project", "subyard", "--"]
    return [(guest + ["timeout", "10", "docker", "container", "ls", "-a", "--format", "{{.Names}}"], ""),
            (guest + ["sh", "-eu", "-c", assessment], "")]
