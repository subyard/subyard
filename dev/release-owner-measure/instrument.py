#!/usr/bin/env python3
"""Instrument an explicitly isolated source snapshot, preserving source hashes."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess

HERE = Path(__file__).resolve().parent
ACTIVE = HERE.parents[1]


def digest(data):
    return hashlib.sha256(data).hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, required=True, help="isolated extracted source, never active checkout")
    parser.add_argument("--receipt", type=Path, required=True)
    args = parser.parse_args()
    source = args.source.resolve(strict=True)
    if source == ACTIVE or ACTIVE.is_relative_to(source) or (source / ".git").exists():
        parser.error("use an isolated extracted source snapshot without a Git repository")
    if args.receipt.exists():
        parser.error("receipt already exists")
    snapshot = source / "source-snapshot.json"
    snapshot_binding = digest(snapshot.read_bytes()) if snapshot.exists() else None
    go_path = source / "cmd/yard/main.go"
    rust_path = source / "release-owner/src/bin/subyard-release-owner.rs"
    go_original = go_path.read_bytes()
    rust_original = rust_path.read_bytes()
    go = go_original.decode()
    rust = rust_original.decode()
    if "measureStart" in go or "allocation_measure" in rust:
        parser.error("source already instrumented")
    if go.count("func main() {") != 1 or rust.count("fn main() {") != 1:
        parser.error("unknown source entrypoint")
    go = go.replace('import (', 'import (\n measureRuntime "runtime"', 1)
    go = go.replace("os.Exit(", "measureExit(")
    go = go.replace("func main() {", "func main() {\n measureStart()\n defer measureReport(true)", 1)
    # Snapshot native/codec helper frames even when session Close later SIGKILLs
    # them. Such snapshots remain explicitly non-final and cannot certify totals.
    go = go.replace("cli.RunReleaseOwnerNative(context.Background(), os.Stdin, os.Stdout)",
                    "cli.RunReleaseOwnerNative(context.Background(), os.Stdin, measureWriter{os.Stdout})")
    go = go.replace("releasetransition.RunLegacyCodec(os.Stdin, os.Stdout)",
                    "releasetransition.RunLegacyCodec(os.Stdin, measureWriter{os.Stdout})")
    go += "\n" + (HERE / "go-stats.txt").read_text()
    rust = rust.replace("std::process::exit(", "allocation_measure::exit(")
    rust = rust.replace("fn main() {", "fn measured_owner_main() {", 1)
    # Keep original inner crate attributes/doc comments ahead of inserted items.
    rust += "\n" + (HERE / "allocator.rs").read_text()
    rust += "\nfn main() { measured_owner_main(); allocation_measure::emit(true); }\n"
    # Capture cumulative owner allocations after each private response so normal
    # forced helper shutdown still leaves a clearly partial owning observation.
    send_start = rust.index("fn send(")
    send_end = rust.index("fn ", send_start + 3)
    send = rust[send_start:send_end]
    if "    Ok(())" not in send:
        parser.error("unknown owner send boundary")
    rust = rust[:send_start] + send.replace("    Ok(())", "    allocation_measure::emit(false);\n    Ok(())") + rust[send_end:]
    go_path.write_text(go)
    rust_path.write_text(rust)
    subprocess.run(["gofmt", "-w", str(go_path)], check=True)
    receipt = {"schema_version": 1, "scope": "developer-only instrumented snapshot; never product latency",
               "source_root": str(source), "source_snapshot_manifest_sha256": snapshot_binding, "inputs": [
                   {"path": str(path.relative_to(source)), "original_sha256": digest(original), "instrumented_sha256": digest(path.read_bytes())}
                   for path, original in ((go_path, go_original), (rust_path, rust_original))],
               "templates": {path.name: digest(path.read_bytes()) for path in (HERE / "allocator.rs", HERE / "go-stats.txt", Path(__file__))},
               "allocation_coverage": "only reporting processes; non-final helper snapshots are lower bounds; killed helpers make whole-tree totals inconclusive",
               "go_counts": "runtime MemStats including reporting overhead; preserve startup totals and main deltas",
               "rust_counts": "successful System allocation/reallocation calls and requested bytes; excludes this thread's reporting allocations; no retained-size or live-object claim"}
    args.receipt.parent.mkdir(parents=True, exist_ok=True)
    args.receipt.write_text(json.dumps(receipt, indent=2, sort_keys=True) + "\n")
    print(json.dumps({"status": "instrumented", "receipt": str(args.receipt)}))


if __name__ == "__main__":
    main()
