#!/usr/bin/env python3
import json
import os
from pathlib import Path
import re
import stat
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
sys.path.insert(0, str(Path(__file__).resolve().parents[4]/'internal/adapters/configmaterial'))
import native
import tomli_w


class NativeTest(unittest.TestCase):
    def setUp(self):
        self.root = Path(self.enterContext(tempfile.TemporaryDirectory(dir='/tmp')))
        self.etc, self.home = self.root/'etc', self.root/'home'
        self.etc.mkdir()
        self.receipt = self.root/'state/native.json'
        self.endpoint = 'http://127.0.0.1:14318'
        self.token = 'a'*64
        self.uid, self.gid = os.getuid(), os.getgid()
        self.paths = {agent: self.etc/parts[0] for agent, parts in native.AGENTS.items()}
        self.documents = {agent: self.home/parts[1] for agent, parts in native.AGENTS.items()}

    def reconcile(self, agents, check=False, templates=None):
        return native.reconcile(self.etc, self.home, self.receipt, agents, self.endpoint, self.token,
                                tomli_w, self.uid, self.gid, templates or {}, check)

    def test_managed_policy_lifecycle_does_not_read_or_change_home_documents(self):
        native.publish(self.documents['claude'], b'invalid home document', None, mode=0o400)
        agents = list(self.paths)
        self.assertFalse(self.reconcile(agents, check=True))
        self.assertFalse(any(path.exists() for path in self.paths.values()))
        self.reconcile(agents)
        codex = self.paths['codex']
        claude = self.paths['claude']
        otel = native.parse('codex', codex.read_bytes())['otel']
        for key, signal in (('exporter', 'logs'), ('metrics_exporter', 'metrics'), ('trace_exporter', 'traces')):
            self.assertEqual(otel[key]['otlp-http'], {'endpoint': self.endpoint+'/v1/'+signal, 'protocol': 'binary',
                                                     'headers': {'Authorization': 'Bearer '+self.token}})
        env = native.parse('claude', claude.read_bytes())['env']
        self.assertEqual(env['OTEL_EXPORTER_OTLP_LOGS_HEADERS'], 'Authorization=Bearer '+self.token)
        self.assertFalse(otel['log_user_prompt'])
        self.assertNotIn('analytics', native.parse('codex', codex.read_bytes()))
        for path in (claude, codex):
            info = path.stat()
            self.assertEqual((info.st_uid, info.st_gid, stat.S_IMODE(info.st_mode)), (self.uid, self.gid, 0o640))
        previous = codex.read_bytes()
        codex.chmod(0o600)
        claude.write_bytes(claude.read_bytes()+b'\n')
        self.assertFalse(self.reconcile(agents, check=True))
        self.assertEqual(stat.S_IMODE(codex.stat().st_mode), 0o600)
        self.reconcile(agents)
        self.assertEqual(codex.read_bytes(), previous)
        self.assertTrue(self.reconcile(agents, check=True))
        self.reconcile(['claude'])
        self.assertFalse(codex.exists())
        self.reconcile([])
        self.reconcile([])
        self.assertTrue(self.reconcile([], check=True))
        self.assertFalse(claude.exists())
        self.assertEqual(self.documents['claude'].read_bytes(), b'invalid home document')
        self.assertEqual(stat.S_IMODE(self.documents['claude'].stat().st_mode), 0o400)
        self.assertFalse(self.documents['codex'].parent.exists())
        self.assertFalse(self.receipt.exists())

    def test_foreign_policies_refuse_before_effects_in_stable_order(self):
        self.reconcile(['claude'])
        claude, codex = (self.paths[agent] for agent in native.AGENTS)
        claude.write_bytes(b'{"env":{}}')
        codex.parent.mkdir()
        codex.symlink_to(claude)
        for check in (False, True):
            with self.assertRaisesRegex(native.Refusal, re.escape(str(claude))+'.*'+re.escape(str(codex))):
                self.reconcile([], check=check)
            self.assertEqual(claude.read_bytes(), b'{"env":{}}')
            self.assertTrue(codex.is_symlink())

    def test_template_conflicts_compare_desired_fields_without_home_or_secret_output(self):
        agents = list(self.paths)
        templates = {'claude': b'{"env":{"OTEL_LOGS_EXPORTER":"otlp","EXTRA":"anything"}}',
                     'codex': b'[otel]\nlog_user_prompt = false\n'}
        self.reconcile(agents, templates=templates)
        self.assertTrue(self.reconcile(agents, check=True, templates=templates))
        templates = {'claude': b'{"env":{"OTEL_LOGS_EXPORTER":"private-template-value"}}',
                     'codex': b'[otel.exporter.otlp-http.headers]\nAuthorization = "private-template-value"\n'}
        with self.assertRaisesRegex(native.Refusal, r'/env/OTEL_LOGS_EXPORTER.*?/otel/exporter/otlp-http/headers/Authorization') as error:
            self.reconcile(agents, templates=templates)
        self.assertNotIn('private-template-value', str(error.exception))
        self.assertNotIn(self.token, str(error.exception))

    def test_v030_cleanup_restores_unchanged_leaves_and_resumes_without_rotating_token(self):
        fixture = Path(__file__).with_name('fixtures')/'v0.30.0-native.py.tar.gz'
        with tarfile.open(fixture) as archive:
            legacy_source = archive.extractfile('config/profiles/telemetry/native.py').read()
        legacy = {}
        exec(compile(legacy_source, 'v0.30.0/native.py', 'exec'), legacy)
        claude, codex = (self.documents[agent] for agent in native.AGENTS)
        native.publish(claude, b'{"env":{"OTEL_LOGS_EXPORTER":"console","UNRELATED":"keep"}}\n', None)
        prior = {'analytics': {'enabled': False},
                 'otel': {'log_user_prompt': True, 'exporter': {'otlp-http': {'endpoint': 'https://original.invalid',
                           'protocol': 'json', 'headers': {'Authorization': 'original', 'Other': 'original'}}}}}
        native.publish(codex, tomli_w.dumps(prior).encode(), None)
        agents = list(self.paths)
        legacy['reconcile'](self.home, self.receipt, agents, self.endpoint, self.token, tomli_w, self.uid, self.gid)

        before = native.parse('codex', codex.read_bytes())
        exporter = before['otel']['exporter']['otlp-http']
        exporter['endpoint'] = 'https://edited.invalid'
        exporter['headers']['Added'] = 'keep'
        before['copy'] = {'nested': ['secret '+self.token, 'keep']}
        before['otel']['trace_exporter'] = {}
        codex.write_text(tomli_w.dumps(before))
        codex.chmod(0o640)
        self.assertFalse(self.reconcile(agents, check=True))
        publish = native.publish

        def interrupt(path, *args, **kwargs):
            if path == codex:
                raise InterruptedError('fixture stopped before second home write')
            publish(path, *args, **kwargs)

        with patch.object(native, 'publish', side_effect=interrupt), self.assertRaises(InterruptedError):
            self.reconcile(agents)
        self.assertTrue(self.receipt.exists())
        self.assertFalse(any(path.exists() for path in self.paths.values()))

        self.reconcile(agents)
        self.assertTrue(self.reconcile(agents, check=True))
        self.assertFalse(self.receipt.exists())
        self.assertEqual(json.loads(claude.read_bytes()), {'env': {'OTEL_LOGS_EXPORTER': 'console', 'UNRELATED': 'keep'}})
        expected = {'analytics': {'enabled': False}, 'copy': {'nested': ['keep']},
                    'otel': {'log_user_prompt': True, 'trace_exporter': {}, 'exporter': {'otlp-http': {'endpoint': 'https://edited.invalid',
                              'protocol': 'json', 'headers': {'Authorization': 'original', 'Other': 'original', 'Added': 'keep'}}}}}
        self.assertTrue(native.semantic_equal(native.parse('codex', codex.read_bytes()), expected))
        self.assertEqual(stat.S_IMODE(codex.stat().st_mode), 0o640)
        for path in self.paths.values():
            self.assertTrue(self.token.encode() in path.read_bytes())
        for path in self.documents.values():
            self.assertFalse(self.token.encode() in path.read_bytes())


if __name__ == '__main__':
    unittest.main()
