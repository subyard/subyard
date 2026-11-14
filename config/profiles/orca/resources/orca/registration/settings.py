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
    for entry in rpc.call("aiVault.listSessions", {"unlimited": True, "force": True})["sessions"]:
        if entry.get("agent") == "claude" and entry.get("structuredSession"):
            sessions.add(entry["structuredSession"]["sessionId"])
    return sorted(sessions)


def reset_chat_permissions(rpc, state):
    """Reset saved Full access Claude chats once; return (reset, chats still pending)."""
    marker = Path(state) / MIGRATED
    if marker.exists():
        return 0, 0
    reset = pending = 0
    for session in claude_chats(rpc):
        current = rpc.call("agentSession.options", {"sessionId": session})["current"]
        if current["permissionMode"] != "full-access":
            continue
        # Orca reports the provider's mode only while the chat's process runs; retry later.
        if current.get("reportedPermissionMode") is not None:
            pending += 1
            continue
        fields = {"key": "permissionMode", "value": "own"}
        payload = json.dumps({"method": "agentSession.setOption", "sessionId": session, "fields": fields},
                             sort_keys=True, separators=(",", ":")).encode()
        envelope = {"sessionId": session, "clientOperationId": f"{time.time_ns() // 1_000_000}-{uuid.uuid4().hex}",
                    "expectedRuntimeFence": rpc.call("agentSession.history", {
                        "sessionId": session, "direction": "tail"})["page"]["fence"],
                    "payloadFingerprint": hashlib.sha256(payload).hexdigest()}
        if (rpc.call("agentSession.setOption", {"envelope": envelope, **fields}).get("ok") is not True
                or rpc.call("agentSession.options", {"sessionId": session})["current"]["permissionMode"] != "own"):
            raise RpcError("Orca did not reset a chat permission mode")
        reset += 1
    if not pending:
        descriptor = os.open(marker, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
        with os.fdopen(descriptor, "w") as output:
            output.write("1\n")
            output.flush()
            os.fsync(output.fileno())
    return reset, pending


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true")
    parser.add_argument("--state", default="/srv/agents/orca")
    args = parser.parse_args()
    # Session History scans saved transcripts, which can take longer than a settings call.
    rpc = RuntimeRPC(Path(args.state) / "config/orca/orca-runtime.json", timeout=30)
    try:
        ready = launch_defaults(rpc, apply=not args.check)
    except Exception:
        # Neither settings nor upstream errors may enter logs: they can contain secrets.
        print("Orca agent launch settings unavailable; run yard orca up", file=sys.stderr)
        return 1
    if args.check or not ready:
        return 0 if ready else 1
    try:
        reset, pending = reset_chat_permissions(rpc, args.state)
    except (RpcError, OSError, KeyError, TypeError, AttributeError):
        # Without the marker, every later apply retries the whole pass and warns again.
        print("Warning: Orca could not reset saved Claude chats from Full access; they may still run "
              "Git without approval. Run yard orca sync, or switch each chat's mode to Ask.", file=sys.stderr)
        return 0
    if reset:
        print(f"Orca reset {reset} saved Claude chat(s) from Full access to the default permission mode; "
              "choose Full access in a chat again to opt out")
    if pending:
        print(f"Warning: {pending} open saved Claude chat(s) still use Full access and may run Git without "
              "approval. Close them and run yard orca sync, or switch each chat's mode to Ask.", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
