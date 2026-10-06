#!/usr/bin/env python3
"""Sanitized stock-Orca RPC client and forced-command fixture for orca-projects.sh."""

import argparse
import grp
import hashlib
import json
import os
from pathlib import Path
import pwd
import re
import shlex
import signal
import socket
import stat
import subprocess
import sys
import tempfile
import time
import uuid


class SafeRpcError(Exception):
    """An error whose text never contains upstream payloads or credentials."""


def read_runtime_metadata(path):
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        with os.fdopen(fd, "rb") as source:
            if not stat.S_ISREG(os.fstat(source.fileno()).st_mode):
                raise ValueError()
            raw = source.read(65537)
        if len(raw) > 65536:
            raise ValueError()
        metadata = json.loads(raw)
        transports = metadata.get("transports", [metadata.get("transport")])
        endpoint = next(
            item["endpoint"]
            for item in transports
            if isinstance(item, dict) and item.get("kind") == "unix"
        )
        token = metadata["authToken"]
        runtime_id = metadata["runtimeId"]
        if not all(isinstance(value, str) and value for value in (endpoint, token, runtime_id)):
            raise ValueError()
        return endpoint, token, runtime_id
    except (OSError, ValueError, TypeError, KeyError, StopIteration, RecursionError):
        raise SafeRpcError("runtime metadata unavailable") from None


def call_any_result(metadata_path, method, params):
    deadline = time.monotonic() + 10
    endpoint, token, runtime_id = read_runtime_metadata(metadata_path)
    request_id = str(uuid.uuid4())
    request = {"id": request_id, "authToken": token, "method": method}
    if params is not None:
        request["params"] = params
    try:
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
            connection.settimeout(10)
            connection.connect(endpoint)
            connection.sendall(json.dumps(request).encode("utf-8") + b"\n")
            buffered = b""
            while True:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise SafeRpcError("runtime request timed out")
                connection.settimeout(remaining)
                chunk = connection.recv(65536)
                if not chunk:
                    raise SafeRpcError("runtime disconnected")
                buffered += chunk
                if len(buffered) > 8 * 1024 * 1024:
                    raise SafeRpcError("response exceeded size limit")
                while b"\n" in buffered:
                    line, buffered = buffered.split(b"\n", 1)
                    if not line.strip():
                        continue
                    try:
                        frame = json.loads(line)
                    except (ValueError, RecursionError):
                        raise SafeRpcError("runtime returned invalid JSON") from None
                    if frame == {"_keepalive": True}:
                        continue
                    if not isinstance(frame, dict) or frame.get("id") != request_id:
                        raise SafeRpcError("response correlation mismatch")
                    metadata = frame.get("_meta")
                    if not isinstance(metadata, dict) or metadata.get("runtimeId") != runtime_id:
                        raise SafeRpcError("response runtime mismatch")
                    if frame.get("ok") is False:
                        raise SafeRpcError("runtime rejected method")
                    if frame.get("ok") is not True or "result" not in frame:
                        raise SafeRpcError("runtime returned invalid envelope")
                    return frame["result"]
    except (TimeoutError, socket.timeout):
        raise SafeRpcError("runtime request timed out") from None
    except OSError:
        raise SafeRpcError("runtime connection failed") from None


def rpc_call(arguments):
    try:
        params = json.loads(arguments.params) if arguments.params is not None else None
        result = call_any_result(
            Path(arguments.state) / "config/orca/orca-runtime.json", arguments.method, params
        )
        print(json.dumps(result, separators=(",", ":")))
    except SafeRpcError as error:
        print(
            f"orca-projects-helper: {arguments.method}: {error}", file=sys.stderr
        )
        return 1
    except (OSError, ValueError, TypeError, RecursionError):
        print(
            f"orca-projects-helper: {arguments.method}: invalid test request", file=sys.stderr
        )
        return 1
    return 0


def snapshot_tabs(result):
    snapshots = result.get("snapshots") if isinstance(result, dict) else None
    if not isinstance(snapshots, list) or any(
        not isinstance(item, dict) or not isinstance(item.get("worktree"), str)
        or not isinstance(item.get("tabs"), list)
        or any(not isinstance(tab, dict) or not isinstance(tab.get("id"), str)
               or not tab["id"] for tab in item["tabs"])
        for item in snapshots
    ):
        raise SafeRpcError("invalid session snapshots")
    return snapshots


def recovery_document(value):
    if (not isinstance(value, dict) or not isinstance(value.get("settings"), dict)
            or not isinstance(value["settings"].get("codexTerminalServerIsolation"), bool)):
        raise SafeRpcError("invalid recovery settings export")
    return value


def cli_result(value):
    if (not isinstance(value, dict) or value.get("ok") is not True
            or not isinstance(value.get("result"), dict)):
        raise SafeRpcError("invalid stock CLI envelope")
    return value["result"]


def isolation_settings(cli, root, value=None, expected=None):
    """Use stock recovery while stopped; never expose the full private document."""
    exports = cli_result(json.loads(cli("profile", "state", "exports", "--json")))
    data_file = Path(exports["dataFile"])
    if data_file.is_symlink() or not data_file.resolve().is_relative_to(root.resolve()):
        raise SafeRpcError("recovery export escaped profile")
    # SQLite owns ordinary runtime state. Stock publishes its canonical JSON.
    cli_result(json.loads(cli("profile", "state", "rollback", "--current-sqlite", "--json")))
    document = recovery_document(json.loads(data_file.read_text()))
    actual = document["settings"]["codexTerminalServerIsolation"]
    if expected is not None and actual is not expected:
        raise SafeRpcError("recovery setting readback mismatch")
    if value is not None:
        document["settings"]["codexTerminalServerIsolation"] = value
        data_file.write_text(json.dumps(document))
        result = cli_result(json.loads(cli("profile", "state", "rollback", "--current-json", "--json")))
        if result.get("storage") != "json" or result.get("restoredPath") != str(data_file):
            raise SafeRpcError("invalid recovery import result")
        return value
    return actual


def set_isolation(arguments):
    root = Path(arguments.state)
    environment = {"PATH": "/usr/local/bin:/usr/bin:/bin", "HOME": str(Path.home()),
                   "XDG_CONFIG_HOME": str(root / "config"),
                   "XDG_DATA_HOME": str(root / "data"),
                   "XDG_STATE_HOME": str(root / "state")}

    def cli(*command):
        return subprocess.run([arguments.binary, *command], env=environment,
                              stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                              timeout=15, check=True).stdout

    try:
        value = None if arguments.value is None else arguments.value == "true"
        expected = None if arguments.expect is None else arguments.expect == "true"
        isolation_settings(cli, root, value, expected)
        print("ok: stock Orca isolation recovery setting")
        return 0
    except SafeRpcError as error:
        print(f"orca-isolation: {error}", file=sys.stderr)
    except Exception:
        print("orca-isolation: stock recovery failed", file=sys.stderr)
    return 1


def stock_probe(arguments):
    """Probe only a disposable profile: never use the caller's accounts or runtime."""
    phase = "version"
    server = None
    try:
        sys.path.insert(0, arguments.registration)
        from reconcile import _records
        from settings import codex_defaults

        with tempfile.TemporaryDirectory(prefix="subyard-orca-stock-probe.") as directory:
            root = Path(directory)
            environment = {"PATH": "/usr/local/bin:/usr/bin:/bin", "HOME": str(root / "home"),
                           "XDG_CONFIG_HOME": str(root / "config"),
                           "XDG_DATA_HOME": str(root / "data"),
                           "XDG_STATE_HOME": str(root / "state")}
            for path in environment.values():
                if path.startswith(directory):
                    Path(path).mkdir(mode=0o700)

            def cli(*command):
                result = subprocess.run([arguments.binary, *command], env=environment,
                                        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                        timeout=15, check=True)
                return result.stdout

            if cli("--version").decode().strip() != arguments.version:
                print("orca-stock-probe: unavailable: pinned stock binary version mismatch", file=sys.stderr)
                return 2
            metadata = root / "config/orca/orca-runtime.json"

            class RPC:
                def call(self, method, params=None):
                    return call_any_result(metadata, method, params)

            rpc = RPC()

            def stop():
                nonlocal server
                if server is None:
                    return
                try:
                    os.killpg(server.pid, signal.SIGTERM)
                except ProcessLookupError:
                    pass
                try:
                    server.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    os.killpg(server.pid, signal.SIGKILL)
                    server.wait(timeout=5)
                    raise SafeRpcError("stock runtime did not stop") from None
                finally:
                    server = None

            def start():
                nonlocal server
                with socket.socket() as listener:
                    listener.bind(("127.0.0.1", 0))
                    port = listener.getsockname()[1]
                server = subprocess.Popen(
                    [arguments.binary, "serve", "--no-pairing", "--port", str(port)],
                    env=environment, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                    start_new_session=True,
                )
                deadline = time.monotonic() + 20
                while time.monotonic() < deadline and server.poll() is None:
                    if metadata.is_file():
                        try:
                            rpc.call("status.get")
                            return
                        except SafeRpcError:
                            pass
                    time.sleep(0.1)
                raise SafeRpcError("stock runtime did not become reachable")

            try:
                phase = "startup"
                start()
                phase = "registration and terminal snapshots"
                codex_defaults(rpc, apply=True)
                project = root / "project"
                project.mkdir(mode=0o700)
                group = rpc.call("projectGroup.create", {
                    "name": "Stock probe", "parentPath": str(project), "createdFrom": "migration"
                })["group"]["id"]
                repo = rpc.call("repo.add", {"path": str(project), "kind": "folder"})["repo"]["id"]
                rpc.call("projectGroup.moveProject", {"repo": "id:" + repo, "groupId": group})

                def check_projects():
                    if not any(item["id"] == group for item in _records(rpc, "projectGroup.list", "groups")):
                        raise SafeRpcError("project group missing from snapshot")
                    if not any(item["id"] == repo and item.get("projectGroupId") == group
                               and item.get("path") == str(project) and item.get("kind") == "folder"
                               for item in _records(rpc, "repo.list", "repos")):
                        raise SafeRpcError("project missing from snapshot")

                check_projects()
                tab = rpc.call("session.tabs.createTerminal", {
                    "worktree": "id:" + repo + "::" + str(project), "activate": False,
                    "command": "printf 'stock-probe\\n'", "clientMutationId": "stock-probe"
                })["tab"]["id"]

                def check_tab():
                    snapshots = snapshot_tabs(rpc.call("session.tabs.listAll"))
                    if not any(item["worktree"].startswith(repo + "::")
                               and any(saved["id"] == tab for saved in item["tabs"])
                               for item in snapshots):
                        raise SafeRpcError("terminal missing from snapshot")

                check_tab()
                phase = "recovery export shutdown"
                stop()
                phase = "recovery settings export"
                before = isolation_settings(cli, root)
                values = (False, True) if arguments.resource_settings else (not before,)
                for expected in values:
                    phase = "recovery setting import"
                    isolation_settings(cli, root, expected)
                    phase = "recovery restart"
                    start()
                    phase = "recovered terminal and project snapshots"
                    check_tab()
                    check_projects()
                    stop()
                    phase = "recovered settings export"
                    isolation_settings(cli, root, expected=expected)
            finally:
                stop()
        print("ok: pinned stock Orca registration, terminal snapshots and recovery setting export/import")
        return 0
    except SafeRpcError as error:
        print(f"orca-stock-probe: {phase}: {error}", file=sys.stderr)
        return 1
    except Exception:
        # CLI output, profile exports and upstream exceptions may contain private state.
        print(f"orca-stock-probe: {phase}: failed", file=sys.stderr)
        return 1


def parse_yard_command(original):
    try:
        outer = shlex.split(original)
        if len(outer) != 3 or outer[:2] != ["bash", "-lc"]:
            return None, None
        inner = shlex.split(outer[2])
    except ValueError:
        return None, None
    environment = {}
    if inner and inner[0].startswith("SUBYARD_OPERATION_ID="):
        operation_id = inner.pop(0).split("=", 1)[1]
        if not re.fullmatch(r"[A-Za-z0-9._:-]{1,160}", operation_id):
            return None, None
        environment["SUBYARD_OPERATION_ID"] = operation_id
    if inner and inner[0] == "exec":
        inner.pop(0)
    if not inner or inner.pop(0) != "yard":
        return None, None
    return inner, environment


def valid_yard_action(command, yard_name):
    if command[:2] == ["-Y", yard_name]:
        command = command[2:]
    if command == ["rpc", "--stdio"] or command == ["_authorize"]:
        return True
    if not command or command[0] != "_project-state" or len(command) < 2:
        return False
    minimum_lengths = {
        "preview": 6,
        "reserve": 7,
    }
    if command[1] in minimum_lengths:
        return len(command) >= minimum_lengths[command[1]]
    expected_lengths = {
        "check-role": 2,
        "finalize": 10,
        "abort": 3,
        "upsert": (6, 9),
        "remove": 4,
        "unregister": 3,
    }
    expected = expected_lengths.get(command[1])
    if isinstance(expected, tuple):
        return len(command) in expected
    return len(command) == expected


def forced_command(arguments):
    original = os.environ.get("SSH_ORIGINAL_COMMAND", "")
    try:
        direct = shlex.split(original)
    except ValueError:
        direct = []
    if (
        len(direct) == 6
        and direct[:2] == ["ssh-keyscan", "-T"]
        and direct[2].isdigit()
        and direct[3] == "-p"
        and direct[4] == str(arguments.yard_port)
        and direct[5] == "127.0.0.1"
    ):
        os.execv("/usr/bin/ssh-keyscan", direct)

    command, additions = parse_yard_command(original)
    if command is None or not valid_yard_action(command, arguments.yard_name):
        print("orca-projects-helper: rejected SSH fixture command", file=sys.stderr)
        return 126
    environment = {
        "HOME": arguments.home,
        "PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
        "SUBYARD_OPERATOR_HOME": arguments.home,
        "SUBYARD_CONFIG_HOME": arguments.config_home,
        "SUBYARD_HOME": arguments.data_home,
        "SUBYARD_NO_AUDIT": "1",
        "SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE": "1",
        "SUBYARD_REPOSITORY_ROOT": arguments.repository,
        "STORAGE_PATH": arguments.storage_path,
        "MIN_DISK_GIB": "1",
    }
    environment.update(additions)
    argv = [arguments.engine, *command]
    group = grp.getgrnam("incus-admin").gr_gid
    user = pwd.getpwuid(os.getuid()).pw_name
    if group not in [os.getegid(), *os.getgroups()] and group in os.getgrouplist(user, os.getgid()):
        os.execve("/usr/bin/sg", ["sg", "incus-admin", "-c", "exec " + shlex.join(argv)], environment)
    os.execve(arguments.engine, argv, environment)


def cleanup_terminal(arguments):
    """Exercise native PTY stops without editing stock session persistence."""
    metadata = "/srv/agents/orca/config/orca/orca-runtime.json"
    worktree = arguments.worktree.removeprefix("id:")
    call = lambda method, params=None: call_any_result(metadata, method, params)

    def inventory():
        result = call("terminal.list", {"limit": 1000, "requireFreshPtyLiveness": True,
                                        "includeVisualLayouts": False})
        rows = result["terminals"]
        scope = result["hostScope"]
        assert result["truncated"] is False and result["totalCount"] == len(rows)
        assert "local" in scope["hostIds"] and "local" not in scope["omittedHostIds"]
        return rows

    try:
        if arguments.phase == "create":
            created = call("session.tabs.createTerminal", {
                "worktree": arguments.worktree, "activate": False,
                "command": "printf 'cleanup-fixture\\n'; exec sleep 3600",
                "clientMutationId": "cleanup-" + str(uuid.uuid4()),
            })
            tab_id = created["tab"]["id"]
        else:
            tab_id = arguments.tab_id
        deadline = time.monotonic() + 30
        while True:
            snapshots = snapshot_tabs(call("session.tabs.listAll"))
            target_snapshots = [item for item in snapshots
                                if item["worktree"].startswith(worktree.split("::", 1)[0] + "::")]
            tabs = [tab for item in snapshots if item["worktree"] == worktree
                    for tab in item["tabs"] if tab["id"] == tab_id]
            rows = inventory()
            matching_pty = any(arguments.pty_id is not None and row["ptyId"] == arguments.pty_id
                               for row in rows)
            matching_worktree = any(row["worktreeId"].startswith(worktree.split("::", 1)[0] + "::")
                                    for row in rows)
            if arguments.phase == "create":
                if len(tabs) == 1 and tabs[0].get("status") == "ready":
                    pty_id = tabs[0]["ptyId"]
                    if any(row["ptyId"] == pty_id and row["worktreeId"] == worktree
                           and row["executionHostId"] == "local" and row["connected"] is True
                           for row in rows):
                        print(json.dumps({"tab_id": tab_id, "pty_id": pty_id}))
                        return 0
            elif arguments.phase == "pending":
                if (len(tabs) == 1 and tabs[0].get("type") == "terminal"
                        and tabs[0].get("status") == "pending-handle"
                        and "terminal" in tabs[0] and tabs[0]["terminal"] is None
                        and tabs[0].get("ptyId") == arguments.pty_id
                        and not any(row["ptyId"] == arguments.pty_id
                                    or row["worktreeId"].startswith(worktree.split("::", 1)[0] + "::")
                                    for row in rows)):
                    return 0
            elif arguments.phase == "absent":
                # Stock may retain an empty in-memory snapshot after native tab close.
                if (all(item["tabs"] == [] for item in target_snapshots)
                        and not matching_pty and not matching_worktree):
                    return 0
            if time.monotonic() >= deadline:
                print("orca-cleanup-fixture: " + json.dumps({
                    "phase": arguments.phase,
                    "target_snapshot_count": len(target_snapshots),
                    "target_nonempty_tab_count": sum(len(item["tabs"]) for item in target_snapshots),
                    "selected_tab_present": bool(tabs),
                    "matching_old_pty": matching_pty,
                    "matching_worktree": matching_worktree,
                }, sort_keys=True), file=sys.stderr)
                raise SafeRpcError("native terminal cleanup fixture did not reach " + arguments.phase)
            time.sleep(0.2)
    except SafeRpcError as error:
        print("orca-cleanup-fixture: " + str(error), file=sys.stderr)
    except (AssertionError, KeyError, TypeError, ValueError):
        print("orca-cleanup-fixture: invalid native terminal inventory", file=sys.stderr)
    return 1


def init_failure_diagnostics(arguments):
    """Report only fixture-safe categories; never copy product or RPC payloads."""
    diagnostics = {"registration_status_available": False, "terminal_census_available": False}
    try:
        hook = Path("/usr/local/libexec/subyard/projects-changed.d/orca").read_text()
        host_names = re.findall(r"--host-name '([a-zA-Z0-9._][a-zA-Z0-9._-]*)'", hook)
        assert len(host_names) == 1 and host_names[0] not in (".", "..")
        result = subprocess.run(
            ["python3", "-B", "/usr/local/libexec/subyard/orca-registration/main.py", "status",
             "--host-name", host_names[0]],
            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=90, check=False,
        )
        report = json.loads(result.stdout)
        errors = report["errors"]
        assert isinstance(errors, list) and all(isinstance(error, str) for error in errors)
        diagnostics.update(registration_status_available=True, registration_error_count=len(errors),
                           registration_warning_count=len(report["warnings"]),
                           registration_ready=report.get("ready") is True)
        for category, marker in {
            "terminal_census_rejected": "rejected terminal.list",
            "rpc_rejected": "runtime rejected",
            "timed_out": "timed out",
            "budget_exhausted": "budget exhausted",
            "inventory_incomplete": "inventory is incomplete",
            "scope_changed": "scope changed",
            "cleanup_changed": "cleanup candidate changed",
        }.items():
            diagnostics[category] = any(marker in error for error in errors)
    except (OSError, ValueError, KeyError, TypeError, AssertionError, subprocess.TimeoutExpired):
        pass
    diagnostics["launch_defaults_check_available"] = False
    try:
        result = subprocess.run(
            ["python3", "-B", "/usr/local/libexec/subyard/orca-registration/settings.py", "--check"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15, check=False,
        )
        diagnostics.update(launch_defaults_check_available=True,
                           launch_defaults_ready=result.returncode == 0)
    except (OSError, subprocess.TimeoutExpired):
        pass
    try:
        result = call_any_result("/srv/agents/orca/config/orca/orca-runtime.json", "terminal.list", {
            "limit": 1000, "requireFreshPtyLiveness": True, "includeVisualLayouts": False,
        })
        rows, scope = result["terminals"], result["hostScope"]
        assert isinstance(rows, list) and isinstance(scope, dict)
        diagnostics.update(terminal_census_available=True, terminal_count=len(rows),
                           terminal_census_complete=result.get("truncated") is False
                           and result.get("totalCount") == len(rows)
                           and "local" in scope.get("hostIds", [])
                           and "local" not in scope.get("omittedHostIds", []))
    except (SafeRpcError, KeyError, TypeError, AssertionError):
        pass
    for property_name in ("Result", "ExecMainStatus"):
        try:
            value = subprocess.run(
                ["systemctl", "show", "subyard-orca-discovery.service", "--value", "-p", property_name],
                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=5, check=True,
            ).stdout.decode().strip()
            if (property_name == "ExecMainStatus" and value.isdigit()) or value in {
                "success", "exit-code", "signal", "timeout", "resources", "core-dump", "oom-kill",
                "start-limit-hit", "protocol", "watchdog",
            }:
                diagnostics["discovery_" + property_name] = int(value) if value.isdigit() else value
        except (OSError, ValueError, subprocess.SubprocessError):
            pass
    try:
        expected = json.loads(arguments.expected_registration)
        diagnostics["registration_helpers_equal"] = all(
            hashlib.sha256((Path("/usr/local/libexec/subyard/orca-registration") / name).read_bytes()).hexdigest() == digest
            for name, digest in expected.items())
        diagnostics["dispatcher_equal"] = hashlib.sha256(
            Path("/usr/local/libexec/subyard/projects-changed").read_bytes()).hexdigest() == arguments.expected_dispatcher
    except (OSError, ValueError, TypeError):
        pass
    print("orca-init-diagnostics: " + json.dumps(diagnostics, sort_keys=True), file=sys.stderr)
    return 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)

    rpc = subparsers.add_parser("rpc")
    rpc.add_argument("method")
    rpc.add_argument("params", nargs="?")
    rpc.add_argument("--state", default="/srv/agents/orca")

    cleanup = subparsers.add_parser("cleanup-terminal")
    cleanup.add_argument("phase", choices=("create", "pending", "absent"))
    cleanup.add_argument("worktree")
    cleanup.add_argument("--tab-id")
    cleanup.add_argument("--pty-id")

    init_diagnostics = subparsers.add_parser("init-failure-diagnostics")
    init_diagnostics.add_argument("--expected-registration", required=True)
    init_diagnostics.add_argument("--expected-dispatcher", required=True)

    probe = subparsers.add_parser("stock-probe")
    probe.add_argument("--binary", default="/usr/bin/orca-ide")
    probe.add_argument("--version", required=True)
    probe.add_argument("--registration", default="/usr/local/libexec/subyard/orca-registration")
    probe.add_argument("--resource-settings", action="store_true",
                       help="check both resource isolation values through stock recovery and restart")

    isolation = subparsers.add_parser("set-isolation")
    isolation.add_argument("--binary", default="/usr/bin/orca-ide")
    isolation.add_argument("--state", default="/srv/agents/orca")
    isolation.add_argument("--value", choices=("false", "true"))
    isolation.add_argument("--expect", choices=("false", "true"))

    forced = subparsers.add_parser("forced-command")
    forced.add_argument("engine")
    forced.add_argument("repository")
    forced.add_argument("home")
    forced.add_argument("config_home")
    forced.add_argument("data_home")
    forced.add_argument("yard_name")
    forced.add_argument("yard_port", type=int)
    forced.add_argument("storage_path")

    arguments = parser.parse_args()
    if arguments.command == "rpc":
        return rpc_call(arguments)
    if arguments.command == "cleanup-terminal":
        return cleanup_terminal(arguments)
    if arguments.command == "init-failure-diagnostics":
        return init_failure_diagnostics(arguments)
    if arguments.command == "stock-probe":
        return stock_probe(arguments)
    if arguments.command == "set-isolation":
        if arguments.value is None and arguments.expect is None:
            parser.error("set-isolation requires --value or --expect")
        return set_isolation(arguments)
    return forced_command(arguments)


if __name__ == "__main__":
    raise SystemExit(main())
