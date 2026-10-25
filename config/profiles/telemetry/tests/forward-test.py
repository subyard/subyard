#!/usr/bin/env python3
import http.client
import contextlib
from pathlib import Path
import socket
import sys
import threading
import time
import unittest
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import forward


class ForwardTest(unittest.TestCase):
    def test_idle_connections_are_bounded_before_thread_and_header_read(self):
        with forward.Server(('127.0.0.1', 0), Path('/does-not-exist')) as server:
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            connections = []
            try:
                for _ in range(16):
                    connections.append(socket.create_connection(server.server_address, timeout=1))
                deadline = time.monotonic()+2
                while server.slots._value and time.monotonic() < deadline:
                    time.sleep(0.01)
                self.assertEqual(server.slots._value, 0)
                with socket.create_connection(server.server_address, timeout=1) as excess:
                    self.assertEqual(excess.recv(1), b'')
                self.assertEqual(server.slots._value, 0)
            finally:
                for connection in connections:
                    connection.close()
                server.shutdown()
                thread.join(timeout=2)

    def test_arbitrary_control_path_is_not_forwarded(self):
        with forward.Server(('127.0.0.1', 0), Path('/does-not-exist')) as server:
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                with contextlib.closing(http.client.HTTPConnection(*server.server_address, timeout=2)) as connection:
                    connection.request('POST', '/v1/records', b'{}')
                    response = connection.getresponse()
                    self.assertEqual(response.status, 400)
                    response.read()
            finally:
                server.shutdown()
                thread.join(timeout=2)


if __name__ == '__main__':
    unittest.main()
