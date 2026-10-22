#!/usr/bin/env bash
# Host-free release orchestration, binding and recovery contracts.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PYTHONDONTWRITEBYTECODE=1 python3 -B - "$ROOT" <<'PY'
import base64
import copy
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
import zlib
from unittest.mock import patch
from contextlib import redirect_stdout

ROOT = Path(sys.argv[1])
sys.argv = [sys.argv[0]]
spec = importlib.util.spec_from_file_location("publication", ROOT / "dev/release-publication.py")
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)


def fixture(directory, lanes=(), external_required=None):
    root = directory / "source"
    for name in ("dev/agent-e2e.sh", "scripts/lib/runtime.sh", "tests/helpers/source-files.sh"):
        target = root / name
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(ROOT / name, target)
    (root / ".gitignore").write_text("/.build/\n/private/\n/.subyard-acceptance/\n")
    (root / "untracked-public.txt").write_text("frozen input\n")
    (root / "dev/test-profiles.sh").write_text("#!/bin/bash\nprintf 'runner\\tsample\\tconfig/profiles/sample/tests/e2e/acceptance.sh\\n'\n")
    profile = root / "config/profiles/sample/tests/e2e"
    profile.mkdir(parents=True)
    (profile / "acceptance.sh").write_text("#!/bin/bash\nexit 0\n")
    if lanes:
        (profile / "acceptance-lanes.json").write_text(json.dumps({"schema_version": 1, "lanes": [
            {"id": lane, "arguments": ["--lane", lane]} for lane in lanes]}))
    external = {"id": "fixture-external", "description": "fixture obligation", "required_inputs": ["fixture account"]}
    if external_required is not None:
        external["required"] = external_required
    (profile / "external-obligations.json").write_text(json.dumps({"schema_version": 1, "obligations": [external]}))
    (root / "dev/package-engine.sh").write_text("#!/bin/bash\necho 'must not repackage'; exit 99\n")
    prepared = directory / "prepared"
    prepared.mkdir()
    assets = prepared / "assets"
    assets.mkdir()
    version = "1.2.3"
    names = ["subyard-install.sh", "subyard-install-runtime-release.sh", "subyard-install-runtime-release.sh.sha256"]
    for arch in ("amd64", "arm64"):
        names += [f"paseo-headless-0.2.1-linux-{arch}.tar.gz", f"paseo-headless-0.2.1-linux-{arch}.tar.gz.sha256"]
        for artifact in (f"yard-{version}-linux-{arch}", f"subyard-{version}-linux-{arch}.tar.gz"):
            names += [artifact, artifact + ".sha256", artifact + ".manifest.json", artifact + ".provenance.json"]
    for name in names:
        (assets / name).write_text("prepared exact CI bytes: " + name + "\n")
    for name in names:
        if name.endswith(".sha256"):
            artifact = name.removesuffix(".sha256")
            (assets / name).write_text(m.a.digest(assets / artifact) + "  " + artifact + "\n")
        if name.endswith(".provenance.json"):
            artifact = name.removesuffix(".provenance.json")
            (assets / name).write_text(json.dumps({"schemaVersion": 1, "artifact": artifact,
                "sha256": m.a.digest(assets / artifact), "version": version, "sourceRevision": "a" * 40}))
    m.a.bundle(root, prepared / "source.tar.gz")
    identity = {"schema_version": 1, "candidate_id": "r123-a1", "repository": m.REPOSITORY,
        "version": version, "commit": "a" * 40, "source_fingerprint": m.a.source_fingerprint(root),
        "source_archive_sha256": m.a.digest(prepared / "source.tar.gz"),
        "release_files": {name: m.a.digest(assets / name) for name in names}}
    m.write_json(prepared / "identity.json", identity)
    return root, prepared, identity


def accepted(directory, prepared, identity, external_passed=True):
    output = directory / "acceptance"
    with patch.object(m.a, "execution_environment", return_value={"system": "Linux", "machine": "x86_64", "go": "go version fixture"}):
        m.a.prepare(output, identity["version"], prepared)
    root, receipt = m.a.load_bound(output)
    for name, command in m.a.commands(root, identity["version"], receipt["profiles"]).items():
        if command:
            log = output / (name.replace(":", "-").replace("/", "-") + ".log")
            log.write_text("synthetic passing evidence fixture\n")
            receipt["checks"][name] = {"status": "passed", "exit_code": 0, "log": log.name,
                "log_sha256": m.a.digest(log), "command": m.a.command_identity(command, root),
                "execution_environment": receipt["environment"]}
            if m.a.is_live(name):
                receipt["checks"][name]["environments"] = [{"type": "subyard-pair", "vm_count": 2, "base_fingerprint": "b" * 64}]
    if external_passed:
        for item in receipt["external_obligations"].values():
            item.update({"status": "passed", "evidence_sha256": "c" * 64})
    m.a.save(output, receipt)
    return output, root, m.attestation(output, identity)



def capture_attempts(output, names=None):
    root, receipt = m.a.load_bound(output)
    for name, result in receipt["checks"].items():
        if name == "package" or result["status"] != "passed" or result.get("inherited") or names is not None and name not in names:
            continue
        result.update(termination="exit", capture_status="recorded", duration_seconds=1)
        if m.a.is_live(name):
            result.update(environment_evidence="recorded", cleanup_status="recorded",
                          cleanup={"lease-release": {"exit_code": 0, "duration_seconds": 1}})
        result["original_attempt"] = m.a.completed_attempt(output, root, receipt, name, result)
    m.a.save(output, receipt)
    return root, receipt

class FakeGitHub:
    repository = m.REPOSITORY
    def __init__(self, identity):
        self.identity = identity
        self.ref = None
        self.tag = None
        self.release = None
        self.assets = []
        self.data = {}
        self.calls = []
        self.fail = None
        self.successful_run = True
        self.complete_jobs = True
        self.immutable = True

    def api(self, path, method="GET", payload=None, missing=False):
        self.calls.append((path, method))
        if path == "actions/workflows/release.yml":
            return {"id": 9}
        if path.startswith("actions/runs/") and path.endswith("/attempts/1"):
            return {"workflow_id": 9, "event": "workflow_dispatch", "status": "completed",
                "conclusion": "success" if self.successful_run else "failure", "run_attempt": 1,
                "repository": {"full_name": self.repository}}
        if path.startswith("git/ref/tags/"):
            return copy.deepcopy(self.ref)
        if path == "git/tags" and method == "POST":
            self.tag = {"message": payload["message"], "object": {"type": "commit", "sha": payload["object"]}, "sha": "t" * 40}
            return self.tag
        if path == "git/refs" and method == "POST":
            self.ref = {"object": {"type": "tag", "sha": payload["sha"]}}
            self.maybe_fail("ref")
            return self.ref
        if path.startswith("git/tags/"):
            return copy.deepcopy(self.tag)
        if path == "releases" and method == "POST":
            self.release = {**payload, "id": 7, "immutable": False, "html_url": "https://fixture/release"}
            self.maybe_fail("draft")
            return copy.deepcopy(self.release)
        if path == "releases/7" and method == "PATCH":
            self.release.update({"draft": False, "immutable": self.immutable})
            self.maybe_fail("published")
            return copy.deepcopy(self.release)
        if path == "releases/7":
            return copy.deepcopy(self.release)
        if path.startswith("releases/assets/") and method == "DELETE":
            asset_id = int(path.split("/")[-1])
            self.assets = [item for item in self.assets if item["id"] != asset_id]
            return None
        raise AssertionError((path, method, payload))

    def pages(self, path, key=None):
        if path.startswith("actions/runs/") and path.endswith("/jobs"):
            names = ["validate", "native-engine-arm64", "paseo-headless-amd64", "paseo-headless-arm64", "prepare"]
            if not self.complete_jobs:
                names.remove("native-engine-arm64")
            return [{"name": name, "status": "completed", "conclusion": "success"} for name in names]
        if path == "releases":
            return [copy.deepcopy(self.release)] if self.release else []
        if path == "releases/7/assets":
            return copy.deepcopy(self.assets)
        raise AssertionError(path)

    def upload(self, release_id, path):
        asset_id = len(self.data) + 1
        self.data[asset_id] = path.read_bytes()
        self.assets.append({"id": asset_id, "name": path.name, "state": "uploaded", "size": path.stat().st_size})
        self.maybe_fail("upload")

    def download(self, path, destination):
        destination.write_bytes(self.data[int(path.split("/")[-1])])

    def maybe_fail(self, point):
        if self.fail == point:
            self.fail = None
            raise ValueError("fixture lost response " + point)


class Downloads(unittest.TestCase):
    def test_download_media_types(self):
        with tempfile.TemporaryDirectory(prefix="subyard-download-", dir="/tmp") as directory:
            destination = Path(directory) / "download"
            for path, accept in (("actions/artifacts/123/zip", "application/json"),
                                 ("releases/assets/456", "application/octet-stream")):
                with self.subTest(path=path), patch.object(m.subprocess, "run",
                        return_value=subprocess.CompletedProcess([], 0, stderr=b"")) as run:
                    m.GitHub().download(path, destination)
                    self.assertEqual(run.call_args.args[0], ["gh", "api", m.GitHub().prefix + path,
                                                            "-H", "Accept: " + accept])

    def test_download_failure_reports_only_http_status(self):
        with tempfile.TemporaryDirectory(prefix="subyard-download-", dir="/tmp") as directory:
            with patch.object(m.subprocess, "run", return_value=subprocess.CompletedProcess(
                    [], 1, stderr=b"private response detail (HTTP 415)")):
                with self.assertRaises(ValueError) as caught:
                    m.GitHub().download("actions/artifacts/123/zip", Path(directory) / "download")
            self.assertEqual(str(caught.exception),
                             "GitHub download failed HTTP 415; retry with retained candidate")


class Contracts(unittest.TestCase):
    def test_v5_deduplicates_manifests_and_rejects_extra_dangling_conflicting_references(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            directory = Path(temporary)
            _, prepared, identity = fixture(directory)
            output, root, legacy = accepted(directory, prepared, identity)
            self.assertEqual(legacy["schema_version"], 1)
            capture_attempts(output)
            document = m.attestation(output, identity)
            self.assertEqual(document["schema_version"], 5)
            self.assertEqual(len(document["manifests"]), 1)
            self.assertGreater(sum("original_attempt" in item for item in document["checks"].values()), 2)
            m.validate_attestation(m.decode_attestation(json.loads(base64.b64decode(m.encode_attestation(document)))), identity, root)
            name = "verify"
            for mutate in (lambda item: item["manifests"].update({"a" * 64: next(iter(item["manifests"].values()))}),
                           lambda item: item["checks"][name]["original_attempt"].update(manifest_sha256="e" * 64),
                           lambda item: item["checks"][name].update(attempt_sha256="d" * 64),
                           lambda item: item["checks"][name]["original_attempt"]["result"].update(capture_status=None),
                           lambda item: item["checks"][name]["original_attempt"].update(extra=True)):
                invalid = copy.deepcopy(document)
                mutate(invalid)
                with self.assertRaises(ValueError):
                    m.validate_attestation(invalid, identity, root)
            invalid = copy.deepcopy(document)
            extra = copy.deepcopy(next(iter(invalid["manifests"].values())))
            extra["environment"]["go"] += " other controller"
            invalid["manifests"][m.a.evidence_hash(extra)] = extra
            with self.assertRaisesRegex(ValueError, "extra or dangling"):
                m.validate_attestation(invalid, identity, root)
            invalid = copy.deepcopy(document)
            other = next(key for key, value in invalid["checks"].items() if key != name and "original_attempt" in value)
            record = invalid["checks"][other]["original_attempt"]
            record["id"] = invalid["checks"][name]["original_attempt"]["id"]
            invalid["checks"][other]["attempt_sha256"] = m.a.evidence_hash(record)
            with self.assertRaisesRegex(ValueError, "conflicting"):
                m.validate_attestation(invalid, identity, root)
            invalid = copy.deepcopy(document)
            invalid["extra"] = "x" * (1 << 20)
            with self.assertRaisesRegex(ValueError, "byte limit"):
                m.validate_attestation(invalid, identity, root)

    def test_v5_preserves_legacy_transfer_and_profile_deferral(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            directory = Path(temporary)
            _, prepared, identity = fixture(directory)
            source, _, _ = accepted(directory, prepared, identity)
            _, old = m.a.load_bound(source)
            target_dir = directory / "target"
            target_dir.mkdir()
            target, root, _ = accepted(target_dir, prepared, identity)
            _, current = m.a.load_bound(target)
            for name in ("verify", "profile:sample"):
                if name == "profile:sample":
                    (target / current["checks"][name]["log"]).unlink()  # Remove only the synthetic fixture's unrun log.
                current["checks"][name] = {"status": "pending"}
            m.a.save(target, current)
            review = {"schema_version": 1, "check": "verify", "tested": m.a.transfer_candidate(old),
                "target": m.a.transfer_candidate(current), "source_receipt_sha256": m.a.digest(source / "receipt.json"),
                "review": {"unchanged_scope": "Exact synthetic check", "reason": "Same inputs", "authorization_reference": "fixture-review"},
                "standalone": None}
            path = directory / "review-v1.json"
            path.write_text(json.dumps(review))
            self.assertEqual(m.a.transfer_evidence(target, source, "verify", path), 0)
            capture_attempts(target)
            document = m.attestation(target, identity, (), ["profile:sample"], "Known fixture issue", "operator:fixture")
            self.assertEqual(document["schema_version"], 5)
            self.assertEqual(document["checks"]["verify"]["evidence_transfer"]["schema_version"], 1)
            self.assertEqual(document["profile_deferrals"], ["profile:sample"])
            self.assertEqual(document["checks"]["profile:sample"]["status"], "not-run")
            m.validate_attestation(document, identity, root)

    def test_v2_attempt_transfer_publishes_with_shared_v5_manifests(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            directory = Path(temporary)
            _, prepared, identity = fixture(directory)
            source, _, _ = accepted(directory, prepared, identity)
            _, old = capture_attempts(source)
            target_dir = directory / "target"
            target_dir.mkdir()
            target, root, _ = accepted(target_dir, prepared, identity)
            _, current = m.a.load_bound(target)
            current["checks"]["verify"] = {"status": "pending"}
            m.a.save(target, current)
            record, source_manifest = m.a.original_attempt(source, old, "verify", old["checks"]["verify"])
            target_manifest = m.a.candidate_manifest(root, current)
            review = {"schema_version": 2, "check": "verify", "tested_manifest_sha256": m.a.evidence_hash(source_manifest),
                "target_manifest_sha256": m.a.evidence_hash(target_manifest), "original_attempt": old["checks"]["verify"]["original_attempt"],
                "review": {"unchanged_scope": "Exact synthetic check", "reason": "Same inputs", "authorization_reference": "fixture-review"},
                "standalone": None}
            path = directory / "review-v2.json"
            path.write_text(json.dumps(review))
            self.assertEqual(m.a.transfer_evidence(target, source, "verify", path), 0)
            capture_attempts(target)
            document = m.attestation(target, identity)
            self.assertEqual(document["schema_version"], 5)
            self.assertEqual(document["reviewed_transfers"], ["verify"])
            # Each transport-bound identity appears once even when many checks share it.
            self.assertEqual(set(document["manifests"]), {review["tested_manifest_sha256"], review["target_manifest_sha256"]})
            m.validate_attestation(document, identity, root)
            github = FakeGitHub(identity)
            self.assertEqual(m.publish_remote(github, prepared, document, root)["html_url"], "https://fixture/release")
            invalid = copy.deepcopy(document)
            invalid["checks"]["verify"]["evidence_transfer"]["original_attempt"]["id"] = "d" * 32
            with self.assertRaises(ValueError):
                m.validate_attestation(invalid, identity, root)

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="subyard-publication-", dir="/tmp")
        self.directory = Path(self.temporary.name)
        self.root, self.prepared, self.identity = fixture(self.directory)
        self.output, self.frozen, self.document = accepted(self.directory, self.prepared, self.identity)
        self.github = FakeGitHub(self.identity)

    def tearDown(self):
        self.temporary.cleanup()

    def publish(self):
        return m.publish_remote(self.github, self.prepared, self.document, self.frozen)

    def test_exact_import_no_rebuild_and_changed_public_inputs_reject(self):
        frozen, receipt = m.a.load_bound(self.output)
        self.assertEqual(receipt["release_files"], self.identity["release_files"])
        self.assertEqual((frozen / "untracked-public.txt").read_text(), "frozen input\n")
        self.assertEqual(receipt["preparation"], self.identity)
        release = frozen / ".subyard-acceptance/release"
        self.assertEqual((release / "yard-1.2.3-linux-amd64").stat().st_mode & 0o777, 0o755)
        self.assertEqual((release / "yard-1.2.3-linux-amd64.sha256").stat().st_mode & 0o777, 0o644)
        self.assertIn("Imported exact prepared CI assets", (self.output / "package-ci.log").read_text())
        (frozen / "untracked-public.txt").write_text("changed")
        with self.assertRaisesRegex(ValueError, "source fingerprint"):
            m.attestation(self.output, self.identity)

    def test_optional_coverage_and_historical_required_publication(self):
        original = (self.output / "receipt.json").read_bytes()
        root, receipt = m.a.load_bound(self.output)
        receipt["checks"]["process-coverage"] = m.a.PROCESS_COVERAGE_DIAGNOSTIC.copy()
        m.a.save(self.output, receipt)
        optional = m.attestation(self.output, self.identity)
        self.assertEqual(optional["checks"]["process-coverage"], {"status": "not-applicable"})
        receipt["checks"]["process-coverage"] = {"status": "pending"}
        m.a.save(self.output, receipt)
        with self.assertRaisesRegex(ValueError, "complete passing"):
            m.attestation(self.output, self.identity)
        command = m.a.commands(root, receipt["version"], receipt["profiles"])["process-coverage"]
        log = self.output / "historical-coverage.log"
        log.write_text("historical process coverage evidence\n")
        receipt["checks"]["process-coverage"] = {"status": "passed", "exit_code": 0, "command": command,
            "log": log.name, "log_sha256": m.a.digest(log), "execution_environment": receipt["environment"]}
        m.a.save(self.output, receipt)
        legacy = m.attestation(self.output, self.identity)
        m.validate_attestation(legacy, self.identity, root)
        del legacy["checks"]["process-coverage"]
        with self.assertRaisesRegex(ValueError, "inventory"):
            m.validate_attestation(legacy, self.identity, root)
        (self.output / "receipt.json").write_bytes(original)

    def test_success_same_bytes_full_inventory_immutable_and_repeat(self):
        self.assertTrue(self.publish()["immutable"])
        self.assertEqual(len(self.github.assets), len(self.identity["release_files"]))
        before = copy.deepcopy(self.github.assets)
        self.assertTrue(self.publish()["immutable"])
        self.assertEqual(self.github.assets, before)
        self.assertFalse(any(method == "DELETE" for _, method in self.github.calls))

    def test_optional_external_not_run_publishes_without_credentials(self):
        directory = self.directory / "r123-a1"
        directory.mkdir()
        _, prepared, identity = fixture(directory, external_required=False)
        output, frozen, document = accepted(directory, prepared, identity, external_passed=False)
        name = "sample:fixture-external"
        self.assertEqual(document["external_obligations"], {name: {"status": "not-run"}})
        self.assertEqual(json.loads((output / "receipt.json").read_text())["result"], "passed")
        github = FakeGitHub(identity)
        captured = io.StringIO()
        with patch.object(m, "GitHub", return_value=github), \
             patch.object(m.a, "execution_environment", return_value=document["environment"]), \
             patch.object(m.a, "execute", side_effect=AssertionError("already passed")), \
             patch.object(m.signal, "signal"), redirect_stdout(captured), \
             patch.object(sys, "argv", ["release", "--state-dir", str(self.directory),
                                       "accept", "--candidate", identity["candidate_id"], "--slots", "1"]):
            self.assertEqual(m.main(), 0)
        self.assertIn("external_optional_not_run=" + name, captured.getvalue())
        self.assertEqual(json.loads((directory / "attestation.json").read_text())["external_obligations"],
                         {name: {"status": "not-run"}})
        self.assertTrue(m.publish_remote(github, prepared, document, frozen)["immutable"])
        # The same strict schema derives optionality from the frozen source.
        self.assertEqual(document["schema_version"], 1)
        for invalid_result in ({"status": "failed"}, {"status": "unknown"}, {"status": "pending"},
                               {"status": "not-run", "evidence_sha256": "c" * 64},
                               {"status": "not-run", "exit_code": 1},
                               {"status": "not-run", "required": False}):
            invalid = copy.deepcopy(document)
            invalid["external_obligations"][name] = invalid_result
            refusing = FakeGitHub(identity)
            with self.subTest(result=invalid_result), self.assertRaises(ValueError):
                m.publish_remote(refusing, prepared, invalid, frozen)
            self.assertFalse(any(method in ("POST", "PATCH", "DELETE") for _, method in refusing.calls))
        for key in (name, "extra:check"):
            invalid = copy.deepcopy(document)
            if key == name:
                del invalid["external_obligations"][key]
            else:
                invalid["external_obligations"][key] = {"status": "not-run"}
            with self.assertRaisesRegex(ValueError, "inventory"):
                m.validate_attestation(invalid, identity, frozen)
        # Optional external checks cannot waive local, physical or profile gates.
        for check in ("verify", "p0-release-smoke", "profile:sample"):
            invalid = copy.deepcopy(document)
            invalid["checks"][check] = {"status": "not-run"}
            with self.subTest(check=check), self.assertRaises(ValueError):
                m.validate_attestation(invalid, identity, frozen)
        _, receipt = m.a.load_bound(output)
        original = copy.deepcopy(receipt)
        for mutation in ({"status": "failed"}, {"status": "unknown"}, {"status": "pending"},
                         {"status": "not-run", "evidence_sha256": "c" * 64}, {"required": True},
                         {"required": 0}, {"required": None},
                         {"deferral": {"reason": "fixture", "authorization_reference": "fixture",
                                       "candidate_sha256": m.identity_hash(identity)}, "exit_code": 1}):
            receipt = copy.deepcopy(original)
            receipt["external_obligations"][name].update(mutation)
            m.a.save(output, receipt)
            with self.subTest(mutation=mutation), self.assertRaises(ValueError):
                m.attestation(output, identity)
        receipt = copy.deepcopy(original)
        del receipt["external_obligations"][name]["required"]
        m.a.save(output, receipt)
        with self.assertRaisesRegex(ValueError, "external obligations changed"):
            m.attestation(output, identity)
        # Frozen-source policy cannot be changed underneath its candidate identity.
        manifest = frozen / "config/profiles/sample/tests/e2e/external-obligations.json"
        contents = json.loads(manifest.read_text())
        contents["obligations"][0]["required"] = True
        manifest.write_text(json.dumps(contents))
        with self.assertRaisesRegex(ValueError, "source/repository mismatch"):
            m.publish_remote(FakeGitHub(identity), prepared, document, frozen)

    def test_historical_external_manifest_remains_required(self):
        name = "sample:fixture-external"
        document = copy.deepcopy(self.document)
        document["external_obligations"][name] = {"status": "not-run"}
        with self.assertRaisesRegex(ValueError, "external obligation evidence"):
            m.validate_attestation(document, self.identity, self.frozen)
        _, receipt = m.a.load_bound(self.output)
        original = copy.deepcopy(receipt)
        for result in ({"status": "not-run"}, {"status": "not-run", "required": False}):
            receipt = copy.deepcopy(original)
            receipt["external_obligations"][name] = {**m.a.external_inventory(self.frozen)[name], **result}
            m.a.save(self.output, receipt)
            with self.subTest(result=result), self.assertRaises(ValueError):
                m.attestation(self.output, self.identity)

    def test_reviewed_transfer_survives_attestation_and_rejects_remote_provenance_drift(self):
        target = self.directory / "transferred"
        with patch.object(m.a, "execution_environment", return_value=self.document["environment"]):
            m.a.prepare(target, self.identity["version"], self.prepared)
        target_root, receipt = m.a.load_bound(target)
        source_root, source = m.a.load_bound(self.output)
        name = "verify"
        review = {"schema_version": 1, "check": name, "tested": m.a.transfer_candidate(source),
            "target": m.a.transfer_candidate(receipt), "source_receipt_sha256": m.a.digest(self.output / "receipt.json"),
            "review": {"unchanged_scope": "Exact unchanged verification scope", "reason": "Original native PASS reviewed",
                       "authorization_reference": "operator-confirmation:fixture"}, "standalone": None}
        declaration = self.directory / "review.json"
        declaration.write_text(json.dumps(review))
        m.a.transfer_evidence(target, self.output, name, declaration)
        _, receipt = m.a.load_bound(target)
        for check, result in source["checks"].items():
            if check in ("package", name):
                continue
            receipt["checks"][check] = copy.deepcopy(result)
            shutil.copyfile(self.output / result["log"], target / result["log"])
        receipt["external_obligations"] = copy.deepcopy(source["external_obligations"])
        m.a.save(target, receipt)
        document = m.attestation(target, self.identity)
        self.assertEqual(document["schema_version"], 2)
        self.assertEqual(document["reviewed_transfers"], [name])
        transferred = document["checks"][name]
        self.assertEqual(transferred["execution"], "inherited")
        self.assertEqual(transferred["evidence_transfer"]["artifacts"]["receipt"], review["source_receipt_sha256"])
        self.assertNotIn("evidence_files", transferred)
        self.assertNotIn(str(self.directory), json.dumps(document))
        m.validate_attestation(document, self.identity, target_root)
        for mutate in (lambda d: d["checks"][name]["evidence_transfer"]["review"].update(authorization_reference=""),
                       lambda d: d["checks"][name]["evidence_transfer"]["target"].update(source_fingerprint="f" * 64),
                       lambda d: d["checks"][name]["evidence_transfer"].update(actual_command=["foreign-command"]),
                       lambda d: d["checks"][name].pop("evidence_transfer"),
                       lambda d: d.update(schema_version=1),
                       lambda d: d.update(reviewed_transfers=[])):
            invalid = copy.deepcopy(document)
            mutate(invalid)
            with self.assertRaises(ValueError):
                m.validate_attestation(invalid, self.identity, target_root)
        self.assertEqual(self.document["schema_version"], 1)
        m.validate_attestation(self.document, self.identity, self.frozen)

    def test_large_reviewed_attestation_roundtrips_existing_workflow_and_retained_request(self):
        large = self.directory / "large"
        _, prepared, identity = fixture(large, ("one", "two", "three", "four"))
        source_output, _, _ = accepted(large, prepared, identity)
        _, source = m.a.load_bound(source_output)
        for name, result in source["checks"].items():
            if name != "package":
                result["duration_seconds"] = 123.5
                if m.a.is_live(name):
                    result["slot"] = 2
        m.a.save(source_output, source)
        state_dir = self.directory / "publication"
        candidate = state_dir / identity["candidate_id"]
        candidate.mkdir(parents=True)
        shutil.copytree(prepared, candidate / "prepared")
        output = candidate / "acceptance"
        with patch.object(m.a, "execution_environment", return_value=source["environment"]):
            m.a.prepare(output, identity["version"], candidate / "prepared")
        root, receipt = m.a.load_bound(output)
        # This fixture explicitly transfers diagnostic execution as historical evidence.
        receipt["checks"]["process-coverage"] = {"status": "pending"}
        m.a.save(output, receipt)
        names = sorted(set(source["checks"]) - {"package"})
        self.assertEqual(len(names), 10)
        for name in names:
            review = {"schema_version": 1, "check": name, "tested": m.a.transfer_candidate(source),
                "target": m.a.transfer_candidate(receipt), "source_receipt_sha256": m.a.digest(source_output / "receipt.json"),
                "review": {"unchanged_scope": "Reviewed unchanged scope. " * 70,
                           "reason": "Retain original passing evidence. " * 30,
                           "authorization_reference": "operator-confirmation:fixture"}, "standalone": None}
            declaration = self.directory / "large-review.json"
            declaration.write_text(json.dumps(review))
            m.a.transfer_evidence(output, source_output, name, declaration)
        m.a.defer_external(output, ["sample:fixture-external"], "External account unavailable", "fixture-authorization")
        document = m.attestation(output, identity)
        original = m.canonical(document).encode()
        self.assertGreater(len(base64.b64encode(original)), m.DISPATCH_INPUT_LIMIT)
        self.assertEqual(document["reviewed_transfers"], names)
        github = FakeGitHub(identity)
        dispatched = []
        run = {"id": 456, "status": "completed", "conclusion": "success", "run_attempt": 1,
               "html_url": "https://fixture/run"}
        original_api = github.api
        def api(path, method="GET", payload=None, missing=False):
            if path.endswith("/dispatches"):
                dispatched.append(copy.deepcopy(payload["inputs"]))
                self.assertLessEqual(len(m.canonical(payload["inputs"])), m.DISPATCH_INPUT_LIMIT)
                print(f"TRANSPORT original_bytes={len(original)} plain_base64_characters={len(base64.b64encode(original))} "
                      f"complete_input_characters={len(m.canonical(payload['inputs']))}")
                # The unchanged first checkout decodes outer Base64 JSON and persists it.
                envelope = json.loads(base64.b64decode(payload["inputs"]["attestation"], validate=True))
                worker_file = self.directory / "worker-attestation.json"
                m.write_json(worker_file, envelope)
                self.assertEqual(worker_file.stat().st_mode & 0o777, 0o600)
                decoded = m.decode_attestation(m.read_attestation(worker_file))
                self.assertEqual(decoded, document)
                self.assertEqual(decoded["external_obligations"]["sample:fixture-external"]["status"], "not-run")
                # The pinned second checkout validates the entire original document.
                m.publish_remote(github, candidate / "prepared", m.read_attestation(worker_file), root)
                return None
            if path == "actions/runs/456":
                return run
            if path == "releases/tags/v1.2.3":
                return copy.deepcopy(github.release)
            return original_api(path, method, payload, missing)
        github.api = api
        argv = ["release-publication", "--state-dir", str(state_dir), "publish", "--candidate", identity["candidate_id"]]
        with patch.object(sys, "argv", argv), patch.object(m, "GitHub", return_value=github), patch.object(m, "discover", return_value=run):
            self.assertEqual(m.main(), 0)
            self.assertEqual(m.main(), 0)
        retained = json.loads((candidate / "publication.json").read_text())
        self.assertEqual(retained["attestation_sha256"], m.identity_hash(document))
        self.assertEqual(retained["request"], dispatched[0]["request"])
        self.assertEqual(len(dispatched), 1)
        self.assertEqual(retained["status"], "published")

    def test_plain_attestation_inputs_keep_working(self):
        encoded = base64.b64encode(m.canonical(self.document).encode()).decode()
        path = self.directory / "legacy-attestation.json"
        m.write_json(path, json.loads(base64.b64decode(encoded, validate=True)))
        self.assertEqual(m.decode_attestation(m.read_attestation(path)), self.document)
        self.assertTrue(m.publish_remote(self.github, self.prepared, m.read_attestation(path), self.frozen)["immutable"])

    def test_compressed_envelope_refuses_malformed_truncated_oversized_and_bomb_before_mutation(self):
        valid = json.loads(base64.b64decode(m.encode_attestation(self.document), validate=True))
        compressed = base64.b64decode(valid["payload"], validate=True)
        invalids = []
        for changes in ({"transport": "unknown"}, {"extra": True}, {"payload": "!invalid"},
                        {"uncompressed_bytes": True}, {"uncompressed_bytes": m.ATTESTATION_BYTE_LIMIT + 1},
                        {"uncompressed_bytes": valid["uncompressed_bytes"] - 1}, {"sha256": "f" * 64},
                        {"payload": base64.b64encode(compressed[:-1]).decode()},
                        {"payload": base64.b64encode(compressed + b"trailing").decode()},
                        {"payload": base64.b64encode(compressed + compressed).decode()},
                        {"payload": "A" * m.DISPATCH_INPUT_LIMIT},
                        {"payload": base64.b64encode(zlib.compress(b"x" * (m.ATTESTATION_BYTE_LIMIT + 1))).decode()}):
            invalids.append({**valid, **changes})
        for invalid in invalids:
            with self.subTest(changes=set(invalid)), self.assertRaises(ValueError):
                m.publish_remote(self.github, self.prepared, invalid, self.frozen)
            self.assertEqual(self.github.calls, [])
            self.assertIsNone(self.github.ref)
        for document in ({"padding": "x" * m.ATTESTATION_BYTE_LIMIT}, [],
                         {"transport": m.ATTESTATION_TRANSPORT}):
            with self.assertRaises(ValueError):
                m.decode_attestation(document)
        with self.assertRaises(ValueError):
            m.encode_attestation({"padding": "x" * m.ATTESTATION_BYTE_LIMIT})
        path = self.directory / "oversized-attestation.json"
        path.write_bytes(b" " * (m.ATTESTATION_BYTE_LIMIT + 1))
        with self.assertRaises(ValueError):
            m.read_attestation(path)

    def test_compressed_attestation_preserves_original_validation_before_mutation(self):
        for change in ("candidate", "inventory", "status", "identity"):
            document = copy.deepcopy(self.document)
            if change == "candidate":
                document["candidate_id"] = "r124-a1"
            elif change == "inventory":
                del document["checks"]["verify"]
            elif change == "status":
                document["checks"]["verify"]["status"] = "failed"
            else:
                document["identity_sha256"] = "d" * 64
            envelope = json.loads(base64.b64decode(m.encode_attestation(document), validate=True))
            with self.subTest(change=change), self.assertRaises(ValueError):
                m.publish_remote(self.github, self.prepared, envelope, self.frozen)
            self.assertFalse(any(method != "GET" for _, method in self.github.calls))
            self.assertIsNone(self.github.ref)

    def operator_failure(self, name="profile:sample", lanes=()):
        source_output = self.directory / "original-failure"
        _, prepared, identity = fixture(source_output, lanes)
        identity["candidate_id"] = "r122-a1"
        m.write_json(prepared / "identity.json", identity)
        original_output, original_root, _ = accepted(source_output, prepared, identity)
        _, original = m.a.load_bound(original_output)
        original["checks"][name] = {"status": "pending"}
        m.a.save(original_output, original)
        _, target = m.a.load_bound(self.output)
        target["checks"][name] = {"status": "pending"}
        m.a.save(self.output, target)
        command = m.a.command_identity(m.a.commands(original_root, identity["version"], original["profiles"])[name], original_root)
        argv = ["timeout", "--signal=TERM", "--kill-after=300s", "90m", "env", "TMPDIR=/tmp",
                "SUBYARD_E2E_CONTROLLER_WORKSPACE=" + str(self.directory / "controller"),
                "SUBYARD_E2E_CANDIDATE_BUNDLE=" + str(original_output / "candidate-bundle.tar.gz"),
                "SUBYARD_E2E_CANDIDATE_SHA256=" + original["candidate_bundle_sha256"], *command, "--slot", "2"]
        launch = {"argv": argv, "cwd": str(original_root), "started_at": "2026-10-12T00:00:00+00:00", "started_monotonic": 1000.0}
        timing = {**launch, "ended_at": "2026-10-12T00:02:03.500000+00:00", "ended_monotonic": 1123.5,
                  "whole_duration_seconds": 123.5, "exit_code": 1}
        native = {"exit_code": 1, "chunk_id": "fixture", "wall_time_seconds": 0.1, "output": "native failure"}
        files = {role: self.directory / ("failure-" + role + ".json") for role in ("native", "launch", "timing")}
        for role, data in (("native", native), ("launch", launch), ("timing", timing)):
            m.write_json(files[role], data)
        files["log"] = self.directory / "failure.log"
        files["log"].write_text("E2E lease: slot=2 run=fixture type=subyard-pair vms=2 base=" + "b" * 64 + "\n"
            "  [fail] phase=fixture/init\n"
            "E2E_PHASE phase=guest-cleanup state=end duration_seconds=1 exit_code=0 vm=1\n"
            "E2E_PHASE phase=guest-cleanup state=end duration_seconds=1 exit_code=0 vm=2\n"
            "E2E lease released: slot=2 run=fixture\n"
            "E2E_PHASE phase=cleanup/release state=end duration_seconds=12 exit_code=0\n")
        declaration = {"schema_version": 1, "check": name, "target": self.identity, "tested": m.a.transfer_candidate(original),
            "command": command, "decision": {"reason": "Accept this known failure for this exact release",
                "authorization_reference": "operator-decision:fixture", "repair_reference": "p0-repair:fixture"},
            "source": {"output": str(original_output), "receipt_sha256": m.a.digest(original_output / "receipt.json")},
            "artifacts": {role: {"path": str(path), "sha256": m.a.digest(path)} for role, path in files.items()}}
        path = self.directory / "operator-acceptance.json"
        m.write_json(path, declaration)
        return path, files, original_output

    def unrun_profile(self, name="profile:sample"):
        _, receipt = m.a.load_bound(self.output)
        # Construct unrun fixture state; no real execution evidence is erased.
        (self.output / receipt["checks"][name]["log"]).unlink()
        receipt["checks"][name] = {"status": "pending"}
        m.a.save(self.output, receipt)
        return receipt

    def profile_deferral_attestation(self, names=("profile:sample",), paths=()):
        return m.attestation(self.output, self.identity, paths, names, "Known profile issue; postpone this check", "operator:fixture")

    def test_profile_deferral_cli_publication_preserves_receipt_and_exact_retry(self):
        self.unrun_profile()
        state_dir = self.directory / "publication"
        candidate = state_dir / self.identity["candidate_id"]
        candidate.mkdir(parents=True)
        shutil.copytree(self.prepared, candidate / "prepared")
        shutil.copytree(self.output, candidate / "acceptance")
        self.output = candidate / "acceptance"
        root = self.output / "source"
        before = (self.output / "receipt.json").read_bytes()
        document = self.profile_deferral_attestation()
        result = document["checks"]["profile:sample"]
        self.assertEqual((result["status"], result["execution"]), ("not-run", "operator-deferred"))
        self.assertEqual(set(result), {"status", "execution", "profile_deferral"})
        decision = result["profile_deferral"]
        self.assertEqual((decision["target"], decision["original_status"], decision["receipt_sha256"]),
                         (self.identity, "pending", m.a.digest(self.output / "receipt.json")))
        self.assertEqual(decision["command"], ["bash", "config/profiles/sample/tests/e2e/acceptance.sh"])
        self.assertNotIn(str(self.directory), json.dumps(document))
        dispatched = []
        run = {"id": 456, "status": "completed", "conclusion": "success", "run_attempt": 1, "html_url": "https://fixture/run"}
        original_api = self.github.api
        def api(path, method="GET", payload=None, missing=False):
            if path.endswith("/dispatches"):
                dispatched.append(copy.deepcopy(payload["inputs"]))
                envelope = json.loads(base64.b64decode(payload["inputs"]["attestation"], validate=True))
                m.publish_remote(self.github, candidate / "prepared", envelope, root)
                return None
            if path == "actions/runs/456":
                return run
            if path == "releases/tags/v1.2.3":
                return copy.deepcopy(self.github.release)
            return original_api(path, method, payload, missing)
        self.github.api = api
        argv = ["release-publication", "--state-dir", str(state_dir), "publish", "--candidate", self.identity["candidate_id"],
                "--defer-profile", "profile:sample", "--deferral-reason", "Known profile issue; postpone this check",
                "--deferral-authorization-reference", "operator:fixture"]
        with patch.object(sys, "argv", argv), patch.object(m, "GitHub", return_value=self.github), patch.object(m, "discover", return_value=run):
            self.assertEqual(m.main(), 0)
            self.assertEqual(m.main(), 0)
            retained = (candidate / "publication.json").read_bytes()
            attestation_before = (candidate / "attestation.json").read_bytes()
            changed = argv.copy()
            changed[changed.index("--deferral-reason") + 1] = "Different decision"
            with patch.object(sys, "argv", changed), self.assertRaisesRegex(ValueError, "attestation changed"):
                m.main()
            self.assertEqual((candidate / "publication.json").read_bytes(), retained)
            self.assertEqual((candidate / "attestation.json").read_bytes(), attestation_before)
        self.assertEqual(len(dispatched), 1)
        self.assertEqual(before, (self.output / "receipt.json").read_bytes())
        self.assertEqual(json.loads(before)["result"], "incomplete")
        with self.assertRaisesRegex(ValueError, "complete passing"):
            m.attestation(self.output, self.identity)

    def test_profile_deferral_refuses_attempts_and_other_gates_before_mutation(self):
        receipt = self.unrun_profile()
        document = self.profile_deferral_attestation()
        for names in (("profile:unknown",), ("verify",), ("package",), ("p0-release-smoke",), ("profile:sample", "profile:sample")):
            with self.subTest(names=names), self.assertRaises(ValueError):
                self.profile_deferral_attestation(names)
        with self.assertRaises(ValueError):
            m.attestation(self.output, self.identity, (), ["profile:sample"], "reason", " ")
        for status in ("failed", "interrupted", "blocked", "passed", "not-applicable"):
            invalid = copy.deepcopy(receipt)
            invalid["checks"]["profile:sample"] = {"status": status}
            m.a.save(self.output, invalid)
            with self.subTest(status=status), self.assertRaises(ValueError):
                self.profile_deferral_attestation()
        m.a.save(self.output, receipt)
        for mutate in (lambda d: d["checks"]["profile:sample"]["profile_deferral"]["target"].update(candidate_id="r124-a1"),
                       lambda d: d["checks"]["profile:sample"]["profile_deferral"].update(command=["true"]),
                       lambda d: d["checks"]["profile:sample"]["profile_deferral"].update(receipt_sha256="d" * 64),
                       lambda d: d["checks"]["profile:sample"].update(status="passed"),
                       lambda d: d["checks"]["verify"].update(status="failed"),
                       lambda d: d["checks"]["p0-release-smoke"].update(status="pending"),
                       lambda d: d.update(profile_deferrals=[]), lambda d: d.update(schema_version=3)):
            invalid = copy.deepcopy(document)
            mutate(invalid)
            with self.assertRaises(ValueError):
                m.publish_remote(self.github, self.prepared, invalid, self.frozen)
            self.assertIsNone(self.github.ref)
            self.assertFalse(any(method != "GET" for _, method in self.github.calls))
        log = self.output / "profile-sample-12345678.log"
        log.write_text("original execution evidence")
        with self.assertRaisesRegex(ValueError, "execution logs"):
            self.profile_deferral_attestation()
        log.unlink()
        m.a.event(self.output, "check-start", check="profile:sample", slot=1)
        with self.assertRaisesRegex(ValueError, "previously attempted"):
            self.profile_deferral_attestation()

    def test_profile_deferral_coexists_with_transfers_failures_and_external_deferrals(self):
        mixed = self.directory / "mixed"
        lanes = ("one", "two", "three")
        self.root, self.prepared, self.identity = fixture(mixed, lanes)
        self.output, self.frozen, _ = accepted(mixed, self.prepared, self.identity)
        self.github = FakeGitHub(self.identity)
        failure, _, original_output = self.operator_failure("profile:sample/one", lanes)
        self.unrun_profile("profile:sample/three")
        original_root, original = m.a.load_bound(original_output)
        original["checks"]["profile:sample/two"] = {"status": "pending"}
        m.a.save(original_output, original)
        _, original = capture_attempts(original_output, ["local-adapters"])
        # The operator declaration binds this synthetic source receipt's final evidence inventory.
        decision = json.loads(failure.read_text())
        decision["source"]["receipt_sha256"] = m.a.digest(original_output / "receipt.json")
        m.write_json(failure, decision)
        _, receipt = m.a.load_bound(self.output)
        for name in ("verify", "local-adapters", "profile:sample/two"):
            receipt["checks"][name] = {"status": "pending"}
        external = receipt["external_obligations"]["sample:fixture-external"]
        external["status"] = "not-run"
        del external["evidence_sha256"]
        m.a.save(self.output, receipt)
        target_manifest = m.a.candidate_manifest(self.frozen, receipt)
        source_manifest = m.a.candidate_manifest(original_root, original)
        review_text = {"unchanged_scope": "Exact synthetic fixture behavior", "reason": "Reviewed complete candidate delta",
                       "authorization_reference": "operator:batch-fixture"}
        native, launch, log = (self.directory / ("standalone-" + role) for role in ("native", "launch", "log"))
        native.write_text(json.dumps({"exit_code": 0, "chunk_id": "original", "output": "", "wall_time_seconds": 1}))
        command = m.a.commands(original_root, self.identity["version"], original["profiles"])["profile:sample/two"]
        launch.write_text(json.dumps({"cwd": str(original_root), "source_fingerprint": original["source_fingerprint"],
            "runtime_sha256": original["runtime_sha256"], "bundle_sha256": original["candidate_bundle_sha256"],
            "acceptance_script_sha256": m.a.digest(original_root / command[1]),
            "argv": ["timeout", "--signal=TERM", "--kill-after=300s", "90m", "env", "TMPDIR=/tmp",
                "SUBYARD_E2E_CONTROLLER_WORKSPACE=/fixture/controller",
                "SUBYARD_E2E_CANDIDATE_BUNDLE=" + str(original_output / "candidate-bundle.tar.gz"),
                "SUBYARD_E2E_CANDIDATE_SHA256=" + original["candidate_bundle_sha256"], *command, "--slot", "2"]}))
        log.write_text("E2E lease: slot=2 run=fixture type=subyard-pair vms=2 base=" + "b" * 64 + "\n"
            "E2E_PHASE phase=guest-cleanup state=end duration_seconds=1 exit_code=0 vm=1\n"
            "E2E_PHASE phase=guest-cleanup state=end duration_seconds=1 exit_code=0 vm=2\n"
            "E2E_PHASE phase=cleanup/release state=end duration_seconds=1 exit_code=0\n"
            "E2E lease released: slot=2 run=fixture\n")
        standalone = {role: {"path": str(path), "sha256": m.a.digest(path)} for role, path in
                      (("native", native), ("launch", launch), ("log", log))}
        request = {"schema_version": 1, "target": m.a.transfer_candidate(receipt), "checks": []}
        for name in ("verify", "local-adapters", "profile:sample/two"):
            reference = original["checks"][name].get("original_attempt")
            if reference:
                review = {"schema_version": 2, "check": name, "tested_manifest_sha256": m.a.evidence_hash(source_manifest),
                    "target_manifest_sha256": m.a.evidence_hash(target_manifest), "original_attempt": reference,
                    "review": review_text, "standalone": None}
            else:
                review = {"schema_version": 1, "check": name, "tested": m.a.transfer_candidate(original),
                    "target": m.a.transfer_candidate(receipt), "source_receipt_sha256": m.a.digest(original_output / "receipt.json"),
                    "review": review_text, "standalone": standalone if name == "profile:sample/two" else None}
            path = self.directory / (name.replace(":", "-").replace("/", "-") + "-batch-review.json")
            m.write_json(path, review)
            request["checks"].append({"check": name, "source": str(original_output), "attempt": reference,
                                      "review": {"path": str(path), "sha256": m.a.digest(path)}})
        request_path = self.directory / "batch-request.json"
        m.write_json(request_path, request)
        with patch.object(m.a, "source_entries", wraps=m.a.source_entries) as scans, patch.object(m.a, "save", wraps=m.a.save) as saved:
            self.assertEqual(m.a.transfer_batch(self.output, request_path), 0)
        self.assertEqual((scans.call_count, saved.call_count), (2, 1))
        m.a.load_bound(self.output)
        m.a.defer_external(self.output, ["sample:fixture-external"], "External account unavailable", "operator:fixture")
        before = (self.output / "receipt.json").read_bytes()
        document = self.profile_deferral_attestation(["profile:sample/three"], [failure])
        self.assertEqual(document["schema_version"], 5)
        self.assertEqual((document["reviewed_transfers"], document["operator_acceptances"], document["profile_deferrals"]),
                         (["local-adapters", "profile:sample/two", "verify"], ["profile:sample/one"], ["profile:sample/three"]))
        self.assertEqual(document["checks"]["profile:sample/two"]["evidence_transfer"]["origin"], "standalone")
        self.assertEqual(document["checks"]["profile:sample/one"]["status"], "failed")
        self.assertEqual(document["external_obligations"]["sample:fixture-external"]["status"], "not-run")
        encoded = json.loads(base64.b64decode(m.encode_attestation(document)))
        self.assertTrue(m.publish_remote(self.github, self.prepared, encoded, self.frozen)["immutable"])
        print(f"MEASURE batch_checks=3 unique_scans=2 attestation_bytes={len(m.canonical(document).encode())}")
        self.assertEqual(before, (self.output / "receipt.json").read_bytes())

    def test_explicit_profile_failure_decision_preserves_native_failure_and_receipts(self):
        path, files, original_output = self.operator_failure()
        before = [(output / "receipt.json").read_bytes() for output in (self.output, original_output)]
        document = m.attestation(self.output, self.identity, [path])
        self.assertEqual(document["schema_version"], 3)
        self.assertEqual(document["operator_acceptances"], ["profile:sample"])
        self.assertEqual(document["reviewed_transfers"], [])
        result = document["checks"]["profile:sample"]
        self.assertEqual((result["status"], result["exit_code"], result["execution"]), ("failed", 1, "operator-accepted"))
        decision = result["operator_acceptance"]
        self.assertEqual(decision["duration_seconds"], 123.5)
        self.assertEqual(decision["native_status"], "failed")
        self.assertEqual(decision["target"], self.identity)
        self.assertEqual(decision["source_status"], "pending")
        self.assertEqual(decision["target_status"], "pending")
        self.assertEqual(decision["artifacts"]["native"], m.a.digest(files["native"]))
        self.assertNotIn(str(self.directory), json.dumps(document))
        envelope = json.loads(base64.b64decode(m.encode_attestation(document), validate=True))
        self.assertEqual(m.decode_attestation(envelope), document)
        self.assertTrue(m.publish_remote(self.github, self.prepared, envelope, self.frozen)["immutable"])
        self.assertEqual(before, [(output / "receipt.json").read_bytes() for output in (self.output, original_output)])
        with self.assertRaisesRegex(ValueError, "complete passing"):
            m.attestation(self.output, self.identity)

    def test_operator_declaration_unknown_core_wrong_candidate_version_command_and_tamper_refuse(self):
        path, files, _ = self.operator_failure()
        declaration = json.loads(path.read_text())
        for change in ("unknown", "core", "candidate", "version", "typed-target", "tested", "command", "authorization", "repair", "artifact", "extra"):
            invalid = copy.deepcopy(declaration)
            if change == "unknown":
                invalid["check"] = "profile:unknown"
            elif change == "core":
                invalid["check"] = "verify"
            elif change == "candidate":
                invalid["target"]["candidate_id"] = "r124-a1"
            elif change == "version":
                invalid["target"]["version"] = "1.2.4"
            elif change == "typed-target":
                invalid["target"]["schema_version"] = True
            elif change == "tested":
                invalid["tested"]["runtime_sha256"] = "f" * 64
            elif change == "command":
                invalid["command"] = ["true"]
            elif change in ("authorization", "repair"):
                invalid["decision"][change + "_reference"] = ""
            elif change == "artifact":
                invalid["artifacts"]["log"]["sha256"] = "f" * 64
            else:
                invalid["automatic"] = True
            m.write_json(path, invalid)
            with self.subTest(change=change), self.assertRaises(ValueError):
                m.attestation(self.output, self.identity, [path])
        m.write_json(path, declaration)
        files["log"].write_text("tampered original failure\n")
        with self.assertRaisesRegex(ValueError, "artifact changed"):
            m.attestation(self.output, self.identity, [path])

    def test_operator_acceptance_nonterminal_pass_signal_bad_timing_and_cleanup_refuse(self):
        path, files, _ = self.operator_failure()
        declaration = json.loads(path.read_text())
        originals = {role: file.read_bytes() for role, file in files.items()}
        for change in ("nonterminal", "pass", "signal", "timing", "transport", "cleanup", "release"):
            for role, content in originals.items():
                files[role].write_bytes(content)
            if change in ("nonterminal", "pass", "signal"):
                data = json.loads(files["native"].read_text())
                data.update({"session_id": 99} if change == "nonterminal" else {"exit_code": 0 if change == "pass" else 143})
                m.write_json(files["native"], data)
            elif change in ("timing", "transport"):
                role = "timing" if change == "timing" else "launch"
                data = json.loads(files[role].read_text())
                if role == "timing":
                    data["whole_duration_seconds"] = 0.1
                else:
                    data["argv"][8] = "SUBYARD_E2E_CANDIDATE_SHA256=" + "f" * 64
                m.write_json(files[role], data)
            else:
                content = originals["log"].decode().replace("exit_code=0 vm=2", "exit_code=1 vm=2") if change == "cleanup" else originals["log"].decode().replace("E2E lease released:", "missing release:")
                files["log"].write_text(content)
            declaration["artifacts"] = {role: {"path": str(file), "sha256": m.a.digest(file)} for role, file in files.items()}
            m.write_json(path, declaration)
            with self.subTest(change=change), self.assertRaises(ValueError):
                m.attestation(self.output, self.identity, [path])

    def test_operator_acceptance_cannot_waive_other_gates_or_remote_binding(self):
        path, _, _ = self.operator_failure()
        document = m.attestation(self.output, self.identity, [path])
        _, receipt = m.a.load_bound(self.output)
        for name in ("verify", "p0-release-smoke"):
            invalid = copy.deepcopy(receipt)
            invalid["checks"][name]["status"] = "failed"
            m.a.save(self.output, invalid)
            with self.subTest(other_gate=name), self.assertRaises(ValueError):
                m.attestation(self.output, self.identity, [path])
        m.a.save(self.output, receipt)
        for change in ("candidate", "version", "typed-target", "inventory", "core", "pass", "zero", "inherited", "cleanup", "repair", "downgrade", "other-gate"):
            invalid = copy.deepcopy(document)
            result = invalid["checks"]["profile:sample"]
            decision = result["operator_acceptance"]
            if change in ("candidate", "version"):
                decision["target"]["candidate_id" if change == "candidate" else "version"] = "r124-a1" if change == "candidate" else "1.2.4"
            elif change == "typed-target":
                decision["target"]["schema_version"] = True
            elif change == "inventory":
                invalid["operator_acceptances"] = []
            elif change == "core":
                invalid["checks"]["verify"] = result
                del invalid["checks"]["profile:sample"]
            elif change == "pass":
                result["status"] = "passed"
            elif change == "zero":
                result["exit_code"] = decision["exit_code"] = 0
            elif change == "inherited":
                result["execution"] = "inherited"
            elif change == "cleanup":
                decision["cleanup"]["release_exit_code"] = 1
            elif change == "repair":
                decision["decision"]["repair_reference"] = ""
            elif change == "downgrade":
                invalid["schema_version"] = 2
                del invalid["operator_acceptances"]
            else:
                invalid["checks"]["verify"]["status"] = "failed"
            envelope = json.loads(base64.b64decode(m.encode_attestation(invalid), validate=True))
            with self.subTest(change=change), self.assertRaises(ValueError):
                m.publish_remote(self.github, self.prepared, envelope, self.frozen)
            self.assertIsNone(self.github.ref)
            self.assertFalse(any(method != "GET" for _, method in self.github.calls))

    def deferred(self):
        receipt = json.loads((self.output / "receipt.json").read_text())
        receipt["external_obligations"]["sample:fixture-external"].update({"status": "not-run"})
        del receipt["external_obligations"]["sample:fixture-external"]["evidence_sha256"]
        m.a.save(self.output, receipt)
        with self.assertRaisesRegex(ValueError, "actual evidence SHA-256 or authorized deferral"):
            m.attestation(self.output, self.identity)
        m.a.defer_external(self.output, ["sample:fixture-external"],
            "External account unavailable for this release", "operator-authorization-1")
        self.document = m.attestation(self.output, self.identity)
        return json.loads((self.output / "receipt.json").read_text())

    def test_authorized_deferral_publishes_without_claiming_external_pass(self):
        receipt = self.deferred()
        self.assertEqual(receipt["reproducible_result"], "passed")
        self.assertEqual(receipt["result"], "incomplete")
        with patch.object(m.a, "execution_environment", return_value=receipt["environment"]):
            self.assertEqual(m.a.run_checks(self.output, [], [], False), 1)
        self.document = m.attestation(self.output, self.identity)
        self.assertEqual(self.document["receipt_sha256"], m.a.digest(self.output / "receipt.json"))
        external = self.document["external_obligations"]["sample:fixture-external"]
        self.assertEqual(external["status"], "not-run")
        self.assertEqual(external["deferral"]["candidate_sha256"], m.identity_hash(self.identity))
        self.assertEqual(external["deferral"]["authorization_reference"], "operator-authorization-1")
        self.assertNotIn("evidence_sha256", external)
        self.assertTrue(self.publish()["immutable"])

    def test_deferral_attestation_rejects_missing_authorization_unknown_ids_and_wrong_candidate(self):
        self.deferred()
        for change in ("no-deferral", "missing-reason", "missing-authorization", "blank-authorization",
                       "unknown", "failed", "passed", "false-evidence", "candidate", "extra"):
            with self.subTest(change=change):
                document = copy.deepcopy(self.document)
                external = document["external_obligations"]["sample:fixture-external"]
                if change == "no-deferral":
                    del external["deferral"]
                elif change == "missing-reason":
                    del external["deferral"]["reason"]
                elif change == "missing-authorization":
                    del external["deferral"]["authorization_reference"]
                elif change == "blank-authorization":
                    external["deferral"]["authorization_reference"] = " "
                elif change == "unknown":
                    document["external_obligations"]["unknown"] = external.copy()
                elif change in ("failed", "passed"):
                    external["status"] = change
                elif change == "false-evidence":
                    external["evidence_sha256"] = "c" * 64
                elif change == "candidate":
                    external["deferral"]["candidate_sha256"] = "d" * 64
                else:
                    external["deferral"]["override"] = True
                with self.assertRaises(ValueError):
                    m.publish_remote(self.github, self.prepared, document, self.frozen)
                self.assertIsNone(self.github.ref)
        other = copy.deepcopy(self.identity)
        other["candidate_id"] = "r124-a1"
        document = copy.deepcopy(self.document)
        document.update(candidate_id=other["candidate_id"], identity_sha256=m.identity_hash(other))
        with self.assertRaisesRegex(ValueError, "candidate binding"):
            m.validate_attestation(document, other, self.frozen)

    def test_deferral_cannot_waive_local_physical_or_superseded_acceptance(self):
        original = self.deferred()
        for name in ("verify", "p0-release-smoke", "profile:sample"):
            for status in ("pending", "failed"):
                with self.subTest(check=name, status=status):
                    receipt = copy.deepcopy(original)
                    receipt["checks"][name]["status"] = status
                    m.a.save(self.output, receipt)
                    with self.assertRaisesRegex(ValueError, "complete passing"):
                        m.attestation(self.output, self.identity)
                    document = copy.deepcopy(self.document)
                    document["checks"][name]["status"] = status
                    with self.assertRaises(ValueError):
                        m.publish_remote(self.github, self.prepared, document, self.frozen)
                    self.assertIsNone(self.github.ref)
        receipt = copy.deepcopy(original)
        receipt["superseded"] = True
        m.a.save(self.output, receipt)
        with self.assertRaisesRegex(ValueError, "complete passing"):
            m.attestation(self.output, self.identity)
        for name in ("verify", "p0-release-smoke"):
            receipt = copy.deepcopy(original)
            receipt["checks"][name]["command"] = ["true"]
            m.a.save(self.output, receipt)
            with self.assertRaisesRegex(ValueError, "command provenance changed"):
                m.attestation(self.output, self.identity)
        m.a.save(self.output, original)
        asset = self.frozen / ".subyard-acceptance/release" / original["runtime_file"]
        asset.write_text("changed release bytes")
        with self.assertRaisesRegex(ValueError, "release assets changed"):
            m.attestation(self.output, self.identity)

    def test_lost_responses_resume_ref_draft_partial_upload_and_published(self):
        for point in ("ref", "draft", "upload", "published"):
            with self.subTest(point=point):
                self.github = FakeGitHub(self.identity)
                self.github.fail = point
                with self.assertRaisesRegex(ValueError, "lost response"):
                    self.publish()
                self.assertTrue(self.publish()["immutable"])
                self.assertEqual(len(self.github.assets), len(self.identity["release_files"]))

    def test_failed_preparation_and_missing_or_extra_evidence_never_create_ref(self):
        self.github.successful_run = False
        with self.assertRaisesRegex(ValueError, "did not pass"):
            self.publish()
        self.assertIsNone(self.github.ref)
        self.github.successful_run = True
        self.github.complete_jobs = False
        with self.assertRaisesRegex(ValueError, "job inventory"):
            self.publish()
        self.assertIsNone(self.github.ref)
        self.github.complete_jobs = True
        for change in ("missing", "extra", "failed", "external", "environment", "identity"):
            with self.subTest(change=change):
                document = copy.deepcopy(self.document)
                if change == "missing":
                    del document["checks"]["p0-release-smoke"]
                elif change == "extra":
                    document["checks"]["unknown"] = {"status": "passed"}
                elif change == "failed":
                    document["checks"]["p0-release-smoke"]["status"] = "failed"
                elif change == "external":
                    document["external_obligations"] = {}
                elif change == "environment":
                    document["checks"]["p0-release-smoke"]["environments"] = []
                else:
                    document["identity_sha256"] = "d" * 64
                with self.assertRaises(ValueError):
                    m.publish_remote(self.github, self.prepared, document, self.frozen)
                self.assertIsNone(self.github.ref)

    def test_local_incomplete_acceptance_and_changed_logs_reject(self):
        receipt = json.loads((self.output / "receipt.json").read_text())
        receipt["checks"]["p0-release-smoke"]["status"] = "pending"
        m.a.save(self.output, receipt)
        with self.assertRaisesRegex(ValueError, "complete passing"):
            m.attestation(self.output, self.identity)
        receipt["checks"]["p0-release-smoke"]["status"] = "passed"
        m.a.save(self.output, receipt)
        (self.output / receipt["checks"]["verify"]["log"]).write_text("altered evidence")
        with self.assertRaisesRegex(ValueError, "evidence is missing or changed"):
            m.attestation(self.output, self.identity)

    def test_foreign_tags_drafts_published_releases_and_different_candidate_refuse(self):
        self.github.ref = {"object": {"type": "commit", "sha": self.identity["commit"]}}
        with self.assertRaisesRegex(ValueError, "occupied"):
            self.publish()
        self.assertIsNone(self.github.release)
        self.github = FakeGitHub(self.identity)
        self.github.release = {"tag_name": "v1.2.3", "body": "foreign", "draft": True, "prerelease": False}
        with self.assertRaisesRegex(ValueError, "foreign"):
            self.publish()
        self.assertIsNone(self.github.ref)
        self.github.release["draft"] = False
        with self.assertRaisesRegex(ValueError, "foreign"):
            self.publish()
        self.github = FakeGitHub(self.identity)
        self.github.fail = "ref"
        with self.assertRaises(ValueError):
            self.publish()
        self.github.tag["message"] = "another candidate"
        with self.assertRaisesRegex(ValueError, "another candidate"):
            self.publish()

    def test_starter_upload_can_resume_only_on_owned_unpublished_draft(self):
        self.github.fail = "draft"
        with self.assertRaises(ValueError):
            self.publish()
        name = next(iter(self.identity["release_files"]))
        self.github.assets = [{"id": 99, "name": name, "state": "starter", "size": 0}]
        self.assertTrue(self.publish()["immutable"])
        self.assertIn(("releases/assets/99", "DELETE"), self.github.calls)
        self.github.assets[0].update({"state": "starter", "size": 0})
        with self.assertRaisesRegex(ValueError, "contradictory"):
            self.publish()

    def test_mismatching_bytes_extra_assets_and_mutable_release_never_report_success(self):
        self.github.fail = "upload"
        with self.assertRaises(ValueError):
            self.publish()
        self.github.data[1] = b"wrong bytes"
        with self.assertRaisesRegex(ValueError, "hash mismatch"):
            self.publish()
        self.assertTrue(self.github.release["draft"])
        self.github.assets.append({"name": "foreign", "id": 999})
        with self.assertRaisesRegex(ValueError, "foreign"):
            self.publish()
        self.github = FakeGitHub(self.identity)
        self.github.immutable = False
        with self.assertRaisesRegex(ValueError, "not immutable"):
            self.publish()
        self.assertFalse(self.github.release["draft"])

    def test_provenance_checksum_inventory_and_candidate_attempt_binding(self):
        self.assertEqual(m.checked_identity(self.prepared), self.identity)
        path = next(path for path in (self.prepared / "assets").iterdir() if path.name.endswith(".provenance.json"))
        original = path.read_text()
        value = json.loads(original)
        value["sourceRevision"] = "f" * 40
        path.write_text(json.dumps(value))
        changed = copy.deepcopy(self.identity)
        changed["release_files"][path.name] = m.a.digest(path)
        m.write_json(self.prepared / "identity.json", changed)
        with self.assertRaisesRegex(ValueError, "provenance"):
            m.checked_identity(self.prepared)
        path.write_text(original)
        m.write_json(self.prepared / "identity.json", self.identity)
        (self.prepared / "assets/extra").write_text("unrecognized")
        with self.assertRaisesRegex(ValueError, "inventory"):
            m.checked_identity(self.prepared)
        for invalid in ("r123", "r123-a0", "../r123-a1"):
            with self.assertRaises(ValueError):
                m.candidate_parts(invalid)

    def test_workflow_contract_no_tag_trigger_pinned_checks_serialized_publication(self):
        prep = (ROOT / ".github/workflows/release.yml").read_text()
        publish = (ROOT / ".github/workflows/release-publish.yml").read_text()
        self.assertNotIn("  push:", prep)
        self.assertNotIn("git/refs", prep)
        self.assertIn("contents: read", prep)
        self.assertEqual(prep.count("uses: actions/checkout@"), prep.count("ref: ${{ inputs.commit }}"))
        for gate in ("native-engine-arm64", "paseo-headless", "make verify", "make cli-docs-check",
                     "dev/test-profiles.sh --e2e --list", "tests/real-host/adapter-contracts.sh", "dev/verify-release-upgrades.py"):
            self.assertIn(gate, prep)
        self.assertIn("release-candidate-${{ github.run_attempt }}", prep)
        self.assertIn("group: subyard-release-publication-stable", publish)
        self.assertIn("cancel-in-progress: false", publish)
        self.assertIn("queue: max", publish)
        self.assertNotIn("  push:", publish)
        self.assertIn("ref: ${{ steps.candidate.outputs.commit }}", publish)
        install = "sudo apt-get install -y ripgrep"
        self.assertIn(install, publish)
        self.assertLess(publish.index(install), publish.index("dev/release-publication.py publish-ci"))

    def test_two_operator_candidates_same_version_cannot_adopt_or_overwrite(self):
        self.github.fail = "ref"
        with self.assertRaises(ValueError):
            self.publish()
        first_ref = copy.deepcopy(self.github.ref)
        second = self.directory / "second-prepared"
        shutil.copytree(self.prepared, second)
        identity = copy.deepcopy(self.identity)
        identity["candidate_id"] = "r124-a1"
        m.write_json(second / "identity.json", identity)
        document = copy.deepcopy(self.document)
        document["candidate_id"] = identity["candidate_id"]
        document["identity_sha256"] = m.identity_hash(identity)
        with self.assertRaisesRegex(ValueError, "another candidate"):
            m.publish_remote(self.github, second, document, self.frozen)
        self.assertEqual(self.github.ref, first_ref)
        self.assertIsNone(self.github.release)
        self.assertTrue(self.publish()["immutable"])

    def test_native_retry_preserves_request_inputs_and_waits_for_new_attempt(self):
        path = self.directory / "retry.json"
        state = {"request": "fixture-request", "workflow_ref": "master", "dispatch_started": True}
        original = {"id": 456, "status": "completed", "conclusion": "failure", "run_attempt": 1,
                    "html_url": "https://fixture/run"}
        retried = {**original, "run_attempt": 2, "conclusion": "success"}
        class Retry:
            repository = "fixture/repository"
            calls = []
            def api(self, path, method="GET", payload=None):
                self.calls.append((path, method, payload))
                return retried
        github = Retry()
        with patch.object(m, "discover", return_value=original):
            actual = m.dispatch(github, "release-publish.yml", {"candidate": "r123-a1"}, state, path)
        self.assertEqual(actual["run_attempt"], 2)
        self.assertEqual(state["rerun_attempt"], 2)
        self.assertIn(("actions/runs/456/rerun", "POST", None), github.calls)
        self.assertFalse(any("dispatches" in path for path, _, _ in github.calls))
        self.assertEqual(json.loads(path.read_text())["status"], "passed")

    def test_dispatch_bounds_whole_inputs_including_candidate_and_request_before_intent(self):
        path = self.directory / "oversized-request.json"
        state = {"request": "fixture-request", "workflow_ref": "master"}
        inputs = {"candidate": "r123-a1", "attestation": ""}
        overhead = len(m.canonical({**inputs, "request": state["request"]}))
        inputs["attestation"] = "A" * (m.DISPATCH_INPUT_LIMIT - overhead)
        class Dispatch:
            calls = []
            def api(self, path, method="GET", payload=None):
                self.calls.append(payload)
                raise ValueError("fixture lost response")
        github = Dispatch()
        for field in ("candidate", "attestation", "request"):
            invalid = copy.deepcopy(inputs)
            attempted = copy.deepcopy(state)
            if field == "request":
                attempted[field] += "x"
            else:
                invalid[field] += "x"
            with self.subTest(field=field), self.assertRaisesRegex(ValueError, "65535-character"):
                m.dispatch(github, "release-publish.yml", invalid, attempted, path)
            self.assertEqual(github.calls, [])
            self.assertNotIn("dispatch_started", attempted)
            self.assertFalse(path.exists())
        with self.assertRaisesRegex(ValueError, "lost response"):
            m.dispatch(github, "release-publish.yml", inputs, state, path)
        self.assertEqual(len(m.canonical(github.calls[0]["inputs"])), m.DISPATCH_INPUT_LIMIT)
        self.assertTrue(json.loads(path.read_text())["dispatch_started"])

    def test_interrupted_watch_retains_request_and_terminal_status(self):
        path = self.directory / "interrupted.json"
        state = {"request": "fixture-request", "workflow_ref": "master", "dispatch_started": True}
        run = {"id": 456, "status": "in_progress", "run_attempt": 1, "html_url": "https://fixture/run"}
        class Watching:
            repository = "fixture/repository"
            def api(self, path, method="GET", payload=None):
                raise KeyboardInterrupt
        with patch.object(m, "discover", return_value=run):
            with self.assertRaises(KeyboardInterrupt):
                m.dispatch(Watching(), "release-publish.yml", {}, state, path)
        retained = json.loads(path.read_text())
        self.assertEqual(retained["status"], "interrupted")
        self.assertTrue(retained["dispatch_started"])
        self.assertEqual(retained["run_id"], 456)

    def test_dispatch_lost_response_persists_intent_and_does_not_duplicate(self):
        path = self.directory / "request.json"
        state = {"request": "fixture-request", "workflow_ref": "master"}
        inputs = {"candidate": self.identity["candidate_id"], "attestation": m.encode_attestation(self.document)}
        class Dispatch:
            def api(self, *args, **kwargs):
                raise ValueError("fixture lost response")
        with self.assertRaisesRegex(ValueError, "lost response"):
            m.dispatch(Dispatch(), "release-publish.yml", inputs, state, path)
        self.assertTrue(json.loads(path.read_text())["dispatch_started"])
        with patch.object(m, "discover", return_value=None), patch.object(m.time, "monotonic", side_effect=[0, 121]):
            with self.assertRaisesRegex(ValueError, "outcome unknown"):
                m.dispatch(Dispatch(), "release-publish.yml", inputs, state, path)

unittest.main()
PY
