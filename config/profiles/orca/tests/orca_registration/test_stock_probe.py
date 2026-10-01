import argparse
import contextlib
import copy
import importlib.util
import io
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

import support
from reconcile import _records
from transport import RpcError

spec = importlib.util.spec_from_file_location(
    "stock_probe", Path(__file__).resolve().parents[1] / "e2e/orca-projects-helper.py"
)
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)


class StockProbeTests(unittest.TestCase):
    def test_valid_snapshot_and_recovery_shapes(self):
        snapshots = {"snapshots": [{"worktree": "repo-1::/synthetic/project",
                                    "tabs": [{"id": "tab-1", "type": "terminal"}]}]}
        self.assertEqual(snapshots["snapshots"], probe.snapshot_tabs(snapshots))
        self.assertEqual([], probe.snapshot_tabs({"snapshots": []}))
        for enabled in (True, False):
            document = {"settings": {"codexTerminalServerIsolation": enabled},
                        "repos": [{"id": "repo-1", "path": "/synthetic/project"}]}
            before = copy.deepcopy(document)
            self.assertIs(document, probe.recovery_document(document))
            self.assertEqual(before, document)
        self.assertEqual({"storage": "json"}, probe.cli_result(
            {"ok": True, "result": {"storage": "json"}}))

    def test_invalid_snapshots_and_settings_are_sanitized(self):
        marker = "synthetic-private-payload"
        for value in (None, [], {}, {"snapshots": None}, {"snapshots": {}},
                      {"snapshots": [marker]}, {"snapshots": [{"worktree": 1, "tabs": []}]},
                      {"snapshots": [{"worktree": marker, "tabs": {}}]},
                      {"snapshots": [{"worktree": marker, "tabs": [None]}]},
                      {"snapshots": [{"worktree": marker, "tabs": [{"id": ""}]}]}):
            with self.subTest(value=value), self.assertRaises(probe.SafeRpcError) as error:
                probe.snapshot_tabs(value)
            self.assertNotIn(marker, str(error.exception))
        for value in (None, [], {}, {"settings": []}, {"settings": {}},
                      {"settings": {"codexTerminalServerIsolation": marker}},
                      {"settings": {"codexTerminalServerIsolation": 1}}):
            with self.subTest(value=value), self.assertRaises(probe.SafeRpcError) as error:
                probe.recovery_document(value)
            self.assertNotIn(marker, str(error.exception))
        for value in (None, [], {}, {"ok": 1, "result": {}},
                      {"ok": True, "result": []}, {"ok": False, "error": marker}):
            with self.subTest(value=value), self.assertRaises(probe.SafeRpcError) as error:
                probe.cli_result(value)
            self.assertNotIn(marker, str(error.exception))

    def test_project_catalog_uses_registration_validation(self):
        rpc = support.Catalog()
        rpc.repos = [{"id": "repo-1", "path": "/synthetic/project", "kind": "folder"}]
        self.assertEqual(rpc.repos, _records(rpc, "repo.list", "repos"))
        for records in (None, [{}], [{"id": "repo-1", "path": None}], rpc.repos * 2):
            rpc.repos = records
            with self.subTest(records=records), self.assertRaises(RpcError):
                _records(rpc, "repo.list", "repos")

    def test_version_mismatch_is_unavailable_without_runtime_or_ambient_environment(self):
        arguments = argparse.Namespace(binary="/synthetic/orca", version="1.4.218",
                                       registration=str(support.COMPONENT))
        with patch.object(probe.subprocess, "run", return_value=subprocess.CompletedProcess(
            [], 0, stdout=b"1.4.212\n")) as run, patch.object(probe.subprocess, "Popen") as start:
            with contextlib.redirect_stderr(io.StringIO()) as output:
                self.assertEqual(2, probe.stock_probe(arguments))
            self.assertIn("unavailable", output.getvalue())
            start.assert_not_called()
            self.assertEqual(1, run.call_count)
            environment = run.call_args.kwargs["env"]
            self.assertEqual({"PATH", "HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"},
                             set(environment))
            self.assertNotEqual(str(Path.home()), environment["HOME"])

    def test_native_failure_does_not_echo_cli_output(self):
        arguments = argparse.Namespace(binary="/synthetic/orca", version="1.4.218",
                                       registration=str(support.COMPONENT))
        with patch.object(probe.subprocess, "run", side_effect=subprocess.CalledProcessError(
            1, [], output=b"synthetic-private-payload")):
            with contextlib.redirect_stderr(io.StringIO()) as output:
                self.assertEqual(1, probe.stock_probe(arguments))
            self.assertEqual("orca-stock-probe: version: failed\n", output.getvalue())

    def test_resource_settings_exact_updates_and_readback_order(self):
        rpc = unittest.mock.Mock()
        rpc.call.side_effect = [{}, {"settings": {"codexTerminalServerIsolation": False}},
                               {}, {"settings": {"codexTerminalServerIsolation": True}}]
        probe.check_resource_settings(rpc)
        self.assertEqual([
            unittest.mock.call("settings.update", {"codexTerminalServerIsolation": False}),
            unittest.mock.call("settings.get"),
            unittest.mock.call("settings.update", {"codexTerminalServerIsolation": True}),
            unittest.mock.call("settings.get"),
        ], rpc.call.call_args_list)

    def test_resource_settings_rejection_is_sanitized_and_not_retried(self):
        for prefix, operation in (([], "settings.update false"),
                                  ([{}], "settings.get after false"),
                                  ([{}, {"settings": {"codexTerminalServerIsolation": False}}],
                                   "settings.update true")):
            rpc = unittest.mock.Mock()
            rpc.call.side_effect = [*prefix, RuntimeError("synthetic-private-payload")]
            with self.subTest(operation=operation), self.assertRaises(probe.SafeRpcError) as error:
                probe.check_resource_settings(rpc)
            self.assertEqual(operation + " unavailable", str(error.exception))
            self.assertEqual(len(prefix) + 1, rpc.call.call_count)

    def test_resource_settings_invalid_boolean_projection_stops_before_next_write(self):
        for result in (None, [], {}, {"settings": None}, {"settings": []},
                       {"settings": {}}, {"settings": {"codexTerminalServerIsolation": 0}},
                       {"settings": {"codexTerminalServerIsolation": "synthetic-private-payload"}},
                       {"settings": {"codexTerminalServerIsolation": True}}):
            rpc = unittest.mock.Mock()
            rpc.call.side_effect = [{}, result]
            with self.subTest(result=result), self.assertRaises(probe.SafeRpcError) as error:
                probe.check_resource_settings(rpc)
            self.assertIn("settings.get after false", str(error.exception))
            self.assertNotIn("synthetic-private-payload", str(error.exception))
            self.assertEqual(2, rpc.call.call_count)

    def test_resource_settings_mode_is_opt_in(self):
        for flags, expected in (([], False), (["--resource-settings"], True)):
            with patch.object(probe.sys, "argv", ["helper", "stock-probe", "--version", "1.4.218", *flags]):
                with patch.object(probe, "stock_probe", return_value=0) as run:
                    self.assertEqual(0, probe.main())
            self.assertIs(expected, run.call_args.args[0].resource_settings)


if __name__ == "__main__":
    unittest.main()
