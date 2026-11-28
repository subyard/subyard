#!/usr/bin/env bash
# Host-free contract for the CI apt helper: the runner image's packages.microsoft.com source is
# disabled and apt is set to IPv4 with retries before apt runs, and every apt failure stops the
# helper with apt's own status.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
python3 -B - "$ROOT" <<'PY'
import os
from pathlib import Path
import subprocess
import sys
import tempfile

root = Path(sys.argv[1])
helper = root / "dev/ci/apt-install.sh"

# Workflows and the hosted runner install packages only through the helper, so no apt failure
# can be skipped inside an && list that a later command in the same step outlives.
for path in [*(root / ".github/workflows").glob("*.yml"), root / "dev/ci/periodic-e2e.sh"]:
    assert "apt-get" not in path.read_text(), f"{path.name} calls apt-get instead of dev/ci/apt-install.sh"

with tempfile.TemporaryDirectory(prefix="subyard-ci-apt-", dir="/tmp") as temporary:
    fixture = Path(temporary)
    tools = fixture / "bin"
    tools.mkdir()
    # Real file operations for the source move; apt calls are traced with configurable status.
    (tools / "sudo").write_text("""#!/bin/bash
[ "$1" = -n ] || exit 99
shift
case "$1" in
  install|mv) exec "$@" ;;
  apt-get) [ -f "$CONF" ] || exit 97
    printf '%s\\n' "$*" >> "$TRACE"
    case "$2" in update) exit "$UPDATE_EXIT" ;; install) exit "$INSTALL_EXIT" ;; esac ;;
esac
exit 98
""")
    (tools / "sudo").chmod(0o700)

    conf = fixture / "apt.conf.d/80-subyard-ci-network"

    def run(update=0, install=0, hosted=True, args=("jq", "ripgrep")):
        sources, disabled, trace = fixture / "sources", fixture / "disabled", fixture / "trace"
        aptconf = fixture / "apt.conf.d"
        trace.unlink(missing_ok=True)
        conf.unlink(missing_ok=True)
        aptconf.mkdir(exist_ok=True)
        for directory in (sources, disabled):
            if directory.exists():
                for entry in directory.iterdir():
                    entry.unlink()
                directory.rmdir()
        sources.mkdir()
        (sources / "microsoft-prod.list").write_text(
            "deb [arch=amd64] https://packages.microsoft.com/ubuntu/24.04/prod noble main\n")
        (sources / "microsoft-edge.sources").write_text(
            "Types: deb\nURIs: https://packages.microsoft.com/repos/edge\nSuites: stable\n")
        (sources / "ubuntu.sources").write_text("Types: deb\nURIs: http://archive.ubuntu.com/ubuntu\n")
        (sources / "notes.txt").write_text("https://packages.microsoft.com/ is not an apt source\n")
        env = dict(os.environ, PATH=f"{tools}:{os.environ['PATH']}", TRACE=str(trace),
                   UPDATE_EXIT=str(update), INSTALL_EXIT=str(install),
                   SUBYARD_CI_APT_SOURCES=str(sources), SUBYARD_CI_APT_DISABLED=str(disabled),
                   SUBYARD_CI_APT_CONF=str(aptconf), CONF=str(conf))
        env.pop("GITHUB_ACTIONS", None)
        env.pop("RUNNER_ENVIRONMENT", None)
        if hosted:
            env.update(GITHUB_ACTIONS="true", RUNNER_ENVIRONMENT="github-hosted")
        result = subprocess.run(["bash", str(helper), *args], env=env, capture_output=True, text=True)
        calls = trace.read_text().splitlines() if trace.exists() else []
        left = sorted(entry.name for entry in sources.iterdir())
        moved = sorted(entry.name for entry in disabled.iterdir()) if disabled.exists() else []
        return result, calls, left, moved

    result, calls, left, moved = run()
    assert result.returncode == 0, result.stderr
    assert calls == ["apt-get update -qq", "apt-get install -y jq ripgrep"], calls
    assert left == ["notes.txt", "ubuntu.sources"], left
    assert moved == ["microsoft-edge.sources", "microsoft-prod.list"], moved
    # Apt calls see the network settings: the fake sudo refuses apt without the file.
    assert conf.read_text() == 'Acquire::ForceIPv4 "true";\nAcquire::Retries "5";\n', conf.read_text()
    assert conf.stat().st_mode & 0o777 == 0o644, oct(conf.stat().st_mode)

    # A failed update stops before install with apt's status; the third-party source is already off.
    result, calls, left, moved = run(update=100)
    assert result.returncode == 100, (result.returncode, result.stderr)
    assert calls == ["apt-get update -qq"], calls
    assert moved == ["microsoft-edge.sources", "microsoft-prod.list"], moved

    result, calls, _, _ = run(install=100, args=("-qq", "--no-install-recommends", "jq"))
    assert result.returncode == 100, (result.returncode, result.stderr)
    assert calls == ["apt-get update -qq", "apt-get install -y -qq --no-install-recommends jq"], calls

    # Outside a GitHub-hosted runner it changes nothing and runs no apt command.
    result, calls, left, moved = run(hosted=False)
    assert result.returncode == 2 and "outside a disposable GitHub-hosted runner" in result.stderr, result
    assert calls == [] and moved == [] and len(left) == 4, (calls, left, moved)
    assert not conf.exists()

    result, calls, _, _ = run(args=())
    assert result.returncode == 2 and "usage:" in result.stderr and calls == [], result
print("ok: CI apt helper disables the runner's Microsoft source, sets IPv4 with retries and fails at the failing apt step")
PY
