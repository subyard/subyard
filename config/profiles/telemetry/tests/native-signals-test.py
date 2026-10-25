#!/usr/bin/env python3
from contextlib import closing, redirect_stdout
import importlib.util
import io
import http.client
import http.server
import json
import os
from pathlib import Path
import queue
import shlex
import signal
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('native_signals_fixture', Path(__file__).parent/'e2e/native-signals.py')
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)


class NativeSignalsTest(unittest.TestCase):
    def test_missing_bubblewrap_tool_error_fails_before_the_completion_deadline(self):
        provider = fixture.load(Path(__file__).parents[2]/'orca/tests/e2e/codex-permissions.py', 'codex_missing_sandbox_fixture')
        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), fixture.codex_provider_class(provider.Provider))
        server.command, server.workspace, server.failure = None, Path('/tmp'), None
        threading.Thread(target=server.serve_forever, daemon=True).start()

        class RejectedTool:
            def turn(self, *args):
                connection = http.client.HTTPConnection('127.0.0.1', server.server_port, timeout=3)
                try:
                    connection.request('POST', '/', json.dumps({'input': [{'type': 'function_call_output',
                        'output': 'Process exited with code 101\nbubblewrap is unavailable: PRIVATE_CONTENT'}]}))
                    response = connection.getresponse()
                    response.read()
                finally:
                    connection.close()

        try:
            with tempfile.TemporaryDirectory(dir='/tmp') as directory:
                started = time.monotonic()
                with self.assertRaises(AssertionError) as error:
                    fixture.run_codex_command(RejectedTool(), server, None, Path(directory), 'true', Path(directory)/'absent', timeout=30)
                self.assertLess(time.monotonic()-started, 1)
                self.assertEqual(json.loads(str(error.exception))['error_class'], 'sandbox_runtime_unavailable')
                self.assertNotIn('PRIVATE_CONTENT', str(error.exception))
        finally:
            server.shutdown()
            server.server_close()

    def test_fixed_probe_error_preserves_known_routing_error_and_redacts_unknown(self):
        self.assertEqual(fixture.safe_probe_error('unsupported call: exec_command', 'private command', 'secret'),
                         'unsupported call: exec_command')
        command = 'python3 -c "print(\'PRIVATE_COMMAND\')"'
        output = command+' '+json.dumps(command)[1:-1]+' PRIVATE_TOKEN /tmp/private-route Bearer abcdef1234567890 '
        diagnostic = fixture.safe_probe_error(output, command, 'PRIVATE_TOKEN')
        for value in ('PRIVATE_COMMAND', 'PRIVATE_TOKEN', '/tmp/private-route', 'abcdef1234567890'):
            self.assertNotIn(value, diagnostic)
        self.assertLessEqual(len(fixture.safe_probe_error('x '*2000, '', '')), 1024)

    def test_readonly_context_command_and_tool_terminal_facts(self):
        class Backend:
            def poll(self):
                return None

        class Frames:
            process = Backend()

            def __init__(self):
                self.frames = queue.Queue()

            def turn(self, *args):
                pass

        with tempfile.TemporaryDirectory(dir='/tmp') as directory:
            root = Path(directory)
            (root/'runs').mkdir(mode=0o700)
            (root/'runs/local.json').write_text(json.dumps(dict(agent='codex', agent_run_id='local')))
            before = set(root.rglob('*'))
            command = fixture.native_context_command(root, root)
            completed = subprocess.run(shlex.split(command), capture_output=True, timeout=5,
                env=dict(PATH=os.defpath, HOME=str(root), SUBYARD_TELEMETRY_DATA_DIR=str(root), SUBYARD_AGENT_RUN_ID='local'))
            self.assertEqual(completed.returncode, 0)
            self.assertEqual(set(root.rglob('*')), before)
            self.assertTrue(all(value is True for key, value in json.loads(completed.stdout).items() if key != 'schema'))
            for status, code, receipt in (('completed', 0, None), ('failed', 7, root/'missing'), ('completed', 0, root/'missing')):
                with self.subTest(status=status, code=code, receipt=receipt is not None):
                    app = fixture.codex_app_class(Frames)()
                    app.frames.put(dict(method='item/completed', params=dict(item=dict(type='commandExecution',
                        status=status, exitCode=code, aggregatedOutput='PRIVATE_CONTENT\n'+completed.stdout.decode()))))
                    if receipt is None:
                        result = fixture.run_codex_command(app, None, None, root, command, timeout=.1)
                        self.assertTrue(all(result['context'].values()))
                        self.assertNotIn('PRIVATE_CONTENT', json.dumps(result))
                    else:
                        with self.assertRaises(AssertionError) as error:
                            fixture.run_codex_command(app, None, None, root, command, receipt, timeout=.1)
                        facts = json.loads(str(error.exception))
                        self.assertEqual(facts['exit_code'], code)
                        self.assertNotIn('PRIVATE_CONTENT', str(error.exception))

    def test_codex_command_outlives_early_turn_until_receipt_or_bounded_cleanup(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as directory:
            root = Path(directory)
            for status, delay, timeout in ((0, .2, 3), (7, .2, 3), (0, 60, .2)):
                with self.subTest(status=status, timeout=timeout):
                    receipt = root/(str(status)+'-'+str(timeout)+'.json')
                    pid = receipt.with_suffix('.pid')
                    command = fixture.command_with_receipt([sys.executable, '-c',
                        'import os,pathlib,sys,time; pathlib.Path(sys.argv[1]).write_text(str(os.getpid())); time.sleep(float(sys.argv[2])); sys.exit(int(sys.argv[3]))',
                        str(pid), str(delay), str(status)], receipt)

                    class EarlyTurn:
                        def turn(self, *args):
                            self.process = subprocess.Popen(shlex.split(args[-1]), start_new_session=True,
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

                        def close(self):
                            if self.process.poll() is None:
                                os.killpg(self.process.pid, signal.SIGTERM)
                            self.process.wait(timeout=10)

                    app = EarlyTurn()
                    started = time.monotonic()
                    try:
                        if status == 0 and delay < timeout:
                            fixture.run_codex_command(app, None, None, root, command, receipt, timeout=timeout)
                            self.assertTrue(receipt.is_file(), 'backend teardown must follow the actual command receipt')
                            self.assertEqual(json.loads(receipt.read_text())['exit_code'], 0)
                        else:
                            with self.assertRaises(AssertionError) as error:
                                fixture.run_codex_command(app, None, None, root, command, receipt, timeout=timeout)
                            facts = json.loads(str(error.exception))
                            if status:
                                self.assertEqual(facts['exit_code'], status)
                            else:
                                self.assertTrue(facts['timed_out'])
                    finally:
                        if hasattr(app, 'process'):
                            app.close()
                    self.assertLess(time.monotonic()-started, 5)
                    self.assertIsNotNone(app.process.returncode)
                    self.assertTrue(pid.is_file())
                    with self.assertRaises(ProcessLookupError):
                        os.kill(int(pid.read_text()), 0)

    def test_isolated_home_resolves_only_the_generated_fixture_pin(self):
        source_root = Path(__file__).resolve().parents[4]
        broker = (source_root/'dev/e2e/broker-memory-boundary.sh').read_text()
        prepare = 'prepare_native_route() {'+broker.split('prepare_native_route() {', 1)[1].split('\n}', 1)[0]+'\n}\n'
        p0 = (source_root/'dev/e2e/p0-guest.sh').read_text()
        normalize = 'normalized_ed25519() {'+p0.split('normalized_ed25519() {', 1)[1].split('\n}', 1)[0]+'\n}\n'
        with tempfile.TemporaryDirectory(dir='/tmp') as directory:
            root = Path(directory)
            (root/'state').mkdir(mode=0o700)
            (root/'home').mkdir(mode=0o700)
            subprocess.run(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', str(root/'fixture-key')],
                           check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=5)
            script = '. "$1"\n'+normalize+prepare+'''
set -euo pipefail
STATE_PARENT="$2/state"
fixture_host_key="$2/fixture-key.pub"
p0_capacity_prepare_subtree() { mkdir -m 0700 "$1"; }
outer_exec() { [ "$*" = 'cat /etc/ssh/ssh_host_ed25519_key.pub' ]; cat "$fixture_host_key"; }
if (resolve_bastion_route) >/dev/null 2>&1; then exit 1; else [ "$?" = 2 ]; fi
prepare_native_route
resolve_bastion_route
[ "$BASTION_HOSTNAME" = 127.0.0.1 ] && [ "$BASTION_PORT" = 2224 ]
[ "$BASTION_HOST_KEY_ALIAS" = telemetry-broker-fixture ]
[ "$BASTION_KNOWN_HOSTS" = "$STATE_PARENT/native-route/known_hosts" ]
ssh -G -F "$SUBYARD_E2E_ROUTE_CONFIG" yard-test-yard > "$2/rendered" 2>/dev/null
truncate -s 0 "$BASTION_KNOWN_HOSTS"
BASTION_KNOWN_HOSTS=''
if (resolve_bastion_route) >/dev/null 2>&1; then exit 1; else [ "$?" = 2 ]; fi
'''
            environment = dict(PATH=os.defpath, HOME=str(root/'home'), SUBYARD_E2E_ROUTE_REGISTRY=str(root/'absent-routes'))
            result = subprocess.run(['bash', '-c', script, 'fixture', str(source_root/'dev/agent-e2e.sh'), str(root)],
                                    env=environment, capture_output=True, timeout=10)
            self.assertEqual(result.returncode, 0)
            self.assertEqual(result.stdout+result.stderr, b'')
            rendered = (root/'rendered').read_text()
            self.assertIn('stricthostkeychecking true\n', rendered)
            self.assertIn('hostname 127.0.0.1\n', rendered)
            self.assertEqual((root/'state/native-route/ssh_config').stat().st_mode & 0o777, 0o600)
            self.assertEqual((root/'state/native-route/known_hosts').stat().st_mode & 0o777, 0o600)
            self.assertFalse((root/'home/.ssh').exists())

    def test_actual_runner_guard_and_route_errors_have_safe_source_callsites(self):
        runner = Path(__file__).resolve().parents[4]/'dev/agent-e2e.sh'
        with tempfile.TemporaryDirectory(dir='/tmp') as directory:
            root = Path(directory)
            cases = [
                (['bash', str(runner), '--fixture-invalid'], 'runner_guard'),
                (['bash', '-c', '. "$1"; build_bundle "$2" "$3"', 'fixture', str(runner), str(root), str(root/'unused.tgz')], 'runner_guard'),
                (['bash', '-c', '. "$1"; resolve_bastion_route', 'fixture', str(runner)], 'missing_pinned_host_key')]
            environment = dict(PATH=os.defpath, HOME=str(root), SUBYARD_E2E_ROUTE_REGISTRY=str(root/'absent-routes'),
                               SUBYARD_E2E_CANDIDATE_BUNDLE=str(root/'PRIVATE_CONTENT'), SUBYARD_E2E_CANDIDATE_SHA256='invalid')
            for index, (argv, reason) in enumerate(cases):
                with self.subTest(index=index):
                    receipt = root/(str(index)+'.json')
                    command = fixture.command_with_receipt(argv, receipt, dict(runner_source=str(runner)))
                    result = subprocess.run(shlex.split(command), env=environment, capture_output=True, timeout=10)
                    self.assertEqual(result.returncode, 2)
                    with self.assertRaises(AssertionError) as error:
                        fixture.require_command_success(receipt, 'codex')
                    facts = json.loads(str(error.exception))
                    self.assertEqual(facts['error_class'], reason)
                    self.assertTrue(facts['runner_error_callsites'])
                    self.assertNotIn('PRIVATE_CONTENT', str(error.exception))
                    self.assertEqual(result.stdout+result.stderr, b'')

    def test_resource_command_classifies_only_known_errors_and_keeps_output_private(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as directory:
            for known in (True, False):
                receipt = Path(directory)/('error-'+str(known)+'.json')
                message = ('agent-e2e: test environment unavailable: no pinned host key for ' if known else 'unknown failure: ')+'PRIVATE_CONTENT'
                # The actual stderr remains private; classification also reads a late error.
                program = 'import sys; print("x"*140000); print(sys.argv[1],file=sys.stderr); raise SystemExit(2)'
                command = fixture.command_with_receipt([sys.executable, '-c', program, message], receipt)
                result = subprocess.run(shlex.split(command), capture_output=True, timeout=10)
                self.assertEqual(result.returncode, 2)
                self.assertEqual(result.stdout+result.stderr, b'')
                output = receipt.with_suffix('.output')
                self.assertEqual(output.stat().st_mode & 0o777, 0o600)
                self.assertIn(b'PRIVATE_CONTENT', output.read_bytes())
                self.assertNotIn('PRIVATE_CONTENT', receipt.read_text())
                with self.assertRaises(AssertionError) as error:
                    fixture.require_command_success(receipt, 'codex')
                facts = json.loads(str(error.exception))
                self.assertEqual(facts['error_class'], 'missing_pinned_host_key' if known else 'unclassified')
                self.assertNotIn('PRIVATE_CONTENT', str(error.exception))

    def test_resource_command_observes_actual_tool_environment(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as directory:
            root = Path(directory)
            data, state, home = root/'data', root/'state', root/'home'
            (data/'runs').mkdir(parents=True)
            (data/'runs/native-run.json').write_text(json.dumps(dict(agent='codex', agent_run_id='native-run')))
            (state/'yards/test-yard').mkdir(parents=True)
            for path in (state/'id_ed25519', state/'id_ed25519.pub', state/'yards/test-yard/client-id'):
                path.write_text('PRIVATE_CONTENT')
            home.mkdir()
            expected = dict(agent='codex', data_dir=str(data), yard='test-yard', environment={
                'HOME': str(home), 'SUBYARD_E2E_STATE_DIR': str(state), 'SUBYARD_CONFIG_HOME': str(root/'config')})
            receipt = root/'context.json'
            command = fixture.command_with_receipt(['/bin/sh', '-c', 'exit 2'], receipt, expected)
            environment = dict(PATH=os.defpath, HOME=str(home), SUBYARD_E2E_STATE_DIR=str(state),
                               SUBYARD_AGENT_RUN_ID='native-run', SUBYARD_E2E_ROUTE_REGISTRY=str(root/'absent-routes'))
            result = subprocess.run(shlex.split(command), env=environment, capture_output=True, timeout=10)
            self.assertEqual(result.returncode, 2)
            context = json.loads(receipt.read_text())['context']
            self.assertTrue(context['managed_agent_matches'])
            self.assertTrue(context['home_matches'])
            self.assertTrue(context['controller_identity_exists'])
            self.assertTrue(context['controller_state_exists'])
            self.assertFalse(context['subyard_config_home_present'])
            self.assertFalse(context['subyard_config_home_matches'])
            self.assertFalse(context['route_registry_readable'])
            self.assertFalse(context['home_ssh_config_exists'])
            self.assertTrue(all(type(value) is bool for value in context.values()))
            with self.assertRaises(AssertionError) as error:
                fixture.require_command_success(receipt, 'codex')
            self.assertTrue(json.loads(str(error.exception))['context']['subyard_e2e_state_dir_matches'])

    def test_resource_command_requires_actual_success(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as directory:
            for status in (0, 7):
                with self.subTest(status=status):
                    receipt = Path(directory)/('exit-'+str(status)+'.json')
                    command = fixture.command_with_receipt(['/bin/sh', '-c', 'exit '+str(status)], receipt)
                    result = subprocess.run(shlex.split(command), capture_output=True, timeout=10)
                    self.assertEqual(result.returncode, status)
                    self.assertEqual(receipt.stat().st_mode & 0o777, 0o600)
                    if status:
                        with self.assertRaises(AssertionError) as error:
                            fixture.require_command_success(receipt, 'codex')
                        self.assertEqual(json.loads(str(error.exception))['exit_code'], status)
                    else:
                        fixture.require_command_success(receipt, 'codex')

    def test_resource_command_timeout_retains_typed_exit(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as directory:
            receipt = Path(directory)/'timeout.json'
            ready = Path(directory)/'ready'
            command = fixture.command_with_receipt(['/bin/sh', '-c', 'echo ready > "$1"; exec sleep 60', 'fixture', str(ready)], receipt)
            process = subprocess.Popen(shlex.split(command), stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            try:
                deadline = time.monotonic()+3
                while not ready.exists() and time.monotonic() < deadline:
                    time.sleep(.01)
                self.assertTrue(ready.is_file())
                with self.assertRaises(subprocess.TimeoutExpired):
                    process.wait(timeout=.02)
                process.send_signal(signal.SIGTERM)
                self.assertEqual(process.wait(timeout=10), 143)
                with self.assertRaises(AssertionError) as error:
                    fixture.require_command_success(receipt, 'codex')
                self.assertEqual(json.loads(str(error.exception))['exit_code'], 143)
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait(timeout=5)

    def test_resource_command_missing_or_invalid_receipt_is_redacted(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as directory:
            receipt = Path(directory)/'missing.json'
            for content in (None, 'PRIVATE_CONTENT', '["PRIVATE_CONTENT"]'):
                if content is not None:
                    receipt.write_text(content)
                with self.assertRaises(AssertionError) as error:
                    fixture.require_command_success(receipt, 'claude')
                self.assertFalse(json.loads(str(error.exception))['exit_code_known'])
                self.assertNotIn('PRIVATE_CONTENT', str(error.exception))

    def test_codex_provider_resource_timeout_is_opt_in(self):
        provider = fixture.load(Path(__file__).parents[2]/'orca/tests/e2e/codex-permissions.py', 'codex_timeout_fixture')
        for tool in ('shell', 'shell_command'):
            for timeout in (None, 1800000):
                with self.subTest(tool=tool, timeout=timeout):
                    server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), fixture.codex_provider_class(provider.Provider))
                    server.command, server.workspace, server.failure = 'true', Path('/tmp'), None
                    if timeout is not None:
                        server.command_timeout_ms = timeout
                    thread = threading.Thread(target=server.serve_forever, daemon=True)
                    thread.start()
                    connection = http.client.HTTPConnection('127.0.0.1', server.server_port, timeout=3)
                    try:
                        connection.request('POST', '/', json.dumps({'tools': [{'name': tool}]}))
                        response = connection.getresponse()
                        self.assertEqual(response.status, 200)
                        events = [json.loads(line.removeprefix('data: ')) for line in response.read().decode().splitlines() if line.startswith('data: ')]
                        call = next(event['item'] for event in events if event['type'] == 'response.output_item.done')
                        self.assertEqual(json.loads(call['arguments'])['timeout_ms'], 10000 if timeout is None else timeout)
                        self.assertEqual(server.native_provider_facts['selected_tool'], tool)
                        self.assertTrue(server.native_provider_facts['command_consumed'])
                        connection.request('POST', '/', json.dumps({'input': [{'type': 'function_call_output',
                            'output': 'Permission denied PRIVATE_CONTENT'}]}))
                        response = connection.getresponse()
                        response.read()
                        self.assertEqual(response.status, 200)
                        self.assertEqual(server.native_provider_facts['request_count'], 2)
                        self.assertTrue(server.native_provider_facts['tool_output_seen'])
                        self.assertEqual(server.native_provider_facts['error_class'], 'permission_denied')
                        self.assertNotIn('PRIVATE_CONTENT', json.dumps(server.native_provider_facts))
                        with tempfile.TemporaryDirectory(dir='/tmp') as directory, redirect_stdout(io.StringIO()) as console:
                            server.probe_error_capture = Path(directory)/'error.txt'
                            server.probe_command, server.probe_secret = 'private command', 'PRIVATE_CONTENT'
                            connection.request('POST', '/', json.dumps({'input': [{'type': 'function_call_output',
                                'output': 'unsupported call: exec_command'}]}))
                            response = connection.getresponse()
                            response.read()
                            self.assertEqual(server.probe_error_capture.read_text(), 'unsupported call: exec_command')
                            self.assertEqual(server.probe_error_capture.stat().st_mode & 0o777, 0o600)
                            self.assertEqual(json.loads(console.getvalue())['diagnostic'], 'unsupported call: exec_command')
                            server.probe_error_capture = None
                    finally:
                        connection.close()
                        server.shutdown()
                        server.server_close()
                        thread.join(timeout=3)

    def database(self, directory):
        data = Path(directory)
        with closing(sqlite3.connect(data/'telemetry.sqlite')) as db:
            db.executescript('''CREATE TABLE runs(agent_run_id TEXT,body TEXT);
                CREATE TABLE relations(agent_run_id TEXT,session_id TEXT);
                CREATE TABLE objects(object_id TEXT,body TEXT);
                INSERT INTO runs VALUES('run','{"agent":"codex"}');
                INSERT INTO relations VALUES('run','session');''')
            db.commit()
        return data

    def insert_scope(self, data, shared):
        with closing(sqlite3.connect(data/'telemetry.sqlite')) as db:
            db.execute('INSERT INTO objects VALUES(?,?)', ('run:scope', json.dumps({'shared': shared})))
            db.commit()

    def test_session_can_precede_scope_projection(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as directory:
            data = self.database(directory)
            self.assertEqual(fixture.wait_sessions(data, 'codex', {'session'}), {'run'})
            self.assertEqual(fixture.query(data, 'SELECT count(*) FROM objects')[0][0], 0)
            with patch.object(fixture.time, 'sleep', side_effect=lambda _: self.insert_scope(data, True)) as sleep:
                fixture.wait_shared_scope(data, 'run', timeout=1)
            sleep.assert_called_once_with(.2)

    def test_present_incorrect_shared_is_never_accepted(self):
        for shared in (False, None, 'PRIVATE_CONTENT'):
            with self.subTest(shared=type(shared).__name__), tempfile.TemporaryDirectory(dir='/tmp') as directory:
                data = self.database(directory)
                self.insert_scope(data, shared)
                with patch.object(fixture.time, 'sleep') as sleep:
                    with self.assertRaises(AssertionError) as error:
                        fixture.wait_shared_scope(data, 'run', timeout=1)
                sleep.assert_not_called()
                diagnostic = json.loads(str(error.exception))
                self.assertTrue(diagnostic['scope_present'])
                self.assertEqual(diagnostic['shared'], 'false' if shared is False else 'missing_or_invalid')
                self.assertNotIn('PRIVATE_CONTENT', str(error.exception))

    def test_missing_scope_is_bounded_and_reports_presence_only(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as directory:
            data = self.database(directory)
            with patch.object(fixture.time, 'monotonic', side_effect=[0, 0, 1]), patch.object(fixture.time, 'sleep') as sleep:
                with self.assertRaises(AssertionError) as error:
                    fixture.wait_shared_scope(data, 'run', timeout=.5)
            sleep.assert_called_once_with(.2)
            diagnostic = json.loads(str(error.exception))
            self.assertFalse(diagnostic['scope_present'])
            self.assertEqual(diagnostic['shared'], 'not_observed')
            self.assertEqual(diagnostic['reason'], 'shared_scope_projection_missing')


if __name__ == '__main__':
    unittest.main()
