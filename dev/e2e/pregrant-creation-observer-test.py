#!/usr/bin/env python3
"""Actual fixture observer over Incus-shaped JSON and native requester processes."""
import importlib.util
import json
import os
from pathlib import Path
import select
import signal
import subprocess
import sys
import tempfile
import time
import unittest

ROOT = Path(__file__).resolve().parent
HELPER = ROOT / 'pregrant-creation-observer.py'
spec = importlib.util.spec_from_file_location('observer', HELPER)
observer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(observer)
PROJECT = 'subyard-e2e-vms-slot-1'


def event(project=PROJECT, name='e2e-vm-1', status=103, kind='task'):
    # Incus api.Event plus api.Operation, including the actual project/resources.
    return dict(type='operation', project=project, timestamp='2026-10-30T00:00:00Z', metadata=dict(
        id='00000000-0000-0000-0000-000000000001', **{'class': kind},
        description='Creating instance' if kind == 'task' else 'Executing command', status_code=status,
        resources=dict(instances=[f'/1.0/instances/{name}?project={project}']), metadata={}))


class ObserverTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='subyard-pregrant-observer-', dir='/tmp')
        self.addCleanup(self.temp.cleanup)
        self.target = subprocess.Popen(['sleep', '60'])
        self.addCleanup(self.stop, self.target)
        self.binding = Path(self.temp.name) / 'binding.json'
        self.proof = dict(project=PROJECT, state='provisioning', epoch=7, generation=4,
                          pid=self.target.pid, start=observer.process_start(self.target.pid),
                          worker_pid=os.getpid(), worker_start=observer.process_start(os.getpid()))
        self.binding.write_text(json.dumps(self.proof))
        self.records = []
        self.pending = b''

    @staticmethod
    def stop(process):
        if process.poll() is None:
            process.kill()
        process.wait(timeout=3)
        for stream in (process.stdin, process.stdout, process.stderr):
            if stream is not None:
                stream.close()

    def launch(self):
        # Incus monitor emits a blank separator after every JSON object.
        producer = "import json,sys; print(sys.argv[1]+'\\n',flush=True); row=json.loads(sys.stdin.readline()); sys.exit(row['exit']) if 'exit' in row else None; [print(json.dumps(item)+'\\n',flush=True) for item in row['events']]"
        self.process = subprocess.Popen([sys.executable, '-B', str(HELPER), '--project', PROJECT,
                                         '--binding', str(self.binding), '--timeout', '3', '--',
                                         sys.executable, '-u', '-c', producer, json.dumps(event(kind='websocket'))],
                                        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, bufsize=0)
        self.addCleanup(self.stop, self.process)
        self.stage('ready')

    def stage(self, stage):
        deadline = time.monotonic() + 4
        while time.monotonic() < deadline:
            for row in self.records:
                if row['stage'] == stage:
                    return row
            if not select.select([self.process.stdout], [], [], .05)[0]:
                continue
            chunk = os.read(self.process.stdout.fileno(), 4096)
            if not chunk:
                break
            self.pending += chunk
            while b'\n' in self.pending:
                line, self.pending = self.pending.split(b'\n', 1)
                if line:
                    self.records.append(json.loads(line))
        self.fail(f'observer did not emit {stage}: {self.records}')

    def send(self, **row):
        self.process.stdin.write((json.dumps(row)+'\n').encode())
        self.process.stdin.flush()
        terminal = self.stage('terminal')
        return self.process.wait(timeout=3), terminal

    def test_blank_separators_and_native_creation_kill_requester(self):
        self.launch(); self.stage('armed')
        code, terminal = self.send(events=[event()])
        self.assertEqual(code, 0)
        self.assertTrue(terminal['signalled'])
        self.assertGreaterEqual(terminal['blank_lines'], 1)
        self.assertEqual(self.target.wait(timeout=3), -signal.SIGKILL)
        self.assertEqual(self.stage('creating')['epoch'], 7)

    def test_delayed_event_after_held_does_not_read_live_slot_or_lose_fencing(self):
        self.launch(); self.stage('armed')
        # The immutable pre-event binding remains authoritative even if current
        # readiness advances before the already-generated live event is read.
        self.proof['state'] = 'held'
        self.binding.write_text(json.dumps(self.proof))
        code, terminal = self.send(events=[event(), event(status=200)])
        self.assertEqual(code, 0)
        self.assertTrue(terminal['signalled'])
        self.assertEqual(self.target.wait(timeout=3), -signal.SIGKILL)

    def test_wrong_project_and_resource_never_signal(self):
        for candidate, counter in ((event(project='foreign'), 'project_mismatch'),
                                   (event(name='foreign'), 'resource_mismatch')):
            with self.subTest(counter=counter):
                self.records=[]; self.pending=b''
                self.launch(); self.stage('armed')
                code, terminal = self.send(events=[candidate])
                self.assertEqual(code, 2)
                self.assertFalse(terminal['signalled'])
                self.assertGreaterEqual(terminal[counter], 1)
                self.assertIsNone(self.target.poll())

    def test_reused_pid_identity_never_signals(self):
        self.proof['start'] += '1'
        self.binding.write_text(json.dumps(self.proof))
        self.launch()
        terminal = self.stage('terminal')
        self.assertEqual(self.process.wait(timeout=3), 2)
        self.assertEqual(terminal['error_class'], 'invalid_binding')
        self.assertIsNone(self.target.poll())

    def test_native_observer_crash_reports_exit_and_counters(self):
        self.launch(); self.stage('armed')
        code, terminal = self.send(exit=7)
        self.assertEqual(code, 2)
        self.assertEqual(terminal['native_exit_code'], 7)
        self.assertEqual(terminal['error_class'], 'event_stream_ended')
        self.assertFalse(terminal['signalled'])
        self.assertIsNone(self.target.poll())


if __name__ == '__main__':
    unittest.main()
