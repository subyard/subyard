#!/usr/bin/env python3
"""Run only approved shipped owner hooks; never print private records."""
import json
import importlib.util
import pathlib
import re
import subprocess
import sys


spec = importlib.util.spec_from_file_location("guard", pathlib.Path(__file__).with_name("teardown-plan.py"))
guard = importlib.util.module_from_spec(spec)
spec.loader.exec_module(guard)

def verify():
    root = pathlib.Path(__file__).resolve().parents[2]
    checks = guard.read_plan()[1].get("ProfileChecks") or []
    if not isinstance(checks, list) or len(checks) > 128:
        raise ValueError("invalid conditional owner inventory")
    for check in checks:
        if set(check) != {"Profile", "Hook", "Environment"} or not re.fullmatch(r"[a-z][a-z0-9-]*", check["Profile"]):
            raise ValueError("invalid conditional owner identity")
        directory = root / "config/profiles" / check["Profile"]
        declaration = json.loads((directory / "profile.json").read_bytes())
        hook = directory / check["Hook"]
        if declaration.get("full_cleanup") != check["Hook"] or hook.is_symlink() or not hook.is_file() or hook.resolve() != hook.absolute() or not hook.is_relative_to(directory):
            raise ValueError("conditional owner hook differs from its declaration")
        environment = check["Environment"]
        if not isinstance(environment, dict) or len(environment) > 2048 or not all(isinstance(k, str) and isinstance(v, str) for k, v in environment.items()):
            raise ValueError("invalid conditional owner context")
        environment = dict(environment, PYTHONDONTWRITEBYTECODE="1", SUBYARD_FULL_CLEANUP_READ_ONLY="1")
        result = subprocess.run([str(hook), "--verify-private"], env=environment, cwd=root, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=30)
        if result.returncode != 0 or result.stdout.strip() != b'{"verified": true}':
            print("profile " + check["Profile"] + " private producer ownership could not be proven; preserve its producers and evidence and restore the approved source inventory", file=sys.stderr)
            raise ValueError("conditional owner proof failed for " + check["Profile"] + "; preserve its producers and data and restore the approved source inventory")


if __name__ == "__main__":
    try:
        verify()
    except (ValueError, OSError, KeyError, subprocess.TimeoutExpired):
        print("full uninstall conditional ownership check failed; preserve dependent resources and data", file=sys.stderr)
        raise SystemExit(75)
