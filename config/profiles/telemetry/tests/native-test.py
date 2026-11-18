#!/usr/bin/env python3
import json
import hashlib
import os
from pathlib import Path
import sys
import subprocess
import tarfile
import tempfile
import tomllib
import types
import unittest
from decimal import Decimal
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

    def materialize(self, agent, path, payload, mode='apply', program=None):
        file_format = 'json' if agent == 'claude' else 'toml'
        if program is None:
            program = Path(native.materializer.__file__).read_text()
            contributions = (Path(__file__).resolve().parents[1]/'config-contributions.json').read_text()
            contributions = contributions.replace('/var/lib/subyard/telemetry-control/native.json',
                                                  str(path.parents[1]/'native.json'))
            program = 'LEGACY_CONTRIBUTIONS = '+contributions+'\n'+program
        program = program.replace('@STATE_ROOT@', str(self.state_root)).replace('@STATE_UID@', str(os.getuid())).replace('@FORMAT@', file_format)
        if file_format == 'toml':
            program = '_toml_writer = {}\nexec('+repr(Path(tomli_w.__file__).read_text())+', _toml_writer)\n'+program
        digest = hashlib.sha256(payload).hexdigest()
        return subprocess.run([sys.executable, '-B', '-c', program, mode, path.parents[1].name,
                               str(path), str(os.getuid()), digest, str(path.parents[1]), digest],
                              input=payload, capture_output=True)

    def test_repair_mode_when_exporters_already_match(self):
        for agent, payload in [('claude', b'{"unrelated":true}'), ('codex', b'unrelated = true\n')]:
            with self.subTest(agent=agent), tempfile.TemporaryDirectory(dir='/tmp') as name:
                root = Path(name)
                args = (root, root/'native.json', [agent], 'http://localhost:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
                templates = {agent: payload}
                path = native.agent_path(root, agent)
                applied = self.materialize(agent, path, payload)
                self.assertEqual(applied.returncode, 0, applied.stderr)
                native.reconcile(*args, templates=templates)
                before = path.read_bytes()
                path.chmod(0o644)
                self.assertFalse(native.reconcile(*args, check=True, templates=templates))
                self.assertEqual(path.stat().st_mode & 0o777, 0o644)
                for _ in range(2):
                    native.reconcile(*args, templates=templates)
                    self.assertTrue(native.reconcile(*args, check=True, templates=templates))
                    self.assertEqual(path.stat().st_mode & 0o777, 0o600)
                    self.assertEqual(path.read_bytes(), before)

    def test_mode_repair_rechecks_current_inode_after_earlier_secure_observation(self):
        path = native.agent_path(self.root, 'claude')
        path.parent.mkdir(mode=0o700)
        native.publish(path, b'{}', None)
        stat_file = os.fstat

        def widen_after_fstat(descriptor):
            observed = stat_file(descriptor)
            if mock.call_count == 1:
                os.fchmod(descriptor, 0o644)
            return observed

        with patch.object(os, 'fstat', side_effect=widen_after_fstat) as mock:
            native.materializer.repair_document_mode(str(path), str(self.root), os.getuid())
        self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        self.assertEqual(path.read_bytes(), b'{}')

    def test_template_and_exporters_converge_in_both_orders_and_repair_damage(self):
        for agent, payload in [('claude', b'{"unrelated":true}'), ('codex', b'unrelated = true\n')]:
            for profile_first in (False, True):
                with self.subTest(agent=agent, profile_first=profile_first), tempfile.TemporaryDirectory(dir='/tmp') as name:
                    root = Path(name)
                    state = root/'native.json'
                    args = (root, state, [agent], 'http://localhost:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
                    templates = {agent: payload}
                    path = native.agent_path(root, agent)
                    if profile_first:
                        native.reconcile(*args, templates=templates)
                        adopted = self.materialize(agent, path, payload, 'assess-adopt')
                        self.assertEqual(adopted.returncode, 0, adopted.stderr)
                        self.assertTrue(json.loads(adopted.stdout)['adoptable'])
                    applied = self.materialize(agent, path, payload)
                    self.assertEqual(applied.returncode, 0, applied.stderr)
                    native.reconcile(*args, templates=templates)
                    damaged_with_token = path.read_bytes() + b'\n[broken'
                    for damaged in (None, b'', b'[broken', damaged_with_token):
                        if damaged is None:
                            path.unlink()
                        else:
                            path.write_bytes(damaged)
                        exposed = damaged == damaged_with_token
                        if exposed:
                            path.chmod(0o644)
                        self.assertFalse(native.reconcile(*args, check=True, templates=templates))
                        observed = self.materialize(agent, path, payload, 'observe')
                        self.assertEqual(observed.returncode, 0, observed.stderr)
                        self.assertFalse(json.loads(observed.stdout)['converged'])
                        if exposed:
                            self.assertEqual(path.stat().st_mode & 0o777, 0o644)
                        native.reconcile(*args, templates=templates)
                        self.assertTrue(native.reconcile(*args, check=True, templates=templates))
                        self.assertEqual(path.stat().st_mode & 0o777, 0o600)
                        values = json.loads(path.read_bytes()) if agent == 'claude' else tomllib.loads(path.read_text())
                        self.assertIs(values['unrelated'], True)
                        self.assertEqual(values['env' if agent == 'claude' else 'otel'],
                                         native.desired(agent, args[3], args[4]))

    def test_template_conflict_repairs_mode_without_changing_document(self):
        for agent, field, payload in [
            ('claude', '/env/OTEL_LOG_USER_PROMPTS', b'{"env":{"OTEL_LOG_USER_PROMPTS":"template-value"}}'),
            ('codex', '/otel/log_user_prompt', b'[otel]\nlog_user_prompt = "template-value"\n'),
        ]:
            with self.subTest(agent=agent), tempfile.TemporaryDirectory(dir='/tmp') as name:
                root = Path(name)
                args = (root, root/'native.json', [agent], 'http://localhost:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
                path = native.agent_path(root, agent)
                with self.assertRaisesRegex(native.materializer.ContributionConflict, field) as error:
                    native.reconcile(*args, templates={agent: payload})
                self.assertNotIn('template-value', str(error.exception))
                self.assertFalse(path.exists())
                native.reconcile(*args)
                before = path.read_bytes()
                path.chmod(0o644)
                refused = self.materialize(agent, path, b'{}' if agent == 'claude' else b'', 'retire')
                self.assertEqual(refused.returncode, 2)
                self.assertIn(b'retirement ownership evidence unavailable', refused.stderr)
                self.assertEqual(path.stat().st_mode & 0o777, 0o644)
                self.assertEqual(path.read_bytes(), before)
                for mode in ('observe', 'apply', 'apply-installed'):
                    if mode == 'apply-installed':
                        self.assertEqual(self.materialize(agent, path, b'{}' if agent == 'claude' else b'').returncode, 0)
                        before = path.read_bytes()
                    path.chmod(0o644)
                    refused = self.materialize(agent, path, payload, mode)
                    self.assertEqual(refused.returncode, 2)
                    self.assertIn(field.encode(), refused.stderr)
                    self.assertNotIn(b'template-value', refused.stderr)
                    self.assertEqual(path.read_bytes(), before)
                    self.assertEqual(path.stat().st_mode & 0o777, 0o644 if mode == 'observe' else 0o600)
                path.chmod(0o644)
                with self.assertRaisesRegex(native.materializer.ContributionConflict, field):
                    native.reconcile(*args, templates={agent: payload})
                self.assertEqual(path.stat().st_mode & 0o777, 0o600)
                self.assertEqual(path.read_bytes(), before)

    def test_migrate_legacy_ownership_and_retry_interrupted_registration(self):
        archive = Path(__file__).with_name('fixtures')/'v0.30.0-agent-writers.tar.gz'
        with tarfile.open(archive) as files:
            retained_template = files.extractfile('internal/adapters/configmaterial/guest_json.py').read().decode()
            retained_native = types.ModuleType('retained_native')
            exec(files.extractfile('config/profiles/telemetry/native.py').read(), retained_native.__dict__)

        for agent in ('claude', 'codex'):
            with self.subTest(agent=agent), tempfile.TemporaryDirectory(dir='/tmp') as name:
                root = Path(name)
                state = root/'native.json'
                endpoint, token = 'http://localhost:14318', 'x'*64
                args = (root, state, [agent], endpoint, token, tomli_w, os.getuid(), os.getgid())
                path = native.agent_path(root, agent)
                section, field, prior = ('env', 'OTEL_LOGS_EXPORTER', 'console') if agent == 'claude' else ('otel', 'log_user_prompt', True)
                encode = json.dumps if agent == 'claude' else tomli_w.dumps
                decode = json.loads if agent == 'claude' else tomllib.loads
                templates = [encode({'unrelated': value}).encode() for value in range(4)]

                def template(payload, *, retained=False, mode='apply'):
                    result = self.materialize(agent, path, payload, mode, retained_template if retained else None)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    return json.loads(result.stdout) if mode == 'observe' else None

                def assert_mode(expected):
                    self.assertEqual(path.stat().st_mode & 0o777, expected)

                template(templates[0], retained=True)
                path.write_text(encode({'unrelated': 0, section: {field: prior}}))
                retained_native.reconcile(*args)
                assert_mode(0o600)
                owned = json.loads(state.read_bytes())['agents'][agent]
                template(templates[1], retained=True)
                assert_mode(0o644)
                self.assertTrue(retained_native.reconcile(*args, check=True))
                self.assertFalse(template(templates[1], mode='observe')['converged'])
                reference = Path(native.materializer.asset_key(root.name, str(path)) + '.contribution')
                self.assertFalse(reference.exists())
                conflict = encode({section: {field: prior}}).encode()
                before = path.read_bytes()
                for mode in ('observe', 'apply'):
                    result = self.materialize(agent, path, conflict, mode)
                    self.assertEqual(result.returncode, 2, result.stderr)
                    self.assertIn(('configuration contribution conflicts at /'+section+'/'+field).encode(), result.stderr)
                    self.assertEqual(path.read_bytes(), before)
                    self.assertFalse(reference.exists())
                template(templates[2])
                assert_mode(0o600)
                self.assertEqual(decode(path.read_text())[section], native.desired(agent, endpoint, token))
                with patch.object(native, 'publish', side_effect=OSError('synthetic interruption')):
                    # A retained write can also race the handoff before registration.
                    template(templates[2], retained=True)
                    with self.assertRaises(OSError):
                        native.reconcile(*args)
                assert_mode(0o600)
                native.reconcile(*args)
                self.assertTrue(native.reconcile(*args, check=True))
                assert_mode(0o600)
                self.assertEqual(json.loads(state.read_bytes())['agents'][agent], owned)
                # Accepted rollback risk: the retained writer may widen the
                # credential-bearing file while retained telemetry reports current.
                template(templates[3], retained=True)
                self.assertTrue(retained_native.reconcile(*args, check=True))
                self.assertFalse(native.reconcile(*args, check=True))
                self.assertFalse(template(templates[3], mode='observe')['converged'])
                native.reconcile(*args)
                assert_mode(0o600)
                self.assertTrue(template(templates[3], mode='observe')['converged'])

                template(templates[3], retained=True)
                retained_native.reconcile(*args[:2], [], *args[3:])
                self.assertEqual(decode(path.read_text()), {'unrelated': 3, section: {field: prior}})
                self.assertEqual(json.loads(state.read_bytes())['agents'], {})
                template(templates[3], retained=True)
                self.assertFalse(template(templates[3], mode='observe')['converged'])
                template(templates[3])
                assert_mode(0o600)
                self.assertEqual(decode(path.read_text()), {'unrelated': 3, section: {field: prior}})

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

    def test_interrupted_enable_does_not_authorize_reconstruction(self):
        for agent, section, encode in (('claude', 'env', json.dumps), ('codex', 'otel', tomli_w.dumps)):
            with self.subTest(agent=agent), tempfile.TemporaryDirectory(dir='/tmp') as name:
                root = Path(name)
                state = root/'native.json'
                path = native.agent_path(root, agent)
                original = encode({'unrelated': True}).encode()
                path.parent.mkdir()
                path.write_bytes(original)
                path.chmod(0o400)
                args = (root, state, [agent], 'http://localhost:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
                with patch.object(native.materializer, 'exchange_destination', side_effect=OSError('interrupted')):
                    with self.assertRaisesRegex(ValueError, 'publication failed'):
                        native.reconcile(*args)
                self.assertFalse(state.exists())
                self.assertTrue(state.with_suffix('.intent').exists())
                self.assertEqual(path.read_bytes(), original)
                self.assertEqual(path.stat().st_mode & 0o777, 0o400)

                path.chmod(0o600)
                path.write_bytes(original + b'[')
                path.chmod(0o400)
                with self.assertRaises(native.AgentConfigRefusal):
                    native.reconcile(*args)
                # The former writer stored this intent in the receipt slot.
                records = {key: {'managed': value, 'present': False, 'value': None, 'previous': []}
                           for key, value in native.desired(agent, *args[3:5]).items()}
                native.publish(state, json.dumps({'schema': 1, 'agents': {agent: records}}).encode(), None)
                with self.assertRaises(native.AgentConfigRefusal):
                    native.reconcile(*args)
                refused = self.materialize(agent, path, encode({section: {}}).encode())
                self.assertEqual(refused.returncode, 2, refused.stderr)
                self.assertIn(b'unowned-invalid-destination', refused.stderr)
                self.assertEqual(path.read_bytes(), original + b'[')
                self.assertEqual(path.stat().st_mode & 0o777, 0o400)

    def test_retry_and_opt_out_recover_publication_before_receipt(self):
        for agent, section, field, prior, encode, decode in (
                ('claude', 'env', 'OTEL_LOGS_EXPORTER', 'console', json.dumps, json.loads),
                ('codex', 'otel', 'log_user_prompt', True, tomli_w.dumps, tomllib.loads)):
            for refresh in (False, True):
                for retry in (False, True):
                    with self.subTest(agent=agent, refresh=refresh, retry=retry), tempfile.TemporaryDirectory(dir='/tmp') as name:
                        root = Path(name)
                        path, state = native.agent_path(root, agent), root/'native.json'
                        journal = state.with_suffix('.intent')
                        path.parent.mkdir()
                        initial = {section: {field: prior}, 'unrelated': 'keep'}
                        path.write_text(encode(initial))
                        args = (root, state, [agent], 'http://localhost:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
                        if refresh:
                            native.reconcile(*args)
                            args = (*args[:4], 'y'*64, *args[5:])
                        baseline = native.snapshot(state)
                        publish = native.publish

                        def interrupt(destination, payload, expected):
                            if destination == state:
                                self.assertEqual(journal.stat().st_mode & 0o777, 0o600)
                                self.assertEqual(decode(path.read_text())[section], native.desired(agent, *args[3:5]))
                                raise OSError('interrupted after publication')
                            publish(destination, payload, expected)

                        with patch.object(native, 'publish', side_effect=interrupt):
                            with self.assertRaisesRegex(OSError, 'interrupted after publication'):
                                native.reconcile(*args)
                        self.assertEqual(native.snapshot(state), baseline)
                        self.assertFalse(native.reconcile(*args, check=True))
                        # An editor can save unrelated fields by replacing the inode.
                        current = decode(path.read_text())
                        current['unrelated'] = initial['unrelated'] = 'editor'
                        editor = root/'editor'
                        editor.write_text(encode(current))
                        editor.replace(path)
                        if retry:
                            native.reconcile(*args)
                            self.assertTrue(native.reconcile(*args, check=True))
                            published = native.snapshot(state)

                            def interrupt_opt_out(destination, payload, expected):
                                if destination == state:
                                    self.assertTrue(journal.exists())
                                    self.assertEqual(decode(path.read_text()), initial)
                                    raise OSError('interrupted opt-out')
                                publish(destination, payload, expected)

                            with patch.object(native, 'publish', side_effect=interrupt_opt_out):
                                with self.assertRaisesRegex(OSError, 'interrupted opt-out'):
                                    native.reconcile(*args[:2], [], *args[3:])
                            self.assertEqual(native.snapshot(state), published)
                        native.reconcile(*args[:2], [], *args[3:])
                        self.assertEqual(decode(path.read_text()), initial)
                        self.assertEqual(json.loads(state.read_bytes())['agents'], {})
                        self.assertFalse(journal.exists())

    def test_permission_repair_requires_selection_and_receipt(self):
        root, state = self.root, self.root/'native.json'
        claude, codex = (native.agent_path(root, agent) for agent in ('claude', 'codex'))
        native.reconcile(root, state, ['claude'], 'http://localhost:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
        claude.chmod(0o644)
        codex.parent.mkdir()
        codex.write_text('')
        codex.chmod(0o400)
        native.repair_modes(root, state, ['codex'], os.getuid())
        self.assertEqual(claude.stat().st_mode & 0o777, 0o644)
        self.assertEqual(codex.stat().st_mode & 0o777, 0o400)
        native.repair_modes(root, state, ['claude'], os.getuid())
        self.assertEqual(claude.stat().st_mode & 0o777, 0o600)

    def test_template_value_is_removed_after_both_owners_release(self):
        for agent, section, field, value, encode, decode in (
                ('claude', 'env', 'OTEL_LOGS_EXPORTER', 'otlp', json.dumps, json.loads),
                ('codex', 'otel', 'log_user_prompt', False, tomli_w.dumps, tomllib.loads)):
            for retire, remove_source in ((False, False), (True, False), (True, True)):
                with self.subTest(agent=agent, retire=retire, remove_source=remove_source), tempfile.TemporaryDirectory(dir='/tmp') as name:
                    root = Path(name)
                    path, state = native.agent_path(root, agent), root/'native.json'
                    template = encode({section: {field: value}}).encode()
                    result = self.materialize(agent, path, template)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    args = (root, state, [agent], 'http://localhost:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
                    native.reconcile(*args, templates={} if remove_source else {agent: template})
                    result = self.materialize(agent, path, encode({}).encode(), 'retire' if retire else 'apply')
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(decode(path.read_text())[section][field], value)
                    native.reconcile(*args[:2], [], *args[3:])
                    self.assertEqual(decode(path.read_text()), {})

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
            with patch.object(tomli_w, 'dumps', side_effect=AssertionError('unchanged TOML must not be serialized')):
                self.assertTrue(native.reconcile(root, state, agents, 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid(), check=True))
                self.assertEqual(state.read_bytes(), ownership, 'check must not change ownership')
                native.reconcile(root, state, agents, 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
            self.assertEqual({path: path.read_bytes() for path in before}, before)

    def test_prior_typed_native_values_require_reconciliation(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            (root/'.codex').mkdir(mode=0o700)
            path = root/'.codex/config.toml'
            path.write_text('[otel]\nlog_user_prompt = 0\n')
            path.chmod(0o600)
            state = root/'native.json'
            native.reconcile(root, state, ['codex'], 'http://127.0.0.1:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
            value = tomllib.loads(path.read_text())
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

    def test_deleted_exporter_value_stays_deleted_through_refresh_and_disable(self):
        for agent, section, field, prior, encode, decode in (
            ('claude', 'env', 'OTEL_LOGS_EXPORTER', 'console', json.dumps, json.loads),
            ('codex', 'otel', 'log_user_prompt', True, tomli_w.dumps, tomllib.loads),
        ):
            with self.subTest(agent=agent), tempfile.TemporaryDirectory(dir='/tmp') as name:
                root = Path(name)
                path, state = native.agent_path(root, agent), root/'native.json'
                path.parent.mkdir(mode=0o700)
                path.write_text(encode({'unrelated': True, section: {field: prior}}))
                args = (root, state, [agent], 'http://localhost:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
                native.reconcile(*args)
                edited = decode(path.read_text())
                del edited[section][field]
                path.write_text(encode(edited))
                path.chmod(0o644)
                before, receipt = path.read_bytes(), state.read_bytes()
                for check in (True, False):
                    with self.assertRaisesRegex(ValueError, 'ownership conflict'):
                        native.reconcile(*args, check=check)
                    self.assertEqual(path.read_bytes(), before)
                    self.assertEqual(state.read_bytes(), receipt)
                    self.assertEqual(path.stat().st_mode & 0o777, 0o644 if check else 0o600)
                native.reconcile(*args[:2], [], *args[3:])
                self.assertEqual(decode(path.read_text()), {'unrelated': True})

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

    def test_opt_out_preserves_scalar_replacement_for_owned_section(self):
        for agent, replacement in [('claude', b'{"env":"user edit"}'), ('codex', b'otel = "user edit"\n')]:
            with self.subTest(agent=agent):
                state = self.root/(agent+'.receipt')
                path = native.agent_path(self.root, agent)
                args = (self.root, state, [agent], 'http://localhost:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
                template = b'{"unrelated":true}' if agent == 'claude' else b'unrelated = true\n'
                result = self.materialize(agent, path, template)
                self.assertEqual(result.returncode, 0, result.stderr)
                templates = {agent: template}
                applied = path.read_bytes()
                for first_enable in (True, False):
                    if not first_enable:
                        path.write_bytes(applied)
                        native.reconcile(*args, templates=templates)
                    path.write_bytes(replacement)
                    before = native.snapshot(state)
                    for check in (True, False):
                        with self.subTest(first_enable=first_enable, check=check), self.assertRaisesRegex(
                                native.materializer.ContributionConflict, '/env' if agent == 'claude' else '/otel'):
                            native.reconcile(*args, check=check, templates=templates)
                        self.assertEqual(path.read_bytes(), replacement)
                        self.assertEqual(native.snapshot(state), before)
                path.write_bytes(replacement)
                native.reconcile(*args[:2], [], *args[3:])
                self.assertEqual(path.read_bytes(), replacement)
                self.assertEqual(json.loads(state.read_bytes())['agents'], {})

    def test_opt_out_removes_unchanged_header_beside_edited_endpoint(self):
        state = self.root/'native.json'
        path = native.agent_path(self.root, 'codex')
        args = (self.root, state, ['codex'], 'http://localhost:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
        prior = {'Authorization': 'previous', 'X-Tenant': 'previous-tenant', 'User-Header': 'old-value'}
        path.parent.mkdir(mode=0o700)
        native.publish(path, tomli_w.dumps({'otel': {'exporter': {'otlp-http': {'headers': prior}}}}).encode(), None)
        native.reconcile(*args)
        values = tomllib.loads(path.read_text())
        exporter = values['otel']['exporter']['otlp-http']
        exporter['endpoint'] = 'http://localhost:23456/v1/logs'
        exporter['headers']['User-Header'] = 'keep'
        path.write_text(tomli_w.dumps(values))
        path.chmod(0o644)
        native.reconcile(*args[:2], [], *args[3:])
        self.assertEqual(tomllib.loads(path.read_text()), {'otel': {'exporter': {'otlp-http': {
            'endpoint': 'http://localhost:23456/v1/logs', 'headers': {**prior, 'User-Header': 'keep'}}}}})
        self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        self.assertEqual(json.loads(state.read_bytes())['agents'], {})

    def test_opt_out_preserves_template_fields_without_its_source(self):
        for agent, template in [
            ('claude', b'{"env":{"OTEL_LOG_USER_PROMPTS":"0"}}'),
            ('codex', b'[otel]\nlog_user_prompt = false\n[otel.exporter.otlp-http]\nprotocol = "binary"\n'),
        ]:
            with self.subTest(agent=agent), tempfile.TemporaryDirectory(dir='/tmp') as name:
                root = Path(name)
                path, state = native.agent_path(root, agent), root/'native.json'
                args = (root, state, [agent], 'http://localhost:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
                native.reconcile(*args)
                result = self.materialize(agent, path, template)
                self.assertEqual(result.returncode, 0, result.stderr)
                native.reconcile(*args[:2], [], *args[3:])
                decode = json.loads if agent == 'claude' else tomllib.loads
                self.assertEqual(decode(path.read_text()), decode(template.decode()))
                result = self.materialize(agent, path, template, 'observe')
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertTrue(json.loads(result.stdout)['converged'])

    def test_concurrent_edit_during_publication_is_preserved(self):
        for agent, encode, decode in [('claude', json.dumps, json.loads), ('codex', tomli_w.dumps, tomllib.loads)]:
            with self.subTest(agent=agent), tempfile.TemporaryDirectory(dir='/tmp') as name:
                root = Path(name)
                path, state = native.agent_path(root, agent), root/'native.json'
                args = (root, state, [agent], 'http://localhost:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
                native.reconcile(*args)
                args = (*args[:4], 'y'*64, *args[5:])
                edited = decode(path.read_text())
                edited['unrelated'] = 'editor'
                edit = encode(edited).encode()
                editor = root/'editor-save'
                fsync = os.fsync

                def editor_after_fsync(fd):
                    fsync(fd)
                    staged = Path(os.readlink(f'/proc/self/fd/{fd}'))
                    if staged.parent == path.parent and staged.name.startswith('.subyard-config.'):
                        editor.write_bytes(edit)
                        editor.chmod(0o644)
                        editor.replace(path)

                with patch.object(os, 'fsync', side_effect=editor_after_fsync):
                    with self.assertRaisesRegex(ValueError, 'concurrent destination change'):
                        native.reconcile(*args)
                self.assertEqual(path.read_bytes(), edit)
                self.assertEqual(path.stat().st_mode & 0o777, 0o600)
                exchange = native.materializer.exchange_destination

                # The second exchange restores the editor's save.
                def save_at_exchange(*arguments):
                    if swap.call_count == 1:
                        path.write_bytes(edit + b'\n')
                        path.chmod(0o644)
                    if swap.call_count == 2:
                        path.write_bytes(edit + b'\n\n')
                        path.chmod(0o644)
                    exchange(*arguments)

                with patch.object(native.materializer, 'exchange_destination', side_effect=save_at_exchange) as swap:
                    with self.assertRaises(native.AgentConfigRefusals) as refused:
                        native.reconcile(*args)
                saved_agent, failure = refused.exception.failures[0]
                self.assertEqual(saved_agent, agent)
                self.assertIn('concurrent destination change', str(failure))
                preserved = path.parent/failure.recovery
                self.assertEqual(path.read_bytes(), edit + b'\n')
                self.assertEqual(preserved.read_bytes(), edit + b'\n\n')
                for document in (path, preserved):
                    self.assertEqual(document.stat().st_mode & 0o777, 0o600)

                read = native.materializer.read_destination
                with path.open('r+b') as open_editor:
                    inode = path.stat().st_ino

                    def save_after_comparison(name, *arguments, **keywords):
                        contents = read(name, *arguments, **keywords)
                        if name == 'document':
                            open_editor.seek(0)
                            open_editor.write(edit + b'\n\n\n')
                            open_editor.flush()
                        return contents

                    with patch.object(native.materializer, 'read_destination', side_effect=save_after_comparison):
                        native.reconcile(*args)
                    old_inode = next(saved for saved in path.parent.glob('.subyard-config-recovery.*/document') if saved.stat().st_ino == inode)
                    self.assertEqual(old_inode.read_bytes(), edit + b'\n\n\n')
                    self.assertEqual(old_inode.stat().st_mode & 0o777, 0o600)
                native.reconcile(*args)
                self.assertEqual(decode(path.read_text())['unrelated'], 'editor')

    def test_interruption_at_exchange_keeps_recovery_file_private(self):
        for agent in ('claude', 'codex'):
            with self.subTest(agent=agent), tempfile.TemporaryDirectory(dir='/tmp') as name:
                root = Path(name)
                path = native.agent_path(root, agent)
                args = (root, root/'native.json', [agent], 'http://localhost:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
                native.reconcile(*args)
                exchange = native.materializer.exchange_destination
                original = path.read_bytes()

                def interrupt(*arguments):
                    path.chmod(0o644)
                    exchange(*arguments)
                    os._exit(57)

                child = os.fork()
                if child == 0:
                    try:
                        with patch.object(native.materializer, 'exchange_destination', side_effect=interrupt):
                            native.reconcile(*args[:4], 'y'*64, *args[5:])
                    finally:
                        os._exit(1)
                self.assertEqual(os.waitstatus_to_exitcode(os.waitpid(child, 0)[1]), 57)
                saved, = path.parent.glob('.subyard-config-recovery.*/document')
                self.assertEqual(saved.read_bytes(), original)
                self.assertEqual(saved.stat().st_mode & 0o777, 0o600)
                self.assertEqual(saved.parent.stat().st_mode & 0o777, 0o700)
                self.assertEqual(path.stat().st_mode & 0o777, 0o600)
                receipt = json.loads(args[1].read_bytes())
                managed = {key: record['managed'] for key, record in receipt['agents'][agent].items()}
                self.assertEqual(managed, native.desired(agent, *args[3:5]))
                native.reconcile(*args[:4], 'y'*64, *args[5:])
                self.assertFalse(args[1].with_suffix('.intent').exists())

    def test_claude_strict_parse_and_precise_round_trip(self):
        state = self.root/'native.json'
        path = native.agent_path(self.root, 'claude')
        path.parent.mkdir()
        args = (self.root, state, ['claude'], 'http://localhost:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
        duplicate = b'{"unrelated":{"key":1,"key":2}}'
        path.write_bytes(duplicate)
        for check in (True, False):
            with self.assertRaises(native.AgentConfigRefusal):
                native.reconcile(*args, check=check)
            self.assertEqual(path.read_bytes(), duplicate)
            self.assertFalse(state.exists())

        number = Decimal('1.00000000000000000001')
        path.write_bytes(b'{"unrelated":1.00000000000000000001}')
        for agents in (['claude'], []):
            native.reconcile(*args[:2], agents, *args[3:])
            values = json.loads(path.read_bytes(), parse_float=Decimal)
            self.assertEqual(values['unrelated'], number)

    def test_opt_out_restores_empty_sections_and_float_types(self):
        state = self.root/'native.json'
        for agent, original, section, expected in [
            ('claude', b'{"env":{}}', 'env', {}),
            ('codex', b'[otel]\n', 'otel', {}),
            ('codex', b'[otel]\nlog_user_prompt = 2e0\n', 'otel', {'log_user_prompt': 2.0}),
        ]:
            with self.subTest(agent=agent, original=original):
                path = native.agent_path(self.root, agent)
                path.parent.mkdir(exist_ok=True)
                path.write_bytes(original)
                args = ('http://localhost:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
                native.reconcile(self.root, state, [agent], *args)
                native.reconcile(self.root, state, [], *args)
                decode = json.loads if agent == 'claude' else tomllib.loads
                restored = decode(path.read_text())[section]
                self.assertEqual(restored, expected)
                if expected:
                    self.assertIs(type(restored['log_user_prompt']), float)
                if agent == 'claude':
                    adopted = self.materialize(agent, path, b'{"other":true}', 'assess-adopt')
                    self.assertEqual(adopted.returncode, 0, adopted.stderr)
                    self.assertTrue(json.loads(adopted.stdout)['adoptable'])
                    native.reconcile(self.root, state, [agent], *args)
                    path.write_bytes(b'{"other":true}')
                    native.reconcile(self.root, state, [], *args)
                    self.assertEqual(path.read_bytes(), b'{"other":true}')

    def test_oversize_output_is_refused_before_replacement(self):
        state = self.root/'native.json'
        path = native.agent_path(self.root, 'claude')
        path.parent.mkdir()
        prefix, suffix = b'{"unrelated":"', b'"}'
        original = prefix + b'x'*(4*1024*1024-6-len(prefix)-len(suffix)) + suffix
        path.write_bytes(original)
        path.chmod(0o644)
        args = (self.root, state, ['claude'], 'http://localhost:14318', 'x'*64, tomli_w, os.getuid(), os.getgid())
        for check in (True, False):
            with self.assertRaisesRegex(ValueError, 'destination exceeds size bound'):
                native.reconcile(*args, check=check)
            self.assertTrue(path.read_bytes() == original)
            self.assertEqual(path.stat().st_mode & 0o777, 0o644)
            self.assertFalse(state.exists())
            self.assertFalse(list(self.state_root.glob('*.contribution')))
        # The template writer has the same bound before contribution registration.
        refused = self.materialize('claude', path, b'{"template":true}')
        self.assertEqual(refused.returncode, 2)
        self.assertIn(b'destination exceeds size bound', refused.stderr)
        self.assertEqual(path.read_bytes(), original)


if __name__ == '__main__':
    unittest.main()
