"""Small adapter for Orca 1.4.159's authenticated local runtime RPC.

The token is read afresh for each connection and never included in diagnostics.
There are no transport retries: a lost write response requires catalog recovery.
"""

import json
import os
from pathlib import Path
import socket
import stat
import time
import uuid


class RpcError(Exception):
    def __init__(self, message, unknown=False):
        super().__init__(message)
        self.unknown = unknown


class RuntimeRPC:
    def __init__(self, metadata_path, timeout=5.0, deadline=None):
        self.metadata_path = Path(metadata_path)
        self.timeout = timeout
        self.deadline = deadline
        self.runtime_id = None

    def call(self, method, params=None, before_send=None):
        write = method not in ("repo.list", "projectGroup.list", "settings.get")
        sent = False
        try:
            fd = os.open(self.metadata_path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
            with os.fdopen(fd, "rb") as source:
                if not stat.S_ISREG(os.fstat(source.fileno()).st_mode):
                    raise ValueError()
                raw = source.read(65537)
            if len(raw) > 65536:
                raise ValueError()
            metadata = json.loads(raw)
            transports = metadata.get("transports", [metadata.get("transport")])
            endpoint = next(t["endpoint"] for t in transports
                            if isinstance(t, dict) and t.get("kind") == "unix")
            token = metadata["authToken"]
            runtime_id = metadata["runtimeId"]
            if not all(isinstance(value, str) and value for value in (endpoint, token, runtime_id)):
                raise ValueError()
        except (OSError, ValueError, TypeError, KeyError, StopIteration, AttributeError, RecursionError):
            raise RpcError("Orca runtime metadata is unavailable or invalid") from None
        self.runtime_id = runtime_id
        request_id = str(uuid.uuid4())
        request = {"id": request_id, "authToken": token, "method": method}
        if params is not None:
            request["params"] = params
        deadline = min(time.monotonic() + self.timeout, self.deadline or float("inf"))
        try:
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise RpcError("Orca registration time budget exhausted")
                connection.settimeout(remaining)
                connection.connect(endpoint)
                if before_send is not None:
                    before_send(runtime_id)
                # sendall may write part or all of the frame before raising.
                sent = True
                connection.sendall(json.dumps(request).encode("utf-8") + b"\n")
                buffer = b""
                while True:
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        raise TimeoutError()
                    connection.settimeout(remaining)
                    chunk = connection.recv(65536)
                    if not chunk:
                        raise RpcError("Orca runtime disconnected before responding", unknown=write)
                    buffer += chunk
                    if len(buffer) > 8 * 1024 * 1024:
                        raise RpcError("Orca runtime response exceeds size limit", unknown=write)
                    while b"\n" in buffer:
                        line, buffer = buffer.split(b"\n", 1)
                        if not line.strip():
                            continue
                        try:
                            frame = json.loads(line)
                        except (ValueError, RecursionError):
                            raise RpcError("Orca runtime returned invalid JSON", unknown=write) from None
                        if frame == {"_keepalive": True}:
                            continue
                        if not isinstance(frame, dict) or frame.get("id") != request_id:
                            raise RpcError("Orca runtime response id mismatch", unknown=write)
                        meta = frame.get("_meta")
                        if not isinstance(meta, dict) or meta.get("runtimeId") != runtime_id:
                            raise RpcError("Orca runtime response runtime mismatch", unknown=write)
                        if frame.get("ok") is False and isinstance(frame.get("error"), dict):
                            # Do not echo upstream messages or codes: either can contain secrets.
                            raise RpcError("Orca runtime rejected " + method)
                        if frame.get("ok") is not True or not isinstance(frame.get("result"), dict):
                            raise RpcError("Orca runtime returned invalid envelope", unknown=write)
                        return frame["result"]
        except (TimeoutError, socket.timeout):
            raise RpcError("Orca runtime request timed out", unknown=write and sent) from None
        except OSError:
            raise RpcError("Orca runtime connection failed", unknown=write and sent) from None
