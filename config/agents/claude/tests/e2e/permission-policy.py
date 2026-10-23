#!/usr/bin/env python3
"""Native managed-drop-in checks on a disposable VM; no external model or Git remote."""
import asyncio
import copy
import dataclasses
import importlib.metadata
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import claude_agent_sdk
from claude_agent_sdk import (ClaudeAgentOptions, ClaudeSDKClient,
                              PermissionResultAllow, PermissionResultDeny, PermissionUpdate)
from claude_agent_sdk.types import PermissionRuleValue

COMMANDS = ["./ordinary.sh", "./private-denied.sh", "./project-denied.sh", "git commit --allow-empty -m parity-one",
            "git commit --allow-empty -m parity-two",
            "git commit --amend --allow-empty -m parity-amended", "git push"]
STATE = {}


def emit(kind, **fields):
    line = json.dumps(dict(kind=kind, **fields), default=str)
    STATE["log"].write(line + "\n")
    STATE["log"].flush()


class Model(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        if "count_tokens" in self.path:
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b'{"input_tokens":100}')
            return
        main = any(t.get("name") == "Bash" for t in body.get("tools", []))
        index = STATE["step"]
        if main:
            STATE["step"] += 1
            emit("model_request", index=index, messages=body.get("messages"))
        tool = main and index < len(COMMANDS)
        block = ({"type": "tool_use", "id": f"tool_{index}", "name": "Bash",
                  "input": {"command": COMMANDS[index], "description": "Permission parity probe"}}
                 if tool else {"type": "text", "text": "Probe complete."})
        usage = {"input_tokens": 100, "output_tokens": 10}
        msg = dict(id=f"msg_{index}", type="message", role="assistant",
                   model=body.get("model", "claude-sonnet-4-6"), content=[block],
                   stop_reason="tool_use" if tool else "end_turn", stop_sequence=None,
                   usage=usage)
        self.send_response(200)
        if not body.get("stream"):
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps(msg).encode())
            return
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        events = [("message_start", {"message": dict(msg, content=[], stop_reason=None)}),
                  ("content_block_start", {"index": 0, "content_block":
                   dict(block, input={}) if tool else dict(block, text="")}),
                  ("content_block_delta", {"index": 0, "delta":
                   {"type": "input_json_delta", "partial_json": json.dumps(block["input"])}
                   if tool else {"type": "text_delta", "text": block["text"]}}),
                  ("content_block_stop", {"index": 0}),
                  ("message_delta", {"delta": {"stop_reason": msg["stop_reason"],
                    "stop_sequence": None}, "usage": {"output_tokens": 10}}),
                  ("message_stop", {})]
        for name, data in events:
            self.wfile.write((f"event: {name}\ndata: " +
                             json.dumps(dict(type=name, **data)) + "\n\n").encode())
        self.wfile.flush()


def git(repo, *args):
    return subprocess.run(["git", "-C", str(repo), *args], check=True,
                          env=dict(os.environ, GIT_CONFIG_GLOBAL="/dev/null",
                                   GIT_CONFIG_NOSYSTEM="1"),
                          text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE).stdout.strip()


async def run_case(source, mode, decision, base, server):
    host = os.environ.get("PARITY_HOST_PROBE") == "1"
    case = f"{'host-' if host else ''}{mode}-{decision}"
    root = Path(tempfile.mkdtemp(prefix=f"claude-parity-{case}-", dir="/tmp"))
    repo, home = root / "repo", root / "home"
    repo.mkdir()
    home.mkdir()
    git(repo, "init")
    git(repo, "config", "user.name", "Permission Fixture")
    git(repo, "config", "user.email", "fixture@example.invalid")
    git(repo, "commit", "--allow-empty", "-m", "seed")
    (repo / "ordinary.sh").write_text("#!/bin/sh\nprintf ordinary > ordinary.marker\n")
    (repo / "ordinary.sh").chmod(0o755)
    for name in ("private", "project"):
        script = repo / f"{name}-denied.sh"
        script.write_text(f"#!/bin/sh\ntouch {name}.marker\n")
        script.chmod(0o755)
    user = home / "claude"
    user.mkdir()
    (user / "settings.json").write_text(json.dumps({"permissions": {
        "allow": ["Bash"], "deny": ["Bash(./private-denied.sh)"]}}))
    project = repo / ".claude"
    project.mkdir()
    (project / "settings.json").write_text(json.dumps({"permissions": {
        "allow": ["Bash"], "ask": [], "deny": ["Bash(./project-denied.sh)"]}}))
    (project / "settings.local.json").write_text(json.dumps({"permissions": {
        "allow": ["Bash(git commit:*)", "Bash(git push:*)"], "ask": []}}))
    settings = copy.deepcopy(source)
    settings["permissions"]["ask"] = []
    settings["permissions"]["defaultMode"] = mode
    if host:
        permissions = settings["permissions"]
        permissions["allow"] = [r for r in permissions.get("allow", []) if r != "Bash"]
        permissions["ask"] = [*permissions.get("ask", []), "Bash"]
    settings["autoMemoryDirectory"] = str(home / "memory")
    asks, native = [], []
    with (base / f"{case}.jsonl").open("w") as log:
        STATE.update(step=0, log=log)

        async def permission(name, inputs, context):
            asks.append(inputs.get("command"))
            emit("permission_request", tool=name, inputs=inputs, decision=decision,
                 context=dataclasses.asdict(context))
            if decision == "allow":
                return PermissionResultAllow(updated_input=inputs, updated_permissions=[
                    PermissionUpdate(type="addRules", behavior="allow", destination="session",
                        rules=[PermissionRuleValue(tool_name="Bash", rule_content="git commit:*"),
                               PermissionRuleValue(tool_name="Bash", rule_content="git push:*")])])
            return PermissionResultDeny(message="Fixture declined this operation")

        env = {"HOME": str(home), "CLAUDE_CONFIG_DIR": str(home / "claude"),
               "ANTHROPIC_API_KEY": "synthetic-local-only",
               "ANTHROPIC_BASE_URL": f"http://127.0.0.1:{server.server_port}",
               "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
               "DISABLE_AUTOUPDATER": "1", "IS_SANDBOX": "1",
               "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1"}
        options = ClaudeAgentOptions(cwd=str(repo), settings=json.dumps(settings),
                  setting_sources=["user", "project", "local"], tools=["Bash"], can_use_tool=permission,
                  env=env, model="claude-sonnet-4-6", max_turns=8,
                  stderr=lambda line: emit("stderr", text=line))
        if os.environ.get("PARITY_CLAUDE_CLI"):
            options.cli_path = os.environ["PARITY_CLAUDE_CLI"]
        async with ClaudeSDKClient(options=options) as client:
            await client.query("Run the local permission fixture sequence.")
            async for message in client.receive_response():
                payload = dataclasses.asdict(message)
                native.append(payload)
                emit("native", message_type=type(message).__name__, payload=payload)
        result = dict(case=case, fixture=str(root), asks=asks,
                      ordinary=(repo / "ordinary.marker").exists(),
                      commits=int(git(repo, "rev-list", "--count", "HEAD")),
                      subject=git(repo, "log", "-1", "--format=%s"),
                      model_steps=STATE["step"], messages=len(native))
        emit("summary", **result)
        print(json.dumps(result), flush=True)
        assert asks == ([COMMANDS[0], *COMMANDS[3:]] if host else COMMANDS[3:]), result
        assert result["ordinary"] == (not (host and decision == "deny")), result
        assert result["commits"] == (1 if decision == "deny" else 3), result
        assert result["subject"] == ("seed" if decision == "deny" else "parity-amended"), result
        assert not (repo / "private.marker").exists(), "user deny lost"
        assert not (repo / "project.marker").exists(), "project deny lost"
        assert result["model_steps"] == len(COMMANDS) + 1, result


async def main():
    assert os.environ.get("SUBYARD_E2E_VM") == "1", "requires disposable VM"
    assert os.geteuid() != 0, "run native CLI as the developer"
    policy = Path("/etc/claude-code/managed-settings.d/50-subyard-git-approval.json")
    assert policy.is_file(), "managed drop-in missing"
    source = json.loads(Path(sys.argv[1]).read_text())
    base = Path(sys.argv[2]).resolve()
    base.mkdir(parents=True, exist_ok=True)
    # Never pass ambient account credentials/provider flags into the native child.
    for key in list(os.environ):
        if key.startswith(("ANTHROPIC_", "CLAUDE_", "AWS_", "GOOGLE_")):
            del os.environ[key]
    cli = Path(os.environ.get("PARITY_CLAUDE_CLI") or
               Path(claude_agent_sdk.__file__).parent / "_bundled" / "claude")
    version = subprocess.run([str(cli), "--version"], check=True, text=True,
                             stdout=subprocess.PIPE).stdout.strip()
    print(json.dumps({"sdk": importlib.metadata.version("claude-agent-sdk"),
                      "cli": version, "cli_path": str(cli),
                      "root": os.geteuid() == 0, "sandbox_declaration": True}), flush=True)
    server = ThreadingHTTPServer(("127.0.0.1", 0), Model)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        modes = (["default"] if os.environ.get("PARITY_HOST_PROBE") == "1" else
                 [source["permissions"]["defaultMode"], "default"])
        for mode in modes:
            for decision in ["deny", "allow"]:
                await run_case(source, mode, decision, base, server)
    finally:
        server.shutdown()


if __name__ == "__main__":
    asyncio.run(main())
