#!/usr/bin/env python3
"""CAS ownership of native exporter leaves; preserve unrelated settings and analytics intent."""
import json
import os
import re
import stat
import tempfile
from contextlib import ExitStack
from decimal import Decimal

import guest_json as materializer

CONFIG_LIMIT = 4*1024*1024


def safe(path):
    for entry in (path, *path.parents):
        if entry.is_symlink():
            raise ValueError('unsafe native path')
    if path.exists() and not path.is_file():
        raise ValueError('native configuration is not regular')


def snapshot(path, max_bytes=CONFIG_LIMIT):
    safe(path)
    if not path.exists():
        return None
    info = path.stat()
    if info.st_nlink != 1 or info.st_size > max_bytes:
        raise ValueError('unsafe native file')
    with path.open('rb') as stream:
        content = stream.read(max_bytes+1)
    if len(content) > max_bytes:
        raise ValueError('native file exceeds size bound')
    return content


def publish(path, content, expected, uid=None, gid=None, max_bytes=CONFIG_LIMIT, mode=0o600):
    safe(path)
    if len(content) > max_bytes:
        raise ValueError('native file exceeds size bound')
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix='.telemetry-', dir=path.parent)
    try:
        with os.fdopen(fd, 'w+b') as output:
            os.fchmod(output.fileno(), mode)
            if uid is not None:
                os.fchown(output.fileno(), uid, gid)
            output.write(content)
            output.flush()
            os.fsync(output.fileno())
            output.seek(0)
            if output.read() != content:
                raise ValueError('native payload verification failed')
        if snapshot(path, max_bytes=max_bytes) != expected:
            raise ValueError('native configuration changed')
        os.replace(temporary, path)
        directory = os.open(path.parent, os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def desired(agent, endpoint, token):
    if agent == 'codex':
        header = re.compile(r'Bearer [0-9a-f]{64}') if token is None else 'Bearer '+token
        result = {'log_user_prompt': False}
        for key, path in [('exporter', 'logs'), ('metrics_exporter', 'metrics'), ('trace_exporter', 'traces')]:
            result[key] = {'otlp-http': {'endpoint': endpoint+'/v1/'+path, 'protocol': 'binary',
                                       'headers': {'Authorization': header}}}
        return result
    header = re.compile(r'Authorization=Bearer [0-9a-f]{64}') if token is None else 'Authorization=Bearer '+token
    result = {'CLAUDE_CODE_ENABLE_TELEMETRY': '1', 'CLAUDE_CODE_ENHANCED_TELEMETRY_BETA': '1',
              'OTEL_LOG_USER_PROMPTS': '0', 'OTEL_LOG_TOOL_DETAILS': '0', 'OTEL_LOG_TOOL_CONTENT': '0'}
    for signal in ('LOGS', 'METRICS', 'TRACES'):
        result['OTEL_'+signal+'_EXPORTER'] = 'otlp'
        result['OTEL_EXPORTER_OTLP_'+signal+'_ENDPOINT'] = endpoint+'/v1/'+signal.lower()
        result['OTEL_EXPORTER_OTLP_'+signal+'_PROTOCOL'] = 'http/protobuf'
        result['OTEL_EXPORTER_OTLP_'+signal+'_HEADERS'] = header
    return result


def agent_path(home, agent):
    return home/('.claude/settings.json' if agent == 'claude' else '.codex/config.toml')


class AgentConfigRefusal(ValueError):
    def __init__(self, agent, path):
        self.agent = agent
        super().__init__('invalid unmanaged agent configuration '+str(path))


class AgentConfigRefusals(ValueError):
    def __init__(self, failures):
        self.failures = failures
        super().__init__('multiple agent configuration refusals')


def reconcile(home, state, agents, endpoint, token, writer, uid, gid, check=False, templates=None, developer=None):
    templates = templates or {}
    developer = developer or home.name
    with ExitStack() as locks:
        return reconcile_locked(home, state, agents, endpoint, token, writer, uid, gid, check, templates, developer, locks)


def reconcile_locked(home, state, agents, endpoint, token, writer, uid, gid, check, templates, developer, locks):
    original = snapshot(state)
    if original is not None:
        info = state.stat()
        if info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o600:
            raise ValueError('private ownership required')
        baseline = json.loads(original, parse_float=Decimal, parse_constant=Decimal)
        if baseline.get('schema') != 1 or not isinstance(baseline.get('agents'), dict):
            raise ValueError('invalid native ownership')
        if set(baseline['agents'])-{'claude', 'codex'}:
            raise ValueError('unknown native ownership')
    else:
        baseline = {'schema': 1, 'agents': {}}
    next_state = {**baseline, 'agents': baseline['agents'].copy()}
    pending = {**baseline, 'agents': baseline['agents'].copy()}
    edits = []
    references = []
    failures = []
    for agent in ('claude', 'codex'):
        section, file_format = ('env', 'json') if agent == 'claude' else ('otel', 'toml')
        owned = baseline['agents'].get(agent, {})
        enabled = agent in agents
        if not enabled and not owned:
            continue
        try:
            path = agent_path(home, agent)
            entry = {'receipt': str(state), 'path': '/agents/'+agent, 'section': section}
            want = desired(agent, endpoint, token) if enabled else None
            final, transitional, edit, register = materializer.prepare_contribution(
                locks, str(path), str(home), uid, file_format, developer, entry, owned, want,
                templates.get(agent), check=check,
                preflight=token is None, writer=writer.dumps, gid=gid)
            if enabled:
                next_state['agents'][agent] = final
                pending['agents'][agent] = transitional
            else:
                next_state['agents'].pop(agent, None)
            if edit is not None:
                edits.append(edit)
            if register is not None:
                references.append(register)
        except materializer.InvalidContributionDestination:
            failures.append((agent, AgentConfigRefusal(agent, path)))
        except (OSError, ValueError, KeyError) as error:
            failures.append((agent, error))
    if len(failures) == 1:
        raise failures[0][1]
    if failures:
        raise AgentConfigRefusals(failures)

    def receipt(value):
        try:
            payload = materializer.semantic_payload(value, compact_numbers=True) + b'\n'
        except (TypeError, ValueError):
            raise ValueError('invalid native ownership value') from None
        if len(payload) > CONFIG_LIMIT:
            raise ValueError('native ownership exceeds size bound')
        return payload

    if check:
        return not edits and not references and receipt(baseline) == receipt(next_state)
    intent, final = receipt(pending), receipt(next_state)
    for register in references:
        register()
    publish(state, intent, original)
    for edit in edits:
        edit()
    publish(state, final, intent)
    return True
