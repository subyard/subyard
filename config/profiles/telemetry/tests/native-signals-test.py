#!/usr/bin/env python3
import ast
import base64
from contextlib import closing, redirect_stdout
import hashlib
import importlib.util
import io
import http.client
import http.server
import json
import os
from pathlib import Path
import pty
import queue
import shlex
import signal
import sqlite3
import struct
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
from types import SimpleNamespace
import unittest
import uuid
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('native_signals_fixture', Path(__file__).parent/'e2e/native-signals.py')
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import launcher
import records
from spool_fixture import prepare


class NativeSignalsTest(unittest.TestCase):
    def test_holder_memory_refusal_keeps_bounded_numeric_diagnostics_and_original_failure(self):
        source_root = Path(__file__).resolve().parents[4]
        broker = (source_root/'config/profiles/test-vms/tests/e2e/broker-memory-boundary.sh').read_text()
        failure = 'holder_failure() {'+broker.split('holder_failure() {', 1)[1].split('\n}', 1)[0]+'\n}\n'
        script = '''set -euo pipefail
die() { printf '%s\\n' "$1" >&2; exit 2; }
timeout() {
  [ "$*" = '--kill-after=1s 5s /fixture/runtime/current/bin/yard -Y test-yard test-vms status --json' ] || return 91
  printf called > "$STATE_PARENT/probed"
  [ "$fixture_probe" != unavailable ] || return 124
  printf '%s\\n' "$fixture_status"
}
'''+failure+'holder_failure "original holder failure"\n'
        for resource, probe, headroom in (('memory', 'available', 12345), ('memory', 'unavailable', 12345),
                                          ('memory', 'available', 'private-canary'), ('disk', 'available', 12345)):
            with self.subTest(resource=resource, probe=probe, headroom=headroom), tempfile.TemporaryDirectory(dir='/tmp') as directory:
                root = Path(directory)
                (root/'holder.log').write_text('private-canary\nagent-e2e: retryable capacity refusal for slot-001: resource='
                                             +resource+'; retry after resources become available\n')
                status = {'resources': {'memory_admission': {'headroom_bytes': headroom}}, 'private': 'private-canary'}
                result = subprocess.run(['bash', '-c', script], text=True, capture_output=True, timeout=10,
                                        env={**os.environ, 'STATE_PARENT': directory, 'SUBYARD_HOME': '/fixture',
                                             'fixture_probe': probe, 'fixture_status': json.dumps(status)})
                self.assertEqual(result.returncode, 2)
                self.assertEqual(result.stdout, '')
                self.assertTrue(result.stderr.endswith('original holder failure\n'))
                self.assertNotIn('private-canary', result.stderr)
                self.assertEqual((root/'probed').exists(), resource == 'memory')
                if resource == 'memory':
                    self.assertRegex(result.stderr, r'MemTotal_kib=\d+ MemAvailable_kib=\d+\n')
                else:
                    self.assertNotIn('MemTotal_kib=', result.stderr)
                rows = [json.loads(line) for line in result.stderr.splitlines() if line.startswith('{')]
                expected = [{'check': 'holder-memory-refusal', 'admission_headroom_bytes': 12345}]
                self.assertEqual(rows, expected if resource == 'memory' and probe == 'available' and headroom == 12345 else [])

    def test_terminal_quit_waits_for_exit_flush_and_rejects_timeout_or_failure(self):
        terminal = fixture.load(Path(__file__).parents[2]/'orca/tests/e2e/codex-permissions.py', 'codex_quit_fixture').Terminal
        with tempfile.TemporaryDirectory(dir='/tmp') as directory:
            flushed = Path(directory)/'flushed'
            for mode in ('flush', 'timeout', 'failure', 'signal'):
                with self.subTest(mode=mode):
                    client = terminal.__new__(terminal)
                    client.pid, client.fd = pty.fork()
                    if client.pid == 0:
                        code = '''import os,pathlib,signal,sys,time,tty
print('Fixture complete.', flush=True)
assert input().strip() == '/quit'
tty.setraw(sys.stdin.fileno())
os.write(sys.stdout.fileno(), b'\x1b[6n\x1b[c')
responses = b''
while b'\x1b[1;1R' not in responses or b'\x1b[?1;2c' not in responses:
    responses += os.read(sys.stdin.fileno(), 64)
print('shutdown diagnostic: '+sys.argv[1], flush=True)
if sys.argv[1] == 'timeout':
    time.sleep(60)
if sys.argv[1] == 'failure':
    sys.exit(7)
if sys.argv[1] == 'signal':
    os.kill(os.getpid(), signal.SIGTERM)
time.sleep(.15)
# More than a PTY buffer: shutdown must keep draining terminal output.
print('x'*200000, flush=True)
pathlib.Path(sys.argv[2]).write_text('export acknowledged')
'''
                        os.execl(sys.executable, sys.executable, '-c', code, mode, str(flushed))
                    client.buffer = ''
                    try:
                        client.wait('Fixture complete.')
                        self.assertFalse(flushed.exists())
                        if mode == 'flush':
                            fixture.quit_codex_terminal(client, timeout=5)
                            self.assertEqual(flushed.read_text(), 'export acknowledged')
                            flushed.unlink()
                        else:
                            message = 'graceful exit timed out' if mode == 'timeout' else 'did not exit cleanly'
                            with self.assertRaisesRegex(AssertionError, message) as error:
                                fixture.quit_codex_terminal(client, timeout=.2 if mode == 'timeout' else 5)
                            facts = json.loads(str(error.exception).split(': ', 1)[1])
                            self.assertIn('shutdown diagnostic: '+mode, facts['terminal_tail'])
                            self.assertEqual(facts['exit_code'], 7 if mode == 'failure' else None)
                            self.assertEqual(facts['signal'], signal.SIGTERM if mode == 'signal' else None)
                    finally:
                        client.close()

    def test_failed_attribution_turn_still_reports_missing_receipts(self):
        class FailedTerminal:
            def wait(self, *args):
                raise AssertionError('fixture turn failed')

            def close(self):
                pass

        provider = SimpleNamespace(Provider=http.server.BaseHTTPRequestHandler, Terminal=FailedTerminal)
        native = SimpleNamespace(desired=lambda *args: {'otel': {}}, publish=lambda *args: None, snapshot=lambda *args: None)
        output = io.StringIO()
        with tempfile.TemporaryDirectory(dir='/tmp') as directory, \
                patch('pty.fork', return_value=(1, 2)), patch('fcntl.ioctl'), \
                patch.object(fixture.subprocess, 'run'), patch.object(fixture, 'query', return_value=[]), \
                redirect_stdout(output):
            with self.assertRaisesRegex(AssertionError, 'fixture turn failed'):
                fixture.assert_run_attribution(Path(directory), Path(directory)/'bin', provider, native,
                    SimpleNamespace(dumps=lambda value: ''), {}, 'http://127.0.0.1:1', '', Path(directory), False)
        line, = output.getvalue().splitlines()
        self.assertTrue(line.startswith('run-attribution plain: '))
        self.assertEqual(json.loads(line.split(': ', 1)[1]),
            dict(new=[], tool_run='', tool_exe='', traces_grew_for=[]))

    @staticmethod
    def trace_payload(run, *trace_ids):
        """Encode stored OTLP traces whose resource carries the collector's run attribute."""
        def varint(value):
            out = bytearray()
            while True:
                out.append((value & 0x7f) | (0x80 if value > 0x7f else 0))
                value >>= 7
                if not value:
                    return bytes(out)
        def field(number, payload):
            return varint(number << 3 | 2)+varint(len(payload))+payload
        resource = field(1, field(1, b'subyard.agent_run.id')+field(2, field(1, run.encode())))
        spans = b''.join(field(2, field(1, bytes.fromhex(trace))+field(2, bytes(8))) for trace in trace_ids)
        return field(1, field(1, resource)+field(2, spans))

    def run_attribution(self, turns):
        """Drive the attribution check with synthetic turns: name -> (registered runs, tool run, stored payloads)."""
        state = dict(runs=set(), payloads=[])
        order = iter(('plain', 'first', 'second'))

        class Terminal:
            def wait(self, text, provider):
                name = next(order)
                registered, run, payloads = turns[name]
                state['runs'] |= set(registered)
                state['payloads'] += payloads
                (provider.workspace/(name+'.run')).write_text(run)
                (provider.workspace/(name+'.exe')).write_text('/usr/local/bin/codex')
                daemon = provider.workspace.parent/'codex/app-server-daemon'
                daemon.mkdir(parents=True, exist_ok=True)
                (daemon/'daemon.pid').write_text('1')

            def close(self):
                pass

        provider = SimpleNamespace(Provider=http.server.BaseHTTPRequestHandler, Terminal=Terminal)
        native = SimpleNamespace(desired=lambda *args: {'otel': {}}, publish=lambda *args: None, snapshot=lambda *args: None)
        output = io.StringIO()
        with tempfile.TemporaryDirectory(dir='/tmp') as directory, \
                patch('pty.fork', return_value=(1, 2)), patch('fcntl.ioctl'), \
                patch.object(fixture.subprocess, 'run'), patch.object(fixture, 'quit_codex_terminal'), \
                patch.object(fixture, 'query', side_effect=lambda *args: [(run,) for run in sorted(state['runs'])]), \
                patch.object(fixture, 'raw_otlp', side_effect=lambda *args: list(state['payloads'])), \
                redirect_stdout(output):
            fixture.assert_run_attribution(Path(directory), Path(directory)/'bin', provider, native,
                SimpleNamespace(dumps=lambda value: ''), {}, 'http://127.0.0.1:1', '', Path(directory), False)
        return output.getvalue()

    def test_attribution_accepts_late_earlier_run_spans_only_on_their_own_traces(self):
        first, second, late = 'a'*32, 'c'*32, 'b'*32
        def turns(*second_payloads, plain=()):
            return dict(plain=(plain, '', []),
                        first=(['run-1'], 'run-1', [self.trace_payload('run-1', first)]),
                        second=(['run-2'], 'run-2', [*second_payloads, self.trace_payload('run-2', second)]))
        # Leftover run-1 processes flush after the second turn's baseline.
        output = self.run_attribution(turns(self.trace_payload('run-1', late, first)))
        self.assertIn('"traces_grew_for": ["run-1", "run-2"]', output.splitlines()[-1])
        refusals = [
            ('late spans joined this run', turns(self.trace_payload('run-1', second)),
             '^second: late spans of earlier run run-1 shared a trace with this run$'),
            ('unregistered run', turns(self.trace_payload('run-unknown', late)),
             '^second: spans carried a run that is not an earlier managed run of this test: \\["run-unknown"\\]$'),
            ('plain run', turns(self.trace_payload('run-plain', late), plain=['run-plain']),
             '^second: spans carried a run that is not an earlier managed run of this test: \\["run-plain"\\]$'),
        ]
        for case, scenario, message in refusals:
            with self.subTest(case):
                with self.assertRaisesRegex(AssertionError, message):
                    self.run_attribution(scenario)
        # A resource without spans does not count as a run's traces.
        output = self.run_attribution(turns(self.trace_payload('run-1'), self.trace_payload('run-unknown')))
        self.assertIn('"traces_grew_for": ["run-2"]', output.splitlines()[-1])
        for case, payloads in (('no run-2 resource', [self.trace_payload('run-1', late)]),
                               ('run-2 resource without spans', [self.trace_payload('run-2'), self.trace_payload('run-1', late)])):
            with self.subTest(case):
                missing = turns()
                missing['second'] = (['run-2'], 'run-2', payloads)
                with self.assertRaisesRegex(AssertionError, '^second: spans did not carry this run$'):
                    self.run_attribution(missing)
        early = turns()
        early['first'] = (['run-1'], 'run-1', [self.trace_payload('run-1', first), self.trace_payload('run-2', late)])
        with self.assertRaisesRegex(AssertionError, '^first: spans carried a run that is not an earlier managed run of this test: \\["run-2"\\]$'):
            self.run_attribution(early)

    def test_native_resource_timeout_budget_contract(self):
        profile = fixture.load(Path(__file__).parent/'e2e/profile-lifecycle.py', 'profile_budget_fixture')
        # The enclosing deadline must cover two sequential native commands,
        # their session/allocation projections, and orderly backend shutdown.
        budget = fixture.RESOURCE_EXERCISE_TIMEOUT
        self.assertGreaterEqual(budget, fixture.RESOURCE_CODEX_TIMEOUT + fixture.RESOURCE_CLAUDE_TIMEOUT + 190)
        self.assertEqual(profile.native_signals.RESOURCE_EXERCISE_TIMEOUT, budget)
        self.assertGreater(fixture.RESOURCE_CODEX_TIMEOUT, fixture.RESOURCE_TOOL_TIMEOUT_MS // 1000)
        self.assertGreater(fixture.RESOURCE_CLAUDE_TIMEOUT, fixture.RESOURCE_TOOL_TIMEOUT_MS // 1000)
        environment = next(node for node in ast.walk(ast.parse(Path(fixture.__file__).read_text()))
            if isinstance(node, ast.keyword) and node.arg == 'BASH_MAX_TIMEOUT_MS')
        maximum = eval(compile(ast.Expression(environment.value), '<fixture-budget>', 'eval'),
                       {'RESOURCE_TOOL_TIMEOUT_MS': fixture.RESOURCE_TOOL_TIMEOUT_MS})
        self.assertGreaterEqual(int(maximum), fixture.RESOURCE_TOOL_TIMEOUT_MS)

        def timeout_value(tree, matches, environment):
            call = next(node for node in ast.walk(tree) if isinstance(node, ast.Call) and matches(node))
            value = next(item.value for item in call.keywords if item.arg == 'timeout')
            return eval(compile(ast.Expression(value), '<fixture-budget>', 'eval'), environment)

        exercise = timeout_value(ast.parse(profile.PREPARE),
            lambda call: isinstance(call.func, ast.Attribute) and call.func.attr == 'run'
            and isinstance(call.func.value, ast.Name) and call.func.value.id == 'failure_evidence',
            {'NATIVE_EXERCISE_TIMEOUT': budget})
        self.assertEqual(exercise, budget)
        deadline = next(node.value.right.value for node in ast.walk(ast.parse(profile.START))
            if isinstance(node, ast.Assign) and any(isinstance(target, ast.Name) and target.id == 'deadline' for target in node.targets))
        guest = timeout_value(ast.parse(Path(profile.__file__).read_text()),
            lambda call: isinstance(call.func, ast.Name) and call.func.id == 'guest' and call.args
            and isinstance(call.args[0], ast.Name) and call.args[0].id == 'START', {'native_signals': profile.native_signals})
        self.assertGreater(deadline, exercise)
        self.assertGreater(guest, deadline)

    def test_native_agent_delivery_uses_verified_fixture_archives(self):
        def package(files):
            archive = io.BytesIO()
            with tarfile.open(fileobj=archive, mode='w:gz') as bundle:
                for name, body in files.items():
                    member = tarfile.TarInfo(name)
                    member.size, member.mode = len(body), 0o755
                    bundle.addfile(member, io.BytesIO(body))
            return archive.getvalue()

        codex_version = f'codex-cli {fixture.CODEX_FIXTURE_VERSION}'
        codex = f'#!/bin/sh\nprintf "{codex_version}\\n"\n'.encode()
        claude = f'#!/bin/sh\nprintf "{fixture.CLAUDE_FIXTURE_VERSION} (Claude Code)\\n"\n'.encode()
        codex_archive = package({'bin/codex': codex, 'bin/fixture-sidecar': b'fixture'})
        claude_archive = package({'package/claude': claude})
        download_urls = [
            f'https://github.com/openai/codex/releases/download/rust-v{fixture.CODEX_FIXTURE_VERSION}/'
            'codex-package-x86_64-unknown-linux-musl.tar.gz',
            f'https://registry.npmjs.org/@anthropic-ai/claude-code-linux-x64/-/claude-code-linux-x64-{fixture.CLAUDE_FIXTURE_VERSION}.tgz',
        ]
        run = subprocess.run
        for corrupted in (False, True):
            with self.subTest(corrupted=corrupted), tempfile.TemporaryDirectory(dir='/tmp') as directory:
                root = Path(directory)
                downloads = []

                def run_fixture_command(argv, **kwargs):
                    if argv[0] == 'curl':
                        downloads.append(argv[-1])
                        body = {download_urls[0]: codex_archive, download_urls[1]: claude_archive}[argv[-1]]
                        if corrupted and argv[-1] == download_urls[0]:
                            body += b'corrupt'
                        Path(argv[argv.index('--output')+1]).write_bytes(body)
                        return subprocess.CompletedProcess(argv, 0)
                    self.assertIn(argv, ([str(root/'bin/codex'), '--version'], [str(root/'bin/claude'), '--version']))
                    return run(argv, **kwargs)

                with patch.object(fixture.os, 'uname', return_value=SimpleNamespace(machine='x86_64')), \
                        patch.object(fixture.subprocess, 'run', side_effect=run_fixture_command), \
                        patch.dict(fixture.CODEX_FIXTURE_DIGESTS, x86_64=hashlib.sha256(codex_archive).hexdigest()), \
                        patch.dict(fixture.CLAUDE_FIXTURE_PACKAGES, x86_64=('linux-x64', hashlib.sha256(claude).hexdigest())):
                    if corrupted:
                        with self.assertRaisesRegex(ValueError, 'Codex fixture archive checksum mismatch'):
                            fixture.install_agents(root)
                    else:
                        self.assertEqual(fixture.install_agents(root), root/'bin')
                if corrupted:
                    self.assertEqual(downloads, download_urls[:1])
                    self.assertFalse((root/'codex-native').exists())
                    self.assertFalse((root/'bin/codex').exists())
                else:
                    self.assertEqual(downloads, download_urls)
                    self.assertEqual((root/'bin/codex').resolve(), root/'codex-native/bin/codex')
                    self.assertEqual((root/'codex-native/bin/fixture-sidecar').read_bytes(), b'fixture')
                    versions = json.loads((root/'native-versions.json').read_text())
                    self.assertEqual(versions['codex'], dict(version=codex_version, sha256=hashlib.sha256(codex).hexdigest()))

    def test_fixture_thread_requests_and_checks_effective_permissions(self):
        class App:
            def request(self, method, params):
                request.append((method, params))
                return {'thread': {'id': 'fixture'}, 'sandbox': {'type': sandbox}}

        request = []
        app = fixture.codex_app_class(App)()
        sandbox = 'dangerFullAccess'
        self.assertEqual(app.start_fixture_thread(Path('/tmp/fixture')), 'fixture')
        self.assertEqual(request, [('thread/start', {'cwd': '/tmp/fixture', 'sandbox': 'danger-full-access'})])
        sandbox = 'readOnly'
        with self.assertRaisesRegex(AssertionError, 'fixture tool permissions were not applied'):
            app.start_fixture_thread(Path('/tmp/fixture'))

    def register_run(self, data, run_id):
        prepare(data)
        run = dict(agent_run_id=run_id, agent='codex', host_id='private-fixture', boot_id=launcher.boot_id(),
                   pid=os.getpid(), start_ticks=launcher.processes.start_ticks(os.getpid()))
        self.assertTrue(launcher.admit_manifest(data, run, reserve=True))
        launcher.finish_manifest(data, run)
        self.assertNotIn('agent', records.read_private(data/'runs-v5'/(run_id+'.json')))
        database = data/'telemetry.sqlite'
        database.unlink()
        with closing(sqlite3.connect(database)) as db:
            database.chmod(0o600)
            db.execute('CREATE TABLE runs(agent_run_id TEXT PRIMARY KEY, body TEXT)')
            db.execute('INSERT INTO runs VALUES(?,?)', (run_id, json.dumps(dict(agent='codex'))))
            db.commit()

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
            run = str(uuid.uuid4())
            self.register_run(root, run)
            before = set(root.rglob('*'))
            command = fixture.native_context_command(root, root)
            completed = subprocess.run(shlex.split(command), capture_output=True, timeout=5,
                env=dict(PATH=os.defpath, HOME=str(root), SUBYARD_TELEMETRY_DATA_DIR=str(root), SUBYARD_AGENT_RUN_ID=run))
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
                            # Establish the running-child premise independently
                            # of the short completion timeout under test.
                            ready_deadline = time.monotonic()+2
                            while not pid.is_file() and self.process.poll() is None and time.monotonic() < ready_deadline:
                                time.sleep(.01)
                            if not pid.is_file():
                                raise RuntimeError('fixture command did not become ready')

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
                            self.assertGreaterEqual(json.loads(receipt.read_text())['elapsed_seconds'], delay)
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
        broker = (source_root/'config/profiles/test-vms/tests/e2e/broker-memory-boundary.sh').read_text()
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
            run = str(uuid.uuid4())
            self.register_run(data, run)
            (state/'yards/test-yard').mkdir(parents=True)
            for path in (state/'id_ed25519', state/'id_ed25519.pub', state/'yards/test-yard/client-id'):
                path.write_text('PRIVATE_CONTENT')
            home.mkdir()
            expected = dict(agent='codex', data_dir=str(data), yard='test-yard', environment={
                'HOME': str(home), 'SUBYARD_E2E_STATE_DIR': str(state), 'SUBYARD_CONFIG_HOME': str(root/'config')})
            receipt = root/'context.json'
            command = fixture.command_with_receipt(['/bin/sh', '-c', 'exit 2'], receipt, expected)
            environment = dict(PATH=os.defpath, HOME=str(home), SUBYARD_E2E_STATE_DIR=str(state),
                               SUBYARD_AGENT_RUN_ID=run, SUBYARD_E2E_ROUTE_REGISTRY=str(root/'absent-routes'))
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

    def test_android_resource_failures_keep_native_class_and_lease_stage_private(self):
        cases = [
            ('boot_timeout', 'android_boot_timeout', False),
            ('capacity', 'android_capacity_refusal', False),
            ('busy', 'android_pool_busy', False),
            ('timeout', 'android_pool_wait_timeout', False),
            ('stale', 'android_lease_stale', None),
            ('unknown', 'unclassified', None),
        ]
        with tempfile.TemporaryDirectory(dir='/tmp') as directory:
            for kind in ('android', 'vm'):
                for code, reason, granted in cases:
                    with self.subTest(kind=kind, code=code):
                        receipt = Path(directory)/(kind+'-'+code+'.json')
                        # The native error has no VM runner banner or guard
                        # callsite. Its message must remain private.
                        program = ('import sys; print("x"*140000); '
                                   'print(sys.argv[1],file=sys.stderr); raise SystemExit(1)')
                        command = fixture.command_with_receipt([sys.executable, '-c', program,
                            'Android '+code+': PRIVATE_CONTENT'], receipt, dict(kind=kind))
                        result = subprocess.run(shlex.split(command), capture_output=True, timeout=10)
                        self.assertEqual(result.returncode, 1)
                        self.assertEqual(result.stdout+result.stderr, b'')
                        with self.assertRaises(AssertionError) as error:
                            fixture.require_command_success(receipt, 'codex')
                        facts = json.loads(str(error.exception))
                        self.assertEqual(facts['resource_kind'], kind)
                        self.assertEqual(facts['error_class'], reason if kind == 'android' else 'unclassified')
                        self.assertEqual(facts['runner_error_callsites'], [])
                        context = facts['context']
                        if kind == 'vm':
                            self.assertFalse(context['allocation_started'])
                            self.assertFalse(context['lease_granted'])
                        else:
                            self.assertEqual(context.get('lease_granted'), granted)
                            if code == 'boot_timeout':
                                self.assertTrue(context['allocation_started'])
                            else:
                                self.assertNotIn('allocation_started', context)
                        self.assertNotIn('PRIVATE_CONTENT', str(error.exception))

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

    def test_claude_provider_resource_timeout_is_opt_in(self):
        for timeout in (None, fixture.RESOURCE_TOOL_TIMEOUT_MS):
            with self.subTest(timeout=timeout):
                server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), fixture.ClaudeProvider)
                server.requests, server.command = 0, 'true'
                if timeout is not None:
                    server.command_timeout_ms = timeout
                thread = threading.Thread(target=server.serve_forever, daemon=True)
                thread.start()
                connection = http.client.HTTPConnection('127.0.0.1', server.server_port, timeout=3)
                try:
                    connection.request('POST', '/v1/messages', json.dumps({'model': 'fixture'}))
                    response = connection.getresponse()
                    self.assertEqual(response.status, 200)
                    events = [json.loads(line.removeprefix('data: ')) for line in response.read().decode().splitlines()
                              if line.startswith('data: ')]
                    arguments = next(json.loads(event['delta']['partial_json']) for event in events
                                     if event.get('delta', {}).get('type') == 'input_json_delta')
                    self.assertEqual(arguments['timeout'], 600000 if timeout is None else timeout)
                    self.assertIsNone(server.command)
                finally:
                    connection.close()
                    server.shutdown()
                    server.server_close()
                    thread.join(timeout=3)

    def database(self, directory):
        data = Path(directory)
        with closing(sqlite3.connect(data/'telemetry.sqlite')) as db:
            db.executescript('''CREATE TABLE runs(agent_run_id TEXT,body TEXT,origin_id TEXT DEFAULT 'fixture');
                CREATE TABLE relations(agent_run_id TEXT,session_id TEXT,origin_id TEXT DEFAULT 'fixture');
                CREATE TABLE objects(object_id TEXT,body TEXT,kind TEXT,accounting TEXT,origin_id TEXT DEFAULT 'fixture');
                CREATE TABLE associations(object_id TEXT,agent_run_id TEXT,origin_id TEXT DEFAULT 'fixture');
                INSERT INTO runs(agent_run_id,body) VALUES('run','{"agent":"codex"}');
                INSERT INTO relations(agent_run_id,session_id) VALUES('run','session');''')
            db.commit()
        return data

    def insert_scope(self, data, shared):
        with closing(sqlite3.connect(data/'telemetry.sqlite')) as db:
            db.execute("INSERT INTO objects(object_id,body,kind,accounting) VALUES(?,?,'scope','inclusive')", ('scope-digest', json.dumps({'shared': shared})))
            db.execute("INSERT INTO associations(object_id,agent_run_id) VALUES('scope-digest','run')")
            db.commit()

    def test_native_log_fields_are_read_from_stored_protobuf(self):
        def varint(value):
            out = bytearray()
            while True:
                out.append((value & 0x7f) | (0x80 if value > 0x7f else 0))
                value >>= 7
                if not value:
                    return bytes(out)
        def field(number, payload):
            return varint(number << 3 | 2)+varint(len(payload))+payload
        def attribute(key, value):
            if isinstance(value, bool):
                typed = varint(2 << 3)+varint(int(value))
            elif isinstance(value, int):
                typed = varint(3 << 3)+varint(value)
            elif isinstance(value, float):
                typed = varint(4 << 3 | 1)+struct.pack('<d', value)
            else:
                typed = field(1, value.encode())
            return field(1, key.encode())+field(2, typed)
        def logs(*records_):
            # Resource attributes and a fixed64 timestamp precede the attributes.
            resource = field(1, field(1, attribute('service.name', 'subyard-fixture')))
            body = b''.join(field(2, b'\x09'+bytes(8)+b''.join(field(6, attribute(*pair)) for pair in pairs)) for pairs in records_)
            return field(1, resource+field(2, body))
        codex = logs([('event.name', 'codex.sse_event'), ('model', 'subyard-fixture'), ('input_token_count', 1)],
                     [('event.name', 'codex.tool_result'), ('success', True), ('duration_ms', 12.0)])
        claude = [('event.name', 'api_request'), ('model', 'claude-fixture'), ('input_tokens', 0), ('cost_usd', 0.5)]
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            data = Path(name)
            def store(*signals):
                # Replay through the collector, which stores raw OTLP compressed.
                lines = [json.dumps(dict(schema='subyard.telemetry.export/v1', origin_id='fixture-origin', event_id=str(uuid.uuid4()),
                    source_id='native-otlp', source_version='collector-pdata/1.69.0', kind='otlp/logs', encoding='otlp/logs',
                    policy_version='subyard.telemetry.capture/v1', protobuf=base64.b64encode(signal).decode())) for signal in signals]
                subprocess.run([fixture.collector(), 'replay', '--data-dir', name], input='\n'.join(lines).encode(), check=True, timeout=30)
            store(codex)
            with self.assertRaisesRegex(AssertionError, 'claude event.name and model, claude token counter: lost'):
                fixture.assert_native_log_fields(data, success=('codex',), timeout=0)
            store(logs(claude, [('event.name', 'tool_result'), ('duration_ms', 1.0)]))
            fixture.assert_native_log_fields(data, success=('codex',), timeout=0)
            with self.assertRaisesRegex(AssertionError, '^claude boolean success: lost'):
                fixture.assert_native_log_fields(data, timeout=0)
            store(logs([('event.name', 'tool_result'), ('success', False)]))
            fixture.assert_native_log_fields(data, timeout=0)
            stored = fixture.stored_log_attributes(data)
            self.assertIn({'event.name': 'codex.tool_result', 'success': True, 'duration_ms': 12.0}, stored)
            self.assertIn({'event.name': 'api_request', 'model': 'claude-fixture', 'input_tokens': 0, 'cost_usd': 0.5}, stored)

    def test_session_can_precede_scope_projection(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as directory:
            data = self.database(directory)
            self.assertEqual(fixture.wait_sessions(data, 'codex', {'session'}), {'run'})
            self.assertEqual(fixture.query(data, 'SELECT count(*) FROM objects')[0][0], 0)
            with patch.object(fixture.time, 'sleep', side_effect=lambda _: self.insert_scope(data, True)) as sleep:
                fixture.wait_shared_scope(data, 'run', timeout=1)
            sleep.assert_called_once_with(.2)

    def test_session_timeout_reports_only_binding_metadata_and_preserves_failure(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as directory:
            data = self.database(directory)
            with closing(sqlite3.connect(data/'telemetry.sqlite')) as db:
                db.executescript('''CREATE TABLE records(kind TEXT,body TEXT);
                    INSERT INTO records VALUES('otlp/logs','PRIVATE_PROMPT');
                    INSERT INTO records VALUES('otlp/metrics','PRIVATE_HEADER');
                    INSERT INTO runs(agent_run_id,body) VALUES
                        ('claude-run','{"agent":"claude"}'),('foreign-run','{"agent":"PRIVATE_AGENT"}'),('missing-agent','{}');
                    INSERT INTO relations(agent_run_id,session_id) VALUES
                        ('claude-run','PRIVATE_SESSION'),('foreign-run','PRIVATE_FOREIGN_SESSION'),('orphan','PRIVATE_ORPHAN_SESSION');''')
                db.commit()
            with redirect_stdout(io.StringIO()) as output, patch.object(fixture.time, 'monotonic', side_effect=[0, 0, 41]), \
                 patch.object(fixture.time, 'sleep') as sleep:
                with self.assertRaisesRegex(AssertionError, '^claude: native exported session IDs did not bind to managed runs$'):
                    fixture.wait_sessions(data, 'claude', {'PRIVATE_SESSION', 'PRIVATE_MISSING_SESSION'},
                        stderr=b'telemetry authentication required; run registration pending; [3P telemetry] PRIVATE_TOKEN https://private-provider/')
            sleep.assert_called_once_with(.2)
            metadata = json.loads(output.getvalue())
            self.assertNotIn('PRIVATE_', output.getvalue())
            self.assertNotIn('private-provider', output.getvalue())
            self.assertLess(len(output.getvalue()), 2048)
            self.assertEqual(metadata['expected_count'], 2)
            self.assertEqual(metadata['matched_count'], 1)
            self.assertEqual(len(metadata['expected_digests']), 2)
            self.assertEqual(len(metadata['matched_digests']), 1)
            self.assertTrue(set(metadata['matched_digests']) <= set(metadata['expected_digests']))
            self.assertEqual(metadata['managed_runs'], dict(codex=1, claude=1, unknown=1, missing=1))
            self.assertEqual(metadata['relations'], dict(codex=1, claude=1, unknown=1, missing=1))
            self.assertEqual(metadata['otlp_signals'], {'otlp/logs': 1, 'otlp/metrics': 1})
            self.assertEqual(metadata['stderr_error_classes'], dict(authentication_required=1, registration_pending=1, exporter_error=1))
            with closing(sqlite3.connect(data/'telemetry.sqlite')) as db:
                db.execute('DROP TABLE records')
                db.commit()
            with redirect_stdout(io.StringIO()) as output, patch.object(fixture.time, 'monotonic', side_effect=[0, 41]):
                with self.assertRaisesRegex(AssertionError, '^claude: native exported session IDs did not bind to managed runs$'):
                    fixture.wait_sessions(data, 'claude', {'PRIVATE_MISSING_SESSION'})
            self.assertEqual(json.loads(output.getvalue())['database'], 'unavailable')
            self.assertNotIn('PRIVATE_', output.getvalue())

    def test_session_diagnostics_never_run_on_success_or_replace_binding_failure(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as directory:
            data = self.database(directory)
            with patch.object(fixture, 'session_failure_metadata', side_effect=RuntimeError('PRIVATE_ERROR')) as diagnostic:
                self.assertEqual(fixture.wait_sessions(data, 'codex', {'session'}), {'run'})
                diagnostic.assert_not_called()
                with redirect_stdout(io.StringIO()) as output, patch.object(fixture.time, 'monotonic', side_effect=[0, 41]):
                    with self.assertRaisesRegex(AssertionError, '^claude: native exported session IDs did not bind to managed runs$'):
                        fixture.wait_sessions(data, 'claude', {'missing'})
                diagnostic.assert_called_once()
                self.assertEqual(output.getvalue(), '')

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
