#!/usr/bin/env python3
"""Host-free checks for dev/pr-stats.py on fixture repositories under /tmp."""
import contextlib
import importlib.util
import io
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
import unittest.mock

spec = importlib.util.spec_from_file_location("pr_stats", Path(__file__).with_name("pr-stats.py"))
tool = importlib.util.module_from_spec(spec)
spec.loader.exec_module(tool)
ENVIRONMENT = {"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1", "GIT_AUTHOR_NAME": "Fixture",
               "GIT_AUTHOR_EMAIL": "fixture@example.invalid", "GIT_COMMITTER_NAME": "Fixture",
               "GIT_COMMITTER_EMAIL": "fixture@example.invalid"}


class PRStatsTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="pr-stats-test-", dir="/tmp")
        self.addCleanup(temporary.cleanup)
        patcher = unittest.mock.patch.dict(os.environ, ENVIRONMENT)
        patcher.start()
        self.addCleanup(patcher.stop)
        self.work = Path(temporary.name)
        previous = Path.cwd()
        os.chdir(self.work)
        self.addCleanup(os.chdir, previous)
        self.git("init", "-q", "-b", "master")
        self.commit({"README.md": "seed\n", "internal/a.go": "one\ntwo\n"})
        self.git("switch", "-q", "-c", "feature")

    def git(self, *arguments):
        return subprocess.run(["git", *arguments], check=True, text=True, stdout=subprocess.PIPE,
                              stderr=subprocess.PIPE).stdout

    def commit(self, files):
        for path, content in files.items():
            Path(path).parent.mkdir(parents=True, exist_ok=True)
            Path(path).write_bytes(content) if isinstance(content, bytes) else Path(path).write_text(content)
        self.git("add", "-A")
        self.git("commit", "-q", "-m", "Change")
        return self.git("rev-parse", "HEAD").strip()

    def invoke(self, *arguments):
        stdout, stderr = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = tool.main(list(arguments))
        return code, stdout.getvalue(), [line.removeprefix("pr-stats: refused: ") for line in stderr.getvalue().splitlines()]

    def test_area_rules_are_explicit(self):
        # Only paths whose area depends on rule order.
        cases = {
            "config/skills/core/subyard-environment/SKILL.md": "product", "config/skills/tests/delivery-test.py": "tests",
            "tests/fixtures/x.env": "fixtures and tools", "tests/fixtures/sub/y.json": "fixtures and tools", "tests/a.sh": "tests", "dev/e2e/notes.md": "tests",
            "dev/pr-stats-test.py": "tests", "dev/pr-stats.py": "fixtures and tools",
            "api/yard-rpc/v1/fixtures/context.json": "fixtures and tools", "api/yard-rpc/v1/README.md": "documentation",
            ".agents/skills/subyard-dev-flow/SKILL.md": "documentation", "config/agents/claude/provision.sh": "product",
            "unknown/file": None,
        }
        for path, expected in cases.items():
            self.assertEqual(tool.area(path), expected, path)

    def test_block_counts_areas_renames_binary_and_binds_head(self):
        self.git("mv", "internal/a.go", "internal/b.go")
        head = self.commit({"docs/a.md": "doc\n", "tests/blob.bin": b"\0\1", "tests/a.sh": "x\ny\n"})
        code, stdout, refusals = self.invoke(
            "--base", "master", "--review-cycles", "2", "--check", "unit=https://example.invalid/runs/1",
            "--purpose", "tests=Cover | a\nnew line", "--purpose", "documentation=Describe a")
        self.assertEqual((code, refusals), (0, []))
        self.assertEqual(stdout, "\n".join([
            "### Statistics", "", f"Computed for `{head}`.", "", "| Area | Added | Removed | Purpose |",
            "| --- | ---: | ---: | --- |", "| tests | 2 | 0 | Cover \\| a new line |",
            "| documentation | 1 | 0 | Describe a |", "| total | 3 | 0 | |", "", "Review cycles: 2", "", "Checks:",
            "- unit: https://example.invalid/runs/1"]) + "\n")

    def test_all_independent_refusals_are_reported_in_order(self):
        self.commit({"internal/c.go": "x\n", "unknown/file": "x\n"})
        Path("untracked.txt").write_text("dirty\n")
        code, stdout, refusals = self.invoke("--base", "master", "--review-cycles", "-1", "--check", "bad",
                                             "--check", "lint=local", "--check", "lint=https://example.invalid/2",
                                             "--purpose", "other=x")
        self.assertEqual((code, stdout), (1, ""))
        self.assertEqual(refusals, [
            "--review-cycles must be a non-negative integer", "--check must be NAME=TEXT: 'bad'",
            "duplicate --check name 'lint'", "--check lint needs an https URL", "unknown --purpose area 'other'",
            "working tree is not clean; statistics describe committed HEAD only",
            "unclassified path 'unknown/file'; add an area rule", "missing --purpose for changed area 'product'"])

    def test_unknown_base_keeps_independent_refusals(self):
        code, _, refusals = self.invoke("--base", "missing", "--review-cycles", "x", "--purpose", "other=x")
        self.assertEqual(code, 1)
        self.assertEqual(refusals, ["--review-cycles must be a non-negative integer",
                                    "unknown --purpose area 'other'", "cannot resolve HEAD and base 'missing'"])


if __name__ == "__main__":
    unittest.main()
