#!/usr/bin/env python3
"""Host-free bridge checks with native-shaped fake stdio agents."""

import fcntl
import json
import os
from pathlib import Path
import shutil
import subprocess
import signal
import sys
import tempfile
import time
import unittest


BRIDGE = Path(__file__).resolve().parents[1] / 'core/subyard-agent-bridge/bridge.py'
CREDENTIAL_HEADERS = ('Authorization: Basic synthetic-basic-value\n'
                      'Cookie: session=synthetic-cookie\n'
                      'Set-Cookie: id=synthetic-set-cookie; HttpOnly\n'
                      'Authorization = Basic Zml4dHVyZTphdXRo\n'
                      'Proxy-Authorization = Basic Zml4dHVyZTpwcm94eQ==\n'
                      'Visible line after headers.')
ESCAPED_PASSWORD = json.dumps({'password': 'prefix" with synthetic escaped suffix'})
SCOPE_FIXTURE = r'''#!/usr/bin/env python3
import os
from pathlib import Path
import signal
import sys
import time

scopes = Path(__file__).parent.parent / 'scopes'
args = sys.argv[1:]
if (scopes / 'offline').exists():
    sys.exit(1)

if Path(__file__).name == 'systemd-run':
    unit = scopes / next(arg[7:] for arg in args if arg.startswith('--unit='))
    gate = os.environ.get('BRIDGE_TEST_SCOPE_GATE')
    if gate:
        Path(gate + '.pid').write_text(str(os.getpid()))
        deadline = time.monotonic() + 8
        while not Path(gate).exists() and time.monotonic() < deadline:
            time.sleep(0.02)
    unit.write_text(str(os.getpid()))
    argv = args[args.index('--') + 1:]
    if os.environ.get('BRIDGE_TEST_RESULT_CRASH') or os.environ.get('BRIDGE_TEST_REPLY_GATE'):
        argv[1:1] = ['-B', '-c', """
import os, runpy, signal, sys, time
from pathlib import Path
main = runpy.run_path(sys.argv.pop(1))['main']
namespace = main.__globals__
save = namespace['save_state']
def crash(root, state):
    if state['status'] == 'done' and (point := os.environ.get('BRIDGE_TEST_RESULT_CRASH')):
        if point in ('after', 'suspend'):
            save(root, state)
        if point == 'suspend':
            os.kill(os.getpid(), signal.SIGSTOP)
            return
        os._exit(92)
    save(root, state)
namespace['save_state'] = crash
if gate := os.environ.get('BRIDGE_TEST_REPLY_GATE'):
    unlink = Path.unlink
    def pause_request_removal(path, *args, **kwargs):
        if path.suffix == '.json' and path.parent.name == 'approvals' and not args and not kwargs:
            Path(gate + '.held').touch()
            deadline = time.monotonic() + 6
            while not Path(gate).exists():
                assert time.monotonic() < deadline
                time.sleep(0.02)
        return unlink(path, *args, **kwargs)
    Path.unlink = pause_request_removal
main()
"""]
    os.execv(argv[0], argv)

unit = scopes / args[-1]
try:
    pid = int(unit.read_text())
except FileNotFoundError:
    pid = None
if 'show' in args:
    print('LoadState=loaded\nActiveState=active\nControlGroup=/fixture' if pid else
          'LoadState=not-found\nActiveState=inactive\nControlGroup=')
elif pid and not (scopes / 'busy').exists():
    unit.unlink(missing_ok=True)
    try:
        os.killpg(pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
'''
FAKE = r'''#!/usr/bin/env python3
import json
import os
from pathlib import Path
import select
import subprocess
import sys
import time

codex = Path(sys.argv[0]).name == 'codex'
session = 'fixture-session'
resumed = any(arg == '--resume=' + session for arg in sys.argv)
scenario = ''
if marker := os.environ.get('BRIDGE_TEST_NOT_READING'):
    assert select.select([sys.stdin], [], [], 5)[0], 'assignment never reached stdin'
    Path(marker).write_text(str(os.getpid()))
    time.sleep(60)
    sys.exit(0)
if os.environ.get('BRIDGE_TEST_CAPTURE'):
    Path(os.environ['BRIDGE_TEST_CAPTURE']).write_text(json.dumps({
        'argv': sys.argv[1:], 'cwd': os.getcwd(),
        'env': {key: os.environ.get(key) for key in ('BRIDGE_TEST_VALUE', 'TMPDIR', 'PATH')}}))

def emit(message):
    print(json.dumps(message), flush=True)

def finish(text):
    if codex:
        emit({'method': 'thread/tokenUsage/updated', 'params': {'tokenUsage': {
            'last': {'inputTokens': 3, 'outputTokens': 2, 'totalTokens': 5}}}})
        emit({'method': 'item/completed', 'params': {'item': {
            'type': 'agentMessage', 'text': text}}})
        emit({'method': 'turn/completed', 'params': {'turn': {'status': 'completed'}}})
    else:
        emit({'type': 'result', 'subtype': 'success', 'is_error': False, 'result': text,
              'usage': {'input_tokens': 3, 'output_tokens': 2,
                        'cache_creation': {'ephemeral_5m_input_tokens': 1}, 'example_flag': True}})

def say(text):
    if codex:
        emit({'method': 'item/completed', 'params': {'item': {'type': 'agentMessage', 'text': text}}})
    else:
        emit({'type': 'assistant', 'message': {'content': [{'type': 'text', 'text': text}]}})

def await_gate(suffix=''):
    gate = Path(os.environ['BRIDGE_TEST_HOLD_RESULT'] + suffix)
    deadline = time.monotonic() + 8
    while not gate.exists():
        assert time.monotonic() < deadline
        assert not select.select([sys.stdin], [], [], 0.02)[0], 'a follow-up arrived before the result'
    assert not select.select([sys.stdin], [], [], 0)[0], 'a follow-up arrived before the result'

def approval():
    if codex:
        if scenario == 'file-approval':
            emit({'method': 'item/started', 'params': {'item': {
                'id': 'file-item', 'type': 'fileChange', 'status': 'inProgress',
                'changes': [{'path': 'fixture.txt', 'kind': {'type': 'add'}, 'diff': '+fixture'}]}}})
        method = ('item/fileChange/requestApproval' if scenario == 'file-approval' else
                  'item/permissions/requestApproval' if scenario == 'unsupported' else
                  'item/commandExecution/requestApproval')
        request = {'id': 'approval-1', 'method': method, 'params': {
            'itemId': 'file-item',
            'command': 'fixture-command', 'reason': 'fixture-reason',
            'availableDecisions': ['accept', 'acceptForSession', 'decline']}}
    else:
        request = {'type': 'control_request', 'request_id': 'approval-1', 'request': {
            'subtype': 'unsupported' if scenario == 'unsupported' else 'can_use_tool',
            'tool_name': 'Bash', 'input': {'command': 'fixture-command'},
            'decision_reason': 'fixture-reason'}}
    emit(request)
    if scenario == 'duplicate':
        emit(request)


def fail_turn():
    if codex:
        emit({'method': 'turn/completed', 'params': {'turn': {'status': 'failed'}}})
    else:
        emit({'type': 'result', 'subtype': 'error_during_execution', 'is_error': True})


def begin(text):
    global scenario
    assert 'standalone worker with no parent agent' in text
    assert 'full report in your final message' in text
    scenario = text.split('\n')[-1]
    if codex:
        emit({'method': 'item/started', 'params': {'item': {
            'id': 'tool-call', 'type': 'commandExecution', 'status': 'inProgress'}}})
    else:
        emit({'type': 'assistant', 'message': {'content': [{'type': 'tool_use', 'name': 'Bash'}]}})
    if scenario in ('approval', 'pending', 'file-approval', 'unsupported', 'duplicate'):
        approval()
    elif scenario == 'eof':
        sys.exit(7)
    elif scenario == 'malformed':
        print('not json', flush=True)
    elif scenario == 'unknown-response':
        emit({'id': 'fixture-private-native-body', 'result': {}})
    elif scenario == 'result-error':
        fail_turn()
    elif scenario == 'report':
        say('The complete report.')
        if not codex:
            say('I sent the report to the parent.')
        finish('I sent the report to the parent.')
    elif scenario == 'credentials':
        say('Report text. Echoed value: ' + os.environ['BRIDGE_TEST_TOKEN'])
        say(os.environ['BRIDGE_TEST_HEADERS'])
        say(os.environ['BRIDGE_TEST_JSON'])
        finish('ghp_' + 'X' * 36)
    elif scenario == 'large':
        assert text.split('\n')[-2] == 'x' * (1024 * 1024)
        finish('complete large frame')
    elif scenario == 'message':
        if not codex:
            await_gate()
            finish('first result')
    elif scenario != 'steer-race':
        finish(('resumed:' if resumed else 'started:') + scenario)


for line in sys.stdin:
    message = json.loads(line)
    if codex:
        method = message.get('method')
        if method == 'initialize':
            emit({'id': message['id'], 'result': {}})
        elif method in ('thread/start', 'thread/resume'):
            resumed = method == 'thread/resume'
            if resumed:
                assert message['params']['threadId'] == session
            sandbox = 'dangerFullAccess'
            modes = {'read-only': 'readOnly', 'workspace-write': 'workspaceWrite',
                     'danger-full-access': 'dangerFullAccess'}
            for arg in sys.argv:
                if arg.startswith('sandbox_mode='):
                    sandbox = modes[json.loads(arg.split('=', 1)[1])]
            sandbox = os.environ.get('BRIDGE_TEST_EFFECTIVE_SANDBOX', sandbox)
            permissions = {'sandbox': {'type': sandbox}}
            if report := os.environ.get('BRIDGE_TEST_REVIEWER_REPORT'):
                permissions = json.loads(Path(report).read_text())
            emit({'id': message['id'], 'result': {'thread': {'id': session}, **permissions}})
        elif method == 'turn/start':
            if stamp := os.environ.get('BRIDGE_TEST_TURN_START'):
                Path(stamp).touch()
            text = message['params']['input'][0]['text']
            if text.split('\n')[-1] == 'rpc-error':
                emit({'id': message['id'], 'error': {'message': 'fixture error'}})
            else:
                emit({'id': message['id'], 'result': {'turn': {'id': 'fixture-turn'}}})
                if text.split('\n')[-1] == 'carryover':
                    assert resumed
                    finish('received:' + message['params']['input'][1]['text'])
                else:
                    begin(text)
        elif method == 'turn/steer':
            assert message['params']['expectedTurnId'] == 'fixture-turn'
            if scenario == 'steer-race':
                emit({'id': message['id'], 'error': {'code': -32602, 'message': 'turn id mismatch'}})
                finish('Original turn finished.')
            else:
                emit({'id': message['id'], 'result': {}})
                finish('message:' + message['params']['input'][0]['text'])
        elif method == 'turn/interrupt':
            emit({'id': message['id'], 'result': {}})
            emit({'method': 'turn/completed', 'params': {'turn': {'status': 'interrupted'}}})
        elif 'result' in message:
            decision = message['result']['decision']
            assert decision in ('accept', 'decline'), 'persistent permission response'
            finish('allow' if decision == 'accept' else 'deny')
    else:
        if message.get('type') == 'user':
            text = message['message']['content'][0]['text']
            if scenario:
                if text == 'held follow-up':
                    Path(os.environ['BRIDGE_TEST_HOLD_RESULT'] + '.received').touch()
                    await_gate('.second')
                if text == 'failing follow-up':
                    fail_turn()
                else:
                    finish('message:' + text)
            else:
                assert '--permission-prompt-tool' in sys.argv
                assert sys.argv[sys.argv.index('--permission-prompt-tool') + 1] == 'stdio'
                emit({'type': 'system', 'subtype': 'init', 'session_id': session, 'model': 'fixture-effective'})
                begin(text)
        elif message.get('type') == 'control_response':
            response = message['response']['response']
            assert 'updatedPermissions' not in response
            if response['behavior'] == 'allow':
                assert response['updatedInput'] == {'command': 'fixture-command'}
            finish(response['behavior'])
        elif message.get('type') == 'control_request':
            assert message['request']['subtype'] == 'interrupt'
            finish('interrupted')

if scenario == 'pending' and os.environ.get('BRIDGE_TEST_DESCENDANT'):
    child = subprocess.Popen(['sleep', '60'], stdin=subprocess.DEVNULL,
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env={},
                             start_new_session=bool(os.environ.get('BRIDGE_TEST_NEW_SESSION')))
    Path(os.environ['BRIDGE_TEST_DESCENDANT']).write_text(str(child.pid))
'''


def process_alive(pid):
    try:
        return (Path('/proc') / str(pid) / 'stat').read_text().rsplit(') ', 1)[1].split()[0] != 'Z'
    except FileNotFoundError:
        return False


class Bridge(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='subyard-agent-bridge-', dir='/tmp')
        self.root = Path(self.temp.name)
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        for vendor in ('codex', 'claude'):
            path = self.bin / vendor
            path.write_text(FAKE)
            path.chmod(0o755)
        self.scopes = self.root / 'scopes'
        self.scopes.mkdir()
        for command in ('systemd-run', 'systemctl'):
            path = self.bin / command
            path.write_text(SCOPE_FIXTURE)
            path.chmod(0o755)
        self.env = {**os.environ, 'PATH': str(self.bin) + os.pathsep + os.environ['PATH']}
        self.mask = -1
        self.workers = []
        self.addCleanup(self.cleanup)

    def cleanup(self):
        for root in self.workers:
            try:
                self.cli('stop', root, '--timeout', '1')
            except (OSError, AssertionError):
                pass
        self.temp.cleanup()

    def cli(self, command, root, *args, success=True):
        positional = ()
        if command in ('approve', 'deny'):
            positional, args = args[:1], args[1:]
        result = subprocess.run([sys.executable, str(BRIDGE), command, *positional, '--dir', str(root), *args],
                                env=self.env, text=True, capture_output=True, timeout=8, umask=self.mask)
        if success:
            self.assertEqual(result.returncode, 0, result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0)
        return result

    def start(self, vendor, task, *args, preset=False, dirname=None):
        root = self.root / (dirname or str(len(self.workers)))
        root.mkdir(mode=0o700)
        root.chmod(0o700)
        (root / 'task.md').write_text(task)
        self.workers.append(root)
        model = [] if preset else ['--model', 'fixture', '--effort', 'low']
        self.cli('start', root, '--vendor', vendor, *model,
                 '--cwd', str(self.root), '--executable', str(self.bin / vendor), *args)
        return root

    def start_message(self, vendor):
        gate = self.root / f'message-gate-{len(self.workers)}'
        root = self.start(vendor, 'message', '--env', f'BRIDGE_TEST_HOLD_RESULT={gate}')
        self.until(lambda: self.state(root).get('session_id'))
        return root, gate

    def until(self, condition):
        deadline = time.monotonic() + 6
        while time.monotonic() < deadline:
            result = condition()
            if result:
                return result
            time.sleep(0.02)
        self.fail('bounded fixture condition did not arrive')

    def state(self, root):
        return json.loads((root / 'state').read_text())

    def done(self, root, status='done'):
        self.until(lambda: self.state(root)['status'] not in ('running', 'launching'))
        state = self.state(root)
        self.assertEqual(state['status'], status, (root / 'events.jsonl').read_text())
        return Path(state['final_file']).read_text().strip() if status == 'done' else None

    def turn(self, root):
        return root / 'turns' / self.state(root)['generation']

    def result(self, root):
        return json.loads(Path(self.state(root)['result_file']).read_text())

    def approval(self, root):
        names = self.until(lambda: self.state(root)['pending_approvals'])
        path = self.turn(root) / 'approvals' / (names[0] + '.json')
        request = json.loads(path.read_text())
        self.assertEqual(request['command'], 'fixture-command')
        self.assertEqual(request['reason'], 'fixture-reason')
        self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        return path

    def reply(self, path, decision):
        temporary = path.with_suffix('.tmp')
        temporary.write_text(decision + '\n')
        temporary.replace(path.with_suffix('.reply'))

    def test_allow_deny_and_single_use_across_resume(self):
        for vendor in ('codex', 'claude'):
            with self.subTest(vendor=vendor):
                root = self.start(vendor, 'approval')
                first = self.approval(root)
                self.reply(first, 'allow')
                self.assertEqual(self.done(root), 'allow')
                self.assertEqual(list((self.turn(root) / 'approvals').iterdir()), [])
                self.assertEqual(self.state(root)['pending_approvals'], [])
                self.cli('resume', root)
                second = self.approval(root)
                self.assertNotEqual(first, second)
                self.assertNotEqual(first.parent, second.parent)
                self.reply(first, 'allow')
                self.assertEqual(self.cli('wait', root, '--timeout', '1', success=False).returncode, 2)
                self.assertEqual(self.state(root)['status'], 'running')
                self.reply(second, 'deny')
                self.assertEqual(self.done(root), 'deny')
                self.assertEqual(list((self.turn(root) / 'approvals').iterdir()), [])

    def test_pending_wait_and_interrupt(self):
        for vendor in ('codex', 'claude'):
            with self.subTest(vendor=vendor):
                root = self.start(vendor, 'pending')
                self.approval(root)
                result = self.cli('wait', root, '--timeout', '0.15', success=False)
                self.assertEqual(result.returncode, 2)
                state = json.loads(self.cli('status', root).stdout)
                self.assertEqual(state['status'], 'running')
                self.assertEqual(len(state['pending_approvals']), 1)
                self.assertFalse('final_file' in self.state(root))
                self.cli('interrupt', root)
                self.done(root, 'interrupted')
                self.assertEqual(list((self.turn(root) / 'approvals').iterdir()), [])
                (root / 'task.md').write_text('after-interrupt')
                self.cli('resume', root)
                self.assertEqual(self.done(root), 'resumed:after-interrupt')

    def test_message_resume_and_invalid_commands(self):
        for vendor in ('codex', 'claude'):
            with self.subTest(vendor=vendor):
                root, gate = self.start_message(vendor)
                refused = self.cli('resume', root, success=False)
                self.assertIn('resume requires a stopped worker with a session id', refused.stderr)
                duplicate = self.cli('start', root, '--vendor', vendor, '--cwd', str(self.root), success=False)
                self.assertIn('worker already exists', duplicate.stderr)
                self.cli('send', root, '--message', 'hello')
                gate.touch()
                self.assertEqual(self.done(root), 'first result\n\nmessage:hello' if vendor == 'claude' else 'message:hello')
                self.assertEqual(list((root / 'inbox').iterdir()), [])
                self.assert_send_refused(root)
                (root / 'task.md').write_text('next-task')
                self.cli('resume', root)
                self.assertEqual(self.done(root), 'resumed:next-task')
                self.assertEqual(self.state(root)['session_id'], 'fixture-session')

    def assert_send_refused(self, root):
        generation = self.state(root)['generation']
        result = self.cli('send', root, '--message', 'late', success=False)
        self.assertIn('use resume', result.stderr)
        self.assertEqual(self.state(root)['generation'], generation)
        self.assertFalse(list((root / 'inbox').iterdir()))

    def test_claude_followups_stay_queued_until_their_own_results(self):
        root, gate = self.start_message('claude')
        self.cli('send', root, '--message', 'held follow-up')
        self.cli('send', root, '--message', 'another follow-up')
        self.assertEqual(len(list((root / 'inbox').iterdir())), 2)
        gate.touch()
        self.until(gate.with_suffix('.received').exists)
        self.assertEqual(self.cli('wait', root, '--timeout', '0.1', success=False).returncode, 124)
        self.assertEqual(len(list((root / 'inbox').iterdir())), 2)
        gate.with_suffix('.second').touch()
        self.assertEqual(self.done(root), 'first result\n\nmessage:held follow-up\n\nmessage:another follow-up')
        self.assertEqual(list((root / 'inbox').iterdir()), [])
        self.assertEqual(self.result(root)['tokens'], {'input_tokens': 9, 'output_tokens': 6})

    def test_claude_retires_only_the_followup_that_returned_an_error(self):
        root, gate = self.start_message('claude')
        self.cli('send', root, '--message', 'failing follow-up')
        self.cli('send', root, '--message', 'next follow-up')
        gate.touch()
        self.done(root, 'failed')
        queued = list((root / 'inbox').glob('*.md'))
        self.assertEqual([path.read_text() for path in queued], ['next follow-up'])
        (root / 'task.md').write_text('recover')
        self.cli('resume', root)
        self.assertEqual(self.done(root), 'resumed:recover\n\nmessage:next follow-up')
        self.assertFalse(list((root / 'inbox').iterdir()))

    def test_claude_completion_waits_for_an_accepted_send_to_finish_enqueueing(self):
        held = self.root / 'enqueue-held'
        release = self.root / 'enqueue-release'
        root, gate = self.start_message('claude')
        script = """
import os
from pathlib import Path
import runpy
import sys
import time

bridge, held, release = sys.argv[1:4]
del sys.argv[1:4]
rename = os.replace
def pause(source, target):
    if target.parent.name == 'inbox':
        Path(held).touch()
        deadline = time.monotonic() + 6
        while not Path(release).exists():
            assert time.monotonic() < deadline
            time.sleep(0.02)
    rename(source, target)
os.replace = pause
runpy.run_path(bridge, run_name='__main__')
"""
        with subprocess.Popen([sys.executable, '-B', '-c', script, str(BRIDGE), str(held), str(release),
                               'send', '--dir', str(root), '--message', 'racing message'], env=self.env) as sender:
            try:
                self.until(held.exists)
                gate.touch()
                self.assertEqual(self.cli('wait', root, '--timeout', '0.3', success=False).returncode, 124)
            finally:
                release.touch()
            self.assertEqual(sender.wait(timeout=4), 0)
        self.assertEqual(self.done(root), 'first result\n\nmessage:racing message')
        self.assertEqual(list((root / 'inbox').iterdir()), [])

    def test_rejected_steer_preserves_worker_and_delivers_on_next_turn(self):
        root = self.start('codex', 'steer-race')
        self.until(lambda: self.state(root)['turn_id'])
        self.cli('send', root, '--message', 'racing message')
        self.assertEqual(self.done(root), 'Original turn finished.')
        self.assertEqual(len(list((root / 'inbox').iterdir())), 1)
        (root / 'task.md').write_text('carryover')
        self.cli('resume', root)
        self.assertEqual(self.done(root), 'received:racing message')
        self.assertEqual(self.state(root)['session_id'], 'fixture-session')
        self.assertEqual(list((root / 'inbox').iterdir()), [])

    def test_final_preserves_the_full_report_and_following_message(self):
        for vendor in ('codex', 'claude'):
            with self.subTest(vendor=vendor):
                root = self.start(vendor, 'report')
                self.assertEqual(self.done(root), 'The complete report.\n\nI sent the report to the parent.')

    def test_event_messages_and_tail_redact_credentials(self):
        for vendor in ('codex', 'claude'):
            with self.subTest(vendor=vendor):
                root = self.start(vendor, 'credentials', '--env', 'BRIDGE_TEST_TOKEN=synthetic-env-value',
                                  '--env', 'BRIDGE_TEST_HEADERS=' + CREDENTIAL_HEADERS,
                                  '--env', 'BRIDGE_TEST_JSON=' + ESCAPED_PASSWORD)
                self.done(root)
                events = (root / 'events.jsonl').read_text()
                secrets = ('synthetic-env-value', 'ghp_' + 'X' * 36, 'synthetic-basic-value',
                           'synthetic-cookie', 'synthetic-set-cookie', 'synthetic escaped suffix',
                           'Zml4dHVyZTphdXRo', 'Zml4dHVyZTpwcm94eQ==')
                for secret in secrets:
                    self.assertNotIn(secret, events)
                tail = self.cli('tail', root).stdout
                self.assertTrue(set(tail.splitlines()) <= set(events.splitlines()))
                self.assertIn('Report text. Echoed value: [redacted]', tail)
                expected = ('Authorization: [redacted]\nCookie: [redacted]\n'
                            'Set-Cookie: [redacted]\n'
                            'Authorization = [redacted]\nProxy-Authorization = [redacted]\n'
                            'Visible line after headers.')
                self.assertIn(expected, [json.loads(line)['text'] for line in events.splitlines()
                                         if json.loads(line)['type'] == 'message'])

    def test_invalid_and_unsolicited_replies_fail_closed(self):
        for vendor in ('codex', 'claude'):
            for reply in ('allow-for-session', 'unsolicited'):
                with self.subTest(vendor=vendor, reply=reply):
                    root = self.start(vendor, 'approval')
                    path = self.approval(root)
                    target = path.with_suffix('.reply') if reply != 'unsolicited' else path.parent / '999999.reply'
                    self.reply(target.with_suffix('.json'), reply)
                    self.done(root, 'failed')
                    self.assertFalse('final_file' in self.state(root))

    def test_codex_file_approval_and_directory_permissions(self):
        root = self.start('codex', 'file-approval')
        path = self.approval(root)
        self.assertEqual(json.loads(path.read_text())['item']['changes'][0]['path'], 'fixture.txt')
        self.reply(path, 'allow')
        self.assertEqual(self.done(root), 'allow')
        for path in (root / 'state', root / 'config.json', root / 'events.jsonl', root / 'bridge.log',
                     Path(self.state(root)['final_file']), Path(self.state(root)['result_file'])):
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        unsafe = self.root / 'unsafe'
        unsafe.mkdir()
        unsafe.chmod(0o755)
        self.cli('status', unsafe, success=False)
        linked = self.root / 'linked'
        linked.symlink_to(root, target_is_directory=True)
        self.cli('status', linked, success=False)

    def test_missing_executable_fails_without_a_session(self):
        root = self.start('codex', 'unused', '--executable', str(self.root / 'missing'))
        self.done(root, 'failed')
        self.assertNotIn('error', self.result(root))
        error = json.loads(self.cli('tail', root, '-n', '1').stdout)
        self.assertEqual(error['error_type'], 'FileNotFoundError')
        self.assertNotIn(str(self.root), error['message'])
        self.cli('resume', root, success=False)

    def test_exact_modes_under_restrictive_umask(self):
        self.mask = 0o777
        root = self.start('claude', 'approval')
        self.reply(self.approval(root), 'allow')
        self.assertEqual(self.done(root), 'allow')
        for path in (root / 'state', root / 'config.json', root / 'events.jsonl', root / 'bridge.log',
                     root / 'command.lock', root / 'worker.lock',
                     Path(self.state(root)['final_file']), Path(self.state(root)['result_file'])):
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        for path in (root, root / 'inbox', self.turn(root), self.turn(root) / 'approvals'):
            self.assertEqual(path.stat().st_mode & 0o777, 0o700)

    def test_passthrough_environment_cwd_and_native_executable(self):
        scratch = self.root / 'scratch'
        scratch.mkdir()
        for vendor in ('codex', 'claude'):
            with self.subTest(vendor=vendor):
                capture = self.root / (vendor + '-capture.json')
                vendor_args = (['-c', 'fixture.key="value"', '--profile', 'fixture-profile',
                                '--sandbox', 'workspace-write'] if vendor == 'codex' else
                               ['--permission-mode', 'manual', '--max-turns', '2',
                                '--append-system-prompt', 'fixture instruction'])
                fixture_path = str(self.bin) + ':/usr/bin'
                root = self.start(vendor, 'complete', '--env', 'BRIDGE_TEST_CAPTURE=' + str(capture),
                                  '--env', 'BRIDGE_TEST_VALUE=fixture-value', '--env', 'TMPDIR=' + str(scratch),
                                  '--env', 'PATH=' + fixture_path, '--', *vendor_args)
                self.done(root)
                info = json.loads(capture.read_text())
                argv = info['argv']
                offset = argv.index(vendor_args[0])
                expected = (vendor_args[:-2] + ['-c', 'sandbox_mode="workspace-write"']
                            if vendor == 'codex' else vendor_args)
                self.assertEqual(argv[offset:offset + len(expected)], expected)
                self.assertEqual(info['cwd'], str(self.root))
                self.assertEqual(info['env'], {'BRIDGE_TEST_VALUE': 'fixture-value',
                                              'TMPDIR': str(scratch), 'PATH': fixture_path})
                (root / 'task.md').write_text('next')
                self.cli('resume', root)
                self.assertEqual(self.done(root), 'resumed:next')
                self.assertEqual(json.loads(capture.read_text())['env'], info['env'])

    def test_codex_refuses_mismatched_sandbox_before_starting_turn(self):
        flags = (('--sandbox', 'read-only'), ('-s=read-only',), ('-c', 'sandbox_mode="read-only"'))
        for args in flags:
            with self.subTest(flags=args):
                stamp = self.root / 'codex-turn-start'
                root = self.start('codex', 'complete', '--env', 'BRIDGE_TEST_TURN_START=' + str(stamp),
                                  '--env', 'BRIDGE_TEST_EFFECTIVE_SANDBOX=dangerFullAccess', '--', *args)
                self.done(root, 'failed')
                self.assertEqual(self.result(root)['error'], 'Codex did not apply the requested sandbox.')
                self.assertFalse(stamp.exists())
                self.assertEqual(json.loads(self.cli('stop', root).stdout)['cleanup'], 'verified')

    def test_codex_reviewer_refuses_explicit_sandbox_at_start(self):
        root = self.root / 'reviewer'
        for flags in (('--sandbox', 'read-only'), ('-s=workspace-write',), ('-c', 'sandbox_mode="read-only"')):
            with self.subTest(flags=flags):
                result = self.cli('start', root, '--vendor', 'codex', '--role', 'reviewer', '--cwd', str(self.root),
                                  '--', *flags, success=False)
                self.assertIn('--role reviewer, which fixes the sandbox', result.stderr)
                self.assertFalse((root / 'state').exists())

    def test_codex_reviewer_rechecks_permissions_on_resume(self):
        report = self.root / 'reviewer-report.json'
        stamp = self.root / 'reviewer-turn-start'
        restricted = {'sandbox': {'type': 'workspaceWrite', 'writableRoots': ['/tmp']},
                      'activePermissionProfile': {'id': 'bridge-reviewer', 'extends': ':read-only'}}
        report.write_text(json.dumps(restricted))
        root = self.start('codex', 'complete', '--role', 'reviewer', '--env', 'TMPDIR=/tmp',
                          '--env', f'BRIDGE_TEST_REVIEWER_REPORT={report}',
                          '--env', f'BRIDGE_TEST_TURN_START={stamp}')
        self.assertEqual(self.done(root), 'started:complete')
        stamp.unlink()
        for incorrect in (
            {**restricted, 'sandbox': {'type': 'dangerFullAccess'}},
            {**restricted, 'sandbox': {'type': 'workspaceWrite', 'writableRoots': ['/tmp', str(self.root)]}},
            {'sandbox': restricted['sandbox']},
        ):
            with self.subTest(permissions=incorrect):
                report.write_text(json.dumps(incorrect))
                self.cli('resume', root)
                self.done(root, 'failed')
                self.assertEqual(self.result(root)['error'], 'Codex did not apply the reviewer permissions.')
                self.assertFalse(stamp.exists())
                self.assertEqual(self.state(root)['session_id'], 'fixture-session')
        report.write_text(json.dumps(restricted))
        self.cli('resume', root)
        self.assertEqual(self.done(root), 'resumed:complete')
        self.assertTrue(stamp.exists())

    def test_role_defaults_overrides_and_reviewer_temporary_writes(self):
        scratch = self.root.parent / (self.root.name + '-scratch')
        scratch.mkdir()
        self.addCleanup(scratch.rmdir)
        for vendor in ('codex', 'claude'):
            for role in ('developer', 'reviewer', 'observer'):
                with self.subTest(vendor=vendor, role=role):
                    capture = self.root / 'role-capture.json'
                    stamp = self.root / f'{vendor}-{role}-turn'
                    # The fake Codex server reports dangerFullAccess without a fixture report.
                    refused = vendor == 'codex' and role == 'reviewer'
                    root = self.start(vendor, 'complete', '--role', role, '--env', 'TMPDIR=' + str(scratch),
                                      '--env', f'BRIDGE_TEST_CAPTURE={capture}',
                                      '--env', f'BRIDGE_TEST_TURN_START={stamp}', preset=True)
                    self.done(root, 'failed' if refused else 'done')
                    if refused:
                        self.assertEqual(self.result(root)['error'], 'Codex did not apply the reviewer permissions.')
                        self.assertFalse(stamp.exists())
                    config = json.loads((root / 'config.json').read_text())
                    self.assertEqual(config['effort'], 'medium' if role == 'observer' else 'high')
                    if role == 'observer':
                        self.assertEqual(config['model'], 'gpt-6-luna' if vendor == 'codex' else 'haiku')
                    if role == 'reviewer':
                        argv = json.loads(capture.read_text())['argv']
                        if vendor == 'codex':
                            profile = next(arg for arg in argv if arg.startswith('permissions.bridge-reviewer='))
                            import tomllib
                            permissions = tomllib.loads(profile)['permissions']['bridge-reviewer']['filesystem']
                            self.assertEqual(permissions[str(self.root)], 'read')
                            self.assertEqual(permissions['/tmp'], 'write')
                            self.assertEqual(permissions[str(scratch)], 'write')
                        else:
                            settings = json.loads(argv[argv.index('--settings') + 1])
                            self.assertEqual(settings['sandbox']['filesystem']['denyWrite'], [str(self.root)])
                            self.assertIn(str(scratch), settings['sandbox']['filesystem']['allowWrite'])
                            self.assertFalse(settings['sandbox']['allowUnsandboxedCommands'])
                            self.assertTrue(settings['sandbox']['failIfUnavailable'])
                    override = self.start(vendor, 'complete', '--role', role)
                    self.done(override, 'failed' if refused else 'done')
                    self.assertEqual(json.loads((override / 'config.json').read_text())['model'], 'fixture')

    def test_wait_exit_codes_result_and_tail(self):
        for vendor in ('codex', 'claude'):
            with self.subTest(vendor=vendor):
                root, gate = self.start_message(vendor)
                timed = self.cli('wait', root, '--timeout', '0.1', success=False)
                self.assertEqual(timed.returncode, 124)
                self.cli('send', root, '--message', 'finished')
                gate.touch()
                self.cli('wait', root, '--timeout', '5')
                result = self.result(root)
                self.assertEqual(result['state'], 'done')
                self.assertEqual(result['exit_code'], 0)
                self.assertEqual(result['session_id'], 'fixture-session')
                self.assertIsNotNone(result['tokens'])
                self.assertGreaterEqual(result['duration_seconds'], 0)
                tail = self.cli('tail', root, '-n', '1').stdout.splitlines()
                self.assertEqual(len(tail), 1)
                self.assertEqual(json.loads(tail[0])['text'], 'message:finished')
                self.assertEqual(self.cli('tail', root, '-n', '0').stdout, '')
                failed = self.start(vendor, 'result-error')
                self.assertEqual(self.cli('wait', failed, '--timeout', '5', success=False).returncode, 1)
                self.assertEqual(self.result(failed)['exit_code'], 1)
                self.assert_send_refused(failed)
                pending = self.start(vendor, 'pending')
                self.approval(pending)
                self.assertEqual(self.cli('wait', pending, success=False).returncode, 2)
                self.cli('stop', pending, '--timeout', '5')
                self.assertEqual(self.cli('wait', pending, success=False).returncode, 130)
                self.assertEqual(self.result(pending)['exit_code'], 130)
                self.assert_send_refused(pending)
                self.cli('stop', pending)

    def test_approval_commands_and_independent_named_workers(self):
        roots = [self.start('codex', 'approval', '--name', 'codex-worker', dirname='state'),
                 self.start('claude', 'approval', '--name', 'claude-worker')]
        for root in roots:
            self.approval(root)
        listed = json.loads(self.cli('list', self.root).stdout)
        self.assertEqual({item['dir'] for item in listed}, {str(root) for root in roots})
        self.assertEqual({item['name'] for item in listed}, {'codex-worker', 'claude-worker'})
        for root, command in zip(roots, ('approve', 'deny')):
            requests = json.loads(self.cli('approvals', root).stdout)
            self.assertEqual(len(requests), 1)
            name = requests[0]['id']
            self.cli(command, root, name)
            self.assertEqual(self.done(root), 'allow' if command == 'approve' else 'deny')
            self.cli(command, root, name, success=False)
            self.assertEqual(json.loads(self.cli('approvals', root).stdout), [])

    def test_approval_stays_answered_during_request_retirement(self):
        gate = self.root / 'reply-gate'
        self.env['BRIDGE_TEST_REPLY_GATE'] = str(gate)
        root = self.start('codex', 'approval')
        request = self.approval(root)
        pid = self.state(root)['pid']
        try:
            os.kill(pid, signal.SIGSTOP)
            try:
                self.cli('approve', root, request.stem)
                self.assertEqual(json.loads(self.cli('approvals', root).stdout), [])
                waiting = self.cli('wait', root, '--timeout', '0', success=False)
                self.assertEqual(waiting.returncode, 124)
                self.assertEqual(json.loads(waiting.stdout)['pending_approvals'], [])
            finally:
                os.kill(pid, signal.SIGCONT)
            self.until(gate.with_suffix('.held').exists)
            self.assertTrue(request.exists())
            self.assertIn(request.stem, self.state(root)['pending_approvals'])
            refused = self.cli('deny', root, request.stem, success=False)
            self.assertIn('approval already answered', refused.stderr)
            self.assertEqual(request.with_suffix('.reply').read_text().strip(), 'allow')
        finally:
            gate.touch()
        self.assertEqual(self.done(root), 'allow')
        self.assertFalse(request.with_suffix('.reply').exists())

    def test_missing_worker_commands_do_not_create_directories(self):
        missing = self.root / 'missing'
        for command, args in (('send', ('--message', 'hello')), ('resume', ()),
                              ('approve', ('000001',)), ('deny', ('000001',))):
            refused = self.cli(command, missing, *args, success=False)
            self.assertEqual(refused.stderr.strip(), 'worker state is missing')
            self.assertFalse(missing.exists())
        missing.mkdir(mode=0o700)
        refused = self.cli('resume', missing, success=False)
        self.assertEqual(refused.stderr.strip(), 'worker state is missing')
        self.assertFalse(list(missing.iterdir()))
        missing.rmdir()
        for command in ('status', 'tail'):
            refused = self.cli(command, missing, success=False)
            self.assertEqual(refused.stderr.strip(), 'worker state is missing')
            self.assertFalse(missing.exists())
        self.assertEqual(json.loads(self.cli('list', missing).stdout), [])
        self.assertFalse(missing.exists())
        root = self.start('codex', 'complete')
        self.done(root)
        (root / 'inbox').rmdir()
        for command, directory in (('status', root), ('tail', root), ('list', self.root)):
            self.cli(command, directory)
            self.assertFalse((root / 'inbox').exists())
        (root / 'events.jsonl').unlink()
        self.assertEqual(self.cli('tail', root, '-n', '0').stdout, '')
        self.cli('tail', root, '-n', '1', success=False)

    def test_resume_task_file_and_no_stdin_hang(self):
        root = self.root / 'no-stdin'
        task = self.root / 'assignment.md'
        task.write_text('complete')
        process = subprocess.Popen([sys.executable, str(BRIDGE), 'start', '--dir', str(root),
                                    '--vendor', 'codex', '--model', 'fixture', '--effort', 'low',
                                    '--cwd', str(self.root), '--task', str(task),
                                    '--executable', str(self.bin / 'codex')],
                                   env=self.env, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.workers.append(root)
        try:
            self.assertEqual(process.wait(timeout=3), 0)
        finally:
            process.stdin.close()
            process.stdout.close()
            process.stderr.close()
        self.done(root)
        task.write_text('task-from-file')
        self.cli('resume', root, '--task', str(task))
        self.assertEqual(self.done(root), 'resumed:task-from-file')
        self.cli('stop', root)
        self.cli('stop', self.root / 'missing-worker')

    def test_stop_and_resume_fence_a_send_waiting_for_eof(self):
        root = self.start('codex', 'pending')
        self.approval(root)
        marker = self.root / 'reading'
        script = """
from pathlib import Path
import runpy
import sys

bridge, marker = sys.argv[1:3]
del sys.argv[1:3]
read = sys.stdin.read
def wait_for_eof():
    Path(marker).touch()
    return read()
sys.stdin.read = wait_for_eof
runpy.run_path(bridge, run_name='__main__')
"""
        sender = subprocess.Popen([sys.executable, '-B', '-c', script, str(BRIDGE), str(marker),
                                   'send', '--dir', str(root)], env=self.env, stdin=subprocess.PIPE,
                                  stderr=subprocess.PIPE, text=True)
        try:
            self.until(marker.exists)
            stopped = json.loads(self.cli('stop', root, '--timeout', '0.1').stdout)
            self.assertEqual(stopped['cleanup'], 'verified')
            self.assertIsNone(sender.poll())
            previous = self.state(root)['generation']
            self.cli('resume', root)
            self.approval(root)
            self.assertNotEqual(previous, self.state(root)['generation'])
            _, diagnostic = sender.communicate('stale message', timeout=3)
            self.assertEqual(sender.returncode, 1)
            self.assertIn('active turn changed', diagnostic)
            self.assertFalse(list((root / 'inbox').iterdir()))
        finally:
            if sender.poll() is None:
                sender.kill()
                sender.communicate(timeout=2)

    def test_lock_timeouts(self):
        timeout = ('--timeout', '0.1')
        with self.subTest(lock='command'):
            root = self.start('codex', 'pending')
            request = self.approval(root)
            with (root / 'command.lock').open() as lock:
                fcntl.flock(lock, fcntl.LOCK_EX)
                for command in ('start', 'resume', 'interrupt', 'stop', 'approve', 'deny'):
                    with self.subTest(command=command):
                        approval = [request.stem] if command in ('approve', 'deny') else []
                        start = ['--vendor', 'codex', '--cwd', str(self.root)] if command == 'start' else []
                        waiting = self.cli(command, root, *approval, *start, *timeout, success=False)
                        self.assertEqual(waiting.returncode, 124)
                        self.assertIn('command lock', waiting.stderr)
                self.assertFalse((self.turn(root) / 'interrupt').exists())
                self.assertFalse(request.with_suffix('.reply').exists())
            self.cli('stop', root, *timeout)

        for command, scenario in (('send', 'pending'), ('resume', 'complete')):
            with self.subTest(lock='inbox', command=command):
                root = self.start('claude' if command == 'send' else 'codex', scenario)
                options = []
                lock_path = root / 'inbox.lock'
                if command == 'send':
                    self.approval(root)
                    lock_path.touch(mode=0o600)
                    lock_path.chmod(0o600)
                    options = ['--message', 'late']
                else:
                    self.done(root)
                    self.cli('stop', root)
                    (root / 'bridge.log').unlink()
                    (root / 'bridge.log').symlink_to(root / 'task.md')
                with lock_path.open() as lock:
                    fcntl.flock(lock, fcntl.LOCK_EX)
                    waiting = self.cli(command, root, *options, *timeout, success=False)
                    self.assertEqual(waiting.returncode, 124)
                    self.assertIn('inbox lock', waiting.stderr)
                    if command == 'send':
                        self.assertFalse(list((root / 'inbox').iterdir()))
                if command == 'resume':
                    self.assertIn('Launch failed', waiting.stderr)
                    state = json.loads(self.cli('status', root).stdout)
                    self.assertEqual(state['status'], 'failed')
                    self.assertNotIn('result_file', state)
                    stopped = json.loads(self.cli('stop', root).stdout)
                    self.assertEqual(stopped['cleanup'], 'verified')
                    self.assertEqual(self.result(root)['state'], 'failed')
                    self.assertEqual(self.result(root)['error'], 'Systemd user scope launcher could not start.')
                    log = root / 'bridge.log'
                    log.unlink()
                    log.touch()
                    log.chmod(0o644)
                    refused = self.cli('resume', root, success=False)
                    self.assertIn('unsafe state file', refused.stderr)
                    self.done(root, 'failed')
                    self.assertEqual(self.result(root)['error'], 'unsafe state file')
                    with lock_path.open() as lock:
                        fcntl.flock(lock, fcntl.LOCK_EX)
                        refused = self.cli('resume', root, *timeout, success=False)
                        self.assertEqual(refused.returncode, 124)
                        self.assertIn('inbox lock', refused.stderr)
                        failed = json.loads(self.cli('status', root).stdout)
                        self.assertEqual(failed['status'], 'failed')
                        self.assertEqual(failed['error'], 'unsafe state file')
                        self.assertNotIn('result_file', failed)
                    stopped = json.loads(self.cli('stop', root).stdout)
                    self.assertEqual(stopped['cleanup'], 'verified')
                    self.assertEqual(self.result(root)['error'], 'unsafe state file')
                    event = json.loads(self.cli('tail', root, '-n', '1').stdout)
                    self.assertEqual(event, {'type': 'error', 'message': 'unsafe state file'})

    def test_bridge_crash_is_failed_and_stop_cleans_worker(self):
        root = self.start('codex', 'pending')
        self.approval(root)
        state = self.state(root)
        os.kill(state['pid'], signal.SIGKILL)
        self.until(lambda: json.loads(self.cli('status', root).stdout)['status'] == 'failed')
        self.assertEqual(self.cli('wait', root, success=False).returncode, 1)
        self.cli('stop', root)
        self.cli('stop', root)
        self.assertEqual(self.result(root)['state'], 'failed')
        self.assertEqual(list((self.turn(root) / 'approvals').iterdir()), [])

    def resume_with_gate(self, root, task, point, gate=None):
        script = """
import os
from pathlib import Path
import runpy
import sys
import time

bridge, point = sys.argv[1:3]
del sys.argv[1:3]
main = runpy.run_path(bridge)['main']
module = main.__globals__['subprocess']
popen = module.Popen
def crash(argv, *args, **kwargs):
    if argv[0] != 'systemd-run':
        return popen(argv, *args, **kwargs)
    if point != 'before':
        child = popen(argv, *args, **kwargs)
        stamp = (Path(os.environ['BRIDGE_TEST_SCOPE_GATE'] + '.pid') if point == 'delayed'
                 else Path(os.environ['BRIDGE_TEST_SCOPES']) /
                 next(arg[7:] for arg in argv if arg.startswith('--unit=')))
        deadline = time.monotonic() + 5
        while not stamp.exists():
            assert time.monotonic() < deadline and child.poll() is None
            time.sleep(0.02)
    os._exit(91)
module.Popen = crash
main()
"""
        env = {**self.env, 'BRIDGE_TEST_SCOPES': str(self.scopes)}
        if gate is not None:
            env['BRIDGE_TEST_SCOPE_GATE'] = str(gate)
        crash = subprocess.run([sys.executable, '-B', '-c', script, str(BRIDGE), point,
                                'resume', '--dir', str(root), '--task', str(task)],
                               env=env,
                               capture_output=True, text=True, timeout=8)
        self.assertEqual(crash.returncode, 91, crash.stderr)
        return self.state(root)

    def test_launcher_crash_on_either_side_of_scope_creation(self):
        root = self.start('codex', 'complete')
        self.done(root)
        original_session = self.state(root)['session_id']
        task = self.root / 'retry.md'
        task.write_text('after-launch-crash')
        for point in ('before', 'after'):
            with self.subTest(point=point):
                previous = self.state(root)
                state = self.resume_with_gate(root, task, point)
                self.assertEqual(state['status'], 'launching')
                self.assertGreater(state['pid'], 0)
                self.assertEqual((self.scopes / state['scope']).exists(), point == 'after')
                status = json.loads(self.cli('status', root).stdout)
                self.assertEqual(status['status'], 'failed')
                self.assertFalse('final_file' in status or 'result_file' in status)
                self.assertTrue(Path(previous['final_file']).exists())
                self.assertEqual(self.cli('wait', root, '--timeout', '1', success=False).returncode, 1)
                if point == 'before':
                    self.cli('stop', root, '--timeout', '1')
                    self.assertEqual(self.result(root)['state'], 'failed')
                self.cli('resume', root, '--task', str(task))
                self.assertEqual(self.done(root), 'resumed:after-launch-crash')
                self.assertEqual(self.state(root)['session_id'], original_session)
                self.assertFalse((self.scopes / state['scope']).exists())

    def test_delayed_old_launcher_is_fenced_after_resume(self):
        root = self.start('codex', 'complete')
        self.done(root)
        task = self.root / 'delayed.md'
        task.write_text('approval')
        gate = self.root / 'scope-gate'
        old = self.resume_with_gate(root, task, 'delayed', gate)
        pid = int(gate.with_suffix('.pid').read_text())
        try:
            self.assertFalse((self.scopes / old['scope']).exists())
            task.write_text('pending')
            self.cli('resume', root, '--task', str(task))
            pending = self.approval(root)
            current = self.state(root)
            self.assertNotEqual(current['generation'], old['generation'])
            gate.touch()
            self.until(lambda: not process_alive(pid))
            self.assertEqual(self.state(root), current)
            self.assertFalse(list((root / 'turns' / old['generation'] / 'approvals').iterdir()))
            self.cli('deny', root, pending.stem)
            self.assertEqual(self.done(root), 'deny')
        finally:
            try:
                os.killpg(pid, signal.SIGKILL)
            except ProcessLookupError:
                pass

    def test_large_assignment_is_delivered_intact(self):
        root = self.start('claude', 'x' * (1024 * 1024) + '\nlarge')
        self.assertEqual(self.done(root), 'complete large frame')

    def test_interrupt_and_stop_with_vendor_stdin_backpressure(self):
        for forced in (False, True):
            with self.subTest(forced=forced):
                marker = self.root / f'non-reader-{forced}'
                root = self.start('claude', 'x' * (1024 * 1024),
                                  '--env', 'BRIDGE_TEST_NOT_READING=' + str(marker))
                pid = int(self.until(lambda: marker.read_text() if marker.exists() else None))
                self.assertTrue(process_alive(pid))
                bridge_pid = self.state(root)['pid']
                if forced:
                    os.kill(bridge_pid, signal.SIGSTOP)
                try:
                    if not forced:
                        self.cli('interrupt', root)
                        result = self.cli('wait', root, '--timeout', '2', success=False)
                        self.assertEqual(result.returncode, 130)
                    stopped = json.loads(self.cli('stop', root, '--timeout', '0.1').stdout)
                    self.assertIn(stopped['status'], ('interrupted', 'failed') if forced else ('interrupted',))
                    self.assertEqual(stopped['cleanup'], 'verified')
                    self.until(lambda: not process_alive(pid))
                    self.cli('stop', root, '--timeout', '0')
                finally:
                    if forced and process_alive(bridge_pid):
                        os.kill(bridge_pid, signal.SIGCONT)

    def test_result_publication_has_one_commit_point(self):
        failures = ('eof', 'malformed', 'result-error', 'unsupported', 'duplicate')
        for vendor in ('codex', 'claude'):
            scenarios = failures + ('rpc-error', 'unknown-response') if vendor == 'codex' else failures
            for scenario in scenarios:
                with self.subTest(vendor=vendor, scenario=scenario):
                    root = self.start(vendor, scenario)
                    self.done(root, 'failed')
                    self.assertNotIn('final_file', self.state(root))
                    self.assertFalse((self.turn(root) / 'final.md').exists())
                    self.assertFalse(list((self.turn(root) / 'approvals').iterdir()))
                    if scenario == 'unsupported':
                        reason = ('unsupported Codex server request' if vendor == 'codex'
                                  else 'unsupported Claude control request')
                        self.assertEqual(self.result(root)['error'], reason)
                        self.assertEqual(json.loads(self.cli('tail', root, '-n', '1').stdout)['message'], reason)
                    if scenario == 'unknown-response':
                        error = json.loads(self.cli('tail', root, '-n', '1').stdout)
                        self.assertEqual(error['error_type'], 'KeyError')
                        self.assertNotIn('fixture-private-native-body', json.dumps(error))
                        self.assertNotIn('fixture-private-native-body', json.dumps(self.result(root)))
        for point, status, code in (('before', 'failed', 1), ('after', 'done', 0)):
            with self.subTest(point=point):
                self.env['BRIDGE_TEST_RESULT_CRASH'] = point
                try:
                    root = self.start('codex', 'complete')
                finally:
                    self.env.pop('BRIDGE_TEST_RESULT_CRASH')
                result = self.cli('wait', root, '--timeout', '4', success=(code == 0))
                self.assertEqual(result.returncode, code)
                state = json.loads(result.stdout)
                self.assertEqual(state['status'], status)
                self.assertTrue((self.turn(root) / 'final.md').exists())
                if point == 'before':
                    self.assertFalse('result_file' in state or 'final_file' in state)
                else:
                    self.assertEqual(Path(state['final_file']).read_text().strip(), 'started:complete')
                    self.assertEqual(self.result(root)['state'], 'done')
                self.cli('stop', root)
                self.assertEqual(self.result(root)['state'], status)
                self.assertEqual('final_file' in self.state(root), point == 'after')

    def test_stop_retires_bridge_suspended_after_publishing_done(self):
        self.env['BRIDGE_TEST_RESULT_CRASH'] = 'suspend'
        try:
            root = self.start('codex', 'complete')
        finally:
            self.env.pop('BRIDGE_TEST_RESULT_CRASH')
        self.assertEqual(self.done(root), 'started:complete')
        published = self.state(root)
        pid = published['pid']
        try:
            stat_file = Path('/proc') / str(pid) / 'stat'
            self.until(lambda: stat_file.read_text().rsplit(') ', 1)[1].startswith('T '))
            result = self.result(root)
            late = self.cli('send', root, '--message', 'late', '--timeout', '0', success=False)
            self.assertEqual(late.returncode, 1)
            self.assertIn('use resume', late.stderr)
            resumed = self.cli('resume', root, '--timeout', '0.1', success=False)
            self.assertEqual(resumed.returncode, 124)
            self.assertIn('worker lock', resumed.stderr)
            self.assertEqual(self.state(root), published)
            stopped = json.loads(self.cli('stop', root, '--timeout', '0.1').stdout)
            self.assertEqual(stopped['status'], 'done')
            self.assertEqual(stopped['cleanup'], 'verified')
            self.assertEqual(stopped['final_file'], published['final_file'])
            self.assertEqual(stopped['result_file'], published['result_file'])
            self.assertEqual(self.result(root), result)
            self.until(lambda: not process_alive(pid))
        finally:
            if process_alive(pid):
                os.kill(pid, signal.SIGCONT)

    def test_stop_cleans_descendant_after_bridge_crash(self):
        for new_session in (False, True):
            with self.subTest(new_session=new_session):
                if new_session:
                    self.use_real_manager()
                self.check_scope_cleanup(new_session)

    def use_real_manager(self):
        if not (shutil.which('systemd-run') and shutil.which('systemctl')):
            self.skipTest('systemd user manager tools are unavailable')
        try:
            manager = subprocess.run(['systemctl', '--user', 'show', '--property=Version', '--value'],
                                     capture_output=True, timeout=3)
        except (OSError, subprocess.TimeoutExpired):
            self.skipTest('systemd user manager is unavailable')
        if manager.returncode != 0 or not manager.stdout.strip():
            self.skipTest('systemd user manager is unavailable')
        for command in ('systemd-run', 'systemctl'):
            (self.bin / command).unlink()

    def check_scope_cleanup(self, new_session):
        marker = self.root / f'descendant-pid-{new_session}'
        options = ['--env', 'BRIDGE_TEST_NEW_SESSION=1'] if new_session else []
        root = self.start('codex', 'pending', '--env', 'BRIDGE_TEST_DESCENDANT=' + str(marker), *options)
        self.approval(root)
        old = self.state(root)
        os.kill(old['pid'], signal.SIGKILL)
        self.until(lambda: marker.read_text().strip() if marker.exists() else None)
        pid = int(marker.read_text())

        def clean_up_descendant():
            if process_alive(pid):
                os.kill(pid, signal.SIGKILL)

        self.addCleanup(clean_up_descendant)
        self.assertTrue(process_alive(pid))
        self.assertEqual((Path('/proc') / str(pid) / 'environ').read_bytes(), b'')
        if new_session:
            self.assertEqual(os.getsid(pid), pid)
            self.assertIn(old['scope'], (Path('/proc') / str(pid) / 'cgroup').read_text())
        self.cli('stop', root)
        self.until(lambda: not process_alive(pid))
        self.assertEqual(self.state(root)['cleanup'], 'verified')

    def test_resume_after_bridge_crash_retires_pending_worker(self):
        root = self.start('claude', 'pending')
        self.approval(root)
        old = self.state(root)
        os.kill(old['pid'], signal.SIGKILL)
        self.until(lambda: json.loads(self.cli('status', root).stdout)['status'] == 'failed')
        task = self.root / 'recovery.md'
        task.write_text('recovered')
        self.cli('resume', root, '--task', str(task))
        self.assertEqual(self.done(root), 'resumed:recovered')
        self.assertEqual(list((self.turn(root) / 'approvals').iterdir()), [])
        self.assertFalse((self.scopes / old['scope']).exists())

    def test_stop_and_resume_refuse_unverified_cleanup(self):
        root = self.start('codex', 'pending')
        self.approval(root)
        old = self.state(root)
        os.kill(old['pid'], signal.SIGKILL)
        for failure in ('offline', 'busy'):
            with self.subTest(failure=failure):
                fault = self.scopes / failure
                fault.touch()
                try:
                    failed = self.cli('stop', root, success=False)
                    self.assertIn('Cleanup unverified', failed.stderr)
                    self.assertEqual(json.loads(failed.stdout)['cleanup'], 'unverified')
                    self.cli('resume', root, success=False)
                    self.assertEqual(self.state(root)['generation'], old['generation'])
                finally:
                    fault.unlink()
        self.cli('stop', root)
        self.assertEqual(self.state(root)['cleanup'], 'verified')


if __name__ == '__main__':
    unittest.main()
