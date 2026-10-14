#!/usr/bin/env python3
"""Project bounded native pull and registry facts without private command output."""
import json
import base64
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import time

SERVICES = ('clickhouse', 'openlit', 'collector')
STATUSES = ('Pulling fs layer', 'Pull complete', 'Download complete', 'Verifying Checksum',
            'Already exists', 'Downloading', 'Extracting', 'Waiting', 'Pulling', 'Pulled', 'Error')


def pull_projection(log, images):
    """Project fixed status words and aggregate numbers; never copy log content."""
    size = log.stat().st_size
    with log.open('rb') as source:
        source.seek(max(0, size-65536))
        text = source.read(65536).decode(errors='replace')
    lower = text.lower()
    category = next((category for category, terms in (
        ('authorization', ('unauthorized', 'denied')),
        ('rate-limit', ('too many requests', 'rate limit', '429')),
        ('timeout', ('timed out', 'timeout', 'deadline exceeded')),
        ('dns', ('no such host', 'lookup')),
        ('tls', ('tls handshake', 'x509:', 'certificate')),
        ('network', ('connection refused', 'network is unreachable', 'cannot connect',
                     'connection reset', 'unexpected eof'))
    ) if any(term in lower for term in terms)), 'other')
    services = {}
    layers = {}
    status_pattern = '('+'|'.join(re.escape(value) for value in STATUSES)+')'
    for line in text.splitlines():
        # Compose versions identify services either by name or exact image.
        # Only those known names may escape; repository/digest text stays local.
        service = re.match(r'^\s*(clickhouse|openlit|collector)\s+'+status_pattern+r'(?:\s|$)', line)
        if service:
            name, status = service.groups()
        else:
            name = next((name for name, image in zip(SERVICES, images)
                         if re.match(r'^\s*(?:Image\s+)?'+re.escape(image)+r'\s+'+status_pattern+r'(?:\s|$)', line)), None)
            if name is None:
                status = None
            else:
                status = re.match(r'^\s*(?:Image\s+)?'+re.escape(images[SERVICES.index(name)])+r'\s+'+status_pattern+r'(?:\s|$)', line)[1]
        if name is not None:
            value = services.setdefault(name, {'status_counts': {}, 'last_status': status})
            value['status_counts'][status] = value['status_counts'].get(status, 0)+1
            value['last_status'] = status
        layer = re.match(r'^\s*([a-f0-9]{6,64})\s+'+status_pattern+r'(?:\s|$)', line)
        if layer:
            identity, status = layer.groups()
            value = layers.setdefault(identity, {})
            value['status'] = status
            # Parse only Compose's explicit downloaded/total byte pair. Unknown
            # progress syntax is omitted rather than inferred to be completed.
            pair = re.search(r'(?<![\d.])(\d{1,15}(?:\.\d{1,6})?)\s*(B|kB|KB|KiB|MB|MiB|GB|GiB)\s*/\s*(\d{1,15}(?:\.\d{1,6})?)\s*(B|kB|KB|KiB|MB|MiB|GB|GiB)(?:\s|$)', line[layer.end():])
            if status == 'Downloading' and pair:
                factors = {'B': 1, 'kB': 1000, 'KB': 1000, 'KiB': 1024,
                           'MB': 1000**2, 'MiB': 1024**2, 'GB': 1000**3, 'GiB': 1024**3}
                downloaded = int(float(pair[1])*factors[pair[2]])
                total = int(float(pair[3])*factors[pair[4]])
                if 0 <= downloaded <= total <= 1000**5:
                    value.update(downloaded_bytes=downloaded, total_bytes=total)
    counts = {}
    for value in layers.values():
        counts[value['status']] = counts.get(value['status'], 0)+1
    measured = [row for row in layers.values() if 'downloaded_bytes' in row]
    return {'pull_log_bytes': size, 'pull_category': category,
            'pull_progress': sorted(set(re.findall(r'\b(clickhouse|openlit|collector)\s+(Pulling|Pulled|Error)\b', text))),
            'service_progress': services,
            'layer_progress': {'observed_count': len(layers), 'status_counts': counts,
                               'byte_pairs_observed_count': len(measured),
                               'downloaded_bytes': sum(row['downloaded_bytes'] for row in measured) if measured else None,
                               'total_bytes': sum(row['total_bytes'] for row in measured) if measured else None},
            'progress_state': 'observed' if services or layers else 'unknown'}


def dependency(compose, images):
    """Own capture/projection/cleanup of one unchanged native bounded pull."""
    if os.getuid() != 0 or len(compose) > 65536 or re.findall(rb'^    image: ([^\n]+)$', compose, re.M) != [image.encode() for image in images]:
        raise ValueError('dependency pull requires root and exact pinned public Compose')
    directory = Path(tempfile.mkdtemp(prefix='subyard-openlit-images-', dir='/tmp'))
    os.chmod(directory, 0o700)
    native_exit = None
    try:
        for name, data in (('compose.yaml', compose), ('pull.log', b'')):
            fd = os.open(directory/name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
            os.fchmod(fd, 0o600)
            with os.fdopen(fd, 'wb') as stream:
                stream.write(data)
        env = os.environ.copy()
        env['OPENLIT_DB_PASSWORD'] = 'synthetic-preflight'
        started = time.monotonic()
        with (directory/'pull.log').open('wb') as output:
            result = subprocess.run(['timeout', '--signal=TERM', '--kill-after=30', '240',
                'docker', 'compose', '--ansi', 'never', '--progress', 'plain', '-f', str(directory/'compose.yaml'), 'pull'],
                stdout=output, stderr=subprocess.STDOUT, env=env)
        native_exit = result.returncode
        try:
            facts = pull_projection(directory/'pull.log', images)
        except (ValueError, OSError):
            facts = {'capture_error': True, 'progress_state': 'unknown', 'pull_category': 'other'}
        facts.update(exit_code=result.returncode, elapsed_seconds=round(time.monotonic()-started, 3))
        if result.returncode == 124:
            facts['pull_category'] = 'timeout'
        print('openlit_e2e_dependency_pull='+json.dumps(facts, sort_keys=True), flush=True)
        return result.returncode or (2 if facts.get('capture_error') else 0)
    finally:
        try:
            shutil.rmtree(directory)
        except OSError:
            print('openlit_e2e_dependency_cleanup={"failed":true}', flush=True)
            # Keep a known native failure code, including124; cleanup must not
            # obscure it. A cleanup failure after successful pull still fails.
            if native_exit in (None, 0):
                raise


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
    for service, image in zip(SERVICES, images):
        try:
            result = subprocess.run(['docker', 'image', 'inspect', image],
                capture_output=True, timeout=5)
            state = 'present' if result.returncode == 0 else 'unknown'
            # Docker's inspector also exits 1 on API/transport failures. Admit
            # only its exact empty-result/missing-image response for this pin.
            if (result.returncode == 1 and result.stdout.strip() == b'[]'
                    and result.stderr.strip() == ('Error response from daemon: No such image: '+image).encode()):
                state = 'absent'
            facts[service] = {'inspect_exit': result.returncode, 'state': state}
        except (OSError, subprocess.TimeoutExpired) as error:
            facts[service] = {'category': type(error).__name__, 'state': 'unknown'}
    return facts


def journals(capture=None, phase=None):
    facts = {}
    for name, selector in (('docker', ['--unit', 'docker.service']), ('kernel', ['--kernel'])):
        try:
            result = subprocess.run(['journalctl', *selector, '--since=-10min',
                '--no-pager', '--lines=200', '--output=cat'], capture_output=True, timeout=5)
            if capture is not None:
                fd = os.open(capture/(phase+'-'+name+'.log'), os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
                with os.fdopen(fd, 'wb') as stream:
                    os.fchmod(stream.fileno(), 0o600)
                    stream.write(result.stdout+result.stderr)
            text = result.stdout.decode(errors='replace').lower()
            facts[name] = {'exit_code': result.returncode, 'oom_present': any(
                term in text for term in ('out of memory', 'oom-kill', 'killed process')),
                'timeout_present': any(term in text for term in ('timed out', 'timeout', 'deadline exceeded'))}
        except (OSError, subprocess.TimeoutExpired) as error:
            facts[name] = {'category': type(error).__name__}
    return facts


def pull_context(phase, images, directory=Path('/tmp/subyard-openlit-pull-diagnostic')):
    if os.getuid() != 0 or phase not in ('before', 'after-ordinary', 'after-fallback'):
        raise ValueError('invalid pull context')
    if any(path.is_symlink() for path in (directory, *directory.parents)):
        raise ValueError('unsafe pull context directory')
    if phase == 'before':
        directory.mkdir(mode=0o700)
        os.chmod(directory, 0o700)
        fd = os.open(directory/'marker', os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, 'w') as marker:
            os.fchmod(marker.fileno(), 0o600)
            marker.write('openlit-pull-diagnostic-v1\n')
    marker = directory/'marker'
    if (directory.stat().st_uid != 0 or directory.stat().st_mode & 0o777 != 0o700
            or marker.is_symlink() or not marker.is_file() or marker.stat().st_uid != 0
            or marker.stat().st_nlink != 1 or marker.stat().st_mode & 0o777 != 0o600
            or marker.read_text() != 'openlit-pull-diagnostic-v1\n'):
        raise ValueError('unowned pull context directory')
    cwd = Path.cwd()
    value = {'phase': phase, 'observation_timing': 'before-or-after-command',
             'uid': os.getuid(), 'cwd_owner_uid': cwd.stat().st_uid,
             'cwd_category': 'root-home' if cwd == Path('/root') else 'root' if cwd == Path('/') else 'other',
             'cli_environment_present': {name: name in os.environ for name in (
                 'HOME', 'PATH', 'TMPDIR', 'DOCKER_HOST', 'DOCKER_CONTEXT', 'DOCKER_CONFIG',
                 'HTTP_PROXY', 'HTTPS_PROXY', 'ALL_PROXY', 'NO_PROXY')}, 'daemon': {}}
    try:
        info = subprocess.run(['timeout', '--kill-after=1', '10', 'docker', 'info',
                               '--format', '{{.ServerVersion}}'], capture_output=True, timeout=12)
        value['docker_info_exit'] = info.returncode
        result = subprocess.run(['systemctl', 'show', 'docker.service',
            '--property=MainPID,InvocationID,ExecMainStartTimestampMonotonic'], capture_output=True, timeout=3)
        if result.returncode == 0:
            properties = dict(line.split('=', 1) for line in result.stdout.decode().splitlines() if '=' in line)
            for name in ('MainPID', 'ExecMainStartTimestampMonotonic'):
                if re.fullmatch(r'[0-9]{1,20}', properties.get(name, '')):
                    value['daemon'][name] = int(properties[name])
            if re.fullmatch('[0-9a-f]{32}', properties.get('InvocationID', '')):
                value['daemon']['InvocationID'] = properties['InvocationID']
    except (OSError, subprocess.TimeoutExpired):
        value['docker_info_exit'] = None
    value['image_cache'] = cache(images)
    value['journal_categories'] = journals(directory, phase)
    value['cold_cache_observed'] = value['docker_info_exit'] == 0 and all(
        row.get('state') == 'absent' for row in value['image_cache'].values())
    if phase != 'before':
        value['product'] = failure(Path('/var/lib/subyard/openlit'), images)
    print('openlit_e2e_pull_context='+json.dumps(value, sort_keys=True), flush=True)
    return 0 if phase != 'before' or value['cold_cache_observed'] else 2


def failure(state, images):
    owned = (state.is_dir() and not state.is_symlink() and state.stat().st_uid == 0
             and state.stat().st_mode & 0o777 == 0o700)
    names = ('marker', 'database.env', 'pull.log', 'compose.yaml', 'receipt.json')
    value = {'runtime_directory_owned': owned, 'files_present': {
        name: owned and (state/name).is_file() and not (state/name).is_symlink() for name in names}}
    log = state/'pull.log'
    if value['files_present']['pull.log']:
        value.update(pull_projection(log, images))
    value['registry_transport'] = registries()
    value['image_cache'] = cache(images)
    value['journal_categories'] = journals()
    return value


if __name__ == '__main__':
    mode, *arguments = sys.argv[1:]
    images = arguments[:3]
    if len(images) != 3 or any(not re.fullmatch(r'[a-z0-9./:-]+@sha256:[a-f0-9]{64}', image) for image in images):
        raise SystemExit(2)
    if mode == 'dependency' and len(arguments) == 4:
        try:
            compose = base64.b64decode(arguments[3], validate=True)
            raise SystemExit(dependency(compose, images))
        except (ValueError, OSError):
            print('openlit_e2e_dependency_pull={"capture_error":true}', flush=True)
            raise SystemExit(2)
    elif mode == 'preflight' and len(arguments) == 3:
        print('openlit_e2e_registry_transport='+json.dumps({'registry_transport': registries(), 'image_cache': cache(images)}, sort_keys=True))
    elif mode == 'failure' and len(arguments) == 3:
        print('openlit_e2e_failure_facts='+json.dumps(failure(Path('/var/lib/subyard/openlit'), images), sort_keys=True))
    elif mode == 'context' and len(arguments) == 4:
        try:
            raise SystemExit(pull_context(arguments[3], images))
        except (ValueError, OSError):
            print('openlit_e2e_pull_context={"capture_error":true}', flush=True)
            raise SystemExit(2)
    else:
        raise SystemExit(2)
