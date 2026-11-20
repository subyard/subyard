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

BASE_TEST = """package cli

func TestOutputFailure(t *testing.T) {
	root, environment := fixture(t)
	program, err := New(Options{RepositoryRoot: root, Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	if code := program.Run(); code != 2 || !strings.Contains(stderr.String(), "no space left on device") {
		t.Fatalf("output failure: code=%d stderr=%s", code, stderr.String())
	}
	if len(calls) != 1 {
		t.Fatalf("calls: %v", calls)
	}
}
"""

CANDIDATE_TEST = """package cli

func TestOutputFailure(t *testing.T) {
	fixture := newFixture(t)
	program := fixture.New(Options{})
	if code := program.Run(); code != 2 || !strings.Contains(stderr.String(), "no space left on device") {
		t.Fatalf("output failure: code=%d stderr=%s", code, stderr.String())
	}
	if len(calls) != 1 {
		t.Fatalf("calls: %v", calls)
	}
}
"""

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

    def asserts(self, candidate, renames="", waivers="", replacements=("New=fixture.New(",)):
        self.write("base/a_test.go", BASE_TEST)
        self.write("candidate/a_test.go", candidate)
        return self.run_tool(tool.asserts, self.work / "base", self.work / "candidate",
                             self.write("renames.tsv", LIST_HEADER + renames),
                             self.write("waivers.tsv", LIST_HEADER + waivers), list(replacements))

    def test_migration_that_keeps_every_predicate_passes(self):
        ok, output = self.asserts(CANDIDATE_TEST)
        self.assertTrue(ok, output)
        self.assertIn("replaced TestOutputFailure (a_test.go:7): error check of New by fixture.New(", output)

    def test_removed_condition_fails_although_the_assert_count_is_kept(self):
        weakened = CANDIDATE_TEST.replace(' || !strings.Contains(stderr.String(), "no space left on device")', "")
        ok, output = self.asserts(weakened)
        self.assertFalse(ok)
        self.assertIn("FAIL removed TestOutputFailure (a_test.go:10)", output)

    def test_assertion_traded_for_an_unrelated_assert_calls_fails(self):
        traded = CANDIDATE_TEST.replace("\tif len(calls) != 1 {\n\t\tt.Fatalf(\"calls: %v\", calls)\n\t}\n",
                                        "\tfixture.Exec().AssertCalls()\n")
        ok, output = self.asserts(traded)
        self.assertFalse(ok)
        self.assertIn('FAIL removed TestOutputFailure (a_test.go:13): if len(calls) != 1 {', output)

    def test_error_check_needs_its_named_replacement(self):
        ok, output = self.asserts(CANDIDATE_TEST.replace("fixture.New(Options{})", "build(Options{})"))
        self.assertFalse(ok)
        self.assertIn("FAIL removed TestOutputFailure (a_test.go:7): if err != nil { [New] => t.Fatal(err)", output)

    def test_rename_rule_and_waiver_bind_their_replacements(self):
        base_unit = 'if len(calls) != 1 { => t.Fatalf("calls: %v", calls)'
        renamed = CANDIDATE_TEST.replace("len(calls) != 1", "len(fixture.Calls) != 1")
        ok, output = self.asserts(renamed, renames="(?<![\\w.])calls\\b(?!\")\tfixture.Calls\n")
        self.assertFalse(ok, "the rule also renames the message argument")
        ok, output = self.asserts(renamed, renames="len\\(calls\\)\tlen(fixture.Calls)\n")
        self.assertTrue(ok, output)
        self.assertIn("renamed TestOutputFailure (a_test.go:13) by rule 2", output)
        replaced = CANDIDATE_TEST.replace("\tif len(calls) != 1 {\n\t\tt.Fatalf(\"calls: %v\", calls)\n\t}\n",
                                          "\tfixture.Exec().AssertCalls(one)\n")
        waiver = f"TestOutputFailure\t{base_unit}\tTestOutputFailure\tfixture.Exec().AssertCalls(one)\treviewed\n"
        ok, output = self.asserts(replaced, waivers=waiver)
        self.assertTrue(ok, output)
        self.assertIn("waived TestOutputFailure (a_test.go:13) by waiver line 2", output)
        ok, output = self.asserts(replaced.replace("AssertCalls(one)", "AssertCalls(other)"), waivers=waiver)
        self.assertFalse(ok)
        self.assertIn("names a replacement that TestOutputFailure does not add", output)

    def test_unused_waiver_fails(self):
        ok, output = self.asserts(CANDIDATE_TEST, waivers="TestOutputFailure\tif x { => t.Fatal(x)\tTestOutputFailure\tx\tstale\n")
        self.assertFalse(ok)
        self.assertIn("FAIL waiver line 2 matches no removed unit", output)

    def events(self, name, outcomes, package="ok"):
        lines = [json.dumps({"Action": action, "Test": test}) for test, action in outcomes.items()]
        lines.append(json.dumps({"Action": "pass" if package == "ok" else "fail"}))
        return self.write(name, "\n".join(lines) + "\n")

    def check_tests(self, base, candidate, added=(), plain=(), base_plain=None, candidate_plain=None, package="ok"):
        names = sorted(set(base) | set(candidate))
        base_list = self.write("base.list", "\n".join(name for name in names if name in base) + "\nok\n")
        candidate_list = self.write("candidate.list", "\n".join(name for name in names if name in candidate) + "\nok\n")
        return self.run_tool(tool.tests, base_list, candidate_list, self.events("base.json", base),
                             self.events("candidate.json", candidate, package),
                             self.events("base.plain.json", base_plain or {}),
                             self.events("candidate.plain.json", candidate_plain or {}), list(plain), list(added))

    def test_pass_sets_must_match_and_added_tests_must_pass(self):
        ok, output = self.check_tests({"TestA": "pass"}, {"TestA": "pass", "TestNew": "pass"}, added=["^TestNew$"])
        self.assertTrue(ok, output)
        ok, output = self.check_tests({"TestA": "pass"}, {"TestA": "pass", "TestNew": "fail"}, added=["^TestNew$"])
        self.assertFalse(ok)
        self.assertIn("FAIL added test TestNew did not pass (fail)", output)

    def test_any_failure_or_unexplained_package_failure_fails(self):
        ok, output = self.check_tests({"TestA": "fail"}, {"TestA": "fail"})
        self.assertFalse(ok)
        self.assertIn("FAIL base failed TestA", output)
        ok, output = self.check_tests({"TestA": "pass"}, {"TestA": "pass"}, package="fail")
        self.assertFalse(ok)
        self.assertIn("FAIL candidate package failed without a failing test", output)

    def test_tests_run_without_coverage_use_their_plain_outcome(self):
        covered = {"TestA": "pass", "TestReexec": "fail", "TestReexec/owner": "fail"}
        plain = {"TestReexec": "pass", "TestReexec/owner": "pass"}
        ok, output = self.check_tests(covered, covered, plain=["TestReexec"], base_plain=plain, candidate_plain=plain)
        self.assertTrue(ok, output)
        ok, output = self.check_tests(covered, covered, plain=["TestReexec"], base_plain=plain, candidate_plain={})
        self.assertFalse(ok)
        self.assertIn("FAIL candidate did not pass TestReexec without coverage (missing)", output)

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

    def test_verdict_separates_build_failures_from_failing_tests(self):
        for content, want in (
            ('{"Action":"fail","Test":"TestA"}\n', "failed"),
            ('{"Action":"pass","Test":"TestA"}\n', "passed"),
            ('{"Action":"output","Output":"FAIL\\tpkg [build failed]\\n"}\n', "invalid"),
            ('{"Action":"pass"}\n', "no-tests"),
        ):
            _, output = self.run_tool(tool.verdict, self.write("run.json", content))
            self.assertEqual(output.strip(), want, content)


if __name__ == "__main__":
    unittest.main()
