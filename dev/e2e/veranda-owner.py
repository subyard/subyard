#!/usr/bin/env python3
"""Bounded framed RPC checks on the marker-owned fixture prepared by the shell."""
import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import select
import struct
import subprocess
import sys
import threading
import time
import uuid

MAX_FRAME = 1024 * 1024
RPC_TIMEOUT = 300
METHODS = frozenset(("rpc.negotiate", "profile.list", "settings.list", "host.sync.status",
                    "project.list", "session.prepare", "operation.plan", "operation.execute",
                    "operation.discard", "owner.inventory", "context.get", "yard.status",
                    "system.snapshot", "system.ping", "incus.events"))
LIFECYCLE = frozenset(("operation.planned", "operation.confirmed", "operation.started",
                       "operation.finished"))
# Diagnostic only: exact native initReporter lines never establish product state.
STAGE_LINES = {("  [ .. ] " + stage).encode(): stage for stage in
               ("incus", "project", "network", "network-policy", "power-import", "instance",
                "mounts", "provision", "test-vms", "ssh", "git-identity", "profile-services",
                "extras", "power", "keys", "security", "profile-runtimes", "finalize")}


def safe_fault(value):
    fault = value.get("error", {})
    code = fault.get("code") if isinstance(fault, dict) else None
    code = code if isinstance(code, str) and re.fullmatch(r"[a-z][a-z0-9_]{0,63}", code) else "invalid_fault"
    message = fault.get("message", "") if isinstance(fault, dict) else ""
    category = "unclassified"
    if isinstance(message, str) and len(message) <= 8192:
        if "context deadline exceeded" in message:
            category = "deadline"
        elif "signal: killed" in message:
            category = "command_killed"
    return code, category


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


class Owner:
    def __init__(self, argv, *, env=None, timeout=RPC_TIMEOUT):
        self.timeout = timeout
        self.process = subprocess.Popen(argv + ["rpc", "--stdio"], stdin=subprocess.PIPE,
                                        stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env)
        self.sequence = self.revision = 0
        self.events = []
        self.replies = {}
        self.last_stage = "unobserved"
        self.diagnostics = threading.Thread(target=self.drain_diagnostics, daemon=True)
        self.diagnostics.start()

    def diagnostic_line(self, line):
        stage = STAGE_LINES.get(line)
        if stage:
            self.last_stage = stage

    def drain_diagnostics(self):
        # Drain stderr without retaining raw output. Oversized/unrecognized lines are discarded.
        line = bytearray()
        oversized = False
        while True:
            chunk = os.read(self.process.stderr.fileno(), 4096)
            if not chunk:
                return
            for byte in chunk:
                if byte == 10:
                    if not oversized:
                        self.diagnostic_line(bytes(line))
                    line.clear()
                    oversized = False
                elif not oversized:
                    if len(line) < 128:
                        line.append(byte)
                    else:
                        line.clear()
                        oversized = True

    def write(self, value):
        encoded = json.dumps(value, separators=(",", ":")).encode()
        require(len(encoded) <= MAX_FRAME, "request frame exceeded bound")
        self.process.stdin.write(struct.pack(">I", len(encoded)) + encoded)
        self.process.stdin.flush()

    def send(self, method, params=None, operation=None):
        request = uuid.uuid4().hex
        operation = operation or request
        self.write({"version": 1, "type": "request", "id": request,
                    "operationId": operation, "method": method, "params": params or {},
                    "deadline": (datetime.datetime.now(datetime.timezone.utc) +
                                 datetime.timedelta(seconds=self.timeout)).isoformat()})
        return request, operation

    def read_exact(self, length):
        data = bytearray()
        end = time.monotonic() + self.timeout + 5
        while len(data) < length:
            remaining = end - time.monotonic()
            require(remaining > 0 and select.select([self.process.stdout], [], [], remaining)[0],
                    "owner frame timed out")
            part = os.read(self.process.stdout.fileno(), length - len(data))
            require(part, "owner disconnected before a complete frame")
            data.extend(part)
        return bytes(data)

    def read(self):
        length = struct.unpack(">I", self.read_exact(4))[0]
        require(0 < length <= MAX_FRAME, "invalid owner frame length")
        value = json.loads(self.read_exact(length))
        require(value.get("version") == 1, "unexpected owner wire version")
        if value.get("type") == "event":
            require(value.get("sequence", 0) == self.sequence + 1 and
                    value.get("revision", 0) > self.revision, "owner event stream reordered")
            self.sequence, self.revision = value["sequence"], value["revision"]
            self.events.append(value)
        return value

    def await_reply(self, request):
        if request in self.replies:
            return self.replies.pop(request)
        while True:
            value = self.read()
            if value.get("type") == "event":
                continue
            require(value.get("type") == "response" and value.get("id"), "invalid owner response")
            if value["id"] == request:
                return value
            require(value["id"] not in self.replies, "duplicate owner response")
            self.replies[value["id"]] = value

    def call(self, method, params=None, operation=None):
        request, _ = self.send(method, params, operation)
        return self.await_reply(request)

    def result(self, method, params=None, operation=None):
        require(method in METHODS, "fixture method is not allowlisted")
        started = time.monotonic()
        if method == "operation.execute":
            self.last_stage = "unobserved"
        request, _ = self.send(method, params, operation)
        try:
            value = self.await_reply(request)
        except RuntimeError as failure:
            if str(failure) != "owner frame timed out":
                raise
            value = {"id": request, "error": {"code": "frame_timeout"}}
        if "result" not in value or "error" in value:
            code, category = safe_fault(value)
            phase = next((event["event"] for event in reversed(self.events)
                          if event.get("id") == value.get("id") and event.get("event") in LIFECYCLE), "unobserved")
            raise RuntimeError("owner query/action rejected: method=" + method + " code=" + code +
                               " elapsedSeconds=" + str(round(time.monotonic() - started, 1)) +
                               " lifecycle=" + phase + " nativeStage=" + self.last_stage +
                               " category=" + category)
        return value["result"]

    def close(self):
        self.process.stdin.close()
        try:
            require(self.process.wait(timeout=10) == 0, "owner session exited unsuccessfully")
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait(timeout=5)
            raise RuntimeError("owner session failed to close")


def error(value, code):
    require(value.get("error", {}).get("code") == code, "unexpected rejection category: " + code)


def run_bootstrap(owner, engine, yard, profile, root, config):
    created_yard = yard + "-new"
    created_config = root / "config" / "yards" / created_yard / "config.env"
    preset = Path(engine).resolve().parent.parent / "config" / "profiles" / profile / "yard.env"
    require(any(item.get("name") == profile and item.get("hasYardPreset") is True
                for item in owner.result("profile.list").get("profiles", [])), "named preset metadata missing")
    initial_yards = {y["name"] for y in owner.result("owner.inventory")["yards"]}
    require(created_yard not in initial_yards and not created_config.exists(),
            "bootstrap target already registered")
    initial_context = owner.result("context.get")
    initial_power = owner.result("yard.status").get("state")
    initial_config = hashlib.sha256(config.read_bytes()).digest()

    def bootstrap_read_only():
        require(not created_config.exists() and not created_config.is_symlink(),
                "bootstrap preparation published target registration")
        require(not (created_config.parent.parent / (created_yard + ".env")).exists(),
                "bootstrap preparation published flat target registration")
        observed_yards = {y["name"] for y in owner.result("owner.inventory")["yards"]}
        require(created_yard not in observed_yards, "bootstrap preparation registered target in inventory")
        require(observed_yards == initial_yards, "bootstrap preparation changed registered yard scope")
        require(owner.result("context.get") == initial_context,
                "bootstrap preparation changed immutable session context")
        require(owner.result("yard.status").get("state") == initial_power,
                "bootstrap preparation changed existing yard power state")
        require(hashlib.sha256(config.read_bytes()).digest() == initial_config,
                "bootstrap preparation changed existing yard configuration")

    def bootstrap():
        operation = "veranda-bootstrap-" + uuid.uuid4().hex
        exact = owner.result("operation.plan", {"command": "init", "arguments":
                             ["--profile", profile], "targetYard": created_yard,
                             "exact": True, "stepSchema": 1}, operation)
        require(exact.get("stepSchema") == 1 and exact.get("plan", {}).get("steps") and
                exact["plan"].get("confirmation") == "prompt-default-yes",
                "bootstrap did not retain a complete native confirmed plan")
        bootstrap_read_only()
        return operation, exact["digest"]

    operation, digest = bootstrap()
    owner.result("operation.discard", {}, operation)
    bootstrap_read_only()
    print("veranda-owner: bootstrap plan/discard registration and session guards passed", flush=True)
    operation, digest = bootstrap()
    original_preset = preset.read_bytes()
    try:
        preset.write_bytes(original_preset + b"VERANDA_SAMPLE_LIMIT=9\n")
        error(owner.call("operation.execute", {"confirmed": True, "digest": digest}, operation), "plan_stale")
    finally:
        preset.write_bytes(original_preset)
    bootstrap_read_only()
    error(owner.call("operation.execute", {"confirmed": True, "digest": digest}, operation), "plan_not_found")
    print("veranda-owner: bootstrap stale preset and replay guards passed", flush=True)
    operation, digest = bootstrap()
    print("veranda-owner: bootstrap confirmed native execution", flush=True)
    executed = owner.result("operation.execute", {"confirmed": True, "digest": digest}, operation)
    require(executed.get("result", {}).get("status") == "ok" and created_config.is_file() and
            any(y["name"] == created_yard for y in owner.result("owner.inventory")["yards"]) and
            owner.result("context.get").get("yardName") == yard,
            "confirmed bootstrap failed native registration or switched session context")
    error(owner.call("operation.execute", {"confirmed": True, "digest": digest}, operation), "plan_not_found")
    print("ok: native named-yard bootstrap plan/discard, stale preset refusal, confirmed execution and replay guard", flush=True)


def run(engine, yard, profile, state):
    argv = [engine, "-Y", yard]
    root = Path(state)
    config = root / "config" / "yards" / yard / "config.env"
    require(root.name.startswith("subyard-veranda-owner.") and (root / ".marker").is_file(),
            "fixture marker missing")

    def native(*arguments):
        result = subprocess.run(argv + list(arguments), stdout=subprocess.DEVNULL,
                                stderr=subprocess.DEVNULL, timeout=600)
        require(result.returncode == 0, "native fixture operation failed")

    def selected(owner):
        catalog = owner.result("profile.list")
        require(catalog.get("yardName") == yard, "profile query escaped yard")
        matches = [p for p in catalog.get("profiles", []) if p.get("name") == profile]
        require(len(matches) == 1, "synthetic profile missing or duplicated")
        return matches[0]

    def plan(owner, setting, value):
        operation = "veranda-exact-" + uuid.uuid4().hex
        exact = owner.result("operation.plan", {"command": "config", "arguments":
                             ["set", setting, value, "--scope", "yard"],
                             "exact": True, "stepSchema": 1}, operation)
        require(exact.get("schema") == 1 and exact.get("stepSchema") == 1 and
                len(exact.get("digest", "")) == 64 and exact.get("plan", {}).get("steps"),
                "invalid native exact executable plan")
        require(str(root) not in json.dumps(exact), "private fixture path leaked into config plan")
        return operation, exact["digest"]

    owner = Owner(argv)
    try:
        negotiate = owner.result("rpc.negotiate")
        require(all(cap in negotiate.get("capabilities", []) for cap in
                    ("profile-list-v1", "settings-list-v1", "host-sync-status-v1",
                     "session-prepare-v1", "yard-bootstrap-v1", "operation-exact-plan-v1", "operation-steps-v1")),
                "owner lacks required capability")
        if os.environ.get("SUBYARD_E2E_VERANDA_BOOTSTRAP_ONLY") == "1":
            run_bootstrap(owner, engine, yard, profile, root, config)
            return
        before = hashlib.sha256(config.read_bytes()).digest()
        for _ in range(3):
            observed = selected(owner)
            require(observed.get("selected") and observed.get("convergence") == "current",
                    "running native profile check did not converge")
        require(hashlib.sha256(config.read_bytes()).digest() == before, "profile reads changed config")
        settings = owner.result("settings.list")
        limit = next((s for s in settings.get("settings", []) if s.get("name") == "VERANDA_SAMPLE_LIMIT"), {})
        require(limit.get("value") == "8" and limit.get("editable") and
                limit.get("owner") == "profile:" + profile and
                any(p.get("scope") == "yard" and p.get("status") == "effective"
                    for p in limit.get("provenance", [])), "typed setting provenance incorrect")
        require(str(root) not in json.dumps(settings), "settings query leaked owner paths")
        fetch_head = root / "settings-checkout" / ".git" / "FETCH_HEAD"
        fetch_before = fetch_head.read_bytes() if fetch_head.exists() else None
        fetch_mtime = fetch_head.stat().st_mtime_ns if fetch_head.exists() else None
        sync = owner.result("host.sync.status")
        require(sync.get("offline") is True and sync.get("automation") == "manual" and
                sync.get("registration") == "configured" and
                sync.get("git", {}).get("remote") == "<local-source>", "host sync projection incorrect")
        require(str(root) not in json.dumps(sync), "host sync query exposed a local source path")
        require((fetch_head.read_bytes() if fetch_head.exists() else None) == fetch_before and
                (fetch_head.stat().st_mtime_ns if fetch_head.exists() else None) == fetch_mtime,
                "offline sync query fetched")
        projects = owner.result("project.list")
        records = projects.get("projects", []) if isinstance(projects, dict) else projects
        require(len(records) == 1, "native project registration is ambiguous")
        project_id = records[0].get("projectId", "")
        require(project_id, "native project lacks stable identity")
        launch = owner.result("session.prepare", {"kind": "shell", "projectId": project_id})
        require(launch.get("projectId") == project_id and launch.get("ownerArguments") ==
                ["yard", "-Y", yard, "shell", project_id], "shell launch lost native project identity")
        vscode = owner.result("session.prepare", {"kind": "vscode", "projectId": project_id})
        transport = vscode.get("vscode", {})
        require(transport.get("hostKey", "").startswith("ssh-ed25519 ") and
                transport.get("hostKeyFingerprint", "").startswith("SHA256:") and
                transport.get("address") == "127.0.0.1" and
                transport.get("remoteAuthentication") == "already-authorized-desktop-agent-key",
                "VS Code launch lacks pinned native transport")
        native("shell", project_id, "--", "sh", "-c", '[ "$PWD" = "$1" ]',
               "veranda-session", transport.get("folderPath", ""))
        require(owner.result("session.prepare", {"kind": "resources"}).get("ownerArguments") ==
                ["yard", "-Y", yard, "shell", "--", "htop"], "resource session scope incorrect")
        require(owner.result("session.prepare", {"kind": "shell", "scope": "host"}).get("ownerArguments") ==
                ["bash", "-l"], "host shell scope incorrect")
        print("ok: running profiles, typed provenance, native stable project and pinned session descriptors", flush=True)

        operation, digest = plan(owner, "ENVIRONMENT_PROFILES", "")
        require(selected(owner).get("selected"), "exact preparation mutated profile selection")
        executed = owner.result("operation.execute", {"confirmed": True, "digest": digest}, operation)
        require(executed.get("result", {}).get("status") == "ok", "selection execution failed")
        require(not selected(owner).get("selected"), "saved deselection not observed in same session")
        error(owner.call("operation.execute", {"confirmed": True, "digest": digest}, operation), "plan_not_found")
        operation, digest = plan(owner, "ENVIRONMENT_PROFILES", profile)
        owner.result("operation.execute", {"confirmed": True, "digest": digest}, operation)
        require(selected(owner).get("selected"), "saved selection not observed in same session")
        operation, digest = plan(owner, "VERANDA_SAMPLE_LIMIT", "9")
        native("config", "set", "VERANDA_SAMPLE_LIMIT", "10", "--scope", "yard", "--yes")
        error(owner.call("operation.execute", {"confirmed": True, "digest": digest}, operation), "plan_stale")
        error(owner.call("operation.execute", {"confirmed": True, "digest": digest}, operation), "plan_not_found")
        settings = owner.result("settings.list")
        require(next(s for s in settings["settings"] if s["name"] == "VERANDA_SAMPLE_LIMIT")["value"] == "10",
                "stale attempt changed effective native state")
        print("ok: exact selection save/requery, replay refusal and native stale-target protection", flush=True)

        run_bootstrap(owner, engine, yard, profile, root, config)

        snapshot = owner.result("system.snapshot")
        require(snapshot.get("revision") == owner.revision and owner.events, "snapshot lost event revision")
        stream_request, stream_operation = owner.send("incus.events", {"types": ["lifecycle"]})
        owner.result("system.ping")
        time.sleep(0.5)
        native("stop", "--force", "--yes")
        observed = selected(owner)
        require(observed.get("convergence") == "unknown" and observed.get("diagnostic") == "yard-not-running",
                "stopped profile query started yard or reported convergence")
        error(owner.call("session.prepare", {"kind": "shell"}), "session_yard_unavailable")
        cancel_request = uuid.uuid4().hex
        owner.write({"version": 1, "type": "cancel", "id": cancel_request, "operationId": stream_operation})
        require(owner.await_reply(cancel_request).get("result", {}).get("cancelled") == stream_operation,
                "stream cancellation acknowledgement missing")
        error(owner.await_reply(stream_request), "cancelled")
        require(any(e.get("event") == "incus.lifecycle" for e in owner.events), "physical lifecycle event missing")
        status = owner.result("yard.status")
        require(status.get("state", "").lower() == "stopped",
                "read/session checks changed stopped state")
        native("start", "--yes")
        require(selected(owner).get("convergence") == "current", "profile query failed after explicit restart")
        print("ok: ordered snapshot/lifecycle events, explicit stream cancellation and stopped no-autostart", flush=True)
    finally:
        failure_in_flight = sys.exc_info()[0] is not None
        try:
            owner.close()
        except RuntimeError:
            if not failure_in_flight:
                raise
            print("veranda-owner: owner close failed after scenario failure", file=sys.stderr)


def self_test():
    require(safe_fault({"error": {"code": "internal", "message": "signal: killed"}}) ==
            ("internal", "command_killed"), "known native fault classification failed")
    for code in ("private\nvalue", "x" * 65, "caf\u00e9", None):
        require(safe_fault({"error": {"code": code, "message": "private-value"}}) ==
                ("invalid_fault", "unclassified"), "unsafe fault code survived validation")
    require(safe_fault({"error": {"code": "internal", "message": "x" * 8193}}) ==
            ("internal", "unclassified"), "oversized fault message was inspected")
    owner = Owner.__new__(Owner)
    owner.last_stage = "unobserved"
    owner.events = [{"id": "reply", "event": "private-event"},
                    {"id": "other-reply", "event": "operation.finished"}]
    owner.send = lambda *args: ("reply", "operation")
    owner.await_reply = lambda *args: {"id": "reply", "error": {
        "code": "private\nvalue", "message": "private-content"}}
    try:
        owner.result("operation.execute")
    except RuntimeError as failure:
        diagnostic = str(failure)
        require("code=invalid_fault" in diagnostic and "lifecycle=unobserved" in diagnostic and
                "private" not in diagnostic, "fault diagnostic leaked untrusted content")
    else:
        raise RuntimeError("fault result unexpectedly accepted")
    read_fd, write_fd = os.pipe()
    with os.fdopen(read_fd, "rb") as diagnostic_input:
        owner.process = type("Process", (), {"stderr": diagnostic_input})()
        os.write(write_fd, b"  [ .. ] provision\nprivate-content\n" + b"x" * 200 +
                 b"  [ .. ] ssh\n  [ .. ] instance (already converged)\n")
        os.close(write_fd)
        owner.drain_diagnostics()
    require(owner.last_stage == "provision", "diagnostic drain accepted a noncanonical stage line")
    print("ok: bounded fault/stage diagnostics redact untrusted content")


if __name__ == "__main__":
    try:
        if sys.argv[1:] == ["--self-test"]:
            self_test()
        else:
            run(*sys.argv[1:])
    except (RuntimeError, OSError, ValueError, StopIteration, subprocess.TimeoutExpired) as failure:
        # Never copy raw native output, environment, paths or response payloads to evidence.
        print("FAIL: veranda-owner: " + (str(failure) if isinstance(failure, RuntimeError)
                                          else type(failure).__name__), file=sys.stderr)
        sys.exit(1)
