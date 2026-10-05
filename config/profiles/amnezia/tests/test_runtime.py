#!/usr/bin/env python3
"""Behavioral checks for Amnezia's owner handler and guest runtime."""
import contextlib
import copy
import hashlib
import importlib.util
import io
import json
import os
import runpy
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

    @contextlib.contextmanager
    def admin_handler(self):
        with mock.patch.object(handler, 'ADMIN', True), \
                mock.patch.object(handler, 'DEVICE', 'amnezia-admin'), \
                mock.patch.object(handler, 'KEY', 'user.subyard.resource.amnezia-admin'), \
                mock.patch.object(handler, 'STARTUP', 'user.subyard.startup.amnezia-admin'):
            yield

    def test_admin_custom_port_targets_guest_ssh_and_requires_management_ready(self):
        os.environ.update(RESOURCE_VPN_IPV4='10.20.30.40', RESOURCE_VPN_INTERFACE='eth0', RESOURCE_VPN_ADMIN_PORT='22999')
        instance = self.instance()
        links = [{'flags': ['UP'], 'addr_info': [{'local': '10.20.30.40'}]}]
        with self.admin_handler(), mock.patch.object(handler, 'run', return_value=result(json.dumps(links).encode())):
            _, _, port, device = handler.endpoint(instance)
            self.assertEqual(port, '22999')
            self.assertEqual(device['listen'], 'tcp:10.20.30.40:22999')
            self.assertEqual(device['connect'], 'tcp:10.80.0.10:22')
            instance['devices'][handler.DEVICE] = device
            instance['config'][handler.KEY] = handler.fingerprint(device)
            with mock.patch.object(handler, 'runtime_status', return_value=dict(management_ready=False, ready=True, enabled=True)):
                self.assertFalse(handler.ready(instance))
            with mock.patch.object(handler, 'runtime_status', return_value=dict(management_ready=True, ready=False, enabled=False)):
                self.assertTrue(handler.ready(instance))

    def test_stopped_admin_route_is_not_ready_and_down_never_reaches_guest(self):
        os.environ.update(SUBYARD_RESOURCE_MODE='apply', SUBYARD_RESOURCE_ACTION='down', SUBYARD_OPERATION_ID='op-test',
                          RESOURCE_VPN_IPV4='10.20.30.40', RESOURCE_VPN_INTERFACE='eth0')
        instance = self.instance()
        instance['status'] = 'Stopped'
        device = dict(type='proxy', bind='host', nat='true', listen='tcp:10.20.30.40:2226', connect='tcp:10.80.0.10:22')
        with self.admin_handler():
            instance['devices'][handler.DEVICE] = device
            instance['config'][handler.KEY] = handler.fingerprint(device)
            links = [{'flags': ['UP'], 'addr_info': [{'local': '10.20.30.40'}]}]
            with mock.patch.object(handler, 'run', return_value=result(json.dumps(links).encode())), \
                    mock.patch.object(handler, 'guest', side_effect=AssertionError('stopped admin guest accessed')):
                self.assertFalse(handler.ready(instance))
            with mock.patch.object(handler, 'inspect', return_value=instance), \
                    mock.patch.object(handler, 'guest', side_effect=AssertionError('admin down changed guest')), \
                    mock.patch.object(handler, 'remove_ingress') as remove, \
                    mock.patch.object(sys, 'argv', ['vpn-admin', 'down']):
                self.assertEqual(handler.main(), 0)
            self.assertEqual(remove.call_args_list, [mock.call(retain_pending=True), mock.call()])

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
        self.assertFalse(status['network_enabled'])
        self.assertFalse(status['vpn_installed'])
        self.assertFalse(status['vpn_running'])
        self.assertEqual(status['management'], dict(application='AmneziaVPN', host='', port=2226, user='amnezia'))
        self.assertEqual(status['vpn_port'], 51820)

    def test_stopped_runtime_status_has_management_and_native_vpn_facts(self):
        instance = self.instance()
        instance['status'] = 'Stopped'
        with mock.patch.object(handler, 'guest', side_effect=AssertionError('stopped guest accessed')):
            self.assertEqual(handler.runtime_status(instance), dict(ready=False, running=False, enabled=False,
                management_ready=False, vpn_installed=False, vpn_running=False))

    def test_admin_down_on_stopped_vm_assesses_only_owned_route(self):
        instance = self.instance()
        instance['status'] = 'Stopped'
        device = dict(type='proxy', bind='host', nat='true', listen='tcp:10.20.30.40:2226', connect='tcp:10.80.0.10:22')
        admin_key = 'user.subyard.resource.amnezia-admin'
        instance['devices']['amnezia-admin'] = device
        instance['config'][admin_key] = handler.fingerprint(device)
        with mock.patch.object(handler, 'ADMIN', True), \
                mock.patch.object(handler, 'DEVICE', 'amnezia-admin'), \
                mock.patch.object(handler, 'KEY', admin_key), \
                mock.patch.object(handler, 'inspect', return_value=instance), \
                mock.patch.object(handler, 'guest', side_effect=AssertionError('stopped guest accessed')), \
                mock.patch.object(handler, 'incus', side_effect=AssertionError('assessment changed route')), \
                contextlib.redirect_stdout(io.StringIO()) as output:
            handler.prepare('down')
        assessment = json.loads(output.getvalue())
        self.assertTrue(assessment['changed'])
        self.assertEqual([step['id'] for step in assessment['steps']], ['ingress'])

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

    def test_custom_udp_port_is_preserved_at_guest_boundary(self):
        os.environ.update(RESOURCE_VPN_IPV4='10.20.30.40', RESOURCE_VPN_INTERFACE='eth0', RESOURCE_VPN_PORT='51999')
        links = [{'flags': ['UP'], 'addr_info': [{'local': '10.20.30.40'}]}]
        with mock.patch.object(handler, 'run', return_value=result(json.dumps(links).encode())):
            _, _, port, device = handler.endpoint(self.instance())
        self.assertEqual(port, '51999')
        self.assertEqual(device['listen'], 'udp:10.20.30.40:51999')
        self.assertEqual(device['connect'], 'udp:10.80.0.10:51999')

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
        for protocol in ('tcp', 'udp'):
            mapped = dict(want, listen=protocol + ':10.20.30.40:51820')
            with mock.patch.object(handler, 'run', return_value=result(
                    b'LISTEN 0 128 [::ffff:10.20.30.40]:51820 [::]:*\n')):
                with self.assertRaisesRegex(RuntimeError, 'already bound'):
                    handler.collisions(mapped)

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
    @contextlib.contextmanager
    def root_owned_stat(self):
        original = Path.lstat
        with mock.patch.object(Path, 'lstat', lambda path: SimpleNamespace(
                st_uid=0, st_gid=0, st_mode=original(path).st_mode)), \
                mock.patch.object(runtime.os, 'fchown'):
            yield

    @contextlib.contextmanager
    def environment(self):
        with tempfile.TemporaryDirectory() as directory, self.root_owned_stat():
            state = Path(directory) / 'amnezia'
            with mock.patch.object(runtime, 'STATE', state), \
                    mock.patch.object(runtime.os.path, 'ismount', return_value=True):
                runtime.initialize()
                yield state

    def test_state_modes_types_ownership_and_idempotence(self):
        with self.environment() as state:
            before = {path.name: path.read_bytes() for path in state.iterdir()}
            runtime.initialize()
            self.assertEqual(before, {path.name: path.read_bytes() for path in state.iterdir()})
            self.assertEqual(set(before), {'environment', 'settings.json'})
            self.assertEqual(state.stat().st_mode & 0o777, 0o700)
            for path in state.iterdir():
                self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            marker = state / 'environment'
            marker.chmod(0o644)
            with self.assertRaisesRegex(RuntimeError, 'unsafe ownership or permissions'):
                runtime.initialize()
            marker.chmod(0o600)
            marker.write_text('foreign\n')
            with self.assertRaisesRegex(RuntimeError, 'ownership does not match'):
                runtime.initialize()
            marker.unlink()
            marker.symlink_to(state / 'settings.json')
            with self.assertRaises(RuntimeError):
                runtime.initialize()
        with self.environment() as state:
            state.chmod(0o755)
            with self.assertRaises(RuntimeError):
                runtime.initialize()

    def test_legacy_state_is_rejected_without_modification(self):
        with tempfile.TemporaryDirectory() as directory, self.root_owned_stat():
            state = Path(directory) / 'amnezia'
            state.mkdir(mode=0o700)
            state.chmod(0o700)
            legacy = state / 'awg0.conf'
            legacy.write_text('synthetic legacy state\n')
            with mock.patch.object(runtime, 'STATE', state), \
                    mock.patch.object(runtime.os.path, 'ismount', return_value=True):
                with self.assertRaisesRegex(RuntimeError, 'legacy or unowned'):
                    runtime.initialize()
            self.assertEqual(list(state.iterdir()), [legacy])
            self.assertEqual(legacy.read_text(), 'synthetic legacy state\n')

    def test_state_preflight_does_not_create_missing_state_or_acquire_lock(self):
        with tempfile.TemporaryDirectory() as directory, self.root_owned_stat(), \
                mock.patch.object(runtime, 'STATE', Path(directory) / 'amnezia'), \
                mock.patch.object(runtime.os.path, 'ismount', return_value=True), \
                mock.patch.object(runtime, 'open', side_effect=AssertionError('preflight wrote lock'), create=True), \
                mock.patch.object(runtime, 'run', side_effect=AssertionError('preflight ran target command')), \
                mock.patch.object(runtime.os, 'geteuid', return_value=0), \
                mock.patch.object(sys, 'argv', ['runtime', 'check-state']):
            self.assertFalse(runtime.check_state())
            runtime.main()
            self.assertEqual(list(Path(directory).iterdir()), [])
        with self.environment() as state:
            self.assertTrue(runtime.check_state())
            (state / 'environment').unlink()
            with self.assertRaisesRegex(RuntimeError, 'legacy or unowned'):
                runtime.check_state()

    def test_state_mount_required_before_mutation(self):
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory) / 'amnezia'
            with mock.patch.object(runtime, 'STATE', state), \
                    mock.patch.object(runtime.os.path, 'ismount', return_value=False), \
                    mock.patch.object(runtime, 'run') as run:
                for operation in (runtime.initialize, runtime.settings,
                                  lambda: runtime.up('10.20.30.40', 51820), runtime.down):
                    with self.assertRaisesRegex(RuntimeError, 'not mounted'):
                        operation()
                run.assert_not_called()
                self.assertFalse(state.exists())
            link = Path(directory) / 'srv-link'
            link.symlink_to(directory, target_is_directory=True)
            with mock.patch.object(runtime, 'STATE', link / 'amnezia'), \
                    mock.patch.object(runtime.os.path, 'ismount', return_value=True):
                with self.assertRaisesRegex(RuntimeError, 'not mounted'):
                    runtime.initialize()

    def test_readiness_does_not_require_native_vpn_installation(self):
        with self.environment(), mock.patch.object(runtime, 'admin_ready', return_value=True), \
                mock.patch.object(runtime, 'storage_initialized', return_value=True), \
                mock.patch.object(runtime, 'docker_dns_ready', return_value=True), \
                mock.patch.object(runtime, 'storage_policy_ready', return_value=True), \
                mock.patch.object(runtime, 'storage_bound', return_value=True), \
                mock.patch.object(runtime, 'firewall_ready', return_value=True), \
                mock.patch.object(runtime, 'native_status', return_value=dict(vpn_installed=False, vpn_running=False)):
            self.assertEqual(runtime.observe(), dict(ready=True, running=False, enabled=False,
                management_ready=True, vpn_installed=False, vpn_running=False))
            with mock.patch.object(runtime, 'storage_bound', return_value=False):
                self.assertFalse(runtime.observe()['ready'])
            with mock.patch.object(runtime, 'storage_initialized', return_value=False):
                self.assertFalse(runtime.observe()['management_ready'])
            with mock.patch.object(runtime, 'storage_policy_ready', return_value=False):
                self.assertFalse(runtime.observe()['ready'])
            with mock.patch.object(runtime, 'docker_dns_ready', return_value=False):
                self.assertFalse(runtime.observe()['ready'])
            with mock.patch.object(runtime, 'admin_ready', return_value=False):
                self.assertFalse(runtime.observe()['ready'])
            with mock.patch.object(runtime, 'firewall_ready', return_value=False):
                self.assertFalse(runtime.observe()['ready'])

    def test_native_status_checks_both_names_and_exact_configured_port(self):
        calls = []
        def command(*args, **kwargs):
            calls.append(args)
            if args[:3] == ('docker', 'container', 'inspect'):
                return result(json.dumps([{'State': {'Running': args[3] == 'amnezia-awg2'}}]).encode())
            return result(b'51999\n')
        with mock.patch.object(runtime, 'run', side_effect=command):
            self.assertEqual(runtime.native_status(51999), dict(vpn_installed=True, vpn_running=True))
            self.assertFalse(runtime.native_status(51820)['vpn_running'])
        self.assertTrue(all(args[:3] in (('docker', 'container', 'inspect'), ('docker', 'exec', 'amnezia-awg2'))
                            for args in calls))
        with mock.patch.object(runtime, 'run', return_value=result(code=1)):
            self.assertEqual(runtime.native_status(51999), dict(vpn_installed=False, vpn_running=False))

    def test_network_changes_preserve_native_state_and_never_manage_containers(self):
        with self.environment() as state:
            native = state / 'native-fixture'
            native.write_bytes(b'native application data')
            calls = []
            with mock.patch.object(runtime, 'observe', return_value={'ready': True, 'running': False, 'enabled': False}), \
                    mock.patch.object(runtime, 'run', side_effect=lambda *args, **kwargs: (calls.append(args), result())[1]):
                runtime.up('10.20.30.40', 51999)
                runtime.up('10.20.30.40', 51999)
                self.assertEqual(runtime.settings(), dict(enabled=True, endpoint='10.20.30.40', port=51999))
                runtime.down()
                self.assertEqual(runtime.settings(), dict(enabled=False, endpoint='10.20.30.40', port=51999))
            self.assertEqual(native.read_bytes(), b'native application data')
            self.assertTrue(all(args == ('systemctl', 'restart', runtime.UNIT) for args in calls))
            self.assertFalse((state / 'client.conf').exists())
            self.assertFalse((state / 'awg0.conf').exists())

    def test_firewall_boundary_closes_exact_port_and_denies_management_networks(self):
        with self.environment() as state, mock.patch.object(runtime, 'uplink', return_value='eth0'):
            runtime.atomic(state / 'settings.json', json.dumps(dict(enabled=True, endpoint='10.20.30.40', port=51999)))
            policy = runtime.firewall_policy()
            self.assertIn('iifname "lo" accept\n  iifname "eth0" accept\n  drop', policy)
            self.assertIn('iifname != "eth0" oifname "eth0" meta nfproto ipv6 drop', policy)
            self.assertIn('iifname != "eth0" oifname "eth0" ip daddr', policy)
            self.assertNotIn('docker0', policy)
            self.assertNotIn('amn0', policy)
            self.assertNotIn('dport 53 accept', policy)
            self.assertIn('10.20.30.40/32', policy)
            self.assertIn('169.254.0.0/16', policy)
            self.assertIn('udp sport 51999 ct direction reply', policy)
            self.assertLess(policy.index('meta nfproto ipv6 drop'), policy.index('udp sport 51999 ct direction reply'))
            self.assertNotIn('udp dport 51999 drop', policy)
            runtime.atomic(state / 'settings.json', json.dumps(dict(enabled=False, endpoint='10.20.30.40', port=51999)))
            self.assertIn('udp dport 51999 drop', runtime.firewall_policy())
            self.assertNotIn('ct direction reply', runtime.firewall_policy())

    def test_uplink_discovery_requires_one_safe_active_interface(self):
        with mock.patch.object(runtime, 'run', side_effect=[
                result(b'[{"dev":"eth0"}]'), result(b'[{"ifname":"eth0","flags":["UP"]}]')]) as run:
            self.assertEqual(runtime.uplink(), 'eth0')
        self.assertEqual(run.call_args_list, [mock.call('ip', '-4', '-j', 'route', 'get', '1.1.1.1'),
                                             mock.call('ip', '-j', 'link', 'show', 'dev', 'eth0')])
        for routes in (None, {}, [None], [], [{'dev': 'eth0'}, {'dev': 'eth1'}], [{'dev': 'lo'}],
                       [{'dev': 'bad"name'}], [{'dev': 'bridge@eth0'}], [{}]):
            with self.subTest(routes=routes), mock.patch.object(runtime, 'run', return_value=result(json.dumps(routes).encode())):
                with self.assertRaises(RuntimeError):
                    runtime.uplink()
        for links in (None, {}, [None], [], [{'ifname': 'eth1', 'flags': ['UP']}],
                      [{'ifname': 'eth0', 'flags': 'UP'}], [{'ifname': 'eth0', 'flags': []}]):
            with self.subTest(links=links), mock.patch.object(runtime, 'run', side_effect=[
                    result(b'[{"dev":"eth0"}]'), result(json.dumps(links).encode())]):
                with self.assertRaisesRegex(RuntimeError, 'not active'):
                    runtime.uplink()

    def test_public_docker_dns_preserves_settings_and_repeated_preparation_is_noop(self):
        with self.environment() as state, mock.patch.object(runtime, 'DOCKER_CONFIG', state / 'docker-config/daemon.json'):
            runtime.DOCKER_CONFIG.parent.mkdir()
            original = {'dns': ['10.0.0.1'], 'log-driver': 'local', 'features': {'containerd-snapshotter': True}}
            runtime.DOCKER_CONFIG.write_text(json.dumps(original))
            runtime.DOCKER_CONFIG.chmod(0o600)
            self.assertFalse(runtime.docker_dns_ready())
            with mock.patch.object(runtime.os, 'fchown') as ownership:
                self.assertTrue(runtime.prepare_docker_dns())
                ownership.assert_called_once()
                self.assertEqual(ownership.call_args.args[1:], (0, 0))
            self.assertEqual(runtime.DOCKER_CONFIG.stat().st_mode & 0o777, 0o600)
            self.assertEqual(json.loads(runtime.DOCKER_CONFIG.read_text()), original | {'dns': runtime.PUBLIC_DNS})
            self.assertTrue(runtime.docker_dns_ready())
            with mock.patch.object(runtime, 'atomic', side_effect=AssertionError('unchanged DNS was rewritten')):
                self.assertFalse(runtime.prepare_docker_dns())

    def test_docker_dns_rejects_unsafe_files_and_malformed_configuration(self):
        with self.environment() as state, mock.patch.object(runtime, 'DOCKER_CONFIG', state / 'daemon.json'):
            config = runtime.DOCKER_CONFIG
            for content in ('{broken', '[]', 'null'):
                config.write_text(content)
                config.chmod(0o644)
                self.assertFalse(runtime.docker_dns_ready())
                with self.assertRaises((RuntimeError, ValueError)):
                    runtime.prepare_docker_dns()
                self.assertEqual(config.read_text(), content)
            config.write_text('{}')
            config.chmod(0o644)
            for uid, gid in ((1000, 0), (0, 1000)):
                with mock.patch.object(Path, 'lstat', return_value=SimpleNamespace(
                        st_uid=uid, st_gid=gid, st_mode=config.stat().st_mode)):
                    self.assertFalse(runtime.docker_dns_ready())
                    with self.assertRaisesRegex(RuntimeError, 'unsafe ownership'):
                        runtime.prepare_docker_dns()
            config.chmod(0o666)
            with self.assertRaisesRegex(RuntimeError, 'unsafe ownership or permissions'):
                runtime.prepare_docker_dns()
            config.unlink()
            config.symlink_to(state / 'environment')
            self.assertFalse(runtime.docker_dns_ready())
            with self.assertRaises(RuntimeError):
                runtime.prepare_docker_dns()
            self.assertEqual((state / 'environment').read_text(), runtime.MARKER + '\n')

    def test_provision_passes_dns_change_into_single_storage_restart(self):
        for changed in (False, True):
            with self.subTest(changed=changed), mock.patch.object(runtime, 'initialize'), \
                    mock.patch.object(runtime, 'prepare_docker_dns', return_value=changed), \
                    mock.patch.object(runtime, 'prepare_storage') as storage, \
                    mock.patch.object(runtime, 'prepare_admin'), mock.patch.object(runtime, 'run'), \
                    mock.patch.object(runtime, 'observe', return_value={'ready': True}):
                runtime.provision()
            storage.assert_called_once_with(force_restart=changed)

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

    def test_persistent_storage_seed_and_mounts_are_idempotent(self):
        with self.environment() as state:
            etc = state / 'systemd'
            etc.mkdir()
            sources = state / 'sources'
            for name in ('docker', 'containerd'):
                (state / name).mkdir(mode=0o700)
                (sources / name).mkdir(parents=True)
                (sources / name / 'native-data').write_text(name)
            original_path = runtime.Path
            def isolated_path(value):
                return {'/var/lib': sources, '/etc/systemd/system': etc}.get(str(value), original_path(value))
            calls = []
            foreign_ownership = set()
            def command(*args, data=None, **kwargs):
                calls.append(args)
                if args[0] == 'install':
                    Path(args[-1]).write_bytes(data)
                    Path(args[-1]).chmod(0o644)
                    foreign_ownership.discard(Path(args[-1]))
                return result()
            with mock.patch.object(runtime, 'Path', side_effect=isolated_path), \
                    mock.patch.object(runtime, 'storage_bound', return_value=True), \
                    mock.patch.object(runtime, 'run', side_effect=command):
                runtime.prepare_storage()
                first_calls = len(calls)
                runtime.prepare_storage()
                self.assertEqual(len(calls), first_calls)
                runtime.prepare_storage(force_restart=True)
                self.assertEqual(calls[first_calls:], [('systemctl', 'daemon-reload'),
                    ('systemctl', 'restart', 'containerd.service', 'docker.service')])
                for name in ('docker', 'containerd'):
                    self.assertEqual((state / name / 'native-data').read_text(), name)
                    dropin = (etc / (name + '.service.d') / '90-subyard-amnezia-storage.conf').read_text()
                    self.assertIn('AssertPathIsMountPoint=/srv', dropin)
                    self.assertIn(f'BindPaths={state / name}:/var/lib/{name}', dropin)
                    path, content = runtime.storage_policy(name)
                    self.assertTrue(runtime.storage_policy_ready(name))
                    path.write_text(content + '# drift\n')
                    self.assertFalse(runtime.storage_policy_ready(name))
                    runtime.prepare_storage()
                    self.assertEqual(path.read_text(), content)
                    path.chmod(0o600)
                    self.assertFalse(runtime.storage_policy_ready(name))
                    runtime.prepare_storage()
                    self.assertEqual(path.stat().st_mode & 0o777, 0o644)
                    original_lstat = Path.lstat
                    foreign_ownership.add(path)
                    def foreign_owner(candidate):
                        info = original_lstat(candidate)
                        if candidate in foreign_ownership:
                            return SimpleNamespace(st_uid=1000, st_gid=1000, st_mode=info.st_mode)
                        return info
                    with mock.patch.object(Path, 'lstat', side_effect=foreign_owner, autospec=True):
                        self.assertFalse(runtime.storage_policy_ready(name))
                        runtime.prepare_storage()
                        self.assertTrue(runtime.storage_policy_ready(name))
                    path.unlink()
                    path.symlink_to(state / 'environment')
                    self.assertFalse(runtime.storage_policy_ready(name))
                    with self.assertRaisesRegex(RuntimeError, 'must not be a symlink'):
                        runtime.prepare_storage()
                    path.unlink()
                    path.write_text(content)
                    path.chmod(0o644)
                with mock.patch.object(runtime, 'storage_bound', return_value=False):
                    with self.assertRaisesRegex(RuntimeError, 'not bound'):
                        runtime.prepare_storage()

    def test_lost_storage_marker_rejects_existing_data_before_commands_or_copy(self):
        with self.environment() as state:
            marker = state / 'storage-seeded'
            runtime.atomic(marker, runtime.MARKER + '\n')
            self.assertTrue(runtime.storage_initialized())
            marker.chmod(0o644)
            self.assertFalse(runtime.storage_initialized())
            marker.chmod(0o600)
            marker.write_text('foreign ownership\n')
            self.assertFalse(runtime.storage_initialized())
            marker.unlink()
            native = state / 'containerd/native-database'
            native.parent.mkdir()
            native.write_bytes(b'synthetic native data')
            with mock.patch.object(runtime, 'run', side_effect=AssertionError('lost marker changed services')), \
                    mock.patch.object(runtime.shutil, 'copytree', side_effect=AssertionError('lost marker merged data')):
                with self.assertRaisesRegex(RuntimeError, 'data but no ownership marker'):
                    runtime.prepare_storage()
            self.assertEqual(native.read_bytes(), b'synthetic native data')
            self.assertFalse(marker.exists())
            self.assertFalse(runtime.storage_initialized())
            with mock.patch.object(runtime, 'admin_ready', return_value=True), \
                    mock.patch.object(runtime, 'docker_dns_ready', return_value=True), \
                    mock.patch.object(runtime, 'storage_policy_ready', return_value=True), \
                    mock.patch.object(runtime, 'storage_bound', return_value=True), \
                    mock.patch.object(runtime, 'firewall_ready', return_value=True), \
                    mock.patch.object(runtime, 'native_status', return_value=dict(vpn_installed=True, vpn_running=True)):
                self.assertFalse(runtime.observe()['management_ready'])
                self.assertFalse(runtime.observe()['ready'])
            marker.symlink_to(state / 'missing-marker')
            self.assertFalse(runtime.storage_initialized())
            with mock.patch.object(runtime, 'run', side_effect=AssertionError('broken marker changed services')):
                with self.assertRaises(RuntimeError):
                    runtime.prepare_storage()
            self.assertEqual(native.read_bytes(), b'synthetic native data')

    def test_unseeded_storage_rejects_symlinks_and_non_directories_without_commands(self):
        for kind in ('file', 'symlink'):
            with self.subTest(kind=kind), self.environment() as state:
                destination = state / 'docker'
                if kind == 'file':
                    destination.write_bytes(b'native data')
                else:
                    destination.symlink_to(state / 'missing')
                with mock.patch.object(runtime, 'run', side_effect=AssertionError('unsafe storage changed services')):
                    with self.assertRaisesRegex(RuntimeError, 'unsafe file type'):
                        runtime.prepare_storage()

    def test_systemd_start_installs_boundary_without_reacquiring_lifecycle_lock(self):
        with self.environment(), mock.patch.object(runtime.os, 'geteuid', return_value=0), \
                mock.patch.object(runtime, 'open', side_effect=AssertionError('start acquired parent lifecycle lock'), create=True), \
                mock.patch.object(runtime.fcntl, 'flock', side_effect=AssertionError('start acquired parent lifecycle lock')), \
                mock.patch.object(runtime, 'firewall') as firewall, \
                mock.patch.object(sys, 'argv', ['runtime', 'start']):
            runtime.main()
        firewall.assert_called_once_with()

    def test_runtime_errors_preserve_bounded_diagnostics_without_raw_exception_contents(self):
        for error, message in ((RuntimeError('environment command failed: nft'), 'amnezia: environment command failed: nft'),
                               (OSError('synthetic-private-value'), 'amnezia: environment operation failed'),
                               (ValueError('synthetic-private-value'), 'amnezia: environment operation failed'),
                               (KeyError('synthetic-private-value'), 'amnezia: environment operation failed')):
            with self.subTest(error=type(error).__name__), \
                    mock.patch.object(runtime.os, 'geteuid', return_value=0), \
                    mock.patch.object(subprocess, 'run', side_effect=error), \
                    mock.patch.object(sys, 'argv', ['runtime', 'start']), \
                    contextlib.redirect_stderr(io.StringIO()) as output:
                with self.assertRaises(SystemExit) as exit_status:
                    runpy.run_path(str(PROFILE / 'runtime.py'), run_name='__main__')
            self.assertEqual(exit_status.exception.code, 1)
            self.assertEqual(output.getvalue().strip(), message)

    def test_admin_preparation_reuses_key_and_restricts_authentication(self):
        with self.environment() as state:
            key = state / 'admin.key'
            key.write_text('synthetic fixture\n')
            key.chmod(0o600)
            home = state / 'homes'
            sudoers = state / 'sudoers'
            policy = state / 'sshd.conf'
            original_path = runtime.Path
            def isolated_path(value):
                return {'/home': home, '/etc/sudoers.d/91-subyard-amnezia-admin': sudoers,
                        '/etc/ssh/sshd_config.d/01-subyard-amnezia-admin.conf': policy}.get(str(value), original_path(value))
            calls = []
            def command(*args, data=None, **kwargs):
                calls.append(args)
                if args[0] == 'install':
                    if '-d' in args:
                        Path(args[-1]).mkdir(parents=True, exist_ok=True)
                    else:
                        Path(args[-1]).write_bytes(data)
                return result(b'ssh-ed25519 synthetic\n')
            with mock.patch.object(runtime, 'Path', side_effect=isolated_path), \
                    mock.patch.object(runtime, 'run', side_effect=command), \
                    mock.patch.object(runtime, 'admin_ready', return_value=True):
                runtime.prepare_admin()
            self.assertEqual(key.read_text(), 'synthetic fixture\n')
            self.assertFalse(any(args[:2] == ('ssh-keygen', '-q') for args in calls))
            self.assertEqual((home / runtime.ADMIN / '.ssh/authorized_keys').read_text(), 'restrict ssh-ed25519 synthetic\n')
            self.assertIn('AuthenticationMethods publickey', policy.read_text())
            self.assertIn('PasswordAuthentication no', policy.read_text())
            self.assertIn('DisableForwarding yes', policy.read_text())
            self.assertIn(('visudo', '-cf', str(sudoers)), calls)
            self.assertTrue(any(args[:8] == ('install', '-o', runtime.ADMIN, '-g', runtime.ADMIN, '-m', '0600', '/dev/stdin') for args in calls))
            self.assertTrue(any(args[:8] == ('install', '-o', '0', '-g', '0', '-m', '0440', '/dev/stdin') for args in calls))

    def test_admin_key_permissions_and_authorization_symlink_rejected(self):
        with self.environment() as state:
            key = state / 'admin.key'
            key.write_text('synthetic fixture\n')
            key.chmod(0o644)
            with self.assertRaisesRegex(RuntimeError, 'unsafe ownership or permissions'):
                runtime.prepare_admin()
            key.chmod(0o600)
            home = state / 'home'
            home.mkdir()
            (home / '.ssh').symlink_to(state, target_is_directory=True)
            original_path = runtime.Path
            def isolated_path(value):
                return home.parent if str(value) == '/home' else original_path(value)
            with mock.patch.object(runtime, 'ADMIN', 'home'), \
                    mock.patch.object(runtime, 'Path', side_effect=isolated_path), \
                    mock.patch.object(runtime, 'run', return_value=result(b'ssh-ed25519 synthetic\n')):
                with self.assertRaisesRegex(RuntimeError, 'home must not be a symlink'):
                    runtime.prepare_admin()


class ProfileProvisionTest(unittest.TestCase):
    def test_check_detects_modes_and_owner_and_apply_repairs_modes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / 'profile'
            source.mkdir()
            destination = root / 'installed'
            unit = root / 'subyard-amnezia.service'
            for name in ('runtime.py', 'subyard-amnezia.service'):
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
                'uname': '#!/bin/sh\necho x86_64\n',
                'python3': '#!/bin/sh\n[ "$1" != -c ] || { cat >/dev/null; exit; }\ncase "$2" in observe) echo \'{"ready":true}\' ;; check-state) [ "${PROFILE_LEGACY:-}" != yes ] || { echo "legacy or unowned VPN state" >&2; exit 1; } ;; provision) : ;; *) exit 2 ;; esac\n',
                'apt-get': '#!/bin/sh\n[ -z "${PROFILE_MUTATION_LOG:-}" ] || echo apt >> "$PROFILE_MUTATION_LOG"\nexit 0\n',
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
                               (unit, 0o666)):
                with self.subTest(path=path):
                    path.chmod(mode)
                    self.assertEqual(invoke('--check').returncode, 10)
                    self.assertEqual(invoke().returncode, 0)
                    self.assertEqual(invoke('--check').returncode, 0)
            self.assertEqual(invoke('--check', environment_override={'PROFILE_FAKE_UID': '1000'}).returncode, 10)
            (destination / 'runtime.py').write_bytes(b'synthetic legacy runtime\n')
            unit.write_bytes(b'synthetic legacy unit\n')
            before = ((destination / 'runtime.py').read_bytes(), unit.read_bytes())
            mutation_log = root / 'mutations'
            rejected = invoke(environment_override={'PROFILE_LEGACY': 'yes', 'PROFILE_MUTATION_LOG': str(mutation_log)})
            self.assertNotEqual(rejected.returncode, 0)
            self.assertIn('legacy or unowned', rejected.stderr)
            self.assertEqual(((destination / 'runtime.py').read_bytes(), unit.read_bytes()), before)
            self.assertFalse(mutation_log.exists())
            self.assertEqual(invoke('--check', environment_override={'PROFILE_LEGACY': 'yes'}).returncode, 10)


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
