"""Flag common early-exit pipeline consumers on a single source line."""

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
}

PIPE = re.compile(
    r"(?<!\|)\|&?(?!\|)\s*(?![({]\s*head\b[^;|&]*&&\s*cat\s*>\s*/dev/null\s*[;)])"
    r"(?:\{\s+|\(\s*)?(?:sudo\s+(?:-\w+\s+)*)?"
    r"""(?:(?:head|read)|(?:grep|rg)(?:\s+(?!--(?:\s|$))[^\s;|&()]+)*?\s+"""
    r"""(?:-[A-Za-z]*[qmlL][A-Za-z0-9]*|--(?:quiet|max-count)(?:=[^\s;|&()]*)?))"""
    r"""(?=[\s;&<>()'"]|$)"""
)
RUN = re.compile(r"^(\s*(?:-\s+)?)run:\s*(.*)$")
SHEBANG = re.compile(rb"^#!.*[/ ](?:ba|da|z|k)?sh\b")

failed = False
for path in map(Path, filter(None, subprocess.check_output(
        ["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"],
        text=True).split("\0"))):
    name = path.as_posix()
    if name in HISTORICAL or path.is_symlink() or not path.is_file():
        continue
    workflow = path.parent == Path(".github/workflows") and path.suffix in {".yml", ".yaml"}
    with path.open("rb") as stream:
        if not (workflow or path.suffix in {".sh", ".bash", ".zsh"} or SHEBANG.match(stream.readline(128))):
            continue
    block_indent = None
    for number, line in enumerate(path.read_text().splitlines(), 1):
        code = line.lstrip()
        if not code or code.startswith("#"):
            continue
        if workflow:
            if run := RUN.match(line):
                block_indent = len(run[1]) if run[2].startswith(("|", ">")) else None
                code = run[2]
            elif block_indent is None or len(line) - len(code) <= block_indent:
                block_indent = None
                continue
        if PIPE.search(code.replace(ALLOW[name], "") if name in ALLOW else code):
            print(f"{name}:{number}: consumer can close pipeline input early under pipefail", file=sys.stderr)
            failed = True
sys.exit(failed)
