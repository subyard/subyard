#!/usr/bin/env python3
"""Focused host-free cursor/projection checks; fixtures stay outside workspaces."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("projection", Path(__file__).with_name("project-run-events.py"))
projection = importlib.util.module_from_spec(spec)
spec.loader.exec_module(projection)


def record(event, **fields):
    return (json.dumps({"event": event, **fields}) + "\n").encode()


class ProjectionTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="run-events-test-", dir="/tmp")
        self.addCleanup(self.temporary.cleanup)
        self.journal = Path(self.temporary.name) / "events.jsonl"

    def test_backlog_reread_and_appended_failure_terminal(self):
        original = record("check-start", check="one") + record("heartbeat") + record("check-result", status="passed")
        self.journal.write_bytes(original)
        first = projection.project(self.journal, max_lines=1)
        self.assertTrue(first["backlog"])
        self.assertEqual(first["cursor"]["offset"], len(record("check-start", check="one")))
        second = projection.project(self.journal, first["cursor"])
        self.assertEqual([event["event"] for event in second["events"]], ["check-result"])
        empty = projection.project(self.journal, second["cursor"])
        self.assertEqual(empty["events"], [])
        self.assertEqual(empty["cursor"], second["cursor"])
        appended = record("check-result", status="failed", exit_code=7) + record("run-result", status="failed")
        with self.journal.open("ab") as stream:
            stream.write(appended)
        failure = projection.project(self.journal, empty["cursor"], max_lines=1)
        self.assertEqual(failure["events"][0]["exit_code"], 7)
        self.assertTrue(failure["backlog"])
        terminal = projection.project(self.journal, failure["cursor"], max_lines=1)
        self.assertEqual(terminal["events"], [{"event": "run-result", "status": "failed"}])
        self.assertFalse(terminal["backlog"])
        self.assertEqual(terminal["cursor"]["offset"], len(original + appended))
        self.assertEqual(self.journal.read_bytes(), original + appended)

    def test_split_utf8_line_is_deferred_without_loss(self):
        line = record("check-result", check="\u2713", status="blocked")
        # Use raw UTF-8 too, splitting inside its multibyte sequence.
        line = line.replace(b"\\u2713", "\u2713".encode())
        split = line.index("\u2713".encode()) + 1
        self.journal.write_bytes(line[:split])
        first = projection.project(self.journal)
        self.assertEqual(first["events"], [])
        self.assertEqual(first["cursor"]["offset"], 0)
        self.assertEqual(first["incomplete_bytes"], split)
        self.assertFalse(first["backlog"])
        with self.journal.open("ab") as stream:
            stream.write(line[split:])
        final = projection.project(self.journal, first["cursor"])
        self.assertEqual(final["events"][0]["check"], "\u2713")
        self.assertEqual(final["cursor"]["offset"], len(line))

    def test_byte_cap_advances_only_processed_newlines(self):
        line = record("heartbeat")
        self.journal.write_bytes(line * 6 + record("run-result", status="passed"))
        cursor = None
        events = []
        offsets = []
        for _ in range(8):
            delta = projection.project(self.journal, cursor, max_bytes=50)
            cursor = delta["cursor"]
            offsets.append(cursor["offset"])
            events.extend(delta["events"])
            if not delta["backlog"]:
                break
        self.assertEqual(events, [{"event": "run-result", "status": "passed"}])
        self.assertEqual(offsets[0], len(line) * 2)
        self.assertEqual(cursor["offset"], self.journal.stat().st_size)

    def test_malformed_complete_record_is_explicit_and_cursor_retained(self):
        self.journal.write_bytes(record("heartbeat"))
        cursor = projection.project(self.journal)["cursor"]
        before = json.loads(json.dumps(cursor))
        with self.journal.open("ab") as stream:
            stream.write(b'{"event": invalid}\n')
        result = subprocess.run([sys.executable, "-B", str(Path(__file__).with_name("project-run-events.py")),
                                 str(self.journal), "--cursor", json.dumps(cursor)], capture_output=True, text=True)
        self.assertEqual(result.returncode, 1)
        self.assertIn("malformed complete JSON", json.loads(result.stdout)["error"])
        self.assertEqual(cursor, before)
        self.assertNotIn("invalid", result.stdout)

    def test_truncate_replace_and_regrow_are_diagnostics(self):
        original = record("check-result", status="passed")
        for kind in ("truncate", "replace", "regrow"):
            with self.subTest(kind=kind):
                self.journal.write_bytes(original)
                cursor = projection.project(self.journal)["cursor"]
                if kind == "replace":
                    replacement = self.journal.with_suffix(".new")
                    replacement.write_bytes(original)
                    os.replace(replacement, self.journal)
                elif kind == "regrow":
                    self.journal.write_bytes(original.replace(b"passed", b"failed"))
                else:
                    self.journal.write_bytes(b"")
                with self.assertRaisesRegex(ValueError, "replaced|truncated|changed"):
                    projection.project(self.journal, cursor)

    def test_oversized_line_invalid_event_and_bad_cursor(self):
        for content, message in ((b"x" * 51, "exceeds byte limit"), (b"[]\n", "invalid event object"),
                                 (b'{"event":"result","exit_code":NaN}\n', "malformed complete JSON")):
            self.journal.write_bytes(content)
            with self.assertRaisesRegex(ValueError, message):
                projection.project(self.journal, max_bytes=50)
        self.journal.write_bytes(record("heartbeat"))
        cursor = projection.project(self.journal)["cursor"]
        cursor["offset"] = True
        with self.assertRaisesRegex(ValueError, "invalid cursor"):
            projection.project(self.journal, cursor)

    def test_explicit_compact_whitelist_and_unknown_payload(self):
        self.journal.write_bytes(record("run-start", checks={"private": "untrusted"})
            + record("phase", state="started") + record("heartbeat")
            + record("check-result", check="one", status="failed", exit_code=7, log="one.log",
                     command=["untrusted"], environment={"secret": "do-not-echo"},
                     cleanup={"lease-release": {"exit_code": 9, "duration_seconds": 2, "output": "untrusted"}})
            + record("unknown-kind", payload="do-not-echo")
            + record("run-result", invocation_id="one", status="incomplete", selected_status="passed",
                     overall_status="incomplete", result="runs/one.json"))
        raw = projection.project(self.journal)
        compact = projection.project(self.journal, compact=True)
        self.assertEqual(compact["cursor"], raw["cursor"])
        self.assertEqual([item["event"] for item in compact["events"]],
                         ["check-result", "unknown-event", "run-result"])
        self.assertEqual(compact["events"][0]["cleanup"],
                         {"lease-release": {"exit_code": 9, "duration_seconds": 2}})
        unknown = compact["events"][1]
        self.assertEqual((unknown["type"], unknown["count"]), ("unknown-kind", 1))
        self.assertGreater(unknown["offset"], 0)
        self.assertNotIn("do-not-echo", json.dumps(compact))
        self.assertNotIn("untrusted", json.dumps(compact))
        self.assertNotIn("required_action", json.dumps(compact))
        self.assertEqual(projection.project(self.journal, compact["cursor"], compact=True)["events"], [])

    def test_snapshot_eof_does_not_expand_when_appended(self):
        self.journal.write_bytes(record("heartbeat") * 3 + record("check-result", status="blocked"))
        first = projection.project(self.journal, max_lines=1, compact=True)
        boundary = first["eof"]
        with self.journal.open("ab") as stream:
            stream.write(record("run-result", status="incomplete"))
        rest = projection.project(self.journal, first["cursor"], compact=True, until=boundary)
        self.assertEqual([item["event"] for item in rest["events"]], ["check-result"])
        self.assertFalse(rest["backlog"])
        self.assertEqual(rest["cursor"]["offset"], boundary)
        later = projection.project(self.journal, rest["cursor"], compact=True)
        self.assertEqual(later["events"][0]["event"], "run-result")
        with self.assertRaisesRegex(ValueError, "snapshot EOF"):
            projection.project(self.journal, later["cursor"], until=boundary)


if __name__ == "__main__":
    unittest.main()
