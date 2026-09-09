from pathlib import Path
import tempfile
import time
import unittest
from unittest import mock

from support import git, init_git, project


class DiscoveryTests(unittest.TestCase):
    def setUp(self):
        from discovery import discover
        self.discover = discover
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.workspaces = Path(self.tmp.name) / "workspaces"
        self.root = project(self.workspaces)

    def test_folder_root_and_nested_checkout_names_include_ignored_hidden_deep_directories(self):
        init_git(self.root / "private")
        init_git(self.root / "private/vendor/fixtures/deep")
        init_git(self.root / ".hidden")
        (self.root / ".gitignore").write_text("private/\n.hidden/\n")
        scan = self.discover(self.workspaces)
        self.assertEqual([], scan.errors)
        self.assertEqual([("Sample", "folder"), (".hidden", "git"),
                          ("private", "git"), ("private/vendor/fixtures/deep", "git")],
                         [(r.name, r.kind) for r in scan.projects[0].roots])

    def test_git_root_does_not_stop_scan_and_symlink_targets_are_not_discovered(self):
        init_git(self.root)
        init_git(self.root / "nested")
        init_git(Path(self.tmp.name) / "outside")
        (self.root / "loop").symlink_to(self.root, target_is_directory=True)
        (self.root / "alias").symlink_to(self.root / "nested", target_is_directory=True)
        (self.root / "outside").symlink_to(Path(self.tmp.name) / "outside", target_is_directory=True)
        init_git(self.root / ".git/hidden-repo")
        scan = self.discover(self.workspaces)
        self.assertEqual([], scan.errors)
        self.assertEqual([("Sample", "git"), ("nested", "git")],
                         [(r.name, r.kind) for r in scan.projects[0].roots])

    def test_git_files_for_submodule_and_two_worktrees_are_distinct(self):
        init_git(self.root, commit=True)
        upstream = Path(self.tmp.name) / "upstream"
        init_git(upstream, commit=True)
        git(self.root, "-c", "protocol.file.allow=always", "submodule", "add", "-q",
            str(upstream), "module")
        git(upstream, "worktree", "add", "-q", "--detach", str(self.root / "checkout-one"))
        git(upstream, "worktree", "add", "-q", "--detach", str(self.root / "checkout-two"))
        scan = self.discover(self.workspaces)
        self.assertEqual([], scan.errors)
        self.assertEqual(["Sample", "checkout-one", "checkout-two", "module"],
                         [r.name for r in scan.projects[0].roots])
        self.assertNotIn(str(upstream), [r.path for r in scan.projects[0].roots])

    def test_broken_git_is_error_and_not_folder_transition(self):
        (self.root / ".git").write_text("gitdir: /nonexistent/orca-fixture\n")
        scan = self.discover(self.workspaces)
        self.assertTrue(scan.errors)
        self.assertEqual("unknown", scan.projects[0].roots[0].kind)

    def test_checkout_path_preserves_whitespace_and_unicode(self):
        name = "packages/checkout café\n"
        init_git(self.root / name)
        scan = self.discover(self.workspaces)
        self.assertEqual([], scan.errors)
        self.assertEqual([("Sample", "folder"), (name, "git")],
                         [(root.name, root.kind) for root in scan.projects[0].roots])

    def test_invalid_metadata_and_symlink_root_cannot_be_ready(self):
        (self.root.parent / ".subyard-meta.json").write_text('{"schema":1,"projectId":"different","name":"Wrong"}')
        self.assertTrue(self.discover(self.workspaces).errors)
        other = project(self.workspaces, "other-id")
        other.rmdir()
        other.symlink_to(self.root, target_is_directory=True)
        self.assertTrue(self.discover(self.workspaces).errors)

    def test_missing_root_is_warning_and_not_desired(self):
        self.root.rmdir()
        scan = self.discover(self.workspaces)
        self.assertEqual([], scan.errors)
        self.assertTrue(scan.warnings)
        self.assertEqual([], scan.projects[0].roots)

    def test_scan_error_and_budget_exhaustion_keep_known_root_but_fail_closed(self):
        (self.root / "blocked").mkdir()
        import discovery
        original = discovery.os.scandir

        def denied(path):
            if isinstance(path, int) and Path("/proc/self/fd/" + str(path)).resolve().name == "blocked":
                raise PermissionError("fixture")
            return original(path)

        with mock.patch.object(discovery.os, "scandir", side_effect=denied):
            scan = self.discover(self.workspaces)
        self.assertTrue(scan.errors)
        self.assertEqual("Sample", scan.projects[0].roots[0].name)
        limited = self.discover(self.workspaces, max_entries=1)
        self.assertTrue(limited.errors)
        self.assertTrue(self.discover(self.workspaces, deadline=time.monotonic() - 1).errors)

    def test_unreadable_project_does_not_skip_later_projects(self):
        project(self.workspaces, "zzz-healthy", "Healthy")
        import discovery
        original = discovery._open_dir

        def denied(path, parent=None):
            if path == "sample-id":
                raise PermissionError("fixture")
            return original(path, parent)
        with mock.patch.object(discovery, "_open_dir", side_effect=denied):
            scan = self.discover(self.workspaces)
        self.assertTrue(scan.errors)
        self.assertEqual(["Healthy"], [p.name for p in scan.projects])

    def test_changed_ancestor_symlink_is_rejected_before_git_probe(self):
        import discovery
        root = self.discover(self.workspaces).projects[0].roots[0]
        outside = Path(self.tmp.name) / "outside"
        (outside / "src").mkdir(parents=True)
        original = discovery._open_dir

        def replace_ancestor(path, parent=None):
            self.root.parent.rename(Path(self.tmp.name) / "original-project")
            self.root.parent.symlink_to(outside, target_is_directory=True)
            return original(path, parent)
        with mock.patch.object(discovery, "_open_dir", side_effect=replace_ancestor):
            with mock.patch.object(discovery.subprocess, "run") as probe:
                self.assertFalse(discovery.verify_root(root, time.monotonic() + 2))
                self.assertEqual(0, probe.call_count, "Git must not inspect a replaced workspace")
