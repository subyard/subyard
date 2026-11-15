#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
python3 -I - "$ROOT" <<'PY'
from pathlib import Path
from datetime import datetime, timedelta, timezone
import json
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
        for name in ("lint.sh", "audit-go.sh", "check-govulncheck.py"):
            shutil.copy2(source / "dev" / name, self.root / "dev" / name)

    def messages(self, *findings):
        config = {"config": {"protocol_version": "v1.0.0", "scan_mode": "source", "scan_level": "symbol"}}
        return "\n".join(json.dumps(message, indent=2) for message in [config, *findings]) + "\n"

    def finding(self, identifier, function="Affected", package="example.org/dependency"):
        return {"finding": {"osv": identifier, "trace": [{
            "module": "example.org/dependency", "package": package, "function": function,
        }]}}

    def entry(self, identifier="GO-2026-1000", days=30):
        deadline = datetime.now(timezone.utc).date() + timedelta(days=days)
        return {"id": identifier, "reason": "Synthetic reviewed exception", "review_by": deadline.isoformat()}

    def policy(self, messages, entries):
        report = self.write("findings.json", messages)
        allowlist = self.write("allowlist.json", json.dumps(entries))
        return subprocess.run(["python3", "-I", str(self.root / "dev/check-govulncheck.py"),
                               str(report), str(allowlist)], capture_output=True, text=True)

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

    def audit(self, payload, exit_code, entries=()):
        report = self.root / "payload"
        report.write_bytes(payload)
        self.write("bin/go", '#!/bin/sh\n[ "$*" = "env GOVERSION" ] || exit 97\necho go1.26.11\n')
        findings = [self.finding("GO-2026-1000")] if exit_code == 3 else []
        raw = self.write("findings.json", self.messages(*findings))
        self.write("dev/govulncheck-allowlist.json", json.dumps(entries))
        self.write(".build/tools/govulncheck-1.1.4-go1.26.11", f'''#!/bin/sh
case "$*" in
  '-format json ./...')
    if [ {exit_code} -eq 2 ]; then cat "$AUDIT_FIXTURE_REPORT" >&2; exit 2; fi
    cat "$AUDIT_FIXTURE_JSON" ;;
  '-mode convert') cat "$AUDIT_FIXTURE_REPORT"; exit {exit_code} ;;
  *) exit 97 ;;
esac
''')
        summary = self.root / "summary.md"
        env = dict(os.environ, PATH=str(self.root / "bin") + ":" + os.environ["PATH"],
                   AUDIT_FIXTURE_REPORT=str(report), AUDIT_FIXTURE_JSON=str(raw),
                   GITHUB_STEP_SUMMARY=str(summary))
        result = subprocess.run(["bash", str(self.root / "dev/audit-go.sh")], env=env, capture_output=True)
        return result, summary.read_bytes()

    def test_small_report_is_complete(self):
        payload = "Small report: no issues.\n".encode()
        result, summary = self.audit(payload, 0)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(result.stdout.startswith(payload))
        self.assertIn(b"PASS: 0 symbol finding IDs", result.stdout)
        self.assertEqual(summary, b"### Go dependency audit\n\n```text\n" + result.stdout + b"\n```\n")

    def test_large_findings_keep_full_console_and_bound_valid_utf8_summary(self):
        payload = ("Report \U0001f642\n" * 100000).encode()
        result, summary = self.audit(payload, 3)
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertTrue(result.stdout.startswith(payload))
        self.assertIn(b"GO-2026-1000: symbol finding is not allowlisted", result.stdout)
        self.assertLessEqual(len(summary), 900000)
        self.assertGreater(len(summary), 899990)
        self.assertTrue(summary.decode("utf-8").endswith("Report truncated; full report in job log.\n"))

    def test_tool_error_remains_a_failure_and_is_reported(self):
        payload = b"Analyzer failed.\n"
        result, summary = self.audit(payload, 2)
        self.assertEqual(result.returncode, 2)
        self.assertEqual(result.stdout, payload)
        self.assertIn(payload, summary)

    def test_unused_allowlist_warnings_are_in_output_and_summary(self):
        entries = [self.entry("GO-2026-1002"), self.entry("GO-2026-1000")]
        result, summary = self.audit(b"No symbol findings.\n", 0, entries)
        self.assertEqual(result.returncode, 0, result.stderr)
        warnings = [f"WARN: GO-2026-{number}: allowlist entry has no symbol finding in this scan".encode()
                    for number in (1000, 1002)]
        self.assertEqual([line for line in result.stdout.splitlines() if line.startswith(b"WARN:")], warnings)
        for warning in warnings:
            self.assertIn(warning, summary)

    def test_policy_accepts_reviewed_symbol_and_deduplicates_traces(self):
        finding = self.finding("GO-2026-1000")
        result = self.policy(self.messages(finding, finding), [self.entry()])
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("PASS: 1 symbol finding IDs", result.stdout)

    def test_policy_reports_every_unlisted_id_in_stable_order(self):
        findings = [self.finding(identifier) for identifier in
                    ["GO-2026-1003", "GO-2026-1001", "GO-2026-1002", "GO-2026-1001"]]
        result = self.policy(self.messages(*findings), [])
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertEqual(result.stdout.splitlines(), [
            f"FAIL: GO-2026-{number}: symbol finding is not allowlisted" for number in (1001, 1002, 1003)
        ])

    def test_policy_checks_unused_expiration_and_unlisted_finding_together(self):
        result = self.policy(self.messages(self.finding("GO-2026-1001")), [self.entry(days=-1)])
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn("GO-2026-1000: allowlist review expired", result.stdout)
        self.assertIn("GO-2026-1001: symbol finding is not allowlisted", result.stdout)

    def test_review_date_cannot_exceed_92_days(self):
        for days, expected in ((92, 0), (93, 1)):
            with self.subTest(days=days):
                result = self.policy(self.messages(self.finding("GO-2026-1000")), [self.entry(days=days)])
                self.assertEqual(result.returncode, expected, result.stdout + result.stderr)
                if expected:
                    self.assertIn("is more than 92 days ahead", result.stdout)

    def test_combined_date_errors_and_unlisted_ids_are_all_reported(self):
        entries = [self.entry("GO-2026-1002", days=93), self.entry(days=-1)]
        result = self.policy(self.messages(self.finding("GO-2026-1003"),
                                           self.finding("GO-2026-1001")), entries)
        self.assertEqual(result.returncode, 1, result.stderr)
        errors = [line for line in result.stdout.splitlines() if line.startswith("FAIL:")]
        self.assertEqual(errors, sorted(errors))
        self.assertEqual(len(errors), 4)
        self.assertIn("GO-2026-1000: allowlist review expired", errors[0])
        self.assertIn("GO-2026-1001: symbol finding is not allowlisted", errors[1])
        self.assertIn("GO-2026-1002: review_by", errors[2])
        self.assertIn("is more than 92 days ahead", errors[2])
        self.assertIn("GO-2026-1003: symbol finding is not allowlisted", errors[3])

    def test_review_date_is_inclusive(self):
        result = self.policy(self.messages(self.finding("GO-2026-1000")), [self.entry(days=0)])
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_package_and_module_only_findings_do_not_block(self):
        result = self.policy(self.messages(self.finding("GO-2026-1001", function=""),
                                           self.finding("GO-2026-1002", function="", package="")), [])
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("PASS: 0 symbol finding IDs", result.stdout)

    def test_invalid_entries_report_independent_errors(self):
        entry = dict(self.entry(), reason=" ", review_by="2026-02-30")
        result = self.policy(self.messages(), [entry, self.entry(), {"id": "bad"}])
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertEqual(len([line for line in result.stdout.splitlines() if line.startswith("FAIL:")]), 6)
        self.assertIn("reason must be nonempty", result.stdout)
        self.assertIn("review_by must be an ISO date", result.stdout)
        self.assertIn("duplicate allowlist entry", result.stdout)
        self.assertIn("invalid ID", result.stdout)

    def test_invalid_report_does_not_hide_expired_review(self):
        for report in ("", "{", self.messages() + '{"finding": {}}',
                       self.messages().replace('"symbol"', '"package"')):
            with self.subTest(report=report):
                result = self.policy(report, [self.entry(days=-1)])
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertIn("govulncheck report:", result.stdout)
                self.assertIn("allowlist review expired", result.stdout)


unittest.main()
PY
