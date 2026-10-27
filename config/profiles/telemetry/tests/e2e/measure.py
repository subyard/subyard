#!/usr/bin/env python3
"""Compare two real collector binaries on a leased disposable VM.

The load driver runs outside the measured process. CPU percentages use one core;
RSS, kernel HWM, logical writes and block writes retain separate semantics.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import secrets
import signal
import socket
import statistics
import subprocess
import tempfile
import time


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def snapshot(pid, directory):
    proc = Path("/proc") / str(pid)
    stat = (proc / "stat").read_text().rsplit(")", 1)[1].split()
    values = {}
    for line in (proc / "status").read_text().splitlines():
        key, _, value = line.partition(":")
        if key in ("VmRSS", "VmHWM"):
            values[key] = int(value.split()[0]) * 1024
    try:
        io = dict(line.split(":", 1) for line in (proc / "io").read_text().splitlines())
    except PermissionError:
        io = {}
    files = [directory / name for name in ("telemetry.sqlite", "telemetry.sqlite-wal", "telemetry.sqlite-shm")]
    # Include the actual database basename without treating application outbox as DB.
    files = list({*files, *directory.glob("*.db"), *directory.glob("*.db-wal"), *directory.glob("*.db-shm")})
    apparent = allocated = 0
    for path in files:
        try:
            info = path.stat()
        except FileNotFoundError:
            continue
        apparent += info.st_size
        allocated += info.st_blocks * 512
    ticks = os.sysconf("SC_CLK_TCK")
    return {
        "monotonic_ns": time.monotonic_ns(), "start_ticks": int(stat[19]),
        "cpu_user_seconds": int(stat[11]) / ticks,
        "cpu_system_seconds": int(stat[12]) / ticks,
        "rss_bytes": values.get("VmRSS"), "kernel_process_hwm_bytes": values.get("VmHWM"),
        "block_write_bytes": int(io["write_bytes"]) if "write_bytes" in io else None,
        "logical_wchar": int(io["wchar"]) if "wchar" in io else None,
        "db_wal_shm_apparent_bytes": apparent, "db_wal_shm_allocated_bytes": allocated,
    }


def interval(process, directory, seconds, driver=None):
    samples = [snapshot(process.pid, directory)]
    start = time.monotonic()
    while time.monotonic() - start < seconds or (driver and driver.poll() is None):
        if time.monotonic() - start > seconds + 30:
            raise RuntimeError("load driver exceeded bounded measurement window")
        if process.poll() is not None:
            raise RuntimeError("collector exited during measurement")
        time.sleep(min(0.5, max(0.01, seconds - (time.monotonic() - start))))
        samples.append(snapshot(process.pid, directory))
    first, last = samples[0], samples[-1]
    assert all(s["start_ticks"] == first["start_ticks"] for s in samples), "PID generation changed"
    elapsed = (last["monotonic_ns"] - first["monotonic_ns"]) / 1e9
    delta = {}
    for key in ("cpu_user_seconds", "cpu_system_seconds", "block_write_bytes", "logical_wchar",
                "db_wal_shm_apparent_bytes", "db_wal_shm_allocated_bytes"):
        delta[key] = None if first[key] is None or last[key] is None else last[key] - first[key]
    cpu = delta["cpu_user_seconds"] + delta["cpu_system_seconds"]
    rss = [s["rss_bytes"] for s in samples if s["rss_bytes"] is not None]
    return {
        "elapsed_seconds": elapsed, "sample_count": len(samples), "delta": delta,
        "cpu_percent_one_core": 100 * cpu / elapsed,
        "rss_sample_mean_bytes": statistics.fmean(rss) if rss else None,
        "rss_sample_max_bytes": max(rss) if rss else None,
        "kernel_process_hwm_bytes": last["kernel_process_hwm_bytes"],
        "db_wal_shm_end_apparent_bytes": last["db_wal_shm_apparent_bytes"],
        "db_wal_shm_end_allocated_bytes": last["db_wal_shm_allocated_bytes"],
    }


def stop(process):
    if process.poll() is None:
        process.send_signal(signal.SIGTERM)
        try:
            process.wait(timeout=15)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)


def stack_targets():
    result = {}
    for name, unit in (("collector", "subyard-telemetry.service"), ("manager", "subyard-telemetry-runs.service")):
        response = subprocess.run(["systemctl", "show", unit, "--property=MainPID,ControlGroup"],
                                  capture_output=True, text=True, check=True, timeout=5)
        fields = dict(line.split("=", 1) for line in response.stdout.splitlines())
        pid = int(fields["MainPID"])
        group = fields["ControlGroup"]
        assert group.startswith("/") and Path(group).name == unit and ".." not in Path(group).parts
        relative = group + ("/manager" if name == "manager" else "")
        member = next(line[3:] for line in (Path("/proc")/str(pid)/"cgroup").read_text().splitlines() if line.startswith("0::"))
        assert member == relative, "service PID is outside its measured leaf"
        path = Path("/sys/fs/cgroup")/relative.lstrip("/")
        info = path.stat()
        result[name] = (pid, path, (info.st_dev, info.st_ino))
    return result


def stack_snapshot(targets, data):
    result = {"monotonic_ns": time.monotonic_ns()}
    for name, (pid, path, identity) in targets.items():
        info = path.stat()
        assert (info.st_dev, info.st_ino) == identity, "service cgroup generation changed"
        values = snapshot(pid, data)
        cpu = dict(line.split() for line in (path/"cpu.stat").read_text().splitlines())
        values["cgroup_cpu_ns"] = int(cpu["usage_usec"]) * 1000
        for filename, key in (("memory.current", "cgroup_memory_bytes"), ("memory.peak", "cgroup_memory_peak_bytes")):
            try:
                values[key] = int((path/filename).read_text())
            except FileNotFoundError:
                values[key] = None
        rows = (path/"io.stat").read_text().splitlines()
        io = [dict(pair.split("=", 1) for pair in line.split()[1:]) for line in rows]
        for field in ("rbytes", "wbytes"):
            values["cgroup_"+field] = sum(int(row[field]) for row in io) if io and all(field in row for row in io) else None
        result[name] = values
    return result


def stack_interval(targets, data, seconds, children=()):
    start = time.monotonic()
    first = stack_snapshot(targets, data)
    last = first
    high = {name: {"rss_bytes": first[name]["rss_bytes"], "cgroup_memory_bytes": first[name]["cgroup_memory_bytes"]} for name in targets}
    while time.monotonic()-start < seconds or any(child.poll() is None for child in children):
        assert time.monotonic()-start < seconds+30, "stack workload deadline"
        time.sleep(min(1, max(0.05, seconds-(time.monotonic()-start))))
        last = stack_snapshot(targets, data)
        for name in targets:
            assert first[name]["start_ticks"] == last[name]["start_ticks"], "service PID generation changed"
            for key in high[name]:
                if last[name][key] is not None:
                    high[name][key] = max(high[name][key] or 0, last[name][key])
    elapsed = (last["monotonic_ns"]-first["monotonic_ns"])/1e9
    result = {"elapsed_seconds": elapsed}
    for name in targets:
        delta = {}
        for key in ("cgroup_cpu_ns", "cgroup_rbytes", "cgroup_wbytes", "block_write_bytes", "logical_wchar"):
            a, b = first[name][key], last[name][key]
            delta[key] = None if a is None or b is None else b-a
            assert delta[key] is None or delta[key] >= 0, "counter reset during stack measurement"
        result[name] = {"delta": delta, "cpu_percent_one_core": delta["cgroup_cpu_ns"]/elapsed/1e7,
                        "sampled_max": high[name], "end": last[name]}
    result["db_wal_shm_apparent_growth_bytes"] = last["collector"]["db_wal_shm_apparent_bytes"]-first["collector"]["db_wal_shm_apparent_bytes"]
    return result


def stack_failure(driver_status, workload_status, stderr):
    # Only these fixed launcher diagnostics are safe to forward. The
    # synthetic workload log is not general command/env capture.
    known = {"Managed telemetry scope unavailable; native command was not started",
             "Managed telemetry placement unavailable; native command was not started"}
    lines = stderr.splitlines()
    diagnostics = [line for line in lines if line in known]
    membership = next(line[3:] for line in Path('/proc/self/cgroup').read_text().splitlines()
                      if line.startswith('0::'))
    root = Path('/sys/fs/cgroup')
    source = root/membership.lstrip('/')
    ceilings = []
    for path in (source, *source.parents):
        if not path.is_relative_to(root):
            break
        values = {}
        for name in ('memory.max', 'memory.high', 'memory.swap.max', 'pids.max', 'cpu.max',
                     'cpuset.cpus.effective', 'cpuset.mems.effective', 'io.max'):
            candidate = path/name
            if candidate.exists():
                values[name] = candidate.read_text()[:4096].strip()
        ceilings.append({'cgroup': str(path.relative_to(root)), 'limits': values})
    print(json.dumps({'schema': 'subyard.telemetry.stack-failure/v1',
                      'driver_exit': driver_status, 'workload_exit': workload_status,
                      'launcher_diagnostics': diagnostics, 'caller_ceilings': ceilings}), flush=True)


def measure_stack(args):
    assert os.environ.get("SUBYARD_E2E_SLOT") and Path("/run/subyard-e2e-lease.json").is_file(), "requires leased disposable VM"
    output = args.output
    assert output is not None, "stack measurement requires an output directory"
    output.mkdir(mode=0o700, parents=True, exist_ok=True)
    output.chmod(0o700)
    targets = stack_targets()
    data = args.data_dir.resolve()
    # Fail before timed load if the real caller cannot enter a safe scope.
    probe = subprocess.run(["subyard-telemetry-run", "--agent", "other", "--agent-version", "benchmark-v1",
                            "--scope-mode", "require", "--project-id", "telemetry-benchmark", "--", "/bin/true"],
                           capture_output=True, text=True, timeout=15)
    if probe.returncode:
        stack_failure(None, probe.returncode, probe.stderr)
        raise AssertionError('managed scope prerequisite failed')
    print(json.dumps({"progress": "stack-idle-start", "seconds": args.idle}), flush=True)
    idle = stack_interval(targets, data, args.idle)
    print(json.dumps({"progress": "stack-load-start", "seconds": args.load}), flush=True)
    # Synthetic, repeatable short descendants exercise cgroup accounting between
    # sampler ticks. This workload is outside the service overhead leaf groups.
    workload = "import subprocess,sys,time; end=time.monotonic()+float(sys.argv[1]); memory=bytearray(16*1024*1024)\nwhile time.monotonic()<end:\n subprocess.run([sys.executable,'-c','sum(range(100000))'],check=True); time.sleep(1)"
    with (output/"load.json").open("w+") as driver_output, (output/"workload.log").open("wb") as workload_output:
        driver = subprocess.Popen([str(args.assets.resolve()/"otlp-load"), "--duration", f"{args.load}s", "--rate", str(args.rate),
                                   "--seed", str(args.seed), "--token-file", str(data/"auth.token"), "--endpoint", "http://127.0.0.1:14318"],
                                  stdout=driver_output, stderr=subprocess.DEVNULL)
        command = subprocess.Popen(["subyard-telemetry-run", "--agent", "other", "--agent-version", "benchmark-v1", "--scope-mode", "require",
                                    "--project-id", "telemetry-benchmark", "--", "python3", "-c", workload, str(args.load)],
                                   stdout=workload_output, stderr=workload_output)
        try:
            active = stack_interval(targets, data, args.load, (driver, command))
            driver_status, workload_status = driver.wait(timeout=5), command.wait(timeout=5)
            if driver_status or workload_status:
                workload_output.flush()
                stack_failure(driver_status, workload_status, (output/"workload.log").read_text())
                raise AssertionError('stack workload failed')
            driver_output.seek(0)
            offered = json.load(driver_output)
            assert offered["accepted_requests"] > 0 and offered["rejected_requests"] == 0
        finally:
            stop(driver)
            stop(command)
    result = {"schema": "subyard.telemetry.stack-overhead/v1", "idle": idle, "load": active,
              "offered": offered, "sample_interval_seconds": 1, "agent_sample_interval_seconds": 5,
              "scope": "collector service and manager leaf only; workload sibling groups excluded"}
    library = Path("/usr/local/lib/subyard-telemetry")
    result["installed_sha256"] = {path.name: digest(path) for path in (library/"subyard-telemetry", library/"launcher.py", library/"processes.py", library/"records.py", library/"sources.py")}
    stats = subprocess.run([str(library/"subyard-telemetry"), "stats", "--data-dir", str(data)],
                           capture_output=True, text=True, check=True, timeout=10)
    result["database"] = json.loads(stats.stdout)
    (output/"stack-overhead.json").write_text(json.dumps(result)+"\n")
    print(json.dumps(result), flush=True)


def trial(binary, driver_binary, root, args, repetition, variant):
    directory = root / f"{repetition}-{variant}"
    directory.mkdir(mode=0o700)
    token = directory / "auth"
    token.write_text(secrets.token_urlsafe(32))
    token.chmod(0o600)
    with socket.socket() as probe:
        probe.bind(("127.0.0.1", 0))
        port = probe.getsockname()[1]
    endpoint = f"http://127.0.0.1:{port}"
    with (directory / "service.log").open("wb") as log:
        process = subprocess.Popen([str(binary), "serve", "--data-dir", str(directory),
                                    "--listen", f"127.0.0.1:{port}", "--auth-file", str(token)],
                                   stdout=log, stderr=log)
        try:
            deadline = time.monotonic() + 20
            while True:
                if process.poll() is not None:
                    raise RuntimeError("collector startup failed; inspect private trial service.log")
                try:
                    with socket.create_connection(("127.0.0.1", port), timeout=0.5):
                        break
                except OSError:
                    if time.monotonic() > deadline:
                        raise RuntimeError("collector startup timeout")
                    time.sleep(0.1)
            warmup = subprocess.run([str(driver_binary), "--duration", f"{args.warmup}s", "--rate", str(args.rate),
                                     "--seed", str(args.seed), "--token-file", str(token), "--endpoint", endpoint],
                                    capture_output=True, text=True, check=True, timeout=args.warmup + 30)
            warmup_result = json.loads(warmup.stdout)
            assert warmup_result["accepted_requests"] > 0 and warmup_result["rejected_requests"] == 0
            idle = interval(process, directory, args.idle)
            with (directory / "load.json").open("w+") as output:
                driver = subprocess.Popen([str(driver_binary), "--duration", f"{args.load}s", "--rate", str(args.rate),
                                           "--seed", str(args.seed), "--token-file", str(token), "--endpoint", endpoint],
                                          stdout=output, stderr=subprocess.DEVNULL)
                try:
                    load = interval(process, directory, args.load, driver)
                    assert driver.wait(timeout=15) == 0, "load driver failed"
                finally:
                    stop(driver)
                output.seek(0)
                offered = json.load(output)
            assert offered["accepted_requests"] > 0 and offered["rejected_requests"] == 0, "rejected load"
            stats = subprocess.run([str(binary), "stats", "--data-dir", str(directory)],
                                   capture_output=True, text=True, check=True, timeout=10)
            database = json.loads(stats.stdout)
            assert database["records"] == warmup_result["accepted_requests"] + offered["accepted_requests"], "ACK/commit count mismatch"
            return {"schema": "subyard.telemetry.overhead/v1", "variant": variant, "repetition": repetition,
                    "binary_sha256": digest(binary), "driver_sha256": digest(driver_binary),
                    "warmup": warmup_result, "idle": idle, "load": load,
                    "offered": offered, "database": database}
        finally:
            stop(process)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--assets", type=Path, default=Path(".subyard-acceptance/assets"))
    parser.add_argument("--output", type=Path)
    parser.add_argument("--repetitions", type=int, default=3)
    parser.add_argument("--warmup", type=int, default=15)
    parser.add_argument("--idle", type=int, default=60)
    parser.add_argument("--load", type=int, default=90)
    parser.add_argument("--rate", type=int, default=30)
    parser.add_argument("--seed", type=int, default=1)
    parser.add_argument("--preflight", action="store_true")
    parser.add_argument("--stack", action="store_true", help="measure installed collector and manager leaf separately")
    parser.add_argument("--data-dir", type=Path, default=Path("/var/lib/subyard/telemetry"))
    args = parser.parse_args()
    assert all(getattr(args, key) > 0 for key in ("repetitions", "warmup", "idle", "load", "rate"))
    if args.stack:
        measure_stack(args)
        return
    assets = args.assets.resolve()
    binaries = {key: assets / filename for key, filename in (
        ("standalone", "subyard-telemetry"), ("minimal_collector", "otelcol-minimal"), ("driver", "otlp-load"))}
    assert all(path.is_file() and os.access(path, os.X_OK) for path in binaries.values()), "build comparison assets first"
    snapshot(os.getpid(), Path("/tmp"))
    if args.preflight:
        print(json.dumps({"preflight": "PASS", "assets": {key: digest(path) for key, path in binaries.items()}}))
        return
    assert os.environ.get("SUBYARD_E2E_SLOT") and os.environ.get("SUBYARD_E2E_GENERATION"), "requires leased disposable VM"
    output = args.output or Path(tempfile.mkdtemp(prefix="subyard-telemetry-measure-", dir="/var/tmp"))
    output.mkdir(mode=0o700, parents=True, exist_ok=True)
    output.chmod(0o700)
    conditions = {"schema": "subyard.telemetry.overhead.conditions/v1", "system": platform.platform(),
                  "machine": platform.machine(), "ticks_per_second": os.sysconf("SC_CLK_TCK"),
                  "page_size": os.sysconf("SC_PAGE_SIZE"), "sample_interval_seconds": 0.5,
                  "slot": os.environ["SUBYARD_E2E_SLOT"], "generation": os.environ["SUBYARD_E2E_GENERATION"],
                  "base_fingerprint": os.environ.get("SUBYARD_E2E_BASE_FINGERPRINT"),
                  "granted_cpu": os.environ.get("SUBYARD_E2E_CPU_PER_VM"),
                  "granted_memory": os.environ.get("SUBYARD_E2E_MEMORY_PER_VM"),
                  "warmup_seconds": args.warmup, "idle_seconds": args.idle, "load_seconds": args.load,
                  "rate": args.rate, "seed": args.seed, "repetitions": args.repetitions}
    print(json.dumps(conditions), flush=True)
    (output / "conditions.json").write_text(json.dumps(conditions) + "\n")
    for repetition in range(1, args.repetitions + 1):
        order = ["standalone", "minimal_collector"]
        if repetition % 2 == 0:
            order.reverse()
        for variant in order:
            print(json.dumps({"progress": "trial-start", "variant": variant, "repetition": repetition}), flush=True)
            result = trial(binaries[variant], binaries["driver"], output, args, repetition, variant)
            (output / f"{repetition}-{variant}.json").write_text(json.dumps(result) + "\n")
            print(json.dumps(result), flush=True)


if __name__ == "__main__":
    main()
