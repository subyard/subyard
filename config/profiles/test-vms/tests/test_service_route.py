#!/usr/bin/env python3
import importlib.util
import base64
import json
import os
import pathlib
import subprocess
import tempfile
import unittest

PROFILE = pathlib.Path(__file__).resolve().parents[1]
ROOT = PROFILE.parents[2]
spec = importlib.util.spec_from_file_location("service_route", PROFILE / "service-route.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
binding_spec = importlib.util.spec_from_file_location("service_binding", PROFILE / "service-binding.py")
binding_module = importlib.util.module_from_spec(binding_spec)
binding_spec.loader.exec_module(binding_module)


class ServiceRoute(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="subyard-service-route-", dir="/tmp")
        self.addCleanup(self.temp.cleanup)
        self.root = pathlib.Path(self.temp.name)
        for path in (self.root, self.root / "services", self.root / "services/test-vms"):
            path.mkdir(exist_ok=True)
            path.chmod(0o755)
        self.record = dict(schema_version=1, owner="owner-a", service="test-vms", pool="main",
                           yard="service-runtime", project="owned-project", instance="owned-instance",
                           runtime_uuid="original-uuid", registration_generation="a" * 64)
        self.publish(self.record)

    def publish(self, record):
        path = self.root / "services/test-vms" / (record["pool"] + ".json")
        path.write_text(json.dumps(record))
        path.chmod(0o644)
        return path

    def test_single_and_exact_pool(self):
        self.assertEqual(module.resolve(self.root), self.record)
        self.assertEqual(module.resolve(self.root, "owner-a", "main"), self.record)
        with self.assertRaisesRegex(ValueError, "service_absent"):
            module.resolve(self.root, "another-owner", "main")

    def test_ambiguity_requires_exact_selector(self):
        other = dict(self.record, pool="second")
        self.publish(other)
        with self.assertRaisesRegex(ValueError, "service_ambiguous"):
            module.resolve(self.root)
        self.assertEqual(module.resolve(self.root, pool="second"), other)

    def test_unsafe_files_refuse_instead_of_selecting_another(self):
        for kind in ("mode", "symlink", "hardlink", "extra", "identity"):
            with self.subTest(kind=kind):
                path = self.publish(self.record)
                if kind == "mode":
                    path.chmod(0o666)
                elif kind == "symlink":
                    original = path.with_suffix(".target")
                    path.rename(original)
                    path.symlink_to(original.name)
                elif kind == "hardlink":
                    original = path.with_suffix(".target")
                    os.link(path, original)
                elif kind == "extra":
                    path.write_text(json.dumps(dict(self.record, owner_endpoint="private")))
                else:
                    path.write_text(json.dumps(dict(self.record, registration_generation="invalid")))
                with self.assertRaises((ValueError, OSError)):
                    module.resolve(self.root)
                path.unlink()
                if kind in ("symlink", "hardlink"):
                    original.unlink()

    def test_unsafe_directory_refuses(self):
        directory = self.root / "services/test-vms"
        directory.chmod(0o777)
        with self.assertRaisesRegex(ValueError, "service_unavailable"):
            module.resolve(self.root)

    def test_pool_binding_ignores_heartbeats_and_fences_reused_slots(self):
        slot = dict(slot_id="slot-001", resource_generation=7, lease_epoch=3,
                    state="held", lease_id="public-lease-id", expires_at="before")
        response = dict(status="ok", pool=dict(resource_type="agent-e2e", resource_id="test-vms", slots=[slot]))
        before = binding_module.project_pool(response)
        slot["expires_at"] = "after"
        slot["last_heartbeat_at"] = "new-heartbeat"
        self.assertEqual(before, binding_module.project_pool(response))
        slot["lease_epoch"] += 1
        self.assertNotEqual(before["binding"], binding_module.project_pool(response)["binding"])
        self.assertIn("resource_generation=7 lease_epoch=4", binding_module.project_pool(response)["facts"][0])

    def test_cli_absence_has_stable_safe_diagnostic(self):
        result = subprocess.run(["python3", str(PROFILE / "service-route.py"), "--registry",
                                 str(self.root / "missing")], text=True, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("test environment unavailable: service_absent", result.stderr)
        self.assertEqual(result.stdout, "")

    def install_route(self):
        yard = self.root / self.record["yard"]
        directory = yard / ".route-fixture"
        directory.mkdir(parents=True)
        yard.chmod(0o755)
        directory.chmod(0o755)
        (yard / "current").symlink_to(directory.name)
        (directory / "route.tsv").write_text("subyard-e2e-route-v1\nhostname\t192.0.2.1\nport\t22\nhost_key_alias\tsubyard-e2e-bastion\n")
        key = base64.b64encode(b"\x00\x00\x00\x0bssh-ed25519\x00\x00\x00\x20" + bytes(32)).decode()
        (directory / "known_hosts").write_text("subyard-e2e-bastion ssh-ed25519 " + key + "\n")
        for path in directory.iterdir():
            path.chmod(0o644)
        return directory

    def test_trusted_route_returns_only_safe_public_locator(self):
        self.install_route()
        result = subprocess.run(["python3", str(PROFILE / "service-route.py"), "--registry", str(self.root)],
                                text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip().split("\t"),
                         [self.record[key] for key in ("owner", "pool", "yard", "registration_generation", "runtime_uuid")] + [".route-fixture"])

    def test_registered_service_with_missing_transport_is_unavailable(self):
        directory = self.install_route()
        yard = directory.parent
        for target in (yard, yard / "current", directory,
                       directory / "route.tsv", directory / "known_hosts"):
            with self.subTest(target=target.name):
                saved = target.with_name(target.name + "-missing")
                target.rename(saved)
                try:
                    result = subprocess.run(["python3", str(PROFILE / "service-route.py"),
                                             "--registry", str(self.root)], text=True, capture_output=True)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("service_unavailable", result.stderr)
                    self.assertNotIn("service_absent", result.stderr)
                    self.assertNotIn(str(self.root), result.stderr)
                    self.assertEqual(result.stdout, "")
                finally:
                    saved.rename(target)

    def test_missing_registration_is_absent_even_with_transport(self):
        self.install_route()
        (self.root / "services/test-vms/main.json").unlink()
        result = subprocess.run(["python3", str(PROFILE / "service-route.py"), "--registry", str(self.root)],
                                text=True, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("service_absent", result.stderr)
        self.assertNotIn("service_unavailable", result.stderr)

    def test_unsafe_route_is_not_followed(self):
        directory = self.install_route()
        self.assertEqual(module.route_generation(self.root, self.record), ".route-fixture")
        route = directory / "route.tsv"
        route.chmod(0o666)
        with self.assertRaisesRegex(ValueError, "service_unavailable"):
            module.route_generation(self.root, self.record)
        route.chmod(0o644)
        original = route.read_bytes()
        route.unlink()
        route.symlink_to("known_hosts")
        with self.assertRaises(OSError):
            module.route_generation(self.root, self.record)
        route.unlink()
        route.write_bytes(original)
        route.chmod(0o644)
        current = directory.parent / "current"
        current.unlink()
        current.symlink_to("../foreign")
        with self.assertRaisesRegex(ValueError, "unsafe route generation"):
            module.route_generation(self.root, self.record)

    def test_captured_endpoint_and_state_do_not_follow_rebinding(self):
        script = r'''
set -euo pipefail
. "$1/dev/agent-e2e.sh"
E2E_OWNER=owner-a E2E_POOL=main E2E_SERVICE_GENERATION=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
STATE_BASE="$2/state"
configure_yard_scope
case "$STATE_ROOT" in "$STATE_BASE/services/owner-a/main/$E2E_SERVICE_GENERATION") ;; *) exit 1 ;; esac
BASTION_HOSTNAME=192.0.2.1
BASTION_KNOWN_HOSTS="$2/captured-known-hosts"
printf 'original pin\n' > "$BASTION_KNOWN_HOSTS"
SUBYARD_E2E_BASTION_HOSTNAME=192.0.2.2
resolve_bastion_route
[ "$BASTION_HOSTNAME" = 192.0.2.1 ]
[ "$(cat "$BASTION_KNOWN_HOSTS")" = 'original pin' ]
E2E_OWNER=owner-b
configure_yard_scope
case "$STATE_ROOT" in "$STATE_BASE/services/owner-b/main/$E2E_SERVICE_GENERATION") ;; *) exit 1 ;; esac
'''
        result = subprocess.run(["bash", "-c", script, "service-route-test", str(ROOT), str(self.root)],
                                text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
