#!/usr/bin/env python3
"""Export strict hosted PASS evidence or separate bounded diagnostic snapshots."""
import argparse
import importlib.util
import hashlib
import math
from collections import Counter
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
DIAGNOSTIC_INPUT_BYTES = 128 << 20


SECRET_NAME = re.compile(r"TOKEN|SECRET|PASS|KEY|CREDENTIAL|AUTH|COOKIE|SESSION", re.I)
VALUE = r'''(?:\[redacted\]|"(?:\\.|[^"\\])*(?:"|$)|'(?:\\.|[^'\\])*(?:'|$)|[^\s,;}\]]+)'''
LABEL = re.compile(r'''(?<![\w/-])(?P<label>["']?(?:[\w-]*(?:token|secret|password|passphrase|key)|pass|passwd|auth|credentials?)["']?)'''
                   r"(?P<sep>[ \t]*(?::|=)[ \t]*|[ \t]+)(?P<value>" + VALUE + ")", re.I | re.S)
FLAGS = re.compile(r"(?P<label>--(?:password|token|secret|passphrase|api-key))"
                   r"(?P<sep>[ \t]*=[ \t]*|[ \t]+)(?P<value>" + VALUE + ")", re.I | re.S)
HEADERS = re.compile(r'''(?<![\w-])(?P<label>["']?(?:proxy-authorization|authorization|set-cookie|cookie)["']?)'''
                     r"(?P<sep>[ \t]*(?::|=)[ \t]*|[ \t]+)(?P<value>[^\n]*)", re.I)
BEARER = re.compile(r"(?P<label>\bBearer)(?P<sep>[ \t]+)(?P<value>" + VALUE + ")", re.I | re.S)


def normalize(text):
    text = re.sub(r"\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)", "", text)
    text = re.sub(r"(?:\x1b\[|\x9b)[0-?]*[ -/]*[@-~]", "", text)
    return re.sub(r"[^\x20-\x7e\n\t]", "", text)


def unquote(value):
    if value.startswith(('"', "'")):
        try:
            return json.loads(value)
        except ValueError:
            return re.sub(r'''\\(["'\\])''', r"\1", value[1:-1] if value.endswith(value[0]) else value[1:])
    return value


def path_value(value):
    return value.startswith(("/", "./", "../", "~/", "file://")) or bool(re.fullmatch(r"[\w.-]+(?:/[\w.@+ :-]+)+", value))


def labelled_secrets(text):
    normalized = normalize(text)
    for rule in (LABEL, FLAGS, HEADERS, BEARER):
        for match in rule.finditer(normalized):
            value = unquote(match["value"])
            if rule is LABEL and path_value(value):
                continue
            if not value.startswith("-----BEGIN") or "\n" in value:
                yield value
            if rule is HEADERS and "authorization" in match["label"].lower():
                fields = value.split(None, 1)
                if len(fields) == 2:
                    yield fields[1]
    try:
        document = json.loads(text)
    except ValueError:
        return
    pending = [document]
    while pending:
        value = pending.pop()
        if isinstance(value, dict):
            for key, item in value.items():
                if isinstance(item, str) and SECRET_NAME.search(key) and not path_value(item):
                    yield item
                elif isinstance(item, (dict, list)):
                    pending.append(item)
        elif isinstance(value, list):
            pending.extend(value)


def secret_values(texts=()):
    values = {value for key, value in os.environ.items() if SECRET_NAME.search(key) and len(value) >= 8}
    # Broker contexts may be JSON in an environment variable or in a captured
    # record. Inspect only in-process values and allowlisted snapshot inputs.
    for text in (*os.environ.values(), *texts):
        values.update(value for value in labelled_secrets(text) if len(value) >= 8)
        if len(values) > 4096 or sum(map(len, values)) > 1 << 20:
            raise ValueError("diagnostic secret masking table exceeds bounds")
    return values


def sanitize(text, secrets=None):
    if secrets is None:
        secrets = secret_values((text,))
    if secrets:
        exact = re.compile("|".join(map(re.escape, sorted(secrets, key=lambda value: (-len(value), value)))))
        text = exact.sub("[redacted]", text)
    text = normalize(text)
    # Whole-input PEM handling precedes any tail cut. Also suppress orphaned
    # base64 body blocks or the final body line next to an END marker.
    text = re.sub(r"-----BEGIN ([A-Z0-9 ]{1,60})-----.*?(?:-----END \1-----|$)", "[redacted PEM]", text, flags=re.S)
    text = re.sub(r"(?m)(?:^(?![a-fA-F0-9]+$)[A-Za-z0-9+/]{48,76}={0,2}[ \t]*(?:\n|$)){2,}"
                  r"(?:^[A-Za-z0-9+/]{1,76}={0,2}[ \t]*(?:\n|$))?", "[redacted PEM body]\n", text)
    text = re.sub(r"(?m)^[A-Za-z0-9+/]{1,76}={0,2}\n(?=-----END [A-Z0-9 ]+-----)", "[redacted PEM body]\n", text)
    def mask_label(match):
        if path_value(unquote(match["value"])):
            return match[0]
        quote = match["value"][0] if match["value"].startswith(('"', "'")) else ""
        return match["label"] + match["sep"] + quote + "[redacted]" + quote
    text = HEADERS.sub(lambda match: match["label"] + match["sep"] + '"[redacted]"', text)
    for rule in (FLAGS, BEARER):
        text = rule.sub(lambda match: match["label"] + match["sep"] + '"[redacted]"', text)
    text = LABEL.sub(mask_label, text)
    text = re.sub(r"(\b[A-Za-z][A-Za-z0-9+.-]*://[^/\s:@]+:)[^@\s/]*(@)", r"\1[redacted]\2", text)
    text = re.sub(r"\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|sk-[A-Za-z0-9_-]{20,})\b", "[redacted]", text)
    text = re.sub(r"\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b", "[redacted]", text)
    def hex_tag(match):
        value = match[0]
        return f"[hex{len(value)}:{hashlib.sha256(value.lower().encode()).hexdigest()[:8]}]"
    text = re.sub(r"(?<![a-fA-F0-9])[a-fA-F0-9]{32,}(?![a-fA-F0-9])", hex_tag, text)
    def entropy_mask(match):
        value = match[0]
        entropy = -sum((count / len(value)) * math.log2(count / len(value)) for count in Counter(value).values())
        return "[redacted]" if entropy >= 4 else value
    return re.sub(r"(?<![\w./-])[A-Za-z0-9_+-]{32,}={0,2}(?![\w./=-])", entropy_mask, text)

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
    # Read whole bounded inputs first, so PEM/quoted-value context is never lost
    # at a tail boundary and broker secrets in one record mask other input logs.
    inputs, seen = [], set()
    input_remaining = DIAGNOSTIC_INPUT_BYTES
    summary["input_limit_bytes"] = DIAGNOSTIC_INPUT_BYTES
    for source, pattern, folder in sources:
        for path in sorted(source.glob(pattern)):
            relative = folder / path.relative_to(source)
            if relative in seen:
                continue
            seen.add(relative)
            if len(inputs) >= DIAGNOSTIC_FILES:
                summary["omitted"] += 1
                continue
            try:
                if any(part.is_symlink() for part in (path, *path.parents)) or not path.is_file():
                    summary["omitted"] += 1
                    continue
                with path.open("rb") as stream:
                    data = stream.read(input_remaining + 1)
                if len(data) > input_remaining:
                    summary["omitted"] += 1
                    continue
                input_remaining -= len(data)
                inputs.append((relative, path.name, folder, data.decode("utf-8", errors="replace")))
            except OSError:
                summary["omitted"] += 1
    secrets = secret_values(item[3] for item in inputs)
    remaining = DIAGNOSTIC_TOTAL_BYTES
    cleanup, tails = [], []
    failed_probes, probe_tails = [], {}
    for relative, name, folder, content in inputs:
        if remaining <= 0:
            summary["omitted"] += 1
            continue
        sanitized = sanitize(content, secrets).encode()
        limit = min(remaining, DIAGNOSTIC_FILE_BYTES)
        data = sanitized[-limit:]
        target = destination / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(data)
        remaining -= len(data)
        summary["files"].append({"path": str(relative), "truncated": len(sanitized) > limit})
        text = data.decode()
        if folder == Path("runner") and name == "results.tsv":
            failed_probes = [re.sub(r"[^a-zA-Z0-9_-]", "-", fields[0]) for line in text.splitlines()
                             if len(fields := line.split("\t")) >= 2 and fields[1].startswith("fail(")]
        if relative.suffix == ".log":
            lines = text.splitlines()
            cleanup.extend(line[:512] for line in lines if "cleanup" in line.lower() or "lease released:" in line)
            if name.startswith(prefix) or name == "check.log":
                tails.extend(lines[-80:])
            if folder == Path("runner"):
                probe_tails[relative.stem] = lines[-80:]
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
