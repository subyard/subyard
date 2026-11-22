#!/usr/bin/env python3
import json
import os
from pathlib import Path
import stat
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
sys.path.insert(0, str(Path(__file__).resolve().parents[4]/'internal/adapters/configmaterial'))
import native
from records import safefs
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
            self.assertEqual(otel[key]['otlp-http'], {'endpoint': self.endpoint+'/v1/'+signal, 'protocol': 'binary', 'tls': {}})
        self.assertEqual((otel['headers_helper'], otel['headers_helper_refresh_interval_ms']), (native.HELPER, 300000))
        document = native.parse('claude', claude.read_bytes())
        self.assertEqual(document['otelHeadersHelper'], native.HELPER)
        self.assertFalse([key for key in document['env'] if 'HEADERS' in key])
        # The bearer stays in the private token file; no policy names it.
        for path in (claude, codex):
            self.assertNotIn(self.token.encode(), path.read_bytes())
        self.assertFalse(otel['log_user_prompt'])
        self.assertIs(otel['log_agent_responses'], True)
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
            self.assertTrue(self.reconcile([], check=check))
            with self.assertRaisesRegex(native.Refusal, '/etc/claude-code/.*; unsafe.* /etc/codex/'):
                self.reconcile(list(self.paths), check=check)
            self.assertEqual(claude.read_bytes(), b'{"env":{}}')
            self.assertTrue(codex.is_symlink())

    def test_policy_observation_requires_traversal_as_developer(self):
        self.reconcile(['codex'])
        path = self.paths['codex']
        access = os.access

        def restricted(entry, mode, **kwargs):
            return entry != path.parent and access(entry, mode, **kwargs)

        with patch.object(native.os, 'access', side_effect=restricted), \
                self.assertRaisesRegex(native.Refusal, 'developer cannot read telemetry policy'):
            self.reconcile(['codex'], check=True)

    def test_policy_publish_has_final_permissions_and_private_staging(self):
        path = self.paths['codex']

        def fault(point):
            if point == safefs.BEFORE_PUBLISH:
                stage, = path.parent.glob('.telemetry-*')
                self.assertEqual(stat.S_IMODE(stage.stat().st_mode), 0o700)
                pending, = stage.iterdir()
                self.assertEqual(stat.S_IMODE(pending.stat().st_mode), native.POLICY_MODE)
                self.assertEqual(pending.stat().st_gid, self.gid)
                self.assertFalse(path.exists())
            if point == safefs.AFTER_PUBLISH:
                raise InterruptedError('after publish')

        with patch.object(safefs, 'FAULT', fault), self.assertRaises(InterruptedError):
            self.reconcile(['codex'])
        self.assertEqual(stat.S_IMODE(path.stat().st_mode), native.POLICY_MODE)
        self.assertTrue(self.reconcile(['codex'], check=True))
        # Releases that published before setting the final mode can still be repaired.
        path.chmod(0o600)
        self.assertFalse(self.reconcile(['codex'], check=True))
        self.reconcile(['codex'])
        self.assertTrue(self.reconcile(['codex'], check=True))

    def test_publish_compares_against_pinned_destination_with_separate_staging(self):
        for can_replace in (False, True):
            with self.subTest(can_replace=can_replace):
                parent = self.root/f'policy-{can_replace}'
                parent.mkdir()
                moved = parent.with_suffix('.moved')
                path = parent/'settings'
                original = b'expected' if can_replace else b'other'
                replacement = b'other' if can_replace else b'expected'
                path.write_bytes(original)

                def move_parent(point):
                    if point == safefs.AFTER_STAGE:
                        parent.rename(moved)
                        parent.mkdir()
                        path.write_bytes(replacement)

                with patch.object(safefs, 'FAULT', move_parent):
                    if can_replace:
                        native.publish(path, b'new', b'expected', policy_gid=self.gid)
                    else:
                        with self.assertRaisesRegex(ValueError, 'native configuration changed'):
                            native.publish(path, b'new', b'expected', policy_gid=self.gid)
                self.assertEqual((moved/path.name).read_bytes(), b'new' if can_replace else original)
                self.assertEqual(path.read_bytes(), replacement)

    @unittest.skipIf(os.geteuid() == 0, 'user-file symlink case runs as the developer')
    def test_legacy_cleanup_keeps_user_symlink(self):
        legacy = self.legacy()
        legacy(self.home, self.receipt, ['codex'], self.endpoint, self.token, tomli_w, self.uid, self.gid)
        codex = self.documents['codex']
        target = self.home/'managed-by-user.toml'
        codex.rename(target)
        codex.symlink_to(target)
        self.assertIn(self.token.encode(), target.read_bytes())
        self.reconcile(['codex'])
        self.assertTrue(codex.is_symlink())
        self.assertNotIn(self.token.encode(), target.read_bytes())

    def test_template_conflicts_compare_desired_fields_without_home_or_secret_output(self):
        agents = list(self.paths)
        templates = {'claude': b'{"env":{"OTEL_LOGS_EXPORTER":"otlp","EXTRA":"anything"}}',
                     'codex': b'[otel]\nlog_user_prompt = false\n'}
        self.reconcile(agents, templates=templates)
        self.assertTrue(self.reconcile(agents, check=True, templates=templates))
        # The helper's Authorization replaces a static template header.
        templates['codex'] += b'[otel.exporter.otlp-http.headers]\nAuthorization = "private-template-value"\n'
        self.reconcile(agents, templates=templates)
        self.assertTrue(self.reconcile(agents, check=True, templates=templates))
        templates = {'claude': b'{"env":{"OTEL_LOGS_EXPORTER":"private-template-value"},"otelHeadersHelper":"/private-template-value"}',
                     'codex': b'[otel]\nheaders_helper = "/private-template-value"\n'
                              b'[otel.exporter.otlp-http.tls]\nca-certificate = "/private.pem"\n'}
        with self.assertRaisesRegex(native.Refusal, r'claude template at /env/OTEL_LOGS_EXPORTER and 1 more field\(s\); '
                                    r'.*codex template at /otel/headers_helper and 1 more field\(s\)\Z') as error:
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
        user_header = 'Bearer '+'b'*64
        prior = {'analytics': {'enabled': False}, 'unrelated': user_header,
                 'otel': {'log_user_prompt': True, 'exporter': {'otlp-http': {'endpoint': 'https://original.invalid',
                           'protocol': 'json', 'headers': {'Authorization': user_header, 'Other': 'original'}}},
                          'metrics_exporter': {'otlp-http': {'endpoint': 'https://original.invalid',
                           'protocol': 'json', 'headers': {'Authorization': 'Basic dXNlcjpwYXNz'}}}}}
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
        codex.chmod(0o644)
        claude.chmod(0o400)
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
        expected = {'analytics': {'enabled': False}, 'copy': {'nested': ['keep']}, 'unrelated': user_header,
                    'otel': {'log_user_prompt': True, 'trace_exporter': {}, 'exporter': {'otlp-http': {'endpoint': 'https://edited.invalid',
                              'protocol': 'json', 'headers': {'Authorization': user_header, 'Other': 'original', 'Added': 'keep'}}},
                             'metrics_exporter': prior['otel']['metrics_exporter']}}
        self.assertTrue(native.semantic_equal(native.parse('codex', codex.read_bytes()), expected))
        self.assertEqual(stat.S_IMODE(codex.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(claude.stat().st_mode), 0o400)
        for path in [*self.paths.values(), *self.documents.values()]:
            self.assertFalse(self.token.encode() in path.read_bytes())

    def test_legacy_cleanup_refuses_token_left_in_comments_or_shadowed_members(self):
        claude, codex = (self.documents[agent] for agent in native.AGENTS)
        for path, payload in (
            (claude, b'{"env":{"OTEL_EXPORTER_OTLP_LOGS_HEADERS":"Authorization=Bearer '+self.token.encode()+b'"},"env":{}}\n'),
            (codex, b'# Authorization = "Bearer '+self.token.encode()+b'"\n[otel]\nlog_user_prompt = true\n'),
        ):
            native.publish(path, payload, None, mode=0o644)
        legacy = self.legacy_module()
        entries = {agent: {key: {'managed': value, 'present': False}
                           for key, value in legacy['desired'](agent, self.endpoint, self.token).items()}
                   for agent in native.AGENTS}
        receipt = json.dumps({'schema': 1, 'agents': entries}).encode()
        native.publish(self.receipt, receipt, None)
        for check in (True, False):
            with self.assertRaisesRegex(native.Refusal, 'collector token remains in legacy claude configuration; '
                                        'collector token remains in legacy codex configuration'):
                self.reconcile([], check=check)
        self.assertEqual(self.receipt.read_bytes(), receipt)
        self.assertIn(self.token.encode(), claude.read_bytes())
        self.assertIn(self.token.encode(), codex.read_bytes())

    def test_rollback_round_trip_cleans_the_token_the_v030_receipt_names(self):
        # Rolled back, v0.30 mints its own token while the private one stays. The
        # next upgrade removes what the receipt recorded and keeps the private token.
        legacy, minted = self.legacy(), 'b'*64
        claude, codex = (self.documents[agent] for agent in native.AGENTS)
        native.publish(codex, b'[analytics]\nenabled = false\n', None)
        copied = b'{"copied":"'+minted.encode()+b'"}'
        for agents in ([], ['claude', 'codex']):
            with self.subTest(agents=agents):
                legacy(self.home, self.receipt, ['claude', 'codex'], self.endpoint, minted, tomli_w, self.uid, self.gid)
                self.assertEqual(native.legacy_token(self.receipt), minted)
                self.assertFalse(self.reconcile(agents, check=True))
                self.reconcile(agents)
                self.assertTrue(self.reconcile(agents, check=True))
                self.assertFalse(self.receipt.exists())
                self.assertNotIn(minted.encode(), claude.read_bytes())
                self.assertEqual(codex.read_bytes(), b'[analytics]\nenabled = false\n')
                for path in self.paths.values():
                    self.assertEqual(path.exists(), bool(agents))
        # A file the receipt never covered is still scanned for the recorded token.
        legacy(self.home, self.receipt, ['codex'], self.endpoint, minted, tomli_w, self.uid, self.gid)
        native.publish(claude, copied, native.snapshot(claude))
        for check in (True, False):
            with self.assertRaisesRegex(native.Refusal, r'\Acollector token remains in legacy claude configuration\Z'):
                self.reconcile(['claude', 'codex'], check=check)
        self.assertTrue(self.receipt.exists())
        self.assertEqual(claude.read_bytes(), copied)
        # The private token is scanned too, in covered and uncovered files alike.
        private = b'{"copied":"'+self.token.encode()+b'"}'
        for covered in (['claude', 'codex'], ['codex']):
            with self.subTest(covered=covered):
                native.publish(claude, b'{}', native.snapshot(claude))
                legacy(self.home, self.receipt, covered, self.endpoint, minted, tomli_w, self.uid, self.gid)
                document = json.loads(claude.read_bytes())
                document['copied'] = self.token
                native.publish(claude, json.dumps(document).encode() if 'claude' in covered else private,
                               native.snapshot(claude))
                before = claude.read_bytes()
                for check in (True, False):
                    with self.assertRaisesRegex(native.Refusal, r'\Acollector token remains in legacy claude configuration\Z'):
                        self.reconcile(['claude', 'codex'], check=check)
                self.assertTrue(self.receipt.exists())
                self.assertEqual(claude.read_bytes(), before)

    def test_legacy_cleanup_refuses_headers_that_name_no_single_token(self):
        path = self.documents['claude']
        key = 'OTEL_EXPORTER_OTLP_LOGS_HEADERS'
        different_header = 'Authorization=Bearer '+'b'*64
        payload = json.dumps({'env': {key: different_header}}).encode()
        native.publish(path, payload, None)
        receipt = json.dumps({'schema': 1, 'agents': {'claude': {
            key: {'managed': different_header, 'present': False},
            'OTEL_EXPORTER_OTLP_METRICS_HEADERS': {'managed': 'Authorization=Bearer '+self.token, 'present': False},
        }}}).encode()
        native.publish(self.receipt, receipt, None)
        self.assertIsNone(native.legacy_token(self.receipt))
        for token in (self.token, None):
            with self.subTest(token=bool(token)), self.assertRaisesRegex(
                    native.Refusal, r'\Athe legacy telemetry receipt names no single collector token; inside the yard, remove the '
                                    r"collector Bearer tokens from the developer's ~/\.claude/settings\.json and ~/\.codex/config\.toml, "
                                    r'then remove the receipt /var/lib/subyard/telemetry-control/native\.json and retry\Z'):
                native.reconcile(self.etc, self.home, self.receipt, [], self.endpoint, token, tomli_w,
                                 self.uid, self.gid, {})
            self.assertEqual(path.read_bytes(), payload)
            self.assertEqual(self.receipt.read_bytes(), receipt)

    def legacy_module(self):
        fixture = Path(__file__).with_name('fixtures')/'v0.30.0-native.py.tar.gz'
        with tarfile.open(fixture) as archive:
            source = archive.extractfile('config/profiles/telemetry/native.py').read()
        legacy = {}
        exec(compile(source, 'v0.30.0/native.py', 'exec'), legacy)
        return legacy

    def legacy(self):
        return self.legacy_module()['reconcile']

    def test_legacy_cleanup_recovers_a_missing_collector_token_from_the_receipt(self):
        legacy = self.legacy()
        for agents in ([], ['codex']):
            with self.subTest(agents=agents):
                codex = self.documents['codex']
                if self.receipt.exists():
                    self.receipt.unlink()
                if codex.exists():
                    codex.unlink()
                legacy(self.home, self.receipt, ['codex'], self.endpoint, self.token, tomli_w, self.uid, self.gid)
                self.assertIn(self.token.encode(), codex.read_bytes())
                self.assertEqual(native.legacy_token(self.receipt), self.token)
                missing = (self.etc, self.home, self.receipt, agents, self.endpoint, None, tomli_w,
                           self.uid, self.gid, {})
                self.assertFalse(native.reconcile(*missing, True))
                if agents:
                    # Apply restores the token file before it writes the policy.
                    with self.assertRaisesRegex(ValueError, 'collector token required'):
                        native.reconcile(*missing)
                    self.assertTrue(self.receipt.exists())
                    self.reconcile(agents)
                else:
                    native.reconcile(*missing)
                self.assertFalse(self.receipt.exists())
                self.assertNotIn(self.token.encode(), codex.read_bytes())

    def test_legacy_cleanup_only_scans_files_the_receipt_never_covered(self):
        legacy = self.legacy()
        claude = self.documents['claude']
        claude.parent.mkdir(parents=True)
        dotfiles = self.root/'dotfiles/settings.json'
        dotfiles.parent.mkdir()
        cases = [('symlinked', b'{"env":{}}', True), ('invalid', b'{"env": {,}', False),
                 ('symlinked token', b'{"x":"'+self.token.encode()+b'"}', True),
                 ('invalid token', b'{"env": {,} '+self.token.encode(), False)]
        for disabled in (False, True):
            for name, payload, symlinked in cases:
                with self.subTest(disabled=disabled, case=name):
                    for path in (self.receipt, claude, self.documents['codex']):
                        if path.exists() or path.is_symlink():
                            path.unlink()
                    legacy(self.home, self.receipt, ['codex'], self.endpoint, self.token, tomli_w, self.uid, self.gid)
                    if disabled:
                        legacy(self.home, self.receipt, [], self.endpoint, self.token, tomli_w, self.uid, self.gid)
                        self.assertEqual(json.loads(self.receipt.read_bytes()), {'schema': 1, 'agents': {}})
                    dotfiles.write_bytes(payload)
                    if symlinked:
                        claude.symlink_to(dotfiles)
                    else:
                        claude.write_bytes(payload)
                    agents = [] if disabled else ['codex']
                    if self.token.encode() in payload:
                        for check in (True, False):
                            with self.assertRaisesRegex(native.Refusal, r'\Acollector token remains in legacy claude configuration\Z'):
                                self.reconcile(agents, check=check)
                        self.assertTrue(self.receipt.exists())
                    else:
                        self.assertFalse(self.reconcile(agents, check=True))
                        self.reconcile(agents)
                        self.assertTrue(self.reconcile(agents, check=True))
                        self.assertFalse(self.receipt.exists())
                    self.assertEqual(claude.is_symlink(), symlinked)
                    self.assertEqual(claude.read_bytes(), payload)


if __name__ == '__main__':
    unittest.main()
