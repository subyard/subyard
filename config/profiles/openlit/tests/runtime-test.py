#!/usr/bin/env python3
import io
import json
import os
from pathlib import Path
import runpy
import shutil
import subprocess
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
    def test_released_updater_fixture_observes_absence_and_rejects_mutation(self):
        harness = runpy.run_path(str(ROOT/'dev/verify-release-upgrades.py'))
        observation = runpy.run_path(str(PROFILE/'tests/release-observation.py'))
        runtime_root = Path(os.environ.get('SUBYARD_OPENLIT_TEST_RUNTIME_ROOT', ROOT))
        with tempfile.TemporaryDirectory(prefix='subyard-openlit-release-', dir='/tmp') as temporary:
            root = Path(temporary)
            control, rejected = root/'control.json', root/'rejected'
            control.write_text(json.dumps({'instances': [{'info': {'status': 'Running'}}]}))
            harness['write_observation_incus'](root, control, rejected)
            environment = {
                'PATH': str(root)+':/usr/bin:/bin',
                'SUBYARD_ENGINE_CONTEXT': '1', 'SUBYARD_ENGINE_CONTEXT_SCHEMA': '1',
                'SUBYARD_OPERATOR_HOME': str(root), 'SUBYARD_CONFIG_DIR': str(runtime_root/'config'),
                'SUBYARD_CONFIG_HOME': str(root), 'SUBYARD_HOME': str(root),
                'STORAGE_PATH': str(root), 'HOST_BASE': str(root), 'RESTRICTED_DISK_PATHS': str(root),
                'ACCESS_KIND': 'local', 'YARD_KIND': 'container', 'YARD_INSTANCE_NAME': 'yard',
                'INCUS_PROJECT': 'subyard', 'INCUS_BRIDGE': 'fixture', 'SSH_HOST': '127.0.0.1',
                'DEV_USER': 'dev', 'DEV_UID': '1000', 'DEV_SUDO': '0',
                'FORWARD_SSH_AGENT': '0', 'NESTED_E2E_VMS': '0',
                'ENVIRONMENT_PROFILES': '', 'CODING_TOOL_INTEGRATIONS': 'codex',
            }
            result = subprocess.run([str(runtime_root/'config/profiles/openlit/runtime.sh'), 'observe'], env=environment,
                                    capture_output=True, text=True, timeout=30)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(json.loads(result.stdout), {'state': 'absent', 'actual': '', 'desired': ''})
            self.assertFalse(rejected.exists())
            arguments, response = observation['read_only_commands'](PROFILE)[0]
            # The accepted transport drains binary archives larger than a pipe.
            result = subprocess.run([str(root/'incus'), *arguments], input=b'\xff'*131072,
                                    capture_output=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout.decode(), response)
            for changed in (
                arguments[:-4] + ['refresh', '0', 'dev', 'codex', 'a'*64, 'b'*64],
                arguments[:-4] + ['apply', '0', 'dev', 'codex'],
                arguments[:9] + [arguments[9]+'\n'] + arguments[10:],
                arguments[:1] + ['foreign'] + arguments[2:],
                arguments + ['unexpected'],
                ['config', 'set', 'yard', 'key', 'value'],
            ):
                with self.subTest(arguments=changed[:2]):
                    result = subprocess.run([str(root/'incus'), *changed],
                                            capture_output=True, timeout=10)
                    self.assertEqual(result.returncode, 99)
                    self.assertEqual(rejected.read_text(), 'incus operation rejected')
                    rejected.unlink()

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
        self.mounted = {name: (self.source/asset).read_bytes() for name, (asset, _) in runtime.MOUNTS.items()}
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
        self.mounted = {name: (self.source/asset).read_bytes() for name, (asset, _) in runtime.MOUNTS.items()}

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
            return b'OK'
        raise AssertionError(arguments)

    def test_absent_is_read_only(self):
        self.assertEqual(runtime.observe(*self.args), {'state': 'absent', 'actual': '', 'desired': ''})
        self.assertFalse(self.state.exists())
        self.assertEqual(self.commands, [])

    def test_integrated_receiver_mount_and_healthcheck_require_both_signals(self):
        environment = dict(os.environ, OPENLIT_DB_PASSWORD='synthetic-private')
        rendered = subprocess.run(['docker', 'compose', '-f', str(PROFILE/'compose.yaml'),
                                   'config', '--format', 'json'], env=environment,
                                  capture_output=True, text=True, check=True, timeout=10)
        service = json.loads(rendered.stdout)['services']['openlit']
        mount = next(volume for volume in service['volumes']
                     if volume['target'] == runtime.MOUNTS['openlit'][1])
        self.assertEqual(mount['type'], 'bind')
        self.assertEqual(Path(mount['source']), PROFILE/'openlit-collector.yaml')
        self.assertTrue(mount['read_only'])
        self.assertEqual(service['healthcheck']['test'][:3], ['CMD', 'node', '-e'])
        script = service['healthcheck']['test'][3]
        for statuses, expected in [([200, 405, 405], 0), ([500, 405, 405], 1),
                                   ([200, 404, 405], 1), ([200, 405, 404], 1),
                                   ([200, 0, 405], 1)]:
            with self.subTest(statuses=statuses):
                urls = ['http://localhost:3000', 'http://localhost:4318/v1/logs',
                        'http://localhost:4318/v1/metrics']
                responses = dict(zip(urls, statuses))
                stub = 'const responses='+json.dumps(responses)+';global.fetch=url=>{const status=responses[url];return status?Promise.resolve({ok:status===200,status}):Promise.reject(new Error("unavailable"));};'
                result = subprocess.run(['node', '-e', stub+script], capture_output=True, timeout=5)
                self.assertEqual(result.returncode, expected, result.stderr)

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
                with (self.source/asset).open('a') as out:
                    out.write('\n# new receiver recipe\n')
                self.publish_assets()
                self.assertEqual(runtime.observe(*self.args)['state'], 'stale')
                with self.assertRaises(ValueError):
                    runtime.publish(*self.args)
                self.assertEqual((self.state/'receipt.json').read_bytes(), receipt)
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
