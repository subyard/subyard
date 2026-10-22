#!/usr/bin/env python3
import contextlib
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import threading
import unittest
from unittest import mock

PROFILE = Path(__file__).resolve().parents[1]
ROOT = PROFILE.parents[2]
sys.path.insert(0, str(PROFILE))
sys.path.insert(0, str(ROOT/'internal/adapters/configmaterial'))
import native
import runtime
import tomli_w


class RuntimeTest(unittest.TestCase):

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='subyard-openlit-runtime-', dir='/tmp')
        self.addCleanup(self.tmp.cleanup)
        self.source = Path(self.tmp.name)/'source'
        self.state = Path(self.tmp.name)/'state'
        self.home = Path(self.tmp.name)/'home'
        self.unit = Path(self.tmp.name)/'subyard-openlit.service'
        self.source.mkdir(mode=0o700)
        self.home.mkdir(mode=0o700)
        for name in (*runtime.FILES, 'subyard-openlit.service'):
            shutil.copyfile(ROOT/'internal/adapters/configmaterial/tomli_w.py' if name == 'tomli_w.py' else PROFILE/name, self.source/name)
        self.args = (self.state, self.source, self.unit, self.home, True, ['claude', 'codex'], 'dev')
        self.expected_images = {name: 'sha256:'+str(i)*64 for i, name in enumerate(sorted(runtime.SERVICES), 1)}
        self.image_output = None
        self.rows = [{'Id': str(i+3)*64, 'Image': self.expected_images[name],
                      'Config': {'Labels': {'com.docker.compose.service': name, 'com.docker.compose.config-hash': str(i)*64},
                                 'Env': ['PRIVATE=synthetic-unexported'], 'User': '10001:10001' if name == 'collector' else ''},
                      'State': {'Status': 'running', **({} if name == 'collector' else {'Health': {'Status': 'healthy'}})}}
                     for i, name in enumerate(sorted(runtime.SERVICES), 1)]
        next(row for row in self.rows if row['Config']['Labels']['com.docker.compose.service'] == 'openlit')['Mounts'] = [
            {'Destination': '/app/client/data', 'Type': 'volume', 'Name': 'subyard-openlit_openlit-data'}]
        self.mounted = {name: (self.source/asset).read_bytes() for name, (asset, _) in runtime.MOUNTS.items()}
        self.service_active = True
        self.service_enabled = True
        self.timer_active = True
        self.timer_enabled = True
        self.probe_failure = False
        self.collector_unavailable = False
        self.trace_status = b'405'
        self.account_unready = False
        self.commands = []
        original = runtime.run
        self.native_run = original
        self.addCleanup(setattr, runtime, 'run', original)
        runtime.run = self.probe

    def install(self):
        self.state.mkdir(mode=0o700, exist_ok=True)
        native.write(self.state/'marker', runtime.MARKER+'\n')
        native.write(self.state/'database.env', 'OPENLIT_DB_PASSWORD=synthetic-private\n')
        native.write(self.state/'trace-cutover.env', 'OPENLIT_TRACE_CUTOVER_ID=none\n')
        self.publish_assets()
        native.reconcile(self.home, self.state/'native.json', ['claude', 'codex'], 'apply', os.getuid(), os.getgid(), tomli_w)

    def publish_assets(self):
        for name in runtime.FILES:
            native.write(self.state/name, (self.source/name).read_text())
        (self.state/'collector.yaml').chmod(0o644)
        native.write(self.unit, (self.source/'subyard-openlit.service').read_text())
        self.unit.chmod(0o644)
        for name in ('subyard-openlit-trace-cutover.service', 'subyard-openlit-trace-cutover.timer'):
            native.write(self.unit.with_name(name), (self.source/name).read_text())
            self.unit.with_name(name).chmod(0o644)

    def recreate(self):
        for i, row in enumerate(self.rows, 7):
            row['Id'] = format(i, 'x')*64
            row['Image'] = self.expected_images[row['Config']['Labels']['com.docker.compose.service']]
        self.mounted = {name: (self.source/asset).read_bytes() for name, (asset, _) in runtime.MOUNTS.items()}

    def probe(self, arguments, timeout=None, input=None):
        self.commands.append(arguments)
        if self.probe_failure:
            raise ValueError('synthetic native probe failed')
        if arguments[0] == 'systemctl':
            timer = 'subyard-openlit-trace-cutover.timer' in arguments
            active = self.timer_active if timer else self.service_active
            enabled = self.timer_enabled if timer else self.service_enabled
            return ('ActiveState='+('active' if active else 'inactive')+'\nUnitFileState='+('enabled' if enabled else 'disabled')+'\n').encode()
        if arguments[:2] == ['docker', 'ps']:
            return '\n'.join(row['Id'] for row in self.rows).encode()
        if arguments[:2] == ['docker', 'inspect']:
            return json.dumps(self.rows).encode()
        if arguments[:3] == ['docker', 'image', 'inspect']:
            self.assertEqual(arguments[3:5], ['--format', '{{.Id}}'])
            self.assertEqual(arguments[5:], ['fixture/'+name+'@sha256:'+str(i)*64
                                            for i, name in enumerate(sorted(runtime.SERVICES), 1)])
            return self.image_output if self.image_output is not None else ('\n'.join(self.expected_images[name] for name in sorted(runtime.SERVICES))+'\n').encode()
        if arguments[:3] == ['docker', 'volume', 'inspect']:
            return json.dumps([{'Name': 'subyard-openlit_openlit-data', 'Labels': {
                'com.docker.compose.project': 'subyard-openlit', 'com.docker.compose.volume': 'openlit-data'}}]).encode()
        if arguments[:2] == ['docker', 'compose']:
            if arguments[-2:] == ['--format', 'json']:
                # This private environment must never enter observation output or CAS projection.
                return json.dumps({'services': {name: {'image': 'fixture/'+name+'@sha256:'+str(i)*64,
                    'environment': {'PASSWORD': 'synthetic-private'}} for i, name in enumerate(sorted(runtime.SERVICES), 1)}}).encode()
            if arguments[-2:] == ['--hash', '*']:
                return '\n'.join(name+' '+str(i)*64 for i, name in enumerate(sorted(runtime.SERVICES), 1)).encode()
        if arguments[:2] == ['docker', 'cp']:
            container, destination = arguments[2].split(':', 1)
            name = next(row['Config']['Labels']['com.docker.compose.service'] for row in self.rows if row['Id'] == container)
            self.assertEqual(destination, runtime.MOUNTS[name][1])
            mounted = self.mounted[name]
            out = io.BytesIO()
            with tarfile.open(fileobj=out, mode='w') as tar:
                member = tarfile.TarInfo('config.yaml')
                member.size = len(mounted)
                tar.addfile(member, io.BytesIO(mounted))
            return out.getvalue()
        if arguments[0] == 'curl':
            if arguments[-1] == 'http://127.0.0.1:13133/' and self.collector_unavailable:
                raise ValueError('synthetic collector health unavailable')
            if arguments[-1].startswith('http://127.0.0.1:4318/v1/'):
                return self.trace_status if arguments[-1].endswith('/traces') else b'405'
            return b'OK'
        if arguments[:2] == ['python3', str(self.source/'account.py')]:
            self.assertEqual(timeout, 10)
            self.assertEqual(input, next(row['Id'].encode('ascii') for row in self.rows
                                       if row['Config']['Labels']['com.docker.compose.service'] == 'openlit'))
            if self.account_unready:
                raise ValueError('synthetic account linkage drift')
            return b'{"kind":"fresh","phase":"ready"}\n'
        raise AssertionError(arguments)

    def test_absent_is_read_only(self):
        observed = runtime.observe(*self.args)
        self.assertEqual(observed['state'], 'stale')
        self.assertNotEqual(observed['actual'], observed['desired'])
        self.assertEqual(len(observed['desired']), 64)
        self.assertFalse(self.state.exists())
        self.assertEqual(self.commands, [])



    def test_image_batch_runs_alone_after_overlapping_live_reads(self):
        self.install()
        lock, mounted = threading.Lock(), threading.Barrier(2)
        active, completed = set(), set()
        image_done = threading.Event()
        peak = 0
        def probe(arguments, timeout=None, input=None):
            nonlocal peak
            command = tuple(arguments)
            if arguments[:3] == ['docker', 'image', 'inspect']:
                with lock:
                    self.assertFalse(active, 'image batch overlapped another capture probe')
                    self.assertEqual(len(completed), 14, 'image batch preceded a pending live read')
                result = self.probe(arguments, timeout, input)
                image_done.set()
                return result
            if arguments[0] == 'python3':
                self.assertTrue(image_done.is_set(), 'account ran before image validation')
                with lock:
                    self.assertFalse(active)
                return self.probe(arguments, timeout, input)
            with lock:
                active.add(command)
                peak = max(peak, len(active))
            try:
                if arguments[:2] == ['docker', 'cp']:
                    mounted.wait(2)
                return self.probe(arguments, timeout, input)
            finally:
                with lock:
                    active.remove(command)
                    completed.add(command)
        runtime.run = probe
        for operation in (runtime.publish, runtime.observe):
            with self.subTest(operation=operation.__name__):
                completed.clear()
                image_done.clear()
                self.commands.clear()
                self.assertEqual(operation(*self.args)['state'], 'current')
                self.assertEqual(sum(command[:3] == ['docker', 'image', 'inspect'] for command in self.commands), 1)
        self.assertGreaterEqual(peak, 2)
        self.assertLessEqual(peak, 4)

    def test_waiting_for_pending_failures_preserves_image_validation_precedence(self):
        self.install()
        runtime.publish(*self.args)
        receipt = (self.state/'receipt.json').read_bytes()
        def probe(arguments, timeout=None, input=None):
            if arguments[:2] == ['docker', 'cp']:
                raise ValueError('synthetic mounted read failure')
            if arguments[:3] == ['docker', 'image', 'inspect']:
                self.commands.append(arguments)
                raise subprocess.TimeoutExpired(arguments, 5)
            return self.probe(arguments, timeout, input)
        runtime.run = probe
        self.commands.clear()
        observation, facts = runtime.capture(*self.args)
        self.assertEqual(observation['state'], 'stale')
        self.assertTrue(facts['probe_failed'])
        self.assertNotIn('configuration_matches', facts)
        self.assertTrue(any(command[:3] == ['docker', 'image', 'inspect'] for command in self.commands))
        self.assertFalse(any(command[0] == 'python3' for command in self.commands))
        with self.assertRaises(ValueError):
            runtime.publish(*self.args)
        self.assertEqual((self.state/'receipt.json').read_bytes(), receipt)

    def test_shipped_openlit_healthcheck_requires_each_signal(self):
        rendered = json.loads(subprocess.check_output(['docker', 'compose', '--env-file', '/dev/null',
            '-f', str(PROFILE/'compose.yaml'), 'config', '--format', 'json'],
            env={**os.environ, 'OPENLIT_DB_PASSWORD': 'synthetic'}))
        check = rendered['services']['openlit']['healthcheck']['test']
        self.assertEqual(check[:3], ['CMD', 'node', '-e'])
        endpoints = ['http://localhost:3000', *('http://localhost:4318/v1/'+name for name in ('logs', 'metrics', 'traces'))]
        for failed in (None, *endpoints):
            statuses = dict(zip(endpoints, (200, 405, 405, 405)))
            if failed:
                statuses[failed] = 503
            script = 'globalThis.fetch=async(url)=>{const status='+json.dumps(statuses)+'[url];if(status===undefined)throw Error("unexpected endpoint");return {ok:status===200,status};};'
            with self.subTest(failed=failed):
                result = subprocess.run(['node', '-e', script+check[3]], capture_output=True, timeout=5)
                self.assertEqual(result.returncode, 1 if failed else 0, result.stderr)

    def test_receiver_account_and_volume_drift_refuse_receipts(self):
        self.install()
        runtime.publish(*self.args)
        receipt = (self.state/'receipt.json').read_bytes()
        row = next(row for row in self.rows if row['Config']['Labels']['com.docker.compose.service'] == 'openlit')
        for drift in ('receiver', 'account', 'volume'):
            self.trace_status = b'404' if drift == 'receiver' else b'405'
            self.account_unready = drift == 'account'
            row['Mounts'][0]['Name'] = 'foreign-volume' if drift == 'volume' else 'subyard-openlit_openlit-data'
            self.commands.clear()
            with self.subTest(drift=drift), self.assertRaises(ValueError):
                runtime.publish(*self.args)
            self.assertEqual((self.state/'receipt.json').read_bytes(), receipt)
            if drift != 'account':
                self.assertFalse(any(command[0] == 'python3' for command in self.commands))
            self.assertFalse(any(command[0] in ('claude', 'codex') for command in self.commands))

    def test_unexpected_inventory_is_stale_without_archive_or_account_reads(self):
        self.install()
        runtime.publish(*self.args)
        receipt = (self.state/'receipt.json').read_bytes()
        expected = list(self.rows)
        for rows in ([], [*expected, dict(expected[0], Id='f'*64)]):
            with self.subTest(containers=len(rows)):
                self.rows = rows
                self.commands.clear()
                self.assertEqual(runtime.observe(*self.args)['state'], 'stale')
                with self.assertRaises(ValueError):
                    runtime.publish(*self.args)
                self.assertEqual((self.state/'receipt.json').read_bytes(), receipt)
                self.assertFalse(any(command[:2] == ['docker', 'cp'] or command[0] == 'python3'
                                     for command in self.commands))

    def test_image_probe_failures_preserve_receipt_account_gate_and_bounds(self):
        self.install()
        runtime.publish(*self.args)
        receipt = (self.state/'receipt.json').read_bytes()
        ids = [self.expected_images[name] for name in sorted(runtime.SERVICES)]
        outputs = [b'\xff', '\n'.join(ids[:2]).encode(), '\n'.join([*ids, ids[0]]).encode(),
                   '\n'.join([ids[0], 'sha256:'+'A'*64, ids[2]]).encode()]
        outcomes = [subprocess.CompletedProcess([], 0, stdout=value) for value in outputs]
        outcomes += [subprocess.CompletedProcess([], 7, stdout=b'private-sentinel'),
                     subprocess.TimeoutExpired(['docker'], 5)]
        def probe(arguments, timeout=None, input=None):
            if arguments[:3] == ['docker', 'image', 'inspect']:
                return self.native_run(arguments, timeout=timeout, input=input)
            return self.probe(arguments, timeout, input)
        runtime.run = probe
        for outcome in outcomes:
            with self.subTest(outcome=type(outcome).__name__), mock.patch.object(runtime.subprocess, 'run',
                    side_effect=outcome if isinstance(outcome, Exception) else None,
                    return_value=outcome) as native_probe:
                self.commands.clear()
                with self.assertRaises(ValueError):
                    runtime.publish(*self.args)
                native_probe.assert_called_once()
                self.assertEqual(native_probe.call_args.kwargs,
                                 {'input': None, 'capture_output': True, 'timeout': 5, 'check': False})
                self.assertEqual(native_probe.call_args.args[0][:5], ['docker', 'image', 'inspect', '--format', '{{.Id}}'])
                self.assertEqual((self.state/'receipt.json').read_bytes(), receipt)
                self.assertFalse(any(command[0] == 'python3' for command in self.commands))
        runtime.run = self.probe
        self.assertEqual(runtime.observe(*self.args)['state'], 'current')

    def test_native_probe_default_budgets_and_explicit_overrides(self):
        for arguments, override, expected in ((['docker', 'ps'], None, 5),
                                              (['docker', 'compose', 'config'], None, 5),
                                              (['systemctl', 'show'], None, 2),
                                              (['curl', 'http://127.0.0.1/'], None, 2),
                                              (['python3', 'account.py', 'check'], 10, 10),
                                              (['docker', 'ps'], 2, 2),
                                              (['docker', 'ps'], 1, 1)):
            with self.subTest(arguments=arguments, override=override), mock.patch.object(runtime.subprocess, 'run',
                    return_value=subprocess.CompletedProcess(arguments, 0, stdout=b'ok')) as native_probe:
                self.assertEqual(self.native_run(arguments, timeout=override), b'ok')
                self.assertEqual(native_probe.call_args.kwargs,
                                 {'input': None, 'capture_output': True, 'timeout': expected, 'check': False})


    def test_receipt_is_required_and_published_only_after_actual_verification(self):
        self.install()
        self.assertEqual(runtime.observe(*self.args)['state'], 'stale')
        self.assertFalse((self.state/'receipt.json').exists())
        observation = runtime.publish(*self.args)
        self.assertEqual(observation['state'], 'current')
        self.assertEqual(runtime.observe(*self.args), observation)
        receipt = json.loads((self.state/'receipt.json').read_text())
        self.assertTrue(runtime.valid_receipt(receipt))
        self.assertNotIn('synthetic-private', json.dumps(receipt))
        self.assertNotIn('com.docker.compose.config-hash', json.dumps(receipt))
        self.assertNotIn('Env', json.dumps(receipt))

    def test_nonroot_collector_permission_boundary_and_native_health(self):
        self.install()
        collector = next(row for row in self.rows if row['Config']['Labels']['com.docker.compose.service'] == 'collector')
        self.assertEqual(collector['Config']['User'], '10001:10001')
        self.assertNotIn('Health', collector['State'])
        observation = runtime.publish(*self.args)
        receipt = (self.state/'receipt.json').read_bytes()
        for path, permissions in [(self.state, 0o700), (self.state/'database.env', 0o600),
                                  (self.state/'native.json', 0o600), (self.state/'receipt.json', 0o600),
                                  (self.state/'collector.yaml', 0o644),
                                  (self.state/'openlit-collector.yaml', 0o600)]:
            self.assertEqual(path.stat().st_mode & 0o777, permissions)
        for bad_mode in (0o600, 0o666):
            (self.state/'collector.yaml').chmod(bad_mode)
            self.assertEqual(runtime.observe(*self.args)['state'], 'stale')
            with self.assertRaises(ValueError):
                runtime.publish(*self.args)
            self.assertEqual((self.state/'receipt.json').read_bytes(), receipt)
        (self.state/'collector.yaml').chmod(0o644)
        self.assertEqual(runtime.observe(*self.args), observation)
        self.collector_unavailable = True
        self.assertEqual(runtime.observe(*self.args)['state'], 'stale')
        with self.assertRaises(ValueError):
            runtime.publish(*self.args)
        self.assertEqual((self.state/'receipt.json').read_bytes(), receipt)
        self.assertIn(['curl', '-fsS', '--max-time', '1', 'http://127.0.0.1:13133/'], self.commands)
        self.collector_unavailable = False
        self.assertEqual(runtime.observe(*self.args), observation)

    def test_failed_refresh_after_publication_cannot_accept_old_healthy_images_and_retry_converges(self):
        self.install()
        runtime.publish(*self.args)
        receipt = (self.state/'receipt.json').read_bytes()
        self.expected_images['collector'] = 'sha256:'+'a'*64
        with (self.source/'compose.yaml').open('a') as out:
            out.write('\n# updated image recipe fixture\n')
        # Simulate interrupted legacy publication followed by failed image download:
        # files equal the desired recipe and all three old containers remain healthy.
        self.publish_assets()
        self.assertEqual(runtime.observe(*self.args)['state'], 'stale')
        with self.assertRaises(ValueError):
            runtime.publish(*self.args)
        self.assertEqual((self.state/'receipt.json').read_bytes(), receipt)
        self.recreate()
        self.assertEqual(runtime.observe(*self.args)['state'], 'stale')
        self.assertEqual(runtime.publish(*self.args)['state'], 'current')
        self.assertEqual(runtime.observe(*self.args)['state'], 'current')

    def test_changed_receiver_bind_inode_requires_recreate_even_with_matching_compose_hash(self):
        self.install()
        runtime.publish(*self.args)
        for asset, _ in runtime.MOUNTS.values():
            with self.subTest(asset=asset):
                receipt = (self.state/'receipt.json').read_bytes()
                original = (self.source/asset).read_bytes()
                with (self.source/asset).open('a') as out:
                    out.write('\n# new receiver recipe\n')
                self.publish_assets()
                self.assertEqual(runtime.observe(*self.args)['state'], 'stale')
                with self.assertRaises(ValueError):
                    runtime.publish(*self.args)
                self.assertEqual((self.state/'receipt.json').read_bytes(), receipt)
                self.recreate()
                self.assertEqual(runtime.publish(*self.args)['state'], 'current')
                (self.source/asset).write_bytes(original)
                self.assertEqual(runtime.observe(*self.args)['state'], 'stale')
                self.publish_assets()
                self.recreate()
                self.assertEqual(runtime.publish(*self.args)['state'], 'current')

    def test_integrated_receiver_unsafe_asset_and_unhealthy_service_prevent_receipts(self):
        self.install()
        runtime.publish(*self.args)
        receipt = (self.state/'receipt.json').read_bytes()
        asset = self.state/'openlit-collector.yaml'
        asset.chmod(0o644)
        with self.assertRaises(ValueError):
            runtime.publish(*self.args)
        asset.chmod(0o600)
        asset.unlink()
        asset.symlink_to(self.source/asset.name)
        with self.assertRaises(ValueError):
            runtime.publish(*self.args)
        self.assertEqual((self.state/'receipt.json').read_bytes(), receipt)
        asset.unlink()
        self.publish_assets()
        openlit = next(row for row in self.rows if row['Config']['Labels']['com.docker.compose.service'] == 'openlit')
        openlit['State']['Health']['Status'] = 'unhealthy'
        with self.assertRaises(ValueError):
            runtime.publish(*self.args)
        self.assertEqual((self.state/'receipt.json').read_bytes(), receipt)

    def test_receipt_container_identity_and_compose_labels_detect_drift(self):
        self.install()
        runtime.publish(*self.args)
        self.rows[0]['Config']['Labels']['com.docker.compose.config-hash'] = '0'*64
        with self.assertRaises(ValueError):
            runtime.publish(*self.args)
        self.rows[0]['Config']['Labels']['com.docker.compose.config-hash'] = '1'*64
        self.rows[0]['Id'] = 'e'*64
        self.assertEqual(runtime.observe(*self.args)['state'], 'stale')
        self.assertEqual(runtime.publish(*self.args)['state'], 'current')

    def test_timer_drift_requires_repair_while_pending_migration_does_not(self):
        self.install()
        runtime.publish(*self.args)
        ready = runtime.observe(*self.args)
        native.write(self.state/'trace-cutover.json', '{"phase":"adopted","pending":"sessions-or-events"}\n')
        self.assertEqual(runtime.observe(*self.args), ready)
        self.timer_enabled = False
        disabled = runtime.observe(*self.args)
        self.assertEqual(disabled['state'], 'stale')
        self.timer_enabled = True
        self.timer_active = False
        inactive = runtime.observe(*self.args)
        self.assertEqual(inactive['state'], 'stale')
        self.assertNotEqual(inactive['actual'], disabled['actual'])
        with self.assertRaises(ValueError):
            runtime.guard(self.args, disabled['actual'], disabled['desired'])
        self.timer_active = True
        self.assertEqual(runtime.observe(*self.args), ready)

    def test_recovered_admission_requests_fresh_cas_repair_without_pending_failure(self):
        self.install()
        native.reconcile(self.home, self.state/'native.json', [], 'apply', os.getuid(), os.getgid(), tomli_w)
        native.write(self.home/'.codex/config.toml', tomli_w.dumps({'otel': {'trace_exporter': runtime.trace_cutover.TEMP}}))
        trace = native.CODEX.pop('trace_exporter')
        try:
            native.reconcile(self.home, self.state/'native.json', ['claude', 'codex'], 'apply', os.getuid(), os.getgid(), tomli_w)
        finally:
            native.CODEX['trace_exporter'] = trace
        intent = {'schema': 1, 'home': str(self.home), 'uid': os.getuid(), 'status': 'unavailable'}
        runtime.trace_cutover.save_admission(self.state, intent)
        runtime.publish(*self.args)
        current = runtime.observe(*self.args)
        self.assertEqual(current['state'], 'current')
        runtime.trace_cutover.save_admission(self.state, {**intent, 'status': 'ready'})
        repair = runtime.observe(*self.args)
        self.assertEqual(repair['state'], 'stale')
        self.assertEqual(repair['desired'], current['desired'])
        self.assertNotEqual(repair['actual'], current['actual'])
        runtime.guard(self.args, repair['actual'], repair['desired'])
        facts = runtime.capture(*self.args)[1]
        self.assertTrue(facts['trace_admission_ready'])
        self.assertEqual(facts['service']['ActiveState'], 'active')
        self.assertEqual(facts['timer']['ActiveState'], 'active')
        runtime.trace_cutover.save_admission(self.state, {**intent, 'status': 'mismatch'})
        self.assertEqual(runtime.observe(*self.args), current)
        with self.assertRaises(ValueError):
            runtime.guard(self.args, repair['actual'], repair['desired'])
        runtime.trace_cutover.cancel_admission(self.state)
        self.assertEqual(runtime.observe(*self.args), current)

    def test_receipt_interruption_retry_and_disabled_installed_recipe_update(self):
        self.install()
        writer = native.write
        native.write = lambda *args: (_ for _ in ()).throw(OSError('synthetic interrupted receipt publication'))
        try:
            with self.assertRaises(OSError):
                runtime.publish(*self.args)
        finally:
            native.write = writer
        self.assertEqual(runtime.observe(*self.args)['state'], 'stale')
        runtime.publish(*self.args)
        native.reconcile(self.home, self.state/'native.json', [], 'apply', os.getuid(), os.getgid(), tomli_w)
        self.rows = []
        self.service_active = self.service_enabled = False
        disabled = (*self.args[:4], False, [], 'dev')
        self.assertEqual(runtime.observe(*disabled)['state'], 'stale')
        self.timer_enabled = False
        self.assertEqual(runtime.observe(*disabled)['state'], 'stale')
        self.timer_active = False
        self.assertEqual(runtime.observe(*disabled)['state'], 'stale')
        self.assertEqual(runtime.publish(*disabled)['state'], 'current')
        with (self.source/'collector.yaml').open('a') as out:
            out.write('\n# disabled release refresh\n')
        self.publish_assets()
        self.assertEqual(runtime.observe(*disabled)['state'], 'stale')
        self.assertEqual(runtime.publish(*disabled)['state'], 'current')
        self.assertFalse(any(command[:3] == ['docker', 'image', 'inspect'] for command in self.commands[-3:]))


    def test_guest_guard_rejects_changed_assessment_before_publication(self):
        self.install()
        runtime.publish(*self.args)
        receipt = (self.state/'receipt.json').read_bytes()
        with (self.source/'collector.yaml').open('a') as out:
            out.write('\n# approved update\n')
        approved = runtime.observe(*self.args)
        self.assertEqual(runtime.guard(self.args, approved['actual'], approved['desired']), approved)
        native.write(self.state/'database.env', 'OPENLIT_DB_PASSWORD=synthetic-new-private\n')
        with self.assertRaises(ValueError):
            runtime.guard(self.args, approved['actual'], approved['desired'])
        approved = runtime.observe(*self.args)
        self.rows[0]['Id'] = 'f'*64
        with self.assertRaises(ValueError):
            runtime.guard(self.args, approved['actual'], approved['desired'])
        self.assertEqual((self.state/'receipt.json').read_bytes(), receipt)
        self.rows[0]['Config']['Labels']['com.docker.compose.service'] = 'synthetic-private-label'
        captured, facts = runtime.capture(*self.args)
        self.assertEqual(captured['state'], 'stale')
        self.assertNotIn('synthetic-private-label', json.dumps(facts))


if __name__ == '__main__':
    unittest.main()
