#!/usr/bin/env python3
"""Freeze a release candidate and run its acceptance checks with local reports."""
import argparse
from concurrent.futures import ThreadPoolExecutor, as_completed
from contextlib import contextmanager
from datetime import datetime, timezone
import fcntl
import hashlib
import json
import os
from pathlib import Path
import platform
import queue
import re
import shutil
import signal
import subprocess
import sys
import tarfile
import tempfile
import time
import threading
import uuid

ROOT = Path(__file__).resolve().parent.parent
EVENT_LOCK = threading.Lock()
STOP = threading.Event()
HEARTBEAT_SECONDS = 60
TERM_GRACE_SECONDS = 30
KILL_GRACE_SECONDS = 10


@contextmanager
def acceptance_lock(output, shared=False):
    with private_file(output / "collector.lock", "a") as stream:
        try:
            fcntl.flock(stream, (fcntl.LOCK_SH if shared else fcntl.LOCK_EX) | fcntl.LOCK_NB)
        except BlockingIOError:
            raise ValueError("candidate is already in use by another collector") from None
        yield


def private_file(path, mode="w"):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | (os.O_APPEND if "a" in mode else os.O_TRUNC), 0o600)
    os.fchmod(fd, 0o600)
    return os.fdopen(fd, mode)


def sync_directory(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def event(output, kind, **fields):
    # Only collector-owned fields and validated structured markers enter events.
    with EVENT_LOCK, private_file(output / "events.jsonl", "a") as stream:
        stream.write(json.dumps({"time": datetime.now(timezone.utc).isoformat(),
                                 "event": kind, **fields}, sort_keys=True) + "\n")
        stream.flush()
        os.fsync(stream.fileno())
        sync_directory(output)


def command_identity(command, root):
    return [argument.replace(str(root), "${SOURCE}") for argument in command]


def execution_environment():
    return {"system": platform.system(), "machine": platform.machine(),
            "go": subprocess.check_output(["go", "version"], text=True, stderr=subprocess.DEVNULL).strip()}


def group_alive(group):
    try:
        os.killpg(group, 0)
    except ProcessLookupError:
        return False
    # killpg(0) includes orphan zombies. Only state and pgrp metadata are read;
    # environments, command lines and private configuration are never inspected.
    try:
        for path in Path("/proc").iterdir():
            if not path.name.isdecimal():
                continue
            try:
                fields = (path / "stat").read_text().rsplit(")", 1)[1].split()
            except FileNotFoundError:
                continue
            if int(fields[2]) == group and fields[0] not in ("Z", "X"):
                return True
    except (OSError, ValueError, IndexError):
        return True  # Missing process evidence is never a claim of cleanup.
    return False


def interrupt_group(process):
    def send(signum):
        try:
            os.killpg(process.pid, signum)
        except ProcessLookupError:
            pass
    send(signal.SIGTERM)
    for grace, signum in ((TERM_GRACE_SECONDS, signal.SIGKILL), (KILL_GRACE_SECONDS, None)):
        deadline = time.monotonic() + grace
        while group_alive(process.pid):
            process.poll()  # Reap our leader even while descendants finish traps.
            if time.monotonic() >= deadline:
                break
            time.sleep(0.1)
        if not group_alive(process.pid):
            process.poll()
            return "drained"
        if signum:
            send(signum)
    process.poll()
    return "unconfirmed"


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def source_fingerprint(root):
    result = hashlib.sha256()
    paths = subprocess.check_output(["bash", "tests/helpers/source-files.sh"], cwd=root, stderr=subprocess.DEVNULL)
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
                                   cwd=root, text=True, stderr=subprocess.DEVNULL).splitlines()
    result = {}
    for row in rows:
        kind, name, path = row.split("\t")
        if kind not in ("runner", "not-applicable") or name in result:
            raise ValueError("invalid profile acceptance inventory")
        result[name] = {"kind": kind, "path": path}
        if kind == "not-applicable":
            result[name]["reason"] = (root / path).read_text().strip()
        manifest = (root / path).parent / "acceptance-lanes.json"
        if manifest.exists():
            document = json.loads(manifest.read_text())
            lanes = document.get("lanes")
            if kind != "runner" or document.get("schema_version") != 1 or not isinstance(lanes, list) or not lanes:
                raise ValueError("invalid profile acceptance lanes")
            ids = set()
            for lane in lanes:
                if (not isinstance(lane, dict) or set(lane) != {"id", "arguments"}
                    or not isinstance(lane["id"], str) or not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9._-]{0,79}", lane["id"])
                    or lane["id"] in ids or not isinstance(lane["arguments"], list)
                    or any(not isinstance(arg, str) or len(arg) > 256 or any(ord(c) < 32 or ord(c) == 127 for c in arg) for arg in lane["arguments"])
                    or "--slot" in lane["arguments"]):
                    raise ValueError("invalid or duplicate profile acceptance lane")
                ids.add(lane["id"])
            result[name]["lanes"] = lanes
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
        if "lanes" in entry:
            for lane in entry["lanes"]:
                result["profile:" + name + "/" + lane["id"]] = ["bash", entry["path"], *lane["arguments"]]
        else:
            result["profile:" + name] = ["bash", entry["path"]] if entry["kind"] == "runner" else None
    return result


def update_result(receipt):
    receipt["profile_results"] = {}
    for name in receipt.get("profiles", {}):
        family = "profile:" + name
        children = [value["status"] for key, value in receipt["checks"].items()
                    if key == family or key.startswith(family + "/")]
        receipt["profile_results"][name] = ("failed" if "failed" in children else
            "passed" if children and all(state == "passed" for state in children) else
            "not-applicable" if children == ["not-applicable"] else "incomplete")
    states = [item["status"] for item in receipt["checks"].values()]
    receipt["reproducible_result"] = (
        "failed" if "failed" in states else
        "passed" if all(state in ("passed", "not-applicable") for state in states) else "incomplete")
    if receipt["reproducible_result"] == "passed" and any(
        receipt["checks"].get(name, {}).get("status") != "not-applicable"
        for name in receipt.get("operator_exclusions", [])):
        receipt["reproducible_result"] = "incomplete"
    receipt["result"] = receipt["reproducible_result"]
    if receipt["result"] == "passed" and any(item["status"] != "passed" for item in receipt["external_obligations"].values()):
        receipt["result"] = "incomplete"
    if receipt.get("superseded"):
        receipt["result"] = "incomplete"


def save(output, receipt):
    update_result(receipt)
    receipt["updated_at"] = datetime.now(timezone.utc).isoformat()
    temporary = output / "receipt.json.next"
    with private_file(temporary) as stream:
        stream.write(json.dumps(receipt, indent=2, sort_keys=True) + "\n")
        stream.flush()
        os.fsync(stream.fileno())
    temporary.replace(output / "receipt.json")
    sync_directory(output)


def execute(name, command, root, output, env, slot=None):
    log = output / (name.replace(":", "-").replace("/", "-") + "-" + uuid.uuid4().hex[:8] + ".log")
    argv = command + (["--slot", str(slot)] if slot else [])
    print(f"RUN {name}" + (f" slot={slot}" if slot else ""), flush=True)
    start = time.monotonic()
    event(output, "check-start", check=name, slot=slot)
    interrupted = False
    cleanup_status = None
    markers = set()
    offset = 0
    next_heartbeat = start + HEARTBEAT_SECONDS
    with private_file(log, "wb") as stream:
        process = subprocess.Popen(argv, cwd=root, env=env, stdout=stream,
                                   stderr=subprocess.STDOUT, start_new_session=True)
        while process.poll() is None:
            if STOP.wait(0.2):
                interrupted = True
                # This new process group belongs exclusively to this invocation.
                cleanup_status = interrupt_group(process)
                break
            if time.monotonic() >= next_heartbeat:
                event(output, "heartbeat", check=name, slot=slot,
                      duration_seconds=round(time.monotonic() - start, 2))
                next_heartbeat = time.monotonic() + HEARTBEAT_SECONDS
            offset = phase_events(output, name, log, offset, markers)
        if STOP.is_set() and not interrupted:
            interrupted = True
            cleanup_status = interrupt_group(process)
        code = process.poll() if cleanup_status == "unconfirmed" else process.wait()
        stream.flush()
        os.fsync(stream.fileno())
    phase_events(output, name, log, offset, markers, final=True)
    blocked = capacity_reason(log) if code == 4 and not interrupted else None
    result = {"status": "interrupted" if interrupted else "blocked" if blocked else "passed" if code == 0 else "failed", "exit_code": code,
              "duration_seconds": round(time.monotonic() - start, 2), "log": log.name,
              "log_sha256": digest(log), "command": command_identity(command, root)}
    environments = sorted(marker[1:] for marker in markers if marker[0] == "environment")
    if slot:
        result["environments"] = [{"type": kind, "vm_count": count, "base_fingerprint": fingerprint}
                                  for kind, count, fingerprint in environments]
        result["environment_evidence"] = "recorded" if environments else "unknown"
    if blocked:
        result["reason"] = "capacity_" + blocked
    if cleanup_status:
        result["cleanup_status"] = cleanup_status
    if slot:
        result["slot"] = slot
    print(f"RESULT {name} {result['status']} exit={code} log={log}", flush=True)
    event(output, "check-result", check=name, **{key: value for key, value in result.items() if key != "command"})
    return result


def phase_events(output, name, log, offset, seen, final=False):
    offset, discard = offset if isinstance(offset, tuple) else (offset, False)
    with log.open("rb") as stream:
        stream.seek(offset)
        count = 0
        while final or count < 200:
            count += 1
            line = stream.readline(4097)
            if not line:
                break
            if discard or len(line) > 4096:
                discard = not line.endswith(b"\n")
                offset = stream.tell()
                continue
            if not line.endswith(b"\n"):
                break
            offset = stream.tell()
            marker = re.fullmatch(rb"  \[( \.\. | ok |fail)\] phase=([a-zA-Z0-9_./-]{1,100})(?: [^\r\n]*)?\n", line)
            if marker:
                key = (marker[1], marker[2])
                if key not in seen:
                    seen.add(key)
                    event(output, "phase", check=name, phase=marker[2].decode(),
                          state={b" .. ": "started", b" ok ": "passed", b"fail": "failed"}[marker[1]])
            stage = re.fullmatch(rb"E2E_PHASE phase=(allocation|packing|transport|guest|guest-cleanup|cleanup/release|fixture/[a-z0-9-]{1,60}) state=(start|end) duration_seconds=([0-9]{1,9}) exit_code=([0-9]{1,3})(?: vm=([12]))?\n", line)
            if (stage and int(stage[4]) <= 255
                and (stage[2] != b"start" or (stage[3] == b"0" and stage[4] == b"0"))
                and bool(stage[5]) == (stage[1] in (b"transport", b"guest", b"guest-cleanup"))):
                event(output, "controller-phase", check=name, phase=stage[1].decode(),
                      state=stage[2].decode(), duration_seconds=int(stage[3]),
                      exit_code=int(stage[4]), **({"vm": int(stage[5])} if stage[5] else {}))
            environment = re.fullmatch(rb"E2E lease: [^\r\n]{0,2000} type=(subyard-pair|android-test) vms=([12]) base=([0-9a-f]{64})\n", line)
            if environment and (environment[1], environment[2]) in ((b"subyard-pair", b"1"), (b"subyard-pair", b"2"), (b"android-test", b"1")):
                key = ("environment", environment[1].decode(), int(environment[2]), environment[3].decode())
                if key not in seen:
                    seen.add(key)
                    event(output, "environment", check=name, type=key[1], vm_count=key[2], base_fingerprint=key[3])
    return offset, discard


def capacity_reason(log):
    # The runner emits this only after validating the typed broker refusal.
    reason = None
    with log.open("rb") as stream:
        for line in stream:
            match = re.fullmatch(rb"agent-e2e: retryable capacity refusal for slot-[0-9]{1,3}: resource=(memory|disk); retry after resources become available\n", line)
            if match:
                reason = match[1].decode()
            elif line.startswith(b"E2E lease:") or re.match(rb"E2E_PHASE phase=guest state=start ", line):
                reason = None
    return reason


def prepare(output, version):
    output.mkdir(parents=True, exist_ok=True)
    if any(output.iterdir()):
        raise ValueError("prepare requires an empty output directory")
    output.chmod(0o700)
    event(output, "prepare-start", version=version)
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
               "environment": execution_environment()}
    for name, command in commands(root, version, inventory).items():
        receipt["checks"][name] = {"status": "pending" if command else "not-applicable"}
        if command is None:
            receipt["checks"][name]["reason"] = inventory[name.split(":", 1)[1]]["reason"]
    receipt["checks"]["package"] = execute("package", ["bash", "dev/package-engine.sh", "--output-dir", str(release), "--version", version], root, output, clean_environment())
    receipt["checks"]["package"]["execution_environment"] = receipt["environment"]
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
    transport.chmod(0o600)
    save(output, receipt)
    print(f"PREPARED {output / 'receipt.json'}", flush=True)


def load_bound(output):
    receipt = json.loads((output / "receipt.json").read_text())
    root = output / "source"
    if receipt.get("schema_version") != 1 or source_fingerprint(root) != receipt["source_fingerprint"]:
        raise ValueError("frozen source fingerprint mismatch")
    if not isinstance(receipt.get("environment"), dict) or set(receipt["environment"]) != {"system", "machine", "go"}:
        raise ValueError("candidate execution environment is missing")
    if profile_inventory(root) != receipt["profiles"]:
        raise ValueError("profile obligations changed")
    external = external_inventory(root)
    if set(external) != set(receipt["external_obligations"]) or any(
        any(receipt["external_obligations"][name].get(key) != value
            for key, value in item.items() if key != "status") for name, item in external.items()):
        raise ValueError("external obligations changed")
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
    required_commands = commands(root, receipt["version"], receipt["profiles"])
    for name, result in receipt["checks"].items():
        if result["status"] == "passed":
            log = result.get("log", "")
            if not log or Path(log).name != log or digest(output / log) != result.get("log_sha256"):
                raise ValueError("passing result evidence is missing or changed")
            if name in required_commands and required_commands[name] is not None and "command" in result:
                if result["command"] != command_identity(required_commands[name], root):
                    raise ValueError("passing command provenance changed")
            if result.get("inherited") and (not re.fullmatch(r"[0-9a-f]{64}", result.get("from_receipt_sha256", ""))
                                            or result.get("from_log_sha256") != result["log_sha256"]):
                raise ValueError("inherited evidence provenance is incomplete")
            if result.get("inherited"):
                binding = result.get("inherited_from", {})
                if (binding.get("target_source_fingerprint") != receipt["source_fingerprint"]
                    or not re.fullmatch(r"[0-9a-f]{64}", binding.get("source_fingerprint", ""))):
                    raise ValueError("inherited source provenance is incomplete")
                if binding.get("scope") == "exact-source":
                    if binding["source_fingerprint"] != receipt["source_fingerprint"]:
                        raise ValueError("inherited exact source differs")
                elif binding.get("scope") == "other-profile-tests-only" and name.startswith("profile:"):
                    closure = profile_closure(root, receipt["profiles"], name)
                    if binding.get("closure_sha256") != closure:
                        raise ValueError("inherited profile closure changed")
                else:
                    raise ValueError("invalid inherited source scope")
                provenance = binding.get("release_provenance")
                if provenance is not None:
                    if (not isinstance(provenance, dict) or set(provenance) != {"scope", "files"}
                        or provenance["scope"] != "generated-at-only" or not isinstance(provenance["files"], dict)
                        or not provenance["files"]):
                        raise ValueError("invalid inherited release provenance scope")
                    for filename, metadata in provenance["files"].items():
                        identity, generated = release_provenance_identity(root, receipt, filename)
                        expected = {"source_sha256", "target_sha256", "source_generated_at", "target_generated_at", "identity_sha256"}
                        if (not isinstance(metadata, dict) or set(metadata) != expected
                            or any(not isinstance(value, str) for value in metadata.values())
                            or not re.fullmatch(r"[0-9a-f]{64}", metadata["source_sha256"])
                            or metadata["source_sha256"] == metadata["target_sha256"]
                            or metadata["target_sha256"] != receipt["release_files"].get(filename)
                            or metadata["target_generated_at"] != generated
                            or metadata["source_generated_at"] == generated
                            or metadata["identity_sha256"] != hashlib.sha256(json.dumps(identity, sort_keys=True).encode()).hexdigest()):
                            raise ValueError("inherited release provenance changed")
    if digest(root / ".subyard-acceptance/release" / receipt["runtime_file"]) != receipt["runtime_sha256"]:
        raise ValueError("candidate runtime checksum mismatch")
    return root, receipt


def select_checks(checks, selectors):
    selected = set()
    for selector in selectors:
        children = [name for name in checks if name == selector or name.startswith(selector + "/")]
        if not children:
            raise ValueError("unknown check selection")
        selected.update(children)
    return selected


def is_live(name):
    return name == "p0-release-smoke" or name.startswith("profile:")


def preflight(output=None, only=(), exclude=(), broker_status=None, external_readiness=None, slots=(), types=()):
    if len(set(slots)) != len(slots) or any(type(slot) is not int or slot < 1 or slot > 999 for slot in slots):
        raise ValueError("slots must be unique integers from 1 to 999")
    if any(not re.fullmatch(r"[a-z][a-z0-9-]{0,63}", kind) for kind in types):
        raise ValueError("invalid environment type selection")
    if output:
        root, receipt = load_bound(output)
    else:
        root = ROOT
        inventory = profile_inventory(root)
        receipt = {"version": "preflight", "profiles": inventory,
                   "external_obligations": external_inventory(root),
                   "checks": {name: {"status": "pending" if command else "not-applicable"}
                              for name, command in commands(root, "preflight", inventory).items()}}
        receipt["checks"]["package"] = {"status": "pending"}
    checks = receipt["checks"]
    selected = select_checks(checks, only) if only else set(checks)
    excluded = select_checks(checks, exclude)
    readiness = {}
    if external_readiness:
        if external_readiness.stat().st_size > 1024 * 1024:
            raise ValueError("external readiness declaration is too large")
        readiness = json.loads(external_readiness.read_text())
        if (not isinstance(readiness, dict) or set(readiness) - set(receipt["external_obligations"])
            or any(value not in ("available", "unavailable", "unknown") for value in readiness.values())):
            raise ValueError("invalid external readiness declaration")
    report = {"schema_version": 1, "candidate": "frozen" if output else "worktree", "superseded": bool(receipt.get("superseded")),
              "checks": [{"id": name, "status": entry["status"], "required": entry["status"] != "not-applicable",
                          "selected": name in selected and name not in excluded,
                          "excluded": name in excluded, "physical": is_live(name)}
                         for name, entry in checks.items()],
              "pending": [name for name, entry in checks.items() if entry["status"] not in ("passed", "not-applicable")],
              "external_prerequisites": [{"id": name, "status": entry["status"],
                                          "required_inputs": entry["required_inputs"],
                                          "readiness": readiness.get(name, "unknown")}
                                         for name, entry in receipt["external_obligations"].items()],
              "architecture_responsibility": {"local": platform.machine(),
                                               "arm64": "GitHub CI native build and runtime smoke; separate from local physical acceptance"},
              "local_tools": {tool: shutil.which(tool) is not None for tool in
                              ("bash", "python3", "go", "make", "rg", "shellcheck", "git", "jq", "tar", "gzip", "sha256sum", "systemd-analyze", "script")},
              "physical_prerequisites": ["explicitly approved slots", "available managed test environment; fresh leases per attempt"],
              "broker": public_broker_status(broker_status)}
    broker = report["broker"]
    observed_slots = {int(slot["id"].split("-")[1]): slot for slot in broker.get("slots", [])}
    selected_slots = [observed_slots.get(slot, {"id": f"slot-{slot}", "state": "unknown"}) for slot in slots]
    headroom = broker.get("headroom", {})
    physical = any(check["physical"] and check["selected"] and check["status"] not in ("passed", "not-applicable")
                   for check in report["checks"])
    shortages = []
    if physical:
        shortages = [slot["id"] + ": " + slot["state"] for slot in selected_slots
                     if slot["state"] not in ("available", "unknown")]
        shortages += [kind + " headroom exhausted" for kind in ("memory", "disk") if headroom.get(kind + "_bytes") == 0]
    report["capacity_summary"] = {
        "selected_slots": selected_slots, "selected_types": list(dict.fromkeys(types)),
        "headroom_bytes": {kind: headroom.get(kind + "_bytes") for kind in ("memory", "disk")},
        "observed_at": broker.get("observed_at"), "age_seconds": broker.get("age_seconds"),
        "freshness": broker.get("freshness", "unknown"),
        "planning": "insufficient" if shortages else "unknown" if physical else "not-required",
        "shortages": shortages,
        "prerequisites": {"physical": report["physical_prerequisites"] if physical else [],
                          "missing_local_tools": [tool for tool, available in report["local_tools"].items() if not available],
                          "external": report["external_prerequisites"]},
        "authority": "planning snapshot only; request requirements and current native broker admission remain authoritative",
        "continuation": "after capacity changes, run --output DIR --only CHECK on the same frozen candidate; passed checks are retained",
    }
    summary = report["capacity_summary"]
    slot_text = ",".join(slot["id"] + ":" + slot["state"] + "/" + slot.get("type", "unknown") for slot in selected_slots) or "unknown"
    missing_tools = ",".join(summary["prerequisites"]["missing_local_tools"]) or "none"
    external_unknown = sum(item["readiness"] != "available" for item in report["external_prerequisites"])
    numeric = {key: value if value is not None else "unknown"
               for key, value in {**summary["headroom_bytes"], "age": summary["age_seconds"]}.items()}
    print(f"CAPACITY slots={slot_text} types={','.join(summary['selected_types']) or 'unknown'} "
          f"memory_headroom_bytes={numeric['memory']} disk_headroom_bytes={numeric['disk']} "
          f"observed_at={summary['observed_at'] or 'unknown'} age_seconds={numeric['age']} planning={summary['planning']} "
          f"prerequisites=approved-slots,managed-environment missing_local_tools={missing_tools} external_not_ready={external_unknown}",
          file=sys.stderr, flush=True)
    print(json.dumps(report, indent=2, sort_keys=True))
    return report


def public_broker_status(path):
    if not path:
        return {"status": "unavailable", "reason": "no supplied public status; no authentication attempted"}
    try:
        if path.stat().st_size > 1024 * 1024:
            raise ValueError("status too large")
        value = json.loads(path.read_text())
        if value.get("schema_version") != 1 or value.get("status") != "ok":
            raise ValueError("invalid status")
        slots = value.get("pool", {}).get("slots", [])
        result = {"status": "observed", "observed_at": None, "age_seconds": None,
                  "freshness": "unknown; supplied file snapshot", "slots": []}
        observed_at = value.get("observed_at")
        if isinstance(observed_at, str) and re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?(?:Z|[+-][0-9]{2}:[0-9]{2})", observed_at):
            try:
                observed = datetime.fromisoformat(observed_at.replace("Z", "+00:00")).astimezone(timezone.utc)
                age = (datetime.now(timezone.utc) - observed).total_seconds()
                result["observed_at"] = observed.isoformat()
                if age >= 0:
                    result.update(age_seconds=int(age), freshness="supplied file snapshot; age is not an admission guarantee")
                else:
                    result["freshness"] = "unknown; observation timestamp is in the future"
            except (ValueError, OverflowError):
                pass
        states = {"available", "unavailable", "held", "provisioning", "draining", "quarantined"}
        for slot in slots[:999]:
            identifier = slot.get("slot_id", "")
            if re.fullmatch(r"slot-[0-9]{1,3}", identifier) and slot.get("state") in states:
                public_slot = {"id": identifier, "state": slot["state"]}
                kind = (slot.get("environment") or {}).get("type")
                if kind in ("subyard-pair", "android-test"):
                    public_slot["type"] = kind
                result["slots"].append(public_slot)
        resources = value.get("resources", {})
        for kind, keys in (("memory", ("available_bytes", "cgroup_limit_bytes", "cgroup_current_bytes")),
                           ("storage", ("physical_total_bytes", "physical_used_bytes", "physical_free_bytes", "budget_used_bytes"))):
            result[kind] = {key: amount for key, amount in resources.get(kind, {}).items()
                            if key in keys and type(amount) is int and 0 <= amount < 2**64}
        def amount(mapping, key):
            number = mapping.get(key)
            return number if type(number) is int and 0 <= number < 2**64 else None
        budgets = resources.get("budgets", {})
        reservations = resources.get("slots")
        builder = resources.get("builder") or {}
        def growth(kind):
            if not isinstance(reservations, list) or len(reservations) > 999:
                return None
            parts = [amount(slot, "remaining_" + kind + "_growth_bytes") for slot in reservations]
            parts.append(amount(builder, "reserved_memory_bytes" if kind == "memory" else "reserved_disk_peak_bytes") if builder else 0)
            return None if None in parts else sum(parts)
        memory_parts = [amount(resources.get("memory", {}), "available_bytes"),
                        amount(budgets, "memory_reserve_bytes"), growth("memory")]
        disk_parts = [amount(resources.get("storage", {}), "physical_free_bytes"),
                      amount(budgets, "disk_reserve_bytes"), growth("disk")]
        memory_headroom = max(0, memory_parts[0] - sum(memory_parts[1:])) if None not in memory_parts else None
        disk_headroom = max(0, disk_parts[0] - sum(disk_parts[1:])) if None not in disk_parts else None
        quota = amount(budgets, "disk_bytes")
        used = amount(resources.get("storage", {}), "budget_used_bytes")
        if quota is None or (quota and (used is None or disk_parts[2] is None)):
            disk_headroom = None
        elif quota and disk_headroom is not None:
            disk_headroom = min(disk_headroom, max(0, quota - used - disk_parts[2]))
        if "legacy_reservation_unknown" in resources.get("errors", []):
            memory_headroom = disk_headroom = None
        result["headroom"] = {"memory_bytes": memory_headroom, "disk_bytes": disk_headroom,
                              "evidence": "public snapshot only; native broker admission is final"}
        return result
    except (OSError, ValueError, TypeError, AttributeError):
        return {"status": "unavailable", "reason": "invalid or unreadable public status"}


def run_checks(output, slots, only, rerun, first=(), exclude=()):
    root, receipt = load_bound(output)
    if receipt.get("superseded"):
        raise ValueError("candidate was explicitly superseded; prepare a new candidate")
    measured_environment = execution_environment()
    if measured_environment != receipt.get("environment", measured_environment):
        raise ValueError("execution environment changed since candidate preparation")
    checks = commands(root, receipt["version"], receipt["profiles"])
    selected = select_checks(checks, only) if only else set(checks)
    excluded = select_checks(checks, exclude)
    selected -= excluded
    prioritized = select_checks(checks, first)
    priority_order = [name for selector in first for name in checks
                      if name in prioritized and (name == selector or name.startswith(selector + "/"))]
    order = list(dict.fromkeys([*priority_order, *checks]))
    env = clean_environment()
    pending = [name for name in order if checks[name] and name in selected
               and (rerun or receipt["checks"][name]["status"] != "passed")]
    live = [name for name in pending if is_live(name)]
    if live and not slots:
        raise ValueError("live checks require --slots N [N ...]")
    receipt["operator_exclusions"] = sorted(excluded)
    save(output, receipt)
    event(output, "run-start", checks=pending, slots=slots, exclusions=sorted(excluded))
    for name in [item for item in pending if item not in live]:
        if STOP.is_set():
            break
        receipt["checks"][name] = execute(name, checks[name], root, output, env)
        receipt["checks"][name]["execution_environment"] = measured_environment
        save(output, receipt)
        if receipt["checks"][name]["status"] != "passed":
            break
    local_names = [name for name in checks if not is_live(name)] + (["package"] if "package" in receipt["checks"] else [])
    if live and any(receipt["checks"][name]["status"] != "passed" for name in local_names):
        print("INCOMPLETE: live checks await successful local gates", flush=True)
        event(output, "run-result", status="interrupted" if STOP.is_set() else "incomplete")
        return 1
    # Only physical controllers consume the frozen transport. Host-free tests build
    # synthetic worktrees and must retain the normal bundler contract.
    env.update(SUBYARD_E2E_CANDIDATE_BUNDLE=str(output / "candidate-bundle.tar.gz"),
               SUBYARD_E2E_CANDIDATE_SHA256=receipt["candidate_bundle_sha256"],
               SUBYARD_E2E_CONTROLLER_WORKSPACE=str(ROOT))
    # One controller per approved slot, taking the next independent check when free.
    lock = threading.Lock()
    work = queue.Queue()
    for name in live:
        work.put(name)
    def collect(slot):
        while not STOP.is_set():
            try:
                name = work.get_nowait()
            except queue.Empty:
                return
            result = execute(name, checks[name], root, output, env, slot)
            result["execution_environment"] = measured_environment
            with lock:
                receipt["checks"][name] = result
                save(output, receipt)
            if result["status"] != "passed":
                return
    with ThreadPoolExecutor(max_workers=max(1, len(slots))) as pool:
        futures = [pool.submit(collect, slot) for slot in slots]
        for future in as_completed(futures):
            future.result()
    if source_fingerprint(root) != receipt["source_fingerprint"]:
        receipt["checks"]["verify"]["status"] = "failed"
        receipt["error"] = "source changed during acceptance"
    save(output, receipt)
    event(output, "run-result", status="interrupted" if STOP.is_set() else receipt["result"])
    print(f"ACCEPTANCE reproducible={receipt['reproducible_result']} overall={receipt['result']} receipt={output / 'receipt.json'}", flush=True)
    return 0 if receipt["result"] == "passed" else 1


def source_entries(root):
    entries = {}
    paths = subprocess.check_output(["bash", "tests/helpers/source-files.sh"], cwd=root, stderr=subprocess.DEVNULL)
    for raw in filter(None, paths.split(b"\0")):
        name = os.fsdecode(raw)
        if name.startswith(".subyard-acceptance/"):
            continue
        path = root / name
        if not path.exists() and not path.is_symlink():
            continue
        entries[name] = ["executable" if path.lstat().st_mode & 0o111 else "regular",
                         "link:" + os.readlink(path) if path.is_symlink() else digest(path)]
    return entries


def profile_closure(root, profiles, name):
    owner = name.split(":", 1)[1].split("/", 1)[0]
    other_prefixes = tuple("config/profiles/" + profile + "/tests/" for profile in profiles if profile != owner)
    closure = {path: value for path, value in source_entries(root).items() if not path.startswith(other_prefixes)}
    return hashlib.sha256(json.dumps(closure, sort_keys=True).encode()).hexdigest()


def release_provenance_identity(root, receipt, name):
    artifact = name.removesuffix(".provenance.json")
    recognized = re.fullmatch(r"(?:yard-" + re.escape(receipt["version"]) + r"-linux-(?:amd64|arm64)|subyard-" +
                              re.escape(receipt["version"]) + r"-linux-(?:amd64|arm64)\.tar\.gz)", artifact)
    if not name.endswith(".provenance.json") or not recognized or artifact not in receipt["release_files"]:
        raise ValueError("unrecognized release provenance artifact")
    def unique_fields(pairs):
        document = dict(pairs)
        if len(document) != len(pairs):
            raise ValueError("duplicate release provenance fields")
        return document
    path = root / ".subyard-acceptance/release" / name
    if path.stat().st_size > 4096:
        raise ValueError("release provenance is too large")
    document = json.loads(path.read_text(), object_pairs_hook=unique_fields)
    keys = {"schemaVersion", "artifact", "sha256", "version", "sourceRepository", "canonicalRepository", "sourceRevision", "generatedAt"}
    if (not isinstance(document, dict) or set(document) != keys or type(document["schemaVersion"]) is not int
        or document["schemaVersion"] != 1 or any(not isinstance(document[key], str) for key in keys - {"schemaVersion"})
        or document["artifact"] != artifact or document["version"] != receipt["version"]
        or document["sha256"] != receipt["release_files"][artifact]
        or document["sourceRepository"] != "github.com/Dmitry-Borodin/Subyard"
        or document["canonicalRepository"] != "github.com/Subyard/Subyard"
        or not re.fullmatch(r"unknown|[0-9a-f]{40}|[0-9a-f]{64}", document["sourceRevision"])
        or not re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z", document["generatedAt"])):
        raise ValueError("invalid release provenance identity")
    try:
        datetime.strptime(document["generatedAt"], "%Y-%m-%dT%H:%M:%SZ")
    except ValueError:
        raise ValueError("invalid release provenance timestamp") from None
    generated = document.pop("generatedAt")
    return document, generated


def release_import_binding(root, receipt, source_root, source):
    if set(receipt["release_files"]) != set(source["release_files"]):
        raise ValueError("evidence import release inventory differs")
    metadata = {}
    for name, target_hash in receipt["release_files"].items():
        source_hash = source["release_files"][name]
        if not name.endswith(".provenance.json"):
            if target_hash != source_hash:
                raise ValueError("evidence import candidate identity differs")
            continue
        target_identity, target_time = release_provenance_identity(root, receipt, name)
        source_identity, source_time = release_provenance_identity(source_root, source, name)
        if target_identity != source_identity:
            raise ValueError("evidence import release provenance identity differs")
        if target_hash != source_hash:
            if target_time == source_time:
                raise ValueError("evidence import release metadata differs beyond timestamp")
            metadata[name] = {"source_sha256": source_hash, "target_sha256": target_hash,
                              "source_generated_at": source_time, "target_generated_at": target_time,
                              "identity_sha256": hashlib.sha256(json.dumps(target_identity, sort_keys=True).encode()).hexdigest()}
    return {"scope": "generated-at-only", "files": metadata} if metadata else None


def import_evidence(output, source_output, names, allow_other_profile_tests=False):
    root, receipt = load_bound(output)
    source_root, source = load_bound(source_output)
    if receipt.get("superseded") or source.get("superseded"):
        raise ValueError("superseded candidates cannot import evidence")
    if output == source_output:
        raise ValueError("evidence import requires a different receipt")
    if not names or len(set(names)) != len(names):
        raise ValueError("evidence import requires unique named checks")
    identity_keys = ("version", "runtime_file", "runtime_sha256", "profiles")
    if any(receipt[key] != source[key] for key in identity_keys):
        raise ValueError("evidence import candidate identity differs")
    release_binding = release_import_binding(root, receipt, source_root, source)
    source_hash = digest(source_output / "receipt.json")
    check_commands = commands(root, receipt["version"], receipt["profiles"])
    incoming = []
    for name in names:
        if name not in check_commands or check_commands[name] is None:
            raise ValueError("evidence import requires an exact executable check name")
        result = source["checks"][name]
        expected_command = command_identity(check_commands[name], root)
        if result["status"] != "passed" or result.get("command") != expected_command:
            raise ValueError("evidence import requires passing command-bound evidence")
        if not is_live(name) and (receipt.get("environment") != source.get("environment")
                                 or result.get("execution_environment") != receipt.get("environment")):
            raise ValueError("evidence import local execution environment differs")
        binding = {"source_fingerprint": source["source_fingerprint"],
                   "target_source_fingerprint": receipt["source_fingerprint"]}
        if release_binding:
            binding["release_provenance"] = release_binding
        if source["source_fingerprint"] != receipt["source_fingerprint"]:
            if not allow_other_profile_tests or not name.startswith("profile:"):
                raise ValueError("evidence import source identity differs")
            owner = name.split(":", 1)[1].split("/", 1)[0]
            other_prefixes = tuple("config/profiles/" + profile + "/tests/"
                                   for profile in receipt["profiles"] if profile != owner)
            left, right = source_entries(source_root), source_entries(root)
            changes = {path for path in left.keys() | right.keys() if left.get(path) != right.get(path)}
            if not changes or any(not path.startswith(other_prefixes) for path in changes):
                raise ValueError("evidence import changes owning profile or shared inputs")
            binding["closure_sha256"] = profile_closure(root, receipt["profiles"], name)
            binding["scope"] = "other-profile-tests-only"
        else:
            binding["scope"] = "exact-source"
        incoming.append((name, result, binding))
    # Validate the entire requested import before copying evidence or updating state.
    for name, result, binding in incoming:
        log = output / ("import-" + uuid.uuid4().hex + ".log")
        with (source_output / result["log"]).open("rb") as original, private_file(log, "wb") as copied:
            shutil.copyfileobj(original, copied)
            copied.flush()
            os.fsync(copied.fileno())
        if digest(log) != result["log_sha256"]:
            raise ValueError("imported evidence changed during copy")
        receipt["checks"][name] = {**result, "log": log.name, "inherited": True,
                                   "from_receipt_sha256": source_hash,
                                   "from_log_sha256": result["log_sha256"],
                                   "inherited_from": binding}
        event(output, "evidence-import", check=name, from_receipt_sha256=source_hash,
              from_log_sha256=result["log_sha256"], inherited_from=binding)
    save(output, receipt)
    return 0


def supersede(output):
    _, receipt = load_bound(output)
    receipt["superseded"] = True
    receipt["superseded_at"] = datetime.now(timezone.utc).isoformat()
    save(output, receipt)
    event(output, "candidate-superseded", source_fingerprint=receipt["source_fingerprint"])
    return 0


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
    run.add_argument("--first", action="append", default=[], help="prioritize a check/family after required local gates")
    run.add_argument("--exclude", action="append", default=[], help="leave excluded checks incomplete")
    pre = sub.add_parser("preflight", help="read-only obligations and prerequisites; never packages or authenticates")
    pre.add_argument("--output", type=Path, help="inspect a bound frozen receipt instead of the worktree")
    pre.add_argument("--only", action="append", default=[])
    pre.add_argument("--exclude", action="append", default=[])
    pre.add_argument("--broker-status", type=Path, help="already-collected public broker JSON; no live query")
    pre.add_argument("--external-readiness", type=Path, help="JSON obligation IDs mapped to available/unavailable/unknown")
    pre.add_argument("--slots", type=int, nargs="+", default=[], help="explicit slots being considered; does not reserve them")
    pre.add_argument("--types", nargs="+", default=[], help="environment types being considered; planning inputs only")
    imp = sub.add_parser("import", help="explicitly inherit named passing evidence between bound candidates")
    imp.add_argument("--output", type=Path, required=True)
    imp.add_argument("--from", dest="source_output", type=Path, required=True)
    imp.add_argument("--check", action="append", required=True)
    imp.add_argument("--allow-other-profile-tests", action="store_true", help="allow only changes to other shipped profiles' test files")
    superseded = sub.add_parser("supersede", help="retain original evidence and explicitly retire this candidate")
    superseded.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if args.action == "preflight":
        report = preflight(args.output.resolve() if args.output else None, args.only, args.exclude,
                           args.broker_status, args.external_readiness, args.slots, args.types)
        return 1 if report["capacity_summary"]["planning"] == "insufficient" else 0
    output = args.output.resolve()
    if output == ROOT or (ROOT in output.parents and ROOT / ".build" not in output.parents):
        parser.error("output must be under .build or outside the source checkout")
    if args.action == "prepare":
        if not args.version or any(c not in "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._+-" for c in args.version):
            parser.error("unsafe version")
        prepare(output, args.version)
        return 0
    if args.action == "import":
        with acceptance_lock(output), acceptance_lock(args.source_output.resolve(), shared=True):
            return import_evidence(output, args.source_output.resolve(), args.check, args.allow_other_profile_tests)
    if args.action == "supersede":
        with acceptance_lock(output):
            return supersede(output)
    if len(set(args.slots)) != len(args.slots) or any(slot < 1 or slot > 999 for slot in args.slots):
        parser.error("slots must be unique integers from 1 to 999")
    with acceptance_lock(output):
        return run_checks(output, args.slots, args.only, args.rerun, args.first, args.exclude)


if __name__ == "__main__":
    for signum in (signal.SIGINT, signal.SIGTERM):
        signal.signal(signum, lambda signum, frame: STOP.set())
    try:
        sys.exit(main())
    except (ValueError, KeyError, OSError, subprocess.SubprocessError) as error:
        message = str(error) if isinstance(error, (ValueError, KeyError)) else type(error).__name__
        print(f"release-acceptance: {message}", file=sys.stderr)
        sys.exit(2)
