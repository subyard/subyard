#!/usr/bin/python3 -I
# Managed by Subyard Git approval provision.
"""Recognize literal gated command words without executing or expanding shell input.

Arguments name the gates: git (commit and push) and sudo (privilege escalation).
"""

import json
import posixpath
import re
import shlex
import sys

# Keep shell separators distinct from quoted words. Comments are recognized only
# at token boundaries. A full shell evaluator is outside this hook's scope.
TOKEN = re.compile(r"""[^\S\n]+|\#[^\n]*|[;&|\n]+|[()]|<{1,3}|>+|(?:'[^']*'|"(?:\\.|[^"\\])*"|\\.|[^\s'";&|()<>\\])+""", re.S)
ASSIGNMENT = re.compile(r"^[A-Za-z_][A-Za-z_0-9]*\+?=")
GIT_VALUE_OPTIONS = {"-C", "-c", "--git-dir", "--work-tree",
                     "--namespace", "--config-env", "--super-prefix"}
GIT_FLAGS = {"-p", "--paginate", "-P", "--no-pager", "--no-replace-objects",
             "--no-optional-locks", "--no-lazy-fetch", "--literal-pathspecs",
             "--glob-pathspecs", "--noglob-pathspecs", "--icase-pathspecs",
             "--bare", "--no-advice"}
ESCALATION = {"sudo", "su", "doas", "pkexec", "sudoedit", "run0"}
SHELLS = {"sh", "bash", "dash", "zsh", "ksh"}
# Wrapper options that take a separate operand, per wrapper.
WRAPPER_VALUE_OPTIONS = {"env": {"-u", "--unset", "-C", "--chdir"}, "exec": {"-a"},
                         "nice": {"-n", "--adjustment"}, "time": set(), "nohup": set(),
                         "command": set(), "timeout": {"-s", "--signal", "-k", "--kill-after"}}


def git_action(words):
    args = iter(words)
    for arg in args:
        if arg in GIT_VALUE_OPTIONS:
            next(args, None)
        elif arg in GIT_FLAGS or (
            "=" in arg and arg.partition("=")[0] in GIT_VALUE_OPTIONS | {"--exec-path"}
        ):
            continue
        else:
            return arg if arg in {"commit", "push"} else None
    return None


def simple_commands(command):
    words = []
    offset = 0
    array_depth = 0
    while offset < len(command):
        token = TOKEN.match(command, offset)
        if token is None:
            return
        offset = token.end()
        raw = token[0]
        if array_depth:
            array_depth += (raw == "(") - (raw == ")")
            continue
        if (raw == "(" and words and words[-1][2] == token.start()
                and ASSIGNMENT.fullmatch(words[-1][0])):
            array_depth = 1
            continue
        if raw == "<<":
            # A heredoc body contains data, not a list of shell commands.
            if words:
                yield words
            return
        if raw.startswith("#"):
            continue
        if raw.startswith("\n") or raw[0] in ";&|()":
            if words:
                yield words
                words = []
        elif not raw.isspace():
            words.append((raw, token.start(), token.end()))
    if words:
        yield words


def shell_command(executable, args):
    options = iter(args)
    command_mode = noexec = False
    # sh can be Bash under its POSIX invocation name. POSIX sh and dash
    # use -o; Bash also takes operands for -O, --init-file and --rcfile.
    bash_options = executable in {"bash", "sh"}
    for arg in options:
        if arg == "--":
            arg = next(options, None)
            break
        if bash_options and arg in {"--init-file", "--rcfile"}:
            next(options, None)
            continue
        if re.fullmatch(r"[-+][a-zA-Z]+", arg):
            enabled = arg[0] == "-"
            for flag in arg[1:]:
                if flag == "c" and enabled:
                    command_mode = True
                elif flag == "n":
                    noexec = enabled
                elif flag == "o" or (flag == "O" and bash_options):
                    option = next(options, None)
                    if flag == "o" and option == "noexec":
                        noexec = enabled
        elif not arg.startswith("--"):
            break
    else:
        return None
    return arg if command_mode and not noexec else None


def unwrap(executable, args):
    """Return the offset of the command a transparent wrapper runs.

    None means that the wrapper runs no command; a string is an env -S command line.
    """
    index, values = 0, WRAPPER_VALUE_OPTIONS[executable]
    while index < len(args):
        arg = args[index]
        if arg == "--":
            index += 1
            break
        if executable == "env" and ASSIGNMENT.match(arg):
            index += 1
        elif executable == "env" and arg.startswith(("-S", "--split-string")):
            script = arg[2:] if arg.startswith("-S") else arg.partition("=")[2]
            rest = args[index + 1:]
            if not script and "=" not in arg:
                script, rest = (rest[0], rest[1:]) if rest else ("", [])
            return " ".join([script, *rest])
        elif arg.startswith("-") and arg != "-":
            # command -v and -V only describe the named command.
            if executable == "command" and ("v" in arg or "V" in arg):
                return None
            index += 2 if arg in values else 1
        else:
            break
    if executable == "timeout":
        index += 1
    return index if index < len(args) else None


def gated(executable, args, gates):
    """Return the gated command name, if this simple command runs one."""
    if "git" in gates and executable == "git":
        action = git_action(args)
        return action and f"git {action}"
    if "sudo" in gates and executable in ESCALATION:
        return executable
    return None


def denied_action(command, gates=("git",), nested=False, depth=0):
    if depth == 8:
        return None
    command = command.replace("\\\n", "")
    for tokens in simple_commands(command):
        try:
            words = [shlex.split(raw, comments=False)[0] for raw, _, _ in tokens]
        except (IndexError, ValueError):
            continue
        start = 0
        while start < len(words) and ASSIGNMENT.match(tokens[start][0]):
            start += 1
        position, wrapped = start, False
        while position < len(words):
            executable = posixpath.basename(words[position])
            args = words[position + 1:]
            if executable not in WRAPPER_VALUE_OPTIONS:
                break
            offset = unwrap(executable, args)
            if isinstance(offset, str):
                action = denied_action(offset, gates, nested=True, depth=depth + 1)
                if action:
                    return action
            if not isinstance(offset, int):
                position = len(words)
            else:
                position, wrapped = position + 1 + offset, True
        if position >= len(words):
            continue
        action = gated(executable, args, gates)
        if action:
            raw = tokens[position][0] if executable in ESCALATION else (
                command[tokens[position][1]:tokens[position + 1][2]])
            if nested or wrapped or start or raw != action:
                return action
        elif executable in SHELLS:
            script = shell_command(executable, args)
            if script is not None:
                action = denied_action(script, gates, nested=True, depth=depth + 1)
                if action:
                    return action
    return None


def main():
    gates = set(sys.argv[1:]) or {"git"}
    payload = json.load(sys.stdin)
    action = denied_action(payload["tool_input"]["command"], gates)
    if action and action.startswith("git "):
        print(f"Git approval requires a direct `{action}` command. "
              f"For another directory, use `cd <directory> && {action}`.",
              file=sys.stderr)
        return 2
    if action:
        print(f"Sudo approval requires a direct `{action}` command at the start of a command, "
              f"without a path, wrapper or nested shell.",
              file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
