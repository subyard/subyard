#!/usr/bin/env python3
"""Project bounded native pull and registry facts without private command output."""
import json
from pathlib import Path
import re
import subprocess
import sys


def registries():
    facts = {}
    for name, url in (('dockerhub', 'https://registry-1.docker.io/v2/'),
                      ('dockerhub-auth', 'https://auth.docker.io/token?service=registry.docker.io&scope=repository:clickhouse/clickhouse-server:pull'),
                      ('ghcr', 'https://ghcr.io/v2/'),
                      ('ghcr-auth', 'https://ghcr.io/token?service=ghcr.io&scope=repository:openlit/openlit:pull')):
        try:
            result = subprocess.run(['curl', '-q', '--silent', '--show-error',
                '--connect-timeout', '5', '--max-time', '10', '--output', '/dev/null',
                '--write-out', '%{http_code}', url], capture_output=True, timeout=12)
            code = result.stdout.decode(errors='replace').strip()
            facts[name] = {'exit_code': result.returncode,
                          'http_status': int(code) if re.fullmatch(r'[0-9]{3}', code) else None}
        except (OSError, subprocess.TimeoutExpired) as error:
            facts[name] = {'category': type(error).__name__}
    return facts


def cache(images):
    facts = {}
    for service, image in zip(('clickhouse', 'openlit', 'collector'), images):
        try:
            result = subprocess.run(['docker', 'image', 'inspect', image],
                capture_output=True, timeout=5)
            facts[service] = {'inspect_exit': result.returncode}
        except (OSError, subprocess.TimeoutExpired) as error:
            facts[service] = {'category': type(error).__name__}
    return facts


def journals():
    facts = {}
    for name, selector in (('docker', ['--unit', 'docker.service']), ('kernel', ['--kernel'])):
        try:
            result = subprocess.run(['journalctl', *selector, '--since=-10min',
                '--no-pager', '--lines=200', '--output=cat'], capture_output=True, timeout=5)
            text = result.stdout.decode(errors='replace').lower()
            facts[name] = {'exit_code': result.returncode, 'oom_present': any(
                term in text for term in ('out of memory', 'oom-kill', 'killed process')),
                'timeout_present': any(term in text for term in ('timed out', 'timeout', 'deadline exceeded'))}
        except (OSError, subprocess.TimeoutExpired) as error:
            facts[name] = {'category': type(error).__name__}
    return facts


def failure(state, images):
    owned = (state.is_dir() and not state.is_symlink() and state.stat().st_uid == 0
             and state.stat().st_mode & 0o777 == 0o700)
    names = ('marker', 'database.env', 'pull.log', 'compose.yaml', 'receipt.json')
    value = {'runtime_directory_owned': owned, 'files_present': {
        name: owned and (state/name).is_file() and not (state/name).is_symlink() for name in names}}
    log = state/'pull.log'
    if value['files_present']['pull.log']:
        value['pull_log_bytes'] = log.stat().st_size
        with log.open('rb') as source:
            source.seek(max(0, log.stat().st_size-65536))
            text = source.read(65536).decode(errors='replace')
        lower = text.lower()
        value['pull_category'] = next((category for category, terms in (
            ('authorization', ('unauthorized', 'denied')),
            ('rate-limit', ('too many requests', 'rate limit', '429')),
            ('timeout', ('timed out', 'timeout', 'deadline exceeded')),
            ('dns', ('no such host', 'lookup')),
            ('tls', ('tls handshake', 'x509:', 'certificate')),
            ('network', ('connection refused', 'network is unreachable', 'cannot connect',
                         'connection reset', 'unexpected eof'))
        ) if any(term in lower for term in terms)), 'other')
        # Only stack service names and fixed progress words may escape pull.log.
        value['pull_progress'] = sorted(set(re.findall(
            r'\b(clickhouse|openlit|collector)\s+(Pulling|Pulled|Error)\b', text)))
    value['registry_transport'] = registries()
    value['image_cache'] = cache(images)
    value['journal_categories'] = journals()
    return value


if __name__ == '__main__':
    mode, *images = sys.argv[1:]
    if len(images) != 3 or any(not re.fullmatch(r'[a-z0-9./:-]+@sha256:[a-f0-9]{64}', image) for image in images):
        raise SystemExit(2)
    if mode == 'preflight':
        print('openlit_e2e_registry_transport='+json.dumps({'registry_transport': registries(), 'image_cache': cache(images)}, sort_keys=True))
    elif mode == 'failure':
        print('openlit_e2e_failure_facts='+json.dumps(failure(Path('/var/lib/subyard/openlit'), images), sort_keys=True))
    else:
        raise SystemExit(2)
