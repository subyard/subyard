#!/usr/bin/env bash
# Host-free release orchestration, binding and recovery contracts.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PYTHONDONTWRITEBYTECODE=1 python3 -B - "$ROOT" <<'PY'
import copy
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(sys.argv[1])
sys.argv = [sys.argv[0]]
spec = importlib.util.spec_from_file_location("publication", ROOT / "dev/release-publication.py")
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)


def fixture(directory):
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
    (profile / "external-obligations.json").write_text(json.dumps({"schema_version": 1, "obligations": [
        {"id": "fixture-external", "description": "fixture obligation", "required_inputs": ["fixture account"]}]}))
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


def accepted(directory, prepared, identity):
    output = directory / "acceptance"
    with patch.object(m.a, "execution_environment", return_value={"system": "Linux", "machine": "x86_64", "go": "go version fixture"}):
        m.a.prepare(output, identity["version"], prepared)
    root, receipt = m.a.load_bound(output)
    for name, command in m.a.commands(root, identity["version"], receipt["profiles"]).items():
        if command:
            log = output / (name.replace(":", "-") + ".log")
            log.write_text("synthetic passing evidence fixture\n")
            receipt["checks"][name] = {"status": "passed", "exit_code": 0, "log": log.name,
                "log_sha256": m.a.digest(log), "command": m.a.command_identity(command, root),
                "execution_environment": receipt["environment"]}
            if m.a.is_live(name):
                receipt["checks"][name]["environments"] = [{"type": "subyard-pair", "vm_count": 2, "base_fingerprint": "b" * 64}]
    for item in receipt["external_obligations"].values():
        item.update({"status": "passed", "evidence_sha256": "c" * 64})
    m.a.save(output, receipt)
    return output, root, m.attestation(output, identity)


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


class Contracts(unittest.TestCase):
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

    def test_success_same_bytes_full_inventory_immutable_and_repeat(self):
        self.assertTrue(self.publish()["immutable"])
        self.assertEqual(len(self.github.assets), len(self.identity["release_files"]))
        before = copy.deepcopy(self.github.assets)
        self.assertTrue(self.publish()["immutable"])
        self.assertEqual(self.github.assets, before)
        self.assertFalse(any(method == "DELETE" for _, method in self.github.calls))

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
        class Dispatch:
            def api(self, *args, **kwargs):
                raise ValueError("fixture lost response")
        with self.assertRaisesRegex(ValueError, "lost response"):
            m.dispatch(Dispatch(), "release.yml", {}, state, path)
        self.assertTrue(json.loads(path.read_text())["dispatch_started"])
        with patch.object(m, "discover", return_value=None), patch.object(m.time, "monotonic", side_effect=[0, 121]):
            with self.assertRaisesRegex(ValueError, "outcome unknown"):
                m.dispatch(Dispatch(), "release.yml", {}, state, path)

unittest.main()
PY
