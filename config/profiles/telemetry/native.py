#!/usr/bin/env python3
"""CAS ownership of native exporter leaves; preserve unrelated settings and analytics intent."""
import copy
import json
import math
import os
from pathlib import Path
import stat
import tempfile
import tomllib

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


def publish(path, content, expected, uid=None, gid=None, max_bytes=CONFIG_LIMIT):
    safe(path)
    if len(content) > max_bytes:
        raise ValueError('native file exceeds size bound')
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix='.telemetry-', dir=path.parent)
    try:
        with os.fdopen(fd, 'wb') as output:
            os.fchmod(output.fileno(), 0o600)
            if uid is not None:
                os.fchown(output.fileno(), uid, gid)
            output.write(content)
            output.flush()
            os.fsync(output.fileno())
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


def reconcile(home, state, agents, endpoint, token, writer, uid, gid, check=False):
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
    for agent in ('claude', 'codex'):
        path = home/('.claude/settings.json' if agent == 'claude' else '.codex/config.toml')
        section = 'env' if agent == 'claude' else 'otel'
        owned = baseline['agents'].get(agent, {})
        enabled = agent in agents
        if not enabled and not owned:
            continue
        before = snapshot(path)
        obj = (json.loads(before) if agent == 'claude' else tomllib.loads(before.decode())) if before else {}
        if not isinstance(obj, dict) or not isinstance(obj.get(section, {}), dict):
            raise ValueError('invalid native configuration')
        previous = copy.deepcopy(obj)
        group = obj.setdefault(section, {})
        want = desired(agent, endpoint, token) if enabled else {}
        final, transitional = {}, {}
        for key, value in want.items():
            old = owned.get(key, {'present': key in group, 'value': group.get(key), 'managed': value})
            allowed = [old['managed'], *old.get('previous', [])]
            current = {'present': key in group, 'value': group.get(key)}
            prior = {'present': old['present'], 'value': old['value']}
            if not semantic_equal(current, prior) and not (key in group and any(semantic_equal(group[key], item) for item in allowed)):
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
        if not semantic_equal(previous, obj):
            content = (json.dumps(obj, indent=2)+'\n').encode() if agent == 'claude' else writer.dumps(obj).encode()
            edits.append((path, content, before))
    if check:
        return not edits and semantic_equal(baseline, next_state)
    publish(state, (json.dumps(pending)+'\n').encode(), original)
    for path, content, before in edits:
        publish(path, content, before, uid, gid)
        os.chown(path.parent, uid, gid)
    publish(state, (json.dumps(next_state)+'\n').encode(), snapshot(state))
    return True


def relinquish_openlit(home, legacy, writer, uid, gid):
    if not legacy.exists():
        return
    marker = legacy.parent/'marker'
    if snapshot(marker) != b'subyard-openlit-v1\n' or marker.stat().st_uid != 0 or legacy.parent.stat().st_uid != 0 or stat.S_IMODE(legacy.parent.stat().st_mode) != 0o700:
        raise ValueError('legacy ownership unavailable')
    baseline = json.loads(snapshot(legacy))
    for agent, owned in baseline.get('agents', {}).items():
        if agent not in ('claude', 'codex'):
            raise ValueError('unknown legacy native ownership')
        path = home/('.claude/settings.json' if agent == 'claude' else '.codex/config.toml')
        content = snapshot(path)
        value = (json.loads(content) if agent == 'claude' else tomllib.loads(content.decode())) if content else {}
        group = value.get('env' if agent == 'claude' else 'otel', {})
        for key, entry in owned.items():
            prior = {'present': entry['present'], 'value': entry['value']}
            current = {'present': key in group, 'value': group.get(key)}
            if not semantic_equal(current, prior) and not (key in group and any(semantic_equal(group[key], item) for item in [entry['managed'], *entry.get('previous', [])])):
                raise ValueError('legacy native exporter changed')
    reconcile(home, legacy, [], '', '', writer, uid, gid)
