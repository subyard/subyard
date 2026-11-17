"""Run the shared payload through the PreToolUse command contract."""

import json
import subprocess
import sys

hook = sys.argv[1]
for action in ("commit", "push"):
    for command in (
        f"git -C 'other repo' {action}",
        f"git -c user.name=example {action}",
        f"git --git-dir=.git --no-pager {action}",
        f"git '{action}'",
        f'"git" {action}',
        f"git\t{action}",
        f"/usr/bin/git {action}",
        f"sh -c 'git {action}'",
        f'bash -lc "cd x && git {action}"',
        f"bash -c -- '/usr/bin/git {action}'",
        f"bash -n +n -c '/usr/bin/git {action}'",
        f"examples=(/usr/bin/git {action}); (/usr/bin/git {action})",
        f"true; /usr/bin/git {action}",
        f"GIT_DIR=.git git {action}",
    ):
        result = subprocess.run([hook], input=json.dumps({"tool_input": {"command": command}}),
                                text=True, capture_output=True)
        assert result.returncode == 2 and f"git {action}" in result.stderr, (command, result)

for command in (
    "git commit --amend",
    "git push",
    "cd 'other repo' && git commit",
    "git stash push",
    "git log --grep commit",
    "git -C x stash push",
    "git -C push rebase",
    "git -c alias.commit=example status",
    "echo '&&' /usr/bin/git push",
    "echo '# text'; git log",
    "# /usr/bin/git push",
    "cat <<'EOF'\n/usr/bin/git push\nEOF",
    "sh -c 'git log --grep commit; git stash push'",
    "bash -n -c '/usr/bin/git commit'",
    "bash -cn '/usr/bin/git commit'",
    "bash -c -o noexec -- '/usr/bin/git commit'",
    "examples=(/usr/bin/git push)",
    "examples+=(\n/usr/bin/git push\n); git status",
):
    result = subprocess.run([hook], input=json.dumps({"tool_input": {"command": command}}),
                            text=True, capture_output=True)
    assert result.returncode == 0 and not result.stdout and not result.stderr, (command, result)
