"""File-based cross-vendor workers using native stdio protocols."""

import argparse
from collections import deque
from contextlib import nullcontext, suppress
from datetime import UTC, datetime
import fcntl
import json
import math
import os
from pathlib import Path
import re
import selectors
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import time
import uuid


EXIT_CODES = {'done': 0, 'failed': 1, 'interrupted': 130}
ACTIVE_STATES = ('launching', 'running')
RESUME_COMMAND = 'resume --dir DIR --task FILE'
CLEANUP_UNVERIFIED = 'Cleanup unverified. Could not verify that the systemd user scope is empty or gone.'
CODEX_SANDBOXES = {'read-only': 'readOnly', 'workspace-write': 'workspaceWrite',
                   'danger-full-access': 'dangerFullAccess'}
MODELS = {'codex': {'developer': 'gpt-6.1-sol', 'reviewer': 'gpt-6.1-sol', 'observer': 'gpt-6-luna'},
          'claude': {'developer': 'opus', 'reviewer': 'opus', 'observer': 'haiku'}}
STANDALONE = ('You are a standalone worker with no parent agent. Put your full report in your final '
              'message. Do not send it to a parent.\n\n')
SECRET_NAME = r'token|secret|password|passwd|passphrase|credentials?|cookie|api[_-]?key|private[_-]?key|access[_-]?key|signing[_-]?key'
SECRET_ENV = re.compile(rf'(?:^|_)(?:{SECRET_NAME})(?:_|$)', re.IGNORECASE)
SECRET_HEADER = re.compile(r'(?i)(\b(?:(?:proxy-)?authorization[ \t]*[:=]|(?:set-)?cookie[ \t]*:))[^\r\n]*')
SECRET_TEXT = re.compile(
    rf'(?i)(\b(?:[a-z0-9]+[_-])*(?:{SECRET_NAME}|authorization)(?:[_-][a-z0-9]+)*["\x27]?\s*[:=]\s*)'
    r'(?:(?:basic|bearer)\s+)?(?:"(?:\\[^\r\n]|[^"\\\r\n])*"|\x27(?:\\[^\r\n]|[^\x27\\\r\n])*\x27|[^\s,;]+)'
    r'|\bbearer [\w.+/=-]+|\b(?:gh[pousr]_|github_pat_|sk-)[\w-]{16,}'
    r'|\beyJ[\w-]+\.eyJ[\w-]+\.[\w-]+')
PRIVATE_KEY = re.compile(r'-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|\Z)',
                         re.DOTALL)


class BridgeError(ValueError):
    """A bridge-authored diagnostic safe to display."""


def event_redactor(env):
    values = {value for name, value in env.items() if SECRET_ENV.search(name)
              and not name.upper().endswith(('_FILE', '_PATH', '_DIR', '_SOCK')) and len(value) >= 8}
    exact = re.compile('|'.join(re.escape(value) for value in sorted(values, key=len, reverse=True))) if values else None

    def redact(text):
        text = PRIVATE_KEY.sub('[redacted]', text)
        text = SECRET_HEADER.sub(r'\1 [redacted]', text)
        if exact:
            text = exact.sub('[redacted]', text)
        return SECRET_TEXT.sub(lambda match: (match[1] or '') + '[redacted]', text)

    return redact


def codex_arguments(args):
    arguments = []
    sandbox = None
    args = iter(args)
    for arg in args:
        option, equals, value = arg.partition('=')
        if arg.startswith(('-s', '-c')) and not arg.startswith('--') and len(option) > 2:
            option, equals, value = arg[:2], '=', arg[2:]
        if option in ('-s', '--sandbox'):
            mode = value if equals else next(args, '')
            sandbox = CODEX_SANDBOXES[mode]
            arguments.extend(['-c', 'sandbox_mode=' + json.dumps(mode)])
        elif option in ('-c', '--config'):
            config = value if equals else next(args, '')
            arguments.extend(['-c', config])
            key, _, setting = config.partition('=')
            if key.strip().strip('"\'') == 'sandbox_mode':
                sandbox = CODEX_SANDBOXES[setting.strip().strip('"\'')]
        else:
            arguments.append(arg)
    return arguments, sandbox


def reviewer_scratch(env):
    return str(Path(env.get('TMPDIR', '/tmp')).resolve())


def role_arguments(config, env):
    if config.get('role') != 'reviewer':
        return []
    cwd = config['cwd']
    scratch = reviewer_scratch(env)
    if cwd in ('/tmp', scratch) or Path(cwd) in Path('/tmp').parents or Path(cwd) in Path(scratch).parents:
        raise BridgeError('reviewer cwd must be distinct from temporary roots')
    if config['vendor'] == 'codex':
        filesystem = {'/tmp': 'write', scratch: 'write', cwd: 'read'}
        return ['-c', 'default_permissions="bridge-reviewer"',
                '-c', 'permissions.bridge-reviewer={extends=":read-only",filesystem={' + ','.join(
                    json.dumps(path) + '=' + json.dumps(mode) for path, mode in filesystem.items()) + '}}']
    # Bash is sandboxed; file tools use explicit deny rules for repository writes.
    settings = {'sandbox': {'enabled': True, 'failIfUnavailable': True,
                           'allowUnsandboxedCommands': False, 'excludedCommands': [],
                           'autoAllowBashIfSandboxed': False,
                           'filesystem': {'denyWrite': [cwd], 'allowWrite': ['/tmp', scratch]}},
                'permissions': {'deny': ['Edit(/' + cwd + '/**)']}}
    return ['--settings', json.dumps(settings)]


def add_usage(totals, usage):
    for key, value in usage.items():
        if type(value) is int:
            totals[key] = totals.get(key, 0) + value
    return totals


def private_open(path, flags, mode, *, require_private=True):
    fd = os.open(path, flags | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600)
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                or (require_private and (info.st_mode & 0o077 or info.st_nlink != 1))):
            raise BridgeError('unsafe state file')
        return os.fdopen(fd, mode)
    except BaseException:
        os.close(fd)
        raise


def read(path):
    # Assignment and reply files can be supplied by the caller.
    with private_open(path, os.O_RDONLY, 'r', require_private=False) as stream:
        return stream.read()


def write(path, text):
    fd, temporary = tempfile.mkstemp(prefix='.bridge-', dir=path.parent)
    try:
        with os.fdopen(fd, 'w') as stream:
            os.fchmod(stream.fileno(), 0o600)
            stream.write(text)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def directory(path, *, create=True):
    if create:
        try:
            path.mkdir(mode=0o700)
            path.chmod(0o700)
        except FileExistsError:
            pass
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
        raise BridgeError('state directories must be owned by you with mode 0700')


def locked(path, blocking=True):
    stream = private_open(path, os.O_CREAT | os.O_RDWR, 'w')
    try:
        os.fchmod(stream.fileno(), 0o600)
        fcntl.flock(stream, fcntl.LOCK_EX if blocking else fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError:
        stream.close()
        raise
    return stream


def lock_until(path, deadline):
    while True:
        try:
            return locked(path, blocking=False)
        except BlockingIOError:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return None
            time.sleep(min(0.02, remaining))


def claim_turn(root, generation):
    while True:
        state = json.loads(read(root / 'state'))
        if state['status'] != 'launching' or state['generation'] != generation:
            return None
        try:
            return locked(root / 'worker.lock', blocking=False)
        except BlockingIOError:
            time.sleep(0.02)


def append(path):
    return private_open(path, os.O_WRONLY | os.O_APPEND, 'a')


def record_event(root, kind, **fields):
    timestamp = datetime.now(UTC).isoformat(timespec='milliseconds')
    with append(root / 'events.jsonl') as stream:
        stream.write(json.dumps({'type': kind, 'timestamp': timestamp, **fields}) + '\n')


def turn_dir(root, state):
    generation = state['generation']
    if not re.fullmatch(r'[0-9a-f]{32}', generation):
        raise BridgeError('invalid turn generation')
    return root / 'turns' / generation


def save_state(root, state):
    write(root / 'state', json.dumps(state) + '\n')


def publish_result(root, state, model, tokens=None, duration=None, answers=()):
    turn = turn_dir(root, state)
    result = {'state': state['status'], 'exit_code': EXIT_CODES[state['status']],
              'session_id': state.get('session_id'), 'vendor': state['vendor'],
              'model': model, 'duration_seconds': duration, 'tokens': tokens}
    if state.get('error'):
        result['error'] = state['error']
    write(turn / 'result.json', json.dumps(result) + '\n')
    if state['status'] == 'done':
        write(turn / 'final.md', '\n\n'.join(answers) + '\n')
        state['final_file'] = str(turn / 'final.md')
    else:
        (turn / 'final.md').unlink(missing_ok=True)
    state['result_file'] = str(turn / 'result.json')
    save_state(root, state)


class Codex:
    def __init__(self, worker):
        self.w = worker
        self.requests = {}
        self.items = {}
        self.counter = 0
        self.sent = set()
        self.turn_error = None
        self.arguments, self.sandbox = codex_arguments(worker.config.get('vendor_args', []))
        # Pin the complete reviewer profile after native overrides.
        self.arguments.extend(role_arguments(worker.config, worker.env))

    def request(self, method, params, callback=lambda result: None):
        self.counter += 1
        self.requests[self.counter] = (method, callback)
        self.w.send({'id': self.counter, 'method': method, 'params': params})

    def start(self):
        self.request('initialize', {'clientInfo': {'name': 'subyard-agent-bridge', 'version': '1'}},
                     self.initialized)

    def initialized(self, result):
        self.w.send({'method': 'initialized'})
        params = {'cwd': self.w.config['cwd'], 'model': self.w.config['model']}
        method = 'thread/start'
        if self.w.state.get('session_id'):
            method = 'thread/resume'
            params['threadId'] = self.w.state['session_id']
        self.request(method, params, self.thread)

    def thread(self, result):
        sandbox = result.get('sandbox') or {}
        if self.w.config.get('role') == 'reviewer':
            profile = result.get('activePermissionProfile') or {}
            if (sandbox.get('type') != 'workspaceWrite'
                    or set(sandbox.get('writableRoots') or ()) != {'/tmp', reviewer_scratch(self.w.env)}
                    or profile.get('id') != 'bridge-reviewer' or profile.get('extends') != ':read-only'):
                raise BridgeError('Codex did not apply the reviewer permissions.')
        if self.sandbox and sandbox.get('type') != self.sandbox:
            raise BridgeError('Codex did not apply the requested sandbox.')
        self.w.model = result.get('model', self.w.model)
        session = result['thread']['id']
        if self.w.state.get('session_id') not in (None, session):
            raise BridgeError('Codex resumed a different session')
        self.w.state['session_id'] = session
        queued = [self.w.root / 'inbox' / name for name in self.w.state.pop('carryover', [])]
        self.w.save()
        self.request('turn/start', {'threadId': self.w.state['session_id'],
                     'input': [{'type': 'text', 'text': text} for text in
                               [self.w.task, *(read(path) for path in queued)]],
                     'effort': self.w.config['effort']}, lambda result: self.turn(result, queued))

    def turn(self, result, queued):
        for path in queued:
            path.unlink()
        self.w.state['turn_id'] = result['turn']['id']
        self.w.save()

    def steer(self):
        if not self.w.state.get('turn_id'):
            return
        for path in sorted((self.w.root / 'inbox').glob('*.md')):
            if path.name not in self.sent:
                self.sent.add(path.name)
                self.request('turn/steer', {'threadId': self.w.state['session_id'],
                             'expectedTurnId': self.w.state['turn_id'],
                             'input': [{'type': 'text', 'text': read(path)}]},
                             lambda result, path=path: path.unlink())

    def interrupt(self):
        if self.w.state.get('turn_id'):
            self.request('turn/interrupt', {'threadId': self.w.state['session_id'],
                         'turnId': self.w.state['turn_id']})

    def error_message(self, error):
        if not isinstance(error, dict):
            return None
        info = error.get('codexErrorInfo')
        if info == 'modelNotFound':
            fallback = 'Codex model was not found.'
        elif info == 'badRequest' or error.get('code') in (-32600, -32602):
            fallback = 'Codex rejected the request as invalid.'
        else:
            return None
        message = error.get('message')
        if isinstance(message, str):
            message = message.strip()
            # Provider responses can include raw bodies, endpoints, and headers.
            if (0 < len(message) <= 512 and message.isprintable()
                    and not re.search(r'[{}<>]|https?://', message, re.IGNORECASE)):
                return self.w.redact(message)
        return fallback

    def event(self, event):
        method = event.get('method')
        if method is None and 'id' in event:
            request, callback = self.requests.pop(event['id'])
            if 'error' in event:
                if request == 'turn/steer':
                    # The inbox file has not been acknowledged. Deliver it on the next turn.
                    self.w.record('message_deferred')
                    return
                raise BridgeError(self.error_message(event['error']) or 'Codex request failed')
            callback(event['result'])
        elif method in ('item/commandExecution/requestApproval', 'item/fileChange/requestApproval'):
            params = event['params']
            offered = params.get('availableDecisions', ['accept', 'decline', 'cancel'])
            if 'accept' not in offered or not any(d in offered for d in ('decline', 'cancel')):
                raise BridgeError('single-use approval unavailable')
            deny = 'decline' if 'decline' in offered else 'cancel'
            item = self.items.get(params.get('itemId'), {})
            self.w.approval(event, params.get('command') or item.get('command', ''), params.get('reason', ''),
                            method.split('/')[1], lambda allow, sent: self.w.send({
                                'id': event['id'], 'result': {'decision': 'accept' if allow else deny}}, sent), item)
        elif method in ('item/started', 'item/completed'):
            item = event['params']['item']
            if item['type'] == 'agentMessage' and method == 'item/completed':
                self.w.message(item['text'], stage=method)
            elif item['type'] in ('commandExecution', 'fileChange', 'mcpToolCall'):
                self.w.record('tool', tool=item['type'], status=item.get('status'), stage=method)
                if method == 'item/started':
                    self.items[item['id']] = item
                else:
                    self.items.pop(item.get('id'), None)
        elif method == 'error':
            params = event['params']
            if not params.get('willRetry') and (reason := self.error_message(params.get('error'))):
                self.turn_error = (params['turnId'], reason)
        elif method == 'turn/completed':
            turn = event['params']['turn']
            status = turn['status']
            if status == 'failed':
                reason = self.error_message(turn.get('error'))
                if not reason and self.turn_error and self.turn_error[0] == turn.get('id'):
                    reason = self.turn_error[1]
                self.w.state['error'] = reason or 'Codex turn failed.'
                self.w.record('error', message=self.w.state['error'], stage=method)
            self.w.outcome = {'completed': 'done', 'interrupted': 'interrupted', 'failed': 'failed'}[status]
        elif method == 'thread/tokenUsage/updated':
            self.w.tokens = event['params']['tokenUsage']
        elif 'id' in event and method:
            # Unknown approval/user-input protocols must never be silently approved.
            raise BridgeError('unsupported Codex server request')


class Claude:
    def __init__(self, worker):
        self.w = worker
        self.inflight = None
        self.answer_start = 0

    def start(self):
        self.user(self.w.task)

    def user(self, text):
        self.answer_start = len(self.w.answers)
        self.w.send({'type': 'user', 'message': {'role': 'user', 'content': [
            {'type': 'text', 'text': text}]}})

    def next_turn(self):
        self.inflight = next(iter(sorted((self.w.root / 'inbox').glob('*.md'))), None)
        if self.inflight:
            self.user(read(self.inflight))
            return True
        return False

    def interrupt(self):
        self.w.send({'type': 'control_request', 'request_id': 'interrupt-' + uuid.uuid4().hex,
                     'request': {'subtype': 'interrupt'}})

    def event(self, event):
        kind = event.get('type')
        if kind == 'control_request':
            request = event['request']
            if request['subtype'] != 'can_use_tool':
                raise BridgeError('unsupported Claude control request')
            inputs = request['input']
            self.w.approval(event, inputs.get('command', ''), request.get('decision_reason', ''),
                            request['tool_name'], lambda allow, sent: self.w.send({
                                'type': 'control_response', 'response': {
                                    'subtype': 'success', 'request_id': event['request_id'],
                                    'response': ({'behavior': 'allow', 'updatedInput': inputs} if allow else
                                                 {'behavior': 'deny', 'message': 'Operator denied this request.'})}}, sent))
        elif kind == 'system' and event.get('subtype') == 'init':
            self.w.model = event.get('model', self.w.model)
            session = event['session_id']
            if self.w.state.get('session_id') not in (None, session):
                raise BridgeError('Claude resumed a different session')
            self.w.state['session_id'] = session
            self.w.save()
        elif kind == 'assistant':
            for block in event['message']['content']:
                if block['type'] == 'text':
                    self.w.message(block['text'])
                elif block['type'] == 'tool_use':
                    self.w.record('tool', tool=block['name'])
        elif kind == 'result':
            if self.inflight:
                self.inflight.unlink()
                self.inflight = None
            if usage := event.get('usage'):
                self.w.tokens = add_usage(self.w.tokens or {}, usage)
            answer = event.get('result')
            answers = self.w.answers[self.answer_start:]
            if answer and (not answers or answer not in (answers[-1], '\n\n'.join(answers))):
                self.w.message(answer)
            self.w.outcome = 'failed' if event.get('is_error') or event.get('subtype') != 'success' else 'done'


class Worker:
    def __init__(self, root, state):
        self.root = root
        self.config = json.loads(read(root / 'config.json'))
        self.state = state
        self.turn = turn_dir(root, state)
        self.approvals = self.turn / 'approvals'
        self.task = read(self.turn / 'task.md')
        if self.config.get('role') == 'reviewer':
            self.task = 'Review read-only. Do not modify the repository. Write test/build outputs only in /tmp or TMPDIR.\n' + self.task
        elif self.config.get('role') == 'observer':
            self.task = 'Observe the assigned checks. Do not change repository sources.\n' + self.task
        self.task = STANDALONE + self.task
        self.env = {**os.environ, **self.config.get('env', {})}
        self.redact = event_redactor(self.env)
        self.pending = {}
        self.replying = set()
        self.outgoing = deque()
        self.approval_ids = set()
        self.outcome = None
        self.answers = []
        self.model = self.config['model']
        self.tokens = None
        self.interrupted = False
        self.adapter = Codex(self) if self.config['vendor'] == 'codex' else Claude(self)

    def save(self):
        self.state['pending_approvals'] = sorted(self.pending)
        save_state(self.root, self.state)

    def record(self, kind, **fields):
        fields = {key: self.redact(value) if isinstance(value, str) else value for key, value in fields.items()}
        record_event(self.root, kind, **fields)

    def message(self, text, **fields):
        self.answers.append(text)
        self.record('message', text=text, **fields)

    def send(self, event, sent=None):
        if not self.outgoing:
            self.selector.register(self.child.stdin, selectors.EVENT_WRITE)
        self.outgoing.append((memoryview((json.dumps(event) + '\n').encode()), sent))

    def flush(self):
        frame, sent = self.outgoing[0]
        try:
            count = os.write(self.child.stdin.fileno(), frame)
        except BlockingIOError:
            return
        if count < len(frame):
            self.outgoing[0] = (frame[count:], sent)
            return
        self.outgoing.popleft()
        if not self.outgoing:
            self.selector.unregister(self.child.stdin)
        if sent:
            sent()

    def approval(self, request, command, reason, tool, reply, item=None):
        request_id = request.get('request_id', request.get('id'))
        if not isinstance(request_id, (str, int)) or request_id in self.approval_ids:
            raise BridgeError('invalid or duplicate approval id')
        self.approval_ids.add(request_id)
        self.state['approval_sequence'] += 1
        # Persist the sequence before publication, so a crash cannot reuse a displayed ID.
        self.save()
        name = f'{self.state["approval_sequence"]:06d}'
        path = self.approvals / (name + '.json')
        # A reply must follow publication of this request, never preauthorize a future one.
        if os.path.lexists(path.with_suffix('.reply')):
            raise BridgeError('unsolicited approval reply')
        write(path, json.dumps({'vendor': self.config['vendor'], 'tool': tool,
                               'command': command, 'reason': reason, 'request': request,
                               'item': item}) + '\n')
        self.pending[name] = reply
        self.record('approval', id=name)
        self.save()

    def reply_sent(self, path, decision):
        name = path.stem
        path.with_suffix('.json').unlink()
        self.pending.pop(name)
        self.replying.remove(name)
        self.save()
        # Keep the reply visible until both the request file and pending ID are gone.
        path.unlink()
        self.record('approval_decision', id=name, decision=decision)

    def controls(self):
        if (self.turn / 'interrupt').exists():
            self.interrupted = True
        if self.interrupted:
            return
        for path in sorted(self.approvals.glob('*.reply')):
            name = path.stem
            if name in self.replying:
                continue
            if name not in self.pending:
                raise BridgeError('unsolicited approval reply')
            reply = self.pending[name]
            decision = read(path).strip()
            if decision not in ('allow', 'deny'):
                raise BridgeError('approval reply must be allow or deny')
            self.replying.add(name)
            reply(decision == 'allow', lambda path=path, decision=decision: self.reply_sent(path, decision))
        if isinstance(self.adapter, Codex):
            self.adapter.steer()

    def run(self):
        if (self.turn / 'interrupt').exists():
            self.outcome = 'interrupted'
            return
        env = self.env
        executable = self.config.get('executable')
        if not executable:
            installed = '/usr/local/bin/' + self.config['vendor']
            executable = installed if Path(installed).is_file() else shutil.which(self.config['vendor'], path=env.get('PATH'))
        if not executable:
            raise BridgeError('worker executable missing')
        if self.config['vendor'] == 'codex':
            argv = [executable, *self.adapter.arguments, 'app-server']
        else:
            extra = role_arguments(self.config, env) + self.config.get('vendor_args', [])
            argv = [executable, '-p', '--input-format', 'stream-json', '--output-format', 'stream-json',
                    '--verbose', '--model', self.config['model'], '--effort', self.config['effort'],
                    *extra, '--permission-prompt-tool', 'stdio']
            if self.state.get('session_id'):
                argv.append('--resume=' + self.state['session_id'])
        signal.signal(signal.SIGTERM, lambda *_: setattr(self, 'interrupted', True))
        signal.signal(signal.SIGINT, lambda *_: setattr(self, 'interrupted', True))
        self.child = subprocess.Popen(argv, cwd=self.config['cwd'], stdin=subprocess.PIPE,
                                      stdout=subprocess.PIPE, env=env, bufsize=0)
        self.selector = selectors.DefaultSelector()
        os.set_blocking(self.child.stdin.fileno(), False)
        self.selector.register(self.child.stdout, selectors.EVENT_READ)
        buffer = b''
        try:
            self.save()
            self.adapter.start()
            while not self.outcome:
                self.controls()
                if self.interrupted:
                    # An interrupt cannot overtake a partially sent JSON frame.
                    if not self.outgoing:
                        self.adapter.interrupt()
                        if self.outgoing:
                            self.flush()
                    self.outcome = 'interrupted'
                    break
                for key, events in self.selector.select(0.1):
                    if self.outcome:
                        break
                    if events == selectors.EVENT_WRITE:
                        self.flush()
                        continue
                    chunk = os.read(key.fileobj.fileno(), 65536)
                    if not chunk:
                        raise BridgeError('worker stream closed before completion')
                    buffer += chunk
                    while b'\n' in buffer and not self.outcome:
                        frame, buffer = buffer.split(b'\n', 1)
                        if len(frame) > 16 * 1024 * 1024:
                            raise BridgeError('worker frame too large')
                        self.adapter.event(json.loads(frame))
                    if len(buffer) > 16 * 1024 * 1024:
                        raise BridgeError('worker frame too large')
                if self.outcome == 'done':
                    if self.pending:
                        raise BridgeError('worker completed with unanswered approvals')
                    # Serialize the last inbox scan and terminal publication with send.
                    with locked(self.root / 'inbox.lock'):
                        if self.config['vendor'] == 'claude' and self.adapter.next_turn():
                            self.outcome = None
                        else:
                            self.finish()
        finally:
            self.selector.close()
            self.child.stdin.close()
            self.child.stdout.close()

    def finish(self):
        for name in self.pending:
            for suffix in ('.json', '.reply'):
                (self.approvals / (name + suffix)).unlink(missing_ok=True)
        self.state.update(status=self.outcome or 'failed', pending_approvals=[])
        publish_result(self.root, self.state, self.model, self.tokens,
                       round(time.time() - self.state['started_at'], 3), self.answers)
        self.record(self.state['status'])


def process_identity(pid):
    try:
        fields = Path(f'/proc/{pid}/stat').read_text().rsplit(') ', 1)[1].split()
        return None if fields[0] == 'Z' else fields[19]
    except (FileNotFoundError, ProcessLookupError):
        return None


def snapshot(root):
    path = root / 'state'
    while True:
        raw = read(path)
        state = json.loads(raw)
        if state['status'] not in ACTIVE_STATES:
            break
        pid, start = state.get('pid'), state.get('process_start')
        if pid and start and process_identity(pid) == start:
            break
        if read(path) == raw:
            state.update(status='failed', pending_approvals=[], cleanup='unverified',
                         error=state.get('error', 'Bridge process exited without a terminal state.'))
            break
    state['queued_messages'] = sorted(path.name for path in (root / 'inbox').glob('*.md'))
    return state


def unanswered_approvals(root, state):
    if state['pending_approvals']:
        approvals = turn_dir(root, state) / 'approvals'
        for name in state['pending_approvals']:
            request = approvals / (name + '.json')
            if not os.path.lexists(request.with_suffix('.reply')) and request.exists():
                yield request


def scope_command(*args):
    return subprocess.run(['systemctl', '--user', *args], stdin=subprocess.DEVNULL,
                          capture_output=True, text=True, timeout=10)


def scope_stopped(root, state):
    scope = state.get('scope')
    if not scope or scope != 'subyard-bridge-' + turn_dir(root, state).name + '.scope':
        return False
    try:
        scope_command('stop', scope)
        result = scope_command('show', '--property=LoadState,ActiveState,ControlGroup', scope)
        properties = dict(line.split('=', 1) for line in result.stdout.splitlines() if '=' in line)
        if result.returncode != 0 or properties.get('ActiveState') not in ('inactive', 'failed'):
            return False
        group = properties.get('ControlGroup')
        if group is None or properties.get('LoadState') not in ('loaded', 'not-found'):
            return False
        if not group:
            return True
        events = Path('/sys/fs/cgroup') / group.lstrip('/') / 'cgroup.events'
        try:
            return 'populated 0' in events.read_text().splitlines()
        except FileNotFoundError:
            return True
    except (OSError, subprocess.TimeoutExpired):
        return False


def publish_failure(root, state, deadline=None):
    path = root / 'inbox.lock'
    lock = locked(path) if deadline is None else lock_until(path, deadline)
    if lock is None:
        return False
    with lock:
        publish_result(root, state, state.get('model'))
    return True


def recover(root, state, deadline, stopped=False):
    if state.get('cleanup') == 'verified' or (not state.get('scope') and not state.get('error')):
        return True
    if state.get('scope'):
        # Publish the fence and failure result before contacting the manager. A
        # delayed _run can enter this scope but cannot start the vendor.
        if not state.get('result_file') and not publish_failure(root, state, deadline):
            print('Timed out waiting for the bridge inbox lock.', file=sys.stderr)
            return False
        if stopped or scope_stopped(root, state):
            for path in (turn_dir(root, state) / 'approvals').iterdir():
                if path.suffix in ('.json', '.reply'):
                    path.unlink(missing_ok=True)
            state['cleanup'] = 'verified'
            save_state(root, state)
            return True
    print(CLEANUP_UNVERIFIED, file=sys.stderr)
    return False


def launch(root, state, config, task, deadline):
    config.setdefault('name', root.name)
    lock = lock_until(root / 'worker.lock', deadline)
    if lock is None:
        print('Timed out waiting for the bridge worker lock; use stop to recover.', file=sys.stderr)
        return 124
    with lock:
        if not recover(root, state, deadline):
            return 1
        generation = uuid.uuid4().hex
        scope = 'subyard-bridge-' + generation + '.scope'
        state = dict(session_id=state['session_id'], approval_sequence=state['approval_sequence'],
                     status='launching', pid=os.getpid(), process_start=process_identity(os.getpid()),
                     turn_id=None, pending_approvals=[], generation=generation, scope=scope,
                     cleanup='unverified', vendor=config['vendor'], name=config['name'],
                     model=config['model'], started_at=time.time())
        directory(root / 'turns')
        turn = turn_dir(root, state)
        directory(turn)
        directory(turn / 'approvals')
        write(turn / 'task.md', task)
        write(root / 'task.md', task)
        write(root / 'config.json', json.dumps(config) + '\n')
        if config['vendor'] == 'codex':
            state['carryover'] = sorted(path.name for path in (root / 'inbox').glob('*.md'))
        for name in ('events.jsonl', 'bridge.log'):
            if not (root / name).exists():
                write(root / name, '')
        save_state(root, state)
        try:
            with append(root / 'bridge.log') as log:
                child = subprocess.Popen(['systemd-run', '--user', '--scope', '--quiet',
                                          '--unit=' + scope, '--property=TimeoutStopSec=2s', '--',
                                          sys.executable, str(Path(__file__).resolve()), '_run', '--dir', str(root),
                                          '--generation', generation],
                                         stdin=subprocess.DEVNULL, stdout=log, stderr=log, start_new_session=True)
        except (OSError, BridgeError) as error:
            reason = str(error) if isinstance(error, BridgeError) else 'Systemd user scope launcher could not start.'
            # Keep the refusal in the launching state until the inbox lock allows terminal publication.
            state['error'] = reason
            save_state(root, state)
            state['status'] = 'failed'
            published = publish_failure(root, state, deadline)
            if isinstance(error, BridgeError):
                with suppress(OSError, BridgeError):
                    record_event(root, 'error', message=reason)
            if not published:
                print('Launch failed and the bridge inbox lock timed out; use stop to recover.', file=sys.stderr)
                return 124
            raise
        state.update(pid=child.pid, process_start=process_identity(child.pid))
        save_state(root, state)
    print(json.dumps({'status': 'launching', 'pid': child.pid, 'dir': str(root)}))
    return 0


def wait(root, deadline, stopping=False):
    while True:
        state = snapshot(root)
        if state['status'] not in ACTIVE_STATES:
            return state, EXIT_CODES[state['status']]
        if not stopping:
            state['pending_approvals'] = [path.stem for path in unanswered_approvals(root, state)]
            if state['pending_approvals']:
                return state, 2
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            return state, 124
        time.sleep(min(0.1, remaining))


def stop(root, deadline):
    state = snapshot(root)
    if state['status'] in ACTIVE_STATES:
        write(turn_dir(root, state) / 'interrupt', '')
    state, _ = wait(root, deadline, stopping=True)
    lock_path = root / 'worker.lock'
    lock = lock_until(lock_path, deadline)
    forced = lock is None
    if forced:
        if not scope_stopped(root, state):
            print(CLEANUP_UNVERIFIED, file=sys.stderr)
            return snapshot(root), 1
        lock = lock_until(lock_path, deadline)
        if lock is None:
            print('Scope stopped, but worker state is still locked.', file=sys.stderr)
            return snapshot(root), 1
    with lock:
        state = snapshot(root)
        if state['status'] in ACTIVE_STATES:
            # A launcher may still be waiting to enter its scope. The worker
            # lock and terminal state fence it off from starting the vendor.
            state.update(status='interrupted', pending_approvals=[])
        recovered = recover(root, state, deadline, stopped=forced)
    return state, 0 if recovered else 1


def worker_view(state):
    if state['status'] == 'done' and state.get('cleanup') == 'unverified':
        return {**state, 'cleanup': 'pending_stop'}
    return state


def print_worker_table(workers):
    columns = ('name', 'vendor', 'status', 'cleanup', 'dir')
    rows = [tuple(column.upper() for column in columns)]
    for state in map(worker_view, workers):
        rows.append(tuple(str(state.get(column, '-')).replace('\n', r'\n').replace('\r', r'\r')
                          for column in columns))
    widths = [max(map(len, column)) for column in zip(*rows)]
    for row in rows:
        print('  '.join(cell.ljust(width) for cell, width in zip(row, widths)).rstrip())


def parse_args():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command', required=True, metavar='COMMAND')
    descriptions = {
        'start': 'Start a worker in a new state directory.',
        'resume': 'Continue a finished or stopped worker in the same session.',
        'send': 'Send a message to an active turn.',
        'interrupt': 'Request interruption of the active turn.',
        'stop': 'Interrupt the worker and verify scope cleanup.',
        'status': 'Show the current worker state.',
        'wait': 'Wait for completion or a pending approval.',
        'tail': 'Show recent messages, tool events, and errors.',
        'approvals': 'List pending approval requests.',
        'approve': 'Allow a pending request once.',
        'deny': 'Deny a pending request.',
        'list': 'List workers below a parent directory.',
    }
    for name, description in descriptions.items():
        command = commands.add_parser(name, help=description, description=description)
        if name == 'list':
            command.add_argument('--root', '--dir', type=Path, required=True, help='parent of worker directories')
            command.add_argument('--json', action='store_true', help='show full worker records as JSON')
        else:
            command.add_argument('--dir', type=Path, required=True, help='worker state directory')
        if name in ('approve', 'deny'):
            command.add_argument('id', help='pending approval ID')
        if name in ('start', 'resume'):
            command.add_argument('--task', type=Path, help='assignment file (defaults to task.md in the state directory)')
        if name not in ('list', 'status', 'tail', 'approvals'):
            command.add_argument('--timeout', type=float, default=60,
                                 help='maximum wait seconds (default: 60)')
        if name == 'start':
            command.add_argument('--vendor', choices=['codex', 'claude'], required=True)
            command.add_argument('--cwd', type=Path, required=True)
            command.add_argument('--model')
            command.add_argument('--effort')
            command.add_argument('--role', choices=['developer', 'reviewer', 'observer'])
            command.add_argument('--name')
            command.add_argument('--env', action='append', default=[], metavar='K=V')
            command.add_argument('--executable', type=Path,
                                 help='explicit native CLI path (otherwise /usr/local/bin, then PATH)')
            command.epilog = 'Pass native vendor arguments after --.'
        if name == 'resume':
            command.epilog = f'Run {RESUME_COMMAND}.'
        if name == 'send':
            command.add_argument('--message', help='send text; defaults to stdin')
        if name == 'tail':
            command.add_argument('-n', type=int, default=20, help='number of summarized events')
    internal = commands.add_parser('_run')
    internal.add_argument('--dir', type=Path, required=True)
    internal.add_argument('--generation', required=True)
    arguments = sys.argv[1:]
    separator = arguments.index('--') if '--' in arguments else len(arguments)
    args = parser.parse_args(arguments[:separator])
    if hasattr(args, 'timeout') and (args.timeout < 0 or not math.isfinite(args.timeout)):
        raise BridgeError('timeout must be finite and nonnegative')
    vendor_args = arguments[separator + 1:]
    if vendor_args and args.command != 'start':
        raise BridgeError('vendor arguments are set at start and retained on resume')
    return args, vendor_args


def main():
    args, vendor_args = parse_args()
    if args.command == 'list':
        root = args.root
        if os.path.lexists(root):
            directory(root, create=False)
        workers = []
        for path in sorted(root.rglob('state')):
            try:
                if not stat.S_ISREG(path.lstat().st_mode):
                    continue
                directory(path.parent, create=False)
                state = snapshot(path.parent)
                workers.append({'dir': str(path.parent), **state})
            except (OSError, ValueError, KeyError):
                workers.append({'dir': str(path.parent), 'status': 'failed'})
        if args.json:
            print(json.dumps(workers))
        else:
            print_worker_table(workers)
        return 0
    root = args.dir.absolute()
    if args.command == 'stop' and not (root / 'state').exists():
        return 0
    inspect = args.command in ('status', 'tail', 'wait', 'approvals')
    starting = args.command == 'start'
    if starting:
        if os.path.lexists(root):
            directory(root, create=False)
        try:
            task = read(args.task or root / 'task.md')
        except FileNotFoundError as error:
            raise BridgeError(
                'start requires an assignment file. Supply --task FILE or put task.md in the worker directory.'
            ) from error
    try:
        directory(root, create=starting)
    except FileNotFoundError as error:
        if starting:
            raise
        raise BridgeError('worker state is missing') from error
    if not inspect and not starting and not (root / 'state').exists():
        raise BridgeError('worker state is missing')
    if not inspect or os.path.lexists(root / 'inbox'):
        directory(root / 'inbox', create=not inspect)
    if args.command == '_run':
        lock = claim_turn(root, args.generation)
        if lock is None:
            return 1
        with lock:
            state = snapshot(root)
            if (state['status'] != 'launching' or state['generation'] != args.generation
                    or state['pid'] != os.getpid()):
                return 1
            state.pop('queued_messages', None)
            state['status'] = 'running'
            save_state(root, state)
            worker = None
            try:
                worker = Worker(root, state)
                worker.run()
            except Exception as error:
                # Native errors and protocol bodies can contain credentials.
                if isinstance(error, BridgeError):
                    state['error'] = str(error)
                if worker is None:
                    state['status'] = 'failed'
                    state.setdefault('error', 'Worker initialization failed.')
                    publish_failure(root, state)
                    record_event(root, 'error', message=state['error'])
                else:
                    worker.outcome = 'interrupted' if worker.interrupted else 'failed'
                    worker.record('error', message=state.get('error', 'Worker executable, protocol or state operation failed.'),
                                  error_type=type(error).__name__)
            if worker and not state.get('result_file'):
                with locked(root / 'inbox.lock'):
                    worker.finish()
        # Stop the whole scope after publishing the result. Explicit stop and
        # resume verify cleanup independently.
        scope_command('--no-block', 'stop', state['scope'])
        return 0
    if args.command == 'wait':
        state, code = wait(root, time.monotonic() + args.timeout)
        print(json.dumps(state))
        return code
    if args.command == 'status':
        print(json.dumps(worker_view(snapshot(root))))
        return 0
    if args.command == 'tail':
        if args.n < 0:
            raise BridgeError('tail count must not be negative')
        if args.n == 0:
            return 0
        lines = deque(maxlen=args.n)
        with private_open(root / 'events.jsonl', os.O_RDONLY, 'r', require_private=False) as stream:
            for line in stream:
                if json.loads(line)['type'] in ('message', 'tool', 'error'):
                    lines.append(line.rstrip('\n'))
        print('\n'.join(lines), end='\n' if lines else '')
        return 0
    if args.command == 'approvals':
        requests = []
        state = snapshot(root)
        for path in unanswered_approvals(root, state):
            try:
                request = json.loads(read(path))
            except FileNotFoundError:
                continue
            requests.append({'id': path.stem, **{key: request[key] for key in ('vendor', 'tool', 'command', 'reason')},
                             'file': str(path)})
        print(json.dumps(requests))
        return 0
    if args.command == 'send':
        hint = f'send requires an active turn. To continue a finished or stopped worker, use {RESUME_COMMAND}.'
        original = snapshot(root) if (root / 'state').exists() else None
        if original is None or original['status'] not in ACTIVE_STATES:
            raise BridgeError(hint)
        if args.message is None and sys.stdin.isatty():
            raise BridgeError('send requires --message or piped stdin')
        message = args.message if args.message is not None else sys.stdin.read()
        lock = lock_until(root / 'inbox.lock', time.monotonic() + args.timeout)
        with lock if lock is not None else nullcontext():
            current = snapshot(root)
            if current['status'] not in ACTIVE_STATES:
                raise BridgeError(hint)
            if current['generation'] != original['generation']:
                raise BridgeError('send refused because the active turn changed before enqueueing the message.')
            if lock is None:
                print('Timed out waiting for the bridge inbox lock.', file=sys.stderr)
                return 124
            write(root / 'inbox' / (f'{time.time_ns():020d}-' + uuid.uuid4().hex + '.md'), message)
        print('Queued for the next available Claude turn.' if current['vendor'] == 'claude'
              else 'Queued for steering the Codex turn.')
        return 0
    deadline = time.monotonic() + args.timeout
    lock = lock_until(root / 'command.lock', deadline)
    if lock is None:
        print('Timed out waiting for the bridge command lock.', file=sys.stderr)
        if args.command in ('stop', 'resume') and (root / 'state').exists():
            print(json.dumps(snapshot(root)))
        return 124
    with lock:
        if args.command in ('approve', 'deny'):
            state = snapshot(root)
            name = args.id
            if not name.isascii() or not name.isdigit() or len(name) < 6 or name not in state['pending_approvals']:
                raise BridgeError('approval is not pending')
            reply = turn_dir(root, state) / 'approvals' / (name + '.reply')
            if os.path.lexists(reply):
                raise BridgeError('approval already answered')
            read(reply.with_suffix('.json'))
            write(reply, 'allow\n' if args.command == 'approve' else 'deny\n')
            return 0
        if args.command in ('start', 'resume'):
            if args.command == 'start':
                if (root / 'state').exists():
                    raise BridgeError('worker already exists; use resume')
                if args.role:
                    args.model = args.model or MODELS[args.vendor][args.role]
                    args.effort = args.effort or ('medium' if args.role == 'observer' else 'high')
                if args.role == 'reviewer' and args.vendor == 'codex' and codex_arguments(vendor_args)[1]:
                    print('An explicit --sandbox or sandbox_mode conflicts with --role reviewer, which fixes the sandbox.',
                          file=sys.stderr)
                    return 1
                if not args.model or not args.effort:
                    raise BridgeError('start requires --role or explicit --model and --effort')
                if not args.cwd.is_dir():
                    raise BridgeError('worker cwd is not a directory')
                env = {}
                for entry in args.env:
                    key, separator, value = entry.partition('=')
                    if not separator or not key or '\0' in key + value:
                        raise BridgeError('invalid environment override')
                    env[key] = value
                config = dict(vendor=args.vendor, model=args.model, effort=args.effort, cwd=str(args.cwd.resolve()),
                              role=args.role, name=args.name or root.name, env=env, vendor_args=vendor_args,
                              executable=str(args.executable.resolve()) if args.executable else None)
                role_arguments(config, {**os.environ, **env})
                state = dict(session_id=None, approval_sequence=0)
            else:
                state = snapshot(root)
                if state['status'] in ACTIVE_STATES or not state['session_id']:
                    raise BridgeError('resume requires a stopped worker with a session id')
                config = json.loads(read(root / 'config.json'))
                task = read(args.task or root / 'task.md')
            return launch(root, state, config, task, deadline)
        if args.command == 'stop':
            state, code = stop(root, deadline)
            print(json.dumps(state))
            return code
        if args.command == 'interrupt':
            state = snapshot(root)
            if state['status'] not in ACTIVE_STATES:
                raise BridgeError('worker is not running')
            write(turn_dir(root, state) / 'interrupt', '')
    return 0


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except BridgeError as error:
        print(error, file=sys.stderr)
        raise SystemExit(1)
    except (OSError, ValueError, KeyError, TypeError):
        print('Bridge command failed; check arguments, private state directory and worker state.', file=sys.stderr)
        raise SystemExit(1)
