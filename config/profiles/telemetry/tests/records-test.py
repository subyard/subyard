#!/usr/bin/env python3
import concurrent.futures
import fcntl
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import records
from spool_fixture import prepare, spooled


class RecordsTest(unittest.TestCase):
    def test_concurrent_streams_respect_quota_and_preserve_reserved_loss(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as directory:
            root = prepare(Path(directory))
            source = str(Path(records.__file__).parent)
            script = """
import sys
sys.path.insert(0, sys.argv[1])
import records
records.MAX_RECORDS = 2
# Stream creation fsyncs under the spool lock; keep the wait below the run timeout.
records.LOCK_WAIT = 2
for index in range(6):
    body = {'agent_run_id': sys.argv[3], 'state': 'starting'}
    assert records.emit(sys.argv[2], 'run', body, broker=sys.argv[3] == 'broker')
"""
            with concurrent.futures.ThreadPoolExecutor(2) as pool:
                runs = [pool.submit(subprocess.run, [sys.executable, '-B', '-c', script, source, directory, stream],
                                    check=True, timeout=5) for stream in ('run-one', 'broker')]
                for run in runs:
                    run.result()
            self.assertEqual(len(list((root/'spool').iterdir())), 2)
            self.assertEqual(len(list(spooled(root))), 12)
            with patch.object(records, 'MAX_RECORDS', 2):
                for _ in range(2):
                    self.assertFalse(records.emit(root, 'run', {'agent_run_id':'overflow'}))
            marker = (root/'spool-loss').read_bytes()
            slots = [json.loads(slot.split(b' ', 1)[1].rstrip(b'\0')) for slot in
                     (marker[:records.MARKER_SLOT], marker[records.MARKER_SLOT:])]
            loss = max(slots, key=lambda slot: slot['revision'])
            self.assertEqual(loss['count'], 2)
            self.assertEqual(loss['run'], 'overflow')
            self.assertTrue(loss['first'] <= loss['last'])

    def test_existing_segment_append_under_directory_contention(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as directory:
            root = prepare(Path(directory))
            with patch.object(records, 'LOCK_WAIT', 0):
                self.assertTrue(records.emit(root, 'gap', {}, broker=True))
            marker = (root/'spool-loss').read_bytes()
            with records.directory_lock(root/'spool', fcntl.LOCK_EX):
                self.assertTrue(records.emit(root, 'gap', {}, broker=True))
            self.assertEqual(len(list(spooled(root))), 2)
            self.assertEqual((root/'spool-loss').read_bytes(), marker)

    def test_new_segment_creation_under_directory_contention(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as directory:
            root = prepare(Path(directory))
            with records.directory_lock(root/'spool', fcntl.LOCK_EX):
                self.assertFalse(records.emit(root, 'gap', {}, broker=True))
            self.assertEqual(list((root/'spool').iterdir()), [])
            marker = (root/'spool-loss').read_bytes()
            slots = [json.loads(slot.split(b' ', 1)[1].rstrip(b'\0')) for slot in
                     (marker[:records.MARKER_SLOT], marker[records.MARKER_SLOT:]) if slot.strip(b'\0')]
            self.assertEqual(max(slots, key=lambda slot: slot['revision'])['count'], 1)
            # A longer bounded wait succeeds once the holder releases the lock.
            script = """
import sys, time
sys.path.insert(0, sys.argv[1])
import records
records.LOCK_WAIT = 30
sleep = time.sleep
def waiting(seconds):
    if not waiting.told:
        waiting.told = True
        print('waiting', flush=True)
    sleep(seconds)
waiting.told = False
time.sleep = waiting
sys.exit(0 if records.emit(sys.argv[2], 'gap', {}, broker=True) else 1)
"""
            with records.directory_lock(root/'spool', fcntl.LOCK_EX):
                child = subprocess.Popen([sys.executable, '-B', '-c', script, str(Path(records.__file__).parent), directory],
                                         stdout=subprocess.PIPE, text=True)
                try:
                    self.assertEqual(child.stdout.readline(), 'waiting\n')
                except BaseException:
                    child.kill()
                    raise
            self.assertEqual(child.wait(timeout=30), 0)
            child.stdout.close()
            self.assertEqual(len(list(spooled(root))), 1)

    def test_policy_and_path_validation_fail_closed(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as directory:
            root = Path(directory)
            self.assertFalse(records.emit(root, 'gap', {}))
            self.assertFalse((root/'spool').exists())
            prepare(root)
            value = records.read_private(root/'spool-policy.json')
            records.atomic_json(root/'spool-policy.json', dict(value, policy_version='unsupported'))
            self.assertFalse(records.emit(root, 'gap', {}))
            records.atomic_json(root/'spool-policy.json', value)
            self.assertTrue(records.emit(root, 'gap', {'reason':'private reason', 'text': 'private'}))
            self.assertEqual(next(spooled(root))['body'], {})
            spool = root/'spool'
            self.assertEqual(spool.stat().st_mode & 0o777, 0o700)
            for path in spool.iterdir():
                self.assertEqual(path.stat().st_mode & 0o777, 0o600)
                path.unlink()
            os.mkfifo(spool/'broker.open', 0o600)
            self.assertFalse(records.emit(root, 'gap', {}))
            self.assertTrue((spool/'broker.open').is_fifo())

if __name__ == '__main__':
    unittest.main()
