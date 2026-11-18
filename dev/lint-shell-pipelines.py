"""Reject common consumers that close a pipe before its producer finishes."""

import os
from pathlib import Path
import re
import subprocess
import sys

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
    # The pipe characters here are awk regex alternation.
    "dev/e2e/config-sync-two-host.sh":
        "awk '/^  (owner-host|remote|branch|upstream|head|fetch|fetched-upstream|",
}

PIPE = re.compile(
    r"(?<!\|)\|&?(?!\|)\s*(?:(?:command|env|sudo(?:\s+-n)?)\s+)?"
    r"(?:[\w./-]*/)?(?P<consumer>head|read|grep|egrep|fgrep|rg)(?=[\s;|&<>()\"'`]|$)"
)
OPTIONS = re.compile(r"(?:\s+(?:-[^\s;|&()]+|\d*>[^\s;|&()]+))*")
EARLY_GREP_OPTIONS = re.compile(
    r"(?:^|\s)(?:-[A-Za-z]*[qmlL][A-Za-z]*\d*|"
    r"--(?:quiet|silent|max-count|files-with-matches|files-without-match|files-without-matches)"
    r"(?:=\d+)?)(?=\s|$)"
)


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

        # Scan fragments too. They may be sourced by a shell with pipefail enabled.
        # Keep newline counts when joining backslash continuations for diagnostics.
        source = path.read_text().replace("\\\n", " \n")
        for match in PIPE.finditer(source):
            consumer = match["consumer"]
            if consumer not in {"head", "read"}:
                options = OPTIONS.match(source, match.end())[0].split()
                if "--" in options:
                    options = options[:options.index("--")]
                if not any(EARLY_GREP_OPTIONS.fullmatch(option) for option in options):
                    continue
            line_start = source.rfind("\n", 0, match.start()) + 1
            line_end = source.find("\n", match.end())
            line = source[line_start:line_end if line_end != -1 else len(source)]
            if name in ALLOW:
                fragment = ALLOW[name]
                start = line.find(fragment)
                if start != -1 and start <= match.start() - line_start < start + len(fragment):
                    continue
            line_number = source.count("\n", 0, match.start()) + 1
            print(f"{name}:{line_number}: {consumer} can close pipeline input early under pipefail", file=sys.stderr)
            failed = True
    return int(failed)


if __name__ == "__main__":
    sys.exit(main())
