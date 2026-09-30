#!/usr/bin/env python3
"""Freeze a release candidate and run its acceptance checks with local reports."""
import argparse
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import platform
import subprocess
import sys
import tarfile
import tempfile
import time
import threading
import uuid

ROOT = Path(__file__).resolve().parent.parent


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def source_fingerprint(root):
    result = hashlib.sha256()
    paths = subprocess.check_output(["bash", "tests/helpers/source-files.sh"], cwd=root)
    for raw in sorted(filter(None, paths.split(b"\0"))):
        name = os.fsdecode(raw)
        if name.startswith(".subyard-acceptance/"):
            continue
        path = root / name
        if not path.exists() and not path.is_symlink():
            continue
        result.update(raw + b"\0")
        # Git preserves the executable bit, not checkout umask permissions.
        mode = 0o755 if path.lstat().st_mode & 0o111 else 0o644
        result.update(f"{mode:o}\0".encode())
        if path.is_symlink():
            result.update(b"link\0" + os.fsencode(os.readlink(path)))
        else:
            result.update(bytes.fromhex(digest(path)))
    return result.hexdigest()


def clean_environment():
    env = os.environ.copy()
    for key in ("SUBYARD_E2E_CANDIDATE_BUNDLE", "SUBYARD_E2E_CANDIDATE_SHA256",
                "SUBYARD_E2E_CONTROLLER_WORKSPACE",
                "SUBYARD_REPOSITORY_ROOT", "YARD_ENGINE_PATH"):
        env.pop(key, None)
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    return env


def bundle(root, destination, *extra_paths):
    subprocess.run(["bash", "-c", '. "$1"; build_bundle "$2" "$3" "${@:4}"',
                    "release-acceptance", str(root / "dev/agent-e2e.sh"),
                    str(root), str(destination), *extra_paths], cwd=root, env=clean_environment(), check=True)


def profile_inventory(root):
    rows = subprocess.check_output(["bash", "dev/test-profiles.sh", "--e2e", "--list"],
                                   cwd=root, text=True).splitlines()
    result = {}
    for row in rows:
        kind, name, path = row.split("\t")
        if kind not in ("runner", "not-applicable") or name in result:
            raise ValueError("invalid profile acceptance inventory")
        result[name] = {"kind": kind, "path": path}
        if kind == "not-applicable":
            result[name]["reason"] = (root / path).read_text().strip()
    if not result:
        raise ValueError("empty profile acceptance inventory")
    return result


def external_inventory(root):
    result = {}
    for path in sorted(root.glob("config/profiles/*/tests/e2e/external-obligations.json")):
        document = json.loads(path.read_text())
        if document.get("schema_version") != 1 or not document.get("obligations"):
            raise ValueError(f"invalid external obligations: {path.relative_to(root)}")
        for item in document["obligations"]:
            key = path.parts[-4] + ":" + item["id"]
            if key in result or not item.get("description") or not item.get("required_inputs"):
                raise ValueError("invalid or duplicate external obligation")
            result[key] = {**item, "status": "not-run"}
    return result


def commands(root, version, inventory):
    release = root / ".subyard-acceptance/release"
    result = {
        "verify": ["make", "verify"],
        "shellcheck": ["bash", "-c", 'mapfile -t files < <(find scripts dev config/profiles config/agents tests -type f -name "*.sh" -print | sort); shellcheck -x -S warning bin/yard completions/yard.bash "${files[@]}"'],
        "process-coverage": ["bash", "dev/process-coverage.sh"],
        "local-adapters": ["bash", "tests/real-host/adapter-contracts.sh"],
        "compatibility": ["python3", "dev/verify-release-upgrades.py", "--release-dir", str(release), "--version", version],
        "p0-release-smoke": ["bash", "dev/e2e/p0-acceptance.sh"],
    }
    for name, entry in inventory.items():
        result["profile:" + name] = ["bash", entry["path"]] if entry["kind"] == "runner" else None
    return result


def update_result(receipt):
    states = [item["status"] for item in receipt["checks"].values()]
    receipt["reproducible_result"] = (
        "failed" if "failed" in states else
        "passed" if all(state in ("passed", "not-applicable") for state in states) else "incomplete")
    receipt["result"] = receipt["reproducible_result"]
    if receipt["result"] == "passed" and any(item["status"] != "passed" for item in receipt["external_obligations"].values()):
        receipt["result"] = "incomplete"


def save(output, receipt):
    update_result(receipt)
    receipt["updated_at"] = datetime.now(timezone.utc).isoformat()
    temporary = output / "receipt.json.next"
    temporary.write_text(json.dumps(receipt, indent=2, sort_keys=True) + "\n")
    temporary.replace(output / "receipt.json")


def execute(name, command, root, output, env, slot=None):
    log = output / (name.replace(":", "-") + "-" + uuid.uuid4().hex[:8] + ".log")
    argv = command + (["--slot", str(slot)] if slot else [])
    print(f"RUN {name}" + (f" slot={slot}" if slot else ""), flush=True)
    start = time.monotonic()
    with log.open("wb") as stream:
        code = subprocess.run(argv, cwd=root, env=env, stdout=stream,
                              stderr=subprocess.STDOUT, check=False).returncode
    result = {"status": "passed" if code == 0 else "failed", "exit_code": code,
              "duration_seconds": round(time.monotonic() - start, 2), "log": log.name,
              "log_sha256": digest(log)}
    if slot:
        result["slot"] = slot
    print(f"RESULT {name} {result['status']} exit={code} log={log}", flush=True)
    return result


def prepare(output, version):
    output.mkdir(parents=True, exist_ok=True)
    if any(output.iterdir()):
        raise ValueError("prepare requires an empty output directory")
    original = source_fingerprint(ROOT)
    root = output / "source"
    root.mkdir()
    with tempfile.TemporaryDirectory(prefix=".source-", dir=output) as temporary:
        source_archive = Path(temporary) / "source.tar.gz"
        bundle(ROOT, source_archive)
        with tarfile.open(source_archive, "r:gz") as archive:
            for member in archive.getmembers():
                if member.name.startswith("/") or ".." in Path(member.name).parts or not (member.isfile() or member.isdir()):
                    raise ValueError("unsafe source archive member")
            archive.extractall(root, filter="data")
            for member in archive.getmembers():
                path = root / member.name
                path.chmod(0o755 if member.isdir() or member.mode & 0o111 else 0o644)
    if source_fingerprint(root) != original or source_fingerprint(ROOT) != original:
        raise ValueError("source changed while freezing the candidate")
    inventory = profile_inventory(root)  # Fail before packaging or allocating any VM.
    external = external_inventory(root)
    release = root / ".subyard-acceptance/release"
    release.mkdir(parents=True)
    receipt = {"schema_version": 1, "version": version, "source_fingerprint": original,
               "profiles": inventory, "external_obligations": external, "checks": {},
               "environment": {"system": platform.system(), "machine": platform.machine(),
                               "go": subprocess.check_output(["go", "version"], text=True).strip()}}
    for name, command in commands(root, version, inventory).items():
        receipt["checks"][name] = {"status": "pending" if command else "not-applicable"}
        if command is None:
            receipt["checks"][name]["reason"] = inventory[name.split(":", 1)[1]]["reason"]
    receipt["checks"]["package"] = execute("package", ["bash", "dev/package-engine.sh", "--output-dir", str(release), "--version", version], root, output, clean_environment())
    save(output, receipt)
    if receipt["checks"]["package"]["status"] != "passed":
        raise ValueError("packaging failed; see the package log recorded in receipt.json")
    arch = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
    runtime = release / f"subyard-{version}-linux-{arch}.tar.gz"
    receipt["runtime_file"] = runtime.name
    receipt["runtime_sha256"] = digest(runtime)
    receipt["release_files"] = {path.name: digest(path) for path in sorted(release.iterdir()) if path.is_file()}
    metadata = {key: receipt[key] for key in ("version", "runtime_file", "runtime_sha256")}
    (root / ".subyard-acceptance/candidate.json").write_text(json.dumps(metadata) + "\n")
    transport = output / "candidate-bundle.tar.gz"
    bundle(root, transport, ".subyard-acceptance/candidate.json",
           *(".subyard-acceptance/release/" + name for name in receipt["release_files"]))
    receipt["candidate_bundle_sha256"] = digest(transport)
    save(output, receipt)
    print(f"PREPARED {output / 'receipt.json'}", flush=True)


def load_bound(output):
    receipt = json.loads((output / "receipt.json").read_text())
    root = output / "source"
    if receipt.get("schema_version") != 1 or source_fingerprint(root) != receipt["source_fingerprint"]:
        raise ValueError("frozen source fingerprint mismatch")
    if profile_inventory(root) != receipt["profiles"]:
        raise ValueError("profile obligations changed")
    if set(receipt["checks"]) != set(commands(root, receipt["version"], receipt["profiles"])) | {"package"}:
        raise ValueError("required check inventory is incomplete")
    if digest(output / "candidate-bundle.tar.gz") != receipt["candidate_bundle_sha256"]:
        raise ValueError("candidate transport checksum mismatch")
    if Path(receipt["runtime_file"]).name != receipt["runtime_file"]:
        raise ValueError("invalid candidate runtime filename")
    metadata = json.loads((root / ".subyard-acceptance/candidate.json").read_text())
    if metadata != {key: receipt[key] for key in ("version", "runtime_file", "runtime_sha256")}:
        raise ValueError("candidate metadata changed")
    release = root / ".subyard-acceptance/release"
    if {path.name: digest(path) for path in sorted(release.iterdir()) if path.is_file()} != receipt["release_files"]:
        raise ValueError("candidate release assets changed")
    for result in receipt["checks"].values():
        if result["status"] == "passed":
            log = result.get("log", "")
            if not log or Path(log).name != log or digest(output / log) != result.get("log_sha256"):
                raise ValueError("passing result evidence is missing or changed")
    if digest(root / ".subyard-acceptance/release" / receipt["runtime_file"]) != receipt["runtime_sha256"]:
        raise ValueError("candidate runtime checksum mismatch")
    return root, receipt


def run_checks(output, slots, only, rerun):
    root, receipt = load_bound(output)
    checks = commands(root, receipt["version"], receipt["profiles"])
    if only and set(only) - set(checks):
        raise ValueError("unknown --only check: " + ", ".join(sorted(set(only) - set(checks))))
    env = clean_environment()
    pending = [name for name, command in checks.items() if command and (not only or name in only)
               and (rerun or receipt["checks"][name]["status"] != "passed")]
    live = [name for name in pending if name == "p0-release-smoke" or name.startswith("profile:")]
    if live and not slots:
        raise ValueError("live checks require --slots N [N ...]")
    for name in [item for item in pending if item not in live]:
        receipt["checks"][name] = execute(name, checks[name], root, output, env)
        save(output, receipt)
    local_names = [name for name in checks if name != "p0-release-smoke" and not name.startswith("profile:")]
    if live and any(receipt["checks"][name]["status"] != "passed" for name in local_names):
        print("INCOMPLETE: live checks await successful local gates", flush=True)
        return 1
    # Only physical controllers consume the frozen transport. Host-free tests build
    # synthetic worktrees and must retain the normal bundler contract.
    env.update(SUBYARD_E2E_CANDIDATE_BUNDLE=str(output / "candidate-bundle.tar.gz"),
               SUBYARD_E2E_CANDIDATE_SHA256=receipt["candidate_bundle_sha256"],
               SUBYARD_E2E_CONTROLLER_WORKSPACE=str(ROOT))
    # Each slot owns its sequential queue; persist every result as it completes.
    lock = threading.Lock()
    def collect(slot, names):
        for name in names:
            result = execute(name, checks[name], root, output, env, slot)
            with lock:
                receipt["checks"][name] = result
                save(output, receipt)
    with ThreadPoolExecutor(max_workers=max(1, len(slots))) as pool:
        futures = [pool.submit(collect, slot, live[index::len(slots)]) for index, slot in enumerate(slots)]
        for future in as_completed(futures):
            future.result()
    if source_fingerprint(root) != receipt["source_fingerprint"]:
        receipt["checks"]["verify"]["status"] = "failed"
        receipt["error"] = "source changed during acceptance"
    save(output, receipt)
    print(f"ACCEPTANCE reproducible={receipt['reproducible_result']} overall={receipt['result']} receipt={output / 'receipt.json'}", flush=True)
    return 0 if receipt["result"] == "passed" else 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="action", required=True)
    prep = sub.add_parser("prepare", help="retain an immutable source/artifact candidate")
    prep.add_argument("--version", required=True)
    prep.add_argument("--output", type=Path, required=True)
    run = sub.add_parser("run", help="run pending checks; each live retry acquires fresh VMs")
    run.add_argument("--output", type=Path, required=True)
    run.add_argument("--slots", type=int, nargs="+", default=[])
    run.add_argument("--only", action="append", default=[])
    run.add_argument("--rerun", action="store_true")
    args = parser.parse_args()
    output = args.output.resolve()
    if output == ROOT or (ROOT in output.parents and ROOT / ".build" not in output.parents):
        parser.error("output must be under .build or outside the source checkout")
    if args.action == "prepare":
        if not args.version or any(c not in "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._+-" for c in args.version):
            parser.error("unsafe version")
        prepare(output, args.version)
        return 0
    if len(set(args.slots)) != len(args.slots) or any(slot < 1 or slot > 999 for slot in args.slots):
        parser.error("slots must be unique integers from 1 to 999")
    return run_checks(output, args.slots, args.only, args.rerun)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ValueError, KeyError, OSError, subprocess.SubprocessError) as error:
        print(f"release-acceptance: {error}", file=sys.stderr)
        sys.exit(2)
