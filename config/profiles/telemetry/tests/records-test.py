#!/usr/bin/env python3
import importlib.util
import fcntl
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import sys
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import records


class OutboxTest(unittest.TestCase):
    def test_capacity_scan_with_concurrent_consumer_preserves_admission_and_limits(self):
        for scenario in ('admit', 'count-cap', 'byte-cap', 'permission'):
            with self.subTest(scenario=scenario), tempfile.TemporaryDirectory(dir='/tmp') as name:
                root = Path(name)
                for event in ('consumed', 'retained'):
                    self.assertTrue(records.emit(root, 'gap', {'reason': 'synthetic'}, source_id='fixture', event_id=event))
                consumed = root/'outbox/consumed.json'
                retained = root/'outbox/retained.json'
                original = Path.stat

                def consumer_removes_before_stat(path, *args, **kwargs):
                    if path == consumed:
                        if scenario == 'permission':
                            raise PermissionError(13, 'synthetic permission denial')
                        path.unlink()
                    return original(path, *args, **kwargs)

                maximum_records = 1 if scenario == 'count-cap' else 2
                maximum_bytes = retained.stat().st_size+1 if scenario == 'byte-cap' else records.MAX_BYTES
                with patch.object(Path, 'stat', consumer_removes_before_stat), \
                     patch.object(records, 'MAX_RECORDS', maximum_records), patch.object(records, 'MAX_BYTES', maximum_bytes):
                    admitted = records.emit(root, 'gap', {'reason': 'new'}, source_id='fixture', event_id='next')
                self.assertEqual(admitted, scenario == 'admit')
                next_record = root/'outbox/next.json'
                self.assertEqual(next_record.exists(), scenario == 'admit')
                self.assertTrue(retained.exists())
                if admitted:
                    self.assertEqual(records.read_private(next_record)['event_id'], 'next')
                    self.assertEqual(next_record.stat().st_mode & 0o777, 0o600)
                    self.assertEqual(len(list((root/'outbox').glob('*.json'))), 2)
                else:
                    self.assertEqual(consumed.exists(), scenario == 'permission')

    def test_capacity_loss_is_private_bounded_and_emits_gap_after_recovery(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name, patch.object(records, 'MAX_RECORDS', 2):
            root = Path(name)
            self.assertTrue(records.emit(root, 'sample', {'object_id': 'one', 'cpu_ns': 1}))
            self.assertTrue(records.emit(root, 'sample', {'object_id': 'two', 'cpu_ns': 2}))
            self.assertFalse(records.emit(root, 'sample', {'object_id': 'PRIVATE_DROPPED_CONTENT', 'cpu_ns': 3}))
            marker = root/'producer-health.json'
            self.assertEqual(marker.stat().st_mode & 0o777, 0o600)
            self.assertNotIn('PRIVATE_DROPPED_CONTENT', marker.read_text())
            self.assertLess(marker.stat().st_size, 512)
            for path in (root/'outbox').glob('*.json'):
                path.unlink()
            self.assertTrue(records.emit(root, 'sample', {'object_id': 'recovered', 'cpu_ns': 4}))
            self.assertFalse(marker.exists())
            rows = [json.loads(path.read_text()) for path in (root/'outbox').glob('*.json')]
            gap = next(row for row in rows if row['kind'] == 'gap')
            self.assertEqual(gap['body']['missing'], 1)
            self.assertEqual(gap['body']['reason'], 'producer_outbox_capacity')

    def test_only_initial_run_registration_waits_for_private_control(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name, patch.object(records, 'deliver', return_value=True) as deliver:
            self.assertTrue(records.emit(name, 'run', {'agent_run_id': 'run', 'agent': 'other', 'state': 'starting'}))
            self.assertEqual(deliver.call_count, 1)
            for kind, body in [('sample', {'object_id': 'scope', 'cpu_ns': 1}),
                               ('association', {'object_id': 'scope', 'agent_run_id': 'run'}),
                               ('run', {'agent_run_id': 'run', 'state': 'main_exited'})]:
                self.assertTrue(records.emit(name, kind, body))
            self.assertEqual(deliver.call_count, 1)

    def test_retry_keeps_source_time_and_busy_outbox_never_waits(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            with patch.object(records, 'deliver', return_value=False):
                self.assertTrue(records.emit(root, 'run', {'state': 'starting'}, event_id='same'))
                self.assertTrue(records.emit(root, 'run', {'state': 'starting'}, event_id='same'))
                with (root/'outbox/.lock').open('rb') as lock:
                    fcntl.flock(lock, fcntl.LOCK_EX)
                    self.assertFalse(records.emit(root, 'run', {'state': 'starting'}))

    def test_private_durable_redacted_record_and_retry_identity(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            body = {'agent_run_id': 'run-1', 'state': 'starting', 'authorization': 'SECRET', 'argv': ['PRIVATE'], 'env': {'TOKEN': 'PRIVATE'}}
            with patch.object(records, 'deliver', return_value=False):
                self.assertTrue(records.emit(root, 'run', body, 'source', '2026-10-24T00:00:00Z', 'event-1'))
                self.assertTrue(records.emit(root, 'run', body, 'source', '2026-10-24T00:00:00Z', 'event-1'))
            files = list((root/'outbox').glob('*.json'))
            self.assertEqual(len(files), 1)
            value = json.loads(files[0].read_text())
            self.assertEqual(value['body'], {'agent_run_id': 'run-1', 'state': 'starting'})
            self.assertEqual(files[0].stat().st_mode & 0o777, 0o600)
            self.assertEqual((root/'outbox').stat().st_mode & 0o777, 0o700)
            self.assertNotIn('SECRET', files[0].read_text())
            self.assertFalse(list((root/'outbox').glob('*.tmp')))

    def test_capacity_disk_failure_and_symlink_are_nonfatal(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            with patch.object(records, 'atomic_json', side_effect=OSError('full')):
                self.assertFalse(records.emit(root, 'run', {'state': 'starting'}))
            outside = root/'outside'
            outside.mkdir(mode=0o700)
            (root/'outbox/.lock').unlink()
            (root/'outbox').rmdir()
            (root/'outbox').symlink_to(outside)
            self.assertFalse(records.emit(root, 'run', {'state': 'starting'}))
            self.assertFalse(list(outside.iterdir()))


if __name__ == '__main__':
    unittest.main()
