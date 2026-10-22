#!/usr/bin/env python3
"""Read a bounded JSONL delta; the caller accepts and persists the returned cursor."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import sys


def anchor(stream, offset):
    stream.seek(max(0, offset - 256))
    return hashlib.sha256(stream.read(min(offset, 256))).hexdigest()


def reject_constant(value):
    raise ValueError("non-JSON numeric constant")


def compact_event(event, offset):
    """Material fields only. Unknown payloads never cross the projection boundary."""
    kind = event["event"]
    quiet = {"heartbeat", "check-start", "check-reuse", "run-start", "environment"}
    if kind in quiet:
        return None
    fields = {
        "check-result": {"check", "status", "exit_code", "termination", "signal", "reason", "log", "log_sha256",
                         "duration_seconds", "capture_status", "cleanup_status", "last_completed_phase", "failure_class"},
        "run-result": {"invocation_id", "status", "state", "selected_status", "reproducible_status", "overall_status",
                       "result", "receipt_sha256", "capture_status"},
        "phase": {"check", "phase", "state"},
        "controller-phase": {"check", "phase", "state", "duration_seconds", "exit_code", "vm"},
        "lease-release": {"check", "state", "duration_seconds", "exit_code", "budget_seconds", "grace_seconds"},
        "capture-error": {"check", "status", "reason"},
        "observation-error": {"status", "reason"},
    }
    if kind not in fields:
        return {"event": "unknown-event", "type": kind if re.fullmatch(r"[a-zA-Z0-9_-]{1,64}", kind) else "unknown",
                "count": 1, "offset": offset}
    if kind == "phase" and event.get("state") != "failed":
        return None
    if kind in ("controller-phase", "lease-release") and not (
            event.get("exit_code") not in (None, 0) or "cleanup" in str(event.get("phase", ""))
            and event.get("state") == "end"):
        return None
    result = {"event": kind}
    for key in fields[kind] | {"time", "required_action"}:
        value = event.get(key)
        if isinstance(value, str):
            result[key] = value[:256]
        elif type(value) in (int, float) or value is None and key in event:
            result[key] = value
    if kind == "check-result" and isinstance(event.get("cleanup"), dict):
        result["cleanup"] = {name: {key: item[key] for key in ("exit_code", "duration_seconds")
                                          if type(item.get(key)) in (int, float)}
                             for name, item in list(event["cleanup"].items())[:32]
                             if re.fullmatch(r"[a-zA-Z0-9/_.-]{1,100}", name) and isinstance(item, dict)}
    return result


def project(journal, cursor=None, max_bytes=65536, max_lines=100, compact=False, until=None):
    if not 1 <= max_bytes <= 1048576 or not 1 <= max_lines <= 1000:
        raise ValueError("batch limits out of range")
    with Path(journal).open("rb") as stream:
        info = os.fstat(stream.fileno())
        identity = [info.st_dev, info.st_ino]
        offset = 0
        if cursor is not None:
            if (not isinstance(cursor, dict) or set(cursor) != {"stream", "offset", "size", "anchor"}
                    or not isinstance(cursor["stream"], list) or len(cursor["stream"]) != 2
                    or any(type(value) is not int or value < 0 for value in cursor["stream"])
                    or any(type(cursor[key]) is not int or cursor[key] < 0 for key in ("offset", "size"))
                    or cursor["offset"] > cursor["size"]
                    or not isinstance(cursor["anchor"], str)):
                raise ValueError("invalid cursor")
            offset = cursor["offset"]
            if identity != cursor["stream"]:
                raise ValueError("journal replaced; cursor retained")
            if info.st_size < cursor["size"] or anchor(stream, offset) != cursor["anchor"]:
                raise ValueError("journal truncated or processed bytes changed; cursor retained")
            if offset:
                stream.seek(offset - 1)
                if stream.read(1) != b"\n":
                    raise ValueError("cursor does not end at a newline")
        eof = info.st_size if until is None else until
        if type(eof) is not int or not offset <= eof <= info.st_size:
            raise ValueError("invalid snapshot EOF")
        stream.seek(offset)
        data = stream.read(min(max_bytes + 1, eof - offset))
        consumed = 0
        events = []
        for _ in range(max_lines):
            end = data.find(b"\n", consumed, max_bytes)
            if end < 0:
                if len(data) - consumed > max_bytes:
                    raise ValueError(f"JSONL line exceeds byte limit at offset {offset + consumed}")
                break
            try:
                event = json.loads(data[consumed:end], parse_constant=reject_constant)
            except (ValueError, UnicodeError, RecursionError):
                raise ValueError(f"malformed complete JSON at offset {offset + consumed}") from None
            if not isinstance(event, dict) or not isinstance(event.get("event"), str):
                raise ValueError(f"invalid event object at offset {offset + consumed}")
            if event["event"] != "heartbeat":
                material = compact_event(event, offset + consumed) if compact else event
                if material is not None:
                    events.append(material)
            consumed = end + 1
        next_offset = offset + consumed
        backlog = b"\n" in data[consumed:] or eof > offset + len(data)
        current = Path(journal).stat()
        if [current.st_dev, current.st_ino] != identity or current.st_size < info.st_size:
            raise ValueError("journal replaced or truncated during read; cursor retained")
        return {"cursor": {"stream": identity, "offset": next_offset, "size": info.st_size,
                           "anchor": anchor(stream, next_offset)},
                "events": events, "backlog": backlog, "eof": eof,
                "incomplete_bytes": 0 if backlog else eof - next_offset}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("journal", type=Path)
    parser.add_argument("--cursor", help="previous accepted cursor JSON; omit only on first read")
    parser.add_argument("--max-bytes", type=int, default=65536)
    parser.add_argument("--max-lines", type=int, default=100)
    parser.add_argument("--compact", action="store_true", help="explicit material whitelist; raw journal stays unchanged")
    parser.add_argument("--until", type=int, help="fixed EOF for this observation step")
    args = parser.parse_args()
    try:
        result = project(args.journal, json.loads(args.cursor) if args.cursor is not None else None,
                         args.max_bytes, args.max_lines, args.compact, args.until)
    except (OSError, ValueError) as error:
        # Never print malformed source records or reset a rejected cursor.
        message = str(error) if isinstance(error, ValueError) else f"journal read error: {error.__class__.__name__}"
        print(json.dumps({"error": message}))
        return 1
    print(json.dumps(result, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    sys.exit(main())
