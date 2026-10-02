import argparse
import contextlib
import copy
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import tempfile
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

    def test_isolation_recovery_preserves_full_document_for_both_values(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            data_file = root / "data.json"
            document = {"settings": {"codexTerminalServerIsolation": True,
                                     "other": "synthetic-private-payload"},
                        "repos": [{"id": "repo-1"}], "grants": [{"id": "client-1"}]}
            data_file.write_text(json.dumps(document))
            cli = unittest.mock.Mock(side_effect=lambda *command: json.dumps({
                "ok": True, "result": {"dataFile": str(data_file)} if command[2] == "exports"
                else {"storage": "json", "restoredPath": str(data_file)}
            }))
            for before, value in ((True, False), (False, True)):
                self.assertIs(value, probe.isolation_settings(cli, root, value, before))
                document["settings"]["codexTerminalServerIsolation"] = value
                self.assertEqual(document, json.loads(data_file.read_text()))
                self.assertIs(value, probe.isolation_settings(cli, root, expected=value))
            self.assertEqual([
                unittest.mock.call("profile", "state", "exports", "--json"),
                unittest.mock.call("profile", "state", "rollback", "--current-sqlite", "--json"),
                unittest.mock.call("profile", "state", "rollback", "--current-json", "--json"),
                unittest.mock.call("profile", "state", "exports", "--json"),
                unittest.mock.call("profile", "state", "rollback", "--current-sqlite", "--json"),
            ] * 2, cli.call_args_list)

    def test_isolation_readback_mismatch_stops_before_write(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            data_file = root / "data.json"
            before = json.dumps({"settings": {"codexTerminalServerIsolation": True}})
            data_file.write_text(before)
            cli = unittest.mock.Mock(side_effect=[
                json.dumps({"ok": True, "result": {"dataFile": str(data_file)}}),
                json.dumps({"ok": True, "result": {}}),
            ])
            with self.assertRaisesRegex(probe.SafeRpcError, "readback mismatch"):
                probe.isolation_settings(cli, root, value=True, expected=False)
            self.assertEqual(before, data_file.read_text())
            self.assertEqual(2, cli.call_count)

    def test_isolation_export_rejects_escape_and_symlink_before_recovery(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            link = root / "link.json"
            link.symlink_to(root / "data.json")
            for path in (root.parent / "synthetic-private-payload.json", link):
                cli = unittest.mock.Mock(return_value=json.dumps({
                    "ok": True, "result": {"dataFile": str(path)}}))
                with self.subTest(path=path), self.assertRaises(probe.SafeRpcError) as error:
                    probe.isolation_settings(cli, root, False)
                self.assertEqual("recovery export escaped profile", str(error.exception))
                self.assertEqual(1, cli.call_count)

    def test_isolation_import_result_is_validated(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            data_file = root / "data.json"
            for result in ({}, {"storage": "sqlite", "restoredPath": str(data_file)},
                           {"storage": "json", "restoredPath": "synthetic-private-payload"}):
                data_file.write_text(json.dumps({"settings": {"codexTerminalServerIsolation": True}}))
                cli = unittest.mock.Mock(side_effect=[
                    json.dumps({"ok": True, "result": {"dataFile": str(data_file)}}),
                    json.dumps({"ok": True, "result": {}}),
                    json.dumps({"ok": True, "result": result}),
                ])
                with self.subTest(result=result), self.assertRaises(probe.SafeRpcError) as error:
                    probe.isolation_settings(cli, root, False)
                self.assertEqual("invalid recovery import result", str(error.exception))
                self.assertEqual(3, cli.call_count)

    def test_isolation_command_failure_does_not_echo_private_payload(self):
        arguments = argparse.Namespace(binary="/synthetic/orca", state="/synthetic/profile",
                                       value="false", expect=None)
        with patch.object(probe.subprocess, "run", side_effect=subprocess.CalledProcessError(
            1, [], output=b"synthetic-private-payload")):
            with contextlib.redirect_stderr(io.StringIO()) as output:
                self.assertEqual(1, probe.set_isolation(arguments))
            self.assertEqual("orca-isolation: stock recovery failed\n", output.getvalue())

    def test_resource_settings_mode_is_opt_in(self):
        for flags, expected in (([], False), (["--resource-settings"], True)):
            with patch.object(probe.sys, "argv", ["helper", "stock-probe", "--version", "1.4.218", *flags]):
                with patch.object(probe, "stock_probe", return_value=0) as run:
                    self.assertEqual(0, probe.main())
            self.assertIs(expected, run.call_args.args[0].resource_settings)


if __name__ == "__main__":
    unittest.main()
