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
        return subprocess.Popen(['cat'], stdin=subprocess.PIPE, stdout=subprocess.PIPE)


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

    def test_two_slots_busy_redaction_and_fencing(self):
        first, one = self.acquire(dict(device='phone', api=34))
        second, two = self.acquire(dict(device='tablet', api=35))
        self.assertNotEqual(one['slot_id'], two['slot_id'])
        with self.assertRaisesRegex(poolmod.PoolError, 'occupied'):
            self.acquire()
        public = json.dumps(self.pool.status())
        self.assertNotIn(first, public)
        self.assertNotIn('credential', public)
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
        self.now += 590
        self.pool.allocation(token, renew=True)
        self.now += 590
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
             patch.object(poolmod.Path, 'glob', side_effect=AssertionError('software inspected render nodes')), \
             patch.object(poolmod, 'run', return_value=result) as command:
            runtime.preflight(dict(device='phone'), [], dict(slot_id='001'))
        calls = [arguments.args[0] for arguments in command.call_args_list]
        self.assertTrue(any('/dev/kvm' in call for call in calls))
        self.assertFalse(any('check-graphics' in call for call in calls))

    def test_emulator_ram_floors_admit_pair_and_reject_insufficient_memory(self):
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
            runtime.preflight(dict(device='phone'), [dict(device='tablet')], dict(slot_id='001'))

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
                runtime.preflight(dict(device='tablet'), [dict(device='tablet')], dict(slot_id='001'))

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
        command.write_text('#!/bin/sh\nprintf "%s\\n" "$*" > "$CONNECT_CALL"\nexit "$CONNECT_EXIT"\n')
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
        command.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$SUBMISSIONS"\n')
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
        def probe(pid='101', service='Service wifi: found', address=''):
            env = dict(os.environ, FRAMEWORK_PID=pid, WIFI_SERVICE=service,
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

        self.assertEqual(probe(), (False, True))
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
            for kind in ('foreign', 'port', 'service'):
                rejected_relay = client.start_viewer_relay(endpoint, 'emulator-5554')
                rejected_port = rejected_relay.server_address[1]
                if kind == 'foreign':
                    row = 'foreign tcp:' + str(rejected_port) + ' localabstract:scrcpy_1234abcd'
                elif kind == 'port':
                    row = 'emulator-5554 tcp:1 localabstract:scrcpy_1234abcd'
                else:
                    row = 'emulator-5554 tcp:' + str(rejected_port) + ' localabstract:not-scrcpy'
                mapping['rows'] = [row]
                rejected = socket.create_connection(('127.0.0.1', rejected_port), timeout=3)
                rejected.settimeout(3)
                self.assertEqual(rejected.recv(1), b'')
                self.assertIn('forward lookup', rejected_relay.failure)
                self.assertIn('viewer forward was not found', rejected_relay.failure)
                rejected.close()
                rejected_relay.shutdown()
                rejected_relay.server_close()

            self.assertEqual(records.count('host:list-forward'), 3)
            self.assertNotIn('host:transport:emulator-5554', records)

            with lock:
                records.clear()
            relay = client.start_viewer_relay(endpoint, 'emulator-5554')
            port = relay.server_address[1]
            with self.assertRaises(OSError):
                client.ViewerRelay(relay.server_address, client.ViewerHandler)
            mapping['rows'] = [
                'foreign tcp:' + str(port) + ' localabstract:scrcpy_1234abcd',
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
                            patch.object(client.subprocess, 'Popen', side_effect=spawn), \
                            contextlib.redirect_stderr(io.StringIO()) as output:
                        self.assertEqual(client.main(['view']), 139)
                    self.assertEqual(output.getvalue(), f'Android viewer: child returncode {child_status}\n')
                    self.assertFalse(self.runtime.running)
                    self.assertTrue(all(slot['state'] == 'available' for slot in self.pool.status()['slots']))
        finally:
            server.shutdown()
            server.server_close()

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

    def test_signal_releases_lease_and_stops_command_group(self):
        address = str(self.root / 'control.sock')
        server = poolmod.Server(address, poolmod.Handler)
        server.pool = self.pool
        threading.Thread(target=server.serve_forever, daemon=True).start()
        marker = self.root / 'descendant'
        payload = ('import subprocess,time,pathlib; p=subprocess.Popen(["sleep","60"]); '
                   f'pathlib.Path({str(marker)!r}).write_text(str(p.pid)); time.sleep(60)')
        environment = dict(os.environ, ANDROID_PUBLIC_ROOT=str(self.root))
        command = subprocess.Popen([sys.executable, str(ROOT / 'config/profiles/android/client.py'),
                                    'run', '--', sys.executable, '-c', payload], env=environment,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        try:
            deadline = time.monotonic() + 5
            while not marker.exists() and command.poll() is None and time.monotonic() < deadline:
                time.sleep(0.02)
            self.assertTrue(marker.exists())
            pid = int(marker.read_text())
            command.send_signal(signal.SIGTERM)
            _, stderr = command.communicate(timeout=8)
            self.assertEqual(command.returncode, 143, stderr.decode())
            self.assertFalse(self.runtime.running)
            process = Path(f'/proc/{pid}/stat')
            if process.exists():
                self.assertEqual(process.read_text().split(') ', 1)[1].split()[0], 'Z')
        finally:
            if command.poll() is None:
                command.kill()
                command.communicate()
            server.shutdown()
            server.server_close()

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
