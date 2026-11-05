#!/usr/bin/env python3
"""Print the GitHub Actions matrix of release VM checks from their existing owners."""
import importlib.util
import json
import pathlib
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
SLOT = ["--slot", "1"]
# Agent jobs selected by docs/test-vms.md: each ownership-evidence boundary, without the full matrix.
AGENT_BASELINES = {"0.11.0": "all", "0.17.1": "all", "0.22.1": "all", "0.25.5": "all",
                   "0.23.0": "claude", "0.24.0": "claude"}
# Checks that boot a VM inside the leased VM. On AMD hosted runners such a VM boots far too slowly
# under the runner's hypervisor for these checks to finish, so they run only on Intel runners.
NESTED = {"p0-release-smoke", "p0:release", "p0:nested-teardown", "profile:amnezia",
          "profile:telemetry", "profile:test-vms/broker-cold-upgrade"}


def collector():
    spec = importlib.util.spec_from_file_location("release_acceptance", ROOT / "dev/release-acceptance.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def published_upgrade(version, agents):
    return ["dev/agent-e2e.sh", "--slot", "1", "--purpose", "published-upgrade", "--vm-count", "1",
            "--vm", "1", "--", "bash", "dev/e2e/published-upgrade.sh", version, agents]


def plan(root=ROOT):
    module = collector()
    declared = module.commands(root, "0.0.0", module.profile_inventory(root))
    # The same release checks the collector runs, including every shipped profile lane.
    include = [{"id": name, "command": command + SLOT} for name, command in declared.items()
               if command and module.is_live(name)]
    lanes = subprocess.check_output(["bash", "dev/e2e/p0-acceptance.sh", "--list-lanes"],
                                    cwd=root, text=True).splitlines()
    full = [line.split("\t", 1)[1].split() for line in lanes if line.startswith("full\t")]
    if len(full) != 1 or not full[0]:
        raise SystemExit("plan-release-checks: P0 full lane inventory is missing")
    # The workflow's pool gate already runs boundary. The cleanup lane verifies a previous
    # lane's leftovers, and a fresh runner has none.
    include += [{"id": "p0:" + lane, "command": ["bash", "dev/e2e/p0-acceptance.sh", "--lane", lane, *SLOT]}
                for lane in full[0] if lane not in ("boundary", "cleanup")]
    baselines = (root / "dev/e2e/published-upgrade-baselines.tsv").read_text().splitlines()
    versions = [line.split("\t", 1)[0] for line in baselines if line]
    include += [{"id": "published:" + version, "command": published_upgrade(version, "none")}
                for version in versions]
    missing = sorted(set(AGENT_BASELINES) - set(versions))
    if missing:
        raise SystemExit("plan-release-checks: unpinned agent baselines: " + " ".join(missing))
    include += [{"id": f"published:{version}:{agents}", "command": published_upgrade(version, agents)}
                for version, agents in AGENT_BASELINES.items()]
    unknown = sorted(NESTED - {check["id"] for check in include})
    if unknown:
        raise SystemExit("plan-release-checks: unknown nested checks: " + " ".join(unknown))
    return {"checks": {"include": [c for c in include if c["id"] not in NESTED]},
            "nested": {"include": [c for c in include if c["id"] in NESTED]}}


if __name__ == "__main__":
    json.dump(plan(), sys.stdout, separators=(",", ":"))
    print()
