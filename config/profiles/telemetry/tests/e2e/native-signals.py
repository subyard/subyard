#!/usr/bin/env python3
"""Real native exporters, isolated homes, deterministic local providers only."""
import argparse
from contextlib import closing
import hashlib
import http.server
import importlib.util
import io
import json
import os
from pathlib import Path
import pwd
import queue
import re
import shutil
import shlex
import sqlite3
import subprocess
import sys
import tarfile
import threading
import time


def load(path, name):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class ClaudeProvider(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_POST(self):
        size = int(self.headers.get('Content-Length', '0'))
        if not 0 < size <= 4*1024*1024:
            self.send_error(413)
            return
        request = json.loads(self.rfile.read(size))
        if self.path.split('?')[0].endswith('/count_tokens'):
            content = b'{"input_tokens":1}'
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
        elif self.path.split('?')[0].endswith('/messages'):
            self.server.requests += 1
            message = dict(id='msg_subyard_probe', type='message', role='assistant', model=request['model'],
                           content=[], stop_reason=None, stop_sequence=None, usage=dict(input_tokens=1, output_tokens=0))
            events = [('message_start', dict(message=message)),
                      ('content_block_start', dict(index=0, content_block=dict(type='text', text=''))),
                      ('content_block_delta', dict(index=0, delta=dict(type='text_delta', text='probe'))),
                      ('content_block_stop', dict(index=0)),
                      ('message_delta', dict(delta=dict(stop_reason='end_turn', stop_sequence=None), usage=dict(output_tokens=1))),
                      ('message_stop', {})]
            command = getattr(self.server, 'command', None)
            if command is not None:
                self.server.command = None
                events = [('message_start', dict(message=message)),
                          ('content_block_start', dict(index=0, content_block=dict(type='tool_use', id='toolu_subyard', name='Bash', input={}))),
                          ('content_block_delta', dict(index=0, delta=dict(type='input_json_delta', partial_json=json.dumps(dict(command=command, description='Run the disposable local fixture', timeout=600000))))),
                          ('content_block_stop', dict(index=0)),
                          ('message_delta', dict(delta=dict(stop_reason='tool_use', stop_sequence=None), usage=dict(output_tokens=1))),
                          ('message_stop', {})]
            content = ''.join('event: '+kind+'\ndata: '+json.dumps(dict(type=kind, **body))+'\n\n' for kind, body in events).encode()
            self.send_response(200)
            self.send_header('Content-Type', 'text/event-stream')
        else:
            self.send_error(404)
            return
        self.send_header('Content-Length', str(len(content)))
        self.end_headers()
        self.wfile.write(content)


def query(data, sql, args=()):
    with closing(sqlite3.connect('file:'+str(data/'telemetry.sqlite')+'?mode=ro', uri=True, timeout=1)) as db:
        return db.execute(sql, args).fetchall()


def session_failure_metadata(data, agent, expected, stderr):
    selected = sorted(expected)[:8]
    digest = lambda value: hashlib.sha256(value.encode()).hexdigest()
    result = dict(schema='subyard.telemetry.session-binding-failure/v1', agent=agent,
                  expected_count=len(expected), expected_digests=[digest(value) for value in selected],
                  digests_truncated=len(expected) > len(selected))
    error_text = stderr[:65536].decode('utf-8', errors='replace').lower()
    result['stderr_truncated'] = len(stderr) > 65536
    result['stderr_error_classes'] = {label: error_text.count(text) for label, text in (
        ('authentication_required', 'telemetry authentication required'),
        ('invalid_run_context', 'invalid run context'),
        ('conflicting_run_context', 'conflicting resource and header run context'),
        ('registration_pending', 'run registration pending'),
        ('exporter_error', '[3p telemetry]'), ('connection_refused', 'econnrefused'),
        ('timeout', 'timed out')) if text in error_text}
    deadline = time.perf_counter()+.25
    try:
        with closing(sqlite3.connect('file:'+str(data/'telemetry.sqlite')+'?mode=ro', uri=True, timeout=.1)) as db:
            db.set_progress_handler(lambda: int(time.perf_counter() >= deadline), 1000)
            classification = "CASE coalesce(json_extract(runs.body,'$.agent'),'missing') WHEN 'codex' THEN 'codex' WHEN 'claude' THEN 'claude' WHEN 'other' THEN 'other' WHEN 'missing' THEN 'missing' ELSE 'unknown' END"
            for label, source in (('managed_runs', 'runs'), ('relations', 'relations LEFT JOIN runs USING(agent_run_id)')):
                result[label] = dict(db.execute('SELECT '+classification+',count(*) FROM '+source+' GROUP BY 1').fetchall())
            result['otlp_signals'] = dict(db.execute("SELECT kind,count(*) FROM records WHERE kind IN ('otlp/logs','otlp/metrics','otlp/traces') GROUP BY kind").fetchall())
            placeholders = ','.join('?' for _ in selected)
            matched = db.execute("SELECT DISTINCT relations.session_id FROM relations JOIN runs USING(agent_run_id) WHERE json_extract(runs.body,'$.agent')=? AND relations.session_id IN ("+placeholders+') LIMIT 8', (agent, *selected)).fetchall()
            result['matched_count'] = len(matched)
            result['matched_digests'] = sorted(digest(row[0]) for row in matched)
    except sqlite3.Error:
        result['database'] = 'unavailable'
    return result


def wait_sessions(data, agent, expected, stderr=b''):
    deadline = time.monotonic()+40
    while time.monotonic() < deadline:
        rows = query(data, 'SELECT relations.agent_run_id,relations.session_id FROM relations JOIN runs USING(agent_run_id) WHERE json_extract(runs.body,\'$.agent\')=?', (agent,))
        found = {row[1] for row in rows}
        if expected <= found:
            return {row[0] for row in rows if row[1] in expected}
        time.sleep(0.2)
    try:
        print(json.dumps(session_failure_metadata(data, agent, expected, stderr), sort_keys=True), flush=True)
    except Exception:
        pass  # Diagnostics must not replace the original binding failure.
    raise AssertionError(agent+': native exported session IDs did not bind to managed runs')


def wait_shared_scope(data, run_id, timeout=40):
    deadline = time.monotonic()+timeout
    while time.monotonic() < deadline:
        rows = query(data, "SELECT json_extract(body,'$.shared') FROM objects WHERE object_id=?", (run_id+':scope',))
        if rows:
            shared = 'true' if rows[0][0] == 1 else 'false' if rows[0][0] == 0 else 'missing_or_invalid'
            if shared == 'true':
                return
            raise AssertionError(json.dumps({'schema': 'subyard.telemetry.shared-scope-failure/v1',
                'reason': 'shared_scope_not_shared', 'scope_present': True, 'shared': shared}))
        time.sleep(.2)
    raise AssertionError(json.dumps({'schema': 'subyard.telemetry.shared-scope-failure/v1',
        'reason': 'shared_scope_projection_missing', 'scope_present': False, 'shared': 'not_observed'}))


def install_agents(args, state):
    binaries = state/'bin'
    binaries.mkdir()
    provision = dict(DEV_USER=pwd.getpwuid(os.getuid()).pw_name, CODEX_INSTALL_ROOT=str(state/'codex-native'),
                     CODEX_BIN_LINK=str(binaries/'codex'), CODEX_CHECK_PATH=str(binaries/'codex-check'),
                     CODEX_REQUIREMENTS_PATH=str(state/'codex-requirements/requirements.toml'))
    subprocess.run(['sudo', '-n', 'env', *[key+'='+value for key, value in provision.items()],
                    'bash', str(args.source/'config/agents/codex/provision.sh')], check=True, timeout=180)
    arch = os.uname().machine
    platforms = {'x86_64': ('linux-x64', '3920489a5109cff5786a1a392c25277408ff22bc796d5edb9c16a60e5a1718f0'),
                 'aarch64': ('linux-arm64', 'e4daf793d1e74fb0d9874dd09e98690bbfd7be515f78a87fd05b9e2b4bb33b03')}
    platform, digest = platforms[arch]
    archive = state/'claude-native.tgz'
    url = f'https://registry.npmjs.org/@anthropic-ai/claude-code-{platform}/-/claude-code-{platform}-2.1.287.tgz'
    subprocess.run(['curl', '--fail', '--silent', '--show-error', '--location', '--proto', '=https', '--proto-redir', '=https',
                    '--connect-timeout', '20', '--max-time', '120', '--output', str(archive), url], check=True, timeout=125)
    with tarfile.open(archive, 'r:gz') as package, (binaries/'claude').open('wb') as output:
        member = package.getmember('package/claude')
        assert member.isfile() and member.size < 512*1024*1024
        with package.extractfile(member) as content:
            shutil.copyfileobj(content, output)
    assert hashlib.sha256((binaries/'claude').read_bytes()).hexdigest() == digest
    (binaries/'claude').chmod(0o755)
    versions = {}
    for agent in ('codex', 'claude'):
        version = subprocess.run([str(binaries/agent), '--version'], capture_output=True, check=True, timeout=10).stdout.decode().strip()
        versions[agent] = dict(version=version, sha256=hashlib.sha256((binaries/agent).read_bytes()).hexdigest())
    (state/'native-versions.json').write_text(json.dumps(versions, indent=2)+'\n')
    return binaries


def command_with_receipt(command, receipt, expected=None):
    code = '''import hashlib,json,os,pathlib,re,signal,subprocess,sys
expected=json.loads(sys.argv[2])
context={}
for name,value in expected.get('environment',{}).items():
    context[name.lower()+'_present']=bool(os.environ.get(name))
    context[name.lower()+'_matches']=os.environ.get(name)==value
run=os.environ.get('SUBYARD_AGENT_RUN_ID','')
context['agent_run_present']=bool(run)
context['managed_agent_matches']=False
if re.fullmatch('[A-Za-z0-9_.:-]{1,128}',run) and expected.get('data_dir'):
    try:
        manifest=json.loads((pathlib.Path(expected['data_dir'])/'runs'/(run+'.json')).read_text())
        context['managed_agent_matches']=isinstance(manifest,dict) and manifest.get('agent')==expected.get('agent') and manifest.get('agent_run_id')==run
    except (OSError,ValueError,TypeError): pass
yard=expected.get('yard','test-yard')
registry=pathlib.Path(os.environ.get('SUBYARD_E2E_ROUTE_REGISTRY','/var/lib/subyard/e2e-routes'))/yard/'current'
context['route_registry_readable']=all(os.access(registry/name,os.R_OK) for name in ('route.tsv','known_hosts'))
home=pathlib.Path(os.environ.get('HOME','/nonexistent'))
context['home_ssh_config_exists']=(home/'.ssh/config').is_file()
state=pathlib.Path(os.environ.get('SUBYARD_E2E_STATE_DIR','/nonexistent'))
context['controller_identity_exists']=all((state/name).is_file() for name in ('id_ed25519','id_ed25519.pub'))
context['controller_state_exists']=(state/'yards'/yard/'client-id').is_file()
output=pathlib.Path(sys.argv[1]).with_suffix('.output')
fd=os.open(output,os.O_CREAT|os.O_EXCL|os.O_RDWR,0o600)
os.fchmod(fd,0o600)
stream=os.fdopen(fd,'w+b')
def finish(status):
    stream.flush()
    stream.seek(0)
    raw=stream.read(65536)
    stream.seek(0,os.SEEK_END)
    stream.seek(max(0,stream.tell()-65536))
    raw+=stream.read(65536)
    errors=[('workspace_attribution',b'agent-e2e: E2E runner must execute from /srv/workspaces/<ProjectID>/src'),
            ('missing_pinned_host_key',b'agent-e2e: test environment unavailable: no pinned host key for '),
            ('controller_identity',b'agent-e2e: incomplete agent identity at '),
            ('capability_probe',b'agent-e2e: broker capability probe failed'),
            ('unknown_acquire_outcome',b'agent-e2e: lease acquire outcome is unknown;'),
            ('ssh_authentication',b'Permission denied (publickey)'),
            ('ssh_name_resolution',b'Could not resolve hostname'),
            ('ssh_connection_refused',b'Connection refused'),
            ('shell_unbound_variable',b'unbound variable'),
            ('shell_command_missing',b'command not found'),
            ('capacity_refusal',b'agent-e2e: retryable capacity refusal for ')]
    reason=next((name for name,prefix in errors if prefix in raw),'unclassified')
    matches=[]
    if expected.get('runner_source'):
        try:
            for line_number,line in enumerate(pathlib.Path(expected['runner_source']).read_text().splitlines(),1):
                for marker in ('die "',"die '"):
                    if marker not in line: continue
                    literal=line.split(marker,1)[1].split(marker[-1],1)[0].split('$',1)[0].split('`',1)[0]
                    prefix=('agent-e2e: '+literal).encode()
                    if len(literal)>=8 and prefix in raw: matches.append((len(prefix),line_number))
        except OSError: pass
    longest=max((length for length,line in matches),default=0)
    callsites=sorted({line for length,line in matches if length==longest})[:8]
    if reason=='unclassified' and callsites: reason='runner_guard'
    stream.seek(0)
    fingerprint=hashlib.file_digest(stream,'sha256').hexdigest()
    context['allocation_started']=b'E2E_PHASE phase=allocation state=start ' in raw
    context['lease_granted']=b'E2E lease: yard=' in raw
    fd=os.open(sys.argv[1],os.O_CREAT|os.O_EXCL|os.O_WRONLY,0o600)
    with os.fdopen(fd,'w') as receipt:
        os.fchmod(receipt.fileno(),0o600)
        json.dump(dict(schema='subyard.telemetry.native-command/v1',exit_code=status,context=context,error_class=reason,
                       runner_error_callsites=callsites,output_sha256=fingerprint),receipt)
process=subprocess.Popen(sys.argv[3:],stdout=stream,stderr=subprocess.STDOUT)
def cancel(signum,frame):
    signal.signal(signal.SIGTERM,signal.SIG_IGN)
    signal.signal(signal.SIGINT,signal.SIG_IGN)
    process.terminate()
    raise InterruptedError(signum)
signal.signal(signal.SIGTERM,cancel)
signal.signal(signal.SIGINT,cancel)
try: status=process.wait()
except InterruptedError as stopped:
    # Preserve the native controller's 180-second lease-release budget.
    try: process.wait(timeout=190)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=5)
    status=128+stopped.args[0]
status=status if status>=0 else 128-status
finish(status)
raise SystemExit(status)
'''
    return shlex.join([sys.executable, '-B', '-c', code, str(receipt), json.dumps(expected or {}), *command])


def require_command_success(receipt, agent):
    status = None
    try:
        result = json.loads(receipt.read_text())
        if isinstance(result, dict) and result.get('schema') == 'subyard.telemetry.native-command/v1' and type(result.get('exit_code')) is int:
            status = result['exit_code']
    except (OSError, ValueError, TypeError):
        pass
    facts = dict(check='native-resource-command', agent=agent, receipt_present=receipt.is_file(),
                 exit_code_known=status is not None, exit_code=status)
    if status is not None:
        context = result.get('context', {})
        facts['context'] = {key: value for key, value in (context.items() if isinstance(context, dict) else [])
                            if type(value) is bool and re.fullmatch('[a-z0-9_]{1,80}', key)}
        reason = result.get('error_class')
        facts['error_class'] = reason if reason in ('workspace_attribution', 'missing_pinned_host_key', 'controller_identity',
            'capability_probe', 'unknown_acquire_outcome', 'ssh_authentication', 'ssh_name_resolution', 'ssh_connection_refused',
            'shell_unbound_variable', 'shell_command_missing', 'capacity_refusal', 'runner_guard') else 'unclassified'
        facts['runner_error_callsites'] = [line for line in result.get('runner_error_callsites', [])
                                           if type(line) is int and 1 <= line <= 10000][:8]
        fingerprint = result.get('output_sha256', '')
        if isinstance(fingerprint, str) and re.fullmatch('[a-f0-9]{64}', fingerprint):
            facts['output_sha256'] = fingerprint
    assert status == 0, json.dumps(facts, sort_keys=True)
    print(json.dumps(facts, sort_keys=True), flush=True)
    return result


def safe_probe_error(output, command, secret):
    # Only the fixed native-context probe enables this diagnostic, never resource tools.
    text = output.strip()
    if re.fullmatch(r'unsupported (?:custom tool )?call: (?:functions\.)?(?:shell|shell_command|exec_command)', text):
        return text
    for value in (command, json.dumps(command)[1:-1], json.dumps(json.dumps(command))[1:-1], secret):
        if value:
            text = text.replace(value, '<redacted>')
    text = re.sub(r'(?is)\b(?:arguments|command|cmd|argv|input)[\"\']?\s*[:=].*', '<command>', text)
    text = re.sub(r'/[^\s\"\'<>\[\]{}(),;:]+', '<path>', text)
    text = re.sub(r'(?i)\b(?:authorization\s*[:=]\s*)?bearer\s+[^\s,;]+', '<credential>', text)
    text = re.sub(r'(?i)\b(?:bearer|authorization|api[_-]?key|token)\b\s*[:=]?\s*[^\s,;]+', '<credential>', text)
    text = re.sub(r'\b[0-9a-fA-F-]{16,}\b|[A-Za-z0-9_+=.-]{32,}', '<value>', text)
    return text[:1024]


def codex_provider_class(base):
    class ObservedProvider(base):
        def do_POST(self):
            body = self.rfile.read(int(self.headers['Content-Length']))
            request = json.loads(body)
            facts = getattr(self.server, 'native_provider_facts', dict(request_count=0,
                command_consumed=False, selected_tool='none', tool_output_seen=False, error_class='unclassified'))
            facts['request_count'] += 1
            tools = [child for tool in request.get('tools', []) for child in
                     (tool.get('tools', []) if tool.get('type') == 'namespace' else [tool])]
            pending = self.server.command is not None
            if pending:
                names = {tool.get('name') for tool in tools}
                facts['selected_tool'] = next((name for name in ('shell', 'shell_command', 'exec_command') if name in names), 'unavailable')
                selected = facts['selected_tool']
                facts['namespace_present'] = any(tool.get('type') == 'namespace' and any(
                    child.get('name') == selected for child in tool.get('tools', [])) for tool in request.get('tools', []))
                facts['namespace_default'] = any(tool.get('type') == 'namespace' and tool.get('name') == 'functions' and any(
                    child.get('name') == selected for child in tool.get('tools', [])) for tool in request.get('tools', []))
                facts['tool_shape'] = next((tool.get('type') if tool.get('type') in ('function', 'custom') else 'unknown'
                    for tool in tools if tool.get('name') == selected), 'unknown')
            for item in request.get('input', []):
                if not isinstance(item, dict) or item.get('type') != 'function_call_output':
                    continue
                facts['tool_output_seen'] = True
                output = str(item.get('output', ''))[-16384:]
                capture = getattr(self.server, 'probe_error_capture', None)
                context_output = False
                for line in output.splitlines():
                    try:
                        value = json.loads(line)
                        context_output |= isinstance(value, dict) and value.get('schema') == 'subyard.telemetry.native-context/v1'
                    except (ValueError, TypeError):
                        pass
                if capture is not None and not context_output:
                    fd = os.open(capture, os.O_CREAT | os.O_TRUNC | os.O_WRONLY | os.O_NOFOLLOW, 0o600)
                    with os.fdopen(fd, 'w') as stream:
                        os.fchmod(stream.fileno(), 0o600)
                        stream.write(output)
                    print(json.dumps(dict(schema='subyard.telemetry.native-probe-error/v1', private_capture_present=True,
                        diagnostic=safe_probe_error(output, self.server.probe_command, self.server.probe_secret),
                        output_sha256=hashlib.sha256(output.encode()).hexdigest()), sort_keys=True), flush=True)
                for text, reason in (('Error parsing function call', 'tool_arguments_rejected'),
                                     ('bubblewrap is unavailable:', 'sandbox_runtime_unavailable'),
                                     ('unsupported call:', 'unsupported_call'),
                                     ('Read-only file system', 'read_only'), ('Permission denied', 'permission_denied'),
                                     ('Operation not permitted', 'sandbox_unavailable'), ('rejected', 'tool_rejected')):
                    if text in output:
                        facts['error_class'] = reason
                        break
            self.server.native_provider_facts = facts
            original = self.rfile
            self.rfile = io.BytesIO(body)
            try:
                super().do_POST()
            finally:
                self.rfile = original
                facts['command_consumed'] |= pending and self.server.command is None

    return ObservedProvider


def codex_app_class(base):
    class ObservedApp(base):
        command_result = None
        command_started = False
        app_error_seen = False

        def observe(self, frame):
            item = frame.get('params', {}).get('item', {})
            if frame.get('method') == 'error':
                self.app_error_seen = True
            if frame.get('method') == 'item/started' and item.get('type') == 'commandExecution':
                self.command_started = True
            if frame.get('method') != 'item/completed' or item.get('type') != 'commandExecution':
                return
            status = item.get('status')
            result = dict(check='native-tool-completion', agent='codex',
                status=status if status in ('completed', 'failed', 'declined', 'inProgress') else 'unknown',
                exit_code=item.get('exitCode') if type(item.get('exitCode')) is int else None,
                command_session_present=bool(item.get('processId')))
            output = item.get('aggregatedOutput') or ''
            result['error_class'] = next((name for text, name in (
                ('Permission denied', 'permission_denied'), ('Read-only file system', 'read_only'),
                ('Operation not permitted', 'sandbox_unavailable')) if text in output), 'unclassified')
            for line in output[-4096:].splitlines():
                try:
                    proof = json.loads(line)
                    if isinstance(proof, dict) and proof.get('schema') == 'subyard.telemetry.native-context/v1':
                        result['context'] = {key: proof.get(key) is True for key in (
                            'agent_run_present', 'managed_agent_matches', 'home_matches', 'data_dir_matches')}
                except (ValueError, TypeError):
                    pass
            self.command_result = result

        def read(self):
            frame = super().read()
            self.observe(frame)
            return frame

        def poll_command_result(self):
            while True:
                try:
                    self.observe(self.frames.get_nowait())
                except queue.Empty:
                    return self.command_result

    return ObservedApp


def native_context_command(home, data_dir):
    code = '''import json,os,pathlib,sys,time
run=os.environ.get('SUBYARD_AGENT_RUN_ID','')
proof=dict(schema='subyard.telemetry.native-context/v1',agent_run_present=bool(run),managed_agent_matches=False,home_matches=os.environ.get('HOME')==sys.argv[1],data_dir_matches=os.environ.get('SUBYARD_TELEMETRY_DATA_DIR')==sys.argv[2])
try:
    manifest=json.loads((pathlib.Path(sys.argv[2])/'runs'/(run+'.json')).read_text())
    proof['managed_agent_matches']=manifest.get('agent')=='codex' and manifest.get('agent_run_id')==run
except (OSError,ValueError,TypeError): pass
time.sleep(3)
print(json.dumps(proof,sort_keys=True))
'''
    return shlex.join([sys.executable, '-B', '-c', code, str(home), str(data_dir)])


def run_codex_command(app, provider, session, project, command, receipt=None, timeout=1830):
    # exec_command may finish its turn while its command session is still running.
    deadline = time.monotonic()+timeout
    app.command_result = None
    app.command_started = False
    app.app_error_seen = False
    if provider is not None:
        provider.native_provider_facts = dict(request_count=0, command_consumed=False, selected_tool='none',
            tool_output_seen=False, error_class='unclassified')
    app.turn(provider, session, project, command)
    while True:
        try:
            result = json.loads(receipt.read_text()) if receipt is not None else None
            if isinstance(result, dict) and result.get('schema') == 'subyard.telemetry.native-command/v1' and type(result.get('exit_code')) is int:
                return require_command_success(receipt, 'codex')
        except (OSError, ValueError, TypeError):
            pass
        tool = app.poll_command_result() if hasattr(app, 'poll_command_result') else None
        provider_facts = getattr(provider, 'native_provider_facts', {})
        assert provider_facts.get('error_class') != 'sandbox_runtime_unavailable', json.dumps(dict(
            check='native-command-startup', agent='codex', error_class='sandbox_runtime_unavailable',
            tool_started=app.command_started, sandbox=getattr(app, 'effective_sandbox', 'unknown')), sort_keys=True)
        if tool is not None and (tool['exit_code'] is not None or tool['status'] in ('failed', 'declined')):
            assert tool['status'] == 'completed' and tool['exit_code'] == 0, json.dumps(tool, sort_keys=True)
            if receipt is None:
                print(json.dumps(tool, sort_keys=True), flush=True)
                return tool
            assert receipt.is_file(), json.dumps(dict(tool, receipt_present=False), sort_keys=True)
        alive = app.process.poll() is None
        assert alive and time.monotonic() < deadline, json.dumps(dict(check='native-command-completion',
            agent='codex', backend_alive=alive, tool_observed=tool is not None,
            tool_started=app.command_started, app_error_seen=app.app_error_seen,
            receipt_present=receipt is not None and receipt.is_file(), timed_out=time.monotonic() >= deadline,
            provider=getattr(provider, 'native_provider_facts', {}), sandbox=getattr(app, 'effective_sandbox', 'unknown')), sort_keys=True)
        time.sleep(.05)


def exercise(args):
    """Invoke the ordinary resource client from two real native agent tools."""
    prepared = Path(os.environ['SUBYARD_TELEMETRY_NATIVE_STATE'])
    binaries = prepared/'bin'
    args.state.mkdir(mode=0o700, parents=True, exist_ok=True)
    sys.path.insert(0, str(args.source/'config/profiles/telemetry'))
    sys.path.insert(0, str(args.source/'internal/adapters/configmaterial'))
    import native
    import tomli_w
    fixture = load(args.source/'config/profiles/orca/tests/e2e/codex-permissions.py', 'codex_fixture_exercise')
    fixture.TIMEOUT = 1830
    command = args.command[1:] if args.command[:1] == ['--'] else args.command
    assert command and args.kind in ('vm', 'android')
    token = (args.data_dir/'auth.token').read_text().strip()
    identities = []
    project = prepared/'project'
    for agent in ('codex', 'claude'):
        state = args.state/agent
        state.mkdir(mode=0o700)
        for name in ('home', 'codex', 'claude'):
            (state/name).mkdir(mode=0o700)
        env = {key: value for key, value in os.environ.items() if key.startswith(('SUBYARD_', 'YARD_', 'INCUS_')) or key in ('STORAGE_PATH', 'MIN_DISK_GIB')}
        env.update(PATH=str(binaries)+':/usr/local/bin:/usr/bin:/bin', LANG='C.UTF-8', HOME=str(state/'home'),
                   CODEX_HOME=str(state/'codex'), CLAUDE_CONFIG_DIR=str(state/'claude'), SUBYARD_PROJECT_ID='telemetry-fixture',
                   SUBYARD_TELEMETRY_DATA_DIR=str(args.data_dir), OTEL_METRIC_EXPORT_INTERVAL='1000', OTEL_BSP_SCHEDULE_DELAY='100', OTEL_BLRP_SCHEDULE_DELAY='100')
        receipt = state/'native-command-result.json'
        expected = dict(agent=agent, data_dir=str(args.data_dir), yard=os.environ.get('SUBYARD_E2E_YARD', 'test-yard'),
                        runner_source=str(args.source/'dev/agent-e2e.sh'),
                        environment={name: env[name] for name in ('HOME', 'PATH', 'SUBYARD_E2E_STATE_DIR', 'SUBYARD_CONFIG_HOME',
                            'SUBYARD_E2E_TEST_MODE', 'SUBYARD_E2E_WORKSPACES_ROOT', 'SUBYARD_TELEMETRY_DATA_DIR') if name in env})
        tool_command = command_with_receipt(command, receipt, expected)
        if agent == 'codex':
            provider = http.server.ThreadingHTTPServer(('127.0.0.1', 0), codex_provider_class(fixture.Provider))
            provider.command, provider.failure, provider.workspace = None, None, project
            provider.command_timeout_ms = 1800000
            threading.Thread(target=provider.serve_forever, daemon=True).start()
            config = {'model': 'subyard-fixture', 'model_provider': 'fixture', 'sandbox_mode': 'danger-full-access', 'approval_policy': 'on-request',
                      'features': {'plugins': False, 'unified_exec': False},
                      'otel': native.desired('codex', 'http://127.0.0.1:14318', token), 'analytics': {'enabled': True},
                      'model_providers': {'fixture': {'name': 'Fixture', 'base_url': f'http://127.0.0.1:{provider.server_port}/v1',
                          'wire_api': 'responses', 'requires_openai_auth': False, 'request_max_retries': 0, 'stream_max_retries': 0}}}
            (state/'codex/config.toml').write_text(tomli_w.dumps(config))
            (state/'codex/config.toml').chmod(0o600)
            app = codex_app_class(fixture.App)(['subyard-telemetry-run', '--agent', agent, '--scope-mode', 'require', '--project-id', 'telemetry-fixture', '--', str(binaries/agent), 'app-server'], env, project)
            try:
                started = app.request('thread/start', {'cwd': str(project)})
                sandbox = started.get('sandbox', {}).get('type')
                app.effective_sandbox = sandbox if sandbox in ('readOnly', 'workspaceWrite', 'dangerFullAccess', 'externalSandbox') else 'unknown'
                session = started['thread']['id']
                run_codex_command(app, provider, session, project, tool_command, receipt)
            finally:
                app.close()
                provider.shutdown()
                provider.server_close()
        else:
            provider = http.server.ThreadingHTTPServer(('127.0.0.1', 0), ClaudeProvider)
            provider.requests, provider.command = 0, tool_command
            threading.Thread(target=provider.serve_forever, daemon=True).start()
            env.update(native.desired('claude', 'http://127.0.0.1:14318', token))
            native.publish(state/'claude/settings.json', (json.dumps({'env': native.desired('claude', 'http://127.0.0.1:14318', token)})+'\n').encode(), None)
            env.update(ANTHROPIC_BASE_URL=f'http://127.0.0.1:{provider.server_port}', ANTHROPIC_API_KEY='local-fixture-only',
                       DISABLE_AUTOUPDATER='1', CLAUDE_CODE_PROPAGATE_TRACEPARENT='1')
            try:
                result = subprocess.run(['subyard-telemetry-run', '--agent', agent, '--scope-mode', 'require', '--project-id', 'telemetry-fixture', '--', str(binaries/agent),
                    '-p', 'Run the fixture command.', '--allowedTools', 'Bash', '--output-format', 'stream-json', '--verbose', '--max-turns', '3'],
                    env=env, cwd=project, capture_output=True, timeout=630, check=True)
                frames = [json.loads(line) for line in result.stdout.splitlines() if line.strip()]
                session = next(frame['session_id'] for frame in frames if frame.get('session_id'))
                assert provider.requests >= 2, 'native Claude did not complete its real tool call'
            finally:
                provider.shutdown()
                provider.server_close()
        if agent != 'codex':
            require_command_success(receipt, agent)
        runs = wait_sessions(args.data_dir, agent, {session}, stderr=result.stderr if agent == 'claude' else b'')
        assert len(runs) == 1
        run = next(iter(runs))
        deadline = time.monotonic()+45
        while time.monotonic() < deadline:
            allocations = query(args.data_dir, 'SELECT DISTINCT allocations.object_id FROM allocations JOIN associations USING(object_id) WHERE associations.agent_run_id=?', (run,))
            if allocations:
                break
            time.sleep(0.2)
        assert allocations, agent+': actual native tool acquire did not bind its allocation'
        assert json.loads(query(args.data_dir, 'SELECT body FROM runs WHERE agent_run_id=?', (run,))[0][0])['project_id'] == 'telemetry-fixture'
        identities.append(dict(agent=agent, agent_run_id=run, session_id=session, object_ids=[row[0] for row in allocations]))
    assert identities[0]['agent_run_id'] != identities[1]['agent_run_id']
    assert set(identities[0]['object_ids']).isdisjoint(identities[1]['object_ids']), 'independent acquisitions reused one resource generation'
    output = args.state/'native-resource-bindings.json'
    output.write_text(json.dumps(dict(kind=args.kind, project_id='telemetry-fixture', runs=identities), indent=2)+'\n')
    output.chmod(0o600)
    print('ok: native Codex and Claude tools acquired distinct '+args.kind+' allocations with explicit exported session bindings')


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--state', type=Path, required=True)
    parser.add_argument('--data-dir', type=Path, required=True)
    parser.add_argument('--source', type=Path, required=True)
    parser.add_argument('--exercise', action='store_true')
    parser.add_argument('--kind', choices=['vm', 'android'])
    parser.add_argument('command', nargs=argparse.REMAINDER)
    args = parser.parse_args()
    if args.exercise:
        return exercise(args)
    state = args.state/'native'
    state.mkdir(mode=0o700)
    binaries = install_agents(args, state)
    sys.path.insert(0, str(args.source/'config/profiles/telemetry'))
    sys.path.insert(0, str(args.source/'internal/adapters/configmaterial'))
    import native
    import tomli_w
    token = (args.data_dir/'auth.token').read_text().strip()
    endpoint = 'http://127.0.0.1:14318'
    env = {'PATH': str(binaries)+':/usr/local/bin:/usr/bin:/bin', 'LANG': 'C.UTF-8',
           'HOME': str(state/'home'), 'CODEX_HOME': str(state/'codex'), 'CLAUDE_CONFIG_DIR': str(state/'claude'),
           'SUBYARD_TELEMETRY_DATA_DIR': str(args.data_dir), 'OTEL_METRIC_EXPORT_INTERVAL': '1000',
           'SUBYARD_PROJECT_ID': 'telemetry-fixture',
           'OTEL_BSP_SCHEDULE_DELAY': '100', 'OTEL_BLRP_SCHEDULE_DELAY': '100'}
    for name in ('home', 'codex', 'claude', 'project'):
        (state/name).mkdir(mode=0o700)
    subprocess.run(['git', 'init', '--quiet', str(state/'project')], check=True)
    fixture = load(args.source/'config/profiles/orca/tests/e2e/codex-permissions.py', 'codex_fixture')
    provider = http.server.ThreadingHTTPServer(('127.0.0.1', 0), codex_provider_class(fixture.Provider))
    provider.command, provider.failure, provider.workspace = None, None, state/'project'
    threading.Thread(target=provider.serve_forever, daemon=True).start()
    config = {'model': 'subyard-fixture', 'model_provider': 'fixture', 'features': {'plugins': False, 'unified_exec': False}, 'otel': native.desired('codex', endpoint, token),
              'analytics': {'enabled': True}, # Explicit isolated fixture opt-in; production intent remains untouched.
              'model_providers': {'fixture': {'name': 'Fixture', 'base_url': f'http://127.0.0.1:{provider.server_port}/v1',
                  'wire_api': 'responses', 'requires_openai_auth': False, 'request_max_retries': 0, 'stream_max_retries': 0}}}
    (state/'codex/config.toml').write_text(tomli_w.dumps(config))
    (state/'codex/config.toml').chmod(0o600)
    app = codex_app_class(fixture.App)(['subyard-telemetry-run', '--agent', 'codex', '--scope-mode', 'require', '--shared-backend', '--', str(binaries/'codex'), 'app-server'], env, state/'project')
    threads = set()
    try:
        for index in range(2):
            started = app.request('thread/start', {'cwd': str(state/'project')})
            sandbox = started.get('sandbox', {}).get('type')
            app.effective_sandbox = sandbox if sandbox in ('readOnly', 'workspaceWrite', 'dangerFullAccess', 'externalSandbox') else 'unknown'
            thread = started['thread']['id']
            threads.add(thread)
            if index == 0:
                command = native_context_command(state/'home', args.data_dir)
                provider.probe_error_capture = state/'native-probe-tool-error.txt'
                provider.probe_command, provider.probe_secret = command, token
                result = run_codex_command(app, provider, thread, state/'project', command, timeout=30)
                provider.probe_error_capture = None
                assert all(result['context'].get(name) is True for name in ('agent_run_present', 'managed_agent_matches',
                    'home_matches', 'data_dir_matches')), 'native delayed tool lost its managed context'
            else:
                app.turn(provider, thread, state/'project', None)
    finally:
        app.close()
        provider.shutdown()
        provider.server_close()
    runs = wait_sessions(args.data_dir, 'codex', threads)
    assert len(runs) == 1, 'two backend sessions must retain the one real shared backend run'
    run_id = next(iter(runs))
    wait_shared_scope(args.data_dir, run_id)
    server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), ClaudeProvider)
    server.requests = 0
    threading.Thread(target=server.serve_forever, daemon=True).start()
    local = dict(env, **native.desired('claude', endpoint, token), ANTHROPIC_BASE_URL=f'http://127.0.0.1:{server.server_port}',
                 ANTHROPIC_API_KEY='local-fixture-only', DISABLE_AUTOUPDATER='1', CLAUDE_CODE_PROPAGATE_TRACEPARENT='1')
    native.publish(state/'claude/settings.json', (json.dumps({'env': native.desired('claude', endpoint, token)})+'\n').encode(), None)
    try:
        result = subprocess.run(['subyard-telemetry-run', '--agent', 'claude', '--scope-mode', 'require', '--', str(binaries/'claude'),
                                 '-p', 'Return probe.', '--output-format', 'stream-json', '--verbose', '--max-turns', '1'],
                                env=local, cwd=state/'project', capture_output=True, timeout=60, check=True)
        frames = [json.loads(line) for line in result.stdout.splitlines() if line.strip()]
        session = next(frame['session_id'] for frame in frames if frame.get('session_id'))
        assert server.requests > 0
    finally:
        server.shutdown()
        server.server_close()
    wait_sessions(args.data_dir, 'claude', {session}, stderr=result.stderr)
    kinds = {row[0] for row in query(args.data_dir, "SELECT DISTINCT kind FROM records WHERE kind IN ('otlp/logs','otlp/metrics','otlp/traces')")}
    assert kinds == {'otlp/logs', 'otlp/metrics', 'otlp/traces'}, 'actual native signal coverage incomplete'
    print('ok: real native versions; loopback providers; two shared Codex sessions and Claude session linked by exported evidence')


if __name__ == '__main__':
    main()
