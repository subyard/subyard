#!/usr/bin/env python3
"""Drive Claude under stock Orca against a synthetic loopback model; no account or model API."""

import argparse
import fcntl
import glob
import hashlib
import importlib.util
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import pty
import select
import shlex
import shutil
import signal
import struct
import subprocess
import sys
import tempfile
import termios
import threading
import time
import tomllib
import uuid

TOKEN = 'synthetic-local-only'
PROMPT = 'Run the fixture command.'
RUNTIME = '/opt/subyard/claude/claude'
CODEX_RUNTIME = os.path.realpath('/usr/local/bin/codex')
SHADOW = Path.home() / '.local/bin/claude'
SETTINGS = Path.home() / '.claude/settings.json'


class Model(BaseHTTPRequestHandler):
    """Answer each fixture prompt with the current command and allow every classifier request."""

    def log_message(self, *arguments):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        if 'count_tokens' in self.path:
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b'{"input_tokens":100}')
            return
        server = self.server
        block = {'type': 'text', 'text': 'Fixture turn complete.'}
        server.request_log.append({
            'tools': len(body.get('tools', [])),
            'bash': any(tool.get('name') == 'Bash' for tool in body.get('tools', [])),
            'max_tokens': body.get('max_tokens'), 'messages': len(body.get('messages', [])),
            'last': json.dumps(body['messages'][-1])[-240:] if body.get('messages') else None,
            'system': json.dumps(body.get('system'))[:120]})
        if any(tool.get('name') == 'Bash' for tool in body.get('tools', [])):
            server.requests += 1
            # A side request can precede the chat turn, so an unanswered prompt selects the command.
            # Claude appends a system-role context message after the prompt.
            unanswered = []
            for item in reversed(body['messages']):
                if item['role'] == 'assistant':
                    break
                content = item['content']
                unanswered += [{'type': 'text', 'text': content}] if isinstance(content, str) else content
            server.tool_results.extend(part for part in unanswered if part.get('type') == 'tool_result')
            command = (server.current_command() if any(
                PROMPT in part.get('text', '') for part in unanswered if part.get('type') == 'text') else None)
            if command:
                block = {'type': 'tool_use', 'id': 'tool_' + uuid.uuid4().hex, 'name': 'Bash',
                         'input': {'command': command, 'description': 'Fixture command'}}
        elif 'security monitor' in json.dumps(body.get('system')):
            block = {'type': 'text', 'text': '<block>no</block>'}
        stop = 'tool_use' if block['type'] == 'tool_use' else 'end_turn'
        message = {'id': 'msg_' + uuid.uuid4().hex, 'type': 'message', 'role': 'assistant',
                   'model': body.get('model', 'claude-sonnet-4-6'), 'content': [block],
                   'stop_reason': stop, 'stop_sequence': None,
                   'usage': {'input_tokens': 100, 'output_tokens': 10}}
        self.send_response(200)
        if not body.get('stream'):
            self.send_header('Content-Type', 'application/json')
            self.end_headers()
            self.wfile.write(json.dumps(message).encode())
            return
        self.send_header('Content-Type', 'text/event-stream')
        self.end_headers()
        delta = ({'type': 'input_json_delta', 'partial_json': json.dumps(block['input'])}
                 if stop == 'tool_use' else {'type': 'text_delta', 'text': block['text']})
        start = dict(block, input={}) if stop == 'tool_use' else dict(block, text='')
        for name, data in (
                ('message_start', {'message': dict(message, content=[], stop_reason=None)}),
                ('content_block_start', {'index': 0, 'content_block': start}),
                ('content_block_delta', {'index': 0, 'delta': delta}),
                ('content_block_stop', {'index': 0}),
                ('message_delta', {'delta': {'stop_reason': stop, 'stop_sequence': None},
                                   'usage': {'output_tokens': 10}}),
                ('message_stop', {})):
            self.wfile.write(('event: ' + name + '\ndata: ' +
                              json.dumps(dict(type=name, **data)) + '\n\n').encode())
        self.wfile.flush()


def model(current_command):
    server = ThreadingHTTPServer(('127.0.0.1', 0), Model)
    server.requests = 0
    server.tool_results = []
    server.request_log = []
    server.current_command = current_command
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


def resolved_exe(pid):
    try:
        return os.path.realpath('/proc/' + str(pid) + '/exe')
    except OSError:
        return None


def is_agent_executable(agent, executable):
    if agent == 'codex':
        return os.path.basename(executable) == 'codex'
    return executable == RUNTIME or '/claude/versions/' in executable


def agent_processes(matches, agent='claude'):
    """Return the resolved executables of this user's agent processes selected by matches(pid)."""
    found = {}
    for entry in Path('/proc').iterdir():
        try:
            if not entry.name.isdigit() or entry.stat().st_uid != os.getuid():
                continue
            executable = resolved_exe(entry.name)
            if executable and is_agent_executable(agent, executable) and matches(entry):
                found[int(entry.name)] = executable
        except OSError:
            continue
    return found


def managed_settings():
    value = json.loads(SETTINGS.read_text())
    return {key: value.get(key) for key in ('autoMemoryDirectory', 'permissions')}


def launch(arguments):
    """Start an interactive Claude under the requested data home and report its self-install."""
    config = Path(tempfile.mkdtemp(prefix='subyard-managed-claude-', dir='/tmp'))
    workspace = config / 'workspace'
    workspace.mkdir()
    (config / '.claude.json').write_text(json.dumps({
        'hasCompletedOnboarding': True, 'lastOnboardingVersion': '2.1.316',
        'projects': {str(workspace): {'hasTrustDialogAccepted': True,
                                      'hasCompletedProjectOnboarding': True}}}))
    server = model(lambda: None)
    environment = {
        'HOME': str(Path.home()), 'CLAUDE_CONFIG_DIR': str(config), 'PATH': os.environ['PATH'],
        'TERM': 'xterm-256color', 'TMPDIR': '/tmp', 'XDG_DATA_HOME': arguments.xdg,
        'ANTHROPIC_AUTH_TOKEN': TOKEN,
        'ANTHROPIC_BASE_URL': 'http://127.0.0.1:' + str(server.server_port)}
    pid, terminal = pty.fork()
    if pid == 0:
        os.chdir(workspace)
        os.execve(arguments.cli, [arguments.cli, '--model', 'claude-sonnet-4-6'], environment)
    fcntl.ioctl(terminal, termios.TIOCSWINSZ, struct.pack('HHHH', 50, 160, 0, 0))
    deadline = time.monotonic() + arguments.seconds
    sent = False
    last_output = time.monotonic()
    executable = None
    while time.monotonic() < deadline:
        ready, _, _ = select.select([terminal], [], [], 0.3)
        if ready:
            try:
                data = os.read(terminal, 65536)
            except OSError:
                break
            if not data:
                break
            last_output = time.monotonic()
            if b'\x1b[6n' in data:
                os.write(terminal, b'\x1b[1;1R')
        executable = resolved_exe(pid) or executable
        if not sent and time.monotonic() - last_output > 3:
            os.write(terminal, b'hello')
            time.sleep(0.4)
            os.write(terminal, b'\r')
            sent = True
        if arguments.expect_link and SHADOW.is_symlink():
            break
    result = {'executable': executable, 'model_requests': server.requests,
              'link': os.readlink(SHADOW) if SHADOW.is_symlink() else None,
              'versions': sorted(item.name for item in (Path(arguments.xdg) / 'claude/versions').glob('*'))}
    if arguments.pid_file:
        # A held session stays alive past this helper, with its own terminal end kept open.
        Path(arguments.pid_file).write_text(str(pid))
        print(json.dumps(result), flush=True)
        while True:
            try:
                if not os.read(terminal, 65536):
                    break
            except OSError:
                break
        return 0
    os.kill(pid, signal.SIGKILL)
    os.waitpid(pid, 0)
    print(json.dumps(result), flush=True)
    assert executable == os.path.realpath(arguments.cli), 'Claude ran another executable'
    if arguments.expect_link:
        assert result['link'], 'unlocked Claude did not self-install'
    else:
        assert result['model_requests'] > 0, 'Claude session did not reach the fixture model'
        assert not result['link'] and not result['versions'], 'locked Claude self-installed'
    return 0


def serve(arguments):
    """Serve the terminal lane; each fixture prompt runs the current content of the command file."""
    command_file = Path(arguments.command_file)
    server = model(lambda: command_file.read_text().strip() if command_file.exists() else None)
    Path(arguments.port_file).write_text(str(server.server_port))
    while True:
        time.sleep(1)


def terminal_exe(arguments):
    workspace = os.path.realpath(arguments.workspace)
    runtime = CODEX_RUNTIME if arguments.agent == 'codex' else RUNTIME
    def in_workspace(entry):
        return os.path.realpath(entry / 'cwd') == workspace
    deadline = time.monotonic() + arguments.seconds
    found = agent_processes(in_workspace, arguments.agent)
    while not found and time.monotonic() < deadline:
        time.sleep(0.5)
        found = agent_processes(in_workspace, arguments.agent)
    print('orca-terminal-executables: ' + arguments.agent + ' ' + json.dumps(found, sort_keys=True), flush=True)
    assert found, 'no agent process is running in the Orca terminal workspace'
    assert set(found.values()) == {runtime}, 'Orca terminal ran another agent executable'
    return 0


def exe_of(arguments):
    """Start a long-lived native command and report the executable it settles on."""
    process = subprocess.Popen(arguments.command, stdin=subprocess.PIPE, stdout=subprocess.DEVNULL,
                               stderr=subprocess.DEVNULL)
    executable = None
    deadline = time.monotonic() + 15
    try:
        while time.monotonic() < deadline and process.poll() is None:
            executable = resolved_exe(process.pid)
            if executable == arguments.expect:
                break
            time.sleep(0.1)
    finally:
        process.kill()
        process.wait()
    print('agent-executable: ' + json.dumps({'command': arguments.command[0], 'exe': executable}), flush=True)
    assert executable == arguments.expect, 'command reached another executable'
    return 0


def hold(arguments):
    """Keep a native app server running until this helper is killed."""
    process = subprocess.Popen(arguments.command, stdin=subprocess.PIPE, stdout=subprocess.DEVNULL,
                               stderr=subprocess.DEVNULL)
    Path(arguments.pid_file).write_text(str(process.pid))
    process.wait()
    return 0


def codex_chat(arguments):
    """Require stock Orca's native Codex chat to spawn the managed runtime."""
    sys.path.insert(0, '/usr/local/libexec/subyard/orca-registration')
    from transport import RuntimeRPC
    spec = importlib.util.spec_from_file_location('provider', arguments.provider)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    rpc = RuntimeRPC('/srv/agents/orca/config/orca/orca-runtime.json', timeout=30)
    workspace = Path(arguments.workspace).resolve()
    repo = next(item for item in rpc.call('repo.list')['repos'] if item['path'] == str(workspace))
    worktree = 'id:' + repo['id'] + '::' + str(workspace)
    support = rpc.call('agentSession.createSupport', {'worktree': worktree, 'agent': 'codex'})
    assert support.get('supported') is True, 'stock Orca native Codex chat is unavailable'
    default_arguments = rpc.call('settings.get')['settings'].get('agentDefaultArgs', {})
    config = Path.home() / '.codex/config.toml'
    original = config.read_bytes()
    provider = ThreadingHTTPServer(('127.0.0.1', 0), module.Provider)
    provider.command, provider.failure, provider.workspace = None, None, workspace
    threading.Thread(target=provider.serve_forever, daemon=True).start()
    session = str(uuid.uuid4())
    created = False
    try:
        config.write_bytes(original + (
            '\n[model_providers.fixture]\nname = "Fixture"\n'
            f'base_url = "http://127.0.0.1:{provider.server_port}/v1"\n'
            'wire_api = "responses"\nrequires_openai_auth = false\n'
            'request_max_retries = 0\nstream_max_retries = 0\n').encode())
        rpc.call('settings.update', {'agentDefaultArgs': {
            **default_arguments, 'codex': ' '.join(shlex.quote(a) for a in module.FIXTURE_CONFIG)}})
        fields = {'worktree': worktree, 'agent': 'codex'}
        result = accepted(rpc.call('agentSession.create', {
            **fields, 'envelope': envelope('agentSession.create', session, fields)}), 'create')
        created = True
        body = {'kind': 'message', 'role': 'user', 'blocks': [{'type': 'text', 'text': PROMPT}]}
        accepted(rpc.call('agentSession.send', {
            'envelope': envelope('agentSession.send', session, {'body': body}, result['fence']),
            'body': body}), 'send')
        def in_chat(entry):
            return b'ORCA_AGENT_SESSION_SPAWN_TOKEN=' in (entry / 'environ').read_bytes()
        deadline = time.monotonic() + 30
        found = agent_processes(in_chat, 'codex')
        while not found and time.monotonic() < deadline:
            time.sleep(0.2)
            found = agent_processes(in_chat, 'codex')
        print('orca-chat-executables: codex ' + json.dumps(found, sort_keys=True), flush=True)
        assert found, 'Orca Codex chat spawned no identifiable process'
        assert set(found.values()) == {CODEX_RUNTIME}, 'Orca chat ran another Codex executable'
    finally:
        if created:
            rpc.call('agentSession.close', {'sessionId': session})
        config.write_bytes(original)
        rpc.call('settings.update', {'agentDefaultArgs': default_arguments})
        provider.shutdown()
    return 0


def peer_executable(client_port, server_port):
    """Name the process image holding the client end of a loopback model connection."""
    for line in Path('/proc/net/tcp').read_text().splitlines()[1:]:
        fields = line.split()
        if int(fields[1].split(':')[1], 16) == client_port and int(fields[2].split(':')[1], 16) == server_port:
            target = 'socket:[' + fields[9] + ']'
            for descriptor in glob.glob('/proc/[0-9]*/fd/*'):
                try:
                    if os.readlink(descriptor) == target:
                        return resolved_exe(descriptor.split('/')[2])
                except OSError:
                    pass
    return 'unknown'


def codex_routing(arguments):
    """Assert where a Codex client from a login shell, as over SSH, runs its turns and tools."""
    spec = importlib.util.spec_from_file_location('provider', arguments.provider)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)

    class Provider(module.Provider):
        def do_POST(self):
            self.server.peers.add(peer_executable(self.client_address[1], self.server.server_port))
            super().do_POST()

    class Client(module.Terminal):
        def __init__(self, client_arguments, cwd):
            self.pid, self.fd = pty.fork()
            if self.pid == 0:
                os.chdir(cwd)
                os.execve('/bin/bash', ['bash', '-lc', 'exec codex "$@"', 'codex', *client_arguments], {
                    'HOME': str(Path.home()), 'USER': os.environ.get('USER', 'dev'), 'LANG': 'C.UTF-8',
                    'PATH': '/usr/local/bin:/usr/bin:/bin', 'TERM': 'xterm-256color'})
            fcntl.ioctl(self.fd, termios.TIOCSWINSZ, struct.pack('HHHH', 40, 140, 0, 0))
            self.buffer = ''

    home = Path.home() / '.codex'
    config = home / 'config.toml'
    package = home / 'packages/app-server-daemon/current/bin/codex'
    daemon_pid = home / 'app-server-daemon/daemon.pid'
    original = config.read_bytes()
    parsed = tomllib.loads(original.decode())
    assert 'model' not in parsed and 'model_provider' not in parsed, 'the yard Codex config names a model'
    root = Path(tempfile.mkdtemp(prefix='subyard-codex-routing-', dir='/tmp'))
    provider = ThreadingHTTPServer(('127.0.0.1', 0), Provider)
    provider.command, provider.failure, provider.workspace, provider.peers = None, None, None, set()
    threading.Thread(target=provider.serve_forever, daemon=True).start()
    command = 'git commit --allow-empty -m routing-fixture'
    plain = ['--no-alt-screen', PROMPT]

    def repository(name):
        path = root / name
        subprocess.run(['git', 'init', '-q', str(path)], check=True, timeout=20)
        subprocess.run(['git', '-C', str(path), 'config', 'user.name', 'Fixture'], check=True, timeout=20)
        subprocess.run(['git', '-C', str(path), 'config', 'user.email', 'fixture@example.invalid'], check=True, timeout=20)
        return path

    def commits(path):
        result = subprocess.run(['git', '-C', str(path), 'rev-list', '--count', 'HEAD'], capture_output=True, text=True, timeout=20)
        return int(result.stdout) if result.returncode == 0 else 0

    def daemon(action):
        result = subprocess.run(['/usr/local/bin/codex', 'app-server', 'daemon', action], cwd=Path.home(), capture_output=True,
                                text=True, timeout=120, stdin=subprocess.DEVNULL, check=False)
        return result.returncode

    def prompted(client, path, before):
        text = client.wait('Press enter to confirm', provider)
        assert command in text, 'the prompt names another command'
        assert commits(path) == before, 'Git ran before approval'

    def turn(name, client_arguments, expected, prompts):
        path = workspaces[name]
        provider.peers.clear()
        provider.workspace, provider.command = path, 'readlink /proc/$PPID/exe > executor.txt; command -v rg > helper.txt'
        client = Client(client_arguments, path)
        try:
            client.wait('Fixture complete.', provider)
            client_executable = resolved_exe(client.pid)
            executor = (path / 'executor.txt').read_text().strip()
            expected = expected or os.path.realpath(package)
            helper = (path / 'helper.txt').read_text().strip()
            assert helper.startswith(os.path.dirname(os.path.dirname(expected)) + '/'), name + ': the package helper is elsewhere: ' + helper
            print('codex-routing: %s: client %s, tool executor %s, model peers %s' % (
                name, client_executable, executor, sorted(provider.peers)), flush=True)
            assert client_executable == CODEX_RUNTIME, name + ': the client is another binary'
            assert executor == expected, name + ': tools ran on an unexpected binary'
            assert provider.peers == {expected}, name + ': the model was reached from an unexpected binary'
            if not prompts:
                return
            client.buffer, provider.command = '', command
            client.submit()
            prompted(client, path, 0)
            client.buffer = ''
            client.settle()
            client.send('\x1b')
            client.wait('interrupted', provider)
            assert commits(path) == 0, name + ': a declined commit ran'
            client.buffer, provider.command = '', command
            client.submit()
            prompted(client, path, 0)
            client.buffer = ''
            client.settle()
            client.send('y')
            client.wait('Fixture complete.', provider)
            assert commits(path) == 1, name + ': an approved commit did not run once'
            print('codex-routing: %s: Git prompt named the command, decline kept history, approval added one commit' % name, flush=True)
        finally:
            client.close()

    def disconnect(name, client_arguments, expected, survives):
        path = workspaces[name]
        provider.workspace, provider.command, provider.command_timeout_ms = path, 'touch started.txt; sleep 15; touch done.txt', 60000
        client = Client(client_arguments, path)
        try:
            deadline = time.monotonic() + module.TIMEOUT
            while not (path / 'started.txt').exists():
                assert time.monotonic() < deadline, name + ': the slow tool did not start'
                client.read()
            provider.peers.clear()
            os.kill(client.pid, signal.SIGKILL)
            time.sleep(25)
            assert (path / 'done.txt').exists() == survives, name + ': the tool outcome after the disconnect changed'
            assert provider.peers == ({expected} if survives else set()), name + ': unexpected requests after the disconnect'
            print('codex-routing: %s: client killed during a slow tool; tool %s, model peers %s' % (
                name, 'finished' if survives else 'did not finish', sorted(provider.peers)), flush=True)
        finally:
            client.close()

    def daemon_executable():
        return resolved_exe(json.loads(daemon_pid.read_text())['pid'])

    names = ('plain', 'managed', 'plain-disconnect', 'managed-disconnect') if arguments.package == 'absent' \
        else ('plain', 'managed', 'bypass', 'stopped')
    workspaces = {name: repository(name) for name in names}
    features = '' if 'features' in parsed else 'features.plugins = false\nfeatures.unified_exec = false\n'
    # A -c override keeps the client in-process, so this fixture configures the model in the file.
    config.write_bytes(('model = "subyard-fixture"\nmodel_provider = "fixture"\n' + features).encode() + original + (
        '\n[model_providers.fixture]\nname = "Fixture"\n'
        f'base_url = "http://127.0.0.1:{provider.server_port}/v1"\n'
        'wire_api = "responses"\nrequires_openai_auth = false\n'
        'request_max_retries = 0\nstream_max_retries = 0\n' + ''.join(
            f'[projects."{path}"]\ntrust_level = "trusted"\n' for path in workspaces.values())).encode())
    try:
        managed = ['--no-daemon', *plain]
        if arguments.package == 'absent':
            assert not package.exists(), 'the fresh yard already has a home package'
            turn('plain', plain, None, True)
            binary = os.path.realpath(package)
            assert binary.startswith(str(home / 'packages/app-server-daemon/releases') + '/'), 'the home copy moved: ' + binary
            assert daemon_executable() == binary, 'a plain client started another daemon binary'
            print('codex-routing: a plain client copied the package and started its daemon, which outlives it: %s' % binary, flush=True)
            turn('managed', managed, CODEX_RUNTIME, True)
            disconnect('plain-disconnect', plain, binary, True)
            disconnect('managed-disconnect', managed, CODEX_RUNTIME, False)
        else:
            binary = os.path.realpath(package)
            assert daemon('restart') == 0, 'the package daemon did not restart'
            assert daemon_executable() == binary, 'the daemon runs another binary'
            turn('plain', plain, binary, True)
            turn('bypass', ['--dangerously-bypass-approvals-and-sandbox', *plain], binary, True)
            turn('managed', managed, CODEX_RUNTIME, True)
            assert daemon('stop') == 0, 'the package daemon did not stop'
            assert not daemon_pid.exists(), 'the stopped daemon left its record'
            turn('stopped', plain, binary, False)
            assert daemon_executable() == binary, 'a plain client started another daemon binary'
            print('codex-routing: a plain client started the stopped daemon, which outlives it: %s' % binary, flush=True)
    finally:
        daemon('stop')
        config.write_bytes(original)
        provider.shutdown()
        provider.server_close()
        shutil.rmtree(root, ignore_errors=True)
    return 0


def envelope(method, session, fields, fence=None):
    payload = {'method': method, 'sessionId': session, 'fields': fields}
    digest = hashlib.sha256(json.dumps(payload, sort_keys=True, separators=(',', ':')).encode()).hexdigest()
    # Stock Orca's durable ledger requires a timestamp and 32 hex entropy digits.
    operation_id = str(time.time_ns() // 1_000_000) + '-' + uuid.uuid4().hex
    return {'sessionId': session, 'clientOperationId': operation_id,
            'expectedRuntimeFence': fence, 'payloadFingerprint': digest}


def accepted(result, operation):
    if result.get('ok') is not True:
        refusal = result.get('refusal', {})
        # Only closed protocol enums are safe to report; messages may contain runtime data.
        reason = refusal.get('details', {}).get('reason')
        print('orca-chat-refusal: ' + json.dumps({
            'operation': operation, 'reason': reason if reason in {
                'cliMissing', 'notSignedIn', 'providerStartFailed', 'providerRejected',
                'managedAccountUnsupported', 'managedAccountEnvOverride', 'attachFailed',
                'hostDisabled', 'hostUnsupported', 'clientCapabilityMissing'} else 'unspecified'},
            sort_keys=True), flush=True)
        raise AssertionError(operation + ' was refused')
    return result


def chat_diagnostic(page, server):
    # The chat and its tool output contain only this fixture's commands.
    print('orca-chat-diagnostic: ' + json.dumps({
        'model_requests': server.requests, 'tool_results': server.tool_results[-2:],
        'request_log': server.request_log[-6:],
        'items': [{key: value for key, value in item['body'].items()
                   if key in ('kind', 'role', 'state', 'title', 'output', 'blocks', 'resolution')}
                  for item in page['items'][-8:]]}, sort_keys=True, default=str)[:9000], flush=True)


def chat(arguments):
    """Require a fresh native prompt for every Git operation in stock Orca's Claude chat."""
    sys.path.insert(0, '/usr/local/libexec/subyard/orca-registration')
    from transport import RuntimeRPC
    runtime = '/srv/agents/orca/config/orca/orca-runtime.json'
    rpc = RuntimeRPC(runtime, timeout=30)
    workspace = Path(arguments.workspace).resolve()
    repo = next(item for item in rpc.call('repo.list')['repos'] if item['path'] == str(workspace))
    worktree = 'id:' + repo['id'] + '::' + str(workspace)
    support = rpc.call('agentSession.createSupport', {'worktree': worktree, 'agent': 'claude'})
    assert support.get('supported') is True, 'stock Orca native Claude chat is unavailable'

    def git(*command):
        result = subprocess.run(['git', '-C', str(workspace), *command], capture_output=True,
                                text=True, timeout=10)
        return result.stdout.strip() if result.returncode == 0 else None

    current = [None]
    server = model(lambda: current[0])
    settings_path = SETTINGS
    original = settings_path.read_bytes()
    managed_before = managed_settings()
    # Orca's sign-in gate accepts this token source; the base URL keeps it on loopback.
    settings = json.loads(original)
    settings['env'] = {**settings.get('env', {}), 'ANTHROPIC_AUTH_TOKEN': TOKEN,
                       'ANTHROPIC_BASE_URL': 'http://127.0.0.1:' + str(server.server_port)}
    session = str(uuid.uuid4())
    created = False
    try:
        settings_path.write_text(json.dumps(settings))
        fields = {'worktree': worktree, 'agent': 'claude'}
        accepted(rpc.call('agentSession.create', {
            **fields, 'envelope': envelope('agentSession.create', session, fields)}), 'create')
        created = True

        def history():
            return accepted(rpc.call('agentSession.history', {
                'sessionId': session, 'direction': 'tail'}), 'history')['page']

        # A rule added for the session must not suppress the next prompt.
        for index, (command, choice) in enumerate([
                ('git commit --allow-empty -m denied', 'deny'),
                ('git commit --allow-empty -m first', 'allow'),
                ('git commit --allow-empty -m repeated', 'allowForSession'),
                ('git commit --allow-empty -m after-session-rule', 'allow'),
                ('git commit --amend --allow-empty -m amended', 'allow'),
                ('git commit --amend --allow-empty -m amended-again', 'deny'),
                ('git push', 'deny'), ('git push', 'allowForSession'), ('git push', 'deny')]):
            before = git('rev-parse', '--verify', 'HEAD')
            commits_before = int(git('rev-list', '--count', 'HEAD'))
            current[0] = command
            requests = server.requests
            body = {'kind': 'message', 'role': 'user', 'blocks': [{'type': 'text', 'text': PROMPT}]}
            page = history()
            previous_turn = (page.get('latestTurn') or {}).get('itemId')
            accepted(rpc.call('agentSession.send', {
                'envelope': envelope('agentSession.send', session, {'body': body}, page['fence']),
                'body': body}), 'send')
            deadline = time.monotonic() + 45
            prompt = None
            finished_since = None
            while time.monotonic() < deadline and prompt is None:
                page = history()
                prompts = [item for item in page['items'] if item['body'].get('kind') == 'approval'
                           and item['body']['resolution']['state'] == 'pending']
                assert len(prompts) <= 1, 'duplicate native chat approval'
                prompt = prompts[0] if prompts else None
                if prompt is None:
                    latest = page.get('latestTurn') or {}
                    finished = (server.requests > requests and latest.get('itemId') != previous_turn
                                and latest.get('turn', {}).get('state') == 'completed')
                    # Only a settled turn counts; one early read must not decide this assertion.
                    finished_since = (finished_since or time.monotonic()) if finished else None
                    settled = finished_since is not None and time.monotonic() - finished_since > 5
                    moved = git('rev-parse', '--verify', 'HEAD') != before
                    if moved or settled:
                        chat_diagnostic(page, server)
                    # No approval and a moved HEAD is the finding this lane exists to catch.
                    assert not moved, 'FINDING: Orca Claude chat ran Git without approval'
                    assert not settled, 'Orca Claude chat finished a Git turn without showing an approval'
                    time.sleep(0.1)
            assert prompt, 'native Claude chat did not request Git approval'
            options = [option['id'] for option in prompt['body']['options']]
            assert {'allow', 'allowForSession', 'deny'} <= set(options) <= {
                'allow', 'allowForSession', 'deny', 'cancel'}, options
            assert git('rev-parse', '--verify', 'HEAD') == before, 'chat executed Git before approval'
            assert command in json.dumps(prompt['body']), 'native chat approval names another command'
            if index == 0:
                def in_chat(entry):
                    return b'ORCA_AGENT_SESSION_SPAWN_TOKEN=' in (entry / 'environ').read_bytes()
                found = agent_processes(in_chat)
                print('orca-chat-executables: ' + json.dumps(found, sort_keys=True), flush=True)
                assert found and set(found.values()) == {RUNTIME}, 'Orca chat ran another Claude executable'
            turn = (page.get('latestTurn') or {}).get('itemId')
            reconnected = RuntimeRPC(runtime, timeout=30)
            reread = accepted(reconnected.call('agentSession.history', {
                'sessionId': session, 'direction': 'tail'}), 'reconnect-history')['page']
            fields = {'itemId': prompt['itemId'], 'expectedRevision': prompt['revision'], 'optionId': choice}
            accepted(reconnected.call('agentSession.respondToApproval', {
                **fields, 'envelope': envelope('agentSession.respondTo:approval', session, fields,
                                              reread['fence'])}), 'respond')
            deadline = time.monotonic() + 45
            while time.monotonic() < deadline:
                latest = history().get('latestTurn') or {}
                if latest.get('itemId') == turn and latest.get('turn', {}).get('state') in (
                        'completed', 'interrupted'):
                    break
                time.sleep(0.1)
            else:
                raise AssertionError('native chat approval turn did not finish')
            after = git('rev-parse', '--verify', 'HEAD')
            expected_change = choice != 'deny' and command != 'git push'
            assert (after != before) == expected_change, 'native chat approval effect mismatch: ' + command
            added = 1 if expected_change and '--amend' not in command else 0
            assert int(git('rev-list', '--count', 'HEAD')) == commits_before + added, (
                'native chat approval commit count mismatch: ' + command)
            print('ok: native Claude chat approval ' + str(index + 1) + ' ' + choice + ': ' + command, flush=True)
        assert managed_settings() == managed_before, 'Orca chat changed managed Claude settings'
    finally:
        if created:
            rpc.call('agentSession.close', {'sessionId': session})
        settings_path.write_bytes(original)
        server.shutdown()
    return 0


def main():
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest='operation', required=True)
    launch_command = commands.add_parser('launch')
    launch_command.add_argument('--cli', required=True)
    launch_command.add_argument('--xdg', required=True)
    launch_command.add_argument('--seconds', type=int, default=30)
    launch_command.add_argument('--expect-link', action='store_true')
    launch_command.add_argument('--pid-file')
    launch_command.set_defaults(handler=launch)
    serve_command = commands.add_parser('serve')
    serve_command.add_argument('--port-file', required=True)
    serve_command.add_argument('--command-file', required=True)
    serve_command.set_defaults(handler=serve)
    exe_command = commands.add_parser('terminal-exe')
    exe_command.add_argument('--agent', choices=('claude', 'codex'), default='claude')
    exe_command.add_argument('--seconds', type=int, default=0)
    exe_command.add_argument('workspace')
    exe_command.set_defaults(handler=terminal_exe)
    exe_of_command = commands.add_parser('exe-of')
    exe_of_command.add_argument('--expect', required=True)
    exe_of_command.add_argument('command', nargs=argparse.REMAINDER)
    exe_of_command.set_defaults(handler=exe_of)
    hold_command = commands.add_parser('hold')
    hold_command.add_argument('--pid-file', required=True)
    hold_command.add_argument('command', nargs=argparse.REMAINDER)
    hold_command.set_defaults(handler=hold)
    codex_chat_command = commands.add_parser('codex-chat')
    codex_chat_command.add_argument('--provider', required=True)
    codex_chat_command.add_argument('workspace')
    codex_chat_command.set_defaults(handler=codex_chat)
    routing_command = commands.add_parser('codex-routing')
    routing_command.add_argument('--provider', required=True)
    routing_command.add_argument('--package', choices=('absent', 'present'), required=True)
    routing_command.set_defaults(handler=codex_routing)
    chat_command = commands.add_parser('chat')
    chat_command.add_argument('workspace')
    chat_command.set_defaults(handler=chat)
    arguments = parser.parse_args()
    return arguments.handler(arguments)


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except Exception as error:
        # All fixture data is synthetic, but upstream exceptions can include runtime details.
        print('managed-agents: ' + (str(error) if isinstance(error, AssertionError)
                                    else type(error).__name__), file=sys.stderr)
        raise SystemExit(1)
