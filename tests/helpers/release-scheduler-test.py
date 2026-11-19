#!/usr/bin/env python3
"""Host-free tests for local release acceptance dispatch."""
import argparse
from concurrent.futures import Future
from contextlib import contextmanager, redirect_stdout
import importlib.util
import io
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location(
    "acceptance", Path(__file__).resolve().parents[2] / "dev/release-acceptance.py")
acceptance = importlib.util.module_from_spec(spec)
spec.loader.exec_module(acceptance)

PASSED = {"status": "passed"}
FAILED = {"status": "failed", "exit_code": 3}
BLOCKED = {"status": "blocked", "exit_code": 4, "termination": "exit", "reason": "capacity_memory"}


class FakeControllers:
    def __init__(self, attempts, broker=None):
        self.attempts = {name: iter(results) for name, results in attempts.items()}
        self.broker = broker or (lambda _: dict.fromkeys((1, 2, 3, 4), "available"))
        self.now = 0
        self.started = []
        self.probes = []
        self.active = {}

    def __enter__(self):
        return self

    def __exit__(self, *_):
        pass

    def submit(self, attempt, name, slot):
        assert slot not in self.active
        self.started.append((self.now, name, slot))
        duration, result = next(self.attempts[name])
        future = Future()
        if callable(result):
            result = result(self.now, slot)
        self.active[slot] = (self.now + duration, future, attempt, name, result)
        return future

    def advance(self, seconds):
        deadline = self.now + seconds
        self.now = min([deadline, *(end for end, *_ in self.active.values())])
        for slot, (end, future, attempt, name, result) in list(self.active.items()):
            if end <= self.now:
                del self.active[slot]
                try:
                    self.result = result
                    future.set_result(attempt(name, slot))
                except Exception as error:
                    future.set_exception(error)

    def wait(self, futures, *, timeout=None, return_when):
        self.advance(float("inf") if timeout is None else timeout)

    def status(self, env):
        self.probes.append(self.now)
        return self.broker(self.now)


class SchedulerTests(unittest.TestCase):
    @contextmanager
    def controller_patches(self, controllers):
        with (
            patch.multiple(acceptance, ThreadPoolExecutor=lambda **_: controllers,
                           wait=controllers.wait, time=SimpleNamespace(monotonic=lambda: controllers.now),
                           broker_slots=controllers.status),
            patch.object(acceptance.STOP, "wait", side_effect=controllers.advance),
        ):
            yield

    def dispatch(self, slots, attempts, broker=None, recovery_wait_seconds=None):
        controllers = FakeControllers(attempts, broker)
        env = {"SUBYARD_E2E_CAPACITY_WAIT": acceptance.LIVE_CAPACITY_WAIT}
        with self.controller_patches(controllers):
            acceptance.dispatch_live(attempts, slots, env, lambda name, slot: controllers.result,
                                     recovery_wait_seconds=recovery_wait_seconds)
        return controllers

    @contextmanager
    def run_fixture(self, controllers, output, *, completed_attempt=None):
        checks = {"verify": ["true"]} | {name: ["true"] for name in controllers.attempts}
        receipt = {"version": "test", "profiles": {}, "source_fingerprint": "a" * 64,
                   "candidate_bundle_sha256": "b" * 64, "external_obligations": {},
                   "checks": {name: {"status": "passed" if name == "verify" else "pending"} for name in checks}}
        with (
            self.controller_patches(controllers),
            patch.object(acceptance, "load_bound", return_value=(output, receipt)),
            patch.object(acceptance, "commands", return_value=checks),
            patch.object(acceptance, "execution_environment", return_value={}),
            patch.object(acceptance, "source_fingerprint", return_value=receipt["source_fingerprint"]),
            patch.object(acceptance, "execute", side_effect=lambda *_: controllers.result.copy()),
            patch.object(acceptance, "completed_attempt", side_effect=completed_attempt,
                         return_value={"id": "a" * 32, "sha256": "b" * 64}),
            redirect_stdout(io.StringIO()),
        ):
            yield

    def test_all_selected_slots_start_and_free_slots_keep_working(self):
        controllers = self.dispatch((2, 4, 7), {
            "a": [(30, PASSED)], "b": [(4, PASSED)], "c": [(3, PASSED)],
            "d": [(8, PASSED)], "e": [(1, PASSED)], "f": [(1, PASSED)],
        }, lambda _: dict.fromkeys((2, 4, 7), "available"))
        self.assertEqual(controllers.started, [
            (0, "a", 2), (0, "b", 4), (0, "c", 7),
            (3, "d", 7), (4, "e", 4), (5, "f", 4),
        ])
        self.assertEqual(controllers.probes, [0, 0, 0, 3, 4, 5])

    def test_first_pass_refreshes_status_between_assignments(self):
        snapshots = iter((
            {1: "available", 2: "available", 3: "held"},
            {1: "available", 2: "held", 3: "available"},
        ))
        controllers = self.dispatch((1, 2, 3), {
            "a": [(2, PASSED)], "b": [(1, PASSED)],
        }, lambda _: next(snapshots))
        self.assertEqual(controllers.started, [(0, "a", 1), (0, "b", 3)])

    def test_unclean_slot_rejoins_while_sibling_runs(self):
        def broker(now):
            return {2: "available" if now < 2 or now >= 7 else "draining",
                    3: "available", 4: "available"}

        controllers = self.dispatch((2, 4), {
            "a": [(2, FAILED)], "b": [(30, PASSED)], "c": [(1, PASSED)],
        }, broker)
        self.assertEqual(controllers.started, [(0, "a", 2), (0, "b", 4), (7, "c", 2)])
        self.assertEqual(controllers.probes, [0, 0, 2, 7])

    def test_pending_lane_stays_pending_when_no_slot_returns(self):
        controllers = self.dispatch((4,), {
            "a": [(3, FAILED)], "b": [(1, PASSED)],
        }, lambda now: {1: "available", 4: "available" if now < 3 else "draining"}, recovery_wait_seconds=12)
        self.assertEqual(controllers.started, [(0, "a", 4)])
        self.assertEqual(controllers.probes, [0, 3, 8, 13])
        self.assertEqual(controllers.now, 15)

    def test_capacity_retry_waits_for_the_sibling_holding_resources(self):
        blocked = "vm:blocked"
        controllers = FakeControllers({
            blocked: [(2, BLOCKED), (1, lambda now, _: PASSED if now >= 30 else BLOCKED)],
            "vm:short": [(9, PASSED)],
            "vm:long": [(30, PASSED)],
        })
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            output = Path(temporary)
            with self.run_fixture(controllers, output):
                self.assertEqual(acceptance.run_checks(output, (1, 2, 3), (), False, with_vms=True), 0)
            receipt = json.loads((output / "receipt.json").read_text())
        self.assertEqual(receipt["result"], "passed")
        self.assertEqual(receipt["checks"][blocked]["status"], "passed")
        self.assertEqual(controllers.started, [
            (0, blocked, 1), (0, "vm:short", 2), (0, "vm:long", 3), (30, blocked, 1),
        ])

    def test_capacity_retry_uses_current_status_after_first_pass(self):
        blocked, unclean, long = "vm:blocked", "vm:unclean", "vm:long"

        def broker(now):
            slot_one = "available" if now == 0 or 5.1 <= now < 10.1 else "held"
            return {1: slot_one, 2: "available"}

        controllers = FakeControllers({
            unclean: [(0.1, FAILED)],
            blocked: [(0, BLOCKED), (1, lambda _, slot: (PASSED if slot == 2 else FAILED) | {"slot": slot})],
            long: [(12, PASSED)],
        }, broker)
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            output = Path(temporary)
            with self.run_fixture(controllers, output):
                self.assertEqual(acceptance.run_checks(output, (1, 2), (), False, with_vms=True), 1)
            receipt = json.loads((output / "receipt.json").read_text())
        self.assertEqual(controllers.started, [
            (0, unclean, 1), (0, blocked, 2), (0, long, 2), (12, blocked, 2),
        ])
        self.assertEqual((receipt["checks"][blocked]["status"], receipt["checks"][blocked]["slot"]), ("passed", 2))

    def test_attempt_recording_error_drains_the_queue_on_broker_available_slots(self):
        broken, sibling, queued = "vm:broken", "vm:sibling", "vm:queued"

        def record(_output, _root, _receipt, name, _result):
            if name == broken:
                raise OSError("injected attempt recording error")
            return {"id": "a" * 32, "sha256": "b" * 64}

        for sibling_result, available_slot in ((PASSED, 2), (FAILED, 1)):
            def broker(now):
                if now < 2:
                    return {1: "available" if now == 0 else "draining", 2: "available"}
                return {available_slot: "available", 3 - available_slot: "quarantined"}

            controllers = FakeControllers({
                broken: [(1, PASSED)], sibling: [(2, sibling_result)], queued: [(1, PASSED)],
            }, broker)
            with self.subTest(available_slot=available_slot), tempfile.TemporaryDirectory(dir="/tmp") as temporary:
                output = Path(temporary)
                with self.run_fixture(controllers, output, completed_attempt=record):
                    with self.assertRaisesRegex(OSError, "injected attempt recording error"):
                        acceptance.run_checks(output, (1, 2), (), False, with_vms=True)
                receipt = json.loads((output / "receipt.json").read_text())
                invocation = json.loads(next((output / "runs").glob("*.json")).read_text())
                self.assertEqual(controllers.started, [(0, broken, 1), (0, sibling, 2), (2, queued, available_slot)])
                self.assertEqual(receipt["checks"][queued]["status"], "passed")
                self.assertEqual(invocation["checks"][broken]["status"], "incomplete")
                self.assertEqual(invocation["checks"][queued]["status"], "passed")
                self.assertEqual((invocation["state"], invocation["capture_status"]), ("error", "failed"))

    def test_capacity_block_relaunches_once_when_idle(self):
        controllers = self.dispatch((1,), {
            "blocked": [(2, BLOCKED), (2, BLOCKED)], "next": [(1, PASSED)],
        })
        self.assertEqual(controllers.started, [(0, "blocked", 1), (2, "next", 1), (3, "blocked", 1)])

    def test_recovered_slot_does_not_bypass_first_pass_capacity_barrier(self):
        controllers = self.dispatch((1, 2, 3), {
            "blocked": [(2, BLOCKED), (1, lambda now, _: PASSED if now >= 30 else BLOCKED)],
            "unclean": [(1, FAILED)], "long": [(30, PASSED)],
        }, lambda now: {1: "available", 2: "available" if now < 1 or now >= 7 else "draining", 3: "available"})
        self.assertEqual(controllers.probes, [0, 0, 0, 30])
        self.assertEqual(controllers.started, [
            (0, "blocked", 1), (0, "unclean", 2), (0, "long", 3), (30, "blocked", 1),
        ])

    def test_queued_retry_uses_first_available_slot_during_retry_pass(self):
        for recovery, duration, expected in ((True, 30, (9, 2)), (False, 4, (8, 3))):
            with self.subTest(recovery=recovery):
                controllers = self.dispatch((1, 2, 3), {
                    "a": [(2, BLOCKED), (30, PASSED)], "unclean": [(1, FAILED)],
                    "b": [(3, BLOCKED), (duration, PASSED)], "c": [(2, BLOCKED), (1, PASSED)],
                }, lambda now: {1: "available", 2: "available" if now < 1 or (recovery and now >= 6) else "draining",
                                3: "available"})
                when, slot = expected
                self.assertEqual(controllers.started, [
                    (0, "a", 1), (0, "unclean", 2), (0, "b", 3), (2, "c", 1),
                    (4, "a", 1), (4, "b", 3), (when, "c", slot),
                ])

    def test_slot_arguments(self):
        parser = argparse.ArgumentParser(exit_on_error=False)
        acceptance.add_slot_arguments(parser)
        for flags, slots in (
            ([], (1, 2, 3, 4)),
            (["--slot-count", "2"], (1, 2)),
            (["--slots", "slot-4,slot-2"], (4, 2)),
        ):
            with self.subTest(flags=flags):
                self.assertEqual(acceptance.selected_slots(parser.parse_args(flags)), slots)
        for flags in (["--slots", "2"], ["--slots", "slot-1,slot-1"], ["--slots", "slot-0"],
                      ["--slot-count", "5"], ["--slots", "slot-1", "--slot-count", "2"]):
            with self.subTest(flags=flags), self.assertRaises(argparse.ArgumentError):
                parser.parse_args(flags)


if __name__ == "__main__":
    unittest.main()
