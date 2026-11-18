#!/usr/bin/env python3
"""System-managed exporter policy and one-way cleanup of the v0.30 home writer."""
from contextlib import contextmanager
import json
import math
import os
import re
import stat
import tempfile
import tomllib

CONFIG_LIMIT = 4*1024*1024
POLICY_MODE = 0o640
AGENTS = {
    'claude': ('claude-code/managed-settings.d/60-subyard-telemetry.json', '.claude/settings.json', 'env'),
    'codex': ('codex/managed_config.toml', '.codex/config.toml', 'otel'),
}
MARKERS = {
    'claude': b'{\n  "$comment": "Managed by Subyard Telemetry",\n',
    'codex': b'# Managed by Subyard Telemetry\n',
}
MISSING = object()


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
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    except FileNotFoundError:
        return None
    with os.fdopen(fd, 'rb') as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_size > max_bytes:
            raise ValueError('unsafe native file')
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
            output.write(content)
            output.flush()
            if uid is not None:
                os.fchown(output.fileno(), uid, gid)
            os.fchmod(output.fileno(), mode)
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
                                       'headers': {'Authorization': 'Bearer '+token if token else MISSING}}}
        return result
    result = {'CLAUDE_CODE_ENABLE_TELEMETRY': '1', 'CLAUDE_CODE_ENHANCED_TELEMETRY_BETA': '1',
              'OTEL_LOG_USER_PROMPTS': '0', 'OTEL_LOG_TOOL_DETAILS': '0', 'OTEL_LOG_TOOL_CONTENT': '0'}
    for signal in ('LOGS', 'METRICS', 'TRACES'):
        result['OTEL_'+signal+'_EXPORTER'] = 'otlp'
        result['OTEL_EXPORTER_OTLP_'+signal+'_ENDPOINT'] = endpoint+'/v1/'+signal.lower()
        result['OTEL_EXPORTER_OTLP_'+signal+'_PROTOCOL'] = 'http/protobuf'
        result['OTEL_EXPORTER_OTLP_'+signal+'_HEADERS'] = 'Authorization=Bearer '+token if token else MISSING
    return result


def policy(agent, fields, writer):
    if agent == 'claude':
        document = {'$comment': 'Managed by Subyard Telemetry', 'env': fields}
        return (json.dumps(document, indent=2)+'\n').encode()
    return MARKERS[agent]+writer.dumps({'otel': fields}).encode()


def parse(agent, data):
    document = json.loads(data) if agent == 'claude' else tomllib.loads(data.decode())
    if not isinstance(document, dict):
        raise ValueError('invalid agent configuration object')
    return document


def template_conflicts(template, fields, path):
    if not isinstance(template, dict) or not isinstance(fields, dict):
        return [] if fields is not MISSING and semantic_equal(template, fields) else [path]
    conflicts = []
    for key, value in fields.items():
        if key in template:
            conflicts.extend(template_conflicts(template[key], value, path+'/'+key))
    return conflicts


def policy_snapshot(etc, path, agent):
    for directory in reversed([p for p in path.parents if p == etc or etc in p.parents]):
        try:
            info = directory.lstat()
        except FileNotFoundError:
            return None, None
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o022:
            raise ValueError('unsafe policy directory')
    content = snapshot(path)
    if content is None:
        return None, None
    info = path.lstat()
    if info.st_uid != os.geteuid() or not content.startswith(MARKERS[agent]):
        raise ValueError('foreign policy file')
    return content, info


def create_policy_directory(etc, path):
    # All ancestors have been checked for root ownership and writable bits.
    for directory in reversed([p for p in path.parents if etc in p.parents]):
        if not directory.exists():
            directory.mkdir(mode=0o755)
            directory.chmod(0o755)


@contextmanager
def developer_files(uid, gid):
    # The home and its parents can change during migration. Access them with
    # the developer's privileges, even when the deployment runs as guest root.
    if os.geteuid() != 0 or uid == 0:
        yield
        return
    groups, old_gid = os.getgroups(), os.getegid()
    os.setgroups([])
    os.setegid(gid)
    os.seteuid(uid)
    try:
        yield
    finally:
        os.seteuid(0)
        os.setegid(old_gid)
        os.setgroups(groups)


def restore(current, managed, prior):
    if any(semantic_equal(current, value) for value in managed):
        return prior
    if not isinstance(current, dict) or not current:
        return current
    tables = [value for value in managed if isinstance(value, dict)]
    if not tables:
        return current
    keys = set().union(*(value.keys() for value in tables))
    previous = prior if isinstance(prior, dict) else {}
    for key in current.keys() & keys:
        values = [value[key] for value in tables if key in value]
        restored = restore(current[key], values, previous.get(key, MISSING))
        if restored is MISSING:
            del current[key]
        else:
            current[key] = restored
    for key in previous.keys()-keys:
        current.setdefault(key, previous[key])
    return MISSING if not current and prior is MISSING else current


def scrub_tokens(value, tokens):
    if isinstance(value, dict):
        return {key: cleaned for key, item in value.items()
                if not any(token in key for token in tokens)
                and (cleaned := scrub_tokens(item, tokens)) is not MISSING}
    if isinstance(value, list):
        return [cleaned for item in value if (cleaned := scrub_tokens(item, tokens)) is not MISSING]
    if isinstance(value, str) and any(token in value for token in tokens):
        return MISSING
    return value


def legacy_edits(receipt_path, home, token, writer, uid, gid, failures):
    before = snapshot(receipt_path)
    if before is None:
        return None, []
    info = receipt_path.lstat()
    if info.st_uid != os.geteuid() or stat.S_IMODE(info.st_mode) != 0o600:
        raise ValueError('unsafe legacy telemetry receipt')
    receipt = json.loads(before)
    entries = receipt.get('agents') if isinstance(receipt, dict) and receipt.get('schema') == 1 else None
    if not isinstance(entries, dict) or entries.keys()-AGENTS.keys():
        raise ValueError('invalid legacy telemetry receipt')
    tokens = set(re.findall(rb'Bearer ([a-f0-9]{64})(?![a-f0-9])', before))
    tokens = {value.decode() for value in tokens}
    if token:
        tokens.add(token)
    edits = []
    with developer_files(uid, gid):
        for agent, (_, relative, section) in AGENTS.items():
            path = home/relative
            try:
                raw = snapshot(path)
                if raw is None or not raw.strip():
                    continue
                document = parse(agent, raw)
                fields = document.get(section)
                if isinstance(fields, dict) and fields:
                    for key, entry in entries.get(agent, {}).items():
                        if key not in fields:
                            continue
                        prior = entry['value'] if entry['present'] else MISSING
                        managed = (entry['managed'], *entry.get('previous', ()))
                        value = restore(fields[key], managed, prior)
                        if value is MISSING:
                            del fields[key]
                        else:
                            fields[key] = value
                    if not fields:
                        document.pop(section)
                cleaned = scrub_tokens(document, tokens)
                if not semantic_equal(parse(agent, raw), cleaned):
                    payload = (json.dumps(cleaned, indent=2)+'\n') if agent == 'claude' else writer.dumps(cleaned)
                    info = path.lstat()
                    if info.st_uid != uid or info.st_gid != gid:
                        raise ValueError('foreign home configuration')
                    edits.append((path, payload.encode(), raw, stat.S_IMODE(info.st_mode)))
            except (OSError, ValueError, TypeError, AttributeError, KeyError, RecursionError):
                failures.append('cannot clean legacy '+agent+' configuration')
    return before, edits


class Refusal(ValueError):
    pass


def reconcile(etc, home, receipt_path, agents, endpoint, token, writer, uid, gid, templates, check=False):
    failures = []
    policies = []
    for agent, (relative, _, section) in AGENTS.items():
        path = etc/relative
        enabled = agent in agents
        fields = desired(agent, endpoint, token) if enabled else {}
        if enabled and agent in templates:
            try:
                template = parse(agent, templates[agent])
                # A first install has no collector token to match a template header.
                conflicts = template_conflicts(template, {section: fields}, '')
                # An alternative Codex exporter variant makes the merged TOML invalid.
                if agent == 'codex' and isinstance(template.get(section), dict):
                    for key, value in fields.items():
                        supplied = template[section].get(key)
                        if isinstance(value, dict) and isinstance(supplied, dict) and supplied.keys()-value.keys():
                            conflicts.append('/otel/'+key)
                if conflicts:
                    paths = list(dict.fromkeys(conflicts))
                    detail = ', '.join(paths[:1])
                    if len(paths) > 1:
                        detail += f' and {len(paths)-1} more field(s)'
                    failures.append('telemetry conflicts with '+agent+' template at '+detail)
            except (ValueError, RecursionError):
                failures.append('invalid '+agent+' telemetry template')
        try:
            before, info = policy_snapshot(etc, path, agent)
            wanted = policy(agent, fields, writer) if enabled and token else None
            metadata_current = info is None or (stat.S_IMODE(info.st_mode), info.st_gid) == (POLICY_MODE, gid)
            if before != wanted or (enabled and not token) or not metadata_current:
                policies.append((agent, path, before, wanted))
        except (OSError, ValueError):
            failures.append('unsafe or foreign telemetry policy '+str(path))
    try:
        receipt, edits = legacy_edits(receipt_path, home, token, writer, uid, gid, failures)
    except (OSError, ValueError):
        failures.append('invalid legacy telemetry receipt')
    if failures:
        raise Refusal('; '.join(failures))
    if check:
        return receipt is None and not policies
    if agents and not token:
        raise ValueError('collector token required before writing policy')
    if receipt is not None:
        with developer_files(uid, gid):
            for path, payload, before, mode in edits:
                publish(path, payload, before, mode=mode)
        if snapshot(receipt_path) != receipt:
            raise ValueError('legacy telemetry receipt changed')
        receipt_path.unlink()
    for agent, path, before, wanted in policies:
        if policy_snapshot(etc, path, agent)[0] != before:
            raise ValueError('telemetry policy changed')
        if wanted is None:
            path.unlink()
        else:
            create_policy_directory(etc, path)
            publish(path, wanted, before, os.geteuid(), gid, mode=POLICY_MODE)
    return True
