#!/usr/bin/env python3
"""Export strict hosted PASS evidence or separate bounded diagnostic snapshots."""
import argparse
import importlib.util
import hashlib
import tempfile
import json
import os
import platform
import re
from pathlib import Path
import shutil

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("acceptance", ROOT / "dev/release-acceptance.py")
a = importlib.util.module_from_spec(spec)
spec.loader.exec_module(a)


def export(output, destination, check):
    # The native aggregate may remain incomplete because other checks were unrequested.
    # Preserve that receipt; job success follows only the terminal selected invocation.
    root, receipt = a.load_bound(output)
    result = receipt["checks"][check]
    record, manifest = a.original_attempt(output, receipt, check, result)
    a.validate_attempt(record, manifest, check, a.command_identity(a.commands(root, receipt["version"], receipt["profiles"])[check], root))
    invocations = [json.loads(path.read_text()) for path in output.glob("runs/*.json")]
    if (len(invocations) != 1 or invocations[0].get("state") != "finished"
        or invocations[0].get("capture_status") != "recorded"
        or invocations[0].get("selected") != [check] or invocations[0].get("selected_status") != "passed"
        or result["status"] != "passed" or result.get("cleanup_status") != "recorded"
        or not result.get("cleanup")):
        raise ValueError("hosted collector selection did not finish with a complete PASS and cleanup")

    destination.mkdir(parents=True, exist_ok=False)
    execution = {"run_id": int(os.environ["GITHUB_RUN_ID"]), "run_attempt": int(os.environ["GITHUB_RUN_ATTEMPT"]),
                 "head_sha": os.environ["GITHUB_SHA"], "check": check}
    (destination / "ci-execution.json").write_text(json.dumps(execution))
    total = 0
    for pattern in ("receipt.json", "events.jsonl", "*.log", "attempts/*.json", "manifests/*.json", "runs/*.json"):
        for path in sorted(output.glob(pattern)):
            size = path.stat().st_size
            if path.is_symlink() or not path.is_file() or size > 20 << 20:
                raise ValueError("hosted evidence is not a bounded regular file")
            total += size
            if total > 128 << 20:
                raise ValueError("hosted evidence exceeds total byte limit")
            target = destination / path.relative_to(output)
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(path, target)


# Diagnostic snapshots are deliberately ineligible for evidence import. Keep tails rather
# than refusing oversized logs, so a verbose failure cannot erase the useful last lines.
DIAGNOSTIC_FILE_BYTES = 2 << 20
DIAGNOSTIC_TOTAL_BYTES = 16 << 20
DIAGNOSTIC_FILES = 128


SECRET_NAME = re.compile(r"(?<![A-Z0-9])(?:TOKEN|SECRET|PASSWORD|PASSWD|PASSPHRASE|CREDENTIALS?|COOKIE|API_KEY|PRIVATE_KEY|ACCESS_KEY|SIGNING_KEY)(?![A-Z0-9])", re.I)
SECRET_PATH = re.compile(r"_(?:FILE|PATH|DIR|SOCK)$", re.I)
BEGIN_KEY = re.compile(rb"-----BEGIN (?P<kind>[A-Z0-9 ]*PRIVATE KEY)-----")
TOKEN = re.compile(rb"gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{20,}|eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+")
HEX = re.compile(rb"[a-fA-F0-9]{64,}")


class Redactor:
    """The four diagnostic rules; no values are learned from input text."""

    def __init__(self, environ=None):
        environ = os.environ if environ is None else environ
        values = {os.fsencode(value) for name, value in environ.items()
                  if SECRET_NAME.search(name) and not SECRET_PATH.search(name) and len(value) >= 8}
        self.exact = re.compile(b"|".join(re.escape(value) for value in sorted(values, key=lambda value: (-len(value), value)))) if values else None
        self.overlap = max(map(len, values), default=1) - 1

    def lines(self, stream):
        # A bounded overlap lets literal env values span reads and newlines.
        if self.exact is None:
            yield from stream
            return
        carry, pending = b"", b""
        while True:
            chunk = stream.read(65536)
            carry += chunk
            cut = max(0, len(carry) - self.overlap) if chunk else len(carry)
            for match in self.exact.finditer(carry):
                if match.start() < cut < match.end():
                    cut = match.start()
                    break
            pending += self.exact.sub(b"[redacted]", carry[:cut])
            carry = carry[cut:]
            parts = pending.split(b"\n")
            pending = parts.pop()
            for line in parts:
                yield line + b"\n"
            if not chunk:
                if pending:
                    yield pending
                return

    @staticmethod
    def plain(line):
        line = TOKEN.sub(b"[redacted]", line)
        def tag(match):
            value = match[0]
            return f"[hex{len(value)}:{hashlib.sha256(value).hexdigest()[:8]}]".encode()
        return HEX.sub(tag, line)

    def write(self, stream, target):
        # Detect PEM on original bytes so an env value cannot alter its delimiters.
        with tempfile.TemporaryFile() as pem:
            self.write_pem(stream, pem)
            pem.seek(0)
            for line in self.lines(pem):
                target.write(self.plain(line))

    def write_pem(self, stream, target):
        # Spool provisional output privately on disk. A matching END rolls back
        # even a key larger than the tail window. Without END, only BEGIN and
        # 128 following lines are suppressed; later diagnostic text survives.
        keys = []
        for line in stream:
            while line:
                endings = [(line.find(key[0]), index) for index, key in enumerate(keys) if key[0] in line]
                if endings:
                    offset, index = min(endings)
                    end, checkpoint, _ = keys[index]
                    target.seek(checkpoint)
                    target.truncate()
                    keys = keys[:index]
                    line = line[offset + len(end):]
                    continue
                if keys and keys[-1][2]:
                    keys[-1][2] -= 1
                    break
                begin = BEGIN_KEY.search(line)
                if begin:
                    target.write(line[:begin.start()])
                    target.write(b"[redacted PEM]")
                    checkpoint = target.tell()
                    end = b"-----END " + begin["kind"] + b"-----"
                    keys.append([end, checkpoint, 128])
                    rest = line[begin.end():]
                    if end in rest:
                        line = rest
                        continue
                    target.write(b"\r\n" if line.endswith(b"\r\n") else b"\n" if line.endswith(b"\n") else b"")
                    break
                target.write(line)
                break


def capture_tail(stream, limit, redactor):
    with tempfile.TemporaryFile() as spool:
        redactor.write(stream, spool)
        size = spool.tell()
        spool.seek(max(0, size - limit))
        return spool.read(limit), size > limit


def diagnostic_snapshot(output, destination, check, status, logs=None, evidence=None):
    destination.mkdir(parents=True, exist_ok=False)
    summary = {"kind": "diagnostics-only", "check": check, "job_status": status,
               "execution": {key: os.environ.get(key) for key in
                             ("GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT", "GITHUB_SHA")},
               "environment": {"system": platform.system(), "machine": platform.machine(),
                               "kernel": platform.release()}, "files": [], "omitted": 0, "skipped": []}
    redactor = Redactor()
    remaining, print_bytes, print_lines = DIAGNOSTIC_TOTAL_BYTES, 16384, 80
    seen, cleanup = set(), []
    prefix = check.replace(":", "-").replace("/", "-") + "-"

    def skip(path, reason):
        summary["omitted"] += 1
        summary["skipped"].append({"path": str(path), "reason": reason})

    def capture(path, source, folder, primary=False):
        nonlocal remaining, print_bytes, print_lines
        relative = folder / path.relative_to(source)
        if relative in seen:
            return
        seen.add(relative)
        try:
            if len(summary["files"]) >= DIAGNOSTIC_FILES:
                skip(relative, "file limit")
                return
            if remaining <= 0:
                skip(relative, "total byte limit")
                return
            if any(part.is_symlink() for part in (path, *path.parents)) or not path.is_file():
                skip(relative, "not a regular file or symlink path")
                return
            with path.open("rb") as stream:
                data, truncated = capture_tail(stream, min(remaining, DIAGNOSTIC_FILE_BYTES), redactor)
            target = destination / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(data)
            remaining -= len(data)
            summary["files"].append({"path": str(relative), "truncated": truncated})
            if primary and status != "success":
                print(f"Hosted check tail: {relative} (sanitized; at most 80 lines / 16 KiB)", flush=True)
                tail = []
                for line in reversed(data.splitlines()[-80:]):
                    rendered = (b"| " + line[:512]).decode("utf-8", errors="backslashreplace")
                    size = len(rendered.encode("utf-8")) + 1
                    if not print_lines or size > print_bytes:
                        break
                    tail.append(rendered)
                    print_bytes -= size
                    print_lines -= 1
                for line in reversed(tail):
                    print(line, flush=True)
            if relative.suffix == ".log":
                for line in data.splitlines():
                    if b"cleanup" in line.lower() or b"lease released:" in line:
                        cleanup.append(line[:512])
                        cleanup[:] = cleanup[-200:]
            return bool(data)
        except Exception as error:
            skip(relative, type(error).__name__ + (f" errno={error.errno}" if isinstance(error, OSError) else ""))

    try:
        # Nothing except failed-probe selection is opened before the check log.
        primary = [(path, output, Path("acceptance")) for path in sorted(output.glob(prefix + "*.log"))]
        if logs is not None and (logs / "check.log").exists():
            primary.append((logs / "check.log", logs, Path("runner")))
        captured_check = False
        for path, source, folder in primary:
            captured_check = capture(path, source, folder, primary=True) or captured_check
        if not captured_check and logs is not None:
            try:
                with (logs / "results.tsv").open("rb") as stream:
                    for line in redactor.lines(stream):
                        fields = line.split(b"\t")
                        if len(fields) >= 2 and fields[1].startswith(b"fail("):
                            name = re.sub(rb"[^a-zA-Z0-9_-]", b"-", fields[0]).decode()
                            capture(logs / (name + ".log"), logs, Path("runner"), primary=True)
            except OSError as error:
                skip("runner/results.tsv", type(error).__name__ + f" errno={error.errno}")
        sources = [(output, pattern, Path("acceptance")) for pattern in
                   ("runs/*.json", "receipt.json", "attempts/*.json", "events.jsonl")]
        if evidence is not None:
            sources += [(evidence, "**/evidence/p0-*.failure.log", Path("p0"))]
        if logs is not None:
            sources += [(logs, pattern, Path("runner")) for pattern in ("*.json", "results.tsv", "*.log")]
        sources += [(output, "*.log", Path("acceptance"))]
        for source, pattern, folder in sources:
            try:
                for path in sorted(source.glob(pattern)):
                    capture(path, source, folder)
            except OSError as error:
                skip(folder / pattern, type(error).__name__ + f" errno={error.errno}")
    except Exception as error:
        skip("snapshot", type(error).__name__)
    finally:
        try:
            (destination / "cleanup.log").write_bytes(b"\n".join(cleanup) + b"\n" if cleanup else
                b"No cleanup lines captured; cleanup is unconfirmed. See the check log and receipt.\n")
        except OSError as error:
            skip("cleanup.log", type(error).__name__ + f" errno={error.errno}")
        (destination / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")

if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--destination", type=Path, required=True)
    parser.add_argument("--check", required=True)
    parser.add_argument("--diagnostics", action="store_true", help="snapshot diagnostics without certifying PASS")
    parser.add_argument("--status", choices=("success", "failure", "cancelled"), default="failure")
    parser.add_argument("--logs", type=Path, help="fixed runner probe/log directory for diagnostics")
    parser.add_argument("--evidence", type=Path, help="runner E2E state root containing P0 failure logs")
    args = parser.parse_args()
    if args.diagnostics:
        diagnostic_snapshot(args.output.resolve(), args.destination.resolve(), args.check, args.status, args.logs, args.evidence)
    else:
        if args.check not in a.CI_HOSTED_CHECKS:
            parser.error("unknown hosted evidence check")
        export(args.output.resolve(), args.destination.resolve(), args.check)
