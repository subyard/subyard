#!/usr/bin/env python3
"""Host-free tests for local release acceptance dispatch."""
import argparse
from concurrent.futures import Future
import importlib.util
from pathlib import Path
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
    def __init__(self, attempts, broker):
        self.attempts = {name: iter(results) for name, results in attempts.items()}
        self.broker = broker
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
        duration, result = attempt(name, slot)
        future = Future()
        self.active[slot] = (self.now + duration, future, result)
        return future

    def advance(self, seconds):
        deadline = self.now + seconds
        self.now = min([deadline, *(end for end, _, _ in self.active.values())])
        for slot, (end, future, result) in list(self.active.items()):
            if end <= self.now:
                del self.active[slot]
                future.set_result(result)

    def wait(self, futures, *, timeout, return_when):
        self.advance(timeout)

    def status(self, env):
        self.probes.append(self.now)
        return self.broker(self.now)


class SchedulerTests(unittest.TestCase):
    def dispatch(self, slots, attempts, broker=None, recovery_wait_seconds=None):
        controllers = FakeControllers(attempts, broker)
        env = {"SUBYARD_E2E_CAPACITY_WAIT": acceptance.LIVE_CAPACITY_WAIT}
        with (
            patch.multiple(acceptance, ThreadPoolExecutor=lambda **_: controllers,
                           wait=controllers.wait, time=SimpleNamespace(monotonic=lambda: controllers.now),
                           broker_slots=controllers.status),
            patch.object(acceptance.STOP, "wait", side_effect=controllers.advance),
        ):
            acceptance.dispatch_live(attempts, slots, env, lambda name, slot: next(controllers.attempts[name]),
                                     recovery_wait_seconds=recovery_wait_seconds)
        return controllers

    def test_all_selected_slots_start_and_free_slots_keep_working(self):
        controllers = self.dispatch((2, 4, 7), {
            "a": [(30, PASSED)], "b": [(4, PASSED)], "c": [(3, PASSED)],
            "d": [(8, PASSED)], "e": [(1, PASSED)], "f": [(1, PASSED)],
        })
        self.assertEqual(controllers.started, [
            (0, "a", 2), (0, "b", 4), (0, "c", 7),
            (3, "d", 7), (4, "e", 4), (5, "f", 4),
        ])
        self.assertEqual(controllers.probes, [])

    def test_unclean_slot_rejoins_while_sibling_runs(self):
        def broker(now):
            if now < 7:
                return {}
            return {2: "available", 3: "available"}

        controllers = self.dispatch((2, 4), {
            "a": [(2, FAILED)], "b": [(30, PASSED)], "c": [(1, PASSED)],
        }, broker)
        self.assertEqual(controllers.started, [(0, "a", 2), (0, "b", 4), (7, "c", 2)])
        self.assertEqual(controllers.probes, [2, 7])

    def test_pending_lane_stays_pending_when_no_slot_returns(self):
        controllers = self.dispatch((4,), {
            "a": [(3, FAILED)], "b": [(1, PASSED)],
        }, lambda now: {1: "available", 4: "draining"}, recovery_wait_seconds=12)
        self.assertEqual(controllers.started, [(0, "a", 4)])
        self.assertEqual(controllers.probes, [3, 8, 13])
        self.assertEqual(controllers.now, 15)

    def test_capacity_block_relaunches_after_sibling_without_waiting_for_all(self):
        controllers = self.dispatch((1, 2, 3), {
            "blocked": [(2, BLOCKED), (1, BLOCKED)],
            "short": [(9, PASSED)],
            "long": [(30, PASSED)],
        })
        self.assertEqual(controllers.started, [
            (0, "blocked", 1), (0, "short", 2), (0, "long", 3), (9, "blocked", 1),
        ])

    def test_capacity_block_relaunches_once_when_idle(self):
        controllers = self.dispatch((1,), {
            "blocked": [(2, BLOCKED), (2, BLOCKED)], "next": [(1, PASSED)],
        })
        self.assertEqual(controllers.started, [(0, "blocked", 1), (2, "next", 1), (3, "blocked", 1)])

    def test_capacity_block_relaunches_after_work_on_a_returned_slot(self):
        controllers = self.dispatch((1, 2), {
            "blocked": [(2, BLOCKED), (1, PASSED)], "unclean": [(1, FAILED)],
            "long": [(30, PASSED)], "short": [(1, PASSED)],
        }, lambda now: {2: "available"} if now >= 6 else {})
        self.assertEqual(controllers.started, [
            (0, "blocked", 1), (0, "unclean", 2), (2, "long", 1),
            (6, "short", 2), (7, "blocked", 2),
        ])

    def test_slot_arguments(self):
        parser = argparse.ArgumentParser(exit_on_error=False)
        acceptance.add_slot_arguments(parser)
        for flags, slots in (
            ([], ()),
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
