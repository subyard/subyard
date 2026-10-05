import json
import os
import shutil
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import threading
import unittest

from support import Catalog, COMPONENT, init_git, project


class MainTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.workspaces = Path(self.tmp.name) / "workspaces"
        self.root = project(self.workspaces)
        self.state = Path(self.tmp.name) / "state"
        self.rpc = Catalog()

    def run_cli(self, command):
        result = subprocess.run([sys.executable, "-B", str(COMPONENT / "main.py"), command,
                                 "--host-name", "owner-host",
                                 "--workspaces", str(self.workspaces), "--state", str(self.state)],
                                capture_output=True, text=True, timeout=5)
        self.assertEqual("", result.stderr)
        self.assertEqual(1, len(result.stdout.splitlines()))
        return result.returncode, json.loads(result.stdout)

    def start_server(self):
        metadata = self.state / "config/orca/orca-runtime.json"
        metadata.parent.mkdir(parents=True)
        self.state.chmod(0o700)
        endpoint = str(Path(self.tmp.name) / "socket")
        metadata.write_text(json.dumps({"transports": [{"kind": "unix", "endpoint": endpoint}],
                                       "runtimeId": "runtime-1", "authToken": "fixture-token"}))
        listener = socket.socket(socket.AF_UNIX)
        listener.bind(endpoint)
        listener.listen()
        listener.settimeout(0.05)
        stopped = threading.Event()
        failures = []

        def serve():
            while not stopped.is_set():
                try:
                    connection, _ = listener.accept()
                except socket.timeout:
                    continue
                try:
                    with connection:
                        connection.settimeout(2)
                        with connection.makefile("rb") as source:
                            request = json.loads(source.readline())
                        if request["authToken"] != "fixture-token":
                            raise AssertionError("wrong token")
                        result = self.rpc.call(request["method"], request.get("params"))
                        response = {"id": request["id"], "ok": True, "result": result,
                                    "_meta": {"runtimeId": "runtime-1"}}
                        connection.sendall(json.dumps(response).encode() + b"\n")
                except Exception as error:
                    failures.append(error)
            listener.close()

        thread = threading.Thread(target=serve)
        thread.start()

        def finish():
            stopped.set()
            thread.join(3)
            self.assertFalse(thread.is_alive())
            self.assertEqual([], failures)
        self.addCleanup(finish)

    def test_unavailable_runtime_outputs_nonzero_json_without_creating_status_state(self):
        code, report = self.run_cli("status")
        self.assertNotEqual(0, code)
        self.assertFalse(report["ready"])
        self.assertEqual(1, report["total"])
        self.assertTrue(report["errors"])
        self.assertFalse(self.state.exists())

    def test_full_cli_wire_sync_status_and_manual_membership_repair(self):
        init_git(self.root / "nested")
        self.start_server()
        self.assertEqual(0, self.run_cli("discover")[0])
        code, report = self.run_cli("sync")
        self.assertEqual(0, code, report)
        self.assertEqual((2, 2), (report["registered"], report["total"]))
        self.assertEqual("Sample / owner-host", self.rpc.groups[0]["name"])
        self.rpc.calls.clear()
        code, report = self.run_cli("status")
        self.assertEqual(0, code, report)
        self.assertTrue(all(method.endswith(".list") for method, _ in self.rpc.calls))
        self.rpc.repos[1]["projectGroupId"] = None
        code, report = self.run_cli("status")
        self.assertNotEqual(0, code)
        self.assertEqual((1, 2), (report["registered"], report["total"]))
        self.assertEqual(0, self.run_cli("sync")[0])
        self.assertEqual(1, len(self.rpc.groups))

    def test_cli_capacity_refuses_new_roots_and_recovers_after_a_slot_is_freed(self):
        # Synthetic native records exercise real CLI/RPC without mass Git roots.
        self.rpc.repos = [{"id": "manual-" + str(index), "path": "/manual/" + str(index),
                           "kind": "folder"} for index in range(1000)]
        self.start_server()
        for command in ("discover", "status", "sync"):
            code, report = self.run_cli(command)
            self.assertNotEqual(0, code)
            self.assertEqual(1, report["deferred"])
            self.assertIn("admission limit reached", " ".join(report["errors"]))
            self.assertEqual(1000, len(self.rpc.repos))
            self.assertEqual([], self.rpc.groups)
        self.assertTrue(all(method.endswith(".list") for method, _ in self.rpc.calls))
        self.rpc.repos.pop()
        code, report = self.run_cli("sync")
        self.assertEqual(0, code, report)
        self.assertEqual((1, 1, 0), (report["registered"], report["total"], report["deferred"]))
        self.assertEqual(1000, len(self.rpc.repos))
        self.assertEqual(str(self.root), self.rpc.repos[-1]["path"])

    def test_sync_and_periodic_discovery_prune_missing_ungrouped_git_checkouts(self):
        self.start_server()
        self.assertEqual(0, self.run_cli("sync")[0])
        for command in ("sync", "discover"):
            with self.subTest(command=command):
                path = self.root / ".build/cleanup-trial" / command
                init_git(path)
                repo = self.rpc.call("repo.add", {"path": str(path), "kind": "git"})["repo"]
                shutil.rmtree(path)
                code, report = self.run_cli(command)
                self.assertEqual(0, code, report)
                self.assertNotIn(repo["id"], [record["id"] for record in self.rpc.repos])
                self.assertEqual(0, self.run_cli(command)[0])

    def test_fifo_project_metadata_fails_promptly_without_hanging_discovery(self):
        metadata = self.root.parent / ".subyard-meta.json"
        metadata.unlink()
        os.mkfifo(metadata)
        code, report = self.run_cli("status")
        self.assertNotEqual(0, code)
        self.assertTrue(report["errors"])

    def test_fifo_runtime_metadata_fails_promptly_without_hanging_transport(self):
        metadata = self.state / "config/orca/orca-runtime.json"
        metadata.parent.mkdir(parents=True)
        os.mkfifo(metadata)
        code, report = self.run_cli("status")
        self.assertNotEqual(0, code)
        self.assertTrue(report["errors"])

    def test_concurrent_cli_processes_share_durable_group_identity(self):
        self.start_server()
        command = [sys.executable, "-B", str(COMPONENT / "main.py"), "sync",
                   "--workspaces", str(self.workspaces), "--state", str(self.state)]
        children = [subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                    for _ in range(2)]
        for child in children:
            stdout, stderr = child.communicate(timeout=5)
            self.assertEqual("", stderr)
            self.assertEqual(0, child.returncode, stdout)
            self.assertTrue(json.loads(stdout)["ready"])
        self.assertEqual((1, 1), (len(self.rpc.groups), len(self.rpc.repos)))

    def test_nonregular_lock_file_is_rejected_without_blocking(self):
        self.state.mkdir()
        os.mkfifo(self.state / "subyard-registration.lock")
        code, report = self.run_cli("status")
        self.assertNotEqual(0, code)
        self.assertTrue(report["errors"])
