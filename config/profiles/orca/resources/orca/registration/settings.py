"""Keep stock Orca launch defaults from overriding the yard's Codex and Claude config."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import sys
import time
import uuid

from transport import RpcError, RuntimeRPC


YOLO = "--dangerously-bypass-approvals-and-sandbox"
# Orca's stock arguments per agent. A stock Claude flag also makes Orca's native chat
# answer every permission prompt itself, bypassing the yard's managed approval rules.
STOCK = {"codex": YOLO, "claude": "--dangerously-skip-permissions"}
# Orca saves a chat's permission mode when the chat is created, so chats created under
# the stock Claude flag keep Full access after the repair. A one-time pass returns them
# to Orca's default mode; the marker then leaves later explicit Full access choices alone.
MIGRATED = "subyard-claude-chat-permissions-v1"


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
    if history["issues"] or any(history.get(key) for key in ("truncated", "hasMore", "nextFingerprint")):
        raise RpcError("Orca Session History scan is incomplete")
    for entry in history["sessions"]:
        if entry.get("agent") == "claude" and entry.get("structuredSession"):
            sessions.add(entry["structuredSession"]["sessionId"])
    return sorted(sessions)


def _chat(rpc, session):
    """Return (saved mode, running) for a saved chat."""
    current = rpc.call("agentSession.options", {"sessionId": session})["current"]
    # Orca reports provider facts only once a chat's process starts; any of them means running.
    running = any(current.get(key) is not None for key in ("reportedPermissionMode", "reportedModel", "confirmed"))
    return current["permissionMode"], running


def reset_chat_permissions(rpc, state):
    """Reset saved Full access Claude chats once; return (reset, chats still running)."""
    marker = Path(state) / MIGRATED
    if marker.exists():
        return 0, 0
    reset = running = 0
    for session in claude_chats(rpc):
        mode, live = _chat(rpc, session)
        if live:
            # A process started from a Full access chat keeps it until the chat closes.
            running += 1
            continue
        if mode != "full-access":
            continue
        fields = {"key": "permissionMode", "value": "own"}
        payload = json.dumps({"method": "agentSession.setOption", "sessionId": session, "fields": fields},
                             sort_keys=True, separators=(",", ":")).encode()
        # The fence makes Orca refuse the write if the chat's lease moved since this read.
        envelope = {"sessionId": session, "clientOperationId": f"{time.time_ns() // 1_000_000}-{uuid.uuid4().hex}",
                    "expectedRuntimeFence": rpc.call("agentSession.history", {
                        "sessionId": session, "direction": "tail"})["page"]["fence"],
                    "payloadFingerprint": hashlib.sha256(payload).hexdigest()}
        if rpc.call("agentSession.setOption", {"envelope": envelope, **fields}).get("ok") is not True:
            raise RpcError("Orca refused a chat permission reset")
        mode, live = _chat(rpc, session)
        if live:
            running += 1
        elif mode != "own":
            raise RpcError("Orca did not reset a chat permission mode")
        reset += 1
    if not running:
        descriptor = os.open(marker, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
        with os.fdopen(descriptor, "w") as output:
            output.write("1\n")
            output.flush()
            os.fsync(output.fileno())
    return reset, running


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
        return 0 if ready else 1
    try:
        # Session History scans saved transcripts, which can take longer than a settings call.
        reset, running = reset_chat_permissions(RuntimeRPC(runtime, timeout=30), args.state)
    except (RpcError, OSError, KeyError, TypeError, AttributeError):
        # Without the marker, every later apply retries the whole pass and warns again.
        print("Warning: Orca could not reset saved Claude chats from Full access; they may still run "
              "Git without approval. Run yard orca sync, or switch each chat's mode to Ask.", file=sys.stderr)
        return 0
    if reset:
        print(f"Orca reset {reset} saved Claude chat(s) from Full access to the default permission mode; "
              "choose Full access in a chat again to opt out")
    if running:
        print(f"Warning: {running} running Claude chat(s) may still run Git without approval if they started "
              "with Full access. Close them and run yard orca sync, or switch each chat's mode to Ask.",
              file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
