#!/usr/bin/env python3
"""System-managed exporter policy and one-way cleanup of the v0.30 home writer."""
from contextlib import contextmanager, nullcontext
import json
import math
import os
from pathlib import Path
import re
import stat
import tempfile

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


def snapshot(path, max_bytes=CONFIG_LIMIT, *, dir_fd=None):
    if dir_fd is None:
        safe(path)
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=dir_fd)
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


def publish(path, content, expected, uid=None, gid=None, max_bytes=CONFIG_LIMIT, mode=0o600, policy_gid=None,
            *, no_replace=False):
    from records import safefs

    safe(path)
    if len(content) > max_bytes:
        raise ValueError('native file exceeds size bound')

    def check(pending):
        pending.file.seek(0)
        if pending.file.read() != content:
            raise ValueError('native payload verification failed')
        if snapshot(pending.target, max_bytes=max_bytes, dir_fd=pending.parent_fd) != expected:
            raise ValueError('native configuration changed')

    owner = (uid, gid) if uid is not None else None
    staging = nullcontext()
    if policy_gid is not None:
        # Policy staging includes a bearer token. Contain it in a private
        # directory while setting its final group and mode before publication.
        owner = (-1 if uid is None else uid, policy_gid)
        staging = tempfile.TemporaryDirectory(prefix='.telemetry-', dir=path.parent)
    with staging as directory:
        safefs.write_file(path, content, owner=owner, mode=mode, prefix='.telemetry-',
                          parent_mode=0o700, stage_dir=directory, check=check, no_replace=no_replace)


def desired(agent, endpoint, token):
    if agent == 'codex':
        # Agent responses carry the only child-thread link; the collector keeps their IDs, not the text.
        result = {'log_user_prompt': False, 'log_agent_responses': True}
        for key, path in [('exporter', 'logs'), ('metrics_exporter', 'metrics'), ('trace_exporter', 'traces')]:
            result[key] = {'otlp-http': {'endpoint': endpoint+'/v1/'+path, 'protocol': 'binary',
                                       'headers': {'Authorization': 'Bearer '+token if token else MISSING},
                                       'tls': {}}}
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
    import tomllib

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


def restore(current, managed, prior, restored_values):
    if any(semantic_equal(current, value) for value in managed):
        restored_values.append(prior)
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
        restored = restore(current[key], values, previous.get(key, MISSING), restored_values)
        if restored is MISSING:
            del current[key]
        else:
            current[key] = restored
    for key in previous.keys()-keys:
        if key not in current:
            current[key] = previous[key]
            restored_values.append({key: previous[key]})
    return MISSING if not current and prior is MISSING else current


def scrub_token(value, token):
    if isinstance(value, dict):
        return {key: cleaned for key, item in value.items()
                if token not in key and (cleaned := scrub_token(item, token)) is not MISSING}
    if isinstance(value, list):
        return [cleaned for item in value if (cleaned := scrub_token(item, token)) is not MISSING]
    if isinstance(value, str) and token in value:
        return MISSING
    return value


def recorded_headers(entries):
    claude, codex = entries.get('claude', {}), entries.get('codex', {})
    for signal in ('LOGS', 'METRICS', 'TRACES'):
        key = 'OTEL_EXPORTER_OTLP_'+signal+'_HEADERS'
        if key in claude:
            yield claude[key]['managed'], 'Authorization=Bearer '
    for key in ('exporter', 'metrics_exporter', 'trace_exporter'):
        if key in codex:
            yield codex[key]['managed']['otlp-http']['headers']['Authorization'], 'Bearer '


def legacy_receipt(receipt_path):
    before = snapshot(receipt_path)
    if before is None:
        return None, None
    info = receipt_path.lstat()
    if info.st_uid != os.geteuid() or stat.S_IMODE(info.st_mode) != 0o600:
        raise ValueError('unsafe legacy telemetry receipt')
    receipt = json.loads(before)
    entries = receipt.get('agents') if isinstance(receipt, dict) and receipt.get('schema') == 1 else None
    if not isinstance(entries, dict) or entries.keys()-AGENTS.keys():
        raise ValueError('invalid legacy telemetry receipt')
    return before, entries


def recorded_token(entries):
    """The one collector token that every header recorded by v0.30 names, if any."""
    tokens = set()
    for value, prefix in recorded_headers(entries):
        if not isinstance(value, str) or not value.startswith(prefix):
            return None
        tokens.add(value.removeprefix(prefix))
    token = tokens.pop() if len(tokens) == 1 else None
    return token if token and re.fullmatch('[0-9a-f]{64}', token) else None


def legacy_token(receipt_path):
    """Recover a missing collector token from the root-owned v0.30 receipt."""
    try:
        before, entries = legacy_receipt(receipt_path)
        return recorded_token(entries) if before is not None else None
    except (OSError, ValueError, TypeError, AttributeError, KeyError, RecursionError):
        return None


def contains_token(path, token):
    """Scan raw bytes of a home file the receipt never covered.

    v0.30 wrote only regular files it recorded, so an unreadable or irregular
    file cannot hold its token and is left alone."""
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NONBLOCK)
    except OSError:
        return False
    with os.fdopen(fd, 'rb') as stream:
        try:
            if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
                return False
            return token.encode() in stream.read(CONFIG_LIMIT+len(token))
        except OSError:
            return False


def legacy_path(path):
    from records import safefs

    if any(parent.is_symlink() for parent in path.parents):
        raise ValueError('unsafe native path')
    return Path(safefs.resolve_file(path))


def legacy_edits(receipt_path, home, token, writer, uid, gid, failures):
    before, entries = legacy_receipt(receipt_path)
    if before is None:
        return None, [], token, set()
    headers = list(recorded_headers(entries))
    if headers and not token:
        token = recorded_token(entries)
    if headers and (not token or any(value != prefix+token for value, prefix in headers)):
        failures.append(('legacy-token-unverified',))
    token_bytes = token.encode() if token else None
    covered = {agent for agent in AGENTS if entries.get(agent)}
    edits = []
    with developer_files(uid, gid):
        for agent, (_, relative, section) in AGENTS.items():
            path = home/relative
            if agent not in covered:
                if token and contains_token(path, token):
                    failures.append(('legacy-token-remains', agent))
                continue
            try:
                target = legacy_path(path)
                raw = snapshot(target)
                if raw is None or not raw.strip():
                    continue
                document = parse(agent, raw)
                restored_values = []
                fields = document.get(section)
                if isinstance(fields, dict) and fields:
                    for key, entry in entries.get(agent, {}).items():
                        if key not in fields:
                            continue
                        prior = entry['value'] if entry['present'] else MISSING
                        managed = (entry['managed'], *entry.get('previous', ()))
                        value = restore(fields[key], managed, prior, restored_values)
                        if value is MISSING:
                            del fields[key]
                        else:
                            fields[key] = value
                    if not fields:
                        document.pop(section)
                cleaned = scrub_token(document, token) if token and headers else document
                payload = raw
                if not semantic_equal(parse(agent, raw), cleaned):
                    payload = ((json.dumps(cleaned, indent=2)+'\n') if agent == 'claude' else writer.dumps(cleaned)).encode()
                # A saved value stays user-owned even if it contains the live
                # token. Refuse rather than deleting or publishing that value.
                unsafe_restore = token and any(not semantic_equal(value, scrub_token(value, token))
                                               for value in restored_values)
                if unsafe_restore or (token_bytes and token_bytes in payload):
                    failures.append(('legacy-token-remains', agent))
                elif payload != raw:
                    info = target.lstat()
                    if info.st_uid != uid or info.st_gid != gid:
                        raise ValueError('foreign home configuration')
                    # Any rewritten home document can contain user credentials.
                    edits.append((path, target, payload, raw, info.st_mode & 0o600))
            except (OSError, ValueError, TypeError, AttributeError, KeyError, RecursionError):
                failures.append(('legacy-config', agent))
    return before, edits, token, covered


class Refusal(ValueError):
    def __init__(self, reasons, *, message=None):
        self.reasons = reasons
        rendered = '; '.join(refusal_messages(reasons))
        super().__init__(rendered if message is None else message)


CHECK_REFUSAL_MESSAGES = frozenset((
    'managed asset changed', 'foreign control directory', 'unsafe native path',
    'native configuration is not regular', 'unsafe native file', 'native file exceeds size bound',
    'private collector token required', 'invalid collector token',
    'shared safefs module check failed', 'native telemetry check failed',
))


def combined_check_refusal(support_error, native_error):
    def check_reason(error, fallback):
        message = str(error)
        return ('check-refusal', message if message in CHECK_REFUSAL_MESSAGES else fallback)

    reasons = [check_reason(support_error, 'shared safefs module check failed')]
    if isinstance(native_error, Refusal):
        reasons.extend(native_error.reasons)
    else:
        reasons.append(check_reason(native_error, 'native telemetry check failed'))
    return Refusal(reasons, message='; '.join(map(str, (support_error, native_error))))


def field_paths(fields, prefix=''):
    for key, value in fields.items():
        path = prefix+'/'+key
        yield path
        if isinstance(value, dict):
            yield from field_paths(value, path)


def refusal_messages(reasons):
    """Render known identifiers against local policy, without using guest text."""
    if not isinstance(reasons, (list, tuple)) or not 1 <= len(reasons) <= 11:
        raise ValueError('invalid telemetry refusal')
    messages = []
    for reason in reasons:
        match reason:
            case ('check-refusal', str(message)) if message in CHECK_REFUSAL_MESSAGES:
                messages.append(message)
            case ('legacy-receipt',):
                messages.append('invalid legacy telemetry receipt')
            case ('legacy-token-unverified',):
                messages.append('cannot identify the collector token in the legacy telemetry receipt; '
                                'write the Bearer token from the legacy home agent configuration to '
                                '/var/lib/subyard/telemetry-private/auth.token, a developer-owned file with '
                                'mode 0600 in a developer-owned directory with mode 0700, then retry')
            case ('legacy-token-remains', agent) if agent in AGENTS:
                messages.append('collector token remains in legacy '+agent+' configuration')
            case ('legacy-config', agent) if agent in AGENTS:
                messages.append('cannot clean legacy '+agent+' configuration')
            case ('unsafe-policy', agent) if agent in AGENTS:
                messages.append('unsafe or foreign telemetry policy /etc/'+AGENTS[agent][0])
            case ('unreadable-policy', agent) if agent in AGENTS:
                messages.append('developer cannot read telemetry policy /etc/'+AGENTS[agent][0])
            case ('invalid-template', agent) if agent in AGENTS:
                messages.append('invalid '+agent+' telemetry template')
            case ('template-conflict', agent, *paths) if agent in AGENTS and paths:
                known = set(field_paths({AGENTS[agent][2]: desired(agent, '', '')}))
                if len(paths) > len(known) or any(path not in known for path in paths):
                    raise ValueError('invalid telemetry refusal field')
                detail = paths[0]
                if len(paths) > 1:
                    detail += f' and {len(paths)-1} more field(s)'
                messages.append('telemetry conflicts with '+agent+' template at '+detail)
            case _:
                raise ValueError('invalid telemetry refusal')
    return messages


def policy_readable(path, uid, gid, file_current):
    with developer_files(uid, gid):
        # A missing directory can be created with mode 0755, but every
        # existing ancestor must be searchable by the intended reader.
        return (all(not directory.exists() or os.access(directory, os.X_OK, effective_ids=True)
                    for directory in reversed(path.parents))
                and (not file_current or os.access(path, os.R_OK, effective_ids=True)))


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
                        http = supplied.get('otlp-http') if isinstance(supplied, dict) else None
                        if isinstance(http, dict) and http.get('tls'):
                            conflicts.append('/otel/'+key+'/otlp-http/tls')
                if conflicts:
                    failures.append(('template-conflict', agent, *dict.fromkeys(conflicts)))
            except (ValueError, RecursionError):
                failures.append(('invalid-template', agent))
        try:
            before, info = policy_snapshot(etc, path, agent)
            wanted = policy(agent, fields, writer) if enabled and token else None
            metadata_current = info is None or (stat.S_IMODE(info.st_mode), info.st_gid) == (POLICY_MODE, gid)
            if enabled and not policy_readable(path, uid, gid, info is not None and metadata_current):
                failures.append(('unreadable-policy', agent))
            if before != wanted or (enabled and not token) or not metadata_current:
                policies.append((agent, path, before, wanted))
        except (OSError, ValueError):
            if enabled:
                failures.append(('unsafe-policy', agent))
    try:
        receipt, edits, legacy, covered = legacy_edits(receipt_path, home, token, writer, uid, gid, failures)
    except (OSError, ValueError, TypeError, AttributeError, KeyError, RecursionError):
        failures.append(('legacy-receipt',))
    if failures:
        raise Refusal(failures)
    if check:
        return receipt is None and not policies
    if agents and not token:
        raise ValueError('collector token required before writing policy')
    if receipt is not None:
        with developer_files(uid, gid):
            for path, target, payload, before, mode in edits:
                if legacy_path(path) != target:
                    raise ValueError('native configuration changed')
                publish(target, payload, before, mode=mode)
            if legacy:
                for agent, (_, relative, _) in AGENTS.items():
                    try:
                        if (legacy.encode() in (snapshot(legacy_path(home/relative)) or b'') if agent in covered
                                else contains_token(home/relative, legacy)):
                            failures.append(('legacy-token-remains', agent))
                    except (OSError, ValueError):
                        failures.append(('legacy-config', agent))
        if failures:
            raise Refusal(failures)
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
            publish(path, wanted, before, mode=POLICY_MODE, policy_gid=gid)
    return True
