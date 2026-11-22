#!/usr/bin/env python3
"""Run the host-free profile inventory selected by test-profiles.sh."""

from concurrent.futures import FIRST_COMPLETED, ThreadPoolExecutor, wait
from itertools import islice
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
from threading import Lock


def main():
    log_dir = Path(sys.argv[1])
    count = int(sys.argv[2])
    runners = [Path(arg) for arg in sys.argv[3:3 + count]]
    args = sys.argv[3 + count:]
    stopped = []
    launch_lock = Lock()

    def stop(signum, _frame):
        # Include foreground children without changing the caller's process group.
        # Mark stopped first so our re-delivered signal cannot broadcast again.
        if not stopped:
            stopped.append(signum)
            # Finish any in-flight launch before signalling; no runner may start later.
            with launch_lock:
                os.killpg(os.getpgrp(), signum)

    # Bash background children inherit SIGINT ignored; restore cancellation.
    signal.signal(signal.SIGINT, stop)
    signal.signal(signal.SIGTERM, stop)

    def run(runner):
        name = runner.parent.parent.name
        log = log_dir / f"{name}.log"
        preflight = ["--after-permissions"] if runner.with_name("permissions.sh").is_file() else []
        with log.open("ab") as output:
            with launch_lock:
                if stopped:
                    return name, log, 128 + stopped[0]
                print(f"PROFILE TESTS {name} starting", flush=True)
                # Inherit the caller's process group and the run-lock descriptors.
                process = subprocess.Popen(["bash", runner, *preflight, *args], stdout=output,
                                           stderr=subprocess.STDOUT, close_fds=False)
            code = process.wait()
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
        # Launch on a worker here too: the main-thread handler can wait for launch_lock.
        with ThreadPoolExecutor(max_workers=1) as pool:
            for runner in runners:
                result = pool.submit(run, runner).result()
                if stopped:
                    return 128 + stopped[0]
                if code := report(result):
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
            if stopped:
                continue  # Drain the interrupted runners; start and report nothing more.
            for result in done:
                code = report(result.result())
                status = status or code
            pending.update(pool.submit(run, runner) for runner in islice(remaining, len(done)))
    finally:
        pool.shutdown(cancel_futures=True)
    return 128 + stopped[0] if stopped else status


if __name__ == "__main__":
    sys.exit(main())
