#!/usr/bin/env python3
"""Host-free private bootstrap, identity preservation and retry contracts."""
import copy
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

SCRIPT = Path(__file__).resolve().parents[1]/'account.py'
spec = importlib.util.spec_from_file_location('openlit_account', SCRIPT)
account = importlib.util.module_from_spec(spec)
spec.loader.exec_module(account)
IDS = {'user': 'user-1', 'organisation': 'org-1', 'project': 'project-1', 'database': 'db-1'}


class AccountTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='openlit-account-', dir='/tmp')
        self.addCleanup(self.temporary.cleanup)
        self.state = Path(self.temporary.name)
        os.chmod(self.state, 0o700)
        (self.state/'marker').write_text('subyard-openlit-v1\n')
        os.chmod(self.state/'marker', 0o600)
        self.actual = {'ids': copy.deepcopy(IDS), 'stock': True, 'fresh': True}
        self.password = 'openlituser'
        self.calls = []
        self.fail_after_rotation = False

    def prepare(self, existing=False):
        with patch.object(account, 'volume_exists', return_value=existing):
            return account.prepare(self.state)

    def probe(self, state, source):
        return copy.deepcopy(self.actual)

    def client(self):
        owner = self
        class Client:
            def login(self, password):
                owner.calls.append('login')
                if owner.password != password:
                    raise ValueError('credentials rejected')
                return IDS['user']
            def request(self, path, payload):
                owner.calls.append(path)
                owner.assertEqual(path, '/api/user/profile')
                owner.assertEqual(payload['currentPassword'], owner.password)
                owner.password = payload['newPassword']
                owner.actual['stock'] = False
                if owner.fail_after_rotation:
                    raise ValueError('response lost')
                return {'id': IDS['user']}
        return Client()

    def apply(self):
        return account.apply(self.state, self.state, self.probe, self.client)

    def test_fresh_private_password_and_stable_linkage(self):
        self.assertEqual(self.prepare()['kind'], 'fresh')
        private = (self.state/'operator.json').read_bytes()
        self.assertEqual((self.state/'operator.json').stat().st_mode & 0o777, 0o600)
        self.assertEqual((self.state/'account.json').stat().st_mode & 0o777, 0o600)
        self.assertEqual(self.apply()['ids'], IDS)
        self.assertEqual(self.calls.count('/api/user/profile'), 1)
        self.prepare(existing=True)
        self.apply()
        account.check(self.state, self.state, self.probe)
        self.assertEqual(self.calls.count('/api/user/profile'), 1)
        self.assertEqual((self.state/'operator.json').read_bytes(), private)

    def test_lost_rotation_response_retains_intent_and_recovers(self):
        self.prepare()
        private = (self.state/'operator.json').read_bytes()
        self.fail_after_rotation = True
        with self.assertRaises(ValueError):
            self.apply()
        value = account.load(self.state)
        self.assertEqual(value['phase'], 'pending')
        self.assertEqual(value['ids'], IDS)
        self.fail_after_rotation = False
        self.assertEqual(self.apply()['phase'], 'ready')
        self.assertEqual(self.calls.count('/api/user/profile'), 1)
        self.assertEqual((self.state/'operator.json').read_bytes(), private)

    def test_completed_account_keeps_operator_password_changes(self):
        self.prepare()
        self.apply()
        calls = len(self.calls)
        self.password = 'Operator-selected-password-9'
        self.apply()
        account.check(self.state, self.state, self.probe)
        self.assertEqual(len(self.calls), calls)
        self.assertEqual(self.password, 'Operator-selected-password-9')

    def test_existing_volume_preserves_accounts_without_credentials_or_login(self):
        self.assertEqual(self.prepare(existing=True)['kind'], 'existing')
        self.actual['fresh'] = False
        self.assertEqual(self.apply()['phase'], 'ready')
        account.check(self.state, self.state, self.probe)
        self.assertFalse((self.state/'operator.json').exists())
        self.assertEqual(self.password, 'openlituser')
        self.assertEqual(self.calls, [])

    def test_missing_existing_volume_cannot_be_reclassified_as_fresh(self):
        self.prepare(existing=True)
        with self.assertRaises(ValueError):
            self.prepare(existing=False)
        self.assertFalse((self.state/'operator.json').exists())

    def test_fresh_install_refuses_modified_or_ambiguous_state(self):
        self.prepare()
        self.actual['fresh'] = False
        with self.assertRaises(ValueError):
            self.apply()
        self.assertEqual(self.calls, [])
        self.assertEqual(account.load(self.state)['phase'], 'pending')

    def test_identity_drift_and_stock_password_reversion_fail_closed(self):
        self.prepare()
        self.apply()
        self.actual['ids']['database'] = 'replacement-db'
        with self.assertRaises(ValueError):
            self.apply()
        with self.assertRaises(ValueError):
            account.check(self.state, self.state, self.probe)
        self.actual['ids'] = copy.deepcopy(IDS)
        self.actual['stock'] = True
        with self.assertRaises(ValueError):
            account.check(self.state, self.state, self.probe)

    def test_unsafe_private_inputs_fail_before_volume_probe_or_mutation(self):
        for name in ('marker', 'operator.json', 'account.json', 'database.env'):
            with self.subTest(name=name):
                path = self.state/name
                original = path.read_bytes() if path.exists() else None
                if original is not None:
                    path.unlink()
                path.symlink_to(self.state/'outside')
                with patch.object(account, 'volume_exists') as probe:
                    with self.assertRaises(ValueError):
                        account.prepare(self.state)
                    probe.assert_not_called()
                path.unlink()
                if original is not None:
                    path.write_bytes(original)
                    os.chmod(path, 0o600)
        self.assertFalse((self.state/'operator.json').exists())
        os.chmod(self.state, 0o750)
        with self.assertRaises(ValueError):
            self.prepare()

    def test_incomplete_credential_publication_is_retryable_only_without_volume(self):
        account.write(self.state/'operator.json', {'email': account.EMAIL, 'password': 'Openlit-7-'+'a'*64})
        original = (self.state/'operator.json').read_bytes()
        with self.assertRaises(ValueError):
            self.prepare(existing=True)
        self.assertFalse((self.state/'account.json').exists())
        self.prepare()
        self.assertEqual((self.state/'operator.json').read_bytes(), original)

    def test_readiness_is_read_only_and_pending_is_not_ready(self):
        self.prepare()
        with self.assertRaises(ValueError):
            account.check(self.state, self.state, self.probe)
        self.apply()
        before = {p.name: p.read_bytes() for p in self.state.iterdir()}
        account.check(self.state, self.state, self.probe)
        self.assertEqual(before, {p.name: p.read_bytes() for p in self.state.iterdir()})

    def test_readiness_projects_linkage_with_one_bounded_native_call(self):
        self.prepare()
        self.apply()
        account.write(self.state/'database.env', {})
        with patch.object(account, 'run', return_value=json.dumps(self.actual).encode()) as native:
            account.check(self.state, self.state)
        native.assert_called_once()
        self.assertEqual(native.call_args.kwargs, {'timeout': 8})
        self.assertEqual(native.call_args.args[0][-5:-2], ['-T', 'openlit', 'node'])

    def test_foreign_volume_refused(self):
        def run(argv):
            if argv[1:4] == ['volume', 'ls', '-q']:
                return (account.VOLUME+'\n').encode()
            return json.dumps([{'Name': account.VOLUME, 'Labels': {'com.docker.compose.project': 'foreign'}}]).encode()
        with patch.object(account, 'run', side_effect=run):
            with self.assertRaises(ValueError):
                account.prepare(self.state)
        self.assertFalse((self.state/'operator.json').exists())

    def test_foreign_container_mount_refused_before_account_probe(self):
        account.write(self.state/'database.env', {})
        container_id = 'a'*64
        calls = []
        def run(argv):
            calls.append(argv)
            if argv[-3:] == ['ps', '-q', 'openlit']:
                return container_id.encode()
            return json.dumps([{'Id': container_id, 'Config': {'Labels': {'com.docker.compose.project': 'subyard-openlit', 'com.docker.compose.service': 'openlit'}}, 'Mounts': [{'Destination': '/app/client/data', 'Type': 'volume', 'Name': 'foreign-data'}]}]).encode()
        with patch.object(account, 'volume_exists', return_value=True), patch.object(account, 'run', side_effect=run):
            with self.assertRaises(ValueError):
                account.inspect(self.state, self.state)
        self.assertEqual(len(calls), 2)

    def test_cli_error_does_not_print_private_state(self):
        self.prepare()
        sentinel = 'private-password-sentinel'
        account.write(self.state/'operator.json', {'email': account.EMAIL, 'password': sentinel})
        result = subprocess.run(['python3', str(SCRIPT), 'check', '--state', str(self.state), '--source', str(self.state)], capture_output=True, text=True, timeout=10)
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn(sentinel, result.stdout+result.stderr)
        self.assertNotIn(str(self.state), result.stdout+result.stderr)

    def test_login_callback_cannot_escape_loopback_origin(self):
        client = account.Client()
        for target in ('https://foreign.invalid/dashboard', account.ORIGIN+'/login?error=secret', account.ORIGIN+'/dashboard?error=secret'):
            def request(path, payload=None, form=False):
                return {'csrfToken': 'fixture'} if path.endswith('/csrf') else {'url': target}
            with self.subTest(target=target), patch.object(client, 'request', side_effect=request):
                with self.assertRaises(ValueError):
                    client.login('synthetic')


if __name__ == '__main__':
    unittest.main()
