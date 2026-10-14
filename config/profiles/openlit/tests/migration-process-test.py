#!/usr/bin/env python3
"""Host-free JSONL session protocol tests; these do not prove native telemetry."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

source = Path(__file__).parent/'e2e/migration-process.py'
spec = importlib.util.spec_from_file_location('migration_process', source)
driver = importlib.util.module_from_spec(spec)
spec.loader.exec_module(driver)

SERVER = '''
import json,sys,time
mode=sys.argv[1]
for line in sys.stdin:
    frame=json.loads(line)
    if frame.get('method')=='initialized':
        continue
    method=frame['method']
    if mode=='missing':
        time.sleep(5)
        continue
    if mode=='invalid':
        print('invalid JSON',flush=True)
        continue
    if mode=='reject':
        print(json.dumps({'id':frame['id'],'error':{'message':'synthetic'}}),flush=True)
        continue
    result={'thread':{'id':'fixture-thread'}} if method=='thread/start' else {}
    print(json.dumps({'method':'ignored-notification','params':{}}),flush=True)
    print(json.dumps({'id':frame['id'],'result':result}),flush=True)
    if method=='turn/start':
        status='failed' if mode=='failed' else 'completed'
        print(json.dumps({'method':'turn/completed','params':{'turn':{'status':status}}}),flush=True)
'''


class ProtocolTests(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.TemporaryDirectory(prefix='subyard-migration-protocol-', dir='/tmp')
        self.path = Path(self.root.name)/'app.py'
        self.path.write_text(SERVER)
        os.chmod(self.path, 0o600)

    def tearDown(self):
        self.root.cleanup()

    def app(self, mode='complete'):
        return driver.App([sys.executable, '-B', str(self.path), mode], {'PATH': '/usr/bin:/bin'}, Path(self.root.name))

    def test_complete_turn_and_normal_eof(self):
        app = self.app()
        try:
            self.assertEqual(app.thread, 'fixture-thread')
            app.turn()
            app.turn()
            app.close()
            self.assertEqual(app.process.returncode, 0)
        finally:
            app.abort()

    def test_failed_native_turn_is_not_success(self):
        app = self.app('failed')
        try:
            with self.assertRaisesRegex(RuntimeError, 'did not complete'):
                app.turn()
        finally:
            app.abort()

    def test_missing_reply_has_total_deadline(self):
        previous = driver.TIMEOUT
        driver.TIMEOUT = 0.1
        try:
            with self.assertRaisesRegex(RuntimeError, 'deadline expired'):
                self.app('missing')
        finally:
            driver.TIMEOUT = previous

    def test_invalid_json_and_rpc_error_fail_closed(self):
        for mode, message in [('invalid', 'invalid JSON'), ('reject', 'rejected initialize')]:
            with self.subTest(mode=mode), self.assertRaisesRegex(RuntimeError, message):
                self.app(mode)

    def test_protocol_double_cannot_claim_genuine_native_identity(self):
        app = self.app()
        try:
            with self.assertRaisesRegex(RuntimeError, 'not the genuine native'):
                app.identity()
        finally:
            app.abort()

    def test_guest_guard_fails_before_allocating_or_building(self):
        env = {'PATH': '/usr/bin:/bin', 'SUBYARD_E2E_VM': '0'}
        result = subprocess.run(['bash', str(source.parent/'migration.sh')], env=env,
                                capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 2)
        self.assertIn('requires a disposable allocated owner VM', result.stderr)
        self.assertNotIn('openlit_migration_stage=', result.stdout)


class RetentionTests(unittest.TestCase):
    def exercise(self, *, delayed_stop=False, never_finishes=False, failed=False, replaced=False):
        proof = driver.Proof.__new__(driver.Proof)
        proof.old_identity = {'pid': 123, 'start': 10}
        proof.old = mock.Mock()
        proof.old.identity.return_value = proof.old_identity
        proof.container = 'fixture-receiver'
        calls = 0
        clock = 0
        def monotonic():
            return clock
        def sleep(seconds):
            nonlocal clock
            clock += seconds
        def manager(arguments):
            nonlocal calls
            calls += 1
            start = 10 if calls == 1 else 20
            if replaced and calls >= 3:
                start = 30
            complete = calls == 1 or (calls >= 3 and not never_finishes)
            # Model a oneshot that begins Docker stop asynchronously; its old
            # container is still running when the start timestamp first changes.
            # Only after the third manager observation does stop actually finish.
            if '--value' in arguments:
                return str(start)
            state = {'Type': 'oneshot', 'ExecMainStartTimestampMonotonic': str(start),
                     'ExecMainExitTimestampMonotonic': str(start+5 if complete else 15),
                     'InvocationID': ('a' if calls == 1 else 'b')*32,
                     'ActiveState': 'inactive' if complete else 'activating',
                     'SubState': 'dead' if complete else 'start', 'Result': 'exit-code' if failed else 'success',
                     'ExecMainCode': '1', 'ExecMainStatus': '1' if failed else '0'}
            return '\n'.join(key+'='+value for key, value in state.items())
        proof.running = lambda _: not (delayed_stop and calls >= 3)
        with mock.patch.object(driver, 'run', side_effect=manager), \
             mock.patch.object(driver.time, 'monotonic', side_effect=monotonic), \
             mock.patch.object(driver.time, 'sleep', side_effect=sleep):
            result = proof.retain()
        self.assertGreaterEqual(calls, 3)
        proof.old.close.assert_not_called()
        return result

    def test_delayed_stop_cannot_pass_at_invocation_start(self):
        with self.assertRaisesRegex(RuntimeError, 'retired compatibility receiver'):
            self.exercise(delayed_stop=True)

    def test_success_requires_complete_oneshot_and_retained_backend(self):
        result = self.exercise()
        self.assertEqual(result, {'automatic_retry_completed': True, 'old_backend_retained': True})

    def test_still_running_stop_never_counts_as_retention(self):
        with self.assertRaisesRegex(RuntimeError, 'did not complete within deadline'):
            self.exercise(never_finishes=True)

    def test_failed_oneshot_is_not_retention(self):
        with self.assertRaisesRegex(RuntimeError, 'invocation failed'):
            self.exercise(failed=True)

    def test_another_invocation_cannot_complete_the_observed_retry(self):
        with self.assertRaisesRegex(RuntimeError, 'invocation changed'):
            self.exercise(replaced=True)


if __name__ == '__main__':
    unittest.main()
