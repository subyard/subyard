#!/usr/bin/env python3
"""CAS ownership of native exporter leaves; preserve unrelated settings and analytics intent."""
import hashlib
import json
import os
import stat
import tempfile
from contextlib import ExitStack
from decimal import Decimal

import guest_json as materializer

CONFIG_LIMIT = 4*1024*1024
TOKEN_BYTES = 32


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
        sync_directory(path.parent)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def desired(agent, endpoint, token):
    def bearer(prefix):
        if token is None:
            return materializer.GeneratedValue(prefix + '0'*(TOKEN_BYTES*2))
        return prefix + token

    if agent == 'codex':
        header = bearer('Bearer ')
        result = {'log_user_prompt': False}
        for key, path in [('exporter', 'logs'), ('metrics_exporter', 'metrics'), ('trace_exporter', 'traces')]:
            result[key] = {'otlp-http': {'endpoint': endpoint+'/v1/'+path, 'protocol': 'binary',
                                       'headers': {'Authorization': header}}}
        return result
    header = bearer('Authorization=Bearer ')
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


def read_baseline(state):
    original = snapshot(state)
    if original is not None:
        info = state.stat()
        if info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o600:
            raise ValueError('private ownership required')
        baseline = json.loads(original, parse_float=Decimal, parse_constant=Decimal)
        if not isinstance(baseline, dict) or baseline.get('schema') != 1 or not isinstance(baseline.get('agents'), dict):
            raise ValueError('invalid native ownership')
        if set(baseline['agents'])-{'claude', 'codex'}:
            raise ValueError('unknown native ownership')
    else:
        baseline = {'schema': 1, 'agents': {}}
    return original, baseline


def receipt(value):
    try:
        payload = materializer.semantic_payload(value, compact_numbers=True, typed_floats=True) + b'\n'
    except (TypeError, ValueError):
        raise ValueError('invalid native ownership value') from None
    if len(payload) > CONFIG_LIMIT:
        raise ValueError('native ownership exceeds size bound')
    return payload


def sync_directory(path):
    directory = os.open(path, os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(directory)
    finally:
        os.close(directory)


def save_intent(journal, state_before, agent, path, owned, retired, projection, staged):
    record = {'schema': 1, 'agent': agent, 'destination': str(path),
              'receipt': hashlib.sha256(state_before or b'').hexdigest(),
              'inode': [staged.st_dev, staged.st_ino], 'owned': owned,
              'retired': retired, 'projection': projection}
    publish(journal, receipt(record), snapshot(journal))
    # The control directory itself may have been created on this first apply.
    sync_directory(journal.parent.parent)


def recover_intent(journal, state, original, baseline, home, uid, developer, check):
    pending = snapshot(journal)
    if pending is None:
        return original, baseline
    materializer.validate_regular(journal, os.getuid(), 0o600)
    intent = json.loads(pending, parse_float=Decimal, parse_constant=Decimal)
    if not isinstance(intent, dict) or intent.get('schema') != 1 or intent.get('agent') not in ('claude', 'codex'):
        raise ValueError('invalid native publication intent')
    agent = intent['agent']
    path = agent_path(home, agent)
    if intent['destination'] != str(path) or intent['receipt'] != hashlib.sha256(original or b'').hexdigest():
        return original, baseline
    owned = materializer.published_contribution(intent['owned'])
    retired = materializer.published_contribution(intent['retired'])
    file_format = 'json' if agent == 'claude' else 'toml'
    section = 'env' if agent == 'claude' else 'otel'
    entries = materializer.contribution_entries(section, owned.keys() | retired.keys())
    asset = materializer.asset_key(developer, str(path))
    with materializer.asset_lock(asset+'.lock', check, allow_missing=True):
        if not materializer.validate_destination_parent(str(path), str(home), uid, False):
            return original, baseline
        try:
            before = path.lstat()
            current, _ = materializer.read_current(path, allow_invalid=True, file_format=file_format, private=True)
            after = path.lstat()
        except FileNotFoundError:
            return original, baseline
        if current is None or not os.path.samestat(before, after) or after.st_uid != uid:
            return original, baseline
        # A surviving staged inode witnesses publication despite in-place edits.
        # An editor can also atomically save unrelated changes after publication.
        # A changed, matching owned projection witnesses that case.
        group = current.get(section)
        matching_inode = [after.st_dev, after.st_ino] == intent['inode'] and isinstance(group, dict) and any(
            key in group and materializer.json_equal(group[key], record['managed'], file_format)
            for key, record in owned.items())
        matching_projection = (intent['projection'] and
                               materializer.projection_digest(current, entries, file_format) == intent['projection'])
        if not matching_inode and not matching_projection:
            return original, baseline
        agents = baseline['agents'].copy()
        if owned:
            agents[agent] = owned
        else:
            agents.pop(agent, None)
        recovered = {**baseline, 'agents': agents}
        if not check:
            completed = receipt(recovered)
            publish(state, completed, original)
            original = completed
        return original, recovered


def receipt_lock(state, read_only):
    # The receipt covers both destinations, even for disjoint selections.
    path = materializer.asset_key('telemetry', str(state)) + '.lock'
    return materializer.asset_lock(path, read_only, allow_missing=True)


def repair_modes(home, state, agents, uid):
    if agents:
        with receipt_lock(state, True):
            _, baseline = read_baseline(state)
            repair_receipted_modes(home, baseline, agents, uid)


def repair_receipted_modes(home, baseline, agents, uid):
    failures = []
    for agent in ('claude', 'codex'):
        if agent not in agents:
            continue
        try:
            if materializer.published_contribution(baseline['agents'].get(agent, {})):
                materializer.repair_document_mode(str(agent_path(home, agent)), str(home), uid)
        except (OSError, ValueError) as error:
            failures.append((agent, error))
    raise_refusals(failures)


def raise_refusals(failures):
    if len(failures) == 1:
        raise failures[0][1]
    if failures:
        raise AgentConfigRefusals(failures)


def reconcile(home, state, agents, endpoint, token, writer, uid, gid, check=False, templates=None, developer=None):
    templates = templates or {}
    developer = developer or home.name
    with receipt_lock(state, check), ExitStack() as locks:
        return reconcile_locked(home, state, agents, endpoint, token, writer, uid, gid, check, templates, developer, locks)


def reconcile_locked(home, state, agents, endpoint, token, writer, uid, gid, check, templates, developer, locks):
    original, baseline = read_baseline(state)
    journal = state.with_suffix('.intent')
    if not check:
        repair_receipted_modes(home, baseline, agents, uid)
        if token is None:
            raise ValueError('collector token required')
    original, recovered = recover_intent(journal, state, original, baseline, home, uid, developer, check)
    if not check and recovered is not baseline:
        repair_receipted_modes(home, recovered, agents, uid)
    baseline, recovered_receipt = recovered, recovered is not baseline
    next_state = {**baseline, 'agents': baseline['agents'].copy()}
    publications = []
    failures = []

    for agent in ('claude', 'codex'):
        section, file_format = ('env', 'json') if agent == 'claude' else ('otel', 'toml')
        enabled = agent in agents
        if not enabled and agent not in baseline['agents']:
            continue
        path = agent_path(home, agent)
        try:
            owned = materializer.published_contribution(baseline['agents'].get(agent, {}))
            if not enabled and not owned:
                next_state['agents'].pop(agent)
                publications.append((agent, None, None, receipt(next_state)))
                continue
            entry = {'receipt': str(state), 'path': '/agents/'+agent, 'section': section}
            want = desired(agent, endpoint, token) if enabled else None
            final, edit, register = materializer.prepare_contribution(
                locks, str(path), str(home), uid, file_format, developer, entry, owned, want,
                templates.get(agent), check=check, writer=writer.dumps, gid=gid,
                save_intent=lambda final, retired, projection, staged, agent=agent, path=path:
                    save_intent(journal, original, agent, path, final, retired, projection, staged))
            if enabled:
                next_state['agents'][agent] = final
            else:
                next_state['agents'].pop(agent, None)
            publications.append((agent, edit, register, receipt(next_state)))
        except materializer.InvalidContributionDestination:
            failures.append((agent, AgentConfigRefusal(agent, path)))
        except (OSError, ValueError, KeyError) as error:
            failures.append((agent, error))
    raise_refusals(failures)

    if check:
        return (not recovered_receipt and all(edit is None and register is None for _, edit, register, _ in publications)
                and receipt(baseline) == receipt(next_state))
    for agent, edit, register, final in publications:
        if register is not None:
            register()
        try:
            if edit is not None:
                edit()
        except materializer.PublicationRefusal as error:
            if not error.recovery:
                raise
            raise AgentConfigRefusals([(agent, error)]) from None
        publish(state, final, original)
        original = final
        if edit is not None and agent in agents:
            repair_receipted_modes(home, next_state, [agent], uid)
    if journal.exists():
        journal.unlink()
        sync_directory(journal.parent)
    return True
