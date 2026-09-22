import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

import support
from codex_launch import config_arguments


class CodexLaunchTests(unittest.TestCase):
    def test_only_stock_override_is_removed_and_prompt_separator_is_preserved(self):
        yolo = "--dangerously-bypass-approvals-and-sandbox"
        self.assertEqual(["--model", "example", "a prompt with spaces"],
                         config_arguments(["--model", "example", yolo, "a prompt with spaces"]))
        self.assertEqual(["--", yolo], config_arguments([yolo, "--", yolo]))
        self.assertEqual(["--yolo", "--sandbox", "workspace-write"],
                         config_arguments(["--yolo", "--sandbox", "workspace-write"]))

    def test_native_command_receives_config_environment_arguments_and_exit_status(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            binary = root / "codex"
            binary.write_text(
                "#!" + sys.executable + "\n"
                "import json, os, sys\n"
                "print(json.dumps({'args': sys.argv[1:], 'home': os.environ['HOME'], "
                "'codex_home': os.environ['CODEX_HOME']}))\n"
                "raise SystemExit(17)\n"
            )
            binary.chmod(0o755)
            environment = {**os.environ, "PATH": str(root), "HOME": str(root / "home"),
                           "CODEX_HOME": str(root / "codex-home")}
            result = subprocess.run(
                [sys.executable, "-B", str(support.COMPONENT / "codex_launch.py"),
                 "--dangerously-bypass-approvals-and-sandbox", "--model", "example", "literal $prompt"],
                env=environment, capture_output=True, text=True, timeout=5,
            )
            self.assertEqual(17, result.returncode, result.stderr)
            self.assertEqual({"args": ["--model", "example", "literal $prompt"],
                              "home": environment["HOME"], "codex_home": environment["CODEX_HOME"]},
                             json.loads(result.stdout))
