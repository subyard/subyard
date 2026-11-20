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

# Compound starts, substitutions, heredoc tails and command-running operands are not direct.
for gates, name, call in ((["sudo"], "sudo", "sudo id"), (["git", "sudo"], "sudo", "sudo id"),
                          (["git"], "git push", "git push"), (["git", "sudo"], "git commit", "git commit")):
    for command in (
        f"if {call}; then true; fi",
        f"if true; then {call}; fi",
        f"if false; then true; elif {call}; then true; fi",
        f"if false; then true; else {call}; fi",
        f"while {call}; do break; done",
        f"until {call}; do break; done",
        f"for x in 1; do {call}; done",
        f"{{ {call}; }}",
        f"! {call}",
        f"f() {{ {call}; }}; f",
        f"function f {{ {call}; }}",
        f"({call})",
        f"echo $({call})",
        f'echo "result: $({call})"',
        f"x=$({call})",
        f"echo `{call}`",
        f'echo "`{call}`"',
        f"echo $(echo $({call}))",
        f"cat > notes.txt <<'EOF'\nharmless text\nEOF\n/usr/bin/{call}",
        f"cat > notes.txt <<EOF && env {call}\nharmless text\nEOF",
        f"cat <<-EOF\n\tdata\n\tEOF\nenv {call}",
        f"cat <<EOF\n$({call})\nEOF",
        f"printf '%s\\n' a | xargs {call}",
        f"xargs -I{{}} -n 1 {call} < list",
        f"xargs -0 --max-args 1 {call} < list",
        f"find . -name x -exec {call} \\;",
        f"find . -execdir {call} {{}} +",
        f"find . -ok {call} ';'",
        f"find . -exec sh -c '{call}' \\;",
        f"eval '{call}'",
        f"eval {call}",
        f"env -iu HOME {call}",
        f"env -i -C /tmp {call}",
        f"env -uHOME {call}",
        f"env -iS '{call}'",
        f"env --split-string='{call}'",
        f"setsid {call}",
        f"stdbuf -oL {call}",
    ):
        denied(gates, command, name)

for gates in (["sudo"], ["git", "sudo"], ["git"]):
    for command in (
        "cat > notes.txt <<'EOF'\nsudo id; git push; $(sudo id); `git commit`\nEOF\ngit status",
        "cat > script.sh <<'EOF'\nif sudo id; then git push; fi\nEOF",
        "cat <<EOF\nplain $HOME text\nEOF\necho done",
        "echo 'literal $(sudo id) and `git push`'",
        "echo \"price: \\$(sudo id)\"",
        "echo done # $(sudo id) `git push`",
        "if true; then echo ok; fi",
        "for x in a b; do echo $x; done",
        "echo $(date) $(( 1 + 2 ))",
        "x=$(git status)",
        "printf '%s\\n' a | xargs echo",
        "find . -name '*.go' -exec grep -n sudo {} +",
        "find . -name 'git push'",
        "eval 'echo git push sudo'",
        "env -S 'printf %s' 'release note; /usr/bin/sudo'",
        "env -S 'printf %s' 'note; git push'",
        "env -iu HOME printf '%s' sudo",
        "xargs -I{} echo sudo {} < list",
        "{ echo sudo; }",
        "! grep -q sudo /etc/group",
        "echo then sudo",
    ):
        allowed(gates, command)

# Direct escalation stays with the agent's own approval prompt.
for gates in (["sudo"], ["git", "sudo"]):
    for command in ("sudo id && echo ok", "echo x | sudo tee /tmp/x > /dev/null",
                    "cat > f <<'EOF'\ndata\nEOF\nsudo install -m 0644 f /etc/f", "sudo $(which id)"):
        allowed(gates, command)

# --plain (Codex): a gated command needs a command of words joined by &&, ||, ; or |.
for gates, name, call in ((["--plain", "sudo"], "sudo", "sudo id"), (["--plain", "git", "sudo"], "sudo", "sudo id"),
                          (["--plain", "git"], "git push", "git push"), (["--plain", "git", "sudo"], "git push", "git push")):
    for command in (f"{call} > /dev/null", f"{call} 2>&1", f"echo x | {call} > out", f"{call}\necho done",
                    f"{call} $HOME", f"{call} # note", f"{call} &", f"{call} < /dev/null"):
        denied(gates, command, name)
    for command in (call, f"{call} | cat", f"true && {call}", f"{call}; echo ok", f"{call} || true",
                    f"{call} 'quoted $text'", "echo hi > f", "cat f > /dev/null && echo $HOME"):
        allowed(gates, command)
    for command in (f"{call} > /dev/null", f"{call}\necho done"):
        allowed([gate for gate in gates if gate != "--plain"], command)
