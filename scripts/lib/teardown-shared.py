#!/usr/bin/env python3
"""Retire exact unused host resources after all approved yard projects vanish."""
import importlib.util
import os
import subprocess
import sys

spec = importlib.util.spec_from_file_location("guard", os.path.join(os.path.dirname(__file__), "teardown-plan.py"))
guard = importlib.util.module_from_spec(spec)
spec.loader.exec_module(guard)


def present(kind, name):
    endpoint = "/1.0/storage-pools" if kind == "pool" else "/1.0/networks"
    values = guard.query(endpoint, "default")
    return any(value.split("?")[0].endswith("/" + name) for value in values)


def main(selected=False):
    resources = [item for item in guard.inventory() if item["kind"] in ("host-profile", "pool", "network")]
    if not any(item["kind"] != "host-profile" for item in resources):
        return
    profiles = [item for item in resources if item["kind"] == "host-profile"]
    if len(profiles) != 1:
        raise ValueError("missing exact host default profile")
    profile = guard.query("/1.0/profiles/default", "default")
    candidates = []
    for resource in resources:
        kind, name = resource["kind"], resource["name"]
        if kind == "host-profile":
            continue
        if kind not in ("pool", "network"):
            raise ValueError("invalid full shared target")
        if not present(kind, name):
            if kind == "network":
                print(name)
            continue
        guard.guard(resource, "default")
        endpoint = "/1.0/storage-pools/" if kind == "pool" else "/1.0/networks/"
        actual = guard.query(endpoint + name, "default")
        for consumer in actual.get("used_by", []):
            if consumer.split("?")[0] != "/1.0/profiles/default" or profile.get("used_by"):
                if selected:
                    break
                raise ValueError("approved shared resource acquired a foreign consumer")
        else:
            candidates.append(resource)
            continue
        continue
    if candidates:
        guard.guard(profiles[0], "default")
    # Only unused exact matching default devices are eligible. Never remove a
    # device from a profile which a surviving foreign workload still consumes.
    for resource in candidates:
        kind, name = resource["kind"], resource["name"]
        device, property_name = ("root", "pool") if kind == "pool" else ("eth0", "network")
        if profile.get("devices", {}).get(device, {}).get(property_name) == name:
            if profile.get("used_by"):
                raise ValueError("foreign workload consumes the host default profile")
            subprocess.check_call(["incus", "profile", "device", "remove", "default", device, "--project", "default"], stdout=subprocess.DEVNULL)
            profile["devices"].pop(device)
        guard.guard(resource, "default")
        command = ["incus", "storage" if kind == "pool" else "network", "delete", name, "--project", "default"]
        subprocess.check_call(command, stdout=subprocess.DEVNULL)
        if present(kind, name):
            raise ValueError("approved shared resource remains after apply")
        if kind == "network":
            print(name)


if __name__ == "__main__":
    try:
        main(selected=sys.argv[1:] == ["--selected"])
    except (OSError, ValueError, KeyError, TypeError, subprocess.CalledProcessError):
        print("full uninstall shared ownership/consumer verification failed; preserve backing data and resume the exact saved plan", file=sys.stderr)
        sys.exit(75)
