"""Run the shared payload through the PreToolUse command contract."""

import json
import subprocess
import sys

hook = sys.argv[1]
CLAUDE, CODEX = [], ["--plain"]
MODES = (CLAUDE, CODEX)
ESCALATION = ("sudo", "su", "doas", "pkexec", "sudoedit", "run0")
SUDO_GATES = (["sudo"], ["git", "sudo"])
GIT_GATES = ([], ["git"], ["git", "sudo"])


def run(gates, command):
    return subprocess.run([hook, *gates], input=json.dumps({"tool_input": {"command": command}}),
                          text=True, capture_output=True)


def denied(gates, command, name):
    result = run(gates, command)
    assert result.returncode == 2 and f"`{name}" in result.stderr, (gates, command, result)


def allowed(gates, command):
    result = run(gates, command)
    assert result.returncode == 0 and not result.stdout and not result.stderr, (gates, command, result)


def each(gate_sets, modes=MODES):
    return [mode + gates for gates in gate_sets for mode in modes]


# Any other spelling of git commit or push is refused in both modes.
for gates in each(GIT_GATES):
    for action in ("commit", "push"):
        for command in (
            f"git -C 'other repo' {action}",
            f"git -c user.name=example {action}",
            f"git --git-dir=.git --no-pager {action}",
            f"git '{action}'",
            f'"git" {action}',
            f"git\t{action}",
            f"git  {action}",
            f"/usr/bin/git {action}",
            f"sh -c 'git {action}'",
            f'bash -lc "cd x && git {action}"',
            f"bash -O extglob -c 'git {action}'",
            f"bash --init-file /dev/null -oO errexit extglob -c 'git {action}'",
            f"bash -c -- '/usr/bin/git {action}'",
            f"examples=(/usr/bin/git {action}); (/usr/bin/git {action})",
            f"true; /usr/bin/git {action}",
            f"GIT_DIR=.git git {action}",
            f"env git {action}",
            f"command git {action}",
            f"nohup git {action}",
            f"timeout -k 1 10 git {action}",
            f"env -S 'git {action}'",
            f"git `true` {action}",
            f"git $(true) {action}",
            f"git > /dev/null {action}",
            f"gh pr create --title 'git {action} fix'",
        ):
            denied(gates, command, f"git {action}")

for gates in each(GIT_GATES):
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
        "sh -c 'git log --grep commit; git stash push'",
        "command -v git",
        "env git status",
        "git commit -m 'Fix (push) and ; text'",
        "echo hi > f",
        "cat f > /dev/null && echo $HOME",
        "if true; then echo ok; fi",
        "x=$(git status)",
    ):
        allowed(gates, command)

for gates in each(SUDO_GATES):
    for name in ESCALATION:
        for command in (
            f"/usr/bin/{name} id",
            f"'{name}' id",
            f"\\{name} id",
            f"{name}\tid",
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
        "sudo id || true",
        "sudo -u root /usr/bin/sudo id",
        "git commit -m 'sudo'",
        # A mention inside a read-only command's arguments is text.
        "rg sudo src | head",
        "echo su",
        "which sudo",
        "git commit -m 'Fix sudo'",
        "du -su",
        "cat /etc/sudoers",
        "git stash push",
        "git log --grep commit",
        "echo then sudo",
        "grep -rn sudo docs | cut -d: -f1 | uniq | wc -l",
        "type sudo; ls /usr/bin/sudo",
        "echo sudo | sudo tee /tmp/x",
    ):
        allowed(gates, command)

# Without the sudo gate, escalation spellings stay ungated.
for gates in each(([], ["git"])):
    for command in ("/usr/bin/sudo id", "env sudo id", "bash -c 'sudo id'", "man sudo"):
        allowed(gates, command)

# A direct Git segment makes the sudo mention inert when both policies are active.
for gates in each((["git", "sudo"],)):
    allowed(gates, "echo sudo | git push")

# Compound commands, substitutions, heredocs, stdin-fed shells and command-running operands.
for gates, name, call in ((["sudo"], "sudo", "sudo id"), (["git", "sudo"], "sudo", "sudo id"),
                          (["git"], "git push", "git push"), (["git", "sudo"], "git commit", "git commit")):
    for mode in MODES:
        for command in (
            f"if {call}; then true; fi",
            f"if true; then {call}; fi",
            f"if true; then true; {call}; fi",
            f"if false; then true; elif {call}; then true; fi",
            f"while {call}; do break; done",
            f"until {call}; do break; done",
            f"for x in 1; do {call}; done",
            f"{{ {call}; }}",
            f"! {call}",
            f"f() {{ {call}; }}; f",
            f"function f {{ {call}; }}",
            f"({call})",
            f"coproc {call}",
            f"builtin command {call}",
            f"x=1; {call}",
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
            f"bash <<'EOF'\n{call}\nEOF",
            f"echo '{call}' | bash",
            f"printf '%s\\n' '{call}' | sh",
            f"echo $((1<<2))\nenv {call}",
            f"</dev/null {call}",
            f">&2 /usr/bin/{call}",
            f"timeout 5s -- {call}",
            f"printf '%s\\n' a | xargs {call}",
            f"xargs -I{{}} -n 1 {call} < list",
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
            f"echo x | xargs -0 {call}",
            # Deep quoting is rescanned word by word, without a depth limit.
            f"bash -c \"bash -c \\\"sh -c '{call}'\\\"\"",
            f"eval \"$(printf '%s' '{call}')\"",
        ):
            denied(mode + gates, command, name)

for gates in each(SUDO_GATES):
    # Quoting and escapes are removed before a word is named. Unplaceable text, such as an
    # unterminated quote, separates words instead of hiding them.
    for command in ("$'sudo' id", "$\"sudo\" id", "s\\udo id", "su''do id", "'/usr/bin/sudo' id",
                    "cat 'notes; sudo id"):
        denied(gates, command, "sudo")

# Expected false positives: a mention outside a plain read-only command is refused.
for gates in each(SUDO_GATES):
    for command in (
        "man sudo",
        "grep sudo . 2>/dev/null",
        "cat > notes.txt <<'EOF'\nRun sudo id first.\nEOF",
        "cat <<'EOF'\n/usr/bin/sudo id\nEOF",
        "gh pr create --title 'Fix sudo'",
        "ssh host 'sudo id'",
        "command -v sudo",
        "apt-cache show sudo",
        "rg sudo | sort",
        "echo 'literal $(sudo id)'",
        "echo done # sudo id",
        "{ echo sudo; }",
        "env -iu HOME printf '%s' sudo",
        "find . -name '*.go' -exec grep -n sudo {} +",
    ):
        denied(gates, command, "sudo")
for gates in each(GIT_GATES):
    for command in ("# /usr/bin/git push", "cat <<'EOF'\n/usr/bin/git push\nEOF",
                    "bash -n -c '/usr/bin/git push'", "examples=(/usr/bin/git push)",
                    "find . -name 'git push'"):
        denied(gates, command, "git push")

# Claude prompts once for a whole call, so its check ends at the first direct command.
for gates, name, call in ((["sudo"], "sudo", "sudo id"), (["git", "sudo"], "sudo", "sudo id"),
                          (["git"], "git push", "git push"), (["git", "sudo"], "git push", "git push")):
    for command in (f"{call} > /dev/null", f"{call} 2>&1", f"echo x | {call} > out", f"{call}\necho done",
                    f"{call} $HOME", f"{call} # note", f"{call} &", f"{call} < /dev/null",
                    f"{call} 'quoted $text'", f"cd /tmp && {call} > /dev/null; echo $(id) | bash",
                    f"{call}; /usr/bin/{call}"):
        allowed(CLAUDE + gates, command)
        denied(CODEX + gates, command, name)
    for command in (call, f"{call} | cat", f"true && {call}", f"{call}; echo ok", f"{call} || true",
                    f"{call} 'quoted text'", f'{call} "quoted text"', f"cd ./a-b_c/d.e:f,g@h%i+j=k && {call}"):
        allowed(CLAUDE + gates, command)
        allowed(CODEX + gates, command)
    # Codex runs these without asking even though they are plain to the shell.
    for command in (f"ls -d /t* && {call}", f"cd ~ && {call}", f"ls {{a,b}} && {call}", f"{call} a\\ b",
                    f"ls [ab] && {call}", f'{call} "a\\"b"', f"ls a!b && {call}"):
        allowed(CLAUDE + gates, command)
        denied(CODEX + gates, command, name)
    # A refused prefix is not rescued by a later direct command.
    for command in (f"echo $HOME; {call}", f"ls > f; {call}", f"x=1 {call}", f"echo '{call}' | bash; {call}"):
        denied(CLAUDE + gates, command, name)
        denied(CODEX + gates, command, name)

# ANSI-C escapes, braces, `!` and `=` glue, and Git's exec-path commands still name the command.
for gates in each(SUDO_GATES):
    for command in ("$'\\x73udo' id", "$'\\163udo' id", "$'\\u0073udo' id", "{sudo,} id",
                    "/usr/bin/{sudo,x} id", "git -c alias.x='!sudo id' x", "!sudo id"):
        denied(gates, command, "sudo")
    for command in ("cat {a,b}.txt", "echo a,b", "echo $'a\\x62'", "rg '!x' src"):
        allowed(gates, command)
for gates in each(GIT_GATES):
    for command in ("/usr/lib/git-core/git-push origin", "git-commit -m x", "env PATH=/usr/lib/git-core git-push"):
        denied(gates, command, "git " + ("push" if "push" in command else "commit"))
    allowed(gates, "cat /usr/lib/git-core/git-push")

for gates in SUDO_GATES:
    # A lone & runs the next command unprompted, and Codex runs a quoted backslash without asking.
    denied(CLAUDE + gates, "ls & sudo id", "sudo")
    denied(CODEX + gates, "ls & sudo id", "sudo")
    allowed(CLAUDE + gates, 'sudo id "a\\b"')
    denied(CODEX + gates, 'sudo id "a\\b"', "sudo")
    for mode in MODES:
        allowed(mode + gates, "\tsudo id")
        allowed(mode + gates, " \t cd /tmp && \tsudo id")

# Commands above the scan limit fail closed instead of outlasting the client's hook timeout.
for gates in each((["git", "sudo"],)):
    allowed(gates, "a" * (256 * 1024))
    result = run(gates, "a" * (256 * 1024 + 1))
    assert result.returncode == 2 and "256 KiB" in result.stderr, (gates, result.returncode, result.stderr)

# Bash ends a $'…' string at its first NUL, however the NUL is spelled; \c makes control characters.
for gates in each(SUDO_GATES):
    for command in ("$'sudo\\0x' id", "$'sudo\\x00' id", "$'sudo\\x0' id", "$'sudo\\c@' id",
                    "$'sudo\\000junk' id", "/usr/bin/$'sudo\\0' id", "$'sudo\\u0000x' id",
                    "$'sudo\\U00000000x' id", "env -Ssudo id", "env -S'sudo id'"):
        denied(gates, command, "sudo")
    denied(gates, "$'su\\c@do' id", "su")
    for command in ("echo $'\\cAsudo'", "$'s\\cudo' id"):
        result = run(gates, command)
        assert "`sudo" not in result.stderr, (gates, command, result)
