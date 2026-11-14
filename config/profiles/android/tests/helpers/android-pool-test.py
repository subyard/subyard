#!/usr/bin/env python3
"""Host-free lease conformance at the real profile owner/client boundary."""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import secrets
import signal
import socket
import socketserver
import subprocess
import sys
import tempfile
import threading
import time
from types import SimpleNamespace
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[5]

def load(name):
    spec = importlib.util.spec_from_file_location(name, ROOT / 'config/profiles/android' / (name + '.py'))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module

poolmod, client = load('pool'), load('client')
lifecycle = load('tests/e2e/android-pool-lifecycle')


class ClientBridge(unittest.TestCase):
    def test_native_acquire_registers_managed_context_over_existing_owner_route(self):
        with tempfile.TemporaryDirectory(prefix='android-managed-correlation-', dir='/tmp') as name:
            data = Path(name) / 'telemetry'
            owner = dict(yard='fixture-owner', project='fixture-project', run='native-acquire-run', purpose='fixture')
            allocation = dict(state='held', slot_id='slot-001', generation=7)
            requests = []
            callbacks = []
            original_run = subprocess.run

            def rpc(operation, **values):
                requests.append(dict(operation=operation, **values))
                return allocation if operation == 'allocation' else None

            def callback(command, **values):
                callbacks.append(json.loads(values['input']))
                self.assertEqual(values['timeout'], .5)
                return original_run([*command[:-1], str(ROOT / 'config/profiles/telemetry/broker-correlation.py')], **values)

            args = SimpleNamespace(device=None, api=36, variant=None, abi=None, wait=0)
            with patch.dict(os.environ, SUBYARD_AGENT_RUN_ID='managed-native-run', SUBYARD_TELEMETRY_DATA_DIR=str(data),
                            SUBYARD_RESOURCE_SESSION_TRANSPORT='', SUBYARD_EMU_INSTANCE='fixture-instance',
                            SUBYARD_EMU_PROJECT='fixture-incus-project'), \
                    patch.object(client.Path, 'is_file', return_value=True), \
                    patch.object(client, 'attribution', return_value=owner), patch.object(client, 'rpc', side_effect=rpc), \
                    patch.object(client.subprocess, 'run', side_effect=callback):
                self.assertEqual(client.acquire(args, 'a' * 64), allocation)
                self.assertEqual([body['phase'] for body in callbacks], ['request', 'held'])
                intent = json.loads(next((data / 'sources/intents').glob('intent-*.json')).read_text())
                self.assertEqual(intent['agent_run_id'], 'managed-native-run')
                self.assertEqual(intent['request']['correlation_id'], owner['run'])
                self.assertEqual(intent['request']['transport']['instance'], 'fixture-instance')
                self.assertEqual(intent['request']['transport']['project'], 'fixture-incus-project')
                self.assertEqual(intent['request']['resource_generation'], 7)
                with patch.object(client.subprocess, 'run', side_effect=OSError('observer unavailable')):
                    self.assertEqual(client.acquire(args, 'a' * 64), allocation)
            expected = [dict(operation='reserve', token='a' * 64, request=dict(api=36), owner=owner),
                        dict(operation='allocation', token='a' * 64)]
            self.assertEqual(requests, expected * 2)  # Native wire has no managed-context fields.

    def test_attribution_skips_unreadable_directories_but_refuses_invalid_metadata(self):
        args = SimpleNamespace(project=None, yard=None, purpose='fixture')
        previous = os.getcwd()
        with tempfile.TemporaryDirectory(prefix='android-attribution-', dir='/tmp') as name:
            root = Path(name)
            hidden = root / 'hidden'
            hidden.mkdir()
            invalid = root / 'workspace'
            invalid.mkdir()
            (invalid / '.subyard-meta.json').write_text('{"schema": 2}')
            try:
                # Like /root after runuser: the caller may stand in a directory it cannot search.
                os.chdir(hidden)
                hidden.chmod(0)
                if os.geteuid() != 0:
                    with self.assertRaises(PermissionError):
                        (hidden / '.subyard-meta.json').is_file()
                with patch.dict(os.environ, SUBYARD_PROJECT_ID='', SUBYARD_YARD_NAME='fixture-yard'):
                    owner = client.attribution(args)
                self.assertEqual((owner['project'], owner['yard']), ('hidden', 'fixture-yard'))
                # A present but invalid workspace record is still refused.
                hidden.chmod(0o700)
                os.chdir(invalid)
                with self.assertRaisesRegex(client.Error, 'attribution metadata is invalid'):
                    client.attribution(args)
            finally:
                os.chdir(previous)
                hidden.chmod(0o700)

    def test_coalesced_process_ack_and_payload_are_delivered_before_eof(self):
        payload = b'\x00synthetic-stream-payload\xff'
        output = b'{"ok":true,"result":{}}\n' + payload
        command = 'import sys; sys.stdout.buffer.write(' + repr(output) + '); sys.stdout.buffer.flush()'
        channel = subprocess.Popen([sys.executable, '-I', '-B', '-c', command],
                                   stdin=subprocess.PIPE, stdout=subprocess.PIPE)
        local, peer = socket.socketpair()
        peer.settimeout(3)
        try:
            channel.wait(timeout=5)
            self.assertEqual(client.response(channel), {})
            self.assertEqual(channel.stdout.peek(1), payload)
            client.bridge(local, channel)
            received = bytearray()
            while chunk := peer.recv(65536):
                received.extend(chunk)
            self.assertEqual(bytes(received), payload)
            self.assertTrue(channel.stdin.closed and channel.stdout.closed)
            self.assertEqual(channel.returncode, 0)
        finally:
            local.close()
            peer.close()
            if not channel.stdout.closed:
                client.close(channel)

    def test_coalesced_socket_ack_and_payload_are_delivered_before_eof(self):
        payload = b'\x00synthetic-stream-payload\xff'
        channel, remote = socket.socketpair()
        local, peer = socket.socketpair()
        peer.settimeout(3)
        try:
            remote.sendall(b'{"ok":true,"result":{}}\n' + payload)
            remote.shutdown(socket.SHUT_WR)
            self.assertEqual(client.response(channel), {})
            client.bridge(local, channel)
            received = bytearray()
            while chunk := peer.recv(65536):
                received.extend(chunk)
            self.assertEqual(bytes(received), payload)
            self.assertEqual(channel.fileno(), -1)
        finally:
            for stream in (channel, remote, local, peer):
                stream.close()


class LifecycleObservation(unittest.TestCase):
    def test_http_response_before_completion_timeout_remains_failed_and_private(self):
        now, limits = 0, []
        def stalled(*args, **kwargs):
            nonlocal now
            limits.append(kwargs['timeout'])
            now += kwargs['timeout']
            raise subprocess.TimeoutExpired(['private-command'], kwargs['timeout'],
                output=b'HTTP/1.0 204 No Content\r\nPrivate: private-output\r\n',
                stderr=b'private-stderr')
        lease = dict(endpoint='/synthetic-adb', allocation=dict(android_serial='emulator-5554'))
        observed = io.StringIO()
        with patch.object(lifecycle.subprocess, 'run', side_effect=stalled), \
                patch.object(lifecycle.time, 'monotonic', side_effect=lambda: now), \
                patch.object(lifecycle.time, 'sleep'), contextlib.redirect_stdout(observed):
            with self.assertRaises(lifecycle.Failure) as raised:
                lifecycle.require_network_and_renderer(lease)
        self.assertIn('first=completion_timeout(http=204)', str(raised.exception))
        self.assertIn('last=completion_timeout(http=204)', str(raised.exception))
        self.assertEqual(sum(limits[:-1]), 90)
        self.assertEqual(limits[-1], 5, 'failure-only observation has its own five-second bound')
        self.assertIn('adb=timeout', observed.getvalue())
        self.assertNotIn('private-', observed.getvalue() + str(raised.exception))

    def test_slow_power_sample_recovers_without_accepting_awake_device(self):
        with patch.object(lifecycle, 'adb', side_effect=[
            '', lifecycle.CommandTimeout('slow'), 'mWakefulness=Awake', 'mWakefulness=Asleep'
        ]), patch.object(lifecycle.time, 'sleep'), contextlib.redirect_stdout(io.StringIO()):
            lifecycle.idle_display({})

        with patch.object(lifecycle, 'adb', side_effect=[
            '', lifecycle.Failure('device unavailable')
        ]), contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaisesRegex(lifecycle.Failure, 'device unavailable'):
                lifecycle.idle_display({})

    def test_power_observation_deadline_remains_bounded(self):
        now = 0
        def sample(*args, **kwargs):
            nonlocal now
            if args[2] == 'input':
                return ''
            now += kwargs['timeout']
            raise lifecycle.CommandTimeout('slow')
        with patch.object(lifecycle, 'adb', side_effect=sample), \
                patch.object(lifecycle.time, 'monotonic', side_effect=lambda: now), \
                patch.object(lifecycle.time, 'sleep'), contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaisesRegex(lifecycle.Failure, 'within 20 seconds'):
                lifecycle.idle_display({})
        self.assertEqual(now, 20)

    def test_command_failure_reports_no_arguments(self):
        with patch.object(lifecycle.subprocess, 'run', side_effect=subprocess.TimeoutExpired(['private-argument'], 5)):
            with self.assertRaises(lifecycle.CommandTimeout) as raised:
                lifecycle.call(['private-argument'], timeout=5)
        self.assertNotIn('private-argument', str(raised.exception))
        with patch.object(lifecycle.subprocess, 'run', side_effect=FileNotFoundError('private-argument')):
            with self.assertRaisesRegex(lifecycle.Failure, '^command unavailable$'):
                lifecycle.call(['private-argument'])

    def test_l2_requester_expiry_does_not_query_host_systemd(self):
        now, killed = [0], [False]
        process = SimpleNamespace(stderr=io.BytesIO(), poll=lambda: 0 if killed[0] else None,
                                  kill=lambda: killed.__setitem__(0, True), wait=lambda **kwargs: 0)
        def launch(arguments, **kwargs):
            marker = lifecycle.re.search(r"pathlib.Path\('([^']+)'\)", arguments[-1])[1]
            Path(marker).write_text(json.dumps(dict(pid=12345, endpoint='private-endpoint',
                                                   slot='001', generation=2)))
            return process
        def run(arguments, **kwargs):
            if arguments[0] == 'systemctl':
                raise FileNotFoundError('host systemctl is unavailable in L2')
            if arguments[1] == 'release':
                return SimpleNamespace(returncode=0)
            self.assertEqual(arguments[1], 'status')
            heartbeat = 300 if now[0] >= 300 else 0
            stamp = lambda seconds: time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime(seconds))
            slot = dict(slot_id='001', generation=2, owner=dict(purpose='android-memory-lease'),
                        state='available' if killed[0] and now[0] >= 1500 else 'held',
                        last_heartbeat_at=stamp(heartbeat), expires_at=stamp(heartbeat + 1200))
            memory = dict(visible_available_bytes=1000, physical_available_bytes=800,
                          cgroup_headroom_bytes=None, available_bytes=800, reserve_bytes=100,
                          pending_bytes=0, admission_headroom_bytes=700)
            return SimpleNamespace(returncode=0, stdout=json.dumps(dict(slots=[slot], memory=memory,
                                  heartbeat_seconds=300, ttl_seconds=1200)))
        fresh = dict(allocation=dict(slot_id='001', generation=3))
        with patch.object(lifecycle.subprocess, 'Popen', side_effect=launch), \
                patch.object(lifecycle.subprocess, 'run', side_effect=run), \
                patch.object(lifecycle.time, 'monotonic', side_effect=lambda: now[0]), \
                patch.object(lifecycle.time, 'time', side_effect=lambda: now[0]), \
                patch.object(lifecycle.time, 'sleep', side_effect=lambda seconds: now.__setitem__(0, now[0] + seconds)), \
                patch.object(lifecycle, 'adb', return_value=''), \
                patch.object(lifecycle, 'acquire', side_effect=lambda directory, name: (directory / 'fresh.json', fresh)), \
                patch.object(lifecycle, 'require_adb_unusable') as stale, \
                patch.object(lifecycle, 'require_sdk') as sdk, \
                patch.object(lifecycle.os, 'killpg'), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(lifecycle.memory_lease(), 0)
        self.assertTrue(killed[0])
        self.assertEqual(now[0], 1500)
        stale.assert_called_once()
        sdk.assert_called_once_with(fresh, 36)


class Runtime:
    def __init__(self):
        self.running = set()
        self.fail_stop = False
        self.fail_start = False
        self.block = None
        self.stop_block = None
        self.started = threading.Event()
        self.stop_started = threading.Event()
        self.stopped = []

    def preflight(self, request, reserved, slot):
        pass

    def check_capacity(self, request, reserved):
        pass

    def memory_capacity(self):
        raise poolmod.PoolError('memory_source', 'fixture has no physical memory evidence')

    def broker_memory(self):
        raise poolmod.PoolError('memory_source', 'fixture has no broker memory evidence')

    def start(self, slot, image, cancel):
        self.running.add((slot['slot_id'], slot['generation']))
        self.started.set()
        if self.block:
            while not self.block.wait(0.01):
                if cancel.is_set():
                    raise poolmod.PoolError('cancelled', 'cancelled')
        if self.fail_start:
            raise poolmod.PoolError('boot', 'boot failed')

    def stop(self, slot):
        self.stop_started.set()
        if self.stop_block:
            self.stop_block.wait(3)
        if self.fail_stop:
            raise OSError('stop failed')
        key = (slot['slot_id'], slot['generation'])
        self.running.discard(key)
        self.stopped.append(key)

    def connect(self, slot):
        return subprocess.Popen([sys.executable, '-I', '-B', '-c', FAKE_ADB], stdin=subprocess.PIPE, stdout=subprocess.PIPE)


# A private ADB server stand-in: answers the relay preflight, otherwise echoes raw bytes.
FAKE_ADB = '''
import os
expected = b'000chost:version'
data = b''
while len(data) < len(expected) and expected.startswith(data):
    chunk = os.read(0, 65536)
    if not chunk:
        break
    data += chunk
if data == expected:
    os.write(1, b'OKAY00040029')
    raise SystemExit(0)
while data:
    os.write(1, data)
    data = os.read(0, 65536)
'''


class Images:
    def __init__(self):
        self.lock = threading.RLock()
        self.fail = False
        self.pins = set()

    def resolve(self, request):
        return dict(key=str(request['api']), revision='1.0.0', package='image')

    def ensure(self, image, cancel):
        if self.fail:
            raise poolmod.PoolError('download', 'failed')
        return '/unused'

    def prune(self, pins, dry_run):
        self.pins = pins
        return dict(dry_run=dry_run, skipped=sorted(pins))


class Leases(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='android-pool-test-')
        self.root = Path(self.temp.name)
        self.now = 1000
        self.runtime, self.images = Runtime(), Images()
        self.config = dict(state_root=str(self.root / 'state'), sdk_root='/unused', size=2,
                           sdk_lock=str(self.root / 'run/locks/sdk.lock'))
        self.pool = poolmod.Pool(self.config, self.runtime, self.images, lambda: self.now)
        self.owner = dict(yard='yard', project='project', run='run', purpose='tests')

    def tearDown(self):
        self.runtime.fail_stop = False
        self.pool.shutdown()
        self.temp.cleanup()

    def acquire(self, request=None):
        token = secrets.token_hex(32)
        self.pool.reserve(request or {}, token, self.owner)
        deadline = time.monotonic() + 3
        while time.monotonic() < deadline:
            allocation = self.pool.allocation(token)
            if allocation['state'] == 'held' and not self.pool.workers:
                return token, allocation
            time.sleep(0.01)
        self.fail('provisioning did not complete')

    def test_observer_cursor_survives_release_and_never_authorizes_mutation(self):
        def sample(slot):
            return dict(slot_id=slot['slot_id'], resource_generation=slot['generation'],
                        selector='emulator', state=slot['state'], cpu_ns=123,
                        memory_bytes=456, missing=['io_read_bytes'], final_complete=False)
        self.runtime.observe = sample
        token, allocation = self.acquire()
        capability = self.pool.observer_capability(token)
        before = self.pool.state_path.read_bytes()
        page = self.pool.observe(capability, '-', 1)
        self.assertEqual(len(page['events']), 1)
        self.assertTrue(page['more'])
        self.assertEqual(page['observations'][0]['cpu_ns'], 123)
        self.assertEqual(self.pool.state_path.read_bytes(), before)
        again = self.pool.observe(capability, '-', 1)
        self.assertEqual(page['events'], again['events'])
        self.assertEqual(page['next_cursor'], again['next_cursor'])
        self.assertEqual(self.pool.observe('c' * 64)['events'], [])
        self.pool.release(capability)  # Unknown cancellation is idempotent; it cannot stop this lease.
        self.assertEqual(self.pool.allocation(token)['state'], 'held')
        self.pool.release(token)
        page = self.pool.observe(capability)
        self.assertEqual(page['observations'], [])
        phases = [event['phase'] for event in page['events']]
        self.assertIn('baseline', phases)
        self.assertIn('pre_stop', phases)
        self.assertIn('stopped', phases)
        stopped = next(event for event in page['events'] if event['phase'] == 'stopped')
        self.assertEqual(stopped['allocation']['owner'], self.owner)
        self.assertEqual(stopped['allocation']['state'], 'available')
        old_files = sorted((self.pool.root / 'observations').glob('*/*.json'))
        old_files[0].unlink()
        self.assertTrue(self.pool.observe(capability, again['next_cursor'])['gap'])
        token2, successor = self.acquire()
        self.assertGreater(successor['generation'], allocation['generation'])
        self.assertEqual(self.pool.observe(capability)['observations'], [])
        self.assertNotIn(token, json.dumps(page))
        self.assertNotIn(capability, json.dumps(page))

    def test_optional_journal_failure_does_not_block_native_lifecycle(self):
        (self.pool.root / 'observations').write_text('unavailable')
        token, _ = self.acquire()
        self.pool.release(token)
        self.assertTrue(all(slot['state'] == 'available' for slot in self.pool.slots))

    def test_runtime_observer_uses_only_generation_owned_cgroup(self):
        runtime = poolmod.Runtime(self.config)
        slot = dict(slot_id='001', generation=2, state='held')
        group = self.root / 'cgroup/system.slice' / runtime.unit(slot)
        group.mkdir(parents=True)
        boot = self.root / 'proc/sys/kernel/random/boot_id'
        boot.parent.mkdir(parents=True)
        boot.write_text('fixture-boot')
        for name, value in {'cpu.stat': 'usage_usec 123\n', 'memory.current': '456',
                            'memory.peak': '789', 'memory.swap.current': '0',
                            'io.stat': '8:0 rbytes=10 wbytes=20 rios=1 wios=2\n'}.items():
            (group / name).write_text(value)
        good = ('ControlGroup=/system.slice/' + runtime.unit(slot) + '\nActiveState=active\nMainPID=42\n').encode()
        with patch.object(poolmod, 'run', return_value=SimpleNamespace(stdout=good)) as runner:
            result = runtime.observe(slot, cgroup_root=self.root / 'cgroup', proc_root=self.root / 'proc')
            self.assertEqual(result['cpu_ns'], 123000)
            self.assertEqual(result['memory_bytes'], 456)
            self.assertEqual(result['io_write_bytes'], 20)
            self.assertEqual(result['missing'], [])
            self.assertFalse(result['final_complete'])
            self.assertEqual(runner.call_args.args[0][0:2], ['systemctl', 'show'])
        foreign = good.replace(runtime.unit(slot).encode(), b'foreign.service')
        with patch.object(poolmod, 'run', return_value=SimpleNamespace(stdout=foreign)):
            result = runtime.observe(slot, cgroup_root=self.root / 'cgroup', proc_root=self.root / 'proc')
            self.assertIsNone(result['cpu_ns'])
            self.assertIsNone(result['counter_generation'])

    def test_two_slots_busy_redaction_and_fencing(self):
        first, one = self.acquire(dict(device='phone', api=34))
        second, two = self.acquire(dict(device='tablet', api=35))
        self.assertNotEqual(one['slot_id'], two['slot_id'])
        with self.assertRaisesRegex(poolmod.PoolError, 'occupied'):
            self.acquire()
        public = json.dumps(self.pool.status())
        self.assertNotIn(first, public)
        self.assertNotIn('credential', public)
        common = one['lease']
        self.assertEqual(common, dict(contract='resource-lease/v1', resource_type='android-emulator',
                                     resource_id='emulator', slot_id=one['slot_id'],
                                     generation=one['generation'], state='held',
                                     owner=self.owner, expires_at=one['expires_at']))
        self.assertEqual(one['request']['android_version'], '14')
        self.pool.release(first)
        self.pool.release(first)
        replacement, three = self.acquire()
        self.assertEqual(three['slot_id'], one['slot_id'])
        self.assertGreater(three['generation'], one['generation'])
        self.pool.release(first)
        with self.assertRaises(poolmod.PoolError):
            self.pool.allocation(first, renew=True)
        self.assertEqual(self.pool.allocation(second)['state'], 'held')
        self.assertEqual(self.pool.allocation(replacement)['state'], 'held')

    def test_cancellation_overtaking_catalog_resolution_never_launches(self):
        entered, finish = threading.Event(), threading.Event()
        original = self.images.resolve
        def delayed(request):
            entered.set()
            finish.wait(3)
            return original(request)
        self.images.resolve = delayed
        token, failures = secrets.token_hex(32), []
        def reserve():
            try:
                self.pool.reserve({}, token, self.owner)
            except poolmod.PoolError as exc:
                failures.append(exc.code)
        thread = threading.Thread(target=reserve)
        thread.start()
        self.assertTrue(entered.wait(2))
        self.pool.release(token)
        finish.set()
        thread.join(3)
        self.assertEqual(failures, ['cancelled'])
        self.assertFalse(self.runtime.running)
        self.assertTrue(all(s['state'] == 'available' for s in self.pool.slots))

    def test_expiry_renew_and_restart(self):
        token, _ = self.acquire()
        self.assertEqual(self.pool.status()['heartbeat_seconds'], 300)
        self.assertEqual(self.pool.status()['ttl_seconds'], 1200)
        self.now += 1190
        self.pool.allocation(token, renew=True)
        self.now += 1190
        self.pool.reap()
        self.assertEqual(self.pool.allocation(token)['state'], 'held')
        self.now += 11
        self.pool.reap()
        self.assertFalse(self.runtime.running)
        token, _ = self.acquire()
        restarted = poolmod.Pool(self.config, self.runtime, self.images, lambda: self.now)
        restarted.reap(recovery=True)
        self.assertFalse(self.runtime.running)
        with self.assertRaises(poolmod.PoolError):
            restarted.allocation(token)

    def test_cancel_boot_and_failed_download_free_slot(self):
        self.runtime.block = threading.Event()
        token = secrets.token_hex(32)
        self.pool.reserve({}, token, self.owner)
        self.assertTrue(self.runtime.started.wait(2))
        self.pool.release(token)
        self.assertFalse(self.runtime.running)
        self.assertEqual(self.pool.slots[0]['state'], 'available')
        self.runtime.block = None
        self.images.fail = True
        token = secrets.token_hex(32)
        self.pool.reserve({}, token, self.owner)
        deadline = time.monotonic() + 2
        while self.pool.workers and time.monotonic() < deadline:
            time.sleep(0.01)
        with self.assertRaisesRegex(poolmod.PoolError, 'failed; runtime was stopped'):
            self.pool.allocation(token)
        self.assertEqual(self.pool.slots[0]['state'], 'available')

    def test_provisioning_failure_is_reported_while_cleanup_drains(self):
        self.runtime.fail_start = True
        self.runtime.stop_block = threading.Event()
        token = secrets.token_hex(32)
        self.pool.reserve({}, token, self.owner)
        self.assertTrue(self.runtime.stop_started.wait(2))
        try:
            with self.assertRaisesRegex(poolmod.PoolError, '^boot failed$') as error:
                self.pool.allocation(token)
            self.assertEqual(error.exception.code, 'boot')
            self.assertEqual(self.pool.slots[0]['state'], 'draining')
        finally:
            self.runtime.stop_block.set()
        deadline = time.monotonic() + 2
        while self.pool.workers and time.monotonic() < deadline:
            time.sleep(0.01)
        with self.assertRaisesRegex(poolmod.PoolError, 'boot failed; runtime was stopped'):
            self.pool.allocation(token)

    def test_quarantine_keeps_image_pinned_and_never_reallocates(self):
        token, allocation = self.acquire()
        self.runtime.fail_stop = True
        with self.assertRaises(poolmod.PoolError):
            self.pool.release(token)
        self.assertEqual(self.pool.slots[0]['state'], 'quarantined')
        self.pool.prune(False)
        self.assertEqual(self.images.pins, {'36'})
        other, second = self.acquire(dict(api=35))
        self.assertNotEqual(second['slot_id'], allocation['slot_id'])
        self.runtime.fail_stop = False
        self.pool.release(token)
        self.assertEqual(self.pool.allocation(other)['state'], 'held')

    def test_quarantined_capacity_is_unavailable_and_expiry_status_is_read_only(self):
        first, _ = self.acquire()
        second, _ = self.acquire()
        self.now += 1201
        before = self.pool.state_path.read_bytes()
        self.assertTrue(all(s['lease']['state'] == 'draining' for s in self.pool.status()['slots']))
        self.assertEqual(before, self.pool.state_path.read_bytes())
        self.runtime.fail_stop = True
        for token in (first, second):
            with self.assertRaises(poolmod.PoolError):
                self.pool.release(token)
        with self.assertRaises(poolmod.PoolError) as failed:
            self.acquire()
        self.assertEqual(failed.exception.code, 'unavailable')
        self.assertTrue(all(s['lease']['state'] == 'quarantined' for s in self.pool.status()['slots']))

    def test_idle_resize_and_restart_preserve_removed_slot_generation(self):
        first, _ = self.acquire()
        second, old = self.acquire()
        self.assertEqual(old['slot_id'], '002')
        self.pool.release(first)
        self.pool.release(second)
        self.pool.shutdown()
        self.pool = poolmod.Pool(dict(self.config, size=1), self.runtime, self.images, lambda: self.now)
        self.assertEqual([s['slot_id'] for s in self.pool.status()['slots']], ['001'])
        self.pool.shutdown()
        self.pool = poolmod.Pool(self.config, self.runtime, self.images, lambda: self.now)
        self.acquire()
        _, replacement = self.acquire()
        self.assertEqual(replacement['slot_id'], old['slot_id'])
        self.assertGreater(replacement['generation'], old['generation'])

    def test_invalid_selection_and_shrink_do_not_mutate(self):
        before = self.pool.state_path.read_bytes()
        for request in [dict(api=99), dict(device='wrong'), dict(abi='arm64-v8a')]:
            with self.assertRaises(poolmod.PoolError):
                self.pool.reserve(request, secrets.token_hex(32), self.owner)
        self.assertEqual(before, self.pool.state_path.read_bytes())
        self.assertEqual(self.pool.status()['graphics_mode'], 'host')
        with self.assertRaisesRegex(poolmod.PoolError, 'EMULATOR_GPU'):
            poolmod.Pool(dict(self.config, gpu='invalid'), self.runtime, self.images)
        self.acquire()
        self.acquire()
        with self.assertRaisesRegex(poolmod.PoolError, 'shrink'):
            poolmod.Pool(dict(self.config, size=1), self.runtime, self.images)

    def test_software_graphics_needs_kvm_but_not_a_render_node(self):
        sdk = self.root / 'sdk'
        emulator = sdk / 'emulator/emulator'
        emulator.parent.mkdir(parents=True)
        emulator.touch()
        runtime = poolmod.Runtime(dict(state_root=str(self.root / 'runtime'), sdk_root=str(sdk),
                                       runtime_user='subyard-android', gpu='software'))
        result = SimpleNamespace(returncode=0, stdout=b'', stderr=b'')
        with patch.object(poolmod.platform, 'machine', return_value='x86_64'), \
             patch.object(poolmod.os, 'access', return_value=True), \
             patch.object(poolmod.shutil, 'which', return_value='/usr/bin/tool'), \
             patch.object(poolmod.Runtime, 'nameservers', return_value=['1.1.1.1']), \
             patch.object(poolmod.Runtime, 'check_capacity'), \
             patch.object(poolmod.Path, 'glob', side_effect=AssertionError('software inspected render nodes')), \
             patch.object(poolmod, 'run', return_value=result) as command:
            runtime.preflight(dict(device='phone'), [], dict(slot_id='001'))
        calls = [arguments.args[0] for arguments in command.call_args_list]
        self.assertTrue(any('/dev/kvm' in call for call in calls))
        self.assertFalse(any('check-graphics' in call for call in calls))

    def host_graphics_runtime(self):
        emulator = self.root / 'sdk/emulator/emulator'
        emulator.parent.mkdir(parents=True)
        emulator.touch()
        (self.root / 'drm/renderD128/device').mkdir(parents=True)
        return poolmod.Runtime(dict(state_root=str(self.root / 'runtime'), sdk_root=str(self.root / 'sdk'),
                                    runtime_user='subyard-android', gpu='host'))

    def render_node(self, vendor, product):
        device = self.root / 'drm/renderD128/device'
        (device / 'vendor').write_text(vendor + '\n')
        (device / 'device').write_text(product + '\n')

    @contextlib.contextmanager
    def host_graphics_preflight(self, run, real_capacity=False):
        """Patch a ready x86_64 host with one render node; yields the mocked command runner."""
        with contextlib.ExitStack() as stack:
            for target, name, options in [
                    (poolmod.Runtime, 'DRM_CLASS', dict(new=self.root / 'drm')),
                    (poolmod.platform, 'machine', dict(return_value='x86_64')),
                    (poolmod.os, 'access', dict(return_value=True)),
                    (poolmod.shutil, 'which', dict(return_value='/usr/bin/tool')),
                    (poolmod.Runtime, 'nameservers', dict(return_value=['1.1.1.1'])),
                    (poolmod.Path, 'glob', dict(return_value=[poolmod.Path('/dev/dri/renderD128')])),
                    (poolmod.pwd, 'getpwnam', dict(return_value=SimpleNamespace(pw_uid=os.getuid(), pw_gid=os.getgid()))),
                    (poolmod.os, 'chown', {}),
                    (poolmod.tempfile, 'mkdtemp',
                     dict(side_effect=lambda make=tempfile.mkdtemp, **_: make(dir=self.root))),
            ] + ([] if real_capacity else [(poolmod.Runtime, 'check_capacity', {})]):
                stack.enter_context(patch.object(target, name, **options))
            yield stack.enter_context(patch.object(poolmod, 'run', **run))

    def test_host_graphics_refuses_a_virtio_display_without_3d(self):
        runtime = self.host_graphics_runtime()
        result = SimpleNamespace(returncode=0, stdout=b'', stderr=b'')
        for vendor, product, features, refused, probed in [
                ('0x1af4', '0x1050', 0, True, True),      # Incus virtio-vga: no virgl
                ('0x1af4', '0x1050', 1, False, True),     # virtio-gpu with 3D
                ('0x8086', '0x56a0', None, False, False),  # a real GPU is never probed
        ]:
            self.render_node(vendor, product)
            probe = (lambda node, value=features: value) if probed else \
                (lambda node: (_ for _ in ()).throw(AssertionError('probed a non-virtio node')))
            with self.subTest(vendor=vendor, features=features), \
                 patch.object(poolmod.Runtime, 'virtio_3d_features', side_effect=probe), \
                 self.host_graphics_preflight(dict(return_value=result)) as command:
                if refused:
                    with self.assertRaisesRegex(poolmod.PoolError, 'virtio display without 3D'):
                        runtime.preflight(dict(device='phone'), [], dict(slot_id='001'))
                    calls = [arguments.args[0] for arguments in command.call_args_list]
                    self.assertFalse(any('--check-graphics' in call for call in calls))
                    self.assertFalse(any('/dev/dri/renderD128' in call for call in calls))
                else:
                    runtime.preflight(dict(device='phone'), [], dict(slot_id='001'))
                    calls = [arguments.args[0] for arguments in command.call_args_list]
                    self.assertTrue(any('--check-graphics' in call for call in calls))

    def test_host_graphics_probe_is_reported_independently_of_kvm_access(self):
        runtime = self.host_graphics_runtime()
        self.render_node('0x8086', '0x56a0')

        def command(arguments, **_):
            refused = '/dev/kvm' in arguments or '--check-graphics' in arguments
            return SimpleNamespace(returncode=1 if refused else 0, stdout=b'', stderr=b'')
        with self.host_graphics_preflight(dict(side_effect=command)) as run:
            with self.assertRaises(poolmod.PoolError) as refused:
                runtime.preflight(dict(device='phone'), [], dict(slot_id='001'))
        message = str(refused.exception)
        self.assertEqual(refused.exception.code, 'preflight')
        kvm = message.find('cannot access /dev/kvm')
        graphics = message.find('hardware graphics unavailable')
        self.assertGreaterEqual(kvm, 0, message)
        self.assertGreater(graphics, kvm, message)
        self.assertNotIn('cannot access /dev/dri/renderD128', message)
        calls = [arguments.args[0] for arguments in run.call_args_list]
        self.assertTrue(any('--check-graphics' in call for call in calls))

    def test_preflight_reports_refused_host_graphics_and_insufficient_memory_together(self):
        cgroups = self.root / 'cgroups'
        (cgroups / 'system.slice').mkdir(parents=True)
        for directory in (cgroups, cgroups / 'system.slice'):
            (directory / 'memory.max').write_text('max')
            (directory / 'memory.current').write_text('0')
        runtime = self.host_graphics_runtime()
        self.render_node('0x1af4', '0x1050')
        result = SimpleNamespace(returncode=0, stdout=b'', stderr=b'')
        original_read_text = poolmod.Path.read_text
        def constrained_memory(path, *args, **kwargs):
            if str(path) == '/proc/meminfo':
                return 'MemTotal:        7863296 kB\nMemAvailable:    7863296 kB\n'
            return original_read_text(path, *args, **kwargs)
        with patch.object(poolmod.Runtime, 'CGROUP_ROOT', cgroups), \
             patch.object(poolmod.Runtime, 'HOST_MEMORY', self.root / 'absent-host-meminfo'), \
             patch.object(poolmod.Runtime, 'physical_memory', return_value=None), \
             patch.object(poolmod.Runtime, 'virtio_3d_features', return_value=0), \
             patch.object(poolmod.Path, 'read_text', new=constrained_memory), \
             self.host_graphics_preflight(dict(return_value=result), real_capacity=True) as command:
            with self.assertRaises(poolmod.PoolError) as refused:
                runtime.preflight(dict(device='tablet'), [dict(request=dict(device='tablet'), state='held')],
                                  dict(slot_id='001'))
        message = str(refused.exception)
        self.assertEqual(refused.exception.code, 'preflight')
        graphics = message.find('virtio display without 3D')
        memory = message.find('insufficient memory')
        self.assertGreaterEqual(graphics, 0, message)
        self.assertGreater(memory, graphics, message)
        calls = [arguments.args[0] for arguments in command.call_args_list]
        self.assertFalse(any('--check-graphics' in call for call in calls))

    def test_emulator_ram_floors_admit_pair_and_reject_insufficient_memory(self):
        cgroups = self.root / 'cgroups'
        (cgroups / 'system.slice').mkdir(parents=True)
        for directory in (cgroups, cgroups / 'system.slice'):
            (directory / 'memory.max').write_text('max')
            (directory / 'memory.current').write_text('0')
        self.addCleanup(patch.stopall)
        patch.object(poolmod.Runtime, 'CGROUP_ROOT', cgroups).start()
        patch.object(poolmod.Runtime, 'HOST_MEMORY', self.root / 'absent-host-meminfo').start()
        patch.object(poolmod.Runtime, 'physical_memory', return_value=None).start()
        sdk = self.root / 'sdk'
        emulator = sdk / 'emulator/emulator'
        emulator.parent.mkdir(parents=True)
        emulator.touch()
        runtime = poolmod.Runtime(dict(state_root=str(self.root / 'runtime'), sdk_root=str(sdk),
                                       runtime_user='subyard-android', gpu='software'))
        result = SimpleNamespace(returncode=0, stdout=b'', stderr=b'')
        original_read_text = poolmod.Path.read_text
        def read_text(path, *args, **kwargs):
            if str(path) == '/proc/meminfo':
                return 'MemTotal:        8387584 kB\nMemAvailable:    8387584 kB\n'
            return original_read_text(path, *args, **kwargs)
        with patch.object(poolmod.platform, 'machine', return_value='x86_64'), \
             patch.object(poolmod.os, 'access', return_value=True), \
             patch.object(poolmod.shutil, 'which', return_value='/usr/bin/tool'), \
             patch.object(poolmod.Runtime, 'nameservers', return_value=['1.1.1.1']), \
             patch.object(poolmod.Path, 'read_text', new=read_text), \
             patch.object(poolmod, 'run', return_value=result):
            runtime.preflight(dict(device='phone'), [dict(request=dict(device='tablet'), state='held')], dict(slot_id='001'))

        def constrained_memory(path, *args, **kwargs):
            if str(path) == '/proc/meminfo':
                return 'MemTotal:        7863296 kB\nMemAvailable:    7863296 kB\n'
            return original_read_text(path, *args, **kwargs)
        with patch.object(poolmod.platform, 'machine', return_value='x86_64'), \
             patch.object(poolmod.os, 'access', return_value=True), \
             patch.object(poolmod.shutil, 'which', return_value='/usr/bin/tool'), \
             patch.object(poolmod.Runtime, 'nameservers', return_value=['1.1.1.1']), \
             patch.object(poolmod.Path, 'read_text', new=constrained_memory), \
             patch.object(poolmod, 'run', return_value=result):
            with self.assertRaisesRegex(poolmod.PoolError, 'insufficient memory'):
                runtime.preflight(dict(device='tablet'), [dict(request=dict(device='tablet'), state='held')], dict(slot_id='001'))

    def test_memory_bounds_exclude_broker_ceiling_and_reject_unsafe_physical_source(self):
        proc, cgroups = self.root / 'proc', self.root / 'cgroups'
        (proc / 'self').mkdir(parents=True)
        unit = cgroups / 'system.slice/subyard-android-pool.service'
        unit.mkdir(parents=True)
        gib = 1024**3
        (proc / 'meminfo').write_text('MemTotal: 16777216 kB\nMemAvailable: 12582912 kB\n'
                                    'MemFree: 1024 kB\nSReclaimable: 1048576 kB\nSwapFree: 999999999 kB\n')
        (proc / 'self/mountinfo').write_text('')
        (proc / 'self/cgroup').write_text('0::/system.slice/subyard-android-pool.service\n')
        for directory, limit, used in ((cgroups, 16*gib, 0), (unit.parent, 10*gib, 2*gib),
                                       (unit, 2*gib, gib)):
            (directory / 'memory.max').write_text(str(limit))
            (directory / 'memory.current').write_text(str(used))
        (unit / 'memory.swap.max').write_text(str(2*gib))
        (unit / 'memory.swap.current').write_text('0')
        runtime = poolmod.Runtime(dict(state_root=str(self.root / 'runtime'), sdk_root='/unused'))
        runtime.PROC_ROOT, runtime.CGROUP_ROOT, runtime.HOST_MEMORY = proc, cgroups, self.root / 'host-meminfo'
        self.assertEqual(runtime.memory_capacity()['available_bytes'], 8*gib)
        self.assertEqual(runtime.broker_memory()['ram_limit_bytes'], 2*gib)
        with patch.object(runtime, 'physical_memory', return_value=dict(MemTotal=16*gib, MemAvailable=6*gib)):
            capacity = runtime.memory_capacity()
            self.assertEqual((capacity['available_bytes'], capacity['limiting_source']), (6*gib, 'physical'))
        (unit.parent / 'memory.current').write_text(str(11*gib))
        self.assertEqual(runtime.memory_capacity()['available_bytes'], 0)
        (unit.parent / 'memory.max').write_text(str(gib))
        with self.assertRaisesRegex(poolmod.PoolError, 'ancestor'):
            runtime.broker_memory()
        (unit.parent / 'memory.max').write_text('max')
        (cgroups / 'memory.max').unlink()
        (cgroups / 'cgroup.controllers').write_text('cpu memory')
        self.assertEqual(runtime.memory_capacity()['available_bytes'], 12*gib)
        (proc / 'self/mountinfo').write_text('1 2 0:1 / /proc/meminfo rw - fuse.lxcfs lxcfs rw\n')
        with self.assertRaisesRegex(poolmod.PoolError, 'required'):
            runtime.memory_capacity()
        runtime.HOST_MEMORY.symlink_to(proc / 'meminfo')
        with self.assertRaisesRegex(poolmod.PoolError, 'unsafe'):
            runtime.memory_capacity()
        runtime.HOST_MEMORY.unlink()
        runtime.HOST_MEMORY.write_text('MemTotal: 16777216 kB\nMemAvailable: 12582912 kB\n')
        original_read_text = poolmod.Path.read_text
        def descriptor(path, *args, **kwargs):
            if '/fdinfo/' in str(path):
                return 'mnt_id: 91\n'
            return original_read_text(path, *args, **kwargs)
        (proc / 'self/mountinfo').write_text(f'91 2 0:1 /meminfo {runtime.HOST_MEMORY} rw - proc proc rw\n')
        with patch.object(poolmod.Path, 'read_text', new=descriptor):
            with self.assertRaisesRegex(poolmod.PoolError, 'read-only procfs'):
                runtime.memory_capacity()
        (proc / 'meminfo').write_text('MemTotal: 16777216 kB\nMemAvailable: invalid kB\n')
        with self.assertRaisesRegex(poolmod.PoolError, 'invalid memory'):
            runtime.memory_capacity()

    def test_status_projects_pending_memory_and_saturates_headroom(self):
        self.runtime.budget = poolmod.Runtime.budget
        request = self.pool.normalize({})
        pending = self.runtime.budget(request)
        self.pool.slots[0].update(state='provisioning', request=request)
        self.pool.slots[1].update(state='held', request=request)
        for available, expected in ((pending + poolmod.MEMORY_RESERVE + 123, 123), (0, 0)):
            with self.subTest(available=available):
                self.runtime.memory_capacity = lambda: dict(available_bytes=available,
                                                            reserve_bytes=poolmod.MEMORY_RESERVE)
                memory = self.pool.status()['memory']
                self.assertEqual(memory['pending_bytes'], pending)
                self.assertEqual(memory['admission_headroom_bytes'], expected)

    def test_pending_starts_are_atomic_and_held_memory_is_not_subtracted_twice(self):
        self.runtime.budget = poolmod.Runtime.budget
        self.runtime.memory_capacity = lambda: dict(total_bytes=16*1024**3, available_bytes=8*1024**3,
                                                   limiting_source='visible')
        self.runtime.check_capacity = lambda request, reserved: poolmod.Runtime.check_capacity(self.runtime, request, reserved)
        self.runtime.preflight = lambda request, reserved, slot: self.runtime.check_capacity(request, reserved)
        self.runtime.block = threading.Event()
        self.pool = poolmod.Pool(dict(self.config, size=3), self.runtime, self.images, lambda: self.now)
        first, second = secrets.token_hex(32), secrets.token_hex(32)
        self.pool.reserve({}, first, self.owner)
        self.assertTrue(self.runtime.started.wait(2))
        self.pool.reserve({}, second, self.owner)
        with self.assertRaisesRegex(poolmod.PoolError, 'pending=7516192768'):
            self.pool.reserve({}, secrets.token_hex(32), self.owner)
        self.runtime.block.set()
        for worker in list(self.pool.workers.values()):
            worker.join(2)
        self.assertEqual(self.pool.allocation(first)['state'], 'held')
        self.assertEqual(self.pool.allocation(second)['state'], 'held')
        # Availability already measures both running emulators. Only the new request needs RAM.
        self.runtime.memory_capacity = lambda: dict(total_bytes=16*1024**3, available_bytes=4*1024**3,
                                                   limiting_source='visible')
        self.pool.reserve({}, secrets.token_hex(32), self.owner)

    def test_acquire_renewal_cadence_and_requester_identity(self):
        args = client.parser().parse_args(['acquire', '--lease-file', '/unused'])
        now, renewals, allocations = [0], [], [0]
        def rpc(operation, **kwargs):
            if operation == 'renew':
                renewals.append(now[0])
            if operation == 'allocation':
                allocations[0] += 1
                return dict(state='held' if now[0] >= 601 else 'provisioning')
            return {}
        with patch.object(client, 'attribution', return_value=self.owner), \
             patch.object(client, 'rpc', side_effect=rpc), \
             patch.object(client.time, 'monotonic', side_effect=lambda: now[0]), \
             patch.object(client.time, 'sleep', side_effect=lambda seconds: now.__setitem__(0, now[0] + seconds)):
            self.assertEqual(client.acquire(args, secrets.token_hex(32))['state'], 'held')
        self.assertEqual(renewals, [300, 600])
        with patch.object(client, 'rpc', side_effect=AssertionError('orphan made request')):
            with self.assertRaisesRegex(client.Error, 'requesting session ended'):
                client.acquire(args, secrets.token_hex(32), lambda: False)
        alive = [True]
        now[0] = 0
        renewals.clear()
        def ended_during_allocation(operation, **kwargs):
            if operation == 'allocation':
                alive[0] = False
                now[0] = 301
                return dict(state='held')
            if operation == 'renew':
                renewals.append(now[0])
            return {}
        with patch.object(client, 'attribution', return_value=self.owner), \
             patch.object(client, 'rpc', side_effect=ended_during_allocation), \
             patch.object(client.time, 'monotonic', side_effect=lambda: now[0]):
            with self.assertRaisesRegex(client.Error, 'requesting session ended'):
                client.acquire(args, secrets.token_hex(32), lambda: alive[0])
        self.assertEqual(renewals, [])
        pid = os.getpid()
        self.assertIsNotNone(client.requester_identity(pid))
        self.assertIsNone(client.requester_identity(1))
        with patch.object(client.Path, 'read_text', return_value='1 (reused pid) Z ' + ' '.join(['0']*20)):
            self.assertIsNone(client.requester_identity(pid))

    def test_runtime_software_graphics_check_requires_no_cage_or_render_node(self):
        root = self.root / 'software-runtime'
        root.mkdir()
        commands = root / 'bin'
        commands.mkdir()
        identity = commands / 'id'
        identity.write_text('#!/bin/sh\nprintf "%s\\n" subyard-android-001\n')
        identity.chmod(0o755)
        environment = dict(os.environ, HOME=str(root), PATH=str(commands) + ':' + os.environ['PATH'],
                           EMULATOR_GPU='software')
        check = subprocess.run(['bash', str(ROOT / 'config/profiles/android/runtime.sh'), '--check-graphics'],
                               env=environment, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.assertEqual(check.returncode, 0, check.stderr.decode())
        environment['EMULATOR_GPU'] = 'invalid'
        invalid = subprocess.run(['bash', str(ROOT / 'config/profiles/android/runtime.sh'), '--check-graphics'],
                                 env=environment, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.assertEqual(invalid.returncode, 2)
        sdk = root / 'sdk'
        for path, body in ((sdk / 'platform-tools/adb', 'exit 0'),
                           (sdk / 'emulator/emulator', 'printf "%s\\n" "$@"; exit 42'),
                           (commands / 'cage', 'shift; exec "$@"')):
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text('#!/bin/sh\n' + body + '\n')
            path.chmod(0o755)
        ready = root / 'ready'
        ready.touch()
        environment.update(ANDROID_HOME=str(sdk), ANDROID_AVD_HOME=str(root / 'avd'),
                           SUBYARD_NETWORK_READY=str(ready), WLR_RENDER_DRM_DEVICE='/fixture/render-node')
        for mode in ('software', 'software-gles', 'host'):
            environment['EMULATOR_GPU'] = mode
            launch = subprocess.run(['bash', str(ROOT / 'config/profiles/android/runtime.sh')],
                                    env=environment, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            self.assertEqual(launch.returncode, 42, launch.stderr)
            arguments = launch.stdout.splitlines()
            self.assertEqual(arguments[arguments.index('-accel') + 1], 'on')
            self.assertEqual(arguments[arguments.index('-gpu') + 1],
                             'host' if mode == 'host' else 'swiftshader')
            if mode == 'software-gles':
                self.assertEqual(arguments[arguments.index('-feature') + 1], '-Vulkan')
            else:
                self.assertNotIn('-feature', arguments)

    def test_start_passes_software_mode_and_ram_floor_to_avd_and_runtime(self):
        sdk, image = self.root / 'sdk', self.root / 'image'
        image.mkdir()
        runtime = poolmod.Runtime(dict(state_root=str(self.root / 'runtime'), sdk_root=str(sdk), size=2,
                                       runtime_user='subyard-android', gpu='software'))
        account = SimpleNamespace(pw_uid=os.getuid(), pw_gid=os.getgid(), pw_name='subyard-android-001')
        commands = []
        netns = self.root / 'netns'
        def record(arguments, **_kwargs):
            commands.append(arguments)
            if arguments[:3] == ['ip', 'netns', 'add']:
                netns.mkdir(exist_ok=True)
                (netns / arguments[3]).touch()
            return SimpleNamespace(returncode=0, stdout=b'', stderr=b'')
        slots = [
            (dict(slot_id='001', generation=1,
                  request=dict(api=35, variant='google_apis', abi='x86_64', device='phone')), 2560),
            (dict(slot_id='002', generation=1,
                  request=dict(api=35, variant='google_apis', abi='x86_64', device='tablet')), 2560),
        ]
        with patch.object(poolmod.shutil, 'disk_usage', return_value=SimpleNamespace(free=64 * 1024**3)), \
             patch.object(poolmod.Runtime, 'nameservers', return_value=['1.1.1.1']), \
             patch.object(poolmod.pwd, 'getpwnam', return_value=account), \
             patch.object(poolmod.os, 'chown'), patch.object(poolmod, 'run', side_effect=record), \
             patch.object(poolmod.Runtime, 'start_network'), patch.object(poolmod.Runtime, 'pid', return_value=1), \
             patch.object(poolmod.Runtime, 'connect'), patch.object(poolmod.Runtime, 'adb_ready', return_value=(True, False)), \
             patch.object(poolmod.Runtime, 'NETNS_ROOT', netns):
            for slot, _ in slots:
                runtime.start(slot, image, threading.Event())
        for slot, memory in slots:
            avd = runtime.root / runtime.unit(slot).removesuffix('.service') / 'home/avd/managed.avd/config.ini'
            self.assertIn('hw.gpu.mode=swiftshader\n', avd.read_text())
            self.assertIn(f'hw.ramSize={memory}\n', avd.read_text())
            if slot['request']['device'] == 'tablet':
                self.assertIn('hw.lcd.width=800\n', avd.read_text())
                self.assertIn('hw.lcd.height=1280\n', avd.read_text())
                self.assertIn('hw.lcd.density=160\n', avd.read_text())
            command = next(command for command in commands if '--unit' in command
                           and command[command.index('--unit') + 1] == runtime.unit(slot))
            self.assertIn('--setenv=EMULATOR_GPU=software', command)
            self.assertIn(f'--property=MemoryMax={memory + poolmod.RUNTIME_OVERHEAD_MIB}M', command)
            self.assertIn('--property=NetworkNamespacePath=' + str(netns / runtime.namespace_name(slot)), command)
            self.assertNotIn('--property=PrivateNetwork=yes', command)

    def test_runtime_namespace_rejects_collisions_and_cleans_only_its_name(self):
        runtime = poolmod.Runtime(dict(state_root=str(self.root / 'runtime'), sdk_root='/unused'))
        namespace_root = self.root / 'netns'
        slot = dict(slot_id='001', generation=1)
        directory = runtime.root / runtime.unit(slot).removesuffix('.service')
        directory.mkdir()
        commands = []
        def record(arguments, **_kwargs):
            commands.append(arguments)
            if arguments[:3] == ['ip', 'netns', 'add']:
                namespace_root.mkdir(exist_ok=True)
                (namespace_root / arguments[3]).touch()
            if arguments[:3] == ['ip', 'netns', 'delete']:
                (namespace_root / arguments[3]).unlink()
            return SimpleNamespace(returncode=0, stdout=b'', stderr=b'')
        with patch.object(poolmod.Runtime, 'NETNS_ROOT', namespace_root), \
             patch.object(poolmod, 'run', side_effect=record):
            path = runtime.create_namespace(slot, directory)
            self.assertEqual(path, namespace_root / 'subyard-android-slot-001-1')
            with self.assertRaisesRegex(poolmod.PoolError, 'already exists'):
                runtime.create_namespace(slot, directory)
            runtime.delete_namespace(slot, directory)
            collided = dict(slot_id='001', generation=2)
            collision_path = namespace_root / runtime.namespace_name(collided)
            collision_path.touch()
            collision_directory = runtime.root / runtime.unit(collided).removesuffix('.service')
            collision_directory.mkdir()
            with self.assertRaisesRegex(poolmod.PoolError, 'already exists'):
                runtime.create_namespace(collided, collision_directory)
            with self.assertRaisesRegex(poolmod.PoolError, 'receipt is missing'):
                runtime.delete_namespace(collided, collision_directory)
            self.assertTrue(collision_path.exists())
        self.assertFalse(path.exists())
        self.assertEqual(commands[0][:3], ['ip', 'netns', 'add'])
        self.assertIn(['ip', '-n', 'subyard-android-slot-001-1', 'link', 'set', 'lo', 'up'], commands)
        self.assertEqual(commands[-1][:3], ['ip', 'netns', 'delete'])

    def test_runtime_network_rejects_shared_namespace_tap_and_failed_egress(self):
        runtime = poolmod.Runtime(dict(state_root=str(self.root / 'runtime'), sdk_root='/unused'))
        slot, directory, namespace = dict(slot_id='001', generation=1), self.root / 'runtime-dir', self.root / 'netns'
        directory.mkdir()
        namespace.mkdir()
        (namespace / 'subyard-android-slot-001-1').touch()
        (namespace / 'subyard-android-slot-002-1').touch()
        poolmod.atomic(directory / 'network-namespace.json',
                       dict(name='subyard-android-slot-001-1', inode=17))
        with patch.object(poolmod.Runtime, 'NETNS_ROOT', namespace), \
            patch.object(poolmod.Runtime, 'namespace_inode', return_value=17), \
             patch.object(poolmod.os, 'stat', return_value=SimpleNamespace(st_ino=17)):
            with self.assertRaisesRegex(poolmod.PoolError, 'collides with another slot'):
                runtime.verify_namespace(slot, 42, namespace / 'subyard-android-slot-001-1', directory)
        original_read_text = poolmod.Path.read_text
        def cgroup(path, *args, **kwargs):
            if str(path) == '/proc/42/cgroup':
                return runtime.unit(slot) + '\n'
            return original_read_text(path, *args, **kwargs)
        with patch.object(poolmod.Runtime, 'pid', return_value=42), \
             patch.object(poolmod.Runtime, 'verify_namespace'), \
             patch.object(poolmod.Path, 'read_text', new=cgroup), \
             patch.object(poolmod, 'run', return_value=SimpleNamespace(returncode=0, stdout=b'')) as command:
            with self.assertRaisesRegex(poolmod.PoolError, 'already has tap0'):
                runtime.start_network(slot, directory, threading.Event(), namespace)
        self.assertFalse(any('/usr/bin/slirp4netns' in call.args[0] for call in command.call_args_list))

        def failed_egress(arguments, **_kwargs):
            if arguments[:3] == ['nsenter', '--net=' + str(namespace), 'ip']:
                return SimpleNamespace(returncode=1, stdout=b'')
            if arguments[:2] == ['systemctl', 'show']:
                return SimpleNamespace(returncode=0, stdout=b'MainPID=0\nActiveState=failed\n')
            return SimpleNamespace(returncode=0, stdout=b'')
        with patch.object(poolmod.Runtime, 'pid', return_value=42), \
             patch.object(poolmod.Runtime, 'verify_namespace'), \
             patch.object(poolmod.Path, 'read_text', new=cgroup), \
             patch.object(poolmod, 'run', side_effect=failed_egress):
            with self.assertRaisesRegex(poolmod.PoolError, 'egress helper exited'):
                runtime.start_network(slot, directory, threading.Event(), namespace)

    def test_runtime_connect_revalidates_named_namespace(self):
        runtime = poolmod.Runtime(dict(state_root=str(self.root / 'runtime'), sdk_root='/unused'))
        slot = dict(slot_id='001', generation=1)
        directory = runtime.root / runtime.unit(slot).removesuffix('.service')
        directory.mkdir()
        original_read_text = poolmod.Path.read_text
        def cgroup(path, *args, **kwargs):
            if str(path) == '/proc/42/cgroup':
                return runtime.unit(slot) + '\n'
            return original_read_text(path, *args, **kwargs)
        process = SimpleNamespace()
        with patch.object(poolmod.Runtime, 'pid', return_value=42), \
             patch.object(poolmod.Path, 'read_text', new=cgroup), \
             patch.object(poolmod.Runtime, 'verify_namespace') as verify, \
             patch.object(poolmod.subprocess, 'Popen', return_value=process) as popen:
            self.assertIs(runtime.connect(slot), process)
        verify.assert_called_once_with(slot, 42, runtime.namespace_path(slot), directory)
        self.assertEqual(popen.call_args.args[0][:2], ['nsenter', '--net=' + str(runtime.namespace_path(slot))])

    def test_adb_readiness_uses_sequential_bounded_protocol(self):
        protocol = r'''
import os, select, sys
def read_exact(size):
    data = bytearray()
    while len(data) < size:
        chunk = os.read(0, size - len(data))
        if not chunk:
            raise SystemExit(3)
        data.extend(chunk)
    return bytes(data)
def request():
    return read_exact(int(read_exact(4), 16))
out = sys.stdout.buffer
if request() != b'host:transport:emulator-5554':
    raise SystemExit(4)
if select.select([0], [], [], 0.1)[0]:
    out.write(b'FAIL')
    out.flush()
    raise SystemExit(5)
out.write(b'OKAY')
out.flush()
if not request().startswith(b'shell:'):
    raise SystemExit(6)
out.write(b'OKAY101:ready\n')
out.flush()
'''
        cases = [(protocol, True, 1, 0),
                 (protocol.replace("b'OKAY101:ready\\n'", "b'OKAY101:pending\\n'"), False, 1, 0),
                 ('import os; os.read(0, 64)', False, 0.1, 0),
                 ('import sys; sys.stdin.buffer.read()', False, 0.1, None)]
        for program, expected, timeout, exit_code in cases:
            process = subprocess.Popen([sys.executable, '-c', program], stdin=subprocess.PIPE, stdout=subprocess.PIPE)
            try:
                self.assertEqual(poolmod.Runtime.adb_ready(process, timeout=timeout), (expected, False))
            finally:
                try:
                    returned = process.wait(timeout=1) if exit_code is not None else None
                except subprocess.TimeoutExpired:
                    returned = None
                if returned is None:
                    with contextlib.suppress(ProcessLookupError):
                        process.kill()
                    returned = process.wait()
                process.stdin.close()
                process.stdout.close()
            if exit_code is not None:
                self.assertEqual(returned, exit_code)

    def test_adb_readiness_waits_for_guest_network_after_boot(self):
        commands = self.root / 'readiness-bin'
        commands.mkdir()
        for name, variable in (('getprop', 'BOOT_COMPLETE'), ('ip', 'GUEST_ADDRESS'),
                               ('settings', 'WIFI_ENABLED'), ('pidof', 'FRAMEWORK_PID'),
                               ('service', 'WIFI_SERVICE')):
            command = commands / name
            command.write_text('#!/bin/sh\nprintf "%s\\n" "$' + variable + '"\n')
            command.chmod(0o755)
        service = commands / 'svc'
        service.write_text('#!/bin/sh\nprintf "%s\\n" "$*" > "$WIFI_CALL"\nexit "$WIFI_EXIT"\n')
        service.chmod(0o755)
        command = commands / 'cmd'
        command.write_text('#!/bin/sh\ncase "$*" in\n'
                           '"wifi status") printf "%s\\n" "$ACTUAL_WIFI_STATUS";;\n'
                           '"wifi connect-network AndroidWifi open") printf "%s\\n" "$*" > "$CONNECT_CALL"; '
                           'exit "$CONNECT_EXIT";;\n*) exit 2;;\nesac\n')
        command.chmod(0o755)
        protocol = r'''
import os, subprocess

def read_exact(size):
    result = b''
    while len(result) < size:
        value = os.read(0, size - len(result))
        if not value:
            raise SystemExit(2)
        result += value
    return result

def request():
    return read_exact(int(read_exact(4), 16))

assert request() == b'host:transport:emulator-5554'
os.write(1, b'OKAY')
command = request()
assert command.startswith(b'shell:')
os.write(1, b'OKAY')
result = subprocess.run(['/bin/sh', '-c', command[6:].decode()], stdout=subprocess.PIPE)
os.write(1, result.stdout)
'''
        address = '    inet 10.0.2.16/24 scope global wlan0'
        cases = [
            # A configured address is ready and never creates another saved network.
            ('1', '1', address, '0', '0', True, (True, False), False, False),
            # A visible but unconfigured network asks for one connection per framework lifetime.
            ('1', '1', '', '0', '0', True, (False, True), False, True),
            # A failed connection does not claim either readiness or configuration.
            ('1', '1', '', '0', '1', True, (False, False), False, True),
            # Later polling must not reconnect repeatedly.
            ('1', '1', '', '0', '0', False, (False, False), False, False),
            # Wi-Fi is enabled first when Android reports it disabled.
            ('1', '0', '', '0', '0', True, (False, True), True, True),
            ('1', '0', address, '1', '0', True, (False, False), True, False),
            ('0', '0', address, '0', '0', True, (False, False), False, False),
        ]
        for boot, wifi, address, wifi_exit, connect_exit, configure, expected, enable, connect in cases:
            with self.subTest(boot=boot, wifi=wifi, network=bool(address), configure=configure,
                              wifi_exit=wifi_exit, connect_exit=connect_exit):
                wifi_call, connect_call = self.root / 'wifi-call', self.root / 'connect-call'
                wifi_call.unlink(missing_ok=True)
                connect_call.unlink(missing_ok=True)
                env = dict(os.environ, BOOT_COMPLETE=boot, GUEST_ADDRESS=address,
                           WIFI_ENABLED=wifi, WIFI_EXIT=wifi_exit, WIFI_CALL=str(wifi_call),
                           CONNECT_EXIT=connect_exit, CONNECT_CALL=str(connect_call),
                           FRAMEWORK_PID='101', WIFI_SERVICE='Service wifi: found',
                           ACTUAL_WIFI_STATUS='Wifi is enabled\nprivate connection details',
                           PATH=str(commands) + ':' + os.environ['PATH'])
                process = subprocess.Popen([sys.executable, '-c', protocol], env=env,
                                           stdin=subprocess.PIPE, stdout=subprocess.PIPE)
                try:
                    self.assertEqual(poolmod.Runtime.adb_ready(process, timeout=1,
                                                                wifi_state={} if configure else {'submitted_for': '101'}), expected)
                finally:
                    process.stdin.close()
                    process.wait(timeout=2)
                    process.stdout.close()
                self.assertEqual(wifi_call.exists(), enable)
                if enable:
                    self.assertEqual(wifi_call.read_text(), 'wifi enable\n')
                self.assertEqual(connect_call.exists(), connect)
                if connect:
                    self.assertEqual(connect_call.read_text(), 'wifi connect-network AndroidWifi open\n')

    def test_wifi_submission_recovers_only_after_framework_or_service_change(self):
        commands = self.root / 'framework-readiness-bin'
        commands.mkdir()
        for name, value in (
            ('pidof', '$FRAMEWORK_PID'), ('service', '$WIFI_SERVICE'),
            ('getprop', '1'), ('settings', '1'), ('ip', '$GUEST_ADDRESS')):
            path = commands / name
            path.write_text('#!/bin/sh\nprintf "%s\\n" "' + value + '"\n')
            path.chmod(0o755)
        submissions = self.root / 'wifi-submissions'
        command = commands / 'cmd'
        command.write_text('#!/bin/sh\ncase "$*" in\n'
                           '"wifi status") printf "%s\\n" "$ACTUAL_WIFI_STATUS";;\n'
                           '"wifi connect-network AndroidWifi open") printf "%s\\n" "$*" >> "$SUBMISSIONS";;\n'
                           '*) exit 2;;\nesac\n')
        command.chmod(0o755)
        protocol = r'''
import os, subprocess

def request():
    size = int(os.read(0, 4), 16)
    result = b''
    while len(result) < size:
        result += os.read(0, size - len(result))
    return result

assert request() == b'host:transport:emulator-5554'
os.write(1, b'OKAY')
command = request()
assert command.startswith(b'shell:')
os.write(1, b'OKAY')
result = subprocess.run(['/bin/sh', '-c', command[6:].decode()], stdout=subprocess.PIPE)
os.write(1, result.stdout)
'''
        state = {}
        def probe(pid='101', service='Service wifi: found', address='',
                  radio='Wifi is enabled\nprivate connection details'):
            env = dict(os.environ, FRAMEWORK_PID=pid, WIFI_SERVICE=service,
                       ACTUAL_WIFI_STATUS=radio,
                       GUEST_ADDRESS=address, SUBMISSIONS=str(submissions),
                       PATH=str(commands) + ':' + os.environ['PATH'])
            process = subprocess.Popen([sys.executable, '-c', protocol], env=env,
                                       stdin=subprocess.PIPE, stdout=subprocess.PIPE)
            try:
                return poolmod.Runtime.adb_ready(process, timeout=1, wifi_state=state)
            finally:
                process.stdin.close()
                process.wait(timeout=2)
                process.stdout.close()
        def count():
            return len(submissions.read_text().splitlines()) if submissions.exists() else 0

        # Desired wifi_on=1 precedes an actually enabled radio; status is disabled
        # while Android is enabling. No accepted connection may be cached yet.
        self.assertEqual(probe(radio='Wifi is disabled'), (False, False))
        self.assertEqual(probe(radio='Wifi is disabled\nWifi is enabled'), (False, False))
        self.assertEqual(state, {})
        self.assertEqual(count(), 0)
        self.assertEqual(probe(), (False, True))
        self.assertEqual(state, {'submitted_for': '101'})
        self.assertEqual(probe(), (False, False))
        self.assertEqual(count(), 1, 'pending DHCP must not trigger another connection')
        self.assertEqual(probe(pid='202'), (False, True))
        self.assertEqual(probe(pid='202'), (False, False))
        self.assertEqual(count(), 2, 'new framework must receive one connection')
        self.assertEqual(probe(pid='202', address='    inet 10.0.2.16/24'), (True, False))
        self.assertEqual(probe(pid='202', service='Service wifi: not found'), (False, False))
        self.assertEqual(probe(pid='202'), (False, True))
        self.assertEqual(probe(pid='202'), (False, False))
        self.assertEqual(count(), 3, 'returning service must receive one connection')
        for pid, service in [('garbage', 'Service wifi: found'), ('0', 'Service wifi: found'),
                             ('202 303', 'Service wifi: found'),
                             ('12345678901', 'Service wifi: found'), ('202', 'unexpected')]:
            self.assertEqual(probe(pid=pid, service=service, address='    inet 10.0.2.16/24'),
                             (False, False))
        self.assertEqual(count(), 3, 'malformed identity must not submit a connection')
        self.assertEqual(probe(pid='202'), (False, False))

    def test_viewer_relay_selects_scrcpy_forward_and_fences_clients(self):
        endpoint = str(self.root / 'viewer-adb.sock')
        records, lock = [], threading.Lock()
        mapping = {'rows': [], 'media': 0, 'transport': [], 'target': []}

        def read_exact(source, size):
            data = bytearray()
            while len(data) < size:
                part = source.recv(size - len(data))
                if not part:
                    raise ConnectionError('closed')
                data.extend(part)
            return bytes(data)

        class Adb(socketserver.BaseRequestHandler):
            def handle(handler):
                try:
                    first = read_exact(handler.request, int(read_exact(handler.request, 4), 16)).decode()
                    with lock:
                        records.append(first)
                    if first == 'host:list-forward':
                        with lock:
                            payload = '\n'.join(mapping['rows']).encode()
                        handler.request.sendall(b'OKAY' + f'{len(payload):04x}'.encode() + payload)
                        return
                    self.assertEqual(first, 'host:transport:emulator-5554')
                    with lock:
                        transport = mapping['transport'].pop(0) if mapping['transport'] else 'okay'
                    if transport == 'eof':
                        return
                    if transport == 'fail':
                        handler.request.sendall(b'FAIL0004busy')
                        return
                    handler.request.sendall(b'OKAY')
                    second = read_exact(handler.request, int(read_exact(handler.request, 4), 16)).decode()
                    with lock:
                        records.append(second)
                    self.assertRegex(second, r'^localabstract:scrcpy_[0-9a-f]{8}$')
                    with lock:
                        target = mapping['target'].pop(0) if mapping['target'] else 'okay'
                    if target == 'eof':
                        return
                    if target == 'fail':
                        handler.request.sendall(b'FAIL0004busy')
                        return
                    if target == 'stall':
                        handler.request.sendall(b'OKAY')
                        handler.request.settimeout(5)
                        while handler.request.recv(65536):
                            pass
                        return
                    # scrcpy removes its ADB forward after the first socket receives
                    # this dummy byte, while audio/control handler threads may still
                    # need the selected device service.
                    with lock:
                        mapping['media'] += 1
                        ordinal = mapping['media']
                        if mapping['media'] == 1:
                            mapping['rows'] = []
                    handler.request.sendall(b'OKAY\0')
                    if mapping.get('tag_streams'):
                        handler.request.sendall(bytes([ordinal]))
                    while data := handler.request.recv(65536):
                        handler.request.sendall(data)
                except (ConnectionError, OSError):
                    pass

        server = socketserver.ThreadingUnixStreamServer(endpoint, Adb)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        relay = None
        clients = []
        try:
            # A relay must reject a forward belonging to another device, port, or
            # arbitrary device socket before it has selected a cached target.
            for kind in ('foreign', 'port', 'service', 'ambiguous'):
                rejected_relay = client.start_viewer_relay(endpoint, 'emulator-5554')
                rejected_port = rejected_relay.server_address[1]
                if kind == 'foreign':
                    row = 'emulator-5556 tcp:' + str(rejected_port) + ' localabstract:scrcpy_1234abcd'
                elif kind == 'port':
                    row = 'emulator-5554 tcp:1 localabstract:scrcpy_1234abcd'
                elif kind == 'service':
                    row = 'emulator-5554 tcp:' + str(rejected_port) + ' localabstract:not-scrcpy'
                else:
                    row = 'emulator-5554 tcp:' + str(rejected_port) + ' localabstract:scrcpy_1234abcd'
                mapping['rows'] = [row] * (2 if kind == 'ambiguous' else 1)
                rejected = socket.create_connection(('127.0.0.1', rejected_port), timeout=3)
                rejected.settimeout(3)
                self.assertEqual(rejected.recv(1), b'')
                self.assertIn('forward lookup', rejected_relay.failure)
                self.assertIn('viewer forward was not found', rejected_relay.failure)
                rejected.close()
                rejected_relay.shutdown()
                rejected_relay.server_close()

            self.assertEqual(records.count('host:list-forward'), 4)
            self.assertNotIn('host:transport:emulator-5554', records)

            with lock:
                records.clear()
            relay = client.start_viewer_relay(endpoint, 'emulator-5554')
            port = relay.server_address[1]
            with self.assertRaises(OSError):
                client.ViewerRelay(relay.server_address, client.ViewerHandler)
            mapping['rows'] = [
                'emulator-5556 tcp:' + str(port) + ' localabstract:scrcpy_ffffffff',
                'emulator-5554 tcp:1 localabstract:scrcpy_1234abcd',
                'emulator-5554 tcp:' + str(port) + ' localabstract:not-scrcpy',
                'emulator-5554 tcp:' + str(port) + ' localabstract:scrcpy_1234abcd',
            ]
            # scrcpy opens video, audio and control independently; each gets a fresh
            # capability-facade stream and receives the forward-mode dummy byte intact.
            for payload in (b'video', b'audio', b'control'):
                peer = socket.create_connection(('127.0.0.1', port), timeout=3)
                clients.append(peer)
                peer.settimeout(3)
                self.assertEqual(peer.recv(1), b'\0')
                peer.sendall(payload)
                self.assertEqual(peer.recv(len(payload)), payload)
            self.assertEqual(records.count('host:list-forward'), 1)
            self.assertEqual(records.count('host:transport:emulator-5554'), 3)
            self.assertEqual(records.count('localabstract:scrcpy_1234abcd'), 3)
            self.assertNotIn('host:transport:emulator-5556', records)
            self.assertNotIn('localabstract:scrcpy_ffffffff', records)

            # Force control's handler to run before audio's. Guest connections must
            # still open in TCP accept order, while established video keeps flowing.
            relay.shutdown()
            relay.server_close()
            relay = client.start_viewer_relay(endpoint, 'emulator-5554')
            port = relay.server_address[1]
            mapping.update(rows=['emulator-5554 tcp:' + str(port) + ' localabstract:scrcpy_1234abcd'],
                           media=0, tag_streams=True)
            video = socket.create_connection(('127.0.0.1', port), timeout=3)
            clients.append(video)
            self.assertEqual(read_exact(video, 2), b'\0\1')
            audio, control = socket.socket(), socket.socket()
            clients.extend((audio, control))
            for peer in (audio, control):
                peer.bind(('127.0.0.1', 0))
                peer.settimeout(3)
            audio_port, control_port = audio.getsockname()[1], control.getsockname()[1]
            release_audio, control_entered = threading.Event(), threading.Event()
            original_handle = client.ViewerHandler.handle

            def inverted_schedule(handler):
                if handler.client_address[1] == audio_port:
                    release_audio.wait(3)
                elif handler.client_address[1] == control_port:
                    control_entered.set()
                original_handle(handler)

            with patch.object(client.ViewerHandler, 'handle', inverted_schedule):
                try:
                    audio.connect(('127.0.0.1', port))
                    control.connect(('127.0.0.1', port))
                    self.assertTrue(control_entered.wait(2))
                    control.settimeout(0.2)
                    with self.assertRaises(socket.timeout):
                        control.recv(1)
                    video.sendall(b'video-still-flowing')
                    self.assertEqual(read_exact(video, 19), b'video-still-flowing')
                finally:
                    release_audio.set()
                control.settimeout(3)
                self.assertEqual(read_exact(audio, 2), b'\0\2')
                self.assertEqual(read_exact(control, 2), b'\0\3')
            mapping['tag_streams'] = False

            # A cold Android server can reject the first abstract-socket attempts.
            # The initial media stream retries only those native ADB FAIL responses.
            relay.shutdown()
            relay.server_close()
            relay = client.start_viewer_relay(endpoint, 'emulator-5554')
            port = relay.server_address[1]
            with lock:
                records.clear()
                mapping.update(rows=['emulator-5554 tcp:' + str(port) + ' localabstract:scrcpy_1234abcd'],
                               media=0, transport=[], target=['fail', 'fail', 'okay'])
            retry = socket.create_connection(('127.0.0.1', port), timeout=3)
            clients.append(retry)
            retry.settimeout(3)
            self.assertEqual(retry.recv(1), b'\0')
            retry.sendall(b'retry')
            self.assertEqual(retry.recv(5), b'retry')
            self.assertEqual(records.count('host:list-forward'), 1)
            self.assertEqual(records.count('host:transport:emulator-5554'), 3)
            self.assertEqual(records.count('localabstract:scrcpy_1234abcd'), 3)

            # Once media has started, a later stream must not retry an unavailable
            # service: scrcpy owns the stream ordering after its first connection.
            with lock:
                before = len(records)
                mapping['target'] = ['fail']
            later = socket.create_connection(('127.0.0.1', port), timeout=3)
            later.settimeout(3)
            self.assertEqual(later.recv(1), b'')
            later.close()
            self.assertEqual(len(records), before + 2)

            # A transport failure and any EOF are terminal. Only the explicit
            # native ADB FAIL on the first localabstract request is retryable.
            def terminal(stage, action):
                nonlocal relay
                relay.shutdown()
                relay.server_close()
                relay = client.start_viewer_relay(endpoint, 'emulator-5554')
                terminal_port = relay.server_address[1]
                with lock:
                    records.clear()
                    mapping.update(rows=['emulator-5554 tcp:' + str(terminal_port) + ' localabstract:scrcpy_1234abcd'],
                                   media=0, transport=[action] if stage == 'transport' else [],
                                   target=[action] if stage == 'target' else [])
                peer = socket.create_connection(('127.0.0.1', terminal_port), timeout=3)
                peer.settimeout(3)
                self.assertEqual(peer.recv(1), b'')
                peer.close()
                return records.copy()

            for stage, action in (('transport', 'fail'), ('transport', 'eof'), ('target', 'eof')):
                with self.subTest(stage=stage, action=action):
                    observed = terminal(stage, action)
                    self.assertEqual(observed.count('host:list-forward'), 1)
                    self.assertEqual(observed.count('host:transport:emulator-5554'), 1)
                    self.assertEqual(observed.count('localabstract:scrcpy_1234abcd'), stage == 'target')

            # An ADB OKAY does not establish media readiness. The first dummy byte
            # must arrive before the same bounded startup deadline.
            relay.shutdown()
            relay.server_close()
            with patch.object(client, 'VIEWER_START_TIMEOUT', 0.25):
                relay = client.start_viewer_relay(endpoint, 'emulator-5554')
                port = relay.server_address[1]
                with lock:
                    records.clear()
                    mapping.update(rows=['emulator-5554 tcp:' + str(port) + ' localabstract:scrcpy_1234abcd'],
                                   media=0, transport=[], target=['stall'])
                started = time.monotonic()
                stalled = socket.create_connection(('127.0.0.1', port), timeout=3)
                stalled.settimeout(2)
                self.assertEqual(stalled.recv(1), b'')
                stalled.close()
                self.assertLess(time.monotonic() - started, 1)
                self.assertEqual(records.count('localabstract:scrcpy_1234abcd'), 1)
                self.assertIn('initial media byte', relay.failure)
                self.assertIn('TimeoutError', relay.failure)

            # A permanent native FAIL honors the explicit first-media deadline.
            relay.shutdown()
            relay.server_close()
            with patch.object(client, 'VIEWER_START_TIMEOUT', 0.15):
                relay = client.start_viewer_relay(endpoint, 'emulator-5554')
                port = relay.server_address[1]
                with lock:
                    records.clear()
                    mapping.update(rows=['emulator-5554 tcp:' + str(port) + ' localabstract:scrcpy_1234abcd'],
                                   media=0, transport=[], target=['fail'] * 20)
                started = time.monotonic()
                permanent = socket.create_connection(('127.0.0.1', port), timeout=3)
                permanent.settimeout(3)
                self.assertEqual(permanent.recv(1), b'')
                permanent.close()
                self.assertLess(time.monotonic() - started, 1)
                self.assertGreater(records.count('localabstract:scrcpy_1234abcd'), 1)
                relay.shutdown()
                relay.server_close()

            # Closing the local client stops retrying before that deadline.
            with patch.object(client, 'VIEWER_START_TIMEOUT', 3):
                relay = client.start_viewer_relay(endpoint, 'emulator-5554')
                port = relay.server_address[1]
                with lock:
                    records.clear()
                    mapping.update(rows=['emulator-5554 tcp:' + str(port) + ' localabstract:scrcpy_1234abcd'],
                                   media=0, transport=[], target=['fail'] * 20)
                disconnected = socket.create_connection(('127.0.0.1', port), timeout=3)
                disconnected.close()
                deadline = time.monotonic() + 1
                while not records and time.monotonic() < deadline:
                    time.sleep(0.01)
                time.sleep(0.15)
                self.assertEqual(records.count('localabstract:scrcpy_1234abcd'), 1)

            relay.shutdown()
            relay.server_close()
            for peer in clients:
                self.assertEqual(peer.recv(1), b'')
                peer.close()
            clients.clear()
            relay = None
        finally:
            for peer in clients:
                with contextlib.suppress(OSError):
                    peer.close()
            if relay is not None:
                relay.shutdown()
                relay.server_close()
            server.shutdown()
            server.server_close()
            Path(endpoint).unlink(missing_ok=True)

    def test_tunnel_initial_ack_failure_cleans_process_and_tracking(self):
        token, allocation = self.acquire()

        class Process:
            def __init__(process):
                process.stdin, process.stdout = io.BytesIO(), io.BytesIO()
                process.terminated, process.waited = False, []

            def terminate(process):
                process.terminated = True

            def wait(process, timeout=None):
                process.waited.append(timeout)
                return 0

        class Connection:
            def __init__(connection):
                connection.closed = threading.Event()
                connection.shutdowns = 0

            def recv(connection, _size):
                connection.closed.wait(1)
                return b''

            def sendall(_connection, _data):
                raise BrokenPipeError('peer closed before tunnel acknowledgement')

            def shutdown(connection, _how):
                connection.shutdowns += 1
                connection.closed.set()

        process, connection = Process(), Connection()
        previous = self.runtime.connect
        self.runtime.connect = lambda _slot: process
        try:
            self.pool.tunnel(token, connection)
        finally:
            self.runtime.connect = previous
        self.assertTrue(process.terminated)
        self.assertEqual(process.waited, [3])
        self.assertTrue(process.stdin.closed and process.stdout.closed)
        self.assertEqual(connection.shutdowns, 1)
        self.assertFalse(self.pool.connections.get(allocation['slot_id']))

    def test_real_socket_client_payload_exit_and_revoked_stream(self):
        address = str(self.root / 'control.sock')
        server = poolmod.Server(address, poolmod.Handler)
        server.pool = self.pool
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        previous = client.CONTROL
        client.CONTROL = address
        try:
            # Actual subprocess gets the ADB context; its raw data crosses both real relays.
            payload = ('import os,socket; s=socket.socket(socket.AF_UNIX); '
                       's.connect(os.environ["ADB_SERVER_SOCKET"].split(":",1)[1]); '
                       's.sendall(b"hello"); assert s.recv(5)==b"hello"; '
                       'assert os.environ["ANDROID_SERIAL"]=="emulator-5554"; '
                       'raise SystemExit(23)')
            self.assertEqual(client.main(['run', '--', sys.executable, '-c', payload]), 23)
            self.assertFalse(self.runtime.running)
            token, _ = self.acquire()
            channel = client.connection(dict(operation='tunnel', token=token))
            client.response(channel)
            channel.sendall(b'hello')
            self.assertEqual(channel.recv(5), b'hello')
            self.pool.release(token)
            channel.settimeout(2)
            self.assertEqual(channel.recv(1), b'')
            channel.close()
            # Even host UID 0 cannot exercise admin over the public L2-mounted socket.
            with self.assertRaisesRegex(client.Error, 'authority'):
                client.rpc('drain')
        finally:
            client.CONTROL = previous
            server.shutdown()
            server.server_close()

    def serve_control(self):
        address = str(self.root / 'control.sock')
        server = poolmod.Server(address, poolmod.Handler)
        server.pool = self.pool
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        return address

    @staticmethod
    def refused(port):
        try:
            socket.create_connection(('127.0.0.1', port), timeout=2).close()
        except ConnectionRefusedError:
            return True
        return False

    def test_run_tcp_relay_serves_ddmlib_port_and_closes_with_release(self):
        address = self.serve_control()
        record = self.root / 'tcp-run.json'
        # ddmlib connects to 127.0.0.1:ANDROID_ADB_SERVER_PORT; ADB_SERVER_SOCKET stays for the adb CLI.
        payload = ('import json,os,socket,sys; port=int(os.environ["ANDROID_ADB_SERVER_PORT"]); '
                   's=socket.create_connection(("127.0.0.1",port)); s.sendall(b"hello"); data=s.recv(5); '
                   f'open({str(record)!r},"w").write(json.dumps(dict(port=port, data=data.decode(), '
                   'socket=os.environ["ADB_SERVER_SOCKET"], serial=os.environ["ANDROID_SERIAL"], '
                   'gradle=os.environ.get("GRADLE_OPTS")))); '
                   'raise SystemExit(23)')
        with patch.object(client, 'CONTROL', address), patch.dict(os.environ, GRADLE_OPTS='-Xmx1g'):
            self.assertEqual(client.main(['run', '--', sys.executable, '-c', payload]), 23)
        result = json.loads(record.read_text())
        self.assertEqual(result['data'], 'hello')
        # A Gradle daemon would outlive the lease; the caller's options are kept and extended.
        self.assertEqual(result['gradle'], '-Xmx1g -Dorg.gradle.daemon=false')
        self.assertEqual(client.gradle_without_daemon(None), dict(GRADLE_OPTS='-Dorg.gradle.daemon=false'))
        self.assertEqual(client.gradle_without_daemon('-Dorg.gradle.daemon=true'), {})
        self.assertEqual(client.gradle_without_daemon('-Xmx1g -Dorg.gradle.daemon'), {})
        # Only the exact property counts: a related daemon setting still gets the guard.
        self.assertEqual(client.gradle_without_daemon('-Dorg.gradle.daemon.idletimeout=60000'),
                         dict(GRADLE_OPTS='-Dorg.gradle.daemon.idletimeout=60000 -Dorg.gradle.daemon=false'))
        self.assertTrue(result['socket'].startswith('localfilesystem:'))
        self.assertEqual(result['serial'], 'emulator-5554')
        self.assertFalse(self.runtime.running)
        self.assertTrue(self.refused(result['port']))

    def test_relay_preflight_failure_stops_before_the_command(self):
        address = self.serve_control()
        marker = self.root / 'started'
        # An ADB server that never answers host:version (raw echo) must fail fast.
        self.runtime.connect = lambda slot: subprocess.Popen(['cat'], stdin=subprocess.PIPE, stdout=subprocess.PIPE)
        with patch.object(client, 'CONTROL', address), patch.object(client, 'ADB_PREFLIGHT_TIMEOUT', 2):
            with self.assertRaisesRegex(client.Error, 'host:version'):
                client.main(['run', '--', 'touch', str(marker)])
        self.assertFalse(marker.exists())
        self.assertFalse(self.runtime.running)
        self.assertTrue(all(slot['state'] == 'available' for slot in self.pool.status()['slots']))

    def test_loopback_peer_owner_is_required_for_the_tcp_relay(self):
        # Kernel tables print addresses as host-order words; IPv4-mapped IPv6 maps back to IPv4.
        self.assertEqual(client.proc_address('0100007F:1F90'), ('127.0.0.1', 8080))
        self.assertEqual(client.proc_address('0000000000000000FFFF00000100007F:0035'), ('127.0.0.1', 53))
        table = self.root / 'tcp'
        class Stream:
            def getsockname(self):
                return ('127.0.0.1', 5037)
            def getpeername(self):
                return ('127.0.0.1', 40000)
        header = '  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n'
        row = '   1: 0100007F:9C40 0100007F:13AD {state} 00000000:00000000 00:00000000 00000000  {uid}        0 {inode} 1\n'
        cases = [([dict(state='01', uid=1234, inode=77)], 1234),
                 # Server-side and timewait rows never identify the connecting owner.
                 ([dict(state='06', uid=0, inode=0), dict(state='01', uid=1234, inode=77)], 1234),
                 ([dict(state='01', uid=1234, inode=77), dict(state='01', uid=0, inode=78)], None),
                 ([], None)]
        for rows, expected in cases:
            table.write_text(header + ''.join(row.format(**values) for values in rows))
            self.assertEqual(client.loopback_peer_uid(Stream(), (str(table), str(self.root / 'missing'))), expected)
        # The real kernel table resolves this process as the owner of its loopback connection.
        listener = client.tcp_listener()
        try:
            own = socket.create_connection(listener.getsockname(), timeout=2)
            accepted, _ = listener.accept()
            try:
                self.assertEqual(client.loopback_peer_uid(accepted), os.getuid())
            finally:
                own.close()
                accepted.close()
        finally:
            listener.close()
        listener = client.tcp_listener()
        port = listener.getsockname()[1]
        tunnels = []
        def tunnel(request, timeout=400):
            tunnels.append(request['operation'])
            raise client.Error('transport', 'fixture tunnel')
        with patch.object(client, 'connection', side_effect=tunnel):
            relay = client.start_tcp_relay('a' * 64, listener)
            try:
                with socket.create_connection(('127.0.0.1', port), timeout=2) as own:
                    own.settimeout(2)
                    self.assertEqual(own.recv(1), b'')
                # Another local account's connection is closed before any lease tunnel opens.
                with patch.object(client, 'loopback_peer_uid', return_value=os.getuid() + 1), \
                        contextlib.redirect_stderr(io.StringIO()) as output:
                    with socket.create_connection(('127.0.0.1', port), timeout=2) as foreign:
                        foreign.settimeout(2)
                        self.assertEqual(foreign.recv(1), b'')
                self.assertIn('another account', output.getvalue())
            finally:
                relay.shutdown()
                relay.server_close()
        self.assertEqual(tunnels, ['tunnel'])
        self.assertTrue(self.refused(port))

    def test_acquire_retains_tcp_relay_until_release(self):
        address = self.serve_control()
        lease = self.root / 'tcp-lease.json'
        environment = dict(os.environ, ANDROID_PUBLIC_ROOT=str(self.root))
        result = subprocess.run([sys.executable, '-B', str(ROOT / 'config/profiles/android/client.py'), 'acquire',
                                 '--lease-file', str(lease)], env=environment, capture_output=True, timeout=30)
        self.assertEqual(result.returncode, 0, result.stderr.decode())
        printed = json.loads(result.stdout)
        port = int(printed['environment']['ANDROID_ADB_SERVER_PORT'])
        self.assertEqual(client.read_lease(lease)['adb_port'], port)
        self.assertTrue(printed['environment']['ADB_SERVER_SOCKET'].startswith('localfilesystem:'))
        with socket.create_connection(('127.0.0.1', port), timeout=5) as stream:
            stream.sendall(b'hello')
            stream.settimeout(5)
            self.assertEqual(stream.recv(5), b'hello')
        result = subprocess.run([sys.executable, '-B', str(ROOT / 'config/profiles/android/client.py'), 'release',
                                 '--lease-file', str(lease)], env=environment, capture_output=True, timeout=30)
        self.assertEqual(result.returncode, 0, result.stderr.decode())
        endpoint = Path(printed['environment']['ADB_SERVER_SOCKET'].split(':', 1)[1])
        deadline = time.monotonic() + 10
        while (endpoint.exists() or not self.refused(port)) and time.monotonic() < deadline:
            time.sleep(0.2)
        self.assertTrue(self.refused(port))
        self.assertFalse(endpoint.exists())

    def test_viewer_reports_signed_child_status_and_releases_its_lease(self):
        address = str(self.root / 'control.sock')
        server = poolmod.Server(address, poolmod.Handler)
        server.pool = self.pool
        threading.Thread(target=server.serve_forever, daemon=True).start()
        original_spawn = subprocess.Popen
        try:
            for program, child_status in (
                ('raise SystemExit(139)', 139),
                ('import os, resource, signal; resource.setrlimit(resource.RLIMIT_CORE, (0, 0)); '
                 'os.kill(os.getpid(), signal.SIGSEGV)', -11),
            ):
                with self.subTest(child_status=child_status):
                    def spawn(command, *args, **kwargs):
                        if command[0] == 'scrcpy':
                            command = [sys.executable, '-I', '-B', '-c', program]
                        return original_spawn(command, *args, **kwargs)
                    with patch.object(client, 'CONTROL', address), \
                            patch.object(client.shutil, 'which', return_value='/fixture/tool'), \
                            patch.object(client.subprocess, 'Popen', side_effect=spawn), \
                            contextlib.redirect_stderr(io.StringIO()) as output:
                        self.assertEqual(client.main(['view']), 139)
                    self.assertEqual(output.getvalue(), f'Android viewer: child returncode {child_status}\n')
                    self.assertFalse(self.runtime.running)
                    self.assertTrue(all(slot['state'] == 'available' for slot in self.pool.status()['slots']))
        finally:
            server.shutdown()
            server.server_close()

    def test_remote_viewer_uses_private_wire_and_keeps_attached_lease(self):
        address = str(self.root / 'control.sock')
        server = poolmod.Server(address, poolmod.Handler)
        server.pool = self.pool
        threading.Thread(target=server.serve_forever, daemon=True).start()
        original_spawn = subprocess.Popen
        config = json.dumps(dict(public_root=str(self.root), state_root=str(self.root)))
        wire_command = ('import importlib.util; from unittest.mock import patch; '
                        f'spec=importlib.util.spec_from_file_location("wire_client", {str(ROOT / "config/profiles/android/client.py")!r}); '
                        'module=importlib.util.module_from_spec(spec); spec.loader.exec_module(module); '
                        f'config={config!r}; '
                        'guard=patch.object(module.Path,"read_text",return_value=config); guard.start(); module.wire()')
        transport = [sys.executable, '-I', '-B', '-c', wire_command]
        def spawn(command, *args, **kwargs):
            if command[0] == 'scrcpy':
                self.assertEqual(kwargs['env']['DISPLAY'], ':fixture')
                self.assertEqual(kwargs['env']['ANDROID_SERIAL'], 'emulator-5554')
                command = [sys.executable, '-I', '-B', '-c', 'raise SystemExit(23)']
            else:
                self.assertEqual(command, transport)
            return original_spawn(command, *args, **kwargs)
        try:
            with patch.dict(os.environ, SUBYARD_RESOURCE_SESSION_TRANSPORT=json.dumps(transport),
                            SUBYARD_EMU_INSTANCE='must-not-use-owner-incus', DISPLAY=':fixture'), \
                    patch.object(client.shutil, 'which', return_value='/fixture/tool'), \
                    patch.object(client.subprocess, 'Popen', side_effect=spawn), \
                    contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(client.main(['view']), 23)
                self.assertTrue(all(slot['state'] == 'available' for slot in self.pool.status()['slots']))
                token, allocation = self.acquire()
                lease_file = self.root / 'remote-lease.json'
                client.save_lease(lease_file, dict(token=token))
                # The controller cannot read the remote path; only the wire subprocess does.
                with patch.object(client, 'read_lease', side_effect=AssertionError('local lease read')):
                    self.assertEqual(client.main(['view', '--lease-file', str(lease_file)]), 23)
                self.assertEqual(self.pool.allocation(token)['state'], 'held')
                self.assertEqual(self.pool.allocation(token)['generation'], allocation['generation'])
                os.chmod(lease_file, 0o644)
                with self.assertRaisesRegex(client.Error, 'private regular file'):
                    client.main(['view', '--lease-file', str(lease_file)])
                self.assertEqual(self.pool.allocation(token)['state'], 'held')
        finally:
            server.shutdown()
            server.server_close()

    def test_missing_viewer_tools_and_invalid_remote_path_do_not_reserve(self):
        with patch.object(client, 'rpc', side_effect=AssertionError('pool operation')), \
                patch.object(client.shutil, 'which', return_value=None):
            with self.assertRaisesRegex(client.Error, 'scrcpy must be installed'):
                client.main(['view'])
        with patch.dict(os.environ, SUBYARD_RESOURCE_SESSION_TRANSPORT='["ssh"]'), \
                contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit) as error:
                client.validate(['view', '--lease-file', 'relative.json'])
            self.assertEqual(error.exception.code, 2)

    def test_authorized_revoke_fences_tunnel_and_retries_quarantine(self):
        public = str(self.root / 'control.sock')
        admin = str(self.root / 'admin.sock')
        server = poolmod.Server(public, poolmod.Handler)
        server.pool = self.pool
        admin_server = poolmod.Server(admin, poolmod.Handler)
        admin_server.pool, admin_server.admin = self.pool, True
        threading.Thread(target=server.serve_forever, daemon=True).start()
        threading.Thread(target=admin_server.serve_forever, daemon=True).start()
        previous = client.CONTROL
        channel = None
        try:
            first, allocation = self.acquire(dict(device='phone', api=35))
            second, _ = self.acquire(dict(device='tablet', api=36))
            client.CONTROL = public
            channel = client.connection(dict(operation='tunnel', token=first))
            client.response(channel)
            channel.sendall(b'hello')
            self.assertEqual(channel.recv(5), b'hello')
            client.CONTROL = admin
            with patch.object(poolmod.struct, 'unpack', return_value=(0, 1000, 0)):
                with self.assertRaisesRegex(client.Error, 'authority'):
                    client.rpc('revoke', slot=allocation['slot_id'])
            self.assertEqual(self.pool.allocation(first)['state'], 'held')
            client.CONTROL = public
            channel.sendall(b'still-held')
            self.assertEqual(channel.recv(10), b'still-held')
            client.CONTROL = admin
            # The actual admin Unix server and request are used; only peer credentials
            # are made root because this host-free test runs unprivileged.
            with patch.object(poolmod.struct, 'unpack', return_value=(0, 0, 0)):
                self.assertEqual(client.rpc('revoke', slot=allocation['slot_id']), dict(stopped=True))
            channel.settimeout(2)
            self.assertEqual(channel.recv(1), b'')
            with self.assertRaises(poolmod.PoolError) as error:
                self.pool.allocation(first)
            self.assertEqual(error.exception.code, 'stale')
            self.assertEqual(self.pool.allocation(second)['state'], 'held')
            replacement, replaced = self.acquire(dict(device='phone', api=35))
            self.assertEqual(replaced['slot_id'], allocation['slot_id'])
            self.runtime.fail_stop = True
            with patch.object(poolmod.struct, 'unpack', return_value=(0, 0, 0)):
                with self.assertRaisesRegex(client.Error, 'quarantined'):
                    client.rpc('revoke', slot=replaced['slot_id'])
            self.assertEqual(self.pool.slots[0]['state'], 'quarantined')
            self.assertEqual(self.pool.allocation(second)['state'], 'held')
            self.runtime.fail_stop = False
            with patch.object(poolmod.struct, 'unpack', return_value=(0, 0, 0)):
                self.assertEqual(client.rpc('revoke', slot=replaced['slot_id']), dict(stopped=True))
            self.assertEqual(self.pool.slots[0]['state'], 'available')
            with self.assertRaises(poolmod.PoolError) as error:
                self.pool.allocation(replacement)
            self.assertEqual(error.exception.code, 'stop_unconfirmed')
            self.assertEqual(self.pool.allocation(second)['state'], 'held')
        finally:
            if channel is not None:
                with contextlib.suppress(OSError):
                    channel.close()
            client.CONTROL = previous
            self.runtime.fail_stop = False
            server.shutdown()
            server.server_close()
            admin_server.shutdown()
            admin_server.server_close()

    def test_signal_releases_owned_lease_and_stops_viewer_command_group(self):
        address = str(self.root / 'control.sock')
        server = poolmod.Server(address, poolmod.Handler)
        server.pool = self.pool
        threading.Thread(target=server.serve_forever, daemon=True).start()
        marker = self.root / 'descendant'
        payload = ('import subprocess,time,pathlib; p=subprocess.Popen(["sleep","60"]); '
                   f'pathlib.Path({str(marker)!r}).write_text(str(p.pid)); time.sleep(60)')
        tools = self.root / 'tools'
        tools.mkdir()
        (tools / 'adb').write_text('#!/bin/sh\nexit 0\n')
        (tools / 'scrcpy').write_text('#!' + sys.executable + '\n' + payload + '\n')
        for tool in tools.iterdir():
            tool.chmod(0o700)
        environment = dict(os.environ, ANDROID_PUBLIC_ROOT=str(self.root),
                           PATH=str(tools) + ':' + os.environ['PATH'])
        try:
            for mode in ('run', 'standalone-view', 'attached-view'):
                with self.subTest(mode=mode):
                    marker.unlink(missing_ok=True)
                    if mode == 'run':
                        arguments = ['run', '--', sys.executable, '-c', payload]
                    elif mode == 'standalone-view':
                        arguments = ['view']
                    else:
                        token, allocation = self.acquire()
                        lease_path = self.root / 'attached-cancel.json'
                        client.save_lease(lease_path, dict(token=token))
                        arguments = ['view', '--lease-file', str(lease_path)]
                        before = self.pool.allocation(token)
                        lease_before = lease_path.read_bytes()
                    command = subprocess.Popen([sys.executable, str(ROOT / 'config/profiles/android/client.py'),
                                                *arguments], env=environment,
                                               stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                    try:
                        deadline = time.monotonic() + 5
                        # The payload writes the marker non-atomically; an empty read means not ready yet.
                        while not (marker.exists() and marker.read_text()) and command.poll() is None \
                                and time.monotonic() < deadline:
                            time.sleep(0.02)
                        self.assertTrue(marker.exists())
                        pid = int(marker.read_text())
                        command.send_signal(signal.SIGTERM)
                        _, stderr = command.communicate(timeout=8)
                        self.assertEqual(command.returncode, 143, stderr.decode())
                        if mode == 'attached-view':
                            self.assertEqual(self.pool.allocation(token), before)
                            self.assertEqual(lease_path.read_bytes(), lease_before)
                            self.pool.release(token)
                        self.assertFalse(self.runtime.running)
                        self.assertTrue(all(slot['state'] == 'available' for slot in self.pool.status()['slots']))
                        process = Path(f'/proc/{pid}/stat')
                        if process.exists():
                            self.assertEqual(process.read_text().split(') ', 1)[1].split()[0], 'Z')
                    finally:
                        if command.poll() is None:
                            command.kill()
                            command.communicate()
        finally:
            server.shutdown()
            server.server_close()

    def test_run_stops_before_renew_when_requester_identity_changes(self):
        address = str(self.root / 'control.sock')
        server = poolmod.Server(address, poolmod.Handler)
        server.pool = self.pool
        threading.Thread(target=server.serve_forever, daemon=True).start()
        marker = self.root / 'requester-ended'
        payload = f'import pathlib,time; pathlib.Path({str(marker)!r}).touch(); time.sleep(30)'
        renewals = []
        original = self.pool.allocation
        original_spawn = subprocess.Popen
        def delayed_spawn(*args, **kwargs):
            # Initial provisioning may legitimately renew before the requester changes.
            renewals.clear()
            command = original_spawn(*args, **kwargs)
            time.sleep(1.2)
            return command
        def allocation(token, renew=False):
            if renew:
                renewals.append(token)
            return original(token, renew)
        try:
            with patch.object(client, 'CONTROL', address), patch.object(client, 'HEARTBEAT', 0.01), \
                 patch.object(client, 'requester_identity', side_effect=lambda pid: 'reused' if marker.exists() else 'original'), \
                 patch.object(client.subprocess, 'Popen', side_effect=delayed_spawn), \
                 patch.object(self.pool, 'allocation', side_effect=allocation):
                started = time.monotonic()
                with self.assertRaisesRegex(client.Error, 'renewal failed'):
                    client.main(['run', '--', sys.executable, '-c', payload])
                self.assertLess(time.monotonic() - started, 5)
            self.assertEqual(renewals, [])
            self.assertFalse(self.runtime.running)
        finally:
            server.shutdown()
            server.server_close()

    def test_standalone_viewer_renews_but_attached_viewer_does_not(self):
        address = str(self.root / 'control.sock')
        server = poolmod.Server(address, poolmod.Handler)
        server.pool = self.pool
        threading.Thread(target=server.serve_forever, daemon=True).start()
        original_rpc, original_spawn = client.rpc, subprocess.Popen
        operations = []
        def rpc(operation, **kwargs):
            operations.append(operation)
            return original_rpc(operation, **kwargs)
        def spawn(command, *args, **kwargs):
            if command[0] == 'scrcpy':
                command = [sys.executable, '-c', 'import time; time.sleep(1.2)']
            return original_spawn(command, *args, **kwargs)
        try:
            with patch.object(client, 'CONTROL', address), patch.object(client, 'HEARTBEAT', 0), \
                 patch.object(client, 'rpc', side_effect=rpc), \
                 patch.object(client.shutil, 'which', return_value='/fixture/scrcpy'), \
                 patch.object(client.subprocess, 'Popen', side_effect=spawn):
                self.assertEqual(client.main(['view']), 0)
                self.assertIn('renew', operations)
                self.assertFalse(self.runtime.running)
                token, allocation = self.acquire()
                lease_file = self.root / 'attached.json'
                client.save_lease(lease_file, dict(token=token))
                operations.clear()
                self.assertEqual(client.main(['view', '--lease-file', str(lease_file)]), 0)
                self.assertNotIn('renew', operations)
                self.assertNotIn('release', operations)
                self.assertEqual(self.pool.allocation(token)['generation'], allocation['generation'])
                self.now += 1201
                self.pool.reap()
                self.assertFalse(self.runtime.running)
        finally:
            server.shutdown()
            server.server_close()

    def test_wait_retries_only_busy_and_requester_cancellation_stops_retry(self):
        args = SimpleNamespace(wait=30)
        with patch.object(client, 'attribution', return_value=self.owner), \
             patch.object(client, 'rpc', side_effect=client.Error('unavailable', 'quarantined')) as rpc:
            with self.assertRaises(client.Error) as failed:
                client.acquire(args, secrets.token_hex(32))
            self.assertEqual(failed.exception.code, 'unavailable')
            self.assertEqual(rpc.call_count, 1)
        alive = iter([True, False])
        with patch.object(client, 'attribution', return_value=self.owner), \
             patch.object(client.time, 'sleep'), \
             patch.object(client, 'rpc', side_effect=client.Error('busy', 'occupied')) as rpc:
            with self.assertRaises(client.Error) as failed:
                client.acquire(args, secrets.token_hex(32), lambda: next(alive))
            self.assertEqual(failed.exception.code, 'owner')
            self.assertEqual(rpc.call_count, 1)

    def test_busy_wait_timeout_and_command_not_started(self):
        self.acquire()
        self.acquire()
        address = str(self.root / 'control.sock')
        server = poolmod.Server(address, poolmod.Handler)
        server.pool = self.pool
        threading.Thread(target=server.serve_forever, daemon=True).start()
        previous, client.CONTROL = client.CONTROL, address
        try:
            marker = self.root / 'payload'
            with self.assertRaisesRegex(client.Error, 'timed out'):
                client.main(['run', '--wait', '1', '--', 'touch', str(marker)])
            self.assertFalse(marker.exists())
            self.assertEqual(len(self.runtime.running), 2)
        finally:
            client.CONTROL = previous
            server.shutdown()
            server.server_close()

    def test_image_policy_selects_latest_stable_revision_independent_of_order(self):
        images = poolmod.Images(self.root / 'policy-images', self.root / 'sdk')
        def package(major, channel='channel-0', preview=''):
            return f"""<remotePackage path="system-images;android-35;google_apis;x86_64">
                <revision><major>{major}</major>{preview}</revision><channelRef ref="{channel}"/>
                <archives><archive><complete><url>fixture.zip</url><size>16</size>
                <checksum>{'a' * 40}</checksum></complete></archive></archives></remotePackage>"""
        (images.root / 'google_apis.xml').write_text('<repository>' + package(2) + package(1)
            + package(9, 'channel-1') + package(10, preview='<preview>1</preview>') + '</repository>')
        selected = images.resolve(dict(api=35, variant='google_apis', abi='x86_64'))
        self.assertEqual(selected['revision'], '2.0.0')
        with self.assertRaisesRegex(poolmod.PoolError, 'unsupported Android image policy'):
            poolmod.Images(self.root / 'invalid-policy', self.root / 'sdk', image_policy='preview')

    @patch.object(poolmod.shutil, 'disk_usage', return_value=SimpleNamespace(free=64 * 1024**3))
    def test_sdk_installer_failure_diagnostics_are_bounded_private_and_atomic(self, _disk):
        sdk = self.root / 'diagnostic-sdk'
        (sdk / 'licenses').mkdir(parents=True)
        (sdk / 'licenses/android-sdk-license').write_text('accepted\n')
        installer = sdk / 'cmdline-tools/latest/bin/sdkmanager'
        installer.parent.mkdir(parents=True)
        image = dict(key='a' * 64, package='system-images;android-36;google_apis;x86_64',
                     revision='1.0.0', size=16, sha1='b' * 40)
        images = poolmod.Images(self.root / 'diagnostic-images', sdk)
        real_popen, calls = subprocess.Popen, []
        def spawn(args, **kwargs):
            process = real_popen(args, **kwargs)
            calls.append((process, args, kwargs))
            return process
        for known in (True, False):
            with self.subTest(known=known):
                installer.write_text(f'#!{sys.executable}\n' + """
import os, pathlib, sys, time
target = pathlib.Path(sys.argv[1].removeprefix('--sdk_root='))
assert sys.argv[2] == 'system-images;android-36;google_apis;x86_64'
assert (target / 'licenses/android-sdk-license').read_text() == 'accepted\\n'
assert os.environ['HOME'] == str(target.parent / 'home')
assert os.environ['TMPDIR'] == str(target.parent / 'tmp')
partial = target.joinpath(*sys.argv[2].split(';'))
partial.mkdir(parents=True)
(partial / 'system.img').write_bytes(b'partial')
os.write(1, b'private-token /private/path 192.0.2.17 ' +
         (b'java.net.UnknownHost' if KNOWN else b'private-unknown'))
time.sleep(.3)
os.write(2, b'Exception\\n' if KNOWN else b'private-unknown\\n')
os.write(1, b'Z' * 100000)
os.write(2, b'java.net.ConnectException\\n')
os.write(1, b'Z' * 100000)
os.write(2, (b'No space left on device' if KNOWN else b'private-unknown') + b' private-token\\n')
raise SystemExit(7)
""".replace('KNOWN', repr(known)))
                installer.chmod(0o755)
                with patch.object(poolmod.subprocess, 'Popen', side_effect=spawn):
                    with self.assertRaises(poolmod.PoolError) as raised:
                        images.ensure(image, threading.Event())
                message = str(raised.exception)
                self.assertIn('observed=' + ('dns,no_space' if known else 'unknown'), message)
                self.assertIn('exit 7', message)
                self.assertIn('free_bytes=' + str(64 * 1024**3), message)
                self.assertIn('truncated=1', message)
                for private in ('private-token', '/private/path', '192.0.2.17', str(self.root)):
                    self.assertNotIn(private, message)
                self.assertFalse(images.cached(image))
                self.assertFalse(list(images.root.glob('download-*')))
                process, args, kwargs = calls[-1]
                self.assertEqual(args[0], str(installer))
                self.assertEqual(args[2], image['package'])
                self.assertEqual(kwargs['stdin'], subprocess.DEVNULL)
                self.assertEqual(kwargs['stdout'], subprocess.PIPE)
                self.assertEqual(kwargs['stderr'], subprocess.STDOUT)
                self.assertTrue(kwargs['start_new_session'])
                self.assertTrue(process.stdout.closed)
                self.assertEqual(process.returncode, 7)

    @patch.object(poolmod.shutil, 'disk_usage', return_value=SimpleNamespace(free=64 * 1024**3))
    def test_image_os_and_value_errors_retain_only_bounded_failure_facts(self, _disk):
        sdk = self.root / 'failed-metadata-sdk'
        installer = sdk / 'cmdline-tools/latest/bin/sdkmanager'
        installer.parent.mkdir(parents=True)
        image = dict(key='a' * 64, package='system-images;android-36;google_apis;x86_64',
                     revision='1.0.0', size=16, sha1='b' * 40)
        images = poolmod.Images(self.root / 'failed-metadata-images', sdk)
        for failure in ('sdk-start', 'metadata-missing', 'metadata-invalid'):
            with self.subTest(failure=failure):
                if failure != 'sdk-start':
                    installer.write_text(f'#!{sys.executable}\n' + """
import os
os.write(1, b'private-token https://private.example/image?token=secret /private/path ' * 1000)
os.write(1, b'java.net.SocketTimeoutException checksum mismatch\\n')
""")
                    installer.chmod(0o755)
                metadata = (patch.object(poolmod.Path, 'read_text', side_effect=ValueError('private-token /private/path'))
                            if failure == 'metadata-invalid' else contextlib.nullcontext())
                with metadata, self.assertRaises(poolmod.PoolError) as raised:
                    images.ensure(image, threading.Event())
                error = raised.exception
                message = str(error)
                self.assertEqual(error.code, 'download')
                self.assertLess(len(message), 512)
                self.assertIsInstance(error.__cause__, ValueError if failure == 'metadata-invalid' else FileNotFoundError)
                self.assertIn('phase=' + ('sdk-start' if failure == 'sdk-start' else 'metadata'), message)
                self.assertIn('exception=' + type(error.__cause__).__name__, message)
                if failure == 'metadata-invalid':
                    self.assertNotIn('errno=', message)
                else:
                    self.assertIn('errno=2', message)
                if failure == 'sdk-start':
                    self.assertNotIn('sdk_exit=', message)
                else:
                    self.assertIn('sdk_exit=0 sdk_tail=network_timeout,checksum', message)
                for private in ('private-token', 'private.example', 'secret', '/private/path', str(self.root)):
                    self.assertNotIn(private, message)
                self.assertFalse(images.cached(image))
                self.assertFalse(list(images.root.glob('download-*')))

    @patch.object(poolmod.shutil, 'disk_usage', return_value=SimpleNamespace(free=64 * 1024**3))
    def test_noisy_sdk_installer_cancellation_still_kills_reaps_and_removes_stage(self, _disk):
        sdk = self.root / 'noisy-sdk'
        installer = sdk / 'cmdline-tools/latest/bin/sdkmanager'
        installer.parent.mkdir(parents=True)
        installer.write_text(f'#!{sys.executable}\nimport os\nwhile True: os.write(1, b"private-token" * 16384)\n')
        installer.chmod(0o755)
        image = dict(key='a' * 64, package='system-images;android-36;google_apis;x86_64',
                     revision='1.0.0', size=16, sha1='b' * 40)
        images = poolmod.Images(self.root / 'noisy-images', sdk)
        class CancelAfterOutput:
            def __init__(self):
                self.reads = 0
            def is_set(self):
                return self.reads >= 3
            def wait(self, seconds):
                if seconds == 0:
                    self.reads += 1
                else:
                    time.sleep(seconds)
                return self.is_set()
        real_popen, processes = subprocess.Popen, []
        def spawn(*args, **kwargs):
            process = real_popen(*args, **kwargs)
            processes.append(process)
            return process
        started = time.monotonic()
        with patch.object(poolmod.subprocess, 'Popen', side_effect=spawn):
            with self.assertRaisesRegex(poolmod.PoolError, 'allocation cancelled'):
                images.ensure(image, CancelAfterOutput())
        self.assertLess(time.monotonic() - started, 3)
        self.assertLess(processes[0].returncode, 0)
        self.assertTrue(processes[0].stdout.closed)
        self.assertFalse(images.cached(image))
        self.assertFalse(list(images.root.glob('download-*')))

    @patch.object(poolmod.shutil, 'disk_usage', return_value=SimpleNamespace(free=64 * 1024**3))
    def test_single_download_atomic_pin_prune_and_redownload(self, _disk_usage):
        sdk = self.root / 'sdk'
        (sdk / 'licenses').mkdir(parents=True)
        (sdk / 'licenses/android-sdk-license').write_text('accepted\n')
        installer = sdk / 'cmdline-tools/latest/bin/sdkmanager'
        installer.parent.mkdir(parents=True)
        installer.write_text(f'#!{sys.executable}\n' + """
import pathlib, sys
source = pathlib.Path(__file__).resolve().parents[3]
target = pathlib.Path(sys.argv[1].removeprefix('--sdk_root='))
if not (target / 'licenses/android-sdk-license').exists():
    raise SystemExit(7)
count = source / 'downloads'
count.write_text(str(int(count.read_text()) + 1 if count.exists() else 1))
image = target.joinpath(*sys.argv[2].split(';'))
image.mkdir(parents=True)
(image / 'system.img').write_bytes(b'base-image')
(image / 'source.properties').write_text('AndroidVersion.ApiLevel=35\\nSystemImage.Abi=x86_64\\nSystemImage.TagId=google_apis\\nPkg.Revision=1\\n')
""")
        installer.chmod(0o755)
        images = poolmod.Images(self.root / 'real-images', sdk)
        (images.root / 'google_apis.xml').write_text(f'''<repository>
            <remotePackage path="system-images;android-35;google_apis;x86_64">
              <revision><major>1</major></revision>
              <archives><archive><complete><url>fixture.zip</url><size>16</size>
              <checksum>{'a' * 40}</checksum>
              </complete></archive></archives>
            </remotePackage></repository>''')
        self.pool.images = images
        token1 = secrets.token_hex(32)
        token2 = secrets.token_hex(32)
        self.pool.reserve(dict(api=35), token1, self.owner)
        self.pool.reserve(dict(api=35, device='tablet'), token2, self.owner)
        deadline = time.monotonic() + 3
        while self.pool.workers and time.monotonic() < deadline:
            time.sleep(0.01)
        self.assertEqual(self.pool.allocation(token1)['state'], 'held')
        self.assertEqual(self.pool.allocation(token2)['state'], 'held')
        self.assertEqual((sdk / 'downloads').read_text(), '1')
        self.assertEqual(len(self.pool.prune(False)['skipped']), 1)
        self.pool.release(token1)
        self.assertEqual(len(self.pool.prune(False)['skipped']), 1)
        self.pool.release(token2)
        self.assertEqual(len(self.pool.prune(True)['candidates']), 1)
        self.assertEqual(len(self.pool.prune(False)['removed']), 1)
        third, _ = self.acquire(dict(api=35))
        self.assertEqual((sdk / 'downloads').read_text(), '2')
        self.pool.release(third)
        self.pool.prune(False)
        # The native installer's license/download failure never publishes a partial image.
        image = images.resolve(dict(api=35, variant='google_apis', abi='x86_64'))
        (sdk / 'licenses/android-sdk-license').unlink()
        with self.assertRaisesRegex(poolmod.PoolError, r'exit 7.*licensing'):
            images.ensure(image, threading.Event())
        self.assertFalse(images.cached(image))
        self.assertFalse(list(images.root.glob('download-*')))
        _disk_usage.return_value.free = 1024**3
        with self.assertRaisesRegex(poolmod.PoolError, 'insufficient space'):
            images.ensure(image, threading.Event())
        self.assertFalse(list(images.root.glob('download-*')))

    @patch.object(poolmod.shutil, 'disk_usage', return_value=SimpleNamespace(free=64 * 1024**3))
    def test_cache_prepare_is_idle_only_and_reuses_verified_images(self, _disk_usage):
        sdk = self.root / 'sdk-prepare'
        (sdk / 'licenses').mkdir(parents=True)
        license = sdk / 'licenses/android-sdk-license'
        license.write_text('accepted\n')
        installer = sdk / 'cmdline-tools/latest/bin/sdkmanager'
        installer.parent.mkdir(parents=True)
        installer.write_text(f'#!{sys.executable}\n' + """
import pathlib, sys
source = pathlib.Path(__file__).resolve().parents[3]
target = pathlib.Path(sys.argv[1].removeprefix('--sdk_root='))
if not (target / 'licenses/android-sdk-license').exists():
    raise SystemExit(7)
count = source / 'downloads'
count.write_text(str(int(count.read_text()) + 1 if count.exists() else 1))
parts = sys.argv[2].split(';')
image = target.joinpath(*parts)
image.mkdir(parents=True)
(image / 'system.img').write_bytes(b'base-image')
(image / 'source.properties').write_text('AndroidVersion.ApiLevel=' + parts[1][8:] + '\\nSystemImage.Abi=' + parts[3] + '\\nSystemImage.TagId=' + parts[2] + '\\nPkg.Revision=1\\n')
""")
        installer.chmod(0o755)
        images = poolmod.Images(self.root / 'prepared-images', sdk)
        packages = ''.join(f'''<remotePackage path="system-images;android-{api};google_apis;x86_64">
              <revision><major>1</major></revision>
              <archives><archive><complete><url>fixture.zip</url><size>16</size>
              <checksum>{'a' * 40}</checksum>
              </complete></archive></archives>
            </remotePackage>''' for api in (35, 36))
        (images.root / 'google_apis.xml').write_text('<repository>' + packages + '</repository>')
        self.pool.images = images
        initial = [slot['generation'] for slot in self.pool.slots]
        self.assertEqual(self.pool.prepare(dict(api=35)),
                         dict(api=35, variant='google_apis', abi='x86_64', revision='1.0.0', cached=True))
        self.assertEqual((sdk / 'downloads').read_text(), '1')
        self.assertFalse(self.runtime.running)
        self.assertEqual([slot['generation'] for slot in self.pool.slots], initial)
        address = str(self.root / 'prepare.sock')
        server = poolmod.Server(address, poolmod.Handler)
        server.pool = self.pool
        threading.Thread(target=server.serve_forever, daemon=True).start()
        previous, client.CONTROL = client.CONTROL, address
        try:
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(client.main(['cache', 'prepare', '--api', '35']), 0)
            with contextlib.redirect_stdout(io.StringIO()):
                client.assessment(['cache', 'prepare', '--api', '35'])
        finally:
            client.CONTROL = previous
            server.shutdown()
            server.server_close()
        self.assertEqual((sdk / 'downloads').read_text(), '1')
        token, _ = self.acquire(dict(api=35))
        with self.assertRaisesRegex(poolmod.PoolError, 'idle'):
            self.pool.prepare(dict(api=36))
        self.pool.release(token)
        license.unlink()
        with self.assertRaises(poolmod.PoolError):
            self.pool.prepare(dict(api=36))
        self.assertFalse(list(images.root.glob('download-*')))
        license.write_text('accepted\n')
        entered, finish, failures = threading.Event(), threading.Event(), []
        original = images.ensure
        def blocked(image, cancel):
            entered.set()
            finish.wait(2)
            return original(image, cancel)
        images.ensure = blocked
        thread = threading.Thread(target=lambda: self._prepare(self.pool, dict(api=36), failures))
        thread.start()
        self.assertTrue(entered.wait(2))
        with self.assertRaisesRegex(poolmod.PoolError, 'maintenance is in progress'):
            self.pool.reserve({}, secrets.token_hex(32), self.owner)
        finish.set()
        thread.join(3)
        self.assertEqual(failures, [])
        token, _ = self.acquire(dict(api=36))
        self.pool.release(token)

    @staticmethod
    def _prepare(pool, request, failures):
        try:
            pool.prepare(request)
        except poolmod.PoolError as exc:
            failures.append(exc.code)

    @patch.object(poolmod.shutil, 'disk_usage', return_value=SimpleNamespace(free=64 * 1024**3))
    def test_sdk_install_is_add_only_verified_licensed_and_lease_compatible(self, _disk):
        sdk = self.root / 'shared-sdk'
        # A current default, an earlier default with metadata and one provisioning has not repaired yet.
        # android-34 r03 is a vendored retired entry; android-30 has no vendored entry at all.
        for name, metadata in (('android-37.2', True), ('android-36', True), ('android-34', False), ('android-30', False)):
            component = sdk / 'platforms' / name
            component.mkdir(parents=True)
            (component / 'source.properties').write_text('Pkg.Revision=2\nAndroidVersion.ApiLevel=' + name[8:] + '\n')
            archive = '1f2e9478d6a7601425ceaa553311dc43191f103d' if name == 'android-34' else 'a' * 40
            (component / '.subyard-toolchain.json').write_text(json.dumps(dict(archive=archive)) + '\n')
            if metadata:
                (component / 'package.xml').write_text('<repository><localPackage path="platforms;' + name + '"/></repository>')
        (sdk / 'licenses').mkdir()
        (sdk / 'licenses/android-sdk-license').write_text('\n' + 'b' * 40)
        installer = sdk / 'cmdline-tools/latest/bin/sdkmanager'
        installer.parent.mkdir(parents=True)
        (sdk / 'cmdline-tools/latest/source.properties').write_text('Pkg.Revision=23.0\n')
        calls = self.root / 'sdk-calls'
        installer.write_text(f'#!{sys.executable}\n' + f"""
import os, pathlib, sys
target = pathlib.Path(sys.argv[1].removeprefix('--sdk_root='))
with open({str(calls)!r}, 'a') as log:
    log.write(' '.join(sys.argv[2:]) + '\\n')
assert os.environ['HOME'] == str(target.parent / 'home') and os.environ['TMPDIR'] == str(target.parent / 'tmp')
if os.environ.get('FIXTURE_SDK_LINGER'):
    # Like the Android CLI's metrics upload: a helper outliving the installer writes into HOME.
    import subprocess
    subprocess.Popen([sys.executable, '-c', 'import os, pathlib, time; time.sleep(1); '
                      'pathlib.Path(os.environ["HOME"], "late").mkdir(parents=True)'])
if os.environ.get('FIXTURE_SDK_FAIL'):
    target.joinpath('platforms', 'android-33').mkdir(parents=True)
    raise SystemExit(7)
for package in sys.argv[2:]:
    directory = target.joinpath(*package.split(';'))
    directory.mkdir(parents=True)
    declared = 'cmake;9.9.9' if package == 'cmake;3.22.1' else package
    (directory / 'package.xml').write_text('<repository><localPackage path="' + declared + '"/></repository>')
    revision = package.split(';')[1].removeprefix('android-')
    metadata = 'Pkg.Revision=' + revision + '\\n'
    if package.startswith('platforms;'):
        metadata = 'Pkg.Revision=1\\nAndroidVersion.ApiLevel=' + revision + '\\n'
    (directory / 'source.properties').write_text(metadata)
    tool = directory / 'tool'
    tool.write_text('#!/bin/sh\\n')
    tool.chmod(0o700)
licenses = target / 'licenses'
licenses.mkdir()
(licenses / 'android-sdk-license').write_text('\\n' + 'c' * 40)
""")
        installer.chmod(0o755)
        packages = poolmod.SdkPackages(sdk, sdk_lock=self.config['sdk_lock'])
        self.pool.sdk_packages = packages
        def invocations():
            return calls.read_text().splitlines() if calls.exists() else []
        # Every invalid identifier is reported at once, before any SDK manager run.
        with self.assertRaises(poolmod.PoolError) as refused:
            self.pool.sdk_install(['bad', 'platforms;android-36.1', 'system-images;android-36;google_apis;x86_64',
                                   'build-tools;1.2', 'emulator'])
        self.assertEqual(refused.exception.code, 'request')
        message = str(refused.exception)
        self.assertLess(message.index("'bad'"), message.index("'system-images"))
        self.assertLess(message.index("'system-images"), message.index("'build-tools;1.2'"))
        self.assertLess(message.index("'build-tools;1.2'"), message.index("'emulator'"))
        self.assertNotIn("'platforms;android-36.1'", message)
        self.assertEqual(invocations(), [])
        # An add-only install does not wait for, or disturb, a held emulator lease.
        token, allocation = self.acquire()
        with contextlib.redirect_stderr(io.StringIO()) as log, patch.dict(os.environ, FIXTURE_SDK_LINGER='1'):
            result = self.pool.sdk_install(['platforms;android-36.1', 'build-tools;37.0.0-rc2', 'platforms;android-36'])
        self.assertEqual(result, dict(installed=['platforms;android-36.1', 'build-tools;37.0.0-rc2'],
                                      already_installed=['platforms;android-36'], licenses=['android-sdk-license'],
                                      accepted_for=['platforms;android-36.1', 'build-tools;37.0.0-rc2']))
        self.assertEqual(log.getvalue(), 'Android: accepted Google Android SDK licenses for '
                         'platforms;android-36.1, build-tools;37.0.0-rc2\n')
        self.assertEqual(invocations(), ['platforms;android-36.1 build-tools;37.0.0-rc2'])
        self.assertEqual(self.pool.allocation(token)['generation'], allocation['generation'])
        self.assertEqual(poolmod.SdkPackages.revision(sdk / 'platforms/android-36'), '2')
        self.assertEqual((sdk / 'licenses/android-sdk-license').read_text(), '\n' + 'b' * 40 + '\n' + 'c' * 40)
        published = sdk / 'build-tools/37.0.0-rc2'
        self.assertEqual(published.stat().st_mode & 0o777, 0o755)
        self.assertEqual((published / 'tool').stat().st_mode & 0o777, 0o755)
        self.assertEqual((published / 'source.properties').stat().st_mode & 0o777, 0o644)
        time.sleep(1.5)
        self.assertFalse((sdk / '.subyard-staging').exists())
        self.pool.release(token)
        # Repeats are no-ops; existing unrecognized paths are never replaced.
        self.assertEqual(self.pool.sdk_install(['platforms;android-36.1'])['already_installed'], ['platforms;android-36.1'])
        (sdk / 'ndk/27.0.12077973').mkdir(parents=True)
        (sdk / 'ndk/27.0.12077973/user-file').write_text('keep')
        with self.assertRaisesRegex(poolmod.PoolError, 'refusing replacement: ndk;27.0.12077973'):
            self.pool.sdk_install(['ndk;27.0.12077973'])
        self.assertEqual((sdk / 'ndk/27.0.12077973/user-file').read_text(), 'keep')
        # Independent refusals are reported together, in stable order, before any installer run.
        (sdk / '.subyard-staging').write_text('not a directory')
        _disk.return_value = SimpleNamespace(free=1024**3)
        before = invocations()
        with self.assertRaises(poolmod.PoolError) as refused:
            self.pool.sdk_install(['ndk;27.0.12077973', 'platforms;android-34', 'platforms;android-30', 'platforms;android-33'])
        _disk.return_value = SimpleNamespace(free=64 * 1024**3)
        (sdk / '.subyard-staging').unlink()
        self.assertEqual(refused.exception.code, 'ownership')
        parts = str(refused.exception).split('; ')
        self.assertEqual(len(parts), 5, parts)
        self.assertIn('repair them with yard provision android: platforms;android-34', parts[0])
        # Without a vendored entry provisioning cannot repair it; it is only kept.
        self.assertEqual(parts[1], 'profile-installed SDK packages are kept without package metadata and are not '
                         'replaced: platforms;android-30')
        self.assertIn('refusing replacement: ndk;27.0.12077973', parts[2])
        self.assertIn('staging path is not a pool-owned directory', parts[3])
        self.assertIn('insufficient space', parts[4])
        self.assertEqual(invocations(), before)
        self.assertFalse((sdk / 'platforms/android-34/package.xml').exists())
        # A failed publication reports exactly the packages that became visible.
        original_rename = Path.rename
        def rename(path, target):
            if str(target).endswith('build-tools/34.0.0'):
                raise OSError('fixture rename failure')
            return original_rename(path, target)
        with patch.object(poolmod.Path, 'rename', rename), contextlib.redirect_stderr(io.StringIO()) as log:
            with self.assertRaisesRegex(poolmod.PoolError, 'published before the failure: platforms;android-31$'):
                self.pool.sdk_install(['platforms;android-31', 'build-tools;34.0.0'])
        self.assertEqual(log.getvalue(), 'Android: accepted Google Android SDK licenses for platforms;android-31\n')
        self.assertTrue(self.pool.sdk_packages.installed('platforms;android-31'))
        self.assertFalse((sdk / 'build-tools/34.0.0').exists())
        self.assertFalse((sdk / '.subyard-staging').exists())
        # Provisioning and image preparation hold the SDK lock exclusively; another install is serialized.
        with open(self.config['sdk_lock'], 'a') as exclusive:
            poolmod.fcntl.flock(exclusive, poolmod.fcntl.LOCK_EX)
            with self.assertRaisesRegex(poolmod.PoolError, 'maintenance is in progress'):
                self.pool.sdk_install(['platforms;android-35'])
        with packages.lock:
            with self.assertRaisesRegex(poolmod.PoolError, 'another Android SDK package installation'):
                self.pool.sdk_install(['platforms;android-35'])
        # Installer failure or a mismatched package publishes nothing.
        with patch.dict(os.environ, FIXTURE_SDK_FAIL='1'):
            with self.assertRaisesRegex(poolmod.PoolError, 'exit 7'):
                self.pool.sdk_install(['platforms;android-33'])
        with self.assertRaisesRegex(poolmod.PoolError, 'does not match'):
            self.pool.sdk_install(['cmake;3.22.1', 'platforms;android-35'])
        for missing in ('platforms/android-33', 'platforms/android-35', 'cmake/3.22.1'):
            self.assertFalse((sdk / missing).exists())
        self.assertFalse((sdk / '.subyard-staging').exists())
        # list separates provisioned defaults (receipt) from added packages, also over the client.
        listed = {item['package']: item for item in self.pool.sdk_list()['packages']}
        self.assertEqual(listed['platforms;android-37.2'], dict(package='platforms;android-37.2', revision='2',
                                                                source='default', metadata=True))
        self.assertEqual(listed['platforms;android-36']['source'], 'previous-default')
        self.assertEqual(listed['platforms;android-34'], dict(package='platforms;android-34', revision='2',
                                                              source='previous-default', metadata=False))
        self.assertTrue(listed['platforms;android-36.1']['metadata'])
        self.assertEqual(listed['platforms;android-36.1']['source'], 'added')
        self.assertEqual(listed['build-tools;37.0.0-rc2']['source'], 'added')
        self.assertEqual(listed['cmdline-tools;latest']['source'], 'added')
        self.assertNotIn('ndk;27.0.12077973', listed)
        address = self.serve_control()
        with patch.object(client, 'CONTROL', address):
            with contextlib.redirect_stdout(io.StringIO()) as output:
                self.assertEqual(client.main(['sdk', 'list']), 0)
            self.assertEqual(json.loads(output.getvalue())['packages'], self.pool.sdk_list()['packages'])
            with contextlib.redirect_stdout(io.StringIO()) as output, contextlib.redirect_stderr(io.StringIO()) as log:
                self.assertEqual(client.main(['sdk', 'install', 'platforms;android-35']), 0)
            self.assertEqual(json.loads(output.getvalue())['installed'], ['platforms;android-35'])
            self.assertIn('Android: accepted Google Android SDK licenses for platforms;android-35\n', log.getvalue())
            with self.assertRaisesRegex(client.Error, 'unsupported SDK package identifiers'):
                client.main(['sdk', 'install', 'add-ons;addon-google_apis-google-24'])
        # The singleton owner removes only its interrupted marker-owned staging.
        interrupted = sdk / '.subyard-staging' / ('install-' + 'c' * 24)
        interrupted.mkdir(parents=True)
        poolmod.atomic(interrupted / '.install-owner.json', dict(schema=poolmod.SCHEMA, kind='sdk-install'))
        unknown = sdk / '.subyard-staging' / ('install-' + 'd' * 24)
        unknown.mkdir()
        (unknown / 'foreign').write_text('preserve')
        poolmod.SdkPackages(sdk)
        self.assertFalse(interrupted.exists())
        self.assertTrue((unknown / 'foreign').exists())

    def test_prune_only_managed_unused_images(self):
        root = self.root / 'images'
        images = poolmod.Images(root, self.root / 'sdk')
        key = 'a' * 64
        image = dict(key=key, package='system-images;android-35;google_apis;x86_64', revision='1')
        directory = root / key
        (directory / 'image').mkdir(parents=True)
        (directory / 'image/system.img').write_bytes(b'base')
        poolmod.atomic(directory / 'owned.json', image)
        foreign = root / ('b' * 64)
        foreign.mkdir()
        (foreign / 'user-data').write_text('retained')
        self.assertEqual(len(images.prune(set(), True)['candidates']), 1)
        self.assertTrue(directory.exists())
        self.assertEqual(images.prune({key}, False)['skipped'], [image['package'] + '@1'])
        self.assertTrue(directory.exists())
        result = images.prune(set(), False)
        self.assertEqual(len(result['removed']), 1)
        self.assertFalse(directory.exists())
        self.assertTrue((foreign / 'user-data').exists())
        self.assertEqual(images.prune(set(), False)['bytes'], 0)

        interrupted = root / ('download-' + 'c' * 24)
        interrupted.mkdir()
        poolmod.atomic(interrupted / '.download-owner.json',
                       dict(schema=poolmod.SCHEMA, kind='image-download'))
        (interrupted / 'image.zip').write_bytes(b'partial')
        unknown = root / ('download-' + 'd' * 24)
        unknown.mkdir()
        (unknown / 'foreign').write_text('preserve')
        empty = root / ('download-' + 'e' * 24)
        empty.mkdir()
        poolmod.Images(root, self.root / 'sdk')
        self.assertFalse(interrupted.exists())
        self.assertFalse(empty.exists())
        self.assertTrue(unknown.exists())


if __name__ == '__main__':
    unittest.main()
