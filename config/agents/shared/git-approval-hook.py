#!/usr/bin/python3 -I
# Managed by Subyard Git approval provision.
"""Refuse a literal mention of a gated command unless the client's own prompt covers it.

Arguments name the gates: git (commit and push) and sudo (privilege escalation).
--plain selects the Codex rules, which require the whole command to be plain; without
it, the Claude rules check the plain prefix up to the first direct gated command.
"""

import json
import posixpath
import re
import shlex
import sys

# A flat lexer: shell separators, comments and quoted words, without evaluating anything.
TOKEN = re.compile(r"""[^\S\n]+|\#[^\n]*|[;&|\n]+|[()`]|<{1,3}|>+|(?:'[^']*'|"(?:\\.|[^"\\])*"|\\.|[^\s'"`;&|()<>\\])+""", re.S)
ASSIGNMENT = re.compile(r"^[A-Za-z_][A-Za-z_0-9]*\+?=")
GIT_VALUE_OPTIONS = {"-C", "-c", "--git-dir", "--work-tree",
                     "--namespace", "--config-env", "--super-prefix"}
GIT_FLAGS = {"-p", "--paginate", "-P", "--no-pager", "--no-replace-objects",
             "--no-optional-locks", "--no-lazy-fetch", "--literal-pathspecs",
             "--glob-pathspecs", "--noglob-pathspecs", "--icase-pathspecs",
             "--bare", "--no-advice"}
ESCALATION = ("sudo", "su", "doas", "pkexec", "sudoedit", "run0")
GIT_ACTIONS = ("git commit", "git push")
RESERVED = {"!", "{", "}", "[[", "]]", "case", "coproc", "do", "done", "elif", "else", "esac",
            "fi", "for", "function", "if", "in", "select", "then", "time", "until", "while"}
# Commands that only print, search or list their arguments, so a mention there is text.
INERT = {"echo", "printf", "grep", "rg", "which", "type", "ls", "cat", "head", "tail", "wc",
         "cut", "uniq", "cd", "true"}
PLAIN_SEPARATORS = {";", "|", "&&", "||"}
RESCAN = re.compile(r"""[\s;&|()<>`$#'"\\]""")


def git_action(words):
    """Return the subcommand a git argument list runs; ... marks one hidden by an expansion."""
    args = iter(words)
    for arg in args:
        if arg is None or "$" in arg or "`" in arg:
            return "..."
        if arg in GIT_VALUE_OPTIONS:
            next(args, None)
        elif arg in GIT_FLAGS or (
            "=" in arg and arg.partition("=")[0] in GIT_VALUE_OPTIONS | {"--exec-path"}
        ):
            continue
        else:
            return arg
    return None


def unquote(raw):
    """Return the value of one shell word; $'…' and $"…" lose their quoting prefix."""
    raw = re.sub(r"""(?<!\\)\$(?=['"])""", "", raw)
    try:
        return "".join(shlex.split(raw, comments=False))
    except ValueError:
        return re.sub(r"""[\\'"]""", "", raw)


def name(value):
    """Return the command name a word spells, also after an option's or assignment's =."""
    names = {posixpath.basename(value), posixpath.basename(value.rpartition("=")[2])}
    return next((n for n in ESCALATION + ("git",) if n in names), None)


def git_mention(rest):
    action = git_action(rest)
    if action in ("commit", "push"):
        return f"git {action}"
    if action == "...":
        # An expansion hides the subcommand; a later literal commit or push counts.
        for word in rest:
            if word in ("commit", "push"):
                return f"git {word}"
    return None


def mention(text, gates):
    """Return the first gated command the text names anywhere, in any position."""
    pending = [text]
    while pending:
        current = pending.pop(0)
        run = []
        offset = 0
        while offset <= len(current):
            token = TOKEN.match(current, offset) if offset < len(current) else None
            raw = token[0] if token else ""
            if token is None or raw.isspace() and "\n" in raw or raw[0] in ";&|\n#":
                # The end of a command's words: name a git subcommand in them.
                for index, word in enumerate(run):
                    if "git" in gates and word is not None and name(word) == "git":
                        found = git_mention(run[index + 1:])
                        if found:
                            return found
                run = []
                if token is None:
                    offset += 1
                    continue
                if raw[0] == "#":
                    pending.append(raw[1:])
            elif raw.isspace():
                pass
            elif raw[0] in "()`<>":
                run.append(None)
            else:
                value = unquote(raw)
                if "sudo" in gates and name(value) in ESCALATION:
                    return name(value)
                if len(value) < len(raw) and RESCAN.search(value):
                    pending.append(value)
                run.append(value)
            offset = token.end()
    return None


def native(segment, gates):
    """Return the gated command a segment starts with literally, as the client's rules match it."""
    names = (ESCALATION if "sudo" in gates else ()) + (GIT_ACTIONS if "git" in gates else ())
    for name in names:
        if segment == name or segment.startswith(name) and segment[len(name)] in " ;&|\n":
            return name
    return None


def inert(segment):
    return segment.split(None, 1)[0] in INERT or segment == "git commit" or segment.startswith("git commit ")


def pipelines(command, gates, claude):
    """Split a plain command into pipelines of (text, direct) segments, or return None.

    Plain means simple commands of words joined by &&, ||, ; or |, with no expansion,
    redirect, grouping, newline or comment, and no reserved word or assignment first.
    Claude prompts once for a whole call, so its check ends at the first direct segment.
    """
    result, pipeline, offset = [], [], 0
    while True:
        while command.startswith((" ", "\t"), offset):
            offset += 1
        start = offset
        if claude and native(command[start:], gates):
            return result + [pipeline + [(command[start:], True)]]
        words, separator = [], None
        while offset < len(command):
            token = TOKEN.match(command, offset)
            if token is None:
                return None
            raw = token[0]
            if raw in PLAIN_SEPARATORS:
                separator = raw
                break
            if raw.isspace():
                if raw.strip(" \t"):
                    return None
                offset = token.end()
                continue
            if raw[0] in "#;&|\n()`<>" or any(char in raw for char in "$`\n"):
                return None
            words.append(raw)
            offset = token.end()
        if not words or words[0] in RESERVED or ASSIGNMENT.match(words[0]):
            return None
        segment = command[start:offset].rstrip(" \t")
        pipeline.append((segment, bool(native(segment, gates))))
        if separator is None:
            return result + [pipeline]
        if claude and pipeline[-1][1]:
            return result + [pipeline]
        offset += len(separator)
        if separator != "|":
            result.append(pipeline)
            pipeline = []


def refused(command, gates, claude):
    """Return the mention that the client's own prompt does not cover, if any."""
    first = mention(command, gates)
    if not first:
        return None, True
    split = pipelines(command, gates, claude)
    if split is None:
        return first, False
    for pipeline in split:
        for segment, direct in pipeline:
            found = not direct and mention(segment, gates)
            if found and not (inert(segment) and all(d or inert(s) for s, d in pipeline)):
                return found, True
    return None, True


def main():
    gates = {arg for arg in sys.argv[1:] if not arg.startswith("--")} or {"git"}
    codex = "--plain" in sys.argv[1:]
    command = json.load(sys.stdin)["tool_input"]["command"]
    action, plain = refused(command, gates, claude=not codex)
    if not action:
        return 0
    if codex and not plain:
        print(f"Approval requires `{action}` in a plain command: words joined only by "
              f"&&, ||, ; or |, without redirects, variables, substitutions or grouping.",
              file=sys.stderr)
    elif action.startswith("git "):
        print(f"Git approval requires a direct `{action}` command. "
              f"For another directory, use `cd <directory> && {action}`.",
              file=sys.stderr)
    else:
        print(f"Sudo approval requires a direct `{action}` command at the start of a command, "
              f"without a path, wrapper, keyword, substitution or nested shell.",
              file=sys.stderr)
    if not codex or plain:
        print("Another command may name it only in a plain command that starts with "
              + ", ".join(sorted(INERT)) + ".", file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main())
