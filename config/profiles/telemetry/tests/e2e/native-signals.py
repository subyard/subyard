#!/usr/bin/env python3
"""Real native exporters, isolated homes, deterministic local providers only."""
import argparse
import base64
from contextlib import closing
import hashlib
import http.server
import importlib.util
import inspect
import io
import json
import os
from pathlib import Path
import queue
import re
import select
import shutil
import shlex
import sqlite3
import struct
import subprocess
import sys
import tarfile
import threading
import time
import urllib.request
import uuid


def load(path, name):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


failure_evidence = load(Path(__file__).with_name('failure-evidence.py'), 'telemetry_failure_evidence')


def claude_failure(phase, result, token):
    failure_evidence.emit(phase, {'claude.stdout': getattr(result, 'stdout', None),
        'claude.stderr': getattr(result, 'stderr', None)}, secrets=(token, 'local-fixture-only'),
        return_code=getattr(result, 'returncode', None), timed_out=isinstance(result, subprocess.TimeoutExpired))


def claude_command(command, token, **kwargs):
    try:
        return subprocess.run(command, **kwargs)
    except (subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
        claude_failure('claude-command', error, token)
        raise


def wait_claude_sessions(data, expected, result, token):
    try:
        return wait_sessions(data, 'claude', expected, stderr=result.stderr)
    except AssertionError:
        claude_failure('claude-session-binding', result, token)
        raise


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


def protobuf_fields(data):
    """Yield (field, value) pairs of one protobuf message; nested messages stay bytes."""
    offset = 0
    def varint():
        nonlocal offset
        value = shift = 0
        while True:
            byte = data[offset]
            offset += 1
            value |= (byte & 0x7f) << shift
            shift += 7
            if byte < 0x80:
                return value
    while offset < len(data):
        key = varint()
        field, wire = key >> 3, key & 7
        if wire == 0:
            yield field, varint()
        elif wire == 1:
            yield field, data[offset:offset+8]
            offset += 8
        elif wire == 2:
            size = varint()
            yield field, data[offset:offset+size]
            offset += size
        elif wire == 5:
            yield field, data[offset:offset+4]
            offset += 4
        else:
            raise ValueError('unsupported protobuf wire type')


def otlp_attributes(message, field):
    # KeyValue: key = 1, value = 2. AnyValue: string = 1, bool = 2, int = 3, double = 4.
    attributes = {}
    for number, value in protobuf_fields(message):
        if number == field:
            pair = dict(protobuf_fields(value))
            for kind, raw in protobuf_fields(pair.get(2, b'')):
                if kind == 1:
                    raw = raw.decode()
                elif kind == 2:
                    raw = bool(raw)
                elif kind == 3:
                    raw = raw-(1 << 64) if raw >= 1 << 63 else raw
                elif kind == 4:
                    raw = struct.unpack('<d', raw)[0]
                else:
                    continue
                attributes[pair[1].decode()] = raw
    return attributes


def collector():
    return os.environ.get('SUBYARD_TELEMETRY_COLLECTOR') or '/usr/local/lib/subyard-telemetry/subyard-telemetry'


_exported = {}


def exported(data):
    """Return every exported row; the collector decompresses stored raw OTLP."""
    cursor, rows = _exported.get(str(data), (0, []))
    while True:
        page = subprocess.run([collector(), 'export', '--data-dir', str(data), '--after', str(cursor), '--limit', '1000'],
                              capture_output=True, check=True, timeout=60).stdout
        batch = [json.loads(line) for line in page.splitlines() if line.strip()]
        if not batch:
            break
        rows += batch
        cursor = batch[-1]['sequence']
    _exported[str(data)] = (cursor, rows)
    return rows


def raw_otlp(data, kind):
    return [base64.b64decode(row['protobuf']) for row in exported(data) if row['kind'] == kind and row.get('protobuf')]


def stored_log_attributes(data):
    """Return the attributes of every stored native log record."""
    found = []
    for protobuf in raw_otlp(data, 'otlp/logs'):
        for resource_logs in (value for number, value in protobuf_fields(protobuf) if number == 1):
            for scope_logs in (value for number, value in protobuf_fields(resource_logs) if number == 2):
                for record in (value for number, value in protobuf_fields(scope_logs) if number == 2):
                    found.append(otlp_attributes(record, 6))
    return found


NATIVE_TOKEN_COUNTERS = {
    'codex': ('input_token_count', 'output_token_count', 'cached_token_count', 'reasoning_token_count', 'tool_token_count'),
    'claude': ('input_tokens', 'output_tokens', 'cache_read_tokens', 'cache_creation_tokens'),
}


def native_log_shortfalls(logs, success):
    """Name each client field the installed clients' stored logs lack.

    Codex sends prefixed event names; Claude sends them without a prefix."""
    agents = {'codex': lambda name: name.startswith('codex.'), 'claude': lambda name: '.' not in name}
    missing = []
    for agent, matches in agents.items():
        own = [fields for fields in logs if matches(fields.get('event.name') or '.')]
        checks = {'event.name and model': lambda fields: isinstance(fields.get('model'), str) and fields['model'] != '',
                  'token counter': lambda fields: any(type(fields.get(name)) is int and fields[name] >= 0
                                                       for name in NATIVE_TOKEN_COUNTERS[agent])}
        if agent in success:
            checks['boolean success'] = lambda fields: type(fields.get('success')) is bool
        missing += [agent+' '+label for label, check in checks.items() if not any(check(fields) for fields in own)]
    return missing


def assert_native_log_fields(data, success=('codex', 'claude'), timeout=20):
    """Installed clients keep their event name, model, tokens and success through the sanitizer."""
    deadline = time.monotonic()+timeout
    while True:
        logs = stored_log_attributes(data)
        missing = native_log_shortfalls(logs, success)
        if not missing or time.monotonic() >= deadline:
            break
        time.sleep(0.5)
    assert not missing, (', '.join(missing)+': lost from stored native logs; kept keys '+
                         json.dumps(sorted({key for fields in logs for key in fields})))


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
            for label, source in (('managed_runs', 'runs'), ('relations', 'relations LEFT JOIN runs USING(origin_id,agent_run_id)')):
                result[label] = dict(db.execute('SELECT '+classification+',count(*) FROM '+source+' GROUP BY 1').fetchall())
            result['otlp_signals'] = dict(db.execute("SELECT kind,count(*) FROM records WHERE kind IN ('otlp/logs','otlp/metrics','otlp/traces') GROUP BY kind").fetchall())
            placeholders = ','.join('?' for _ in selected)
            matched = db.execute("SELECT DISTINCT relations.session_id FROM relations JOIN runs USING(origin_id,agent_run_id) WHERE json_extract(runs.body,'$.agent')=? AND relations.session_id IN ("+placeholders+') LIMIT 8', (agent, *selected)).fetchall()
            result['matched_count'] = len(matched)
            result['matched_digests'] = sorted(digest(row[0]) for row in matched)
    except sqlite3.Error:
        result['database'] = 'unavailable'
    return result


def wait_sessions(data, agent, expected, stderr=b''):
    deadline = time.monotonic()+40
    while time.monotonic() < deadline:
        rows = query(data, 'SELECT relations.agent_run_id,relations.session_id FROM relations JOIN runs USING(origin_id,agent_run_id) WHERE json_extract(runs.body,\'$.agent\')=?', (agent,))
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
        rows = query(data, """SELECT json_extract(objects.body,'$.shared') FROM objects JOIN associations USING(origin_id,object_id)
                             WHERE associations.agent_run_id=? AND objects.kind='scope' AND objects.accounting='inclusive'""", (run_id,))
        if rows:
            shared = 'true' if rows[0][0] == 1 else 'false' if rows[0][0] == 0 else 'missing_or_invalid'
            if shared == 'true':
                return
            raise AssertionError(json.dumps({'schema': 'subyard.telemetry.shared-scope-failure/v1',
                'reason': 'shared_scope_not_shared', 'scope_present': True, 'shared': shared}))
        time.sleep(.2)
    raise AssertionError(json.dumps({'schema': 'subyard.telemetry.shared-scope-failure/v1',
        'reason': 'shared_scope_projection_missing', 'scope_present': False, 'shared': 'not_observed'}))


CODEX_FIXTURE_VERSION = '0.170.0'
CODEX_FIXTURE_DIGESTS = {
    'x86_64': 'a77ff87be7ba809d715728270a78f5ce3527dc07529416d1ef8929b90b8475eb',
    'aarch64': '3f7e1ae38c4fe27322e082b855e8eec936d08fa538d62b71c07e2a53f34e8847',
}
# 2.1.327 is the first fixture release that names native subagents in its logs.
CLAUDE_FIXTURE_VERSION = '2.1.327'
CLAUDE_FIXTURE_PACKAGES = {
    'x86_64': ('linux-x64', '343c744baa14bfd38286a7cb90871051f04379956da6c5b803a9e66019a76ac9'),
    'aarch64': ('linux-arm64', 'acf0ef2f9da8d5ab08f193f007e19a06cbfb608b96a2f1f758d63a92a386cff8'),
}


def install_agents(state):
    binaries = state/'bin'
    binaries.mkdir()
    arch = os.uname().machine
    download = ['curl', '--fail', '--silent', '--show-error', '--location', '--proto', '=https', '--proto-redir', '=https',
                '--connect-timeout', '20', '--max-time', '120', '--output']
    # These clients run on the lease VM with fixture permissions. Keep the full
    # Codex package for app-server, without invoking yard sandbox provisioning.
    archive = state/'codex-native.tgz'
    url = (f'https://github.com/openai/codex/releases/download/rust-v{CODEX_FIXTURE_VERSION}/'
           f'codex-package-{arch}-unknown-linux-musl.tar.gz')
    subprocess.run([*download, str(archive), url], check=True, timeout=125)
    with archive.open('rb') as stream:
        if hashlib.file_digest(stream, 'sha256').hexdigest() != CODEX_FIXTURE_DIGESTS[arch]:
            raise ValueError('Codex fixture archive checksum mismatch')
    with tarfile.open(archive, 'r:gz') as package:
        package.extractall(state/'codex-native', filter='data')
    (binaries/'codex').symlink_to('../codex-native/bin/codex')

    platform, digest = CLAUDE_FIXTURE_PACKAGES[arch]
    archive = state/'claude-native.tgz'
    url = f'https://registry.npmjs.org/@anthropic-ai/claude-code-{platform}/-/claude-code-{platform}-{CLAUDE_FIXTURE_VERSION}.tgz'
    subprocess.run([*download, str(archive), url], check=True, timeout=125)
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


def managed_agent_matches(data, run, agent):
    import re
    import sqlite3
    from contextlib import closing
    from pathlib import Path
    import records
    if not re.fullmatch('[A-Za-z0-9_.:-]{1,128}', run):
        return False
    data = Path(data)
    try:
        manifest = records.read_private(data/'runs-v5'/(run+'.json'))
        if manifest.get('agent_run_id') != run or manifest.get('manifest_state') == 'reserved':
            return False
        deadline = time.monotonic()+2
        with closing(sqlite3.connect((data/'telemetry.sqlite').as_uri()+'?mode=ro', uri=True, timeout=.2)) as db:
            while time.monotonic() < deadline:
                row = db.execute("SELECT json_extract(body,'$.agent') FROM runs WHERE agent_run_id=?", (run,)).fetchone()
                if row and row[0]:
                    return row[0] == agent
                time.sleep(.05)
    except (OSError, ValueError, TypeError, AttributeError, sqlite3.Error):
        pass
    return False


def run_probe_code():
    profile = Path(__file__).resolve().parents[2]
    return 'import sys,time\nsys.path.insert(0, '+repr(str(profile))+')\n'+inspect.getsource(managed_agent_matches)+'\n'


def command_with_receipt(command, receipt, expected=None):
    code = run_probe_code()+'''import hashlib,json,os,pathlib,re,signal,subprocess,sys
expected=json.loads(sys.argv[2])
context={}
for name,value in expected.get('environment',{}).items():
    context[name.lower()+'_present']=bool(os.environ.get(name))
    context[name.lower()+'_matches']=os.environ.get(name)==value
run=os.environ.get('SUBYARD_AGENT_RUN_ID','')
context['agent_run_present']=bool(run)
context['managed_agent_matches']=bool(expected.get('data_dir') and managed_agent_matches(expected['data_dir'],run,expected.get('agent')))
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

        def start_fixture_thread(self, project):
            # The local provider sends only fixture commands. Exercise telemetry
            # without depending on the VM's Bubblewrap/AppArmor compatibility.
            started = self.request('thread/start', {'cwd': str(project), 'sandbox': 'danger-full-access'})
            self.effective_sandbox = started.get('sandbox', {}).get('type')
            assert self.effective_sandbox == 'dangerFullAccess', 'fixture tool permissions were not applied'
            return started['thread']['id']

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
    code = run_probe_code()+'''import json,os,sys,time
run=os.environ.get('SUBYARD_AGENT_RUN_ID','')
proof=dict(schema='subyard.telemetry.native-context/v1',agent_run_present=bool(run),managed_agent_matches=managed_agent_matches(sys.argv[2],run,'codex'),home_matches=os.environ.get('HOME')==sys.argv[1],data_dir_matches=os.environ.get('SUBYARD_TELEMETRY_DATA_DIR')==sys.argv[2])
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
    token = (args.data_dir.with_name(args.data_dir.name+'-private')/'auth.token').read_text().strip()
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
                      'otel': native.desired('codex', 'http://127.0.0.1:14318')['otel'], 'analytics': {'enabled': True},
                      'model_providers': {'fixture': {'name': 'Fixture', 'base_url': f'http://127.0.0.1:{provider.server_port}/v1',
                          'wire_api': 'responses', 'requires_openai_auth': False, 'request_max_retries': 0, 'stream_max_retries': 0}}}
            path = state/'codex/config.toml'
            native.publish(path, tomli_w.dumps(config).encode(), native.snapshot(path))
            app = codex_app_class(fixture.App)(['subyard-telemetry-run', '--agent', agent, '--scope-mode', 'require', '--project-id', 'telemetry-fixture', '--', str(binaries/agent), 'app-server'], env, project)
            try:
                session = app.start_fixture_thread(project)
                run_codex_command(app, provider, session, project, tool_command, receipt)
            finally:
                app.close()
                provider.shutdown()
                provider.server_close()
        else:
            provider = http.server.ThreadingHTTPServer(('127.0.0.1', 0), ClaudeProvider)
            provider.requests, provider.command = 0, tool_command
            threading.Thread(target=provider.serve_forever, daemon=True).start()
            # The installed helper supplies Authorization; the bearer stays out of both documents.
            settings = native.desired('claude', 'http://127.0.0.1:14318')
            env.update(settings['env'])
            native.publish(state/'claude/settings.json', (json.dumps(settings)+'\n').encode(), None)
            env.update(ANTHROPIC_BASE_URL=f'http://127.0.0.1:{provider.server_port}', ANTHROPIC_API_KEY='local-fixture-only',
                       DISABLE_AUTOUPDATER='1', CLAUDE_CODE_PROPAGATE_TRACEPARENT='1')
            try:
                result = claude_command(['subyard-telemetry-run', '--agent', agent, '--scope-mode', 'require', '--project-id', 'telemetry-fixture', '--', str(binaries/agent),
                    '-p', 'Run the fixture command.', '--allowedTools', 'Bash', '--output-format', 'stream-json', '--verbose', '--max-turns', '3'],
                    token, env=env, cwd=project, capture_output=True, timeout=630, check=True)
                frames = [json.loads(line) for line in result.stdout.splitlines() if line.strip()]
                session = next(frame['session_id'] for frame in frames if frame.get('session_id'))
                assert provider.requests >= 2, 'native Claude did not complete its real tool call'
            finally:
                provider.shutdown()
                provider.server_close()
        if agent != 'codex':
            require_command_success(receipt, agent)
        runs = (wait_claude_sessions(args.data_dir, {session}, result, token) if agent == 'claude'
                else wait_sessions(args.data_dir, agent, {session}))
        assert len(runs) == 1
        run = next(iter(runs))
        deadline = time.monotonic()+45
        while time.monotonic() < deadline:
            allocations = query(args.data_dir, 'SELECT DISTINCT allocations.object_id FROM allocations JOIN associations USING(origin_id,object_id) WHERE associations.agent_run_id=?', (run,))
            if allocations:
                break
            time.sleep(0.2)
        assert allocations, agent+': actual native tool acquire did not bind its allocation'
        assert json.loads(query(args.data_dir, 'SELECT body FROM runs WHERE agent_run_id=?', (run,))[0][0])['project_id'] == 'telemetry-fixture'
        identities.append(dict(agent=agent, agent_run_id=run, session_id=session, object_ids=[row[0] for row in allocations]))
    assert identities[0]['agent_run_id'] != identities[1]['agent_run_id']
    assert set(identities[0]['object_ids']).isdisjoint(identities[1]['object_ids']), 'independent acquisitions reused one resource generation'
    assert_native_log_fields(args.data_dir)
    output = args.state/'native-resource-bindings.json'
    output.write_text(json.dumps(dict(kind=args.kind, project_id='telemetry-fixture', runs=identities), indent=2)+'\n')
    output.chmod(0o600)
    print('ok: native Codex and Claude tools acquired distinct '+args.kind+' allocations with explicit exported session bindings')


def quit_codex_terminal(client, timeout=60):
    """Wait for normal exit, which drains Codex's embedded server and exporters.

    Leave the child waitable for Terminal.close() to reap during cleanup.
    """
    client.settle()
    client.send('/quit')
    client.settle()
    client.send('\r')
    deadline = time.monotonic()+timeout
    readable = True

    def read_output(seconds):
        nonlocal readable
        try:
            client.read(seconds)
        except RuntimeError as error:
            if not str(error).startswith('Codex terminal closed:'):
                raise
            readable = False

    def diagnostic(exited=None):
        return json.dumps(dict(
            exit_code=exited.si_status if exited is not None and exited.si_code == os.CLD_EXITED else None,
            signal=exited.si_status if exited is not None and exited.si_code != os.CLD_EXITED else None,
            terminal_tail=client.text()[-1800:]), sort_keys=True)

    while True:
        exited = os.waitid(os.P_PID, client.pid, os.WEXITED | os.WNOHANG | os.WNOWAIT)
        if exited is not None:
            while readable and select.select([client.fd], [], [], 0)[0]:
                read_output(0)
            assert exited.si_code == os.CLD_EXITED and exited.si_status == 0, 'Codex did not exit cleanly: '+diagnostic(exited)
            return
        remaining = deadline-time.monotonic()
        assert remaining > 0, 'Codex graceful exit timed out before exporter flush completed: '+diagnostic()
        if readable:
            read_output(min(.1, remaining))
        else:
            time.sleep(min(.05, remaining))


def assert_run_attribution(state, binaries, fixture, native, tomli_w, env, endpoint, token, data, system_policy):
    """A running shared daemon must not take a managed run's tools or their identity."""
    import fcntl, pty, struct, termios
    root = state/'attribution'
    home, codex_home, project = root/'home', root/'codex', root/'project'
    for path in (root, home, codex_home, project):
        path.mkdir(mode=0o700)
    subprocess.run(['git', 'init', '--quiet', str(project)], check=True)
    provider = http.server.ThreadingHTTPServer(('127.0.0.1', 0), fixture.Provider)
    provider.command, provider.failure, provider.workspace = None, None, project
    threading.Thread(target=provider.serve_forever, daemon=True).start()
    config = {'model': 'subyard-fixture', 'model_provider': 'fixture', 'sandbox_mode': 'danger-full-access', 'approval_policy': 'on-request',
              'features': {'plugins': False, 'unified_exec': False},
              'projects': {str(project): {'trust_level': 'trusted'}},
              'model_providers': {'fixture': {'name': 'Fixture', 'base_url': f'http://127.0.0.1:{provider.server_port}/v1',
                  'wire_api': 'responses', 'requires_openai_auth': False, 'request_max_retries': 0, 'stream_max_retries': 0}}}
    if not system_policy:
        config['otel'] = native.desired('codex', endpoint)['otel']
    path = codex_home/'config.toml'
    native.publish(path, tomli_w.dumps(config).encode(), native.snapshot(path))
    environment = dict(env, HOME=str(home), CODEX_HOME=str(codex_home), TERM='xterm-256color')

    class Client(fixture.Terminal):
        def __init__(self, argv):
            self.pid, self.fd = pty.fork()
            if self.pid == 0:
                os.chdir(project)
                os.execvpe(argv[0], argv, environment)
            fcntl.ioctl(self.fd, termios.TIOCSWINSZ, struct.pack('HHHH', 40, 140, 0, 0))
            self.buffer = ''

    def runs():
        return {row[0] for row in query(data, 'SELECT agent_run_id FROM runs')}

    def traces(run):
        # The collector stores spans as protobuf and writes the exporting run on their resource.
        return sum(run.encode() in protobuf for protobuf in raw_otlp(data, 'otlp/traces'))

    def turn(name, argv):
        known = runs()
        counts = {run: traces(run) for run in known}
        provider.command = f'printf %s "$SUBYARD_AGENT_RUN_ID" > {name}.run; readlink /proc/$PPID/exe > {name}.exe'
        client = Client([*argv, str(binaries/'codex'), '--no-alt-screen', 'Run the fixture command.'])
        try:
            client.wait('Fixture complete.', provider)
            # Rendering the answer is not an exporter completion signal. Quit
            # normally so Codex drains its embedded server and flushes OTLP;
            # close() alone sends SIGTERM and can discard pending spans.
            quit_codex_terminal(client)
        finally:
            try:
                client.close()
            finally:
                new = runs()-known
                grown = {run for run in known|new if traces(run) > counts.get(run, 0)}
                def receipt(suffix):
                    path = project/(name+suffix)
                    return path.read_text().strip() if path.exists() else ''
                result = dict(new=sorted(new), tool_run=receipt('.run'), tool_exe=receipt('.exe'), traces_grew_for=sorted(grown))
                print('run-attribution '+name+': '+json.dumps(result, sort_keys=True), flush=True)
        return result

    telemetry = ['subyard-telemetry-run', '--agent', 'codex', '--scope-mode', 'require', '--project-id', 'attribution', '--']
    try:
        turn('plain', [])
        assert (codex_home/'app-server-daemon/daemon.pid').exists(), 'the plain client started no daemon'
        for name in ('first', 'second'):
            result = turn(name, telemetry)
            assert result['new'] == [result['tool_run']], name+': tools carried another run identity'
            assert 'app-server-daemon' not in result['tool_exe'], name+': a managed run reached the shared daemon'
            assert result['traces_grew_for'] == [result['tool_run']], name+': spans did not carry only this run'
    finally:
        subprocess.run([str(binaries/'codex'), 'app-server', 'daemon', 'stop'], env=environment, cwd=home, timeout=120,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, stdin=subprocess.DEVNULL, check=False)
        # The updater outlives a stopped daemon, and this fixture's delegated scope waits for its last process.
        subprocess.run(['pkill', '-KILL', '-f', str(codex_home)], check=False)
        provider.shutdown()
        provider.server_close()


def assert_short_run_traces(data, endpoint, env):
    """Check both immediate exports from a short-lived managed run."""
    trace_id = uuid.uuid4().hex
    span_ids = ['0000000000000001', '0000000000000002']
    now = time.time_ns()
    batches = [dict(resourceSpans=[dict(scopeSpans=[dict(spans=[
        dict(traceId=trace_id, spanId=span_id, name='codex.turn',
             startTimeUnixNano=str(now), endTimeUnixNano=str(now+1))])])]) for span_id in span_ids]
    code = """import os, sys, urllib.request
from pathlib import Path
data = Path(os.environ['SUBYARD_TELEMETRY_DATA_DIR'])
token = (data.with_name(data.name+'-private')/'auth.token').read_text().strip()
run = os.environ['SUBYARD_AGENT_RUN_ID']
for batch in sys.stdin.buffer:
    request = urllib.request.Request(sys.argv[1]+'/v1/traces', data=batch, headers={
        'Content-Type': 'application/json', 'Authorization': 'Bearer '+token, 'X-Subyard-Agent-Run-ID': run})
    with urllib.request.urlopen(request, timeout=1) as response:
        assert response.status == 200
print(run)
"""
    started = time.monotonic()
    completed = subprocess.run(['subyard-telemetry-run', '--agent', 'other', '--agent-version', 'fixture',
                                '--scope-mode', 'require', '--', sys.executable, '-c', code, endpoint],
                               env=env, input=''.join(json.dumps(batch)+'\n' for batch in batches),
                               text=True, stdout=subprocess.PIPE, check=True, timeout=10)
    elapsed = time.monotonic()-started
    run = completed.stdout.strip()
    found = []
    for payload in raw_otlp(data, 'otlp/traces'):
        for field, group in protobuf_fields(payload):
            if field != 1:
                continue
            for scope_field, scope in protobuf_fields(group):
                if scope_field != 2:
                    continue
                for span_field, span in protobuf_fields(scope):
                    if span_field != 2:
                        continue
                    fields = dict(protobuf_fields(span))
                    if fields.get(1) == bytes.fromhex(trace_id):
                        attributes = otlp_attributes(dict(protobuf_fields(group))[1], 1)
                        assert attributes.get('subyard.agent_run.id') == run, 'fast span lost its run identity'
                        found.append(fields[2].hex())
    assert sorted(found) == span_ids, 'fast trace fixture lost or duplicated spans'
    print(f'ok: {elapsed:.3f}s managed run kept both immediate trace exports', flush=True)


class CoordinatorProvider(http.server.BaseHTTPRequestHandler):
    """A Claude coordinator delegates to a native subagent, then runs a worker in Bash."""
    def log_message(self, *_):
        pass

    def do_POST(self):
        size = int(self.headers.get('Content-Length', '0'))
        if not 0 < size <= 8*1024*1024:
            self.send_error(413)
            return
        request = json.loads(self.rfile.read(size))
        path = self.path.split('?')[0]
        if path.endswith('/count_tokens'):
            return self.reply('application/json', b'{"input_tokens":1}')
        if not path.endswith('/messages'):
            return self.send_error(404)
        messages = request.get('messages') or [{}]
        content = messages[0].get('content')
        first = content if isinstance(content, str) else ' '.join(block.get('text', '') for block in content or [] if isinstance(block, dict))
        tools = {tool.get('name') for tool in request.get('tools', [])}
        results = sum(block.get('type') == 'tool_result' for message in messages if isinstance(message.get('content'), list)
                      for block in message['content'] if isinstance(block, dict))
        role, tool = 'other', None
        if 'SUBAGENT-PROBE' in first:
            role = 'subagent'
        elif 'COORDINATOR-PROBE' in first and 'Bash' in tools:
            role = 'coordinator'
            agent = 'Agent' if 'Agent' in tools else 'Task'
            if results == 0 and agent in tools:
                tool = (agent, dict(description='Fixture subagent', prompt='SUBAGENT-PROBE: reply done.', subagent_type='general-purpose'))
            elif results <= 1:
                tool = ('Bash', dict(command=self.server.command, description='Run the fixture worker', timeout=600000))
        self.server.roles.append(role)
        # Distinct input counts let one query tell the three parties apart.
        usage = dict(input_tokens={'coordinator': 100, 'subagent': 20}.get(role, 0), output_tokens=0, cache_read_input_tokens=3)
        message = dict(id='msg_'+uuid.uuid4().hex, type='message', role='assistant', model=request['model'],
                       content=[], stop_reason=None, stop_sequence=None, usage=usage)
        if tool:
            block = dict(type='tool_use', id='toolu_'+uuid.uuid4().hex, name=tool[0], input={})
            delta, stop = dict(type='input_json_delta', partial_json=json.dumps(tool[1])), 'tool_use'
        else:
            block, delta, stop = dict(type='text', text=''), dict(type='text_delta', text='done'), 'end_turn'
        events = [('message_start', dict(message=message)), ('content_block_start', dict(index=0, content_block=block)),
                  ('content_block_delta', dict(index=0, delta=delta)), ('content_block_stop', dict(index=0)),
                  ('message_delta', dict(delta=dict(stop_reason=stop, stop_sequence=None), usage=dict(output_tokens=1))),
                  ('message_stop', {})]
        self.reply('text/event-stream', ''.join('event: '+kind+'\ndata: '+json.dumps(dict(type=kind, **body))+'\n\n' for kind, body in events).encode())

    def reply(self, kind, content):
        self.send_response(200)
        self.send_header('Content-Type', kind)
        self.send_header('Content-Length', str(len(content)))
        self.end_headers()
        self.wfile.write(content)


# Codex logs each final answer as `response`; the collector must keep only IDs.
WORKER_ANSWER_CANARY = 'Fixture complete. telemetry-canary-worker-final-answer'


class WorkerProvider(http.server.BaseHTTPRequestHandler):
    """A Codex worker spawns one native child thread, waits for it and finishes."""
    def log_message(self, *_):
        pass

    def do_POST(self):
        size = int(self.headers.get('Content-Length', '0'))
        if not 0 < size <= 8*1024*1024:
            return self.send_error(413)
        request = json.loads(self.rfile.read(size))
        server = self.server
        with server.lock:
            server.request_count += 1
        namespaces = {child.get('name'): tool.get('name') for tool in request.get('tools', []) if tool.get('type') == 'namespace'
                      for child in tool.get('tools', [])}
        tools = {child.get('name'): child for tool in request.get('tools', [])
                 for child in (tool.get('tools', []) if tool.get('type') == 'namespace' else [tool])}
        items = [item for item in request.get('input', []) if isinstance(item, dict)]
        text = ' '.join(part.get('text', '') for item in items if item.get('role') == 'user'
                        for part in (item.get('content') if isinstance(item.get('content'), list) else []) if isinstance(part, dict))
        last = items[-1] if items else {}
        call, role = None, 'worker'
        if 'CHILD-PROBE' in text:
            role = 'child'
        elif last.get('type') == 'function_call_output' and server.calls.get(last.get('call_id')) == 'spawn':
            wait = next((name for name in ('wait_agent', 'wait') if name in tools), None)
            if wait:
                output = last.get('output')
                try:
                    spawned = json.loads(output if isinstance(output, str) else json.dumps(output))
                    agent = spawned.get('agent_id') or spawned.get('id') or spawned.get('task_name')
                except (ValueError, AttributeError):
                    agent = None
                properties = tools[wait].get('parameters', {}).get('properties', {})
                arguments = {key: [agent] for key in ('ids', 'targets') if key in properties and agent}
                if 'timeout_ms' in properties:
                    arguments['timeout_ms'] = 60000
                call = (wait, arguments, 'wait')
        elif 'WORKER-PROBE' in text and 'spawn_agent' in tools and not any(item.get('type') == 'function_call_output' for item in items):
            properties = tools['spawn_agent'].get('parameters', {}).get('properties', {})
            arguments = {key: 'CHILD-PROBE: reply done.' for key in ('message', 'task', 'prompt') if key in properties}
            if 'task_name' in properties:
                arguments['task_name'] = 'fixture_child'
            if 'items' in properties and not arguments:
                arguments['items'] = [{'type': 'text', 'text': 'CHILD-PROBE: reply done.'}]
            call = ('spawn_agent', arguments, 'spawn')
        elif not request.get('tools'):
            role = 'other'
        response = uuid.uuid4().hex
        if call:
            server.calls['call_'+response] = call[2]
            item = {'type': 'function_call', 'id': 'fc_'+response, 'call_id': 'call_'+response, 'name': call[0], 'arguments': json.dumps(call[1])}
            if namespaces.get(call[0]) not in (None, 'functions'):
                item['namespace'] = namespaces[call[0]]
        else:
            # Codex logs an agent response only for a final answer, as real models mark it.
            item = {'type': 'message', 'id': 'msg_'+response, 'role': 'assistant', 'status': 'completed', 'phase': 'final_answer',
                    'content': [{'type': 'output_text', 'text': WORKER_ANSWER_CANARY, 'annotations': []}]}
        with server.lock:
            server.roles.append(role)
        # The worker's and its child's input counts tell them apart in one query.
        usage = {'input_tokens': {'worker': 1, 'child': 2}.get(role, 0), 'output_tokens': 1, 'total_tokens': 2}
        events = [{'type': 'response.created', 'response': {'id': 'resp_'+response}},
                  {'type': 'response.output_item.done', 'output_index': 0, 'item': item},
                  {'type': 'response.completed', 'response': {'id': 'resp_'+response, 'status': 'completed', 'output': [item], 'usage': usage}}]
        self.send_response(200)
        self.send_header('Content-Type', 'text/event-stream')
        self.end_headers()
        self.wfile.write(b''.join(('data: '+json.dumps(event)+'\n\n').encode() for event in events))
        self.wfile.flush()


# The task report: the coordinator by session, its native subagents by agent
# ID, workers through the parent-session binding they reported, and their
# native child threads through the child-thread binding.
USAGE_REPORT = """SELECT CASE WHEN m.session_id=:session AND m.native_agent_id IS NULL THEN 'coordinator'
    WHEN m.session_id=:session THEN 'subagent'
    WHEN m.session_id IN (SELECT session_id FROM relations WHERE json_extract(body,'$.relation')='child_thread') THEN 'worker-thread'
    ELSE 'worker' END AS party, m.client, count(*) AS requests,
    sum(coalesce(m.input_tokens,0)+coalesce(m.input_token_count,0)) AS input_tokens
  FROM model_requests m WHERE m.session_id=:session OR m.session_id IN (SELECT session_id FROM relations
    WHERE json_extract(body,'$.relation')='parent_session' AND json_extract(body,'$.parent_session_id')=:session)
  GROUP BY 1,2 ORDER BY 1"""


def usage_report(data, session):
    with closing(sqlite3.connect('file:'+str(data/'telemetry.sqlite')+'?mode=ro', uri=True, timeout=2)) as db:
        return [tuple(row) for row in db.execute(USAGE_REPORT, dict(session=session))]


def assert_usage_attribution(root, binaries, fixture, native, tomli_w, env, endpoint, token, data, system_policy):
    """A plain Claude coordinator, its native subagent, a Codex worker from its Bash and the worker's child thread, by one query."""
    home, claude, codex, project = root/'home', root/'claude', root/'codex', root/'project'
    for path in (root, home, claude, codex, project):
        path.mkdir(mode=0o700)
    worker = http.server.ThreadingHTTPServer(('127.0.0.1', 0), WorkerProvider)
    worker.request_count, worker.calls, worker.roles, worker.lock = 0, {}, [], threading.Lock()
    coordinator = http.server.ThreadingHTTPServer(('127.0.0.1', 0), CoordinatorProvider)
    coordinator.roles = []
    # Telemetry names no parent itself: the worker starts with the environment
    # the shipped agent bridge gives a worker, which names the starting session.
    bridge = Path(__file__).resolve().parents[5]/'config/skills/core/subyard-agent-bridge/bridge.py'
    start = ('import importlib.util,os,sys\n'
             'spec=importlib.util.spec_from_file_location("bridge",sys.argv[1]);bridge=importlib.util.module_from_spec(spec);spec.loader.exec_module(bridge)\n'
             'os.execve(sys.argv[2],sys.argv[2:],bridge.parent_session_hint(dict(os.environ),os.environ))')
    coordinator.command = shlex.join(['python3', '-I', '-c', start, str(bridge), str(binaries/'codex'), 'exec',
                                      '--skip-git-repo-check', 'WORKER-PROBE: delegate to one child, then reply done.'])
    for server in (worker, coordinator):
        threading.Thread(target=server.serve_forever, daemon=True).start()
    config = {'model': 'subyard-fixture', 'model_provider': 'fixture', 'sandbox_mode': 'danger-full-access', 'approval_policy': 'never',
              'features': {'plugins': False, 'unified_exec': False}, 'analytics': {'enabled': True},
              'model_providers': {'fixture': {'name': 'Fixture', 'base_url': f'http://127.0.0.1:{worker.server_port}/v1',
                  'wire_api': 'responses', 'requires_openai_auth': False, 'request_max_retries': 0, 'stream_max_retries': 0}}}
    settings = {'env': {'OTEL_LOGS_EXPORTER': 'none'}} if system_policy else native.desired('claude', endpoint)
    claude_env = settings['env']
    if not system_policy:
        config['otel'] = native.desired('codex', endpoint)['otel']
    native.publish(codex/'config.toml', tomli_w.dumps(config).encode(), None)
    native.publish(claude/'settings.json', (json.dumps(settings)+'\n').encode(), None)
    local = dict(env, HOME=str(home), CODEX_HOME=str(codex), CLAUDE_CONFIG_DIR=str(claude), DISABLE_AUTOUPDATER='1',
                 ANTHROPIC_BASE_URL=f'http://127.0.0.1:{coordinator.server_port}', ANTHROPIC_API_KEY='local-fixture-only')
    if not system_policy:
        local.update(claude_env)
    try:
        result = claude_command([str(binaries/'claude'), '-p', 'COORDINATOR-PROBE: delegate, then run the worker.',
            '--allowedTools', 'Bash,Agent,Task', '--output-format', 'stream-json', '--verbose', '--max-turns', '6'],
            token, env=local, cwd=project, capture_output=True, timeout=300, check=True)
    finally:
        for server in (worker, coordinator):
            server.shutdown()
            server.server_close()
    session = next(json.loads(line)['session_id'] for line in result.stdout.splitlines() if line.strip() and json.loads(line).get('session_id'))
    roles = {role: coordinator.roles.count(role) for role in set(coordinator.roles)}
    threads = {role: worker.roles.count(role) for role in set(worker.roles)}
    assert roles.get('subagent') and roles.get('coordinator', 0) >= 3 and threads.get('worker') and threads.get('child'), json.dumps(dict(roles=roles, worker=threads))
    # Each worker request reports one input token and each child request two;
    # Codex sends some auxiliary requests without a completed-response event.
    expected = {'coordinator': ('claude', 100*roles['coordinator']), 'subagent': ('claude', 20*roles['subagent']),
                'worker': ('codex', True), 'worker-thread': ('codex', True)}
    per_request = {'worker': 1, 'worker-thread': 2}
    deadline = time.monotonic()+40
    while True:
        report = usage_report(data, session)
        parties = {row[0]: (row[1], row[3] if row[0] not in per_request else 0 < row[3] == per_request[row[0]]*row[2]) for row in report}
        if parties == expected or time.monotonic() >= deadline:
            break
        time.sleep(.5)
    assert parties == expected, json.dumps(dict(report=report, roles=roles, worker=threads))
    marker = WORKER_ANSWER_CANARY.split()[-1].encode()
    for row in exported(data):
        assert marker not in base64.b64decode(row.get('protobuf') or '') and marker not in json.dumps(row.get('body')).encode(), \
            'worker final-answer text reached the store: '+row['kind']
    for name in ('telemetry.sqlite', 'telemetry.sqlite-wal'):
        if (data/name).exists():
            assert marker not in (data/name).read_bytes(), 'worker final-answer text reached '+name
    # Re-delivered native exports add no usage.
    for protobuf in raw_otlp(data, 'otlp/logs'):
        request = urllib.request.Request(endpoint+'/v1/logs', data=protobuf, headers={'Content-Type': 'application/x-protobuf', 'Authorization': 'Bearer '+token})
        with urllib.request.urlopen(request, timeout=10) as response:
            assert response.status == 200
    assert usage_report(data, session) == report, 're-delivery changed the usage report'
    archive, copy = root/'export.jsonl', root/'replayed'
    archive.write_bytes(b''.join(json.dumps(row).encode()+b'\n' for row in exported(data)))
    copy.mkdir(mode=0o700)
    for _ in range(2):
        subprocess.run([collector(), 'replay', '--data-dir', str(copy), '--input', str(archive)], check=True, timeout=300, capture_output=True)
        assert usage_report(copy, session) == report, 'replay changed the usage report'
    print('usage-report: '+json.dumps(report), flush=True)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--state', type=Path, required=True)
    parser.add_argument('--data-dir', type=Path, required=True)
    parser.add_argument('--source', type=Path, required=True)
    parser.add_argument('--exercise', action='store_true')
    parser.add_argument('--system-policy', action='store_true')
    parser.add_argument('--kind', choices=['vm', 'android'])
    parser.add_argument('command', nargs=argparse.REMAINDER)
    args = parser.parse_args()
    if args.exercise:
        return exercise(args)
    state = args.state/'native'
    state.mkdir(mode=0o700)
    binaries = install_agents(state)
    sys.path.insert(0, str(args.source/'config/profiles/telemetry'))
    sys.path.insert(0, str(args.source/'internal/adapters/configmaterial'))
    import native
    import tomli_w
    token = (args.data_dir.with_name(args.data_dir.name+'-private')/'auth.token').read_text().strip()
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
    config = {'model': 'subyard-fixture', 'model_provider': 'fixture', 'features': {'plugins': False, 'unified_exec': False},
              'analytics': {'enabled': True}, # Explicit isolated fixture opt-in; production intent remains untouched.
              'model_providers': {'fixture': {'name': 'Fixture', 'base_url': f'http://127.0.0.1:{provider.server_port}/v1',
                  'wire_api': 'responses', 'requires_openai_auth': False, 'request_max_retries': 0, 'stream_max_retries': 0}}}
    # A competing user exporter, helper and static header exercise the managed configuration's recursive merge.
    config['otel'] = native.desired('codex', 'http://127.0.0.1:1' if args.system_policy else endpoint,
                                    '/bin/false' if args.system_policy else native.HELPER)['otel']
    if args.system_policy:
        for key in ('exporter', 'metrics_exporter', 'trace_exporter'):
            config['otel'][key]['otlp-http'].update(protocol='json', headers={'Authorization': 'Bearer fixture-user-header'})
    path = state/'codex/config.toml'
    native.publish(path, tomli_w.dumps(config).encode(), native.snapshot(path))
    app = codex_app_class(fixture.App)(['subyard-telemetry-run', '--agent', 'codex', '--scope-mode', 'require', '--shared-backend', '--', str(binaries/'codex'), 'app-server'], env, state/'project')
    threads = set()
    try:
        for index in range(2):
            thread = app.start_fixture_thread(state/'project')
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
    runs = wait_sessions(args.data_dir, 'codex', set(threads))
    assert len(runs) == 1, 'two backend sessions must retain the one real shared backend run'
    run_id = next(iter(runs))
    wait_shared_scope(args.data_dir, run_id)
    server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), ClaudeProvider)
    server.requests = 0
    threading.Thread(target=server.serve_forever, daemon=True).start()
    local = dict(env, ANTHROPIC_BASE_URL=f'http://127.0.0.1:{server.server_port}',
                 ANTHROPIC_API_KEY='local-fixture-only', DISABLE_AUTOUPDATER='1', CLAUDE_CODE_PROPAGATE_TRACEPARENT='1')
    if args.system_policy:
        # The managed policy's env and helper take precedence over these user settings.
        settings = {'env': {'OTEL_LOGS_EXPORTER': 'none'}, 'otelHeadersHelper': '/bin/false'}
    else:
        settings = native.desired('claude', endpoint)
        local.update(settings['env'])
    native.publish(state/'claude/settings.json', (json.dumps(settings)+'\n').encode(), None)
    try:
        result = claude_command(['subyard-telemetry-run', '--agent', 'claude', '--scope-mode', 'require', '--', str(binaries/'claude'),
                                 '-p', 'Return probe.', '--output-format', 'stream-json', '--verbose', '--max-turns', '1'],
                                token, env=local, cwd=state/'project', capture_output=True, timeout=60, check=True)
        frames = [json.loads(line) for line in result.stdout.splitlines() if line.strip()]
        session = next(frame['session_id'] for frame in frames if frame.get('session_id'))
        assert server.requests > 0
    finally:
        server.shutdown()
        server.server_close()
    wait_claude_sessions(args.data_dir, {session}, result, token)
    kinds = {row[0] for row in query(args.data_dir, "SELECT DISTINCT kind FROM records WHERE kind IN ('otlp/logs','otlp/metrics','otlp/traces')")}
    assert kinds == {'otlp/logs', 'otlp/metrics', 'otlp/traces'}, 'actual native signal coverage incomplete'
    # The Claude probe here runs no tool; the resources exercise covers its success field.
    assert_native_log_fields(args.data_dir, success=('codex',))
    assert_short_run_traces(args.data_dir, endpoint, env)
    assert_run_attribution(state, binaries, fixture, native, tomli_w, env, endpoint, token, args.data_dir, args.system_policy)
    assert_usage_attribution(state/'usage', binaries, fixture, native, tomli_w, env, endpoint, token, args.data_dir, args.system_policy)
    print('ok: real native versions; loopback providers; two shared Codex sessions and Claude session linked by exported evidence; '
          'coordinator, subagent and worker usage by one query')


if __name__ == '__main__':
    main()
