#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
python3 -I - "$ROOT" <<'PY'
from pathlib import Path
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

source = Path(sys.argv[1])
sys.argv = [sys.argv[0]]


class LintTools(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="subyard-lint-tools-", dir="/tmp")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.root.chmod(0o700)
        (self.root / "dev").mkdir(mode=0o700)
        for name in ("lint.sh", "audit-go.sh"):
            shutil.copy2(source / "dev" / name, self.root / "dev" / name)

    def write(self, relative, content):
        path = self.root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)
        path.chmod(0o700)
        return path

    def test_all_linters_run_in_order_and_first_failure_is_retained(self):
        names = ["lint-workflows.sh", "lint-python.sh", "lint-go.sh"]
        for exits in ((0, 0, 0), (7, 9, 11), (0, 9, 11)):
            with self.subTest(exits=exits):
                for name, code in zip(names, exits):
                    self.write("dev/" + name, f"#!/bin/sh\necho {name}\nexit {code}\n")
                result = subprocess.run(["bash", str(self.root / "dev/lint.sh")], capture_output=True, text=True)
                self.assertEqual(result.stdout.splitlines(), names)
                self.assertEqual(result.returncode, next((code for code in exits if code), 0))
                self.assertEqual(result.stderr.splitlines(), [
                    f"dev/{name}: failed (exit {code})" for name, code in zip(names, exits) if code
                ])

    def audit(self, payload, exit_code):
        report = self.root / "payload"
        report.write_bytes(payload)
        self.write("bin/go", '#!/bin/sh\n[ "$*" = "env GOVERSION" ] || exit 97\necho go1.26.10\n')
        self.write(".build/tools/govulncheck-1.1.4-go1.26.10",
                   '#!/bin/sh\ncat "$AUDIT_FIXTURE_REPORT"\nexit ' + str(exit_code) + '\n')
        summary = self.root / "summary.md"
        env = dict(os.environ, PATH=str(self.root / "bin") + ":" + os.environ["PATH"],
                   AUDIT_FIXTURE_REPORT=str(report), GITHUB_STEP_SUMMARY=str(summary))
        result = subprocess.run(["bash", str(self.root / "dev/audit-go.sh")], env=env, capture_output=True)
        return result, summary.read_bytes()

    def test_small_report_is_complete(self):
        payload = "Small report: no issues.\n".encode()
        result, summary = self.audit(payload, 0)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, payload)
        self.assertEqual(summary, b"### Go dependency audit\n\n```text\n" + payload + b"\n```\n")

    def test_large_findings_keep_full_console_and_bound_valid_utf8_summary(self):
        payload = ("Report \U0001f642\n" * 100000).encode()
        result, summary = self.audit(payload, 3)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(result.stdout.startswith(payload))
        self.assertIn(b"report-only", result.stdout)
        self.assertLessEqual(len(summary), 900000)
        self.assertGreater(len(summary), 899990)
        self.assertTrue(summary.decode("utf-8").endswith("Report truncated; full report in job log.\n"))

    def test_tool_error_remains_a_failure_and_is_reported(self):
        payload = b"Analyzer failed.\n"
        result, summary = self.audit(payload, 2)
        self.assertEqual(result.returncode, 2)
        self.assertEqual(result.stdout, payload)
        self.assertIn(payload, summary)
        self.assertNotIn(b"report-only", summary)


unittest.main()
PY
