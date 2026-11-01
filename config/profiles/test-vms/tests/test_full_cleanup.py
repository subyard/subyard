import importlib.util
import hashlib
import json
import os
import pathlib
import tempfile
import subprocess
import sys
import signal
import shlex
import time
import unittest
from unittest import mock


PROFILE = pathlib.Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("sink_full_cleanup", PROFILE / "full-cleanup.py")
owner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(owner)


class SinkOwnershipTests(unittest.TestCase):
    def test_route_namespace_removes_empty_producer_ancestors_and_preserves_peers(self):
        declaration = json.loads((PROFILE / "profile.json").read_bytes())
        script = (owner.ROOT / "scripts/03-create-subyard.sh").read_text()
        start = script.index("reconcile_e2e_route_mount() {")
        producer = script[start:script.index("\n}\n", start) + 3]
        for mode in ("empty", "foreign"):
            with self.subTest(mode=mode):
                home = self.root / mode
                home.mkdir(mode=0o700)
                # Execute the actual producer function with only its Incus/logging
                # boundaries doubled; do not copy its mkdir or mount implementation.
                subprocess.run(["bash", "-eu", "-c", "PROJ=(--project fixture); device_exists() { return 1; }; incus() { :; }; ok() { :; };\n" + producer + "\nreconcile_e2e_route_mount"],
                               env=dict(os.environ, SUBYARD_HOME=str(home), YARD_INSTANCE_NAME="fixture"), check=True, capture_output=True)
                route = home / "e2e/routes/default"
                if mode != "empty":
                    route.mkdir()
                    (route / "route.tsv").write_text("synthetic owned route")
                    peer = home / "e2e/routes/foreign-yard"
                    peer.mkdir()
                    (peer / "foreign.control").write_text("preserve")
                    (home / "e2e/foreign.control").write_text("preserve")
                paths = [item["path"].replace("{yard}", "default") for item in declaration.get("managed_paths", []) if item["root"] == "data"]
                tree = owner.guard.capture_tree({"Path": str(home), "Entries": [], "Scoped": True, "OwnedPaths": paths})
                plan = {"Installation": {"operatorUid": os.getuid()}, "Trees": [tree]}
                path = self.root / (mode + ".plan.json")
                body = json.dumps(plan).encode()
                self.write(path, body, 0o600)
                subprocess.run([sys.executable, "-B", str(owner.ROOT / "scripts/lib/teardown-files.py")],
                               env=dict(os.environ, SUBYARD_TEARDOWN_PLAN=str(path), SUBYARD_TEARDOWN_PLAN_SHA256=hashlib.sha256(body).hexdigest()), check=True, capture_output=True)
                self.assertFalse(route.exists())
                if mode == "empty":
                    self.assertFalse(home.exists(), "approved artifact remains: data")
                else:
                    self.assertEqual((peer / "foreign.control").read_text(), "preserve")
                    self.assertEqual((home / "e2e/foreign.control").read_text(), "preserve")

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="subyard-sink-cleanup-", dir="/tmp")
        self.addCleanup(self.temporary.cleanup)
        self.root = pathlib.Path(self.temporary.name)
        self.service = self.root / "subyard-test-vms-host-sink.service"
        self.timer = self.root / "subyard-test-vms-host-sink.timer"
        self.helper = self.root / "sink"
        self.engine = self.root / "engine"
        self.environment = {
            "SUBYARD_HOME": str(self.root / "owner-data"),
            "SUBYARD_TEST_VMS_SINK_PATH": str(self.helper),
            "SUBYARD_TEST_VMS_SINK_SERVICE_PATH": str(self.service),
            "SUBYARD_TEST_VMS_SINK_TIMER_PATH": str(self.timer),
            "SUBYARD_DISPATCHER_PATH": str(self.engine),
        }

    def write(self, path, data, mode):
        path.write_bytes(data)
        path.chmod(mode)

    def install(self, home=None):
        template = (owner.ROOT / "config/systemd/subyard-test-vms-host-sink.service.in").read_bytes()
        template = template.replace(b"@SUBYARD_TEST_VMS_HOST_SINK@", str(self.helper).encode())
        template = template.replace(b"@SUBYARD_HOME@", (home or self.environment["SUBYARD_HOME"]).encode())
        template = template.replace(b"@SUBYARD_OPERATOR_GID@", str(os.getgid()).encode())
        self.write(self.service, template, 0o644)
        self.write(self.timer, (owner.ROOT / "config/systemd/subyard-test-vms-host-sink.timer.in").read_bytes(), 0o644)
        self.write(self.engine, b"synthetic native engine", 0o755)
        self.write(self.helper, self.engine.read_bytes(), 0o755)
        wants = self.root / "timers.target.wants"
        wants.mkdir(mode=0o755)
        (wants / self.timer.name).symlink_to(self.timer)

    def test_matching_inventory_is_read_only_and_complete(self):
        self.install()
        before = {str(path): (path.lstat().st_mode, path.lstat().st_uid, path.lstat().st_gid, path.lstat().st_ino, path.lstat().st_size, path.lstat().st_mtime_ns, path.lstat().st_ctime_ns) for path in self.root.rglob("*")}
        plan = owner.assess(self.environment, system_uid=os.getuid(), inventory=[])
        self.assertEqual(len(plan["artifacts"]), 4)
        self.assertEqual(plan["services"][0]["units"], [self.timer.name, self.service.name])
        self.assertFalse(plan["preserved"])
        after = {str(path): (path.lstat().st_mode, path.lstat().st_uid, path.lstat().st_gid, path.lstat().st_ino, path.lstat().st_size, path.lstat().st_mtime_ns, path.lstat().st_ctime_ns) for path in self.root.rglob("*")}
        self.assertEqual(before, after)

    def test_foreign_data_home_preserves_another_producer(self):
        self.install(str(self.root / "foreign-data"))
        plan = owner.assess(self.environment, system_uid=os.getuid(), inventory=[])
        self.assertFalse(plan["artifacts"])
        self.assertFalse(plan["services"])
        self.assertTrue(plan["preserved"])
        self.assertTrue(self.helper.is_file())

    def test_other_producer_retains_shared_service_and_evidence(self):
        self.install()
        foreign = [{"project": "foreign-project", "expanded_config": {"user.subyard.test_vms_spool_schema": "1"}}]
        plan = owner.assess(self.environment, system_uid=os.getuid(), inventory=foreign)
        self.assertFalse(plan["artifacts"])
        self.assertFalse(plan["services"])
        self.assertTrue(plan["preservedRoots"])
        self.assertTrue(self.service.exists())

    def test_retained_incident_without_owner_refuses_full_cleanup(self):
        self.install()
        directory = pathlib.Path(self.environment["SUBYARD_HOME"]) / "logs/test-vms-broker-incidents"
        directory.mkdir(mode=0o700, parents=True)
        path = directory / "retained.json"
        self.write(path, b'{"source":"unknown-producer"}', 0o640)
        plan = owner.assess(self.environment, system_uid=os.getuid(), inventory=[])
        self.assertTrue(plan["blockers"])
        self.assertFalse(plan["artifacts"])
        self.assertTrue(path.exists())

    def test_private_matching_incident_is_conditional_and_checked_without_mutation(self):
        self.install()
        self.environment["SUBYARD_FULL_CLEANUP_SOURCES"] = '["default"]'
        directory = pathlib.Path(self.environment["SUBYARD_HOME"]) / "logs/test-vms-broker-incidents"
        directory.mkdir(mode=0o700, parents=True)
        record = directory / "retained.json"
        self.write(record, b'{"source":"default"}', 0o600)
        host = directory / "host"
        host.mkdir(mode=0o700)
        self.write(host / record.name, b'{"observed_at":"2026-10-13T00:00:00Z","timing":"after incident"}', 0o600)
        with mock.patch.object(owner, "verify_evidence", side_effect=PermissionError()):
            plan = owner.assess(self.environment, system_uid=os.getuid(), inventory=[])
        self.assertTrue(plan["conditional"])
        self.assertFalse(plan["blockers"])
        self.assertTrue(plan["services"])
        before = {str(path): path.read_bytes() for path in directory.rglob("*.json")}
        owner.verify_private(self.environment, inventory=[])
        self.assertEqual(before, {str(path): path.read_bytes() for path in directory.rglob("*.json")})
        self.write(record, b'{"source":"foreign"}', 0o600)
        with self.assertRaises(ValueError):
            owner.verify_private(self.environment, inventory=[])
        self.assertTrue(self.service.exists())
        self.assertTrue(record.exists())

    def test_private_proof_refuses_new_foreign_producer_and_orphan_host_evidence(self):
        self.install()
        self.environment["SUBYARD_FULL_CLEANUP_SOURCES"] = '["default"]'
        with self.assertRaises(ValueError):
            owner.verify_private(self.environment, inventory=[{"project":"foreign", "config":{"user.subyard.test_vms_spool_schema":"1"}}])
        directory = pathlib.Path(self.environment["SUBYARD_HOME"]) / "logs/test-vms-broker-incidents/host"
        directory.mkdir(mode=0o700, parents=True)
        self.write(directory / "orphan.json", b'{"observed_at":"2026-10-13T00:00:00Z","timing":"after incident"}', 0o600)
        with self.assertRaises(ValueError):
            owner.verify_private(self.environment, inventory=[])

    def completed_owner_migration(self):
        config = self.root / "owner-config"
        config.mkdir(mode=0o700)
        self.environment.update(SUBYARD_CONFIG_HOME=str(config), SUBYARD_FULL_CLEANUP_SOURCES='["test-yard"]')
        before = dict(state="legacy-directory+projects", registration="a" * 64, overrides="b" * 64, controller="c" * 64, sharedImages=False)
        digest = lambda value: hashlib.sha256(json.dumps(value, separators=(",", ":")).encode()).hexdigest()
        migration = next(item for item in json.loads((owner.ROOT / "config/release-transition.json").read_bytes())["migrations"] if item["kind"] == "test-yard-owner-v1-to-v2")
        transaction, step = "tx-owner-proof", migration["id"] + ".test-yard-owner"
        releases = {"from": "old", "target": "current"}
        recovery = dict(schemaVersion=1, transaction=transaction, step=step, migration=migration["id"], resource="test-yard-owner", expectedFingerprint=digest(before), token="d" * 32, before=before)
        recovery_raw = json.dumps(recovery, separators=(",", ":")).encode() + b"\n"
        common = dict(schemaVersion=2, transaction=transaction, releases=releases, step=step, expectedFingerprint=digest(before), desiredFingerprint=digest(dict(before, state="current")), recoveryFingerprint=hashlib.sha256(recovery_raw).hexdigest())
        records = {"release-transition/v2/transactions/" + transaction + "/recovery/" + step + ".json": recovery_raw}
        for phase in ("captured", "applied", "verified"):
            evidence = dict(common, checkpoint=phase, observedFingerprint=common["expectedFingerprint" if phase == "captured" else "desiredFingerprint"])
            records["release-transition/v2/transactions/" + transaction + "/evidence/" + step + "." + phase + ".json"] = json.dumps(evidence).encode()
        journal = dict(schemaVersion=2, checkpoint="complete", transaction=transaction, releases=releases, registryDigest=hashlib.sha256((owner.ROOT / "config/release-transition.json").read_bytes()).hexdigest(), steps=[dict(id=step, migration=migration["id"], resource="test-yard-owner", decision="canonicalize", checkpoint="verified", expectedFingerprint=common["expectedFingerprint"], desiredFingerprint=common["desiredFingerprint"], evidence=evidence)])
        records["release-transition/v2/journal.json"] = json.dumps(journal).encode()
        records["release-transition/v2/ledger.json"] = json.dumps(dict(schemaVersion=2, domains={"owner-registration": dict(epoch=2, applied=[migration["id"]])})).encode()
        for relative, raw in records.items():
            path = config / relative
            path.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
            for parent in path.parents:
                if parent == config.parent:
                    break
                parent.chmod(0o700)
            self.write(path, raw, 0o600)
        directory = pathlib.Path(self.environment["SUBYARD_HOME"]) / "logs/test-vms-broker-incidents"
        directory.mkdir(mode=0o700, parents=True)
        self.write(directory / "retained.json", b'{"source":"e2e-yard"}', 0o600)
        return config, journal

    def test_completed_owner_migration_retains_original_producer_source(self):
        self.install()
        self.completed_owner_migration()
        materialized = pathlib.Path(self.environment["SUBYARD_HOME"]) / "logs/test-vms-broker.jsonl"
        self.write(materialized, b'{"source":"e2e-yard"}\n', 0o600)
        before = {path: (path.read_bytes(), path.stat().st_mode, path.stat().st_mtime_ns, path.stat().st_ctime_ns) for path in self.root.rglob("*") if path.is_file()}
        owner.verify_private(self.environment, inventory=[])
        self.assertFalse(owner.assess(self.environment, system_uid=os.getuid(), inventory=[])["blockers"])
        self.assertEqual(before, {path: (path.read_bytes(), path.stat().st_mode, path.stat().st_mtime_ns, path.stat().st_ctime_ns) for path in before})

    def test_original_producer_requires_closed_protected_proof_in_selected_root(self):
        self.install()
        config, journal = self.completed_owner_migration()
        records = {path: path.read_bytes() for path in config.rglob("*.json")}
        journal_path = config / "release-transition/v2/journal.json"
        recovery = next(config.glob("**/recovery/*.json"))
        captured = next(config.glob("**/evidence/*.captured.json"))
        applied = next(config.glob("**/evidence/*.applied.json"))
        ledger = config / "release-transition/v2/ledger.json"
        foreign_root = self.root / "other-config"
        foreign_root.mkdir(mode=0o700)

        def change(path, key, value):
            record = json.loads(path.read_bytes())
            record[key] = value
            self.write(path, json.dumps(record).encode(), 0o600)

        failures = ("cross-root", "cross-transaction", "registry", "before", "recovery", "captured", "applied", "unverified", "pending", "ledger", "collected", "missing-recovery", "unsafe-file", "unsafe-directory", "symlink", "unapproved-current", "unknown-producer")
        failures += tuple("numeric-" + field + "-" + number for field in ("journal", "ledger", "recovery", "evidence", "summary", "epoch") for number in ("bool", "float"))
        for failure in failures:
            with self.subTest(failure=failure):
                self.environment.update(SUBYARD_CONFIG_HOME=str(config), SUBYARD_FULL_CLEANUP_SOURCES='["test-yard"]')
                for path, raw in records.items():
                    if path.is_symlink():
                        path.unlink()
                    self.write(path, raw, 0o600)
                recovery.parent.chmod(0o700)
                incident = pathlib.Path(self.environment["SUBYARD_HOME"]) / "logs/test-vms-broker-incidents/retained.json"
                self.write(incident, b'{"source":"e2e-yard"}', 0o600)
                if failure.startswith("numeric-"):
                    _, field, number = failure.split("-")
                    path = {"journal": journal_path, "ledger": ledger, "recovery": recovery, "evidence": applied, "summary": journal_path, "epoch": ledger}[field]
                    value = True if number == "bool" else (1.0 if field == "recovery" else 2.0)
                    if field == "epoch":
                        change(path, "domains", {"owner-registration": {"epoch": value, "applied": [journal["steps"][0]["migration"]]}})
                    elif field == "summary":
                        step = journal["steps"][0]
                        change(path, "steps", [dict(step, evidence=dict(step["evidence"], schemaVersion=value))])
                    else:
                        change(path, "schemaVersion", value)
                    if field == "recovery":
                        # Preserve all integrity bindings so only the numeric type is wrong.
                        fingerprint = hashlib.sha256(recovery.read_bytes()).hexdigest()
                        for evidence in config.glob("**/evidence/*.json"):
                            change(evidence, "recoveryFingerprint", fingerprint)
                        step = journal["steps"][0]
                        change(journal_path, "steps", [dict(step, evidence=dict(step["evidence"], recoveryFingerprint=fingerprint))])
                elif failure == "cross-root":
                    self.environment["SUBYARD_CONFIG_HOME"] = str(foreign_root)
                elif failure == "cross-transaction":
                    change(captured, "transaction", "another-transaction")
                elif failure == "registry":
                    change(journal_path, "registryDigest", "0" * 64)
                elif failure == "before":
                    change(recovery, "before", dict(json.loads(recovery.read_bytes())["before"], registration="0" * 64))
                elif failure == "recovery":
                    change(recovery, "token", "e" * 32)
                elif failure == "captured":
                    captured.unlink()
                elif failure == "applied":
                    change(applied, "observedFingerprint", "0" * 64)
                elif failure == "unverified":
                    change(journal_path, "steps", [dict(journal["steps"][0], checkpoint="applied")])
                elif failure == "pending":
                    change(journal_path, "checkpoint", "migrating")
                elif failure == "ledger":
                    change(ledger, "domains", {"owner-registration": {"epoch": 1, "applied": []}})
                elif failure == "collected":
                    journal_path.unlink()
                elif failure == "missing-recovery":
                    recovery.unlink()
                elif failure == "unsafe-file":
                    recovery.chmod(0o644)
                elif failure == "unsafe-directory":
                    recovery.parent.chmod(0o755)
                elif failure == "symlink":
                    recovery.unlink()
                    recovery.symlink_to(ledger)
                elif failure == "unapproved-current":
                    self.environment["SUBYARD_FULL_CLEANUP_SOURCES"] = '["another-yard"]'
                elif failure == "unknown-producer":
                    self.write(incident, b'{"source":"unknown-producer"}', 0o600)
                with self.assertRaises(ValueError):
                    owner.verify_private(self.environment, inventory=[])
                plan = owner.assess(self.environment, system_uid=os.getuid(), inventory=[])
                self.assertTrue(plan["blockers"])
                self.assertFalse(plan["artifacts"])
                self.assertTrue(incident.exists())

    def test_missing_owner_unsafe_helper_and_foreign_uid_refuse(self):
        self.install()
        with self.assertRaises(ValueError):
            owner.assess(self.environment, system_uid=os.getuid() + 1, inventory=[])
        self.helper.unlink()
        self.helper.symlink_to(self.engine)
        with self.assertRaises(ValueError):
            owner.assess(self.environment, system_uid=os.getuid(), inventory=[])
        self.service.unlink()
        with self.assertRaises(ValueError):
            owner.assess(self.environment, system_uid=os.getuid(), inventory=[])


class RecipePreflightTests(unittest.TestCase):
    def test_installer_precedes_required_recipe_cases_and_failures_stop(self):
        required = ["TestBaseRecipeInstallerContract", "TestBaseRecipeInstallerContract/subyard-pair",
                    "TestBaseRecipeInstallerContract/android-test"]
        for kind in ("pass", "missing", "skip", "installer-failure"):
            with self.subTest(kind=kind), tempfile.TemporaryDirectory(dir="/tmp") as directory:
                root = pathlib.Path(directory)
                runner = root / "config/profiles/test-vms/tests/run.sh"
                runner.parent.mkdir(parents=True)
                runner.write_bytes((PROFILE / "tests/run.sh").read_bytes())
                (root / "tests").mkdir()
                (root / "tests/install-incus-data-home.sh").write_text(
                    'printf "installer\\n" >> "$CHECK_ORDER"\nexit "${INSTALLER_EXIT:-0}"\n')
                (runner.parent / "test_full_cleanup.py").write_text('')
                observer = root / "dev/e2e/staging-reliability-test.py"
                observer.parent.mkdir(parents=True)
                observer.write_text('')
                tools = root / "tools"
                tools.mkdir()
                events = [dict(Action="skip" if kind == "skip" and index == 1 else "pass", Test=name)
                          for index, name in enumerate(required) if kind != "missing" or index != 1]
                go = tools / "go"
                go.write_text('#!/bin/bash\nprintf "go\\n" >> "$CHECK_ORDER"\n' +
                              'printf \'%s\\n\' ' + shlex.quote('\n'.join(map(json.dumps, events))) + '\n')
                go.chmod(0o700)
                order = root / "order"
                environment = dict(os.environ, PATH=str(tools)+":"+os.environ["PATH"], CHECK_ORDER=str(order),
                                   INSTALLER_EXIT="23" if kind == "installer-failure" else "0")
                environment.pop("SUBYARD_E2E_LOCAL_EVIDENCE", None)
                result = subprocess.run(["bash", str(runner), "--preflight"], env=environment,
                                        capture_output=True, text=True, timeout=5)
                self.assertEqual(result.returncode == 0, kind == "pass", result.stdout + result.stderr)
                self.assertEqual(order.read_text().splitlines(),
                                 ["installer"] if kind == "installer-failure" else ["installer", "go"])


class AcceptanceRunnerTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="subyard-sink-acceptance-", dir="/tmp")
        self.addCleanup(temporary.cleanup)
        self.root = pathlib.Path(temporary.name)
        self.root.chmod(0o700)
        for directory in ("config", "config/profiles", "config/profiles/test-vms",
                          "config/profiles/test-vms/tests", "config/profiles/test-vms/tests/e2e",
                          "dev", "dev/e2e"):
            (self.root / directory).mkdir(mode=0o700)
        self.runner = self.root / "config/profiles/test-vms/tests/e2e/acceptance.sh"
        self.runner.write_bytes((PROFILE / "tests/e2e/acceptance.sh").read_bytes())
        self.runner.chmod(0o755)
        controller = self.root / "dev/e2e/p0-acceptance.sh"
        controller.write_text("""#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$@" > "$CONTROLLER_STATE/argv"
printf '%s\\n' "$$" > "$CONTROLLER_STATE/pid"
printf 'controller output\\n'
printf 'controller error\\n' >&2
if [ "${CONTROLLER_SIGNAL:-0}" = 1 ]; then exec sleep 30; fi
exit "${CONTROLLER_EXIT:-0}"
""")
        controller.chmod(0o755)
        preflight = self.root / "config/profiles/test-vms/tests/run.sh"
        preflight.write_text('exit "${PREFLIGHT_EXIT:-0}"\n')
        broker = self.root / "dev/agent-e2e.sh"
        broker.write_text('printf "%s\\n" "$@" > "$CONTROLLER_STATE/broker-argv"\nexit "${BROKER_EXIT:-0}"\n')
        self.bundle = self.root / "candidate.tar.gz"
        self.bundle.write_bytes(b"frozen candidate")
        self.environment = dict(os.environ, CONTROLLER_STATE=str(self.root),
                                SUBYARD_E2E_CANDIDATE_BUNDLE=str(self.bundle),
                                SUBYARD_E2E_CANDIDATE_SHA256=hashlib.sha256(self.bundle.read_bytes()).hexdigest())

    def test_full_uninstall_lane_slot_and_native_exit_are_preserved(self):
        for code in (0, 23):
            with self.subTest(code=code):
                result = subprocess.run(["bash", str(self.runner), "--slot", "2", "--lane", "full-uninstall"],
                                        env=dict(self.environment, CONTROLLER_EXIT=str(code)),
                                        capture_output=True, text=True, timeout=5)
                self.assertEqual(result.returncode, code)
                self.assertEqual((self.root / "argv").read_text().splitlines(),
                                 ["--slot", "2", "--lane", "full-uninstall"])
                self.assertTrue(result.stdout.endswith("controller output\n"))
                self.assertEqual(result.stderr, "controller error\n")

    def test_default_preserves_both_required_lanes(self):
        result = subprocess.run(["bash", str(self.runner), "--slot", "2"],
                                env=self.environment, capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 0, result.stderr)
        arguments = (self.root / "broker-argv").read_text().splitlines()
        self.assertEqual(arguments[:8], ["--slot", "2", "--type", "android-test", "--purpose", "broker-cold-upgrade", "--vm", "1"])
        self.assertEqual(arguments[-2:], ["dev/e2e/staging-reliability.sh", "--supported-upgrade"])
        self.assertEqual((self.root / "argv").read_text().splitlines(), ["--slot", "2", "--lane", "full-uninstall"])

    def test_failed_prerequisite_or_cold_lane_stops_dependent_work(self):
        for setting in ({"PREFLIGHT_EXIT": "23"}, {"BROKER_EXIT": "23"},
                        {"SUBYARD_E2E_CANDIDATE_SHA256": "0" * 64}):
            with self.subTest(setting=setting):
                result = subprocess.run(["bash", str(self.runner), "--slot", "2"],
                                        env=dict(self.environment, **setting), capture_output=True, text=True, timeout=5)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse((self.root / "argv").exists())
                if "PREFLIGHT_EXIT" in setting:
                    self.assertFalse((self.root / "broker-argv").exists())

    def test_invalid_arguments_never_enter_controller(self):
        for arguments in ([], ["--slot"], ["--slot", "0"], ["--slot", "-1"],
                          ["--slot", "01"], ["--slot", "x"], ["--slot", "1", "extra"],
                          ["--slot", "1", "--slot", "2"], ["--slot", "1", "--lane", "smoke"]):
            with self.subTest(arguments=arguments):
                result = subprocess.run(["bash", str(self.runner), *arguments],
                                        env=self.environment, capture_output=True, timeout=5)
                self.assertEqual(result.returncode, 2)
                self.assertFalse((self.root / "argv").exists())

    def test_controller_replaces_adapter_and_receives_signal(self):
        process = subprocess.Popen(["bash", str(self.runner), "--slot", "1", "--lane", "full-uninstall"],
                                   env=dict(self.environment, CONTROLLER_SIGNAL="1"),
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                   start_new_session=True)
        try:
            deadline = time.monotonic() + 5
            while not (self.root / "pid").exists() and time.monotonic() < deadline:
                self.assertIsNone(process.poll())
                time.sleep(0.01)
            self.assertEqual(int((self.root / "pid").read_text()), process.pid)
            process.send_signal(signal.SIGTERM)
            self.assertEqual(process.wait(timeout=5), -signal.SIGTERM)
        finally:
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait(timeout=5)


if __name__ == "__main__":
    unittest.main()
