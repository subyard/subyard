#!/usr/bin/env python3
"""Export bounded collector evidence and return the exact hosted selection's outcome."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import shutil

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("acceptance", ROOT / "dev/release-acceptance.py")
a = importlib.util.module_from_spec(spec)
spec.loader.exec_module(a)


def export(output, destination, check):
    # The native aggregate may remain incomplete because other checks were unrequested.
    # Preserve that receipt; job success follows only the terminal selected invocation.
    destination.mkdir(parents=True, exist_ok=False)
    execution = {"run_id": int(os.environ["GITHUB_RUN_ID"]), "run_attempt": int(os.environ["GITHUB_RUN_ATTEMPT"]),
                 "head_sha": os.environ["GITHUB_SHA"], "check": check}
    (destination / "ci-execution.json").write_text(json.dumps(execution))
    total = 0
    for pattern in ("receipt.json", "events.jsonl", "*.log", "attempts/*.json", "manifests/*.json", "runs/*.json"):
        for path in sorted(output.glob(pattern)):
            size = path.stat().st_size
            if path.is_symlink() or not path.is_file() or size > 20 << 20:
                raise ValueError("hosted evidence is not a bounded regular file")
            total += size
            if total > 128 << 20:
                raise ValueError("hosted evidence exceeds total byte limit")
            target = destination / path.relative_to(output)
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(path, target)
    root, receipt = a.load_bound(output)
    result = receipt["checks"][check]
    record, manifest = a.original_attempt(output, receipt, check, result)
    a.validate_attempt(record, manifest, check, a.command_identity(a.commands(root, receipt["version"], receipt["profiles"])[check], root))
    invocations = [json.loads(path.read_text()) for path in output.glob("runs/*.json")]
    if (len(invocations) != 1 or invocations[0].get("state") != "finished"
        or invocations[0].get("capture_status") != "recorded"
        or invocations[0].get("selected") != [check] or invocations[0].get("selected_status") != "passed"
        or result["status"] != "passed" or result.get("cleanup_status") != "recorded"
        or not result.get("cleanup")):
        raise ValueError("hosted collector selection did not finish with a complete PASS and cleanup")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--destination", type=Path, required=True)
    parser.add_argument("--check", required=True, choices=sorted(a.CI_HOSTED_CHECKS))
    args = parser.parse_args()
    export(args.output.resolve(), args.destination.resolve(), args.check)
