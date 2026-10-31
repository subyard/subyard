#!/usr/bin/env python3
"""Public allocation scope for operator service stop/removal confirmation."""
import argparse
import hashlib
import json
import re
import subprocess
import sys


def project_pool(response):
    pool = response.get("pool")
    if response.get("status") != "ok" or not isinstance(pool, dict) or pool.get("resource_type") != "agent-e2e" or pool.get("resource_id") != "test-vms":
        raise ValueError("invalid broker status")
    result = []
    for slot in pool.get("slots", []):
        identity = {key: slot.get(key) for key in ("slot_id", "resource_generation", "lease_epoch", "state", "lease_id")}
        identity["lease_id"] = identity["lease_id"] or ""
        if (not isinstance(identity["slot_id"], str) or not re.fullmatch(r"slot-[0-9]{3}", identity["slot_id"]) or
                any(not isinstance(identity[key], int) or identity[key] < 0 for key in ("resource_generation", "lease_epoch")) or
                identity["state"] not in ("available", "provisioning", "held", "draining", "quarantined", "recovering", "unavailable")):
            raise ValueError("invalid broker allocation identity")
        result.append(identity)
    if not result or len(result) > 999 or len({slot["slot_id"] for slot in result}) != len(result):
        raise ValueError("invalid broker slot inventory")
    result.sort(key=lambda slot: slot["slot_id"])
    binding = hashlib.sha256(json.dumps(result, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    facts = [f"slot={slot['slot_id']} resource_generation={slot['resource_generation']} lease_epoch={slot['lease_epoch']} state={slot['state']}" for slot in result]
    return dict(schema_version=1, binding=binding, facts=facts)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--project", required=True)
    parser.add_argument("--instance", required=True)
    parser.add_argument("--apply-binding")
    parser.add_argument("--resume", action="store_true")
    args = parser.parse_args()
    if any(not re.fullmatch(r"[a-z0-9][a-z0-9_-]*", value) for value in (args.project, args.instance)):
        sys.exit("service allocation binding unavailable")
    try:
        if args.apply_binding or args.resume:
            if args.apply_binding and not re.fullmatch(r"[0-9a-f]{64}", args.apply_binding):
                raise ValueError("invalid binding")
            action = "fence-pool-expected-" + args.apply_binding if args.apply_binding else "resume-admission"
            result = subprocess.run(["incus", "exec", args.instance, "--project", args.project, "--",
                                     "/usr/local/libexec/subyard/test-vms-inner", "_test-vms-worker", action],
                                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=8)
            if result.returncode == 75:
                sys.exit(75)
            if result.returncode:
                sys.exit("service allocation fencing unavailable; update the installed broker with service update")
            return
        response = subprocess.run(["incus", "exec", args.instance, "--project", args.project, "--",
                                   "/usr/local/libexec/subyard/test-vms-inner", "_test-vms-worker", "status"],
                                  stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=8, check=True)
        if len(response.stdout) > 65536:
            raise ValueError("oversized broker status")
        print(json.dumps(project_pool(json.loads(response.stdout))))
    except (OSError, subprocess.SubprocessError, ValueError, TypeError, KeyError):
        sys.exit("service allocation binding unavailable")


if __name__ == "__main__":
    main()
