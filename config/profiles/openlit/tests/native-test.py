#!/usr/bin/env python3
import copy
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
import sys
import tomllib

ROOT = Path(__file__).resolve().parents[4]
sys.path.insert(0, str(ROOT / 'internal/adapters/configmaterial'))
import tomli_w
spec = importlib.util.spec_from_file_location('native', Path(__file__).resolve().parents[1] / 'native.py')
native = importlib.util.module_from_spec(spec)
spec.loader.exec_module(native)


class NativeTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='subyard-openlit-', dir='/tmp')
        self.addCleanup(self.tmp.cleanup)
        self.home = Path(self.tmp.name) / 'home'
        self.home.mkdir(mode=0o700)
        self.state = Path(self.tmp.name) / 'state.json'
        self.claude = self.home / '.claude/settings.json'
        self.codex = self.home / '.codex/config.toml'

    def run_mode(self, agents='claude codex', mode='apply'):
        native.reconcile(self.home, self.state, agents.split(), mode, os.getuid(), os.getgid(), tomli_w)

    def seed(self, claude, codex):
        native.write(self.claude, json.dumps(claude))
        native.write(self.codex, tomli_w.dumps(codex))

    def test_enable_repeat_remove_preserves_settings_and_prior_exporters(self):
        claude = {'env': {'CUSTOM': 'keep', 'OTEL_LOGS_EXPORTER': 'console'}, 'hooks': {'PreToolUse': []}}
        codex = {'model': 'custom', 'otel': {'environment': 'test', 'exporter': 'none'}, 'tui': {'foo': True}}
        self.seed(claude, codex)
        self.run_mode()
        self.run_mode(mode='check')
        before = self.codex.read_bytes()
        self.run_mode()
        self.assertEqual(before, self.codex.read_bytes())
        self.run_mode('', 'apply')
        self.run_mode('', 'check')
        self.assertEqual(native.load(self.claude, 'claude'), claude)
        self.assertEqual(native.load(self.codex, 'codex'), codex)
        self.assertEqual(self.state.stat().st_mode & 0o777, 0o600)

    def test_selection_changes_remove_only_unselected_overlay(self):
        self.seed({'env': {'CUSTOM': 'keep'}}, {'otel': {'environment': 'custom'}})
        self.run_mode()
        self.run_mode('codex')
        self.assertEqual(native.load(self.claude, 'claude'), {'env': {'CUSTOM': 'keep'}})
        self.run_mode('codex', 'check')
        self.run_mode('claude')
        self.assertEqual(native.load(self.codex, 'codex'), {'otel': {'environment': 'custom'}})

    def test_changed_managed_field_fails_apply_but_disable_preserves_user_edit(self):
        self.run_mode()
        obj = native.load(self.claude, 'claude')
        obj['env']['OTEL_EXPORTER_OTLP_LOGS_ENDPOINT'] = 'http://custom:4318'
        obj['hooks'] = {'custom': []}
        native.write(self.claude, json.dumps(obj))
        before = self.claude.read_bytes()
        for mode in ('check', 'apply', 'validate'):
            with self.assertRaises(ValueError):
                self.run_mode(mode=mode)
        self.assertEqual(before, self.claude.read_bytes())
        self.run_mode('')
        self.assertEqual(native.load(self.claude, 'claude'), {
            'env': {'OTEL_EXPORTER_OTLP_LOGS_ENDPOINT': 'http://custom:4318'}, 'hooks': {'custom': []}})

    def test_partial_enable_and_disable_retry(self):
        self.seed({'env': {'CUSTOM': 'keep'}}, {'model': 'custom'})
        original = native.write
        calls = []
        def fail_second_config(path, text, *args):
            calls.append(path)
            if path == self.codex:
                raise OSError('simulated interruption')
            original(path, text, *args)
        native.write = fail_second_config
        try:
            with self.assertRaises(OSError):
                self.run_mode()
        finally:
            native.write = original
        self.run_mode()
        native.write = fail_second_config
        try:
            with self.assertRaises(OSError):
                self.run_mode('')
        finally:
            native.write = original
        self.run_mode('')
        self.assertEqual(native.load(self.claude, 'claude'), {'env': {'CUSTOM': 'keep'}})
        self.assertEqual(native.load(self.codex, 'codex'), {'model': 'custom'})

    def test_validation_is_read_only_and_templates_cannot_own_overlay(self):
        self.seed({'env': {'CUSTOM': 'keep'}}, {'otel': {'environment': 'dev'}})
        self.run_mode(mode='validate')
        self.assertFalse(self.state.exists())
        native.template_check(self.claude, 'claude')
        native.template_check(self.codex, 'codex')
        native.write(self.codex, '[otel]\nexporter = "none"\n')
        with self.assertRaises(ValueError):
            native.template_check(self.codex, 'codex')

    def test_shared_trace_routing_is_preserved_while_log_metric_routes_change(self):
        shared = {'OTEL_TRACES_EXPORTER': 'otlp',
                  'OTEL_EXPORTER_OTLP_ENDPOINT': 'https://trace-fixture.invalid',
                  'OTEL_EXPORTER_OTLP_PROTOCOL': 'grpc',
                  'OTEL_EXPORTER_OTLP_HEADERS': 'authorization=synthetic-trace-token'}
        self.seed({'env': shared}, {})
        self.run_mode()
        env = native.load(self.claude, 'claude')['env']
        self.assertEqual({key: env[key] for key in shared}, shared)
        self.assertEqual(env['OTEL_EXPORTER_OTLP_LOGS_ENDPOINT'], 'http://127.0.0.1:4318/v1/logs')
        self.assertEqual(env['OTEL_EXPORTER_OTLP_METRICS_PROTOCOL'], 'http/protobuf')
        self.run_mode('')
        self.assertEqual(native.load(self.claude, 'claude'), {'env': shared})

    def test_pinned_template_preserves_engine_anchor_and_rejects_nested_symlinks(self):
        self.seed({'env': {'CUSTOM': 'keep'}}, {})
        fd = os.open(self.home, os.O_DIRECTORY)
        try:
            source = Path(f'/proc/{os.getpid()}/fd/{fd}')
            alias = Path(f'/proc/self/fd/{fd}')
            native.template_check(alias/'.claude/settings.json', 'claude', source)
            native.write(self.claude, json.dumps({'env': {'OTEL_LOGS_EXPORTER': 'none'}}))
            with self.assertRaises(ValueError):
                native.template_check(alias/'.claude/settings.json', 'claude', source)
            link = self.home/'template-link'
            link.symlink_to(self.home/'.codex', target_is_directory=True)
            with self.assertRaises(ValueError):
                native.template_check(alias/'template-link/config.toml', 'codex', source)
        finally:
            os.close(fd)

    def test_native_recipe_update_rollback_and_interruption_restore_prior_values(self):
        self.seed({'env': {'OTEL_LOGS_EXPORTER': 'console'}}, {'model': 'custom'})
        self.run_mode()
        original = copy.deepcopy(native.CLAUDE)
        writer = native.write
        def interrupted(path, content, *args):
            if path == self.codex:
                raise OSError('simulated interruption')
            writer(path, content, *args)
        try:
            native.CLAUDE['OTEL_EXPORTER_OTLP_LOGS_ENDPOINT'] = 'http://127.0.0.1:14318/v1/logs'
            native.CLAUDE['NEW_SIGNAL_SETTING'] = 'new'
            del native.CLAUDE['OTEL_EXPORTER_OTLP_LOGS_HEADERS']
            # Interrupt after a changed first agent was published, then prove retry
            # recognizes both prior and pending managed values.
            native.CODEX['log_user_prompt'] = True
            native.write = interrupted
            with self.assertRaises(OSError):
                self.run_mode()
            native.write = writer
            self.run_mode()
            self.run_mode(mode='check')
            native.CLAUDE.clear()
            native.CLAUDE.update(original)
            native.CODEX['log_user_prompt'] = False
            self.run_mode()
            self.run_mode(mode='check')
            self.assertNotIn('NEW_SIGNAL_SETTING', native.load(self.claude, 'claude')['env'])
            self.run_mode('')
            self.assertEqual(native.load(self.claude, 'claude'), {'env': {'OTEL_LOGS_EXPORTER': 'console'}})
        finally:
            native.CLAUDE.clear()
            native.CLAUDE.update(original)
            native.CODEX['log_user_prompt'] = False
            native.write = writer

    def test_invalid_config_and_symlink_are_not_overwritten(self):
        self.seed({}, {})
        self.claude.write_text('{ invalid JSON')
        before = self.claude.read_bytes()
        with self.assertRaises(ValueError):
            self.run_mode()
        self.assertEqual(before, self.claude.read_bytes())
        self.claude.unlink()
        self.claude.symlink_to(self.codex)
        with self.assertRaises(ValueError):
            self.run_mode()


if __name__ == '__main__':
    unittest.main()
