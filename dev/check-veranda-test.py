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
        env = {"TMPDIR": "/synthetic-original", "FIXTURE": "inherited"}
        original = dict(env)
        ambient = dict(os.environ)
        paths = []

        def child(_name, _command, child_env):
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


if __name__ == "__main__":
    unittest.main()
