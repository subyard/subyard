"""Exact read-only empty predecessor retirement fixture."""
import ast


def read_only_commands(package):
    tree = ast.parse((package / 'retire.py').read_text())
    control = next(ast.literal_eval(node.value) for node in tree.body
                   if isinstance(node, ast.Assign) and any(isinstance(target, ast.Name) and target.id == 'CONTROL_ASSESSMENT' for target in node.targets))
    # Released agents used the same closed control probe before the retirement
    # receipt existed. Keep that exact read-only grammar with this owner.
    legacy = control[:control.index("parent=/var/lib/subyard")] + "if" + control.split("\nelif", 1)[1]
    prefix = ['exec', 'yard', '--project', 'subyard', '--']
    return [(prefix+['sh','-eu','-c',control], ''),
            (prefix+['sh','-eu','-c',legacy], ''),
            (prefix+['timeout','10','docker','container','ls','-a','--format','{{.Names}}'], '')]
