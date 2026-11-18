#!/usr/bin/env python3
import fcntl
import os
import subprocess
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import sys
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import records


def pending(root, event_id):
    return root/'outbox'/(records.opaque_id(records.identity_key(root), event_id)+'.json')


class OutboxTest(unittest.TestCase):
    def test_first_use_key_contention_does_not_take_the_outbox_lock(self):
        code = 'import records,sys; sys.exit(records.emit(sys.argv[1], "gap", {}))'
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            directory = os.open(root, os.O_RDONLY | os.O_DIRECTORY)
            try:
                fcntl.flock(directory, fcntl.LOCK_EX)
                reply = subprocess.run([sys.executable, '-B', '-c', code, str(root)], timeout=2,
                                       cwd=Path(records.__file__).parent, check=False)
                self.assertEqual(reply.returncode, 0)
                self.assertFalse((root/'outbox').exists())
            finally:
                os.close(directory)
            self.assertTrue(records.emit(root, 'gap', {}))

    def test_capacity_scan_with_concurrent_consumer_preserves_admission_and_limits(self):
        for scenario in ('admit', 'count-cap', 'byte-cap', 'permission'):
            with self.subTest(scenario=scenario), tempfile.TemporaryDirectory(dir='/tmp') as name:
                root = Path(name)
                for event in ('consumed', 'retained'):
                    self.assertTrue(records.emit(root, 'gap', {'reason': 'synthetic'}, source_id='fixture', event_id=event))
                consumed = pending(root, 'consumed')
                retained = pending(root, 'retained')
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
                next_record = pending(root, 'next')
                self.assertEqual(next_record.exists(), scenario == 'admit')
                self.assertTrue(retained.exists())
                if admitted:
                    self.assertEqual(records.read_outbox(next_record)['event_id'], next_record.stem)
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
            rows = [records.read_outbox(path) for path in (root/'outbox').glob('*.json')]
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
                body = {'agent_run_id': 'run', 'state': 'starting'}
                self.assertTrue(records.emit(root, 'run', body, event_id='same'))
                self.assertTrue(records.emit(root, 'run', body, event_id='same'))
                path = pending(root, 'same')
                original = records.read_outbox(path)
                path.unlink()
                self.assertTrue(records.emit(root, 'run', body, event_id='same'))
                self.assertEqual(records.read_outbox(path), original)
                with (root/'outbox/.lock').open('rb') as lock:
                    fcntl.flock(lock, fcntl.LOCK_EX)
                    self.assertFalse(records.emit(root, 'run', body))

    def test_private_durable_redacted_record_and_retry_identity(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            body = {'agent_run_id': 'run-1', 'state': 'starting', 'authorization': 'SECRET', 'argv': ['PRIVATE'], 'env': {'TOKEN': 'PRIVATE'},
                    'reason': 'PRIVATE body text', 'observed_at': 'PRIVATE body timestamp'}
            with patch.object(records, 'deliver', return_value=False):
                self.assertTrue(records.emit(root, 'run', body, 'source', 'PRIVATE timestamp', '.event-1'))
                self.assertTrue(records.emit(root, 'run', body, 'source', 'PRIVATE timestamp', '.event-1'))
            files = list((root/'outbox').glob('*.json'))
            self.assertEqual(len(files), 1)
            value = records.read_outbox(files[0])
            self.assertEqual(value['body'], {'agent_run_id': records.opaque_id(records.identity_key(root), 'run-1'), 'state': 'starting'})
            self.assertEqual(value['source_time'], '0001-01-01T00:00:00Z')
            self.assertEqual(files[0].stat().st_mode & 0o777, 0o600)
            self.assertEqual((root/'outbox').stat().st_mode & 0o777, 0o700)
            self.assertNotIn('SECRET', files[0].read_text())
            self.assertNotIn('PRIVATE', files[0].read_text())
            self.assertFalse(list((root/'outbox').glob('*.tmp')))
            files[0].unlink()
            with patch.object(records, 'deliver', return_value=False):
                self.assertTrue(records.emit(root, 'run', body, 'source', 'PRIVATE timestamp', '.event-1'))
            self.assertEqual(records.read_outbox(files[0]), value)

    def test_outbox_uses_closed_gap_reasons(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            self.assertTrue(records.emit(root, 'gap', {'reason': '/home/fixture/private-notes',
                                                      'status': 'fixture@example.invalid'}, event_id='private'))
            self.assertTrue(records.emit(root, 'gap', {'reason': 'source_offline'}, event_id='known'))
            self.assertEqual(records.read_outbox(pending(root, 'private'))['body'], {})
            self.assertEqual(records.read_outbox(pending(root, 'known'))['body'], {'reason': 'source_offline'})

    def test_invalid_sample_does_not_poison_outbox(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            self.assertFalse(records.emit(root, 'sample', {'object_id': 'bad\nid'}, event_id='.bad'))
            self.assertTrue(records.emit(root, 'sample', {'object_id': 'valid', 'monotonic_ns': -1}, event_id='.bad-counter'))
            self.assertNotIn('monotonic_ns', records.read_outbox(pending(root, '.bad-counter'))['body'])
            self.assertTrue(records.emit(root, 'sample', {'object_id': 'good', 'cpu_ns': 1}, event_id='good'))
            self.assertEqual(set((root/'outbox').glob('*.json')), {pending(root, '.bad-counter'), pending(root, 'good')})

    def test_legacy_fifo_retry_does_not_block_other_producers(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            outbox = records.private_directory(root/'outbox')
            fifo = outbox/'stable.json'
            os.mkfifo(fifo, 0o600)
            retry = "import records, sys; sys.exit(records.emit(sys.argv[1], 'gap', {}, event_id='stable'))"
            subprocess.run([sys.executable, '-B', '-c', retry, name], cwd=Path(records.__file__).parent,
                           check=True, timeout=5)
            self.assertTrue(records.emit(root, 'gap', {}, event_id='following'))
            self.assertTrue(pending(root, 'following').exists())

    def test_capacity_disk_failure_and_symlink_are_nonfatal(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            body = {'agent_run_id': 'run', 'state': 'starting'}
            with patch.object(records, 'atomic_json', side_effect=OSError('full')):
                self.assertFalse(records.emit(root, 'run', body))
            outside = root/'outside'
            outside.mkdir(mode=0o700)
            (root/'outbox/.lock').unlink()
            (root/'outbox').rmdir()
            (root/'outbox').symlink_to(outside)
            self.assertFalse(records.emit(root, 'run', body))
            self.assertFalse(list(outside.iterdir()))


if __name__ == '__main__':
    unittest.main()
