"""Exact absent-package observation for the released-updater fixture."""
import ast
import shlex


def read_only_commands(package):
    if (package / 'dashboard.py').is_file():
        tree = ast.parse((package / 'dashboard.py').read_text())
        script = next(ast.literal_eval(node.value) for node in tree.body
                      if isinstance(node, ast.Assign) and any(isinstance(target, ast.Name) and target.id == 'GUEST_SCRIPT' for target in node.targets))
    else:
        # Released owners kept the exact observe transport in their shell owner.
        owner = (package / 'owner.sh').read_text()
        invocation = owner.split('incus exec ', 1)[1].split('\n\n', 1)[0].replace('\\\n', '')
        arguments = shlex.split(invocation)
        assert arguments[:8] == ['$YARD_INSTANCE_NAME', '--project', '$INCUS_PROJECT',
                                 '--', 'bash', '-euo', 'pipefail', '-c']
        assert arguments[9:13] == ['subyard', '$mode', '$selected', '$DEV_USER']
        script = arguments[8]
    command = ["exec", "yard", "--project", "subyard", "--",
               "bash", "-euo", "pipefail", "-c", script, "subyard", "observe", "0", "dev"]
    return [(command + [agents], '{"state":"absent","actual":"","desired":""}\n')
            for agents in ("", "claude", "codex", "claude codex")]
