import json
from pathlib import Path
import socket
import tempfile
import threading
import time
import unittest

import support  # Adds the standalone component directory to the import path.


class TransportTests(unittest.TestCase):
    def setUp(self):
        from transport import RpcError, RuntimeRPC
        self.error = RpcError
        self.client = RuntimeRPC
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.metadata = Path(self.tmp.name) / "metadata.json"
        self.endpoint = str(Path(self.tmp.name) / "socket")
        self.write_metadata("runtime-1", "fixture-token-one")

    def write_metadata(self, runtime, token):
        self.metadata.write_text(json.dumps({"runtimeId": runtime, "authToken": token,
            "transports": [{"kind": "unix", "endpoint": self.endpoint}], "pid": 1, "startedAt": 1}))

    def server(self, respond, calls=1):
        server = socket.socket(socket.AF_UNIX)
        server.bind(self.endpoint)
        server.listen()
        server.settimeout(2)
        failures = []

        def serve():
            try:
                for index in range(calls):
                    connection, _ = server.accept()
                    with connection:
                        connection.settimeout(2)
                        with connection.makefile("rb") as reader:
                            request = json.loads(reader.readline())
                        answer = respond(request, index)
                        if answer is not None:
                            connection.sendall(answer)
            except Exception as exc:
                failures.append(exc)
            finally:
                server.close()

        thread = threading.Thread(target=serve)
        thread.start()

        def finish():
            thread.join(3)
            self.assertFalse(thread.is_alive())
            if failures:
                raise failures[0]

        self.addCleanup(finish)

    def frame(self, request, **changes):
        result = {"id": request["id"], "ok": True, "result": {"repos": []},
                  "_meta": {"runtimeId": "runtime-1"}}
        result.update(changes)
        return json.dumps(result).encode() + b"\n"

    def test_wire_params_omitted_for_list_and_metadata_rotation_on_next_call(self):
        def answer(request, index):
            self.assertNotIn("params", request)
            self.assertEqual("repo.list", request["method"])
            self.assertEqual(["fixture-token-one", "fixture-token-two"][index], request["authToken"])
            return b'{"_keepalive":true}\n' + self.frame(request, _meta={"runtimeId": "runtime-" + str(index + 1)})
        self.server(answer, 2)
        client = self.client(self.metadata, timeout=1)
        self.assertEqual({"repos": []}, client.call("repo.list"))
        self.write_metadata("runtime-2", "fixture-token-two")
        self.assertEqual({"repos": []}, client.call("repo.list"))

    def test_mismatched_id_is_unknown_write_and_not_retried(self):
        self.server(lambda request, _: self.frame(request, id="wrong"))
        with self.assertRaises(self.error) as caught:
            self.client(self.metadata).call("repo.add", {"path": "/fixture", "kind": "folder"})
        self.assertTrue(caught.exception.unknown)
        self.assertIn("id", str(caught.exception))

    def test_runtime_mismatch_is_rejected(self):
        self.server(lambda request, _: self.frame(request, _meta={"runtimeId": "different"}))
        with self.assertRaises(self.error):
            self.client(self.metadata).call("repo.list")

    def test_timeout_and_disconnect_after_write_are_unknown(self):
        self.server(lambda request, _: time.sleep(0.15))
        with self.assertRaises(self.error) as caught:
            self.client(self.metadata, timeout=0.04).call("projectGroup.create", {"name": "Fixture"})
        self.assertTrue(caught.exception.unknown)
        self.assertNotIn("fixture-token", str(caught.exception))

    def test_rpc_rejection_is_known_failure_without_server_secret_text(self):
        self.server(lambda request, _: self.frame(request, ok=False,
            error={"code": "permission_denied", "message": "fixture-token-one secret"}))
        with self.assertRaises(self.error) as caught:
            self.client(self.metadata).call("repo.add", {"path": "/fixture"})
        self.assertFalse(caught.exception.unknown)
        self.assertNotIn("fixture-token-one", str(caught.exception))

    def test_bad_metadata_has_sanitized_failure(self):
        self.metadata.write_text('{"authToken":"fixture-token-one", invalid')
        with self.assertRaises(self.error) as caught:
            self.client(self.metadata).call("repo.list")
        self.assertFalse(caught.exception.unknown)
        self.assertNotIn("fixture-token-one", str(caught.exception))

    def test_malformed_frame_and_missing_runtime_are_rejected(self):
        self.server(lambda request, _: self.frame(request, _meta={}))
        with self.assertRaises(self.error):
            self.client(self.metadata).call("repo.list")

    def test_before_send_observes_actual_bootstrap_runtime_and_precedes_wire_write(self):
        captured = []

        def answer(request, _):
            self.assertEqual(["runtime-1"], captured)
            self.assertEqual({"name": "Fixture"}, request["params"])
            return self.frame(request)
        self.server(answer)
        self.client(self.metadata).call("projectGroup.create", {"name": "Fixture"}, before_send=captured.append)
