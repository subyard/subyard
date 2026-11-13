#!/usr/bin/env bash
# Pre-baseline adoption of every shipped Claude legacy settings document through the real JSON
# materializer, converging to the current home template.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
python3 - "$ROOT" <<'PY'
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile

root = Path(sys.argv[1])
package = root / "config/agents/claude"
desired = (package / "settings.json").read_bytes()
digest = hashlib.sha256(desired).hexdigest()
uid, developer = os.getuid(), "dev"
template = (root / "internal/adapters/configmaterial/guest_json.py").read_text()
for historical_path in sorted((package / "legacy-config").glob("*.json")):
    historical = historical_path.read_bytes()
    with tempfile.TemporaryDirectory(prefix="subyard-claude-legacy-", dir="/tmp") as temporary:
        work = Path(temporary)
        state = work / "state"
        state.mkdir(mode=0o700)
        home = work / "home/dev"
        (home / ".claude").mkdir(parents=True, mode=0o755)
        destination = home / ".claude/settings.json"
        # The released native writer left exactly these bytes, without baseline, lock or receipt.
        destination.write_bytes(historical)
        destination.chmod(0o644)
        program = template
        for old, new in (('STATE_ROOT = "@STATE_ROOT@"', "STATE_ROOT = " + json.dumps(str(state))),
                         ('STATE_UID = int("@STATE_UID@")', "STATE_UID = " + str(uid)),
                         ('FORMAT = "@FORMAT@"', 'FORMAT = "json"')):
            if program.count(old) != 1:
                raise SystemExit("FAIL: guest materializer test hook changed")
            program = program.replace(old, new)
        program = "LEGACY_TEMPLATES = " + json.dumps([historical.decode()]) + "\n" + program

        def run(mode):
            result = subprocess.run([sys.executable, "-B", "-c", program, mode, developer, str(destination),
                                     str(uid), digest, str(home), digest],
                                    input=desired, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            if result.returncode != 0:
                raise SystemExit(f"FAIL: {historical_path.name} {mode}: {result.stderr.decode().strip()}")
            return json.loads(result.stdout) if result.stdout.strip() else None

        before = destination.stat().st_mtime_ns
        assessment = run("assess-adopt")
        if (assessment.get("adoptable") is not True or destination.read_bytes() != historical
                or destination.stat().st_mtime_ns != before or any(state.iterdir())):
            raise SystemExit(f"FAIL: {historical_path.name} adoption refused or changed state")
        run("apply")
        if json.loads(destination.read_text()) != json.loads(desired) or not run("observe").get("converged"):
            raise SystemExit(f"FAIL: {historical_path.name} did not converge to the current template")
        print(f"ok: Claude {historical_path.stem} pre-baseline adoption")
PY
