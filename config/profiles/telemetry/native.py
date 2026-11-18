#!/usr/bin/env python3
"""CAS ownership of native exporter leaves; preserve unrelated settings and analytics intent."""
import json
import os
import stat
import tempfile
from contextlib import ExitStack
from decimal import Decimal
from pathlib import Path

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
        materializer.sync_directory(path.parent)
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


def receipt_lock(state, read_only):
    # The receipt covers both destinations, even for disjoint selections.
    path = materializer.asset_key('telemetry', str(state)) + '.lock'
    return materializer.asset_lock(path, read_only, allow_missing=True)


def pending_publications(home, state):
    journal = materializer.publication_journal(state)
    failures = {}
    for agent, section in (('claude', 'env'), ('codex', 'otel')):
        path = str(agent_path(home, agent))
        if pending := materializer.pending_publication(journal, path, section):
            failures[agent] = pending
    if os.path.lexists(journal) and not failures:
        for agent in ('claude', 'codex'):
            failures[agent] = materializer.PendingContributionPublication(agent_path(home, agent), journal)
    return failures


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
    pending = pending_publications(home, state)
    try:
        original, baseline = read_baseline(state)
    except (OSError, ValueError):
        raise_refusals(list(pending.items()))
        raise
    journal = Path(materializer.publication_journal(state))
    next_state = {**baseline, 'agents': baseline['agents'].copy()}
    publications, failures, repairs = [], [], []

    for agent in ('claude', 'codex'):
        section, file_format = ('env', 'json') if agent == 'claude' else ('otel', 'toml')
        enabled = agent in agents
        if agent in pending:
            failures.append((agent, pending[agent]))
            continue
        if not enabled and agent not in baseline['agents']:
            continue
        path = agent_path(home, agent)
        try:
            owned = materializer.published_contribution(baseline['agents'].get(agent, {}), path, state, section)
            if owned:
                repairs.append((agent, path))
            entry = {'receipt': str(state), 'path': '/agents/'+agent, 'section': section}
            want = desired(agent, endpoint, token) if enabled else None
            final, edit, register, intent = materializer.prepare_contribution(
                locks, str(path), str(home), uid, file_format, developer, entry, owned, want,
                templates.get(agent), check=check or bool(pending), writer=writer.dumps, gid=gid)
            if final:
                next_state['agents'][agent] = final
            else:
                next_state['agents'].pop(agent, None)
            publications.append((agent, edit, register, receipt(next_state), receipt(intent) if intent is not None else None))
        except materializer.InvalidContributionDestination:
            failures.append((agent, AgentConfigRefusal(agent, path)))
        except (OSError, ValueError, KeyError) as error:
            failures.append((agent, error))
    if not check and not any(isinstance(error, materializer.PendingContributionPublication) for _, error in failures):
        for agent, path in repairs:
            try:
                materializer.repair_document_mode(str(path), str(home), uid)
            except (OSError, ValueError) as error:
                failures.append((agent, error))
    raise_refusals(failures)

    if check:
        return (all(edit is None and register is None for _, edit, register, *_ in publications)
                and receipt(baseline) == receipt(next_state))
    if token is None:
        raise ValueError('collector token required')
    for agent, edit, register, final, intent in publications:
        if register is not None:
            register()
        if edit is not None:
            def save_intent():
                publish(journal, intent, None)
                # The control directory itself may have been created on this first apply.
                materializer.sync_directory(journal.parent.parent)

            try:
                edit(before_publish=save_intent if intent is not None else None)
            except materializer.PublicationRefusal as error:
                if not error.recovery:
                    raise
                raise AgentConfigRefusals([(agent, error)]) from None
        if final != original:
            publish(state, final, original)
            original = final
        if intent is not None:
            materializer.clear_publication_intent(journal)
    return True
