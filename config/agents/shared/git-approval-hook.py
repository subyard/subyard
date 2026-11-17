#!/usr/bin/python3 -I
# Managed by Subyard Git approval provision.
"""Recognize literal Git command words without executing or expanding shell input."""

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


def shell_command(args):
    options = iter(args)
    command_mode = noexec = False
    for arg in options:
        if arg == "--":
            arg = next(options, None)
            break
        if re.fullmatch(r"[-+][a-zA-Z]+", arg):
            enabled = arg[0] == "-"
            flags = arg[1:]
            if "c" in flags and enabled:
                command_mode = True
            if "n" in flags:
                noexec = enabled
            if flags.endswith("o") and next(options, None) == "noexec":
                noexec = enabled
        elif not arg.startswith("--"):
            break
    else:
        return None
    return arg if command_mode and not noexec else None


def denied_action(command, nested=False, depth=0):
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
        if start == len(words):
            continue
        executable = posixpath.basename(words[start])
        args = words[start + 1:]
        if executable == "git":
            action = git_action(args)
            if action and (nested or start or
                           command[tokens[0][1]:tokens[1][2]] != f"git {action}"):
                return action
        elif executable in {"sh", "bash", "dash", "zsh", "ksh"}:
            script = shell_command(args)
            if script is not None:
                action = denied_action(script, nested=True, depth=depth + 1)
                if action:
                    return action
    return None


def main():
    payload = json.load(sys.stdin)
    action = denied_action(payload["tool_input"]["command"])
    if action:
        print(f"Git approval requires a direct `git {action}` command. "
              f"For another directory, use `cd <directory> && git {action}`.",
              file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
