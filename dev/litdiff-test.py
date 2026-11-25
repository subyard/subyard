#!/usr/bin/env python3
"""Host-free checks for dev/litdiff.py on fixture repositories under /tmp."""
import contextlib
import importlib.util
import io
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
import unittest.mock

spec = importlib.util.spec_from_file_location("litdiff", Path(__file__).with_name("litdiff.py"))
tool = importlib.util.module_from_spec(spec)
spec.loader.exec_module(tool)
ENVIRONMENT = {"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1", "GIT_AUTHOR_NAME": "Fixture",
               "GIT_AUTHOR_EMAIL": "fixture@example.invalid", "GIT_COMMITTER_NAME": "Fixture",
               "GIT_COMMITTER_EMAIL": "fixture@example.invalid"}
SOURCE = '''package cli

import "fmt"
import (
	_ "embed"
	name "path/to/x"
	. "strings"
)

var imports = "counted" // "comment"
var url = "https://x" // c
/* "block
comment" */
var r, q = '"', '\\''
var s = "escaped \\" quote"
var raw = `multi
"line"`
'''


class LitdiffTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="litdiff-test-", dir="/tmp")
        self.addCleanup(temporary.cleanup)
        patcher = unittest.mock.patch.dict(os.environ, ENVIRONMENT)
        patcher.start()
        self.addCleanup(patcher.stop)
        self.work = Path(temporary.name)
        previous = Path.cwd()
        os.chdir(self.work)
        self.addCleanup(os.chdir, previous)
        self.git("init", "-q", "-b", "master")

    def git(self, *arguments):
        return subprocess.run(["git", *arguments], check=True, text=True, stdout=subprocess.PIPE,
                              stderr=subprocess.PIPE).stdout

    def write(self, files):
        for path, content in files.items():
            Path(path).parent.mkdir(parents=True, exist_ok=True)
            Path(path).write_text(content)

    def commit(self, files):
        self.write(files)
        self.git("add", "-A")
        self.git("commit", "-q", "-m", "Change")
        return self.git("rev-parse", "HEAD").strip()

    def invoke(self, *arguments):
        stdout, stderr = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = tool.main(list(arguments))
        return code, stdout.getvalue().splitlines(), stderr.getvalue().splitlines()

    def test_literals_skip_comments_runes_and_imports(self):
        self.assertEqual(list(tool.literals(SOURCE)),
                         ['"counted"', '"https://x"', '"escaped \\" quote"', '`multi\n"line"`'])

    def test_moved_literals_compare_equal(self):
        base = self.commit({"internal/cli/a.go": 'var a, b = "first: %w", "second"\n'})
        head = self.commit({"internal/cli/a.go": 'var a = "first: %w"\n',
                            "internal/application/b.go": 'import "fmt"\nvar b = "second"\n'})
        self.assertEqual(self.invoke("--base", base, "--head", head),
                         (0, ["literals: 2 before, 2 after; 0 removed, 0 added"], []))

    def test_worktree_lists_every_changed_literal(self):
        base = self.commit({"internal/cli/a.go": 'var a, b = "old message", "kept"\n',
                            "internal/cli/a_test.go": 'var t = "test"\n',
                            "internal/other/c.go": 'var c = "other"\n'})
        self.write({"internal/cli/a.go": 'var a, b = "new message", "kept"\n',
                    "internal/cli/a_test.go": 'var t = "changed test"\n',
                    "internal/other/c.go": 'var c = "changed other"\n',
                    "internal/application/new.go": 'var n = `untracked`\n'})
        self.assertEqual(self.invoke("--base", base), (1, [
            "literals: 2 before, 3 after; 1 removed, 2 added",
            '  -1 "old message"',
            '  +1 "new message"',
            "  +1 `untracked`",
        ], []))
        os.chdir("internal")
        self.assertEqual(self.invoke("--base", base, "other")[0:2], (1, [
            "literals: 1 before, 1 after; 1 removed, 1 added", '  -1 "other"', '  +1 "changed other"']))

    def test_every_refusal_is_listed(self):
        self.commit({"internal/cli/a.go": "package cli\n"})
        self.assertEqual(self.invoke("--base", "missing-base", "--head", "missing-head", "/"), (2, [], [
            "litdiff: refused: directory '/' is outside the repository",
            "litdiff: refused: cannot resolve revision 'missing-base'",
            "litdiff: refused: cannot resolve revision 'missing-head'",
        ]))
        self.assertEqual(self.invoke("--base", "HEAD", "internal/cli", "internal/typo"), (2, [], [
            "litdiff: refused: no non-test Go files under 'internal/typo'"]))


if __name__ == "__main__":
    unittest.main()
