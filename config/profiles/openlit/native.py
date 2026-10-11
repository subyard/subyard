#!/usr/bin/env python3
"""Own only native telemetry leaves; retain prior settings and unrelated fields."""
import argparse
import copy
import importlib.util
import json
import os
from pathlib import Path
import re
import sys
import tempfile
import tomllib

CLAUDE = {
    'CLAUDE_CODE_ENABLE_TELEMETRY': '1',
    'OTEL_METRICS_EXPORTER': 'otlp',
    'OTEL_LOGS_EXPORTER': 'otlp',
    'OTEL_EXPORTER_OTLP_LOGS_ENDPOINT': 'http://127.0.0.1:4318/v1/logs',
    'OTEL_EXPORTER_OTLP_METRICS_ENDPOINT': 'http://127.0.0.1:4318/v1/metrics',
    'OTEL_EXPORTER_OTLP_LOGS_PROTOCOL': 'http/protobuf',
    'OTEL_EXPORTER_OTLP_METRICS_PROTOCOL': 'http/protobuf',
    'OTEL_EXPORTER_OTLP_LOGS_HEADERS': '',
    'OTEL_EXPORTER_OTLP_METRICS_HEADERS': '',
    'OTEL_LOG_USER_PROMPTS': '0',
}
CODEX = {
    'exporter': {'otlp-http': {'endpoint': 'http://127.0.0.1:4318/v1/logs', 'protocol': 'binary'}},
    'metrics_exporter': {'otlp-http': {'endpoint': 'http://127.0.0.1:4318/v1/metrics', 'protocol': 'binary'}},
    'log_user_prompt': False,
}


def safe(path, anchor=None):
    if path.is_symlink() or (path.exists() and not path.is_file()):
        raise ValueError('unsafe native configuration or ownership path')
    for parent in path.parents:
        if parent.is_symlink() and parent != anchor:
            raise ValueError('unsafe native configuration parent')


def load(path, agent, anchor=None):
    safe(path, anchor)
    if not path.exists():
        return {}
    if path.stat().st_size > 4 * 1024 * 1024:
        raise ValueError('native configuration exceeds size limit')
    text = path.read_text()
    result = json.loads(text) if agent == 'claude' else tomllib.loads(text)
    if not isinstance(result, dict):
        raise ValueError('native configuration must be an object')
    return result


def write(path, content, uid=None, gid=None):
    safe(path)
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=path.parent, prefix='.openlit-')
    try:
        with os.fdopen(fd, 'w') as out:
            out.write(content)
            out.flush()
            os.fsync(out.fileno())
        os.chmod(tmp, 0o600)
        if uid is not None:
            os.chown(tmp, uid, gid)
        os.replace(tmp, path)
        directory = os.open(path.parent, os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)


def expected(agent):
    return CLAUDE if agent == 'claude' else CODEX


def section(agent):
    return 'env' if agent == 'claude' else 'otel'


def template_check(path, agent, source_root=None):
    anchor = None
    # The engine's pinned repository descriptor is a trusted kernel anchor. Its
    # self alias belongs to the parent engine, not this child; preserve that pin
    # and reject all symlinks below it instead of resolving to a movable release.
    if source_root is not None and re.fullmatch(r'/proc/[1-9][0-9]*/fd/(0|[1-9][0-9]*)', str(source_root)):
        alias = Path('/proc/self/fd')/source_root.name
        if path.is_relative_to(alias):
            path = source_root/path.relative_to(alias)
        if path.is_relative_to(source_root):
            anchor = source_root
    obj = load(path, agent, anchor)
    values = obj.get(section(agent), {})
    if not isinstance(values, dict) or any(key in values for key in expected(agent)):
        raise ValueError('agent template owns native telemetry fields; remove overlapping template fields before enabling OpenLIT')


def reconcile(home, state, agents, mode, uid, gid, writer):
    safe(state)
    if state.exists():
        if state.stat().st_mode & 0o077 or state.stat().st_uid != os.getuid():
            raise ValueError('native ownership state must be private')
        baseline = json.loads(state.read_text())
        if baseline.get('schema') != 1 or not isinstance(baseline.get('agents'), dict):
            raise ValueError('invalid native ownership state')
    else:
        baseline = {'schema': 1, 'agents': {}}
    edits = []
    retained = copy.deepcopy(baseline)
    for agent in ('claude', 'codex'):
        path = home / ('.claude/settings.json' if agent == 'claude' else '.codex/config.toml')
        owned = baseline['agents'].get(agent)
        selected = agent in agents
        if not selected and owned is None:
            continue
        obj = load(path, agent)
        before = copy.deepcopy(obj)
        group = obj.get(section(agent), {})
        if not isinstance(group, dict):
            raise ValueError('native telemetry section must be an object')
        if selected:
            owned = owned or {}
            final = {}
            pending = copy.deepcopy(owned)
            for key, value in expected(agent).items():
                old = owned.get(key)
                if old is None:
                    old = {'present': key in group, 'value': group.get(key), 'managed': value}
                current = {'present': key in group, 'value': group.get(key)}
                original = {'present': old['present'], 'value': old['value']}
                allowed = [old['managed'], *old.get('previous', [])]
                matches = key in group and any(group[key] == item for item in allowed)
                if not matches and (mode == 'check' or current != original):
                    raise ValueError('native telemetry settings changed; disable OpenLIT before reconfiguring')
                group[key] = value
                final[key] = {'present': old['present'], 'value': old['value'], 'managed': value}
                pending[key] = {**final[key], 'previous': allowed}
            # A newer or older recipe may retire previously managed leaves. Restore
            # only values that still belong to the prior recipe, preserving user edits.
            for key, old in owned.items():
                if key not in final and key in group and any(group[key] == item for item in [old['managed'], *old.get('previous', [])]):
                    if old['present']:
                        group[key] = old['value']
                    else:
                        del group[key]
            baseline['agents'][agent] = final
            retained['agents'][agent] = pending
        else:
            for key, old in owned.items():
                if key in group and any(group[key] == item for item in [old['managed'], *old.get('previous', [])]):
                    if old['present']:
                        group[key] = old['value']
                    else:
                        del group[key]
            del baseline['agents'][agent]
        if group:
            obj[section(agent)] = group
        else:
            obj.pop(section(agent), None)
        if mode == 'check':
            if before != obj or (selected and owned is None):
                raise ValueError('native telemetry is not converged')
        elif before != obj:
            text = json.dumps(obj, indent=2) + '\n' if agent == 'claude' else writer.dumps(obj)
            edits.append((path, text))
    if mode in ('check', 'validate'):
        if mode == 'check' and agents and not state.exists():
            raise ValueError('native telemetry ownership state is missing')
        return
    # Write ownership before native edits: interruptions can retry from prior or managed values.
    write(state, json.dumps(retained) + '\n')
    for path, content in edits:
        write(path, content, uid, gid)
        os.chown(path.parent, uid, gid)
    write(state, json.dumps(baseline) + '\n')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('mode', choices=['apply', 'check', 'validate', 'template-check'])
    parser.add_argument('--home', type=Path)
    parser.add_argument('--state', type=Path)
    parser.add_argument('--agents', default='')
    parser.add_argument('--uid', type=int)
    parser.add_argument('--gid', type=int)
    parser.add_argument('--template', type=Path)
    parser.add_argument('--agent', choices=['claude', 'codex'])
    parser.add_argument('--source-root', type=Path)
    args = parser.parse_args()
    try:
        if args.mode == 'template-check':
            template_check(args.template, args.agent, args.source_root)
            return
        spec = importlib.util.spec_from_file_location('tomli_w', Path(__file__).with_name('tomli_w.py'))
        writer = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(writer)
        reconcile(args.home, args.state, args.agents.split(), args.mode, args.uid, args.gid, writer)
    except (ValueError, OSError, KeyError, TypeError):
        # Never expose configuration values, headers, parser excerpts or credentials.
        print('openlit: native telemetry configuration conflict or invalid protected state', file=sys.stderr)
        sys.exit(1)


if __name__ == '__main__':
    main()
