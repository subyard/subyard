#!/usr/bin/env python3
"""Exact package-native assessments protect provisioning and retirement."""
import copy
import importlib.util
import itertools
import json
import os
from pathlib import Path
import runpy
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

os.environ.update(INCUS_PROJECT="sample", YARD_INSTANCE_NAME="sample", SUBYARD_INTEGRATION_SELECTED="1")
spec = importlib.util.spec_from_file_location("observer_lifecycle", Path(__file__).resolve().parents[1] / "lifecycle.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class LifecycleTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.source = Path(__file__).resolve().parents[4]
        cls.native_root = cls.source / ".build/aiobserver-tests/native"
        cls.native_root.joinpath("bin").mkdir(parents=True, exist_ok=True)
        subprocess.run(["go", "build", "-buildvcs=false", "-o", str(cls.native_root / "bin/observation"), "./config/agents/aiobserver/cmd/observation"], cwd=cls.source, check=True, timeout=180)

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(dir="/tmp")
        self.addCleanup(temporary.cleanup)
        module.ROOT = Path(temporary.name)
        (module.ROOT / "provision.sh").write_text("package-v1")
        module.SELECTED = True
        module.ENV = {"DEV_USER": "dev", "DEV_UID": "1000", "CODING_TOOL_INTEGRATIONS": "codex aiobserver", "AI_OBSERVER_HOST_PORT": "22222", "SUBYARD_INTEGRATION_SOURCE_BINDING": "a" * 64, "YARD_KIND": "container"}
        module.ENV["SUBYARD_INTEGRATION_NATIVE_ROOT"] = str(self.native_root)
        self.snapshot = {"config": {module.PROXY: "v1:22222"}, "devices": {"ai-observer": {"type": "proxy", "bind": "host", "listen": "tcp:127.0.0.1:22222", "connect": "tcp:127.0.0.1:8080"}}}
        self.service = "owned-hash\nactive\nenabled\n"
        self.container = {"Name": "/subyard-ai-observer", "Id": "container-1", "Image": "sha256:image", "Config": {"Image": "owned-image", "Labels": {"org.subyard.managed": "ai-observer-v1"}, "Cmd": ["watch", "all", "--backfill"], "Env": ["AI_OBSERVER_API_PORT=8080"]}, "HostConfig": {"Binds": ["/data:/app/data:rw"], "PortBindings": {}}, "Mounts": [], "State": {"Running": True, "Status": "running", "Health": {"Status": "starting"}}}
        self.installed = True
        self.enumeration_error = self.inspection_error = False
        self.effects = []
        module.run = self.native_run
        module.guest = self.guest

    def native_run(self, *arguments, **options):
        if arguments[:2] == ("incus", "query"):
            output = json.dumps(self.snapshot).encode()
        elif arguments[0] == "bash":
            output = b"127.0.0.1\n"
        else:
            self.effects.append(arguments)
            output = b""
        return subprocess.CompletedProcess(arguments, 0, output, b"")

    def guest(self, *arguments, **options):
        if arguments[:4] == ("timeout", "10", "docker", "container"):
            if arguments[4] == "ls":
                if self.enumeration_error:
                    raise RuntimeError("native enumeration failed")
                output = b"subyard-ai-observer\n" if self.container else b""
            else:
                if self.inspection_error:
                    raise RuntimeError("native inspection failed")
                output = json.dumps([self.container]).encode()
            return subprocess.CompletedProcess(arguments, 0, output, b"")
        if arguments[0] == "sh":
            return subprocess.CompletedProcess(arguments, 0, self.service.encode(), b"")
        return subprocess.CompletedProcess(arguments, 0 if self.installed else 1, b"", b"")

    def apply(self, observed):
        with patch.object(sys, "argv", ["lifecycle.py", "apply", "sample-operation", observed["actual"], observed["desired"]]):
            module.main()

    def startup_fixture(self, states, enumeration_codes=(124, 0), probe_duration=0):
        clock, calls = [0], []
        states, codes = iter(states), iter(enumeration_codes)
        original_guest = self.guest

        def guest(*arguments, **options):
            if arguments[0] == "timeout" and arguments[2:5] == ("docker", "container", "ls"):
                self.assertLessEqual(float(arguments[1]), min(10, options.get("timeout", 20)))
                code = next(codes)
                calls.append(("enumerate", options))
                if code and options.get("check", True):
                    raise module.Failure("native-unavailable")
                return subprocess.CompletedProcess(arguments, code, b"subyard-ai-observer\n", b"PRIVATE native failure")
            if arguments[0] == "timeout" and arguments[2:4] == ("systemctl", "show"):
                self.assertLessEqual(float(arguments[1]), min(10, options["timeout"]))
                calls.append(("state", options))
                state = next(states)
                clock[0] += min(probe_duration, options["timeout"])
                if probe_duration >= options["timeout"]:
                    raise subprocess.TimeoutExpired("PRIVATE transport", options["timeout"])
                if isinstance(state, Exception):
                    raise state
                return subprocess.CompletedProcess(arguments, 0, state if isinstance(state, bytes) else state.encode(), b"")
            return original_guest(*arguments, **options)

        module.guest = guest
        for mocked in (patch.object(module.time, "monotonic", side_effect=lambda: clock[0]),
                       patch.object(module.time, "sleep", side_effect=lambda delay: clock.__setitem__(0, clock[0] + delay))):
            mocked.start()
            self.addCleanup(mocked.stop)
        return clock, calls

    def test_healthy_enumeration_does_not_wait_or_probe_service(self):
        _, calls = self.startup_fixture([], enumeration_codes=(0,))
        with patch.object(module.time, "sleep", side_effect=AssertionError("healthy API waited")):
            self.assertEqual(module.observe()[0]["state"], "stale")
        self.assertEqual(calls, [("enumerate", {"check": False})])
        self.assertEqual(self.effects, [])

    def test_docker_startup_and_completion_race_keep_native_ownership_required(self):
        for states in (("activating", "activating", "active"), ("active",)):
            for selected in (True, False):
                for foreign in (False, True):
                    with self.subTest(states=states, selected=selected, foreign=foreign):
                        module.SELECTED = selected
                        self.container["Config"]["Labels"]["org.subyard.managed"] = "foreign" if foreign else "ai-observer-v1"
                        _, calls = self.startup_fixture(states)
                        if foreign:
                            with self.assertRaisesRegex(module.Failure, "foreign-ownership"):
                                module.observe()
                        else:
                            self.assertEqual(module.observe()[0]["state"], "stale")
                        self.assertEqual([kind for kind, _ in calls].count("enumerate"), 2)
                        self.assertEqual([kind for kind, _ in calls].count("state"), len(states))
                        self.assertEqual(self.effects, [])

    def test_docker_startup_terminal_state_query_failure_and_second_read_failure(self):
        for state in ("inactive", "failed", "deactivating", "", "unknown", b"\xff",
                      module.Failure("native-unavailable"), subprocess.TimeoutExpired("PRIVATE", 20)):
            with self.subTest(state=type(state).__name__ if isinstance(state, Exception) else state):
                _, calls = self.startup_fixture((state,))
                with self.assertRaises((module.Failure, subprocess.TimeoutExpired)) as failure:
                    module.native_container_json()
                if isinstance(failure.exception, module.Failure):
                    self.assertEqual(failure.exception.reason, "native-unavailable")
                self.assertEqual([kind for kind, _ in calls], ["enumerate", "state"])
        _, calls = self.startup_fixture(("active",), enumeration_codes=(124, 1))
        with self.assertRaisesRegex(module.Failure, "native-unavailable"):
            module.native_container_json()
        self.assertEqual([kind for kind, _ in calls], ["enumerate", "state", "enumerate"])
        self.assertEqual(self.effects, [])

    def test_docker_startup_has_one_absolute_deadline_and_caps_transport(self):
        clock, calls = self.startup_fixture(itertools.repeat("activating"), probe_duration=2)
        with self.assertRaisesRegex(module.Failure, "native-unavailable"):
            module.native_container_json()
        self.assertEqual(clock[0], 60)
        self.assertEqual([kind for kind, _ in calls].count("enumerate"), 1)
        clock, calls = self.startup_fixture(("activating", "activating", "active"), probe_duration=19)
        module.native_container_json()
        self.assertEqual(calls[-1], ("enumerate", {"timeout": 1}))
        clock, calls = self.startup_fixture(("activating", "activating", "active"), probe_duration=19)
        original_guest = module.guest

        def late_enumeration(*arguments, **options):
            result = original_guest(*arguments, **options)
            if arguments[2:5] == ("docker", "container", "ls") and "timeout" in options:
                clock[0] += 1
            return result

        module.guest = late_enumeration
        with self.assertRaisesRegex(module.Failure, "native-unavailable"):
            module.native_container_json()
        self.assertEqual(clock[0], 60)
        clock, calls = self.startup_fixture(itertools.repeat("activating"), probe_duration=19.5)
        with self.assertRaises(subprocess.TimeoutExpired):
            module.native_container_json()
        self.assertEqual(clock[0], 60)
        self.assertEqual(calls[-1], ("state", {"timeout": 19}))
        self.assertEqual(self.effects, [])

    def test_initial_transport_failure_does_not_start_readiness_recovery(self):
        with patch.object(module, "guest", side_effect=subprocess.TimeoutExpired("PRIVATE transport", 20)) as guest:
            with self.assertRaises(subprocess.TimeoutExpired):
                module.native_container_json()
        self.assertEqual(guest.call_count, 1)
        self.assertEqual(self.effects, [])

    def test_probe_failures_keep_origin_duration_and_private_output_hidden(self):
        for stage, function in (("instance-query", module.observe), ("enumeration", module.native_container_json)):
            for error, category in ((subprocess.TimeoutExpired("PRIVATE argv", 20, output=b"PRIVATE stdout", stderr=b"PRIVATE stderr"), "transport-timeout"),
                                    (OSError("PRIVATE path"), "os-error"), (module.Failure("native-unavailable"), "native-exit")):
                with self.subTest(stage=stage, category=category):
                    name = "run" if stage == "instance-query" else "guest"
                    with patch.object(module, name, side_effect=error), patch.object(module.time, "monotonic", side_effect=(100, 100.125)):
                        with self.assertRaises(type(error)) as failed:
                            function()
                    self.assertIs(failed.exception, error)
                    self.assertEqual(error.readiness_diagnostic, (stage, category, 125, 20000))
                    self.assertNotIn("PRIVATE", str(error.readiness_diagnostic))

        for states, codes, stage, category in ((("failed",), (124,), "service-window", "service-failed"),
                                               (("PRIVATE state",), (124,), "service-window", "service-unknown"),
                                               (("active",), (124, 1), "recovery-enumeration", "native-exit"),
                                               ((subprocess.TimeoutExpired("PRIVATE", 20),), (124,), "service-state", "transport-timeout")):
            with self.subTest(stage=stage, category=category):
                self.startup_fixture(states, enumeration_codes=codes)
                with self.assertRaises((module.Failure, subprocess.TimeoutExpired)) as failed:
                    module.native_container_json()
                self.assertEqual(failed.exception.readiness_diagnostic[:2], (stage, category))
                self.assertNotIn("PRIVATE", str(failed.exception.readiness_diagnostic))
        self.startup_fixture(itertools.repeat("activating"), probe_duration=2)
        with self.assertRaises(module.Failure) as failed:
            module.native_container_json()
        self.assertEqual(failed.exception.readiness_diagnostic, ("service-window", "startup-deadline", 60000, 60000))

    def test_native_failure_terminal_record_does_not_expose_output_or_environment(self):
        package = Path(__file__).resolve().parents[1]
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            fake = Path(temporary) / "incus"
            fake.write_text("#!/bin/sh\nprintf 'PRIVATE native stdout'\nprintf 'PRIVATE native stderr' >&2\nexit 1\n")
            fake.chmod(0o755)
            environment = {"PATH": temporary + ":/usr/bin:/bin", "INCUS_PROJECT": "PRIVATE-project",
                           "YARD_INSTANCE_NAME": "PRIVATE-instance", "SUBYARD_INTEGRATION_SELECTED": "0",
                           "PRIVATE_KEY": "PRIVATE-value"}
            result = subprocess.run(["python3", str(package / "lifecycle.py"), "observe"],
                                    env=environment, capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, 1)
            self.assertEqual(result.stdout, "")
            lines = result.stderr.splitlines()
            self.assertEqual(len(lines), 2)
            self.assertEqual(lines[0], module.DIAGNOSTIC + "inspect native-unavailable")
            self.assertRegex(lines[1], r"^subyard-readiness-diagnostic-v1 inspect instance-query native-exit [0-9]+ 20000$")
            self.assertLessEqual(len(lines[1]), 160)
            self.assertNotIn("PRIVATE", result.stderr)
        with patch.object(module.time, "monotonic", side_effect=(0, 999999999)):
            with self.assertRaises(OSError) as failed:
                module.probe("instance-query", 20, lambda: (_ for _ in ()).throw(OSError("PRIVATE")))
        self.assertEqual(failed.exception.readiness_diagnostic, ("instance-query", "os-error", 1320000, 20000))

    def test_later_native_probes_preserve_failure_origin(self):
        for stage in ("inspection", "ownership", "installation", "native-producer"):
            with self.subTest(stage=stage):
                error = subprocess.TimeoutExpired("PRIVATE argv", 20, stderr=b"PRIVATE stderr")
                original = self.guest
                def guest(*arguments, **options):
                    if stage == "inspection" and arguments[2:5] == ("docker", "container", "inspect") or stage == "ownership" and arguments[0] == "sh" or stage == "installation" and arguments[0] == "/usr/local/bin/ai-observer-installed":
                        raise error
                    return original(*arguments, **options)
                with patch.object(module, "guest", guest), patch.object(module, "_producer_run", side_effect=error) if stage == "native-producer" else patch.object(module, "_producer_run", module._producer_run):
                    with self.assertRaises(subprocess.TimeoutExpired) as failed:
                        module.observe()
                self.assertEqual(failed.exception.readiness_diagnostic[:2], (stage, "transport-timeout"))
                self.assertNotIn("PRIVATE", str(failed.exception.readiness_diagnostic))

    def test_released_updater_fixture_observes_absence_and_rejects_mutation(self):
        package = Path(__file__).resolve().parents[1]
        harness = runpy.run_path(str(package.parents[2] / "dev/verify-release-upgrades.py"))
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            root = Path(temporary)
            control, rejected = root / "control.json", root / "rejected"
            snapshot = {"name": "yard", "status": "Running", "type": "container",
                        "config": {}, "devices": {}, "expanded_config": {}, "expanded_devices": {}}
            control.write_text(json.dumps({"instances": [{"info": snapshot}]}))
            harness["write_observation_incus"](root, control, rejected)
            environment = {"PATH": str(root) + ":/usr/bin:/bin", "INCUS_PROJECT": "subyard",
                           "YARD_INSTANCE_NAME": "yard", "SUBYARD_INTEGRATION_SELECTED": "0",
                           "SUBYARD_INTEGRATION_SOURCE_BINDING": "a" * 64,
                           "SUBYARD_INTEGRATION_NATIVE_ROOT": str(self.native_root)}
            result = subprocess.run(["python3", str(package / "lifecycle.py"), "observe"],
                                    env=environment, capture_output=True, text=True, timeout=30)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(json.loads(result.stdout), {"state": "absent", "actual": "", "desired": ""})
            self.assertFalse(rejected.exists())
            query = subprocess.run([str(root / "incus"), "query", "/1.0/instances/yard?project=subyard"],
                                   capture_output=True, text=True, timeout=10)
            self.assertEqual(json.loads(query.stdout), snapshot)
            guest = ["exec", "yard", "--project", "subyard", "--"]
            for arguments in (["config", "set", "yard", "key", "value"],
                              guest + ["timeout", "20", "docker", "stop", "container"],
                              guest + ["sh", "-eu", "-c", "true"],
                              guest + ["sh", "-eu", "-c", module.CONTROL_ASSESSMENT + "\n"],
                              guest + ["bash", "-c", "true"],
                              ["query", "/1.0/instances/foreign?project=subyard"],
                              ["query", "/1.0/instances/yard?project=foreign"], ["unknown"]):
                with self.subTest(arguments=arguments[:2]):
                    result = subprocess.run([str(root / "incus"), *arguments],
                                            capture_output=True, timeout=10)
                    self.assertEqual(result.returncode, 99)
                    self.assertEqual(rejected.read_text(), "incus operation rejected")
                    rejected.unlink()

    def test_exact_context_and_owned_retirement(self):
        _, context, _, _ = module.observe()
        self.snapshot["config"][module.PROVISION] = context
        self.assertEqual(module.observe()[0]["state"], "current")
        for key, value in (("CODING_TOOL_INTEGRATIONS", "claude codex aiobserver"), ("HOST_BASE", "/new/mount"), ("SUBYARD_INTEGRATION_SOURCE_BINDING", "b" * 64), ("AI_OBSERVER_HOST_PORT", "22223")):
            before = module.ENV.get(key)
            module.ENV[key] = value
            self.assertEqual(module.observe()[0]["state"], "stale")
            if before is None:
                del module.ENV[key]
            else:
                module.ENV[key] = before
        (module.ROOT / "provision.sh").write_text("package-v2")
        self.assertEqual(module.observe()[0]["state"], "stale")
        (module.ROOT / "provision.sh").write_text("package-v1")
        self.snapshot["config"][module.PROXY] = "v1:pending:22222"
        self.assertEqual(module.observe()[0]["state"], "stale")
        self.snapshot["devices"]["ai-observer"]["connect"] = "tcp:127.0.0.1:9999"
        with self.assertRaises(RuntimeError):
            module.observe()
        self.snapshot["devices"] = {}
        self.snapshot["config"].pop(module.PROXY)
        module.ENV["YARD_KIND"] = "vm"
        self.assertEqual(module.observe()[0]["state"], "current")
        module.SELECTED = False
        self.assertEqual(module.observe()[0]["state"], "stale")
        self.snapshot["config"].pop(module.PROVISION)
        self.service = "owned-hash\n"
        self.assertEqual(module.observe()[0]["state"], "stale")
        self.container["State"]["Running"] = False
        self.assertEqual(module.observe()[0]["state"], "current")
        self.service = ""
        with self.assertRaises(RuntimeError):
            module.observe()  # An orphan owned container is not absence.
        self.container = None
        self.assertEqual(module.observe()[0]["state"], "absent")

    def test_native_mount_order_preserves_approval_and_exact_mount_guards(self):
        mounts = [
            {"Type": "bind", "Source": "/fixtures/claude", "Destination": "/sessions/claude", "Mode": "ro", "RW": False, "Propagation": "rprivate"},
            {"Type": "bind", "Source": "/fixtures/codex", "Destination": "/sessions/codex", "Mode": "ro", "RW": False, "Propagation": "rprivate"},
            {"Type": "bind", "Source": "/fixtures/data", "Destination": "/app/data", "Mode": "rw", "RW": True, "Propagation": "rprivate"},
        ]
        self.container["Mounts"] = copy.deepcopy(mounts)
        approved = module.observe()[0]
        self.assertEqual(approved["state"], "stale")
        for permutation in itertools.permutations(mounts):
            with self.subTest(permutation=permutation):
                self.container["Mounts"] = list(permutation)
                self.assertEqual(module.observe()[0], approved)
        changes = [mounts[1:], mounts + [dict(mounts[0], Destination="/other")], mounts + [mounts[0]]]
        for key, value in (("Type", "volume"), ("Source", "/other"), ("Destination", "/other"),
                           ("Mode", "rw"), ("RW", True), ("Propagation", "shared"), ("Driver", "foreign")):
            changed = copy.deepcopy(mounts)
            changed[0][key] = value
            changes.append(changed)
        for changed in changes:
            with self.subTest(mounts=changed):
                self.container["Mounts"] = changed
                observed = module.observe()[0]
                self.assertEqual(observed["desired"], approved["desired"])
                self.assertNotEqual(observed["actual"], approved["actual"])
                with self.assertRaisesRegex(RuntimeError, "observation-changed"):
                    self.apply(approved)
                self.assertEqual(self.effects, [])

    def test_owned_native_drift_invalidates_approval_before_any_effect(self):
        self.installed = False
        original = copy.deepcopy(self.container)
        for section, key, value in (("Config", "Image", "different-image"), ("Config", "Cmd", ["watch", "codex"]), ("Config", "Env", ["changed=true"]), ("HostConfig", "Binds", ["/other:/app/data:rw"]), ("State", "Running", False)):
            with self.subTest(section=section, key=key):
                self.container = copy.deepcopy(original)
                observed = module.observe()[0]
                self.container[section][key] = value
                changed = module.observe()[0]
                self.assertEqual(observed["desired"], changed["desired"])
                self.assertNotEqual(observed["actual"], changed["actual"])
                with self.assertRaisesRegex(RuntimeError, "observation-changed"):
                    self.apply(observed)
                self.assertEqual(self.effects, [])
        self.container = original
        before = module.observe()[0]
        self.container["Id"] = "replacement-container"
        with self.assertRaisesRegex(RuntimeError, "observation-changed"):
            self.apply(before)
        self.assertEqual(self.effects, [])
        self.container["Id"] = "container-1"
        self.container["State"]["Health"]["Status"] = "healthy"
        self.assertEqual(before, module.observe()[0])
        del self.container["Id"]
        with self.assertRaises(RuntimeError):
            module.observe()

    def test_foreign_or_ambiguous_container_never_writes_a_pending_receipt(self):
        self.installed = False
        observed = module.observe()[0]
        self.service = ""  # Missing controls must not hide same-name ownership.
        for selected in (True, False):
            module.SELECTED = selected
            for failure in ("foreign", "orphan", "enumeration", "inspection"):
                with self.subTest(selected=selected, failure=failure):
                    self.container["Config"]["Labels"]["org.subyard.managed"] = "foreign" if failure == "foreign" else "ai-observer-v1"
                    self.enumeration_error = failure == "enumeration"
                    self.inspection_error = failure == "inspection"
                    with self.assertRaises(RuntimeError):
                        module.observe()
                    with self.assertRaises(RuntimeError):
                        self.apply(observed)
                    self.assertEqual(self.effects, [])

    def test_mount_permutation_and_unknown_field_drift_before_effects(self):
        self.installed = False
        mounts = [{"Type": "bind", "Source": "/one", "Destination": "/data", "RW": True, "Future": {"count": 9007199254740993}},
                  {"Type": "bind", "Source": "/two", "Destination": "/logs", "RW": False}]
        for selected in (True, False):
            module.SELECTED = selected
            self.container["Mounts"] = copy.deepcopy(mounts)
            before = module.observe()[0]
            self.container["Mounts"].reverse()
            self.assertEqual(module.observe()[0], before)
            for changed in (copy.deepcopy(mounts) + [copy.deepcopy(mounts[0])],
                            [dict(mounts[0], Added=True), mounts[1]]):
                self.container["Mounts"] = changed
                with self.assertRaisesRegex(RuntimeError, "observation-changed"):
                    self.apply(before)
                self.assertEqual(self.effects, [])
            self.container["Mounts"] = copy.deepcopy(mounts)
            self.container["HostConfig"]["Future"] = 9007199254740993
            before = module.observe()[0]
            self.container["HostConfig"]["Future"] = 9007199254740992
            with self.assertRaisesRegex(RuntimeError, "observation-changed"):
                self.apply(before)
            self.assertEqual(self.effects, [])

    def test_inherited_provision_receipt_fails_before_any_effect(self):
        self.installed = False
        for selected in (True, False):
            with self.subTest(selected=selected):
                module.SELECTED = selected
                self.snapshot["config"][module.PROVISION] = "a" * 64
                self.snapshot["expanded_config"] = copy.deepcopy(self.snapshot["config"])
                observed = module.observe()[0]
                # Effective bytes stay identical, but local receipt ownership
                # disappeared after approval. It must not be claimed or unset.
                del self.snapshot["config"][module.PROVISION]
                with self.assertRaisesRegex(RuntimeError, "ambiguous-ownership"):
                    module.observe()
                with self.assertRaisesRegex(RuntimeError, "ambiguous-ownership"):
                    self.apply(observed)
                self.assertEqual(self.effects, [])


    def test_published_legacy_controls_observation_and_strict_refusals(self):
        native = module.ROOT / "native-controls"
        native.mkdir()
        binary = native / "bin"
        binary.mkdir()
        fixture = Path(__file__).resolve().parent / "fixtures/wrapper-v0.14.0.sh"
        wrapper = binary / "ai-observer"
        wrapper.write_bytes(fixture.read_bytes())
        wrapper.chmod(0o755)
        files = {
            "/usr/local/bin/ai-observer": wrapper,
            "/etc/subyard/ai-observer/managed": native / "managed",
            "/etc/systemd/system/subyard-ai-observer.service": native / "unit",
            "/usr/local/bin/ai-observer-check": native / "check",
            "/usr/local/bin/ai-observer-installed": native / "installed",
            "/usr/local/bin/ai-observer-health": native / "health",
        }
        for name, mode, content in (("managed", 0o644, "subyard-ai-observer-v1"), ("unit", 0o644, "# Managed by Subyard AI Observer provision."), ("check", 0o755, "# Managed by Subyard AI Observer provision.")):
            (native / name).write_text(content + "\n")
            (native / name).chmod(mode)
        systemctl = binary / "systemctl"
        systemctl.write_text("#!/bin/sh\nexit 1\n")
        systemctl.chmod(0o755)
        environment = {"PATH": str(binary) + ":/usr/bin:/bin", "FIXTURE_ROOT": str(native)}
        unsupported = subprocess.run(["bash", str(wrapper), "assess-disable"], env=environment, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.assertEqual(unsupported.returncode, 2)  # The actual legacy dispatch has no such command.
        real_run, base_guest = subprocess.run, self.guest
        script = module.CONTROL_ASSESSMENT
        for original, path in sorted(files.items(), key=lambda item: -len(item[0])):
            script = script.replace(original, str(path))
        script = script.replace("'0:0'", f"'{os.getuid()}:{os.getgid()}'")

        def guest(*arguments, **options):
            if arguments == ("sh", "-eu", "-c", module.CONTROL_ASSESSMENT):
                result = real_run(["sh", "-eu", "-c", script], env=environment, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                if result.returncode:
                    raise RuntimeError("native controls assessment failed")
                return result
            return base_guest(*arguments, **options)

        module.guest = guest
        self.installed = False  # Legacy controls are valid; current install remains pending.
        for selected in (True, False):
            module.SELECTED = selected
            observed = module.observe()[0]
            self.assertEqual(observed["state"], "stale")
            self.assertEqual(self.effects, [])
            for failure in ("foreign", "symlink", "mode", "partial-current", "declared-current"):
                with self.subTest(selected=selected, failure=failure):
                    original = wrapper.read_bytes()
                    if failure == "foreign":
                        wrapper.write_text("#!/bin/sh\n# Foreign wrapper\n")
                    elif failure == "symlink":
                        (native / "check").rename(native / "check.real")
                        (native / "check").symlink_to("check.real")
                    elif failure == "mode":
                        wrapper.chmod(0o777)
                    elif failure == "partial-current":
                        (native / "installed").write_text("# Managed by Subyard AI Observer provision.\n")
                        (native / "installed").chmod(0o755)
                    else:
                        wrapper.write_bytes(original + b"\ninstalled_path=/usr/local/bin/ai-observer-installed\n")
                    with self.assertRaisesRegex(RuntimeError, "native controls"):
                        module.observe()
                    with self.assertRaisesRegex(RuntimeError, "native controls"):
                        self.apply(observed)
                    self.assertEqual(self.effects, [])
                    wrapper.write_bytes(original)
                    wrapper.chmod(0o755)
                    if failure == "symlink":
                        (native / "check").unlink()
                        (native / "check.real").rename(native / "check")
                    (native / "installed").unlink(missing_ok=True)
            # Complete current controls are accepted too; byte drift is bound.
            for name in ("installed", "health"):
                (native / name).write_text("# Managed by Subyard AI Observer provision.\n")
                (native / name).chmod(0o755)
            current = module.observe()[0]
            (native / "health").write_text("# Managed by Subyard AI Observer provision.\n# changed\n")
            self.assertNotEqual(current["actual"], module.observe()[0]["actual"])
            for name in ("installed", "health"):
                (native / name).unlink()

    def test_legacy_wrapper_retirement_retry_and_foreign_identity_guards(self):
        module.SELECTED = False
        self.service = "owned-hash\n"  # Unit is already disabled and inactive.
        native_root = module.ROOT / "native"
        native_root.mkdir()
        binary_root = native_root / "bin"
        binary_root.mkdir()
        (native_root / "managed").write_text("subyard-ai-observer-v1\n")
        (native_root / "unit").write_text("# Managed by Subyard AI Observer provision.\n")
        (native_root / "check").write_text("# Managed by Subyard AI Observer provision.\n")
        (native_root / "owner").write_text("ai-observer-v1")
        (native_root / "running").write_text("true")
        (native_root / "data").write_bytes(b"retained database and checkpoints")
        systemctl = binary_root / "systemctl"
        systemctl.write_text('#!/bin/sh\n[ "$*" = "disable --now subyard-ai-observer.service" ] || exit 2\nprintf "%s\\n" "$*" >> "$FIXTURE_ROOT/systemctl.log"\n')
        systemctl.chmod(0o755)
        docker = binary_root / "docker"
        docker.write_text('#!/bin/sh\ncase "$*" in "container inspect subyard-ai-observer") exit 0 ;; "inspect -f "*) cat "$FIXTURE_ROOT/owner"; exit ;; esac\n[ "$*" = "stop --time 10 container-1" ] || exit 2\n[ "$(cat "$FIXTURE_ROOT/owner")" = ai-observer-v1 ] || exit 3\nprintf "%s\\n" "$*" >> "$FIXTURE_ROOT/docker.log"\nif [ -f "$FIXTURE_ROOT/stop-fail" ]; then rm "$FIXTURE_ROOT/stop-fail"; exit 4; fi\nprintf false > "$FIXTURE_ROOT/running"\n')
        docker.chmod(0o755)
        environment = {"PATH": str(binary_root) + ":/usr/bin:/bin", "FIXTURE_ROOT": str(native_root)}
        source_fixture = Path(__file__).resolve().parent / "fixtures/wrapper-v0.14.0.sh"
        fixture = native_root / "wrapper"
        fixture.write_bytes(source_fixture.read_bytes())
        fixture.chmod(0o755)
        real_run, base_guest = subprocess.run, self.guest
        mutation = None

        def execute(*arguments):
            result = real_run(arguments, env=environment, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10)
            if result.returncode:
                raise RuntimeError("native retirement failed")
            return result

        # Execute the old dispatch itself and establish its observable limitation.
        execute("bash", str(fixture), "disable")
        self.assertEqual((native_root / "running").read_text(), "true")

        def guest(*arguments, **options):
            if arguments[0] == "sh" and "/usr/local/bin/ai-observer disable" in arguments[-1]:
                result = execute("bash", str(fixture), "disable")
                if mutation == "foreign":
                    (native_root / "owner").write_text("foreign")
                elif mutation == "replacement":
                    self.container["Id"] = "replacement-container"
                return result
            if arguments[:4] == ("timeout", "20", "docker", "stop"):
                return execute("docker", *arguments[3:])
            self.container["Config"]["Labels"]["org.subyard.managed"] = (native_root / "owner").read_text()
            self.container["State"]["Running"] = (native_root / "running").read_text() == "true"
            return base_guest(*arguments, **options)

        def publication(*arguments, **options):
            self.snapshot["devices"].pop("ai-observer", None)
            self.snapshot["config"].pop(module.PROXY, None)
            self.snapshot["config"].pop(module.PROVISION, None)
            return subprocess.CompletedProcess(arguments, 0, b"", b"")

        module.guest = guest
        observed = module.observe()[0]
        (native_root / "stop-fail").touch()
        with patch.object(module.subprocess, "run", publication):
            with self.assertRaisesRegex(RuntimeError, "native retirement failed"):
                self.apply(observed)
            self.assertEqual((native_root / "running").read_text(), "true")
            self.assertIn(module.PROXY, self.snapshot["config"])
            self.apply(module.observe()[0])  # Fresh assessment retries partial retirement.
        self.assertEqual(module.observe()[0]["state"], "current")
        self.assertEqual(self.container["Id"], "container-1")
        self.assertEqual((native_root / "data").read_bytes(), b"retained database and checkpoints")
        calls = (native_root / "docker.log").read_text()
        self.assertEqual(calls.splitlines(), ["stop --time 10 container-1"] * 2)
        execute("bash", str(fixture), "disable")
        self.assertEqual(module.observe()[0]["state"], "current")
        self.assertEqual((native_root / "docker.log").read_text(), calls)

        for mutation in ("foreign", "replacement"):
            with self.subTest(mutation=mutation):
                (native_root / "owner").write_text("ai-observer-v1")
                (native_root / "running").write_text("true")
                self.container["Id"] = "container-1"
                observed = module.observe()[0]
                with patch.object(module.subprocess, "run", publication):
                    with self.assertRaises(RuntimeError):
                        self.apply(observed)
                self.assertEqual((native_root / "docker.log").read_text(), calls)
                self.assertEqual((native_root / "running").read_text(), "true")
                self.assertEqual((native_root / "data").read_bytes(), b"retained database and checkpoints")


if __name__ == "__main__":
    unittest.main()
