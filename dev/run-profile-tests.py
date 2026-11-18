#!/usr/bin/env python3
"""Run the host-free profile inventory selected by test-profiles.sh."""

from concurrent.futures import ThreadPoolExecutor, as_completed
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile


def main():
    count = int(sys.argv[1])
    runners = [Path(arg) for arg in sys.argv[2:2 + count]]
    args = sys.argv[2 + count:]
    if not runners:
        return 0

    root = Path(__file__).resolve().parent.parent
    logs = root / ".build/profile-test-runs"
    logs.mkdir(parents=True, exist_ok=True)
    log_dir = Path(tempfile.mkdtemp(prefix="run.", dir=logs))
    print(f"PROFILE TESTS logs: {log_dir}", flush=True)

    def run(runner):
        name = runner.parent.parent.name
        log = log_dir / f"{name}.log"
        preflight = ["--after-permissions"] if runner.with_name("permissions.sh").is_file() else []
        print(f"PROFILE TESTS {name} starting", flush=True)
        with log.open("wb") as output:
            code = subprocess.run(["bash", runner, *preflight, *args],
                                  stdout=output, stderr=subprocess.STDOUT, check=False).returncode
        return name, log, code if code >= 0 else 128 - code

    def report(result):
        name, log, code = result
        if code:
            print(f"PROFILE TESTS {name} failed (exit {code}): {log}", file=sys.stderr, flush=True)
            with log.open("rb") as output:
                shutil.copyfileobj(output, sys.stderr.buffer)
            sys.stderr.buffer.flush()
        else:
            print(f"PROFILE TESTS {name} passed", flush=True)
        return code

    if os.environ.get("SUBYARD_PROFILE_TESTS_SERIAL") == "1":
        for runner in runners:
            if code := report(run(runner)):
                return code
        return 0

    status = 0
    with ThreadPoolExecutor(max_workers=3) as pool:
        for result in as_completed([pool.submit(run, runner) for runner in runners]):
            code = report(result.result())
            status = status or code
    return status


if __name__ == "__main__":
    sys.exit(main())
