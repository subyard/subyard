#!/usr/bin/env python3
"""Check the installed producer against complete real native records, read-only."""
import copy
import json
from pathlib import Path
import subprocess
import sys

helper, snapshot_file, container_file = map(Path, sys.argv[1:])
snapshot, native = snapshot_file.read_text(), container_file.read_text()
containers = json.loads(native)
if len(containers) != 1 or len(containers[0]["Mounts"]) < 2:
    raise SystemExit("native fixture needs one container with multiple mounts")


def observe(selected, container_json):
    request = {"snapshot": snapshot, "container": container_json,
               "service": "synthetic owned controls; native controls checked by lifecycle acceptance\n",
               "installed": False, "selected": selected, "owner_host": "127.0.0.1",
               "environment": {"YARD_KIND": "container", "AI_OBSERVER_HOST_PORT": "22222",
                               "SUBYARD_INTEGRATION_SOURCE_BINDING": "a" * 64}, "provision": ""}
    result = subprocess.run([str(helper), "observe"], input=json.dumps(request),
                            capture_output=True, text=True, timeout=10)
    if result.returncode:
        raise SystemExit("installed native producer refused its real observations")
    return json.loads(result.stdout)["observation"]


for selected in (True, False):
    original = observe(selected, native)
    if original["state"] != "stale" or observe(selected, json.dumps(containers)) != original:
        raise SystemExit("native fixture did not retain its exact protected values")
    permuted = copy.deepcopy(containers)
    permuted[0]["Mounts"].reverse()
    if observe(selected, json.dumps(permuted)) != original:
        raise SystemExit("real mount enumeration changed the installed fingerprint")
    for group in ("Mounts", "HostConfig"):
        changed = copy.deepcopy(containers)
        target = changed[0][group][0] if group == "Mounts" else changed[0][group]
        target["AdditionalNativeField"] = {"count": 9007199254740993}
        if observe(selected, json.dumps(changed))["actual"] == original["actual"]:
            raise SystemExit("added native mount property did not bind drift")
    duplicate = copy.deepcopy(containers)
    duplicate[0]["Mounts"].append(copy.deepcopy(duplicate[0]["Mounts"][0]))
    if observe(selected, json.dumps(duplicate))["actual"] == original["actual"]:
        raise SystemExit("real mount multiplicity did not bind drift")
print("installed producer: real mount ordering, additive fields and multiplicity passed")
