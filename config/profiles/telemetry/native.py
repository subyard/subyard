#!/usr/bin/env python3
"""System-managed exporter policy and one-way cleanup of the v0.30 home writer."""
from contextlib import contextmanager, nullcontext
import json
import math
import os
from pathlib import Path
import re
import shlex
import stat
import tempfile

CONFIG_LIMIT = 4*1024*1024
POLICY_MODE = 0o640
# Exporters read the collector bearer through this helper; no policy names it.
HELPER = '/usr/local/lib/subyard-telemetry/otlp-headers'
HELPER_REFRESH_MS = 300000
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


def snapshot(path, max_bytes=CONFIG_LIMIT, *, dir_fd=None, legacy=False):
    if dir_fd is None:
        safe(path)
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=dir_fd)
    except FileNotFoundError:
        return None
    with os.fdopen(fd, 'rb') as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_size > max_bytes:
            if legacy:
                raise LegacyConfigError('type' if not stat.S_ISREG(info.st_mode) else
                                        'links' if info.st_nlink != 1 else 'size')
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


def desired(agent, endpoint, helper=HELPER):
    """The managed document. Exporters get Authorization only from the helper."""
    if agent == 'codex':
        # Agent responses carry the only child-thread link; the collector keeps their IDs, not the text.
        result = {'log_user_prompt': False, 'log_agent_responses': True}
        result |= {'headers_helper': helper, 'headers_helper_refresh_interval_ms': HELPER_REFRESH_MS}
        for key, path in [('exporter', 'logs'), ('metrics_exporter', 'metrics'), ('trace_exporter', 'traces')]:
            result[key] = {'otlp-http': {'endpoint': endpoint+'/v1/'+path, 'protocol': 'binary', 'tls': {}}}
        return {'otel': result}
    result = {'CLAUDE_CODE_ENABLE_TELEMETRY': '1', 'CLAUDE_CODE_ENHANCED_TELEMETRY_BETA': '1',
              'OTEL_LOG_USER_PROMPTS': '0', 'OTEL_LOG_TOOL_DETAILS': '0', 'OTEL_LOG_TOOL_CONTENT': '0'}
    for signal in ('LOGS', 'METRICS', 'TRACES'):
        result['OTEL_'+signal+'_EXPORTER'] = 'otlp'
        result['OTEL_EXPORTER_OTLP_'+signal+'_ENDPOINT'] = endpoint+'/v1/'+signal.lower()
        result['OTEL_EXPORTER_OTLP_'+signal+'_PROTOCOL'] = 'http/protobuf'
    return {'env': result, 'otelHeadersHelper': helper}


def policy(agent, document, writer):
    if agent == 'claude':
        return (json.dumps({'$comment': 'Managed by Subyard Telemetry', **document}, indent=2)+'\n').encode()
    return MARKERS[agent]+writer.dumps(document).encode()


def parse(agent, data):
    import tomllib

    document = json.loads(data) if agent == 'claude' else tomllib.loads(data.decode())
    if not isinstance(document, dict):
        raise ValueError('invalid agent configuration object')
    return document


def template_conflicts(template, fields, path):
    if not isinstance(template, dict) or not isinstance(fields, dict):
        return [] if semantic_equal(template, fields) else [path]
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
    os.setgroups([uid])
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
        raise ValueError('symlinked legacy parent')
    return Path(safefs.resolve_file(path))


class LegacyConfigError(ValueError):
    def __init__(self, code, *detail):
        self.reason = (code, *detail)
        super().__init__(code)


def legacy_config_failure(agent, error):
    # Only bounded identifiers and numeric metadata cross the guest boundary.
    # Parser and OS exception text can contain credentials or document contents.
    if isinstance(error, LegacyConfigError):
        return ('legacy-config', agent, *error.reason)
    if isinstance(error, OSError):
        return ('legacy-config', agent, 'access')
    from records import safefs
    if isinstance(error, safefs.Unsafe) and error.reason == safefs.SYMLINK:
        return ('legacy-config', agent, 'symlink')
    code = {'symlinked legacy parent': 'symlink', 'unsafe native path': 'symlink',
            'native configuration is not regular': 'type',
            'native file has multiple hard links': 'links',
            'native file exceeds size bound': 'size'}.get(str(error), 'parse')
    return ('legacy-config', agent, code)


def legacy_edits(receipt_path, home, token, writer, uid, gid, failures):
    before, entries = legacy_receipt(receipt_path)
    if before is None:
        return None, [], [token] if token else [], set()
    headers = list(recorded_headers(entries))
    private = token
    if headers:
        # After a rollback to v0.30 and another upgrade, the receipt names the
        # token that release minted, not the private one the collector keeps.
        token = recorded_token(entries)
        if not token:
            failures.append(('legacy-token-unverified',))
    # The recorded token is scrubbed; neither it nor the private one may remain.
    tokens = [value for value in dict.fromkeys((token, private)) if value]
    covered = {agent for agent in AGENTS if entries.get(agent)}
    edits = []
    with developer_files(uid, gid):
        for agent, (_, relative, section) in AGENTS.items():
            path = home/relative
            if agent not in covered:
                if any(contains_token(path, value) for value in tokens):
                    failures.append(('legacy-token-remains', agent))
                continue
            try:
                target = legacy_path(path)
                try:
                    info = target.lstat()
                except FileNotFoundError:
                    continue
                if info.st_uid != uid or info.st_gid not in (uid, gid):
                    raise LegacyConfigError('owner', info.st_uid, info.st_gid, uid, gid, str(home))
                raw = snapshot(target, legacy=True)
                if raw is None or not raw.strip():
                    continue
                try:
                    document = parse(agent, raw)
                except (ValueError, TypeError, RecursionError):
                    raise LegacyConfigError('parse') from None
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
                unsafe_restore = any(not semantic_equal(value, scrub_token(value, secret))
                                     for value in restored_values for secret in tokens)
                if unsafe_restore or any(value.encode() in payload for value in tokens):
                    failures.append(('legacy-token-remains', agent))
                elif payload != raw:
                    info = target.lstat()
                    if info.st_uid != uid or info.st_gid not in (uid, gid):
                        raise LegacyConfigError('owner', info.st_uid, info.st_gid, uid, gid, str(home))
                    # Any rewritten home document can contain user credentials.
                    edits.append((path, target, payload, raw, info.st_mode & 0o600, info.st_gid))
            except (OSError, ValueError, TypeError, AttributeError, KeyError, RecursionError) as error:
                failures.append(legacy_config_failure(agent, error))
    return before, edits, tokens, covered


class Refusal(ValueError):
    def __init__(self, reasons, *, message=None):
        self.reasons = reasons
        rendered = '; '.join(refusal_messages(reasons))
        super().__init__(rendered if message is None else message)


CHECK_FAILURES = {'safefs': 'shared safefs module check failed', 'native': 'native telemetry check failed'}
CHECK_REFUSAL_MESSAGES = frozenset((
    'managed asset changed', 'foreign control directory', 'unsafe native path',
    'native configuration is not regular', 'unsafe native file', 'native file exceeds size bound',
    'private collector token required', 'invalid collector token', *CHECK_FAILURES.values(),
))


def combined_check_refusal(support_error, native_error):
    def check_reason(check, error):
        if isinstance(error, OSError) and type(error.errno) is int and 0 < error.errno < 4096:
            return ('check-errno', check, error.errno)
        message = str(error)
        return ('check-refusal', message if message in CHECK_REFUSAL_MESSAGES else CHECK_FAILURES[check])

    reasons, messages = [], []
    for check, error in (('safefs', support_error), ('native', native_error)):
        details = error.reasons if isinstance(error, Refusal) else [check_reason(check, error)]
        reasons.extend(details)
        messages.extend(refusal_messages(details) if isinstance(error, OSError) else [str(error)])
    return Refusal(reasons, message='; '.join(messages))


def field_paths(fields, prefix=''):
    for key, value in fields.items():
        path = prefix+'/'+key
        yield path
        if isinstance(value, dict):
            yield from field_paths(value, path)


LEGACY_CONFIG_REASONS = {
    'symlink': 'unsafe symbolic link', 'type': 'not a regular file',
    'links': 'multiple hard links; expected one link',
    'size': f'file exceeds size bound ({CONFIG_LIMIT} bytes)',
    'parse': 'document cannot be parsed safely', 'access': 'file cannot be accessed',
}


def valid_developer_home(home):
    return (isinstance(home, str) and len(home) <= 4096 and home.startswith('/') and home != '/'
            and str(Path(home)) == home and '..' not in Path(home).parts
            and all(ord(character) >= 32 and ord(character) != 127 for character in home))


def refusal_messages(reasons):
    """Render closed reasons and validated home paths, never document or exception text."""
    if not isinstance(reasons, (list, tuple)) or not 1 <= len(reasons) <= 11:
        raise ValueError('invalid telemetry refusal')
    messages = []
    for reason in reasons:
        match reason:
            case ('check-refusal', str(message)) if message in CHECK_REFUSAL_MESSAGES:
                messages.append(message)
            case ('check-errno', str(check), code) if check in CHECK_FAILURES and type(code) is int and 0 < code < 4096:
                messages.append(f'{CHECK_FAILURES[check]} ([Errno {code}] {os.strerror(code)})')
            case ('legacy-receipt',):
                messages.append('invalid legacy telemetry receipt')
            case ('legacy-token-unverified',):
                messages.append('the legacy telemetry receipt names no single collector token; inside the yard, '
                                'remove the collector Bearer tokens from the developer\'s ~/.claude/settings.json '
                                'and ~/.codex/config.toml, then remove the receipt '
                                '/var/lib/subyard/telemetry-control/native.json and retry')
            case ('legacy-token-remains', agent) if agent in AGENTS:
                messages.append('collector token remains in legacy '+agent+' configuration')
            case ('legacy-config', agent, 'owner', actual_uid, actual_gid, uid, gid, home) if agent in AGENTS and valid_developer_home(home) and all(
                    type(value) is int and 0 <= value < 2**32 for value in (actual_uid, actual_gid, uid, gid)):
                path = str(Path(home)/AGENTS[agent][1])
                messages.append(f'cannot clean legacy {agent} configuration {path}: actual owner '
                                f'{actual_uid}:{actual_gid}, expected {uid}:{uid} (materializer) or {uid}:{gid} '
                                f'(developer primary group); as root inside the yard, run chown {uid}:{uid} -- {shlex.quote(path)} and retry')
            case ('legacy-config', agent, code) if agent in AGENTS and code in LEGACY_CONFIG_REASONS:
                messages.append('cannot clean legacy '+agent+' configuration ~/'+AGENTS[agent][1]+': '+LEGACY_CONFIG_REASONS[code])
            case ('unsafe-policy', agent) if agent in AGENTS:
                messages.append('unsafe or foreign telemetry policy /etc/'+AGENTS[agent][0])
            case ('unreadable-policy', agent) if agent in AGENTS:
                messages.append('developer cannot read telemetry policy /etc/'+AGENTS[agent][0])
            case ('rollback-enabled',):
                messages.append('telemetry is enabled, and releases up to v0.30 cannot open the telemetry store '
                                'of this release; disable telemetry before the rollback')
            case ('rollback-unconverged',):
                messages.append('telemetry is not converged with this release; run yard migrate before the rollback')
            case ('invalid-template', agent) if agent in AGENTS:
                messages.append('invalid '+agent+' telemetry template')
            case ('template-conflict', agent, *paths) if agent in AGENTS and paths:
                known = set(field_paths(desired(agent, '')))
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


def reconcile(etc, home, receipt_path, agents, endpoint, token, writer, uid, gid, templates, check=False, helper=HELPER):
    failures = []
    policies = []
    for agent, (relative, _, section) in AGENTS.items():
        path = etc/relative
        enabled = agent in agents
        document = desired(agent, endpoint, helper) if enabled else {}
        fields = document.get(section, {})
        if enabled and agent in templates:
            try:
                template = parse(agent, templates[agent])
                conflicts = template_conflicts(template, document, '')
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
            # Write policies only once the token file the helper reads exists.
            wanted = policy(agent, document, writer) if enabled and token else None
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
            for path, target, payload, before, mode, owner_gid in edits:
                if legacy_path(path) != target:
                    raise ValueError('native configuration changed')
                publish(target, payload, before, uid=uid, gid=owner_gid, mode=mode)
            for agent, (_, relative, _) in AGENTS.items():
                try:
                    if any(value.encode() in (snapshot(legacy_path(home/relative), legacy=True) or b'') if agent in covered
                           else contains_token(home/relative, value) for value in legacy):
                        failures.append(('legacy-token-remains', agent))
                except (OSError, ValueError) as error:
                    failures.append(legacy_config_failure(agent, error))
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
