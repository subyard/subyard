"""Synthetic contract tests for resumable checkout discovery state."""

import os
from pathlib import Path
import shutil
import sqlite3
import tempfile
import time
import unittest
from unittest import mock

from support import init_git, project
from discovery import discover
from inventory import Directory, Inventory, InventoryError


class InventoryTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="subyard-checkout-inventory.", dir="/tmp")
        self.addCleanup(self.tmp.cleanup)
        self.workspaces = Path(self.tmp.name) / "workspaces"
        self.root = project(self.workspaces)
        self.state = Path(self.tmp.name) / "state"

    def scan(self):
        return discover(self.workspaces, recursive=False)

    def advance(self, entries=50000):
        scan = self.scan()
        with Inventory(self.state, apply=True) as inventory:
            inventory.advance(scan, time.monotonic() + 15, max_entries=entries, portion=4)
            return inventory.paths(), inventory.progress()
    def test_real_directory_cookie_resumes_in_new_process_stream(self):
        for index in range(300):
            (self.root / str(index)).touch()
        stream = Directory(str(self.root))
        first = [stream.next() for _ in range(100)]
        cookie = first[-1][2]
        stream.close()
        stream = Directory(str(self.root), cookie)
        try:
            remaining = []
            while True:
                item = stream.next()
                if item is None:
                    break
                remaining.append(item[0])
        finally:
            stream.close()
        self.assertEqual(302, len(first) + len(remaining))
        self.assertFalse(set(item[0] for item in first) & set(remaining))

    def test_checkpointed_read_creates_no_journal_or_other_files(self):
        self.advance()
        before = {path.name: (path.stat().st_ino, path.stat().st_size, path.stat().st_mtime_ns)
                  for path in self.state.iterdir()}
        with Inventory(self.state) as inventory:
            inventory.paths()
            inventory.progress()
        after = {path.name: (path.stat().st_ino, path.stat().st_size, path.stat().st_mtime_ns)
                 for path in self.state.iterdir()}
        self.assertEqual(before, after)

    def test_existing_committed_WAL_roots_remain_visible_without_creating_files(self):
        self.advance()
        connection = sqlite3.connect(self.state / "subyard-discovery.sqlite3")
        self.addCleanup(connection.close)
        connection.execute("INSERT INTO roots VALUES (?)", (str(self.root / "committed"),))
        connection.commit()
        before = {path.name for path in self.state.iterdir()}
        with Inventory(self.state) as inventory:
            self.assertIn(str(self.root / "committed"), inventory.paths())
        self.assertEqual(before, {path.name for path in self.state.iterdir()})

    def test_readers_reject_busy_writer_lock_with_bounded_wait(self):
        with Inventory(self.state, apply=True):
            with mock.patch("inventory.time.monotonic", side_effect=[0, 11]):
                with self.assertRaises(InventoryError):
                    with Inventory(self.state):
                        self.fail("Reader bypassed the writer lock")

    def test_readers_reject_missing_inventory_lock(self):
        self.advance()
        (self.state / "subyard-discovery.lock").unlink()
        with self.assertRaises(InventoryError):
            with Inventory(self.state):
                self.fail("Reader invented an unlocked snapshot")

    def test_readers_reject_changed_missing_or_replaced_database(self):
        for change in ("changed", "missing", "replaced"):
            with self.subTest(change=change):
                state = self.state / change
                with Inventory(state, apply=True):
                    pass
                path = state / "subyard-discovery.sqlite3"
                with self.assertRaises(InventoryError):
                    with Inventory(state) as inventory:
                        inventory.paths()
                        if change == "changed":
                            os.utime(path, ns=(0, 0))
                        elif change == "missing":
                            path.unlink()
                        else:
                            replacement = state / "replacement"
                            shutil.copyfile(path, replacement)
                            replacement.replace(path)

    def test_symlink_state_and_workspace_identity_change_are_not_trusted(self):
        outside = Path(self.tmp.name) / "outside"
        outside.mkdir()
        self.state.symlink_to(outside)
        with self.assertRaises(InventoryError):
            self.advance()
        self.state.unlink()
        init_git(self.root / "nested")
        self.advance()
        self.workspaces.rename(Path(self.tmp.name) / "old-workspaces")
        self.root = project(self.workspaces)
        paths, _ = self.advance()
        self.assertEqual([], paths)

    def test_symlink_directories_and_git_contents_are_never_traversed(self):
        init_git(self.root)
        init_git(self.root / ".git/fixture")
        outside = Path(self.tmp.name) / "outside"
        init_git(outside)
        (self.root / ".build").symlink_to(outside)
        (self.root / "loop").symlink_to(self.root)
        paths, _ = self.advance()
        self.assertEqual([str(self.root)], paths)

    def test_expiring_git_probe_rolls_back_portion_and_resumes(self):
        from discovery import ScanLimit
        init_git(self.root / "nested")
        with mock.patch("inventory._git_kind", side_effect=ScanLimit("time")):
            paths, progress = self.advance()
            self.assertEqual([], paths)
            self.assertEqual("scanning", progress["state"])
            self.assertEqual([], progress["errors"])
        paths, _ = self.advance()
        self.assertIn(str(self.root / "nested"), paths)
