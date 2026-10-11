#!/usr/bin/env python3
"""Read a bounded JSONL delta; the caller accepts and persists the returned cursor."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import sys


def anchor(stream, offset):
    stream.seek(max(0, offset - 256))
    return hashlib.sha256(stream.read(min(offset, 256))).hexdigest()


def reject_constant(value):
    raise ValueError("non-JSON numeric constant")


def project(journal, cursor=None, max_bytes=65536, max_lines=100):
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
        stream.seek(offset)
        data = stream.read(min(max_bytes + 1, info.st_size - offset))
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
                events.append(event)
            consumed = end + 1
        next_offset = offset + consumed
        backlog = b"\n" in data[consumed:] or info.st_size > offset + len(data)
        current = Path(journal).stat()
        if [current.st_dev, current.st_ino] != identity or current.st_size < info.st_size:
            raise ValueError("journal replaced or truncated during read; cursor retained")
        return {"cursor": {"stream": identity, "offset": next_offset, "size": info.st_size,
                           "anchor": anchor(stream, next_offset)},
                "events": events, "backlog": backlog,
                "incomplete_bytes": 0 if backlog else info.st_size - next_offset}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("journal", type=Path)
    parser.add_argument("--cursor", help="previous accepted cursor JSON; omit only on first read")
    parser.add_argument("--max-bytes", type=int, default=65536)
    parser.add_argument("--max-lines", type=int, default=100)
    args = parser.parse_args()
    try:
        result = project(args.journal, json.loads(args.cursor) if args.cursor is not None else None,
                         args.max_bytes, args.max_lines)
    except (OSError, ValueError) as error:
        # Never print malformed source records or reset a rejected cursor.
        message = str(error) if isinstance(error, ValueError) else f"journal read error: {error.__class__.__name__}"
        print(json.dumps({"error": message}))
        return 1
    print(json.dumps(result, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    sys.exit(main())
