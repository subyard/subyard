"""File-based cross-vendor workers using native stdio protocols."""

import argparse
import fcntl
import json
import os
from pathlib import Path
import selectors
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import time
import uuid


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


class Codex:
    def __init__(self, worker):
        self.w = worker
        self.requests = {}
        self.items = {}
        self.counter = 0

    def request(self, method, params, callback=lambda result: None):
        self.counter += 1
        self.requests[self.counter] = callback
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
        session = result['thread']['id']
        if self.w.state.get('session_id') not in (None, session):
            raise ValueError('Codex resumed a different session')
        self.w.state['session_id'] = session
        self.w.save()
        self.request('turn/start', {'threadId': self.w.state['session_id'],
                     'input': [{'type': 'text', 'text': self.w.task}],
                     'effort': self.w.config['effort']}, self.turn)

    def turn(self, result):
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
            callback = self.requests.pop(event['id'])
            if 'error' in event:
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
                self.w.answer = item['text']
                self.w.record('message', text=item['text'])
            elif item['type'] in ('commandExecution', 'fileChange', 'mcpToolCall'):
                self.w.record('tool', tool=item['type'], status=item.get('status'), stage=method)
                if method == 'item/started':
                    self.items[item['id']] = item
                else:
                    self.items.pop(item.get('id'), None)
        elif method == 'turn/completed':
            status = event['params']['turn']['status']
            self.w.outcome = {'completed': 'done', 'interrupted': 'interrupted', 'failed': 'failed'}[status]
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
            session = event['session_id']
            if self.w.state.get('session_id') not in (None, session):
                raise ValueError('Claude resumed a different session')
            self.w.state['session_id'] = session
            self.w.save()
        elif kind == 'assistant':
            for block in event['message']['content']:
                if block['type'] == 'text':
                    self.w.record('message', text=block['text'])
                elif block['type'] == 'tool_use':
                    self.w.record('tool', tool=block['name'])
        elif kind == 'result':
            self.w.answer = event.get('result', '')
            self.w.outcome = 'failed' if event.get('is_error') or event.get('subtype') != 'success' else 'done'


class Worker:
    def __init__(self, root):
        self.root = root
        self.config = json.loads(read(root / 'config.json'))
        self.state = json.loads(read(root / 'state'))
        self.state['pid'] = os.getpid()
        self.task = read(root / 'task.md')
        self.pending = {}
        self.approval_ids = set()
        self.sent = set()
        self.ready = False
        self.outcome = None
        self.answer = ''
        self.interrupted = False
        self.adapter = Codex(self) if self.config['vendor'] == 'codex' else Claude(self)

    def save(self):
        self.state['pending_approvals'] = sorted(self.pending)
        write(self.root / 'state', json.dumps(self.state) + '\n')

    def record(self, kind, **fields):
        with append(self.root / 'events.jsonl') as stream:
            stream.write(json.dumps({'type': kind, **fields}) + '\n')

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
        executable = shutil.which(self.config['vendor'])
        if not executable:
            raise ValueError('worker executable missing')
        argv = [executable, 'app-server'] if self.config['vendor'] == 'codex' else [
            executable, '-p', '--input-format', 'stream-json', '--output-format', 'stream-json',
            '--verbose', '--permission-prompt-tool', 'stdio', '--model', self.config['model'],
            '--effort', self.config['effort']]
        if self.config['vendor'] == 'claude' and self.state.get('session_id'):
            argv.append('--resume=' + self.state['session_id'])
        signal.signal(signal.SIGTERM, lambda *_: setattr(self, 'interrupted', True))
        signal.signal(signal.SIGINT, lambda *_: setattr(self, 'interrupted', True))
        self.child = subprocess.Popen(argv, cwd=self.config['cwd'], stdin=subprocess.PIPE,
                                      stdout=subprocess.PIPE, start_new_session=True, bufsize=0)
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
            write(self.root / 'final.md', self.answer + '\n')
        self.state['status'] = self.outcome or 'failed'
        self.record(self.state['status'])
        self.save()


def snapshot(root):
    state = json.loads(read(root / 'state'))
    if state['status'] == 'running' and state.get('pid'):
        try:
            os.kill(state['pid'], 0)
        except ProcessLookupError:
            state.update(status='failed', error='Bridge process exited without a terminal state.')
    state['queued_messages'] = sorted(path.name for path in (root / 'inbox').glob('*.md'))
    return state


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['start', 'send', 'interrupt', 'status', 'wait', 'resume', '_run'])
    parser.add_argument('--dir', required=True, type=Path)
    parser.add_argument('--vendor', choices=['codex', 'claude'])
    parser.add_argument('--model')
    parser.add_argument('--effort')
    parser.add_argument('--cwd', type=Path)
    parser.add_argument('--message', help='send text; defaults to stdin')
    parser.add_argument('--timeout', type=float, default=60, help='maximum wait seconds (default: 60)')
    args = parser.parse_args()
    root = args.dir.absolute()
    directory(root)
    for name in ('inbox', 'approvals'):
        directory(root / name)
    if args.command == '_run':
        with locked(root / 'worker.lock'):
            worker = None
            try:
                worker = Worker(root)
                worker.run()
            except Exception:
                # Native errors and protocol bodies can contain credentials. Keep diagnostics generic.
                if worker is None:
                    state = json.loads(read(root / 'state'))
                    state.update(status='failed', error='Worker initialization failed.')
                    write(root / 'state', json.dumps(state) + '\n')
                    return 1
                worker.outcome = 'failed'
                worker.record('error', message='Worker executable, protocol or state operation failed.')
            worker.finish()
        return 0
    if args.command in ('status', 'wait'):
        initial = snapshot(root)
        state = initial
        deadline = time.monotonic() + args.timeout
        while args.command == 'wait' and state['status'] == 'running' and state == initial:
            if time.monotonic() >= deadline:
                print(json.dumps(state))
                return 124
            time.sleep(0.1)
            state = snapshot(root)
        print(json.dumps(state))
        return 0
    with locked(root / 'command.lock'):
        if args.command in ('start', 'resume'):
            if args.command == 'start':
                if (root / 'state').exists():
                    raise ValueError('worker already exists; use resume')
                if not all((args.vendor, args.model, args.effort, args.cwd)):
                    raise ValueError('start requires --vendor, --model, --effort and --cwd')
                if not args.cwd.is_dir():
                    raise ValueError('worker cwd is not a directory')
                config = dict(vendor=args.vendor, model=args.model, effort=args.effort, cwd=str(args.cwd.resolve()))
                state = dict(session_id=None, approval_sequence=0)
            else:
                state = snapshot(root)
                if state['status'] == 'running' or not state['session_id']:
                    raise ValueError('resume requires a stopped worker with a session id')
                config = json.loads(read(root / 'config.json'))
            read(root / 'task.md')
            with locked(root / 'worker.lock'):
                if args.command == 'resume':
                    for path in (root / 'approvals').glob('*.json'):
                        path.unlink()
                        path.with_suffix('.reply').unlink(missing_ok=True)
                write(root / 'config.json', json.dumps(config) + '\n')
                state.pop('error', None)
                state.pop('queued_messages', None)
                state.update(status='running', pid=None, turn_id=None, pending_approvals=[],
                             generation=uuid.uuid4().hex, vendor=config['vendor'])
                write(root / 'state', json.dumps(state) + '\n')
                (root / 'final.md').unlink(missing_ok=True)
                (root / 'interrupt').unlink(missing_ok=True)
                for name in ('events.jsonl', 'bridge.log'):
                    if not (root / name).exists():
                        write(root / name, '')
                with append(root / 'bridge.log') as log:
                    child = subprocess.Popen([sys.executable, str(Path(__file__).resolve()), '_run', '--dir', str(root)],
                                             stdin=subprocess.DEVNULL, stdout=log, stderr=log, start_new_session=True)
                # The child acquires worker.lock after this block; it owns all subsequent state writes.
                child_pid = child.pid
                state['pid'] = child_pid
                write(root / 'state', json.dumps(state) + '\n')
            print(json.dumps({'status': 'running', 'pid': child_pid, 'dir': str(root)}))
        else:
            if snapshot(root)['status'] != 'running':
                raise ValueError('worker is not running')
            if args.command == 'send':
                text = args.message if args.message is not None else sys.stdin.read()
                write(root / 'inbox' / (f'{time.time_ns():020d}-' + uuid.uuid4().hex + '.md'), text)
            else:
                write(root / 'interrupt', '')
    return 0


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except (OSError, ValueError, KeyError, TypeError):
        print('Bridge command failed; check arguments, private state directory and worker state.', file=sys.stderr)
        raise SystemExit(1)
