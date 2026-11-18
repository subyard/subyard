"""Reject common consumers that close a pipe before its producer finishes."""

import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
from textwrap import dedent

# Frozen predecessor scripts used as migration inputs.
HISTORICAL = {
    "tests/fixtures/migrations/v0.1.0-install-runtime-release.sh",
    "tests/fixtures/migrations/v0.3.1-install-runtime-release.sh",
    "internal/adapters/releaseruntime/testdata/historical-project-creators/0.11.0.txt",
    "internal/adapters/releaseruntime/testdata/historical-project-creators/0.19.0.txt",
}
ALLOW = {
    # Deliberate bounded reads in explicit POSIX sh invocations.
    "config/profiles/openclaw/resources/qa-bot-broker/handler.sh":
        """yexec sh -c 'umask 077; { openssl rand -hex 32 2>/dev/null || tr -dc "a-f0-9" </dev/urandom | head -c 64;""",
    "dev/e2e/staging-reliability.sh":
        "outer_exec sh -eu -c 'incus version; qemu-system-x86_64 --version | head -n 1;",
}

PIPE = re.compile(
    r"(?<!\|)\|&?(?!\|)\s*(?P<group>\{\s+|\(\s*)?(?:(?:command|env|sudo(?:\s+-n)?)\s+)?"
    r"(?:[\w./-]*/)?(?P<consumer>head|read|grep|egrep|fgrep|rg)(?=[\s;|&<>()]|$)"
)
DRAIN = re.compile(r"[^\n;|&]*&&\s*cat\s*>\s*/dev/null\s*(?:;\s*\}|;?\s*\))")
COMMAND_END = re.compile(r"[\n;|()]|(?<![<>])&")
EARLY_GREP_OPTIONS = {
    "--quiet", "--silent", "--max-count", "--files-with-matches",
    "--files-without-match", "--files-without-matches",
}
GREP_OPTION_VALUES = {
    "--regexp", "--file", "--after-context", "--before-context", "--context",
    "--include", "--exclude", "--exclude-dir", "--exclude-from", "--glob", "--iglob", "--type", "--type-not",
}

# Keep original offsets while hiding shell data. Literal sh -c programs and
# command substitutions are scanned as their own shell fragments.
SHELL_TOKEN = re.compile(
    r"""(?P<comment>(?<![^\s;|&()<>])\#[^\n]*)|(?P<single>'[^']*')|(?P<double>")"""
    r"""|(?P<substitution>\$\((?!\())|(?P<backtick>`(?:\\.|[^`])*`)|(?P<escape>\\[\s\S])"""
    r"""|(?P<heredoc>(?<!<)<<-?[ \t]*['"\\]?[A-Za-z_][\w-]*['"]?)|(?P<paren>[()])|(?P<newline>\n)"""
)
DOUBLE_TOKEN = re.compile(r'\\[\s\S]|\$\((?!\()|"')
SHELL_COMMAND = re.compile(r"(?:^|[\s;&|()])(?:[\w./-]*/)?(?:ba|da|k|z)?sh\s+(?:-[a-zA-Z]+\s+)*-[a-zA-Z]*c[a-zA-Z]*\s*$")
# Require Bash in command position, including behind the repository's launchers.
BASH_STDIN = re.compile(
    r"(?:^|[;&|()])\s*(?:(?:if|then|elif|else|do|\{)\s+)?(?:!\s+)?"
    r"(?:(?:\w+=\S+|command|env|sudo(?:\s+-n)?|guest(?:_root|_dev|\s+_+)?|yexec|"
    r"peer|operator_env|remote(?:_dev)?|(?:[\w./-]*/)?runuser\s+-u\s+[\w-]+\s+--)\s+)*"
    r"(?:(?:timeout(?:\s+--[\w=-]+)*\s+\d+[smhd]?|(?:incus|_+)\s+exec\s+[^;&|<>()]*?\s+--)\s+)?"
    r"(?:[\w./-]*/)?bash(?:\s+-[a-bd-zA-Z]+|\s+(?:pipefail|errexit|nounset))*"
    r"(?:\s+(?:--|-[a-bd-zA-Z]*s[a-bd-zA-Z]*)(?:\s+[^;&|<>()]*)?)?\s*$"
)
WORKFLOW_RUN = re.compile(r"^( *)(?:- )?run:\s*(\S.*)$")
YAML_DOUBLE_QUOTED = re.compile(r'"((?:[^"\\]|\\[\s\S])*)"')
YAML_ESCAPE = re.compile(
    r"\\(?P<escape>x[0-9a-fA-F]{2}|u[0-9a-fA-F]{4}|U[0-9a-fA-F]{8}|.)"
    r"|(?P<escaped_break>\\)\n[ \t]*|[ \t]*(?P<breaks>\n(?:[ \t]*\n)*)[ \t]*"
)
YAML_ESCAPES = dict(zip('0abtnvfre "/\\N_LP', '\0\a\b\t\n\v\f\r\x1b "/\\\x85\xa0\u2028\u2029'))


def yaml_escape(match):
    if match["escaped_break"]:
        return ""
    if breaks := match["breaks"]:
        return "\n" * (breaks.count("\n") - 1) or " "
    escape = match["escape"]
    if len(escape) > 1:
        return chr(int(escape[1:], 16))
    return YAML_ESCAPES.get(escape, match[0])


def shell_fragments(source, start=0, limit=None, substitution=False):
    if limit is None:
        limit = len(source)
    fragments, masked, heredocs = [], [], []
    cursor, depth = start, 0
    line_start, mask_start = start, 0
    while match := SHELL_TOKEN.search(source, cursor, limit):
        token = match[0]
        cursor = match.end()
        if match.lastgroup == "paren":
            if substitution and token == ")" and depth == 0:
                limit = match.start()
                break
            depth += 1 if token == "(" else -1
            continue
        if match.lastgroup == "heredoc":
            delimiter = token.removeprefix("<<")
            prefix = list(source[line_start:match.start()])
            for first, last, fill in masked[mask_start:]:
                prefix[first - line_start:last - line_start] = fill * (last - first)
            executable = BASH_STDIN.search("".join(prefix)) is not None
            heredocs.append((delimiter.removeprefix("-").strip().strip("'\"\\"),
                             delimiter.startswith("-"), executable))
            continue
        if match.lastgroup == "newline":
            for delimiter, strip_tabs, executable in heredocs:
                body_start = cursor
                while cursor < limit:
                    line_offset = cursor
                    end = source.find("\n", cursor, limit)
                    end = limit if end == -1 else end
                    line = source[cursor:end]
                    cursor = min(end + 1, limit)
                    if (line.lstrip("\t") if strip_tabs else line) == delimiter:
                        if executable:
                            _, children = shell_fragments(source, body_start, line_offset)
                            fragments.extend(children)
                        break
            heredocs.clear()
            if cursor > match.end():
                masked.append((match.end(), cursor, " "))
            line_start, mask_start = cursor, len(masked)
            continue
        if match.lastgroup == "double":
            while inner := DOUBLE_TOKEN.search(source, cursor, limit):
                cursor = inner.end()
                if inner[0] == '"':
                    break
                if inner[0] == "$(":
                    end, children = shell_fragments(source, cursor, limit, substitution=True)
                    fragments.extend(children)
                    cursor = min(end + 1, limit)
        elif match.lastgroup == "substitution":
            end, children = shell_fragments(source, cursor, limit, substitution=True)
            fragments.extend(children)
            cursor = min(end + 1, limit)
        quoted_command = False
        if match.lastgroup in {"single", "double"} and source[cursor - 1] == token[0]:
            quote_line_start = max(start, source.rfind("\n", start, match.start()) + 1)
            quoted_command = SHELL_COMMAND.search(source[quote_line_start:match.start()]) is not None
        if match.lastgroup == "backtick" or quoted_command:
            _, children = shell_fragments(source, match.start() + 1, cursor - 1)
            fragments.extend(children)
        fill = " " if match.lastgroup == "comment" or token == "\\\n" else "_"
        masked.append((match.start(), cursor, fill))
    code = list(source[start:limit])
    for first, last, fill in masked:
        code[first - start:last - start] = fill * (last - first)
    fragments.append((start, "".join(code)))
    return limit, fragments


def early_grep(source, code, start, consumer):
    end = COMMAND_END.search(code, start)
    end = end.start() if end else len(code)
    skip_value = False
    for token in re.finditer(r"\S+", code[start:end]):
        raw = source[start + token.start():start + token.end()]
        if skip_value:
            skip_value = False
            continue
        if not raw.startswith(("-", "'", '"', "\\")):
            continue
        try:
            word, = shlex.split(raw)
        except ValueError:
            continue
        if word == "--":
            break
        if word.startswith("--"):
            option, equal, _ = word.partition("=")
            if option in EARLY_GREP_OPTIONS:
                return True
            skip_value = not equal and option in GREP_OPTION_VALUES
        elif word.startswith("-"):
            value_options = "efABCDd" + ("gtrEjMT" if consumer == "rg" else "")
            for index, flag in enumerate(word[1:], 2):
                if flag in ("qml" if consumer == "rg" else "qmlL"):
                    return True
                if flag in value_options:
                    skip_value = index == len(word)
                    break
    return False


def workflow_shell(source):
    lines = source.splitlines(keepends=True)
    block_start, block_indent = 0, 0
    offset, quoted_end = 0, -1
    for index, line in enumerate(lines + ["end"]):
        line_offset, offset = offset, offset + len(line)
        if index <= quoted_end:
            lines[index] = "\n"
            continue
        if block_start and (not line.strip() or len(line) - len(line.lstrip()) > block_indent):
            continue
        if block_start:
            lines[block_start:index] = dedent("".join(lines[block_start:index])).splitlines(keepends=True)
            block_start = 0
        if index == len(lines):
            break
        if match := WORKFLOW_RUN.match(line):
            if re.fullmatch(r"[|>][+-]?", match[2]):
                block_start = index + 1
                block_indent = len(match[1]) + (2 if line.lstrip().startswith("- ") else 0)
            else:
                command = match[2]
                if len(command) > 1 and command[0] == command[-1] == "'":
                    command = command[1:-1].replace("''", "'")
                elif quoted := YAML_DOUBLE_QUOTED.match(source, line_offset + match.start(2)):
                    command = YAML_ESCAPE.sub(yaml_escape, quoted[1])
                    quoted_end = index + quoted[0].count("\n")
                lines[index] = command + "\n"
                continue
        lines[index] = "\n"
    return lines


def main():
    failed = False
    files = subprocess.check_output(["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"])
    for name in map(os.fsdecode, filter(None, files.split(b"\0"))):
        path = Path(name)
        if name in HISTORICAL or not path.is_file() or path.is_symlink():
            continue
        if path.suffix not in {".sh", ".bash", ".zsh", ".yml", ".yaml"}:
            with path.open("rb") as stream:
                shebang = stream.readline(128)
            if not (shebang.startswith(b"#!") and re.search(rb"[/ ](?:ba|da|z|k)?sh\b", shebang)):
                continue

        source = path.read_text()
        line_numbers = None
        if path.suffix in {".yml", ".yaml"}:
            lines = workflow_shell(source.removesuffix("\n") + "\n")
            line_numbers = [number for number, line in enumerate(lines, 1) for _ in range(line.count("\n"))]
            source = "".join(lines)
        _, fragments = shell_fragments(source)
        findings = set()
        for offset, code in fragments:
            raw = source[offset:offset + len(code)]
            for match in PIPE.finditer(code):
                consumer = match["consumer"]
                if match["group"] and DRAIN.match(code, match.end()):
                    continue
                if consumer not in {"head", "read"} and not early_grep(raw, code, match.end(), consumer):
                    continue
                findings.add((offset + match.start(), consumer))
        for position, consumer in sorted(findings):
            line_start = source.rfind("\n", 0, position) + 1
            line_end = source.find("\n", position)
            line = source[line_start:line_end if line_end != -1 else len(source)]
            if name in ALLOW:
                fragment = ALLOW[name]
                start = line.find(fragment)
                if start != -1 and start <= position - line_start < start + len(fragment):
                    continue
            line_number = source.count("\n", 0, position) + 1
            if line_numbers is not None:
                line_number = line_numbers[line_number - 1]
            print(f"{name}:{line_number}: {consumer} can close pipeline input early under pipefail", file=sys.stderr)
            failed = True
    return int(failed)


if __name__ == "__main__":
    sys.exit(main())
