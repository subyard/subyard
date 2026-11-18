#!/usr/bin/env python3
import json
import hashlib
import os
from pathlib import Path
import sys
import subprocess
import tempfile
import tomllib
import unittest
from unittest.mock import patch
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
sys.path.insert(0, str(Path(__file__).resolve().parents[4]/'internal/adapters/configmaterial'))
import native
import tomli_w


class NativeTest(unittest.TestCase):
    def setUp(self):
        self.root = Path(self.enterContext(tempfile.TemporaryDirectory(dir='/tmp')))
        self.state_root = self.root/'materialization'
        self.enterContext(patch.object(native.materializer, 'STATE_ROOT', str(self.state_root)))
        self.enterContext(patch.object(native.materializer, 'STATE_UID', os.getuid()))

    def materialize(self, agent, path, payload, mode='apply'):
        file_format = 'json' if agent == 'claude' else 'toml'
        program = Path(native.materializer.__file__).read_text()
        program = program.replace('@STATE_ROOT@', str(self.state_root)).replace('@STATE_UID@', str(os.getuid())).replace('@FORMAT@', file_format)
        if file_format == 'toml':
            program = '_toml_writer = {}\nexec('+repr(Path(tomli_w.__file__).read_text())+', _toml_writer)\n'+program
        digest = hashlib.sha256(payload).hexdigest()
        return subprocess.run([sys.executable, '-B', '-c', program, mode, path.parents[1].name,
                               str(path), str(os.getuid()), digest, str(path.parents[1]), digest],
                              input=payload, capture_output=True)

    def test_repair_mode_when_exporters_already_match(self):
        for agent in ('claude', 'codex'):
            with self.subTest(agent=agent), tempfile.TemporaryDirectory(dir='/tmp') as name:
                root = Path(name)
                args = (root, root/'native.json', [agent], 'http://localhost:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
                native.reconcile(*args)
                path, before, _, _ = native.agent_document(root, agent)
                path.chmod(0o644)
                self.assertFalse(native.reconcile(*args, check=True))
                self.assertEqual(path.stat().st_mode & 0o777, 0o644)
                for _ in range(2):
                    native.reconcile(*args)
                    self.assertTrue(native.reconcile(*args, check=True))
                    self.assertEqual(path.stat().st_mode & 0o777, 0o600)
                    self.assertEqual(path.read_bytes(), before)

    def test_template_and_exporters_converge_in_both_orders_and_repair_damage(self):
        for agent, payload in [('claude', b'{"unrelated":true}'), ('codex', b'unrelated = true\n')]:
            for profile_first in (False, True):
                with self.subTest(agent=agent, profile_first=profile_first), tempfile.TemporaryDirectory(dir='/tmp') as name:
                    root = Path(name)
                    state = root/'native.json'
                    args = (root, state, [agent], 'http://localhost:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
                    templates = {agent: payload}
                    path = native.agent_document(root, agent)[0]
                    if profile_first:
                        native.reconcile(*args, templates=templates)
                        adopted = self.materialize(agent, path, payload, 'assess-adopt')
                        self.assertEqual(adopted.returncode, 0, adopted.stderr)
                        self.assertTrue(json.loads(adopted.stdout)['adoptable'])
                    applied = self.materialize(agent, path, payload)
                    self.assertEqual(applied.returncode, 0, applied.stderr)
                    native.reconcile(*args, templates=templates)
                    for damaged in (None, b'', b'[broken'):
                        if damaged is None:
                            path.unlink()
                        else:
                            path.write_bytes(damaged)
                        self.assertTrue(native.reconcile(*args, check=True, templates=templates))
                        observed = self.materialize(agent, path, payload, 'observe')
                        self.assertEqual(observed.returncode, 0, observed.stderr)
                        self.assertFalse(json.loads(observed.stdout)['converged'])
                        repaired = self.materialize(agent, path, payload)
                        self.assertEqual(repaired.returncode, 0, repaired.stderr)
                        self.assertEqual(path.stat().st_mode & 0o777, 0o600)
                        _, _, values, cause = native.agent_document(root, agent)
                        self.assertEqual(cause, '')
                        self.assertIs(values['unrelated'], True)
                        self.assertEqual(values['env' if agent == 'claude' else 'otel'],
                                         native.desired(agent, args[3], args[4]))

    def test_template_conflict_refuses_before_document_write(self):
        for agent, field, payload in [
            ('claude', '/env/OTEL_LOG_USER_PROMPTS', b'{"env":{"OTEL_LOG_USER_PROMPTS":"template-value"}}'),
            ('codex', '/otel/log_user_prompt', b'[otel]\nlog_user_prompt = "template-value"\n'),
        ]:
            with self.subTest(agent=agent), tempfile.TemporaryDirectory(dir='/tmp') as name:
                root = Path(name)
                args = (root, root/'native.json', [agent], 'http://localhost:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
                path = native.agent_document(root, agent)[0]
                with self.assertRaisesRegex(native.materializer.ContributionConflict, field) as error:
                    native.reconcile(*args, templates={agent: payload})
                self.assertNotIn('template-value', str(error.exception))
                self.assertFalse(path.exists())
                native.reconcile(*args)
                before = path.read_bytes()
                for mode in ('observe', 'apply'):
                    refused = self.materialize(agent, path, payload, mode)
                    self.assertEqual(refused.returncode, 2)
                    self.assertIn(field.encode(), refused.stderr)
                    self.assertNotIn(b'template-value', refused.stderr)
                    self.assertEqual(path.read_bytes(), before)

    def test_migrate_legacy_ownership_and_retry_interrupted_registration(self):
        for agent in ('claude', 'codex'):
            with self.subTest(agent=agent), tempfile.TemporaryDirectory(dir='/tmp') as name:
                root = Path(name)
                state = root/'native.json'
                endpoint, token = 'http://localhost:14318', 'x'*64
                args = (root, state, [agent], endpoint, token, tomli_w, os.getuid(), os.getgid())
                path = native.agent_document(root, agent)[0]
                path.parent.mkdir()
                managed = native.desired(agent, endpoint, token)
                section, field, prior = ('env', 'OTEL_LOGS_EXPORTER', 'console') if agent == 'claude' else ('otel', 'log_user_prompt', True)
                values = {section: managed}
                path.write_text(json.dumps(values) if agent == 'claude' else tomli_w.dumps(values))
                path.chmod(0o644)
                owned = {key: {'managed': value, 'present': key == field, 'value': prior if key == field else None}
                         for key, value in managed.items()}
                native.publish(state, json.dumps({'schema': 1, 'agents': {agent: owned}}).encode(), None)
                with patch.object(native.materializer, 'replace_destination', side_effect=OSError('synthetic interruption')):
                    with self.assertRaises(OSError):
                        native.reconcile(*args)
                self.assertFalse(native.reconcile(*args, check=True))
                native.reconcile(*args)
                self.assertTrue(native.reconcile(*args, check=True))
                self.assertEqual(path.stat().st_mode & 0o777, 0o600)
                self.assertEqual(json.loads(state.read_bytes())['agents'][agent], owned)
                # A retained release reads and writes the same schema, including
                # the original values. Its release leaves no active contribution.
                values = {section: {field: prior}}
                path.write_text(json.dumps(values) if agent == 'claude' else tomli_w.dumps(values))
                native.publish(state, b'{"schema":1,"agents":{}}', state.read_bytes())
                payload = b'{"env":{"OTEL_LOGS_EXPORTER":"console"}}' if agent == 'claude' else b'[otel]\nlog_user_prompt = true\n'
                result = self.materialize(agent, path, payload)
                self.assertEqual(result.returncode, 0, result.stderr)

    def test_unmanaged_empty_documents_converge_and_invalid_documents_name_preservation(self):
        for agent in ('claude', 'codex'):
            with self.subTest(agent=agent), tempfile.TemporaryDirectory(dir='/tmp') as name:
                root = Path(name)
                path = root/('.claude/settings.json' if agent == 'claude' else '.codex/config.toml')
                path.parent.mkdir()
                state = root/'native.json'
                args = (root, state, [agent], 'http://localhost:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
                path.write_bytes(b'[broken')
                with self.assertRaises(native.AgentConfigRefusal) as caught:
                    native.reconcile(*args, check=True)
                self.assertIn(str(path), str(caught.exception))
                self.assertEqual(caught.exception.agent, agent)
                self.assertFalse(state.exists())
                self.assertEqual(path.read_bytes(), b'[broken')
                path.write_bytes(b'')
                native.reconcile(*args)
                self.assertTrue(native.reconcile(*args, check=True))
                path.write_bytes(b'[broken')
                self.assertFalse(native.reconcile(*args, check=True))
                native.reconcile(*args)
                self.assertTrue(native.reconcile(*args, check=True))

    def test_format_only_native_changes_preserve_bytes(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            state = root/'native.json'
            agents = ['codex', 'claude']
            native.reconcile(root, state, agents, 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
            codex = root/'.codex/config.toml'
            codex.write_text('# Keep this comment and spacing.\n'+codex.read_text().replace('log_user_prompt = false', 'log_user_prompt=false')+
                             '\n[unrelated]\nnumber = 0\nenabled = false\nvalues = [nan, -nan, inf, -inf]\n'+
                             'date = 2026-10-30\ntime = 12:34:56\nlocal = 2026-10-30T12:34:56\noffset = 2026-10-30T12:34:56Z\n')
            claude = root/'.claude/settings.json'
            value = json.loads(claude.read_bytes())
            value['unrelated'] = {'keep': ['personal', 0, False]}
            value['env'] = dict(reversed(list(value['env'].items())))
            claude.write_text(json.dumps(value, separators=(',', ':'))+'\n')
            before = {path: path.read_bytes() for path in (codex, claude)}
            ownership = state.read_bytes()
            original_dumps = json.dumps
            def state_only_dumps(value, *args, **kwargs):
                self.assertNotIn('env', value, 'unchanged native configuration must not be serialized')
                return original_dumps(value, *args, **kwargs)
            with patch.object(tomli_w, 'dumps', side_effect=AssertionError('unchanged TOML must not be serialized')), \
                 patch.object(native.json, 'dumps', side_effect=state_only_dumps):
                self.assertTrue(native.reconcile(root, state, agents, 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid(), check=True))
                self.assertEqual(state.read_bytes(), ownership, 'check must not change ownership')
                native.reconcile(root, state, agents, 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
            self.assertEqual({path: path.read_bytes() for path in before}, before)

    def test_missing_and_prior_typed_native_values_require_reconciliation(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            (root/'.codex').mkdir(mode=0o700)
            path = root/'.codex/config.toml'
            path.write_text('[otel]\nlog_user_prompt = 0\n')
            path.chmod(0o600)
            state = root/'native.json'
            native.reconcile(root, state, ['codex'], 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
            for change in ('missing', 'prior integer'):
                with self.subTest(change=change):
                    value = tomllib.loads(path.read_text())
                    if change == 'missing':
                        value['otel'].pop('metrics_exporter')
                    else:
                        value['otel']['log_user_prompt'] = 0
                    path.write_text(tomli_w.dumps(value))
                    self.assertFalse(native.reconcile(root, state, ['codex'], 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid(), check=True))
                    native.reconcile(root, state, ['codex'], 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
                    self.assertIs(tomllib.loads(path.read_text())['otel']['log_user_prompt'], False)
                    self.assertTrue(native.reconcile(root, state, ['codex'], 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid(), check=True))
            native.reconcile(root, state, [], '', '', tomli_w, os.getuid(), os.getgid())
            restored = tomllib.loads(path.read_text())['otel']['log_user_prompt']
            self.assertIs(type(restored), int)
            self.assertEqual(restored, 0)

    def test_managed_boolean_rejects_numeric_foreign_edit(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            state = root/'native.json'
            native.reconcile(root, state, ['codex'], 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
            path = root/'.codex/config.toml'
            path.write_text(path.read_text().replace('log_user_prompt = false', 'log_user_prompt = 0'))
            before, ownership = path.read_bytes(), state.read_bytes()
            for check in (True, False):
                with self.subTest(check=check), self.assertRaisesRegex(ValueError, 'ownership conflict'):
                    native.reconcile(root, state, ['codex'], 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid(), check=check)
            self.assertEqual(path.read_bytes(), before)
            self.assertEqual(state.read_bytes(), ownership)
            native.reconcile(root, state, [], '', '', tomli_w, os.getuid(), os.getgid())
            preserved = tomllib.loads(path.read_text())['otel']['log_user_prompt']
            self.assertIs(type(preserved), int)
            self.assertEqual(preserved, 0)

    def test_existing_exporter_leaves_are_restored_without_old_owner_cleanup(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            prior = {'otel': {
                'exporter': {'otlp-http': {'endpoint': 'http://127.0.0.1:4318/v1/logs', 'protocol': 'binary'}},
                'metrics_exporter': {'otlp-http': {'endpoint': 'http://127.0.0.1:4318/v1/metrics', 'protocol': 'binary'}},
                'trace_exporter': {'otlp-http': {'endpoint': 'http://127.0.0.1:4318/v1/traces', 'protocol': 'binary'}},
                'log_user_prompt': False}}
            path = root/'.codex/config.toml'
            path.parent.mkdir(mode=0o700)
            path.write_text(tomli_w.dumps(prior))
            path.chmod(0o600)
            claude = {'env': {
                'CLAUDE_CODE_ENABLE_TELEMETRY': '1',
                'OTEL_METRICS_EXPORTER': 'otlp', 'OTEL_LOGS_EXPORTER': 'otlp',
                'OTEL_EXPORTER_OTLP_LOGS_ENDPOINT': 'http://127.0.0.1:4318/v1/logs',
                'OTEL_EXPORTER_OTLP_METRICS_ENDPOINT': 'http://127.0.0.1:4318/v1/metrics',
                'OTEL_EXPORTER_OTLP_LOGS_PROTOCOL': 'http/protobuf',
                'OTEL_EXPORTER_OTLP_METRICS_PROTOCOL': 'http/protobuf',
                'OTEL_EXPORTER_OTLP_LOGS_HEADERS': '', 'OTEL_EXPORTER_OTLP_METRICS_HEADERS': '',
                'OTEL_LOG_USER_PROMPTS': '0'}}
            claude_path = root/'.claude/settings.json'
            claude_path.parent.mkdir(mode=0o700)
            claude_path.write_text(json.dumps(claude))
            claude_path.chmod(0o600)
            legacy = root/'old-owner/native.json'
            legacy.parent.mkdir(mode=0o700)
            legacy.write_bytes(b'old receipt is outside current ownership')
            before = legacy.read_bytes()
            state = root/'native.json'
            native.reconcile(root, state, ['claude', 'codex'], 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
            self.assertTrue(native.reconcile(root, state, ['claude', 'codex'], 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid(), check=True))
            native.reconcile(root, state, [], '', '', tomli_w, os.getuid(), os.getgid())
            self.assertEqual(tomllib.loads(path.read_text()), prior)
            self.assertEqual(json.loads(claude_path.read_text()), claude)
            self.assertEqual(legacy.read_bytes(), before)

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
