#!/usr/bin/env python3
"""Export strict hosted PASS evidence or separate bounded diagnostic snapshots."""
import argparse
import importlib.util
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


def sanitize(text):
    # Normalize terminal decoration before recognizing credentials. Quoted values
    # consume escapes as pairs, including embedded JSON quotes and backslashes.
    text = re.sub(r"\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)", "", text)
    text = re.sub(r"(?:\x1b\[|\x9b)[0-?]*[ -/]*[@-~]", "", text)
    text = re.sub(r"[^\x20-\x7e\n\t]", "", text)
    quoted_value = r'''(?:"(?:\\[^\n]|[^"\\\n])*(?:"|$)|'(?:\\[^\n]|[^'\\\n])*(?:'|$)|[^\s,}]+)'''
    text = re.sub(r"-----BEGIN [^-]*PRIVATE KEY-----.*?(?:-----END [^-]*PRIVATE KEY-----|$)",
                  "[redacted private key]", text, flags=re.S)
    text = re.sub(r'(?i)(["\']?(?:token|password|secret|authorization|credential|api[_-]?key)[\w-]*["\']?\s*[:=]\s*)'
                  r'(?:(?:Bearer|Basic)\s+)?' + quoted_value,
                  r'\1"[redacted]"', text, flags=re.M)
    text = re.sub(r"\b([A-Z_][A-Z0-9_]*=)" + quoted_value, r'\1"[redacted]"', text, flags=re.M)
    text = re.sub(r"(?i)(https?://)[^/\s:@]+:[^/\s@]+@", r"\1[redacted]@", text)
    text = re.sub(r"\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|sk-[A-Za-z0-9_-]{20,})\b",
                  "[redacted]", text)
    text = re.sub(r"\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b", "[redacted]", text)
    # Keep paths, filenames and key= prefixes. Unlabelled material needs a token
    # shape: hex, SSH key prefix, or base64 material rather than path separators.
    boundary = r"(?<![\w./-])"
    end = r"(?![\w./-])"
    text = re.sub(boundary + r"[a-fA-F0-9]{32,}" + end, "[redacted]", text)
    text = re.sub(boundary + r"AAAA[BC][A-Za-z0-9+/]{30,}={0,2}" + end, "[redacted]", text)
    text = re.sub(boundary + r"(?=[A-Za-z0-9+/]{40,}={0,2}" + end + r")"
                  r"(?=[A-Za-z0-9/]*[+=])[A-Za-z0-9+/]+={0,2}" + end, "[redacted]", text)
    text = re.sub(boundary + r"(?=[A-Za-z0-9]*[A-Z])(?=[A-Za-z0-9]*[a-z])(?=[A-Za-z0-9]*[0-9])"
                  r"(?:[A-Za-z0-9]{4}){10,}(?![\w./=-])", "[redacted]", text)
    return text


def diagnostic_snapshot(output, destination, check, status, logs=None, evidence=None):
    destination.mkdir(parents=True, exist_ok=False)
    summary = {"kind": "diagnostics-only", "check": check, "job_status": status,
               "execution": {key: os.environ.get(key) for key in
                             ("GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT", "GITHUB_SHA")},
               "environment": {"system": platform.system(), "machine": platform.machine(),
                               "kernel": platform.release()}, "files": [], "omitted": 0}
    prefix = check.replace(":", "-").replace("/", "-") + "-"
    sources = [(output, pattern, Path("acceptance")) for pattern in
               ("receipt.json", "events.jsonl", "attempts/*.json", "runs/*.json", prefix + "*.log")]
    if evidence is not None:
        # P0's failure reporter owns these files (keeper, capacity, OOM and broker
        # snapshots). Use the same bounds/redaction; never sweep other evidence.
        sources += [(evidence, "**/evidence/p0-*.failure.log", Path("p0"))]
    if logs is not None:
        sources += [(logs, pattern, Path("runner")) for pattern in ("check.log", "results.tsv", "*.log", "*.json")]
    sources += [(output, "*.log", Path("acceptance"))]
    remaining = DIAGNOSTIC_TOTAL_BYTES
    cleanup, tails, seen = [], [], set()
    failed_probes, probe_tails = [], {}
    for source, pattern, folder in sources:
        for path in sorted(source.glob(pattern)):
            relative = folder / path.relative_to(source)
            if relative in seen:
                continue
            seen.add(relative)
            if len(summary["files"]) >= DIAGNOSTIC_FILES or remaining <= 0:
                summary["omitted"] += 1
                continue
            try:
                # Never follow directory aliases into source, assets or private files.
                if any(part.is_symlink() for part in (path, *path.parents)) or not path.is_file():
                    summary["omitted"] += 1
                    continue
                size = path.stat().st_size
                limit = min(remaining, DIAGNOSTIC_FILE_BYTES)
                with path.open("rb") as stream:
                    stream.seek(max(0, size - limit))
                    data = stream.read(limit)
                if size > limit:
                    data = data.partition(b"\n")[2]  # discard a potentially partial secret/line
                text = sanitize(data.decode("utf-8", errors="replace"))
                data = text.encode()[-limit:]
                target = destination / relative
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_bytes(data)
                remaining -= len(data)
                summary["files"].append({"path": str(relative), "truncated": size > limit})
                if folder == Path("runner") and path.name == "results.tsv":
                    failed_probes = [re.sub(r"[^a-zA-Z0-9_-]", "-", fields[0]) for line in text.splitlines()
                                     if len(fields := line.split("\t")) >= 2 and fields[1].startswith("fail(")]
                if path.suffix == ".log":
                    lines = text.splitlines()
                    cleanup.extend(line[:512] for line in lines if "cleanup" in line.lower() or "lease released:" in line)
                    if path.name.startswith(prefix) or path.name == "check.log":
                        tails.extend(lines[-80:])
                    if folder == Path("runner"):
                        probe_tails[path.stem] = lines[-80:]
            except OSError:
                summary["omitted"] += 1
    (destination / "cleanup.log").write_text("\n".join(cleanup[-200:]) + "\n" if cleanup else
        "No cleanup lines captured; cleanup is unconfirmed. See the check log and receipt.\n")
    (destination / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    if status != "success":
        if not tails:
            for probe in failed_probes:
                tails.extend([f"runner/{probe}.log", *probe_tails.get(probe, [])])
        print("Failed hosted check log tail (sanitized; at most 80 lines / 16 KiB):")
        # Prefix every line so captured workflow commands cannot execute in Actions.
        budget = 16384
        for line in tails[-80:]:
            rendered = "| " + line[:512]
            if len(rendered) + 1 > budget:
                break
            print(rendered)
            budget -= len(rendered) + 1


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
