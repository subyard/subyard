#!/usr/bin/env python3
"""Probe native skill loaders without provider credentials or model inference."""
import argparse
import http.server
import json
import os
from pathlib import Path
import pwd
import selectors
import signal
import subprocess
import threading
import time


def environment():
    user = pwd.getpwuid(os.getuid())
    home = user.pw_dir
    return dict(HOME=home, USER=user.pw_name, LOGNAME=user.pw_name,
                PATH=f"{home}/.local/bin:/usr/local/bin:/usr/bin:/bin",
                LANG="C.UTF-8", XDG_CONFIG_HOME=f"{home}/.config",
                XDG_DATA_HOME=f"{home}/.local/share", XDG_CACHE_HOME=f"{home}/.cache",
                XDG_STATE_HOME=f"{home}/.local/state")


def command(arguments, cwd, env, timeout=60):
    process = subprocess.Popen(arguments, cwd=cwd, env=env, stdout=subprocess.PIPE,
                               stderr=subprocess.PIPE, text=True, start_new_session=True)
    try:
        output, _ = process.communicate(timeout=timeout)
        if process.returncode:
            raise ValueError(f"{arguments[0]} native probe exited {process.returncode}")
        if len(output) > 8 * 1024 * 1024:
            raise ValueError("native probe output exceeds bound")
        return output
    finally:
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.wait()


def check_metadata(skills, expected, client):
    by_name = {entry["name"]: entry for entry in skills}
    for name, description in expected.items():
        entry = by_name.get(name)
        if (sum(item["name"] == name for item in skills) != 1 or not entry
                or entry.get("description") != description or entry.get("enabled") is False):
            raise ValueError(f"{client}: missing, disabled or incorrect metadata for {name}")
    print(f"native_skill_loader client={client} names={','.join(sorted(expected))} metadata=PASS", flush=True)


def codex(cwd, env, expected):
    process = subprocess.Popen(["codex", "app-server"], cwd=cwd, env=env,
                               stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                               stderr=subprocess.DEVNULL, start_new_session=True)
    selector = selectors.DefaultSelector()
    selector.register(process.stdout, selectors.EVENT_READ)
    pending = b""
    total = 0
    deadline = time.monotonic() + 60

    def rpc(identifier, method, params):
        nonlocal pending, total
        process.stdin.write((json.dumps(dict(id=identifier, method=method, params=params)) + "\n").encode())
        process.stdin.flush()
        while time.monotonic() < deadline:
            while b"\n" in pending:
                line, pending = pending.split(b"\n", 1)
                response = json.loads(line)
                if response.get("id") == identifier:
                    if "error" in response:
                        raise ValueError(f"Codex {method} rejected")
                    return response["result"]
            if not selector.select(max(0, deadline - time.monotonic())):
                break
            chunk = os.read(process.stdout.fileno(), 65536)
            if not chunk:
                break
            total += len(chunk)
            if total > 8 * 1024 * 1024:
                raise ValueError("Codex output exceeds bound")
            pending += chunk
        raise ValueError(f"Codex {method} did not complete")

    try:
        rpc(0, "initialize", dict(clientInfo=dict(name="subyard-skill-probe", title="Subyard skill probe", version="1")))
        process.stdin.write(b'{"method":"initialized","params":{}}\n')
        process.stdin.flush()
        result = rpc(1, "skills/list", dict(cwds=[str(cwd)], forceReload=True))
        entries = result["data"]
        if len(entries) != 1 or entries[0].get("errors"):
            raise ValueError("Codex native loader returned errors")
        check_metadata(entries[0]["skills"], expected, "codex")
    finally:
        selector.close()
        try:
            process.stdin.close()
        except BrokenPipeError:
            pass
        try:
            os.killpg(process.pid, signal.SIGTERM)
            process.wait(timeout=5)
        except ProcessLookupError:
            process.wait()
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait()


def strings(value):
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for item in value.values():
            yield from strings(item)
    elif isinstance(value, list):
        for item in value:
            yield from strings(item)


def claude(cwd, env, expected):
    requests = []

    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_POST(self):
            size = int(self.headers.get("Content-Length", "0"))
            if not 0 <= size <= 8 * 1024 * 1024:
                self.send_error(413)
                return
            request = json.loads(self.rfile.read(size))
            if self.path.split("?", 1)[0].endswith("/count_tokens"):
                body = json.dumps(dict(input_tokens=1)).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                return
            if not self.path.split("?", 1)[0].endswith("/messages") or len(requests) >= 8:
                self.send_error(400)
                return
            requests.append(request)
            message = dict(id="msg_subyard_probe", type="message", role="assistant", model=request["model"],
                           content=[], stop_reason=None, stop_sequence=None,
                           usage=dict(input_tokens=1, output_tokens=0))
            events = [("message_start", dict(message=message)),
                      ("content_block_start", dict(index=0, content_block=dict(type="text", text=""))),
                      ("content_block_delta", dict(index=0, delta=dict(type="text_delta", text="probe"))),
                      ("content_block_stop", dict(index=0)),
                      ("message_delta", dict(delta=dict(stop_reason="end_turn", stop_sequence=None), usage=dict(output_tokens=1))),
                      ("message_stop", {})]
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            for kind, data in events:
                self.wfile.write((f"event: {kind}\ndata: " + json.dumps(dict(type=kind, **data)) + "\n\n").encode())
            self.wfile.flush()

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        local_env = dict(env, ANTHROPIC_BASE_URL=f"http://127.0.0.1:{server.server_port}",
                         ANTHROPIC_API_KEY="probe-only", CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC="1",
                         DISABLE_AUTOUPDATER="1")
        output = command(["claude", "-p", "Return probe.", "--output-format", "stream-json", "--verbose",
                          "--max-turns", "1"], cwd, local_env)
        events = [json.loads(line) for line in output.splitlines() if line.strip()]
        init = next(event for event in events if event.get("type") == "system" and event.get("subtype") == "init")
        if any(init.get("skills", []).count(name) != 1 for name in expected):
            raise ValueError("Claude native startup omitted expected skills")
        # This is the actual native loader's outgoing metadata, not a model answer.
        loaded = "\n".join(strings(requests))
        for name, description in expected.items():
            if name not in loaded or description not in loaded:
                raise ValueError(f"Claude outgoing skill metadata missing for {name}")
        print(f"native_skill_loader client=claude names={','.join(sorted(expected))} metadata=PASS provider=loopback-mock", flush=True)
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


def opencode(cwd, env, expected):
    output = command(["opencode", "debug", "skill"], cwd, env)
    check_metadata(json.loads(output), expected, "opencode")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cwd", type=Path, required=True)
    parser.add_argument("--skill", action="append", required=True, help="name=description from the packaged source")
    options = parser.parse_args()
    expected = dict(item.split("=", 1) for item in options.skill)
    cwd = options.cwd.resolve(strict=True)
    if len(expected) != len(options.skill) or not (cwd / ".git").is_dir():
        raise ValueError("unique skills and a disposable Git project are required")
    env = environment()
    for client, probe in (("codex", codex), ("claude", claude), ("opencode", opencode)):
        version = command([client, "--version"], cwd, env).strip()
        if len(version) > 200 or "\n" in version:
            raise ValueError("unexpected client version")
        print(f"native_skill_client client={client} version={version}", flush=True)
        probe(cwd, env, expected)
    print("native_skill_discovery=PASS model_comprehension=not-run", flush=True)


if __name__ == "__main__":
    main()
