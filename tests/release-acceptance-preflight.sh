#!/usr/bin/env bash
# Host-free profile preflight declarations and the collector's live-dispatch barrier.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PYTHONDONTWRITEBYTECODE=1 python3 -B - "$ROOT" <<'PY'
from contextlib import redirect_stderr, redirect_stdout
import importlib.util
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("acceptance", Path(sys.argv[1]) / "dev/release-acceptance.py")
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
spec = importlib.util.spec_from_file_location("publication", Path(sys.argv[1]) / "dev/release-publication.py")
publication = importlib.util.module_from_spec(spec)
spec.loader.exec_module(publication)
sys.argv = [sys.argv[0]]


class ProfilePreflightTests(unittest.TestCase):
    def setUp(self):
        self.root = Path(self.enterContext(tempfile.TemporaryDirectory(dir="/tmp")))
        self.enterContext(patch.object(m, "completed_attempt", return_value={"id": "a" * 32, "sha256": "b" * 64}))
        self.addCleanup(m.STOP.clear)

    def inventory(self, declarations):
        rows = []
        for name, declaration in declarations.items():
            path = f"config/profiles/{name}/tests/e2e/acceptance.sh"
            directory = (self.root / path).parent
            directory.mkdir(parents=True, exist_ok=True)
            if declaration is not None:
                (directory / "acceptance-lanes.json").write_text(json.dumps(declaration))
            rows.append(f"runner\t{name}\t{path}")
        with patch.object(m.subprocess, "check_output", return_value="\n".join(rows)):
            return m.profile_inventory(self.root)

    def test_opt_in_preserves_lanes_and_profiles_without_preflights(self):
        command = ["bash", "config/profiles/sample/tests/run.sh", "--preflight"]
        inventory = self.inventory({
            "sample": {"schema_version": 1, "preflight": command,
                       "lanes": [{"id": "one", "arguments": ["--lane", "one"]}]},
            "single": {"schema_version": 1, "preflight": ["true"]},
            "plain": None,
        })
        with patch.object(m, "core_vm_checks", return_value=[]):
            checks = m.commands(self.root, "test", inventory)
        self.assertEqual(checks["profile-preflight:sample"], command)
        self.assertFalse(m.is_live("profile-preflight:sample"))
        self.assertEqual(checks["profile:sample/one"][-2:], ["--lane", "one"])
        self.assertEqual(checks["profile:single"], ["bash", inventory["single"]["path"]])
        self.assertNotIn("profile-preflight:plain", checks)

    def test_invalid_preflight_commands_are_refused(self):
        for command in ([], "true", [True], [""], ["true", ""], ["true", "\n"], ["true", "x" * 257], ["bash", "--slot", "1"]):
            with self.subTest(command=command), self.assertRaisesRegex(ValueError, "preflight command"):
                self.inventory({"sample": {"schema_version": 1, "preflight": command}})

    def run_collector(self, outcomes, *, only=(), local_failure=False, local_pending=False, excluded=(), stop=False, live="profile:plain", with_vms=True):
        # Synthetic profile names and commands; no installed services or leases.
        checks = {"verify": ["true"], "profile-preflight:zeta": ["true"],
                  "profile-preflight:alpha": ["true"], live: ["true"]}
        receipt = {"version": "test", "profiles": {}, "source_fingerprint": "a" * 64,
                   "candidate_bundle_sha256": "b" * 64, "external_obligations": {},
                   "checks": {name: {"status": "pending"} for name in checks}}
        if not with_vms:
            receipt["checks"][live] = m.OPTIONAL_VM_NOT_RUN.copy()
        if only and not local_pending:
            receipt["checks"]["verify"] = {"status": "passed"}
        calls = []
        native_execute = m.execute
        def execute(name, command, root, output, env, slot=None):
            calls.append((name, slot))
            if name.startswith("profile-preflight:"):
                self.assertEqual(env["SUBYARD_E2E_LOCAL_EVIDENCE"], str(self.root))
                self.assertNotIn("SUBYARD_E2E_CANDIDATE_BUNDLE", env)
                if stop:
                    m.STOP.set()
                    return {"status": "interrupted", "exit_code": -15}
            code = 9 if local_failure and name == "verify" else outcomes.get(name, 0)
            return native_execute(name, [sys.executable, "-c", f"raise SystemExit({code})"], root, output, env, slot)
        stdout = io.StringIO()
        with patch.object(m, "load_bound", return_value=(self.root, receipt)), \
             patch.object(m, "commands", return_value=checks), \
             patch.object(m, "execution_environment", return_value={}), \
             patch.object(m, "source_fingerprint", return_value="a" * 64), \
             patch.object(m, "execute", side_effect=execute), redirect_stdout(stdout):
            code = m.run_checks(self.root, [1] if with_vms else [], only, False, exclude=excluded, with_vms=with_vms)
        return code, calls, stdout.getvalue(), receipt

    def test_all_refusals_are_collected_in_stable_order_before_any_live_dispatch(self):
        code, calls, stdout, receipt = self.run_collector({"profile-preflight:alpha": 17, "profile-preflight:zeta": 23})
        self.assertEqual(code, 1)
        self.assertEqual(calls, [("verify", None), ("profile-preflight:alpha", None), ("profile-preflight:zeta", None)])
        self.assertIn("profile preflights refused: profile-preflight:alpha, profile-preflight:zeta", stdout)
        self.assertEqual(receipt["checks"]["profile-preflight:alpha"]["exit_code"], 17)
        self.assertEqual(receipt["checks"]["profile-preflight:zeta"]["exit_code"], 23)
        self.assertEqual(receipt["checks"]["profile:plain"]["status"], "pending")

    def test_successful_preflights_allow_full_live_dispatch(self):
        code, calls, _, _ = self.run_collector({})
        self.assertEqual(code, 0)
        self.assertEqual(calls, [("verify", None), ("profile-preflight:alpha", None), ("profile-preflight:zeta", None), ("profile:plain", 1)])

    def test_pending_preflights_allow_hosted_only_selection_and_match_report(self):
        selected = ["profile:sample"]
        code, calls, _, receipt = self.run_collector({}, only=selected, live=selected[0])
        self.assertEqual(code, 1)  # Other unrequested checks keep the aggregate incomplete.
        self.assertEqual(calls, [(selected[0], 1)])
        for name in ("profile-preflight:alpha", "profile-preflight:zeta"):
            self.assertEqual(receipt["checks"][name]["status"], "pending")
        invocation = json.loads(next((self.root / "runs").glob("*.json")).read_text())
        self.assertEqual(invocation["selected"], selected)
        self.assertEqual(invocation["selected_status"], "passed")
        self.assertEqual((invocation["state"], invocation["capture_status"]), ("finished", "recorded"))
        with patch.object(m, "load_bound", return_value=(self.root, receipt)), \
             redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()):
            report = m.preflight(self.root, only=selected, with_vms=True)
        self.assertEqual([item["id"] for item in report["checks"] if item["selected"]], selected)

    def test_explicit_preflight_selection_awaits_local_gates_without_dispatch(self):
        selected = ["profile-preflight:alpha"]
        code, calls, stdout, _ = self.run_collector({}, only=selected, local_pending=True)
        self.assertEqual(code, 1)
        self.assertEqual(calls, [])
        self.assertIn("INCOMPLETE: profile preflights await successful local gates", stdout)
        invocation = json.loads(next((self.root / "runs").glob("*.json")).read_text())
        self.assertEqual(invocation["selected"], selected)
        self.assertEqual(invocation["not_started"], [{"check": selected[0], "reason": "prerequisite-stop"}])
        self.assertEqual(invocation["selected_status"], "incomplete")

    def test_failed_local_gate_stops_before_profile_preflights(self):
        code, calls, _, _ = self.run_collector({}, local_failure=True)
        self.assertEqual(code, 1)
        self.assertEqual(calls, [("verify", None)])

    def test_no_vm_local_failure_retains_acceptance_summary_and_stop_reason(self):
        code, calls, stdout, _ = self.run_collector({}, local_failure=True, with_vms=False)
        self.assertEqual(code, 1)
        self.assertEqual(calls, [("verify", None)])
        self.assertEqual([line for line in stdout.splitlines() if line.startswith("ACCEPTANCE ")], [
            "ACCEPTANCE vm_optional_not_run=profile:plain status=not-run",
            f"ACCEPTANCE reproducible=failed overall=failed receipt={self.root / 'receipt.json'}",
        ])
        self.assertNotIn("await successful local gates", stdout)
        invocation = json.loads(next((self.root / "runs").glob("*.json")).read_text())
        self.assertEqual(invocation["not_started"], [
            {"check": name, "reason": "local-check-stop"}
            for name in ("profile-preflight:alpha", "profile-preflight:zeta")
        ])
        self.assertEqual((invocation["state"], invocation["selected_status"]), ("finished", "failed"))

    def test_preflight_dependency_cannot_be_excluded_for_full_live_selection(self):
        with self.assertRaisesRegex(ValueError, "cannot be excluded"):
            self.run_collector({}, excluded=["profile-preflight:alpha"])

    def test_interruption_stops_preflights_and_live_dispatch(self):
        code, calls, stdout, _ = self.run_collector({}, stop=True)
        self.assertEqual(code, 1)
        self.assertEqual(calls, [("verify", None), ("profile-preflight:alpha", None)])
        self.assertIn("INCOMPLETE: profile preflights interrupted: profile-preflight:alpha", stdout)
        self.assertIn("INCOMPLETE: profile preflights not run: profile-preflight:zeta", stdout)
        self.assertNotIn("profile preflights refused:", stdout)

    def test_accept_checks_hosted_fallback_honors_recorded_preflight_results(self):
        for status in ("failed", "blocked", "interrupted", "running"):
            with self.subTest(status=status), tempfile.TemporaryDirectory(dir="/tmp") as temporary:
                directory = Path(temporary)
                output = directory / "acceptance"
                output.mkdir(mode=0o700)
                live = "profile:sample"
                preflights = ["profile-preflight:alpha", "profile-preflight:zeta"]
                checks = {name: ["true"] for name in ("verify", *reversed(preflights), live)}
                receipt = {"version": "test", "profiles": {}, "source_fingerprint": "a" * 64,
                           "candidate_bundle_sha256": "b" * 64, "external_obligations": {},
                           "checks": {name: {"status": "pending"} for name in checks}}
                calls = []
                def execute(name, *args, **kwargs):
                    calls.append(name)
                    return {"status": status, "exit_code": 1} if name in preflights else {"status": "passed", "exit_code": 0}
                stdout = io.StringIO()
                with patch.object(publication, "a", m), \
                     patch.object(publication, "hosted_open", return_value=[live]), \
                     patch.object(publication, "wait_ci_hosted"), patch.object(publication, "collect_ci"), \
                     patch.object(m, "load_bound", return_value=(output, receipt)), \
                     patch.object(m, "commands", return_value=checks), \
                     patch.object(m, "execution_environment", return_value={}), \
                     patch.object(m, "source_fingerprint", return_value="a" * 64), \
                     patch.object(m, "execute", side_effect=execute), \
                     patch.object(m, "run_checks", wraps=m.run_checks) as collector, redirect_stdout(stdout):
                    self.assertEqual(publication.accept_checks(None, directory, {}, [1], [], True, [], 60), 1)
                self.assertEqual(collector.call_count, 2)
                self.assertEqual(collector.call_args_list[0].args[2], [])
                self.assertEqual(collector.call_args_list[1].args[2], [live])
                self.assertEqual(calls, ["verify", *preflights])
                label = "refused" if status in ("failed", "blocked") else "incomplete" if status == "running" else status
                message = "INCOMPLETE: profile preflights " + label + ": " + ", ".join(preflights)
                self.assertEqual(stdout.getvalue().count(message), 2)
                invocations = [json.loads(path.read_text()) for path in (output / "runs").glob("*.json")]
                self.assertEqual(len(invocations), 2)
                fallback = next(item for item in invocations if item["selected"] == [live])
                self.assertEqual(fallback["attempted"], [])
                self.assertEqual(fallback["not_started"], [{"check": live, "reason": "profile-preflight-stop"}])


unittest.main()
PY
