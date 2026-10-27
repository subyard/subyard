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


class HistoryLedgerCutTest(unittest.TestCase):
    def test_actual_observer_command_selects_final_pending_packaged_edge(self):
        helper = runpy.run_path(str(ROOT/'dev/verify-release-upgrades.py'))['verify_history_process_cuts']
        observer = runpy.run_path(str(ROOT/'dev/e2e/release-transition-post-cas-observer.py'))
        with tempfile.TemporaryDirectory(prefix='subyard-ledger-cut-', dir='/tmp') as temporary:
            root = Path(temporary)
            release, config = root/'release', root/'config'
            release.mkdir()
            state = config/'release-transition/v2'
            state.mkdir(parents=True)
            ledger, journal = state/'ledger.json', state/'journal.json'
            ledger.write_text(json.dumps({'schemaVersion':2, 'domains':{
                'settings':{'epoch':2}, 'power':{'epoch':1}, 'owner':{'epoch':2}}}))
            journal.write_text(json.dumps({'transaction':'source'}))
            fixture = SimpleNamespace(root=root, config=config, journal=journal, runtime=root/'runtime',
                                      old_launcher=root/'old/yard', env={})
            registry = {'schemaVersion':2, 'migrations':[
                {'id':'completed', 'domain':'settings', 'fromEpoch':1, 'toEpoch':2},
                {'id':'first', 'domain':'power', 'fromEpoch':1, 'toEpoch':2},
                {'id':'final', 'domain':'settings', 'fromEpoch':2, 'toEpoch':3},
                {'id':'completed-tail', 'domain':'owner', 'fromEpoch':1, 'toEpoch':2}]}
            bundle = release/'subyard-candidate-linux-amd64.tar.gz'
            payload = json.dumps(registry).encode()
            with tarfile.open(bundle, 'w:gz') as archive:
                member = tarfile.TarInfo('./config/release-transition.json')
                member.size = len(payload)
                archive.addfile(member, io.BytesIO(payload))
            calls = []
            class ObservedCut(Exception): pass
            def observe(command, environment, timeout):
                calls.append(command)
                self.assertEqual(timeout, 120)
                with patch.object(sys, 'argv', ['observer', *command[2:]]):
                    args = observer['parse_args']()
                self.assertEqual(args.migration, 'final')
                # Model delayed event delivery: both real ledger edges are now
                # reflected in the journal and only the final bytes remain.
                first = json.dumps({'schemaVersion':2, 'domains':{
                    'settings':{'epoch':2}, 'power':{'epoch':2}, 'owner':{'epoch':2}}}).encode()
                final = json.dumps({'schemaVersion':2, 'domains':{
                    'settings':{'epoch':3}, 'power':{'epoch':2}, 'owner':{'epoch':2}}}).encode()
                ledger.write_bytes(final)
                value = {'transaction':'candidate', 'checkpoint':'migrating', 'goal':{'target':args.candidate_target}, 'steps':[
                    {'id':'first.ledger', 'migration':'first', 'checkpoint':'verified',
                     'desiredFingerprint':hashlib.sha256(first).hexdigest()},
                    {'id':'final.ledger', 'migration':'final', 'checkpoint':'evidence',
                     'desiredFingerprint':hashlib.sha256(final).hexdigest()}]}
                journal.write_text(json.dumps(value))
                result = observer['observe_ledger'](args)
                self.assertEqual(result['ledgerFingerprint'], hashlib.sha256(final).hexdigest())
                self.assertFalse((state/'history-checkpoint.json').exists())
                for change in ({'migration':'first'}, {'migration':'foreign'},
                               {'candidate_target':'foreign'}, {'source_transaction':'candidate'}):
                    self.assertIsNone(observer['observe_ledger'](SimpleNamespace(**(vars(args)|change))))
                value['steps'][-1]['desiredFingerprint'] = '0'*64
                journal.write_text(json.dumps(value))
                self.assertIsNone(observer['observe_ledger'](args))
                raise ObservedCut()
            with patch.dict(helper.__globals__, {'history_chain':lambda *args: (fixture, 'prior', {}),
                                                 'run_process':observe}):
                with self.assertRaises(ObservedCut): helper(release, 'candidate', 'amd64', root, {})
            self.assertEqual(len(calls), 1)

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
