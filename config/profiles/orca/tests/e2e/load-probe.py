#!/usr/bin/env python3
"""Measure a disposable guest's Orca catalog without retaining RPC payloads."""

import argparse
import importlib.util
import json
import math
import os
import re
from pathlib import Path
import stat
import subprocess
import socket
import struct
import sys
import tempfile
import time
import traceback


METADATA = "/srv/agents/orca/config/orca/orca-runtime.json"
CLONE_ROOT = "/srv/workspaces/clone-project/src"
METHODS = ("repo.list", "worktree.list", "worktree.ps", "session.tabs.listAll")
ADMISSION_LIMIT = 1000
PHASES = ("baseline", "loaded", "restarted", "cleaned",
          "repeated", "interrupted-cleaned", "stock")


class ProbeError(Exception):
    pass


def require(condition, message):
    if not condition:
        raise ProbeError(message)


def process_sample(pid):
    try:
        fields = Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()
        return int(fields[19]), int(fields[11]) + int(fields[12]), time.monotonic()
    except (OSError, ValueError, IndexError):
        raise ProbeError("server process unavailable") from None


def cpu_percent(first, last):
    require(first[0] == last[0] and last[1] >= first[1], "server process identity changed")
    elapsed = last[2] - first[2]
    require(elapsed > 0, "invalid CPU measurement interval")
    return 100 * (last[1] - first[1]) / os.sysconf("SC_CLK_TCK") / elapsed


def percentile(values):
    return sorted(values)[max(0, math.ceil(len(values) * 0.95) - 1)] if values else 0


def measure_phase(sample, round_trip, *, cold=False, repeated=False,
                  sleep=time.sleep, monotonic=time.monotonic):
    startup = sample() if cold else None
    first_rpc = round_trip() if cold else None
    if cold:
        sleep(25)
    idle_start = sample()
    startup_cpu = cpu_percent(startup, idle_start) if cold else None
    sleep(10)
    idle_cpu = cpu_percent(idle_start, sample())
    demand_start = sample()
    deadline = monotonic() + (65 if repeated else 0)
    next_poll = monotonic()
    while True:
        round_trip()
        now = monotonic()
        if not repeated or now >= deadline:
            break
        next_poll = max(next_poll + 3, now)
        sleep(min(next_poll - now, deadline - now))
    final_sample = sample()
    result = {"cpu_percent": round(cpu_percent(idle_start, final_sample), 2),
              "idle_cpu_percent": round(idle_cpu, 2),
              "demand_cpu_percent": round(cpu_percent(demand_start, final_sample), 2)}
    if cold:
        result.update(startup_cpu_percent=round(startup_cpu, 2), settle_seconds=25,
                      first_max_rpc_ms=round(max(value["max_ms"] for value in first_rpc.values()), 2),
                      first_rpc=first_rpc)
    return result


def read_state(path):
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        with os.fdopen(fd) as source:
            metadata = os.fstat(source.fileno())
            require(stat.S_ISREG(metadata.st_mode) and stat.S_IMODE(metadata.st_mode) == 0o600
                    and metadata.st_uid == os.geteuid() and metadata.st_nlink == 1,
                    "unsafe load-probe state")
            raw = source.read(65537)
            require(len(raw) <= 65536, "load-probe state exceeded size limit")
            state = json.loads(raw)
        validate_baseline(state)
        return state
    except (OSError, ValueError, TypeError, RecursionError):
        raise ProbeError("load-probe state unavailable") from None


def validate_baseline(state):
    require(isinstance(state, dict) and set(state) == {"repo_ids", "tab_ids", "repo_count"},
            "invalid load-probe state")
    require(all(isinstance(state[key], list) and state[key]
                and all(isinstance(value, str) and value for value in state[key])
                and len(set(state[key])) == len(state[key]) for key in ("repo_ids", "tab_ids")), "invalid load-probe IDs")
    count = state["repo_count"]
    require(type(count) is int and len(state["repo_ids"]) == count <= ADMISSION_LIMIT,
            "invalid baseline repository count")


def remaining_fixture_repos(repos, tabs, state, root):
    validate_baseline(state)
    require(root == Path(CLONE_ROOT) / ".build/load-diagnostic", "invalid load-probe root")
    require(set(state["repo_ids"]) <= ids(repos), "baseline repository identity lost")
    require(set(state["tab_ids"]) <= ids(tabs), "baseline terminal identity lost")
    require(len(repos) <= ADMISSION_LIMIT, "native repository catalog exceeded admission limit")
    count = 0
    for repo in repos:
        path = repo.get("path")
        require(isinstance(path, str) and Path(path).is_absolute() and ".." not in Path(path).parts,
                "invalid repository path")
        count += Path(path).is_relative_to(root)
    return count


def admission_counts(repos, tabs, state, root, generated_roots, loaded):
    require(type(generated_roots) is int and generated_roots >= 0, "invalid generated-root count")
    actual = remaining_fixture_repos(repos, tabs, state, root)
    baseline_count = state["repo_count"]
    expected = min(generated_roots + 1, ADMISSION_LIMIT - baseline_count) if loaded else 0
    return {"admission_limit": ADMISSION_LIMIT, "admitted_fixture_repos": actual,
            "expected_fixture_repos": expected,
            "deferred_fixture_roots": generated_roots + 1 - expected if loaded else 0,
            "admission_complete": int(actual == expected and len(repos) == baseline_count + expected)}


def classify_sync_failure(output):
    if len(output.encode()) > 65536:
        return "unknown"
    errors = [line.removeprefix("Orca registration error: ") for line in output.splitlines()
              if line.startswith("Orca registration error: ")]
    if not errors:
        return "unknown"
    budget = {"Orca registration time budget exhausted",
              "Orca registration time budget exhausted; result is incomplete"}
    methods = {"repo.list", "repo.add", "repo.rm", "repo.update", "session.tabs.listAll",
               "projectGroup.list", "projectGroup.update", "projectGroup.create", "projectGroup.delete"}
    categories = []
    for error in errors:
        if error in budget:
            categories.append("budget")
        elif error == "Another Orca registration invocation holds the lock":
            categories.append("lock")
        else:
            match = re.fullmatch(r"(?:[^\n]+: )?Orca runtime request timed out: ([a-zA-Z.]+) \(([0-9]+(?:\.[0-9]+)?)s\)", error)
            if match and match[1] in methods and len(match[2]) <= 20:
                categories.append("rpc-timeout:" + match[1] + ":" + match[2])
            else:
                categories.append("unknown")
    unique = set(categories)
    return categories[0] if len(unique) == 1 else "mixed"


def write_state(path, repo_ids, tab_ids, repo_count):
    # Save identifiers and the repository count; pairing and runtime metadata never enter it.
    require(not os.path.lexists(path), "baseline state already exists")
    fd, temporary = tempfile.mkstemp(prefix=".orca-load-state.", dir=Path(path).parent)
    try:
        with os.fdopen(fd, "w") as output:
            os.fchmod(output.fileno(), 0o600)
            json.dump({"repo_ids": sorted(repo_ids), "tab_ids": sorted(tab_ids), "repo_count": repo_count}, output)
            output.flush()
            os.fsync(output.fileno())
        # Exclusive publication avoids replacing an unrelated file or symlink.
        os.link(temporary, path, follow_symlinks=False)
    finally:
        os.unlink(temporary)


def records(result, key):
    require(isinstance(result, dict) and isinstance(result.get(key), list), "invalid catalog response")
    require(all(isinstance(row, dict) for row in result[key]), "invalid catalog record")
    return result[key]


def ids(rows):
    require(all(isinstance(row.get("id"), str) and row["id"] for row in rows), "invalid catalog IDs")
    return {row["id"] for row in rows}


def missing_worktree_paths(rows, repos, baseline_ids, root):
    """Count stock worktree omissions without treating them as admission proof."""
    expected = set()
    for repo in repos:
        path = repo.get("path")
        require(isinstance(path, str) and path, "invalid repository path")
        if repo.get("id") in baseline_ids or Path(path).is_relative_to(root):
            expected.add((path, repo.get("id")))
    actual = {(row.get("path"), row.get("repoId")) for row in rows}
    return len(expected - actual)


TELEMETRY_LIMIT = 2 * 1024 * 1024
RPC_REASONS = frozenset((
    "runtime metadata unavailable", "runtime request timed out", "runtime disconnected",
    "response exceeded size limit", "runtime returned invalid JSON", "response correlation mismatch",
    "response runtime mismatch", "runtime rejected method", "runtime returned invalid envelope",
    "runtime connection failed", "invalid session snapshots",
))


def numeric(value):
    return type(value) in (int, float) and math.isfinite(value) and value >= 0


def aggregate_native_telemetry(journal, trace):
    result = {"main_thread_samples": 0, "max_gap_ms": 0, "gaps_over_50_ms": 0,
              "gaps_over_250_ms": 0, "spawn_count": 0, "spawn_block_ms_total": 0,
              "spawn_block_ms_max": 0}
    duration, queue = [], []
    for line in journal.splitlines():
        marker = "[main-thread] "
        if marker not in line:
            continue
        try:
            row = json.loads(line.split(marker, 1)[1])
        except (ValueError, RecursionError):
            continue
        fields = ("t", "maxGapMs", "gapsOver50Ms", "gapsOver250Ms", "spawnCount")
        if not isinstance(row, dict) or not all(numeric(row.get(key)) for key in fields):
            continue
        result["main_thread_samples"] += 1
        result["max_gap_ms"] = max(result["max_gap_ms"], row["maxGapMs"])
        for source, target in (("gapsOver50Ms", "gaps_over_50_ms"),
                               ("gapsOver250Ms", "gaps_over_250_ms"), ("spawnCount", "spawn_count")):
            result[target] += row[source]
        spawns = row.get("spawns")
        if isinstance(spawns, dict):
            for value in spawns.values():
                if isinstance(value, dict) and numeric(value.get("blockMsTotal")) and numeric(value.get("blockMsMax")):
                    result["spawn_block_ms_total"] += value["blockMsTotal"]
                    result["spawn_block_ms_max"] = max(result["spawn_block_ms_max"], value["blockMsMax"])
    for line in trace.splitlines():
        try:
            row = json.loads(line)
        except (ValueError, RecursionError):
            continue
        if not isinstance(row, dict) or row.get("name") != "git.exec":
            continue
        if numeric(row.get("durationMs")):
            duration.append(row["durationMs"])
        attrs = row.get("attributes")
        if isinstance(attrs, dict) and numeric(attrs.get("git.queue_wait_ms")):
            queue.append(attrs["git.queue_wait_ms"])
    for prefix, values in (("sampled_git_queue_inclusive_duration", duration), ("sampled_git_queue_wait", queue)):
        result[prefix + "_count"] = len(values)
        result[prefix + "_p95_ms"] = percentile(values)
        result[prefix + "_max_ms"] = max(values, default=0)
    return result


def telemetry_start(path):
    # Only disposable service-owned state is inspected; paths never enter output.
    traces = []
    for base in (Path("/srv/agents/orca/config"), Path("/home/dev/.config/Orca")):
        visited = 0
        for directory, children, files in os.walk(base, followlinks=False):
            visited += 1
            if visited > 200:
                break
            children[:] = [name for name in children if not (Path(directory) / name).is_symlink()]
            if "main.trace.ndjson" in files and Path(directory).name == "logs":
                trace = Path(directory) / "main.trace.ndjson"
                info = trace.lstat()
                if stat.S_ISREG(info.st_mode):
                    traces.append([str(trace), info.st_ino, info.st_size])
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "w") as output:
        os.fchmod(output.fileno(), 0o600)
        json.dump({"since": time.time(), "traces": traces}, output)


def telemetry_finish(path, phase):
    snapshot = json.loads(Path(path).read_text())
    now = time.time()
    journal = ""
    available = 0
    try:
        process = subprocess.Popen(["journalctl", "--no-pager", "-u", "subyard-orca.service",
                                    "--since", "@" + str(snapshot["since"]), "--until", "@" + str(now),
                                    "-o", "cat", "-n", "1000"], stdout=subprocess.PIPE,
                                   stderr=subprocess.DEVNULL)
        # Bound bytes and completion independently; no raw journal output is published.
        import selectors
        chunks = bytearray()
        with selectors.DefaultSelector() as selector:
            selector.register(process.stdout, selectors.EVENT_READ)
            deadline = time.monotonic() + 10
            while len(chunks) < TELEMETRY_LIMIT and time.monotonic() < deadline:
                if not selector.select(min(1, max(0, deadline - time.monotonic()))):
                    continue
                part = os.read(process.stdout.fileno(), min(65536, TELEMETRY_LIMIT - len(chunks)))
                if not part:
                    available = int(process.wait(timeout=1) == 0)
                    break
                chunks.extend(part)
        journal = chunks.decode(errors="replace")
    except (OSError, subprocess.TimeoutExpired):
        pass
    finally:
        if 'process' in locals() and process.poll() is None:
            process.kill()
            process.wait(timeout=2)
    traces, trace_bytes = [], 0
    for filename, inode, offset in snapshot["traces"]:
        try:
            fd = os.open(filename, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
            with os.fdopen(fd, "rb") as source:
                info = os.fstat(source.fileno())
                if not stat.S_ISREG(info.st_mode):
                    continue
                if info.st_ino == inode and info.st_size >= offset:
                    source.seek(offset)
                data = source.read(max(0, TELEMETRY_LIMIT - trace_bytes))
                trace_bytes += len(data)
                traces.append(data.decode(errors="replace"))
        except OSError:
            continue
    metrics = aggregate_native_telemetry(journal, "\n".join(traces))
    metrics.update(journal_available=available, trace_bytes_read=trace_bytes,
                   bytes_limited=int(trace_bytes >= TELEMETRY_LIMIT or len(journal.encode()) >= TELEMETRY_LIMIT))
    print(json.dumps({"phase": phase, "native_telemetry": metrics}, separators=(",", ":")),
          file=sys.stderr, flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--phase", required=True, choices=PHASES)
    parser.add_argument("--remaining", action="store_true")
    parser.add_argument("--admission-ready", action="store_true")
    parser.add_argument("--classify-sync-failure", action="store_true")
    parser.add_argument("--telemetry-start", action="store_true")
    parser.add_argument("--telemetry-finish", action="store_true")
    parser.add_argument("--telemetry-state", default="/tmp/orca-load-telemetry.json")
    parser.add_argument("--pid", type=int)
    parser.add_argument("--roots", type=int)
    parser.add_argument("--root")
    parser.add_argument("--state", default="/tmp/orca-load-state.json")
    args = parser.parse_args()
    reasons = {method: {} for method in METHODS}
    try:
        if args.classify_sync_failure:
            raw = sys.stdin.buffer.read(65537)
            print(classify_sync_failure(raw.decode(errors="replace")) if len(raw) <= 65536 else "unknown")
            return 0
        if args.remaining or args.admission_ready:
            state = read_state(args.state)
            root = Path(args.root)
            require(root == Path(CLONE_ROOT) / ".build/load-diagnostic", "invalid load-probe root")
            spec = importlib.util.spec_from_file_location("remaining_fixture", "/tmp/orca-projects-helper.py")
            fixture = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(fixture)
            repos = records(fixture.call_any_result(METADATA, "repo.list", None), "repos")
            snapshots = fixture.snapshot_tabs(fixture.call_any_result(METADATA, "session.tabs.listAll", None))
            tabs = [tab for row in snapshots for tab in row["tabs"]]
            if args.admission_ready:
                print(admission_counts(repos, tabs, state, root, args.roots, True)["admission_complete"])
            else:
                print(remaining_fixture_repos(repos, tabs, state, root))
            return 0
        if args.telemetry_start:
            telemetry_start(args.telemetry_state)
            return 0
        if args.telemetry_finish:
            telemetry_finish(args.telemetry_state, args.phase)
            return 0

        require(args.pid > 0 and args.roots >= 0, "invalid load-probe arguments")
        root = Path(args.root)
        require(root.is_absolute() and str(root) != "/" and ".." not in root.parts,
                "invalid load-probe root")
        spec = importlib.util.spec_from_file_location("orca_load_fixture", "/tmp/orca-projects-helper.py")
        fixture = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(fixture)
        # MainPID is the CLI supervisor. Measure the actual server accepting RPC.
        endpoint, _, _ = fixture.read_runtime_metadata(METADATA)
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
            connection.settimeout(10)
            connection.connect(endpoint)
            pid, uid, _ = struct.unpack("3i", connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
        require(pid > 0 and uid == os.geteuid(), "unexpected runtime socket owner")
        ancestor = pid
        for _ in range(16):
            if ancestor == args.pid:
                break
            fields = Path(f"/proc/{ancestor}/stat").read_text().rsplit(")", 1)[1].split()
            ancestor = int(fields[1])
            require(ancestor > 1, "runtime escaped service process tree")
        require(ancestor == args.pid, "runtime escaped service process tree")
        identity = process_sample(pid)[0]

        def sample():
            value = process_sample(pid)
            require(value[0] == identity, "server process identity changed")
            return value

        def call(method, params=None):
            sample()
            try:
                return fixture.call_any_result(METADATA, method, params)
            finally:
                sample()

        if args.phase == "baseline":
            repos = records(call("repo.list"), "repos")
            matches = [repo for repo in repos if repo.get("path") == CLONE_ROOT
                       and repo.get("executionHostId", "local") == "local"
                       and repo.get("connectionId") is None]
            require(len(matches) == 1, "sentinel repository unavailable")
            result = call("session.tabs.createTerminal", {
                "worktree": f"id:{matches[0]['id']}::{CLONE_ROOT}", "activate": False,
                "clientMutationId": "orca-load-sentinel",
            })
            require(isinstance(result, dict) and isinstance(result.get("tab"), dict)
                    and isinstance(result["tab"].get("id"), str), "sentinel terminal unavailable")
            snapshots = fixture.snapshot_tabs(call("session.tabs.listAll"))
            tab_ids = ids([tab for snapshot in snapshots for tab in snapshot["tabs"]])
            require(result["tab"]["id"] in tab_ids, "sentinel terminal missing from saved tabs")
            write_state(args.state, ids(repos), tab_ids, len(repos))
        state = read_state(args.state)
        counts = {"repos": -1, "load_roots": -1, "worktrees": -1, "ps_worktrees": -1, "tabs": -1}
        omissions = {method: [] for method in METHODS if method.startswith("worktree.")}
        timings = {method: [] for method in METHODS}
        errors = {method: 0 for method in METHODS}
        limit = max(200, args.roots + len(state["repo_ids"]) + 32)
        loaded = args.phase in ("loaded", "restarted", "repeated", "stock")

        def round_trip():
            repos = None
            for method in METHODS:
                started = time.monotonic()
                try:
                    result = call(method, {"limit": limit} if method.startswith("worktree.") else None)
                    if method == "repo.list":
                        repos = records(result, "repos")
                        counts["repos"] = len(repos)
                        counts["load_roots"] = sum(
                            isinstance(repo.get("path"), str) and Path(repo["path"]).parent == root
                            and Path(repo["path"]).name.startswith("checkout-") for repo in repos)
                        require(set(state["repo_ids"]) <= ids(repos), "baseline repository identity lost")
                        require(len(repos) <= ADMISSION_LIMIT, "native repository catalog exceeded admission limit")
                    elif method == "session.tabs.listAll":
                        snapshots = fixture.snapshot_tabs(result)
                        tab_ids = ids([tab for snapshot in snapshots for tab in snapshot["tabs"]])
                        counts["tabs"] = sum(len(snapshot["tabs"]) for snapshot in snapshots)
                        require(set(state["tab_ids"]) <= tab_ids, "baseline terminal identity lost")
                        if repos is not None:
                            counts.update(admission_counts(repos, [tab for snapshot in snapshots for tab in snapshot["tabs"]],
                                                          state, root, args.roots, loaded))
                            require(counts["admission_complete"] == 1, "admitted fixture catalog count mismatch")
                    else:
                        rows = records(result, "worktrees")
                        counts["ps_worktrees" if method == "worktree.ps" else "worktrees"] = len(rows)
                        require(result.get("truncated") is not True, "worktree measurement was truncated")
                        if repos is not None:
                            omissions[method].append(missing_worktree_paths(rows, repos, set(state["repo_ids"]), root))
                except fixture.SafeRpcError as error:
                    errors[method] += 1
                    reason = str(error) if str(error) in RPC_REASONS else "other sanitized RPC failure"
                    reasons[method][reason] = reasons[method].get(reason, 0) + 1
                finally:
                    timings[method].append((time.monotonic() - started) * 1000)
            return {method: {"max_ms": round(values[-1], 2), "failures": errors[method]}
                    for method, values in timings.items()}

        cold = args.phase in ("baseline", "loaded", "restarted", "stock")
        repeated = args.phase in ("repeated", "stock")
        activity = measure_phase(sample, round_trip, cold=cold, repeated=repeated)
        all_times = [value for values in timings.values() for value in values]
        summary = {
            "phase": args.phase, "pid": pid, "supervisor_pid": args.pid, "starttime": identity, **counts,
            **activity,
            "p95_rpc_ms": round(percentile(all_times), 2), "max_rpc_ms": round(max(all_times), 2),
            "rpc": {method: {"calls": len(values), "failures": errors[method],
                              "p95_ms": round(percentile(values), 2), "max_ms": round(max(values), 2)}
                    for method, values in timings.items()},
            "rpc_failures": sum(errors.values()),
        }
        for method, values in omissions.items():
            summary["rpc"][method]["max_missing_repository_paths"] = max(values) if values else -1
        if repeated:
            for method, values in timings.items():
                midpoint = len(values) // 2
                if midpoint:
                    first, last = percentile(values[:midpoint]), percentile(values[midpoint:])
                    summary["rpc"][method].update(first_half_p95_ms=round(first, 2),
                                                   last_half_p95_ms=round(last, 2))
        print(json.dumps(summary, separators=(",", ":")), flush=True)
        require(not errors["repo.list"] and not errors["session.tabs.listAll"], "repository or saved-tab preservation unavailable")
        if not loaded:
            require(not any(errors.values()), "stock runtime RPC failed")
            require(max(all_times) <= (10000 if cold else 5000), "RPC latency exceeded phase budget")
            require(activity["cpu_percent"] < 80 and activity["idle_cpu_percent"] < 80
                    and (not repeated or activity["demand_cpu_percent"] < 80),
                    "server CPU remained saturated")
        return 0
    except ProbeError as error:
        print(f"orca-load-probe: {error}", file=sys.stderr)
    except Exception as error:
        # Locations and exception types diagnose harness failures without payloads.
        frames = ",".join(f"{Path(frame.filename).name}:{frame.lineno}:{frame.name}"
                          for frame in traceback.extract_tb(error.__traceback__))
        print(f"orca-load-probe: measurement unavailable ({type(error).__name__}; {frames})",
              file=sys.stderr)
    finally:
        if not (args.telemetry_start or args.telemetry_finish or args.remaining or args.admission_ready or args.classify_sync_failure):
            print(json.dumps({"phase": args.phase, "rpc_failure_reasons": reasons},
                             separators=(",", ":")), flush=True)
    return 1


if __name__ == "__main__":
    sys.exit(main())
