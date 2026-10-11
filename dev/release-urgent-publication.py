#!/usr/bin/env python3
"""Publish exact prepared bytes under explicit authorization for unfinished acceptance."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import sys
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location("publication", ROOT / "dev/release-publication.py")
p = importlib.util.module_from_spec(spec)
spec.loader.exec_module(p)


def validate(document, identity, root):
    expected = {"schema_version", "candidate_id", "identity_sha256", "receipt_sha256",
                "environment", "checks", "external_obligations", "authorization"}
    if (set(document) != expected or type(document["schema_version"]) is not int
        or document["schema_version"] != 1 or document["candidate_id"] != identity["candidate_id"]
        or document["identity_sha256"] != p.identity_hash(identity)
        or not re.fullmatch(r"[0-9a-f]{64}", document["receipt_sha256"])):
        raise ValueError("urgent publication identity mismatch")
    authorization = document["authorization"]
    if (not isinstance(authorization, dict)
        or set(authorization) != {"reference", "reason", "identity_sha256", "deferred_checks"}
        or authorization["identity_sha256"] != p.identity_hash(identity)):
        raise ValueError("explicit candidate-bound urgent authorization is required")
    for field, limit in (("reference", 256), ("reason", 1024)):
        value = authorization[field]
        if (not isinstance(value, str) or not value.strip() or len(value) > limit
            or any(ord(character) < 32 for character in value)):
            raise ValueError("invalid public authorization reference or reason")
    environment = document["environment"]
    if (not isinstance(environment, dict) or set(environment) != {"system", "machine", "go"}
        or any(not isinstance(value, str) or not value or len(value) > 256
               or any(ord(character) < 32 for character in value) for value in environment.values())):
        raise ValueError("acceptance environment missing")
    required = p.a.commands(root, identity["version"], p.a.profile_inventory(root))
    required["package"] = ["ci-prepared"]
    if set(document["checks"]) != set(required):
        raise ValueError("urgent check inventory mismatch")
    unfinished = []
    for name, command in required.items():
        result = document["checks"][name]
        if command is None:
            if result != {"status": "not-applicable"}:
                raise ValueError("invalid not-applicable check")
        elif result.get("status") in ("pending", "running", "interrupted", "blocked"):
            if set(result) != {"status"}:
                raise ValueError("unfinished check must not claim passing evidence")
            unfinished.append(name)
        elif result.get("status") == "passed":
            keys = {"status", "log_sha256", "exit_code"} | ({"environments"} if p.a.is_live(name) else set())
            if (set(result) != keys or type(result["exit_code"]) is not int or result["exit_code"] != 0
                or not re.fullmatch(r"[0-9a-f]{64}", result["log_sha256"])):
                raise ValueError("passing check lacks evidence")
            if p.a.is_live(name):
                environments = result["environments"]
                if not isinstance(environments, list) or not environments:
                    raise ValueError("passing physical check lacks environment evidence")
                for item in environments:
                    if (not isinstance(item, dict) or set(item) != {"type", "vm_count", "base_fingerprint"}
                        or not re.fullmatch(r"[a-z][a-z0-9-]{0,79}", item["type"])
                        or type(item["vm_count"]) is not int or not 1 <= item["vm_count"] <= 100
                        or not re.fullmatch(r"[0-9a-f]{64}", item["base_fingerprint"])):
                        raise ValueError("invalid physical acceptance environment")
        else:
            raise ValueError("failed or unknown check cannot be deferred")
    if authorization["deferred_checks"] != sorted(unfinished) or not unfinished:
        raise ValueError("authorization must name exactly every unfinished check")
    if any(document["checks"][name]["status"] != "passed" for name in ("package", "verify")):
        raise ValueError("prepared package and host-free verify must pass before urgent publication")
    if set(document["external_obligations"]) != set(p.a.external_inventory(root)):
        raise ValueError("external inventory mismatch")
    for result in document["external_obligations"].values():
        if "deferral" in result:
            if set(result) != {"status", "deferral"}:
                raise ValueError("invalid external deferral")
            p.a.validate_external_deferral(result, p.identity_hash(identity))
        elif (set(result) != {"status", "evidence_sha256"} or result["status"] != "passed"
              or not re.fullmatch(r"[0-9a-f]{64}", result["evidence_sha256"])):
            raise ValueError("external check lacks passing evidence or explicit deferral")


def snapshot(output, prepared, reference, reason):
    identity = p.checked_identity(prepared)
    # Receipt publication is atomic. Validate a stable read without taking the active runner's lock.
    for _ in range(3):
        before = (output / "receipt.json").read_bytes()
        root, receipt = p.a.load_bound(output)
        after = (output / "receipt.json").read_bytes()
        if before == after and receipt == json.loads(before):
            break
    else:
        raise ValueError("acceptance snapshot changed; retry without changing the collector")
    if receipt.get("superseded") or receipt.get("preparation") != identity:
        raise ValueError("receipt is superseded or belongs to another candidate")
    checks = {}
    required = p.a.commands(root, identity["version"], receipt["profiles"])
    for name, result in receipt["checks"].items():
        checks[name] = {"status": result["status"]}
        if result["status"] == "passed":
            if (not p.a.is_live(name) and result.get("execution_environment") != receipt["environment"]):
                raise ValueError("passing local execution environment differs")
            if name != "package" and result.get("command") != p.a.command_identity(required[name], root):
                raise ValueError("passing command lacks exact provenance")
            checks[name].update(log_sha256=result["log_sha256"], exit_code=result["exit_code"])
            if p.a.is_live(name):
                checks[name]["environments"] = result.get("environments", [])
    external = {}
    for name, result in receipt["external_obligations"].items():
        external[name] = ({"status": "not-run", "deferral": result["deferral"]} if "deferral" in result
                          else {"status": result["status"], "evidence_sha256": result.get("evidence_sha256", "")})
    document = {"schema_version": 1, "candidate_id": identity["candidate_id"],
                "identity_sha256": p.identity_hash(identity),
                "receipt_sha256": hashlib.sha256(before).hexdigest(), "environment": receipt["environment"],
                "checks": checks, "external_obligations": external,
                "authorization": {"reference": reference, "reason": reason,
                    "identity_sha256": p.identity_hash(identity),
                    "deferred_checks": sorted(name for name, item in checks.items()
                                              if item["status"] not in ("passed", "not-applicable"))}}
    validate(document, identity, root)
    return document


def run(request):
    if (os.environ.get("GITHUB_ACTIONS") != "true"
        or os.environ.get("GITHUB_WORKFLOW") != "Urgent release publication"
        or os.environ.get("GITHUB_EVENT_NAME") != "push"
        or not os.environ.get("GITHUB_REF", "").startswith("refs/heads/release/urgent-publication/")):
        raise ValueError("urgent publication requires its serialized authorized branch workflow")
    document = json.loads(request.read_text())
    github = p.GitHub(os.environ["GITHUB_REPOSITORY"])
    with tempfile.TemporaryDirectory(prefix="subyard-urgent-publication-", dir="/tmp") as temporary:
        prepared = Path(temporary) / "prepared"
        identity = p.fetch(github, document["candidate_id"], prepared)
        if os.environ["GITHUB_REF"] != "refs/heads/release/urgent-publication/" + identity["version"]:
            raise ValueError("urgent branch does not match the prepared version")
        source = Path(temporary) / "source"
        source.mkdir(mode=0o700)
        with tarfile.open(prepared / "source.tar.gz", "r:gz") as archive:
            archive.extractall(source, filter="data")
        if p.a.source_fingerprint(source) != identity["source_fingerprint"]:
            raise ValueError("frozen source fingerprint mismatch")
        validate(document, identity, source)
        print("PUBLICATION urgent authorization=" + document["authorization"]["reference"]
              + " deferred_checks=" + ",".join(document["authorization"]["deferred_checks"]), flush=True)
        release = p.execute_publication(github, prepared, identity)
        print("PUBLISHED " + release["html_url"] + " acceptance=incomplete", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    capture = commands.add_parser("snapshot", help="capture truthful bound evidence without interrupting acceptance")
    capture.add_argument("--acceptance", type=Path, required=True)
    capture.add_argument("--prepared", type=Path, required=True)
    capture.add_argument("--authorization-reference", required=True)
    capture.add_argument("--reason", required=True)
    capture.add_argument("--output", type=Path, required=True)
    publication = commands.add_parser("run", help="serialized Actions-only publication with explicit deferral")
    publication.add_argument("--request", type=Path, required=True)
    args = parser.parse_args()
    if args.command == "snapshot":
        p.write_json(args.output, snapshot(args.acceptance, args.prepared, args.authorization_reference, args.reason))
        print("SNAPSHOT " + str(args.output))
    else:
        run(args.request)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, TypeError, OSError) as error:
        print("urgent release: " + str(error), file=sys.stderr)
        sys.exit(2)
