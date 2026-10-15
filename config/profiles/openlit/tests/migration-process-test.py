#!/usr/bin/env python3
"""Host-free JSONL session protocol tests; these do not prove native telemetry."""
import importlib.util
import json
import os
from pathlib import Path
import sqlite3
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

    def test_native_launch_enables_default_metrics_without_config_override(self):
        proof = driver.Proof.__new__(driver.Proof)
        proof.workspace = Path(self.root.name)
        proof.server = mock.Mock(server_port=12345)
        with mock.patch.object(driver, 'run', return_value='codex-cli 0.162.1'), \
             mock.patch.object(driver, 'App') as launch:
            self.assertIs(proof.app(), launch.return_value)
        arguments = launch.call_args.args[0]
        self.assertEqual(arguments[-2:], ['app-server', '--analytics-default-enabled'])
        # The default flag lets Codex honor an explicit analytics.enabled=false;
        # this fixture must not force that user setting through a -c override.
        self.assertFalse(any(argument.startswith('analytics.') for argument in arguments))
        self.assertEqual(launch.call_args.kwargs['user'], 'dev')

    def test_guest_guard_fails_before_allocating_or_building(self):
        env = {'PATH': '/usr/bin:/bin', 'SUBYARD_E2E_VM': '0'}
        result = subprocess.run(['bash', str(source.parent/'migration.sh')], env=env,
                                capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 2)
        self.assertIn('requires a disposable allocated owner VM', result.stderr)
        self.assertNotIn('openlit_migration_stage=', result.stdout)


class SignalTests(unittest.TestCase):
    def exercise(self, *, service='codex_exec', logs=True, metrics='histogram',
                 delay=0, trace=('trace', 'a'*32, 101), query_timeout=False):
        with tempfile.TemporaryDirectory(prefix='subyard-migration-signals-', dir='/tmp') as temporary:
            state = Path(temporary)
            (state/'trace-cutover.json').write_text(json.dumps({'token': 'a'*32, 'cutover_ns': 100}))
            with sqlite3.connect(':memory:') as db:
                self.addCleanup(db.close)
                db.create_function('fromUnixTimestamp64Nano', 1, lambda ns: ns)
                db.execute('CREATE TABLE otel_traces (ServiceName TEXT, TraceId TEXT, cutover TEXT, Timestamp INTEGER)')
                for table in ('logs', 'metrics_sum', 'metrics_histogram'):
                    db.execute('CREATE TABLE otel_'+table+' (ServiceName TEXT)')
                proof = driver.Proof.__new__(driver.Proof)
                proof.old_identity = {'pid': 1, 'start': 1}
                app = mock.Mock()
                app.identity.return_value = {'pid': 2, 'start': 2}
                proof.app = lambda: app
                clock = 0
                inserted = False
                timeouts = []

                def ingest():
                    nonlocal inserted
                    if inserted:
                        return
                    inserted = True
                    if logs:
                        db.execute('INSERT INTO otel_logs VALUES (?)', (service,))
                    if metrics:
                        db.execute('INSERT INTO otel_metrics_'+metrics+' VALUES (?)', (service,))

                def turn():
                    if trace is not None:
                        db.execute('INSERT INTO otel_traces VALUES (?, ?, ?, ?)', (service, *trace))
                    if not delay:
                        ingest()

                def sleep(seconds):
                    nonlocal clock
                    clock += seconds
                    if clock >= delay:
                        ingest()

                def query(arguments, timeout=15):
                    nonlocal clock
                    if query_timeout and app.close.called:
                        timeouts.append(timeout)
                        clock += timeout
                        raise subprocess.TimeoutExpired('private-sentinel', timeout)
                    sql = arguments[-1].replace("ResourceAttributes['"+driver.ATTRIBUTE+"']", 'cutover')
                    return str(db.execute(sql).fetchone()[0])

                app.turn.side_effect = turn
                with mock.patch.object(driver, 'STATE', state), \
                     mock.patch.object(driver, 'run', side_effect=query), \
                     mock.patch.object(driver.time, 'monotonic', side_effect=lambda: clock), \
                     mock.patch.object(driver.time, 'sleep', side_effect=sleep):
                    try:
                        result = proof.fresh_turn()
                    except RuntimeError as error:
                        self.assertNotIn('private-sentinel', str(error))
                        return str(error), clock, timeouts
                app.close.assert_called_once_with()
                app.abort.assert_not_called()
                return result, clock, timeouts

    def test_histogram_and_sum_metrics_from_exact_codex_services(self):
        for service in ('codex_cli_rs', 'codex_exec', 'codex-app-server'):
            for table in ('sum', 'histogram'):
                with self.subTest(service=service, table=table):
                    result, _, _ = self.exercise(service=service, metrics=table)
                    self.assertEqual(result, {'marked_codex_traces': 1, 'codex_logs': 1, 'codex_metrics': 1})

    def test_delayed_ingestion_keeps_all_three_signal_gates(self):
        result, elapsed, _ = self.exercise(delay=4)
        self.assertEqual(result, {'marked_codex_traces': 1, 'codex_logs': 1, 'codex_metrics': 1})
        self.assertEqual(elapsed, 4)

    def test_missing_signal_or_prefixed_identity_never_passes(self):
        for missing in ({'logs': False}, {'metrics': None}, {'trace': None}, {'service': 'codex_exec-other'},
                        {'trace': ('', 'a'*32, 101)}, {'trace': ('trace', 'b'*32, 101)},
                        {'trace': ('trace', 'a'*32, 100)}):
            with self.subTest(missing=missing):
                error, elapsed, _ = self.exercise(**missing)
                self.assertIn('deadline expired', error)
                for name in ('marked_codex_traces', 'codex_logs', 'codex_metrics'):
                    self.assertRegex(error, name+r'=\d+')
                self.assertEqual(elapsed, 90)

    def test_query_timeouts_share_the_ingestion_deadline(self):
        error, elapsed, timeouts = self.exercise(query_timeout=True)
        self.assertIn('deadline expired', error)
        self.assertIn('codex_logs=-1', error)
        self.assertIn('codex_metrics=-1', error)
        self.assertEqual(elapsed, 90)
        self.assertTrue(all(0 < timeout <= 15 for timeout in timeouts))
        self.assertLess(timeouts[-1], 15)


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
