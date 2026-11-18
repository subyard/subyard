#!/usr/bin/env python3
"""CAS ownership of native exporter leaves; preserve unrelated settings and analytics intent."""
import copy
import json
import math
import os
import stat
import tempfile
from contextlib import ExitStack
import tomllib

import guest_json as materializer

CONFIG_LIMIT = 4*1024*1024


def semantic_equal(left, right):
    # Native booleans are distinct from numbers; unrelated TOML NaNs stay equal
    # across the snapshot copy even though ordinary float equality rejects them.
    if type(left) is not type(right):
        return False
    if isinstance(left, dict):
        return left.keys() == right.keys() and all(semantic_equal(value, right[key]) for key, value in left.items())
    if isinstance(left, list):
        return len(left) == len(right) and all(semantic_equal(a, b) for a, b in zip(left, right))
    if isinstance(left, float) and math.isnan(left) and math.isnan(right):
        return True
    return left == right


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
        result = {'log_user_prompt': False}
        for key, path in [('exporter', 'logs'), ('metrics_exporter', 'metrics'), ('trace_exporter', 'traces')]:
            result[key] = {'otlp-http': {'endpoint': endpoint+'/v1/'+path, 'protocol': 'binary',
                                       'headers': {'Authorization': 'Bearer '+token}}}
        return result
    result = {'CLAUDE_CODE_ENABLE_TELEMETRY': '1', 'CLAUDE_CODE_ENHANCED_TELEMETRY_BETA': '1',
              'OTEL_LOG_USER_PROMPTS': '0', 'OTEL_LOG_TOOL_DETAILS': '0', 'OTEL_LOG_TOOL_CONTENT': '0'}
    for signal in ('LOGS', 'METRICS', 'TRACES'):
        result['OTEL_'+signal+'_EXPORTER'] = 'otlp'
        result['OTEL_EXPORTER_OTLP_'+signal+'_ENDPOINT'] = endpoint+'/v1/'+signal.lower()
        result['OTEL_EXPORTER_OTLP_'+signal+'_PROTOCOL'] = 'http/protobuf'
        result['OTEL_EXPORTER_OTLP_'+signal+'_HEADERS'] = 'Authorization=Bearer '+token
    return result


def agent_document(home, agent):
    path = home/('.claude/settings.json' if agent == 'claude' else '.codex/config.toml')
    before = snapshot(path)
    cause = 'missing' if before is None else 'empty' if not before.strip() else ''
    try:
        obj = (json.loads(before) if agent == 'claude' else tomllib.loads(before.decode())) if before and before.strip() else {}
    except (ValueError, UnicodeError):
        obj, cause = {}, 'invalid'
    section = 'env' if agent == 'claude' else 'otel'
    if not isinstance(obj, dict):
        cause = 'invalid'
    elif not isinstance(obj.get(section, {}), dict):
        cause = 'section'
    return path, before, obj, cause


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
        baseline = json.loads(original)
        if baseline.get('schema') != 1 or not isinstance(baseline.get('agents'), dict):
            raise ValueError('invalid native ownership')
        if set(baseline['agents'])-{'claude', 'codex'}:
            raise ValueError('unknown native ownership')
    else:
        baseline = {'schema': 1, 'agents': {}}
    next_state = copy.deepcopy(baseline)
    pending = copy.deepcopy(baseline)
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
            path = home/('.claude/settings.json' if agent == 'claude' else '.codex/config.toml')
            asset = materializer.asset_key(developer, str(path))
            locks.enter_context(materializer.asset_lock(asset+'.lock', check, allow_missing=True))
            entry = {'receipt': str(state), 'path': '/agents/'+agent, 'section': section}
            reference = materializer.contribution_reference(asset+'.contribution')
            if reference is not None and reference != entry:
                raise ValueError('configuration contribution ownership conflict')
            path, before, obj, cause = agent_document(home, agent)
            template = (json.loads(templates[agent]) if file_format == 'json' else tomllib.loads(templates[agent].decode())) if agent in templates else {}
            if not isinstance(template, dict):
                raise ValueError('agent template root must be an object')
            template_owned = agent in templates and materializer.template_owns_document(asset, developer, str(path), file_format)
            if cause in ('invalid', 'section') and not (owned or template_owned):
                raise AgentConfigRefusal(agent, path)
            if cause == 'missing' and not enabled:
                next_state['agents'].pop(agent, None)
                continue
            if token is None:
                # Initial inspection checks ownership and paths before creating a guest token.
                continue
            want = desired(agent, endpoint, token) if enabled else {}
            materializer.check_contribution(template, {section: want} if want else {}, file_format)
            if check and cause in ('missing', 'empty', 'invalid') and enabled and reference is not None and template_owned and all(
                    key in owned and semantic_equal(value, owned[key]['managed']) for key, value in want.items()):
                # The ordinary template owner can rebuild its installed contribution.
                continue
            if cause in ('invalid', 'section'):
                if not isinstance(obj, dict):
                    obj = {}
                obj.pop(section, None)
            previous = copy.deepcopy(obj)
            if cause and template_owned:
                materializer.overlay(obj, template)
            group = obj.setdefault(section, {})
            final, transitional = {}, {}
            for key, value in want.items():
                old = owned.get(key, {'present': key in group, 'value': group.get(key), 'managed': value})
                allowed = [old['managed'], *old.get('previous', [])]
                current = {'present': key in group, 'value': group.get(key)}
                prior = {'present': old['present'], 'value': old['value']}
                if key in group and not semantic_equal(current, prior) and not any(semantic_equal(group[key], item) for item in allowed):
                    raise ValueError('native exporter ownership conflict')
                final[key] = {'present': old['present'], 'value': old['value'], 'managed': value}
                transitional[key] = {**final[key], 'previous': allowed}
                group[key] = value
            for key, old in owned.items():
                if key not in want and key in group and any(semantic_equal(group[key], item) for item in [old['managed'], *old.get('previous', [])]):
                    if old['present']:
                        group[key] = old['value']
                    else:
                        group.pop(key)
            if not group:
                obj.pop(section, None)
            if enabled:
                next_state['agents'][agent] = final
                pending['agents'][agent] = transitional
            else:
                next_state['agents'].pop(agent, None)
            if not semantic_equal(previous, obj) or cause in ('invalid', 'section'):
                content = materializer.document_payload(obj, before, file_format, writer.dumps)
                edits.append((path, content, before, file_format))
            elif enabled and not (check and reference is not None and template_owned) and stat.S_IMODE(path.stat().st_mode) != 0o600:
                edits.append((path, before, before, file_format))
            if enabled and reference is None:
                references.append((asset+'.contribution', entry))
        except (OSError, ValueError, KeyError) as error:
            failures.append((agent, error))
    if len(failures) == 1:
        raise failures[0][1]
    if failures:
        raise AgentConfigRefusals(failures)
    if check:
        return not edits and not references and semantic_equal(baseline, next_state)
    for path, entry in references:
        materializer.set_contribution_reference(path, entry)
    intent = (json.dumps(pending)+'\n').encode()
    publish(state, intent, original)
    for path, content, before, file_format in edits:
        materializer.replace_destination(str(path), str(home), uid, before, content, True, 0o600, file_format, gid)
    publish(state, (json.dumps(next_state)+'\n').encode(), intent)
    return True
