"""Contract tests for the checkout discovery JSON CLI used by profiles."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

from support import LIBRARY, git, init_git, project


class CheckoutsTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="subyard-checkouts.", dir="/tmp")
        self.addCleanup(self.tmp.cleanup)
        self.workspaces = Path(self.tmp.name) / "workspaces"
        self.root = project(self.workspaces)
        self.state = Path(self.tmp.name) / "state"
        init_git(self.root, commit=True)

    def run_cli(self, command, *options, known=()):
        result = subprocess.run([sys.executable, "-B", "-E", "-s", str(LIBRARY / "checkouts.py"), command,
                                 "--workspaces", str(self.workspaces), "--state", str(self.state), *options],
                                input=json.dumps(list(known)), capture_output=True, text=True, check=True)
        return json.loads(result.stdout)

    def roots(self, report):
        return [root["path"] for root in report["projects"][0]["roots"]]

    def test_advance_caches_nested_repositories_that_roots_then_reports(self):
        init_git(self.root / "private")
        self.assertEqual([str(self.root)], self.roots(self.run_cli("roots")))
        self.assertEqual([], self.run_cli("advance")["errors"])
        self.assertEqual([str(self.root), str(self.root / "private")], self.roots(self.run_cli("roots")))

    def test_siblings_report_linked_worktrees_but_not_their_nested_repositories(self):
        directory = self.root.parent
        git(self.root, "worktree", "add", "-q", str(directory / "wt-feature"))
        init_git(directory / "wt-feature" / "private")
        init_git(directory / "standalone")
        (directory / "plain").mkdir()
        os.symlink(self.root, directory / "wt-link")
        self.run_cli("advance")
        report = self.run_cli("roots", "--siblings")
        self.assertEqual([], report["errors"])
        self.assertEqual([str(self.root), str(directory / "standalone"), str(directory / "wt-feature")],
                         self.roots(report))
        self.assertEqual([str(self.root)], self.roots(self.run_cli("roots")))

    def test_only_proven_absence_is_reported_for_known_paths(self):
        nested, kept = self.root / "nested", self.root / "kept"
        init_git(nested)
        init_git(kept)
        self.run_cli("advance")
        (nested / ".git").rename(self.root / "moved-git")
        os.rename(nested, self.root / "renamed")
        (kept / ".git").rename(self.root / "kept-git")
        outside = Path(self.tmp.name) / "outside"
        report = self.run_cli("roots", known=[str(nested), str(kept), str(outside)])
        self.assertEqual([str(nested)], report["absent"])
        self.assertIn(str(kept), " ".join(report["warnings"]))
        self.assertNotIn(str(kept), self.roots(report))

    def test_invalid_known_paths_and_unsafe_state_are_errors_without_projects(self):
        result = subprocess.run([sys.executable, "-B", str(LIBRARY / "checkouts.py"), "roots",
                                 "--workspaces", str(self.workspaces), "--state", str(self.state)],
                                input='["relative"]', capture_output=True, text=True, check=True)
        self.assertEqual({"errors": ["known paths must be a JSON list of absolute paths"]}, json.loads(result.stdout))
        self.state.parent.joinpath("elsewhere").mkdir()
        self.state.symlink_to(self.state.parent / "elsewhere")
        report = self.run_cli("advance")
        self.assertNotIn("discovery", report)
        self.assertTrue(report["errors"])


if __name__ == "__main__":
    unittest.main()
