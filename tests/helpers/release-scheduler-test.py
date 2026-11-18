#!/usr/bin/env python3
"""Host-free local collector races and broker admission with a deterministic clock."""
from concurrent.futures import Future
from contextlib import redirect_stdout
import argparse
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("acceptance", ROOT / "dev/release-acceptance.py")
a = importlib.util.module_from_spec(spec)
spec.loader.exec_module(a)

PASSED = {"status": "passed", "exit_code": 0}
FAILED = {"status": "failed", "exit_code": 23}
REFUSED = {"status": "blocked", "exit_code": 4, "termination": "exit", "reason": "capacity_memory"}
ATTEMPT = {"id": "a" * 32, "sha256": "b" * 64}


class Clock:
    def __init__(self):
        self.now = 0
        self.stop_at = float("inf")
        self.running = []
        self.events = []
        self.crash = False
        self.stop_before_launch = False

    def is_set(self):
        return self.now >= self.stop_at

    def elapse(self, seconds):
        self.now += seconds
        for at, callback in self.events[:]:
            if at <= self.now:
                self.events.remove((at, callback))
                callback()
        for at, future, result in self.running[:]:
            if at <= self.now:
                future.set_result(result)
                self.running.remove((at, future, result))

    def wait(self, seconds):
        self.elapse(min(seconds, self.stop_at - self.now))

    def wait_active(self, futures, timeout, **kwargs):
        self.wait(min(timeout, *(at - self.now for at, future, _ in self.running if future in futures)))

    def drain(self, futures):
        yield from [future for future in futures if future.done()]
        for at, future, _ in sorted(self.running, key=lambda item: item[0]):
            self.elapse(max(0, at - self.now))
            yield future

    def submit(self, execute, *args):
        if self.crash:
            raise SystemExit("after reservation, before spawn")
        if self.stop_before_launch:
            self.stop_at = self.now
        outcome = execute(*args)
        duration, result = (0, None) if outcome is None else outcome
        future = Future()
        self.running.append((self.now + duration, future, result))
        return future


class Scheduler(unittest.TestCase):
    def fixture(self, names, *, states, controller=lambda *_: (0, PASSED), capacity="4s",
                outcomes=None, requires=None, slots=(2, 4)):
        clock = Clock()
        output = Path(self.enterContext(tempfile.TemporaryDirectory(dir="/tmp")))
        checks = dict.fromkeys(("verify", *names), ["true"])
        a.save(output, {"version": "test", "profiles": {}, "source_fingerprint": "a" * 64,
                        "candidate_bundle_sha256": "b" * 64, "external_obligations": {},
                        "requirements": requires or {}, "checks": {"verify": {"status": "passed"},
                            **{name: {"status": "pending"} for name in names}, **(outcomes or {})}})
        calls = []
        def launch(name, command, root, output, env, slot=None, **kwargs):
            calls.append((clock.now, name, slot))
            if slot and kwargs.get("local_slot"):
                self.assertEqual(env["SUBYARD_P0_WAIT_SECONDS"], "0")
                entry = read()["local_schedule"][name]
                self.assertEqual((entry["state"], entry["slot"]), ("running", slot))
                self.assertEqual(read()["checks"][name]["status"], "running")
            duration, result = controller(name, slot, clock.now)
            return (duration, dict(result)) if slot and kwargs.get("local_slot") else dict(result)

        def read():
            return json.loads((output / "receipt.json").read_text())

        pool = self.enterContext(patch.object(a, "ThreadPoolExecutor")).return_value.__enter__.return_value
        pool.submit.side_effect = clock.submit
        self.enterContext(patch.multiple(a, STOP=clock, SLOT_POLL_SECONDS=2, time=SimpleNamespace(monotonic=lambda: clock.now),
            wait=clock.wait_active, as_completed=clock.drain,
            load_bound=lambda _: (output, read()), commands=lambda *args: checks,
            execution_environment=lambda: {}, source_fingerprint=lambda _: "a" * 64,
            broker_slots=lambda _: states(clock.now), execute=launch, completed_attempt=lambda *args: dict(ATTEMPT)))
        self.enterContext(patch.dict(os.environ, {"SUBYARD_E2E_CAPACITY_WAIT": capacity, "SUBYARD_P0_WAIT_SECONDS": "1200"}))
        self.enterContext(redirect_stdout(io.StringIO()))
        return clock, output, calls, read, lambda only=(), rerun=False, with_vms=True, **opts: a.run_checks(
            output, slots, only, rerun, with_vms=with_vms, **opts)

    def test_slot_selection_and_typed_refusals(self):
        parser = argparse.ArgumentParser()
        a.add_slot_arguments(parser)
        for argv, expected in (
            ([], (1, 2, 3, 4)), (["--slots", "slot-2,slot-4"], (2, 4)),
            (["--slot-count", "2"], (1, 2)), (["--slots", "2"], None),
            (["--slots", "slot-1,slot-001"], None), (["--slot-count", "0"], None),
            (["--slots", "slot-2", "--slots", "slot-4"], None),
            (["--slot-count", "2", "--slot-count", "2"], None),
            (["--slots", "slot-2", "--slot-count", "2"], None),
        ):
            with self.subTest(argv=argv):
                if expected is None:
                    with patch("sys.stderr", new_callable=io.StringIO), self.assertRaises(SystemExit):
                        parser.parse_args(argv)
                else:
                    self.assertEqual(a.selected_slots(parser.parse_args(argv)), expected)

        log = Path(self.enterContext(tempfile.TemporaryDirectory(dir="/tmp"))) / "attempt.log"
        for code, slot, reason, message in (
            (2, 2, "slot_held", "requested E2E slot slot-002 is unavailable: state=held reason=busy; owner=test"),
            (4, 2, "slot_quarantined", "timed out waiting for slot-002: state=quarantined reason=quarantined"),
            (4, 1, None, "timed out waiting for slot-002: state=quarantined reason=quarantined"),
            (4, 2, "capacity_disk", "retryable capacity refusal for slot-2: resource=disk; retry after resources become available"),
            (4, 2, None, "retryable capacity refusal for slot-4: resource=disk; retry after resources become available"),
            (23, 2, None, "timed out waiting for slot-002: state=quarantined reason=quarantined"),
        ):
            with self.subTest(code=code, slot=slot, message=message):
                log.write_text(f"agent-e2e: {message}\n")
                self.assertEqual(a.broker_refusal(log, slot, code), reason)

    def test_orphaned_controller_retains_collector_lock(self):
        output = Path(self.enterContext(tempfile.TemporaryDirectory(dir="/tmp")))
        ready, finish = output / "ready", output / "finish"
        worker = """
import importlib.util
import os
from pathlib import Path
import sys
spec = importlib.util.spec_from_file_location("acceptance", sys.argv[1])
a = importlib.util.module_from_spec(spec)
spec.loader.exec_module(a)
output = Path(sys.argv[2])
with a.acceptance_lock(output) as guard:
    a.execute("vm:lane", ["bash", "-c", 'touch "$1"; while [ ! -f "$2" ]; do sleep .02; done',
                        "fixture", sys.argv[3], sys.argv[4]], output, output, os.environ, 2,
              local_slot=True, guard=guard)
"""
        process = subprocess.Popen([sys.executable, "-B", "-c", worker, str(ROOT / "dev/release-acceptance.py"),
                                    str(output), str(ready), str(finish)], stdout=subprocess.DEVNULL)
        try:
            deadline = time.monotonic() + 5
            while not ready.exists() and process.poll() is None and time.monotonic() < deadline:
                time.sleep(.02)
            self.assertTrue(ready.exists())
            process.kill()
            process.wait(timeout=5)
            with self.assertRaisesRegex(ValueError, "candidate is already in use"):
                with a.acceptance_lock(output):
                    pass
        finally:
            finish.touch()
            process.wait(timeout=5)
        deadline = time.monotonic() + 5
        while True:
            try:
                with a.acceptance_lock(output):
                    break
            except ValueError:
                if time.monotonic() >= deadline:
                    raise
                time.sleep(.02)

    def test_admission_rounds_and_current_diagnostics(self):
        available = {2: "available", 4: "available"}
        cases = (
            ("held", "4s", lambda t: {2: "held", 4: "held"}, (), 4, "slot-2=held, slot-4=held"),
            ("missing", "4s", lambda t: {}, (), 4, "slot-2=unknown, slot-4=unknown"),
            ("last known", "2s", lambda t: {2: "quarantined"} if not t else {}, (), 2,
             "slot-2=unknown (last known: quarantined), slot-4=unknown"),
            ("closed", "2s", lambda t: {2: "admission-closed"}, (), 2, "slot-2=admission-closed"),
            ("stable refusal", "4s", lambda t: available, (2, 4), 2, "slot-2=available, slot-4=available"),
            ("flapping", "6s", lambda t: available if t in (0, 4) else {2: "held"}, (2, 4), 4, "slot-2="),
            ("zero unavailable", "0s", lambda t: {}, (), 0, "slot-2=unknown"),
            ("zero refusal", "0s", lambda t: available, (2,), 0, "slot-2=available"),
        )
        for label, capacity, states, launches, elapsed, diagnostic in cases:
            with self.subTest(label=label):
                clock, _, calls, read, run = self.fixture(["vm:lane"], states=states,
                    controller=lambda *_: (0, REFUSED), capacity=capacity)
                self.assertEqual(run(), 1)
                self.assertEqual(tuple(slot for _, _, slot in calls), launches)
                self.assertEqual(clock.now, elapsed)
                entry = read()["local_schedule"]["vm:lane"]
                self.assertEqual((entry["state"], entry["remaining"]), ("exhausted", 0))
                self.assertIn(diagnostic, entry["broker"])
                self.assertEqual(entry["reason"], "capacity_memory" if launches else "broker-no-progress")
                self.assertEqual(run(), 1)
                self.assertEqual(read()["local_schedule"]["vm:lane"], entry)
                self.assertEqual(len(calls), len(launches))

    def test_status_validates_inventory_and_closed_admission(self):
        def response(slots, **pool):
            return json.dumps({"schema_version": 1, "status": "ok",
                               "pool": {"slots": slots, **pool}}).encode()
        for payload, expected in (
            (response([{"slot_id": "slot-002", "state": "available"}], admission_closed=True), {2: "admission-closed"}),
            (response([{"slot_id": "slot-004", "state": "quarantined"}]), {4: "quarantined"}),
            (response([{"slot_id": "not-a-slot", "state": "available"}]), {}),
            (b"no status", {}),
        ):
            with self.subTest(expected=expected), patch.object(a.subprocess, "check_output", return_value=payload) as probe:
                self.assertEqual(a.broker_slots({}), expected)
                self.assertEqual(probe.call_args.args[0][:3], ["timeout", "--kill-after=1", str(a.SLOT_POLL_SECONDS - 1)])

    def test_recovery_during_sibling_and_fairness_after_refusal(self):
        calls_expected = [(0, "vm:slow", 2), (2, "vm:large", 4), (2, "vm:small", 4), (4, "vm:large", 4)]
        _, _, calls, read, run = self.fixture(
            ["vm:slow", "vm:large", "vm:small"],
            states=lambda t: {2: "available", 4: "available" if t >= 2 else "quarantined"},
            controller=lambda name, slot, t: (8, PASSED) if name == "vm:slow" else
                (0, REFUSED if name == "vm:large" and t == 2 else PASSED))
        self.assertEqual(run(), 0)
        self.assertEqual(calls, calls_expected)
        self.assertEqual(read()["local_schedule"]["vm:large"]["remaining"], 0)
        self.assertEqual(read()["local_schedule"]["vm:small"]["remaining"], 1)

    def test_crash_reservation_and_restart_during_cleanup(self):
        for capacity in ("0s", "4s"):
            with self.subTest(capacity=capacity):
                clock, _, calls, read, run = self.fixture(["vm:lane"], capacity=capacity,
                    states=lambda t: {2: "recovering" if clock.crash_recovered and t < 2 else "available",
                                      4: "available"})
                clock.crash_recovered = False
                clock.crash = True
                with self.assertRaises(SystemExit):
                    run()
                entry = read()["local_schedule"]["vm:lane"]
                self.assertEqual((entry["state"], entry["slot"]), ("running", 2))
                self.assertEqual(calls, [])
                clock.crash = False
                clock.crash_recovered = True
                self.assertEqual(run(), 1 if capacity == "0s" else 0)
                after = read()["local_schedule"]["vm:lane"]
                self.assertEqual(after["id"], entry["id"])
                self.assertEqual(after["remaining"], 0)
                self.assertEqual(calls, [] if capacity == "0s" else [(2, "vm:lane", 4)])

    def test_stop_after_reservation_and_before_launch_restores_the_round(self):
        lane = "vm:lane"
        clock, output, calls, read, run = self.fixture([lane], capacity="0s", states=lambda _: {2: "available"})
        clock.stop_before_launch = True
        self.assertEqual(run(), 1)
        self.assertEqual(calls, [])
        first = read()["local_schedule"][lane]
        self.assertEqual((first["state"], first["remaining"]), ("queued", 1))
        self.assertEqual(read()["checks"][lane]["status"], "pending")
        invocation = json.loads(next((output / "runs").glob("*.json")).read_text())
        self.assertEqual(invocation["not_started"], [{"check": lane, "reason": "interrupted-before-dispatch"}])
        clock.stop_before_launch = False
        clock.stop_at = float("inf")
        self.assertEqual(run(), 0)
        self.assertEqual(calls, [(0, lane, 2)])
        self.assertEqual(read()["local_schedule"][lane]["id"], first["id"])

    def test_submitted_retry_resumes_after_preflight_blocks_an_ordinary_run(self):
        lane, preflight = "vm:lane", "profile-preflight:sample"
        preflight_result = FAILED
        def controller(name, *_):
            return 0, preflight_result if name == preflight else PASSED

        _, output, calls, read, run = self.fixture([lane, preflight], capacity="0s",
            states=lambda _: {2: "available"}, controller=controller,
            outcomes={lane: dict(FAILED, original_attempt=ATTEMPT)})
        a.request_retry(output, lane, ATTEMPT["id"])
        self.assertEqual(run(with_vms=False), 1)
        first = read()["local_schedule"][lane]
        self.assertEqual((first["id"], first["state"], first["remaining"]), (ATTEMPT["id"], "queued", 1))
        preflight_result = PASSED
        self.assertEqual(run(with_vms=False), 0)
        self.assertEqual([name for _, name, _ in calls], [preflight, preflight, lane])
        last = read()["local_schedule"][lane]
        self.assertEqual((last["id"], last["state"], last["remaining"]), (ATTEMPT["id"], "done", 0))

    def test_ordinary_run_resumes_queued_selection_by_identity(self):
        lane = "vm:lane"
        for prior in (PASSED, {"status": "pending"}):
            with self.subTest(prior=prior):
                clock, output, calls, read, run = self.fixture([lane], outcomes={lane: prior},
                    states=lambda now: {2: "held" if now == 0 else "available"})
                clock.stop_at = 1
                self.assertEqual(run(only=[lane], rerun=prior == PASSED), 1)
                first = read()["local_schedule"][lane]
                self.assertEqual((first["state"], first["remaining"]), ("queued", 2))
                self.assertEqual(read()["checks"][lane], prior)
                self.assertEqual(read()["result"], "incomplete")
                self.assertEqual(calls, [])
                self.assertFalse((output / "retries").exists())

                clock.stop_at = float("inf")
                self.assertEqual(run(with_vms=False), 0)
                self.assertEqual(calls, [(1, lane, 2)])
                self.assertEqual(read()["local_schedule"][lane]["id"], first["id"])

    def test_hosted_wait_and_same_phase_retry_use_latest_local_outcomes(self):
        lane, sibling, hosted = "vm:lane", "vm:sibling", "vm:hosted"
        for retry_during in ("sibling", "hosted start", "hosted wait"):
            with self.subTest(retry_during=retry_during):
                during_sibling = retry_during == "sibling"
                def controller(name, slot, now):
                    if name == sibling:
                        return 8, PASSED
                    return 0, FAILED if during_sibling and name == lane and now == 0 else PASSED

                outcomes = {} if during_sibling else {lane: dict(FAILED, original_attempt=ATTEMPT), sibling: PASSED}
                clock, output, calls, read, run = self.fixture([lane, sibling, hosted], outcomes=outcomes,
                    states=lambda _: {2: "available", 4: "available"}, controller=controller, capacity="0s")
                def submit_retry():
                    a.request_retry(output, lane, ATTEMPT["id"])

                late_at = 0 if retry_during == "hosted start" else 4
                if late_at:
                    clock.events.append((late_at, submit_retry))
                hosted_calls = []

                def collect(failed, *_):
                    hosted_calls.append((clock.now, failed))
                    if not late_at:
                        submit_retry()
                    return 20, {}  # The hosted lane falls back to local execution.

                self.assertEqual(run(only=[lane, sibling, hosted] if during_sibling else [hosted],
                                     hosted=[hosted], collect_hosted=collect), 0)
                self.assertEqual(hosted_calls, [(8 if during_sibling else 0, False)])
                initial = [(0, lane, 2), (0, sibling, 4)] if during_sibling else []
                self.assertEqual(calls, [*initial, (late_at, lane, 2), (28 if during_sibling else 20, hosted, 2)])
                self.assertEqual(read()["local_schedule"][lane]["id"], ATTEMPT["id"])
                self.assertFalse((output / "retries").exists())

    def test_stop_or_capture_failure_drains_and_records_siblings(self):
        for mode in ("stop", "capture"):
            with self.subTest(mode=mode):
                clock, _, calls, read, run = self.fixture(["vm:first", "vm:sibling", "vm:queued"],
                    states=lambda _: {2: "available", 4: "available"},
                    controller=lambda name, *_: (1 if name == "vm:first" else 4, PASSED))
                if mode == "stop":
                    clock.stop_at = 1
                    self.assertEqual(run(), 1)
                else:
                    def capture(output, root, receipt, name, result):
                        if name == "vm:first":
                            raise OSError("capture failed")
                        return dict(ATTEMPT)
                    with patch.object(a, "completed_attempt", side_effect=capture), self.assertRaises(OSError):
                        run()
                self.assertEqual(calls, [(0, "vm:first", 2), (0, "vm:sibling", 4)])
                self.assertEqual(read()["checks"]["vm:sibling"]["status"], "passed")
                self.assertEqual(read()["checks"]["vm:queued"]["status"], "pending")

    def test_late_duplicate_retry_and_dispatch_prerequisites(self):
        split, owner, sibling, pre = "vm:split", "vm:owner", "vm:sibling", "profile-preflight:sample"
        for mode in ("ready", "already queued", "failed preflight", "excluded preflight", "failed split"):
            with self.subTest(mode=mode):
                def controller(name, *_):
                    if name == "verify":
                        a.request_retry(output, owner, ATTEMPT["id"])
                        a.request_retry(output, owner, ATTEMPT["id"])
                    return 0, FAILED if name == split and mode == "failed split" else PASSED

                _, output, calls, read, run = self.fixture([owner, split, sibling, pre],
                    states=lambda t: {2: "available", 4: "available"}, controller=controller,
                    outcomes={"verify": {"status": "pending"}, owner: dict(FAILED, original_attempt=ATTEMPT),
                              sibling: PASSED, pre: FAILED if mode == "failed preflight" else PASSED},
                    requires={owner: [split]})
                options = {"exclude": [pre]} if mode == "excluded preflight" else {}
                only = ["verify", owner] if mode == "already queued" else ["verify"]
                success = mode in ("ready", "already queued")
                self.assertEqual(run(only=only, **options), 0 if success else 1)
                expected = ["verify", split, owner] if success else (
                    ["verify", split] if mode == "failed split" else ["verify"])
                self.assertEqual([name for _, name, _ in calls], expected)
                receipt = read()
                self.assertEqual(receipt["local_schedule"][owner]["id"], ATTEMPT["id"])
                self.assertEqual(receipt["checks"][sibling], PASSED)
                if success:
                    a.request_retry(output, owner, ATTEMPT["id"])
                    self.assertEqual(run(only=[owner]), 0)
                    self.assertEqual([name for _, name, _ in calls], expected)

    def test_split_rerun_precedes_high_priority_owner_despite_old_pass(self):
        split, owner, sibling = "vm:split", "vm:owner", "vm:sibling"
        for result in (PASSED, FAILED):
            with self.subTest(split=result):
                _, _, calls, read, run = self.fixture([owner, split, sibling],
                    states=lambda t: {2: "available", 4: "available"},
                    outcomes={split: PASSED, sibling: PASSED}, requires={owner: [split]},
                    controller=lambda name, *_: (2, result) if name == split else (0, PASSED))
                self.assertEqual(run(only=[owner, split], rerun=True, first=[owner]), 0 if result == PASSED else 1)
                self.assertEqual(calls, [(0, split, 2), (2, owner, 2)] if result == PASSED else [(0, split, 2)])
                self.assertEqual(read()["checks"][sibling], PASSED)

    def test_hosted_single_job_keeps_native_wait_and_memory_disk_retry(self):
        for resource in ("memory", "disk"):
            with self.subTest(resource=resource):
                count = 0
                def controller(name, slot, _):
                    nonlocal count
                    count += 1
                    return 0, dict(REFUSED, reason="capacity_" + resource) if count == 1 else PASSED
                _, _, calls, read, run = self.fixture(["vm:hosted"], controller=controller,
                    states=lambda t: self.fail("hosted job polled for local slots"), slots=(1,), capacity="30m")
                execute = a.execute
                def observe(name, command, root, output, env, slot):
                    self.assertEqual(env["SUBYARD_E2E_CAPACITY_WAIT"], "30m")
                    self.assertEqual(env["SUBYARD_P0_WAIT_SECONDS"], "1200")
                    return execute(name, command, root, output, env, slot)
                with patch.object(a, "execute", side_effect=observe):
                    self.assertEqual(run(only=["vm:hosted"], hosted_job=True), 0)
                self.assertEqual(calls, [(0, "vm:hosted", 1)] * 2)
                self.assertNotIn("local_schedule", read())


if __name__ == "__main__":
    unittest.main()
