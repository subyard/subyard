#!/usr/bin/env python3
"""Regression for the shared runner's isolated Linux temporary-path probe."""
import importlib.util
import os
from pathlib import Path
import sys
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("veranda_checks", Path(__file__).with_name("check-veranda.py"))
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


class RustTemporaryPathTest(unittest.TestCase):
    @unittest.skipUnless(sys.platform == "linux", "Linux symlink TMPDIR probe")
    def test_child_environment_failure_and_cleanup(self):
        env = {"TMPDIR": "/synthetic-original", "FIXTURE": "inherited", "RUSTUP_TOOLCHAIN": "uninstalled-override"}
        original = dict(env)
        ambient = dict(os.environ)
        paths = []

        def child(_name, command, child_env):
            self.assertEqual(command[:3], ["cargo", "+stable", "test"])
            alias = Path(child_env["TMPDIR"])
            self.assertTrue(alias.is_symlink())
            self.assertTrue(alias.resolve().is_dir())
            self.assertEqual(alias.resolve().stat().st_mode & 0o777, 0o700)
            self.assertEqual(alias.parent.stat().st_mode & 0o777, 0o700)
            self.assertEqual(alias.parent.parent, Path("/tmp"))
            self.assertEqual(child_env, dict(original, TMPDIR=str(alias)))
            self.assertEqual(env, original)
            self.assertEqual(dict(os.environ), ambient)
            paths.append(alias)
            return 7

        with patch.object(runner, "check", side_effect=child) as check:
            self.assertEqual(runner.rust_check(env), 7)
            check.assert_called_once()
        self.assertEqual(env, original)
        self.assertEqual(dict(os.environ), ambient)
        self.assertFalse(paths[0].is_symlink())
        self.assertFalse(paths[0].parent.exists())


class RustLintTest(unittest.TestCase):
    def test_lint_failure_does_not_skip_independent_checks(self):
        env = {"FIXTURE": "inherited", "RUSTUP_TOOLCHAIN": "uninstalled-override"}
        with patch.object(runner, "check", side_effect=[7, 9]) as check:
            self.assertEqual(runner.rust_lint(env), 7)
        self.assertEqual(check.call_args_list, [
            unittest.mock.call("Rust formatting", [
                "cargo", "+stable", "fmt", "--manifest-path", "veranda/client/Cargo.toml", "--check"], env),
            unittest.mock.call("Rust Clippy", [
                "cargo", "+stable", "clippy", "--manifest-path", "veranda/client/Cargo.toml",
                "--locked", "--all-targets", "--", "-D", "warnings"], env),
        ])
        self.assertEqual(env, {"FIXTURE": "inherited", "RUSTUP_TOOLCHAIN": "uninstalled-override"})

    def test_success(self):
        with patch.object(runner, "check", return_value=0):
            self.assertEqual(runner.rust_lint({}), 0)

    def test_rust_only_includes_lints_and_tests_after_failure(self):
        with patch.object(sys, "argv", ["check-veranda.py", "--rust-only"]), \
                patch.object(runner, "rust_lint", return_value=7) as lint, \
                patch.object(runner, "rust_check", return_value=9) as tests:
            self.assertEqual(runner.main(), 7)
        lint.assert_called_once()
        tests.assert_called_once_with(lint.call_args.args[0])


if __name__ == "__main__":
    unittest.main()
