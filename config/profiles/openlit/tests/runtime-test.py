#!/usr/bin/env python3
import io
import json
import os
from pathlib import Path
import shutil
import sys
import tarfile
import tempfile
import unittest

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
        self.rows = [{'Id': str(i+3)*64, 'Image': self.expected_images[name],
                      'Config': {'Labels': {'com.docker.compose.service': name, 'com.docker.compose.config-hash': str(i)*64},
                                 'Env': ['PRIVATE=synthetic-unexported'], 'User': '10001:10001' if name == 'collector' else ''},
                      'State': {'Status': 'running', **({} if name == 'collector' else {'Health': {'Status': 'healthy'}})}}
                     for i, name in enumerate(sorted(runtime.SERVICES), 1)]
        self.mounted = (self.source/'collector.yaml').read_bytes()
        self.service_active = True
        self.service_enabled = True
        self.probe_failure = False
        self.collector_unavailable = False
        self.commands = []
        original = runtime.run
        self.addCleanup(setattr, runtime, 'run', original)
        runtime.run = self.probe

    def install(self):
        self.state.mkdir(mode=0o700, exist_ok=True)
        native.write(self.state/'marker', runtime.MARKER+'\n')
        native.write(self.state/'database.env', 'OPENLIT_DB_PASSWORD=synthetic-private\n')
        self.publish_assets()
        native.reconcile(self.home, self.state/'native.json', ['claude', 'codex'], 'apply', os.getuid(), os.getgid(), tomli_w)

    def publish_assets(self):
        for name in runtime.FILES:
            native.write(self.state/name, (self.source/name).read_text())
        (self.state/'collector.yaml').chmod(0o644)
        native.write(self.unit, (self.source/'subyard-openlit.service').read_text())
        self.unit.chmod(0o644)

    def recreate(self):
        for i, row in enumerate(self.rows, 7):
            row['Id'] = format(i, 'x')*64
            row['Image'] = self.expected_images[row['Config']['Labels']['com.docker.compose.service']]
        self.mounted = (self.source/'collector.yaml').read_bytes()

    def probe(self, arguments):
        self.commands.append(arguments)
        if self.probe_failure:
            raise ValueError('synthetic native probe failed')
        if arguments[0] == 'systemctl':
            return ('ActiveState='+('active' if self.service_active else 'inactive')+'\nUnitFileState='+('enabled' if self.service_enabled else 'disabled')+'\n').encode()
        if arguments[:2] == ['docker', 'ps']:
            return '\n'.join(row['Id'] for row in self.rows).encode()
        if arguments[:2] == ['docker', 'inspect']:
            return json.dumps(self.rows).encode()
        if arguments[:3] == ['docker', 'image', 'inspect']:
            return json.dumps([{'Id': self.expected_images[name]} for name in sorted(runtime.SERVICES)]).encode()
        if arguments[:2] == ['docker', 'compose']:
            if arguments[-2:] == ['--format', 'json']:
                # This private environment must never enter observation output or CAS projection.
                return json.dumps({'services': {name: {'image': 'fixture/'+name+'@sha256:'+str(i)*64,
                    'environment': {'PASSWORD': 'synthetic-private'}} for i, name in enumerate(sorted(runtime.SERVICES), 1)}}).encode()
            if arguments[-2:] == ['--hash', '*']:
                return '\n'.join(name+' '+str(i)*64 for i, name in enumerate(sorted(runtime.SERVICES), 1)).encode()
        if arguments[:2] == ['docker', 'cp']:
            out = io.BytesIO()
            with tarfile.open(fileobj=out, mode='w') as tar:
                member = tarfile.TarInfo('config.yaml')
                member.size = len(self.mounted)
                tar.addfile(member, io.BytesIO(self.mounted))
            return out.getvalue()
        if arguments[0] == 'curl':
            if arguments[-1] == 'http://127.0.0.1:13133/' and self.collector_unavailable:
                raise ValueError('synthetic collector health unavailable')
            return b'OK'
        raise AssertionError(arguments)

    def test_absent_is_read_only(self):
        self.assertEqual(runtime.observe(*self.args), {'state': 'absent', 'actual': '', 'desired': ''})
        self.assertFalse(self.state.exists())
        self.assertEqual(self.commands, [])

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
                                  (self.state/'collector.yaml', 0o644)]:
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

    def test_changed_collector_bind_inode_requires_recreate_even_with_matching_compose_hash(self):
        self.install()
        runtime.publish(*self.args)
        with (self.source/'collector.yaml').open('a') as out:
            out.write('\n# new collector recipe\n')
        self.publish_assets()
        self.assertEqual(runtime.observe(*self.args)['state'], 'stale')
        with self.assertRaises(ValueError):
            runtime.publish(*self.args)
        self.recreate()
        self.assertEqual(runtime.publish(*self.args)['state'], 'current')

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
        self.assertEqual(runtime.publish(*disabled)['state'], 'current')
        with (self.source/'collector.yaml').open('a') as out:
            out.write('\n# disabled release refresh\n')
        self.publish_assets()
        self.assertEqual(runtime.observe(*disabled)['state'], 'stale')
        self.assertEqual(runtime.publish(*disabled)['state'], 'current')
        self.assertFalse(any(command[:3] == ['docker', 'image', 'inspect'] for command in self.commands[-3:]))

    def test_recipe_update_and_rollback_changes_desired_identity(self):
        self.install()
        original = runtime.publish(*self.args)['desired']
        old = (self.source/'collector.yaml').read_bytes()
        (self.source/'collector.yaml').write_bytes(old+b'\n# updated source\n')
        update = runtime.observe(*self.args)
        self.assertNotEqual(update['desired'], original)
        self.publish_assets()
        self.recreate()
        runtime.publish(*self.args)
        (self.source/'collector.yaml').write_bytes(old)
        rollback = runtime.observe(*self.args)
        self.assertEqual(rollback['desired'], original)
        self.assertEqual(rollback['state'], 'stale')
        self.publish_assets()
        self.recreate()
        self.assertEqual(runtime.publish(*self.args)['desired'], original)

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
