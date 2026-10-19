#!/usr/bin/env python3
"""Host-free identity and original-OFD predicates used by physical loss fixtures."""
import fcntl
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import struct
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[4]
spec = importlib.util.spec_from_file_location('native_loss', ROOT/'tests/helpers/profile-native-loss.py')
loss = importlib.util.module_from_spec(spec)
spec.loader.exec_module(loss)


class NativeLossTest(unittest.TestCase):
    def make_runtime(self, root, version='1.0.0'):
        bodies = {'bin/yard': b'launcher', 'bin/yard-engine': ('#!/bin/sh\nprintf "yard-engine '+version+'\\n"\n').encode(),
                  'bin/subyard-release-owner': b'owner', 'config/release-owner.schema.json': b'{}',
                  'config/release-owner.json': json.dumps(loss.DECLARATION).encode()}
        bodies['config/release-owner-build.json'] = json.dumps(dict(schemaVersion=1, kind='rust-release-owner-build',
            features=[], locked=True, imageSHA256=hashlib.sha256(b'owner').hexdigest())).encode()
        for name, body in bodies.items():
            target = root/name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(body)
            target.chmod(0o755 if name.startswith('bin/') else 0o644)
        manifest = root/'runtime-files.sha256'
        manifest.write_text(''.join(hashlib.sha256(body).hexdigest()+'  ./'+name+'\n'
                                    for name, body in bodies.items()))
        return manifest

    def test_successor_binds_actual_version_and_checked_artifact_identity(self):
        with tempfile.TemporaryDirectory(prefix='subyard-loss-successor-', dir='/tmp') as temporary:
            root = Path(temporary)
            current = self.make_runtime(root/'current')
            successor = root/'successor'
            runtime = root/'runtime'
            successor.mkdir()
            (successor/'subyard-install.sh').write_text('installer')
            arch = {'x86_64': 'amd64', 'aarch64': 'arm64'}[os.uname().machine]
            archive = successor/f'subyard-2.0.0-linux-{arch}.tar.gz'
            metadata = dict(kind='runtime', version='2.0.0', os='linux', arch=arch)
            for actual in ('1.0.0', '2.0.0'):
                self.make_runtime(runtime, actual)
                with tarfile.open(archive, 'w:gz') as bundle:
                    bundle.add(runtime, arcname='.')
                archive.with_name(archive.name+'.sha256').write_text(loss.sha(archive)+'  '+archive.name+'\n')
                archive.with_name(archive.name+'.manifest.json').write_text(json.dumps(metadata))
                if actual == '1.0.0':
                    with self.assertRaisesRegex(ValueError, 'engine version differs'):
                        loss.successor_identity(successor, '1.0.0', loss.sha(current))
                else:
                    checked = loss.successor_identity(successor, '1.0.0', loss.sha(current))
                    self.assertEqual(checked['manifest_sha256'], loss.sha(runtime/'runtime-files.sha256'))
                    self.assertEqual(checked['engine_sha256'], loss.sha(runtime/'bin/yard-engine'))
                    provenance = dict(artifact=archive.name, sha256=loss.sha(archive), version='wrong')
                    archive.with_name(archive.name+'.provenance.json').write_text(json.dumps(provenance))
                    with self.assertRaisesRegex(ValueError, 'provenance identity mismatch'):
                        loss.successor_identity(successor, '1.0.0', loss.sha(current))

    def test_normal_manifest_binds_exact_owner_images_and_rejects_drift(self):
        with tempfile.TemporaryDirectory(prefix='subyard-loss-manifest-', dir='/tmp') as temporary:
            root = Path(temporary)
            manifest = self.make_runtime(root)
            _, inventory = loss.runtime_identity(root, loss.sha(manifest))
            self.assertEqual(inventory['bin/subyard-release-owner'], hashlib.sha256(b'owner').hexdigest())
            (root/'bin/yard-engine').write_bytes(b'replacement')
            with self.assertRaisesRegex(ValueError, 'manifest mismatch'):
                loss.runtime_identity(root, loss.sha(manifest))

    def test_loss_bundle_preflight_refuses_missing_successor(self):
        with tempfile.TemporaryDirectory(prefix='subyard-loss-bundle-', dir='/tmp') as temporary:
            root = Path(temporary)
            runtime = root/'runtime'
            self.make_runtime(runtime)
            source = root/'source'
            release = source/'.subyard-acceptance/release'
            release.mkdir(parents=True)
            archive = release/'subyard-1.0.0-linux-amd64.tar.gz'
            with tarfile.open(archive, 'w:gz') as bundle:
                bundle.add(runtime, arcname='.')
            (release.parent/'candidate.json').write_text(json.dumps(dict(version='1.0.0',
                runtime_file=archive.name, runtime_sha256=loss.sha(archive))))
            transported = root/'bundle.tar.gz'
            with tarfile.open(transported, 'w:gz') as bundle:
                bundle.add(source, arcname='.')
            with self.assertRaisesRegex(ValueError, 'one checked compatible successor'):
                loss.bundle_preflight(transported, loss.sha(transported))

    def test_checksummed_fixture_feature_image_is_not_a_normal_owner(self):
        with tempfile.TemporaryDirectory(prefix='subyard-loss-feature-', dir='/tmp') as temporary:
            root = Path(temporary)
            manifest = self.make_runtime(root)
            build = root/'config/release-owner-build.json'
            value = json.loads(build.read_text())
            value['features'] = ['fixture-control']
            build.write_text(json.dumps(value))
            lines = [line if not line.endswith('./config/release-owner-build.json') else
                     loss.sha(build)+'  ./config/release-owner-build.json' for line in manifest.read_text().splitlines()]
            manifest.write_text('\n'.join(lines)+'\n')
            with self.assertRaisesRegex(ValueError, 'normal locked Rust image'):
                loss.runtime_identity(root, loss.sha(manifest))

    def test_no_replay_guard_refuses_journal_fact_and_dispatch_changes(self):
        with tempfile.TemporaryDirectory(prefix='subyard-loss-proofs-', dir='/tmp') as temporary:
            root = Path(temporary)
            journal = root/'config/release-transition/v2/journal.json'
            intent = root/'config/migrations/native-custody/v1/transaction/001.intent.json'
            for path in (journal, intent, root/'calls'):
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text('original')
            with patch.dict(os.environ, {'SUBYARD_CONFIG_HOME': str(root/'config')}):
                (root/'after-cut.json').write_text(json.dumps(dict(protected=loss.protected_state(root))))
                loss.require_unchanged(root)
                for path in (journal, intent, root/'calls'):
                    (root/'after-cut.json').write_text(json.dumps(dict(protected=loss.protected_state(root))))
                    path.write_text('changed')
                    with self.assertRaisesRegex(ValueError, 'pending journal, native proofs or owning dispatch count'):
                        loss.require_unchanged(root)
                    path.write_text('original')

    def test_generated_owning_pause_wrappers_preserve_shell_grammar(self):
        examples = [('github', 'release-activation-guest.sh', ['fixture.service'], 'systemctl'),
                    ('openlit', 'owner.sh', ['fixture-instance'], 'incus'),
                    ('orca', 'orca-bootstrap.sh', ['fixture-instance'], 'incus'),
                    ('git-fetch', 'installed-activation.sh', ['/tmp/fixture', 'fixture-instance', 'fixture-project'], 'incus')]
        with tempfile.TemporaryDirectory(prefix='subyard-loss wrapper-', dir='/tmp') as temporary:
            for profile, filename, arguments, command in examples:
                with self.subTest(profile=profile):
                    source = (ROOT/'config/profiles'/profile/'tests/e2e'/filename).read_text()
                    start = source.index('  python3 - "$loss_case"')
                    body = source[source.index("<<'PY'\n", start)+7:]
                    body = body[:body.index('\nPY\n')]
                    case = Path(temporary)/profile
                    (case/'bin').mkdir(parents=True)
                    generated = subprocess.run([sys.executable, '-c', body, str(case), *arguments], capture_output=True)
                    self.assertEqual(generated.returncode, 0, generated.stderr)
                    wrapper = case/'bin'/command
                    parsed = subprocess.run(['bash', '-n', str(wrapper)], capture_output=True)
                    self.assertEqual(parsed.returncode, 0, parsed.stderr)
                    checked = subprocess.run(['shellcheck', '-S', 'warning', str(wrapper)], capture_output=True)
                    self.assertEqual(checked.returncode, 0, checked.stdout+checked.stderr)
                    if profile == 'git-fetch':
                        start = source.index("cat >> \"$root/environment\" <<'BASH_ENV'\n")
                        guest = source[start:].split('\n',1)[1].split('\nBASH_ENV\n',1)[0]
                        parsed = subprocess.run(['bash','-n'], input=guest.encode(), capture_output=True)
                        self.assertEqual(parsed.returncode, 0, parsed.stderr)

    def test_descriptor_or_matching_image_without_actual_native_role_is_not_routing(self):
        record = loss.process_identity(os.getpid())
        image = loss.sha('/proc/self/exe')
        with self.assertRaisesRegex(ValueError, 'did not traverse'):
            loss.routed_processes([record], os.getpid(),
                                  {'bin/yard-engine': image, 'bin/subyard-release-owner': image})
        self.assertTrue(loss.same(record))
        self.assertFalse(loss.same(dict(record, start=str(int(record['start'])+1))))

    def test_readonly_barrier_observation_requires_actual_kernel_ofd(self):
        with tempfile.TemporaryDirectory(prefix='subyard-loss-ofd-', dir='/tmp') as temporary:
            path = Path(temporary)/'lock'
            fd = os.open(path, os.O_CREAT|os.O_EXCL|os.O_RDWR, 0o600)
            info = os.fstat(fd)
            identity = dict(device=info.st_dev, inode=info.st_ino)
            try:
                self.assertFalse(loss.barrier_held(identity))
                fcntl.fcntl(fd, fcntl.F_OFD_SETLK, struct.pack('hhqqi', fcntl.F_WRLCK, os.SEEK_SET, 0, 0, 0))
                self.assertTrue(loss.barrier_held(identity))
                self.assertIn('OFDLCK', Path('/proc/self/fdinfo/'+str(fd)).read_text())
            finally:
                os.close(fd)
            self.assertFalse(loss.barrier_held(identity))


if __name__ == '__main__':
    unittest.main()
