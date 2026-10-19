#!/usr/bin/env python3
"""Bounded exact-path reset experiment; not a product noninferiority gate."""
import argparse
import importlib.util
import json
from pathlib import Path
import platform
import select
import subprocess
import tempfile
import time

SCRIPT = Path(__file__).resolve().parents[1] / "measure-release-owner.py"
spec = importlib.util.spec_from_file_location("owner_measure", SCRIPT)
measure = importlib.util.module_from_spec(spec)
spec.loader.exec_module(measure)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--release", type=Path, required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--baselines", type=Path, required=True)
    parser.add_argument("--observation-server", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--workload", choices=measure.WORKLOADS[1:], action="append")
    args = parser.parse_args()
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    arch = {"x86_64": "amd64", "aarch64": "arm64"}[platform.machine()]
    verify = measure.module("upgrade_fixture", measure.ROOT / "dev/verify-release-upgrades.py")
    plan = {"scope": "two-cycle reset plumbing proof; actual Rust pending producer and census required",
            "measure_sha256": measure.sha(SCRIPT), "driver_sha256": measure.sha(Path(__file__)),
            "package": measure.package_binding(args.release.resolve(), args.version, arch)}
    measure.write_json(output / "plan.json", plan)
    summaries = {}
    try:
        for workload in args.workload or measure.WORKLOADS[1:]:
            directory = output / workload
            directory.mkdir()
            setup = directory / "setup"
            setup.mkdir()
            with tempfile.TemporaryDirectory(prefix="subyard-owner-reset-", dir="/tmp") as temporary:
                root = Path(temporary)
                control = root / "incus-observation.json"
                control.write_text("{}")
                with subprocess.Popen([str(args.observation_server.resolve()), str(root), str(control)], stdout=subprocess.PIPE,
                                      stderr=subprocess.PIPE, text=True) as server:
                    try:
                        readable, _, _ = select.select([server.stdout], [], [], 10)
                        verify.require(readable and server.stdout.readline().strip() == "ready", "fixture server not ready")
                        started = time.monotonic()
                        fixture, commands = measure.prepare_arm(verify, args.release.resolve(), args.version,
                            args.baselines.resolve(), arch, workload, root, setup, require_rust_setup=True)
                        setup_seconds = time.monotonic() - started
                        state = measure.FixtureReset(fixture, root)
                        measure.write_json(directory / "initial-state.json", state.public(state.entries))
                        results = []
                        for cycle in range(2):
                            run = directory / f"cycle-{cycle}"
                            run.mkdir()
                            started = time.monotonic()
                            reset = state.restore()
                            reset["reset_seconds"] = time.monotonic() - started
                            measure.write_json(run / "reset.json", reset)
                            result = measure.run_prepared(verify, fixture, commands, workload, root, run, 0.005, False)
                            verify.require(result["rust_owner_observed"], "actual Rust process missing from reset cycle census")
                            results.append(result)
                            measure.write_json(run / "result.json", result)
                        verify.require(results[0]["semantics"] == results[1]["semantics"], "reset changed public semantic outcomes")
                        verify.require(results[0]["native_effects"] == results[1]["native_effects"], "reset changed synthetic native effects")
                        summaries[workload] = {"status": "PASS", "setup_seconds": setup_seconds,
                            "owner_command_wall_seconds": [result["wall_seconds"] for result in results],
                            "rust_owner_observed": [result["rust_owner_observed"] for result in results],
                            "scope": "fresh public inspection/resume accepted each reset; changed IDs recorded; Rust reset remains unproven when not observed"}
                        measure.write_json(output / "summary.json", summaries)
                        print(json.dumps({"workload": workload, **summaries[workload]}), flush=True)
                    finally:
                        server.terminate()
                        server.wait(timeout=10)
    except BaseException as error:
        measure.write_json(output / "failed.json", {"error": str(error), "completed": summaries})
        raise


if __name__ == "__main__":
    main()
