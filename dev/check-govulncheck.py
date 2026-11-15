#!/usr/bin/env python3
"""Apply the reviewed allowlist to govulncheck v1 JSON source/symbol findings."""

from datetime import date, datetime, timezone
import json
from pathlib import Path
import re
import sys


def read_findings(path):
    # govulncheck emits consecutive, pretty-printed JSON objects, not a JSON array.
    content = path.read_text()
    decoder = json.JSONDecoder()
    config = None
    findings = set()
    offset = 0
    while offset < len(content):
        if content[offset].isspace():
            offset += 1
            continue
        message, offset = decoder.raw_decode(content, offset)
        if not isinstance(message, dict):
            raise ValueError("expected a JSON message object")
        if config is None:
            config = message.get("config", {})
            if not isinstance(config, dict) or (config.get("protocol_version"), config.get("scan_mode"), config.get("scan_level")) != (
                "v1.0.0", "source", "symbol"
            ):
                raise ValueError("expected govulncheck v1 source/symbol configuration")
        if "finding" not in message:
            continue
        finding = message["finding"]
        if not isinstance(finding, dict) or not isinstance(finding.get("osv"), str):
            raise ValueError("invalid finding ID")
        trace = finding.get("trace")
        if not isinstance(trace, list) or not trace or not isinstance(trace[0], dict):
            raise ValueError("invalid finding trace")
        # A function in the first (vulnerable) frame distinguishes a symbol finding
        # from a package/module finding in a symbol-level scan.
        if trace[0].get("function"):
            findings.add(finding["osv"])
    if config is None:
        raise ValueError("empty govulncheck report")
    return findings


def read_allowlist(path, today, errors):
    entries = json.loads(path.read_text())
    if not isinstance(entries, list):
        raise ValueError("expected an array of allowlist entries")
    allowed = set()
    for index, entry in enumerate(entries, 1):
        if not isinstance(entry, dict):
            errors.append(f"allowlist entry {index}: expected an object")
            continue
        identifier = entry.get("id")
        if not isinstance(identifier, str) or not re.fullmatch(r"GO-\d{4}-\d+", identifier):
            errors.append(f"allowlist entry {index}: invalid ID")
            identifier = f"allowlist entry {index}"
        else:
            if identifier in allowed:
                errors.append(f"{identifier}: duplicate allowlist entry")
            allowed.add(identifier)
        if not isinstance(entry.get("reason"), str) or not entry["reason"].strip():
            errors.append(f"{identifier}: reason must be nonempty")
        review_by = entry.get("review_by")
        try:
            if not isinstance(review_by, str) or not re.fullmatch(r"\d{4}-\d{2}-\d{2}", review_by):
                raise ValueError()
            deadline = date.fromisoformat(review_by)
        except ValueError:
            errors.append(f"{identifier}: review_by must be an ISO date (YYYY-MM-DD)")
        else:
            if deadline < today:
                errors.append(f"{identifier}: allowlist review expired on {review_by}")
            if (deadline - today).days > 92:
                errors.append(f"{identifier}: review_by {review_by} is more than 92 days ahead")
    return allowed


def main():
    if len(sys.argv) != 3:
        print("usage: check-govulncheck.py REPORT ALLOWLIST", file=sys.stderr)
        return 2
    errors = []
    findings = allowed = None
    try:
        findings = read_findings(Path(sys.argv[1]))
    except (OSError, UnicodeError, ValueError) as error:
        errors.append(f"govulncheck report: {error}")
    try:
        allowed = read_allowlist(Path(sys.argv[2]), datetime.now(timezone.utc).date(), errors)
    except (OSError, UnicodeError, ValueError) as error:
        errors.append(f"allowlist: {error}")
    if findings is not None and allowed is not None:
        errors.extend(f"{identifier}: symbol finding is not allowlisted" for identifier in findings - allowed)
        for identifier in sorted(allowed - findings):
            print(f"WARN: {identifier}: allowlist entry has no symbol finding in this scan")
    for error in sorted(errors):
        print(f"FAIL: {error}")
    if errors:
        return 1
    print(f"PASS: {len(findings)} symbol finding IDs allowlisted; {len(allowed)} review dates current.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
