"""Keep stock Orca launch defaults from overriding the yard's Codex and Claude config."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import sys
import tempfile
import time
import uuid

from transport import RpcError, RuntimeRPC


YOLO = "--dangerously-bypass-approvals-and-sandbox"
# Orca's stock arguments per agent. A stock Claude flag also makes Orca's native chat
# answer every permission prompt itself, bypassing the yard's managed approval rules.
STOCK = {"codex": YOLO, "claude": "--dangerously-skip-permissions"}
# Orca saves a chat's permission mode when the chat is created, so chats created under
# the stock Claude flag keep Full access after the repair. Each such chat is reset to
# Orca's default mode at most once; a process that started with Full access keeps it
# until Orca restarts, so the reset completes only in a later Orca runtime.
CHAT_STATE = "subyard-claude-chat-permissions.json"
RESTART_PENDING = 3


def _arguments(rpc):
    result = rpc.call("settings.get")
    settings = result.get("settings")
    if not isinstance(settings, dict):
        raise RpcError("Orca returned invalid agent settings")
    arguments = settings.get("agentDefaultArgs", {})
    if not isinstance(arguments, dict) or not all(
        isinstance(key, str) and isinstance(value, str) for key, value in arguments.items()
    ):
        raise RpcError("Orca returned invalid agent arguments")
    return arguments


def _stock(arguments):
    return [agent for agent, stock in STOCK.items()
            if agent not in arguments or arguments[agent].strip() == stock]


def launch_defaults(rpc, apply=False):
    arguments = _arguments(rpc)
    # An explicit empty entry overrides Orca's built-in stock default. Preserve custom
    # arguments and every other agent's settings; the agents own their configuration.
    stock = _stock(arguments)
    if not stock:
        return True
    if not apply:
        return False
    rpc.call("settings.update", {"agentDefaultArgs": {**arguments, **dict.fromkeys(stock, "")}})
    if _stock(_arguments(rpc)):
        raise RpcError("Orca agent launch defaults did not converge")
    return True


def claude_chats(rpc):
    """Return saved Claude chats: open tabs, and Session History for chats whose tab closed."""
    sessions = set()
    for snapshot in rpc.call("session.tabs.listAll")["snapshots"]:
        sessions.update(tab["sessionId"] for tab in snapshot["tabs"]
                        if tab.get("type") == "agent-session" and tab.get("agent") == "claude")
    history = rpc.call("aiVault.listSessions", {"unlimited": True, "force": True})
    # A scan with diagnostics or paging may omit a closed chat; never complete from it.
    if history["issues"] or any(history.get(key) for key in ("truncated", "hasMore", "nextFingerprint", "unchanged")):
        raise RpcError("Orca Session History scan is incomplete")
    for entry in history["sessions"]:
        if entry.get("agent") == "claude" and entry.get("structuredSession"):
            sessions.add(entry["structuredSession"]["sessionId"])
    return sorted(sessions)


def load_chat_state(state):
    try:
        data = json.loads((Path(state) / CHAT_STATE).read_bytes())
    except FileNotFoundError:
        return {"version": 1, "reset": [], "runtime": None, "scanned": False, "complete": False}
    if (not isinstance(data, dict) or set(data) != {"version", "reset", "runtime", "scanned", "complete"}
            or data["version"] != 1 or not isinstance(data["reset"], list)
            or not all(isinstance(value, str) and value for value in data["reset"])
            or not (data["runtime"] is None or isinstance(data["runtime"], str) and data["runtime"])
            or not isinstance(data["scanned"], bool) or not isinstance(data["complete"], bool)):
        raise ValueError("invalid Claude chat permission state")
    return data


def _save_chat_state(state, data):
    directory = Path(state)
    descriptor, temporary = tempfile.mkstemp(prefix=".subyard-claude-chat-", dir=directory)
    try:
        with os.fdopen(descriptor, "w") as output:
            json.dump(data, output, sort_keys=True, separators=(",", ":"))
            output.write("\n")
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, directory / CHAT_STATE)
        temporary = None
        directory_fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    finally:
        if temporary is not None:
            os.unlink(temporary)


def restart_pending(data, runtime):
    """Chats were reset in this Orca runtime, so a process may still hold Full access."""
    return not data["complete"] and data["runtime"] == runtime


def _set_default_mode(rpc, session):
    fields = {"key": "permissionMode", "value": "own"}
    payload = json.dumps({"method": "agentSession.setOption", "sessionId": session, "fields": fields},
                         sort_keys=True, separators=(",", ":")).encode()
    for attempt in range(2):
        # The fenced write applies to the saved chat whether or not it is running.
        envelope = {"sessionId": session, "clientOperationId": f"{time.time_ns() // 1_000_000}-{uuid.uuid4().hex}",
                    "expectedRuntimeFence": rpc.call("agentSession.history", {
                        "sessionId": session, "direction": "tail"})["page"]["fence"],
                    "payloadFingerprint": hashlib.sha256(payload).hexdigest()}
        result = rpc.call("agentSession.setOption", {"envelope": envelope, **fields})
        if result.get("ok") is True:
            break
        moved = ((result.get("refusal") or {}).get("details") or {}).get("reason") == "leaseMoved"
        if attempt or not moved:
            raise RpcError("Orca refused a chat permission reset")
    if rpc.call("agentSession.options", {"sessionId": session})["current"]["permissionMode"] != "own":
        raise RpcError("Orca did not reset a chat permission mode")


def reset_chat_permissions(rpc, state, runtime):
    """Reset each saved Full access Claude chat once; return (complete, reset count)."""
    data = load_chat_state(state)
    if data["complete"]:
        return True, 0
    if data["scanned"] and data["runtime"] != runtime:
        # Orca restarted after the last reset, ending every process that started before it.
        _save_chat_state(state, {**data, "complete": True})
        return True, 0
    reset = 0
    for session in claude_chats(rpc):
        if session in data["reset"] or rpc.call("agentSession.options", {
                "sessionId": session})["current"]["permissionMode"] != "full-access":
            continue
        _set_default_mode(rpc, session)
        # Record each reset durably so that no chat is ever reset twice.
        data = {**data, "reset": data["reset"] + [session], "runtime": runtime, "scanned": False}
        _save_chat_state(state, data)
        reset += 1
    data = {**data, "scanned": True, "complete": data["runtime"] != runtime}
    _save_chat_state(state, data)
    return data["complete"], reset


RESTART_WARNING = ("Warning: saved Claude chats may still run Git without approval until Orca restarts; "
                   "run yard orca restart")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true")
    parser.add_argument("--state", default="/srv/agents/orca")
    args = parser.parse_args()
    runtime = Path(args.state) / "config/orca/orca-runtime.json"
    rpc = RuntimeRPC(runtime)
    try:
        ready = launch_defaults(rpc, apply=not args.check)
    except Exception:
        # Neither settings nor upstream errors may enter logs: they can contain secrets.
        print("Orca agent launch settings unavailable; run yard orca up", file=sys.stderr)
        return 1
    if args.check or not ready:
        if not ready:
            return 1
        try:
            pending = restart_pending(load_chat_state(args.state), rpc.runtime_id)
        except (OSError, ValueError):
            pending = False  # The apply pass reports an unreadable state; readiness must not wedge.
        return RESTART_PENDING if pending else 0
    try:
        # Session History scans saved transcripts, which can take longer than a settings call.
        complete, reset = reset_chat_permissions(RuntimeRPC(runtime, timeout=30), args.state, rpc.runtime_id)
    except (RpcError, OSError, ValueError, KeyError, TypeError, AttributeError):
        # Every later apply retries the pass and warns again; reset chats are never reset twice.
        print("Warning: Orca could not finish resetting saved Claude chats from Full access; they may still "
              "run Git without approval. Run yard orca sync, or switch each chat's mode to Ask.", file=sys.stderr)
        return 0
    if reset:
        print(f"Orca reset {reset} saved Claude chat(s) from Full access to the default permission mode; "
              "choose Full access in a chat again to opt out")
    if not complete:
        print(RESTART_WARNING, file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
