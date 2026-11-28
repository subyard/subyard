#!/usr/bin/env bash
# Host-free contract for the failed-lane broker state capture: periodic-e2e.sh keeps it with the
# other diagnostics, and it prints a quarantine's cause and the redacted spool but no identities.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
python3 -B - "$ROOT" <<'PY'
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile

root = Path(sys.argv[1])
script = root / "dev/ci/test-yard-broker-state.sh"
runner = (root / "dev/ci/periodic-e2e.sh").read_text()
keep = runner[runner.index("keep_diagnostics() {"):]
keep = keep[:keep.index("\n}\n")]
assert "test-yard-broker-state.sh" in keep and '"$output/broker-state.log"' in keep, keep
assert "sudo -n /usr/bin/incus exec yard-test-yard --project subyard-test-yard -- bash -s" in keep, keep

with tempfile.TemporaryDirectory(prefix="subyard-ci-broker-state-", dir="/tmp") as temporary:
    fixture = Path(temporary)
    state, tools = fixture / "state", fixture / "bin"
    for directory in (state / "spool/events", state / "spool/incidents", tools):
        directory.mkdir(parents=True)
    secret = "secret-identity-value"
    (state / "leases.json").write_text(json.dumps({"slots": [{
        "slot_id": "slot-001", "state": "quarantined", "lease_epoch": 1,
        "failure_reason": "incus init images:abc e2e-base-1 --vm: exit status 1",
        "lease_id": secret, "client_id": secret, "capability_hash": secret,
        "controller_fingerprint": secret, "acquire_owner": {"pid": 1, "id": secret}}]}))
    (state / "images.json").write_text(json.dumps({
        "last_error": "base_build_failed", "retry_attempt": 1, "owner": secret,
        "build": {"project": "subyard-e2e-vms-builder", "incident_id": "",
                  "readiness_failure": {"phase": "builder-preparation"}, "worker_key": secret,
                  "incident_slot": {"slot_id": "slot-001", "lease_epoch": 1, "client_id": secret}}}))
    (state / "spool/events/0001.json").write_text('{"kind":"lease.acquire"}')
    (state / "spool/incidents/0001.json").write_text('{"incident_id":"i-1","error":"redacted"}')
    for tool in ("journalctl", "incus", "df"):
        (tools / tool).write_text(f"#!/bin/sh\necho {tool} ran\n")
        (tools / tool).chmod(0o700)
    env = dict(os.environ, PATH=f"{tools}:{os.environ['PATH']}", SUBYARD_CI_BROKER_STATE=str(state))
    result = subprocess.run(["bash", str(script)], env=env, capture_output=True, text=True)
    assert result.returncode == 0, result
    out = result.stdout
    for expected in ("incus init images:abc e2e-base-1 --vm: exit status 1", '"state": "quarantined"',
                     "base_build_failed", "builder-preparation", '"lease.acquire"', '"incident_id":"i-1"',
                     "journalctl ran", "incus ran", "df ran"):
        assert expected in out, (expected, out)
    assert secret not in out, out

    # Unreadable state still prints the journal and Incus sections.
    for name in ("leases.json", "images.json"):
        (state / name).write_text("{")
    result = subprocess.run(["bash", str(script)], env=env, capture_output=True, text=True)
    assert result.returncode == 0 and "leases.json: unreadable" in result.stdout, result
    assert "journalctl ran" in result.stdout, result.stdout
print("ok: failed-lane diagnostics keep the test yard broker state without identities")
PY
