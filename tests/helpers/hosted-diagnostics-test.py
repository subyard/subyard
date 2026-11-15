#!/usr/bin/env python3
"""Light, host-free contracts for the closed diagnostic masking/capture policy."""
from contextlib import redirect_stdout
import errno
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("hosted_diagnostics", ROOT / "dev/ci/release-vm.py")
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)


def render(data, environ=None):
    with io.BytesIO() as output:
        m.Redactor({} if environ is None else environ).write(io.BytesIO(data), output)
        return output.getvalue()


class Redaction(unittest.TestCase):
    def test_keep_table_is_byte_identical(self):
        cases = [
            b"--- FAIL: TestNormalizeLegacyYardConfigContent\n",
            b"TestResolveIntegrationSelectionWithAnExtremelyLongReadableTestNameAndDigits123456789\n",
            b"subyard-e2e-slot-1-owner-pool-generation-abc12\n",
            b"SHA256:aB3_dE6-gH9_jK2-mN5_pQ8-sT1_vW4-yZ7_cF0-iL3\n",
            b"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKJmN3rT8uYpQ2vBnMxZaSdFgHjKlQwErTyUiOp\n",
            b"/home/runner/work/_temp/acceptance/profile-orca-managed-agents-76bf920a.log\n",
            b"internal/adapters/reconcileruntime/integration_ownership_release_test.go:123: failure\n",
            b"commit=" + b"0123456789abcdef" * 2 + b"01234567 id=" + b"0123456789abcdef" * 2 + b"\n",
            b"The key is recorded; token auth pass cookie words are prose, not secret values.\n",
            b"password=fixture-value Authorization: Bearer fixture-value Cookie: fixture-value\n",
            b"postgres://user:fixture-value@host/db --token fixture-value\n",
            b"-----BEGIN CERTIFICATE-----\nMIIABase64CertificateBodyWithNonHexCharactersAndNoPrivateKey\n-----END CERTIFICATE-----\n",
            b'{"capture_status":"recorded","status":"passed","log":"fixture.log"}\n',
            b'{"id":"0123456789abcdef0123456789abcdef","result":{"capture_status":"recorded"}}\n',
            b'{"state":"finished","capture_status":"recorded","selected_status":"passed"}\n',
            b"\x1b[31munchanged ANSI\x1b[0m\r\ninvalid UTF-8: \xff\n",
        ]
        for data in cases:
            with self.subTest(keep=data[:48]):
                self.assertEqual(render(data), data)

    def test_only_secret_named_env_values_are_learned(self):
        names = ["TOKEN", "MY_SECRET", "PASSWORD", "APP_PASSWD", "PASSPHRASE", "CREDENTIAL",
                 "CREDENTIALS", "COOKIE", "API_KEY", "APP_PRIVATE_KEY", "ACCESS_KEY", "SIGNING_KEY"]
        excluded = ["MYTOKEN", "SECRETIVE", "AUTH", "SESSION", "PASS", "KEY", "SSH_KEY_PATH",
                    "TOKEN_FILE", "SECRET_PATH", "PASSWORD_DIR", "API_KEY_SOCK"]
        for name in names + excluded:
            with self.subTest(name=name):
                expected = b"x[redacted]y" if name in names else b"xfixture-valuey"
                self.assertEqual(render(b"xfixture-valuey", {name: "fixture-value"}), expected)
        self.assertEqual(render(b"short12", {"TOKEN": "short12"}), b"short12")
        # A labelled value in a log or broker JSON never becomes a mask.
        data = b'{"token":"fixture-log-value"}\nunlabelled fixture-log-value\n'
        self.assertEqual(render(data), data)
        self.assertEqual(render(b"fixture-log-value", {"BROKER_CONTEXT": data.decode()}), b"fixture-log-value")

    def test_literal_env_values_can_cross_reads_and_newlines(self):
        value = "fixture-first\nfixture-second"
        data = b"x" * 65530 + value.encode() + b"\nlast line\n"
        self.assertEqual(render(data, {"TOKEN": value}), b"x" * 65530 + b"[redacted]\nlast line\n")
        self.assertEqual(render(b"abcdefgh-long-value", {"TOKEN": "abcdefgh", "SECRET": "abcdefgh-long-value"}), b"[redacted]")

    def test_known_token_shapes_only(self):
        tokens = [prefix + b"A1b" * 12 for prefix in (b"ghp_", b"gho_", b"ghu_", b"ghs_", b"ghr_")]
        tokens += [b"github_pat_" + b"Ab_1" * 5, b"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJmaXh0dXJlIn0.c2lnbmF0dXJl"]
        for token in tokens:
            self.assertEqual(render(b"token=" + token + b"\n"), b"token=[redacted]\n")
        for token in (b"ghp_" + b"A1b" * 11, b"sk-" + b"Ab_1" * 12,
                      b"aB3_dE6-gH9_jK2-mN5_pQ8-sT1_vW4-yZ7_cF0-iL3"):
            self.assertEqual(render(token), token)

    def test_hex64_tags_are_stable_and_short_ids_are_kept(self):
        first, second = b"0123456789abcdef" * 4, b"fedcba9876543210" * 4
        tag = b"[hex64:" + hashlib.sha256(first).hexdigest()[:8].encode() + b"]"
        self.assertEqual(render(first), tag)
        self.assertEqual(render(first), render(first))
        self.assertNotEqual(render(first), render(second))
        self.assertEqual(render(b"foo_" + first + b" name-7-" + first), b"foo_" + tag + b" name-7-" + tag)
        self.assertEqual(render(first[:40]), first[:40])
        self.assertEqual(render(first[:32]), first[:32])

    def test_paired_private_keys_require_a_matching_end(self):
        for kind in (b"PRIVATE KEY", b"RSA PRIVATE KEY", b"EC PRIVATE KEY", b"OPENSSH PRIVATE KEY", b"ENCRYPTED PRIVATE KEY"):
            data = b"before\n-----BEGIN " + kind + b"-----\nbody\n-----END OTHER PRIVATE KEY-----\n-----END " + kind + b"-----\nafter\n"
            self.assertEqual(render(data), b"before\n[redacted PEM]\nafter\n")

    def test_env_values_cannot_disable_pem_delimiters(self):
        data = b"before\n-----BEGIN PRIVATE KEY-----\nfixture-private-material\n-----END PRIVATE KEY-----\nafter\n"
        for value in ("PRIVATE KEY", "-----BEGIN PRIVATE KEY-----", "-----END PRIVATE KEY-----"):
            with self.subTest(value=value):
                self.assertEqual(render(data, {"FIXTURE_PASSWORD": value}), b"before\n[redacted PEM]\nafter\n")

    def test_key_larger_than_tail_is_removed_before_tail_cut(self):
        body = b"qwertyuiopASDFGHJKLzxcvbnmQWERTYUIasdfghjklZXCVBNM0123456789+/abcd\n" * 34000
        data = b"before\n-----BEGIN PRIVATE KEY-----\n" + body + b"-----END PRIVATE KEY-----\nafter\n"
        self.assertGreater(len(body), 2 << 20)
        tail, truncated = m.capture_tail(io.BytesIO(data), 128, m.Redactor({}))
        self.assertEqual(tail, b"before\n[redacted PEM]\nafter\n")
        self.assertFalse(truncated)

    def test_unclosed_key_keeps_text_after_128_following_lines(self):
        lines = [f"diagnostic line {index}\n".encode() for index in range(1, 201)]
        data = b"before\n-----BEGIN PRIVATE KEY-----\n" + b"".join(lines)
        expected = b"before\n[redacted PEM]\n" + b"".join(lines[128:])
        self.assertEqual(render(data), expected)
        tail, truncated = m.capture_tail(io.BytesIO(data), 64, m.Redactor({}))
        self.assertEqual(tail, expected[-64:])
        self.assertTrue(truncated)

    def test_unclosed_key_ends_at_the_next_begin(self):
        lines = [f"diagnostic line {index}\n".encode() for index in range(1, 141)]
        pair = b"-----BEGIN PRIVATE KEY-----\nsecond body\n-----END PRIVATE KEY-----\n"
        early = b"before\n-----BEGIN RSA PRIVATE KEY-----\nfirst body\n" + b"-----BEGIN RSA PRIVATE KEY-----\n" + pair + b"after\n"
        self.assertEqual(render(early), b"before\n[redacted PEM]\n[redacted PEM]\n[redacted PEM]\nafter\n")
        data = b"before\n-----BEGIN PRIVATE KEY-----\n" + b"".join(lines) + b"mid " + pair + b"after\n"
        expected = b"before\n[redacted PEM]\n" + b"".join(lines[128:]) + b"mid [redacted PEM]\nafter\n"
        self.assertEqual(render(data), expected)
        tail, truncated = m.capture_tail(io.BytesIO(data), 64, m.Redactor({}))
        self.assertEqual(tail, expected[-64:])
        self.assertTrue(truncated)

    def test_realistic_corpus_changes_only_the_four_matches(self):
        digest = b"ab12cd34" * 8
        token = b"ghs_" + b"Ab1" * 12
        prefix = (b"RUN profile:fixture\n--- FAIL: TestNormalizeLegacyYardConfigContent\n"
                  b"subyard-e2e-slot-1-owner-pool-generation-abc12\n"
                  b'{"capture_status":"recorded","status":"failed"}\n')
        data = prefix + b"fixture-env-value " + token + b" digest=" + digest + b"\n"
        data += b"-----BEGIN PRIVATE KEY-----\nfixture body\n-----END PRIVATE KEY-----\nRESULT failed exit=1\n"
        expected = prefix + b"[redacted] [redacted] digest=" + render(digest) + b"\n[redacted PEM]\nRESULT failed exit=1\n"
        self.assertEqual(render(data, {"TOKEN": "fixture-env-value"}), expected)


class Capture(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(dir="/tmp", prefix="subyard-diagnostics-unit-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.output, self.logs = self.root / "acceptance", self.root / "logs"
        self.output.mkdir()
        self.logs.mkdir()
        self.check = self.output / "vm-fixture-12345678.log"
        self.check.write_bytes(b"fixture check failed\nE2E_PHASE phase=cleanup/release exit_code=1\n")
        self.destination = self.root / "snapshot"

    def snapshot(self, **kwargs):
        printed = io.StringIO()
        with redirect_stdout(printed), patch.dict(os.environ, {}, clear=True):
            m.diagnostic_snapshot(self.output, self.destination, "vm:fixture", "failure", self.logs, **kwargs)
        return printed.getvalue(), json.loads((self.destination / "summary.json").read_text())

    def test_huge_events_and_attempts_cannot_displace_the_check(self):
        (self.output / "attempts").mkdir()
        payload = b'{"capture_status":"recorded","event":"heartbeat"}\n' * 8000
        (self.output / "attempts/huge.json").write_bytes(payload)
        (self.output / "events.jsonl").write_bytes(payload)
        with patch.object(m, "DIAGNOSTIC_FILE_BYTES", 128), patch.object(m, "DIAGNOSTIC_TOTAL_BYTES", 256):
            printed, summary = self.snapshot()
        self.assertIn("fixture check failed", printed)
        self.assertEqual(summary["files"][0]["path"], "acceptance/" + self.check.name)
        self.assertEqual((self.destination / "acceptance" / self.check.name).read_bytes(), self.check.read_bytes())
        self.assertTrue(all(item["reason"] for item in summary["skipped"]))
        self.assertLessEqual(sum((self.destination / item["path"]).stat().st_size for item in summary["files"]), 256)

    def test_print_budget_keeps_the_final_failure_line(self):
        payload = (b"x" * 512 + b"\n") * 79 + b"FINAL FAILURE: guest socket missing\n"
        self.check.write_bytes(payload)
        printed, _ = self.snapshot()
        tail = printed.splitlines()[1:]
        self.assertEqual(tail[-1], "| FINAL FAILURE: guest socket missing")
        self.assertLessEqual(len(tail), 80)
        self.assertLessEqual(len(("\n".join(tail) + "\n").encode()), 16384)
        self.assertEqual((self.destination / "acceptance" / self.check.name).read_bytes(), payload)

    def test_later_failure_keeps_the_already_printed_tail_and_summary(self):
        events = self.output / "events.jsonl"
        events.write_text("fixture event\n")
        original = Path.open
        printed = io.StringIO()
        def opened(path, *args, **kwargs):
            if path == events:
                self.assertIn("fixture check failed", printed.getvalue())
                self.assertTrue((self.destination / "acceptance" / self.check.name).is_file())
                raise PermissionError(errno.EACCES, "fixture refusal")
            return original(path, *args, **kwargs)
        with patch.object(Path, "open", opened), redirect_stdout(printed):
            m.diagnostic_snapshot(self.output, self.destination, "vm:fixture", "failure", self.logs)
        summary = json.loads((self.destination / "summary.json").read_text())
        self.assertEqual(summary["skipped"], [{"path": "acceptance/events.jsonl", "reason": "PermissionError errno=13"}])

    def test_fallback_probe_is_captured_before_metadata(self):
        self.check.unlink()
        (self.logs / "results.tsv").write_text("prepare-host\tfail(23)\t1\n")
        (self.logs / "prepare-host.log").write_text("fixture package installation refused\n")
        (self.output / "receipt.json").write_text('{"capture_status":"recorded"}\n')
        printed, summary = self.snapshot()
        self.assertIn("fixture package installation refused", printed)
        self.assertEqual(summary["files"][0]["path"], "runner/prepare-host.log")

    def test_unreadable_check_also_falls_back_to_the_probe(self):
        self.check.unlink()
        self.check.symlink_to(self.root / "unavailable")
        (self.logs / "results.tsv").write_text("prepare-host\tfail(23)\t1\n")
        (self.logs / "prepare-host.log").write_text("fallback fixture failure\n")
        printed, summary = self.snapshot()
        self.assertIn("fallback fixture failure", printed)
        self.assertEqual(summary["skipped"][0]["path"], "acceptance/" + self.check.name)

    def test_same_pass_for_all_files_and_printed_tail(self):
        payload = b"known fixture-env-value\n" + b"f" * 64 + b"\n"
        self.check.write_bytes(payload)
        (self.output / "events.jsonl").write_bytes(payload)
        (self.logs / "service-events.json").write_bytes(payload)
        with patch.dict(os.environ, {"TOKEN": "fixture-env-value"}), redirect_stdout(io.StringIO()) as printed:
            m.diagnostic_snapshot(self.output, self.destination, "vm:fixture", "failure", self.logs)
        expected = render(payload, {"TOKEN": "fixture-env-value"})
        for relative in ("acceptance/" + self.check.name, "acceptance/events.jsonl", "runner/service-events.json"):
            self.assertEqual((self.destination / relative).read_bytes(), expected)
        self.assertIn(expected.splitlines()[1].decode(), printed.getvalue())
        self.assertNotIn("fixture-env-value", printed.getvalue())

    def test_metadata_order_and_skip_reasons(self):
        (self.output / "runs").mkdir()
        (self.output / "attempts").mkdir()
        for name in ("runs/run.json", "receipt.json", "attempts/attempt.json", "events.jsonl"):
            (self.output / name).write_text('{"capture_status":"recorded"}\n')
        (self.logs / "linked.log").symlink_to(self.check)
        _, summary = self.snapshot()
        self.assertEqual([item["path"] for item in summary["files"][:5]], [
            "acceptance/" + self.check.name, "acceptance/runs/run.json", "acceptance/receipt.json",
            "acceptance/attempts/attempt.json", "acceptance/events.jsonl"])
        self.assertTrue(any(item["path"] == "runner/linked.log" and item["reason"] for item in summary["skipped"]))

    def test_p0_failure_snapshots_are_bounded_without_sweeping_other_evidence(self):
        state = self.root / "e2e"
        for route in ("services/owner/pool/generation", "yards/test-yard"):
            evidence = state / route / "evidence"
            evidence.mkdir(parents=True)
            (evidence / "p0-fixture.failure.log").write_bytes(b"padding\n" * 1000 + b"capacity: exhausted\nkernel: oom-kill\n")
            (evidence / "source.json").write_text("not collected")
            (evidence / "assets.tar.gz").write_text("not collected")
        with patch.object(m, "DIAGNOSTIC_FILE_BYTES", 128):
            self.snapshot(evidence=state)
        logs = list((self.destination / "p0").rglob("p0-fixture.failure.log"))
        self.assertEqual(len(logs), 2)
        for path in logs:
            self.assertLessEqual(path.stat().st_size, 128)
            self.assertIn(b"kernel: oom-kill", path.read_bytes())
        self.assertFalse(list(self.destination.rglob("source.json")))
        self.assertFalse(list(self.destination.rglob("assets.tar.gz")))


if __name__ == "__main__":
    unittest.main()
