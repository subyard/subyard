#!/usr/bin/env python3
"""Freeze a release candidate and run its acceptance checks with local reports."""
import argparse
import copy
from concurrent.futures import ThreadPoolExecutor, as_completed
from contextlib import contextmanager, ExitStack
from datetime import datetime, timezone
import fcntl
import hashlib
import json
import math
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
MANIFEST_LOCK = threading.Lock()
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
    return entries_fingerprint(source_entries(root))


def clean_environment():
    env = os.environ.copy()
    for key in ("SUBYARD_E2E_CANDIDATE_BUNDLE", "SUBYARD_E2E_CANDIDATE_SHA256",
                "SUBYARD_E2E_LOCAL_EVIDENCE",
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
            if (key in result or not item.get("description") or not item.get("required_inputs")
                or type(item.get("required", True)) is not bool):
                raise ValueError("invalid or duplicate external obligation")
            # Missing required means true without rewriting historical receipt inventories.
            result[key] = {**item, "status": "not-run"}
    return result


def optional_external_not_run(result):
    return (result.get("required") is False and result.get("status") == "not-run"
            and set(result) <= {"id", "description", "required_inputs", "required", "status"})


def deferral_candidate_sha256(receipt):
    identity = receipt.get("preparation") or {key: receipt[key] for key in
        ("version", "source_fingerprint", "release_files", "candidate_bundle_sha256")}
    return hashlib.sha256(json.dumps(identity, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def validate_external_deferral(result, candidate_sha256):
    declaration = result.get("deferral")
    if (result.get("status") != "not-run" or "evidence_sha256" in result
        or not isinstance(declaration, dict)
        or set(declaration) != {"reason", "authorization_reference", "candidate_sha256"}
        or declaration["candidate_sha256"] != candidate_sha256
        or any(not isinstance(declaration[key], str) or not declaration[key].strip()
               or len(declaration[key]) > limit or any(ord(c) < 32 for c in declaration[key])
               for key, limit in (("reason", 1024), ("authorization_reference", 256)))):
        raise ValueError("invalid external deferral or candidate binding")


def defer_external(output, obligations, reason, authorization_reference):
    _, receipt = load_bound(output)
    if receipt.get("superseded"):
        raise ValueError("candidate was explicitly superseded; prepare a new candidate")
    if not obligations or len(set(obligations)) != len(obligations) or set(obligations) - set(receipt["external_obligations"]):
        raise ValueError("unknown or duplicate external obligation")
    declaration = {"reason": reason, "authorization_reference": authorization_reference,
                   "candidate_sha256": deferral_candidate_sha256(receipt)}
    for name in obligations:
        result = receipt["external_obligations"][name]
        validate_external_deferral({**result, "deferral": declaration}, declaration["candidate_sha256"])
        if "deferral" in result and result["deferral"] != declaration:
            raise ValueError("external deferral already recorded; preserve its authorization")
    for name in obligations:
        receipt["external_obligations"][name]["deferral"] = declaration.copy()
    save(output, receipt)
    event(output, "external-deferred", obligations=obligations, **declaration)
    print(f"DEFERRED external={','.join(obligations)} overall={receipt['result']} receipt={output / 'receipt.json'}", flush=True)
    return 0


PROCESS_COVERAGE_DIAGNOSTIC = {"status": "not-applicable", "reason": "optional process coverage diagnostic; ordinary process contracts run in verify"}


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
    receipt["coverage"] = {kind: sorted(name for name, item in receipt["checks"].items()
        if item["status"] == "passed" and bool(item.get("inherited")) == inherited)
        for kind, inherited in (("executed", False), ("inherited", True))}
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
    if receipt["result"] == "passed" and any(
        item["status"] != "passed" and not optional_external_not_run(item)
        for item in receipt["external_obligations"].values()):
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


class Invocation:
    """One selection run, distinct from the candidate's accumulated evidence."""
    OUTCOME_FIELDS = ("status", "exit_code", "termination", "signal", "reason", "duration_seconds",
                      "log", "log_sha256", "failure_class", "last_completed_phase", "cleanup",
                      "cleanup_status", "capture_status", "environment_evidence", "slot")

    def __init__(self, output, receipt, selected, pending, excluded):
        self.output, self.receipt = output, receipt
        self.id = uuid.uuid4().hex
        self.path = output / "runs" / (self.id + ".json")
        self.selected, self.pending = sorted(selected), set(pending)
        self.attempted, self.outcomes = set(), {}
        self.not_started_reasons = {}
        self.reused = sorted(name for name in selected - self.pending
                             if receipt["checks"][name]["status"] == "passed")
        self.document = {"schema_version": 1, "invocation_id": self.id,
            "candidate": {key: receipt.get(key) for key in
                          ("version", "source_fingerprint", "runtime_sha256", "candidate_bundle_sha256")},
            "started_at": datetime.now(timezone.utc).isoformat(), "selected": self.selected,
            "excluded": sorted(excluded), "state": "running"}
        self.path.parent.mkdir(mode=0o700, exist_ok=True)
        sync_directory(output)
        self.write()
        print(f"INVOCATION id={self.id} result={self.path}", flush=True)

    def start(self, name):
        self.attempted.add(name)
        try:
            self.write()
        except Exception:
            self.attempted.remove(name)
            self.not_started_reasons[name] = "dispatch-capture-error"
            raise

    def cancel_start(self, name):
        self.attempted.remove(name)
        self.not_started_reasons[name] = "interrupted-before-dispatch"
        self.write()

    def record(self, name, result):
        self.outcomes[name] = {key: result[key] for key in self.OUTCOME_FIELDS if key in result}

    def write(self, state="running", reason="not-dispatched", capture_status="recorded"):
        checks = {}
        for name in self.selected:
            if name in self.attempted:
                checks[name] = self.outcomes.get(name, {"status": "incomplete", "reason": "attempt-result-unavailable",
                                                       "exit_code": None, "termination": "unknown",
                                                       "capture_status": "unknown", "cleanup_status": "unknown"})
            elif name in self.reused or name not in self.pending:
                checks[name] = {key: value for key, value in self.receipt["checks"][name].items()
                                if key in self.OUTCOME_FIELDS}
            else:
                checks[name] = {"status": "not-started", "reason": self.not_started_reasons.get(name, reason)}
        statuses = [item["status"] for item in checks.values()]
        selected_status = ("failed" if "failed" in statuses else
                           "passed" if all(status in ("passed", "not-applicable") for status in statuses)
                           else "incomplete")
        # A running or failed capture can never certify the selected invocation.
        if state in ("running", "interrupted", "error") and selected_status == "passed":
            selected_status = "incomplete"
        receipt_bytes = (self.output / "receipt.json").read_bytes() if (self.output / "receipt.json").is_file() else None
        saved_receipt = json.loads(receipt_bytes) if receipt_bytes is not None else {}
        self.document.update(state=state, attempted=sorted(self.attempted), reused=self.reused,
            not_started=[{"check": name, "reason": item["reason"]} for name, item in checks.items()
                         if item["status"] == "not-started"], checks=checks,
            selected_status=selected_status, reproducible_status=saved_receipt.get("reproducible_result", "incomplete"),
            overall_status=saved_receipt.get("result", "incomplete"), capture_status=capture_status,
            receipt="receipt.json", receipt_sha256=hashlib.sha256(receipt_bytes).hexdigest() if receipt_bytes is not None else None)
        if state != "running":
            self.document["finished_at"] = datetime.now(timezone.utc).isoformat()
        temporary = self.path.with_suffix(".json.next")
        with private_file(temporary) as stream:
            stream.write(json.dumps(self.document, indent=2, sort_keys=True) + "\n")
            stream.flush()
            os.fsync(stream.fileno())
        temporary.replace(self.path)
        sync_directory(self.path.parent)

    def finish(self, state, reason="not-dispatched", capture_status="recorded"):
        self.write(state, reason, capture_status)
        event(self.output, "run-result", invocation_id=self.id,
              status="interrupted" if state == "interrupted" else self.document["overall_status"],
              state=state, selected_status=self.document["selected_status"],
              reproducible_status=self.document["reproducible_status"], overall_status=self.document["overall_status"],
              result=f"runs/{self.id}.json", receipt_sha256=self.document["receipt_sha256"],
              capture_status=capture_status)
        print(f"INVOCATION id={self.id} state={state} selected={self.document['selected_status']} "
              f"reproducible={self.document['reproducible_status']} overall={self.document['overall_status']} "
              f"result={self.path}", flush=True)


def execute(name, command, root, output, env, slot=None):
    log = output / (name.replace(":", "-").replace("/", "-") + "-" + uuid.uuid4().hex[:8] + ".log")
    argv = command + (["--slot", str(slot)] if slot else [])
    print(f"RUN {name}" + (f" slot={slot}" if slot else ""), flush=True)
    start = time.monotonic()
    event(output, "check-start", check=name, slot=slot)
    interrupted = False
    cleanup_status = None
    markers = set()
    phases = {}
    offset = 0
    next_heartbeat = start + HEARTBEAT_SECONDS
    process, code, capture_error = None, None, False
    try:
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
                offset = phase_events(output, name, log, offset, markers, phases=phases)
            if STOP.is_set() and not interrupted:
                interrupted = True
                cleanup_status = interrupt_group(process)
            code = process.poll() if cleanup_status == "unconfirmed" else process.wait()
            stream.flush()
            os.fsync(stream.fileno())
        phase_events(output, name, log, offset, markers, final=True, phases=phases)
    except (OSError, subprocess.SubprocessError):
        capture_error = True
        if process is not None:
            if process.poll() is None:
                cleanup_status = interrupt_group(process)
            code = process.poll()
    try:
        log_sha256 = digest(log)
    except OSError:
        log_sha256, capture_error = None, True
    blocked = capacity_reason(log) if code == 4 and not interrupted else None
    result = {"status": "interrupted" if interrupted else "blocked" if blocked else "passed" if code == 0 and not capture_error else "failed", "exit_code": code,
              "duration_seconds": round(time.monotonic() - start, 2), "log": log.name,
              "log_sha256": log_sha256, "command": command_identity(command, root)}
    result["termination"] = ("signal" if code is not None and (code < 0 or code in (130, 137, 143))
                             else "timeout" if code == 124 else "exit" if code is not None else "unknown")
    if result["termination"] == "signal":
        result["signal"] = -code if code < 0 else code - 128
    result["last_completed_phase"] = phases.get("last_completed_phase", "unknown")
    if phases.get("cleanup"):
        result["cleanup"] = phases["cleanup"]
        if result["status"] == "passed" and any(item["exit_code"] != 0 for item in phases["cleanup"].values()):
            result["status"] = "failed"
    result["capture_status"] = "failed" if capture_error else "recorded"
    result["cleanup_status"] = "recorded" if phases.get("cleanup") else "unknown"
    if phases.get("budgets"):
        result["phase_budgets"] = phases["budgets"]
    if code != 0:
        result["failure_class"] = ("environment" if blocked else "timeout" if code == 124
                                   else phases.get("failure_class", "unknown"))
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
    try:
        event(output, "check-result", check=name, **{key: value for key, value in result.items() if key != "command"})
    except OSError:
        result["capture_status"] = "failed"
        if result["status"] == "passed":
            result["status"] = "failed"
    print(f"RESULT {name} {result['status']} exit={code} log={log}", flush=True)
    return result


def phase_events(output, name, log, offset, seen, final=False, phases=None):
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
                if phases is not None and stage[2] == b"end":
                    phase = stage[1].decode()
                    if phase == "cleanup/release" or "cleanup" in phase:
                        key = phase + ("/vm" + stage[5].decode() if stage[5] else "")
                        phases.setdefault("cleanup", {})[key] = {"exit_code": int(stage[4]), "duration_seconds": int(stage[3])}
                    elif stage[4] == b"0":
                        phases["last_completed_phase"] = phase
            failure = re.fullmatch(rb"E2E_FAILURE class=(product|fixture|environment|timeout|unknown)\n", line)
            if failure and phases is not None:
                phases["failure_class"] = failure[1].decode()
            budget = re.fullmatch(rb"E2E_BUDGET phase=(guest-cleanup|fixture/[a-z0-9-]{1,60}) budget_seconds=([0-9]{1,5}) grace_seconds=([0-9]{1,4})(?: vm=([12]))?\n", line)
            if budget and phases is not None and bool(budget[4]) == (budget[1] == b"guest-cleanup"):
                key = budget[1].decode() + ("/vm" + budget[4].decode() if budget[4] else "")
                phases.setdefault("budgets", {})[key] = {"budget_seconds": int(budget[2]), "grace_seconds": int(budget[3])}
            release = re.fullmatch(rb"E2E_RELEASE state=(start|end) duration_seconds=([0-9]{1,9}) budget_seconds=180 grace_seconds=5 exit_code=([0-9]{1,3})\n", line)
            if release and int(release[3]) <= 255 and (release[1] != b"start" or (release[2] == b"0" and release[3] == b"0")):
                event(output, "lease-release", check=name, state=release[1].decode(), duration_seconds=int(release[2]),
                      budget_seconds=180, grace_seconds=5, exit_code=int(release[3]))
                if phases is not None:
                    phases.setdefault("budgets", {})["lease-release"] = {"budget_seconds":180, "grace_seconds":5}
                    if release[1] == b"end":
                        phases.setdefault("cleanup", {})["lease-release"] = {"exit_code":int(release[3]), "duration_seconds":int(release[2])}
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
                return None
    return reason


def prepare(output, version, prepared=None):
    output.mkdir(parents=True, exist_ok=True)
    if any(output.iterdir()):
        raise ValueError("prepare requires an empty output directory")
    output.chmod(0o700)
    event(output, "prepare-start", version=version)
    identity = json.loads((prepared / "identity.json").read_text()) if prepared else None
    if identity and identity["version"] != version:
        raise ValueError("prepared version mismatch")
    original = identity["source_fingerprint"] if identity else source_fingerprint(ROOT)
    root = output / "source"
    root.mkdir()
    with tempfile.TemporaryDirectory(prefix=".source-", dir=output) as temporary:
        source_archive = Path(temporary) / "source.tar.gz"
        if prepared:
            if digest(prepared / "source.tar.gz") != identity["source_archive_sha256"]:
                raise ValueError("prepared source archive mismatch")
            shutil.copyfile(prepared / "source.tar.gz", source_archive)
        else:
            bundle(ROOT, source_archive)
        with tarfile.open(source_archive, "r:gz") as archive:
            for member in archive.getmembers():
                if member.name.startswith("/") or ".." in Path(member.name).parts or not (member.isfile() or member.isdir()):
                    raise ValueError("unsafe source archive member")
            archive.extractall(root, filter="data")
            for member in archive.getmembers():
                path = root / member.name
                path.chmod(0o755 if member.isdir() or member.mode & 0o111 else 0o644)
    if source_fingerprint(root) != original or (not prepared and source_fingerprint(ROOT) != original):
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
    receipt["checks"]["process-coverage"] = PROCESS_COVERAGE_DIAGNOSTIC.copy()
    if prepared:
        assets = prepared / "assets"
        actual = {path.name: digest(path) for path in assets.iterdir() if path.is_file() and not path.is_symlink()}
        if actual != identity["release_files"] or len(list(assets.iterdir())) != len(actual):
            raise ValueError("prepared release assets mismatch")
        for path in assets.iterdir():
            shutil.copyfile(path, release / path.name)
            binary_names = {f"yard-{version}-linux-amd64", f"yard-{version}-linux-arm64"}
            (release / path.name).chmod(0o755 if path.name in binary_names or path.name.endswith(".sh") else 0o644)
        receipt["preparation"] = identity
        log = output / "package-ci.log"
        with private_file(log) as stream:
            stream.write("Imported exact prepared CI assets: " + identity["candidate_id"] + "\n")
        receipt["checks"]["package"] = {"status": "passed", "exit_code": 0,
            "log": log.name, "log_sha256": digest(log), "command": ["ci-prepared", identity["candidate_id"]]}
    else:
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


def load_bound(output, entries=None, context=None):
    receipt = json.loads((output / "receipt.json").read_text())
    root = output / "source"
    fingerprint = source_fingerprint(root) if entries is None else entries_fingerprint(entries)
    if receipt.get("schema_version") != 1 or fingerprint != receipt["source_fingerprint"]:
        raise ValueError("frozen source fingerprint mismatch")
    if "preparation" in receipt:
        identity = receipt["preparation"]
        if any(identity.get(key) != receipt.get(key) for key in ("version", "source_fingerprint", "release_files")):
            raise ValueError("prepared candidate receipt binding changed")
        if receipt["checks"].get("package", {}).get("command") != ["ci-prepared", identity.get("candidate_id")]:
            raise ValueError("prepared package provenance changed")
    if not isinstance(receipt.get("environment"), dict) or set(receipt["environment"]) != {"system", "machine", "go"}:
        raise ValueError("candidate execution environment is missing")
    if profile_inventory(root) != receipt["profiles"]:
        raise ValueError("profile obligations changed")
    external = external_inventory(root)
    if set(external) != set(receipt["external_obligations"]) or any(
        ("required" in receipt["external_obligations"][name]) != ("required" in item) or
        receipt["external_obligations"][name].get("required", True) is not item.get("required", True) or
        any(receipt["external_obligations"][name].get(key) != value
            for key, value in item.items() if key != "status") for name, item in external.items()):
        raise ValueError("external obligations changed")
    for name, result in receipt["external_obligations"].items():
        if result["status"] == "not-run" and external[name].get("required", True) is False:
            expected = {**external[name], **({"deferral": result["deferral"]} if "deferral" in result else {})}
            if result != expected:
                raise ValueError("optional external not-run result contains execution evidence")
        if "deferral" in result:
            validate_external_deferral(result, deferral_candidate_sha256(receipt))
    required_commands = commands(root, receipt["version"], receipt["profiles"])
    if context is not None:
        context.update(commands=required_commands, external=external, attempts={})
    if set(receipt["checks"]) != set(required_commands) | {"package"}:
        raise ValueError("required check inventory is incomplete")
    if receipt["checks"]["process-coverage"]["status"] == "not-applicable" and receipt["checks"]["process-coverage"] != PROCESS_COVERAGE_DIAGNOSTIC:
        raise ValueError("invalid optional process coverage diagnostic")
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
    for name, result in receipt["checks"].items():
        if "original_attempt" in result:
            attempt = original_attempt(output, receipt, name, result)
            if context is not None:
                context["attempts"][name] = attempt
        if result["status"] == "passed":
            log = result.get("log", "")
            if not log or Path(log).name != log or digest(output / log) != result.get("log_sha256"):
                raise ValueError("passing result evidence is missing or changed")
            if name in required_commands and required_commands[name] is not None and "command" in result:
                if result["command"] != command_identity(required_commands[name], root):
                    raise ValueError("passing command provenance changed")
            if result.get("inherited") and result.get("evidence_transfer", {}).get("schema_version") != 2 and (not re.fullmatch(r"[0-9a-f]{64}", result.get("from_receipt_sha256", ""))
                                            or result.get("from_log_sha256") != result["log_sha256"]):
                raise ValueError("inherited evidence provenance is incomplete")
            if "evidence_transfer" in result:
                if not result.get("inherited"):
                    raise ValueError("reviewed transfer must remain inherited coverage")
                validate_transferred_result(output, receipt, name, result, required_commands[name], root)
                continue
            if result.get("inherited"):
                binding = result.get("inherited_from", {})
                if (binding.get("target_source_fingerprint") != receipt["source_fingerprint"]
                    or not re.fullmatch(r"[0-9a-f]{64}", binding.get("source_fingerprint", ""))):
                    raise ValueError("inherited source provenance is incomplete")
                if binding.get("scope") == "exact-source":
                    if binding["source_fingerprint"] != receipt["source_fingerprint"]:
                        raise ValueError("inherited exact source differs")
                elif binding.get("scope") == "other-profile-tests-only" and name.startswith("profile:"):
                    closure = profile_closure(root, receipt["profiles"], name, entries)
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
        receipt["checks"]["process-coverage"] = PROCESS_COVERAGE_DIAGNOSTIC.copy()
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
              "external_prerequisites": [{"id": name, "status": entry["status"], "required": entry.get("required", True),
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
               and (name != "process-coverage" or receipt["checks"][name] != PROCESS_COVERAGE_DIAGNOSTIC or only or name in prioritized)
               and (rerun or receipt["checks"][name]["status"] != "passed")]
    live = [name for name in pending if is_live(name)]
    if live and not slots:
        raise ValueError("live checks require --slots N [N ...]")
    try:
        invocation = Invocation(output, receipt, selected, pending, excluded)
    except OSError:
        print("release-acceptance: invocation capture failed before dispatch", file=sys.stderr)
        raise
    failure_reason = "not-dispatched"
    lock = threading.Lock()
    try:
        receipt["operator_exclusions"] = sorted(excluded)
        save(output, receipt)
        event(output, "run-start", invocation_id=invocation.id, checks=pending, slots=slots, exclusions=sorted(excluded))
        for name in invocation.reused:
            origin = "inherited" if receipt["checks"][name].get("inherited") else "direct"
            print(f"REUSE {name} {origin}: validated command, inputs, environment and complete PASS", flush=True)
            event(output, "check-reuse", invocation_id=invocation.id, check=name,
                  origin=origin, reason="validated-applicable-pass")

        def attempt(name, slot=None):
            # Register only immediately before execute: a stopped queue retains no old PASS.
            with lock:
                if STOP.is_set():
                    return None
                invocation.start(name)
            if STOP.is_set():
                with lock:
                    invocation.cancel_start(name)
                return None
            with lock:
                receipt["checks"][name] = {"status": "running", "command": command_identity(checks[name], root)}
                save(output, receipt)  # Invalidate a previous PASS before any child can start.
            result = execute(name, checks[name], root, output, env, slot) if slot else execute(name, checks[name], root, output, env)
            result["execution_environment"] = measured_environment
            result["original_attempt"] = completed_attempt(output, root, receipt, name, result)
            return result

        for name in [item for item in pending if item not in live]:
            if STOP.is_set():
                break
            result = attempt(name)
            if result is None:
                break
            receipt["checks"][name] = result
            invocation.record(name, result)
            save(output, receipt)
            invocation.write()
            if result["status"] != "passed":
                failure_reason = "local-check-stop"
                break
        local_names = [name for name in checks if not is_live(name)] + (["package"] if "package" in receipt["checks"] else [])
        if live and any(receipt["checks"][name]["status"] != "passed"
                        and not (name == "process-coverage" and receipt["checks"][name] == PROCESS_COVERAGE_DIAGNOSTIC)
                        for name in local_names):
            print("INCOMPLETE: live checks await successful local gates", flush=True)
            invocation.finish("interrupted" if STOP.is_set() else "finished", "prerequisite-stop")
            return 1
        # Physical controllers consume the frozen transport; local tests keep normal bundling.
        env.update(SUBYARD_E2E_CANDIDATE_BUNDLE=str(output / "candidate-bundle.tar.gz"),
                   SUBYARD_E2E_CANDIDATE_SHA256=receipt["candidate_bundle_sha256"],
                   SUBYARD_E2E_LOCAL_EVIDENCE=str(output),
                   SUBYARD_E2E_CONTROLLER_WORKSPACE=str(ROOT))
        work = queue.Queue()
        for name in live:
            work.put(name)
        def collect(slot):
            while not STOP.is_set():
                try:
                    name = work.get_nowait()
                except queue.Empty:
                    return
                result = attempt(name, slot)
                if result is None:
                    return
                with lock:
                    receipt["checks"][name] = result
                    invocation.record(name, result)
                    save(output, receipt)
                    invocation.write()
                capacity_blocked = (result["status"] == "blocked" and result.get("exit_code") == 4
                                    and result.get("termination") == "exit"
                                    and result.get("reason") in ("capacity_memory", "capacity_disk"))
                if result["status"] != "passed" and not capacity_blocked:
                    return
        with ThreadPoolExecutor(max_workers=max(1, len(slots))) as pool:
            futures = [pool.submit(collect, slot) for slot in slots]
            for future in as_completed(futures):
                future.result()
        if source_fingerprint(root) != receipt["source_fingerprint"]:
            receipt["checks"]["verify"]["status"] = "failed"
            receipt["error"] = "source changed during acceptance"
            if "verify" in invocation.outcomes:
                invocation.record("verify", receipt["checks"]["verify"])
        save(output, receipt)
        invocation.finish("interrupted" if STOP.is_set() else "finished",
                          "interrupted-before-dispatch" if STOP.is_set() else failure_reason)
        optional = [name for name, item in receipt["external_obligations"].items() if optional_external_not_run(item)]
        if optional:
            print("ACCEPTANCE external_optional_not_run=" + ",".join(optional) + " status=not-run", flush=True)
        print(f"ACCEPTANCE reproducible={receipt['reproducible_result']} overall={receipt['result']} receipt={output / 'receipt.json'}", flush=True)
        return 0 if receipt["result"] == "passed" and not STOP.is_set() else 1
    except Exception:
        # Keep the current identity even when capture fails; never fall back to another run.
        try:
            invocation.finish("error", "collector-error", "failed")
        except Exception:
            print(f"release-acceptance: invocation capture failed id={invocation.id} result={invocation.path}", file=sys.stderr)
        raise


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


# These exact-key wire objects are shared with the publication validator. Hashes bind
# the exact canonical bytes, without a newline, including all measured nulls.
ATTEMPT_FIELDS = ("status", "exit_code", "termination", "signal", "duration_seconds",
    "log_sha256", "execution_environment", "environments", "environment_evidence",
    "slot", "cleanup", "cleanup_status", "capture_status", "last_completed_phase",
    "phase_budgets", "failure_class", "reason")


def evidence_bytes(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode()


def evidence_hash(value):
    return hashlib.sha256(evidence_bytes(value)).hexdigest()


def is_sha(value):
    return isinstance(value, str) and re.fullmatch(r"[0-9a-f]{64}", value) is not None


def public_text(value, limit):
    return isinstance(value, str) and bool(value.strip()) and len(value) <= limit and not any(ord(c) < 32 for c in value)


def validate_candidate_identity(candidate):
    if (not isinstance(candidate, dict) or set(candidate) != {"version", "source_fingerprint", "runtime_file",
        "runtime_sha256", "candidate_bundle_sha256", "release_files", "candidate_sha256"}
        or not isinstance(candidate["version"], str) or not candidate["version"]
        or not isinstance(candidate["runtime_file"], str) or Path(candidate["runtime_file"]).name != candidate["runtime_file"]
        or any(not is_sha(candidate[key]) for key in ("source_fingerprint", "runtime_sha256", "candidate_bundle_sha256", "candidate_sha256"))
        or not isinstance(candidate["release_files"], dict) or not candidate["release_files"]
        or any(not isinstance(key, str) or not key or Path(key).name != key or not is_sha(value)
               for key, value in candidate["release_files"].items())
        or candidate["release_files"].get(candidate["runtime_file"]) != candidate["runtime_sha256"]):
        raise ValueError("invalid immutable candidate identity")


def entries_fingerprint(entries):
    fingerprint = hashlib.sha256()
    for name, item in sorted(entries.items(), key=lambda entry: os.fsencode(entry[0])):
        fingerprint.update(os.fsencode(name) + b"\0" + (b"755\0" if item[0] == "executable" else b"644\0"))
        fingerprint.update(b"link\0" + os.fsencode(item[1][5:]) if item[1].startswith("link:") else bytes.fromhex(item[1]))
    return fingerprint.hexdigest()


def validate_manifest(manifest):
    if (not isinstance(manifest, dict) or set(manifest) != {"schema_version", "candidate", "source_entries", "environment"}
        or type(manifest["schema_version"]) is not int or manifest["schema_version"] != 1):
        raise ValueError("invalid candidate manifest")
    validate_candidate_identity(manifest["candidate"])
    environment = manifest["environment"]
    if (not isinstance(environment, dict) or set(environment) != {"system", "machine", "go"}
        or any(not isinstance(value, str) or not value or len(value) > 256 or any(ord(c) < 32 for c in value)
               for value in environment.values())):
        raise ValueError("invalid manifest environment")
    entries = manifest["source_entries"]
    if not isinstance(entries, dict) or not entries:
        raise ValueError("missing manifest source inventory")
    for name, item in entries.items():
        if (not isinstance(name, str) or not name or name.startswith("/") or ".." in Path(name).parts
            or name.startswith(".subyard-acceptance/") or not isinstance(item, list) or len(item) != 2
            or item[0] not in ("regular", "executable") or not isinstance(item[1], str)
            or not (is_sha(item[1]) or item[1].startswith("link:"))):
            raise ValueError("invalid manifest source entry")
    if entries_fingerprint(entries) != manifest["candidate"]["source_fingerprint"]:
        raise ValueError("manifest source inventory fingerprint mismatch")


def validate_attempt(record, manifest, name, command, log_sha256=None):
    validate_manifest(manifest)
    if (not isinstance(record, dict) or set(record) != {"schema_version", "id", "manifest_sha256", "check", "command", "result"}
        or type(record["schema_version"]) is not int or record["schema_version"] != 1
        or not isinstance(record["id"], str) or not re.fullmatch(r"[0-9a-f]{32}", record["id"])
        or record["manifest_sha256"] != evidence_hash(manifest) or record["check"] != name or record["command"] != command
        or not isinstance(command, list) or not command or any(not isinstance(value, str) or not value for value in command)):
        raise ValueError("invalid original attempt binding")
    facts = record["result"]
    if (not isinstance(facts, dict) or set(facts) != set(ATTEMPT_FIELDS)
        or facts["status"] not in ("passed", "failed", "interrupted", "blocked")
        or facts["exit_code"] is not None and type(facts["exit_code"]) is not int
        or facts["termination"] not in (None, "exit", "signal", "timeout", "unknown")
        or facts["log_sha256"] is not None and not is_sha(facts["log_sha256"])
        or log_sha256 is not None and facts["log_sha256"] != log_sha256
        or facts["duration_seconds"] is not None and (type(facts["duration_seconds"]) not in (int, float)
            or not math.isfinite(facts["duration_seconds"]) or facts["duration_seconds"] < 0)
        or facts["slot"] is not None and (type(facts["slot"]) is not int or not 1 <= facts["slot"] <= 999)
        or facts["signal"] is not None and (type(facts["signal"]) is not int or facts["signal"] <= 0)
        or facts["execution_environment"] is not None and facts["execution_environment"] != manifest["environment"]
        or not isinstance(facts["environments"], list)):
        raise ValueError("invalid completed attempt result")
    for item in facts["environments"]:
        if (not isinstance(item, dict) or set(item) != {"type", "vm_count", "base_fingerprint"}
            or not isinstance(item["type"], str) or not re.fullmatch(r"[a-z][a-z0-9-]{0,79}", item["type"])
            or type(item["vm_count"]) is not int or not 1 <= item["vm_count"] <= 100 or not is_sha(item["base_fingerprint"])):
            raise ValueError("invalid attempt physical environment")
    for key, allowed in (("capture_status", (None, "recorded", "failed")),
                         ("environment_evidence", (None, "recorded", "unknown")),
                         ("cleanup_status", (None, "recorded", "unknown", "drained", "unconfirmed")),
                         ("failure_class", (None, "product", "fixture", "environment", "timeout", "unknown"))):
        if facts[key] not in allowed:
            raise ValueError("invalid attempt capture or cleanup state")
    for key in ("reason", "last_completed_phase"):
        if facts[key] is not None and (not isinstance(facts[key], str) or len(facts[key]) > 256
            or any(ord(c) < 32 for c in facts[key])):
            raise ValueError("invalid attempt diagnostic")
    for key, fields in (("cleanup", {"exit_code", "duration_seconds"}), ("phase_budgets", {"budget_seconds", "grace_seconds"})):
        values = facts[key]
        if values is None:
            continue
        if not isinstance(values, dict):
            raise ValueError("invalid attempt phase measurements")
        for phase, item in values.items():
            if (not isinstance(phase, str) or not re.fullmatch(r"[a-zA-Z0-9_./-]{1,100}", phase)
                or not isinstance(item, dict) or set(item) != fields
                or any(type(value) is not int or value < 0 for value in item.values())
                or "exit_code" in item and item["exit_code"] > 255):
                raise ValueError("invalid attempt phase measurement")
    if facts["status"] == "passed" and (facts["exit_code"] != 0 or facts["capture_status"] != "recorded"
        or facts["termination"] != "exit" or not is_sha(facts["log_sha256"])
        or facts["execution_environment"] is None or facts["duration_seconds"] is None
        or facts["cleanup"] and any(item["exit_code"] != 0 for item in facts["cleanup"].values())):
        raise ValueError("invalid or incomplete original attempt PASS")


def immutable_json(path, value):
    path.parent.mkdir(mode=0o700, exist_ok=True)
    path.parent.chmod(0o700)
    sync_directory(path.parent.parent)
    data = evidence_bytes(value)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(data)
        stream.flush()
        os.fsync(stream.fileno())
    sync_directory(path.parent)
    return hashlib.sha256(data).hexdigest()


def persist_manifest(path, manifest):
    # Concurrent physical completions share the same content-addressed manifest.
    with MANIFEST_LOCK:
        if path.exists():
            if digest(path) != evidence_hash(manifest):
                raise ValueError("candidate manifest changed")
        else:
            immutable_json(path, manifest)


def completed_attempt(output, root, receipt, name, result):
    manifest = candidate_manifest(root, receipt)
    manifest_sha = evidence_hash(manifest)
    manifest_path = output / "manifests" / (manifest_sha + ".json")
    persist_manifest(manifest_path, manifest)
    if result.get("log_sha256") is not None:
        log = result.get("log")
        if not isinstance(log, str) or Path(log).name != log or digest(output / log) != result["log_sha256"]:
            raise ValueError("completed attempt log changed")
        with (output / log).open("rb") as stream:
            os.fsync(stream.fileno())
    record = {"schema_version": 1, "id": uuid.uuid4().hex, "manifest_sha256": manifest_sha,
              "check": name, "command": result["command"],
              "result": {key: result.get(key, [] if key == "environments" else None) for key in ATTEMPT_FIELDS}}
    validate_attempt(record, manifest, name, result["command"])
    sha = immutable_json(output / "attempts" / (record["id"] + ".json"), record)
    return {"id": record["id"], "sha256": sha}


def original_attempt(output, receipt, name, result):
    reference = result.get("original_attempt")
    if (not isinstance(reference, dict) or set(reference) != {"id", "sha256"}
        or not isinstance(reference["id"], str) or not re.fullmatch(r"[0-9a-f]{32}", reference["id"])
        or not is_sha(reference["sha256"])):
        raise ValueError("invalid original attempt reference")
    path = output / "attempts" / (reference["id"] + ".json")
    if digest(path) != reference["sha256"]:
        raise ValueError("original attempt record changed")
    record = read_transfer_json(path)
    if not isinstance(record, dict) or not is_sha(record.get("manifest_sha256")):
        raise ValueError("invalid attempt manifest reference")
    manifest_path = output / "manifests" / (record["manifest_sha256"] + ".json")
    if digest(manifest_path) != record["manifest_sha256"]:
        raise ValueError("original candidate manifest changed")
    manifest = read_transfer_json(manifest_path)
    validate_attempt(record, manifest, name, result.get("command"), result.get("log_sha256"))
    if (record["id"] != reference["id"] or manifest["candidate"] != transfer_candidate(receipt)
        or manifest["environment"] != receipt["environment"] or result.get("inherited")
        or record["result"] != {key: result.get(key, [] if key == "environments" else None) for key in ATTEMPT_FIELDS}):
        raise ValueError("current original attempt differs")
    if result.get("log_sha256") is not None:
        log = result.get("log")
        if not isinstance(log, str) or Path(log).name != log or digest(output / log) != result["log_sha256"]:
            raise ValueError("original attempt log changed")
    return record, manifest


def attempt_facts(record):
    facts = record["result"]
    return {"actual_command": record["command"] + (["--slot", str(facts["slot"])] if facts["slot"] is not None else []),
            "duration_seconds": facts["duration_seconds"], "launched_at_utc": None,
            "execution_environment": facts["execution_environment"], "environments": facts["environments"]}


def profile_closure(root, profiles, name, entries=None):
    owner = name.split(":", 1)[1].split("/", 1)[0]
    other_prefixes = tuple("config/profiles/" + profile + "/tests/" for profile in profiles if profile != owner)
    closure = {path: value for path, value in (source_entries(root) if entries is None else entries).items() if not path.startswith(other_prefixes)}
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
        if "evidence_transfer" in result:
            raise ValueError("ordinary import cannot chain reviewed evidence transfers")
        if result.get("inherited"):
            raise ValueError("ordinary import cannot chain inherited evidence")
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
        receipt["checks"][name] = {**{key: value for key, value in result.items() if key != "original_attempt"}, "log": log.name, "inherited": True,
                                   "from_receipt_sha256": source_hash,
                                   "from_log_sha256": result["log_sha256"],
                                   "inherited_from": binding}
        event(output, "evidence-import", check=name, from_receipt_sha256=source_hash,
              from_log_sha256=result["log_sha256"], inherited_from=binding)
    save(output, receipt)
    return 0


def transfer_candidate(receipt):
    return {key: receipt[key] for key in ("version", "source_fingerprint", "runtime_file",
        "runtime_sha256", "candidate_bundle_sha256", "release_files")} | {
        "candidate_sha256": deferral_candidate_sha256(receipt)}


def read_transfer_json(path):
    def unique(pairs):
        result = dict(pairs)
        if len(result) != len(pairs):
            raise ValueError("duplicate evidence fields")
        return result
    if path.stat().st_size > 16 << 20:
        raise ValueError("evidence JSON is too large")
    return json.loads(path.read_text(), object_pairs_hook=unique)


def validate_evidence_transfer(document, name, command, target, log_sha256, manifests=None):
    """Validate the public provenance shared by local receipts and remote publication."""
    if isinstance(document, dict) and document.get("schema_version") == 2:
        keys = {"schema_version", "check", "tested_manifest_sha256", "target_manifest_sha256", "original_attempt",
                "command", "actual_command", "origin", "artifacts", "review", "preparation_environment",
                "execution_environment", "environments", "duration_seconds", "launched_at_utc"}
        if set(document) != keys or type(document["schema_version"]) is not int or document["origin"] != "attempt":
            raise ValueError("invalid original attempt transfer")
        if (not isinstance(document["artifacts"], dict) or not isinstance(manifests, dict)
            or any(not is_sha(document[key]) or document[key] not in manifests for key in ("tested_manifest_sha256", "target_manifest_sha256"))):
            raise ValueError("dangling transfer manifest reference")
        tested, destination = (manifests[document[key]] for key in ("tested_manifest_sha256", "target_manifest_sha256"))
        for key, manifest in (("tested_manifest_sha256", tested), ("target_manifest_sha256", destination)):
            validate_manifest(manifest)
            if evidence_hash(manifest) != document[key]:
                raise ValueError("transfer manifest hash mismatch")
        record = document["original_attempt"]
        validate_attempt(record, tested, name, command, log_sha256)
        if (record["result"]["status"] != "passed" or record["result"]["exit_code"] != 0
            or record["result"]["capture_status"] != "recorded"
            or record["result"]["execution_environment"] is None
            or record["result"]["duration_seconds"] is None
            or is_live(name) and (record["result"]["cleanup_status"] != "recorded" or not record["result"]["cleanup"]
                or record["result"]["environment_evidence"] != "recorded")
            or document["preparation_environment"] != tested["environment"]
            or any(document[key] != value for key, value in attempt_facts(record).items())
            or document["artifacts"] != {"attempt": evidence_hash(record), "manifest": document["tested_manifest_sha256"],
                "target_manifest": document["target_manifest_sha256"], "log": log_sha256, "review": document["artifacts"].get("review")}):
            raise ValueError("transfer original attempt facts changed or incomplete")
        legacy = {key: value for key, value in document.items() if key not in
                  ("tested_manifest_sha256", "target_manifest_sha256", "original_attempt")}
        legacy.update(schema_version=1, origin="receipt", tested=tested["candidate"], target=destination["candidate"],
                      artifacts={"receipt": document["artifacts"]["attempt"], "review": document["artifacts"]["review"], "log": log_sha256})
        validate_evidence_transfer(legacy, name, command, target, log_sha256)
        return
    keys = {"schema_version", "check", "tested", "target", "command", "actual_command", "origin",
            "artifacts", "review", "preparation_environment", "execution_environment", "environments",
            "duration_seconds", "launched_at_utc"}
    if (not isinstance(document, dict) or set(document) != keys or type(document["schema_version"]) is not int
        or document["schema_version"] != 1 or name == "package" or document["check"] != name or document["command"] != command
        or document["origin"] not in ("receipt", "standalone")):
        raise ValueError("invalid reviewed evidence transfer")
    for candidate in (document["tested"], document["target"]):
        validate_candidate_identity(candidate)
        if (not public_text(candidate["version"], 64) or not public_text(candidate["runtime_file"], 128)
            or any(not public_text(filename, 128) for filename in candidate["release_files"])):
            raise ValueError("invalid transferred candidate identity")
    if (any(document["target"].get(key) != value for key, value in target.items())
        or document["tested"]["version"] != document["target"]["version"]):
        raise ValueError("reviewed transfer target changed")
    review = document["review"]
    if (not isinstance(review, dict) or set(review) != {"unchanged_scope", "reason", "authorization_reference"}
        or any(not public_text(review[key], limit) for key, limit in
               (("unchanged_scope", 2048), ("reason", 1024), ("authorization_reference", 256)))):
        raise ValueError("reviewed transfer requires scope, reason and authorization")
    roles = {"receipt", "review", "log"} | ({"native", "launch"} if document["origin"] == "standalone" else set())
    if (not isinstance(document["artifacts"], dict) or set(document["artifacts"]) != roles
        or any(not is_sha(value) for value in document["artifacts"].values())
        or document["artifacts"]["log"] != log_sha256):
        raise ValueError("reviewed transfer original evidence missing")
    for key in ("preparation_environment", "execution_environment"):
        environment = document[key]
        if environment is None and key == "execution_environment" and document["origin"] == "standalone":
            continue
        if (not isinstance(environment, dict) or set(environment) != {"system", "machine", "go"}
            or any(not public_text(value, 256) for value in environment.values())):
            raise ValueError("transferred execution provenance missing")
    actual = document["actual_command"]
    if not isinstance(actual, list) or not actual or any(not public_text(value, 4096) for value in actual):
        raise ValueError("transferred actual command missing")
    actual_prefix = (["timeout", "--signal=TERM", "--kill-after=300s", "90m", "env", "TMPDIR=/tmp",
        "SUBYARD_E2E_CONTROLLER_WORKSPACE=${CONTROLLER_WORKSPACE}", "SUBYARD_E2E_CANDIDATE_BUNDLE=${TESTED_BUNDLE}",
        "SUBYARD_E2E_CANDIDATE_SHA256=" + document["tested"]["candidate_bundle_sha256"]]
        if document["origin"] == "standalone" else [])
    base = actual_prefix + command
    if (actual[:len(base)] != base or (actual != base and
        (len(actual) != len(base) + 2 or actual[-2] != "--slot" or not re.fullmatch(r"[1-9][0-9]{0,2}", actual[-1])))
        or document["origin"] == "standalone" and (actual == base or document["duration_seconds"] is not None
                                                    or document["execution_environment"] is not None)):
        raise ValueError("transferred actual check command differs")
    environments = document["environments"]
    if not isinstance(environments, list) or (is_live(name) and not environments):
        raise ValueError("transferred physical environment missing")
    for item in environments:
        if (not isinstance(item, dict) or set(item) != {"type", "vm_count", "base_fingerprint"}
            or not re.fullmatch(r"[a-z][a-z0-9-]{0,79}", item.get("type", ""))
            or type(item["vm_count"]) is not int or not 1 <= item["vm_count"] <= 100 or not is_sha(item["base_fingerprint"])):
            raise ValueError("invalid transferred physical environment")
    duration = document["duration_seconds"]
    if duration is not None and (type(duration) not in (int, float) or not math.isfinite(duration) or duration < 0):
        raise ValueError("invalid transferred duration")
    launched = document["launched_at_utc"]
    if launched is not None:
        try:
            if not public_text(launched, 64) or datetime.fromisoformat(launched).utcoffset() is None:
                raise ValueError()
        except (TypeError, ValueError):
            raise ValueError("invalid transferred launch time") from None


def validate_standalone_bundle(root, receipt, entries=None):
    """Bind an older raw launcher to the original frozen source and runtime bytes."""
    if entries is None:
        bound_root, bound = load_bound(root.parent)
        if bound_root != root or transfer_candidate(bound) != transfer_candidate(receipt):
            raise ValueError("standalone original frozen candidate changed")
        entries = source_entries(root)
    paths = sorted(map(os.fsencode, entries))
    extras = {".subyard-acceptance/candidate.json": digest(root / ".subyard-acceptance/candidate.json"),
              **{".subyard-acceptance/release/" + name: sha for name, sha in receipt["release_files"].items()}}
    expected = set(map(os.fsdecode, paths)) | set(extras)
    directories = {str(parent) for name in expected for parent in Path(name).parents if parent != Path(".")}
    try:
        with tarfile.open(root.parent / "candidate-bundle.tar.gz", "r:gz") as archive:
            members = {}
            # Iterate headers and reject before retaining any unowned member;
            # both our inventory and tarfile's header cache stay bounded.
            for member in archive:
                name = member.name.removeprefix("./")
                if (name.startswith("/") or ".." in Path(name).parts or name in members
                    or not (member.isfile() and name in expected or member.isdir() and name in directories)):
                    raise ValueError("unsafe standalone tested bundle member")
                members[name] = member
            if {name for name, member in members.items() if member.isfile()} != expected:
                raise ValueError("standalone tested bundle inventory differs")
            fingerprint = hashlib.sha256()
            for raw in paths:
                member = members[os.fsdecode(raw)]
                fingerprint.update(raw + b"\0")
                fingerprint.update(f"{0o755 if member.mode & 0o111 else 0o644:o}\0".encode())
                with archive.extractfile(member) as stream:
                    fingerprint.update(bytes.fromhex(hashlib.file_digest(stream, "sha256").hexdigest()))
            if fingerprint.hexdigest() != receipt["source_fingerprint"]:
                raise ValueError("standalone tested bundle source differs")
            for name, sha in extras.items():
                with archive.extractfile(members[name]) as stream:
                    if hashlib.file_digest(stream, "sha256").hexdigest() != sha:
                        raise ValueError("standalone tested bundle runtime or metadata differs")
    except tarfile.TarError:
        raise ValueError("invalid standalone tested bundle archive") from None


# This exact historical producer asserts guest rc=23 in transport_probes(), and
# its direct cleanup trap exits 3 on any cleanup/release failure. It predates
# expected-negative and cleanup/release telemetry; unknown producers stay strict.
HISTORICAL_P0_SHA256 = "97c607ffc81cce7d11646e45702ee68b5cb6a0de316e7726d308c25e765d59f5"


def validate_historical_p0_log(path, bundle, slot):
    expected = ["lease", "transport-start", "negative", "negative-cleanup", "transport-end",
                "cleanup-start", "clean-1", "final-guest-1", "final-cleanup-1",
                "clean-2", "final-guest-2", "final-cleanup-2", "cleanup-end", "success", "release"]
    events, active, owner, lease = [], {}, None, None
    def invalid():
        raise ValueError("historical P0 controller evidence differs")
    with path.open("rb") as stream:
        for line in stream:
            if events and events[-1] == "release" and line.strip():
                invalid()
            if line.startswith(b"E2E lease:"):
                if not re.fullmatch(rb"E2E lease: [^\r\n]{0,2000} type=subyard-pair vms=2 base=[0-9a-f]{64}\n", line):
                    invalid()
                fields = [item.split(b"=", 1) for item in line.split()[2:]]
                if any(len(item) != 2 for item in fields) or len(dict(fields)) != len(fields):
                    invalid()
                fields = dict(fields)
                if fields.get(b"slot") != f"slot-{int(slot):03d}".encode() or not fields.get(b"run"):
                    invalid()
                lease = fields[b"slot"], fields[b"run"]
                events.append("lease")
            marker = re.fullmatch(rb"  \[ (\.\.|ok) \] phase=(transport|cleanup) (bundle=([0-9a-f]{64})|duration=[0-9]{1,9}s)\n", line)
            if not marker and re.match(rb"  \[ (?:\.\.|ok) \] phase=(?:transport|cleanup)(?:[ \r\n]|$)", line):
                invalid()
            if marker:
                phase = marker[2].decode()
                if active:
                    invalid()
                if marker[1] == b"..":
                    if owner is not None or marker[4] != bundle.encode():
                        invalid()
                    owner = phase
                    events.append(phase + "-start")
                else:
                    if owner != phase or marker[4] is not None:
                        invalid()
                    owner = None
                    events.append(phase + "-end")
            if line.startswith(b"E2E_PHASE"):
                stage = re.fullmatch(rb"E2E_PHASE phase=(allocation|packing|transport|guest|guest-cleanup|cleanup/release|fixture/[a-z0-9-]{1,60}) state=(start|end) duration_seconds=([0-9]{1,9}) exit_code=([0-9]{1,3})(?: vm=([12]))?\n", line)
                if not stage or int(stage[4]) > 255:
                    invalid()
                key = stage[1], stage[5]
                if stage[2] == b"start":
                    if active or stage[3] != b"0" or stage[4] != b"0":
                        invalid()
                    pending = {"negative": b"1", "final-guest-1": b"1", "final-guest-2": b"2"}.get(events[-1] if events else None)
                    if pending is not None and key != (b"guest-cleanup", pending):
                        invalid()
                    active[key] = owner
                else:
                    if key not in active:
                        invalid()
                    started_owner = active.pop(key)
                    if stage[4] != b"0":
                        if key != (b"guest", b"1") or stage[4] != b"23" or owner != "transport" or started_owner != owner:
                            invalid()
                        events.append("negative")
                    elif key == (b"guest-cleanup", b"1") and owner == "transport":
                        events.append("negative-cleanup")
                    elif owner == "cleanup" and stage[5] is not None:
                        vm = stage[5].decode()
                        if stage[1] == b"guest" and events[-1:] == ["clean-" + vm]:
                            events.append("final-guest-" + vm)
                        elif stage[1] == b"guest-cleanup" and events[-1:] == ["final-guest-" + vm]:
                            events.append("final-cleanup-" + vm)
            clean = re.fullmatch(rb"ok: VM([12]) exact P0 state, pools, projects and releases removed\n", line)
            if clean:
                if owner != "cleanup" or (b"guest", clean[1]) not in active:
                    invalid()
                events.append("clean-" + clean[1].decode())
            if line == b"ok: P0 release smoke passed within one broker lease\n":
                events.append("success")
            released = re.fullmatch(rb"E2E lease released: slot=([^\r\n ]+) run=([^\r\n ]+)\n", line)
            if released:
                if (released[1], released[2]) != lease:
                    invalid()
                events.append("release")
    if events != expected or active or owner is not None:
        invalid()


def standalone_result(root, receipt, name, command, files, verify_source=True, entries=None):
    """Verify existing bounded controller evidence, without inventing collector timing."""
    native = read_transfer_json(files["native"])
    if (type(native.get("exit_code")) is not int or native["exit_code"] != 0 or native.get("session_id") is not None
        or not isinstance(native.get("chunk_id"), str) or not isinstance(native.get("output"), str)
        or type(native.get("wall_time_seconds")) not in (int, float)
        or not math.isfinite(native["wall_time_seconds"]) or native["wall_time_seconds"] < 0):
        raise ValueError("standalone native execution is not a complete PASS")
    launch = read_transfer_json(files["launch"])
    if not is_live(name) or launch.get("cwd") != str(root):
        raise ValueError("standalone launcher candidate differs")
    bindings = {"source_fingerprint", "runtime_sha256", "bundle_sha256", "acceptance_script_sha256"}
    raw = not (bindings & set(launch))
    if not raw and (not bindings <= set(launch)
        or launch["source_fingerprint"] != receipt["source_fingerprint"]
        or launch["runtime_sha256"] != receipt["runtime_sha256"]
        or launch["bundle_sha256"] != receipt["candidate_bundle_sha256"]):
        raise ValueError("standalone launcher candidate differs")
    argv = launch.get("argv")
    if not isinstance(argv, list) or any(not isinstance(value, str) for value in argv):
        raise ValueError("standalone launcher command missing")
    # Existing manual controller launches use this finite outer timeout and only
    # the four public transport assignments. Never accept an arbitrary shell wrapper.
    prefix = ["timeout", "--signal=TERM", "--kill-after=300s", "90m", "env", "TMPDIR=/tmp"]
    if argv[:len(prefix)] != prefix or len(argv) < len(prefix) + 3:
        raise ValueError("standalone launcher wrapper differs")
    assignments = argv[len(prefix):len(prefix) + 3]
    env = dict(value.split("=", 1) for value in assignments if "=" in value)
    if (set(env) != {"SUBYARD_E2E_CONTROLLER_WORKSPACE", "SUBYARD_E2E_CANDIDATE_BUNDLE", "SUBYARD_E2E_CANDIDATE_SHA256"}
        or env["SUBYARD_E2E_CANDIDATE_BUNDLE"] != str(root.parent / "candidate-bundle.tar.gz")
        or env["SUBYARD_E2E_CANDIDATE_SHA256"] != receipt["candidate_bundle_sha256"]):
        raise ValueError("standalone launcher transport differs")
    actual = argv[len(prefix) + 3:]
    if (actual[:len(command)] != command or len(actual) != len(command) + 2
        or actual[-2] != "--slot" or not re.fullmatch(r"[1-9][0-9]{0,2}", actual[-1])):
        raise ValueError("standalone actual check command differs")
    if not raw and command[:1] == ["bash"] and (not isinstance(launch["acceptance_script_sha256"], str)
        or not re.fullmatch(r"[0-9a-f]{64}", launch["acceptance_script_sha256"])
        or verify_source and launch["acceptance_script_sha256"] != digest(root / command[1])):
        raise ValueError("standalone launcher script changed")
    if "started_monotonic" in launch and (type(launch["started_monotonic"]) not in (int, float)
        or not math.isfinite(launch["started_monotonic"]) or launch["started_monotonic"] < 0):
        raise ValueError("invalid standalone launch time")
    launched = launch.get("launched_at_utc", launch.get("started_at"))
    if raw or launched is not None:
        try:
            if not isinstance(launched, str) or not launched or len(launched) > 64 or any(ord(c) < 32 for c in launched):
                raise ValueError()
            parsed = datetime.fromisoformat(launched)
            if parsed.utcoffset() is None:
                raise ValueError()
            if "started_at" in launch and datetime.fromisoformat(launch["started_at"]) != parsed:
                raise ValueError()
        except (TypeError, ValueError):
            raise ValueError("invalid standalone launch time") from None
        if raw:
            launched = parsed.astimezone(timezone.utc).isoformat()
    if raw and verify_source:
        validate_standalone_bundle(root, receipt, entries)
    # Initial import checks the actual bound controller above. Copied, hash-bound
    # rich evidence retains this exact interpretation during portable revalidation.
    historical_p0 = (not raw and name == "p0-release-smoke" and command == ["bash", "dev/e2e/p0-acceptance.sh"]
        and launch["acceptance_script_sha256"] == HISTORICAL_P0_SHA256)
    if historical_p0:
        validate_historical_p0_log(files["log"], receipt["candidate_bundle_sha256"], actual[-1])
    # The bound source and exact launcher select a checksum-verified bundle.
    # Direct controllers need not use run_guest's transport/guest timing wrapper.
    environments, stages, released = set(), set(), False
    with files["log"].open("rb") as stream:
        for line in stream:
            environment = re.fullmatch(rb"E2E lease: [^\r\n]{0,2000} type=(subyard-pair|android-test) vms=([12]) base=([0-9a-f]{64})\n", line)
            if environment:
                environments.add((environment[1].decode(), int(environment[2]), environment[3].decode()))
            stage = re.fullmatch(rb"E2E_PHASE phase=(allocation|packing|transport|guest|guest-cleanup|cleanup/release|fixture/[a-z0-9-]{1,60}) state=end duration_seconds=[0-9]{1,9} exit_code=([0-9]{1,3})(?: vm=([12]))?\n", line)
            if stage:
                if stage[2] != b"0" and not (historical_p0 and stage.groups() == (b"guest", b"23", b"1")):
                    raise ValueError("standalone controller contains a failed phase")
                stages.add((stage[1].decode(), int(stage[3]) if stage[3] else None))
            if re.fullmatch(rb"  \[fail\] phase=[a-zA-Z0-9_./-]{1,100}(?: [^\r\n]*)?\n", line):
                raise ValueError("standalone controller contains a failed phase")
            released |= bool(re.fullmatch(rb"E2E lease released: slot=[^\r\n ]+ run=[^\r\n ]+\n", line))
    if (not environments or not released or not historical_p0 and ("cleanup/release", None) not in stages
        or any(("guest-cleanup", vm) not in stages for _, count, _ in environments for vm in range(1, count + 1))):
        raise ValueError("standalone controller evidence incomplete")
    sanitized = prefix + ["SUBYARD_E2E_CONTROLLER_WORKSPACE=${CONTROLLER_WORKSPACE}",
        "SUBYARD_E2E_CANDIDATE_BUNDLE=${TESTED_BUNDLE}", "SUBYARD_E2E_CANDIDATE_SHA256=" + receipt["candidate_bundle_sha256"]] + command_identity(actual, root)
    return {"actual_command": sanitized, "duration_seconds": None, "launched_at_utc": launched,
            "execution_environment": None, "environments": [{"type": kind, "vm_count": count, "base_fingerprint": base}
                for kind, count, base in sorted(environments)]}


def validate_transferred_result(output, receipt, name, result, command, root):
    transfer = result["evidence_transfer"]
    if transfer.get("schema_version") == 2:
        files = result.get("evidence_files")
        if not isinstance(files, dict) or set(files) != set(transfer["artifacts"]):
            raise ValueError("original attempt transfer artifacts missing")
        for role, filename in files.items():
            if not isinstance(filename, str) or Path(filename).name != filename or digest(output / filename) != transfer["artifacts"][role]:
                raise ValueError("original attempt transfer artifact changed")
        record = read_transfer_json(output / files["attempt"])
        manifests = {transfer[key]: read_transfer_json(output / files[role]) for key, role in
                     (("tested_manifest_sha256", "manifest"), ("target_manifest_sha256", "target_manifest"))}
        validate_evidence_transfer(transfer, name, command_identity(command, root), transfer_candidate(receipt), result["log_sha256"], manifests)
        review = read_transfer_json(output / files["review"])
        expected_review = {"schema_version": 2, "check": name, "tested_manifest_sha256": transfer["tested_manifest_sha256"],
            "target_manifest_sha256": transfer["target_manifest_sha256"],
            "original_attempt": {"id": record["id"], "sha256": transfer["artifacts"]["attempt"]}, "review": transfer["review"], "standalone": None}
        if (record != transfer["original_attempt"] or review != expected_review or result.get("execution") != "inherited"
            or type(result.get("exit_code")) is not int or result["exit_code"] != 0
            or result.get("from_attempt_sha256") != transfer["artifacts"]["attempt"]
            or any(result.get(key, []) != transfer[key] for key in ("execution_environment", "environments", "duration_seconds"))):
            raise ValueError("original attempt transfer provenance changed")
        return
    validate_evidence_transfer(transfer, name, command_identity(command, root), transfer_candidate(receipt), result["log_sha256"])
    files = result.get("evidence_files")
    if (not isinstance(files, dict) or set(files) != set(transfer["artifacts"])
        or result.get("execution") != "inherited" or type(result.get("exit_code")) is not int or result["exit_code"] != 0
        or result.get("from_receipt_sha256") != transfer["artifacts"]["receipt"]
        or result.get("execution_environment") != transfer["execution_environment"]
        or result.get("environments", []) != transfer["environments"]):
        raise ValueError("reviewed transfer receipt provenance changed")
    for role, filename in files.items():
        if not isinstance(filename, str) or Path(filename).name != filename or digest(output / filename) != transfer["artifacts"][role]:
            raise ValueError("reviewed transfer original artifact changed")
    original = json.loads((output / files["receipt"]).read_text())
    review = read_transfer_json(output / files["review"])
    if (transfer_candidate(original) != transfer["tested"] or original["environment"] != transfer["preparation_environment"]
        or review != {"schema_version": 1, "check": name, "tested": transfer["tested"], "target": transfer["target"],
                      "source_receipt_sha256": transfer["artifacts"]["receipt"], "review": transfer["review"],
                      "standalone": review.get("standalone")}):
        raise ValueError("reviewed transfer original identity changed")
    if transfer["origin"] == "receipt":
        original_result = original["checks"][name]
        if (original_result.get("status") != "passed" or type(original_result.get("exit_code")) is not int
            or original_result["exit_code"] != 0 or original_result.get("inherited")
            or original_result.get("command") != transfer["command"]
            or original_result.get("log_sha256") != transfer["artifacts"]["log"]):
            raise ValueError("reviewed transfer original check changed")
        facts = {"actual_command": original_result["command"] + (["--slot", str(original_result["slot"])] if "slot" in original_result else []),
                 "duration_seconds": original_result.get("duration_seconds"), "launched_at_utc": None,
                 "execution_environment": original_result.get("execution_environment"), "environments": original_result.get("environments", [])}
    else:
        if original["checks"][name]["status"] != "pending":
            raise ValueError("standalone original check was already attempted")
        paths = {role: output / filename for role, filename in files.items()}
        launch = read_transfer_json(paths["launch"])
        facts = standalone_result(Path(launch["cwd"]), original, name, transfer["command"], paths, verify_source=False)
        if any(review["standalone"][role]["sha256"] != transfer["artifacts"][role] for role in ("native", "launch", "log")):
            raise ValueError("standalone original declaration changed")
    if any(transfer[key] != value for key, value in facts.items()):
        raise ValueError("reviewed transfer original execution provenance changed")



def candidate_manifest(root, receipt, entries=None):
    manifest = {"schema_version": 1, "candidate": transfer_candidate(receipt),
                "source_entries": source_entries(root) if entries is None else entries, "environment": receipt["environment"]}
    validate_manifest(manifest)
    return manifest


class EvidenceCandidate:
    def __init__(self, output):
        self.output = output
        self.entries = source_entries(output / "source")
        self.context = {}
        self.root, self.receipt = load_bound(output, entries=self.entries, context=self.context)
        self.manifest = candidate_manifest(self.root, self.receipt, self.entries)


def prepare_attempt_transfer(target, source, name, review_path, review):
    receipt, original = target.receipt, source.receipt["checks"][name]
    expected = command_identity(target.context["commands"][name], target.root)
    if name not in source.context["attempts"]:
        raise ValueError("invalid original attempt reference")
    record, manifest = source.context["attempts"][name]
    target_manifest, source_output = target.manifest, source.output
    tested_sha, target_sha = evidence_hash(manifest), evidence_hash(target_manifest)
    reference = {"id": record["id"], "sha256": evidence_hash(record)}
    if (set(review) != {"schema_version", "check", "tested_manifest_sha256", "target_manifest_sha256", "original_attempt", "review", "standalone"}
        or type(review["schema_version"]) is not int or review["check"] != name or review["standalone"] is not None
        or review["tested_manifest_sha256"] != tested_sha or review["target_manifest_sha256"] != target_sha
        or review["original_attempt"] != reference):
        raise ValueError("review declaration original attempt binding differs")
    files = {"attempt": source_output / "attempts" / (record["id"] + ".json"),
             "manifest": source_output / "manifests" / (tested_sha + ".json"),
             "target_manifest": evidence_bytes(target_manifest), "review": review_path, "log": source_output / original["log"]}
    hashes = {role: hashlib.sha256(path).hexdigest() if isinstance(path, bytes) else digest(path) for role, path in files.items()}
    transfer = {"schema_version": 2, "check": name, "tested_manifest_sha256": tested_sha, "target_manifest_sha256": target_sha,
        "original_attempt": record, "command": expected, "origin": "attempt", "artifacts": hashes,
        "review": review["review"], "preparation_environment": manifest["environment"], **attempt_facts(record)}
    validate_evidence_transfer(transfer, name, expected, transfer_candidate(receipt), hashes["log"], {tested_sha: manifest, target_sha: target_manifest})
    return transfer, files


def prepare_transfer(target_candidate, source_candidate, name, review_path, review):
    root, receipt = target_candidate.root, target_candidate.receipt
    source_root, source = source_candidate.root, source_candidate.receipt
    required, old_commands = target_candidate.context["commands"], source_candidate.context["commands"]
    if target_candidate.output == source_candidate.output or receipt.get("superseded") or source.get("superseded"):
        raise ValueError("reviewed transfer requires distinct active candidates")
    if name not in required or not required[name] or name not in old_commands or not old_commands[name]:
        raise ValueError("reviewed transfer requires an exact executable check")
    expected = command_identity(required[name], root)
    if expected != command_identity(old_commands[name], source_root) or receipt["checks"][name]["status"] != "pending":
        raise ValueError("reviewed transfer command differs or target is already attempted")
    source_output = source_candidate.output
    tested, target = transfer_candidate(source), transfer_candidate(receipt)
    if isinstance(review, dict) and review.get("schema_version") == 2:
        return prepare_attempt_transfer(target_candidate, source_candidate, name, review_path, review)
    if (not isinstance(review, dict) or set(review) != {"schema_version", "check", "tested", "target", "source_receipt_sha256", "review", "standalone"}
        or type(review["schema_version"]) is not int or review["schema_version"] != 1 or review["check"] != name or review["tested"] != tested
        or review["target"] != target or review["source_receipt_sha256"] != digest(source_output / "receipt.json")):
        raise ValueError("review declaration candidate binding differs")
    files = {"receipt": source_output / "receipt.json", "review": review_path}
    original = source["checks"][name]
    if review["standalone"] is None:
        if (original.get("status") != "passed" or type(original.get("exit_code")) is not int or original["exit_code"] != 0
            or original.get("command") != expected or original.get("inherited")):
            raise ValueError("reviewed transfer requires original passing command-bound evidence")
        files["log"] = source_output / original["log"]
        facts = {"actual_command": expected + (["--slot", str(original["slot"])] if "slot" in original else []),
                 "duration_seconds": original.get("duration_seconds"), "launched_at_utc": None,
                 "execution_environment": original.get("execution_environment"), "environments": original.get("environments", [])}
        origin = "receipt"
    else:
        if original["status"] != "pending" or not isinstance(review["standalone"], dict) or set(review["standalone"]) != {"native", "launch", "log"}:
            raise ValueError("standalone transfer requires uncollected original evidence")
        for role, descriptor in review["standalone"].items():
            if not isinstance(descriptor, dict) or set(descriptor) != {"path", "sha256"} or not isinstance(descriptor["path"], str):
                raise ValueError("invalid standalone artifact declaration")
            files[role] = (review_path.parent / descriptor["path"]).resolve()
            if digest(files[role]) != descriptor["sha256"]:
                raise ValueError("standalone original artifact changed")
        facts = standalone_result(source_root, source, name, old_commands[name], files, entries=source_candidate.entries)
        origin = "standalone"
    hashes = {role: hashlib.sha256(path).hexdigest() if isinstance(path, bytes) else digest(path) for role, path in files.items()}
    transfer = {"schema_version": 1, "check": name, "tested": tested, "target": target, "command": expected,
                "origin": origin, "artifacts": hashes, "review": review["review"], "preparation_environment": source["environment"], **facts}
    validate_evidence_transfer(transfer, name, expected, target, hashes["log"])
    return transfer, files


def transfer_evidence(output, source_output, name, review_path):
    target, source = EvidenceCandidate(output), EvidenceCandidate(source_output)
    transfer, files = prepare_transfer(target, source, name, review_path, read_transfer_json(review_path))
    return apply_transfers(target, [(name, target.context["commands"][name], transfer, files)])



BATCH_BYTE_LIMIT = 1 << 20


def bounded_batch(value):
    if len(evidence_bytes(value)) > BATCH_BYTE_LIMIT:
        raise ValueError("batch preparation exceeds 1 MiB byte limit")
    return value


def batch_request(path):
    if path.stat().st_size > BATCH_BYTE_LIMIT:
        raise ValueError("batch request exceeds 1 MiB byte limit")
    document = read_transfer_json(path)
    if (not isinstance(document, dict) or set(document) != {"schema_version", "target", "checks"}
        or type(document["schema_version"]) is not int or document["schema_version"] != 1
        or not isinstance(document["checks"], list) or not document["checks"] or len(document["checks"]) > 1000):
        raise ValueError("invalid batch transfer request")
    validate_candidate_identity(document["target"])
    names, items = set(), []
    for item in document["checks"]:
        if (not isinstance(item, dict) or set(item) != {"check", "source", "attempt", "review"}
            or not isinstance(item["check"], str) or item["check"] in names
            or not isinstance(item["source"], str) or not item["source"]):
            raise ValueError("invalid or duplicate batch check")
        names.add(item["check"])
        attempt = item["attempt"]
        if attempt is not None and (not isinstance(attempt, dict) or set(attempt) != {"id", "sha256"}
            or not isinstance(attempt["id"], str) or not re.fullmatch(r"[0-9a-f]{32}", attempt["id"]) or not is_sha(attempt["sha256"])):
            raise ValueError("invalid batch original attempt reference")
        review = item["review"]
        if review is not None and (not isinstance(review, dict) or set(review) != {"path", "sha256"}
            or not isinstance(review["path"], str) or not review["path"] or not is_sha(review["sha256"])):
            raise ValueError("invalid batch review reference")
        items.append({**item, "source": (path.parent / item["source"]).resolve(),
                      "review": None if review is None else {**review, "path": (path.parent / review["path"]).resolve()}})
    return document, items


def batch_guard(root, receipt, source_root, source, name, target_context, source_context):
    required, previous = target_context["commands"], source_context["commands"]
    if (receipt.get("superseded") or source.get("superseded") or receipt["version"] != source["version"]
        or receipt["profiles"] != source["profiles"]
        or {key: command_identity(value, root) if value else value for key, value in required.items()} !=
           {key: command_identity(value, source_root) if value else value for key, value in previous.items()}
        or target_context["external"] != source_context["external"]):
        raise ValueError("batch requires active candidates with matching complete inventories")
    if name not in required or not required[name] or name == "package" or receipt["checks"][name]["status"] != "pending":
        raise ValueError("batch requires an exact pending executable target check")
    original = source["checks"][name]
    if original.get("inherited") or "evidence_transfer" in original:
        raise ValueError("batch cannot chain inherited evidence")
    if receipt["environment"] != source["environment"]:
        raise ValueError("batch execution environment differs")
    return required[name], original


def batch_diff(source_manifest, target_manifest, source_root, source, root, receipt):
    left, right = source_manifest["source_entries"], target_manifest["source_entries"]
    paths = {name: {"tested": left.get(name), "target": right.get(name)}
             for name in sorted(left.keys() | right.keys()) if left.get(name) != right.get(name)}
    assets = {name: {"tested": source["release_files"].get(name), "target": receipt["release_files"].get(name)}
              for name in sorted(source["release_files"].keys() | receipt["release_files"].keys())
              if source["release_files"].get(name) != receipt["release_files"].get(name)}
    try:
        release_scope = "generated-at-only" if release_import_binding(root, receipt, source_root, source) else "identical"
    except ValueError:
        release_scope = "review-required"
    return {"source_changes": paths, "asset_changes": assets, "release_scope": release_scope,
            "runtime_equal": source["runtime_sha256"] == receipt["runtime_sha256"]}


def batch_review_draft(name, original, source_output, source_manifest, target_manifest, standalone=None):
    review = {"unchanged_scope": "", "reason": "", "authorization_reference": ""}
    if "original_attempt" in original:
        return {"schema_version": 2, "check": name, "tested_manifest_sha256": evidence_hash(source_manifest),
            "target_manifest_sha256": evidence_hash(target_manifest), "original_attempt": original["original_attempt"],
            "review": review, "standalone": None}
    return {"schema_version": 1, "check": name, "tested": source_manifest["candidate"], "target": target_manifest["candidate"],
            "source_receipt_sha256": digest(source_output / "receipt.json"), "review": review, "standalone": standalone}


def transfer_result(transfer, copied, batch_id):
    hashes = transfer["artifacts"]
    return {"status": "passed", "exit_code": 0, "command": transfer["command"], "log": copied["log"],
        "log_sha256": hashes["log"], "inherited": True, "execution": "inherited", "from_log_sha256": hashes["log"],
        ("from_attempt_sha256" if transfer["schema_version"] == 2 else "from_receipt_sha256"):
            hashes["attempt" if transfer["schema_version"] == 2 else "receipt"],
        "evidence_transfer": transfer, "evidence_files": copied,
        **({"transfer_batch_sha256": batch_id} if batch_id is not None else {}),
        **{key: transfer[key] for key in ("execution_environment", "environments", "duration_seconds")}}


def batch_committed(receipt, names, batch_id):
    return all(receipt["checks"].get(name, {}).get("transfer_batch_sha256") == batch_id
               and receipt["checks"][name].get("status") == "passed" for name in names)


@contextmanager
def batch_locks(output, sources, names, batch_id):
    try:
        with ExitStack() as stack:
            for path in sorted(sources | {output}, key=str):
                stack.enter_context(acceptance_lock(path, shared=path != output))
            yield
    except Exception as error:
        # A retry may encounter a subsequently rerun/busy source. Its earlier commit is still a fact.
        try:
            committed = batch_committed(read_transfer_json(output / "receipt.json"), names, batch_id)
        except (ValueError, OSError, KeyError):
            committed = False
        if committed and "committed" not in str(error):
            print(f"BATCH state=committed retry_validation=failed batch={batch_id}", flush=True)
            raise ValueError("batch already committed; retry validation could not complete") from error
        raise


def transfer_batch(output, request_path, plan=False):
    """Validate once under ordered locks, then publish one receipt replacement."""
    output, request_path = output.resolve(), request_path.resolve()
    request_sha = digest(request_path)
    document, items = batch_request(request_path)
    sources = {item["source"] for item in items}
    if output in sources:
        raise ValueError("batch source and target must be distinct")
    # Review bytes, rather than mutable carrier receipts, identify new-attempt retries.
    identities, reviews = [], {}
    for item in items:
        descriptor = item["review"]
        if descriptor is not None:
            if digest(descriptor["path"]) != descriptor["sha256"]:
                raise ValueError("batch review changed")
            reviews[item["check"]] = read_transfer_json(descriptor["path"])
        elif not plan:
            raise ValueError("batch apply requires bound authorized reviews")
        identities.append({"check": item["check"], "source": str(item["source"]), "attempt": item["attempt"],
                           "review_sha256": None if descriptor is None else descriptor["sha256"]})
    batch_id = evidence_hash({"target": document["target"], "checks": sorted(identities, key=lambda value: value["check"])})
    names = [item["check"] for item in items]
    with batch_locks(output, sources, names, batch_id):
        snapshots = {}
        for path in sorted(sources | {output}, key=str):
            snapshots[path] = EvidenceCandidate(path)
            bounded_batch(snapshots[path].manifest)
        target = snapshots[output]
        root, receipt, target_manifest, target_context = target.root, target.receipt, target.manifest, target.context
        if document["target"] != transfer_candidate(receipt):
            raise ValueError("batch target candidate binding changed")
        if not plan and batch_committed(receipt, names, batch_id):
            if digest(request_path) != request_sha:
                raise ValueError("batch request changed during validation")
            print(f"BATCH state=committed retry=true batch={batch_id} receipt={output / 'receipt.json'}", flush=True)
            return 0
        if any(receipt["checks"].get(name, {}).get("transfer_batch_sha256") for name in names):
            raise ValueError("batch conflicts with committed transfer references")
        table = {evidence_hash(target_manifest): target_manifest}
        pairs, decisions, prepared = {}, [], []
        for item in items:
            name, source_output = item["check"], item["source"]
            source_candidate = snapshots[source_output]
            source_root, source = source_candidate.root, source_candidate.receipt
            entries, manifest, source_context = source_candidate.entries, source_candidate.manifest, source_candidate.context
            command, original = batch_guard(root, receipt, source_root, source, name, target_context, source_context)
            if item["attempt"] != original.get("original_attempt"):
                raise ValueError("batch current original attempt reference changed")
            if "original_attempt" in original:
                if is_live(name) and (original.get("cleanup_status") != "recorded" or not original.get("cleanup")
                    or original.get("environment_evidence") != "recorded"):
                    raise ValueError("batch physical attempt lacks recorded environment or cleanup")
            source_sha = evidence_hash(manifest)
            table[source_sha] = manifest
            if source_output not in pairs:
                pairs[source_output] = batch_diff(manifest, target_manifest, source_root, source, root, receipt)
            review = reviews.get(name)
            standalone = review.get("standalone") if isinstance(review, dict) else None
            if plan:
                if standalone is not None:
                    if not isinstance(standalone, dict) or set(standalone) != {"native", "launch", "log"} or original["status"] != "pending":
                        raise ValueError("invalid standalone batch planning evidence")
                    files = {}
                    for role, descriptor in standalone.items():
                        if not isinstance(descriptor, dict) or set(descriptor) != {"path", "sha256"} or not isinstance(descriptor["path"], str):
                            raise ValueError("invalid standalone artifact declaration")
                        files[role] = (item["review"]["path"].parent / descriptor["path"]).resolve()
                        if digest(files[role]) != descriptor["sha256"]:
                            raise ValueError("standalone original artifact changed")
                    standalone_result(source_root, source, name, command, files, entries=entries)
                elif (original["status"] != "passed" or type(original.get("exit_code")) is not int
                      or original["exit_code"] != 0 or original.get("command") != command_identity(command, root)
                      or original.get("execution_environment") != source["environment"]
                      or is_live(name) and not original.get("environments")):
                    raise ValueError("batch plan requires an original direct PASS")
                draft = batch_review_draft(name, original, source_output, manifest, target_manifest, standalone)
                decisions.append({"check": name, "source": str(source_output), "tested_manifest_sha256": source_sha,
                                  "attempt": item["attempt"], "review_draft": draft})
            else:
                transfer, files = prepare_transfer(target, source_candidate, name, item["review"]["path"], review)
                if transfer["artifacts"]["review"] != item["review"]["sha256"]:
                    raise ValueError("batch review changed during validation")
                prepared.append((name, command, transfer, files))
        report = bounded_batch({"schema_version": 1, "target_manifest_sha256": evidence_hash(target_manifest),
            "manifests": table, "pairs": [{"source": str(path), "tested_manifest_sha256": evidence_hash(snapshots[path].manifest),
                "target_manifest_sha256": evidence_hash(target_manifest), **value} for path, value in pairs.items()], "checks": decisions})
        if digest(request_path) != request_sha or any(item["review"] is not None and
            digest(item["review"]["path"]) != item["review"]["sha256"] for item in items):
            raise ValueError("batch request or review changed during validation")
        if plan:
            print(json.dumps(report, sort_keys=True), flush=True)
            return report
        def verify_inputs():
            if digest(request_path) != request_sha or any(digest(item["review"]["path"]) != item["review"]["sha256"] for item in items):
                raise ValueError("batch request or review changed during copy")
        return apply_transfers(target, prepared, batch_id, verify_inputs)


def apply_transfers(target, prepared, batch_id=None, verify_inputs=None):
    """Stage, rehash and validate all evidence, then publish exactly one receipt."""
    output, root, receipt = target.output, target.root, target.receipt
    names = [name for name, _, _, _ in prepared]
    staging = Path(tempfile.mkdtemp(prefix=".transfer-", dir=output))
    staging.chmod(0o700)
    promoted, committed = [], False
    incoming, copied_cache = copy.deepcopy(receipt), {}
    try:
        for name, command, transfer, files in prepared:
            copied = {}
            for role, path in files.items():
                sha = transfer["artifacts"][role]
                key = (role, sha)
                if key not in copied_cache:
                    destination = staging / ("transfer-" + uuid.uuid4().hex + "-" + role)
                    with private_file(destination, "wb") as stream:
                        if isinstance(path, bytes):
                            stream.write(path)
                        else:
                            with path.open("rb") as original_file:
                                shutil.copyfileobj(original_file, stream)
                        stream.flush()
                        os.fsync(stream.fileno())
                    if digest(destination) != sha:
                        raise ValueError("transfer evidence changed during copy")
                    copied_cache[key] = destination.name
                copied[role] = copied_cache[key]
            result = transfer_result(transfer, copied, batch_id)
            validate_transferred_result(staging, receipt, name, result, command, root)
            incoming["checks"][name] = result
        if verify_inputs is not None:
            verify_inputs()
        for path in staging.iterdir():
            destination = output / path.name
            os.link(path, destination)  # Exclusive publication; never overwrite another invocation's artifact.
            promoted.append(destination)
        sync_directory(output)
        try:
            save(output, incoming)
            committed = True
        except Exception:
            # replace may have succeeded before directory fsync raised. Preserve all referenced bytes.
            try:
                saved = read_transfer_json(output / "receipt.json")
                committed = all(saved["checks"].get(name) == incoming["checks"][name] for name in names)
            except (OSError, ValueError, KeyError):
                committed = None  # Unknown replacement outcome must preserve potentially referenced artifacts.
            raise
        if batch_id is not None:
            event(output, "evidence-transfer-batch", batch_sha256=batch_id, checks=sorted(names), state="committed")
            print(f"BATCH state=committed retry=false batch={batch_id} receipt={output / 'receipt.json'}", flush=True)
        else:
            name, _, transfer, _ = prepared[0]
            event(output, "evidence-transfer", check=name, execution="inherited", evidence_transfer=transfer)
            print(f"TRANSFERRED {name} execution=inherited receipt={output / 'receipt.json'}", flush=True)
        return 0
    except Exception as error:
        if committed is not False:
            state = "committed" if committed else "unknown"
            kind = "BATCH" if batch_id is not None else "TRANSFER"
            print(f"{kind} state={state} capture=failed receipt={output / 'receipt.json'}", flush=True)
            raise ValueError(f"transfer commit state {state}; artifacts preserved after capture or durability failure") from error
        for path in promoted:
            path.unlink()
        sync_directory(output)
        raise
    finally:
        shutil.rmtree(staging)  # Only this invocation's newly-created directory.


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
    prep.add_argument("--prepared", type=Path, help="import bound CI sources/assets without rebuilding")
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
    transferred = sub.add_parser("transfer", help="accept one reviewed, target-bound original PASS as inherited coverage")
    transferred.add_argument("--output", type=Path, required=True)
    transferred.add_argument("--from", dest="source_output", type=Path)
    transferred.add_argument("--check", help="one exact declared executable check")
    transferred.add_argument("--review", type=Path, help="target-bound public review declaration JSON")
    transferred.add_argument("--batch", type=Path, help="bound batch request JSON; exclusive with single-check flags")
    transferred.add_argument("--plan", action="store_true", help="print shared manifests, diffs and unsigned review drafts for --batch")
    superseded = sub.add_parser("supersede", help="retain original evidence and explicitly retire this candidate")
    superseded.add_argument("--output", type=Path, required=True)
    deferred = sub.add_parser("defer-external", help="record authorized external deferrals; execution stays not-run")
    deferred.add_argument("--output", type=Path, required=True)
    deferred.add_argument("--obligation", action="append", required=True, help="exact declared external obligation ID")
    deferred.add_argument("--reason", required=True, help="public reason for deferring these external checks")
    deferred.add_argument("--authorization-reference", required=True, help="public reference to explicit operator authorization")
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
        prepare(output, args.version, args.prepared.resolve() if args.prepared else None)
        return 0
    if args.action == "import":
        with acceptance_lock(output), acceptance_lock(args.source_output.resolve(), shared=True):
            return import_evidence(output, args.source_output.resolve(), args.check, args.allow_other_profile_tests)
    if args.action == "transfer":
        if args.batch:
            if args.source_output or args.check or args.review:
                parser.error("--batch is mutually exclusive with --from, --check and --review")
            result = transfer_batch(output, args.batch, args.plan)
            return 0 if args.plan else result
        if args.plan or not all((args.source_output, args.check, args.review)):
            parser.error("transfer requires --batch or all of --from, --check and --review; --plan requires --batch")
        with acceptance_lock(output), acceptance_lock(args.source_output.resolve(), shared=True):
            return transfer_evidence(output, args.source_output.resolve(), args.check, args.review.resolve())
    if args.action == "supersede":
        with acceptance_lock(output):
            return supersede(output)
    if args.action == "defer-external":
        with acceptance_lock(output):
            return defer_external(output, args.obligation, args.reason, args.authorization_reference)
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
