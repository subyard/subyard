import json
import os
from pathlib import Path
import shutil
import tempfile
import time
import unittest
from unittest import mock

from support import Catalog, init_git, project
from discovery import discover
from inventory import Directory, Inventory, InventoryError
from reconcile import reconcile


class InventoryTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.workspaces = Path(self.tmp.name) / "workspaces"
        self.root = project(self.workspaces)
        self.state = Path(self.tmp.name) / "state"
        self.rpc = Catalog()

    def scan(self):
        return discover(self.workspaces, recursive=False)

    def advance(self, entries=50000):
        scan = self.scan()
        with Inventory(self.state, apply=True) as inventory:
            inventory.advance(scan, time.monotonic() + 15, max_entries=entries, portion=4)
            return inventory.paths(), inventory.progress()

    def sync(self, apply=True):
        scan = self.scan()
        with Inventory(self.state) as inventory:
            paths = inventory.paths()
        return reconcile(scan, self.rpc, self.state, apply=apply, known_paths=paths)

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

    def test_restart_fairness_and_billion_entry_lazy_directory_do_not_block_readiness(self):
        build = self.root / ".build"
        build.mkdir()
        init_git(self.root / "later/checkout")
        other = project(self.workspaces, "other", "Other")
        init_git(other / ".hidden/vendor/checkout")
        self.assertTrue(self.sync()["ready"])
        native = Directory.next

        def billion(stream):
            if Path("/proc/self/fd/" + str(stream.fd)).resolve() == build:
                # Cookie-backed lazy stream: never allocate the ordinary files.
                cookie = stream.lib.telldir(stream.handle)
                stream.lib.seekdir(stream.handle, cookie + 1)
                return "artifact", False, cookie + 1
            return native(stream)

        with mock.patch.object(Directory, "next", billion):
            for _ in range(15):
                paths, progress = self.advance(12)
                if str(other / ".hidden/vendor/checkout") in paths and str(self.root / "later/checkout") in paths:
                    break
            self.assertIn(str(other / ".hidden/vendor/checkout"), paths)
            self.assertIn(str(self.root / "later/checkout"), paths)
            self.assertEqual("scanning", progress["state"])
            self.assertTrue(self.sync()["ready"])
            before = self.rpc.calls[:]
            with mock.patch("inventory.Directory", side_effect=AssertionError("Foreground must not scan")):
                self.assertTrue(self.sync(apply=False)["ready"])
                self.assertTrue(self.sync()["ready"])
            self.assertFalse(any(method in ("repo.add", "repo.update", "projectGroup.moveProject")
                                 for method, _ in self.rpc.calls[len(before):]))
            with Inventory(self.state) as inventory:
                row = inventory.db.execute("SELECT cookie FROM queue WHERE path=?", (str(build),)).fetchone()
                self.assertGreater(row[0], 0)
            # A healthy tree is revisited while the enormous cache stays queued.
            late = other / ".build/new-checkout"
            init_git(late)
            with Inventory(self.state, apply=True) as inventory:
                inventory._set("seeded", "0")
                inventory.db.commit()
            for _ in range(20):
                paths, progress = self.advance(12)
                if str(late) in paths:
                    break
            self.assertIn(str(late), paths)
            self.assertEqual("scanning", progress["state"])

    def test_periodic_pass_discovers_late_build_git_and_rename_prunes_only_proven_absence(self):
        self.advance()
        self.assertTrue(self.sync()["ready"])
        init_git(self.root / ".build/new")
        with Inventory(self.state, apply=True) as inventory:
            inventory._set("seeded", "0")
            inventory.db.commit()
        self.advance()
        self.assertEqual(2, self.sync()["registered"])
        old = next(repo for repo in self.rpc.repos if repo["path"].endswith("/.build/new"))
        (self.root / ".build/new").rename(self.root / ".build/renamed")
        with Inventory(self.state, apply=True) as inventory:
            inventory._set("seeded", "0")
            inventory.db.commit()
        self.advance()
        self.rpc.snapshots = [{"worktree": old["id"] + "::main", "tabs": [{}]}]
        report = self.sync()
        self.assertTrue(report["ready"], report)
        self.assertTrue(report["warnings"])
        self.assertEqual(3, len(self.rpc.repos))
        self.rpc.snapshots.clear()
        self.assertTrue(self.sync()["ready"])
        self.assertEqual(2, len(self.rpc.repos))

    def test_thousand_known_roots_survive_missing_catalog_record_without_recursive_rescan(self):
        upstream = Path(self.tmp.name) / "upstream"
        init_git(upstream)
        for index in range(1000):
            checkout = self.root / ".build" / str(index)
            checkout.mkdir(parents=True)
            shutil.copytree(upstream / ".git", checkout / ".git", ignore=shutil.ignore_patterns("hooks", "info"))
        paths, _ = self.advance()
        self.assertEqual(1000, len(paths))
        self.assertTrue(self.sync()["ready"])
        self.rpc.calls.clear()
        self.assertEqual(1001, self.sync()["registered"])
        self.assertLessEqual(sum(method == "repo.list" for method, _ in self.rpc.calls), 2)
        self.rpc.repos.pop()
        self.assertFalse(self.sync(apply=False)["ready"])
        self.assertTrue(self.sync()["ready"])

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

    def test_unavailable_workspace_or_known_git_still_fails_ready(self):
        init_git(self.root / "nested")
        self.advance()
        self.assertTrue(self.sync()["ready"])
        shutil.rmtree(self.root / "nested/.git")
        (self.root / "nested/.git").write_text("gitdir: /missing-fixture\n")
        self.assertFalse(self.sync(apply=False)["ready"])
        self.workspaces.rename(Path(self.tmp.name) / "unavailable")
        self.assertFalse(self.sync(apply=False)["ready"])

    def test_failed_probe_keeps_adopted_root_after_native_record_disappears(self):
        nested = self.root / "nested"
        init_git(nested)
        # Adopt a native root before the background inventory has reached it.
        self.rpc.repos.append({"id": "adopted", "path": str(nested), "kind": "git"})
        self.assertTrue(self.sync()["ready"])
        shutil.rmtree(nested / ".git")
        (nested / ".git").write_text("gitdir: /missing-fixture\n")
        self.assertFalse(self.sync()["ready"])
        registry = json.loads((self.state / "subyard-registration.json").read_text())
        self.assertIn(str(nested), registry["projects"]["sample-id"]["known_roots"])
        self.rpc.repos = [repo for repo in self.rpc.repos if repo["path"] != str(nested)]
        self.assertFalse(self.sync(apply=False)["ready"])

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
