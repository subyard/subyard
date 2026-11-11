#!/usr/bin/env bash
# Verify initial adoption of real Codex configs from supported published templates.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
[ "$(cat "$ROOT/config/agents/codex/legacy-config/repo.rules.sha256")" = \
  173dc6094c13c0710d34ee2ff3952b9ef88a2feca5d6d631ebcb58c3192c4da0 ] \
  || { printf 'FAIL: shipped v0.11.0 Codex rules digest changed\n' >&2; exit 1; }
for version in v0.11.0 v0.13.11 v0.14.0; do
python3 - "$ROOT" "$version" <<'PY'
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import tomllib

root = Path(sys.argv[1])
version = sys.argv[2]
historical_path = root / ("config/agents/codex/legacy-config/" + version + ".toml")
desired_path = root / "config/agents/codex/config.toml"
guest_path = root / "internal/adapters/configmaterial/guest_json.py"
writer_path = root / "internal/adapters/configmaterial/tomli_w.py"
license_path = root / "internal/adapters/configmaterial/TOMLI_W_LICENSE"

historical = historical_path.read_bytes()
expected_hashes = {"v0.11.0": "c7c933cb5b894f679e32f901969de0c86f1826811f35564aa22b2f6574c27f36", "v0.13.11": "9581b77258b2ffb73d54403a7efa494989b4e8acc5caad732206e11fdb9d717c", "v0.14.0": "a32a59cf613724d54dcdfd9780450be62f8ba2bef125f76750aabf9ff9109ec0"}
if hashlib.sha256(historical).hexdigest() != expected_hashes[version]:
    raise SystemExit("FAIL: shipped " + version + " Codex template bytes changed")
desired = desired_path.read_bytes()
desired_digest = hashlib.sha256(desired).hexdigest()
uid = os.getuid()
developer = "dev"

with tempfile.TemporaryDirectory(prefix="subyard-codex-legacy-", dir="/tmp") as temporary:
    work = Path(temporary)
    state_root = work / "state"
    state_root.mkdir(mode=0o700)
    home = work / "home/dev"
    config_dir = home / ".codex"
    config_dir.mkdir(parents=True, mode=0o755)
    destination = config_dir / "config.toml"
    destination_text = str(destination)

    # These are the v0.13.11 schema1 ownership pointers; the baseline contains
    # no value copy and no schema2 receipt, as on an untouched pre-receipt yard.
    owned = [
        {"path": "/approval_policy", "kind": "value"},
        {"path": "/approvals_reviewer", "kind": "value"},
        {"path": "/sandbox_mode", "kind": "value"},
    ]
    identity = hashlib.sha256((developer + "\0" + destination_text).encode()).hexdigest()
    baseline_path = state_root / (identity + ".json")
    receipt_path = state_root / (identity + ".ownership")
    lock_path = state_root / (identity + ".lock")
    baseline_path.write_text(json.dumps({
        "schema": 1,
        "developer": developer,
        "destination": destination_text,
        "desired_digest": hashlib.sha256(historical).hexdigest(),
        "owned": owned,
    }, separators=(",", ":")) + "\n")
    baseline_path.chmod(0o600)
    lock_path.touch(mode=0o600)
    lock_path.chmod(0o600)

    # Simulate user additions beside the historical owned fields. The current
    # desired template also adds [tui], so apply must preserve both additions.
    destination.write_bytes(
        historical
        + b'\nruntime_note = "keep top-level value"\n'
        + b'\n[custom]\nkeep = "keep custom table"\n'
    )
    destination.chmod(0o644)
    original_document = destination.read_bytes()
    original_baseline = baseline_path.read_bytes()

    program = guest_path.read_text()
    for old, new in (
        ('STATE_ROOT = "@STATE_ROOT@"', "STATE_ROOT = " + json.dumps(str(state_root))),
        ('STATE_UID = int("@STATE_UID@")', "STATE_UID = " + str(uid)),
        ('FORMAT = "@FORMAT@"', 'FORMAT = "toml"'),
    ):
        if program.count(old) != 1:
            raise SystemExit("FAIL: guest materializer test hook changed")
        program = program.replace(old, new)

    licensed_writer = "# " + license_path.read_text().replace("\n", "\n# ") + "\n" + writer_path.read_text()
    program = (
        "_toml_writer = {}\nexec(" + json.dumps(licensed_writer) + ", _toml_writer)\n"
        + "LEGACY_TEMPLATES = " + json.dumps([historical.decode("utf-8")]) + "\n"
        + program
    )

    def run(mode, expect_observation=True):
        result = subprocess.run(
            [sys.executable, "-B", "-c", program, mode, developer,
             destination_text, str(uid), desired_digest, str(home), desired_digest],
            input=desired,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
        if result.returncode != 0:
            detail = result.stderr.decode("utf-8", errors="replace").strip()
            raise SystemExit("FAIL: Codex legacy config " + mode + " failed: " + detail)
        if not expect_observation:
            return None
        try:
            return json.loads(result.stdout)
        except Exception:
            raise SystemExit("FAIL: Codex legacy config returned invalid observation")

    if version == "v0.11.0":
        # v0.11.0's native writer installed exactly these bytes, mode 0644,
        # without any materialization baseline, lock or ownership receipt.
        baseline_path.unlink()
        lock_path.unlink()
        destination.write_bytes(historical)
        before = destination.stat()
        assessment = run("assess-adopt")
        after = destination.stat()
        if (assessment.get("adoptable") is not True or assessment.get("converged") is not False
                or destination.read_bytes() != historical
                or (before.st_ino, before.st_mode, before.st_uid, before.st_gid, before.st_mtime_ns)
                != (after.st_ino, after.st_mode, after.st_uid, after.st_gid, after.st_mtime_ns)
                or any(state_root.iterdir())):
            raise SystemExit("FAIL: pre-baseline adoption refused or changed native state")
        run("apply", expect_observation=False)
        if tomllib.loads(destination.read_text()) != tomllib.loads(desired.decode()):
            raise SystemExit("FAIL: pre-baseline Codex config did not reach the current defaults")
        if not baseline_path.is_file() or not receipt_path.is_file() or not run("observe").get("converged"):
            raise SystemExit("FAIL: pre-baseline apply did not publish converged ownership")
        if any(path.stat().st_uid != uid or path.stat().st_mode & 0o777 != 0o600
               for path in (baseline_path, receipt_path)):
            raise SystemExit("FAIL: pre-baseline apply published unsafe ownership evidence")
        print("ok: Codex pre-baseline TOML adoption")
        sys.exit(0)

    for problem in ("managed-value", "unknown-baseline"):
        if problem == "managed-value":
            destination.write_bytes(original_document.replace(b'approval_policy = "on-request"', b'approval_policy = "never"'))
        else:
            baseline = json.loads(original_baseline)
            baseline["desired_digest"] = "0" * 64
            baseline_path.write_text(json.dumps(baseline))
        before = (destination.read_bytes(), baseline_path.read_bytes())
        refused = subprocess.run(
            [sys.executable, "-B", "-c", program, "assess-adopt", developer,
             destination_text, str(uid), desired_digest, str(home), desired_digest],
            input=desired, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
        if refused.returncode == 0 or before != (destination.read_bytes(), baseline_path.read_bytes()) or receipt_path.exists():
            raise SystemExit("FAIL: unsafe historical Codex adoption was accepted or changed state")
        destination.write_bytes(original_document)
        baseline_path.write_bytes(original_baseline)

    assessment = run("assess-adopt")
    if assessment.get("adoptable") is not True or assessment.get("converged") is not False:
        raise SystemExit("FAIL: unchanged historical Codex fields were not adoptable")
    if destination.read_bytes() != original_document or baseline_path.read_bytes() != original_baseline:
        raise SystemExit("FAIL: adoption assessment changed the destination or baseline")
    if receipt_path.exists():
        raise SystemExit("FAIL: adoption assessment published an ownership receipt")

    run("apply", expect_observation=False)
    actual = tomllib.loads(destination.read_text())
    if actual["approval_policy"] != "on-request" or actual["approvals_reviewer"] != "user":
        raise SystemExit("FAIL: historical Codex owned values changed unexpectedly")
    if actual["sandbox_mode"] != "danger-full-access":
        raise SystemExit("FAIL: historical Codex sandbox setting changed unexpectedly")
    if actual["runtime_note"] != "keep top-level value" or actual["custom"]["keep"] != "keep custom table":
        raise SystemExit("FAIL: user-added Codex runtime values were lost")
    if actual["tui"]["fullscreen_transcript"] is not False:
        raise SystemExit("FAIL: current shipped Codex defaults were not applied")

    for path in (baseline_path, receipt_path):
        try:
            info = path.stat()
        except FileNotFoundError:
            raise SystemExit("FAIL: schema2 ownership evidence was not published")
        if info.st_uid != uid or info.st_mode & 0o777 != 0o600:
            raise SystemExit("FAIL: schema2 ownership evidence has unsafe permissions")
    baseline = json.loads(baseline_path.read_text())
    receipt = json.loads(receipt_path.read_text())
    if baseline.get("schema") != 1 or baseline.get("desired_digest") != desired_digest:
        raise SystemExit("FAIL: materialization did not publish the current compatible baseline")
    if receipt.get("schema") != 2 or receipt.get("phase") != "applied":
        raise SystemExit("FAIL: materialization did not publish the applied schema2 receipt")
    print("ok: Codex legacy TOML adoption")
PY
done
