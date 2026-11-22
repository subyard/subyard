#!/usr/bin/env python3
"""Print the periodic E2E matrices from release owners and periodic-only checks."""
import argparse
import importlib.util
import json
import pathlib
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
SLOT = ["--slot", "1"]
IMAGES = ("debian-13", "ubuntu-24.04", "ubuntu-26.04")


def optional_vm(name, duration, *guest_env):
    return {"id": "periodic:" + name, "optional": True,
            "command": ["timeout", "--signal=TERM", "--kill-after=600s", duration,
                        "dev/agent-e2e.sh", *SLOT, "--vm-count", "1", "--vm", "1",
                        "--purpose", name, "--", *guest_env, "bash", f"dev/e2e/{name}.sh"]}


# Checks that boot a VM inside the leased VM. On AMD hosted runners such a VM boots far too slowly
# under the runner's hypervisor for these checks to finish, so they run only on Intel runners.
NESTED = {"p0-release-smoke", "p0:release", "p0:nested-teardown", "profile:amnezia",
          "profile:telemetry/local", "profile:telemetry/resources",
          "profile:android/gradle", "profile:android/memory-lease", "profile:test-vms/broker-cold-upgrade", "profile:test-vms/memory-boundary",
          "vm:staging-reliability",
          "vm:p0-yard-maintenance"}
# The release catalog runs the full P0 matrix as one check; here its lanes run on separate runners.
SPLIT = {"vm:p0-full"}
# A hosted run took 17 minutes; 45m leaves room for slower runners. The 10m TERM grace covers
# two 120s guest cleanups and the 180s lease release.
PERIODIC_ONLY = [{"id": "periodic:config-sync",
                  "command": ["timeout", "--signal=TERM", "--kill-after=600s", "45m",
                              "bash", "dev/e2e/config-sync-acceptance.sh", *SLOT]},
                 # Physical regression outside the release acceptance catalog; uses the normal candidate bundle.
                 {"id": "vm:incus-apparmor-probe",
                  "command": ["dev/agent-e2e.sh", "--slot", "1", "--vm-count", "1", "--vm", "1",
                              "--purpose", "apparmor-probe", "--", "bash", "dev/e2e/incus-apparmor-probe.sh"]},
                 optional_vm("cleanup-acceptance", "60m", "env",
                             "SUBYARD_E2E_CLEANUP_CACHE_HELPER=config/profiles/subyard-dev/tests/e2e/cleanup-cache.sh"),
                 optional_vm("sudo-terminal-ownership", "45m"),
                 optional_vm("zabbly-download-smoke", "15m")]
# Both NM guard lanes keep their ordinary candidate bundle and own their lease/reboots.
PERIODIC_ONLY += [{"id": f"periodic:nm-guard/{lane}", "optional": True,
                   "command": ["timeout", "--signal=TERM", "--kill-after=600s", "90m",
                               "bash", "dev/e2e/nm-admin-uplink.sh", *SLOT,
                               "--lane", lane]}
                  for lane in ("fresh", "upgrade")]
# Veranda GUI SHORT scenarios (veranda/desktop/README.md). They gate no CLI or engine release, so
# they are periodic only and use their own bundle, which adds the GUI and engine ELFs. A manual
# check runs only when a dispatch names it.
VERANDA = {"veranda:x11": ["--x11"], "veranda:wayland": []}
VERANDA_MANUAL = {"veranda:x11-remote-ui": ["--x11", "--remote", "--ui"], "veranda:wayland-ime": ["--ime"]}


def collector():
    spec = importlib.util.spec_from_file_location("release_acceptance", ROOT / "dev/release-acceptance.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def published_upgrade(version, agents):
    return ["dev/agent-e2e.sh", "--slot", "1", "--purpose", "published-upgrade", "--vm-count", "1",
            "--vm", "1", "--", "bash", "dev/e2e/published-upgrade.sh", version, agents]


def veranda_short(name, options):
    return {"id": name, "bundle": "veranda-bundle",
            "command": ["dev/agent-e2e.sh", "--slot", "1", "--vm-count", "1",
                        "--purpose", name.replace(":", "-"), "--", "bash", "dev/e2e/veranda-short.sh", *options]}


def plan(root=ROOT, image=IMAGES[0]):
    module = collector()
    inventory = module.profile_inventory(root)
    declarations = module.check_declarations(root, inventory)
    declared = module.commands(root, "0.0.0", inventory, periodic=True)
    # The same release checks the collector runs: every shipped profile lane, the core VM catalog
    # with its agent upgrade baselines, and the release smoke check, plus periodic-only lanes.
    include = [{"id": name, "command": command + SLOT} for name, command in declared.items()
               if command and module.is_live(name) and name not in SPLIT]
    lanes = subprocess.check_output(["bash", "dev/e2e/p0-acceptance.sh", "--list-lanes"],
                                    cwd=root, text=True).splitlines()
    full = [line.split("\t", 1)[1].split() for line in lanes if line.startswith("full\t")]
    if len(full) != 1 or not full[0]:
        raise SystemExit("plan-periodic-checks: P0 full lane inventory is missing")
    # Release acceptance runs these full lanes as their own catalog checks; here they keep their p0: lane run.
    split = [line.split("\t", 1)[1].split() for line in lanes if line.startswith("split-covered\t")]
    split_lanes = split[0] if len(split) == 1 else []
    split_checks = {}
    catalog = module.core_vm_catalog(root)
    for entry in catalog:
        command = entry["command"]
        if command[:3] == ["bash", "dev/e2e/p0-acceptance.sh", "--lane"] and len(command) == 4 and command[3] in split_lanes:
            split_checks.setdefault(command[3], []).append("vm:" + entry["id"])
    # The collector counts the split-covered full run only together with exactly these checks.
    owners = [set(required) for name, required in module.core_vm_requirements(root).items()]
    if (not split_lanes or not set(split_lanes) <= set(full[0])
            or any(len(split_checks.get(lane, [])) != 1 for lane in split_lanes)
            or owners != [{checks[0] for checks in split_checks.values()}]):
        raise SystemExit("plan-periodic-checks: each P0 split lane needs exactly one release catalog check, "
                         "all required by the split-covered full check")
    include = [check for check in include if [check["id"]] not in split_checks.values()]
    # The workflow's pool gate already runs boundary. The cleanup lane verifies a previous
    # lane's leftovers, and a fresh runner has none.
    include += [{"id": "p0:" + lane, "command": ["bash", "dev/e2e/p0-acceptance.sh", "--lane", lane, *SLOT]}
                for lane in full[0] if lane not in ("boundary", "cleanup")]
    # The release catalog upgrades a few baselines with agents; the rest keep an agent-free run here.
    baselines = (root / "dev/e2e/published-upgrade-baselines.tsv").read_text().splitlines()
    for line in baselines:
        if not line:
            continue
        version, _, _, expectation = line.split("\t")
        if expectation == "pin":
            continue
        catalog_version = expectation if expectation in ("anchor", "hop") else version
        if "vm:published-upgrade/" + catalog_version not in declared:
            include.append({"id": "published:" + version,
                            "command": published_upgrade(version, "none" if expectation in ("direct", "anchor", "hop") else "refusal")})
    include += [{"id": "p0:init-settings", "manual": True,
                 "command": ["bash", "dev/e2e/p0-acceptance.sh", "--lane", "init-settings", *SLOT]}]
    include += PERIODIC_ONLY
    include += [veranda_short(name, options) for name, options in VERANDA.items()]
    include += [veranda_short(name, options) | {"manual": True} for name, options in VERANDA_MANUAL.items()]
    missing = sorted(SPLIT - set(declared))
    if missing:
        raise SystemExit("plan-periodic-checks: split checks left the collector: " + " ".join(missing))
    unknown = sorted(NESTED - {check["id"] for check in include})
    if unknown:
        raise SystemExit("plan-periodic-checks: unknown nested checks: " + " ".join(unknown))
    # Every planned command leases through the hosted controller. Its image flag
    # applies to every acquisition without changing the release lane entrypoints.
    include = [check | {"tier": module.resolve_tier(declarations.get(check["id"], {})),
                        "command": ["--image", image, *check["command"]]} for check in include]
    return {"checks": {"include": [c for c in include if c["id"] not in NESTED]},
            "nested": {"include": [c for c in include if c["id"] in NESTED]}}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", choices=IMAGES, default=IMAGES[0])
    json.dump(plan(image=parser.parse_args().image), sys.stdout, separators=(",", ":"))
    print()
