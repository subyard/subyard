#!/usr/bin/env python3
"""Real Codex approval checks with deterministic loopback model responses only."""
import argparse
import http.server
import json
import os
from pathlib import Path
import queue
import pty
import re
import select
import signal
import struct
import termios
import fcntl
import shlex
import shutil
import subprocess
import tempfile
import threading
import time

TIMEOUT = 30
FIXTURE_CONFIG = ['-c', 'model="subyard-fixture"', '-c', 'model_provider="fixture"',
                  '-c', 'features.unified_exec=false', '-c', 'features.plugins=false']


def run(args, **kwargs):
    return subprocess.run(args, capture_output=True, text=True, timeout=TIMEOUT, check=True, **kwargs).stdout.strip()


class Provider(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_POST(self):
        body = self.rfile.read(int(self.headers['Content-Length']))
        try:
            request = json.loads(body)
            fixture = self.server
            if fixture.command is not None and request.get('tools'):
                command, fixture.command = fixture.command, None
                tools = request.get('tools', [])
                flattened = []
                for tool in tools:
                    if tool.get('type') == 'namespace':
                        flattened.extend(tool.get('tools', []))
                    else:
                        flattened.append(tool)
                names = [t.get('name') for t in flattened]
                if 'shell' in names:
                    name, arguments = 'shell', {'command': ['bash', '-lc', command], 'workdir': str(fixture.workspace), 'timeout_ms': 10000}
                elif 'shell_command' in names:
                    name, arguments = 'shell_command', {'command': command, 'workdir': str(fixture.workspace), 'timeout_ms': 10000}
                elif 'exec_command' in names:
                    name, arguments = 'exec_command', {'cmd': command, 'workdir': str(fixture.workspace), 'yield_time_ms': 1000}
                else:
                    raise RuntimeError('no shell tool: ' + repr(names))
                item = {'type': 'function_call', 'id': 'fc_fixture', 'call_id': 'call_fixture', 'name': name, 'arguments': json.dumps(arguments)}
            else:
                item = {'type': 'message', 'id': 'msg_fixture', 'role': 'assistant', 'content': [{'type': 'output_text', 'text': 'Fixture complete.' if request.get('tools') else 'Subyard fixture'}]}
            self.send_response(200)
            self.send_header('Content-Type', 'text/event-stream')
            self.end_headers()
            for event in [
                {'type': 'response.created', 'response': {'id': 'resp_fixture'}},
                {'type': 'response.output_item.done', 'output_index': 0, 'item': item},
                {'type': 'response.completed', 'response': {'id': 'resp_fixture', 'status': 'completed', 'output': [item], 'usage': {'input_tokens': 1, 'output_tokens': 1, 'total_tokens': 2}}},
            ]:
                self.wfile.write(('data: ' + json.dumps(event) + '\n\n').encode())
            self.wfile.flush()
        except Exception as error:
            self.server.failure = str(error)
            self.send_error(500)


class App:
    def __init__(self, argv, env, cwd):
        self.stderr = tempfile.TemporaryFile(mode='w+')
        self.process = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=self.stderr, text=True, env=env, cwd=cwd, start_new_session=True)
        self.frames = queue.Queue()
        def reader():
            for line in self.process.stdout:
                self.frames.put(json.loads(line))
            self.frames.put({'closed': True})
        threading.Thread(target=reader, daemon=True).start()
        self.serial = 0
        self.request('initialize', {'clientInfo': {'name': 'subyard-fixture', 'version': '1'}, 'capabilities': {'experimentalApi': True}})
        self.send({'method': 'initialized', 'params': {}})

    def send(self, frame):
        self.process.stdin.write(json.dumps(frame) + '\n')
        self.process.stdin.flush()

    def read(self):
        try:
            frame = self.frames.get(timeout=TIMEOUT)
        except queue.Empty:
            raise RuntimeError('Codex response timed out') from None
        if frame.get('closed'):
            self.stderr.seek(0)
            raise RuntimeError('Codex closed: ' + self.stderr.read()[-1800:])
        return frame

    def request(self, method, params, reject=False):
        self.serial += 1
        self.send({'id': self.serial, 'method': method, 'params': params})
        while True:
            frame = self.read()
            if frame.get('id') != self.serial or 'method' in frame:
                continue
            if reject:
                assert 'error' in frame, 'unsafe override was accepted: ' + method
                return
            if 'error' in frame:
                raise RuntimeError(method + ': ' + json.dumps(frame['error']))
            return frame['result']

    def turn(self, provider, thread, workspace, command, decision=None, unchanged=None):
        provider.workspace, provider.command = workspace, command
        self.request('turn/start', {'threadId': thread, 'input': [{'type': 'text', 'text': 'Run the fixture command.'}]})
        approvals = 0
        while True:
            frame = self.read()
            method = frame.get('method', '')
            if method == 'item/commandExecution/requestApproval':
                approvals += 1
                assert decision is not None, 'local work unexpectedly requested approval'
                assert approvals == 1, 'duplicate approval for one command'
                if unchanged:
                    unchanged()
                self.send({'id': frame['id'], 'result': {'decision': decision}})
            elif method == 'turn/completed':
                assert not provider.failure, provider.failure
                assert frame['params']['turn']['status'] == 'completed', 'turn failed: ' + json.dumps(frame['params']['turn'].get('error'))
                assert approvals == (0 if decision is None else 1), 'protected command did not request approval'
                return

    def close(self):
        os.killpg(self.process.pid, signal.SIGTERM)
        try:
            self.process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            os.killpg(self.process.pid, signal.SIGKILL)
            self.process.wait(timeout=5)
        self.stderr.close()


class Terminal:
    def __init__(self, executable, env, cwd, orca=False):
        self.pid, self.fd = pty.fork()
        if self.pid == 0:
            os.chdir(cwd)
            env = dict(env, TERM="xterm-256color")
            argv = [executable, *FIXTURE_CONFIG, "--no-alt-screen", "-c", "check_for_update_on_startup=false", "Run the fixture command."]
            if orca:
                assert os.environ.get("SUBYARD_ORCA_CODEX_CONFIG") == "1", "missing actual Orca service environment"
                env["SUBYARD_ORCA_CODEX_CONFIG"] = "1"
                argv = ["bash", "-lc", shlex.join(["codex", "--dangerously-bypass-approvals-and-sandbox", *argv[1:]])]
            os.execvpe(argv[0], argv, env)
        fcntl.ioctl(self.fd, termios.TIOCSWINSZ, struct.pack("HHHH", 40, 140, 0, 0))
        self.buffer = ""

    def send(self, text):
        os.write(self.fd, text.encode())

    def read(self, seconds=0.1):
        if not select.select([self.fd], [], [], seconds)[0]:
            return
        try:
            data = os.read(self.fd, 65536)
        except OSError:
            raise RuntimeError("Codex terminal closed: " + self.text()[-1500:]) from None
        if b"\x1b[6n" in data:
            self.send("\x1b[1;1R")
        if b"\x1b[c" in data:
            self.send("\x1b[?1;2c")
        self.buffer = (self.buffer + data.decode(errors="replace"))[-200000:]

    def text(self):
        return re.sub(r"\x1b\[[0-?]*[ -/]*[@-~]", "", self.buffer)

    def wait(self, phrase, provider=None):
        deadline = time.monotonic() + TIMEOUT
        while time.monotonic() < deadline:
            self.read()
            if provider and provider.failure:
                raise RuntimeError('provider: ' + provider.failure)
            if phrase in self.text():
                return self.text()
        raise RuntimeError("terminal did not show " + phrase + ": " + self.text()[-1800:])

    def settle(self):
        deadline = time.monotonic() + 0.5
        while time.monotonic() < deadline:
            self.read(0.05)

    def submit(self):
        self.settle()
        self.send('Run the fixture command.')
        self.settle()
        self.send('\r')

    def close(self):
        try:
            os.killpg(self.pid, signal.SIGTERM)
            deadline = time.monotonic() + 5
            while time.monotonic() < deadline:
                if os.waitpid(self.pid, os.WNOHANG)[0]:
                    return
                time.sleep(0.05)
            os.killpg(self.pid, signal.SIGKILL)
            os.waitpid(self.pid, 0)
        except ProcessLookupError:
            pass
        finally:
            os.close(self.fd)


def terminal_checks(executable, env, provider, workspace, orca):
    def head():
        result = subprocess.run(['git', '-C', str(workspace), 'rev-parse', '--verify', 'HEAD'], capture_output=True, text=True, timeout=10)
        return result.stdout.strip() if result.returncode == 0 else None

    def prompt(terminal, before):
        text = terminal.wait("Press enter to confirm", provider)
        assert "Would you like to run" in text, 'not a command approval'
        assert "don't ask" not in text.lower() and "for this session" not in text.lower(), 'persistent approval choice exposed'
        assert head() == before, 'command changed history before approval'

    command = "git commit --allow-empty -m terminal-fixture"
    provider.workspace, provider.command = workspace, 'touch terminal-local-work'
    terminal = Terminal(executable, env, workspace, orca)
    try:
        terminal.wait('Fixture complete.', provider)
        assert (workspace / 'terminal-local-work').exists(), 'terminal local command did not run'
        assert 'Would you like to run' not in terminal.text(), 'local command prompted'
        terminal.buffer = ''
        provider.command = command
        terminal.submit()
        prompt(terminal, None)
        terminal.buffer = ''
        terminal.send('\x1b')
        terminal.wait('interrupted', provider)
        assert head() is None, 'declined terminal command ran'
    finally:
        terminal.close()

    provider.command = command
    terminal = Terminal(executable, env, workspace, orca)
    try:
        for index, protected in enumerate([command, command, 'git commit --amend --allow-empty -m amended', 'git push', 'git push']):
            before = head()
            if index:
                terminal.buffer = ''
                provider.command = protected
                terminal.submit()
            prompt(terminal, before)
            terminal.buffer = ''
            terminal.settle()
            terminal.send('y')
            terminal.wait('Fixture complete.', provider)
            if index < 2:
                assert run(['git', '-C', str(workspace), 'rev-list', '--count', 'HEAD']) == str(index + 1), 'approved commit missing'
            elif index == 2:
                assert head() != before, 'approved amend missing'
            else:
                assert head() == before, 'no-remote push changed history'
        print('ok: terminal local work, denial, repeated commit/push and amend approvals', flush=True)
    finally:
        terminal.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('workspace', nargs=2, type=Path)
    parser.add_argument('--terminal', action='store_true')
    parser.add_argument('--orca', action='store_true')
    parser.add_argument('--codex', default='codex')
    parser.add_argument('--template', type=Path, default=Path('/home/dev/.codex/config.toml'))
    parser.add_argument('--home-rules', type=Path)
    args = parser.parse_args()
    executable = shutil.which(args.codex)
    assert executable, 'native Codex unavailable'
    with tempfile.TemporaryDirectory(prefix='subyard-codex-approval-', dir=args.workspace[0]) as temporary:
        root = Path(temporary).resolve()
        home = root / 'home'
        home.mkdir()
        codex_home = home / '.codex'
        codex_home.mkdir()
        template = args.template.read_text()
        provider = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Provider)
        provider.command, provider.failure = None, None
        threading.Thread(target=provider.serve_forever, daemon=True).start()
        # The template supplies the same policy as VS Code Custom; only model transport is synthetic.
        # Runtime-owned tables can follow the template's root settings; use CLI model overrides
        # so fixture keys cannot accidentally become members of the last TOML table.
        (codex_home / 'config.toml').write_text(template + f'\n[model_providers.fixture]\nname = "Fixture"\nbase_url = "http://127.0.0.1:{provider.server_port}/v1"\nwire_api = "responses"\nrequires_openai_auth = false\nrequest_max_retries = 0\nstream_max_retries = 0\n')
        if args.home_rules:
            (codex_home / 'rules').mkdir()
            shutil.copyfile(args.home_rules, codex_home / 'rules/repo.rules')
        env = {'HOME': str(home), 'CODEX_HOME': str(codex_home), 'PATH': os.environ['PATH'], 'LANG': 'C.UTF-8'}
        try:
            for base in map(Path.resolve, args.workspace):
                assert base.is_dir(), 'missing fixture workspace'
                workspace = base / ('codex-orca' if args.orca else 'codex-terminal' if args.terminal else 'codex-appserver')
                workspace.mkdir()
                assert not (workspace / '.git').exists(), 'refusing existing Git metadata'
                run(['git', 'init', '-q', str(workspace)])
                run(['git', '-C', str(workspace), 'config', 'user.name', 'Fixture'])
                run(['git', '-C', str(workspace), 'config', 'user.email', 'fixture@example.invalid'])
                with (codex_home / 'config.toml').open('a') as config:
                    config.write('\n[projects.' + json.dumps(str(workspace)) + ']\ntrust_level = "trusted"\n')
                if args.terminal or args.orca:
                    terminal_checks(executable, env, provider, workspace, args.orca)
                    continue
                app = App([executable, *FIXTURE_CONFIG, 'app-server'], env, workspace)
                try:
                    started = app.request('thread/start', {'cwd': str(workspace)})
                    assert started['modelProvider'] == 'fixture', 'fixture model provider was not selected'
                    assert started['approvalPolicy'] == 'on-request', 'effective approval policy drift'
                    assert started['approvalsReviewer'] == 'user', 'effective reviewer drift'
                    assert started['sandbox']['type'] == 'dangerFullAccess', 'yard sandbox drift'
                    if not args.home_rules:
                        requirements = app.request('configRequirements/read', {})['requirements']
                        assert requirements['allowedApprovalPolicies'] == ['on-request'], 'managed policy was not loaded'
                    thread = started['thread']['id']
                    marker = workspace / 'local-work'
                    app.turn(provider, thread, workspace, 'touch local-work')
                    assert marker.exists(), 'local command did not run'
                    def no_commit():
                        result = subprocess.run(['git', '-C', str(workspace), 'rev-parse', '--verify', 'HEAD'], capture_output=True, timeout=10)
                        assert result.returncode != 0, 'commit ran before approval'
                    command = 'git commit --allow-empty -m fixture'
                    app.turn(provider, thread, workspace, command, 'decline', no_commit)
                    no_commit()
                    app.turn(provider, thread, workspace, command, 'accept', no_commit)
                    first = run(['git', '-C', str(workspace), 'rev-parse', 'HEAD'])
                    def same_commit():
                        assert run(['git', '-C', str(workspace), 'rev-parse', 'HEAD']) == first, 'history changed before approval'
                    app.turn(provider, thread, workspace, command, 'accept', same_commit)
                    assert run(['git', '-C', str(workspace), 'rev-list', '--count', 'HEAD']) == '2'
                    first = run(['git', '-C', str(workspace), 'rev-parse', 'HEAD'])
                    app.turn(provider, thread, workspace, 'git commit --amend --allow-empty -m amended', 'decline', same_commit)
                    same_commit()
                    # No remote is configured: push approval must precede even the failing Git command.
                    for _ in range(2):
                        app.turn(provider, thread, workspace, 'git push', 'decline', same_commit)
                    if not args.home_rules:
                        for overrides in ({'approvalPolicy': 'never'}, {'approvalsReviewer': 'auto_review'}):
                            app.request('turn/start', {'threadId': thread, **overrides, 'input': [{'type': 'text', 'text': 'Unsafe policy fixture.'}]}, reject=True)
                    print('ok: local work, deny, repeated commit/push prompts and amend for ' + workspace.name, flush=True)
                finally:
                    app.close()
        finally:
            provider.shutdown()
            provider.server_close()
    print('ok: codex-permissions complete', flush=True)


if __name__ == '__main__':
    main()
