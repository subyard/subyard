#!/usr/bin/python3 -I
# Managed by Subyard Git approval provision.
"""Recognize literal gated command words without executing or expanding shell input.

Arguments name the gates: git (commit and push) and sudo (privilege escalation).
--plain also requires the whole command to be words joined by &&, ||, ; or |, which is the
only shell a client such as Codex matches its approval rules against.
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
# Transparent wrappers: short option letters and long options that take an operand.
WRAPPERS = {
    "env": ("uCP", {"--unset", "--chdir"}),
    "exec": ("a", set()),
    "nice": ("n", {"--adjustment"}),
    "time": ("fo", {"--format", "--output"}),
    "nohup": ("", set()),
    "command": ("", set()),
    "timeout": ("sk", {"--signal", "--kill-after"}),
    "setsid": ("", set()),
    "stdbuf": ("ioe", {"--input", "--output", "--error"}),
    "xargs": ("adEILnPs", {"--arg-file", "--delimiter", "--max-args", "--max-procs",
                          "--max-chars", "--process-slot-var"}),
}
# Words that start a compound command; a command after them is not direct.
KEYWORDS = {"{", "if", "then", "elif", "else", "do", "while", "until", "!"}
FIND_EXEC = {"-exec", "-execdir", "-ok", "-okdir"}
SUBSTITUTION = "_"


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


def matching_paren(text, index):
    """Return the index after the parenthesis that closes a substitution body at index."""
    depth, quote = 1, None
    while index < len(text):
        char = text[index]
        if char == "\\" and quote != "'":
            index += 2
            continue
        if quote:
            if char == quote:
                quote = None
        elif char in "'\"":
            quote = char
        elif char == "(":
            depth += 1
        elif char == ")":
            depth -= 1
            if depth == 0:
                return index + 1
        index += 1
    return len(text)


def body_substitutions(text):
    """Return the substitutions an unquoted heredoc body expands."""
    bodies, index = [], 0
    while index < len(text):
        if text[index] == "\\":
            index += 2
        elif text.startswith("$(", index):
            end = matching_paren(text, index + 2)
            bodies.append(text[index + 2:end - 1])
            index = end
        elif text[index] == "`":
            end = text.find("`", index + 1)
            end = len(text) if end < 0 else end
            bodies.append(text[index + 1:end])
            index = end + 1
        else:
            index += 1
    return bodies


def split_substitutions(command):
    """Separate command and backtick substitutions and heredoc bodies from the shell text.

    Returns the text with each substitution replaced by a placeholder word and each
    heredoc body removed, and the substitution bodies, which run as nested shell text.
    Single-quoted text, comments and quoted heredoc bodies are data, not commands.
    """
    out, bodies, heredocs = [], [], []
    index, quote = 0, None
    while index < len(command):
        char = command[index]
        if char == "\\" and quote != "'":
            out.append(command[index:index + 2])
            index += 2
            continue
        if quote == "'":
            out.append(char)
            quote = None if char == "'" else quote
            index += 1
            continue
        if char == "`":
            end = command.find("`", index + 1)
            end = len(command) if end < 0 else end
            bodies.append(command[index + 1:end])
            out.append(SUBSTITUTION)
            index = end + 1
            continue
        if command.startswith("$(", index):
            end = matching_paren(command, index + 2)
            bodies.append(command[index + 2:end - 1])
            out.append(SUBSTITUTION)
            index = end
            continue
        if quote == '"':
            out.append(char)
            quote = None if char == '"' else quote
            index += 1
            continue
        if char in "'\"":
            quote = char
        elif char == "#" and (index == 0 or command[index - 1] in " \t\n;&|()"):
            end = command.find("\n", index)
            index = len(command) if end < 0 else end
            continue
        elif command.startswith("<<", index) and not command.startswith("<<<", index):
            match = re.compile(r"<<(-?)[^\S\n]*((?:'[^']*'|\"[^\"]*\"|\\.|[^\s;&|()<>])+)").match(command, index)
            if match:
                word = match[2]
                heredocs.append((re.sub(r"""['"\\]""", "", word), match[1] == "-",
                                 any(c in word for c in "'\"\\")))
                out.append(match[0])
                index = match.end()
                continue
        elif char == "\n" and heredocs:
            out.append(char)
            index += 1
            for delimiter, tabs, literal in heredocs:
                lines = []
                while index < len(command):
                    end = command.find("\n", index)
                    end = len(command) if end < 0 else end
                    line = command[index:end]
                    index = end + 1
                    if (line.lstrip("\t") if tabs else line) == delimiter:
                        break
                    lines.append(line)
                if not literal:
                    bodies.extend(body_substitutions("\n".join(lines)))
            heredocs = []
            continue
        out.append(char)
        index += 1
    return "".join(out), bodies


def simple_commands(command):
    """Yield each simple command's words and whether a group or subshell starts it."""
    words = []
    grouped = False
    offset = 0
    array_depth = 0
    heredoc_word = False
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
            # The delimiter names the removed heredoc body; it is not a command word.
            heredoc_word = True
            continue
        if raw.startswith("#"):
            continue
        if raw.startswith("\n") or raw[0] in ";&|()":
            if words:
                yield words, grouped
                words = []
            grouped = raw == "("
        elif not raw.isspace():
            if heredoc_word:
                heredoc_word = False
                continue
            words.append((raw, token.start(), token.end()))
    if words:
        yield words, grouped


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

    None means that the wrapper runs no command; a list is the words of an env -S command.
    """
    index = 0
    letters, long_values = WRAPPERS[executable]
    while index < len(args):
        arg = args[index]
        if arg == "--":
            index += 1
            break
        if executable == "env" and ASSIGNMENT.match(arg):
            index += 1
        elif executable == "env" and arg.startswith("--split-string"):
            script = arg.partition("=")[2] if "=" in arg else (args[index + 1] if index + 1 < len(args) else "")
            rest = args[index + (1 if "=" in arg else 2):]
            return split_words(script) + rest
        elif arg.startswith("--"):
            index += 2 if arg in long_values else 1
        elif arg.startswith("-") and arg != "-":
            # command -v and -V only describe the named command.
            if executable == "command" and ("v" in arg or "V" in arg):
                return None
            index += 1
            for position, letter in enumerate(arg[1:], 1):
                if executable == "env" and letter == "S":
                    script = arg[position + 1:]
                    if not script:
                        script, index = (args[index] if index < len(args) else ""), index + 1
                    return split_words(script) + args[index:]
                if letter in letters:
                    # The rest of the word is the operand, or the next argument is.
                    index += 0 if arg[position + 1:] else 1
                    break
        else:
            break
    if executable == "timeout":
        index += 1
    return index if index < len(args) else None


def split_words(text):
    """Split an env -S string as env does: quotes group words; shell operators are text."""
    try:
        return shlex.split(text, comments=False)
    except ValueError:
        return []


def gated(executable, args, gates):
    """Return the gated command name, if this simple command runs one."""
    if "git" in gates and executable == "git":
        action = git_action(args)
        return action and f"git {action}"
    if "sudo" in gates and executable in ESCALATION:
        return executable
    return None


def plain(command):
    """Return whether the command is only words joined by &&, ||, ; or |."""
    command = command.replace("\\\n", "")
    offset = 0
    while offset < len(command):
        token = TOKEN.match(command, offset)
        if token is None:
            return False
        offset, raw = token.end(), token[0]
        if raw.isspace():
            if "\n" in raw:
                return False
        elif raw[0] in ";&|\n":
            if raw not in {";", "|", "&&", "||"}:
                return False
        elif raw[0] in "#()<>" or any(c in re.sub(r"'[^']*'", "", raw) for c in "$`"):
            # Single-quoted text is literal; anything else with $ or ` expands.
            return False
    return True


def direct_action(command, gates):
    """Return the first gated command that runs directly in a top-level simple command."""
    for tokens, _ in simple_commands(split_substitutions(command.replace("\\\n", ""))[0]):
        try:
            words = [shlex.split(raw, comments=False)[0] for raw, _, _ in tokens]
        except (IndexError, ValueError):
            continue
        action = words and gated(posixpath.basename(words[0]), words[1:], gates)
        if action:
            return action
    return None


def nested_words(words, gates, depth):
    """Check words that another program runs as a command; none of them is direct."""
    return denied_action(shlex.join(words), gates, nested=True, depth=depth + 1) if words else None


def denied_action(command, gates=("git",), nested=False, depth=0):
    if depth == 8:
        return None
    command, substitutions = split_substitutions(command.replace("\\\n", ""))
    for body in substitutions:
        action = denied_action(body, gates, nested=True, depth=depth + 1)
        if action:
            return action
    for tokens, grouped in simple_commands(command):
        try:
            words = [shlex.split(raw, comments=False)[0] for raw, _, _ in tokens]
        except (IndexError, ValueError):
            continue
        start = 0
        while start < len(words) and ASSIGNMENT.match(tokens[start][0]):
            start += 1
        position, wrapped = start, grouped
        while position < len(words) and words[position] in KEYWORDS | {"function"}:
            position += 2 if words[position] == "function" else 1
            wrapped = True
        while position < len(words):
            executable = posixpath.basename(words[position])
            args = words[position + 1:]
            if executable not in WRAPPERS:
                break
            offset = unwrap(executable, args)
            if isinstance(offset, list):
                action = nested_words(offset, gates, depth)
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
        elif executable == "eval":
            action = denied_action(" ".join(args), gates, nested=True, depth=depth + 1)
            if action:
                return action
        elif executable == "find":
            for index, arg in enumerate(args):
                if arg in FIND_EXEC:
                    end = next((i for i in range(index + 1, len(args)) if args[i] in {";", "+"}), len(args))
                    action = nested_words(args[index + 1:end], gates, depth)
                    if action:
                        return action
    return None


def main():
    gates = {arg for arg in sys.argv[1:] if not arg.startswith("--")} or {"git"}
    payload = json.load(sys.stdin)
    command = payload["tool_input"]["command"]
    action = denied_action(command, gates)
    if not action and "--plain" in sys.argv[1:] and not plain(command):
        action = direct_action(command, gates)
        if action:
            print(f"Approval requires `{action}` in a plain command: words joined only by "
                  f"&&, ||, ; or |, without redirects, variables, substitutions or grouping.",
                  file=sys.stderr)
            return 2
    if action and action.startswith("git "):
        print(f"Git approval requires a direct `{action}` command. "
              f"For another directory, use `cd <directory> && {action}`.",
              file=sys.stderr)
        return 2
    if action:
        print(f"Sudo approval requires a direct `{action}` command at the start of a command, "
              f"without a path, wrapper, keyword, substitution or nested shell.",
              file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
