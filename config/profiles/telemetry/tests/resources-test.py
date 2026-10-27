#!/usr/bin/env python3
"""Owner resource cursor, explicit correlation and privacy contracts."""
import importlib.util
import fcntl
import io
from contextlib import redirect_stderr, redirect_stdout
import json
import os
import socket
import sqlite3
import stat
from pathlib import Path
import sys
import subprocess
import time
import tempfile
import threading
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import records
import resources
import sources

spec = importlib.util.spec_from_file_location('correlation', Path(__file__).resolve().parents[1] / 'broker-correlation.py')
correlation = importlib.util.module_from_spec(spec)
spec.loader.exec_module(correlation)
spec = importlib.util.spec_from_file_location('resource_observer', Path(__file__).resolve().parent / 'e2e/resource-observer.py')
resource_observer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(resource_observer)
spec = importlib.util.spec_from_file_location('storage_failure', Path(__file__).resolve().parent / 'e2e/storage-failure.py')
storage_failure = importlib.util.module_from_spec(spec)
spec.loader.exec_module(storage_failure)
NOW = '2026-10-24T00:00:00Z'
EVENT = '01792724400000000000-aaaaaaaaaaaaaaaa'


class Resources(unittest.TestCase):
    def test_observer_reports_private_read_failure_without_private_payload(self):
        path = self.root/'producer.json'
        records.atomic_json(path, dict(private_payload='never-print-this'))
        original = records.read_private

        def removed_before_read(target):
            if target == path:
                target.unlink()
            return original(target)

        stderr = io.StringIO()
        with patch.dict(os.environ, SUBYARD_E2E_VM='1', SUBYARD_AGENT_RUN_ID='fixture'), \
             patch.object(Path, 'is_file', return_value=True), \
             patch.object(resource_observer, 'configs', return_value=[dict(kind='vm')]), \
             patch.object(sources, 'merge_intents'), patch.object(sources, 'fetch', return_value=dict(events=[{}],more=False)), \
             patch.object(sources, 'consume', return_value=True), patch.object(records, 'read_private', side_effect=removed_before_read), \
             redirect_stderr(stderr):
            self.assertEqual(resource_observer.main(['--kind','vm','--stage','held','--data-dir',str(self.root)]), 1)
        diagnostic = json.loads(stderr.getvalue().split(' unavailable ',1)[1])
        self.assertEqual(diagnostic['error_class'], 'FileNotFoundError')
        self.assertEqual(diagnostic['source'], 'records.py')
        self.assertEqual(diagnostic['errno'], 2)
        self.assertGreater(diagnostic['line'], 0)
        self.assertGreater(diagnostic['callsite_line'], 0)
        for private in (str(self.root), path.name, 'never-print-this'):
            self.assertNotIn(private, stderr.getvalue())

    def test_outbox_drain_requires_actual_canonical_commit_when_collector_unlinks(self):
        import contextlib
        with contextlib.closing(sqlite3.connect(self.root/'telemetry.sqlite')) as db:
            db.execute('CREATE TABLE records(event_id TEXT, source_id TEXT, origin_id TEXT, encoding TEXT)')
            db.commit()
        records.atomic_json(self.root/'producer.json',dict(source_id='fixture-source'))
        outbox = records.private_directory(self.root/'outbox')
        original = records.read_private
        for scenario in ('committed','deleted','foreign-source','ambiguous','permission','failed-delivery'):
            with self.subTest(scenario=scenario):
                path = outbox/(scenario+'.json')
                records.atomic_json(path,dict(source_id='fixture-source',event_id=scenario))

                def concurrent_reader(target):
                    if target == path:
                        if scenario == 'permission':
                            raise PermissionError(13,'private error',str(target))
                        if scenario != 'failed-delivery':
                            with contextlib.closing(sqlite3.connect(self.root/'telemetry.sqlite')) as db:
                                for source in (['fixture-source'] if scenario == 'committed' else
                                               ['foreign'] if scenario == 'foreign-source' else
                                               ['fixture-source','foreign'] if scenario == 'ambiguous' else []):
                                    db.execute('INSERT INTO records VALUES(?,?,?,?)',(scenario,source,'producer:'+source,'json'))
                                db.commit()
                            target.unlink()
                    return original(target)

                with patch.object(records,'read_private',side_effect=concurrent_reader), patch.object(records,'deliver',return_value=False):
                    if scenario == 'committed':
                        resource_observer.drain_outbox(self.root)
                        with contextlib.closing(sqlite3.connect(self.root/'telemetry.sqlite')) as db:
                            self.assertEqual(db.execute('SELECT source_id,event_id FROM records WHERE event_id=?',(scenario,)).fetchall(),
                                             [('fixture-source',scenario)])
                    elif scenario == 'permission':
                        with self.assertRaises(PermissionError):
                            resource_observer.drain_outbox(self.root)
                    else:
                        with self.assertRaises(resource_observer.Failure):
                            resource_observer.drain_outbox(self.root)
                path.unlink(missing_ok=True)

    def test_observer_key_diagnostic_is_closed_and_does_not_render_payload(self):
        for field in ('transport','secret-private-field'):
            try:
                raise KeyError(field)
            except KeyError as error:
                diagnostic = resource_observer.failure_details(error)
            self.assertEqual(diagnostic.get('field'), 'transport' if field == 'transport' else None)
            self.assertNotIn('secret-private-field', json.dumps(diagnostic))
        self.assertNotIn('private-config-value', json.dumps(resource_observer.failure_details(ValueError('private-config-value'))))

    def test_capture_wait_for_normal_reader_lock_is_bounded(self):
        with patch.object(sources,'intent_lock',side_effect=BlockingIOError), \
             patch.object(resource_observer.time,'monotonic',side_effect=[0,0,16]), patch.object(resource_observer.time,'sleep') as sleep:
            with self.assertRaisesRegex(resource_observer.Failure,'reader-lock deadline'):
                resource_observer.capture(self.root,'vm')
        sleep.assert_called_once_with(.05)

    def test_resource_samples_from_sibling_allocation_cannot_satisfy_current_run(self):
        rows = {'allocations':[dict(object_id=name,object_kind='vm',allocated_memory_bytes=123) for name in ('current','sibling')],
                'associations':[dict(object_id='current',agent_run_id='current-run',quality='explicit')],
                'samples':[dict(object_id='sibling',cpu_ns=1,memory_bytes=1,counter_generation='counter',io_read_bytes=1,io_write_bytes=1)]}
        with patch.dict(os.environ,SUBYARD_AGENT_RUN_ID='current-run'), patch.object(resource_observer,'read_rows',side_effect=lambda data,table:rows[table]):
            with self.assertRaisesRegex(resource_observer.Failure,'sample for the managed allocation'):
                resource_observer.assert_records(self.root,'vm','held')

    def test_storage_command_preserves_vm_and_android_argv_through_both_parsers(self):
        commands = [
            ['bash', '/tmp/fixture with spaces/dev/agent-e2e.sh', '--yard', 'test-yard', '--slot', '1',
             '--vm-count', '1', '--purpose', 'telemetry-storage-failure', '--vm', '1', '--', 'bash', '-c', 'sleep 2'],
            ['/tmp/fixture with spaces/yard', '-Y', 'test-yard', 'emu', 'run', '--purpose', 'telemetry-storage-failure',
             '--', 'python3', '-c', 'import time; time.sleep(2)', '--native-command', '--flaglike-child-arg']]
        for kind, command in zip(('vm', 'android'), commands):
            received = []

            def forward(argv, **kwargs):
                self.assertEqual(argv[:2], [sys.executable, '-B'])
                self.assertEqual(Path(argv[2]).name, 'storage-failure.py')
                storage_failure.main(argv[3:])

            with self.subTest(kind=kind), patch.dict(os.environ, SUBYARD_E2E_VM='1', SUBYARD_AGENT_RUN_ID='fixture'), \
                 patch.object(Path, 'is_file', return_value=True), patch.object(resource_observer.subprocess, 'run', side_effect=forward), \
                 patch.object(storage_failure, 'run', side_effect=received.append), redirect_stdout(io.StringIO()):
                self.assertEqual(resource_observer.main(['--kind', kind, '--stage', 'storage-failure', '--data-dir', str(self.root),
                    '--evidence', str(self.root/'evidence'), '--native-command', *command]), 0)
            self.assertEqual(len(received), 1)
            self.assertEqual(received[0].native_command, command)

    def test_storage_command_rejects_empty_tail_and_unknown_prefix_in_both_layers(self):
        cases = [(resource_observer.main, ['--kind', 'vm', '--stage', 'storage-failure', '--data-dir', '/tmp/fixture']),
                 (storage_failure.main, ['--binary', '/tmp/binary', '--evidence', '/tmp/evidence'])]
        for main, prefix in cases:
            for tail in (['--native-command'], ['--native-command', '--'], ['--unknown-prefix', '--native-command', 'true']):
                with self.subTest(main=main.__module__, tail=tail), redirect_stderr(io.StringIO()):
                    with self.assertRaises(SystemExit) as error:
                        main([*prefix, *tail])
                    self.assertEqual(error.exception.code, 2)

    def test_storage_hook_wrapper_restores_exact_file_and_refuses_foreign_state(self):
        helper = self.root/'broker-correlation.py'
        original = b'#!/usr/bin/env python3\nprint("fixture native helper")\n'
        helper.write_bytes(original)
        helper.chmod(0o755)
        probe = self.root/'probe'
        probe.mkdir(mode=0o700)
        storage_failure.correlation_probe(probe, helper)
        original_lstat, original_run = Path.lstat, subprocess.run

        def root_identity(path, *args, **kwargs):
            info = original_lstat(path, *args, **kwargs)
            if path == helper:
                fields = list(info)
                fields[4] = 0  # Only the leased-stage root ownership boundary is simulated.
                return os.stat_result(fields)
            return info

        def privileged(command, **values):
            self.assertEqual(command[:2], ['sudo', '-n'])
            return original_run(command[2:], **values)  # Real atomic file operations under /tmp only.

        with patch.object(storage_failure, 'HOOK', helper), patch.object(Path, 'lstat', root_identity), \
             patch.object(storage_failure.subprocess, 'run', side_effect=privileged):
            cleanup = []
            with self.assertRaisesRegex(RuntimeError, 'native failure'):
                with storage_failure.installed_probe(probe, helper, cleanup):
                    self.assertNotEqual(helper.read_bytes(), original)
                    raise RuntimeError('native failure')
            self.assertEqual(cleanup, [])
            self.assertEqual(helper.read_bytes(), original)
            self.assertEqual(stat.S_IMODE(helper.stat().st_mode), 0o755)
            with self.assertRaises(subprocess.CalledProcessError):
                storage_failure.replace_hook(helper, '0'*64, b'foreign replacement')
            self.assertEqual(helper.read_bytes(), original)
            self.assertFalse(list(self.root.glob('.broker-correlation-probe.*')))

            cleanup = []
            with self.assertRaisesRegex(RuntimeError, 'primary native failure'):
                with storage_failure.installed_probe(probe, helper, cleanup):
                    helper.write_bytes(b'foreign mutation')
                    raise RuntimeError('primary native failure')
            self.assertEqual(len(cleanup), 1)
            self.assertIsInstance(cleanup[0], subprocess.CalledProcessError)
            self.assertEqual(helper.read_bytes(), b'foreign mutation')

            helper.write_bytes(original)
            helper.chmod(0o644)
            with self.assertRaises(AssertionError):
                with storage_failure.installed_probe(probe, helper, []):
                    self.fail('wrong-mode installed hook admitted')
            helper.unlink()
            target = self.root/'foreign-target'
            target.write_bytes(original)
            target.chmod(0o755)
            helper.symlink_to(target)
            with self.assertRaises(AssertionError):
                with storage_failure.installed_probe(probe, helper, []):
                    self.fail('symlink installed hook admitted')
            self.assertEqual(target.read_bytes(), original)

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='telemetry-resources-', dir='/tmp')
        self.root = Path(self.temp.name)
        self.config = dict(kind='vm', owner_id='owner', source_id='resource-fixture',
                           cursor='-', registered_at=NOW,
                           associations={'broker-run': dict(agent_run_id='agent-run', start_time=NOW,
                               slot_id='slot-001',resource_generation=2,lease_epoch=3)})

    def tearDown(self):
        self.temp.cleanup()

    def rows(self):
        return [records.read_private(path) for path in (self.root / 'outbox').glob('*.json')]

    def page(self):
        event = dict(event_id=EVENT, timestamp=NOW, kind='lease.available', to_state='available',
                     slot_id='slot-001', resource_generation=2, lease_epoch=3,
                     capability='must-not-be-analytics', error='private failure text',
                     allocation=dict(slot_id='slot-001', resource_generation=2, lease_epoch=3, state='draining',
                     context=dict(run='broker-run', yard='yard', project='project', token='private-token'),
                     environment=dict(vm_count=2, cpu_per_vm=4, memory_per_vm='4GiB')))
        return dict(owner_id='a' * 64, events=[event], next_cursor=EVENT, gap=False, more=False, observations=[])

    def held(self, correlation_id='broker-run', generation=2):
        return dict(kind='vm',phase='held',owner_id='owner',correlation_id=correlation_id,
            slot_id='slot-001',resource_generation=str(generation),lease_epoch='3',
            transport=dict(script='/tmp/fixture/dev/agent-e2e.sh',yard='owner'))

    def test_reader_lock_cannot_drop_durable_registration_and_restart_merge(self):
        root = records.private_directory(self.root/'sources')
        fd = os.open(root/'.lock',os.O_CREAT|os.O_RDWR,0o600)
        os.fchmod(fd,0o600)
        with os.fdopen(fd,'rb') as lock:
            fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
            self.assertTrue(correlation.register(self.root,'agent-run',self.held()))
            intent = next((root/'intents').glob('intent-*.json'))
            before = intent.read_bytes()
            self.assertEqual(intent.stat().st_mode & 0o777,0o600)
            self.assertTrue(correlation.register(self.root,'agent-run',self.held()))
            self.assertEqual(intent.read_bytes(),before)
            self.assertFalse(sources.poll(self.root))
        with patch.object(sources,'fetch',return_value=self.page()):
            self.assertTrue(sources.poll(self.root))
        self.assertFalse(list((root/'intents').glob('intent-*.json')))
        config = records.read_private(next(root.glob('*.json')))
        self.assertEqual(config['associations']['broker-run']['agent_run_id'],'agent-run')
        self.assertEqual(len([row for row in self.rows() if row['kind']=='association']),2)

    def test_late_mapping_rewinds_completed_journal_and_duplicate_does_not_rewind(self):
        request = self.held()
        root = records.private_directory(self.root/'sources')
        path, _ = sources.registration_paths(root,request)
        config = dict(self.config,owner_id='a'*64,cursor=EVENT,associations={},transport=request['transport'])
        records.atomic_json(path,config)
        self.assertTrue(correlation.register(self.root,'agent-run',request))
        cursors=[]
        def fetch(config):
            cursors.append(config['cursor'])
            return self.page()
        with patch.object(sources,'fetch',side_effect=fetch):
            self.assertTrue(sources.poll(self.root))
        self.assertEqual(cursors,['-'])
        first = self.rows()
        self.assertEqual(len([row for row in first if row['kind']=='association']),2)
        self.assertTrue(correlation.register(self.root,'agent-run',request))
        with patch.object(sources,'fetch',side_effect=fetch):
            self.assertTrue(sources.poll(self.root))
        self.assertEqual(cursors,['-',EVENT])
        self.assertEqual(len(self.rows()),len(first))

    def test_registration_conflict_generation_fencing_and_capacity_are_diagnostic(self):
        request = self.held()
        self.assertTrue(correlation.register(self.root,'agent-run',request))
        self.assertFalse(correlation.register(self.root,'other-run',request))
        self.assertFalse(correlation.register(self.root,'agent-run',dict(request,resource_generation='3')))
        self.assertFalse(correlation.register(self.root,'agent-run',dict(request,unknown='private')))
        self.assertFalse(correlation.register(self.root,'agent-run',dict(request,resource_generation=True)))
        self.assertFalse(correlation.register(self.root,'agent-run',dict(request,transport=dict(request['transport'],unknown='private'))))
        with patch.object(sources,'MAX_INTENTS',1):
            self.assertFalse(correlation.register(self.root,'second-run',self.held('second',3)))
        with patch.object(sources,'MAX_INTENT_BYTES',1):
            self.assertFalse(correlation.register(self.root,'second-run',self.held('second',3)))
        reasons={row['body'].get('reason') for row in self.rows()}
        self.assertTrue({'correlation_registration_conflict','correlation_registration_capacity','correlation_registration_invalid'} <= reasons)
        sources.merge_intents(self.root,self.root/'sources')
        self.assertFalse(correlation.register(self.root,'other-run',request))
        self.assertFalse(correlation.register(self.root,'agent-run',dict(request,resource_generation='3')))
        self.assertFalse(correlation.register(self.root,'other-run',self.held('collision',2)))
        alternate = dict(request,transport=dict(request['transport'],route='other-route'))
        self.assertFalse(correlation.register(self.root,'other-run',alternate))
        config = dict(self.config,associations={'broker-run':dict(agent_run_id='wrong',slot_id='slot-001',resource_generation=3,lease_epoch=3)})
        self.assertTrue(sources.consume(self.root,config,self.page()))
        self.assertFalse(any(row['kind']=='association' for row in self.rows()))
        self.assertTrue(any(row['body'].get('reason')=='correlation_generation_mismatch' for row in self.rows()))

    def test_mapping_arriving_during_fetch_prevents_old_cursor_commit(self):
        request = self.held()
        root = records.private_directory(self.root/'sources')
        path, _ = sources.registration_paths(root,request)
        records.atomic_json(path,dict(self.config,associations={},cursor=EVENT,transport=request['transport']))
        def fetch(config):
            self.assertTrue(correlation.register(self.root,'agent-run',request))
            return self.page()
        with patch.object(sources,'fetch',side_effect=fetch):
            self.assertTrue(sources.poll(self.root))
        self.assertEqual(records.read_private(path)['cursor'],'-')
        self.assertFalse(any(row['kind']=='association' for row in self.rows()))
        with patch.object(sources,'fetch',return_value=self.page()):
            self.assertTrue(sources.poll(self.root))
        self.assertEqual(len([row for row in self.rows() if row['kind']=='association']),2)

    def test_registration_failure_marker_recovers_after_outbox_admission(self):
        with patch.object(records,'emit',return_value=False):
            self.assertFalse(correlation.register(self.root,'agent-run',dict(self.held(),unknown='private')))
        marker = self.root/'sources/intents/registration-health'
        self.assertTrue(marker.exists())
        self.assertNotIn('private',marker.read_text())
        sources.merge_intents(self.root,self.root/'sources')
        self.assertFalse(marker.exists())
        self.assertTrue(any(row['body'].get('reason')=='correlation_registration_invalid' for row in self.rows()))

    def test_invalid_pending_shape_is_diagnostic_and_does_not_poison_observer(self):
        self.assertTrue(correlation.register(self.root,'agent-run',self.held()))
        intent = next((self.root/'sources/intents').glob('intent-*.json'))
        records.atomic_json(intent,dict(records.read_private(intent),unknown='private-content'))
        self.assertTrue(sources.poll(self.root))
        self.assertFalse(intent.exists())
        self.assertTrue(any(row['body'].get('reason')=='correlation_registration_invalid' for row in self.rows()))
        self.assertNotIn('private-content',json.dumps(self.rows()))

    def test_merge_during_other_reader_fetch_does_not_overwrite_new_mapping(self):
        request = self.held()
        root = records.private_directory(self.root/'sources')
        second, _ = sources.registration_paths(root,request)
        first_request = dict(request,transport=dict(request['transport'],yard='first'))
        first, _ = sources.registration_paths(root,first_request)
        for path, transport, order in ((first,first_request['transport'],'0'),(second,request['transport'],'1')):
            records.atomic_json(path,dict(self.config,associations={},cursor=EVENT,transport=transport,polled_at=order))
        def fetch(config):
            if config['transport']['yard']=='first':
                self.assertTrue(correlation.register(self.root,'agent-run',request))
                return dict(self.page(),events=[])
            self.assertEqual(config['associations']['broker-run']['agent_run_id'],'agent-run')
            self.assertEqual(config['cursor'],'-')
            return self.page()
        with patch.object(sources,'fetch',side_effect=fetch):
            self.assertTrue(sources.poll(self.root))
        self.assertEqual(records.read_private(second)['associations']['broker-run']['agent_run_id'],'agent-run')
        self.assertEqual(len([row for row in self.rows() if row['kind']=='association']),2)

    def test_two_vm_objects_once_explicit_mapping_replay_and_no_private_data(self):
        self.assertTrue(sources.consume(self.root, self.config, self.page()))
        first = self.rows()
        self.assertEqual(len([row for row in first if row['kind'] == 'allocation']), 2)
        edges = [row['body'] for row in first if row['kind'] == 'association']
        self.assertEqual(len(edges), 2)
        self.assertTrue(all(edge['agent_run_id'] == 'agent-run' for edge in edges))
        self.assertEqual(len([row for row in first if row['kind'] == 'gap']), 2)
        self.assertTrue(sources.consume(self.root, self.config, self.page()))
        self.assertEqual(len(self.rows()), len(first))
        text = json.dumps(self.rows())
        for private in ('must-not-be-analytics', 'private failure text', 'private-token'):
            self.assertNotIn(private, text)
        allocation = next(row['body'] for row in first if row['kind'] == 'allocation')
        self.assertEqual(allocation['allocated_memory_bytes'], 4 * 1024**3)
        self.assertEqual(allocation['requested_memory_bytes'], 4 * 1024**3)
        self.assertEqual(allocation['reserved_memory_bytes'], 4 * 1024**3)
        self.assertEqual(allocation['reserved_cpu'], 4)
        self.assertIsNone(allocation['reserved_disk_bytes'])

    def test_android_reserves_only_native_known_capacity(self):
        body = resources.allocation(dict(self.config, kind='android'),
            dict(slot_id='001', generation=2, reserved_memory_bytes=1024**3,
                 request=dict(memory_bytes=768 * 1024**2)))
        self.assertEqual(body['reserved_memory_bytes'], 1024**3)
        self.assertEqual(body['requested_memory_bytes'], 768 * 1024**2)
        self.assertIsNone(body['reserved_cpu'])
        self.assertIsNone(body['reserved_disk_bytes'])
        self.assertEqual(body['reservation_semantics'], 'allocated_virtual_capacity_held')

    def test_cursor_not_advanced_on_failed_durable_admission(self):
        with patch.object(records, 'emit', return_value=False):
            self.assertFalse(sources.consume(self.root, self.config, self.page()))
        self.assertEqual(self.config['cursor'], '-')

    def test_nullable_counter_identity_and_repeated_sample(self):
        self.config['associations'] = {}
        native = dict(slot_id='slot-001', resource_generation=2, lease_epoch=3, selector='1',
                      source_time=NOW, monotonic_ns=123, counter_generation='first-counter', cpu_ns=1234,
                      memory_bytes=4321, missing=['io_read_bytes'])
        self.assertTrue(resources.emit_sample(self.root, self.config, native))
        self.assertTrue(resources.emit_sample(self.root, self.config, native))
        self.assertEqual(len(self.rows()), 1)
        self.assertIsNone(self.rows()[0]['body']['io_read_bytes'])
        self.assertEqual(self.rows()[0]['body']['quality'], 'partial')
        native['counter_generation'] = 'replacement'
        self.assertTrue(resources.emit_sample(self.root, self.config, native))
        self.assertEqual(len(self.rows()), 2)
        native['cpu_ns'] = 2**64 - 1
        native['monotonic_ns'] = 124
        self.assertTrue(resources.emit_sample(self.root, self.config, native))
        self.assertTrue(any(row['body'].get('cpu_ns') == 2**64 - 1 for row in self.rows()))
        self.assertNotEqual(resources.object_id('vm', 'owner', 'slot-001', 2, 3, '1'),
                            resources.object_id('vm', 'owner', 'slot-001', 3, 4, '1'))

    def test_capability_only_private_registration_not_outbox(self):
        request = dict(kind='android', phase='held', owner_id='owner', correlation_id='broker-run',
                       slot_id='001', resource_generation=2,
                       transport=dict(control='/srv/cache/android/.subyard/control.sock', capability='b' * 64))
        self.assertTrue(correlation.register(self.root, 'agent-run', request))
        sources.merge_intents(self.root,self.root/'sources')
        config = records.read_private(next((self.root / 'sources').glob('*.json')))
        self.assertEqual(config['transport']['capability'], 'b' * 64)
        self.assertNotIn('b' * 64, json.dumps(self.rows()))
        self.assertEqual(next((self.root / 'sources').glob('*.json')).stat().st_mode & 0o777, 0o600)

    def test_distinct_android_readers_do_not_replace_an_unfinished_source(self):
        for index, capability in enumerate(('a' * 64, 'b' * 64)):
            request = dict(kind='android', phase='held', owner_id='owner', correlation_id='run-' + str(index),
                           slot_id='001', resource_generation=index + 1,
                           transport=dict(control='/srv/cache/android/.subyard/control.sock', capability=capability))
            self.assertTrue(correlation.register(self.root, 'agent-' + str(index), request))
        sources.merge_intents(self.root,self.root/'sources')
        readers = [records.read_private(path) for path in (self.root / 'sources').glob('*.json')]
        self.assertEqual(len(readers), 2)
        self.assertEqual({item['transport']['capability'] for item in readers}, {'a' * 64, 'b' * 64})
        self.assertEqual(len({item['source_id'] for item in readers}), 1)
        for capability in ('a' * 64, 'b' * 64):
            self.assertNotIn(capability, json.dumps(self.rows()))

    def test_collector_projects_one_owner_interval_and_actual_cpu(self):
        binary = self.root / 'collector'
        subprocess.run(['go', 'build', '-mod=readonly', '-o', str(binary),
                        './config/profiles/telemetry/cmd/telemetry'],
                       cwd=Path(__file__).resolve().parents[4], check=True,
                       stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        data = self.root / 'data'
        data.mkdir(mode=0o700)
        token = self.root / 'auth'
        token.write_text('synthetic-fixture-token-0123456789abcdef')
        token.chmod(0o600)
        process = subprocess.Popen([str(binary), 'serve', '--data-dir', str(data), '--listen', '127.0.0.1:0',
                                    '--auth-file', str(token)], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        try:
            deadline = time.monotonic() + 10
            while not (data / 'control.sock').exists() and time.monotonic() < deadline and process.poll() is None:
                time.sleep(0.02)
            self.assertIsNone(process.poll())
            self.assertTrue((data / 'control.sock').exists())
            config = dict(self.config, owner_id='a' * 64)
            config['associations'] = {'broker-run': dict(agent_run_id='agent-run', start_time='2026-10-24T00:00:05Z',
                                      slot_id='slot-001', resource_generation=2, lease_epoch=3)}
            allocation = dict(slot_id='slot-001', resource_generation=2, lease_epoch=3, state='held',
                              acquired_at=NOW, context=dict(run='broker-run', purpose='bounded-workload'),
                              environment=dict(vm_count=1, cpu_per_vm=4, memory_per_vm='4GiB', disk_per_vm='20GiB'))
            def sample(second, cpu):
                return dict(slot_id='slot-001', resource_generation=2, lease_epoch=3, selector='1',
                            source_time=f'2026-10-24T00:00:{second:02}Z', boot_id='fixture-boot',
                            monotonic_ns=second * 1000000000, counter_generation='qemu-generation', cpu_ns=cpu,
                            memory_bytes=1024**2)
            self.assertTrue(records.emit(data, 'run', dict(agent_run_id='agent-run', state='starting'),
                                         source_id='run-fixture', source_time=NOW, event_id='run'))
            baseline = dict(event_id='baseline', timestamp='2026-10-24T00:00:01Z', kind='resource.observation',
                            allocation=allocation, observations=[sample(1, 100)])
            self.assertTrue(resources.emit_event(data, config, baseline))
            periodic = dict(owner_id='a' * 64, events=[], next_cursor='-', gap=False, more=False,
                            observed_at='2026-10-24T00:00:10Z', allocations=[allocation], observations=[sample(10, 600)])
            self.assertTrue(sources.consume(data, config, periodic))
            # Use native transition state, not an assumed event-kind spelling.
            stop = dict(event_id='stop', timestamp='2026-10-24T00:00:21Z', kind='lease.transition',
                        to_state='available', allocation=allocation, observations=[sample(20, 1100)])
            self.assertTrue(resources.emit_event(data, config, stop))
            # A delayed open sample association must not reopen the confirmed end.
            self.assertTrue(resources.emit_sample(data, config, dict(sample(10, 600), acquired_at=NOW), 'late-periodic'))
            for path in (data / 'outbox').glob('*.json'):
                self.assertTrue(records.deliver(data, records.read_private(path)))
            result = json.loads(subprocess.check_output([str(binary), 'show-run', '--data-dir', str(data), '--run', 'agent-run']))
            self.assertEqual(len(result['selected_nonoverlapping']), 1)
            selected = result['selected_nonoverlapping'][0]
            self.assertEqual(selected['cpu_ns_delta'], 1000)
            self.assertEqual(float(selected['allocated_holding_seconds']), 21)
            self.assertEqual(float(selected['allocated_vcpu_seconds']), 84)
            self.assertEqual(float(selected['reserved_vcpu_seconds']), 84)
            self.assertEqual(float(selected['reserved_memory_byte_seconds']), 4 * 1024**3 * 21)
            self.assertEqual(selected['latest_sample']['memory_bytes'], 1024**2)
            self.assertIsNone(selected['current_memory_bytes'])
            totals = result['totals']['broker_actual']
            self.assertEqual(totals['cpu_ns_delta']['known_sum'], 1000)
            self.assertEqual(float(totals['allocated_vcpu_seconds']['known_sum']), 84)
            self.assertEqual(float(totals['reserved_vcpu_seconds']['known_sum']), 84)
            self.assertEqual(float(totals['reserved_memory_byte_seconds']['known_sum']), 4 * 1024**3 * 21)
            body = selected['object']
            self.assertEqual(body['purpose'], 'bounded-workload')
            self.assertEqual(body['requested_disk_bytes'], 20 * 1024**3)
            self.assertEqual(body['allocated_disk_bytes'], 20 * 1024**3)
            self.assertEqual(body['requested_memory_bytes'], 4 * 1024**3)
            self.assertEqual(body['allocated_memory_bytes'], 4 * 1024**3)
            self.assertEqual(body['allocated_cpu'], 4)
            self.assertEqual(body['reserved_disk_bytes'], 20 * 1024**3)
            self.assertEqual(body['reservation_semantics'], 'allocated_virtual_capacity_held')
            rows = json.loads(subprocess.check_output([str(binary), 'query', '--data-dir', str(data),
                              '--sql', 'SELECT count(*) AS edges,min(start_time) AS start,max(end_time) AS end FROM associations']))
            self.assertEqual(rows[0]['edges'], 1)
            self.assertEqual(rows[0]['start'], '2026-10-24T00:00:00.000000000Z')
            self.assertEqual(rows[0]['end'], '2026-10-24T00:00:21.000000000Z')
            # A short allocation can finish and advance the source cursor before
            # its held callback is merged. Replay must add exactly one real edge.
            request = self.held('late-correlation',4)
            request.update(slot_id='slot-002',lease_epoch='5')
            source_root = records.private_directory(data/'sources')
            source_path, _ = sources.registration_paths(source_root,request)
            records.atomic_json(source_path,dict(kind='vm',owner_id='a'*64,source_id='late-source',
                cursor=EVENT,registered_at=NOW,associations={},transport=request['transport']))
            self.assertTrue(records.emit(data,'run',dict(agent_run_id='late-agent',state='starting')))
            late = dict(slot_id='slot-002',resource_generation=4,lease_epoch=5,state='draining',acquired_at=NOW,
                context=dict(run='late-correlation'),environment=dict(vm_count=1,cpu_per_vm=2,memory_per_vm='1GiB'))
            page = dict(owner_id='a'*64,events=[dict(event_id='late-stop',timestamp='2026-10-24T00:00:21Z',
                kind='lease.transition',to_state='available',allocation=late,observations=[dict(sample(20,1100),
                    slot_id='slot-002',resource_generation=4,lease_epoch=5,phase='pre_stop',io_read_bytes=20,io_write_bytes=30)])],
                observations=[],allocations=[],next_cursor=EVENT,gap=False,more=False)
            self.assertTrue(correlation.register(data,'late-agent',request))
            self.assertTrue(list((source_root/'intents').glob('intent-*.json')))
            self.assertEqual(records.read_private(source_path)['cursor'],EVENT)
            cursors=[]
            def retained(config):
                cursors.append(config['cursor'])
                return page
            with patch.object(sources,'fetch',side_effect=retained), patch.dict(os.environ,SUBYARD_AGENT_RUN_ID='late-agent'), redirect_stdout(io.StringIO()):
                resource_observer.capture(data,'vm')
                resource_observer.assert_records(data,'vm','final')
            self.assertEqual(cursors,['-'])
            self.assertTrue(correlation.register(data,'late-agent',request))
            with patch.object(sources,'fetch',side_effect=retained):
                self.assertTrue(sources.poll(data))
            self.assertEqual(cursors,['-','-'])  # Independent capture did not advance the service cursor.
            self.assertTrue(correlation.register(data,'late-agent',request))
            with patch.object(sources,'fetch',side_effect=retained):
                self.assertTrue(sources.poll(data))
            self.assertEqual(cursors,['-','-',EVENT])
            for pending in (data/'outbox').glob('*.json'):
                self.assertTrue(records.deliver(data,records.read_private(pending)))
            rows=json.loads(subprocess.check_output([str(binary),'query','--data-dir',str(data),'--sql',
                "SELECT count(*) AS edges,min(start_time) AS start,max(end_time) AS end FROM associations WHERE agent_run_id='late-agent'"]))
            self.assertEqual(rows[0],dict(edges=1,start='2026-10-24T00:00:00.000000000Z',end='2026-10-24T00:00:21.000000000Z'))
            with patch.dict(os.environ,SUBYARD_AGENT_RUN_ID='unknown-agent'):
                with self.assertRaisesRegex(resource_observer.Failure,'explicit managed-run association'):
                    resource_observer.assert_records(data,'vm','held')
            # The current agent cannot inherit a sibling's edge when its own
            # declared grant generation disagrees with the real native event.
            wrong_request = self.held('wrong-correlation',6)
            wrong_request.update(slot_id='slot-003',lease_epoch='7')
            self.assertTrue(records.emit(data,'run',dict(agent_run_id='wrong-generation',state='starting')))
            self.assertTrue(correlation.register(data,'wrong-generation',wrong_request))
            wrong_allocation = dict(late,slot_id='slot-003',resource_generation=5,lease_epoch=7,
                                    context=dict(run='wrong-correlation'))
            wrong_sample = dict(sample(20,100),slot_id='slot-003',resource_generation=5,lease_epoch=7,
                                io_read_bytes=1,io_write_bytes=1)
            wrong_page = dict(page,events=[dict(event_id='wrong-stop',timestamp=NOW,kind='lease.transition',
                             to_state='available',allocation=wrong_allocation,observations=[wrong_sample])])
            with patch.object(sources,'fetch',return_value=wrong_page), patch.dict(os.environ,SUBYARD_AGENT_RUN_ID='wrong-generation'):
                resource_observer.capture(data,'vm')
                with self.assertRaisesRegex(resource_observer.Failure,'explicit managed-run association'):
                    resource_observer.assert_records(data,'vm','held')
            # A real Unix reader fails on an isolated absent endpoint, commits its
            # stale gap, then reads the original endpoint again without cursor writes.
            control = self.root/'control.sock'
            with socket.socket(socket.AF_UNIX) as server:
                server.bind(str(control))
                server.listen(2)
                server.settimeout(5)
                failures = []
                def respond():
                    try:
                        for _ in range(2):
                            connection, _ = server.accept()
                            with connection:
                                connection.settimeout(5)
                                connection.recv(4096)
                                page = dict(owner_id='a'*64, events=[], observations=[], allocations=[],
                                            next_cursor=EVENT, gap=False, more=False, observed_at=NOW)
                                connection.sendall((json.dumps(dict(ok=True, result=page))+'\n').encode())
                    except Exception as error:
                        failures.append(type(error).__name__)
                worker = threading.Thread(target=respond, daemon=True)
                worker.start()
                observer = dict(kind='android', source_id='native-reader-fixture', owner_id='a'*64,
                                cursor=EVENT, last_seen=NOW, registered_at=NOW, associations={},
                                transport=dict(control=str(control), capability='b'*64))
                path = records.private_directory(data/'sources')/'native-reader.json'
                records.atomic_json(path, observer)
                before = path.read_bytes()
                resource_observer.offline_reader(data, 'android')
                self.assertEqual(path.read_bytes(), before)
                worker.join(timeout=5)
                self.assertFalse(worker.is_alive())
                self.assertEqual(failures, [])
        finally:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()

    def test_expiry_does_not_close_the_resource_interval(self):
        event = self.page()['events'][0]
        event.update(kind='lease.expired', to_state='draining')
        event['allocation']['acquired_at'] = NOW
        self.assertTrue(resources.emit_event(self.root, self.config, event))
        rows = self.rows()
        self.assertFalse(any(row['body'].get('stopped_at') for row in rows))
        self.assertTrue(all(row['body'].get('end_time') is None for row in rows if row['kind'] == 'association'))
        self.assertFalse(any(row['body'].get('reason') == 'final_tail_missing' for row in rows))

    def test_retention_gap_preserved_and_replay_stable(self):
        page = self.page()
        page['gap'] = True
        self.assertTrue(sources.consume(self.root, self.config, page))
        before = self.rows()
        # Re-delivery uses the original independently persisted cursor after an interrupted commit.
        self.config['cursor'] = '-'
        self.assertTrue(sources.consume(self.root, self.config, page))
        self.assertEqual(len(self.rows()), len(before))
        self.assertTrue(any(row['body'].get('reason') == 'cursor_retention_or_reset' for row in before))


if __name__ == '__main__':
    unittest.main()
