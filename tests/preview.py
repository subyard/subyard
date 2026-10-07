#!/usr/bin/env python3
"""Host-free HTTP and process contracts for the foreground preview helper."""

import http.client
import json
import os
from pathlib import Path
import runpy
import selectors
import signal
import socket
import subprocess
import sys
import tempfile
import unittest


HELPER = Path(__file__).resolve().parents[1] / "config/preview/subyard-preview"
ADDRESS = ("127.0.0.1", 8765)
PREVIEW = runpy.run_path(str(HELPER))
LAUNCH = (
    "import runpy, sys; helper = runpy.run_path(sys.argv.pop(1)); "
    "helper['main'].__globals__['ENDPOINT_FILE'] = sys.argv.pop(1); "
    "sys.exit(helper['main']())"
)


class PreviewTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="subyard-preview-test-", dir="/tmp")
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.endpoint = self.base / "preview.json"
        self.checkout = self.base / "checkout"
        self.checkout.mkdir(mode=0o700)
        subprocess.run(["git", "init", "-q", str(self.checkout)], check=True)
        self.site = self.checkout / "site"
        self.site.mkdir(mode=0o700)
        self.write(self.site / "index.html", b"first preview")
        self.nested = self.checkout / "nested"
        self.nested.mkdir(mode=0o700)

    def write(self, path, content, mode=0o600):
        path.write_bytes(content)
        path.chmod(mode)

    def launch(self, *args, cwd=None):
        process = subprocess.Popen(
            [sys.executable, "-c", LAUNCH, str(HELPER), str(self.endpoint), *args],
            cwd=cwd or self.nested,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
        self.addCleanup(self.stop, process)
        return process

    def stop(self, process):
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        process.stdout.close()
        process.stderr.close()

    def start(self, directory="site", cwd=None, url="http://127.0.0.1:8765/"):
        process = self.launch(directory, cwd=cwd)
        with selectors.DefaultSelector() as selector:
            selector.register(process.stdout, selectors.EVENT_READ)
            self.assertTrue(selector.select(timeout=5), "preview did not report readiness")
        line = process.stdout.readline()
        self.assertEqual(line, ("Preview: " + url + "\n").encode())
        self.assertIsNone(process.poll())
        return process

    def request(self, path="/", method="GET"):
        connection = http.client.HTTPConnection(*ADDRESS, timeout=3)
        try:
            connection.request(method, path)
            response = connection.getresponse()
            return response.status, dict(response.getheaders()), response.read()
        finally:
            connection.close()

    def denied(self, path):
        status, headers, body = self.request(path)
        self.assertEqual(status, 404, path)
        self.assertEqual(headers["Cache-Control"], "no-store")
        self.assertEqual(body, b"Not found\n")
        self.assertNotIn(str(self.base).encode(), body)

    def reject(self, directory, cwd=None):
        process = self.launch(directory, cwd=cwd)
        stdout, stderr = process.communicate(timeout=5)
        self.assertNotEqual(process.returncode, 0)
        self.assertEqual(stdout, b"")
        self.assertTrue(stderr.startswith(b"subyard-preview: "))
        self.assertNotIn(str(self.base).encode(), stderr)
        return stderr

    def test_http_live_edits_and_lifecycle(self):
        process = self.start()
        # A wildcard listener would also reserve this other loopback address.
        with socket.socket() as other_address:
            other_address.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            other_address.bind(("127.0.0.2", ADDRESS[1]))
            with self.assertRaises(OSError):
                socket.create_connection(("127.0.0.2", ADDRESS[1]), timeout=1)
        status, headers, body = self.request()
        self.assertEqual((status, body), (200, b"first preview"))
        self.assertEqual(headers["Cache-Control"], "no-store")
        self.assertEqual(headers["Content-Type"], "text/html")
        self.assertEqual(self.request("/?reload=1")[2], b"first preview")
        self.write(self.site / "index.html", b"edited preview")
        self.assertEqual(self.request()[2], b"edited preview")
        status, headers, body = self.request(method="HEAD")
        self.assertEqual((status, body), (200, b""))
        self.assertEqual(headers["Content-Length"], str(len(b"edited preview")))
        self.assertEqual(headers["Cache-Control"], "no-store")
        self.stop(process)
        with self.assertRaises(OSError):
            socket.create_connection(ADDRESS, timeout=1)
        # The fixed address is reusable as soon as the foreground process exits.
        restarted = self.start()
        restarted.send_signal(signal.SIGINT)
        self.assertEqual(restarted.wait(timeout=5), 0)
        self.assertEqual(restarted.stderr.read(), b"")

    def test_requests_cannot_escape_or_list(self):
        outside = self.base / "outside.html"
        self.write(outside, b"outside secret")
        (self.site / "listing").mkdir(mode=0o700)
        self.write(self.site / "listing/item.txt", b"must not list")
        (self.site / "linked-file").symlink_to(outside)
        (self.site / "linked-dir").symlink_to(self.base, target_is_directory=True)
        (self.site / "internal-link").symlink_to("index.html")
        os.mkfifo(self.site / "fifo", 0o600)
        self.write(self.site / "unreadable", b"private", 0o000)
        process = self.start()
        for path in (
            "/missing", "/listing/", "/../outside.html", "/%2e%2e/outside.html",
            "/%2e%2e%2foutside.html", "/linked-file", "/linked-dir/outside.html",
            "/internal-link", "/fifo", "/unreadable", "/.git/config", "/%00",
        ):
            with self.subTest(path=path):
                self.denied(path)
        status, headers, body = self.request("/missing", "HEAD")
        self.assertEqual((status, body), (404, b""))
        self.assertEqual(headers["Cache-Control"], "no-store")
        status, headers, body = self.request("/outside.html", "POST")
        self.assertEqual((status, body), (501, b"Request failed\n"))
        self.assertEqual(headers["Cache-Control"], "no-store")
        self.assertEqual(self.request()[0], 200, "special files must not block the server")
        process.terminate()
        stdout, stderr = process.communicate(timeout=5)
        self.assertEqual((stdout, stderr), (b"", b""))

    def test_runtime_replacements_and_directory_indexes(self):
        self.write(self.base / "outside.html", b"outside secret")
        child = self.site / "child"
        child.mkdir(mode=0o700)
        self.write(child / "index.html", b"child page")
        self.start()
        self.assertEqual(self.request("/child/")[2], b"child page")
        (child / "index.html").unlink()
        (child / "index.html").symlink_to(self.site / "index.html")
        self.denied("/child/")
        self.write(self.site / "asset.txt", b"original")
        self.assertEqual(self.request("/asset.txt")[2], b"original")
        (self.site / "asset.txt").unlink()
        os.mkfifo(self.site / "asset.txt", 0o600)
        self.denied("/asset.txt")
        child.rename(self.site / "old-child")
        child.symlink_to(self.base, target_is_directory=True)
        self.denied("/child/outside.html")

    def test_cli_checkout_and_root_validation(self):
        outside = self.base / "outside"
        outside.mkdir(mode=0o700)
        (self.checkout / "escape").symlink_to(outside, target_is_directory=True)
        (self.checkout / "internal-link").symlink_to("site", target_is_directory=True)
        os.mkfifo(self.checkout / "fifo", 0o600)
        (self.checkout / "unreadable").mkdir(mode=0o700)
        (self.checkout / "unreadable").chmod(0o000)
        for directory in (
            str(self.site), "../outside", "site/../site", "escape", "internal-link",
            "missing", "site/index.html", "fifo", "unreadable", ".git", "",
        ):
            with self.subTest(directory=directory):
                self.reject(directory)
        self.reject("site", cwd=outside)
        self.reject("site", cwd=self.checkout / ".git")
        bare = self.base / "bare"
        subprocess.run(["git", "init", "--bare", "-q", str(bare)], check=True)
        self.reject(".", cwd=bare)
        process = self.launch()
        self.assertNotEqual(process.wait(timeout=5), 0)

    def test_workspace_root_blocks_git_metadata(self):
        self.write(self.checkout / "index.html", b"workspace page")
        self.start(".")
        self.assertEqual(self.request()[2], b"workspace page")
        self.denied("/.git/config")

    def test_busy_port_has_no_readiness_or_remap(self):
        with socket.socket() as listener:
            listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            listener.bind(ADDRESS)
            listener.listen()
            self.assertIn(b"127.0.0.1:8765 is already in use", self.reject("site"))
            self.write(self.endpoint, b'{"version":1,"host":"100.64.1.20","port":32222}', 0o644)
            self.assertIn(b"127.0.0.1:8765 is already in use", self.reject("site"))

    def test_installed_endpoint_advertises_owner_but_serves_guest_loopback(self):
        endpoint = {"version": 1, "host": "100.64.1.20", "port": 32222}
        self.write(self.endpoint, json.dumps(endpoint).encode(), 0o644)
        process = self.start(url="http://100.64.1.20:32222/")
        self.assertEqual(self.request()[2], b"first preview")
        with socket.socket() as other_address:
            other_address.bind(("127.0.0.2", ADDRESS[1]))
        self.stop(process)
        self.write(self.endpoint, b'{"version":1,"host":"127.0.0.1","port":8765}', 0o644)
        self.start()
        self.assertEqual(self.request()[2], b"first preview")

    def test_endpoint_schema_and_address_validation(self):
        for host, port in (("127.0.0.1", 8765), ("100.64.0.0", 1024), ("100.127.255.255", 65535)):
            payload = json.dumps({"version": 1, "host": host, "port": port})
            self.assertEqual(PREVIEW["parse_endpoint"](payload), "http://%s:%d/" % (host, port))
        valid = {"version": 1, "host": "100.64.1.20", "port": 32222}
        invalid = [
            {}, [], None, {**valid, "extra": 1}, {**valid, "version": 2},
            {**valid, "version": True}, {**valid, "version": 1.0},
            {**valid, "port": True}, {**valid, "port": "32222"}, {**valid, "port": 32222.0},
            {**valid, "port": 1023}, {**valid, "port": 65536},
            {**valid, "host": "127.0.0.1"}, {**valid, "host": "127.0.0.2"},
            {**valid, "host": "100.63.255.255"}, {**valid, "host": "100.128.0.0"},
            {**valid, "host": "100.064.1.20"}, {**valid, "host": "::1"},
            {**valid, "host": "owner.example"}, {**valid, "host": 100},
        ]
        for endpoint in invalid:
            with self.subTest(endpoint=endpoint), self.assertRaises(ValueError):
                PREVIEW["parse_endpoint"](json.dumps(endpoint))
        for payload in ("not JSON", '{"version":1,"host":"127.0.0.1","port":8765,"port":8765}'):
            with self.subTest(payload=payload), self.assertRaises(ValueError):
                PREVIEW["parse_endpoint"](payload)

    def test_invalid_endpoint_metadata_fails_before_listening(self):
        valid = b'{"version":1,"host":"127.0.0.1","port":8765}'
        for payload, mode in ((b"invalid JSON", 0o644), (valid + b" " * 1024, 0o644), (valid, 0o664), (valid, 0o646)):
            with self.subTest(payload=payload[:20], mode=mode):
                self.write(self.endpoint, payload, mode)
                self.assertEqual(self.reject("site"), b"subyard-preview: invalid preview endpoint metadata\n")
        self.endpoint.unlink()
        for kind in ("symlink", "fifo", "directory"):
            with self.subTest(kind=kind):
                if kind == "symlink":
                    self.endpoint.symlink_to(self.base / "missing")
                elif kind == "fifo":
                    os.mkfifo(self.endpoint, 0o600)
                else:
                    self.endpoint.mkdir(mode=0o700)
                self.assertEqual(self.reject("site"), b"subyard-preview: invalid preview endpoint metadata\n")
                if kind == "directory":
                    self.endpoint.rmdir()
                else:
                    self.endpoint.unlink()
        with socket.socket() as listener:
            listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            listener.bind(ADDRESS)


if __name__ == "__main__":
    unittest.main()
