import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import support
from codex_launch import config_arguments, launch_arguments


class CodexLaunchTests(unittest.TestCase):
    def native_fixture(self, root):
        binary = root / "codex"
        binary.write_text(
            "#!" + sys.executable + "\n"
            "import json, os, sys\n"
            "from pathlib import Path\n"
            "if sys.argv[1:] == ['--help']:\n"
            "    Path(os.environ['PROBE_LOG']).write_text('probed')\n"
            "    print(os.environ.get('CLI_HELP', '--no-daemon'))\n"
            "    raise SystemExit(int(os.environ.get('HELP_STATUS', '0')))\n"
            "print(json.dumps({'args': sys.argv[1:], 'home': os.environ['HOME'], "
            "'codex_home': os.environ['CODEX_HOME'], 'stdin': sys.stdin.read(), "
            "'isolate': os.environ.get('ORCA_CODEX_ISOLATE')}))\n"
            "raise SystemExit(17)\n"
        )
        binary.chmod(0o755)
        environment = {key: value for key, value in os.environ.items()
                       if key not in ("ORCA_CODEX_ISOLATE", "ORCA_CODEX_LAUNCH_PREFLIGHT")}
        return {**environment, "PATH": str(root), "HOME": str(root / "home"),
                "CODEX_HOME": str(root / "codex-home"), "PROBE_LOG": str(root / "probe")}

    def launch(self, arguments, environment):
        return subprocess.run(
            [sys.executable, "-B", str(support.COMPONENT / "codex_launch.py"), *arguments],
            env=environment, input="preserved input", capture_output=True, text=True, timeout=10,
        )

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
            environment = self.native_fixture(root)
            result = self.launch(["--dangerously-bypass-approvals-and-sandbox",
                                  "--model", "example", "literal $prompt"], environment)
            self.assertEqual(17, result.returncode, result.stderr)
            self.assertEqual({"args": ["--no-daemon", "--model", "example", "literal $prompt"],
                              "home": environment["HOME"], "codex_home": environment["CODEX_HOME"],
                              "stdin": "preserved input", "isolate": None},
                             json.loads(result.stdout))

    def test_shared_server_arguments_and_opt_out_skip_the_probe(self):
        cases = [(["agents"], {}), (["queue"], {}), (["resume", "--remote", "endpoint"], {}),
                 (["--remote=endpoint"], {}), (["--no-daemon", "resume"], {}),
                 (["--model", "example"], {"ORCA_CODEX_ISOLATE": "0"})]
        for arguments, overrides in cases:
            with self.subTest(arguments=arguments, overrides=overrides), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                environment = {**self.native_fixture(root), **overrides}
                result = self.launch(arguments, environment)
                self.assertEqual(17, result.returncode, result.stderr)
                payload = json.loads(result.stdout)
                self.assertEqual(arguments, payload["args"])
                self.assertEqual("preserved input", payload["stdin"])
                self.assertEqual(overrides.get("ORCA_CODEX_ISOLATE"), payload["isolate"])
                self.assertFalse((root / "probe").exists())

    def test_help_is_rechecked_and_unsupported_or_failed_probes_preserve_arguments(self):
        with tempfile.TemporaryDirectory() as temporary:
            environment = self.native_fixture(Path(temporary))
            for help_text, status, expected in (("--no-daemon", "0", ["--no-daemon"]),
                                                ("older CLI", "0", []),
                                                ("--no-daemon", "2", []),
                                                ("--no-daemon", "0", ["--no-daemon"])):
                with self.subTest(help_text=help_text, status=status):
                    result = self.launch([], {**environment, "CLI_HELP": help_text, "HELP_STATUS": status})
                    self.assertEqual(17, result.returncode, result.stderr)
                    self.assertEqual(expected, json.loads(result.stdout)["args"])

    def test_managed_preparation_runs_even_when_opted_out_and_failure_is_quiet(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            preflight = root / "preflight"
            preflight.write_text(
                "#!" + sys.executable + "\n"
                "import json, os, sys\nfrom pathlib import Path\n"
                "Path(os.environ['PREP_LOG']).write_text(json.dumps(sys.argv[1:]))\n"
                "print('preparation output')\nprint('preparation error', file=sys.stderr)\n"
                "raise SystemExit(9)\n"
            )
            preflight.chmod(0o755)
            environment = {**self.native_fixture(root), "ORCA_CODEX_ISOLATE": "0",
                           "ORCA_CODEX_LAUNCH_PREFLIGHT": str(preflight), "PREP_LOG": str(root / "prep")}
            result = self.launch([], environment)
            self.assertEqual(17, result.returncode, result.stderr)
            self.assertEqual("", result.stderr)
            self.assertEqual([], json.loads(result.stdout)["args"])
            self.assertEqual(["agent", "hooks", "prepare-codex"], json.loads((root / "prep").read_text()))

    def test_probe_timeout_and_missing_executable_preserve_launch_arguments(self):
        for error in (subprocess.TimeoutExpired("codex", 5), FileNotFoundError()):
            with self.subTest(error=type(error).__name__), patch.dict(os.environ, {}, clear=True), \
                    patch("codex_launch.subprocess.run", side_effect=error):
                self.assertEqual(["resume", "example"], launch_arguments(["resume", "example"]))
