#!/usr/bin/env python3
"""Prepare exact CI assets, collect local acceptance, publish through serialized Actions."""
import argparse
import base64
from datetime import datetime
import hashlib
import importlib.util
import json
import math
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import uuid
import zipfile
import zlib

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location("release_acceptance", ROOT / "dev/release-acceptance.py")
a = importlib.util.module_from_spec(spec)
spec.loader.exec_module(a)
REPOSITORY = os.environ.get("GITHUB_REPOSITORY", "Subyard/Subyard")
DISPATCH_INPUT_LIMIT = 65535
ATTESTATION_BYTE_LIMIT = 1 << 20
ATTESTATION_TRANSPORT = "subyard-attestation-zlib-v1"


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"))


def identity_hash(identity):
    return hashlib.sha256(canonical(identity).encode()).hexdigest()


def encode_attestation(document):
    original = canonical(document).encode()
    if len(original) > ATTESTATION_BYTE_LIMIT:
        raise ValueError("acceptance attestation exceeds uncompressed byte limit")
    envelope = {"transport": ATTESTATION_TRANSPORT, "uncompressed_bytes": len(original),
                "sha256": identity_hash(document), "payload": base64.b64encode(zlib.compress(original)).decode()}
    return base64.b64encode(canonical(envelope).encode()).decode()


def decode_attestation(document):
    if not isinstance(document, dict):
        raise ValueError("acceptance attestation must be a JSON object")
    if "transport" not in document:
        if len(canonical(document).encode()) > ATTESTATION_BYTE_LIMIT:
            raise ValueError("acceptance attestation exceeds uncompressed byte limit")
        return document  # Existing plain JSON workflow inputs remain valid.
    if (set(document) != {"transport", "uncompressed_bytes", "sha256", "payload"}
        or document["transport"] != ATTESTATION_TRANSPORT
        or type(document["uncompressed_bytes"]) is not int
        or not 0 < document["uncompressed_bytes"] <= ATTESTATION_BYTE_LIMIT
        or not isinstance(document["sha256"], str) or not re.fullmatch(r"[0-9a-f]{64}", document["sha256"])
        or not isinstance(document["payload"], str)
        or len(base64.b64encode(canonical(document).encode())) > DISPATCH_INPUT_LIMIT):
        raise ValueError("invalid acceptance attestation envelope")
    try:
        compressed = base64.b64decode(document["payload"], validate=True)
        decoder = zlib.decompressobj()
        original = decoder.decompress(compressed, document["uncompressed_bytes"] + 1)
    except (ValueError, zlib.error) as error:
        raise ValueError("invalid compressed acceptance attestation") from error
    if (len(original) != document["uncompressed_bytes"] or not decoder.eof
        or decoder.unconsumed_tail or decoder.unused_data
        or hashlib.sha256(original).hexdigest() != document["sha256"]):
        raise ValueError("acceptance attestation envelope size, stream or hash mismatch")
    decoded = json.loads(original)
    if not isinstance(decoded, dict) or canonical(decoded).encode() != original:
        raise ValueError("acceptance attestation envelope must contain canonical JSON object")
    return decoded


def read_attestation(path):
    with path.open("rb") as stream:
        raw = stream.read(ATTESTATION_BYTE_LIMIT + 1)
    if len(raw) > ATTESTATION_BYTE_LIMIT:
        raise ValueError("acceptance attestation file exceeds byte limit")
    return json.loads(raw)


def write_json(path, value):
    with a.private_file(path.with_suffix(path.suffix + ".next")) as stream:
        stream.write(canonical(value) + "\n")
        stream.flush()
        os.fsync(stream.fileno())
    path.with_suffix(path.suffix + ".next").replace(path)
    a.sync_directory(path.parent)


class GitHub:
    def __init__(self, repository=REPOSITORY):
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
            raise ValueError("invalid repository")
        self.repository = repository
        self.prefix = "repos/" + repository + "/"

    def api(self, path, method="GET", payload=None, missing=False):
        command = ["gh", "api", self.prefix + path, "--method", method]
        if payload is not None:
            command += ["--input", "-"]
        result = subprocess.run(command, input=canonical(payload) if payload is not None else None,
                                text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        if result.returncode:
            if missing and "(HTTP 404)" in result.stderr:
                return None
            status = re.search(r"HTTP ([0-9]{3})", result.stderr)
            diagnostic = " HTTP " + status[1] if status else ""
            raise ValueError("GitHub API failed: " + method + " " + path + diagnostic +
                             "; inspect workflow permissions (historical workflow changes may require Workflows:write), then retry; never change the pinned SHA")
        return json.loads(result.stdout) if result.stdout.strip() else None

    def pages(self, path, key=None):
        rows = []
        for page in range(1, 1001):
            value = self.api(path + ("&" if "?" in path else "?") + f"per_page=100&page={page}")
            items = value[key] if key else value
            rows.extend(items)
            if len(items) < 100:
                return rows
        raise ValueError("GitHub inventory exceeded pagination limit")

    def download(self, path, destination):
        accept = "application/json" if re.fullmatch(r"actions/artifacts/[0-9]+/zip", path) else "application/octet-stream"
        with destination.open("wb") as stream:
            result = subprocess.run(["gh", "api", self.prefix + path, "-H", "Accept: " + accept],
                                    stdout=stream, stderr=subprocess.PIPE)
        if result.returncode:
            status = re.search(rb"HTTP ([0-9]{3})", result.stderr)
            diagnostic = " HTTP " + status[1].decode() if status else ""
            raise ValueError("GitHub download failed" + diagnostic + "; retry with retained candidate")

    def upload(self, release_id, path):
        # gh api supports streaming the exact file body to the uploads endpoint.
        result = subprocess.run(["gh", "api", "https://uploads.github.com/" + self.prefix +
                                 f"releases/{release_id}/assets?name={path.name}", "--method", "POST",
                                 "-H", "Content-Type: application/octet-stream", "--input", str(path)],
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        if result.returncode:
            raise ValueError("asset upload unfinished; retry reconciles remote assets")


def candidate_parts(candidate):
    match = re.fullmatch(r"r([1-9][0-9]*)-a([1-9][0-9]*)", candidate)
    if not match:
        raise ValueError("candidate must be rRUN_ID-aATTEMPT")
    return tuple(map(int, match.groups()))


def checked_identity(prepared):
    for name in ("identity.json", "source.tar.gz"):
        if not (prepared / name).is_file() or (prepared / name).is_symlink():
            raise ValueError("preparation metadata must be regular files")
    identity = json.loads((prepared / "identity.json").read_text())
    expected = {"schema_version", "candidate_id", "repository", "version", "commit", "source_fingerprint",
                "source_archive_sha256", "release_files"}
    if (set(identity) != expected or type(identity["schema_version"]) is not int or identity["schema_version"] != 1
        or not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", identity["version"])
        or not re.fullmatch(r"[0-9a-f]{40}", identity["commit"])
        or any(not re.fullmatch(r"[0-9a-f]{64}", identity[key]) for key in
               ("source_fingerprint", "source_archive_sha256"))):
        raise ValueError("invalid preparation identity")
    candidate_parts(identity["candidate_id"])
    assets = prepared / "assets"
    names = subprocess.check_output(["bash", str(ROOT / "dev/release-assets.sh"), "--release-dir", str(assets),
                                     "--version", identity["version"]], text=True).splitlines()
    expected_names = {Path(name).name for name in names}
    if (set(identity["release_files"]) != expected_names
        or {path.name for path in assets.iterdir()} != expected_names
        or any(path.is_symlink() or not path.is_file() for path in assets.iterdir())
        or {path.name: a.digest(path) for path in assets.iterdir()} != identity["release_files"]
        or a.digest(prepared / "source.tar.gz") != identity["source_archive_sha256"]):
        raise ValueError("prepared inventory or hashes mismatch")
    for name in expected_names:
        if name.endswith(".provenance.json"):
            value = json.loads((assets / name).read_text())
            artifact = name.removesuffix(".provenance.json")
            if (value.get("sourceRevision") != identity["commit"] or value.get("version") != identity["version"]
                or value.get("sha256") != identity["release_files"][artifact] or value.get("artifact") != artifact):
                raise ValueError("prepared provenance mismatch")
        if name.endswith(".sha256"):
            artifact = name.removesuffix(".sha256")
            if (assets / name).read_text().strip() != identity["release_files"][artifact] + "  " + artifact:
                raise ValueError("prepared checksum mismatch")
    return identity


def verify_run(github, candidate):
    run_id, attempt = candidate_parts(candidate)
    run = github.api(f"actions/runs/{run_id}/attempts/{attempt}")
    workflow = github.api("actions/workflows/release.yml")
    if (run["workflow_id"] != workflow["id"] or run["event"] != "workflow_dispatch"
        or run["status"] != "completed" or run["conclusion"] != "success"
        or run["run_attempt"] != attempt or run["repository"]["full_name"] != github.repository):
        raise ValueError("preparation run/attempt did not pass")
    jobs = github.pages(f"actions/runs/{run_id}/attempts/{attempt}/jobs", "jobs")
    required = {"validate", "native-engine-arm64", "paseo-headless-amd64", "paseo-headless-arm64", "prepare"}
    if (len(jobs) != len(required) or {job["name"] for job in jobs} != required
        or any(job["status"] != "completed" or job["conclusion"] != "success" for job in jobs)):
        raise ValueError("preparation job inventory did not completely pass in this attempt")
    return run


def fetch(github, candidate, destination):
    verify_run(github, candidate)
    run_id, attempt = candidate_parts(candidate)
    artifacts = [item for item in github.pages(f"actions/runs/{run_id}/artifacts", "artifacts")
                 if item["name"] == f"release-candidate-{attempt}" and not item["expired"]]
    if len(artifacts) != 1:
        raise ValueError("exact preparation artifact unavailable")
    destination.mkdir(parents=True, exist_ok=False)
    destination.chmod(0o700)
    archive = destination / "download.zip"
    github.download(f"actions/artifacts/{artifacts[0]['id']}/zip", archive)
    with zipfile.ZipFile(archive) as zipped:
        for item in zipped.infolist():
            if item.filename.startswith("/") or ".." in Path(item.filename).parts:
                raise ValueError("unsafe candidate artifact")
        zipped.extractall(destination)
    archive.unlink()
    identity = checked_identity(destination)
    if identity["candidate_id"] != candidate or identity["repository"] != github.repository:
        raise ValueError("preparation artifact belongs to another candidate")
    return identity


def export_candidate(output, version, commit, candidate, repository):
    if subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip() != commit:
        raise ValueError("CI checkout is not the exact requested commit")
    if subprocess.check_output(["git", "status", "--porcelain", "--untracked-files=all"], cwd=ROOT):
        raise ValueError("CI public checkout changed after the pinned checkout")
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", version) or not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("stable semver and full commit SHA required")
    candidate_parts(candidate)
    output.mkdir(parents=True, exist_ok=False)
    assets = output / "assets"
    assets.mkdir()
    names = subprocess.check_output(["bash", "dev/release-assets.sh", "--release-dir", ".build/release",
                                     "--version", version], cwd=ROOT, text=True).splitlines()
    for name in names:
        shutil.copyfile(ROOT / name, assets / Path(name).name)
    fingerprint = a.source_fingerprint(ROOT)
    a.bundle(ROOT, output / "source.tar.gz")
    if a.source_fingerprint(ROOT) != fingerprint:
        raise ValueError("source changed during preparation")
    identity = {"schema_version": 1, "candidate_id": candidate, "repository": repository,
                "version": version, "commit": commit, "source_fingerprint": fingerprint,
                "source_archive_sha256": a.digest(output / "source.tar.gz"),
                "release_files": {path.name: a.digest(path) for path in sorted(assets.iterdir())}}
    write_json(output / "identity.json", identity)
    checked_identity(output)


def validate_operator_acceptance(document, name, command, identity):
    def sha(value):
        return isinstance(value, str) and re.fullmatch(r"[0-9a-f]{64}", value)
    def text(value, limit):
        return isinstance(value, str) and bool(value.strip()) and len(value) <= limit and not any(ord(c) < 32 for c in value)
    keys = {"schema_version", "check", "target", "tested", "command", "actual_command", "decision", "artifacts",
            "source_status", "target_status", "native_status", "exit_code", "duration_seconds", "launched_at_utc",
            "ended_at_utc", "environments", "cleanup"}
    if (not isinstance(document, dict) or set(document) != keys or type(document["schema_version"]) is not int
        or document["schema_version"] != 1 or not name.startswith("profile:") or document["check"] != name
        or canonical(document["target"]) != canonical(identity) or document["command"] != command
        or document["source_status"] not in ("pending", "failed") or document["target_status"] not in ("pending", "failed")
        or document["native_status"] != "failed" or type(document["exit_code"]) is not int
        or not 1 <= document["exit_code"] <= 255 or document["exit_code"] in (130, 143)):
        raise ValueError("invalid exact-candidate operator acceptance")
    tested = document["tested"]
    if (not isinstance(tested, dict) or set(tested) != {"version", "source_fingerprint", "runtime_file", "runtime_sha256",
        "candidate_bundle_sha256", "release_files", "candidate_sha256"} or tested["version"] != identity["version"]
        or not text(tested["runtime_file"], 128) or Path(tested["runtime_file"]).name != tested["runtime_file"]
        or any(not sha(tested[key]) for key in ("source_fingerprint", "runtime_sha256", "candidate_bundle_sha256", "candidate_sha256"))
        or not isinstance(tested["release_files"], dict) or not tested["release_files"]
        or any(not text(filename, 128) or Path(filename).name != filename or not sha(value)
               for filename, value in tested["release_files"].items())
        or tested["release_files"].get(tested["runtime_file"]) != tested["runtime_sha256"]):
        raise ValueError("operator acceptance tested candidate identity missing")
    decision = document["decision"]
    if (not isinstance(decision, dict) or set(decision) != {"reason", "authorization_reference", "repair_reference"}
        or any(not text(decision[key], limit) for key, limit in
               (("reason", 1024), ("authorization_reference", 256), ("repair_reference", 256)))):
        raise ValueError("operator acceptance requires public reason, authorization and repair reference")
    if (not isinstance(document["artifacts"], dict)
        or set(document["artifacts"]) != {"receipt", "declaration", "native", "launch", "timing", "log"}
        or any(not sha(value) for value in document["artifacts"].values())):
        raise ValueError("operator acceptance original evidence hashes missing")
    actual = document["actual_command"]
    prefix = ["timeout", "--signal=TERM", "--kill-after=300s", "90m", "env", "TMPDIR=/tmp",
              "SUBYARD_E2E_CONTROLLER_WORKSPACE=${CONTROLLER_WORKSPACE}",
              "SUBYARD_E2E_CANDIDATE_BUNDLE=${TESTED_BUNDLE}",
              "SUBYARD_E2E_CANDIDATE_SHA256=" + tested["candidate_bundle_sha256"]]
    if (not isinstance(actual, list) or actual[:-2] != prefix + command or actual[-2:-1] != ["--slot"]
        or not isinstance(actual[-1], str) or not re.fullmatch(r"[1-9][0-9]{0,2}", actual[-1])):
        raise ValueError("operator acceptance original command differs")
    duration = document["duration_seconds"]
    try:
        started, ended = (datetime.fromisoformat(document[key]) for key in ("launched_at_utc", "ended_at_utc"))
        if (started.utcoffset() is None or ended.utcoffset() is None or ended < started
            or type(duration) not in (int, float) or not math.isfinite(duration) or duration < 0):
            raise ValueError()
    except (TypeError, ValueError):
        raise ValueError("operator acceptance complete timing missing") from None
    environments = document["environments"]
    if not isinstance(environments, list) or not environments:
        raise ValueError("operator acceptance physical environment missing")
    counts = set()
    for item in environments:
        if (not isinstance(item, dict) or set(item) != {"type", "vm_count", "base_fingerprint"}
            or not isinstance(item["type"], str) or not re.fullmatch(r"[a-z][a-z0-9-]{0,79}", item["type"])
            or type(item["vm_count"]) is not int or not 1 <= item["vm_count"] <= 100 or not sha(item["base_fingerprint"])):
            raise ValueError("invalid operator acceptance physical environment")
        counts.add(item["vm_count"])
    cleanup = {"guest_cleanup": [{"vm": vm, "exit_code": 0} for vm in range(1, max(counts) + 1)],
               "release_exit_code": 0, "lease_released": True}
    if canonical(document["cleanup"]) != canonical(cleanup):
        raise ValueError("operator acceptance cleanup or release unconfirmed")


def operator_acceptances(paths, identity, root, receipt):
    accepted = {}
    required = a.commands(root, identity["version"], receipt["profiles"])
    for path in paths:
        declaration = a.read_transfer_json(path)
        if (not isinstance(declaration, dict) or set(declaration) != {"schema_version", "check", "target", "tested",
            "command", "decision", "source", "artifacts"} or type(declaration["schema_version"]) is not int
            or declaration["schema_version"] != 1):
            raise ValueError("invalid operator acceptance declaration")
        name = declaration["check"]
        if not isinstance(name, str) or not name.startswith("profile:") or name not in required or required[name] is None or name in accepted:
            raise ValueError("operator acceptance requires one exact declared runnable profile check")
        command = a.command_identity(required[name], root)
        source = declaration["source"]
        if not isinstance(source, dict) or set(source) != {"output", "receipt_sha256"} or not isinstance(source["output"], str):
            raise ValueError("operator acceptance source declaration missing")
        source_output = (path.parent / source["output"]).resolve()
        source_root, original = a.load_bound(source_output)
        old_commands = a.commands(source_root, original["version"], original["profiles"])
        if (name not in old_commands or old_commands[name] is None
            or a.command_identity(old_commands[name], source_root) != command
            or declaration["command"] != command or canonical(declaration["target"]) != canonical(identity)
            or declaration["tested"] != a.transfer_candidate(original)
            or source["receipt_sha256"] != a.digest(source_output / "receipt.json")):
            raise ValueError("operator acceptance original or target candidate binding changed")
        descriptors = declaration["artifacts"]
        if not isinstance(descriptors, dict) or set(descriptors) != {"native", "launch", "timing", "log"}:
            raise ValueError("operator acceptance original artifact declarations missing")
        files, hashes = {}, {"receipt": source["receipt_sha256"], "declaration": a.digest(path)}
        for role, descriptor in descriptors.items():
            if not isinstance(descriptor, dict) or set(descriptor) != {"path", "sha256"} or not isinstance(descriptor["path"], str):
                raise ValueError("invalid operator acceptance artifact declaration")
            files[role] = (path.parent / descriptor["path"]).resolve()
            hashes[role] = a.digest(files[role])
            if hashes[role] != descriptor["sha256"]:
                raise ValueError("operator acceptance original artifact changed")
        native, launch, timing = (a.read_transfer_json(files[role]) for role in ("native", "launch", "timing"))
        if (type(native.get("exit_code")) is not int or not 1 <= native["exit_code"] <= 255
            or native["exit_code"] in (130, 143) or native.get("session_id") is not None
            or not isinstance(native.get("chunk_id"), str) or not native["chunk_id"] or not isinstance(native.get("output"), str)
            or type(native.get("wall_time_seconds")) not in (int, float) or not math.isfinite(native["wall_time_seconds"])
            or native["wall_time_seconds"] < 0):
            raise ValueError("operator acceptance requires complete original native failure")
        prefix = ["timeout", "--signal=TERM", "--kill-after=300s", "90m", "env", "TMPDIR=/tmp"]
        argv = launch.get("argv")
        if (launch.get("cwd") != str(source_root) or not isinstance(argv, list) or not all(isinstance(value, str) for value in argv)
            or argv[:6] != prefix or len(argv) != len(prefix) + 3 + len(command) + 2):
            raise ValueError("operator acceptance original launcher differs")
        assignments = dict(value.split("=", 1) for value in argv[6:9] if "=" in value)
        if (set(assignments) != {"SUBYARD_E2E_CONTROLLER_WORKSPACE", "SUBYARD_E2E_CANDIDATE_BUNDLE", "SUBYARD_E2E_CANDIDATE_SHA256"}
            or assignments["SUBYARD_E2E_CANDIDATE_BUNDLE"] != str(source_output / "candidate-bundle.tar.gz")
            or assignments["SUBYARD_E2E_CANDIDATE_SHA256"] != original["candidate_bundle_sha256"]
            or a.command_identity(argv[9:-2], source_root) != command or argv[-2] != "--slot"
            or not re.fullmatch(r"[1-9][0-9]{0,2}", argv[-1])):
            raise ValueError("operator acceptance original transport or command differs")
        duration = timing.get("whole_duration_seconds")
        clocks = [timing.get(key) for key in ("started_monotonic", "ended_monotonic")]
        if (any(timing.get(key) != launch.get(key) for key in ("cwd", "argv", "started_at", "started_monotonic"))
            or type(timing.get("exit_code")) is not int or timing["exit_code"] != native["exit_code"]
            or type(duration) not in (int, float) or not math.isfinite(duration) or duration < 0
            or any(type(value) not in (int, float) or not math.isfinite(value) for value in clocks)
            or not math.isclose(clocks[1] - clocks[0], duration, rel_tol=1e-9, abs_tol=1e-6)):
            raise ValueError("operator acceptance original complete timing differs")
        environments, cleanup, release_codes, released = set(), {}, [], False
        with files["log"].open("rb") as stream:
            for line in stream:
                environment = re.fullmatch(rb"E2E lease: [^\r\n]{0,2000} type=(subyard-pair|android-test) vms=([12]) base=([0-9a-f]{64})\n", line)
                if environment:
                    environments.add((environment[1].decode(), int(environment[2]), environment[3].decode()))
                stage = re.fullmatch(rb"E2E_PHASE phase=(guest-cleanup|cleanup/release) state=end duration_seconds=[0-9]{1,9} exit_code=([0-9]{1,3})(?: vm=([12]))?\n", line)
                if stage:
                    if stage[1] == b"guest-cleanup":
                        vm = int(stage[3]) if stage[3] else None
                        if vm in cleanup:
                            raise ValueError("operator acceptance cleanup evidence is ambiguous")
                        cleanup[vm] = int(stage[2])
                    else:
                        release_codes.append(int(stage[2]))
                released |= bool(re.fullmatch(rb"E2E lease released: slot=[^\r\n ]+ run=[^\r\n ]+\n", line))
        if not environments or release_codes != [0] or cleanup != {vm: 0 for vm in range(1, max(count for _, count, _ in environments) + 1)} or not released:
            raise ValueError("operator acceptance original cleanup or release unconfirmed")
        for original_result in (original["checks"][name], receipt["checks"][name]):
            if (original_result.get("status") not in ("pending", "failed") or original_result.get("inherited")
                or "evidence_transfer" in original_result or original_result.get("status") == "failed" and
                (original_result.get("exit_code") != native["exit_code"] or original_result.get("log_sha256") != hashes["log"]
                 or original_result.get("command") != command)):
                raise ValueError("operator acceptance collector status or original failure differs")
        public = {"schema_version": 1, "check": name, "target": identity, "tested": declaration["tested"], "command": command,
                  "actual_command": prefix + ["SUBYARD_E2E_CONTROLLER_WORKSPACE=${CONTROLLER_WORKSPACE}",
                      "SUBYARD_E2E_CANDIDATE_BUNDLE=${TESTED_BUNDLE}", "SUBYARD_E2E_CANDIDATE_SHA256=" + original["candidate_bundle_sha256"]] + command + argv[-2:],
                  "decision": declaration["decision"], "artifacts": hashes, "source_status": original["checks"][name]["status"],
                  "target_status": receipt["checks"][name]["status"], "native_status": "failed", "exit_code": native["exit_code"],
                  "duration_seconds": duration, "launched_at_utc": launch.get("started_at"), "ended_at_utc": timing.get("ended_at"),
                  "environments": [{"type": kind, "vm_count": count, "base_fingerprint": base} for kind, count, base in sorted(environments)],
                  "cleanup": {"guest_cleanup": [{"vm": vm, "exit_code": cleanup[vm]} for vm in sorted(cleanup)],
                              "release_exit_code": 0, "lease_released": True}}
        validate_operator_acceptance(public, name, command, identity)
        accepted[name] = public
    return accepted


def validate_profile_deferral(document, name, command, identity, receipt_sha256):
    keys = {"schema_version", "check", "target", "command", "decision", "receipt_sha256", "original_status"}
    if (not isinstance(document, dict) or set(document) != keys or type(document["schema_version"]) is not int
        or document["schema_version"] != 1 or not name.startswith("profile:") or not a.is_live(name) or command is None
        or document["check"] != name or canonical(document["target"]) != canonical(identity)
        or document["command"] != command or document["receipt_sha256"] != receipt_sha256
        or document["original_status"] != "pending"):
        raise ValueError("invalid exact-candidate profile deferral")
    decision = document["decision"]
    if (not isinstance(decision, dict) or set(decision) != {"reason", "authorization_reference"}
        or any(not isinstance(decision[key], str) or not decision[key].strip() or len(decision[key]) > limit
               or any(ord(c) < 32 for c in decision[key])
               for key, limit in (("reason", 1024), ("authorization_reference", 256)))):
        raise ValueError("profile deferral requires public reason and authorization reference")


def profile_deferrals(output, names, reason, authorization_reference, identity, root, receipt):
    if not names:
        if reason is not None or authorization_reference is not None:
            raise ValueError("profile deferral reason and authorization require an explicit check")
        return {}
    required = a.commands(root, identity["version"], receipt["profiles"])
    if len(set(names)) != len(names):
        raise ValueError("duplicate profile deferral check")
    deferred = {}
    receipt_sha256 = a.digest(output / "receipt.json")
    for name in names:
        if not isinstance(name, str) or not name.startswith("profile:") or name not in required or required[name] is None:
            raise ValueError("profile deferral requires one exact declared runnable profile check")
        if receipt["checks"][name] != {"status": "pending"}:
            raise ValueError("profile deferral requires an unrun pending check")
        prefix = name.replace(":", "-").replace("/", "-")
        log = output / (prefix + ".log")
        if log.exists() or log.is_symlink() or any(re.fullmatch(re.escape(prefix) + r"-[0-9a-f]{8}\.log", path.name)
                                                  for path in output.glob(prefix + "-*.log")):
            raise ValueError("profile deferral cannot hide existing execution logs")
        deferred[name] = {"schema_version": 1, "check": name, "target": identity,
                          "command": a.command_identity(required[name], root),
                          "decision": {"reason": reason, "authorization_reference": authorization_reference},
                          "receipt_sha256": receipt_sha256, "original_status": "pending"}
        validate_profile_deferral(deferred[name], name, deferred[name]["command"], identity, receipt_sha256)
    events = output / "events.jsonl"
    if events.exists():
        with events.open() as stream:
            for line in stream:
                item = json.loads(line)
                if not isinstance(item, dict):
                    raise ValueError("invalid collector execution event")
                if item.get("check") in deferred:
                    raise ValueError("profile deferral cannot hide a previously attempted check")
    return deferred


def attestation(output, identity, operator_acceptance_paths=(), profile_deferral_checks=(),
                deferral_reason=None, deferral_authorization_reference=None):
    root, receipt = a.load_bound(output)
    a.update_result(receipt)
    accepted = operator_acceptances(operator_acceptance_paths, identity, root, receipt)
    deferred_profiles = profile_deferrals(output, profile_deferral_checks, deferral_reason,
                                         deferral_authorization_reference, identity, root, receipt)
    if set(accepted) & set(deferred_profiles):
        raise ValueError("a profile check cannot have both failure acceptance and deferral")
    if ((receipt["reproducible_result"] != "passed" and not accepted and not deferred_profiles) or receipt.get("superseded") or receipt.get("preparation") != identity
        or receipt["source_fingerprint"] != identity["source_fingerprint"]
        or receipt["release_files"] != identity["release_files"]):
        raise ValueError("complete passing acceptance of this exact candidate is required")
    checks = {}
    for name, result in receipt["checks"].items():
        if name in deferred_profiles:
            checks[name] = {"status": "not-run", "execution": "operator-deferred", "profile_deferral": deferred_profiles[name]}
            continue
        if name in accepted:
            decision = accepted[name]
            checks[name] = {"status": "failed", "exit_code": decision["exit_code"], "log_sha256": decision["artifacts"]["log"],
                            "execution": "operator-accepted", "environments": decision["environments"], "operator_acceptance": decision}
            continue
        if result["status"] == "passed":
            if result.get("exit_code") != 0 or not re.fullmatch(r"[0-9a-f]{64}", result.get("log_sha256", "")):
                raise ValueError("passing check lacks evidence")
            if not a.is_live(name) and "evidence_transfer" not in result and result.get("execution_environment") != receipt["environment"]:
                raise ValueError("passing local check execution environment differs")
            if name != "package" and result.get("command") != a.command_identity(a.commands(root, receipt["version"], receipt["profiles"])[name], root):
                raise ValueError("passing check lacks command binding")
        checks[name] = {"status": result["status"], **({"log_sha256": result["log_sha256"], "exit_code": 0}
                                                     if result["status"] == "passed" else {})}
        if a.is_live(name) and result["status"] == "passed":
            checks[name]["environments"] = result.get("environments", [])
        if "evidence_transfer" in result:
            checks[name]["execution"] = "inherited"
            checks[name]["evidence_transfer"] = result["evidence_transfer"]
    external = {}
    for name, result in receipt["external_obligations"].items():
        if "deferral" in result:
            a.validate_external_deferral(result, identity_hash(identity))
            external[name] = {"status": "not-run", "deferral": result["deferral"]}
        elif a.optional_external_not_run(result):
            external[name] = {"status": "not-run"}
        else:
            if result["status"] != "passed" or not re.fullmatch(r"[0-9a-f]{64}", result.get("evidence_sha256", "")):
                raise ValueError("external obligation lacks actual evidence SHA-256 or authorized deferral")
            external[name] = {"status": "passed", "evidence_sha256": result["evidence_sha256"]}
    transfers = sorted(name for name, result in checks.items() if "evidence_transfer" in result)
    document = {"schema_version": 4 if deferred_profiles else 3 if accepted else 2 if transfers else 1, "candidate_id": identity["candidate_id"], "identity_sha256": identity_hash(identity),
            "receipt_sha256": a.digest(output / "receipt.json"), "environment": receipt["environment"],
            "checks": checks, "external_obligations": external,
            **({"reviewed_transfers": transfers} if transfers or accepted or deferred_profiles else {}),
            **({"operator_acceptances": sorted(accepted)} if accepted or deferred_profiles else {}),
            **({"profile_deferrals": sorted(deferred_profiles)} if deferred_profiles else {})}
    validate_attestation(document, identity, root)
    return document


def validate_attestation(document, identity, root):
    version = document.get("schema_version")
    keys = {"schema_version", "candidate_id", "identity_sha256", "receipt_sha256", "environment", "checks", "external_obligations"}
    if version in (2, 3, 4):
        keys.add("reviewed_transfers")
    if version in (3, 4):
        keys.add("operator_acceptances")
    if version == 4:
        keys.add("profile_deferrals")
    if (set(document) != keys or type(version) is not int or version not in (1, 2, 3, 4) or document["candidate_id"] != identity["candidate_id"]
        or document["identity_sha256"] != identity_hash(identity)
        or not re.fullmatch(r"[0-9a-f]{64}", document["receipt_sha256"])):
        raise ValueError("acceptance attestation identity mismatch")
    transfers = sorted(name for name, result in document["checks"].items() if "evidence_transfer" in result)
    if (version == 1 and transfers or version in (2, 3, 4) and
        (version == 2 and not transfers or document["reviewed_transfers"] != transfers)):
        raise ValueError("reviewed transfer attestation inventory mismatch")
    accepted = sorted(name for name, result in document["checks"].items() if "operator_acceptance" in result)
    if (version not in (3, 4) and accepted or version in (3, 4) and
        (version == 3 and not accepted or document["operator_acceptances"] != accepted)):
        raise ValueError("operator acceptance attestation inventory mismatch")
    deferred_profiles = sorted(name for name, result in document["checks"].items() if "profile_deferral" in result)
    if (version != 4 and deferred_profiles or version == 4 and
        (not deferred_profiles or document["profile_deferrals"] != deferred_profiles)):
        raise ValueError("profile deferral attestation inventory mismatch")
    environment = document["environment"]
    if (not isinstance(environment, dict) or set(environment) != {"system", "machine", "go"}
        or any(not isinstance(value, str) or not value or len(value) > 256 or any(ord(c) < 32 for c in value)
               for value in environment.values())):
        raise ValueError("acceptance execution environment missing")
    inventory = a.profile_inventory(root)
    required = a.commands(root, identity["version"], inventory)
    required["package"] = ["ci-prepared"]
    if set(document["checks"]) != set(required):
        raise ValueError("acceptance attestation check inventory mismatch")
    for name, command in required.items():
        result = document["checks"][name]
        if command is None or (name == "process-coverage" and result == {"status": "not-applicable"}):
            if result != {"status": "not-applicable"}:
                raise ValueError("invalid not-applicable attestation")
        else:
            if name in deferred_profiles:
                validate_profile_deferral(result["profile_deferral"], name, a.command_identity(command, root), identity,
                                          document["receipt_sha256"])
                if (set(result) != {"status", "execution", "profile_deferral"} or result["status"] != "not-run"
                    or result["execution"] != "operator-deferred"):
                    raise ValueError("profile deferral must retain not-run execution")
                continue
            if name in accepted:
                decision = result["operator_acceptance"]
                validate_operator_acceptance(decision, name, a.command_identity(command, root), identity)
                if (set(result) != {"status", "exit_code", "log_sha256", "execution", "environments", "operator_acceptance"}
                    or result["status"] != "failed" or type(result["exit_code"]) is not int or result["exit_code"] != decision["exit_code"]
                    or result["log_sha256"] != decision["artifacts"]["log"] or result["execution"] != "operator-accepted"
                    or result["environments"] != decision["environments"]):
                    raise ValueError("operator acceptance original failure changed")
                continue
            keys = {"status", "log_sha256", "exit_code"} | ({"environments"} if a.is_live(name) else set())
            if "evidence_transfer" in result:
                keys |= {"execution", "evidence_transfer"}
            if (set(result) != keys or result["status"] != "passed" or type(result["exit_code"]) is not int or result["exit_code"] != 0
                or not re.fullmatch(r"[0-9a-f]{64}", result["log_sha256"])):
                raise ValueError("missing passing acceptance evidence")
            if "evidence_transfer" in result:
                target = {"candidate_sha256": identity_hash(identity), "version": identity["version"],
                          "source_fingerprint": identity["source_fingerprint"], "release_files": identity["release_files"]}
                a.validate_evidence_transfer(result["evidence_transfer"], name, a.command_identity(command, root), target, result["log_sha256"])
                if result["execution"] != "inherited" or result.get("environments", []) != result["evidence_transfer"]["environments"]:
                    raise ValueError("reviewed transfer execution provenance changed")
            if a.is_live(name):
                environments = result["environments"]
                if not isinstance(environments, list) or not environments:
                    raise ValueError("physical acceptance environment missing")
                for item in environments:
                    if (not isinstance(item, dict) or set(item) != {"type", "vm_count", "base_fingerprint"}
                        or not re.fullmatch(r"[a-z][a-z0-9-]{0,79}", item["type"])
                        or type(item["vm_count"]) is not int or not 1 <= item["vm_count"] <= 100
                        or not re.fullmatch(r"[0-9a-f]{64}", item["base_fingerprint"])):
                        raise ValueError("invalid physical acceptance environment")
    external = a.external_inventory(root)
    if set(document["external_obligations"]) != set(external):
        raise ValueError("external obligation attestation inventory mismatch")
    for name, result in document["external_obligations"].items():
        if external[name].get("required", True) is False and result == {"status": "not-run"}:
            continue
        if "deferral" in result:
            if set(result) != {"status", "deferral"}:
                raise ValueError("invalid external deferral attestation")
            a.validate_external_deferral(result, identity_hash(identity))
            continue
        if (set(result) != {"status", "evidence_sha256"} or result["status"] != "passed"
            or not re.fullmatch(r"[0-9a-f]{64}", result["evidence_sha256"])):
            raise ValueError("external obligation evidence missing")


def verify_owned_ref(github, tag, marker, commit, ref=None):
    ref = ref if ref is not None else github.api("git/ref/tags/" + tag)
    if ref["object"]["type"] != "tag":
        raise ValueError("stable tag is occupied by foreign/legacy state")
    obj = github.api("git/tags/" + ref["object"]["sha"])
    if (obj["message"].strip() != marker or obj["object"].get("type") != "commit"
        or obj["object"].get("sha") != commit):
        raise ValueError("stable tag belongs to another candidate")
    return ref


def report_unrun_checks(document):
    optional = [name for name, result in document["external_obligations"].items() if result == {"status": "not-run"}]
    if optional:
        print("PUBLICATION external_optional_not_run=" + ",".join(optional) + " status=not-run", flush=True)
    if document.get("profile_deferrals"):
        print("PUBLICATION profile_deferred=" + ",".join(document["profile_deferrals"])
              + " status=not-run execution=operator-deferred", flush=True)


def publish_remote(github, prepared, document, root):
    document = decode_attestation(document)
    identity = checked_identity(prepared)
    if identity["repository"] != github.repository or a.source_fingerprint(root) != identity["source_fingerprint"]:
        raise ValueError("publication source/repository mismatch")
    verify_run(github, identity["candidate_id"])
    validate_attestation(document, identity, root)
    report_unrun_checks(document)
    for name in document.get("operator_acceptances", []):
        print(f"PUBLICATION operator_accepted={name} native_status=failed exit_code={document['checks'][name]['exit_code']}", flush=True)
    deferred = [name for name, item in document["external_obligations"].items() if "deferral" in item]
    if deferred:
        print("PUBLICATION external_deferred=" + ",".join(deferred) + " execution=not-run", flush=True)
    marker = "Subyard candidate " + identity["candidate_id"] + " identity " + identity_hash(identity)
    tag = "v" + identity["version"]
    releases = [item for item in github.pages("releases") if item["tag_name"] == tag]
    if len(releases) > 1:
        raise ValueError("ambiguous release state")
    if releases and (releases[0].get("body", "").strip() != marker or releases[0].get("prerelease")):
        raise ValueError("release is foreign or contradictory")
    ref = github.api("git/ref/tags/" + tag, missing=True)
    if ref is None:
        obj = github.api("git/tags", "POST", {"tag": tag, "message": marker, "object": identity["commit"], "type": "commit"})
        github.api("git/refs", "POST", {"ref": "refs/tags/" + tag, "sha": obj["sha"]})
        ref = github.api("git/ref/tags/" + tag)
    ref = verify_owned_ref(github, tag, marker, identity["commit"], ref)
    release = releases[0] if releases else github.api("releases", "POST", {
        "tag_name": tag, "target_commitish": identity["commit"], "name": "Subyard " + identity["version"],
        "body": marker, "draft": True, "prerelease": False})
    if release.get("body", "").strip() != marker or release.get("prerelease"):
        raise ValueError("release is foreign or contradictory")
    expected = identity["release_files"]
    assets = github.pages(f"releases/{release['id']}/assets")
    if len({item["name"] for item in assets}) != len(assets) or any(item["name"] not in expected for item in assets):
        raise ValueError("release has foreign or duplicate assets")
    present = {item["name"]: item for item in assets}
    for name, sha in expected.items():
        if name in present and present[name].get("state") == "starter":
            if not release["draft"] or present[name].get("size") != 0:
                raise ValueError("contradictory unfinished upload")
            # GitHub documents starter assets after HTTP 502 as safe to delete.
            github.api(f"releases/assets/{present[name]['id']}", "DELETE")
            del present[name]
        if name not in present:
            if not release["draft"]:
                raise ValueError("published release inventory incomplete")
            github.upload(release["id"], prepared / "assets" / name)
    if release["draft"]:
        verify_release_assets(github, release["id"], expected)
        github.api(f"releases/{release['id']}", "PATCH", {"draft": False, "make_latest": "true"})
    actual = github.api(f"releases/{release['id']}")
    if (actual["draft"] or actual.get("immutable") is not True or actual["tag_name"] != tag
        or actual.get("body", "").strip() != marker or actual.get("prerelease")):
        raise ValueError("actual release is not immutable and fully published; never roll back")
    final_ref = github.api("git/ref/tags/" + tag)
    if final_ref["object"] != ref["object"]:
        raise ValueError("stable ref changed during publication")
    verify_release_assets(github, release["id"], expected)
    return actual


def verify_release_assets(github, release_id, expected):
    assets = github.pages(f"releases/{release_id}/assets")
    if len(assets) != len(expected) or {item["name"] for item in assets} != set(expected):
        raise ValueError("remote asset inventory incomplete")
    with tempfile.TemporaryDirectory(prefix="subyard-release-assets-", dir="/tmp") as temporary:
        for item in assets:
            if item.get("state") != "uploaded":
                raise ValueError("unfinished asset upload; preserve draft for operator inspection")
            downloaded = Path(temporary) / "asset"
            github.download(f"releases/assets/{item['id']}", downloaded)
            if a.digest(downloaded) != expected[item["name"]] or downloaded.stat().st_size != item["size"]:
                raise ValueError("remote asset hash mismatch; never overwrite")


def discover(github, workflow, request):
    matches = [run for run in github.pages(f"actions/workflows/{workflow}/runs?event=workflow_dispatch", "workflow_runs")
               if run["display_title"] == "Subyard " + request]
    if len(matches) > 1:
        raise ValueError("ambiguous dispatch identity")
    return matches[0] if matches else None


def dispatch(github, workflow, inputs, state, path):
    request = state["request"]
    if not state.get("dispatch_started"):
        dispatched_inputs = {**inputs, "request": request}
        if len(canonical(dispatched_inputs)) > DISPATCH_INPUT_LIMIT:
            raise ValueError("workflow dispatch inputs exceed 65535-character limit")
        state["dispatch_started"] = True
        state["status"] = "dispatch-outcome-unknown"
        write_json(path, state)  # Persist intent before a possibly lost response.
        github.api(f"actions/workflows/{workflow}/dispatches", "POST", {"ref": state["workflow_ref"], "inputs": dispatched_inputs})
    deadline = time.monotonic() + 120
    while time.monotonic() < deadline:
        run = discover(github, workflow, request)
        if run:
            state["run_id"] = run["id"]
            if run["status"] == "completed" and run["conclusion"] != "success":
                # Persist rerun intent too: response loss must not launch another attempt.
                target = run["run_attempt"] + 1
                if state.get("rerun_attempt", 0) < target:
                    state["rerun_attempt"] = target
                    state["status"] = "rerun-outcome-unknown"
                    write_json(path, state)
                    github.api(f"actions/runs/{run['id']}/rerun", "POST")
                while time.monotonic() < deadline:
                    run = github.api(f"actions/runs/{run['id']}")
                    if run["run_attempt"] >= target:
                        break
                    time.sleep(2)
                else:
                    raise ValueError("rerun outcome unknown; repeat command to discover retained attempt")
            state["status"] = "running"
            write_json(path, state)
            print("RUN " + run["html_url"], flush=True)
            try:
                last_report = 0
                while True:
                    run = github.api(f"actions/runs/{run['id']}")
                    state["run_attempt"] = run["run_attempt"]
                    state["status"] = run["status"]
                    write_json(path, state)
                    if run["status"] == "completed":
                        state["status"] = "passed" if run["conclusion"] == "success" else run["conclusion"]
                        write_json(path, state)
                        if run["conclusion"] != "success":
                            raise ValueError("workflow failed/cancelled; repeat command resumes the same request")
                        return run
                    if time.monotonic() - last_report >= 60:
                        print(f"WAIT run={run['id']} attempt={run['run_attempt']} status={run['status']}", flush=True)
                        last_report = time.monotonic()
                    time.sleep(20)
            except KeyboardInterrupt:
                state["status"] = "interrupted"
                write_json(path, state)
                raise
    raise ValueError("dispatch outcome unknown; repeat command to discover retained request, do not duplicate dispatch")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--state-dir", type=Path, default=ROOT / ".build/release-publication")
    parser.add_argument("--repository", default=REPOSITORY)
    sub = parser.add_subparsers(dest="action", required=True)
    prepare = sub.add_parser("prepare", help="dispatch all preparation gates on a pinned commit; create no stable tag")
    prepare.add_argument("--version", required=True)
    prepare.add_argument("--commit", required=True)
    prepare.add_argument("--workflow-ref", default="master", help="branch containing the dispatch workflow")
    for action in ("accept", "publish"):
        command = sub.add_parser(action)
        command.add_argument("--candidate", required=True)
        if action == "accept":
            command.add_argument("--slots", nargs="+", type=int, required=True)
        else:
            command.add_argument("--workflow-ref", default="master")
            command.add_argument("--operator-acceptance", type=Path, action="append", default=[],
                                 help="explicit exact-candidate profile failure declaration; repeat for each authorized check")
            command.add_argument("--defer-profile", action="append", default=[], metavar="CHECK",
                                 help="explicitly defer one exact unrun live profile check; repeat for each authorized check")
            command.add_argument("--deferral-reason", help="public reason for the named profile deferrals")
            command.add_argument("--deferral-authorization-reference", help="public reference to the profile deferral authorization")
    export = sub.add_parser("export-ci", help="internal CI asset freezing entrypoint")
    for name in ("version", "commit", "candidate"):
        export.add_argument("--" + name, required=True)
    export.add_argument("--output", type=Path, required=True)
    remote = sub.add_parser("publish-ci", help="internal serialized workflow publication entrypoint")
    remote.add_argument("--prepared", type=Path, required=True)
    remote.add_argument("--attestation", type=Path, required=True)
    args = parser.parse_args()
    def interrupted(signum, frame):
        raise KeyboardInterrupt
    signal.signal(signal.SIGTERM, interrupted)
    github = GitHub(args.repository)
    if args.action == "export-ci":
        export_candidate(args.output.resolve(), args.version, args.commit, args.candidate, args.repository)
        return 0
    if args.action == "publish-ci":
        # Mutation entrypoint belongs only to the single native concurrency workflow.
        if os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("GITHUB_WORKFLOW") != "Release publication":
            raise ValueError("publication must run in the serialized Release publication workflow")
        actual = publish_remote(github, args.prepared.resolve(), read_attestation(args.attestation), ROOT)
        print("PUBLISHED " + actual["html_url"])
        return 0
    state_dir = args.state_dir.resolve()
    if state_dir == ROOT or (ROOT in state_dir.parents and ROOT / ".build" not in state_dir.parents):
        raise ValueError("state directory must be under .build or outside the source checkout")
    state_dir.mkdir(parents=True, exist_ok=True)
    state_dir.chmod(0o700)
    if args.action == "prepare":
        if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", args.version) or not re.fullmatch(r"[0-9a-f]{40}", args.commit):
            raise ValueError("stable X.Y.Z and full lowercase 40-character commit SHA required")
        requests = state_dir / "requests"
        requests.mkdir(exist_ok=True)
        path = requests / (args.version + "-" + args.commit + ".json")
        with a.acceptance_lock(requests):
            state = json.loads(path.read_text()) if path.exists() else {
                "request": str(uuid.uuid4()), "version": args.version, "commit": args.commit,
                "repository": args.repository, "workflow_ref": args.workflow_ref}
            if state["repository"] != args.repository:
                raise ValueError("request repository mismatch")
            write_json(path, state)
            run = dispatch(github, "release.yml", {"version": args.version, "commit": args.commit}, state, path)
            candidate = f"r{run['id']}-a{run['run_attempt']}"
            directory = state_dir / candidate
            directory.mkdir(exist_ok=True)
            prepared = directory / "prepared"
            if not prepared.exists():
                with tempfile.TemporaryDirectory(prefix=".download-", dir=directory) as temporary:
                    staging = Path(temporary) / "prepared"
                    identity = fetch(github, candidate, staging)
                    if identity["version"] != args.version or identity["commit"] != args.commit:
                        raise ValueError("preparation inputs mismatch")
                    staging.rename(prepared)
            identity = checked_identity(prepared)
            if (identity["candidate_id"] != candidate or identity["repository"] != github.repository
                or identity["version"] != args.version or identity["commit"] != args.commit):
                raise ValueError("local preparation state mismatch")
            write_json(directory / "identity.json", identity)
            print("CANDIDATE " + candidate + " " + str(directory))
            return 0
    candidate_parts(args.candidate)
    directory = state_dir / args.candidate
    directory.mkdir(exist_ok=True)
    directory.chmod(0o700)
    with a.acceptance_lock(directory):
        prepared = directory / "prepared"
        if not prepared.exists():
            with tempfile.TemporaryDirectory(prefix=".download-", dir=directory) as temporary:
                staging = Path(temporary) / "prepared"
                fetch(github, args.candidate, staging)
                staging.rename(prepared)
        identity = checked_identity(prepared)
        if identity["candidate_id"] != args.candidate or identity["repository"] != github.repository:
            raise ValueError("local state belongs to another candidate/repository")
        write_json(directory / "identity.json", identity)
        output = directory / "acceptance"
        if args.action == "accept":
            for signum in (signal.SIGINT, signal.SIGTERM):
                signal.signal(signum, lambda signum, frame: a.STOP.set())
            if len(set(args.slots)) != len(args.slots) or any(slot < 1 or slot > 999 for slot in args.slots):
                raise ValueError("slots must be unique integers from 1 to 999")
            if not output.exists():
                with tempfile.TemporaryDirectory(prefix=".acceptance-", dir=directory) as temporary:
                    staging = Path(temporary) / "acceptance"
                    a.prepare(staging, identity["version"], prepared)
                    staging.rename(output)
            with a.acceptance_lock(output):
                result = a.run_checks(output, args.slots, [], False)
            if result == 0:
                write_json(directory / "attestation.json", attestation(output, identity))
            return result
        with a.acceptance_lock(output, shared=True):
            document = attestation(output, identity, [path.resolve() for path in args.operator_acceptance], args.defer_profile,
                                   args.deferral_reason, args.deferral_authorization_reference)
            validate_attestation(document, identity, output / "source")
        report_unrun_checks(document)
        deferred = [name for name, item in document["external_obligations"].items() if "deferral" in item]
        if deferred:
            print("PUBLICATION external_deferred=" + ",".join(deferred) + " execution=not-run", flush=True)
        path = directory / "publication.json"
        state = json.loads(path.read_text()) if path.exists() else {
            "request": str(uuid.uuid4()), "workflow_ref": args.workflow_ref, "attestation_sha256": identity_hash(document)}
        if state["attestation_sha256"] != identity_hash(document):
            raise ValueError("publication attestation changed; preserve original unfinished state")
        write_json(directory / "attestation.json", document)
        write_json(path, state)
        encoded = encode_attestation(document)
        run = dispatch(github, "release-publish.yml", {"candidate": args.candidate, "attestation": encoded}, state, path)
        # A failed/cancelled run can be rerun natively; no second publication request is needed.
        release = github.api("releases/tags/v" + identity["version"])
        marker = "Subyard candidate " + args.candidate + " identity " + identity_hash(identity)
        if (release.get("draft") or release.get("immutable") is not True or release.get("body", "").strip() != marker):
            raise ValueError("actual immutable publication not confirmed")
        verify_owned_ref(github, "v" + identity["version"], marker, identity["commit"])
        verify_release_assets(github, release["id"], identity["release_files"])
        state.update({"status": "published", "release_id": release["id"], "run_id": run["id"]})
        write_json(path, state)
        print("PUBLISHED " + release["html_url"])
        return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        print("release: interrupted; retained durable state permits retry", file=sys.stderr)
        sys.exit(130)
    except (ValueError, KeyError, TypeError, AttributeError, OSError, subprocess.SubprocessError) as error:
        message = str(error) if isinstance(error, (ValueError, KeyError)) else type(error).__name__
        print("release: " + message, file=sys.stderr)
        sys.exit(2)
