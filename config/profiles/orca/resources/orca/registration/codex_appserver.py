"""Make Orca native chat inherit Codex's effective yard permissions."""

import json
from collections import deque
import os
import selectors
import signal
import subprocess
import sys
import time
import uuid


MAX_FRAME = 16 * 1024 * 1024
CONFIG_TIMEOUT = 5
PERMISSION_FIELDS = ('approvalPolicy', 'approvalsReviewer', 'sandbox', 'sandboxPolicy',
                     'permissions')


def configured_request(request, response):
    config = response.get('result', {}).get('config')
    if not isinstance(config, dict):
        raise ValueError('config unavailable')
    approval = config.get('approval_policy')
    sandbox = config.get('sandbox_mode')
    reviewer = config.get('approvals_reviewer')
    if approval != 'on-request' or reviewer != 'user' or sandbox not in (
            'danger-full-access', 'workspace-write', 'read-only'):
        raise ValueError('config permissions unavailable')
    params = dict(request['params'])
    for field in PERMISSION_FIELDS:
        params.pop(field, None)
    params.update(approvalPolicy=approval, approvalsReviewer=reviewer, sandbox=sandbox)
    return {**request, 'params': params}


def relay(native, arguments):
    child = subprocess.Popen([native, *arguments], stdin=subprocess.PIPE,
                             stdout=subprocess.PIPE, bufsize=0)
    for signum in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        signal.signal(signum, lambda sig, _frame: child.send_signal(sig))
    selector = selectors.DefaultSelector()
    selector.register(sys.stdin.buffer, selectors.EVENT_READ, 'client')
    selector.register(child.stdout, selectors.EVENT_READ, 'server')
    buffers = {'client': b'', 'server': b''}
    pending = {}
    internal_ids = set()
    queued = deque()
    queued_bytes = 0
    client_closed = False

    def send(stream, message):
        stream.write(json.dumps(message, separators=(',', ':')).encode() + b'\n')
        stream.flush()

    def refuse(request):
        send(sys.stdout.buffer, {'id': request['id'], 'error': {
            'code': -32603, 'message': 'Yard Codex permissions unavailable; run yard init'}})

    def forward(request):
        if request.get('method') in ('thread/start', 'thread/resume'):
            params = request.get('params')
            if 'id' not in request or not isinstance(params, dict):
                raise ValueError('invalid thread request')
            internal_id = 'subyard-config-' + uuid.uuid4().hex
            internal_ids.add(internal_id)
            pending[internal_id] = (request, time.monotonic() + CONFIG_TIMEOUT)
            send(child.stdin, {'id': internal_id, 'method': 'config/read',
                               'params': {'cwd': params.get('cwd', os.getcwd())}})
        else:
            if request.get('method') == 'turn/start' and isinstance(request.get('params'), dict):
                params = dict(request['params'])
                for field in PERMISSION_FIELDS:
                    params.pop(field, None)
                request = {**request, 'params': params}
            send(child.stdin, request)

    try:
        while selector.get_map():
            for internal_id, (request, deadline) in list(pending.items()):
                if time.monotonic() >= deadline:
                    pending.pop(internal_id)
                    refuse(request)
            while queued and not pending:
                request, size = queued.popleft()
                queued_bytes -= size
                forward(request)
            if client_closed and not pending and not queued and not child.stdin.closed:
                child.stdin.close()
            for key, _ in selector.select(0.1):
                side = key.data
                chunk = os.read(key.fileobj.fileno(), 65536)
                if not chunk:
                    selector.unregister(key.fileobj)
                    if buffers[side]:
                        raise ValueError('incomplete frame')
                    if side == 'client':
                        client_closed = True
                    else:
                        if pending:
                            for request, _ in pending.values():
                                refuse(request)
                            pending.clear()
                        return child.wait()
                    continue
                buffers[side] += chunk
                while b'\n' in buffers[side]:
                    frame, buffers[side] = buffers[side].split(b'\n', 1)
                    if len(frame) > MAX_FRAME:
                        raise ValueError('oversized frame')
                    message = json.loads(frame)
                    if not isinstance(message, dict):
                        raise ValueError('invalid frame')
                    if side == 'server':
                        internal_id = message.get('id')
                        if isinstance(internal_id, str) and internal_id in internal_ids:
                            entry = pending.pop(internal_id, None)
                            if entry:
                                request, _ = entry
                                try:
                                    send(child.stdin, configured_request(request, message))
                                except (ValueError, TypeError, AttributeError):
                                    refuse(request)
                            continue
                        sys.stdout.buffer.write(frame + b'\n')
                        sys.stdout.buffer.flush()
                    else:
                        if pending or queued:
                            queued_bytes += len(frame)
                            if queued_bytes > MAX_FRAME:
                                raise ValueError('request queue exceeds limit')
                            queued.append((message, len(frame)))
                        else:
                            forward(message)
                if len(buffers[side]) > MAX_FRAME:
                    raise ValueError('oversized frame')
        return child.wait()
    finally:
        selector.close()
        if child.poll() is None:
            child.terminate()
            try:
                child.wait(timeout=2)
            except subprocess.TimeoutExpired:
                child.kill()
                child.wait()


def main():
    native, *arguments = sys.argv[1:]
    if not os.environ.get('ORCA_AGENT_SESSION_SPAWN_TOKEN') or arguments[-1:] != ['app-server']:
        os.execv(native, [native, *arguments])
    try:
        return relay(native, arguments)
    except (OSError, ValueError, TypeError):
        # Protocol/config bodies can contain private data; never include exception text.
        print('Orca Codex permission transport failed', file=sys.stderr)
        return 1


if __name__ == '__main__':
    raise SystemExit(main())
