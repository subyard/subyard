#!/usr/bin/env python3
"""Exercise the pinned Core JSON-RPC protocol through the real SSH proxy."""

import json
import os
from pathlib import Path
import selectors
import subprocess
import sys
import time


class Client:
    def __init__(self, state, endpoint, key):
        self.process = subprocess.Popen(
            [
                "ssh", "-F", "/dev/null", "-T", "-p", endpoint["port"], "-i", str(state / key),
                "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "IdentitiesOnly=yes",
                "-o", "StrictHostKeyChecking=yes", "-o", f"UserKnownHostsFile={endpoint['known_hosts']}",
                f"{endpoint['user']}@{endpoint['host']}", "~/.local/bin/herdr-core connect",
            ],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
        )
        self.selector = selectors.DefaultSelector()
        self.selector.register(self.process.stdout, selectors.EVENT_READ)
        self.buffer = bytearray()
        self.request_id = 0

    def __enter__(self):
        try:
            assert self.rpc("hello", generation=1)["generation"] == 1
        except BaseException:
            self.__exit__(*sys.exc_info())
            raise
        return self

    def __exit__(self, *_exception):
        self.selector.close()
        self.process.stdin.close()
        self.process.stdout.close()
        try:
            self.process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait()

    def rpc(self, method, **params):
        self.request_id += 1
        request = dict(jsonrpc="2.0", id=self.request_id, method=method, params=params)
        self.process.stdin.write(json.dumps(request).encode() + b"\n")
        self.process.stdin.flush()
        deadline = time.monotonic() + 20
        while True:
            while b"\n" not in self.buffer:
                remaining = deadline - time.monotonic()
                if remaining <= 0 or not self.selector.select(remaining):
                    raise RuntimeError(f"timed out waiting for {method}")
                data = os.read(self.process.stdout.fileno(), 65536)
                if not data:
                    raise RuntimeError(f"SSH disconnected waiting for {method}")
                self.buffer.extend(data)
            line, _, self.buffer = self.buffer.partition(b"\n")
            reply = json.loads(line)
            if reply.get("id") != self.request_id:
                continue
            if "error" in reply:
                raise RuntimeError(f"{method}: {reply['error']}")
            return reply["result"]

    def layout(self):
        return self.rpc("doc.get", name="herdr-app")

    def workspaces(self):
        return {space["name"]: space for space in self.layout()["value"]["spaces"]}

    def start(self, space, cwd):
        return self.rpc(
            "terminal.run",
            name="herdr-lifecycle",
            argv=[
                "/bin/sh", "-c",
                'stty -echo; printf "ready:%s\\n" "$$"; while IFS= read -r line; do '
                'printf "ack:%s:%s\\n" "$$" "$line"; done',
            ],
            cwd=cwd,
            labels={"herdr.space": space["id"]},
        )

    def space_terminal(self, name):
        """Find the one live terminal of a named space the way a client does."""
        space = self.workspaces()[name]
        found = [
            terminal for terminal in self.rpc("terminal.list")["terminals"]
            if terminal.get("labels", {}).get("herdr.space") == space["id"] and terminal["state"] == "live"
        ]
        assert len(found) == 1, found
        return found[0]

    def await_output(self, terminal, expected):
        for _ in range(30):
            result = self.rpc("terminal.read", terminal=terminal["address"])
            if expected in result["text"]:
                return
            time.sleep(0.3)
        raise RuntimeError(f"terminal did not produce {expected}")

    def assert_live(self, terminal):
        current = self.rpc("terminal.get", terminal=terminal["address"])
        assert current["state"] == "live" and current["pid"] == terminal["pid"], current


def remember(state, terminal):
    (state / "terminal.json").write_text(json.dumps(terminal))
    (state / "terminal-pid").write_text(str(terminal["pid"]))


def reconnect(state, fixture, discovered):
    with Client(state, fixture, "a") as first:
        layout = first.layout()
        spaces = {space["name"]: space for space in layout["value"]["spaces"]}
        live = spaces["live"]
        assert live["tabs"] == spaces["pristine"]["tabs"] == []
        terminal = first.start(live, live["folder"])
        first.await_output(terminal, f"ready:{terminal['pid']}")
        live["tabs"] = [{"id": "fixture-tab", "terminal": terminal["address"]}]
        first.rpc("doc.set", name="herdr-app", value=layout["value"], version=layout["version"])
        remember(state, terminal)
    # Client B knows only its pairing output and the space name.
    with Client(state, discovered, "b") as second:
        found = second.space_terminal("live")
        assert found["pid"] == terminal["pid"], found
        second.rpc("terminal.send", terminal=found["address"], text="from-client-b", force=True)
        second.await_output(found, f"ack:{terminal['pid']}:from-client-b")
    with Client(state, fixture, "a") as again:
        again.assert_live(terminal)
        assert again.workspaces()["live"] == live
    print(f"herdr-lifecycle: A/B/A reconnect preserved tab and terminal PID {terminal['pid']}")


def main():
    operation, path, host, port = sys.argv[1:]
    state = Path(path)
    fixture = dict(user="dev", host=host, port=port, known_hosts=str(state / "known-hosts"))
    if operation == "reconnect":
        reconnect(state, fixture, json.loads((state / "b-endpoint.json").read_text()))
        return
    with Client(state, fixture, "a") as client:
        if operation == "live":
            client.assert_live(json.loads((state / "terminal.json").read_text()))
        elif operation == "removed":
            spaces = client.workspaces()
            assert not [name for name in spaces if name == "pristine" or name.startswith("pristine/")], spaces
            assert spaces["live"]["tabs"], spaces
        elif operation == "spaces":
            for name, space in sorted(client.workspaces().items()):
                print(name, space["folder"])
        elif operation == "start":
            terminal = client.start(client.workspaces()["live"], "/home/dev")
            client.await_output(terminal, f"ready:{terminal['pid']}")
            remember(state, terminal)
        else:
            raise ValueError(f"unknown operation: {operation}")


if __name__ == "__main__":
    main()
