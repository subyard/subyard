#!/usr/bin/env python3
"""Run the host-free profile inventory selected by test-profiles.sh."""

from concurrent.futures import FIRST_COMPLETED, ThreadPoolExecutor, wait
from itertools import islice
import os
from pathlib import Path
import shutil
import subprocess
import sys


def main():
    log_dir = Path(sys.argv[1])
    count = int(sys.argv[2])
    runners = [Path(arg) for arg in sys.argv[3:3 + count]]
    args = sys.argv[3 + count:]

    def run(runner):
        name = runner.parent.parent.name
        log = log_dir / f"{name}.log"
        preflight = ["--after-permissions"] if runner.with_name("permissions.sh").is_file() else []
        print(f"PROFILE TESTS {name} starting", flush=True)
        with log.open("ab") as output:
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
    workers = 3
    remaining = iter(runners)
    pool = ThreadPoolExecutor(max_workers=workers)
    try:
        # Keep later profiles out of the executor queue so Ctrl-C stops dispatch.
        pending = {pool.submit(run, runner) for runner in islice(remaining, workers)}
        while pending:
            done, pending = wait(pending, return_when=FIRST_COMPLETED)
            for result in done:
                code = report(result.result())
                status = status or code
            pending.update(pool.submit(run, runner) for runner in islice(remaining, len(done)))
    except KeyboardInterrupt:
        return 130
    finally:
        pool.shutdown(cancel_futures=True)
    return status


if __name__ == "__main__":
    sys.exit(main())
