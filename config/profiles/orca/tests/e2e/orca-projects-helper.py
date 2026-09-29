#!/usr/bin/env python3
"""Sanitized stock-Orca RPC client and forced-command fixture for orca-projects.sh."""

import argparse
import json
import os
from pathlib import Path
import re
import shlex
import socket
import stat
import sys
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
    os.execve(arguments.engine, [arguments.engine, *command], environment)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)

    rpc = subparsers.add_parser("rpc")
    rpc.add_argument("method")
    rpc.add_argument("params", nargs="?")
    rpc.add_argument("--state", default="/srv/agents/orca")

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
    return forced_command(arguments)


if __name__ == "__main__":
    raise SystemExit(main())
