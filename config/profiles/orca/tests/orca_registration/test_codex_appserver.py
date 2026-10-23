import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from contextlib import contextmanager

import support


class CodexAppserverTests(unittest.TestCase):
    @contextmanager
    def approval_adapter(self, ordering=False):
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            native = Path(temporary) / 'native'
            native.write_text('#!' + sys.executable + '\n' + '''
import json, os, sys, time
for line in sys.stdin:
    message = json.loads(line)
    if message.get('method') == 'config/read' and os.environ['FIXTURE_ORDERING'] == '1':
        time.sleep(0.2)
        print(json.dumps({'id': 'future', 'method': 'item/commandExecution/requestApproval',
                          'params': {'availableDecisions': ['accept', 'cancel']}}), flush=True)
        time.sleep(0.2)
        result = {'id': message['id'], 'result': {'config': {
            'approval_policy': 'on-request', 'approvals_reviewer': 'user',
            'sandbox_mode': 'danger-full-access'}}}
    elif message.get('method') == 'fixture/approval':
        result = {'id': message['id'], 'method': 'item/commandExecution/requestApproval',
                  'params': {'command': 'git commit', 'availableDecisions':
                    ['accept', 'acceptForSession', {'acceptWithExecpolicyAmendment': {}}, 'decline', 'cancel']}}
    else:
        result = {'method': 'fixture/received', 'params': message}
    print(json.dumps(result), flush=True)
''')
            native.chmod(0o755)
            child = subprocess.Popen([sys.executable, '-B', str(support.COMPONENT / 'codex_appserver.py'),
                                      str(native), 'app-server'], stdin=subprocess.PIPE,
                                     stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
                                     env={**os.environ, 'ORCA_AGENT_SESSION_SPAWN_TOKEN': 'fixture',
                                          'FIXTURE_ORDERING': str(int(ordering))})
            try:
                yield child
            finally:
                if child.poll() is None:
                    child.terminate()
                child.communicate(timeout=5)

    def approval_send(self, child, message):
        child.stdin.write(json.dumps(message) + '\n')
        child.stdin.flush()

    def approval_request(self, child, request_id):
        self.approval_send(child, {'id': request_id, 'method': 'fixture/approval'})
        return json.loads(child.stdout.readline())

    def test_only_single_use_approval_choices_reach_client_and_native_server(self):
        for decision in ('accept', 'decline', 'cancel', 'acceptForSession',
                         {'acceptWithExecpolicyAmendment': {'execpolicy_amendment': ['git']}}):
            with self.subTest(decision=decision), self.approval_adapter() as child:
                for request_id in ('first', 'repeat'):
                    request = self.approval_request(child, request_id)
                    self.assertEqual(['accept', 'decline', 'cancel'], request['params']['availableDecisions'])
                    self.approval_send(child, {'id': request_id, 'result': {'decision': decision}})
                    received = json.loads(child.stdout.readline())['params']
                    self.assertEqual(decision if isinstance(decision, str) and decision in
                                     ('accept', 'decline', 'cancel') else 'cancel', received['result']['decision'])

    def test_replayed_response_and_reused_native_id_fail_closed(self):
        for replay in ('response', 'request'):
            with self.subTest(replay=replay), self.approval_adapter() as child:
                self.approval_request(child, 'once')
                response = {'id': 'once', 'result': {'decision': 'accept'}}
                self.approval_send(child, response)
                child.stdout.readline()
                self.approval_send(child, response if replay == 'response' else
                                   {'id': 'once', 'method': 'fixture/approval'})
                self.assertEqual(1, child.wait(timeout=5))
                self.assertEqual('', child.stdout.read())

    def test_response_before_native_request_is_rejected_before_config_queueing(self):
        with self.approval_adapter(ordering=True) as child:
            # Keep stdin open: EOF cancellation must not hide the ingress-ordering bug.
            child.stdin.write(json.dumps({'id': 'start', 'method': 'thread/start',
                                          'params': {}}) + '\n' + json.dumps(
                {'id': 'future', 'result': {'decision': 'accept'}}) + '\n')
            child.stdin.flush()
            self.assertEqual(1, child.wait(timeout=5))
            self.assertEqual('', child.stdout.read())

    def test_disconnect_cancels_pending_approval_and_stops_native_server(self):
        with self.approval_adapter() as child:
            self.approval_request(child, 'pending')
            child.stdin.close()
            child.stdin = None
            self.assertEqual(1, child.wait(timeout=5))

    def run_adapter(self, requests, config=None, error=False, arguments=None, scoped=True, mode='normal'):
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            root = Path(temporary)
            native = root / 'native'
            native.write_text('#!' + sys.executable + '\n' + '''
import json, os, sys, time
if sys.argv[1:] != ['app-server']:
    print(json.dumps({'args': sys.argv[1:], 'input': sys.stdin.read()}))
    raise SystemExit(17)
for line in sys.stdin:
    message = json.loads(line)
    if message.get('method') == 'config/read':
        if os.environ['FIXTURE_MODE'] == 'eof':
            raise SystemExit(3)
        if os.environ['FIXTURE_MODE'] == 'timeout':
            time.sleep(6)
        result = {'id': message['id'], 'error': {'code': -1, 'message': 'private config failure'}} if os.environ['FIXTURE_ERROR'] == '1' else {'id': message['id'], 'result': {'config': json.loads(os.environ['FIXTURE_CONFIG'])}}
    else:
        result = {'id': message.get('id'), 'result': {'received': message}}
    print(json.dumps(result), flush=True)
''')
            native.chmod(0o755)
            environment = {**os.environ, 'FIXTURE_CONFIG': json.dumps(config or {
                'approval_policy': 'on-request', 'sandbox_mode': 'danger-full-access',
                'approvals_reviewer': 'user'}), 'FIXTURE_ERROR': str(int(error)),
                'FIXTURE_MODE': mode}
            environment.pop('ORCA_AGENT_SESSION_SPAWN_TOKEN', None)
            if scoped:
                environment['ORCA_AGENT_SESSION_SPAWN_TOKEN'] = 'fixture-token'
            result = subprocess.run([sys.executable, '-B', str(support.COMPONENT / 'codex_appserver.py'),
                                     str(native), *(arguments or ['app-server'])],
                                    input=''.join(json.dumps(r) + '\n' for r in requests),
                                    env=environment, capture_output=True, text=True, timeout=10)
            return result, [json.loads(line) for line in result.stdout.splitlines()]

    def test_new_and_resumed_chats_use_effective_config_without_leaking_internal_responses(self):
        requests = [{'id': index, 'method': method, 'params': {
            'cwd': '/tmp/workspace', 'approvalPolicy': 'never', 'sandbox': 'workspace-write',
            'approvalsReviewer': 'guardian_subagent', 'model': 'fixture-model'}}
            for index, method in enumerate(['thread/start', 'thread/resume'], 1)]
        result, responses = self.run_adapter(requests)
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertEqual([1, 2], [r['id'] for r in responses])
        for response in responses:
            params = response['result']['received']['params']
            self.assertEqual('danger-full-access', params['sandbox'])
            self.assertEqual('on-request', params['approvalPolicy'])
            self.assertEqual('user', params['approvalsReviewer'])
            self.assertEqual('fixture-model', params['model'])

    def test_stricter_config_is_preserved_and_turn_overrides_do_not_reset_thread(self):
        result, responses = self.run_adapter([
            {'id': 1, 'method': 'thread/start', 'params': {'cwd': '/tmp/workspace'}},
            {'id': 2, 'method': 'turn/start', 'params': {'threadId': 'thread', 'input': [],
             'sandboxPolicy': {'type': 'dangerFullAccess'}, 'approvalPolicy': 'never'}}],
            config={'approval_policy': 'on-request', 'sandbox_mode': 'read-only',
                    'approvals_reviewer': 'user'})
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertEqual('read-only', responses[0]['result']['received']['params']['sandbox'])
        self.assertEqual({'threadId': 'thread', 'input': []},
                         responses[1]['result']['received']['params'])

    def test_native_config_failure_refuses_thread_and_does_not_print_private_details(self):
        result, responses = self.run_adapter([
            {'id': 1, 'method': 'thread/resume', 'params': {'cwd': '/tmp/workspace'}}], error=True)
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn('error', responses[0])
        self.assertNotIn('private config failure', result.stdout + result.stderr)

    def test_cli_probes_and_non_chat_appservers_pass_through(self):
        result, responses = self.run_adapter([], arguments=['--version'])
        self.assertEqual(17, result.returncode)
        self.assertEqual({'args': ['--version'], 'input': ''}, responses[0])
        request = {'id': 1, 'method': 'thread/start', 'params': {'sandbox': 'workspace-write'}}
        result, responses = self.run_adapter([request], scoped=False)
        self.assertEqual(0, result.returncode)
        self.assertEqual(request, responses[0]['result']['received'])

    def test_config_timeout_and_native_eof_refuse_the_pending_thread(self):
        request = {'id': 'subyard-config-client', 'method': 'thread/start', 'params': {}}
        for mode, status in [('eof', 3), ('timeout', 0)]:
            with self.subTest(mode=mode):
                result, responses = self.run_adapter([request], mode=mode)
                self.assertEqual(status, result.returncode, result.stderr)
                self.assertEqual(1, len(responses))
                self.assertEqual(request['id'], responses[0]['id'])
                self.assertIn('error', responses[0])

    def test_unknown_permissions_refuse_the_thread_without_a_fallback(self):
        result, responses = self.run_adapter([
            {'id': 1, 'method': 'thread/start', 'params': {}}],
            config={'approval_policy': 'never', 'sandbox_mode': 'read-only',
                    'approvals_reviewer': 'guardian_subagent'})
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn('error', responses[0])
