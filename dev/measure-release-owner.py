#!/usr/bin/env python3
"""Paired packaged release-owner probe; fixtures are synthetic, physical I/O is unverified."""
import argparse
import ctypes
import contextlib
import hashlib
import importlib.util
import json
import math
import os
from pathlib import Path
import platform
import random
import select
import signal
import shutil
import stat
import subprocess
import sys
import tempfile
import tarfile
import time

ROOT = Path(__file__).resolve().parents[1]
WORKLOADS = ("nominal-owner-inspection", "checkpoint-history-recovery", "legacy-history-recovery")
POLICY = {
    "minimum_pairs_per_workload": 100, "bootstrap_replicates": 10000,
    "confidence": 0.95, "wall_p95_ratio_limit": 1.10, "cpu_p95_ratio_limit": 1.10,
    "rss_allowance": "Go + max(0.10 * Go, 8 MiB)",
    "cache": "fresh product processes; warm/uncontrolled OS page cache; never cold cache",
    "pairing": "randomized order; exact-path per-arm reset by default; independent fresh fixtures explicitly labeled",
    "native_io": "synthetic observation service; physical native I/O requires a separate disposable VM scenario",
}


def module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    loaded = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(loaded)
    return loaded


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")


def sha(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def collect(job):
    """Run alone: wait4 each direct/adopted child exactly once, without Popen reaping."""
    probe = module("veranda_measure", ROOT / "dev/measure-veranda.py")
    process_snapshot = probe.processes
    libc = ctypes.CDLL(None, use_errno=True)
    if libc.prctl(36, 1, 0, 0, 0):  # PR_SET_CHILD_SUBREAPER
        raise OSError(ctypes.get_errno(), "cannot become child subreaper")
    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, lambda *_: interrupted.__setitem__(0, True))
    interrupted = [False]
    wake_read, wake_write = os.pipe2(os.O_NONBLOCK | os.O_CLOEXEC)
    signal.signal(signal.SIGCHLD, lambda *_: None)
    signal.set_wakeup_fd(wake_write)
    stdin = os.open(job["stdin"], os.O_RDONLY)
    stdout = os.open(job["stdout"], os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    stderr = os.open(job["stderr"], os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    gate_read, gate_write = os.pipe()
    exec_read, exec_write = os.pipe2(os.O_CLOEXEC)
    pid = os.fork()
    if not pid:
        try:
            os.close(gate_write)
            os.close(exec_read)
            os.setsid()
            os.dup2(stdin, 0)
            os.dup2(stdout, 1)
            os.dup2(stderr, 2)
            os.read(gate_read, 1)
            os.close(gate_read)
            os.execve(job["command"][0], job["command"], job["env"])
        except BaseException:
            os.write(exec_write, b"failed")
            os._exit(127)
    os.close(gate_read)
    os.close(exec_write)
    for descriptor in (stdin, stdout, stderr):
        os.close(descriptor)
    tree = probe.Tree(pid)
    # Seed identity only. Sampling the blocked fork child would attribute the
    # Python collector's inherited RSS to a product that has not exec'd yet.
    identity = process_snapshot().get(pid)
    if identity is not None:
        tree.known[pid] = identity["start"]
    started = last = time.monotonic()
    os.write(gate_write, b"1")
    os.close(gate_write)
    exec_ready, _, _ = select.select([exec_read, wake_read], [], [], job["timeout"])
    exec_confirmed = exec_read in exec_ready and not os.read(exec_read, 1)
    os.close(exec_read)
    reaped, samples, identities, max_gap, timed_out = [], [], {}, 0.0, False
    killed_at = None
    while True:
        now = time.monotonic()
        max_gap = max(max_gap, now - last)
        last = now
        # A child may setsid before its parent exits. Seed adopted direct children
        # as well as the existing Tree group/descendant discovery.
        observed = process_snapshot()
        for child, item in observed.items():
            if item["parent"] == os.getpid():
                tree.known[child] = item["start"]
        # Tree and orphan discovery share one /proc scan, keeping observer work
        # out of the product CPU and reducing its wall-time interference.
        probe.processes = lambda: observed
        try:
            if exec_confirmed:
                samples.append(tree.sample())
        finally:
            probe.processes = process_snapshot
        for child, start in tree.known.items():
            try:
                proc = Path("/proc") / str(child)
                executable = os.readlink(proc / "exe")
                arguments = (proc / "cmdline").read_bytes().split(b"\0")
                # Retain only known private entrypoint markers, never arbitrary
                # arguments or environment from a process that happens to race.
                marker = next((value.decode() for value in arguments[1:] if value in (
                    b"_release-owner-operation-v1", b"_release-owner-native-v1", b"_release-owner-codec-v1", b"_release-transition")), None)
                current_start = int((proc / "stat").read_text().rsplit(")", 1)[1].split()[19])
                key = f"{child}:{start}:{executable}:{marker}"
                if current_start == start:
                    first = identities.get(key, {}).get("first_seen_seconds", now - tree.started)
                    parent = observed.get(child, {}).get("parent")
                    identities[key] = {"pid": child, "start_ticks": start, "executable": executable,
                        "entrypoint": marker, "parent_pid": parent,
                        "parent_start_ticks": observed.get(parent, {}).get("start"),
                        "first_seen_seconds": first, "last_seen_seconds": now - tree.started}
            except OSError:
                pass
        if killed_at is None and (now - started > job["timeout"] or interrupted[0] or not exec_ready):
            timed_out = True
            killed_at = now
        if killed_at is not None:
            # Only identity-bound descendants, including newly adopted children.
            for child, item in observed.items():
                if tree.known.get(child) == item["start"]:
                    try:
                        fd = os.pidfd_open(child)
                        try:
                            start = int((Path("/proc") / str(child) / "stat").read_text().rsplit(")", 1)[1].split()[19])
                            if start == item["start"]:
                                signal.pidfd_send_signal(fd, signal.SIGKILL)
                        finally:
                            os.close(fd)
                    except (ProcessLookupError, FileNotFoundError):
                        pass
            if now - killed_at > 10:
                raise RuntimeError("owned descendant drain exceeded ten seconds")
        no_children = False
        while True:
            try:
                child, status, usage = os.wait4(-1, os.WNOHANG)
            except ChildProcessError:
                no_children = True
                break
            if not child:
                break
            reaped.append({"pid": child, "exit": os.waitstatus_to_exitcode(status),
                           "sampled_identity": tree.known.get(child),
                           "user_seconds": usage.ru_utime, "system_seconds": usage.ru_stime,
                           "block_inputs": usage.ru_inblock, "block_outputs": usage.ru_oublock})
        if no_children:
            break
        ready, _, _ = select.select([wake_read], [], [], job["interval"])
        if ready:
            try:
                os.read(wake_read, 65536)
            except BlockingIOError:
                pass
    elapsed = time.monotonic() - started
    signal.set_wakeup_fd(-1)
    os.close(wake_read)
    os.close(wake_write)
    root = next((entry for entry in reaped if entry["pid"] == pid), None)
    history = tree.process_history()
    for entry in history:
        entry["sampled_cpu_seconds_lower_bound"] = tree.previous.get((entry["pid"], entry["start_ticks"]), 0) / probe.TICKS
    return {"schema_version": 1, "exit": root["exit"] if root else None,
            "wall_seconds": elapsed, "cpu_seconds": sum(x["user_seconds"] + x["system_seconds"] for x in reaped),
            "peak_sampled_tree_rss_bytes": tree.peak_rss, "reaped": reaped,
            "cpu_complete": root is not None and not timed_out and exec_confirmed, "drained": True, "timed_out": timed_out,
            "sample_interval_seconds": job["interval"], "max_sample_gap_seconds": max_gap,
            "unsampled_reaped_children": [x["pid"] for x in reaped if x["sampled_identity"] is None],
            "rss_coverage": "sampled lower bound; descendants born and reaped between samples may be invisible",
            "rss_exact": False, "process_history": history, "samples": samples,
            "sampled_cpu_scope": "per-PID user+system ticks last observed before exit; diagnostic lower bounds, never the CPU comparison metric",
            "root_pid": pid, "executable_identities": list(identities.values()),
            "peak_rss_processes": tree.peak_processes,
            "rust_owner_observed": any(x["entrypoint"] == "_release-owner-operation-v1" for x in identities.values()),
            "cpu_definition": "sum wait4 rusage for root and adopted orphans; each includes only descendants it waited for",
            "exec_confirmed": exec_confirmed, "preexec_rss_excluded": True,
            "observer_excluded": True, "native_io_physical": False}


def percentile(values, fraction=0.95):
    values = sorted(values)
    position = (len(values) - 1) * fraction
    low = math.floor(position)
    return values[low] + (values[math.ceil(position)] - values[low]) * (position - low)


def comparison(pairs, seed, replicates=10000):
    result = {"pairs": len(pairs), "status": "INCONCLUSIVE", "metrics": {}}
    if not pairs:
        return result
    rng = random.Random(seed)
    for key, limit in (("wall_seconds", 1.10), ("cpu_seconds", 1.10)):
        go = [pair["go"][key] for pair in pairs]
        rust = [pair["rust"][key] for pair in pairs]
        if any(not math.isfinite(value) or value <= 0 for value in go + rust):
            result["metrics"][key] = {"status": "INCONCLUSIVE", "limit": limit,
                                      "reason": "nonpositive or nonfinite measurement cannot support a ratio"}
            continue
        ratios = []
        for _ in range(replicates):
            indexes = [rng.randrange(len(pairs)) for _ in pairs]
            denominator = percentile([go[i] for i in indexes])
            ratios.append(percentile([rust[i] for i in indexes]) / denominator)
        low, high = percentile(ratios, 0.025), percentile(ratios, 0.975)
        eligible = len(pairs) >= POLICY["minimum_pairs_per_workload"] and all(
            pair[owner]["cpu_complete"] for pair in pairs for owner in ("go", "rust"))
        result["metrics"][key] = {"go_p95": percentile(go), "rust_p95": percentile(rust),
            "paired_bootstrap_95_ci": [low, high], "limit": limit,
            "status": "INCONCLUSIVE" if not eligible else "PASS" if high <= limit else "FAIL" if low > limit else "INCONCLUSIVE"}
    # Peak means maximum simultaneous sampled sum within any command, not sum of
    # per-process ru_maxrss values and not the sum of sequential command peaks.
    go_rss = max(pair["go"]["peak_sampled_tree_rss_bytes"] for pair in pairs)
    rust_rss = max(pair["rust"]["peak_sampled_tree_rss_bytes"] for pair in pairs)
    allowance = go_rss + max(go_rss * 0.10, 8 * 1024 * 1024)
    result["metrics"]["rss"] = {"go_peak_sampled_bytes": go_rss, "rust_peak_sampled_bytes": rust_rss,
        "limit_bytes": allowance, "observed_within_limit": rust_rss <= allowance,
        "status": "INCONCLUSIVE", "reason": "short-lived helper RSS has no complete census"}
    result["limitations"] = ["physical native I/O not measured", "sampled RSS cannot certify all short-lived helpers"]
    if len(pairs) < POLICY["minimum_pairs_per_workload"]:
        result["limitations"].append("fewer than 100 pairs; preflight only")
    if any(not pair[owner]["cpu_complete"] for pair in pairs for owner in ("go", "rust")):
        result["limitations"].append("incomplete process-tree CPU")
    if any(value["status"] == "FAIL" for value in result["metrics"].values()):
        result["status"] = "FAIL"
    return result


def command_measure(command, env, directory, interval, timeout=180):
    directory.mkdir()
    stdin = directory / "stdin"
    stdin.write_bytes(b"")
    env = dict(env)
    if "SUBYARD_MEASURE_STATS_DIR" in env:
        (directory / "allocations").mkdir()
        env["SUBYARD_MEASURE_STATS_DIR"] = str(directory / "allocations")
    job = {"command": command, "env": env, "stdin": str(stdin), "stdout": str(directory / "stdout"),
           "stderr": str(directory / "stderr"), "interval": interval, "timeout": timeout}
    # Job contains only this harness's synthetic environment, never the host environment.
    with tempfile.NamedTemporaryFile(mode="w", dir="/tmp", prefix="subyard-measure-job-", delete=False) as stream:
        json.dump(job, stream)
        job_path = Path(stream.name)
    try:
        result = subprocess.run([sys.executable, str(Path(__file__).resolve()), "--collect", str(job_path)],
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        write_json(directory / "collector-native.json", {"exit": result.returncode,
                   "stdout": result.stdout, "stderr": result.stderr})
        if result.returncode:
            raise RuntimeError("collector failed; see " + str(directory / "collector-native.json"))
        metrics = json.loads(result.stdout)
        write_json(directory / "metrics.json", metrics)
        if "SUBYARD_MEASURE_STATS_DIR" in env:
            write_json(directory / "allocation-summary.json", allocations(directory / "allocations", metrics))
        return metrics
    finally:
        job_path.unlink()


def allocations(directory, metrics):
    by_pid = {}
    for path in directory.glob("*.json"):
        item = json.loads(path.read_text())
        previous = by_pid.get(item["pid"])
        count_key = ("mallocs_since_runtime_start" if item["runtime"] == "go" else
                     "successful_allocation_calls_including_realloc")
        if previous is None or item[count_key] >= previous[count_key]:
            by_pid[item["pid"]] = item
    observed = {entry["pid"] for entry in metrics["executable_identities"]
                if entry["entrypoint"] in ("_release-owner-operation-v1", "_release-owner-native-v1", "_release-owner-codec-v1", "_release-transition")}
    return {"status": "INCONCLUSIVE", "process_reports": list(by_pid.values()),
            "observed_private_processes_without_snapshot": sorted(observed - by_pid.keys()),
            "nonfinal_report_pids": sorted(pid for pid, item in by_pid.items() if not item["final"]),
            "definition": "per-process cumulative counters, never sum multiple frames from one PID; Go/Rust allocation definitions differ",
            "limitations": ["SIGKILL can prevent a final snapshot", "unobserved short-lived helpers cannot be inventoried",
                            "Go counters include reporting overhead", "instrumented timings are not product latency"]}


def semantics(fixture, inspection, observation, verify):
    """Compare explicit public meanings. Preserve full binding-bearing raw output separately."""
    journal = json.loads(fixture.journal.read_text()) if fixture.journal.exists() else {}
    history = json.loads(observation.read_text()).get("migrationHistory")
    return {"outcome": {key: inspection.get("outcome", {}).get(key) for key in ("status", "code")},
        "domains": sorted(({key: domain.get(key) for key in ("domain", "epoch", "requiredEpoch", "ledgerPresent", "applied", "pending")}
                           for domain in inspection.get("domains", [])), key=lambda domain: domain["domain"]),
        "blockers": [{key: blocker.get(key) for key in ("resource", "code", "message")} for blocker in inspection.get("blockers", [])],
        "journal_checkpoint": journal.get("checkpoint"),
        "steps": [{key: step.get(key) for key in ("migration", "domain", "epoch", "checkpoint")} for step in journal.get("steps", [])],
        "settings_retained": verify.digest(fixture.retained) == fixture.retained_hash,
        "project_retained": fixture.project_marker.read_text() == "project data\n",
        "materialized_config_matches": None if not history else Path(history["source"]).read_bytes() == Path(history["guest"]).read_bytes()}


def prepare_arm(verify, release, version, baseline_root, arch, workload, root, output, nominal_cycle=False, require_rust_setup=False):
    control = root / "incus-observation.json"
    verify.set_fixture_observation(control)
    if workload == "nominal-owner-inspection":
        fixture = verify.Fixture(root / "fixture", release, version, release, arch, version)
        launcher = fixture.runtime / "current/bin/yard"
        commands = [("inspect", [str(launcher), "migrate", "--check", "--json"])]
        if nominal_cycle:
            shims = fixture.root / "commands"
            shims.mkdir(mode=0o700)
            verify.write_observation_incus(shims, control, fixture.root / "unexpected-host-command", [launcher.parent.parent])
            fixture.env["PATH"] = str(shims) + ":/usr/bin:/bin"
            commands.extend([("execute", [str(launcher), "migrate", "--yes"]),
                             ("inspect-ready", [str(launcher), "migrate", "--check", "--json"])])
    else:
        baselines = {v: baseline_root / ("baseline-" + v) for v in verify.HISTORY_BASELINES}
        fixture, _, _ = verify.history_chain(release, version, arch, root, baselines,
                                             workload.startswith("checkpoint"), "fixture")
        verify.history_materialized_fixture(fixture, root, control)
        # Native custody binds the observed instance identity, unlike the older
        # Go fixture. Keep the same generic UUID in both reset arms.
        observation = json.loads(control.read_text())
        for instance in observation["instances"]:
            # The SDK exposes expanded config as InstanceInfo.Config.
            for field in ("config", "expanded_config"):
                instance["info"].setdefault(field, {})["volatile.uuid"] = "00000000-0000-4000-8000-000000000001"
        payload = json.dumps(observation)
        control.write_text(payload)
        deadline = time.monotonic() + 10
        applied = Path(str(control) + ".applied")
        while not applied.exists() or applied.read_text() != payload:
            verify.require(time.monotonic() < deadline, "fixture UUID observation was not applied")
            time.sleep(0.02)
        # Frozen arms can carry different profile scripts from this checkout.
        # Ask the existing observation providers for those exact package bytes.
        packaged_observations = root / "candidate-observations"
        with tarfile.open(release / f"subyard-{version}-linux-{arch}.tar.gz") as archive:
            archive.extractall(packaged_observations, filter="data")
        verify.write_observation_incus(fixture.root / "commands", control,
            fixture.root / "unexpected-host-command", [fixture.old_launcher.parent.parent, packaged_observations])
        # Existing authentic controlled failure leaves a durable resume grant.
        failed = verify.run_process([str(fixture.old_launcher), "update", "--version", version, "--yes"],
                                    fixture.env, timeout=180)
        (output / "setup-failure.stdout").write_text(failed.stdout)
        (output / "setup-failure.stderr").write_text(failed.stderr)
        shutil.copytree(fixture.config, output / "setup-config")
        write_json(output / "setup-observation.json", json.loads(control.read_text()))
        for marker in (*fixture.root.glob("unexpected-host-command*"), *root.glob("incus-observation.json.unexpected*")):
            shutil.copy2(marker, output / marker.name)
        verify.require(failed.returncode != 0 and Path(str(control) + ".failed").exists(),
                       "history setup did not reach controlled materialized apply failure")
        journal = json.loads(fixture.journal.read_text())
        verify.require(journal["checkpoint"] == "reconciling" and
                       all(step["checkpoint"] == "verified" for step in journal["steps"]),
                       "history failure occurred before verified migrations")
        custody = fixture.config / "migrations/native-custody/v1" / journal["transaction"]
        custody_files = {path.name: sha(path) for path in custody.glob("*.json")}
        write_json(output / "setup-owner-witness.json", {"actual_rust_pending_required": require_rust_setup,
            "native_custody_files": custody_files,
            "scope": "existence/hash witness only; fresh Rust inspection must validate original private records"})
        if require_rust_setup:
            verify.require("bundle.json" in custody_files and any(name.endswith(".intent.json") for name in custody_files)
                           and any(name.endswith(".terminal.json") for name in custody_files),
                           "history producer did not create authentic Rust bundle/native proofs; dependent inspection stopped")
        launcher = fixture.runtime / "current/bin/yard"
        commands = [("inspect-recovery", [str(launcher), "migrate", "--check", "--json"]),
                    ("resume", [str(launcher), "migrate", "--yes"]),
                    ("inspect-ready", [str(launcher), "migrate", "--check", "--json"])]
    return fixture, commands


def run_prepared(verify, fixture, commands, workload, root, output, interval, stats):
    control = root / "incus-observation.json"
    fixture.env.update({"SUBYARD_MEASURE_STATS_DIR": str(output / "allocations")} if stats else {})
    if workload == "nominal-owner-inspection":
        # Unaffected config loading is reported separately, never included in owner latency.
        config_metrics = command_measure([commands[0][1][0], "config", "show", "SSH_PORT"], fixture.env,
                                         output / "config-startup", interval)
        if config_metrics["exit"]:
            raise RuntimeError("config startup failed; see " + str(output))
    collected = []
    outcomes = []
    initial_effects = verify.history_effects(control)
    durable_keys = ("transaction", "authorizationPlan", "resumePlan", "observationScope")
    durable = {key: json.loads(fixture.journal.read_text()).get(key) for key in durable_keys} if fixture.journal.exists() else None
    shutil.copytree(fixture.config, output / "before-config")
    write_json(output / "fixture-inputs.json", {"config_hashes": verify.snapshot(fixture.config),
               "observation": json.loads(control.read_text()), "runtime_links": verify.history_links(fixture)})
    for name, command in commands:
        readonly = (verify.history_snapshot(fixture.config), verify.history_links(fixture), verify.history_effects(control)) if "inspect" in name else None
        phase = output / name
        try:
            item = command_measure(command, fixture.env, phase, interval)
            if fixture.journal.exists():
                shutil.copy2(fixture.journal, phase / "journal.json")
            verify.require(item["exit"] == 0, "product command failed; see " + str(phase))
        except BaseException:
            phase.mkdir(exist_ok=True)
            shutil.copytree(fixture.config, phase / "failed-config")
            write_json(phase / "failed-observation.json", json.loads(control.read_text()))
            write_json(phase / "failed-runtime-links.json", verify.history_links(fixture))
            (phase / "failed-native-effects.jsonl").write_bytes(verify.history_effects(control))
            raise
        collected.append(item)
        if "inspect" in name:
            verify.require(readonly == (verify.history_snapshot(fixture.config), verify.history_links(fixture), verify.history_effects(control)),
                           "read-only owner inspection changed protected or guest state")
            inspection = json.loads((output / name / "stdout").read_text())
            outcomes.append(semantics(fixture, inspection, control, verify))
            expected = "recovering" if name == "inspect-recovery" else "ready" if name == "inspect-ready" else "migration-required"
            if outcomes[-1]["outcome"]["status"] != expected:
                shutil.copytree(fixture.config, phase / "refused-config")
                write_json(phase / "refused-runtime-links.json", verify.history_links(fixture))
                raise RuntimeError("owner inspection refused expected " + expected + "; dependent commands stopped: " + str(phase))
    if workload != "nominal-owner-inspection":
        verify.require(outcomes[0]["outcome"]["status"] == "recovering" and
                       outcomes[-1]["outcome"]["status"] == "ready" and
                       outcomes[-1]["materialized_config_matches"], "history recovery semantic outcome is invalid")
        verify.require(durable == {key: json.loads(fixture.journal.read_text()).get(key) for key in durable_keys},
                       "fresh process resume changed its durable authorization/scope binding")
        verify.require(not (fixture.root / "unexpected-host-command").exists() and
                       not Path(str(control) + ".unexpected").exists(), "unexpected synthetic native command")
    effects = verify.history_effects(control)
    verify.require(not (fixture.root / "unexpected-host-command").exists() and
                   not Path(str(control) + ".unexpected").exists(), "unexpected synthetic native command")
    verify.require(effects.startswith(initial_effects), "owner command rewrote prior synthetic native effects")
    native_effects = [{key: event.get(key) for key in ("command", "stdinSHA256", "exit")}
                      for event in map(json.loads, effects[len(initial_effects):].splitlines())]
    write_json(output / "native-effects.json", native_effects)
    write_json(output / "semantics.json", outcomes)
    shutil.copytree(fixture.config, output / "after-config")
    return {"wall_seconds": sum(x["wall_seconds"] for x in collected),
            "cpu_seconds": sum(x["cpu_seconds"] for x in collected),
            "peak_sampled_tree_rss_bytes": max(x["peak_sampled_tree_rss_bytes"] for x in collected),
            "cpu_complete": all(x["cpu_complete"] for x in collected),
            "rust_owner_observed": all(x["rust_owner_observed"] for x in collected),
            "rust_owner_observed_any": any(x["rust_owner_observed"] for x in collected),
            "native_effects": native_effects, "semantics": outcomes}


def run_arm(verify, release, version, baseline_root, arch, workload, root, output, interval, stats, nominal_cycle=False, require_rust_setup=False):
    fixture, commands = prepare_arm(verify, release, version, baseline_root, arch, workload, root, output, nominal_cycle, require_rust_setup)
    return run_prepared(verify, fixture, commands, workload, root, output, interval, stats)


@contextlib.contextmanager
def fixture_root(server_binary, output):
    """Keep the observation server outside each isolated product collector."""
    with tempfile.TemporaryDirectory(prefix="subyard-owner-measure-", dir="/tmp") as temporary:
        root = Path(temporary)
        control = root / "incus-observation.json"
        control.write_text("{}")
        with subprocess.Popen([str(server_binary), str(root), str(control)],
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True) as server:
            try:
                readable, _, _ = select.select([server.stdout], [], [], 10)
                if not readable or server.stdout.readline().strip() != "ready":
                    raise RuntimeError("fixture server not ready")
                yield root
            finally:
                server.terminate()
                server.wait(timeout=10)
                stdout, stderr = server.communicate()
                write_json(output / "observer-native.json", {"exit": server.returncode, "stdout": stdout, "stderr": stderr})


class FixtureReset:
    """Exact-path experiment: keep directories, expose changed file identities."""
    def __init__(self, fixture, root):
        self.fixture, self.root = fixture, root
        self.excluded = {fixture.runtime / "releases", fixture.data / "releases", root / "incus-observation.json.applied"}
        self.entries = self.capture()
        self.digest = hashlib.sha256(json.dumps(self.public(self.entries), sort_keys=True).encode()).hexdigest()

    def paths(self):
        chosen = []
        for path in (self.fixture.root, *self.fixture.root.rglob("*"), *self.root.glob("incus-observation.json*")):
            if not any(path == prefix or path.is_relative_to(prefix) for prefix in self.excluded):
                chosen.append(path)
        return chosen

    def capture(self):
        entries = {}
        for path in self.paths():
            info = path.lstat()
            kind = "link" if path.is_symlink() else "directory" if path.is_dir() else "file"
            if kind == "file" and not stat.S_ISREG(info.st_mode):
                raise RuntimeError("unexpected special file in reset fixture")
            entries[str(path.relative_to(self.root))] = {
                "kind": kind, "mode": info.st_mode, "uid": info.st_uid, "gid": info.st_gid,
                "device": info.st_dev, "inode": info.st_ino, "links": info.st_nlink,
                "mtime_ns": info.st_mtime_ns, "atime_ns": info.st_atime_ns,
                "payload": os.readlink(path) if kind == "link" else path.read_bytes() if kind == "file" else None}
        return entries

    @staticmethod
    def public(entries):
        return {name: {**{key: value for key, value in entry.items() if key not in ("payload", "atime_ns")},
                       "payload_sha256": hashlib.sha256(entry["payload"]).hexdigest() if isinstance(entry["payload"], bytes) else entry["payload"]}
                for name, entry in entries.items()}

    def restore(self):
        current = self.capture()
        for name in sorted(current.keys() - self.entries.keys(), key=lambda value: (value.count("/"), value), reverse=True):
            path = self.root / name
            path.rmdir() if current[name]["kind"] == "directory" else path.unlink()
        for name, entry in sorted(self.entries.items(), key=lambda item: (item[0].count("/"), item[0])):
            path = self.root / name
            if entry["kind"] == "directory":
                # Changing directory identity invalidates this reset experiment.
                if not path.is_dir() or path.is_symlink() or path.stat().st_ino != entry["inode"]:
                    raise RuntimeError("reset lost original directory identity: " + name)
            elif entry["kind"] == "link":
                if not path.is_symlink() or os.readlink(path) != entry["payload"]:
                    if path.exists() or path.is_symlink(): path.unlink()
                    path.symlink_to(entry["payload"])
            else:
                if path.is_symlink(): path.unlink()
                if not path.exists() or path.read_bytes() != entry["payload"]:
                    path.write_bytes(entry["payload"])
                path.chmod(stat.S_IMODE(entry["mode"]))
        # Restore mtimes after directory/file publications. IDs are never forged.
        for name, entry in self.entries.items():
            os.utime(self.root / name, ns=(entry["atime_ns"], entry["mtime_ns"]), follow_symlinks=False)
        after = self.capture()
        changed_ids = []
        for name, entry in self.entries.items():
            restored = after[name]
            for key in ("kind", "mode", "uid", "gid", "payload", "mtime_ns"):
                if restored[key] != entry[key]:
                    raise RuntimeError("reset failed to restore " + key + ": " + name)
            if (restored["device"], restored["inode"], restored["links"]) != (entry["device"], entry["inode"], entry["links"]):
                changed_ids.append(name)
        return {"snapshot_sha256": self.digest, "changed_file_identities": changed_ids,
                "root_directory_identities_preserved": True,
                "exact_bytes_modes_owners_mtimes_restored": True,
                "full_readset_identity_equal": not changed_ids,
                "requires_fresh_binding_validation": True}


def package_binding(release, version, arch):
    bundle = release / f"subyard-{version}-linux-{arch}.tar.gz"
    paths = [bundle, Path(str(bundle) + ".manifest.json"), Path(str(bundle) + ".provenance.json"),
             release / "subyard-install-runtime-release.sh"]
    result = {"files": {path.name: sha(path) for path in paths}}
    with tarfile.open(bundle) as archive:
        names = {member.name.removeprefix("./"): member for member in archive.getmembers()}
        for name in ("config/release-owner.json", "config/release-owner-build.json"):
            if name in names:
                result[name] = json.loads(archive.extractfile(names[name]).read())
    return result


def main():
    if len(sys.argv) == 3 and sys.argv[1] == "--collect":
        print(json.dumps(collect(json.loads(Path(sys.argv[2]).read_text()))))
        return
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go-release", type=Path, required=True)
    parser.add_argument("--go-version", required=True)
    parser.add_argument("--rust-release", type=Path, required=True)
    parser.add_argument("--rust-version", required=True)
    parser.add_argument("--baselines", type=Path, required=True)
    parser.add_argument("--observation-server", type=Path, required=True, help="built internal/testkit/cmd/empty-incus")
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--pairs", type=int, default=100)
    parser.add_argument("--seed", type=int, default=20261018)
    parser.add_argument("--interval", type=float, default=0.005)
    parser.add_argument("--workload", choices=WORKLOADS, action="append")
    parser.add_argument("--instrumented", action="store_true", help="separate allocation experiment, never product acceptance")
    parser.add_argument("--preflight", action="store_true", help="plumbing only with fewer than 100 pairs; actual Rust census remains mandatory")
    parser.add_argument("--fixture-lifecycle", choices=("reset", "fresh"), default="reset")
    parser.add_argument("--supporting-copy-receipt", type=Path, help="bind preliminary copy-only routing provenance; not final acceptance")
    parser.add_argument("--nominal-cycle", action="store_true", help="preflight-only Inspect/Execute/fresh Inspect diagnostic; does not change the declared inspection workload")
    args = parser.parse_args()
    if not 1 <= args.pairs <= 10000 or not 0.001 <= args.interval <= 0.1:
        parser.error("pairs must be 1..10000 and interval 0.001..0.1 seconds")
    if args.pairs < 100 and not args.preflight:
        parser.error("fewer than 100 pairs requires --preflight")
    if args.supporting_copy_receipt and not args.preflight:
        parser.error("copy-only routing receipts require --preflight; never final product acceptance")
    if args.nominal_cycle and not args.preflight:
        parser.error("--nominal-cycle is an exploratory preflight variant only")
    if platform.system() != "Linux":
        parser.error("Linux collector required")
    os.umask(0o077)
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    arch = {"x86_64": "amd64", "aarch64": "arm64"}[platform.machine()]
    releases = {"go": (args.go_release.resolve(), args.go_version), "rust": (args.rust_release.resolve(), args.rust_version)}
    plan = {"policy": POLICY, "seed": args.seed, "pairs": args.pairs, "workloads": args.workload or list(WORKLOADS),
            "mode": "instrumented-allocations" if args.instrumented else "preliminary-packaged-plumbing" if args.preflight else "normal-packaged-product",
            "preflight_only": args.preflight,
            "fixture_lifecycle": args.fixture_lifecycle,
            "nominal_preflight_cycle": args.nominal_cycle,
            "supporting_copy_receipt": None if not args.supporting_copy_receipt else {
                "sha256": sha(args.supporting_copy_receipt), "receipt": json.loads(args.supporting_copy_receipt.read_text())},
            "measure_script_sha256": sha(Path(__file__)),
            "fixture_script_sha256": sha(ROOT / "dev/verify-release-upgrades.py"),
            "tree_script_sha256": sha(ROOT / "dev/measure-veranda.py"),
            "observation_server_sha256": sha(args.observation_server.resolve()),
            "observation_provider_sha256": {str(path.relative_to(ROOT)): sha(path)
                for owner in ("agents", "profiles", "retired-integrations")
                for path in sorted(ROOT.glob(f"config/{owner}/*/tests/release-observation.py"))},
            "packages": {owner: package_binding(*release, arch) for owner, release in releases.items()},
            "aggregation": "owner commands sum wall and CPU; max sampled simultaneous RSS; config-startup excluded"}
    # Write the fixed policy and order BEFORE the first result.
    rng = random.Random(args.seed)
    orders = {workload: [rng.sample(["go", "rust"], 2) for _ in range(args.pairs)] for workload in plan["workloads"]}
    plan["orders"] = orders
    write_json(output / "plan.json", plan)
    verify = module("release_upgrade_fixture", ROOT / "dev/verify-release-upgrades.py")
    summaries = {}
    try:
        for workload, order in orders.items():
            pairs = []
            with contextlib.ExitStack() as stack:
                prepared = {}
                if args.fixture_lifecycle == "reset":
                    for owner in releases:
                        setup = output / workload / "prepared" / owner
                        setup.mkdir(parents=True)
                        root = stack.enter_context(fixture_root(args.observation_server.resolve(), setup))
                        started = time.monotonic()
                        fixture, commands = prepare_arm(verify, *releases[owner], args.baselines.resolve(), arch, workload, root, setup, args.nominal_cycle, owner == "rust")
                        state = FixtureReset(fixture, root)
                        write_json(setup / "initial-state.json", state.public(state.entries))
                        write_json(setup / "preparation.json", {"setup_seconds": time.monotonic() - started,
                            "snapshot_sha256": state.digest, "excluded_from_product_timing": True})
                        prepared[owner] = fixture, commands, state, root
                for number, owners in enumerate(order):
                    pair = {}
                    for owner in owners:
                        directory = output / workload / f"pair-{number:04d}" / owner
                        directory.mkdir(parents=True)
                        if args.fixture_lifecycle == "reset":
                            fixture, commands, state, root = prepared[owner]
                            started = time.monotonic()
                            reset = state.restore()
                            reset["reset_seconds"] = time.monotonic() - started
                            reset["excluded_from_product_timing"] = True
                            write_json(directory / "reset.json", reset)
                            pair[owner] = run_prepared(verify, fixture, commands, workload, root, directory, args.interval, args.instrumented)
                        else:
                            with fixture_root(args.observation_server.resolve(), directory) as root:
                                pair[owner] = run_arm(verify, *releases[owner], args.baselines.resolve(), arch,
                                                     workload, root, directory, args.interval, args.instrumented, args.nominal_cycle, owner == "rust")
                        if owner == "rust":
                            verify.require(pair[owner]["rust_owner_observed"],
                                "Rust operation entrypoint missing from actual command census; owner route is unverified")
                        else:
                            verify.require(not pair[owner]["rust_owner_observed_any"],
                                "Go baseline unexpectedly executed the Rust operation entrypoint")
                        if pairs:
                            verify.require(pair[owner]["semantics"] == pairs[0][owner]["semantics"] and
                                pair[owner]["native_effects"] == pairs[0][owner]["native_effects"],
                                "reset changed semantic/native outcomes for " + owner)
                        write_json(directory / "result.json", pair[owner])
                    verify.require(pair["go"]["semantics"] == pair["rust"]["semantics"],
                                   "paired semantic outcomes differ: " + str(directory.parent))
                    verify.require(pair["go"]["native_effects"] == pair["rust"]["native_effects"],
                                   "paired synthetic native effects differ: " + str(directory.parent))
                    pairs.append(pair)
                    write_json(directory.parent / "pair.json", {"order": owners, **pair})
                    print(json.dumps({"workload": workload, "completed_pairs": len(pairs)}), flush=True)
            if args.instrumented:
                summaries[workload] = {"pairs": len(pairs), "status": "INCONCLUSIVE",
                    "allocation_reports": sorted(str(path.relative_to(output)) for path in (output / workload).rglob("allocation-summary.json")),
                    "limitations": ["instrumented timings are not product latency; no product bootstrap comparison computed",
                                    "killed helpers may have no final allocation snapshot; whole-tree totals are inconclusive"]}
            elif args.preflight:
                summaries[workload] = {"pairs": len(pairs), "status": "INCONCLUSIVE",
                    "scope": "preliminary plumbing only; no product bootstrap comparison computed",
                    "fixture_lifecycle": args.fixture_lifecycle}
            else:
                summaries[workload] = comparison(pairs, args.seed)
            summaries[workload]["rust_owner_observed_in_all_commands"] = {
                owner: all(pair[owner]["rust_owner_observed"] for pair in pairs) for owner in ("go", "rust")}
            write_json(output / "summary.json", summaries)
    except BaseException as error:
        write_json(output / "failed.json", {"error": str(error), "completed_workloads": summaries})
        raise


if __name__ == "__main__":
    main()
