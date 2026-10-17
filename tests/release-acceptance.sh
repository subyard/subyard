#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PYTHONDONTWRITEBYTECODE=1 python3 -B - "$ROOT" <<'PY'
import copy
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import sys
import tarfile
import tempfile
import threading
import time
import unittest
from contextlib import redirect_stderr, redirect_stdout
from unittest.mock import patch

root = Path(sys.argv[1])
sys.argv = [sys.argv[0]]
spec = importlib.util.spec_from_file_location("acceptance", root / "dev/release-acceptance.py")
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)


def fixture_checkout(directory):
    checkout = directory / "checkout"
    for name in ("dev/agent-e2e.sh", "scripts/lib/runtime.sh", "tests/helpers/source-files.sh"):
        target = checkout / name
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(root / name, target)
    (checkout / ".gitignore").write_text("/.build/\n/private/\n/.subyard-acceptance/\n")
    (checkout / "Makefile").write_text("verify:\n\ttrue\n")
    inventory = checkout / "dev/test-profiles.sh"
    inventory.write_text("#!/bin/bash\nprintf 'runner\\tsample\\tconfig/profiles/sample/tests/e2e/acceptance.sh\\nrunner\\tpeer\\tconfig/profiles/peer/tests/e2e/acceptance.sh\\n'\n")
    for profile in ("sample", "peer"):
        runner = checkout / f"config/profiles/{profile}/tests/e2e/acceptance.sh"
        runner.parent.mkdir(parents=True)
        runner.write_text("#!/bin/bash\nexit 0\n")
    (checkout / "config/profiles/sample/tests/e2e/acceptance-lanes.json").write_text(json.dumps({
        "schema_version": 1, "lanes": [{"id": name, "arguments": ["--lane", name]} for name in ("one", "two")]}))
    (checkout / "dev/package-engine.sh").write_text('''#!/usr/bin/env bash
set -euo pipefail
printf 'packaged candidate\\n' > "$2/subyard-$4-linux-ARCH.tar.gz"
printf 'packaged engine\\n' > "$2/yard-$4-linux-ARCH"
python3 - "$2" "$4" <<'PY_PROVENANCE'
import hashlib, json, os, sys
from pathlib import Path
directory, version = Path(sys.argv[1]), sys.argv[2]
for filename in (f"yard-{version}-linux-ARCH", f"subyard-{version}-linux-ARCH.tar.gz"):
    sha = hashlib.sha256((directory / filename).read_bytes()).hexdigest()
    (directory / (filename + ".sha256")).write_text(sha + "  " + filename + "\\n")
    (directory / (filename + ".manifest.json")).write_text(json.dumps({"schemaVersion": 1, "version": version}) + "\\n")
    provenance = {"schemaVersion": 1, "artifact": filename, "sha256": sha, "version": version,
        "sourceRepository": "github.com/Dmitry-Borodin/Subyard", "canonicalRepository": "github.com/Subyard/Subyard",
        "sourceRevision": "unknown", "generatedAt": os.environ.get("SUBYARD_FIXTURE_GENERATED_AT", "2026-01-01T00:00:00Z")}
    provenance.update(json.loads(os.environ.get("SUBYARD_FIXTURE_PROVENANCE_OVERRIDES", "{}")))
    (directory / (filename + ".provenance.json")).write_text(json.dumps(provenance) + "\\n")
(directory / "subyard-install.sh").write_text("fixture installer\\n")
PY_PROVENANCE
'''.replace("ARCH", {"x86_64": "amd64", "aarch64": "arm64"}[m.platform.machine()]))
    return checkout


def prepare_fixture(checkout, output):
    check_output = m.subprocess.check_output
    def tools(command, **kwargs):
        return "go version fixture" if command == ["go", "version"] else check_output(command, **kwargs)
    with patch.object(m, "ROOT", checkout), patch.object(m.subprocess, "check_output", side_effect=tools):
        m.prepare(output, "test")
    return m.load_bound(output)


class TestAcceptance(unittest.TestCase):
    def test_optional_process_coverage_preserves_frozen_required_evidence(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            directory = Path(temporary)
            checkout = fixture_checkout(directory)
            output = directory / "candidate"
            source, receipt = prepare_fixture(checkout, output)
            self.assertEqual(receipt["checks"]["process-coverage"], m.PROCESS_COVERAGE_DIAGNOSTIC)
            with patch.object(m, "execution_environment", return_value=receipt["environment"]), patch.object(m, "execute", side_effect=AssertionError("default diagnostic ran")):
                m.run_checks(output, [], [], False, exclude=[name for name in m.commands(source, "test", receipt["profiles"]) if name != "process-coverage"])
            # Historical pending remains required, and load is read-only.
            receipt["checks"]["process-coverage"] = {"status": "pending"}
            m.save(output, receipt)
            frozen = (output / "receipt.json").read_bytes()
            self.assertEqual(m.load_bound(output)[1]["checks"]["process-coverage"], {"status": "pending"})
            self.assertEqual((output / "receipt.json").read_bytes(), frozen)
            command = m.commands(source, "test", receipt["profiles"])["process-coverage"]
            log = output / "legacy-coverage.log"
            log.write_text("historical coverage evidence\n")
            receipt["checks"]["process-coverage"] = {"status": "passed", "exit_code": 0, "command": command,
                "log": log.name, "log_sha256": m.digest(log), "execution_environment": receipt["environment"]}
            m.save(output, receipt)
            m.load_bound(output)
            original = (output / "receipt.json").read_bytes()
            target = directory / "imported"
            prepare_fixture(checkout, target)
            m.import_evidence(target, output, ["process-coverage"])
            imported = m.load_bound(target)[1]["checks"]["process-coverage"]
            self.assertEqual(imported["status"], "passed")
            self.assertTrue(imported["inherited"])
            self.assertEqual((output / "receipt.json").read_bytes(), original)
            receipt["checks"]["process-coverage"]["command"] = ["true"]
            m.save(output, receipt)
            with self.assertRaisesRegex(ValueError, "command provenance"):
                m.load_bound(output)
            for missing in ("process-coverage", "verify"):
                broken = dict(receipt, checks={k: v for k, v in receipt["checks"].items() if k != missing})
                m.save(output, broken)
                with self.assertRaisesRegex(ValueError, "inventory"):
                    m.load_bound(output)

    def test_optional_coverage_skips_live_prerequisite_but_can_be_selected(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            output = Path(temporary)
            receipt = {"version": "test", "source_fingerprint": "a" * 64, "candidate_bundle_sha256": "b" * 64, "profiles": {}, "external_obligations": {},
                "environment": {}, "checks": {"verify": {"status": "passed"},
                    "process-coverage": m.PROCESS_COVERAGE_DIAGNOSTIC.copy(), "p0-release-smoke": {"status": "pending"}}}
            calls = []
            def execute(name, *args):
                calls.append(name)
                return {"status": "passed", "exit_code": 0}
            with patch.object(m, "load_bound", return_value=(output, receipt)), patch.object(m, "execution_environment", return_value={}), \
                 patch.object(m, "commands", return_value={name: ["true"] for name in receipt["checks"]}), \
                 patch.object(m, "source_fingerprint", return_value="a" * 64), patch.object(m, "execute", side_effect=execute):
                self.assertEqual(m.run_checks(output, [1], [], False), 0)
                self.assertEqual(calls, ["p0-release-smoke"])
                self.assertEqual(m.run_checks(output, [], ["process-coverage"], False), 0)
                self.assertEqual(calls, ["p0-release-smoke", "process-coverage"])

    def transfer_fixture(self, directory, standalone=False):
        checkout = fixture_checkout(directory)
        first, second = directory / "old", directory / "new"
        old_root, old = prepare_fixture(checkout, first)
        name = "profile:sample/one" if standalone else "verify"
        command = m.commands(old_root, "test", old["profiles"])[name]
        artifacts = None
        if standalone:
            log, native, launch = directory / "original.log", directory / "native.json", directory / "launch.json"
            log.write_text("E2E lease: yard=sample slot=slot-002 run=sample type=subyard-pair vms=1 base=" + "a" * 64 + "\n"
                "  [ ok ] worktree bundle ready (sha256=" + old["candidate_bundle_sha256"] + ")\n"
                + "".join(f"E2E_PHASE phase={phase} state=end duration_seconds=1 exit_code=0 vm=1\n"
                          for phase in ("transport", "guest", "guest-cleanup"))
                + "E2E_PHASE phase=cleanup/release state=end duration_seconds=1 exit_code=0\n"
                + "E2E lease released: slot=slot-002 run=sample\n")
            native.write_text(json.dumps({"chunk_id": "original", "exit_code": 0, "output": "", "wall_time_seconds": 1.5}))
            launch.write_text(json.dumps({"cwd": str(old_root), "source_fingerprint": old["source_fingerprint"],
                "runtime_sha256": old["runtime_sha256"], "bundle_sha256": old["candidate_bundle_sha256"],
                "acceptance_script_sha256": m.digest(old_root / command[1]), "launched_at_utc": "2026-01-01T00:00:00+00:00",
                "argv": ["timeout", "--signal=TERM", "--kill-after=300s", "90m", "env", "TMPDIR=/tmp",
                    "SUBYARD_E2E_CONTROLLER_WORKSPACE=/fixture/workspace", "SUBYARD_E2E_CANDIDATE_BUNDLE=" + str(first / "candidate-bundle.tar.gz"),
                    "SUBYARD_E2E_CANDIDATE_SHA256=" + old["candidate_bundle_sha256"], *command, "--slot", "2"]}))
            artifacts = {role: {"path": str(path), "sha256": m.digest(path)} for role, path in (("log", log), ("native", native), ("launch", launch))}
        else:
            old["checks"][name] = m.execute(name, command, old_root, first, m.clean_environment())
            old["checks"][name]["execution_environment"] = old["environment"]
            m.save(first, old)
        package = checkout / "dev/package-engine.sh"
        package.write_text(package.read_text().replace("packaged candidate", "changed reviewed runtime"))
        _, new = prepare_fixture(checkout, second)
        review = {"schema_version": 1, "check": name, "tested": m.transfer_candidate(old), "target": m.transfer_candidate(new),
            "source_receipt_sha256": m.digest(first / "receipt.json"), "review": {"unchanged_scope": "Synthetic check behavior is unchanged",
            "reason": "Reviewed unrelated fixture runtime change", "authorization_reference": "operator-confirmation:fixture"}, "standalone": artifacts}
        declaration = directory / "review.json"
        declaration.write_text(json.dumps(review))
        return first, second, name, declaration, review

    def test_reviewed_receipt_transfer_retains_old_identity_and_strict_import(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            first, second, name, declaration, review = self.transfer_fixture(Path(temporary))
            old_receipt = (first / "receipt.json").read_bytes()
            with self.assertRaises(ValueError):
                m.import_evidence(second, first, [name])
            self.assertEqual(m.transfer_evidence(second, first, name, declaration), 0)
            _, target = m.load_bound(second)
            result = target["checks"][name]
            self.assertNotEqual(result["evidence_transfer"]["tested"]["runtime_sha256"], target["runtime_sha256"])
            self.assertEqual(result["evidence_transfer"]["target"], review["target"])
            self.assertEqual(result["execution"], "inherited")
            self.assertEqual(target["coverage"]["inherited"], [name])
            self.assertNotIn(name, target["coverage"]["executed"])
            self.assertEqual(target["checks"]["compatibility"]["status"], "pending")
            self.assertEqual((first / "receipt.json").read_bytes(), old_receipt)
            for filename in result["evidence_files"].values():
                self.assertEqual((second / filename).stat().st_mode & 0o777, 0o600)
            result["evidence_transfer"]["actual_command"] = ["wrong-command"]
            m.save(second, target)
            with self.assertRaises(ValueError):
                m.load_bound(second)

    def test_ordinary_import_rejects_reviewed_transfer_before_batch_writes(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            directory = Path(temporary)
            first, second, name, declaration, _ = self.transfer_fixture(directory)
            self.assertEqual(m.transfer_evidence(second, first, name, declaration), 0)
            source_root, source = m.load_bound(second)
            executed = "profile:sample/two"
            command = m.commands(source_root, "test", source["profiles"])[executed]
            source["checks"][executed] = m.execute(executed, command, source_root, second, m.clean_environment())
            m.save(second, source)
            third = directory / "third"
            prepare_fixture(directory / "checkout", third)
            before = {path.name: path.read_bytes() for path in third.iterdir() if path.is_file()}
            with self.assertRaisesRegex(ValueError, "cannot chain reviewed evidence transfers"):
                m.import_evidence(third, second, [executed, name])
            self.assertEqual({path.name: path.read_bytes() for path in third.iterdir() if path.is_file()}, before)
            _, target = m.load_bound(third)
            self.assertEqual(target["checks"][name]["status"], "pending")
            self.assertEqual(target["checks"][executed]["status"], "pending")
            self.assertEqual(m.import_evidence(third, second, [executed]), 0)
            _, target = m.load_bound(third)
            self.assertEqual(target["checks"][executed]["status"], "passed")

    def test_reviewed_transfer_rejects_missing_authorization_binding_and_original_pass(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            first, second, name, declaration, review = self.transfer_fixture(Path(temporary))
            before = (second / "receipt.json").read_bytes()
            for change in (lambda r: r["review"].update(authorization_reference=""),
                           lambda r: r.update(schema_version=True),
                           lambda r: r["review"].update(unchanged_scope=""),
                           lambda r: r["target"].update(runtime_sha256="f" * 64),
                           lambda r: r.update(check="compatibility")):
                invalid = copy.deepcopy(review)
                change(invalid)
                declaration.write_text(json.dumps(invalid))
                with self.assertRaises(ValueError):
                    m.transfer_evidence(second, first, name, declaration)
                self.assertEqual((second / "receipt.json").read_bytes(), before)
            _, old = m.load_bound(first)
            declaration.write_text(json.dumps(review)[:-1] + ', "check": "verify"}')
            with self.assertRaisesRegex(ValueError, "duplicate"):
                m.transfer_evidence(second, first, name, declaration)
            original = copy.deepcopy(old["checks"][name])
            for status, code in (("failed", 1), ("interrupted", -15), ("pending", None), ("passed", 1), ("passed", True)):
                old["checks"][name] = {**original, "status": status, "exit_code": code}
                m.save(first, old)
                review["source_receipt_sha256"] = m.digest(first / "receipt.json")
                declaration.write_text(json.dumps(review))
                with self.assertRaises(ValueError):
                    m.transfer_evidence(second, first, name, declaration)
                self.assertEqual((second / "receipt.json").read_bytes(), before)

    def test_standalone_transfer_requires_original_complete_native_and_controller_evidence(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            first, second, name, declaration, review = self.transfer_fixture(Path(temporary), standalone=True)
            before = (second / "receipt.json").read_bytes()
            for role, mutate in (("native", lambda r: r.update(exit_code=1)),
                                 ("native", lambda r: r.update(session_id=123)),
                                 ("launch", lambda r: r["argv"].__setitem__(-4, "foreign.sh"))):
                path = Path(review["standalone"][role]["path"])
                original = path.read_bytes()
                invalid = json.loads(original)
                mutate(invalid)
                path.write_text(json.dumps(invalid))
                review["standalone"][role]["sha256"] = m.digest(path)
                declaration.write_text(json.dumps(review))
                with self.assertRaises(ValueError):
                    m.transfer_evidence(second, first, name, declaration)
                self.assertEqual((second / "receipt.json").read_bytes(), before)
                path.write_bytes(original)
                review["standalone"][role]["sha256"] = m.digest(path)
            log = Path(review["standalone"]["log"]["path"])
            original_log = log.read_bytes()
            log.write_text("incomplete controller\n")
            review["standalone"]["log"]["sha256"] = m.digest(log)
            declaration.write_text(json.dumps(review))
            with self.assertRaises(ValueError):
                m.transfer_evidence(second, first, name, declaration)
            log.write_bytes(original_log.replace(b"phase=guest state=end duration_seconds=1 exit_code=0", b"phase=guest state=end duration_seconds=1 exit_code=1"))
            review["standalone"]["log"]["sha256"] = m.digest(log)
            declaration.write_text(json.dumps(review))
            with self.assertRaises(ValueError):
                m.transfer_evidence(second, first, name, declaration)
            log.write_bytes(original_log)
            review["standalone"]["log"]["sha256"] = m.digest(log)
            declaration.write_text(json.dumps(review))
            self.assertEqual(m.transfer_evidence(second, first, name, declaration), 0)
            _, target = m.load_bound(second)
            result = target["checks"][name]
            self.assertIsNone(result["duration_seconds"])
            self.assertIsNone(result["execution_environment"])
            self.assertEqual(result["evidence_transfer"]["launched_at_utc"], "2026-01-01T00:00:00+00:00")
            self.assertEqual(result["environments"][0]["base_fingerprint"], "a" * 64)
            self.assertEqual(json.loads((first / "receipt.json").read_text())["checks"][name]["status"], "pending")
            (second / result["evidence_files"]["native"]).write_text("tampered")
            with self.assertRaises(ValueError):
                m.load_bound(second)

    def test_standalone_direct_controller_keeps_optional_phases_unknown(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            first, second, name, declaration, review = self.transfer_fixture(Path(temporary), standalone=True)
            old_receipt = (first / "receipt.json").read_bytes()
            before = (second / "receipt.json").read_bytes()
            log = Path(review["standalone"]["log"]["path"])
            direct = ("E2E lease: yard=sample slot=slot-002 run=sample type=subyard-pair vms=2 base=" + "a" * 64 + "\n"
                + "sample_source_bundle_sha256=" + review["tested"]["candidate_bundle_sha256"] + " base_fingerprint=" + "a" * 64 + " lane=one\n"
                + "".join(f"E2E_PHASE phase=guest-cleanup state=end duration_seconds=1 exit_code=0 vm={vm}\n" for vm in (1, 2))
                + "E2E_PHASE phase=cleanup/release state=end duration_seconds=1 exit_code=0\n"
                + "E2E lease released: slot=slot-002 run=sample\n")
            for invalid in (direct.replace(" vm=2\n", " vm=1\n"),
                            direct.replace("E2E_PHASE phase=cleanup/release state=end duration_seconds=1 exit_code=0\n", ""),
                            direct.replace("E2E lease released: slot=slot-002 run=sample\n", ""),
                            direct + "E2E_PHASE phase=fixture/sample state=end duration_seconds=1 exit_code=1\n",
                            direct + "  [fail] phase=sample\n"):
                log.write_text(invalid)
                review["standalone"]["log"]["sha256"] = m.digest(log)
                declaration.write_text(json.dumps(review))
                with self.assertRaises(ValueError):
                    m.transfer_evidence(second, first, name, declaration)
                self.assertEqual((second / "receipt.json").read_bytes(), before)
            log.write_text(direct)
            review["standalone"]["log"]["sha256"] = m.digest(log)
            declaration.write_text(json.dumps(review))
            self.assertEqual(m.transfer_evidence(second, first, name, declaration), 0)
            _, target = m.load_bound(second)
            result = target["checks"][name]
            self.assertEqual(result["execution"], "inherited")
            self.assertEqual(result["environments"][0]["vm_count"], 2)
            self.assertIsNone(result["duration_seconds"])
            self.assertNotIn("phases", result)
            self.assertEqual((first / "receipt.json").read_bytes(), old_receipt)

    def raw_standalone_fixture(self, directory):
        first, second, name, declaration, review = self.transfer_fixture(directory, standalone=True)
        launch_path = Path(review["standalone"]["launch"]["path"])
        launch = json.loads(launch_path.read_text())
        launch = {"cwd": launch["cwd"], "argv": launch["argv"],
                  "started_at": "2026-01-01T00:00:00+00:00", "started_monotonic": 123.5}
        launch_path.write_text(json.dumps(launch))
        review["standalone"]["launch"]["sha256"] = m.digest(launch_path)
        declaration.write_text(json.dumps(review))
        return first, second, name, declaration, review

    def test_raw_standalone_launch_binds_original_bundle_without_rewriting_evidence(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            directory = Path(temporary)
            first, second, name, declaration, review = self.raw_standalone_fixture(directory)
            originals = {role: Path(item["path"]).read_bytes() for role, item in review["standalone"].items()}
            receipt = (first / "receipt.json").read_bytes()
            self.assertEqual(m.transfer_evidence(second, first, name, declaration), 0)
            _, target = m.load_bound(second)
            result = target["checks"][name]
            self.assertEqual((result["status"], result["exit_code"], result["execution"]), ("passed", 0, "inherited"))
            self.assertIsNone(result["duration_seconds"])
            self.assertIsNone(result["execution_environment"])
            self.assertEqual(result["evidence_transfer"]["launched_at_utc"], "2026-01-01T00:00:00+00:00")
            self.assertEqual((first / "receipt.json").read_bytes(), receipt)
            for role, original in originals.items():
                self.assertEqual(Path(review["standalone"][role]["path"]).read_bytes(), original)
                self.assertEqual((second / result["evidence_files"][role]).read_bytes(), original)
            first.rename(directory / "retired-original")
            self.assertEqual(m.load_bound(second)[1]["checks"][name], result)
            copied = second / result["evidence_files"]["launch"]
            data = json.loads(copied.read_text())
            data["started_at"] = "not-a-time"
            copied.write_text(json.dumps(data))
            with self.assertRaises(ValueError):
                m.load_bound(second)

    def test_raw_standalone_launch_rejects_malformed_binding_time_transport_and_native(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            first, second, name, declaration, review = self.raw_standalone_fixture(Path(temporary))
            files = {role: Path(item["path"]) for role, item in review["standalone"].items()}
            launch = json.loads(files["launch"].read_text())
            native = json.loads(files["native"].read_text())
            before = (second / "receipt.json").read_bytes()
            for change in ("partial-rich", "bad-source", "bad-runtime", "bad-bundle", "bad-script", "cwd", "wrapper", "hash", "path", "env", "command", "slot", "missing-time", "bad-time", "naive-time", "clock", "contradictory-time", "failed", "nonterminal"):
                invalid_launch, invalid_native = copy.deepcopy(launch), copy.deepcopy(native)
                if change in ("partial-rich", "bad-source", "bad-runtime", "bad-bundle", "bad-script"):
                    if change == "partial-rich":
                        invalid_launch["source_fingerprint"] = review["tested"]["source_fingerprint"]
                    else:
                        invalid_launch.update(source_fingerprint=review["tested"]["source_fingerprint"],
                            runtime_sha256=review["tested"]["runtime_sha256"], bundle_sha256=review["tested"]["candidate_bundle_sha256"],
                            acceptance_script_sha256=m.digest(first / "source" / launch["argv"][10]))
                        key = {"bad-source": "source_fingerprint", "bad-runtime": "runtime_sha256", "bad-bundle": "bundle_sha256", "bad-script": "acceptance_script_sha256"}[change]
                        invalid_launch[key] = None
                elif change == "cwd":
                    invalid_launch["cwd"] = str(second / "source")
                elif change in ("wrapper", "hash", "path", "env", "command", "slot"):
                    index, value = {"wrapper": (0, "sh"), "hash": (8, "SUBYARD_E2E_CANDIDATE_SHA256=" + "f" * 64),
                        "path": (7, "SUBYARD_E2E_CANDIDATE_BUNDLE=/wrong/bundle"), "env": (6, "OTHER=/wrong"),
                        "command": (10, "foreign.sh"), "slot": (-1, "0")}[change]
                    invalid_launch["argv"][index] = value
                elif change == "missing-time":
                    del invalid_launch["started_at"]
                elif change in ("bad-time", "naive-time", "clock", "contradictory-time"):
                    key, value = {"bad-time": ("started_at", "not-a-time"), "naive-time": ("started_at", "2026-01-01T00:00:00"),
                        "clock": ("started_monotonic", "unknown"), "contradictory-time": ("launched_at_utc", "2026-01-02T00:00:00+00:00")}[change]
                    invalid_launch[key] = value
                else:
                    invalid_native.update({"exit_code": 1} if change == "failed" else {"session_id": 123})
                files["launch"].write_text(json.dumps(invalid_launch))
                files["native"].write_text(json.dumps(invalid_native))
                for role in ("launch", "native"):
                    review["standalone"][role]["sha256"] = m.digest(files[role])
                declaration.write_text(json.dumps(review))
                with self.subTest(change=change), self.assertRaises(ValueError):
                    m.transfer_evidence(second, first, name, declaration)
                self.assertEqual((second / "receipt.json").read_bytes(), before)

    def test_raw_standalone_launch_refuses_checksum_consistent_but_different_bundle(self):
        for change in ("source", "mode", "runtime", "metadata", "extra"):
            with self.subTest(change=change), tempfile.TemporaryDirectory(dir="/tmp") as temporary:
                directory = Path(temporary)
                first, second, name, declaration, review = self.raw_standalone_fixture(directory)
                _, receipt = m.load_bound(first)
                bundle = first / "candidate-bundle.tar.gz"
                replacement = directory / "changed-bundle.tar.gz"
                selected = {"source": "config/profiles/sample/tests/e2e/acceptance.sh", "mode": "config/profiles/sample/tests/e2e/acceptance.sh",
                    "runtime": ".subyard-acceptance/release/" + receipt["runtime_file"], "metadata": ".subyard-acceptance/candidate.json"}.get(change)
                with tarfile.open(bundle, "r:gz") as original, tarfile.open(replacement, "w:gz") as changed:
                    for member in original.getmembers():
                        data = original.extractfile(member).read()
                        member = copy.copy(member)
                        if member.name == selected:
                            if change == "mode":
                                member.mode ^= 0o111
                            else:
                                data += b"tampered"
                                member.size = len(data)
                        changed.addfile(member, io.BytesIO(data))
                    if change == "extra":
                        member = tarfile.TarInfo("unbound-source.txt")
                        member.mode, member.size = 0o644, 4
                        changed.addfile(member, io.BytesIO(b"oops"))
                replacement.replace(bundle)
                receipt["candidate_bundle_sha256"] = m.digest(bundle)
                m.save(first, receipt)
                review["tested"] = m.transfer_candidate(receipt)
                review["source_receipt_sha256"] = m.digest(first / "receipt.json")
                launch_path = Path(review["standalone"]["launch"]["path"])
                launch = json.loads(launch_path.read_text())
                launch["argv"][8] = "SUBYARD_E2E_CANDIDATE_SHA256=" + receipt["candidate_bundle_sha256"]
                launch_path.write_text(json.dumps(launch))
                review["standalone"]["launch"]["sha256"] = m.digest(launch_path)
                declaration.write_text(json.dumps(review))
                before = (second / "receipt.json").read_bytes()
                with self.assertRaisesRegex(ValueError, "tested bundle"):
                    m.transfer_evidence(second, first, name, declaration)
                self.assertEqual((second / "receipt.json").read_bytes(), before)

    def test_raw_standalone_bundle_bounds_directory_members(self):
        for change in ("ancestor", "unowned", "duplicate"):
            with self.subTest(change=change), tempfile.TemporaryDirectory(dir="/tmp") as temporary:
                directory = Path(temporary)
                first, second, name, declaration, review = self.raw_standalone_fixture(directory)
                _, receipt = m.load_bound(first)
                bundle, replacement = first / "candidate-bundle.tar.gz", directory / "changed-bundle.tar.gz"
                directories = (["config"] if change == "ancestor" else ["config", "config"]
                               if change == "duplicate" else [f"unowned-directory-{i}" for i in range(32)])
                with tarfile.open(bundle, "r:gz") as original, tarfile.open(replacement, "w:gz") as changed:
                    for path in directories:
                        member = tarfile.TarInfo(path)
                        member.type = tarfile.DIRTYPE
                        changed.addfile(member)
                    for member in original:
                        changed.addfile(member, original.extractfile(member))
                replacement.replace(bundle)
                receipt["candidate_bundle_sha256"] = m.digest(bundle)
                m.save(first, receipt)
                review["tested"] = m.transfer_candidate(receipt)
                review["source_receipt_sha256"] = m.digest(first / "receipt.json")
                launch_path = Path(review["standalone"]["launch"]["path"])
                launch = json.loads(launch_path.read_text())
                launch["argv"][8] = "SUBYARD_E2E_CANDIDATE_SHA256=" + receipt["candidate_bundle_sha256"]
                launch_path.write_text(json.dumps(launch))
                review["standalone"]["launch"]["sha256"] = m.digest(launch_path)
                declaration.write_text(json.dumps(review))
                before = (second / "receipt.json").read_bytes()
                with patch.object(m.tarfile.TarFile, "getmembers", side_effect=AssertionError("eager archive loading")):
                    if change == "ancestor":
                        self.assertEqual(m.transfer_evidence(second, first, name, declaration), 0)
                    else:
                        with self.assertRaisesRegex(ValueError, "unsafe standalone tested bundle member"):
                            m.transfer_evidence(second, first, name, declaration)
                        self.assertEqual((second / "receipt.json").read_bytes(), before)

    def test_rich_standalone_clock_validation_without_optional_timestamp(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            first, second, name, declaration, review = self.transfer_fixture(Path(temporary), standalone=True)
            source, receipt = m.load_bound(first)
            files = {role: Path(item["path"]) for role, item in review["standalone"].items()}
            launch = json.loads(files["launch"].read_text())
            del launch["launched_at_utc"]
            command = m.commands(source, "test", receipt["profiles"])[name]
            before = (second / "receipt.json").read_bytes()
            for clock in ("unknown", None, True, -1, float("inf"), float("nan")):
                files["launch"].write_text(json.dumps({**launch, "started_monotonic": clock}))
                review["standalone"]["launch"]["sha256"] = m.digest(files["launch"])
                declaration.write_text(json.dumps(review))
                with self.subTest(clock=clock), self.assertRaisesRegex(ValueError, "invalid standalone launch time"):
                    m.transfer_evidence(second, first, name, declaration)
                self.assertEqual((second / "receipt.json").read_bytes(), before)
            for valid in (launch, {**launch, "started_monotonic": 0}, {**launch, "started_monotonic": 123.5}):
                files["launch"].write_text(json.dumps(valid))
                self.assertIsNone(m.standalone_result(source, receipt, name, command, files)["launched_at_utc"])

    def test_prepare_uses_plain_sources_and_retains_only_needed_artifacts(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            checkout = directory / "checkout"
            for name in ("dev/agent-e2e.sh", "scripts/lib/runtime.sh", "tests/helpers/source-files.sh"):
                target = checkout / name
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(root / name, target)
            (checkout / ".gitignore").write_text("/.build/\n/private/\n/.subyard-acceptance/\n")
            (checkout / "input.txt").write_text("current uncommitted input\n")
            (checkout / "private").mkdir()
            (checkout / "private/excluded.txt").write_text("excluded fixture\n")
            package = checkout / "dev/package-engine.sh"
            package.write_text('''#!/usr/bin/env bash
set -euo pipefail
[ "$1" = --output-dir ] && [ "$3" = --version ]
printf 'packaged candidate\\n' > "$2/subyard-$4-linux-ARCH.tar.gz"
'''.replace("ARCH", {"x86_64": "amd64", "aarch64": "arm64"}[m.platform.machine()]))
            output = checkout / ".build/acceptance"
            inventory = {"sample": {"kind": "runner", "path": "sample-acceptance.sh"}}
            check_output = m.subprocess.check_output
            def tool_output(command, **kwargs):
                if command == ["go", "version"]:
                    return "go version fixture"
                return check_output(command, **kwargs)
            with patch.object(m, "ROOT", checkout), \
                 patch.object(m, "profile_inventory", return_value=inventory), \
                 patch.object(m, "external_inventory", return_value={}), \
                 patch.object(m.subprocess, "check_output", side_effect=tool_output):
                m.prepare(output, "test")
                source, receipt = m.load_bound(output)
                self.assertEqual(source, output / "source")
                self.assertEqual((source / "input.txt").read_text(), "current uncommitted input\n")
                self.assertEqual({path.name for path in output.iterdir()},
                                 {"source", "candidate-bundle.tar.gz", "receipt.json", "events.jsonl", receipt["checks"]["package"]["log"]})
                self.assertEqual(output.stat().st_mode & 0o777, 0o700)
                for name in ("receipt.json", "events.jsonl", receipt["checks"]["package"]["log"], "candidate-bundle.tar.gz"):
                    self.assertEqual((output / name).stat().st_mode & 0o777, 0o600)
                self.assertFalse(list(source.rglob(".git")))
                self.assertFalse((source / "private").exists())
                with tarfile.open(output / "candidate-bundle.tar.gz") as archive:
                    names = set(archive.getnames())
                    self.assertIn("input.txt", names)
                    self.assertIn(".subyard-acceptance/candidate.json", names)
                    self.assertIn(".subyard-acceptance/release/" + receipt["runtime_file"], names)
                    self.assertFalse(any(".git" in Path(name).parts for name in names))
                    self.assertNotIn(".subyard-e2e-index", names)
                (source / "input.txt").write_text("changed after preparation\n")
                with self.assertRaisesRegex(ValueError, "frozen source fingerprint mismatch"):
                    m.load_bound(output)
                (source / "input.txt").write_text("current uncommitted input\n")
                runtime = source / ".subyard-acceptance/release" / receipt["runtime_file"]
                runtime.write_text("changed runtime\n")
                with self.assertRaisesRegex(ValueError, "candidate release assets changed"):
                    m.load_bound(output)

    def test_complete_status_requires_every_obligation(self):
        for check, external, expected in (
            ("pending", "passed", "incomplete"),
            ("passed", "not-run", "incomplete"),
            ("failed", "passed", "failed"),
            ("passed", "passed", "passed"),
        ):
            receipt = {"checks": {"verify": {"status": check}},
                       "external_obligations": {"x": {"status": external}}}
            m.update_result(receipt)
            self.assertEqual(receipt["result"], expected)

    def test_explicit_external_deferral_preserves_execution_and_candidate_binding(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            checkout = fixture_checkout(directory)
            manifest = checkout / "config/profiles/sample/tests/e2e/external-obligations.json"
            manifest.write_text(json.dumps({"schema_version": 1, "obligations": [
                {"id": "account", "description": "Authorized account acceptance", "required_inputs": ["account"]}]}))
            output = directory / "acceptance"
            _, receipt = prepare_fixture(checkout, output)
            for obligations, reason, authorization in (
                (["unknown"], "Unavailable external account", "operator-authorization-1"),
                (["verify"], "Unavailable external account", "operator-authorization-1"),
                (["sample:account", "sample:account"], "Unavailable external account", "operator-authorization-1"),
                (["sample:account"], " ", "operator-authorization-1"),
                (["sample:account"], "Unavailable external account", ""),
                (["sample:account"], "Unavailable\naccount", "operator-authorization-1"),
            ):
                before = (output / "receipt.json").read_bytes()
                with self.assertRaises(ValueError):
                    m.defer_external(output, obligations, reason, authorization)
                self.assertEqual((output / "receipt.json").read_bytes(), before)
            self.assertEqual(m.defer_external(output, ["sample:account"],
                "Unavailable external account", "operator-authorization-1"), 0)
            _, receipt = m.load_bound(output)
            external = receipt["external_obligations"]["sample:account"]
            self.assertEqual(external["status"], "not-run")
            self.assertNotIn("evidence_sha256", external)
            self.assertEqual(external["deferral"]["candidate_sha256"], m.deferral_candidate_sha256(receipt))
            self.assertEqual(receipt["checks"]["verify"]["status"], "pending")
            self.assertEqual(receipt["result"], "incomplete")
            with self.assertRaisesRegex(ValueError, "already recorded"):
                m.defer_external(output, ["sample:account"], "Another reason", "operator-authorization-1")
            external["deferral"]["candidate_sha256"] = "f" * 64
            m.save(output, receipt)
            with self.assertRaisesRegex(ValueError, "candidate binding"):
                m.load_bound(output)
            external["deferral"]["candidate_sha256"] = m.deferral_candidate_sha256(receipt)
            external["status"] = "failed"
            m.save(output, receipt)
            with self.assertRaisesRegex(ValueError, "deferral"):
                m.load_bound(output)
            del external["deferral"]
            m.save(output, receipt)
            with self.assertRaisesRegex(ValueError, "deferral"):
                m.defer_external(output, ["sample:account"], "Unavailable external account", "operator-authorization-1")

    def test_transport_override_is_scoped_to_physical_controllers(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            root = output / "source"
            receipt = {"version": "test", "source_fingerprint": "a" * 64, "profiles": {},
                       "candidate_bundle_sha256": "b" * 64, "external_obligations": {},
                       "checks": {name: {"status": "pending"} for name in ("verify", "profile:sample")}}
            observed = {}
            def execute(name, command, root, output, environment, slot=None):
                observed[name] = dict(environment)
                return {"status": "passed", "exit_code": 0}
            with patch.object(m, "load_bound", return_value=(root, receipt)), \
                 patch.object(m, "commands", return_value={"verify": ["true"], "profile:sample": ["true"]}), \
                 patch.object(m, "source_fingerprint", return_value="a" * 64), \
                 patch.object(m, "execute", side_effect=execute), \
                 patch.dict(os.environ, {"SUBYARD_E2E_CANDIDATE_BUNDLE": "ambient",
                                         "SUBYARD_E2E_CONTROLLER_WORKSPACE": "ambient"}):
                self.assertEqual(m.run_checks(output, [1], [], False), 0)
            self.assertNotIn("SUBYARD_E2E_CANDIDATE_BUNDLE", observed["verify"])
            self.assertNotIn("SUBYARD_E2E_CONTROLLER_WORKSPACE", observed["verify"])
            self.assertEqual(observed["profile:sample"]["SUBYARD_E2E_CONTROLLER_WORKSPACE"], str(m.ROOT))
            self.assertEqual(observed["profile:sample"]["SUBYARD_E2E_CANDIDATE_BUNDLE"],
                             str(output / "candidate-bundle.tar.gz"))

    def test_lane_inventory_family_exact_selection_and_complete_aggregate(self):
        with tempfile.TemporaryDirectory() as temporary:
            checkout = fixture_checkout(Path(temporary))
            inventory = m.profile_inventory(checkout)
            checks = m.commands(checkout, "test", inventory)
            self.assertEqual(list(checks)[-3:], ["profile:sample/one", "profile:sample/two", "profile:peer"])
            self.assertEqual(m.select_checks(checks, ["profile:sample"]), {"profile:sample/one", "profile:sample/two"})
            self.assertEqual(m.select_checks(checks, ["profile:sample/two"]), {"profile:sample/two"})
            receipt = {"profiles": inventory, "external_obligations": {},
                       "checks": {name: {"status": "passed"} for name in checks}}
            receipt["checks"]["profile:sample/two"]["status"] = "pending"
            m.update_result(receipt)
            self.assertEqual(receipt["profile_results"]["sample"], "incomplete")
            self.assertEqual(receipt["result"], "incomplete")
            receipt["checks"]["profile:sample/two"]["status"] = "passed"
            m.update_result(receipt)
            self.assertEqual(receipt["profile_results"]["sample"], "passed")
            self.assertEqual(receipt["result"], "passed")
            manifest = checkout / "config/profiles/sample/tests/e2e/acceptance-lanes.json"
            manifest.write_text(json.dumps({"schema_version": 1, "lanes": [{"id": "one", "arguments": ["--slot", "2"]}]}))
            with self.assertRaisesRegex(ValueError, "invalid or duplicate"):
                m.profile_inventory(checkout)

    def test_preflight_does_not_package_allocate_authenticate_or_write(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            checkout = fixture_checkout(directory)
            obligation = checkout / "config/profiles/sample/tests/e2e/external-obligations.json"
            obligation.write_text(json.dumps({"schema_version": 1, "obligations": [{"id": "account",
                "description": "Authorized account acceptance", "required_inputs": ["authorized account"]}]}))
            before = set(checkout.rglob("*"))
            status = directory / "status.json"
            readiness = directory / "readiness.json"
            readiness.write_text('{"sample:account":"available"}')
            status.write_text(json.dumps({"schema_version": 1, "status": "ok", "private": "do-not-export",
                "pool": {"slots": [{"slot_id": "slot-1", "state": "available", "credential": "do-not-export"}]},
                "resources": {"memory": {"available_bytes": 42, "unknown": "do-not-export"},
                              "storage": {"physical_free_bytes": 100}}}))
            inventory = m.profile_inventory(checkout)
            with patch.object(m, "ROOT", checkout), patch.object(m, "execute", side_effect=AssertionError("execution")), \
                 patch.object(m, "bundle", side_effect=AssertionError("packaging")), \
                 patch.object(m, "profile_inventory", return_value=inventory), \
                 patch.object(m.subprocess, "run", side_effect=AssertionError("mutation/authentication")), \
                 redirect_stdout(io.StringIO()):
                report = m.preflight(only=["profile:sample"], exclude=["profile:sample/two"], broker_status=status,
                                     external_readiness=readiness)
            self.assertEqual(before, set(checkout.rglob("*")))
            self.assertIn("profile:sample/two", report["pending"])
            excluded = next(item for item in report["checks"] if item["id"] == "profile:sample/two")
            self.assertTrue(excluded["excluded"])
            self.assertEqual(excluded["status"], "pending")
            self.assertNotIn("do-not-export", json.dumps(report))
            self.assertEqual(report["broker"]["memory"]["available_bytes"], 42)
            self.assertEqual(report["capacity_summary"]["planning"], "unknown")
            self.assertIsNone(report["capacity_summary"]["observed_at"])
            self.assertIsNone(report["capacity_summary"]["headroom_bytes"]["memory"])
            self.assertIn("GitHub CI", report["architecture_responsibility"]["arm64"])
            self.assertEqual(report["external_prerequisites"][0]["readiness"], "available")
            self.assertEqual(report["external_prerequisites"][0]["status"], "not-run")
            readiness.write_text('{"sample:account":"passed"}')
            with patch.object(m, "ROOT", checkout), self.assertRaisesRegex(ValueError, "invalid external readiness"):
                m.preflight(external_readiness=readiness)

    def test_capacity_planning_complete_incomplete_legacy_and_observation_freshness(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            checkout = fixture_checkout(directory)
            status = directory / "status.json"
            value = {"schema_version": 1, "status": "ok", "private": "do-not-export",
                "pool": {"slots": [{"slot_id": "slot-001", "state": "available",
                    "environment": {"type": "subyard-pair", "credential": "do-not-export"}}]},
                "resources": {"memory": {"available_bytes": 1000},
                    "storage": {"physical_free_bytes": 2000, "budget_used_bytes": 100},
                    "budgets": {"memory_reserve_bytes": 100, "disk_reserve_bytes": 200, "disk_bytes": 0},
                    "slots": [{"remaining_memory_growth_bytes": 300, "remaining_disk_growth_bytes": 400}],
                    "builder": {"reserved_memory_bytes": 50, "reserved_disk_peak_bytes": 60}}}
            inventory = m.profile_inventory(checkout)
            def inspect(only=("profile:sample",)):
                status.write_text(json.dumps(value))
                compact = io.StringIO()
                with patch.object(m, "ROOT", checkout), patch.object(m, "profile_inventory", return_value=inventory), \
                     patch.object(m, "execute", side_effect=AssertionError("execution")), \
                     patch.object(m, "bundle", side_effect=AssertionError("packaging")), \
                     patch.object(m.subprocess, "run", side_effect=AssertionError("mutation/authentication")), \
                     redirect_stderr(compact), redirect_stdout(io.StringIO()):
                    report = m.preflight(only=only, broker_status=status, slots=[1], types=["subyard-pair"])
                self.assertEqual(len(compact.getvalue().splitlines()), 1)
                self.assertIn("CAPACITY slots=slot-001:", compact.getvalue())
                self.assertIn("types=subyard-pair", compact.getvalue())
                self.assertIn("prerequisites=", compact.getvalue())
                self.assertNotIn("do-not-export", compact.getvalue())
                return report
            before = set(checkout.rglob("*"))
            report = inspect()
            summary = report["capacity_summary"]
            self.assertEqual(summary["selected_slots"], [{"id": "slot-001", "state": "available", "type": "subyard-pair"}])
            self.assertEqual(summary["selected_types"], ["subyard-pair"])
            self.assertEqual(summary["headroom_bytes"], {"memory": 550, "disk": 1340})
            self.assertEqual(summary["planning"], "unknown")  # Positive headroom does not prove a request fits.
            self.assertIsNone(summary["observed_at"])
            self.assertIsNone(summary["age_seconds"])
            self.assertTrue(summary["prerequisites"]["physical"])
            self.assertNotIn("do-not-export", json.dumps(report))
            self.assertEqual(before, set(checkout.rglob("*")))
            value["pool"]["slots"][0]["environment"]["type"] = "do-not-export"
            self.assertNotIn("do-not-export", json.dumps(inspect()))
            value["pool"]["slots"][0]["environment"]["type"] = "subyard-pair"
            del value["resources"]["slots"][0]["remaining_memory_growth_bytes"]
            self.assertIsNone(inspect()["capacity_summary"]["headroom_bytes"]["memory"])
            value["resources"]["slots"][0]["remaining_memory_growth_bytes"] = 300
            value["resources"]["errors"] = ["legacy_reservation_unknown", "do-not-export"]
            self.assertEqual(inspect()["capacity_summary"]["headroom_bytes"], {"memory": None, "disk": None})
            del value["resources"]["errors"]
            measured_now = m.datetime(2026, 1, 1, 0, 1, tzinfo=m.timezone.utc)
            for observed, age, freshness in (("2026-01-01T00:00:00Z", 60, "supplied file snapshot"),
                ("2026-01-01T00:02:00Z", None, "unknown; observation timestamp is in the future"),
                ("2026-01-01T00:00:00", None, "unknown"), ("2026-99-01T00:00:00Z", None, "unknown"),
                ("0001-01-01T00:00:00+01:00", None, "unknown"),
                ("do-not-export", None, "unknown")):
                value["observed_at"] = observed
                with patch.object(m, "datetime", wraps=m.datetime) as clock:
                    clock.now.return_value = measured_now
                    summary = inspect()["capacity_summary"]
                self.assertEqual(summary["age_seconds"], age)
                self.assertIn(freshness, summary["freshness"])
                self.assertNotIn("do-not-export", json.dumps(summary))
                if observed == "2026-01-01T00:00:00Z":
                    self.assertEqual(summary["observed_at"], "2026-01-01T00:00:00+00:00")
            value["resources"]["memory"]["available_bytes"] = 100
            summary = inspect()["capacity_summary"]
            self.assertEqual(summary["planning"], "insufficient")
            self.assertEqual(summary["shortages"], ["memory headroom exhausted"])
            value["resources"]["memory"]["available_bytes"] = 1000
            value["pool"]["slots"][0]["state"] = "held"
            self.assertEqual(inspect()["capacity_summary"]["shortages"], ["slot-001: held"])
            local_summary = inspect(only=["verify"])["capacity_summary"]
            self.assertEqual(local_summary["planning"], "not-required")
            self.assertEqual(local_summary["shortages"], [])
            status.write_text(json.dumps(value))
            with patch.object(m, "ROOT", checkout), patch.object(m, "profile_inventory", return_value=inventory), \
                 patch.object(m, "execute", side_effect=AssertionError("execution")), \
                 patch.object(sys, "argv", ["acceptance", "preflight", "--broker-status", str(status),
                                          "--slots", "1", "--types", "subyard-pair"]), redirect_stdout(io.StringIO()):
                self.assertEqual(m.main(), 1)
            self.assertEqual(before, set(checkout.rglob("*")))

    def test_first_order_dynamic_dispatch_resume_and_exclusions(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            checks = {name: ["true"] for name in ("verify", "compatibility", "p0-release-smoke", "profile:sample/one", "profile:sample/two", "profile:sample/three")}
            receipt = {"version": "test", "profiles": {}, "source_fingerprint": "a" * 64,
                       "candidate_bundle_sha256": "b" * 64, "external_obligations": {},
                       "checks": {name: {"status": "pending"} for name in checks}}
            started = []
            active = set()
            lock = threading.Lock()
            release_slow = threading.Event()
            def execute(name, command, root, output, env, slot=None):
                with lock:
                    self.assertNotIn(slot, active if slot else set())
                    if slot:
                        active.add(slot)
                    started.append((name, slot))
                if name == "profile:sample/one":
                    self.assertTrue(release_slow.wait(3))
                if name == "p0-release-smoke":
                    release_slow.set()
                with lock:
                    active.discard(slot)
                return {"status": "passed", "exit_code": 0}
            with patch.object(m, "load_bound", return_value=(output / "source", receipt)), \
                 patch.object(m, "commands", return_value=checks), \
                 patch.object(m, "source_fingerprint", return_value="a" * 64), \
                 patch.object(m, "execute", side_effect=execute):
                self.assertEqual(m.run_checks(output, [1, 2], [], False,
                    first=["profile:sample/one", "profile:sample/two"], exclude=["profile:sample/three"]), 1)
                self.assertEqual([name for name, slot in started[:2]], ["verify", "compatibility"])
                self.assertEqual([name for name, slot in started[2:]], ["profile:sample/one", "profile:sample/two", "p0-release-smoke"])
                self.assertEqual(started[3][1], started[4][1])
                started.clear()
                self.assertEqual(m.run_checks(output, [1], ["profile:sample"], False), 0)
                self.assertEqual(started, [("profile:sample/three", 1)])

    def test_capacity_phase_heartbeat_and_interruption_evidence(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            script = output / "check.sh"
            script.write_text("#!/bin/bash\nprintf 'E2E_PHASE phase=allocation state=start duration_seconds=0 exit_code=0\\n'\nprintf 'agent-e2e: retryable capacity refusal for slot-1: resource=memory; retry after resources become available\\n'\nexit 4\n")
            m.STOP.clear()
            result = m.execute("profile:sample/one", ["bash", str(script)], output, output, m.clean_environment(), 1)
            self.assertEqual((result["status"], result["exit_code"], result["reason"]), ("blocked", 4, "capacity_memory"))
            self.assertEqual(result["log_sha256"], m.digest(output / result["log"]))
            script.write_text("#!/bin/bash\nprintf 'E2E_PHASE phase=guest state=start duration_seconds=0 exit_code=0 vm=1\\n'\nsleep 10\n")
            timer = threading.Timer(0.4, m.STOP.set)
            timer.start()
            try:
                with patch.object(m, "HEARTBEAT_SECONDS", 0.05):
                    result = m.execute("profile:sample/two", ["bash", str(script)], output, output, m.clean_environment(), 1)
            finally:
                timer.cancel()
                m.STOP.clear()
            self.assertEqual(result["status"], "interrupted")
            self.assertGreater(result["duration_seconds"], 0)
            self.assertNotEqual(result["exit_code"], 0)
            events = [json.loads(line) for line in (output / "events.jsonl").read_text().splitlines()]
            self.assertTrue(any(item["event"] == "heartbeat" for item in events))
            self.assertTrue(any(item["event"] == "controller-phase" and item["phase"] == "guest" and item["state"] == "start" for item in events))
            self.assertFalse(any(item["event"] == "controller-phase" and item["phase"] == "guest" and item["state"] == "end" for item in events))
            self.assertEqual(events[-1]["status"], "interrupted")
            log = output / "refusal.log"
            log.write_text("agent-e2e: retryable capacity refusal for slot-1: resource=disk; retry after resources become available\nE2E lease: slot=slot-1\nproduct failure\n")
            self.assertIsNone(m.capacity_reason(log))

    def test_explicit_import_identity_closure_and_tamper_guards(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            checkout = fixture_checkout(directory)
            first = checkout / ".build/first"
            source_root, source = prepare_fixture(checkout, first)
            name = "profile:sample/one"
            source["checks"][name] = m.execute(name, m.commands(source_root, "test", source["profiles"])[name],
                                              source_root, first, m.clean_environment(), 1)
            source["checks"]["verify"] = m.execute("verify", ["make", "verify"], source_root, first, m.clean_environment())
            source["checks"]["verify"]["execution_environment"] = source["environment"]
            m.save(first, source)
            second = checkout / ".build/second"
            target_root, target = prepare_fixture(checkout, second)
            self.assertEqual(m.import_evidence(second, first, [name]), 0)
            _, target = m.load_bound(second)
            self.assertTrue(target["checks"][name]["inherited"])
            self.assertEqual(target["checks"][name]["from_receipt_sha256"], m.digest(first / "receipt.json"))
            self.assertEqual(target["checks"]["profile:sample/two"]["status"], "pending")
            with redirect_stdout(io.StringIO()):
                frozen_report = m.preflight(second)
            self.assertEqual(frozen_report["candidate"], "frozen")
            self.assertNotIn(name, frozen_report["pending"])
            with self.assertRaisesRegex(ValueError, "passing command-bound"):
                m.import_evidence(second, first, ["profile:sample/two"])
            (checkout / "config/profiles/peer/tests/e2e/acceptance.sh").write_text("#!/bin/bash\n# independent peer change\nexit 0\n")
            third = checkout / ".build/third"
            prepare_fixture(checkout, third)
            with self.assertRaisesRegex(ValueError, "source identity differs"):
                m.import_evidence(third, first, [name])
            self.assertEqual(m.import_evidence(third, first, [name], True), 0)
            _, inherited = m.load_bound(third)
            self.assertEqual(inherited["checks"][name]["inherited_from"]["scope"], "other-profile-tests-only")
            self.assertEqual(len(inherited["checks"][name]["inherited_from"]["closure_sha256"]), 64)
            (checkout / "config/profiles/sample/tests/e2e/acceptance.sh").write_text("#!/bin/bash\n# own input change\nexit 0\n")
            fourth = checkout / ".build/fourth"
            prepare_fixture(checkout, fourth)
            with self.assertRaisesRegex(ValueError, "owning profile or shared"):
                m.import_evidence(fourth, first, [name], True)
            with self.assertRaisesRegex(ValueError, "source identity differs"):
                m.import_evidence(third, first, ["verify"], True)
            (checkout / "config/profiles/sample/tests/e2e/acceptance.sh").write_text("#!/bin/bash\nexit 0\n")
            (checkout / "shared-input.txt").write_text("shared input change\n")
            fifth = checkout / ".build/fifth"
            prepare_fixture(checkout, fifth)
            with self.assertRaisesRegex(ValueError, "owning profile or shared"):
                m.import_evidence(fifth, first, [name], True)
            package = checkout / "dev/package-engine.sh"
            package.write_text(package.read_text().replace("packaged candidate", "different runtime"))
            sixth = checkout / ".build/sixth"
            prepare_fixture(checkout, sixth)
            with self.assertRaisesRegex(ValueError, "candidate identity differs"):
                m.import_evidence(sixth, first, [name], True)
            (third / inherited["checks"][name]["log"]).write_text("tampered\n")
            with self.assertRaisesRegex(ValueError, "passing result evidence"):
                m.load_bound(third)
            m.supersede(second)
            _, target = m.load_bound(second)
            self.assertTrue(target["superseded"])
            self.assertEqual(target["result"], "incomplete")
            self.assertEqual(target["checks"][name]["status"], "passed")
            with self.assertRaisesRegex(ValueError, "superseded"):
                m.run_checks(second, [1], [], False)

    def test_own_group_drain_waits_for_child_cleanup_and_kills_term_ignored_child(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            parent = output / "parent.py"
            child = output / "child.py"
            ready = output / "ready"
            finished = output / "finished"
            child.write_text('''import signal, sys, time
from pathlib import Path
def finish(signum, frame):
    time.sleep(0.3)
    Path(sys.argv[2]).write_text("cleanup completed")
    print("child cleanup completed", flush=True)
    sys.exit(0)
signal.signal(signal.SIGTERM, finish)
Path(sys.argv[1]).write_text("ready")
while True: time.sleep(1)
''')
            parent.write_text('''import subprocess, sys, time
from pathlib import Path
subprocess.Popen([sys.executable, sys.argv[1], sys.argv[2], sys.argv[3]])
while not Path(sys.argv[2]).exists(): time.sleep(0.01)
while True: time.sleep(1)
''')
            def interrupt_when_ready():
                deadline = time.monotonic() + 3
                while not ready.exists() and time.monotonic() < deadline:
                    time.sleep(0.01)
                m.STOP.set()
            for ignores_term in (False, True):
                ready.unlink(missing_ok=True)
                finished.unlink(missing_ok=True)
                if ignores_term:
                    child.write_text(child.read_text().replace("signal.signal(signal.SIGTERM, finish)",
                                                             "signal.signal(signal.SIGTERM, signal.SIG_IGN)"))
                m.STOP.clear()
                timer = threading.Thread(target=interrupt_when_ready)
                timer.start()
                try:
                    with patch.object(m, "TERM_GRACE_SECONDS", 0.5), patch.object(m, "KILL_GRACE_SECONDS", 1):
                        result = m.execute("profile:sample/one", [sys.executable, str(parent), str(child), str(ready), str(finished)],
                                           output, output, m.clean_environment(), 1)
                finally:
                    timer.join(3)
                    m.STOP.clear()
                self.assertEqual(result["cleanup_status"], "drained")
                self.assertEqual(result["status"], "interrupted")
                self.assertEqual(result["exit_code"], -15)
                if not ignores_term:
                    self.assertTrue(finished.exists())
                    self.assertIn("child cleanup completed", (output / result["log"]).read_text())
                self.assertEqual(result["log_sha256"], m.digest(output / result["log"]))

    def test_execution_environment_drift_rejected_before_any_check(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            receipt = {"version": "test", "profiles": {}, "checks": {},
                       "environment": {"system": "Linux", "machine": "old-machine", "go": "old-toolchain"}}
            with patch.object(m, "load_bound", return_value=(output / "source", receipt)), \
                 patch.object(m, "execution_environment", return_value={"system": "Linux", "machine": "new-machine", "go": "new-toolchain"}), \
                 patch.object(m, "execute", side_effect=AssertionError("check executed")):
                with self.assertRaisesRegex(ValueError, "environment changed"):
                    m.run_checks(output, [1], [], False)

    def test_provenance_generation_time_only_reuse_preserves_raw_identities(self):
        with tempfile.TemporaryDirectory() as temporary:
            checkout = fixture_checkout(Path(temporary))
            first = checkout / ".build/provenance-first"
            source_root, source = prepare_fixture(checkout, first)
            name = "profile:sample/one"
            source["checks"][name] = m.execute(name, m.commands(source_root, "test", source["profiles"])[name],
                                              source_root, first, m.clean_environment(), 1)
            m.save(first, source)
            second = checkout / ".build/provenance-second"
            with patch.dict(os.environ, {"SUBYARD_FIXTURE_GENERATED_AT": "2026-01-02T00:00:00Z"}):
                _, target = prepare_fixture(checkout, second)
            self.assertEqual(source["source_fingerprint"], target["source_fingerprint"])
            self.assertEqual(source["runtime_sha256"], target["runtime_sha256"])
            self.assertNotEqual(source["release_files"], target["release_files"])
            self.assertEqual(m.import_evidence(second, first, [name]), 0)
            target_root, inherited = m.load_bound(second)
            binding = inherited["checks"][name]["inherited_from"]["release_provenance"]
            self.assertEqual(binding["scope"], "generated-at-only")
            self.assertEqual(len(binding["files"]), 2)
            for filename, evidence in binding["files"].items():
                self.assertEqual(evidence["source_sha256"], source["release_files"][filename])
                self.assertEqual(evidence["target_sha256"], target["release_files"][filename])
                self.assertEqual(evidence["source_generated_at"], "2026-01-01T00:00:00Z")
                self.assertEqual(evidence["target_generated_at"], "2026-01-02T00:00:00Z")
            (checkout / "config/profiles/peer/tests/e2e/acceptance.sh").write_text("#!/bin/bash\n# other profile test change\nexit 0\n")
            third = checkout / ".build/provenance-third"
            with patch.dict(os.environ, {"SUBYARD_FIXTURE_GENERATED_AT": "2026-01-03T00:00:00Z"}):
                prepare_fixture(checkout, third)
            self.assertEqual(m.import_evidence(third, first, [name], True), 0)
            _, combined = m.load_bound(third)
            self.assertEqual(combined["checks"][name]["inherited_from"]["scope"], "other-profile-tests-only")
            self.assertEqual(combined["checks"][name]["inherited_from"]["release_provenance"]["scope"], "generated-at-only")
            for index, overrides in enumerate(({"sourceRevision": "a" * 40}, {"sha256": "b" * 64},
                {"version": "different"}, {"canonicalRepository": "different"}, {"unknownField": "unknown"},
                {"generatedAt": "malformed"})):
                with self.subTest(overrides=overrides), patch.dict(os.environ, {
                    "SUBYARD_FIXTURE_GENERATED_AT": "2026-01-04T00:00:00Z",
                    "SUBYARD_FIXTURE_PROVENANCE_OVERRIDES": json.dumps(overrides)}):
                    invalid = checkout / f".build/provenance-invalid-{index}"
                    prepare_fixture(checkout, invalid)
                    with self.assertRaisesRegex(ValueError, "provenance"):
                        m.import_evidence(invalid, first, [name], True)
            filename = next(iter(binding["files"]))
            metadata = target_root / ".subyard-acceptance/release" / filename
            original = metadata.read_text()
            for malformed in ("{", "{}", original[:-2] + ',"schemaVersion":1}\n'):
                metadata.write_text(malformed)
                with self.assertRaises(ValueError):
                    m.release_provenance_identity(target_root, target, filename)
            metadata.write_text(original)
            inherited["checks"][name]["inherited_from"]["release_provenance"]["files"][filename]["target_sha256"] = "c" * 64
            m.save(second, inherited)
            with self.assertRaisesRegex(ValueError, "inherited release provenance changed"):
                m.load_bound(second)

    def test_broker_headroom_subtracts_reservations_reserves_and_builder(self):
        with tempfile.TemporaryDirectory() as temporary:
            status = Path(temporary) / "status.json"
            value = {"schema_version": 1, "status": "ok", "pool": {"slots": []}, "resources": {
                "memory": {"available_bytes": 1000}, "storage": {"physical_free_bytes": 2000, "budget_used_bytes": 100},
                "budgets": {"memory_reserve_bytes": 100, "disk_reserve_bytes": 200, "disk_bytes": 0},
                "slots": [{"remaining_memory_growth_bytes": 300, "remaining_disk_growth_bytes": 400}],
                "builder": {"reserved_memory_bytes": 50, "reserved_disk_peak_bytes": 60}}}
            status.write_text(json.dumps(value))
            headroom = m.public_broker_status(status)["headroom"]
            self.assertEqual((headroom["memory_bytes"], headroom["disk_bytes"]), (550, 1340))
            value["resources"]["budgets"]["disk_bytes"] = 1000
            status.write_text(json.dumps(value))
            self.assertEqual(m.public_broker_status(status)["headroom"]["disk_bytes"], 440)
            del value["resources"]["slots"][0]["remaining_memory_growth_bytes"]
            status.write_text(json.dumps(value))
            self.assertIsNone(m.public_broker_status(status)["headroom"]["memory_bytes"])
            status.write_text('{"schema_version":1,"status":"error","message":"do-not-export"}')
            self.assertNotIn("do-not-export", json.dumps(m.public_broker_status(status)))

    def test_long_partial_lines_terminal_phases_and_environment_projection(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            log = output / "markers.log"
            log.write_bytes(b"x" * 12000)
            seen = set()
            offset = m.phase_events(output, "profile:sample/one", log, 0, seen)
            with log.open("ab") as stream:
                stream.write(b"\n")
                for _ in range(220):
                    stream.write(b"ordinary controller line\n")
                for kind, count, fingerprint in ((b"subyard-pair", b"2", b"a"),
                                                  (b"subyard-pair", b"1", b"b"),
                                                  (b"android-test", b"1", b"c"),
                                                  (b"android-test", b"2", b"d")):
                    stream.write(b"E2E lease: secret-do-not-export type=" + kind + b" vms=" + count
                                 + b" base=" + fingerprint * 64 + b"\n")
                stream.write(b"E2E_PHASE phase=cleanup/release state=end duration_seconds=3 exit_code=0\n")
                stream.write(b"E2E_PHASE phase=guest state=start duration_seconds=4 exit_code=0 vm=1\n")
            m.phase_events(output, "profile:sample/one", log, offset, seen, final=True)
            events = [json.loads(line) for line in (output / "events.jsonl").read_text().splitlines()]
            self.assertEqual([item["event"] for item in events], ["environment"] * 3 + ["controller-phase"])
            environments = [(item["type"], item["vm_count"], item["base_fingerprint"]) for item in events[:3]]
            self.assertEqual(environments, [("subyard-pair", 2, "a" * 64),
                                            ("subyard-pair", 1, "b" * 64),
                                            ("android-test", 1, "c" * 64)])
            self.assertEqual({marker for marker in seen if marker[0] == "environment"},
                             {("environment", *environment) for environment in environments})
            self.assertEqual(events[3]["phase"], "cleanup/release")
            self.assertNotIn("secret-do-not-export", json.dumps(events))

    def test_fixture_phases_are_bounded_and_preserve_failure(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            log = output / "markers.log"
            log.write_bytes(
                b"E2E_PHASE phase=fixture/setup state=start duration_seconds=0 exit_code=0\n"
                b"E2E_PHASE phase=fixture/setup state=end duration_seconds=17 exit_code=23\n"
                b"E2E_PHASE phase=fixture/cleanup state=start duration_seconds=0 exit_code=0\n"
                b"E2E_PHASE phase=fixture/cleanup state=end duration_seconds=2 exit_code=0\n"
                b"E2E_PHASE phase=fixture/private=do-not-export state=end duration_seconds=1 exit_code=0\n"
                b"E2E_PHASE phase=fixture/setup state=start duration_seconds=1 exit_code=0\n"
                b"E2E_PHASE phase=fixture/setup state=end duration_seconds=1 exit_code=256\n"
                b"E2E_PHASE phase=fixture/setup state=end duration_seconds=-1 exit_code=0\n"
                b"E2E_PHASE phase=fixture/setup state=end duration_seconds=1 exit_code=0 vm=1\n"
                b"E2E_PHASE phase=fixture/" + b"x" * 61 +
                b" state=end duration_seconds=1 exit_code=0\n" + b"x" * 5000 + b"\n"
                b"E2E_PHASE phase=fixture/unfinished state=start duration_seconds=0 exit_code=0\n"
                b"E2E_PHASE phase=fixture/unfinished state=end duration_seconds=1 exit_code=0")
            m.phase_events(output, "profile:sample", log, 0, set(), final=True)
            events = [json.loads(line) for line in (output / "events.jsonl").read_text().splitlines()]
            self.assertEqual([(item["phase"], item["state"], item["duration_seconds"], item["exit_code"])
                              for item in events], [
                ("fixture/setup", "start", 0, 0), ("fixture/setup", "end", 17, 23),
                ("fixture/cleanup", "start", 0, 0), ("fixture/cleanup", "end", 2, 0),
                ("fixture/unfinished", "start", 0, 0)])
            self.assertNotIn("do-not-export", json.dumps(events))

    def test_interrupted_attempt_saved_and_blocked_worker_stops_queue(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            names = ["verify", "profile:sample/one", "profile:sample/two"]
            checks = {name: ["true"] for name in names}
            receipt = {"version": "test", "profiles": {}, "source_fingerprint": "a" * 64,
                       "candidate_bundle_sha256": "b" * 64, "external_obligations": {},
                       "checks": {name: {"status": "passed" if name == "verify" else "pending"} for name in names}}
            calls = []
            def execute(name, command, root, output, env, slot=None):
                calls.append(name)
                return {"status": "blocked", "exit_code": 4, "duration_seconds": 1.2}
            with patch.object(m, "load_bound", return_value=(output / "source", receipt)), \
                 patch.object(m, "commands", return_value=checks), \
                 patch.object(m, "source_fingerprint", return_value="a" * 64), \
                 patch.object(m, "execute", side_effect=execute):
                self.assertEqual(m.run_checks(output, [1], [], False), 1)
                self.assertEqual(calls, ["profile:sample/one"])
                persisted = json.loads((output / "receipt.json").read_text())
                self.assertEqual(persisted["checks"]["profile:sample/two"]["status"], "pending")
                self.assertEqual(persisted["checks"]["profile:sample/one"]["exit_code"], 4)
                self.assertEqual(persisted["result"], "incomplete")
                def interrupt(name, *args):
                    m.STOP.set()
                    return {"status": "interrupted", "exit_code": -15, "duration_seconds": 0.7}
                with patch.object(m, "execute", side_effect=interrupt):
                    self.assertEqual(m.run_checks(output, [1], [], False), 1)
                persisted = json.loads((output / "receipt.json").read_text())
                self.assertEqual(persisted["checks"]["profile:sample/one"]["status"], "interrupted")
                self.assertEqual(persisted["checks"]["profile:sample/one"]["duration_seconds"], 0.7)
                self.assertEqual(persisted["checks"]["verify"]["status"], "passed")
                m.STOP.clear()
                calls.clear()
                def pass_after_capacity_change(name, *args):
                    calls.append(name)
                    return {"status": "passed", "exit_code": 0, "duration_seconds": 1}
                with patch.object(m, "execute", side_effect=pass_after_capacity_change):
                    self.assertEqual(m.run_checks(output, [1], ["profile:sample/one"], False), 1)
                persisted = json.loads((output / "receipt.json").read_text())
                self.assertEqual(calls, ["profile:sample/one"])
                self.assertEqual(persisted["checks"]["verify"]["status"], "passed")
                self.assertEqual(persisted["checks"]["profile:sample/two"]["status"], "pending")
                self.assertEqual(persisted["source_fingerprint"], "a" * 64)
                self.assertEqual(persisted["candidate_bundle_sha256"], "b" * 64)
                self.assertEqual(persisted["result"], "incomplete")

    def test_single_collector_lock(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            with m.acceptance_lock(output):
                with self.assertRaisesRegex(ValueError, "already in use"):
                    with m.acceptance_lock(output):
                        self.fail("second collector acquired lock")
            self.assertEqual((output / "collector.lock").stat().st_mode & 0o777, 0o600)


unittest.main()
PY
