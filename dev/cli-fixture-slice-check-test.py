#!/usr/bin/env python3
"""Host-free checks for dev/cli-fixture-slice-check.py on fixture files under /tmp."""
import contextlib
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("slice_check", Path(__file__).with_name("cli-fixture-slice-check.py"))
tool = importlib.util.module_from_spec(spec)
spec.loader.exec_module(tool)

LIST_HEADER = "# comment\n"


class SliceCheckTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="cli-fixture-slice-test-", dir="/tmp")
        self.addCleanup(temporary.cleanup)
        self.work = Path(temporary.name)

    def write(self, name, content):
        path = self.work / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)
        return path

    def run_tool(self, function, *arguments):
        output = io.StringIO()
        with contextlib.redirect_stdout(output), contextlib.redirect_stderr(output):
            result = function(*arguments)
        return result, output.getvalue()

    def events(self, name, passed):
        lines = [json.dumps({"Action": "run", "Test": test}) for test in passed]
        lines += [json.dumps({"Action": "pass", "Test": test}) for test in passed]
        # A non-JSON line, such as a coverage warning, carries no test name.
        return self.write(name, "warning: not JSON\n" + "\n".join(lines) + "\n")

    def check_tests(self, base, candidate, base_passed, candidate_passed, added=()):
        return self.run_tool(
            tool.tests, self.write("base.list", "\n".join(base) + "\nok pkg\n"),
            self.write("candidate.list", "\n".join(candidate) + "\nok pkg\n"),
            [self.events("base.json", base_passed)], [self.events("candidate.json", candidate_passed)], list(added))

    def test_equal_lists_and_pass_sets_pass(self):
        ok, output = self.check_tests(["TestA"], ["TestA"], ["TestA", "TestA/sub"], ["TestA", "TestA/sub"])
        self.assertTrue(ok, output)

    def test_lost_or_gained_passes_fail(self):
        ok, output = self.check_tests(["TestA"], ["TestA"], ["TestA", "TestA/sub"], ["TestA"])
        self.assertFalse(ok)
        self.assertIn("FAIL no longer passes TestA/sub", output)
        ok, output = self.check_tests(["TestA"], ["TestA"], ["TestA"], ["TestA", "TestA/new"])
        self.assertFalse(ok)
        self.assertIn("FAIL newly passes TestA/new", output)

    def test_added_tests_must_be_allowed_and_pass(self):
        ok, output = self.check_tests(["TestA"], ["TestA", "TestNew"], ["TestA"], ["TestA", "TestNew"], ["^TestNew$"])
        self.assertTrue(ok, output)
        ok, output = self.check_tests(["TestA"], ["TestA", "TestNew"], ["TestA"], ["TestA"], ["^TestNew$"])
        self.assertFalse(ok)
        self.assertIn("FAIL added test TestNew did not pass", output)
        ok, output = self.check_tests(["TestA"], ["TestA", "TestNew"], ["TestA"], ["TestA", "TestNew"])
        self.assertFalse(ok)
        self.assertIn("FAIL added test TestNew", output)

    def test_removed_tests_fail(self):
        ok, output = self.check_tests(["TestA", "TestB"], ["TestA"], ["TestA", "TestB"], ["TestA"])
        self.assertFalse(ok)
        self.assertIn("FAIL removed test TestB", output)

    def test_mutant_list_must_name_existing_files_and_cover_changed_files(self):
        self.write("candidate/internal/cli/a_test.go", "")
        self.write("candidate/internal/cli/b_test.go", "")
        self.write("candidate/internal/cli/a.go", "")
        rows = ("internal/cli/a_test.go\t^TestA$\tinternal/cli/a.go\told\tnew\n"
                "internal/cli/gone_test.go\t^TestG$\tinternal/cli/a.go\told\tnew\n")
        listing = self.write("mutants.tsv", LIST_HEADER + rows)
        ok, output = self.run_tool(tool.mutants, listing, self.work / "candidate",
                                   {"internal/cli/a_test.go", "internal/cli/b_test.go"}, False)
        self.assertFalse(ok)
        self.assertIn("FAIL mutant line 3 names missing files ['internal/cli/gone_test.go']", output)
        self.assertIn("FAIL changed test file internal/cli/b_test.go has no mutant", output)
        self.assertIn("internal/cli/a_test.go\0^TestA$\0", output)

    def test_mutant_text_must_occur_once_and_honours_escapes(self):
        target = self.write("a.go", "first\nsecond\nsecond\n")
        tool.apply(target, "first\\n", "changed\\t")
        self.assertEqual(target.read_text(), "changed\tsecond\nsecond\n")
        with self.assertRaises(SystemExit):
            tool.apply(target, "second", "x")

    def test_coverage_counts_a_block_covered_only_by_a_rerun_and_reports_it(self):
        header = "mode: set\n"
        base = self.write("base.cover", header + "pkg/a.go:1.1,2.2 1 1\npkg/a.go:3.1,4.2 1 1\npkg/a_test.go:1.1,2.2 1 1\n")
        first = self.write("first.cover", header + "pkg/a.go:1.1,2.2 1 1\npkg/a.go:3.1,4.2 1 0\n")
        rerun = self.write("rerun.cover", header + "pkg/a.go:1.1,2.2 1 0\npkg/a.go:3.1,4.2 1 1\n")
        ok, output = self.run_tool(tool.coverage, base, first)
        self.assertFalse(ok)
        self.assertIn("FAIL lost pkg/a.go:3.1,4.2", output)
        ok, output = self.run_tool(tool.coverage, base, first, rerun)
        self.assertTrue(ok, output)
        self.assertIn("covered only by a candidate rerun: pkg/a.go:3.1,4.2", output)


if __name__ == "__main__":
    unittest.main()
