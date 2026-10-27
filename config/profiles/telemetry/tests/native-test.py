#!/usr/bin/env python3
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
sys.path.insert(0, str(Path(__file__).resolve().parents[4]/'internal/adapters/configmaterial'))
import native
import tomli_w


class NativeTest(unittest.TestCase):
    def test_legacy_handoff_restores_before_claim_and_refuses_foreign_edits(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            legacy = root/'legacy/native.json'
            legacy.parent.mkdir(mode=0o700)
            (legacy.parent/'marker').write_bytes(b'subyard-openlit-v1\n')
            native.reconcile(root, legacy, ['codex'], 'http://old-local:4318', 'old', tomli_w, os.getuid(), os.getgid())
            original_stat = Path.stat
            def metadata(path, *args, **kwargs):
                info = original_stat(path, *args, **kwargs)
                if path in (legacy.parent, legacy.parent/'marker'):
                    values = list(info)
                    values[4] = 0
                    return os.stat_result(values)
                return info
            with patch.object(Path, 'stat', metadata):
                native.relinquish_openlit(root, legacy, tomli_w, os.getuid(), os.getgid())
                self.assertEqual(json.loads(legacy.read_text())['agents'], {})
                self.assertNotIn('old-local', (root/'.codex/config.toml').read_text())
                native.reconcile(root, legacy, ['claude'], 'http://old-local:4318', 'old', tomli_w, os.getuid(), os.getgid())
                path = root/'.claude/settings.json'
                value = json.loads(path.read_text())
                value['env']['OTEL_LOGS_EXPORTER'] = 'foreign'
                path.write_text(json.dumps(value))
                before = path.read_bytes()
                state = legacy.read_bytes()
                with self.assertRaisesRegex(ValueError, 'legacy native exporter changed'):
                    native.relinquish_openlit(root, legacy, tomli_w, os.getuid(), os.getgid())
                self.assertEqual(path.read_bytes(), before)
                self.assertEqual(legacy.read_bytes(), state)

    def test_claim_restore_privacy_and_analytics_intent(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            (root/'.codex').mkdir(mode=0o700)
            config = root/'.codex/config.toml'
            config.write_text('[analytics]\nenabled = false\n[otel]\nlog_user_prompt = true\n')
            config.chmod(0o600)
            state = root/'native.json'
            native.reconcile(root, state, ['codex', 'claude'], 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
            self.assertIn('enabled = false', config.read_text())
            claude = json.loads((root/'.claude/settings.json').read_text())['env']
            self.assertEqual(claude['OTEL_LOG_TOOL_DETAILS'], '0')
            self.assertEqual(claude['OTEL_TRACES_EXPORTER'], 'otlp')
            self.assertTrue(native.reconcile(root, state, ['codex', 'claude'], 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid(), check=True))
            native.reconcile(root, state, [], '', '', tomli_w, os.getuid(), os.getgid())
            self.assertIn('log_user_prompt = true', config.read_text())
            self.assertNotIn('Bearer', config.read_text())
            self.assertEqual(json.loads(state.read_text())['agents'], {})
            self.assertEqual(state.stat().st_mode & 0o777, 0o600)

    def test_foreign_edit_refused_then_preserved_on_disable(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            state = root/'native.json'
            native.reconcile(root, state, ['claude'], 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
            path = root/'.claude/settings.json'
            value = json.loads(path.read_text())
            value['env']['OTEL_LOGS_EXPORTER'] = 'console'
            path.write_text(json.dumps(value))
            with self.assertRaisesRegex(ValueError, 'ownership conflict'):
                native.reconcile(root, state, ['claude'], 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
            native.reconcile(root, state, [], '', '', tomli_w, os.getuid(), os.getgid())
            self.assertEqual(json.loads(path.read_text())['env'], {'OTEL_LOGS_EXPORTER': 'console'})


if __name__ == '__main__':
    unittest.main()
