#!/usr/bin/env python3
"""Host-free bridge checks with native-shaped fake stdio agents."""

import json
import os
from pathlib import Path
import subprocess
import signal
import sys
import tempfile
import time
import unittest


BRIDGE = Path(__file__).resolve().parents[1] / 'core/subyard-agent-bridge/bridge.py'
FAKE = r'''#!/usr/bin/env python3
import json
import os
from pathlib import Path
import sys

codex = Path(sys.argv[0]).name == 'codex'
session = 'fixture-session'
resumed = any(arg == '--resume=' + session for arg in sys.argv)
scenario = ''
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
              'usage': {'input_tokens': 3, 'output_tokens': 2}})

def begin(text):
    global scenario
    scenario = text.split('\n')[-1]
    if codex:
        emit({'method': 'item/started', 'params': {'item': {
            'id': 'tool-call', 'type': 'commandExecution', 'status': 'inProgress'}}})
    else:
        emit({'type': 'assistant', 'message': {'content': [{'type': 'tool_use', 'name': 'Bash'}]}})
    if scenario == 'eof':
        sys.exit(7)
    if scenario == 'malformed':
        print('not json', flush=True)
        return
    if scenario in ('approval', 'pending', 'file-approval', 'unsupported', 'duplicate'):
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
    elif scenario == 'result-error':
        if codex:
            emit({'method': 'turn/completed', 'params': {'turn': {'status': 'failed'}}})
        else:
            emit({'type': 'result', 'subtype': 'error_during_execution', 'is_error': True})
    elif scenario != 'message':
        finish(('resumed:' if resumed else 'started:') + text)

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
            emit({'id': message['id'], 'result': {'thread': {'id': session}}})
        elif method == 'turn/start':
            text = message['params']['input'][0]['text']
            if text == 'rpc-error':
                emit({'id': message['id'], 'error': {'message': 'fixture error'}})
            else:
                emit({'id': message['id'], 'result': {'turn': {'id': 'fixture-turn'}}})
                begin(text)
        elif method == 'turn/steer':
            assert message['params']['expectedTurnId'] == 'fixture-turn'
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
'''


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
        self.env = {**os.environ, 'PATH': str(self.bin) + os.pathsep + os.environ['PATH']}
        self.mask = -1
        self.workers = []
        self.addCleanup(self.cleanup)

    def cleanup(self):
        for root in self.workers:
            try:
                state = json.loads((root / 'state').read_text())
                if state['status'] == 'running':
                    self.cli('interrupt', root)
                    self.until(lambda: self.state(root)['status'] != 'running')
            except (OSError, AssertionError):
                pass
        self.temp.cleanup()

    def cli(self, command, root, *args, success=True):
        result = subprocess.run([sys.executable, str(BRIDGE), command, '--dir', str(root), *args],
                                env=self.env, text=True, capture_output=True, timeout=8, umask=self.mask)
        if success:
            self.assertEqual(result.returncode, 0, result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0)
        return result

    def start(self, vendor, task, *args, preset=False):
        root = self.root / str(len(self.workers))
        root.mkdir(mode=0o700)
        root.chmod(0o700)
        (root / 'task.md').write_text(task)
        self.workers.append(root)
        model = [] if preset else ['--model', 'fixture', '--effort', 'low']
        self.cli('start', root, '--vendor', vendor, *model,
                 '--cwd', str(self.root), '--executable', str(self.bin / vendor), *args)
        return root

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
        self.until(lambda: self.state(root)['status'] != 'running')
        self.assertEqual(self.state(root)['status'], status, (root / 'events.jsonl').read_text())
        return (root / 'final.md').read_text().strip() if status == 'done' else None

    def approval(self, root):
        names = self.until(lambda: self.state(root)['pending_approvals'])
        path = root / 'approvals' / (names[0] + '.json')
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
                self.assertEqual(list((root / 'approvals').iterdir()), [])
                self.assertEqual(self.state(root)['pending_approvals'], [])
                self.cli('resume', root)
                second = self.approval(root)
                self.assertNotEqual(first, second)
                self.assertEqual(self.state(root)['status'], 'running')
                self.reply(second, 'deny')
                self.assertEqual(self.done(root), 'deny')
                self.assertEqual(list((root / 'approvals').iterdir()), [])

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
                self.assertFalse((root / 'final.md').exists())
                self.cli('interrupt', root)
                self.done(root, 'interrupted')
                self.assertEqual(list((root / 'approvals').iterdir()), [])
                (root / 'task.md').write_text('after-interrupt')
                self.cli('resume', root)
                self.assertEqual(self.done(root), 'resumed:after-interrupt')

    def test_message_resume_and_invalid_commands(self):
        for vendor in ('codex', 'claude'):
            with self.subTest(vendor=vendor):
                root = self.start(vendor, 'message')
                self.until(lambda: self.state(root).get('session_id'))
                self.cli('resume', root, success=False)
                self.cli('start', root, success=False)
                self.cli('send', root, '--message', 'hello')
                self.assertEqual(self.done(root), 'message:hello')
                self.assertEqual(list((root / 'inbox').iterdir()), [])
                self.cli('send', root, '--message', 'late')
                self.assertEqual(self.done(root), 'resumed:late')
                (root / 'task.md').write_text('next-task')
                self.cli('resume', root)
                self.assertEqual(self.done(root), 'resumed:next-task')
                self.assertEqual(self.state(root)['session_id'], 'fixture-session')

    def test_failures_never_publish_a_successful_final(self):
        for vendor in ('codex', 'claude'):
            for scenario in ('eof', 'malformed', 'result-error', 'unsupported', 'duplicate'):
                with self.subTest(vendor=vendor, scenario=scenario):
                    root = self.start(vendor, scenario)
                    self.done(root, 'failed')
                    self.assertFalse((root / 'final.md').exists())
                    self.assertEqual(list((root / 'approvals').iterdir()), [])
        self.done(self.start('codex', 'rpc-error'), 'failed')

    def test_invalid_and_unsolicited_replies_fail_closed(self):
        for vendor in ('codex', 'claude'):
            for reply in ('allow-for-session', 'unsolicited'):
                with self.subTest(vendor=vendor, reply=reply):
                    root = self.start(vendor, 'approval')
                    path = self.approval(root)
                    target = path.with_suffix('.reply') if reply != 'unsolicited' else path.parent / '999999.reply'
                    self.reply(target.with_suffix('.json'), reply)
                    self.done(root, 'failed')
                    self.assertFalse((root / 'final.md').exists())

    def test_codex_file_approval_and_directory_permissions(self):
        root = self.start('codex', 'file-approval')
        path = self.approval(root)
        self.assertEqual(json.loads(path.read_text())['item']['changes'][0]['path'], 'fixture.txt')
        self.reply(path, 'allow')
        self.assertEqual(self.done(root), 'allow')
        for name in ('state', 'config.json', 'events.jsonl', 'final.md', 'bridge.log'):
            self.assertEqual((root / name).stat().st_mode & 0o777, 0o600)
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
        self.cli('resume', root, success=False)

    def test_exact_modes_under_restrictive_umask(self):
        self.mask = 0o777
        root = self.start('claude', 'approval')
        self.reply(self.approval(root), 'allow')
        self.assertEqual(self.done(root), 'allow')
        for name in ('state', 'config.json', 'events.jsonl', 'final.md', 'bridge.log',
                     'command.lock', 'worker.lock'):
            self.assertEqual((root / name).stat().st_mode & 0o777, 0o600)
        for path in (root, root / 'inbox', root / 'approvals'):
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
                self.assertEqual(argv[offset:offset + len(vendor_args)], vendor_args)
                self.assertEqual(info['cwd'], str(self.root))
                self.assertEqual(info['env'], {'BRIDGE_TEST_VALUE': 'fixture-value',
                                              'TMPDIR': str(scratch), 'PATH': fixture_path})
                self.cli('send', root, '--message', 'next')
                self.assertEqual(self.done(root), 'resumed:next')
                self.assertEqual(json.loads(capture.read_text())['env'], info['env'])

    def test_role_defaults_overrides_and_reviewer_temporary_writes(self):
        scratch = self.root.parent / (self.root.name + '-scratch')
        scratch.mkdir()
        self.addCleanup(scratch.rmdir)
        for vendor in ('codex', 'claude'):
            for role in ('developer', 'reviewer', 'observer'):
                with self.subTest(vendor=vendor, role=role):
                    capture = self.root / 'role-capture.json'
                    # Reviewer TMPDIR must be outside its read-only repository root.
                    root = self.start(vendor, 'complete', '--role', role, '--env', 'TMPDIR=' + str(scratch),
                                      '--env', 'BRIDGE_TEST_CAPTURE=' + str(capture), preset=True)
                    self.done(root)
                    config = json.loads((root / 'config.json').read_text())
                    self.assertEqual(config['effort'], 'low' if role == 'observer' else 'high')
                    if role == 'observer':
                        self.assertEqual(config['model'], 'gpt-6-luna' if vendor == 'codex' else 'haiku')
                    if role == 'reviewer':
                        argv = json.loads(capture.read_text())['argv']
                        if vendor == 'codex':
                            filesystem = next(arg for arg in argv if arg.startswith('permissions.bridge-reviewer.filesystem='))
                            import tomllib
                            permissions = tomllib.loads(filesystem)['permissions']['bridge-reviewer']['filesystem']
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
                    self.done(override)
                    self.assertEqual(json.loads((override / 'config.json').read_text())['model'], 'fixture')

    def test_wait_exit_codes_result_and_tail(self):
        for vendor in ('codex', 'claude'):
            with self.subTest(vendor=vendor):
                root = self.start(vendor, 'message')
                timed = self.cli('wait', root, '--timeout', '0.1', success=False)
                self.assertEqual(timed.returncode, 124)
                self.cli('send', root, '--message', 'finished')
                self.cli('wait', root, '--timeout', '5')
                result = json.loads((root / 'result.json').read_text())
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
                self.assertEqual(json.loads((failed / 'result.json').read_text())['exit_code'], 1)
                pending = self.start(vendor, 'pending')
                self.approval(pending)
                self.assertEqual(self.cli('wait', pending, success=False).returncode, 2)
                self.cli('stop', pending, '--timeout', '5')
                self.assertEqual(self.cli('wait', pending, success=False).returncode, 130)
                self.assertEqual(json.loads((pending / 'result.json').read_text())['exit_code'], 130)
                self.cli('stop', pending)

    def test_approval_commands_and_independent_named_workers(self):
        roots = [self.start(vendor, 'approval', '--name', vendor + '-worker') for vendor in ('codex', 'claude')]
        for root in roots:
            self.approval(root)
        result = subprocess.run([sys.executable, str(BRIDGE), 'list', '--root', str(self.root)],
                                capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual({item['name'] for item in json.loads(result.stdout)}, {'codex-worker', 'claude-worker'})
        for root, command in zip(roots, ('approve', 'deny')):
            requests = json.loads(self.cli('approvals', root).stdout)
            self.assertEqual(len(requests), 1)
            name = requests[0]['id']
            self.cli(command, root, name)
            self.assertEqual(self.done(root), 'allow' if command == 'approve' else 'deny')
            self.cli(command, root, name, success=False)
            self.assertEqual(json.loads(self.cli('approvals', root).stdout), [])

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

    def test_bridge_crash_is_failed_and_stop_cleans_worker(self):
        root = self.start('codex', 'pending')
        self.approval(root)
        state = self.state(root)
        os.kill(state['pid'], signal.SIGKILL)
        self.until(lambda: json.loads(self.cli('status', root).stdout)['status'] == 'failed')
        self.assertEqual(self.cli('wait', root, success=False).returncode, 1)
        self.cli('stop', root)
        self.cli('stop', root)
        self.assertEqual(json.loads((root / 'result.json').read_text())['state'], 'failed')
        self.assertEqual(list((root / 'approvals').iterdir()), [])

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
        self.assertEqual(list((root / 'approvals').iterdir()), [])
        def retired():
            try:
                return Path(f'/proc/{old["worker_pid"]}/stat').read_text().rsplit(') ', 1)[1].split()[0] == 'Z'
            except FileNotFoundError:
                return True
        self.until(retired)


if __name__ == '__main__':
    unittest.main()
