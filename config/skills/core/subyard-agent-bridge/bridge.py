"""File-based cross-vendor workers using native stdio protocols."""

import argparse
from collections import deque
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
MODELS = {'codex': {'developer': 'gpt-6.1-sol', 'reviewer': 'gpt-6.1-sol', 'observer': 'gpt-6-luna'},
          'claude': {'developer': 'opus', 'reviewer': 'opus', 'observer': 'haiku'}}
STANDALONE = ('You are a standalone worker with no parent agent. Put your full report in your final '
              'message. Do not send it to a parent.\n\n')
SECRET_NAME = r'token|secret|password|passwd|passphrase|credentials?|cookie|api[_-]?key|private[_-]?key|access[_-]?key|signing[_-]?key'
SECRET_ENV = re.compile(rf'(?:^|_)(?:{SECRET_NAME})(?:_|$)', re.IGNORECASE)
SECRET_TEXT = re.compile(
    rf'(?i)(\b(?:[a-z0-9]+[_-])*(?:{SECRET_NAME}|authorization)(?:[_-][a-z0-9]+)*["\x27]?\s*[:=]\s*)'
    r'(?:bearer\s+)?(?:"[^"\r\n]*"|\x27[^\x27\r\n]*\x27|[^\s,;]+)'
    r'|\bbearer [\w.+/=-]+|\b(?:gh[pousr]_|github_pat_|sk-)[\w-]{16,}'
    r'|\beyJ[\w-]+\.eyJ[\w-]+\.[\w-]+')
PRIVATE_KEY = re.compile(r'-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|\Z)',
                         re.DOTALL)


def event_redactor(env):
    values = {value for name, value in env.items() if SECRET_ENV.search(name)
              and not name.upper().endswith(('_FILE', '_PATH', '_DIR', '_SOCK')) and len(value) >= 8}
    exact = re.compile('|'.join(re.escape(value) for value in sorted(values, key=len, reverse=True))) if values else None

    def redact(text):
        text = PRIVATE_KEY.sub('[redacted]', text)
        if exact:
            text = exact.sub('[redacted]', text)
        return SECRET_TEXT.sub(lambda match: (match[1] or '') + '[redacted]', text)

    return redact


def role_arguments(config, env):
    if config.get('role') != 'reviewer':
        return []
    cwd = config['cwd']
    scratch = str(Path(env.get('TMPDIR', '/tmp')).resolve())
    if cwd in ('/tmp', scratch) or Path(cwd) in Path('/tmp').parents or Path(cwd) in Path(scratch).parents:
        raise ValueError('reviewer cwd must be distinct from temporary roots')
    if config['vendor'] == 'codex':
        filesystem = {'/tmp': 'write', scratch: 'write', cwd: 'read'}
        return ['-c', 'default_permissions="bridge-reviewer"',
                '-c', 'permissions.bridge-reviewer.extends=":read-only"',
                '-c', 'permissions.bridge-reviewer.filesystem={' + ','.join(
                    json.dumps(path) + '=' + json.dumps(mode) for path, mode in filesystem.items()) + '}']
    # Bash is sandboxed; file tools use explicit deny rules for repository writes.
    settings = {'sandbox': {'enabled': True, 'failIfUnavailable': True,
                           'allowUnsandboxedCommands': False, 'excludedCommands': [],
                           'autoAllowBashIfSandboxed': False,
                           'filesystem': {'denyWrite': [cwd], 'allowWrite': ['/tmp', scratch]}},
                'permissions': {'deny': ['Edit(/' + cwd + '/**)']}}
    return ['--settings', json.dumps(settings)]


def read(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd) as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid():
            raise ValueError('unsafe state file')
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


def directory(path):
    try:
        path.mkdir(mode=0o700)
        path.chmod(0o700)
    except FileExistsError:
        pass
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
        raise ValueError('state directories must be owned by you with mode 0700')


def locked(path):
    fd = os.open(path, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    stream = os.fdopen(fd, 'w')
    info = os.fstat(stream.fileno())
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077 or info.st_nlink != 1:
        stream.close()
        raise ValueError('unsafe lock file')
    os.fchmod(stream.fileno(), 0o600)
    fcntl.flock(stream, fcntl.LOCK_EX)
    return stream


def append(path):
    fd = os.open(path, os.O_WRONLY | os.O_APPEND | os.O_NOFOLLOW | os.O_NONBLOCK)
    stream = os.fdopen(fd, 'w')
    info = os.fstat(stream.fileno())
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077 or info.st_nlink != 1:
        stream.close()
        raise ValueError('unsafe log file')
    return stream


def publish_result(root, state, model, tokens=None, duration=None):
    result = {'state': state['status'], 'exit_code': EXIT_CODES[state['status']],
              'session_id': state.get('session_id'), 'vendor': state['vendor'],
              'model': model, 'duration_seconds': duration, 'tokens': tokens}
    if state.get('error'):
        result['error'] = state['error']
    write(root / 'result.json', json.dumps(result) + '\n')


class Codex:
    def __init__(self, worker):
        self.w = worker
        self.requests = {}
        self.items = {}
        self.counter = 0

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
        self.w.model = result.get('model', self.w.model)
        session = result['thread']['id']
        if self.w.state.get('session_id') not in (None, session):
            raise ValueError('Codex resumed a different session')
        self.w.state['session_id'] = session
        queued = [self.w.root / 'inbox' / name for name in self.w.state.pop('carryover', [])]
        self.w.sent.update(path.name for path in queued)
        self.w.save()
        self.request('turn/start', {'threadId': self.w.state['session_id'],
                     'input': [{'type': 'text', 'text': text} for text in
                               [self.w.task, *(read(path) for path in queued)]],
                     'effort': self.w.config['effort']}, lambda result: self.turn(result, queued))

    def turn(self, result, queued):
        for path in queued:
            path.unlink()
        self.w.state['turn_id'] = result['turn']['id']
        self.w.ready = True
        self.w.save()

    def message(self, path, text):
        self.request('turn/steer', {'threadId': self.w.state['session_id'],
                     'expectedTurnId': self.w.state['turn_id'],
                     'input': [{'type': 'text', 'text': text}]}, lambda result: path.unlink())

    def interrupt(self):
        if self.w.ready:
            self.request('turn/interrupt', {'threadId': self.w.state['session_id'],
                         'turnId': self.w.state['turn_id']})

    def event(self, event):
        method = event.get('method')
        if method is None and 'id' in event:
            request, callback = self.requests.pop(event['id'])
            if 'error' in event:
                if request == 'turn/steer':
                    # The inbox file has not been acknowledged. Deliver it on the next turn.
                    self.w.record('message_deferred')
                    return
                raise ValueError('Codex request failed')
            callback(event['result'])
        elif method in ('item/commandExecution/requestApproval', 'item/fileChange/requestApproval'):
            params = event['params']
            offered = params.get('availableDecisions', ['accept', 'decline', 'cancel'])
            if 'accept' not in offered or not any(d in offered for d in ('decline', 'cancel')):
                raise ValueError('single-use approval unavailable')
            deny = 'decline' if 'decline' in offered else 'cancel'
            item = self.items.get(params.get('itemId'), {})
            self.w.approval(event, params.get('command') or item.get('command', ''), params.get('reason', ''),
                            method.split('/')[1], lambda allow: self.w.send({
                                'id': event['id'], 'result': {'decision': 'accept' if allow else deny}}), item)
        elif method in ('item/started', 'item/completed'):
            item = event['params']['item']
            if item['type'] == 'agentMessage' and method == 'item/completed':
                self.w.message(item['text'])
            elif item['type'] in ('commandExecution', 'fileChange', 'mcpToolCall'):
                self.w.record('tool', tool=item['type'], status=item.get('status'), stage=method)
                if method == 'item/started':
                    self.items[item['id']] = item
                else:
                    self.items.pop(item.get('id'), None)
        elif method == 'turn/completed':
            status = event['params']['turn']['status']
            self.w.outcome = {'completed': 'done', 'interrupted': 'interrupted', 'failed': 'failed'}[status]
        elif method == 'thread/tokenUsage/updated':
            self.w.tokens = event['params']['tokenUsage']
        elif 'id' in event and method:
            # Unknown approval/user-input protocols must never be silently approved.
            raise ValueError('unsupported Codex server request')


class Claude:
    def __init__(self, worker):
        self.w = worker

    def start(self):
        self.w.ready = True
        self.user(self.w.task)

    def user(self, text):
        self.w.send({'type': 'user', 'message': {'role': 'user', 'content': [
            {'type': 'text', 'text': text}]}})

    def message(self, path, text):
        self.user(text)
        path.unlink()

    def interrupt(self):
        self.w.send({'type': 'control_request', 'request_id': 'interrupt-' + uuid.uuid4().hex,
                     'request': {'subtype': 'interrupt'}})

    def event(self, event):
        kind = event.get('type')
        if kind == 'control_request':
            request = event['request']
            if request['subtype'] != 'can_use_tool':
                raise ValueError('unsupported Claude control request')
            inputs = request['input']
            self.w.approval(event, inputs.get('command', ''), request.get('decision_reason', ''),
                            request['tool_name'], lambda allow: self.w.send({
                                'type': 'control_response', 'response': {
                                    'subtype': 'success', 'request_id': event['request_id'],
                                    'response': ({'behavior': 'allow', 'updatedInput': inputs} if allow else
                                                 {'behavior': 'deny', 'message': 'Operator denied this request.'})}}))
        elif kind == 'system' and event.get('subtype') == 'init':
            self.w.model = event.get('model', self.w.model)
            session = event['session_id']
            if self.w.state.get('session_id') not in (None, session):
                raise ValueError('Claude resumed a different session')
            self.w.state['session_id'] = session
            self.w.save()
        elif kind == 'assistant':
            for block in event['message']['content']:
                if block['type'] == 'text':
                    self.w.message(block['text'])
                elif block['type'] == 'tool_use':
                    self.w.record('tool', tool=block['name'])
        elif kind == 'result':
            self.w.tokens = event.get('usage')
            answer = event.get('result')
            if answer and (not self.w.answers or
                           answer not in (self.w.answers[-1], '\n\n'.join(self.w.answers))):
                self.w.message(answer)
            self.w.outcome = 'failed' if event.get('is_error') or event.get('subtype') != 'success' else 'done'


class Worker:
    def __init__(self, root):
        self.root = root
        self.config = json.loads(read(root / 'config.json'))
        self.state = json.loads(read(root / 'state'))
        self.state['pid'] = os.getpid()
        self.task = read(root / 'task.md')
        if self.config.get('role') == 'reviewer':
            self.task = 'Review read-only. Do not modify the repository. Write test/build outputs only in /tmp or TMPDIR.\n' + self.task
        elif self.config.get('role') == 'observer':
            self.task = 'Observe the assigned checks. Do not change repository sources.\n' + self.task
        self.task = STANDALONE + self.task
        self.env = {**os.environ, **self.config.get('env', {})}
        self.redact = event_redactor(self.env)
        self.pending = {}
        self.approval_ids = set()
        self.sent = set()
        self.ready = False
        self.outcome = None
        self.answers = []
        self.model = self.config['model']
        self.tokens = None
        self.interrupted = False
        self.adapter = Codex(self) if self.config['vendor'] == 'codex' else Claude(self)

    def save(self):
        self.state['pending_approvals'] = sorted(self.pending)
        write(self.root / 'state', json.dumps(self.state) + '\n')

    def record(self, kind, **fields):
        fields = {key: self.redact(value) if isinstance(value, str) else value for key, value in fields.items()}
        with append(self.root / 'events.jsonl') as stream:
            stream.write(json.dumps({'type': kind, **fields}) + '\n')

    def message(self, text):
        self.answers.append(text)
        self.record('message', text=text)

    def send(self, event):
        self.child.stdin.write((json.dumps(event) + '\n').encode())
        self.child.stdin.flush()

    def approval(self, request, command, reason, tool, reply, item=None):
        request_id = request.get('request_id', request.get('id'))
        if not isinstance(request_id, (str, int)) or request_id in self.approval_ids:
            raise ValueError('invalid or duplicate approval id')
        self.approval_ids.add(request_id)
        self.state['approval_sequence'] += 1
        # Persist the sequence before publication, so a crash cannot reuse a displayed ID.
        self.save()
        name = f'{self.state["approval_sequence"]:06d}'
        path = self.root / 'approvals' / (name + '.json')
        # A reply must follow publication of this request, never preauthorize a future one.
        if path.with_suffix('.reply').exists():
            raise ValueError('unsolicited approval reply')
        write(path, json.dumps({'vendor': self.config['vendor'], 'tool': tool,
                               'command': command, 'reason': reason, 'request': request,
                               'item': item}) + '\n')
        self.pending[name] = reply
        self.record('approval', id=name)
        self.save()

    def controls(self):
        if (self.root / 'interrupt').exists():
            self.interrupted = True
        if self.interrupted:
            return
        for path in sorted((self.root / 'approvals').glob('*.reply')):
            name = path.stem
            if name not in self.pending:
                raise ValueError('unsolicited approval reply')
            decision = read(path).strip()
            if decision not in ('allow', 'deny'):
                raise ValueError('approval reply must be allow or deny')
            self.pending[name](decision == 'allow')
            path.unlink()
            path.with_suffix('.json').unlink()
            self.pending.pop(name)
            self.record('approval_decision', id=name, decision=decision)
            self.save()
        if self.ready:
            for path in sorted((self.root / 'inbox').glob('*.md')):
                if path.name not in self.sent:
                    self.sent.add(path.name)
                    self.adapter.message(path, read(path))

    def run(self):
        env = self.env
        executable = self.config.get('executable')
        if not executable:
            installed = '/usr/local/bin/' + self.config['vendor']
            executable = installed if Path(installed).is_file() else shutil.which(self.config['vendor'], path=env.get('PATH'))
        if not executable:
            raise ValueError('worker executable missing')
        extra = role_arguments(self.config, env) + self.config.get('vendor_args', [])
        argv = [executable, *extra, 'app-server'] if self.config['vendor'] == 'codex' else [
            executable, '-p', '--input-format', 'stream-json', '--output-format', 'stream-json',
            '--verbose', '--model', self.config['model'], '--effort', self.config['effort'],
            *extra, '--permission-prompt-tool', 'stdio']
        if self.config['vendor'] == 'claude' and self.state.get('session_id'):
            argv.append('--resume=' + self.state['session_id'])
        signal.signal(signal.SIGTERM, lambda *_: setattr(self, 'interrupted', True))
        signal.signal(signal.SIGINT, lambda *_: setattr(self, 'interrupted', True))
        self.child = subprocess.Popen(argv, cwd=self.config['cwd'], stdin=subprocess.PIPE,
                                      stdout=subprocess.PIPE, env=env, start_new_session=True, bufsize=0)
        self.state['worker_pid'] = self.child.pid
        self.state['worker_start'] = process_identity(self.child.pid)
        selector = selectors.DefaultSelector()
        selector.register(self.child.stdout, selectors.EVENT_READ)
        buffer = b''
        try:
            self.save()
            self.adapter.start()
            while not self.outcome:
                self.controls()
                if self.interrupted:
                    self.adapter.interrupt()
                    self.outcome = 'interrupted'
                    break
                for key, _ in selector.select(0.1):
                    chunk = os.read(key.fileobj.fileno(), 65536)
                    if not chunk:
                        raise ValueError('worker stream closed before completion')
                    buffer += chunk
                    while b'\n' in buffer and not self.outcome:
                        frame, buffer = buffer.split(b'\n', 1)
                        if len(frame) > 16 * 1024 * 1024:
                            raise ValueError('worker frame too large')
                        self.adapter.event(json.loads(frame))
                    if len(buffer) > 16 * 1024 * 1024:
                        raise ValueError('worker frame too large')
            if self.outcome == 'done' and self.pending:
                raise ValueError('worker completed with unanswered approvals')
        finally:
            selector.close()
            # Close the whole worker process group, including outstanding tool processes.
            self.child.stdin.close()
            try:
                os.killpg(self.child.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                self.child.wait(timeout=2)
            except subprocess.TimeoutExpired:
                os.killpg(self.child.pid, signal.SIGKILL)
                self.child.wait()
            try:
                os.killpg(self.child.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            self.child.stdout.close()

    def finish(self):
        for name in self.pending:
            for suffix in ('.json', '.reply'):
                (self.root / 'approvals' / (name + suffix)).unlink(missing_ok=True)
        self.pending.clear()
        (self.root / 'interrupt').unlink(missing_ok=True)
        if self.outcome == 'done':
            write(self.root / 'final.md', '\n\n'.join(self.answers) + '\n')
        self.state.update(worker_pid=None, worker_start=None)
        self.state['status'] = self.outcome or 'failed'
        publish_result(self.root, self.state, self.model, self.tokens,
                       round(time.time() - self.state['started_at'], 3))
        self.record(self.state['status'])
        self.save()


def process_identity(pid, include_zombie=False):
    try:
        fields = Path(f'/proc/{pid}/stat').read_text().rsplit(') ', 1)[1].split()
        return None if fields[0] == 'Z' and not include_zombie else fields[19]
    except (FileNotFoundError, ProcessLookupError):
        return None


def snapshot(root):
    state = json.loads(read(root / 'state'))
    if state['status'] == 'running' and state.get('pid'):
        if process_identity(state['pid']) != state.get('process_start'):
            state.update(status='failed', pending_approvals=[], error='Bridge process exited without a terminal state.')
    state['queued_messages'] = sorted(path.name for path in (root / 'inbox').glob('*.md'))
    return state


def launch(root, state, config, task):
    config.setdefault('name', root.name)
    with locked(root / 'worker.lock'):
        stop_orphan(state)
        for path in (root / 'approvals').glob('*.json'):
            path.unlink()
            path.with_suffix('.reply').unlink(missing_ok=True)
        write(root / 'task.md', task)
        write(root / 'config.json', json.dumps(config) + '\n')
        state.pop('error', None)
        state.pop('queued_messages', None)
        if config['vendor'] == 'codex':
            state['carryover'] = sorted(path.name for path in (root / 'inbox').glob('*.md'))
        state.update(status='running', pid=None, turn_id=None, pending_approvals=[],
                     generation=uuid.uuid4().hex, vendor=config['vendor'], name=config['name'], model=config['model'],
                     started_at=time.time(), worker_pid=None, worker_start=None)
        write(root / 'state', json.dumps(state) + '\n')
        for name in ('final.md', 'result.json', 'interrupt'):
            (root / name).unlink(missing_ok=True)
        for name in ('events.jsonl', 'bridge.log'):
            if not (root / name).exists():
                write(root / name, '')
        try:
            with append(root / 'bridge.log') as log:
                child = subprocess.Popen([sys.executable, str(Path(__file__).resolve()), '_run', '--dir', str(root)],
                                         stdin=subprocess.DEVNULL, stdout=log, stderr=log, start_new_session=True)
        except OSError:
            state.update(status='failed', error='Bridge process could not start.')
            publish_result(root, state, config['model'])
            write(root / 'state', json.dumps(state) + '\n')
            raise
        state.update(pid=child.pid, process_start=process_identity(child.pid))
        write(root / 'state', json.dumps(state) + '\n')
    print(json.dumps({'status': 'running', 'pid': child.pid, 'dir': str(root)}))


def wait(root, timeout, stopping=False):
    deadline = time.monotonic() + timeout
    while True:
        state = snapshot(root)
        if state['status'] != 'running':
            break
        if state['pending_approvals'] and not stopping:
            print(json.dumps(state))
            return 2
        if time.monotonic() >= deadline:
            print(json.dumps(state))
            return 124
        time.sleep(0.1)
    print(json.dumps(state))
    return 0 if stopping else EXIT_CODES[state['status']]


def stop_orphan(state):
    if not state.get('error'):
        return
    pid = state.get('worker_pid')
    if pid and state.get('worker_start') and process_identity(pid, include_zombie=True) in (None, state['worker_start']):
        # The group can still contain tools after its session leader exits.
        try:
            os.killpg(pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    state.update(worker_pid=None, worker_start=None)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['start', 'send', 'interrupt', 'stop', 'status', 'wait', 'resume',
                                          'tail', 'approvals', 'approve', 'deny', 'list', '_run'])
    parser.add_argument('approval_id', nargs='?')
    parser.add_argument('--dir', type=Path)
    parser.add_argument('--root', type=Path, help='parent of worker directories for list')
    parser.add_argument('--vendor', choices=['codex', 'claude'])
    parser.add_argument('--model')
    parser.add_argument('--effort')
    parser.add_argument('--cwd', type=Path)
    parser.add_argument('--task', type=Path, help='assignment file for start or resume')
    parser.add_argument('--role', choices=['developer', 'reviewer', 'observer'])
    parser.add_argument('--name')
    parser.add_argument('--env', action='append', default=[], metavar='K=V')
    parser.add_argument('--executable', type=Path, help='explicit native CLI path (otherwise /usr/local/bin, then PATH)')
    parser.add_argument('-n', type=int, default=20, help='number of summarized events for tail')
    parser.add_argument('--message', help='send text; defaults to stdin')
    parser.add_argument('--timeout', type=float, default=60, help='maximum wait seconds (default: 60)')
    arguments = sys.argv[1:]
    separator = arguments.index('--') if '--' in arguments else len(arguments)
    args = parser.parse_args(arguments[:separator])
    if args.timeout < 0 or not math.isfinite(args.timeout):
        raise ValueError('timeout must be finite and nonnegative')
    vendor_args = arguments[separator + 1:]
    if vendor_args and args.command != 'start':
        raise ValueError('vendor arguments are set at start and retained on resume')
    if args.command == 'list':
        root = args.root or args.dir
        if root is None:
            raise ValueError('list requires --root or --dir')
        directory(root)
        workers = []
        for path in sorted(root.rglob('state')):
            try:
                directory(path.parent)
                state = snapshot(path.parent)
                workers.append({'dir': str(path.parent), **state})
            except (OSError, ValueError, KeyError):
                workers.append({'dir': str(path.parent), 'status': 'failed'})
        print(json.dumps(workers))
        return 0
    if args.dir is None:
        raise ValueError('--dir is required')
    root = args.dir.absolute()
    if args.command == 'stop' and not (root / 'state').exists():
        return 0
    directory(root)
    for name in ('inbox', 'approvals'):
        directory(root / name)
    if args.command == '_run':
        with locked(root / 'worker.lock'):
            worker = None
            try:
                worker = Worker(root)
                worker.run()
            except Exception as error:
                # Native errors and protocol bodies can contain credentials. Keep diagnostics generic.
                if worker is None:
                    state = json.loads(read(root / 'state'))
                    state.update(status='failed', error='Worker initialization failed.')
                    publish_result(root, state, state.get('model'))
                    write(root / 'state', json.dumps(state) + '\n')
                    return 1
                worker.outcome = 'interrupted' if worker.interrupted else 'failed'
                worker.record('error', message='Worker executable, protocol or state operation failed.',
                              error_type=type(error).__name__)
            worker.finish()
        return 0
    if args.command == 'wait':
        return wait(root, args.timeout)
    if args.command == 'status':
        print(json.dumps(snapshot(root)))
        return 0
    if args.command == 'tail':
        if args.n < 0:
            raise ValueError('tail count must not be negative')
        lines = deque(maxlen=args.n)
        config = json.loads(read(root / 'config.json'))
        redact = event_redactor({**os.environ, **config.get('env', {})})
        for line in read(root / 'events.jsonl').splitlines():
            event = json.loads(line)
            if event['type'] in ('message', 'tool'):
                lines.append(json.dumps({
                    key: redact(value) if isinstance(value, str) else value for key, value in event.items()}))
        print('\n'.join(lines), end='\n' if lines else '')
        return 0
    if args.command == 'approvals':
        requests = []
        for name in snapshot(root)['pending_approvals']:
            path = root / 'approvals' / (name + '.json')
            try:
                request = json.loads(read(path))
            except FileNotFoundError:
                continue
            requests.append({'id': name, **{key: request[key] for key in ('vendor', 'tool', 'command', 'reason')},
                             'file': str(path)})
        print(json.dumps(requests))
        return 0
    with locked(root / 'command.lock'):
        if args.command in ('approve', 'deny'):
            name = args.approval_id
            if not name or not name.isascii() or not name.isdigit() or len(name) < 6 or name not in snapshot(root)['pending_approvals']:
                raise ValueError('approval is not pending')
            reply = root / 'approvals' / (name + '.reply')
            if os.path.lexists(reply):
                raise ValueError('approval already answered')
            read(reply.with_suffix('.json'))
            write(reply, 'allow\n' if args.command == 'approve' else 'deny\n')
            return 0
        if args.command in ('start', 'resume', 'send'):
            text = None
            if args.command == 'send':
                if args.message is None and sys.stdin.isatty():
                    raise ValueError('send requires --message or piped stdin')
                text = args.message if args.message is not None else sys.stdin.read()
                state = snapshot(root)
                if state['status'] == 'running':
                    write(root / 'inbox' / (f'{time.time_ns():020d}-' + uuid.uuid4().hex + '.md'), text)
                    return 0
            if args.command == 'start':
                if (root / 'state').exists():
                    raise ValueError('worker already exists; use resume')
                if not args.vendor or not args.cwd:
                    raise ValueError('start requires --vendor and --cwd')
                if args.role:
                    args.model = args.model or MODELS[args.vendor][args.role]
                    args.effort = args.effort or ('medium' if args.role == 'observer' else 'high')
                if not args.model or not args.effort:
                    raise ValueError('start requires --role or explicit --model and --effort')
                if not args.cwd.is_dir():
                    raise ValueError('worker cwd is not a directory')
                env = {}
                for entry in args.env:
                    key, value = entry.split('=', 1)
                    if not key or '\0' in key + value:
                        raise ValueError('invalid environment override')
                    env[key] = value
                config = dict(vendor=args.vendor, model=args.model, effort=args.effort, cwd=str(args.cwd.resolve()),
                              role=args.role, name=args.name or root.name, env=env, vendor_args=vendor_args,
                              executable=str(args.executable.resolve()) if args.executable else None)
                role_arguments(config, {**os.environ, **env})
                state = dict(session_id=None, approval_sequence=0)
            else:
                state = snapshot(root)
                if state['status'] == 'running' or not state['session_id']:
                    raise ValueError('resume requires a stopped worker with a session id')
                config = json.loads(read(root / 'config.json'))
            task = text if text is not None else read(args.task if args.task else root / 'task.md')
            launch(root, state, config, task)
        else:
            state = snapshot(root)
            if state['status'] != 'running':
                if args.command == 'stop':
                    stop_orphan(state)
                    if state.get('error'):
                        for path in (root / 'approvals').glob('*.json'):
                            path.unlink()
                            path.with_suffix('.reply').unlink(missing_ok=True)
                        publish_result(root, state, state.get('model'))
                        write(root / 'state', json.dumps(state) + '\n')
                    return 0
                raise ValueError('worker is not running')
            write(root / 'interrupt', '')
    if args.command == 'stop':
        return wait(root, args.timeout, stopping=True)
    return 0


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except (OSError, ValueError, KeyError, TypeError):
        print('Bridge command failed; check arguments, private state directory and worker state.', file=sys.stderr)
        raise SystemExit(1)
