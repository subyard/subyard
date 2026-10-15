import importlib.util
import hashlib
import json
import os
import pathlib
import tempfile
import subprocess
import sys
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


if __name__ == "__main__":
    unittest.main()
