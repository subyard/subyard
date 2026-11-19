"""Run the shared payload through the PreToolUse command contract."""

import json
import subprocess
import sys

hook = sys.argv[1]


def run(gates, command):
    return subprocess.run([hook, *gates], input=json.dumps({"tool_input": {"command": command}}),
                          text=True, capture_output=True)


def denied(gates, command, name):
    result = run(gates, command)
    assert result.returncode == 2 and f"`{name}" in result.stderr, (gates, command, result)


def allowed(gates, command):
    result = run(gates, command)
    assert result.returncode == 0 and not result.stdout and not result.stderr, (gates, command, result)


for gates in ([], ["git"], ["git", "sudo"]):
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
            f"bash -O extglob -c 'git {action}'",
            f"bash +O extglob -c 'git {action}'",
            f"bash --init-file /dev/null -oO errexit extglob -c 'git {action}'",
            f"bash --rcfile /dev/null -c 'git {action}'",
            f"sh -oe errexit -c 'git {action}'",
            f"bash -c -- '/usr/bin/git {action}'",
            f"bash -n +n -c '/usr/bin/git {action}'",
            f"examples=(/usr/bin/git {action}); (/usr/bin/git {action})",
            f"true; /usr/bin/git {action}",
            f"GIT_DIR=.git git {action}",
            f"env git {action}",
            f"env -u HOME GIT_DIR=.git git {action}",
            f"command git {action}",
            f"nohup git {action}",
            f"timeout -k 1 10 git {action}",
            f"env -S 'git {action}'",
        ):
            denied(gates, command, f"git {action}")

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
        "command -v git",
        "env git status",
    ):
        allowed(gates, command)

escalation = ("sudo", "su", "doas", "pkexec", "sudoedit", "run0")
for gates in (["sudo"], ["git", "sudo"]):
    for name in escalation:
        for command in (
            f"/usr/bin/{name} id",
            f"'{name}' id",
            f"\\{name} id",
            f"env {name} id",
            f"env -i PATH=/usr/bin {name} id",
            f"command {name} id",
            f"exec {name} id",
            f"nohup {name} id",
            f"nice -n 5 {name} id",
            f"time -p {name} id",
            f"timeout 10 {name} id",
            f"bash -c '{name} id'",
            f'sh -lc "cd /tmp && {name} id"',
            f"FOO=1 {name} id",
            f"true; /usr/bin/{name} -",
            f"env -S '{name} id'",
        ):
            denied(gates, command, name)
    for command in (
        "sudo apt-get update",
        "sudo",
        "su -",
        "doas id",
        "pkexec id",
        "sudoedit /etc/hosts",
        "run0 id",
        "cd /tmp && sudo id",
        "echo x | sudo tee /tmp/x",
        "echo sudo",
        "man sudo",
        "command -v sudo",
        "git commit -m 'sudo'",
        "apt-cache show sudo",
        "cat <<'EOF'\n/usr/bin/sudo id\nEOF",
        "sudo -u root /usr/bin/sudo id",
    ):
        allowed(gates, command)

# Without the sudo gate, escalation spellings stay ungated.
for gates in ([], ["git"]):
    for command in ("/usr/bin/sudo id", "env sudo id", "bash -c 'sudo id'"):
        allowed(gates, command)
