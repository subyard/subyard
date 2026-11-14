#!/usr/bin/env python3
"""Core release archive preparation contracts."""
import hashlib, io, json, subprocess, tarfile, tempfile, unittest
from pathlib import Path
import runpy
import sys
import shutil
from types import SimpleNamespace
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]


class SkillPackageTest(unittest.TestCase):
    def test_current_package_transport_survives_normalized_and_private_layouts(self):
        helper = runpy.run_path(str(ROOT/'dev/verify-release-upgrades.py'))
        contracts = helper['skills_transport_contract'](ROOT)
        expected = contracts[0][1]
        with tempfile.TemporaryDirectory(prefix='subyard-package-skills-', dir='/tmp') as temporary:
            base = Path(temporary)
            helper['write_observation_incus'](base, base/'control', base/'unexpected', skills_roots=[ROOT])
            for layout, directory_mode, file_mode, executable_mode in (
                    ('normalized',0o755,0o644,0o755), ('private',0o700,0o600,0o700)):
                with self.subTest(layout=layout):
                    package = base/layout
                    for name, (kind, _, _) in expected.items():
                        path = package/name
                        path.parent.mkdir(parents=True, exist_ok=True)
                        if kind == 'directory': path.mkdir(exist_ok=True)
                        else: shutil.copyfile(ROOT/name, path)
                        mode = directory_mode if kind == 'directory' else executable_mode if (ROOT/name).stat().st_mode & 0o111 else file_mode
                        path.chmod(mode)
                    archive = subprocess.run([sys.executable, '-B', str(package/'config/skills/package.py'),
                        'archive', str(package)], capture_output=True, check=True, timeout=10).stdout
                    for command, _ in contracts:
                        result = subprocess.run([str(base/'incus'), *command], input=archive, capture_output=True, timeout=10)
                        self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertFalse((base/'unexpected').exists())


class SourceAnchorRefusalTest(unittest.TestCase):
    def test_refusal_requires_guidance_and_preserves_every_protected_input(self):
        module = runpy.run_path(str(ROOT/'dev/verify-release-upgrades.py'))
        verify = module['verify_source_anchor_refusal']
        for failure in ('none', 'success', 'guidance', 'config', 'data', 'link', 'installer-config', 'installer-journal', 'check-json'):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory(dir='/tmp') as temporary:
                root = Path(temporary)
                config, runtime = root/'config', root/'runtime'
                config.mkdir(); runtime.mkdir()
                retained, marker = config/'settings.env', root/'project-data'
                retained.write_text('operator settings'); marker.write_text('project data')
                (runtime/'current').symlink_to('releases/source')
                fixture = SimpleNamespace(config=config, runtime=runtime, project_marker=marker,
                    retained=retained, retained_hash=module['digest'](retained), env={},
                    version='0.29.0', baseline_version='0.11.0', old_launcher=root/'old-yard')
                calls = []
                def run(command, environment, timeout):
                    calls.append(command[1:])
                    self.assertEqual(environment['YARD_RELEASE_VERSION'], '0.29.0')
                    if failure == 'config' or (failure == 'installer-config' and command[0] == 'bash'): retained.write_text('changed')
                    if failure == 'data': marker.write_text('changed')
                    if failure == 'installer-journal' and command[0] == 'bash':
                        journal = config/'release-transition/v2/journal.json'
                        journal.parent.mkdir(parents=True); journal.write_text('changed')
                    if failure == 'link':
                        (runtime/'current').unlink()
                        (runtime/'current').symlink_to('releases/target')
                    return subprocess.CompletedProcess(command, 0 if failure == 'success' or (failure == 'check-json' and '--check' in command) else 1,
                        json.dumps({'outcome':{'reachedGoal':False,'status':'operator-action-required'}}) if failure == 'check-json' else '',
                        'source-anchor-required: ' + ('' if failure == 'guidance' else 'next: yard update --version ' + module['published_anchor'](ROOT)[0] + ' && yard update'))
                with patch.dict(verify.__globals__, {'run_process':run}):
                    if failure in ('none', 'check-json'):
                        verify(fixture, root/'subyard-install.sh')
                        self.assertEqual(calls, [['update'], ['update', '--check'], [str(root/'subyard-install.sh'), '--version', '0.29.0', '--yes']])
                    else:
                        with self.assertRaises(RuntimeError): verify(fixture, root/'subyard-install.sh')


class CandidateArchiveTest(unittest.TestCase):
    def test_actual_archive_verification_repeat_refusal_and_partial_retry(self):
        helper = Path(__file__).resolve().parents[2]/'tests/helpers/release-candidate.sh'
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            root = Path(temporary)
            release = root/'.subyard-acceptance/release'
            release.mkdir(parents=True)
            archive = release/'subyard-0.21.0-linux-amd64.tar.gz'
            metadata = release.parent/'candidate.json'
            payload = b'#!/bin/sh\nprintf "yard-engine 0.21.0\\n"\n'
            def package(bad_manifest=False):
                files = {'bin/yard':payload, 'bin/yard-engine':payload}
                files['runtime-files.sha256'] = ''.join(
                    f'{"0"*64 if bad_manifest else hashlib.sha256(body).hexdigest()}  {name}\n'
                    for name,body in files.items()).encode()
                with tarfile.open(archive,'w:gz') as bundle:
                    for name,body in files.items():
                        entry = tarfile.TarInfo(name)
                        entry.size = len(body)
                        entry.mode = 0o755 if name.startswith('bin/') else 0o644
                        bundle.addfile(entry,io.BytesIO(body))
                metadata.write_text(json.dumps(dict(version='0.21.0',runtime_file=archive.name,
                    runtime_sha256=hashlib.sha256(archive.read_bytes()).hexdigest())))
            def prepare():
                return subprocess.run(['bash','-c','. "$1"; release_candidate_prepare "$2"',
                    'fixture',str(helper),str(root)],capture_output=True,text=True,timeout=5)
            package()
            for _ in range(2):
                result = prepare()
                self.assertEqual(result.returncode,0,result.stderr)
                engine = Path(result.stdout.strip())
                self.assertEqual(subprocess.check_output([str(engine)],text=True),'yard-engine 0.21.0\n')
                self.assertFalse((release.parent/'runtime.next').exists())
            before = engine.read_bytes()
            archive.write_bytes(archive.read_bytes()+b'tampered')
            self.assertNotEqual(prepare().returncode,0)
            self.assertEqual(engine.read_bytes(),before)
            package(bad_manifest=True)
            self.assertNotEqual(prepare().returncode,0)
            package()
            self.assertEqual(prepare().returncode,0)
            self.assertFalse((release.parent/'runtime.next').exists())


if __name__ == '__main__': unittest.main()
