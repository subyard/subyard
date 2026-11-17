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
TOKEN = re.compile(r"""[^\S\n]+|\#[^\n]*|[;&|()\n]+|<{1,3}|>+|(?:'[^']*'|"(?:\\.|[^"\\])*"|\\.|[^\s'";&|()<>\\])+""", re.S)
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
    while offset < len(command):
        token = TOKEN.match(command, offset)
        if token is None:
            return
        offset = token.end()
        raw = token[0]
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
        while start < len(words) and re.match(r"^[A-Za-z_][A-Za-z_0-9]*=", tokens[start][0]):
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
            for index, arg in enumerate(args[:-1]):
                if re.fullmatch(r"-[a-zA-Z]*c[a-zA-Z]*", arg):
                    action = denied_action(args[index + 1], nested=True, depth=depth + 1)
                    if action:
                        return action
                    break
                if arg == "--" or not arg.startswith("-"):
                    break
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
