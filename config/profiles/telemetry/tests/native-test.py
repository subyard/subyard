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
    def test_materializer_and_exporters_converge_in_both_orders_and_repeats(self):
        source = Path(__file__).resolve().parents[4]/'internal/adapters/configmaterial'
        guest = (source/'guest_json.py').read_text()
        writer = (source/'tomli_w.py').read_text()
        for first in ('agents', 'telemetry', 'together'):
            with self.subTest(first=first), tempfile.TemporaryDirectory(dir='/tmp') as name:
                root = Path(name)
                agents, selected = [], False
                managed = ['claude', 'codex']
                state = root/'native.json'
                def materialize(agent):
                    format = 'json' if agent == 'claude' else 'toml'
                    destination = root/('.claude/settings.json' if agent == 'claude' else '.codex/config.toml')
                    payload = b'{"permissions":{"defaultMode":"acceptEdits"}}' if agent == 'claude' else b'sandbox_mode = "danger-full-access"\napproval_policy = "on-request"\n'
                    program = guest.replace('@STATE_ROOT@', str(root/'materialization')).replace('@STATE_UID@', str(os.getuid())).replace('@FORMAT@', format)
                    if format == 'toml':
                        program = '_toml_writer = {}\nexec('+repr(writer)+', _toml_writer)\n'+program
                    digest = hashlib.sha256(payload).hexdigest()
                    result = subprocess.run([sys.executable, '-B', '-c', program, 'apply', 'dev', str(destination), str(os.getuid()), digest, str(root), digest], input=payload, capture_output=True)
                    self.assertEqual(result.returncode, 0, result.stderr.decode())
                initial = [('agents', ['claude']), ('telemetry', True)] if first == 'agents' else [('telemetry', True), ('agents', ['claude'])]
                if first == 'together':
                    initial = [('agents', ['claude', 'codex']), ('telemetry', True)]
                steps = initial+[('agents', ['claude', 'codex']), ('telemetry', True), ('telemetry', False), ('telemetry', True), ('agents', ['codex']), ('agents', ['claude', 'codex'])]
                for kind, value in steps:
                    if kind == 'agents':
                        agents = value
                        for agent in agents:
                            materialize(agent)
                    else:
                        selected = value
                    wanted = agents if selected else []
                    if kind == 'telemetry' and selected and not agents:
                        # The activation consumer can be selected before the materializer.
                        wanted = managed
                        diagnostics = []
                        self.assertFalse(native.reconcile(root, state, wanted, 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid(), check=True, diagnostics=diagnostics, managed_agents=managed))
                        self.assertEqual([item['installed'] for item in diagnostics], ['missing', 'missing'])
                        with self.assertRaisesRegex(ValueError, 'config apply'):
                            native.reconcile(root, state, wanted, 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid(), managed_agents=managed)
                        self.assertEqual(list(root.iterdir()), [], 'pending consumer wrote partial policy or ownership')
                        continue
                    for _ in range(2):
                        native.reconcile(root, state, wanted, 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid(), managed_agents=managed)
                        self.assertTrue(native.reconcile(root, state, wanted, 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid(), check=True, managed_agents=managed))
                    if 'codex' in agents:
                        document = tomllib.loads((root/'.codex/config.toml').read_text())
                        self.assertEqual(document['sandbox_mode'], 'danger-full-access')
                        self.assertEqual(document['approval_policy'], 'on-request')
                    if 'claude' in agents:
                        self.assertEqual(json.loads((root/'.claude/settings.json').read_text())['permissions']['defaultMode'], 'acceptEdits')

                # Model config apply repairing a managed document before exporter refresh.
                materialize('claude')
                (root/'.claude/settings.json').write_bytes(b'broken JSON')
                diagnostics = []
                self.assertFalse(native.reconcile(root, state, ['claude'], 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid(), check=True, diagnostics=diagnostics, managed_agents=['claude']))
                self.assertIn('telemetry.agent.claude.config.invalid', str(diagnostics))
                materialize('claude')
                native.reconcile(root, state, ['claude'], 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid(), managed_agents=['claude'])
                self.assertTrue(native.reconcile(root, state, ['claude'], 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid(), check=True, managed_agents=['claude']))

    def test_damaged_agent_inputs_defer_observation_and_repair_preserves_required_keys(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            state = root/'native.json'
            for agent in ('claude', 'codex'):
                agents = [agent]
                native.reconcile(root, state, agents, 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
                path = root/('.claude/settings.json' if agent == 'claude' else '.codex/config.toml')
                for cause, content in [('empty', b''), ('invalid', b'[broken'), ('missing', None)]:
                    with self.subTest(agent=agent, cause=cause):
                        if content is None:
                            path.unlink()
                        else:
                            path.write_bytes(content)
                        before = state.read_bytes()
                        diagnostics = []
                        self.assertFalse(native.reconcile(root, state, agents, 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid(), check=True, diagnostics=diagnostics, managed_agents=agents))
                        self.assertIn('telemetry.agent.'+agent+'.config.'+cause, [item['parameter'] for item in diagnostics])
                        with self.assertRaisesRegex(ValueError, 'config apply'):
                            native.reconcile(root, state, agents, 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid(), managed_agents=agents)
                        self.assertEqual(state.read_bytes(), before)
                        # The materializer repairs its own fields first.
                        path.write_text('{"permissions":{"defaultMode":"acceptEdits"}}\n' if agent == 'claude' else 'sandbox_mode = "danger-full-access"\napproval_policy = "on-request"\n')
                        for selection in (agents, agents, [], agents):
                            native.reconcile(root, state, selection, 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
                            value = json.loads(path.read_bytes()) if agent == 'claude' else tomllib.loads(path.read_text())
                            if agent == 'codex':
                                self.assertEqual(value['sandbox_mode'], 'danger-full-access')
                                self.assertEqual(value['approval_policy'], 'on-request')
                            else:
                                self.assertEqual(value['permissions']['defaultMode'], 'acceptEdits')

    def test_unmanaged_empty_documents_converge_and_invalid_documents_name_preservation(self):
        for agent in ('claude', 'codex'):
            with self.subTest(agent=agent), tempfile.TemporaryDirectory(dir='/tmp') as name:
                root = Path(name)
                path = root/('.claude/settings.json' if agent == 'claude' else '.codex/config.toml')
                path.parent.mkdir()
                path.write_bytes(b'')
                state = root/'native.json'
                native.reconcile(root, state, [agent], 'http://localhost:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
                self.assertTrue(native.reconcile(root, state, [agent], 'http://localhost:14318', 'x'*64, tomli_w, os.getuid(), os.getgid(), check=True))
                path.write_bytes(b'[broken')
                before = state.read_bytes()
                with self.assertRaises(native.AgentConfigRefusal) as caught:
                    native.reconcile(root, state, [agent], 'http://localhost:14318', 'x'*64, tomli_w, os.getuid(), os.getgid(), check=True)
                self.assertIn(str(path), str(caught.exception))
                self.assertEqual(caught.exception.agent, agent)
                self.assertNotIn('config apply', str(caught.exception))
                self.assertEqual(state.read_bytes(), before)
                self.assertEqual(path.read_bytes(), b'[broken')

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
