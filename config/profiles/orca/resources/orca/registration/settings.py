"""Keep stock Orca launch defaults from overriding the yard's Codex and Claude config."""

import argparse
import contextlib
import fcntl
import hashlib
import json
import os
from pathlib import Path
import stat
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
# Orca's default mode at most once. A process that started with Full access keeps it
# until Orca restarts, so every attempted reset first records its runtime as dirty, and
# only a later runtime's clean pass without any attempt completes the reset.
CHAT_STATE = "subyard-claude-chat-permissions.json"
CHAT_LOCK = ".subyard-claude-chat-permissions.lock"
CHAT_STATE_LIMIT = 1024 * 1024
LOCK_WAIT = 120
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
    """Return (saved Claude chats, whether the scan was complete).

    Chats come from open tabs and from Session History for chats whose tab closed. A failed
    or partial listing still yields the chats it found, but only a complete scan may finish.
    """
    sessions, clean = set(), True
    try:
        for snapshot in rpc.call("session.tabs.listAll")["snapshots"]:
            sessions.update(tab["sessionId"] for tab in snapshot["tabs"]
                            if tab.get("type") == "agent-session" and tab.get("agent") == "claude")
    except (RpcError, KeyError, TypeError, AttributeError):
        clean = False
    try:
        history = rpc.call("aiVault.listSessions", {"unlimited": True, "force": True})
        # A scan with diagnostics or paging may omit a closed chat.
        if history["issues"] or any(history.get(key) for key in ("truncated", "hasMore", "nextFingerprint", "unchanged")):
            clean = False
        for entry in history["sessions"]:
            if entry.get("agent") == "claude" and entry.get("structuredSession"):
                sessions.add(entry["structuredSession"]["sessionId"])
    except (RpcError, KeyError, TypeError, AttributeError):
        clean = False
    valid = sorted(session for session in sessions if isinstance(session, str) and session)
    return valid, clean and len(valid) == len(sessions)


def _initial_chat_state():
    return {"version": 1, "reset": [], "dirty": None, "complete": False}


def load_chat_state(state):
    """Return the saved reset state; raise OSError or ValueError when it is unreadable."""
    try:
        fd = os.open(Path(state) / CHAT_STATE, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    except FileNotFoundError:
        return _initial_chat_state()
    with os.fdopen(fd, "rb") as source:
        if not stat.S_ISREG(os.fstat(source.fileno()).st_mode):
            raise ValueError("Claude chat permission state is not a regular file")
        raw = source.read(CHAT_STATE_LIMIT + 1)
    if len(raw) > CHAT_STATE_LIMIT:
        raise ValueError("Claude chat permission state is too large")
    try:
        data = json.loads(raw)
    except RecursionError:
        raise ValueError("invalid Claude chat permission state") from None
    if (not isinstance(data, dict) or set(data) != {"version", "reset", "dirty", "complete"}
            or data["version"] != 1 or not isinstance(data["reset"], list)
            or not all(isinstance(value, str) and value for value in data["reset"])
            or not (data["dirty"] is None or isinstance(data["dirty"], str) and data["dirty"])
            or not isinstance(data["complete"], bool)):
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


def reset_state(data, runtime):
    """Name the reset state for the current Orca runtime."""
    if data["complete"]:
        return "complete"
    # A reset was attempted in this runtime, so a chat process may still hold Full access.
    return "restart-pending" if data["dirty"] is not None and data["dirty"] == runtime else "incomplete"


@contextlib.contextmanager
def _exclusive(state, wait=None):
    """Serialize every reset pass: sync, project hooks, init, up and restart."""
    wait = LOCK_WAIT if wait is None else wait
    fd = os.open(Path(state) / CHAT_LOCK, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC,
                 0o600)
    try:
        if not stat.S_ISREG(os.fstat(fd).st_mode):
            raise OSError("Claude chat permission lock is not a regular file")
        os.fchmod(fd, 0o600)
        end = time.monotonic() + wait
        while True:
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if time.monotonic() >= end:
                    raise OSError("another Claude chat permission reset holds the lock") from None
                time.sleep(0.05)
        yield
    finally:
        os.close(fd)


class _Observed:
    """Record every runtime this pass talked to, including through failed calls."""

    def __init__(self, rpc):
        self.rpc = rpc
        self.runtimes = []

    def call(self, method, params=None, before_send=None):
        try:
            return self.rpc.call(method, params, before_send=before_send)
        finally:
            runtime = self.rpc.runtime_id
            if runtime is not None and runtime not in self.runtimes:
                self.runtimes.append(runtime)


def _set_default_mode(rpc, session, before_send):
    fields = {"key": "permissionMode", "value": "own"}
    payload = json.dumps({"method": "agentSession.setOption", "sessionId": session, "fields": fields},
                         sort_keys=True, separators=(",", ":")).encode()
    for attempt in range(2):
        # The fenced write applies to the saved chat whether or not it is running.
        envelope = {"sessionId": session, "clientOperationId": f"{time.time_ns() // 1_000_000}-{uuid.uuid4().hex}",
                    "expectedRuntimeFence": rpc.call("agentSession.history", {
                        "sessionId": session, "direction": "tail"})["page"]["fence"],
                    "payloadFingerprint": hashlib.sha256(payload).hexdigest()}
        result = rpc.call("agentSession.setOption", {"envelope": envelope, **fields}, before_send=before_send)
        if result.get("ok") is True:
            break
        moved = ((result.get("refusal") or {}).get("details") or {}).get("reason") == "leaseMoved"
        if attempt or not moved:
            raise RpcError("Orca refused a chat permission reset")
    if rpc.call("agentSession.options", {"sessionId": session})["current"]["permissionMode"] != "own":
        raise RpcError("Orca did not reset a chat permission mode")


def reset_chat_permissions(rpc, state):
    """Reset each saved Full access Claude chat once under the exclusive lock.

    Return (state name, reset count, whether an unreadable state was rewritten).
    """
    with _exclusive(state):
        return _reset_pass(rpc, state)


def _reset_pass(rpc, state):
    recovered = False
    try:
        data = load_chat_state(state)
    except (OSError, ValueError):
        data, recovered = None, True
    if not recovered and data["complete"]:
        # Completed: a later explicit Full access choice is the user's opt-out.
        return "complete", 0, False
    observed = _Observed(rpc)
    sessions, clean = claude_chats(observed)
    if not observed.runtimes:
        raise RpcError("Orca runtime metadata is unavailable or invalid")
    runtime = observed.runtimes[0]
    if recovered:
        # A lost record may hide a reset in this runtime: require one restart.
        data = {**_initial_chat_state(), "dirty": runtime}
        _save_chat_state(state, data)
    attempted = refused = False
    count = 0

    def mark_dirty(connection_runtime):
        # Runs on the write's own connection before the request is sent, so any write
        # Orca may apply keeps a restart pending whatever happens afterwards.
        nonlocal data
        if data["dirty"] != connection_runtime:
            updated = {**data, "dirty": connection_runtime}
            _save_chat_state(state, updated)
            data = updated

    for session in sessions:
        if session in data["reset"]:
            continue
        try:
            if observed.call("agentSession.options", {
                    "sessionId": session})["current"]["permissionMode"] != "full-access":
                continue
        except (RpcError, KeyError, TypeError, AttributeError):
            refused = True
            continue
        attempted = True
        try:
            _set_default_mode(observed, session, mark_dirty)
            # Record each reset durably so that no chat is ever reset twice.
            updated = {**data, "reset": data["reset"] + [session]}
            _save_chat_state(state, updated)
            data = updated
            count += 1
        except (RpcError, OSError, KeyError, TypeError, AttributeError):
            refused = True
    if (clean and not attempted and not refused and observed.runtimes == [runtime]
            and data["dirty"] != runtime):
        data = {**data, "complete": True}
        _save_chat_state(state, data)
    return reset_state(data, observed.runtimes[-1]), count, recovered


RESTART_WARNING = ("Warning: saved Claude chats may still run Git without approval until Orca restarts; "
                   "run yard orca restart")
INCOMPLETE_WARNING = ("Warning: Orca could not finish resetting saved Claude chats from Full access; they may "
                      "still run Git without approval. Run yard orca sync, or switch each chat's mode to Ask.")
RECOVERED_WARNING = ("Warning: the saved Claude chat reset state was unreadable and was rewritten; every saved "
                     "Claude chat is treated as possibly reset in the running Orca, so Orca needs one restart")


def check(rpc, state):
    """Report launch defaults and the reset separately; exit 3 only while a restart is pending."""
    try:
        ready = launch_defaults(rpc)
    except Exception:
        # Neither settings nor upstream errors may enter logs: they can contain secrets.
        print("Orca agent launch settings unavailable; run yard orca up", file=sys.stderr)
        print("launch=unavailable reset=unknown")
        return 1
    try:
        name = reset_state(load_chat_state(state), rpc.runtime_id)
    except (OSError, ValueError):
        name = "unreadable"  # The next apply pass rewrites it; readiness must not wedge.
    print(f"launch={'ready' if ready else 'unready'} reset={name}")
    return RESTART_PENDING if name == "restart-pending" else 0 if ready else 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--check", action="store_true")
    mode.add_argument("--reset-chats", action="store_true", help="run only the saved Claude chat reset")
    parser.add_argument("--state", default="/srv/agents/orca")
    args = parser.parse_args()
    runtime = Path(args.state) / "config/orca/orca-runtime.json"
    if args.check:
        return check(RuntimeRPC(runtime), args.state)
    if not args.reset_chats:
        try:
            launch_defaults(RuntimeRPC(runtime), apply=True)
        except Exception:
            print("Orca agent launch settings unavailable; run yard orca up", file=sys.stderr)
            return 1
    try:
        # Session History scans saved transcripts, which can take longer than a settings call.
        name, reset, recovered = reset_chat_permissions(RuntimeRPC(runtime, timeout=30), args.state)
    except (RpcError, OSError, ValueError, KeyError, TypeError, AttributeError):
        # Every later apply retries the pass and warns again; reset chats are never reset twice.
        print(INCOMPLETE_WARNING, file=sys.stderr)
        return 0
    if recovered:
        print(RECOVERED_WARNING, file=sys.stderr)
    if reset:
        print(f"Orca reset {reset} saved Claude chat(s) from Full access to the default permission mode; "
              "choose Full access in a chat again to opt out")
    if name == "restart-pending":
        print(RESTART_WARNING, file=sys.stderr)
    elif name != "complete":
        print(INCOMPLETE_WARNING, file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
