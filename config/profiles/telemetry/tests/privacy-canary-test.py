#!/usr/bin/env python3
"""Exercise private producer writes and scan the complete telemetry directory."""
import base64
import contextlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import sqlite3
import stat
import subprocess
import sys
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest.mock import patch
import uuid

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import launcher
import records
import sources
from spool_fixture import prepare, spooled

spec = importlib.util.spec_from_file_location('correlation', Path(sources.__file__).with_name('broker-correlation.py'))
correlation = importlib.util.module_from_spec(spec)
spec.loader.exec_module(correlation)

BEARER = 'synthetic-canary-bearer-78212-only-fixture'
PASSWORD = 'unlabelled-canary-password-49216'
API_KEY = 'sk-canary-synthetic-key-9482'
EMAIL = 'canary-51747@example.invalid'
ACCOUNT = 'e61f2221-71ba-44fa-8c20-77c337914c21'
PROMPT = 'synthetic-prompt-canary-12697'
OUTPUT = 'synthetic-tool-output-canary-67943'
CAPABILITY = 'ab19' * 16
CONTROL = '/tmp/canary-private-control-8917/control.sock'
ROUTE = '/tmp/canary-private-identity-5317'
MARKERS = (BEARER, PASSWORD, API_KEY, EMAIL, ACCOUNT, PROMPT, OUTPUT, CAPABILITY, CONTROL, ROUTE)
AT = '2026-11-18T12:04:00Z'


class PrivacyCanary(unittest.TestCase):
    def assert_clean(self, data, stage):
        for index, marker in enumerate(MARKERS):
            self.assertFalse(marker.encode() in data, f'{stage} contains canary {index}')

    def scan(self, root, stage):
        for path in root.rglob('*'):
            try:
                self.assert_clean(os.fsencode(path.relative_to(root)), stage+' entry name')
                if stat.S_ISREG(path.lstat().st_mode):
                    self.assert_clean(path.read_bytes(), stage+' file')
            except FileNotFoundError:
                continue  # A committed segment may be reclaimed during the live scan.

    def cli(self, command, data, *args, **options):
        result = subprocess.run([records.collector_binary(), command, '--data-dir', str(data), *args],
                                capture_output=True, check=True, timeout=15, **options)
        self.assert_clean(result.stderr, command+' diagnostic')
        self.assert_clean(result.stdout, command+' output')
        return result.stdout

    def wait_committed(self, data, auth_file, identities):
        with subprocess.Popen([records.collector_binary(), 'serve', '--data-dir', str(data),
                               '--listen', '127.0.0.1:0', '--auth-file', str(auth_file)],
                              stdout=subprocess.DEVNULL, stderr=subprocess.PIPE) as collector:
            try:
                deadline = time.monotonic()+10
                with contextlib.closing(sqlite3.connect((data/'telemetry.sqlite').as_uri()+'?mode=ro', uri=True)) as db:
                    while collector.poll() is None and time.monotonic() < deadline:
                        committed = set(db.execute('SELECT origin_id,event_id FROM records'))
                        gap = db.execute("SELECT 1 FROM gaps WHERE json_extract(body,'$.reason')='spool_torn_tail'").fetchone()
                        if identities <= committed and gap and (data/'control.sock').exists():
                            self.scan(data, 'active SQLite and spool')
                            return
                        time.sleep(.05)
                self.fail('expected events and torn-tail gap did not commit')
            finally:
                collector.terminate()
                try:
                    _, stderr = collector.communicate(timeout=10)
                except subprocess.TimeoutExpired:
                    collector.kill()
                    _, stderr = collector.communicate()
                self.assert_clean(stderr, 'collector diagnostic')

    def test_full_directory_canary(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            data = prepare(root/'data')
            auth_file = records.operational_directory(data)/'auth.token'
            auth_file.write_text(BEARER)
            auth_file.chmod(0o600)
            archive = records.private_directory(data/'archive')/'telemetry.sqlite'
            shutil.copyfile(data/'telemetry.sqlite', archive)
            archive.chmod(0o400)
            history = archive.read_bytes()
            run_id = str(uuid.uuid4())
            args = SimpleNamespace(command=['/bin/true', PROMPT, OUTPUT, API_KEY], data_dir=data,
                agent='codex', agent_version='1.9.0', scope_mode='partial', host_id=BEARER,
                task_id=ACCOUNT, project_id='safe-launch-project', worktree_id=API_KEY, yard_id=EMAIL)
            launch_context = launcher.context
            with patch.dict(os.environ, OTEL_EXPORTER_OTLP_HEADERS='authorization=Bearer '+BEARER,
                            SYNTHETIC_OPERATOR_ACCOUNT_ID=ACCOUNT), \
                 patch.object(launcher, 'context', side_effect=lambda *values: dict(launch_context(*values), agent_run_id=run_id)), \
                 patch.object(launcher, 'start_standalone_observer'):
                self.assertEqual(launcher.launch(args), 0)

            baseline_id = str(uuid.uuid4())
            baseline = dict(agent_run_id=baseline_id, boot_id=launcher.boot_id(), pid=os.getpid(), start_ticks=123,
                project_id=API_KEY, task_id=ACCOUNT, scope_baselines=[dict(event_id='safe-baseline', source_time=AT,
                    body={'object_id': 'safe-object', 'cpu_ns': 42, 'generation': PASSWORD,
                          'password': PASSWORD, EMAIL: PROMPT, 'output': OUTPUT})])
            self.assertTrue(launcher.admit_manifest(data, baseline, reserve=True))
            manifest = launcher.manifest_path(data/launcher.RUNS, baseline_id)
            replace = os.replace

            def interrupted(temporary, target):
                if target == manifest:
                    self.assertTrue(Path(temporary).is_file())
                    self.scan(data, 'interrupted manifest before rename')
                    raise OSError('synthetic interrupted publication')
                replace(temporary, target)

            with patch.object(records.os, 'replace', side_effect=interrupted):
                with self.assertRaises(OSError):
                    launcher.finish_manifest(data, baseline)
            self.assertEqual(records.read_private(manifest)['manifest_state'], 'reserved')
            launcher.finish_manifest(data, baseline)
            self.assertEqual(records.read_private(manifest)['scope_baselines'][0]['body']['cpu_ns'], 42)

            for kind, transport in (
                    ('vm', dict(script='/tmp/fixture/dev/agent-e2e.sh', yard='fixture', identity=ROUTE)),
                    ('android', dict(control=CONTROL, capability=CAPABILITY))):
                request = dict(kind=kind, owner_id='safe-owner-'+kind, correlation_id='safe-correlation-'+kind,
                    slot_id='safe-slot', resource_generation=2, lease_epoch=3 if kind == 'vm' else None,
                    yard_id='safe-request-yard', project_id=EMAIL, transport=transport)
                self.assertTrue(correlation.register(data, run_id, dict(request, phase='request')))
                request['project_id'] = 'unused-held-project-label'
                self.assertTrue(correlation.register(data, run_id, dict(request, phase='held')))
            intents = list((data/'sources/intents').glob('intent-*.json'))
            self.assertEqual(len(intents), 2)
            for intent in intents:
                self.assertNotIn(b'unused-held-project-label', intent.read_bytes())
            self.scan(data, 'durable held intents')

            def page(config):
                kind = config['kind']
                self.assertTrue('identity' in config['transport'] if kind == 'vm' else 'capability' in config['transport'])
                epoch = 3 if kind == 'vm' else None
                native = dict(slot_id='safe-slot', resource_generation=2, lease_epoch=epoch,
                    source_time=AT, monotonic_ns=10, boot_id='safe-native-boot', cpu_ns=70,
                    counter_generation='safe-native-counter', output=OUTPUT)
                event = dict(event_id='safe-'+kind, source_time=AT, phase='held',
                    allocation=dict(slot_id='safe-slot', resource_generation=2, lease_epoch=epoch, state='held',
                        acquired_at=AT, owner={'run': 'safe-correlation-'+kind, 'yard': ACCOUNT,
                                               'project': 'safe-page-project', 'user.account_id': ACCOUNT,
                                               'password': PASSWORD, 'api_key': API_KEY, EMAIL: PROMPT},
                        environment=dict(vm_count=1)), observations=[native])
                return dict(owner_id='d'*64, gap=False, more=False, events=[event], allocations=[],
                            observations=[], next_cursor='01794918400000000000-1111111111111111')

            with patch.object(sources, 'fetch', side_effect=page):
                self.assertTrue(sources.poll(data))
            self.assertFalse(any((data/'sources/intents').glob('intent-*.json')))
            self.assertEqual(len(list((data/'sources').glob('observer-*.json'))), 2)
            self.scan(data, 'merged observer state')

            self.assertFalse(records.emit(data, 'run', {'agent_run_id': EMAIL}, event_id='malformed-canary'))
            self.assertTrue(records.emit(data, 'session', {'session_id': 'safe-session-id',
                'session_attributes': ['session.id'], 'prompt': PROMPT, 'output': OUTPUT}, event_id='safe-session'))
            with (data/'spool/broker.open').open('ab') as segment:
                segment.write(b'{"interrupted":')
            self.assertTrue(records.emit(data, 'gap', {'reason': 'source_offline'}, broker=True, event_id='safe-after-tail'))
            self.assertTrue(list((data/'spool').glob('*.sealed')))
            self.assertTrue(list((data/'spool').glob('*.open')))
            self.scan(data, 'offline spool and loss slots')
            rows = list(spooled(data))
            registered = next(row for row in rows if row['body'].get('project_id') == 'safe-launch-project')
            self.assertEqual(registered['body']['agent_run_id'], run_id)
            identities = {(row['origin_id'], row['event_id']) for row in rows if not row.get('filtered')}
            for event_id in ('safe-vm-sample-0', 'safe-android-sample-0', 'safe-session', 'safe-after-tail'):
                self.assertIn(event_id, {event for _, event in identities})
            self.wait_committed(data, auth_file, identities)
            self.wait_committed(data, auth_file, identities)
            self.assertEqual(archive.read_bytes(), history)

            exported = self.cli('export', data)
            for line in exported.splitlines():
                row = json.loads(line)
                if row.get('protobuf'):
                    self.assert_clean(base64.b64decode(row['protobuf']), 'decoded protobuf export')
            original = next(json.loads(line) for line in exported.splitlines()
                            if json.loads(line)['event_id'] == registered['event_id'])
            changed = dict(original, body={**original['body'], 'task_id': PASSWORD, 'password': PASSWORD})
            later = dict(changed, event_id='safe-replay-event')
            payload = b''.join(json.dumps(row).encode()+b'\n' for row in (changed, later))
            self.cli('replay', data, input=payload)
            self.cli('replay', data, input=payload)  # Existing pair survives a restart.
            replayed = self.cli('export', data)
            self.assertIn(b'"event_id":"safe-replay-event"', replayed)
            self.assertIn(b'"session_id":"safe-session-id"', replayed)
            self.assertIn(b'"project_id":"safe-page-project"', replayed)
            self.assertIn(b'"cpu_ns":70', replayed)
            self.scan(data, 'replay and archives')


if __name__ == '__main__':
    unittest.main()
