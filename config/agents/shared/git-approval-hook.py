#!/usr/bin/python3 -I
# Managed by Subyard Git approval provision.
"""Refuse a literal mention of a gated command unless the client's own prompt covers it.

Arguments name the gates: git (commit and push) and sudo (privilege escalation).
--plain selects the Codex rules, which require the whole command to be plain; without
it, the Claude rules check the plain prefix up to the first direct gated command.
"""

import collections
import json
import os
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
# Codex runs a command with a glob, tilde, brace or escape without asking, so its plain words
# use only these characters outside quotes, and quotes without expansions or escapes.
CODEX_WORD = re.compile(r"""(?:[A-Za-z0-9_@%+=:,./-]|'[^'\n]*'|"[^"\\$`\n]*")+""")
RESCAN = re.compile(r"""[\s;&|()<>`$#'"\\]""")
# Larger commands are refused: scanning them could outlast the client's hook timeout.
MAX_COMMAND_BYTES = 256 * 1024
ANSI_C = re.compile(r"""(?<!\\)\$'((?:\\.|[^'\\])*)'""", re.S)
ANSI_C_ESCAPE = re.compile(r"\\(x[0-9A-Fa-f]{1,2}|u[0-9A-Fa-f]{1,4}|U[0-9A-Fa-f]{1,8}|[0-7]{1,3}|c.|.)", re.S)
ANSI_C_NAMED = {"a": "\a", "b": "\b", "e": "\x1b", "E": "\x1b", "f": "\f", "n": "\n",
                "r": "\r", "t": "\t", "v": "\v"}
# Brace expansion, option values and history expansion glue names to other text.
NAME_PARTS = re.compile(r"[{},=!]")


def git_action(words, start=0):
    """Return the subcommand git runs from words[start:]; ... marks one hidden by an expansion."""
    index = start
    while index < len(words):
        arg = words[index]
        index += 1
        if arg is None or "$" in arg or "`" in arg:
            return "..."
        if arg in GIT_VALUE_OPTIONS:
            index += 1
        elif not (arg in GIT_FLAGS or (
            "=" in arg and arg.partition("=")[0] in GIT_VALUE_OPTIONS | {"--exec-path"}
        )):
            return arg
    return None


def ansi_c(match):
    """Return a $'…' body with its escapes decoded, quoted again for shlex."""
    def decode(escape):
        code = escape[1]
        if code[0] in "xuU":
            value = chr(min(int(code[1:], 16), 0x10FFFF))
        elif code[0] in "01234567":
            value = chr(int(code, 8) & 0xFF)
        elif code[0] == "c" and len(code) == 2:
            value = chr(ord(code[1]) & 0x1F)
        else:
            value = ANSI_C_NAMED.get(code, code)
        return value
    # Bash ends the string at the first NUL, from \0, \x0, \u0000 or \c@ alike.
    return shlex.quote(ANSI_C_ESCAPE.sub(decode, match[1]).partition("\0")[0])


def unquote(raw):
    """Return the value of one shell word; $'…' is decoded and $"…" loses its prefix."""
    raw = ANSI_C.sub(ansi_c, raw)
    raw = re.sub(r"""(?<!\\)\$(?=["])""", "", raw)
    try:
        return "".join(shlex.split(raw, comments=False))
    except ValueError:
        return re.sub(r"""[\\'"]""", "", raw)


def names(value):
    """Return the command names a word spells, also inside braces, after = or !, or after -S."""
    parts = [value, *NAME_PARTS.split(value)]
    if value.startswith("-S"):
        # env -S takes its command in the same word.
        parts.append(value[2:])
    return {posixpath.basename(part) for part in parts}


def git_mention(run):
    """Return the git action that a git word in one command's words runs."""
    later, action = [None] * (len(run) + 1), None
    for index in range(len(run) - 1, -1, -1):
        action = f"git {run[index]}" if run[index] in ("commit", "push") else action
        later[index] = action
    for index, word in enumerate(run):
        if word is not None and "git" in names(word):
            action = git_action(run, index + 1)
            if action in ("commit", "push"):
                return f"git {action}"
            if action == "...":
                # An expansion hides the subcommand; a later literal commit or push counts.
                return later[index + 1]
    return None


def mention(text, gates):
    """Return the first gated command the text names anywhere, in any position."""
    pending = collections.deque([text])
    while pending:
        current = pending.popleft()
        run = []
        offset = 0
        while offset <= len(current):
            token = TOKEN.match(current, offset) if offset < len(current) else None
            raw = token[0] if token else ""
            if token is None or raw.isspace() and "\n" in raw or raw[0] in ";&|\n#":
                # The end of a command's words: name a git subcommand in them.
                found = "git" in gates and git_mention(run)
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
                spelled = names(value)
                found = "sudo" in gates and next((n for n in ESCALATION if n in spelled), None)
                if found:
                    return found
                # Git runs git-commit and git-push from its exec path.
                found = "git" in gates and next((f"git {n[4:]}" for n in ("git-commit", "git-push")
                                                 if n in spelled), None)
                if found:
                    return found
                if len(value) < len(raw) and RESCAN.search(value):
                    pending.append(value)
                run.append(value)
            offset = token.end()
    return None


def native(text, gates, start=0):
    """Return the gated command text[start:] starts with literally, as the client's rules match it."""
    for name in (ESCALATION if "sudo" in gates else ()) + (GIT_ACTIONS if "git" in gates else ()):
        end = start + len(name)
        if text.startswith(name, start) and (end == len(text) or text[end] in " ;&|\n"):
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
        if claude and native(command, gates, start):
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
            if not claude and not CODEX_WORD.fullmatch(raw):
                return None
            words.append(raw)
            offset = token.end()
        if not words or words[0] in RESERVED or ASSIGNMENT.match(words[0]):
            return None
        segment = command[start:offset].rstrip(" \t")
        pipeline.append((segment, bool(native(segment, gates))))
        if separator is None:
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
        # A mention is text only when no command of its pipeline can run it.
        inert_pipeline = all(d or inert(s) for s, d in pipeline)
        for segment, direct in pipeline:
            found = not direct and mention(segment, gates)
            if found and not (inert_pipeline and inert(segment)):
                return found, True
    return None, True


def main():
    args = sys.argv[1:]
    if "--policies" in args:
        index = args.index("--policies")
        # Provision supplies the two paths under the client-owned managed directory.
        gates = {gate for gate, path in zip(("git", "sudo"), args[index + 1:], strict=True)
                 if os.path.isfile(path)}
        if not gates:
            return 0
        args = args[:index]
    else:
        # Existing sessions can still invoke the original registration.
        gates = {arg for arg in args if not arg.startswith("--")} or {"git"}
    codex = "--plain" in args
    command = json.load(sys.stdin)["tool_input"]["command"]
    if len(command.encode(errors="surrogatepass")) > MAX_COMMAND_BYTES:
        print(f"Approval refuses a command longer than {MAX_COMMAND_BYTES // 1024} KiB; "
              f"write long scripts to a file first.", file=sys.stderr)
        return 2
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
