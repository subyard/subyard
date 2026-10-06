#!/usr/bin/env python3
"""Behavioral checks for Amnezia's owner handler and guest runtime."""
import contextlib
import copy
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest import mock


PROFILE = Path(__file__).resolve().parents[1]


def load_module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


handler = load_module('amnezia_owner_test', PROFILE / 'resources/vpn/handler.py')
runtime = load_module('amnezia_guest_test', PROFILE / 'runtime.py')
boot_observer = load_module('amnezia_boot_test', PROFILE / 'tests/e2e/first-boot-observer.py')


def result(stdout=b'', code=0):
    return subprocess.CompletedProcess([], code, stdout, b'')


class OwnerHandlerTest(unittest.TestCase):
    def setUp(self):
        values = {
            'SUBYARD_ENGINE_CONTEXT': '1', 'EXCLUSIVE_ENVIRONMENT_PROFILE': 'amnezia',
            'YARD_KIND': 'vm', 'YARD_NAME': 'personal-vpn',
            'ENVIRONMENT_PROFILES': 'amnezia', 'SUBYARD_RESOURCE_MODE': 'prepare',
            'RESOURCE_VPN_IPV4': '', 'RESOURCE_VPN_INTERFACE': '', 'RESOURCE_VPN_PORT': '51820',
        }
        patch = mock.patch.dict(os.environ, values)
        patch.start()
        self.addCleanup(patch.stop)
        handler.YARD = 'yard-personal-vpn'
        handler.PROJECT = 'subyard-personal-vpn'

    def instance(self):
        return {'name': handler.YARD, 'project': handler.PROJECT,
                'type': 'virtual-machine', 'status': 'Running',
                'config': {}, 'devices': {},
                'expanded_devices': {'eth0': {'type': 'nic', 'ipv4.address': '10.80.0.10'}}}

    def test_disabled_status_with_unset_endpoint_is_read_only(self):
        os.environ.update(SUBYARD_RESOURCE_MODE='apply', SUBYARD_RESOURCE_ACTION='status',
                          SUBYARD_OPERATION_ID='op-test')
        with mock.patch.object(handler, 'inspect', return_value=self.instance()), \
                mock.patch.object(handler, 'runtime_status', return_value={'ready': False, 'running': False, 'enabled': False}), \
                mock.patch.object(handler, 'incus', side_effect=AssertionError('status wrote Incus state')), \
                mock.patch.object(sys, 'argv', ['vpn', 'status']), io.StringIO() as output, contextlib.redirect_stdout(output):
            self.assertEqual(handler.main(), 0)
            status = json.loads(output.getvalue())
        self.assertFalse(status['ready'])
        self.assertFalse(status['ingress'])

    def test_first_start_prepares_from_stopped_vm_without_guest_access(self):
        instance = self.instance()
        instance['status'] = 'Stopped'
        instance['config'][handler.STARTUP] = 'pending'
        os.environ.update(RESOURCE_VPN_IPV4='10.20.30.40', RESOURCE_VPN_INTERFACE='eth0',
                          SUBYARD_RESOURCE_MODE='prepare-start')
        want = {'type': 'proxy', 'bind': 'host', 'nat': 'true',
                'listen': 'udp:10.20.30.40:51820', 'connect': 'udp:10.80.0.10:51820'}
        with mock.patch.object(handler, 'inspect', return_value=instance), \
                mock.patch.object(handler, 'endpoint', return_value=('10.20.30.40', 'eth0', '51820', want)), \
                mock.patch.object(handler, 'collisions'), \
                mock.patch.object(handler, 'guest', side_effect=AssertionError('prestart reached guest')), \
                mock.patch.object(handler, 'runtime_status', side_effect=AssertionError('prestart probed guest')), \
                contextlib.redirect_stdout(io.StringIO()) as output:
            handler.prepare('up', prestart=True)
        assessment = json.loads(output.getvalue())
        self.assertTrue(assessment['changed'])
        self.assertIn('Publish UDP 10.20.30.40:51820', assessment['consequences'][1])

    def test_native_v2_verifier_reads_real_service_and_ingress_postconditions(self):
        instance = self.instance()
        want = {'type': 'proxy', 'bind': 'host', 'nat': 'true',
                'listen': 'udp:10.20.30.40:51820', 'connect': 'udp:10.80.0.10:51820'}
        instance['devices'][handler.DEVICE] = want
        instance['config'][handler.KEY] = handler.fingerprint(want)
        os.environ.update(RESOURCE_VPN_IPV4='10.20.30.40', RESOURCE_VPN_INTERFACE='eth0',
                          SUBYARD_RESOURCE_MODE='verify')
        with mock.patch.object(handler, 'inspect', return_value=instance), \
                mock.patch.object(handler, 'endpoint', return_value=('10.20.30.40', 'eth0', '51820', want)), \
                mock.patch.object(handler, 'collisions'), \
                mock.patch.object(handler, 'runtime_status', return_value={'ready': True, 'running': True, 'enabled': True}), \
                mock.patch.object(handler, 'incus', side_effect=AssertionError('verify wrote owner state')), \
                mock.patch.object(handler, 'guest', return_value=result()) as guest, \
                mock.patch.object(sys, 'argv', ['vpn', 'up']), \
                contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(handler.main(), 0)
        converged = json.loads(output.getvalue())
        self.assertEqual(converged['schema'], 'yard.resource-action-assessment.v2')
        self.assertFalse(converged['changed'])
        self.assertTrue(all(step['decision'] == 'skip' and step['observed'] == step['desired']
                            for step in converged['steps']))
        guest.assert_called_once_with('find', '/srv/workspaces', '-mindepth', '1', '-maxdepth', '1', '-print', '-quit')
        with mock.patch.object(handler, 'inspect', return_value=instance), \
                mock.patch.object(handler, 'endpoint', return_value=('10.20.30.40', 'eth0', '51820', want)), \
                mock.patch.object(handler, 'collisions'), \
                mock.patch.object(handler, 'runtime_status', return_value={'ready': False, 'running': True, 'enabled': True}), \
                mock.patch.object(handler, 'guest', return_value=result()), \
                contextlib.redirect_stdout(io.StringIO()) as output:
            handler.prepare('up')
        divergent = json.loads(output.getvalue())
        self.assertTrue(divergent['changed'])
        self.assertEqual(divergent['binding'], converged['binding'])
        self.assertEqual([step['decision'] for step in divergent['steps']], ['apply', 'skip'])

    def test_pending_stopped_yard_allows_endpoint_authoring_and_down(self):
        instance = self.instance()
        instance['status'] = 'Stopped'
        instance['config'][handler.STARTUP] = 'pending'
        with mock.patch.object(handler, 'inspect', return_value=instance), \
                mock.patch.object(handler, 'guest', side_effect=AssertionError('stopped guest accessed')), \
                mock.patch.object(handler, 'runtime_status', side_effect=AssertionError('stopped guest probed')), \
                contextlib.redirect_stdout(io.StringIO()) as output:
            handler.prepare('down')
        self.assertFalse(json.loads(output.getvalue())['changed'])
        os.environ.update(SUBYARD_RESOURCE_MODE='apply', SUBYARD_RESOURCE_ACTION='down',
                          SUBYARD_OPERATION_ID='op-test')
        with mock.patch.object(handler, 'inspect', return_value=instance), \
                mock.patch.object(handler, 'guest', side_effect=AssertionError('stopped guest accessed')), \
                mock.patch.object(sys, 'argv', ['vpn', 'down']):
            self.assertEqual(handler.main(), 0)

    def test_unselected_up_and_malformed_arguments_never_reach_target(self):
        os.environ['ENVIRONMENT_PROFILES'] = ''
        with mock.patch.object(handler, 'inspect', side_effect=AssertionError('unselected yard inspected')), \
                mock.patch.object(sys, 'argv', ['vpn', 'up']):
            with self.assertRaisesRegex(RuntimeError, 'selected'):
                handler.main()
        with mock.patch.object(sys, 'argv', ['vpn', 'up', 'extra']), contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(handler.main(), 2)

    def test_unselected_dedicated_down_prepares_read_only_and_stops_runtime(self):
        os.environ['ENVIRONMENT_PROFILES'] = ''
        instance = self.instance()
        with mock.patch.object(handler, 'inspect', return_value=instance), \
                mock.patch.object(handler, 'runtime_status', return_value={'ready': True, 'running': True, 'enabled': True}), \
                mock.patch.object(handler, 'incus', side_effect=AssertionError('prepare mutated Incus')), \
                mock.patch.object(handler, 'guest', side_effect=AssertionError('prepare stopped guest')), \
                mock.patch.object(sys, 'argv', ['vpn', 'down']), \
                contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(handler.main(), 0)
        assessment = json.loads(output.getvalue())
        self.assertTrue(assessment['changed'])
        self.assertEqual(assessment['action'], 'down')

        os.environ.update(SUBYARD_RESOURCE_MODE='apply', SUBYARD_RESOURCE_ACTION='down',
                          SUBYARD_OPERATION_ID='op-test')
        with mock.patch.object(handler, 'inspect', return_value=instance), \
                mock.patch.object(handler, 'guest') as guest, \
                mock.patch.object(sys, 'argv', ['vpn', 'down']):
            self.assertEqual(handler.main(), 0)
        guest.assert_called_once_with('python3', handler.RUNTIME, 'down', timeout=20)

    def test_live_route_requires_durable_service_enablement(self):
        instance = self.instance()
        device = {'type': 'proxy', 'bind': 'host', 'nat': 'true',
                  'listen': 'udp:10.20.30.40:51820', 'connect': 'udp:10.80.0.10:51820'}
        instance['devices'][handler.DEVICE] = device
        instance['config'][handler.KEY] = handler.fingerprint(device)
        with mock.patch.object(handler, 'endpoint', return_value=('', '', '', device)), \
                mock.patch.object(handler, 'runtime_status', return_value={'ready': True, 'enabled': False}):
            self.assertFalse(handler.ready(instance))

    def test_owner_collision_and_foreign_proxy_are_rejected(self):
        want = {'type': 'proxy', 'bind': 'host', 'nat': 'true',
                'listen': 'udp:10.20.30.40:51820', 'connect': 'udp:10.80.0.10:51820'}
        with mock.patch.object(handler, 'run', return_value=result(b'udp UNCONN 0 0 10.20.30.40:51820 0.0.0.0:*\n')):
            with self.assertRaisesRegex(RuntimeError, 'already bound'):
                handler.collisions(want)

        def foreign_proxy(*args, **_):
            if args[0] == 'ss':
                return result()
            raise AssertionError(args)

        with mock.patch.object(handler, 'run', side_effect=foreign_proxy), \
                mock.patch.object(handler, 'query', return_value=[
                    {'name': 'other', 'project': 'other', 'expanded_devices': {'route': want}}]):
            with self.assertRaisesRegex(RuntimeError, 'another Incus proxy'):
                handler.collisions(want)
        for listener in ('udp:10.20.30.40:51819-51821',
                         'udp:0.0.0.0:51700,51820', 'udp:[::]:51819,51820-51822'):
            with self.subTest(listener=listener):
                foreign = want | {'listen': listener}
                with mock.patch.object(handler, 'run', side_effect=foreign_proxy), \
                        mock.patch.object(handler, 'query', return_value=[
                            {'name': 'other', 'project': 'other', 'expanded_devices': {'route': foreign}}]):
                    with self.assertRaisesRegex(RuntimeError, 'another Incus proxy'):
                        handler.collisions(want)
        foreign = {'type': 'proxy', 'bind': 'instance',
                   'listen': 'udp:0.0.0.0:51700,51819-51821',
                   'connect': 'udp:127.0.0.1:51820'}
        with mock.patch.object(handler, 'run', side_effect=foreign_proxy), \
                mock.patch.object(handler, 'query', side_effect=lambda path: (
                    [{'name': 'other', 'project': 'other', 'expanded_devices': {'route': foreign}}]
                    if path == '/1.0/instances?recursion=1&all-projects=true' else [])):
            handler.collisions(want)
        with mock.patch.object(handler, 'run', side_effect=foreign_proxy), \
                mock.patch.object(handler, 'query', side_effect=lambda path: (
                    [] if path == '/1.0/instances?recursion=1&all-projects=true' else
                    [{'name': 'incusbr0', 'managed': True}] if path == '/1.0/networks?recursion=1' else
                    [{'listen_address': '10.20.30.40', 'config': {}, 'ports': [
                        {'protocol': 'udp', 'listen_port': '51700,51819-51821'}]}])):
            with self.assertRaisesRegex(RuntimeError, 'network forward'):
                handler.collisions(want)
        instance = self.instance()
        instance['devices'][handler.DEVICE] = want
        instance['config'][handler.KEY] = 'v1:' + '0' * 64
        with self.assertRaisesRegex(RuntimeError, 'foreign or modified'):
            handler.owned(instance)

    def test_pending_route_recovers_and_rollback_removes_only_its_device(self):
        state = self.instance()
        foreign = {'type': 'proxy', 'listen': 'tcp:127.0.0.1:2022'}
        state['devices']['foreign'] = foreign
        state['expanded_devices']['foreign'] = foreign
        state['config'][handler.KEY] = 'v1:pending:' + 'a' * 64
        want = {'type': 'proxy', 'bind': 'host', 'nat': 'true',
                'listen': 'udp:10.20.30.40:51820', 'connect': 'udp:10.80.0.10:51820'}
        writes = []

        def incus(*args, **_):
            writes.append(args)
            if args[:3] == ('config', 'unset', handler.YARD):
                state['config'].pop(handler.KEY, None)
            elif args[:3] == ('config', 'set', handler.YARD):
                state['config'][handler.KEY] = args[4]
            elif args[:3] == ('config', 'device', 'add'):
                self.assertEqual(args[4], handler.DEVICE)
                state['devices'][handler.DEVICE] = want.copy()
                state['expanded_devices'][handler.DEVICE] = want.copy()
            elif args[:3] == ('config', 'device', 'remove'):
                self.assertEqual(args[4], handler.DEVICE)
                state['devices'].pop(handler.DEVICE, None)
                state['expanded_devices'].pop(handler.DEVICE, None)
            else:
                raise AssertionError(args)
            return result()

        with mock.patch.object(handler, 'inspect', side_effect=lambda: copy.deepcopy(state)), \
                mock.patch.object(handler, 'incus', side_effect=incus), \
                mock.patch.object(handler, 'guest', return_value=result()) as guest:
            handler.ensure_ingress(want)
            self.assertEqual(state['config'][handler.KEY], handler.fingerprint(want))
            self.assertEqual(state['devices'][handler.DEVICE], want)
            os.environ.update(SUBYARD_RESOURCE_MODE='apply', SUBYARD_RESOURCE_ACTION='up',
                              SUBYARD_OPERATION_ID='op-test')
            with mock.patch.object(sys, 'argv', ['vpn', 'rollback-ingress']):
                self.assertEqual(handler.main(), 0)
        self.assertNotIn(handler.DEVICE, state['devices'])
        self.assertNotIn(handler.KEY, state['config'])
        self.assertEqual(state['devices']['foreign'], foreign)
        self.assertTrue(any(action[:3] == ('config', 'device', 'remove') for action in writes))
        guest.assert_called_once_with('python3', handler.RUNTIME, 'down', timeout=20)

    def test_shutdown_failure_keeps_pending_retry_intent_until_guest_stops(self):
        for verb, action in (('down', 'down'), ('rollback-ingress', 'up')):
            with self.subTest(verb=verb):
                self.check_shutdown_retry(verb, action)

    def test_down_assesses_owned_ingress_when_guest_status_is_unavailable(self):
        device = {'type': 'proxy', 'bind': 'host', 'nat': 'true',
                  'listen': 'udp:10.20.30.40:51820', 'connect': 'udp:10.80.0.10:51820'}
        for pending_only in (False, True):
            with self.subTest(pending_only=pending_only):
                instance = self.instance()
                instance['config'][handler.KEY] = handler.fingerprint(device)
                if pending_only:
                    instance['config'][handler.KEY] = instance['config'][handler.KEY].replace('v1:', 'v1:pending:', 1)
                else:
                    instance['devices'][handler.DEVICE] = device
                with mock.patch.object(handler, 'inspect', return_value=instance), \
                        mock.patch.object(handler, 'runtime_status', side_effect=RuntimeError('guest unavailable')) as status, \
                        mock.patch.object(handler, 'incus', side_effect=AssertionError('prepare mutated Incus')), \
                        mock.patch.object(handler, 'guest', side_effect=AssertionError('prepare changed guest')), \
                        contextlib.redirect_stdout(io.StringIO()) as output:
                    handler.prepare('down')
                status.assert_not_called()
                assessment = json.loads(output.getvalue())
                self.assertTrue(assessment['changed'])
                self.assertIn('pending cleanup', assessment['consequences'][0])
                with mock.patch.object(handler, 'inspect', return_value=instance), \
                        mock.patch.object(handler, 'runtime_status', return_value={
                            'ready': True, 'running': True, 'enabled': True}) as status, \
                        contextlib.redirect_stdout(io.StringIO()) as refreshed:
                    handler.prepare('down')
                status.assert_not_called()
                self.assertEqual(json.loads(refreshed.getvalue()), assessment)
                with mock.patch.object(handler, 'inspect', return_value=instance), \
                        mock.patch.object(handler, 'runtime_status', side_effect=RuntimeError('guest unavailable')) as status:
                    with self.assertRaisesRegex(RuntimeError, 'guest unavailable'):
                        handler.prepare('up')
                status.assert_called_once_with(instance)
        with mock.patch.object(handler, 'inspect', return_value=self.instance()), \
                mock.patch.object(handler, 'runtime_status', side_effect=RuntimeError('guest unavailable')) as status:
            with self.assertRaisesRegex(RuntimeError, 'guest unavailable'):
                handler.prepare('down')
        status.assert_called_once()

    def test_guest_shutdown_timeout_is_bounded_and_reported_as_unverified(self):
        with self.assertRaises(subprocess.TimeoutExpired):
            handler.run(sys.executable, '-c', 'import time; time.sleep(1)', timeout=0.02)
        with mock.patch.object(handler, 'guest', side_effect=subprocess.TimeoutExpired(['incus', 'exec'], 20)) as guest:
            with self.assertRaisesRegex(RuntimeError, 'guest shutdown is unverified') as failure:
                handler.shutdown_guest()
        self.assertIsInstance(failure.exception.__cause__, subprocess.TimeoutExpired)
        guest.assert_called_once_with('python3', handler.RUNTIME, 'down', timeout=20)

    def test_late_up_readiness_failure_keeps_pending_rollback_intent(self):
        os.environ.update(SUBYARD_RESOURCE_MODE='apply', SUBYARD_RESOURCE_ACTION='up',
                          SUBYARD_OPERATION_ID='op-test')
        state = self.instance()
        want = {'type': 'proxy', 'bind': 'host', 'nat': 'true',
                'listen': 'udp:10.20.30.40:51820', 'connect': 'udp:10.80.0.10:51820'}

        def publish(_):
            state['devices'][handler.DEVICE] = want.copy()
            state['expanded_devices'][handler.DEVICE] = want.copy()
            state['config'][handler.KEY] = handler.fingerprint(want)

        def incus(*args, **_):
            if args[:3] == ('config', 'set', handler.YARD):
                state['config'][handler.KEY] = args[4]
            elif args[:3] == ('config', 'device', 'remove'):
                state['devices'].pop(handler.DEVICE, None)
                state['expanded_devices'].pop(handler.DEVICE, None)
            else:
                raise AssertionError(args)
            return result()

        with mock.patch.object(handler, 'inspect', side_effect=lambda: copy.deepcopy(state)), \
                mock.patch.object(handler, 'endpoint', return_value=('10.20.30.40', 'eth0', '51820', want)), \
                mock.patch.object(handler, 'collisions'), \
                mock.patch.object(handler, 'guest', return_value=result()), \
                mock.patch.object(handler, 'ensure_ingress', side_effect=publish), \
                mock.patch.object(handler, 'ready', return_value=False), \
                mock.patch.object(handler, 'incus', side_effect=incus), \
                mock.patch.object(sys, 'argv', ['vpn', 'up']):
            with self.assertRaisesRegex(RuntimeError, 'did not converge'):
                handler.main()
        self.assertNotIn(handler.DEVICE, state['devices'])
        self.assertEqual(state['config'][handler.KEY],
                         handler.fingerprint(want).replace('v1:', 'v1:pending:', 1))

    def check_shutdown_retry(self, verb, action):
        os.environ.update(ENVIRONMENT_PROFILES='', SUBYARD_RESOURCE_MODE='apply',
                          SUBYARD_RESOURCE_ACTION=action, SUBYARD_OPERATION_ID='op-test')
        state = self.instance()
        device = {'type': 'proxy', 'bind': 'host', 'nat': 'true',
                  'listen': 'udp:10.20.30.40:51820', 'connect': 'udp:10.80.0.10:51820'}
        state['devices'][handler.DEVICE] = device.copy()
        state['expanded_devices'][handler.DEVICE] = device.copy()
        state['config'][handler.KEY] = handler.fingerprint(device)
        protected_state = {'server': 'synthetic-server-key', 'client': 'synthetic-client-key'}

        def incus(*args, **_):
            if args[:3] == ('config', 'set', handler.YARD):
                state['config'][handler.KEY] = args[4]
            elif args[:3] == ('config', 'device', 'remove'):
                state['devices'].pop(handler.DEVICE, None)
                state['expanded_devices'].pop(handler.DEVICE, None)
            elif args[:3] == ('config', 'unset', handler.YARD):
                state['config'].pop(handler.KEY, None)
            else:
                raise AssertionError(args)
            return result()

        attempts = 0
        def guest(*args, **_):
            nonlocal attempts
            self.assertEqual(args, ('python3', handler.RUNTIME, 'down'))
            self.assertEqual(_.get('timeout'), 20)
            attempts += 1
            if attempts == 1:
                raise RuntimeError('guest shutdown unavailable')
            return result()

        with mock.patch.object(handler, 'inspect', side_effect=lambda: copy.deepcopy(state)), \
                mock.patch.object(handler, 'incus', side_effect=incus), \
                mock.patch.object(handler, 'guest', side_effect=guest), \
                mock.patch.object(sys, 'argv', ['vpn', verb]):
            with self.assertRaisesRegex(RuntimeError, 'guest shutdown is unverified'):
                handler.main()
            self.assertNotIn(handler.DEVICE, state['devices'])
            self.assertEqual(state['config'][handler.KEY],
                             handler.fingerprint(device).replace('v1:', 'v1:pending:', 1))
            self.assertEqual(protected_state['server'], 'synthetic-server-key')
            self.assertEqual(handler.main(), 0)
        self.assertNotIn(handler.KEY, state['config'])
        self.assertEqual(attempts, 2)
        self.assertEqual(protected_state['client'], 'synthetic-client-key')


class GuestRuntimeTest(unittest.TestCase):
    def root_owned_stat(self):
        original = Path.lstat

        def fake_lstat(path):
            info = original(path)
            return SimpleNamespace(st_uid=0, st_mode=info.st_mode)

        return mock.patch.object(Path, 'lstat', fake_lstat)

    def test_state_modes_and_types_are_enforced(self):
        with tempfile.TemporaryDirectory() as directory, self.root_owned_stat():
            root = Path(directory) / 'state'
            root.mkdir(mode=0o700)
            root.chmod(0o700)
            secret = root / 'awg0.conf'
            secret.write_text('synthetic-server-key\n')
            secret.chmod(0o644)
            with self.assertRaisesRegex(RuntimeError, 'unsafe ownership or permissions'):
                runtime.protected(secret)
            secret.chmod(0o600)
            runtime.protected(secret)
            link = root / 'link'
            link.symlink_to(secret)
            with self.assertRaises(RuntimeError):
                runtime.protected(link)
            root.chmod(0o755)
            with self.assertRaises(RuntimeError):
                runtime.protected(root, True)

    def test_state_mount_required_before_keys_endpoint_edits_or_start(self):
        with tempfile.TemporaryDirectory() as directory:
            srv = Path(directory) / 'srv'
            srv.mkdir()
            state = srv / 'amnezia'
            with mock.patch.object(runtime, 'STATE', state), \
                    mock.patch.object(runtime, 'awg') as awg, \
                    mock.patch.object(runtime, 'container') as container:
                self.assertFalse(os.path.ismount(srv))
                with self.assertRaisesRegex(RuntimeError, 'not mounted'):
                    runtime.initialize('10.20.30.40', 51820)
                self.assertFalse(state.exists())
                state.mkdir()
                with self.assertRaisesRegex(RuntimeError, 'not mounted'):
                    runtime.up('10.20.30.41', 51820)
                with self.assertRaisesRegex(RuntimeError, 'not mounted'):
                    runtime.start()
                awg.assert_not_called()
                container.assert_not_called()
            link = Path(directory) / 'srv-link'
            link.symlink_to(srv, target_is_directory=True)
            with mock.patch.object(runtime, 'STATE', link / 'amnezia'), \
                    mock.patch.object(runtime.os.path, 'ismount', return_value=True):
                with self.assertRaisesRegex(RuntimeError, 'not mounted'):
                    runtime.initialize('10.20.30.40', 51820)

    def test_firewall_readiness_detects_effective_rule_drift(self):
        with tempfile.TemporaryDirectory() as directory, self.root_owned_stat():
            record = Path(directory) / 'firewall.json'
            record.write_text(json.dumps({'policy': hashlib.sha256(b'declared-rules').hexdigest(),
                                          'effective': 'expected-rules'}))
            record.chmod(0o600)
            with mock.patch.object(runtime, 'FIREWALL_STATE', record), \
                    mock.patch.object(runtime, 'firewall_owned', return_value=True), \
                    mock.patch.object(runtime, 'firewall_policy', return_value='declared-rules'), \
                    mock.patch.object(runtime, 'firewall_digest', return_value='expected-rules'):
                self.assertTrue(runtime.firewall_ready())
                with mock.patch.object(runtime, 'firewall_digest', return_value='modified-rules'):
                    self.assertFalse(runtime.firewall_ready())

    def test_firewall_allows_ping_only_to_the_tunnel_address(self):
        with tempfile.TemporaryDirectory() as directory, self.root_owned_stat():
            state = Path(directory)
            state.chmod(0o700)
            settings = state / 'settings.json'
            settings.write_text('{"endpoint":"10.20.30.40","port":51820}\n')
            settings.chmod(0o600)
            with mock.patch.object(runtime, 'STATE', state):
                policy = runtime.firewall_policy()
        self.assertIn('iifname "awg0" ip daddr 10.90.0.1 ip protocol icmp icmp type echo-request accept', policy)
        self.assertIn('iifname "awg0" drop', policy)

    def test_effective_container_drift_prevents_ready_and_up_restarts_without_key_change(self):
        expected = {'State': {'Running': True},
                    'Config': {'Image': runtime.IMAGE, 'Entrypoint': ['/bin/bash'],
                               'Cmd': ['/opt/amnezia/start.sh']},
                    'HostConfig': {'NetworkMode': 'host'},
                    'Mounts': [{'Type': 'bind', 'Source': str(runtime.STATE / 'awg0.conf'),
                                'Destination': '/etc/amnezia/awg0.conf', 'RW': False},
                               {'Type': 'bind', 'Source': str(runtime.ROOT / 'container.sh'),
                                'Destination': '/opt/amnezia/start.sh', 'RW': False}]}

        def command(*args, **_):
            if args[:3] == ('systemctl', 'is-active', '--quiet'):
                return result()
            if args[:4] == ('docker', 'exec', runtime.CONTAINER, 'awg'):
                return result(b'51820\n')
            return result()

        with mock.patch.object(runtime, 'container', return_value=expected), \
                mock.patch.object(runtime, 'firewall_ready', return_value=True), \
                mock.patch.object(runtime, 'run', side_effect=command):
            self.assertTrue(runtime.observe()['ready'])
            for mutate in (lambda value: value['HostConfig'].update(NetworkMode='bridge'),
                           lambda value: value['Mounts'][0].update(RW=True),
                           lambda value: value['Mounts'][1].update(Source='/tmp/foreign-start.sh'),
                           lambda value: value['Config'].update(Entrypoint=['/bin/sh']),
                           lambda value: value['Config'].update(Cmd=['/bin/false']),
                           lambda value: value['Config'].update(Image='other-image')):
                with self.subTest(mutate=mutate):
                    drifted = copy.deepcopy(expected)
                    mutate(drifted)
                    with mock.patch.object(runtime, 'container', return_value=drifted):
                        self.assertFalse(runtime.observe()['ready'])
        with mock.patch.object(runtime, 'container', return_value=expected), \
                mock.patch.object(runtime, 'firewall_ready', return_value=True), \
                mock.patch.object(runtime, 'run', side_effect=lambda *args, **kwargs: (
                    result(code=3) if args[:3] == ('systemctl', 'is-active', '--quiet') else command(*args, **kwargs))):
            self.assertFalse(runtime.observe()['ready'])

        with tempfile.TemporaryDirectory() as directory, self.root_owned_stat():
            state = Path(directory)
            state.chmod(0o700)
            for name, content in (('awg0.conf', 'synthetic-server-key\n'),
                                  ('client.conf', 'PrivateKey = synthetic-client-key\n'),
                                  ('settings.json', '{"endpoint": "10.20.30.40", "port": 51820}\n')):
                path = state / name
                path.write_text(content)
                path.chmod(0o600)
            calls = []
            with mock.patch.object(runtime, 'STATE', state), \
                    mock.patch.object(runtime.os.path, 'ismount', return_value=True), \
                    mock.patch.object(runtime, 'observe', side_effect=[{'ready': False}, {'ready': True}]), \
                    mock.patch.object(runtime, 'run', side_effect=lambda *args, **_: (calls.append(args), result())[1]):
                runtime.up('10.20.30.40', 51820)
            self.assertIn(('systemctl', 'restart', runtime.UNIT), calls)
            self.assertEqual((state / 'awg0.conf').read_text(), 'synthetic-server-key\n')
            self.assertIn('synthetic-client-key', (state / 'client.conf').read_text())

    def test_repeated_up_endpoint_change_and_down_preserve_synthetic_keys(self):
        with tempfile.TemporaryDirectory() as directory, self.root_owned_stat():
            root = Path(directory) / 'state'
            keys = iter(('synthetic-server-key', 'synthetic-client-key',
                         'synthetic-psk', 'synthetic-server-public', 'synthetic-client-public'))
            with mock.patch.object(runtime, 'STATE', root), \
                    mock.patch.object(runtime.os.path, 'ismount', return_value=True), \
                    mock.patch.object(runtime, 'awg', side_effect=lambda *_: next(keys)):
                runtime.initialize('10.20.30.40', 51820)
            server = (root / 'awg0.conf').read_text()
            client = (root / 'client.conf').read_text()
            self.assertIn('synthetic-server-key', server)
            self.assertIn('synthetic-client-key', client)
            self.assertEqual(root.stat().st_mode & 0o777, 0o700)
            for name in ('awg0.conf', 'client.conf', 'settings.json'):
                self.assertEqual((root / name).stat().st_mode & 0o777, 0o600)
            commands = []

            def command(*args, **_):
                commands.append(args)
                return result()

            with mock.patch.object(runtime, 'STATE', root), \
                    mock.patch.object(runtime.os.path, 'ismount', return_value=True), \
                    mock.patch.object(runtime, 'observe', return_value={'ready': True, 'running': True, 'enabled': True}), \
                    mock.patch.object(runtime, 'run', side_effect=command):
                runtime.up('10.20.30.40', 51820)
                runtime.up('10.20.30.40', 51820)
                self.assertEqual((root / 'awg0.conf').read_text(), server)
                self.assertEqual((root / 'client.conf').read_text(), client)
                runtime.up('10.20.30.41', 51820)
                self.assertEqual((root / 'awg0.conf').read_text(), server)
                self.assertIn('synthetic-client-key', (root / 'client.conf').read_text())
                self.assertIn('Endpoint = 10.20.30.41:51820', (root / 'client.conf').read_text())

                real_open = open
                def lock_open(path, *args, **kwargs):
                    if path == '/run/subyard-amnezia.lock':
                        return real_open(root / 'lock', 'a')
                    return real_open(path, *args, **kwargs)

                with mock.patch.object(runtime.os, 'geteuid', return_value=0), \
                        mock.patch.object(runtime, 'open', lock_open, create=True), \
                        mock.patch.object(runtime, 'container', return_value=None), \
                        mock.patch.object(runtime, 'firewall') as firewall, \
                        mock.patch.object(sys, 'argv', ['runtime', 'down']):
                    self.assertEqual(runtime.main(), 0)
                firewall.assert_called_with(remove=True)
            self.assertEqual((root / 'awg0.conf').read_text(), server)
            self.assertIn('synthetic-client-key', (root / 'client.conf').read_text())
            self.assertTrue(any(args[:3] == ('systemctl', 'disable', '--now') for args in commands))


class ProfileProvisionTest(unittest.TestCase):
    def test_check_detects_modes_and_owner_and_apply_repairs_modes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / 'profile'
            source.mkdir()
            destination = root / 'installed'
            unit = root / 'subyard-amnezia.service'
            for name in ('runtime.py', 'container.sh', 'release.env', 'subyard-amnezia.service'):
                shutil.copyfile(PROFILE / name, source / name)
            script = (PROFILE / 'provision.sh').read_text().replace(
                'destination=/usr/local/lib/subyard-amnezia', f'destination={destination}').replace(
                'unit=/etc/systemd/system/subyard-amnezia.service', f'unit={unit}')
            provision = source / 'provision.sh'
            provision.write_text(script)
            bin_dir = root / 'bin'
            bin_dir.mkdir()
            commands = {
                'id': '#!/bin/sh\n[ "$1" = -u ] && { echo 0; exit; }; exec /usr/bin/id "$@"\n',
                'dpkg-query': '#!/bin/sh\necho "install ok installed"\n',
                'docker': '#!/bin/sh\nexit 0\n',
                'apt-get': '#!/bin/sh\nexit 0\n',
                'systemctl': '#!/bin/sh\nexit 0\n',
                'stat': ('#!/bin/sh\n'
                         '[ "$1" = -c ] && [ "$2" = "%u:%g:%a" ] || exit 2\n'
                         'mode="$(/usr/bin/stat -c %a "$3")" || exit\n'
                         'printf "%s:%s:%s\\n" "${PROFILE_FAKE_UID:-0}" 0 "$mode"\n'),
                'install': ('#!/bin/bash\nset -euo pipefail\n'
                            'owner= group= args=()\n'
                            'while [ "$#" -gt 0 ]; do\n'
                            '  case "$1" in\n'
                            '    -o) owner="$2"; shift 2 ;;\n'
                            '    -g) group="$2"; shift 2 ;;\n'
                            '    *) args+=("$1"); shift ;;\n'
                            '  esac\n'
                            'done\n'
                            '[ "$owner" = 0 ] && [ "$group" = 0 ] || exit 9\n'
                            'exec /usr/bin/install "${args[@]}"\n'),
            }
            for name, body in commands.items():
                path = bin_dir / name
                path.write_text(body)
                path.chmod(0o755)
            environment = os.environ | {'PATH': f'{bin_dir}:{os.environ["PATH"]}',
                                        'EXCLUSIVE_ENVIRONMENT_PROFILE': 'amnezia', 'YARD_KIND': 'vm'}

            def invoke(*arguments, environment_override=None):
                return subprocess.run(['bash', str(provision), *arguments],
                                      env=environment | (environment_override or {}),
                                      capture_output=True, text=True)

            applied = invoke()
            self.assertEqual(applied.returncode, 0, applied.stderr)
            self.assertEqual(invoke('--check').returncode, 0)
            for path, mode in ((destination, 0o777), (destination / 'runtime.py', 0o666),
                               (destination / 'container.sh', 0o666),
                               (destination / 'release.env', 0o666), (unit, 0o666)):
                with self.subTest(path=path):
                    path.chmod(mode)
                    self.assertEqual(invoke('--check').returncode, 10)
                    self.assertEqual(invoke().returncode, 0)
                    self.assertEqual(invoke('--check').returncode, 0)
            self.assertEqual(invoke('--check', environment_override={'PROFILE_FAKE_UID': '1000'}).returncode, 10)


class FirstBootObserverTest(unittest.TestCase):
    def test_cancel_preserves_owner_process_group_and_native_signal_exit(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            directory.chmod(0o700)
            incus = directory / 'incus'
            incus.write_text('#!/bin/sh\nexit 1\n')
            incus.chmod(0o755)
            marker = directory / 'init-process'
            native = [sys.executable, '-c', '''import json, os, sys, time
fd = os.open(sys.argv[1] + '.tmp', os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
os.write(fd, json.dumps({'pid': os.getpid(), 'group': os.getpgrp()}).encode()); os.close(fd)
os.rename(sys.argv[1] + '.tmp', sys.argv[1])
time.sleep(60)
''', str(marker)]
            wrapper = subprocess.Popen([sys.executable, '-B', boot_observer.__file__, '--', *native],
                                       env=os.environ | {'PATH': f'{directory}:{os.environ["PATH"]}'},
                                       stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
            process = None
            try:
                deadline = time.monotonic() + 5
                while not marker.exists() and time.monotonic() < deadline:
                    time.sleep(0.01)
                self.assertTrue(marker.exists(), 'native init did not start')
                process = json.loads(marker.read_text())
                self.assertEqual(process['group'], wrapper.pid)
                wrapper.send_signal(signal.SIGTERM)
                wrapper.communicate(timeout=5)
                self.assertEqual(wrapper.returncode, 143)
                with self.assertRaises(ProcessLookupError):
                    os.kill(process['pid'], 0)
            finally:
                try:
                    os.killpg(wrapper.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                if process is not None:
                    try:
                        os.kill(process['pid'], signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                wrapper.communicate(timeout=5)

    def test_exit_reason_survives_empty_stopped_logs_without_private_or_foreign_fields(self):
        events = [
            {'type': 'lifecycle', 'project': 'foreign', 'metadata': {
                'action': 'instance-started', 'source': '/1.0/instances/yard-vpn-e2e'}},
            {'type': 'logging', 'metadata': {'message': 'Instance stopped', 'context': {
                'project': 'subyard-vpn-e2e', 'instance': 'foreign', 'reason': 'host-error'}}},
            {'type': 'logging', 'metadata': {'message': 'Instance stopped', 'context': {
                'project': 'subyard-vpn-e2e', 'instance': 'yard-vpn-e2e',
                'reason': 'guest-panic', 'target': 'stop', 'private': 'synthetic-private-value'},
                'requestor': 'synthetic-private-value', 'config': 'synthetic-private-value'}},
        ]
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            directory.chmod(0o700)
            fake = directory / 'incus.py'
            fake.write_text('''import json, os, signal, sys, time
from pathlib import Path
root = Path(__file__).parent
def write(name, value=''):
    fd = os.open(root / name, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    os.write(fd, value.encode()); os.close(fd)
if sys.argv[1] == 'monitor':
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
    for event in json.loads((root / 'events').read_text()):
        print(json.dumps(event), flush=True)
    write('monitor-pid', str(os.getpid()))
    time.sleep(60)
elif sys.argv[1] == 'query':
    print(json.dumps({'status': 'Stopped', 'pid': 0, 'memory': {'usage': 0},
                      'private': 'synthetic-private-value'}))
    if not (root / 'state-read').exists(): write('state-read')
elif sys.argv[1] == 'console':
    pass  # The post-stop console is empty, as in the observed failure.
''')
            fake.chmod(0o600)
            (directory / 'events').write_text(json.dumps(events))
            (directory / 'events').chmod(0o600)
            init = [sys.executable, '-c', '''import sys, time
from pathlib import Path
root = Path(sys.argv[1])
deadline = time.monotonic() + 5
while not ((root / 'monitor-pid').exists() and (root / 'state-read').exists()):
    if time.monotonic() > deadline: sys.exit(99)
    time.sleep(0.01)
sys.exit(17)
''', str(directory)]
            pending = [True]
            def arguments(*args):
                if args[0] == 'monitor' and pending:
                    pending.clear()
                    raise OSError('Incus not installed yet')
                return [sys.executable, str(fake), *args]
            with mock.patch.object(boot_observer, 'incus_arguments', side_effect=arguments), \
                    io.StringIO() as output, contextlib.redirect_stdout(output):
                self.assertEqual(boot_observer.run(init), 17)
                retained = output.getvalue()
            rows = [json.loads(line.split('=', 1)[1]) for line in retained.splitlines()]
            self.assertTrue(any(row.get('state') == 'Stopped' for row in rows))
            self.assertEqual(rows[-1]['shutdown_reason'], 'guest-panic')
            self.assertEqual(rows[-1]['subscription'], 'ready')
            self.assertTrue(any(row.get('gap') == 'monitor_unavailable' for row in rows))
            self.assertNotIn('synthetic-private-value', retained)
            self.assertNotIn('host-error', retained)
            self.assertFalse(any('action' in row for row in rows))
            with self.assertRaises(ProcessLookupError):
                os.kill(int((directory / 'monitor-pid').read_text()), 0)

    def test_observation_deadline_preserves_init_and_probe_timeout_reaps_child(self):
        with mock.patch.object(boot_observer, 'incus_arguments', side_effect=OSError), \
                io.StringIO() as output, contextlib.redirect_stdout(output):
            self.assertEqual(boot_observer.run([sys.executable, '-c', 'raise SystemExit(23)']), 23)
            self.assertIn('monitor_unavailable', output.getvalue())
        with io.StringIO() as output, contextlib.redirect_stdout(output):
            self.assertEqual(boot_observer.run([sys.executable, '-c', 'raise SystemExit(24)'], duration=0), 24)
            self.assertIn('deadline', output.getvalue())
            self.assertEqual(boot_observer.run([sys.executable, '-c', 'pass'], duration=0), 0)
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            directory.chmod(0o700)
            pid = directory / 'probe-pid'
            command = [sys.executable, '-c', '''import os, signal, sys, time
signal.signal(signal.SIGTERM, signal.SIG_IGN)
fd = os.open(sys.argv[1], os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
os.write(fd, str(os.getpid()).encode()); os.close(fd)
time.sleep(60)
''', str(pid)]
            with mock.patch.object(boot_observer, 'incus_arguments', return_value=command):
                self.assertIsNone(boot_observer.probe('query'))
            with self.assertRaises(ProcessLookupError):
                os.kill(int(pid.read_text()), 0)


class DNSProbeTest(unittest.TestCase):
    def test_external_dns_recovery_has_a_terminal_budget(self):
        valid = (0, 'NOERROR', '203.0.113.11')
        cases = {
            'immediate': ([valid], 0, 1),
            'transient': ([(9, '', ''), (0, 'SERVFAIL', ''), valid], 0, 3),
            'query-error': ([(9, '', '')] * 3, 1, 3),
            'servfail': ([(0, 'SERVFAIL', '')] * 3, 1, 3),
            'no-answer': ([(0, 'NOERROR', '')] * 3, 1, 3),
            'ipv6-only': ([(0, 'NOERROR', '2001:db8::1')] * 3, 1, 3),
        }
        for name, (responses, exit_code, attempts) in cases.items():
            with self.subTest(name=name), tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary)
                directory.chmod(0o700)
                (directory / 'responses').write_text(json.dumps(responses))
                dig = directory / 'dig'
                dig.write_text('''#!/usr/bin/env python3
import json
from pathlib import Path
import sys
root = Path(__file__).parent
assert sys.argv[-3:] == ['@1.1.1.1', 'example.com', 'A']
assert '+time=5' in sys.argv and '+tries=1' in sys.argv
count = root / 'count'
index = int(count.read_text()) if count.exists() else 0
count.write_text(str(index + 1))
responses = json.loads((root / 'responses').read_text())
status, rcode, address = responses[min(index, len(responses) - 1)]
print('synthetic-private-response')
print('synthetic-private-error', file=sys.stderr)
if rcode:
    print(';; ->>HEADER<<- opcode: QUERY, status: ' + rcode + ', id: 1')
if address:
    print('example.com. 60 IN A ' + address)
sys.exit(status)
''')
                dig.chmod(0o755)
                sleep = directory / 'sleep'
                sleep.write_text('#!/bin/sh\nexit 0\n')
                sleep.chmod(0o755)
                probe = subprocess.run(['sh', str(PROFILE / 'tests/e2e/dns-probe.sh')],
                                       env={'PATH': str(directory) + os.pathsep + os.environ['PATH']},
                                       capture_output=True, text=True, timeout=3)
                self.assertEqual(probe.returncode, exit_code, probe.stdout + probe.stderr)
                self.assertEqual(int((directory / 'count').read_text()), attempts)
                self.assertEqual(probe.stdout.count('amnezia_dns_probe attempt='), attempts)
                self.assertIn('query_exit=', probe.stdout)
                self.assertNotIn('synthetic-private', probe.stdout + probe.stderr)
                self.assertNotIn('203.0.113.11', probe.stdout)


if __name__ == '__main__':
    unittest.main()
