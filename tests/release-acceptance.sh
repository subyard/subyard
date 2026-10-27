#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PYTHONDONTWRITEBYTECODE=1 python3 -B - "$ROOT" <<'PY'
import copy
import importlib.util
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
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


def fixture_checkout(directory, optional_vms=False):
    checkout = directory / "checkout"
    for name in ("dev/agent-e2e.sh", "dev/e2e/p0-acceptance.sh", "scripts/lib/runtime.sh", "tests/helpers/source-files.sh"):
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
    if optional_vms:
        shutil.copy2(root / "dev/release-acceptance.py", checkout / "dev/release-acceptance.py")
    return checkout


def prepare_fixture(checkout, output):
    check_output = m.subprocess.check_output
    def tools(command, **kwargs):
        return "go version fixture" if command == ["go", "version"] else check_output(command, **kwargs)
    with patch.object(m, "ROOT", checkout), patch.object(m.subprocess, "check_output", side_effect=tools):
        m.prepare(output, "test")
    return m.load_bound(output)


class TestAcceptance(unittest.TestCase):
    def test_owning_preflight_reuses_only_validated_local_receipt(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            directory = Path(temporary)
            checkout = fixture_checkout(directory)
            for name in ('dev/release-acceptance.py', 'config/profiles/github/tests/run.sh'):
                target = checkout/name
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(root/name, target)
            output = directory/'accepted'
            source, receipt = prepare_fixture(checkout, output)
            receipt['checks']['verify'] = m.execute('verify', ['make','verify'], source, output, m.clean_environment())
            receipt['checks']['verify']['execution_environment'] = receipt['environment']
            m.save(output, receipt)
            tools = directory/'tools'
            tools.mkdir()
            go = tools/'go'
            go.write_text('#!/bin/sh\n[ "$*" = version ] || exit 99\nprintf "go version fixture\\n"\n')
            go.chmod(0o700)
            env = dict(os.environ, PATH=str(tools)+':'+os.environ['PATH'], SUBYARD_E2E_LOCAL_EVIDENCE=str(output))
            runner = source/'config/profiles/github/tests/run.sh'
            result = subprocess.run(['bash',str(runner),'--preflight'], env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn('reuse validated make verify PASS', result.stdout)
            receipt['checks']['verify']['command'] = ['true']
            m.save(output, receipt)
            result = subprocess.run(['bash',str(runner),'--preflight'], env=env, capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)

    def test_fast_default_keeps_vm_inventory_honest_and_source_bound(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            directory = Path(temporary)
            checkout = fixture_checkout(directory, optional_vms=True)
            output = directory / "candidate"
            source, receipt = prepare_fixture(checkout, output)
            live = [name for name in receipt["checks"] if m.is_live(name)]
            self.assertTrue(live)
            self.assertTrue(all(receipt["checks"][name] == m.OPTIONAL_VM_NOT_RUN for name in live))
            frozen = (output / "receipt.json").read_bytes()
            with redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()):
                report = m.preflight(output, slots=[1])
                requested = m.preflight(output, slots=[1], with_vms=True)
            self.assertEqual((output / "receipt.json").read_bytes(), frozen)
            self.assertTrue(all(not item["selected"] and not item["required"] for item in report["checks"] if item["physical"]))
            self.assertTrue(all(item["selected"] and item["required"] for item in requested["checks"] if item["physical"]))
            self.assertEqual(report["capacity_summary"]["planning"], "not-required")
            calls, execute = [], m.execute
            def local(name, command, root, output, env, slot=None):
                self.assertFalse(m.is_live(name), "slots alone dispatched a VM")
                calls.append(name)
                result = execute(name, ["true"], root, output, env)
                result["command"] = m.command_identity(command, root)
                return result
            with patch.object(m, "execution_environment", return_value=receipt["environment"]), patch.object(m, "execute", side_effect=local):
                self.assertEqual(m.run_checks(output, [1], [], False), 0)
                self.assertEqual(m.run_checks(output, [1], [], False, exclude=live), 0)
                self.assertEqual(m.run_checks(output, [], ["verify"], False), 0)
            self.assertEqual(calls, ["verify", "shellcheck", "local-adapters", "compatibility"])
            _, receipt = m.load_bound(output)
            self.assertEqual(receipt["result"], "passed")
            self.assertTrue(all(receipt["checks"][name] == m.OPTIONAL_VM_NOT_RUN for name in live))
            self.assertEqual(set(receipt["profile_results"].values()), {"not-run"})
            for name, broken in ((live[0], {**m.OPTIONAL_VM_NOT_RUN, "log_sha256": "a" * 64}),
                                 ("verify", m.OPTIONAL_VM_NOT_RUN.copy())):
                original = receipt["checks"][name]
                receipt["checks"][name] = broken
                m.save(output, receipt)
                with self.assertRaisesRegex(ValueError, "optional VM"):
                    m.load_bound(output)
                receipt["checks"][name] = original
            m.save(output, receipt)
            # Historical source cannot gain the new policy by editing its receipt.
            legacy = fixture_checkout(directory / "legacy")
            legacy_output = directory / "legacy-candidate"
            _, old = prepare_fixture(legacy, legacy_output)
            old["checks"][live[0]] = m.OPTIONAL_VM_NOT_RUN.copy()
            m.save(legacy_output, old)
            with self.assertRaisesRegex(ValueError, "frozen policy"):
                m.load_bound(legacy_output)

    def test_optional_vm_reset_cannot_erase_retained_attempt_or_request(self):
        for state in ("failed", "attempt-only", "requested", "requested-excluded"):
            with self.subTest(state=state), tempfile.TemporaryDirectory(dir="/tmp") as temporary:
                directory = Path(temporary)
                checkout = fixture_checkout(directory, optional_vms=True)
                runner = checkout / "config/profiles/sample/tests/e2e/acceptance.sh"
                runner.write_text("#!/bin/bash\nexit 23\n")
                output = directory / "candidate"
                source, receipt = prepare_fixture(checkout, output)
                name = "profile:sample/one"
                if state in ("failed", "attempt-only"):
                    command = m.commands(source, "test", receipt["profiles"])[name]
                    result = m.execute(name, command, source, output, m.clean_environment())
                    self.assertEqual(result["exit_code"], 23)
                    result["execution_environment"] = receipt["environment"]
                    result["original_attempt"] = m.completed_attempt(output, source, receipt, name, result)
                    receipt["checks"][name] = result
                    m.save(output, receipt)
                else:
                    with patch.object(m, "execution_environment", return_value=receipt["environment"]):
                        m.STOP.set()
                        try:
                            self.assertEqual(m.run_checks(output, [1], [name], False,
                                exclude=[name] if state == "requested-excluded" else []), 1)
                        finally:
                            m.STOP.clear()
                    receipt = m.load_bound(output)[1]
                    self.assertEqual(receipt["checks"][name], {"status": "pending"})
                    (output / "events.jsonl").unlink()  # The invocation alone retains this request.
                if state == "attempt-only":
                    (output / result["log"]).unlink()
                    (output / "events.jsonl").unlink()
                receipt["checks"][name] = m.OPTIONAL_VM_NOT_RUN.copy()
                m.save(output, receipt)
                before = (output / "receipt.json").read_bytes()
                with self.assertRaisesRegex(ValueError, "optional VM.*(request|execution|attempt)"):
                    m.load_bound(output)
                self.assertEqual((output / "receipt.json").read_bytes(), before)

    def test_optional_vm_history_fails_closed_on_corrupt_retained_records(self):
        name = "profile:sample/one"
        for filename, value in (("events.jsonl", "[]\n"), ("events.jsonl", '{"checks":null}\n'),
                                ("runs/sample.json", '{"schema_version":1,"selected":null}'),
                                ("runs/sample.json", '{"schema_version":1,"selected":[],"requested":42}'),
                                ("attempts/sample.json", "{}"), ("events.jsonl", '{"event":')):
            with self.subTest(filename=filename, value=value), tempfile.TemporaryDirectory(dir="/tmp") as temporary:
                output = Path(temporary)
                path = output / filename
                path.parent.mkdir(exist_ok=True)
                path.write_text(value)
                with self.assertRaises(ValueError):
                    m.validate_optional_vm_history(output, {name})
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            output = Path(temporary)
            m.event(output, "run-start", checks=[name])
            with self.assertRaisesRegex(ValueError, "previous request"):
                m.validate_optional_vm_history(output, {name})

    def test_requested_vm_remains_required_after_stop_exclusion_or_failed_attempt(self):
        # Scheduler fixture: the real binding and immutable attempts are covered above and below.
        self.enterContext(patch.object(m, "completed_attempt", return_value={"id": "a" * 32, "sha256": "b" * 64}))
        for mode in ("only", "with-vms", "first", "excluded", "failed", "blocked", "running", "malformed"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory(dir="/tmp") as temporary:
                output = Path(temporary)
                name = "p0-release-smoke"
                vm = m.OPTIONAL_VM_NOT_RUN.copy()
                if mode in ("failed", "blocked", "running"):
                    vm = {"status": mode}
                elif mode == "malformed":
                    vm["exit_code"] = 0
                receipt = {"version": "test", "source_fingerprint": "a" * 64,
                    "candidate_bundle_sha256": "b" * 64, "profiles": {}, "external_obligations": {},
                    "environment": {}, "checks": {"verify": {"status": "passed"}, name: vm}}
                with patch.object(m, "load_bound", return_value=(output, receipt)), \
                     patch.object(m, "execution_environment", return_value={}), \
                     patch.object(m, "commands", return_value={key: ["true"] for key in receipt["checks"]}), \
                     patch.object(m, "source_fingerprint", return_value="a" * 64), \
                     patch.object(m, "execute", side_effect=AssertionError("stopped/unrequested VM ran")):
                    if mode in ("only", "with-vms", "first", "excluded"):
                        m.STOP.set()
                    try:
                        self.assertEqual(m.run_checks(output, [1], [name] if mode == "only" else [], False,
                            first=[name] if mode == "first" else [], exclude=[name] if mode in ("excluded", "failed", "blocked", "running", "malformed") else [],
                            with_vms=mode in ("with-vms", "excluded")), 1)
                    finally:
                        m.STOP.clear()
                saved = json.loads((output / "receipt.json").read_text())
                self.assertNotEqual(saved["checks"][name], m.OPTIONAL_VM_NOT_RUN)
                self.assertNotEqual(saved["result"], "passed")

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
        # This scheduler/reporting fixture supplies synthetic receipts; immutable evidence has separate real fixtures.
        self.enterContext(patch.object(m, "completed_attempt", return_value={"id": "a" * 32, "sha256": "b" * 64}))
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
                self.assertEqual(m.run_checks(output, [1], [], False, with_vms=True), 0)
                self.assertEqual(calls, ["p0-release-smoke"])
                self.assertEqual(m.run_checks(output, [], ["process-coverage"], False), 0)
                self.assertEqual(calls, ["p0-release-smoke", "process-coverage"])

    def transfer_fixture(self, directory, standalone=False, core=False):
        checkout = fixture_checkout(directory)
        if core:
            # Synthetic known producer: exercise parsing and binding without executing it.
            controller = checkout / "dev/e2e/p0-acceptance.sh"
            controller.write_text("#!/bin/bash\nexit 0\n")
            known_digest = m.digest(controller)  # Freeze before preparing either candidate.
            self.enterContext(patch.object(m, "HISTORICAL_P0_SHA256", known_digest))
        first, second = directory / "old", directory / "new"
        old_root, old = prepare_fixture(checkout, first)
        name = "p0-release-smoke" if core else "profile:sample/one" if standalone else "verify"
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

    def test_source_fingerprint_preserves_raw_path_mode_and_link_order(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            directory = Path(temporary)
            paths = [b"executable", b"link", b"\xff", "\uf000".encode()]
            for raw in paths:
                path = directory / os.fsdecode(raw)
                if raw == b"link":
                    path.symlink_to("missing-target")
                else:
                    path.write_bytes(b"source bytes " + raw)
                    path.chmod(0o755 if raw == b"executable" else 0o644)
            expected = hashlib.sha256()
            for raw in sorted(paths):
                path = directory / os.fsdecode(raw)
                expected.update(raw + b"\0" + (b"755\0" if path.lstat().st_mode & 0o111 else b"644\0"))
                expected.update(b"link\0missing-target" if path.is_symlink() else bytes.fromhex(m.digest(path)))
            with patch.object(m.subprocess, "check_output", return_value=b"\0".join(reversed(paths)) + b"\0") as listing:
                self.assertEqual(m.source_fingerprint(directory), expected.hexdigest())
            self.assertEqual(listing.call_count, 1)

    def test_single_transfer_uses_shared_writer_without_batch_admission_guards(self):
        for version in (1, 2):
            with self.subTest(version=version), tempfile.TemporaryDirectory(dir="/tmp") as temporary:
                directory = Path(temporary)
                fixture = self.transfer_fixture if version == 1 else self.immutable_transfer_fixture
                source, _, name, declaration, review = fixture(directory)
                checkout = directory / "checkout"
                lanes = checkout / "config/profiles/peer/tests/e2e/acceptance-lanes.json"
                lanes.write_text(json.dumps({"schema_version": 1, "lanes": [{"id": "new", "arguments": ["--lane", "new"]}]}))
                target = directory / "third"
                target_root, receipt = prepare_fixture(checkout, target)
                receipt["environment"]["go"] = "go version another valid preparation"
                m.save(target, receipt)
                if version == 1:
                    review["target"] = m.transfer_candidate(receipt)
                else:
                    review["target_manifest_sha256"] = m.evidence_hash(m.candidate_manifest(target_root, receipt))
                declaration.write_text(json.dumps(review))
                before = (target / "receipt.json").read_bytes()
                with patch.object(m.shutil, "copyfileobj", side_effect=OSError("copy interrupted")):
                    with self.assertRaises(OSError):
                        m.transfer_evidence(target, source, name, declaration)
                self.assertEqual((target / "receipt.json").read_bytes(), before)
                self.assertEqual(list(target.glob("transfer-*")), [])
                self.assertEqual(list(target.glob(".transfer-*")), [])
                captured = io.StringIO()
                with patch.object(m, "event", side_effect=OSError("event capture unavailable")), redirect_stdout(captured):
                    with self.assertRaisesRegex(ValueError, "commit state committed"):
                        m.transfer_evidence(target, source, name, declaration)
                self.assertIn("TRANSFER state=committed", captured.getvalue())
                _, committed = m.load_bound(target)
                self.assertEqual(committed["checks"][name]["status"], "passed")
                self.assertNotIn("transfer_batch_sha256", committed["checks"][name])
                self.assertTrue(all((target / path).exists() for path in committed["checks"][name]["evidence_files"].values()))

    def batch_fixture(self, directory):
        checkout = fixture_checkout(directory)
        adapter = checkout / "tests/real-host/adapter-contracts.sh"
        adapter.parent.mkdir(parents=True)
        adapter.write_text("#!/bin/bash\nexit 0\n")
        source, target = directory / "source-candidate", directory / "target-candidate"
        source_root, old = prepare_fixture(checkout, source)
        names = ("verify", "local-adapters")
        for name in names:
            old["checks"][name] = m.execute(name, m.commands(source_root, "test", old["profiles"])[name], source_root, source, m.clean_environment())
            result = old["checks"][name]
            result["execution_environment"] = old["environment"]
            result["original_attempt"] = m.completed_attempt(source, source_root, old, name, result)
        m.save(source, old)
        (checkout / "reviewed-document.txt").write_text("Unrelated reviewed fixture documentation\n")
        with patch.dict(os.environ, {"SUBYARD_FIXTURE_GENERATED_AT": "2026-01-02T00:00:00Z"}):
            _, current = prepare_fixture(checkout, target)
        request = {"schema_version": 1, "target": m.transfer_candidate(current), "checks": [
            {"check": name, "source": str(source), "attempt": old["checks"][name]["original_attempt"], "review": None} for name in names]}
        path = directory / "batch.json"
        path.write_text(json.dumps(request))
        with redirect_stdout(io.StringIO()):
            report = m.transfer_batch(target, path, plan=True)
        for item, decision in zip(request["checks"], report["checks"]):
            review = decision["review_draft"]
            review["review"] = {"unchanged_scope": "Only synthetic documentation changed", "reason": "Reviewed fixture delta",
                                "authorization_reference": "operator:batch-fixture"}
            review_path = directory / (item["check"] + "-review.json")
            review_path.write_text(json.dumps(review))
            item["review"] = {"path": str(review_path), "sha256": m.digest(review_path)}
        path.write_text(json.dumps(request))
        return source, target, path, request

    def test_batch_plan_scans_unique_candidates_once_and_does_not_mutate_evidence(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            source, target, path, request = self.batch_fixture(Path(temporary))
            before = (target / "receipt.json").read_bytes()
            inventory = sorted(str(item.relative_to(target)) for item in target.rglob("*"))
            with patch.object(m, "source_entries", wraps=m.source_entries) as scans, \
                 patch.object(m, "source_fingerprint", side_effect=AssertionError("redundant source fingerprint scan")), \
                 redirect_stdout(io.StringIO()):
                report = m.transfer_batch(target, path, plan=True)
            self.assertEqual(scans.call_count, 2)
            self.assertEqual({call.args[0] for call in scans.call_args_list}, {source / "source", target / "source"})
            self.assertEqual((target / "receipt.json").read_bytes(), before)
            self.assertEqual(sorted(str(item.relative_to(target)) for item in target.rglob("*")), inventory)
            self.assertEqual(len(report["manifests"]), 2)
            self.assertEqual(len(report["pairs"]), 1)
            self.assertEqual(set(report["pairs"][0]["source_changes"]), {"reviewed-document.txt"})
            self.assertEqual(report["pairs"][0]["release_scope"], "generated-at-only")
            for decision in report["checks"]:
                self.assertNotIn("source_entries", decision)
                self.assertTrue(all(value == "" for value in decision["review_draft"]["review"].values()))
            shared_bytes = len(m.evidence_bytes(report))
            repeated_bytes = sum(len(m.evidence_bytes(report["manifests"])) for _ in report["checks"])
            self.assertLess(shared_bytes, repeated_bytes)
            print(f"MEASURE batch_checks=2 unique_scans=2 shared_plan_bytes={shared_bytes} repeated_manifest_bytes={repeated_bytes}")
            with self.assertRaisesRegex(ValueError, "1 MiB"):
                m.bounded_batch({"padding": "x" * (1 << 20)})
            invalid = copy.deepcopy(request)
            invalid["checks"].append(copy.deepcopy(invalid["checks"][0]))
            path.write_text(json.dumps(invalid))
            with self.assertRaisesRegex(ValueError, "duplicate"):
                m.transfer_batch(target, path, plan=True)

    def test_batch_atomic_application_ordered_locks_and_idempotent_retry(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            directory = Path(temporary)
            source, target, path, request = self.batch_fixture(directory)
            alias = directory / "source-alias"
            alias.symlink_to(source, target_is_directory=True)
            request["checks"][1]["source"] = str(alias)
            path.write_text(json.dumps(request))
            acquired, native_lock = [], m.acceptance_lock
            @m.contextmanager
            def lock(output, shared=False):
                acquired.append((output, shared))
                with native_lock(output, shared):
                    yield
            with patch.object(m, "acceptance_lock", side_effect=lock), patch.object(m, "save", wraps=m.save) as saved, \
                 patch.object(m, "source_entries", wraps=m.source_entries) as scans:
                self.assertEqual(m.transfer_batch(target, path), 0)
            self.assertEqual(saved.call_count, 1)
            self.assertEqual(scans.call_count, 2)
            self.assertEqual(acquired, [(candidate, candidate != target) for candidate in sorted((source, target), key=str)])
            _, receipt = m.load_bound(target)
            self.assertTrue(all(receipt["checks"][item["check"]]["inherited"] for item in request["checks"]))
            # Shared canonical target/source manifests are copied once for the whole transaction.
            self.assertEqual(len({receipt["checks"][item["check"]]["evidence_files"]["manifest"] for item in request["checks"]}), 1)
            before = (target / "receipt.json").read_bytes()
            with patch.object(m, "save", side_effect=AssertionError("retry rewrote receipt")), \
                 patch.object(m.shutil, "copyfileobj", side_effect=AssertionError("retry copied evidence")):
                self.assertEqual(m.transfer_batch(target, path), 0)
            self.assertEqual((target / "receipt.json").read_bytes(), before)
            # A later terminal failure supersedes the source attempt, but cannot erase the committed target fact.
            source_root, old = m.load_bound(source)
            name = request["checks"][0]["check"]
            result = copy.deepcopy(old["checks"][name])
            result.pop("original_attempt")
            result.update(status="failed", exit_code=23)
            old["checks"][name] = result
            result["original_attempt"] = m.completed_attempt(source, source_root, old, name, result)
            m.save(source, old)
            self.assertEqual(m.transfer_batch(target, path), 0)
            invalid = copy.deepcopy(request)
            invalid["checks"][0]["attempt"] = result["original_attempt"]
            path.write_text(json.dumps(invalid))
            with self.assertRaisesRegex(ValueError, "conflicts"):
                m.transfer_batch(target, path)

    def test_batch_precommit_failures_preserve_receipt_and_foreign_staging(self):
        for failure in ("authorization", "copy", "save", "failed-target", "self-source", "review-mutation", "request-mutation", "chain", "stale-attempt"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory(dir="/tmp") as temporary:
                source, target, path, request = self.batch_fixture(Path(temporary))
                foreign = target / ".transfer-unowned"
                foreign.mkdir()
                (foreign / "keep").write_text("unowned orphan staging is never applied\n")
                if failure == "authorization":
                    descriptor = request["checks"][1]["review"]
                    review_path = Path(descriptor["path"])
                    review = json.loads(review_path.read_text())
                    review["review"]["authorization_reference"] = ""
                    review_path.write_text(json.dumps(review))
                    descriptor["sha256"] = m.digest(review_path)
                if failure == "self-source":
                    request["checks"][1]["source"] = str(target)
                if failure == "chain":
                    _, original = m.load_bound(source)
                    result = original["checks"]["local-adapters"]
                    result.pop("original_attempt")
                    result.update(inherited=True, from_receipt_sha256="a" * 64, from_log_sha256=result["log_sha256"],
                                  inherited_from={"scope": "exact-source", "source_fingerprint": original["source_fingerprint"],
                                                  "target_source_fingerprint": original["source_fingerprint"]})
                    request["checks"][1]["attempt"] = None
                    m.save(source, original)
                if failure == "stale-attempt":
                    request["checks"][1]["attempt"]["id"] = "f" * 32
                if failure == "failed-target":
                    _, receipt = m.load_bound(target)
                    receipt["checks"]["local-adapters"] = {"status": "failed", "exit_code": 23}
                    m.save(target, receipt)
                path.write_text(json.dumps(request))
                before = (target / "receipt.json").read_bytes()
                native_copy = m.shutil.copyfileobj
                mutated = []
                def copyfile(original, destination, *args):
                    if not mutated:
                        mutated.append(True)
                        if failure == "copy":
                            with Path(original.name).open("ab") as stream:
                                stream.write(b"tamper during copy")
                        if failure == "review-mutation":
                            with Path(request["checks"][0]["review"]["path"]).open("ab") as stream:
                                stream.write(b" ")
                        if failure == "request-mutation":
                            with path.open("ab") as stream:
                                stream.write(b" ")
                    return native_copy(original, destination, *args)
                with patch.object(m.shutil, "copyfileobj", side_effect=copyfile):
                    if failure == "save":
                        with patch.object(m, "save", side_effect=OSError("precommit write failed")), self.assertRaises(OSError):
                            m.transfer_batch(target, path)
                    else:
                        with self.assertRaises(ValueError):
                            m.transfer_batch(target, path)
                self.assertEqual((target / "receipt.json").read_bytes(), before)
                self.assertTrue((foreign / "keep").exists())
                self.assertEqual(list(target.glob("transfer-*")), [])
                self.assertEqual(list(target.glob(".transfer-*")), [foreign])

    def test_batch_postcommit_failures_preserve_references_and_report_committed(self):
        for failure in ("event", "after-replace", "unknown-replace"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory(dir="/tmp") as temporary:
                _, target, path, request = self.batch_fixture(Path(temporary))
                native_save, native_read = m.save, m.read_transfer_json
                replaced = []
                def save(output, receipt):
                    native_save(output, receipt)
                    replaced.append(True)
                    if failure != "event":
                        raise OSError("directory durability confirmation unavailable")
                def read(filename):
                    if failure == "unknown-replace" and replaced and filename == target / "receipt.json":
                        raise OSError("receipt read unavailable")
                    return native_read(filename)
                capture = io.StringIO()
                with patch.object(m, "save", side_effect=save), patch.object(m, "read_transfer_json", side_effect=read), \
                     patch.object(m, "event", side_effect=OSError("event sink unavailable")), redirect_stdout(capture):
                    with self.assertRaisesRegex(ValueError, "commit state"):
                        m.transfer_batch(target, path)
                self.assertIn("state=" + ("unknown" if failure == "unknown-replace" else "committed"), capture.getvalue())
                _, receipt = m.load_bound(target)
                for item in request["checks"]:
                    result = receipt["checks"][item["check"]]
                    for filename in result["evidence_files"].values():
                        self.assertTrue((target / filename).is_file())
                self.assertEqual(m.transfer_batch(target, path), 0)

    def test_batch_cli_flags_and_nonblocking_lock_refusal(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            source, target, path, _ = self.batch_fixture(Path(temporary))
            before = (target / "receipt.json").read_bytes()
            with m.acceptance_lock(source):
                with self.assertRaisesRegex(ValueError, "already in use"):
                    m.transfer_batch(target, path)
            self.assertEqual((target / "receipt.json").read_bytes(), before)
            for flags in (("--batch", str(path), "--check", "verify"), ("--plan",), ("--from", str(source), "--check", "verify")):
                with patch.object(sys, "argv", ["release-acceptance", "transfer", "--output", str(target), *flags]), \
                     redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as result:
                    m.main()
                self.assertEqual(result.exception.code, 2)

    def immutable_transfer_fixture(self, directory):
        first, second, name, declaration, review = self.transfer_fixture(directory)
        source_root, source = m.load_bound(first)
        result = source["checks"][name]
        result["original_attempt"] = m.completed_attempt(first, source_root, source, name, result)
        m.save(first, source)
        record, manifest = m.original_attempt(first, source, name, result)
        target_root, target = m.load_bound(second)
        review = {"schema_version": 2, "check": name, "tested_manifest_sha256": m.evidence_hash(manifest),
            "target_manifest_sha256": m.evidence_hash(m.candidate_manifest(target_root, target)),
            "original_attempt": result["original_attempt"], "review": review["review"], "standalone": None}
        declaration.write_text(json.dumps(review))
        return first, second, name, declaration, review

    def test_immutable_transfer_survives_unrelated_receipt_updates_and_rejects_tampering(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            first, second, name, declaration, review = self.immutable_transfer_fixture(Path(temporary))
            source_root, source = m.load_bound(first)
            record, manifest = m.original_attempt(first, source, name, source["checks"][name])
            for path in (first / "attempts" / (record["id"] + ".json"), first / source["checks"][name]["log"],
                         first / "manifests" / (record["manifest_sha256"] + ".json")):
                contents = path.read_bytes()
                path.write_bytes(contents + b" ")
                with self.assertRaises(ValueError):
                    m.transfer_evidence(second, first, name, declaration)
                path.write_bytes(contents)
            for mutate in (lambda item: item.update(extra=True),
                           lambda item: item["original_attempt"].update(id="c" * 32),
                           lambda item: item.update(target_manifest_sha256="d" * 64)):
                invalid = copy.deepcopy(review)
                mutate(invalid)
                declaration.write_text(json.dumps(invalid))
                with self.assertRaises(ValueError):
                    m.transfer_evidence(second, first, name, declaration)
            declaration.write_text(json.dumps(review))
            old_digest = m.digest(first / "receipt.json")
            source["unrelated_note"] = "another check continued"
            m.save(first, source)
            self.assertNotEqual(m.digest(first / "receipt.json"), old_digest)
            self.assertEqual(m.transfer_evidence(second, first, name, declaration), 0)
            _, target = m.load_bound(second)
            self.assertEqual(target["checks"][name]["evidence_transfer"]["schema_version"], 2)
            self.assertNotIn("from_receipt_sha256", target["checks"][name])
            self.assertNotIn("original_attempt", target["checks"][name])
            m.supersede(first)
            with self.assertRaises(ValueError):
                m.transfer_evidence(second, first, name, declaration)

    def test_current_attempt_crash_failure_and_orphan_never_restore_old_pass(self):
        for crash in ("execute", "aggregate", "failed", "interrupted"):
            with self.subTest(crash=crash), tempfile.TemporaryDirectory(dir="/tmp") as temporary:
                first, second, name, declaration, _ = self.immutable_transfer_fixture(Path(temporary))
                _, before = m.load_bound(first)
                old_reference = before["checks"][name]["original_attempt"]
                terminal_execute = m.execute
                def execute(check, command, root, output, env, slot=None):
                    persisted = json.loads((output / "receipt.json").read_text())["checks"][check]
                    self.assertEqual(persisted["status"], "running")
                    self.assertNotIn("original_attempt", persisted)
                    if crash == "execute":
                        raise OSError("controller crashed")
                    result = terminal_execute(check, command, root, output, env, slot)
                    if crash in ("failed", "interrupted"):
                        result.update(status=crash, exit_code=23 if crash == "failed" else -15,
                                      termination="exit" if crash == "failed" else "signal")
                    return result
                save, count = m.save, []
                def save_until_terminal(output, receipt):
                    count.append(True)
                    if crash == "aggregate" and len(count) == 3:
                        raise OSError("aggregate commit failed")
                    save(output, receipt)
                with patch.object(m, "execute", side_effect=execute), patch.object(m, "save", side_effect=save_until_terminal), \
                     patch.object(m, "execution_environment", return_value=before["environment"]):
                    if crash in ("execute", "aggregate"):
                        with self.assertRaises(OSError):
                            m.run_checks(first, [], [name], True)
                    else:
                        self.assertEqual(m.run_checks(first, [], [name], True), 1)
                _, after = m.load_bound(first)
                current = after["checks"][name]
                self.assertEqual(current["status"], "running" if crash in ("execute", "aggregate") else crash)
                self.assertNotEqual(current.get("original_attempt"), old_reference)
                attempts = list((first / "attempts").glob("*.json"))
                self.assertEqual(len(attempts), 1 if crash == "execute" else 2)
                with self.assertRaises(ValueError):
                    m.transfer_evidence(second, first, name, declaration)
                # The historical PASS remains intact, including after an unindexed terminal record.
                self.assertEqual(json.loads((first / "attempts" / (old_reference["id"] + ".json")).read_text())["result"]["status"], "passed")

    def test_immutable_creation_exact_modes_and_incomplete_foreign_records(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            first, _, name, _, _ = self.immutable_transfer_fixture(Path(temporary))
            _, source = m.load_bound(first)
            result = source["checks"][name]
            record, manifest = m.original_attempt(first, source, name, result)
            legacy = {"schema_version": 1, "check": name, "tested": manifest["candidate"], "target": manifest["candidate"],
                "command": record["command"], "origin": "receipt", "preparation_environment": manifest["environment"],
                "artifacts": {"receipt": m.digest(first / "receipt.json"), "review": "a" * 64, "log": result["log_sha256"]},
                "review": {"unchanged_scope": "Synthetic scope", "reason": "Fixture review", "authorization_reference": "fixture"},
                **m.attempt_facts(record)}
            m.validate_evidence_transfer(legacy, name, result["command"], manifest["candidate"], result["log_sha256"])
            # Immutable v1 manifests retain their original accepted text; transfer declarations have narrower public bounds.
            for field, value in (("version", "v" * 65), ("version", " "), ("version", "test\nversion"),
                                 ("runtime_file", "r" * 129), ("runtime_file", "runtime\nfile"), ("asset", "a" * 129)):
                with self.subTest(field=field, value=value):
                    compatible = copy.deepcopy(manifest)
                    candidate = compatible["candidate"]
                    if field == "runtime_file":
                        candidate["release_files"][value] = candidate["release_files"].pop(candidate["runtime_file"])
                    if field == "asset":
                        candidate["release_files"][value] = "b" * 64
                    else:
                        candidate[field] = value
                    m.validate_manifest(compatible)
                    transfer = {**legacy, "tested": candidate, "target": candidate}
                    with self.assertRaisesRegex(ValueError, "transferred candidate"):
                        m.validate_evidence_transfer(transfer, name, result["command"], candidate, result["log_sha256"])
            for field, value in (("version", ""), ("version", None), ("runtime_file", "../foreign"), ("runtime_sha256", "c" * 64)):
                invalid = copy.deepcopy(manifest)
                invalid["candidate"][field] = value
                with self.assertRaises(ValueError):
                    m.validate_manifest(invalid)
            path = first / "attempts" / (record["id"] + ".json")
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            self.assertEqual(path.parent.stat().st_mode & 0o777, 0o700)
            original = path.read_bytes()
            with self.assertRaises(FileExistsError):
                m.immutable_json(path, {"replacement": True})
            self.assertEqual(path.read_bytes(), original)
            for mutate in (lambda item: item["result"].update(capture_status=None),
                           lambda item: item["result"].update(duration_seconds=None),
                           lambda item: item["result"].update(execution_environment=None),
                           lambda item: item.update(manifest_sha256="f" * 64),
                           lambda item: item["result"].update(extra=True)):
                invalid = copy.deepcopy(record)
                mutate(invalid)
                with self.assertRaises(ValueError):
                    m.validate_attempt(invalid, manifest, name, result["command"], result["log_sha256"])
            # Every terminal state is checked against its candidate, reference and log.
            result.update(status="failed", exit_code=23)
            result["original_attempt"] = m.completed_attempt(first, first / "source", source, name, result)
            m.save(first, source)
            path = first / "attempts" / (result["original_attempt"]["id"] + ".json")
            path.write_bytes(path.read_bytes() + b" ")
            with self.assertRaises(ValueError):
                m.load_bound(first)

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

    def test_known_p0_parser_requires_asserted_negative_and_final_cleanup(self):
        self.assertEqual(m.HISTORICAL_P0_SHA256, "97c607ffc81cce7d11646e45702ee68b5cb6a0de316e7726d308c25e765d59f5")
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            first, second, name, declaration, review = self.transfer_fixture(Path(temporary), standalone=True, core=True)
            source, receipt = m.load_bound(first)
            files = {role: Path(item["path"]) for role, item in review["standalone"].items()}
            launch = json.loads(files["launch"].read_text())
            self.assertEqual(launch["acceptance_script_sha256"], m.HISTORICAL_P0_SHA256)
            bundle = receipt["candidate_bundle_sha256"]
            def phase(phase, state, rc=0, vm=1):
                return f"E2E_PHASE phase={phase} state={state} duration_seconds=0 exit_code={rc} vm={vm}\n"
            negative = phase("guest", "start") + phase("guest", "end", 23)
            cleaned = phase("guest-cleanup", "start") + phase("guest-cleanup", "end")
            begin = f"  [ .. ] phase=transport bundle={bundle}\n"
            end = "  [ ok ] phase=transport duration=9s\n"
            final_clean = {vm: f"ok: VM{vm} exact P0 state, pools, projects and releases removed\n" for vm in (1, 2)}
            success = "ok: P0 release smoke passed within one broker lease\n"
            release = "E2E lease released: slot=slot-002 run=sample\n"
            log = ("E2E lease: yard=sample slot=slot-002 run=sample type=subyard-pair vms=2 base=" + "a" * 64 + "\n"
                + begin + phase("transport", "start") + phase("transport", "end") + negative + cleaned + end
                + f"  [ .. ] phase=cleanup bundle={bundle}\n"
                + "".join(phase("guest", "start", vm=vm) + final_clean[vm] + phase("guest", "end", vm=vm)
                    + phase("guest-cleanup", "start", vm=vm) + phase("guest-cleanup", "end", vm=vm) for vm in (1, 2))
                + "  [ ok ] phase=cleanup duration=9s\n" + success + release)
            command = m.commands(source, receipt["version"], receipt["profiles"])[name]
            def check(text, **kwargs):
                files["log"].write_text(text)
                return m.standalone_result(source, receipt, name, command, files, **kwargs)
            # This is the old producer's actual contract: no wrapper cleanup/release marker.
            self.assertEqual(check(log)["environments"][0]["vm_count"], 2)
            self.assertEqual(check(log, verify_source=False)["environments"][0]["vm_count"], 2)
            before = (second / "receipt.json").read_bytes()
            invalid = {
                "missing negative": log.replace(negative, ""),
                "unexpected exit": log.replace("exit_code=23", "exit_code=24"),
                "other VM": log.replace(negative, negative.replace("vm=1", "vm=2")),
                "extra negative": log.replace(negative, negative + negative),
                "missing guest start": log.replace(phase("guest", "start"), ""),
                "unclosed guest": log.replace(phase("guest", "end", 23), ""),
                "guest began outside owner": log.replace(phase("guest", "start"), "", 1).replace(begin, phase("guest", "start") + begin),
                "malformed phase": log.replace("exit_code=23", "exit_code=private-canary"),
                "missing corresponding cleanup": log.replace(cleaned, ""),
                "cleanup before negative": log.replace(negative + cleaned, cleaned + negative),
                "failed cleanup": log.replace(phase("guest-cleanup", "end"), phase("guest-cleanup", "end", 3)),
                "unclosed owner": log.replace(end, ""),
                "duplicate owner": log.replace(begin, begin + begin),
                "malformed extra owner": log.replace(end, "  [ ok ] phase=transport duration=private-canary\n" + end),
                "failed owner": log.replace(end, "  [fail] phase=transport\n"),
                "wrong owner bundle": log.replace(begin, begin.replace(bundle, "b" * 64)),
                "negative outside owner": log.replace(negative, "").replace(begin, negative + begin),
                "missing final VM cleanup": log.replace(final_clean[2], ""),
                "missing final VM worktree cleanup": log.replace(phase("guest-cleanup", "start", vm=2) + phase("guest-cleanup", "end", vm=2), ""),
                "duplicate final VM cleanup": log.replace(final_clean[1], final_clean[1] * 2),
                "missing whole success": log.replace(success, ""),
                "missing release": log.replace(release, ""),
                "different lease release": log.replace(release, release.replace("run=sample", "run=other")),
                "different launcher slot": log.replace("slot=slot-002", "slot=slot-003"),
                "duplicate run": log + log,
                "after release": log + phase("guest", "start"),
                "extra failed phase": log.replace(success, phase("transport", "start") + phase("transport", "end", 1) + success),
            }
            for label, value in invalid.items():
                with self.subTest(label=label), self.assertRaises(ValueError):
                    check(value)
            files["log"].write_text(log)
            for field, value in (("acceptance_script_sha256", "f" * 64), ("source_fingerprint", "f" * 64)):
                files["launch"].write_text(json.dumps(dict(launch, **{field: value})))
                for verify in (True, False):
                    with self.subTest(field=field, verify_source=verify), self.assertRaises(ValueError):
                        check(log, verify_source=verify)
            files["launch"].write_text(json.dumps(launch))
            native = files["native"].read_bytes()
            files["native"].write_text(json.dumps(dict(json.loads(native), exit_code=3)))
            with self.assertRaises(ValueError):
                check(log)
            files["native"].write_bytes(native)
            controller = source / command[1]
            original = controller.read_bytes()
            controller.write_bytes(original + b"\n# unknown producer\n")
            try:
                with self.assertRaises(ValueError):
                    check(log)
                files["launch"].write_text(json.dumps(dict(launch, acceptance_script_sha256=m.digest(controller))))
                with self.assertRaises(ValueError):
                    check(log)
            finally:
                controller.write_bytes(original)
                files["launch"].write_text(json.dumps(launch))
            unavailable = controller.with_suffix(".unavailable")
            controller.rename(unavailable)
            try:
                with self.assertRaises((ValueError, OSError)):
                    check(log)
            finally:
                unavailable.rename(controller)
            files["launch"].write_text(json.dumps({key: value for key, value in launch.items()
                if key not in {"source_fingerprint", "runtime_sha256", "bundle_sha256", "acceptance_script_sha256"}}))
            with self.assertRaises(ValueError):
                check(log, verify_source=False)
            files["launch"].write_text(json.dumps(launch))
            with self.assertRaises(ValueError):
                m.standalone_result(source, receipt, "profile:sample/one", command, files)
            changed_command = command + ["--lane", "transport"]
            changed_launch = dict(launch, argv=launch["argv"][:-2] + ["--lane", "transport"] + launch["argv"][-2:])
            files["launch"].write_text(json.dumps(changed_launch))
            with self.assertRaises(ValueError):
                m.standalone_result(source, receipt, name, changed_command, files)
            files["launch"].write_text(json.dumps(launch))
            # Exercise actual import and reload, retaining unchanged originals and no invented timing.
            files["log"].write_text(log)
            review["review"]["reason"] = "Interpret the synthetic known P0 producer's asserted guest exit 23 and native-propagated direct cleanup"
            for role, path in files.items():
                review["standalone"][role]["sha256"] = m.digest(path)
            declaration.write_text(json.dumps(review))
            self.assertEqual((second / "receipt.json").read_bytes(), before)
            self.assertEqual(m.transfer_evidence(second, first, name, declaration), 0)
            # Copied original evidence stays portable after its initially verified source disappears.
            unavailable_source = source.with_name("source-unavailable")
            source.rename(unavailable_source)
            try:
                result = m.load_bound(second)[1]["checks"][name]
            finally:
                unavailable_source.rename(source)
            self.assertIsNone(result["duration_seconds"])
            self.assertNotIn("phases", result)

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

    def test_external_required_policy_and_optional_execution_status(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            checkout = fixture_checkout(Path(temporary))
            manifest = checkout / "config/profiles/sample/tests/e2e/external-obligations.json"
            declaration = {"id": "account", "description": "Live account check", "required_inputs": ["account"]}
            for required in (None, True, False):
                item = {**declaration, **({"required": required} if required is not None else {})}
                manifest.write_text(json.dumps({"schema_version": 1, "obligations": [item]}))
                inventory = m.external_inventory(checkout)
                self.assertEqual(inventory, {"sample:account": {**item, "status": "not-run"}})
                with patch.object(m, "ROOT", checkout), redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()):
                    report = m.preflight(only=["verify"])
                self.assertEqual(report["external_prerequisites"][0]["required"], required is not False)
                for status in ("not-run", "passed", "failed", "unknown", "pending"):
                    receipt = {"checks": {"verify": {"status": "passed"}},
                               "external_obligations": {"sample:account": {**item, "status": status}}}
                    m.update_result(receipt)
                    self.assertEqual(receipt["result"], "passed" if status == "passed" or
                                     status == "not-run" and required is False else "incomplete")
                    self.assertEqual(receipt["external_obligations"]["sample:account"]["status"], status)
            for required in (0, 1, "false", [], {}):
                manifest.write_text(json.dumps({"schema_version": 1, "obligations": [{**declaration, "required": required}]}))
                with self.subTest(required=required), self.assertRaises(ValueError):
                    m.external_inventory(checkout)

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
        # This scheduler/reporting fixture supplies synthetic receipts; immutable evidence has separate real fixtures.
        self.enterContext(patch.object(m, "completed_attempt", return_value={"id": "a" * 32, "sha256": "b" * 64}))
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
                self.assertEqual(m.run_checks(output, [1], [], False, with_vms=True), 0)
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
                                          "--slots", "1", "--types", "subyard-pair", "--with-vms"]), redirect_stdout(io.StringIO()):
                self.assertEqual(m.main(), 1)
            self.assertEqual(before, set(checkout.rglob("*")))

    def test_first_order_dynamic_dispatch_resume_and_exclusions(self):
        # This scheduler/reporting fixture supplies synthetic receipts; immutable evidence has separate real fixtures.
        self.enterContext(patch.object(m, "completed_attempt", return_value={"id": "a" * 32, "sha256": "b" * 64}))
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
                    first=["profile:sample/one", "profile:sample/two"], exclude=["profile:sample/three"], with_vms=True), 1)
                self.assertEqual([name for name, slot in started[:2]], ["verify", "compatibility"])
                self.assertEqual([name for name, slot in started[2:]], ["profile:sample/one", "profile:sample/two", "p0-release-smoke"])
                self.assertEqual(started[3][1], started[4][1])
                started.clear()
                self.assertEqual(m.run_checks(output, [1], ["profile:sample"], False), 0)
                self.assertEqual(started, [("profile:sample/three", 1)])

    def test_native_failure_signal_timeout_and_cleanup_remain_distinct(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            script = output / "outcome.sh"
            for suffix, expected, termination, category in (
                ('exit 23', 23, 'exit', 'product'),
                ('exit 124', 124, 'timeout', 'timeout'),
                ('kill -TERM $$', -15, 'signal', 'product')):
                with self.subTest(suffix=suffix):
                    script.write_text("#!/bin/bash\n"
                        "printf 'E2E_PHASE phase=packing state=end duration_seconds=2 exit_code=0\\n'\n"
                        "printf 'E2E_BUDGET phase=guest-cleanup budget_seconds=120 grace_seconds=5 vm=1\\n'\n"
                        "printf 'E2E_RELEASE state=start duration_seconds=0 budget_seconds=180 grace_seconds=5 exit_code=0\\n'\n"
                        "printf 'E2E_RELEASE state=end duration_seconds=4 budget_seconds=180 grace_seconds=5 exit_code=7\\n'\n"
                        "printf 'E2E_PHASE phase=cleanup/release state=end duration_seconds=1 exit_code=3\\n'\n"
                        "printf 'E2E_FAILURE class=product\\n'\n" + suffix + "\n")
                    result = m.execute("native", ["bash", str(script)], output, output, m.clean_environment())
                    self.assertEqual(result['exit_code'], expected)
                    self.assertEqual(result['termination'], termination)
                    self.assertEqual(result['failure_class'], category)
                    self.assertEqual(result['last_completed_phase'], 'packing')
                    self.assertEqual(result['cleanup']['cleanup/release']['exit_code'], 3)
                    self.assertEqual(result['cleanup']['lease-release'], {'exit_code':7, 'duration_seconds':4})
                    self.assertEqual(result['phase_budgets']['guest-cleanup/vm1'], {'budget_seconds':120, 'grace_seconds':5})
                    self.assertEqual(result['phase_budgets']['lease-release'], {'budget_seconds':180, 'grace_seconds':5})

    def test_required_local_failure_blocks_every_live_allocation(self):
        # This scheduler/reporting fixture supplies synthetic receipts; immutable evidence has separate real fixtures.
        self.enterContext(patch.object(m, "completed_attempt", return_value={"id": "a" * 32, "sha256": "b" * 64}))
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            checks = {name: ['true'] for name in ('verify', 'compatibility', 'p0-release-smoke', 'profile:sample')}
            receipt = {'version':'test', 'profiles':{}, 'checks':{name:{'status':'pending'} for name in checks}}
            executed = []
            def execute(name, command, root, output, env, slot=None):
                self.assertIsNone(slot, 'local failure reached VM allocation')
                executed.append(name)
                return {'status':'failed', 'exit_code':23}
            with patch.object(m, 'load_bound', return_value=(output, receipt)), \
                 patch.object(m, 'commands', return_value=checks), patch.object(m, 'save'), \
                 patch.object(m, 'execute', side_effect=execute):
                self.assertEqual(m.run_checks(output, [1, 2], [], False, with_vms=True), 1)
            self.assertEqual(executed, ['verify'])

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
            direct_name = "profile:sample/two"
            target["checks"][direct_name] = m.execute(direct_name,
                m.commands(target_root, "test", target["profiles"])[direct_name],
                target_root, second, m.clean_environment(), 1)
            m.save(second, target)
            chained = checkout / ".build/chained"
            prepare_fixture(checkout, chained)
            before = {path: path.read_bytes() for output in (first, second, chained)
                      for path in output.iterdir() if path.is_file()}
            with self.assertRaisesRegex(ValueError, "cannot chain inherited evidence"):
                # Validate the whole batch before copying even the direct result.
                m.import_evidence(chained, second, [direct_name, name])
            after = {path: path.read_bytes() for output in (first, second, chained)
                     for path in output.iterdir() if path.is_file()}
            self.assertEqual(after, before)
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
                while not ready.exists():
                    if cancelled.wait(0.01):
                        return
                    if time.monotonic() >= deadline:
                        break
                readiness["ready"] = ready.exists()
                # A startup failure also drains our group, but cannot satisfy the
                # handler-ready assertion for the intended interruption below.
                if not cancelled.is_set():
                    m.STOP.set()
            for ignores_term in (False, True):
                ready.unlink(missing_ok=True)
                finished.unlink(missing_ok=True)
                if ignores_term:
                    child.write_text(child.read_text().replace("signal.signal(signal.SIGTERM, finish)",
                                                             "signal.signal(signal.SIGTERM, signal.SIG_IGN)"))
                m.STOP.clear()
                cancelled = threading.Event()
                readiness = {"ready": False}
                timer = threading.Thread(target=interrupt_when_ready)
                timer.start()
                try:
                    with patch.object(m, "TERM_GRACE_SECONDS", 2), patch.object(m, "KILL_GRACE_SECONDS", 1):
                        result = m.execute("profile:sample/one", [sys.executable, str(parent), str(child), str(ready), str(finished)],
                                           output, output, m.clean_environment(), 1)
                finally:
                    cancelled.set()
                    timer.join(3)
                    m.STOP.clear()
                self.assertFalse(timer.is_alive())
                self.assertTrue(readiness["ready"])
                self.assertEqual(result["cleanup_status"], "drained")
                self.assertEqual(result["status"], "interrupted")
                self.assertEqual(result["exit_code"], -15)
                if not ignores_term:
                    self.assertTrue(finished.exists())
                    self.assertTrue("child cleanup completed" in (output / result["log"]).read_text())
                else:
                    self.assertFalse(finished.exists())
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
                    m.run_checks(output, [1], [], False, with_vms=True)

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

    def test_prelease_capacity_refusal_continues_once_and_later_retries_same_candidate(self):
        # This scheduler/reporting fixture supplies synthetic receipts; immutable evidence has separate real fixtures.
        self.enterContext(patch.object(m, "completed_attempt", return_value={"id": "a" * 32, "sha256": "b" * 64}))
        for resource in ("memory", "disk"):
            with self.subTest(resource=resource), tempfile.TemporaryDirectory(dir='/tmp') as temporary:
                output = Path(temporary)
                source = output / "source"
                source.mkdir()
                available = output / "capacity-available"
                controller = source / "controller.sh"
                controller.write_text("#!/bin/bash\n"
                    'test ! -f "$1" || exit 0\n'
                    "printf 'agent-e2e: retryable capacity refusal for slot-1: resource=" + resource +
                    "; retry after resources become available\\n'\nexit 4\n")
                blocked, following = "profile:sample/one", "profile:sample/two"
                checks = {"verify": ["true"], blocked: ["bash", str(controller), str(available)], following: ["true"]}
                receipt = {"version": "test", "profiles": {}, "source_fingerprint": "a" * 64,
                           "candidate_bundle_sha256": "b" * 64, "external_obligations": {},
                           "checks": {name: {"status": "passed" if name == "verify" else "pending"} for name in checks}}
                calls, original = [], {}
                execute = m.execute
                def observe(name, *args):
                    calls.append((name, args[-1]))
                    result = execute(name, *args)
                    original[name] = copy.deepcopy(result)
                    return result
                with patch.object(m, "load_bound", return_value=(source, receipt)), \
                     patch.object(m, "commands", return_value=checks), \
                     patch.object(m, "source_fingerprint", return_value="a" * 64), \
                     patch.object(m, "execute", side_effect=observe):
                    self.assertEqual(m.run_checks(output, [1], [], False, with_vms=True), 1)
                    self.assertEqual(calls, [(blocked, 1), (following, 1)])
                    persisted = json.loads((output / "receipt.json").read_text())
                    self.assertEqual(persisted["result"], "incomplete")
                    self.assertEqual(persisted["checks"][blocked], original[blocked] | {
                        "execution_environment": m.execution_environment(), "original_attempt": {"id": "a" * 32, "sha256": "b" * 64}})
                    self.assertEqual(persisted["checks"][blocked]["reason"], "capacity_" + resource)
                    self.assertEqual(persisted["checks"][following]["status"], "passed")
                    events = [json.loads(line) for line in (output / "events.jsonl").read_text().splitlines()]
                    attempts = [item for item in events if item["event"] == "check-result"]
                    self.assertEqual([(item["check"], item["status"]) for item in attempts],
                                     [(blocked, "blocked"), (following, "passed")])
                    self.assertEqual(events[-1]["status"], "incomplete")
                    blocked_log = output / original[blocked]["log"]
                    log_bytes = blocked_log.read_bytes()
                    available.touch()
                    calls.clear()
                    self.assertEqual(m.run_checks(output, [1], [blocked], False), 0)
                    self.assertEqual(calls, [(blocked, 1)])
                    retried = json.loads((output / "receipt.json").read_text())
                    self.assertEqual(retried["result"], "passed")
                    self.assertEqual(retried["checks"][following], persisted["checks"][following])
                    for key in ("source_fingerprint", "candidate_bundle_sha256"):
                        self.assertEqual(retried[key], persisted[key])
                    self.assertEqual(blocked_log.read_bytes(), log_bytes)
                    self.assertEqual((output / "events.jsonl").read_text().splitlines()[:len(events)],
                                     [json.dumps(item, sort_keys=True) for item in events])

    def test_capacity_queue_stops_for_unknown_refusals_failures_and_started_work(self):
        # This scheduler/reporting fixture supplies synthetic receipts; immutable evidence has separate real fixtures.
        self.enterContext(patch.object(m, "completed_attempt", return_value={"id": "a" * 32, "sha256": "b" * 64}))
        typed = "agent-e2e: retryable capacity refusal for slot-1: resource=memory; retry after resources become available\n"
        lease = "E2E lease: slot=slot-1 type=subyard-pair vms=1 base=" + "a" * 64 + "\n"
        guest = "E2E_PHASE phase=guest state=start duration_seconds=0 exit_code=0 vm=1\n"
        for text, code in (("", 4), (typed.replace("memory", "cpu"), 4), (typed, 23),
                           (typed + lease, 4), (typed + guest, 4), (lease + typed, 4),
                           (guest + typed, 4), (typed + lease + typed, 4), (typed + guest + typed, 4)):
            with self.subTest(text=text, code=code), tempfile.TemporaryDirectory(dir='/tmp') as temporary:
                output = Path(temporary)
                blocked, following = "profile:sample/one", "profile:sample/two"
                command = ["python3", "-c", "import sys; sys.stdout.write(" + repr(text) + "); sys.exit(" + str(code) + ")"]
                checks = {"verify": ["true"], blocked: command, following: ["true"]}
                receipt = {"version": "test", "profiles": {}, "source_fingerprint": "a" * 64,
                           "candidate_bundle_sha256": "b" * 64, "external_obligations": {},
                           "checks": {name: {"status": "passed" if name == "verify" else "pending"} for name in checks}}
                with patch.object(m, "load_bound", return_value=(output, receipt)), \
                     patch.object(m, "commands", return_value=checks), \
                     patch.object(m, "source_fingerprint", return_value="a" * 64):
                    self.assertEqual(m.run_checks(output, [1], [], False, with_vms=True), 1)
                persisted = json.loads((output / "receipt.json").read_text())
                attempt = persisted["checks"][blocked]
                self.assertEqual(attempt["status"], "failed")
                self.assertEqual(attempt["exit_code"], code)
                self.assertEqual(attempt["termination"], "exit")
                self.assertEqual(attempt["failure_class"], "unknown")
                self.assertNotIn("reason", attempt)
                self.assertEqual(persisted["result"], "failed")
                self.assertEqual(persisted["checks"][following]["status"], "pending")
                events = [json.loads(line) for line in (output / "events.jsonl").read_text().splitlines()]
                self.assertEqual([item["check"] for item in events if item["event"] == "check-start"], [blocked])
                results = [item for item in events if item["event"] == "check-result"]
                self.assertEqual([(item["status"], item["exit_code"], item["failure_class"]) for item in results],
                                 [("failed", code, "unknown")])
                self.assertNotIn("reason", results[0])
                self.assertEqual(events[-1]["status"], "failed")

    def test_interrupted_attempt_saved_and_untyped_blocked_worker_stops_queue(self):
        # This scheduler/reporting fixture supplies synthetic receipts; immutable evidence has separate real fixtures.
        self.enterContext(patch.object(m, "completed_attempt", return_value={"id": "a" * 32, "sha256": "b" * 64}))
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
                self.assertEqual(m.run_checks(output, [1], [], False, with_vms=True), 1)
                self.assertEqual(calls, ["profile:sample/one"])
                persisted = json.loads((output / "receipt.json").read_text())
                self.assertEqual(persisted["checks"]["profile:sample/two"]["status"], "pending")
                self.assertEqual(persisted["checks"]["profile:sample/one"]["exit_code"], 4)
                self.assertEqual(persisted["result"], "incomplete")
                def interrupt(name, *args):
                    m.STOP.set()
                    return {"status": "interrupted", "exit_code": -15, "duration_seconds": 0.7}
                with patch.object(m, "execute", side_effect=interrupt):
                    self.assertEqual(m.run_checks(output, [1], [], False, with_vms=True), 1)
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

    def test_invocation_selected_pass_reuse_and_final_receipt_binding(self):
        # This scheduler/reporting fixture supplies synthetic receipts; immutable evidence has separate real fixtures.
        self.enterContext(patch.object(m, "completed_attempt", return_value={"id": "a" * 32, "sha256": "b" * 64}))
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            output = Path(temporary)
            checks = {"verify": ["true"], "compatibility": ["true"]}
            receipt = {"version": "test", "profiles": {}, "source_fingerprint": "a" * 64,
                       "candidate_bundle_sha256": "b" * 64, "external_obligations": {},
                       "checks": {name: {"status": "pending"} for name in checks}}
            def result():
                return json.loads(max((output / "runs").glob("*.json"), key=lambda path: path.stat().st_mtime_ns).read_text())
            with patch.object(m, "load_bound", return_value=(output, receipt)), \
                 patch.object(m, "commands", return_value=checks), \
                 patch.object(m, "source_fingerprint", return_value="a" * 64):
                self.assertEqual(m.run_checks(output, [], ["verify"], False), 1)
                first = result()
                self.assertEqual((first["state"], first["selected_status"], first["overall_status"]),
                                 ("finished", "passed", "incomplete"))
                self.assertEqual(first["attempted"], ["verify"])
                self.assertEqual(first["reused"], [])
                self.assertEqual(first["receipt_sha256"], m.digest(output / "receipt.json"))
                saved = json.loads((output / "receipt.json").read_text())
                self.assertIn("execution_environment", saved["checks"]["verify"])
                self.assertNotIn("execution_environment", first["checks"]["verify"])
                self.assertEqual(first["checks"]["verify"]["log"], saved["checks"]["verify"]["log"])
                frozen = (output / "receipt.json").read_bytes()
                m.Invocation(output, receipt, {"verify"}, [], []).write("finished")
                self.assertEqual((output / "receipt.json").read_bytes(), frozen, "sidecar changed receipt digest")
                self.assertEqual(m.run_checks(output, [], ["verify"], False), 1)
                reused = result()
                self.assertNotEqual(first["invocation_id"], reused["invocation_id"])
                self.assertEqual(reused["reused"], ["verify"])
                self.assertEqual(reused["attempted"], [])
                self.assertEqual(reused["selected_status"], "passed")
                self.assertEqual(len(list((output / "runs").glob("*.json"))), 3)
                for path in (output / "runs").glob("*.json"):
                    self.assertEqual(path.stat().st_mode & 0o777, 0o600)
                events = [json.loads(line) for line in (output / "events.jsonl").read_text().splitlines()]
                self.assertEqual(events[-1]["invocation_id"], reused["invocation_id"])
                self.assertEqual(events[-1]["receipt_sha256"], reused["receipt_sha256"])
                # The actual producer journal projects a final pointer to this exact saved result.
                projector = root / "dev/project-run-events.py"
                projected = subprocess.run([sys.executable, "-B", str(projector), "--compact", str(output / "events.jsonl")],
                                           capture_output=True, text=True, check=True)
                final = json.loads(projected.stdout)["events"][-1]
                self.assertEqual(final["result"], "runs/" + reused["invocation_id"] + ".json")
                self.assertEqual(final["selected_status"], "passed")
                self.assertEqual(json.loads((output / final["result"]).read_text())["receipt_sha256"], final["receipt_sha256"])

    def test_invocation_prerequisite_rerun_stop_capacity_and_interruption(self):
        # This scheduler/reporting fixture supplies synthetic receipts; immutable evidence has separate real fixtures.
        self.enterContext(patch.object(m, "completed_attempt", return_value={"id": "a" * 32, "sha256": "b" * 64}))
        for case in ("prerequisite", "rerun", "blocked", "interrupted", "excluded", "stopped"):
            with self.subTest(case=case), tempfile.TemporaryDirectory(dir="/tmp") as temporary:
                output = Path(temporary)
                checks = {"verify": ["true"], "compatibility": ["true"], "profile:sample": ["true"]}
                receipt = {"version": "test", "profiles": {}, "source_fingerprint": "a" * 64,
                           "candidate_bundle_sha256": "b" * 64, "external_obligations": {},
                           "checks": {name: {"status": "passed" if case in ("rerun", "blocked", "interrupted", "excluded", "stopped") else "pending"}
                                      for name in checks}}
                calls = []
                def execute(name, *args):
                    calls.append(name)
                    if case == "interrupted":
                        m.STOP.set()
                        return {"status": "interrupted", "exit_code": -15, "cleanup_status": "drained"}
                    if case == "blocked":
                        return {"status": "blocked", "exit_code": 4, "termination": "exit", "reason": "capacity_memory"}
                    return {"status": "failed", "exit_code": 23}
                selected = ["profile:sample"] if case in ("prerequisite", "blocked", "interrupted") else []
                with patch.object(m, "load_bound", return_value=(output, receipt)), \
                     patch.object(m, "commands", return_value=checks), \
                     patch.object(m, "source_fingerprint", return_value="a" * 64), \
                     patch.object(m, "save", wraps=m.save) as saved, \
                     patch.object(m, "execute", side_effect=execute):
                    try:
                        if case == "stopped":
                            m.STOP.set()
                        self.assertEqual(m.run_checks(output, [1], selected, case in ("rerun", "blocked", "interrupted", "stopped"),
                                                      exclude=["compatibility"] if case == "excluded" else [], with_vms=True), 1)
                    finally:
                        m.STOP.clear()
                invocation = json.loads(next((output / "runs").glob("*.json")).read_text())
                if case == "prerequisite":
                    self.assertEqual(saved.call_count, 1, "display-only prerequisite summary rewrote the receipt")
                    self.assertEqual(calls, [])
                    self.assertEqual(invocation["attempted"], [])
                    self.assertEqual(invocation["not_started"], [{"check": "profile:sample", "reason": "prerequisite-stop"}])
                elif case == "rerun":
                    self.assertEqual(invocation["selected_status"], "failed")
                    self.assertEqual(invocation["checks"]["compatibility"]["status"], "not-started")
                    self.assertEqual(invocation["checks"]["profile:sample"]["status"], "not-started")
                    self.assertEqual(invocation["reused"], [])
                elif case == "stopped":
                    self.assertEqual(calls, [])
                    self.assertEqual(invocation["attempted"], [])
                    self.assertEqual(invocation["selected_status"], "incomplete")
                    self.assertEqual(invocation["state"], "interrupted")
                    self.assertEqual({item["status"] for item in invocation["checks"].values()}, {"not-started"})
                elif case == "excluded":
                    self.assertEqual(invocation["selected_status"], "passed")
                    self.assertEqual(invocation["overall_status"], "incomplete")
                else:
                    self.assertEqual(invocation["selected_status"], "incomplete")
                    self.assertEqual(invocation["checks"]["profile:sample"]["exit_code"], -15 if case == "interrupted" else 4)
                    self.assertEqual(invocation["state"], "interrupted" if case == "interrupted" else "finished")

    def test_invocation_errors_and_stale_running_do_not_certify_pass(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            output = Path(temporary)
            checks = {"verify": ["true"]}
            receipt = {"version": "test", "profiles": {}, "source_fingerprint": "a" * 64,
                       "candidate_bundle_sha256": "b" * 64, "external_obligations": {},
                       "checks": {"verify": {"status": "passed", "exit_code": 0}}}
            m.save(output, receipt)
            stale = m.Invocation(output, receipt, {"verify"}, [], [])
            self.assertEqual(stale.document["selected_status"], "incomplete")
            stale_bytes = stale.path.read_bytes()
            with patch.object(m, "load_bound", return_value=(output, receipt)), \
                 patch.object(m, "commands", return_value=checks), \
                 patch.object(m, "execute", side_effect=OSError("capture unavailable")):
                with self.assertRaises(OSError):
                    m.run_checks(output, [], [], True)
            self.assertEqual(stale.path.read_bytes(), stale_bytes)
            failed = [json.loads(path.read_text()) for path in (output / "runs").glob("*.json")
                      if path != stale.path][0]
            self.assertEqual((failed["state"], failed["selected_status"], failed["capture_status"]),
                             ("error", "incomplete", "failed"))
            self.assertEqual(failed["checks"]["verify"]["cleanup_status"], "unknown")
            count = len(list((output / "runs").glob("*.json")))
            with patch.object(m, "load_bound", return_value=(output, receipt)), \
                 patch.object(m, "commands", return_value=checks):
                with self.assertRaises(ValueError):
                    m.run_checks(output, [], ["unknown-check"], False)
            self.assertEqual(len(list((output / "runs").glob("*.json"))), count)
            with patch.object(m.Invocation, "write", side_effect=OSError("sidecar unavailable")), \
                 patch.object(m, "load_bound", return_value=(output, receipt)), \
                 patch.object(m, "commands", return_value=checks), \
                 patch.object(m, "execute", side_effect=AssertionError("capture failure dispatched work")):
                with self.assertRaises(OSError):
                    m.run_checks(output, [], [], True)
            self.assertEqual(stale.path.read_bytes(), stale_bytes)

    def test_invocation_capture_failure_preserves_last_successful_receipt(self):
        # This scheduler/reporting fixture supplies synthetic receipts; immutable evidence has separate real fixtures.
        self.enterContext(patch.object(m, "completed_attempt", return_value={"id": "a" * 32, "sha256": "b" * 64}))
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            output = Path(temporary)
            receipt = {"version": "test", "profiles": {}, "source_fingerprint": "a" * 64,
                       "candidate_bundle_sha256": "b" * 64, "external_obligations": {},
                       "checks": {"verify": {"status": "pending"}}}
            save, saves = m.save, []
            def fail_after_initial(output, receipt):
                saves.append(True)
                if len(saves) > 1:
                    raise OSError("receipt capture unavailable")
                save(output, receipt)
            with patch.object(m, "load_bound", return_value=(output, receipt)), \
                 patch.object(m, "commands", return_value={"verify": ["true"]}), \
                 patch.object(m, "save", side_effect=fail_after_initial):
                with self.assertRaises(OSError):
                    m.run_checks(output, [], [], False)
            sidecar = json.loads(next((output / "runs").glob("*.json")).read_text())
            self.assertEqual((sidecar["state"], sidecar["selected_status"], sidecar["overall_status"]),
                             ("error", "incomplete", "incomplete"))
            self.assertIsNone(sidecar["checks"]["verify"]["exit_code"])  # Durable pre-dispatch write failed; no native execution.
            self.assertEqual(sidecar["receipt_sha256"], m.digest(output / "receipt.json"))
            self.assertEqual(json.loads((output / "receipt.json").read_text())["checks"]["verify"]["status"], "pending")
            # Failure to persist the terminal sidecar leaves this invocation running, never a prior result.
            with patch.object(m, "load_bound", return_value=(output, receipt)), \
                 patch.object(m, "commands", return_value={"verify": ["true"]}), \
                 patch.object(m, "source_fingerprint", return_value="a" * 64), \
                 patch.object(m.Invocation, "finish", side_effect=OSError("sidecar capture unavailable")):
                with self.assertRaises(OSError):
                    m.run_checks(output, [], [], False)
            documents = [json.loads(path.read_text()) for path in (output / "runs").glob("*.json")]
            running = next(item for item in documents if item["state"] == "running")
            self.assertEqual(running["selected_status"], "incomplete")
            self.assertNotEqual(running["invocation_id"], sidecar["invocation_id"])

        for case in ("start-capture-error", "stop-during-start"):
            with self.subTest(case=case), tempfile.TemporaryDirectory(dir="/tmp") as temporary:
                output = Path(temporary)
                receipt = {"version": "test", "profiles": {}, "source_fingerprint": "a" * 64,
                           "candidate_bundle_sha256": "b" * 64, "external_obligations": {},
                           "checks": {"verify": {"status": "passed", "exit_code": 0}}}
                write = m.Invocation.write
                injected = []
                def dispatch_write(invocation, *args, **kwargs):
                    if invocation.attempted and not injected:
                        injected.append(True)
                        if case == "start-capture-error":
                            raise OSError("dispatch sidecar capture unavailable")
                        result = write(invocation, *args, **kwargs)
                        m.STOP.set()  # Arrives while persisting the start, before execute.
                        return result
                    return write(invocation, *args, **kwargs)
                with patch.object(m, "load_bound", return_value=(output, receipt)), \
                     patch.object(m, "commands", return_value={"verify": ["true"]}), \
                     patch.object(m, "source_fingerprint", return_value="a" * 64), \
                     patch.object(m.Invocation, "write", dispatch_write), \
                     patch.object(m, "execute", side_effect=AssertionError("undispatched check executed")):
                    try:
                        if case == "start-capture-error":
                            with self.assertRaises(OSError):
                                m.run_checks(output, [], [], True)
                        else:
                            self.assertEqual(m.run_checks(output, [], [], True), 1)
                    finally:
                        m.STOP.clear()
                invocation = json.loads(next((output / "runs").glob("*.json")).read_text())
                self.assertEqual(invocation["state"], "error" if case == "start-capture-error" else "interrupted")
                self.assertEqual(invocation["attempted"], [])
                self.assertEqual(invocation["reused"], [])
                self.assertEqual(invocation["selected_status"], "incomplete")
                self.assertEqual(invocation["checks"]["verify"]["status"], "not-started")
                self.assertEqual(invocation["not_started"], [{"check": "verify", "reason":
                    "dispatch-capture-error" if case == "start-capture-error" else "interrupted-before-dispatch"}])

    def test_zero_workload_exit_does_not_hide_cleanup_or_capture_failure(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            output = Path(temporary)
            script = output / "cleanup.sh"
            script.write_text("printf 'E2E_PHASE phase=cleanup/release state=end duration_seconds=1 exit_code=3\\n'\nexit 0\n")
            result = m.execute("cleanup", ["bash", str(script)], output, output, m.clean_environment())
            self.assertEqual((result["status"], result["exit_code"], result["cleanup_status"]), ("failed", 0, "recorded"))
            self.assertEqual(result["cleanup"]["cleanup/release"]["exit_code"], 3)
            event = m.event
            def capture(output, kind, **fields):
                if kind == "check-result":
                    raise OSError("capture unavailable")
                return event(output, kind, **fields)
            with patch.object(m, "event", side_effect=capture):
                result = m.execute("capture", ["true"], output, output, m.clean_environment())
            self.assertEqual((result["status"], result["exit_code"], result["capture_status"]), ("failed", 0, "failed"))
            self.assertEqual(result["cleanup_status"], "unknown")

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
