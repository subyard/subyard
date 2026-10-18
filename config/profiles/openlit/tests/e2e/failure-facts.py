#!/usr/bin/env python3
"""Project bounded native pull and registry facts without private command output."""
import json
import base64
from datetime import datetime, timedelta
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
    errors = pull_errors(text)
    category = next((row['category'] for row in errors if row['category'] != 'unknown'), 'other')
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
    return {'pull_log_bytes': size, 'pull_category': category, 'pull_errors': errors,
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
            result = subprocess.run(['timeout', '--signal=TERM', '--kill-after=30', '900',
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
def pull_errors(text):
    errors = []
    for line in text.splitlines():
        # Match only fixed Docker error phrases; variable URLs, credentials,
        # image names and response bodies never become diagnostic output.
        line = re.sub(r'https?://[^\s"\']+|Bearer\s+\S+', '', line, flags=re.I)
        service = re.match(r'\s*(clickhouse|openlit|collector)\s+Error\b', line)
        phase = re.search(r'\b(failed to fetch anonymous token|failed to resolve reference|failed to do request|error response from daemon)\b', line, re.I)
        category, message = 'unknown', None
        for kind, pattern in (
            ('rate-limit', r'429 Too Many Requests|toomanyrequests|pull rate limit'),
            ('authorization', r'401 Unauthorized|403 Forbidden|unauthorized: authentication required|pull access denied|denied: requested access to the resource is denied'),
            ('manifest', r'manifest unknown|no matching manifest'),
            ('deadline', r'context deadline exceeded'),
            ('timeout', r'TLS handshake timeout|i/o timeout|Client\.Timeout exceeded while awaiting headers|connection timed out'),
            ('dns', r'no such host|temporary failure in name resolution'),
            ('tls', r'x509: certificate signed by unknown authority|x509: certificate has expired|remote error: tls'),
            ('oom', r'out of memory|cannot allocate memory|OOMKilled'),
            ('network', r'connection refused|network is unreachable|connection reset by peer|unexpected EOF|cannot connect to the Docker daemon'),
        ):
            match = re.search(pattern, line, re.I)
            if match:
                category, message = kind, match.group(0)
                break
        if message is None and phase is None and service is None:
            continue
        detail = {'service': service.group(1) if service else 'unknown',
                  'operation': phase.group(0).lower() if phase else 'unknown',
                  'category': category, 'message': message or (phase.group(0) if phase else 'unknown')}
        if detail not in errors:
            errors.append(detail)
            errors = errors[-3:]
    return errors


def health_execution(rows):
    """Project the latest bounded native execution, never timestamps or output."""
    unknown = {'category': 'unknown'}
    if not isinstance(rows, list) or not 1 <= len(rows) <= 5:
        return unknown
    row = rows[-1]
    if (not isinstance(row, dict) or set(row) != {'start', 'end', 'exit_code'}
            or type(row['exit_code']) is not int or not -1 <= row['exit_code'] <= 255):
        return unknown
    times = []
    for field in ('start', 'end'):
        stamp = row[field]
        if not isinstance(stamp, str):
            return unknown
        match = re.fullmatch(
            r'[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}'
            r'(?:\.([0-9]{1,9}))?(?:Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])', stamp)
        if not match:
            return unknown
        try:
            parsed = datetime.fromisoformat(stamp.replace('Z', '+00:00'))
        except ValueError:
            return unknown
        if parsed.year < 1970:
            return unknown
        times.append((parsed.replace(microsecond=0), int((match[1] or '0').ljust(9, '0'))))
    nanoseconds = ((times[1][0]-times[0][0]) // timedelta(seconds=1))*1000000000 + times[1][1]-times[0][1]
    if not 0 <= nanoseconds <= 2147483647*1000000:
        return unknown
    return {'category': 'observed', 'exit_code': row['exit_code'], 'duration_ms': nanoseconds//1000000}


def openlit_health(images):
    """Read only selected state and fixed loopback statuses, never logs/config."""
    deadline = time.monotonic()+12
    def probe(argv):
        remaining = deadline-time.monotonic()
        if remaining <= 0:
            raise subprocess.TimeoutExpired('health probe', 0)
        result = subprocess.run(argv, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                timeout=min(5, remaining))
        if result.returncode or len(result.stdout) > 65536:
            raise ValueError('invalid projection')
        return result.stdout

    def category(error):
        return 'deadline' if isinstance(error, subprocess.TimeoutExpired) else (
            'unavailable' if isinstance(error, OSError) else 'invalid-projection')

    if len(images) != 3 or any(not re.fullmatch(r'[a-z0-9./:-]+@sha256:[a-f0-9]{64}', image) for image in images):
        return {'category': 'invalid-projection'}
    try:
        containers = probe(['docker', 'ps', '-aq', '--no-trunc',
                            '--filter', 'label=com.docker.compose.project=subyard-openlit',
                            '--filter', 'label=com.docker.compose.service=openlit']).decode('ascii').split()
        if not containers:
            return {'category': 'absent'}
        if len(containers) != 1 or not re.fullmatch('[a-f0-9]{64}', containers[0]):
            raise ValueError('invalid container identity')
        template = ('{"project":{{json (index .Config.Labels "com.docker.compose.project")}},'
                    '"service":{{json (index .Config.Labels "com.docker.compose.service")}},'
                    '"image":{{json .Config.Image}},"state":{{json .State.Status}},'
                    '"running":{{.State.Running}},"restarting":{{.State.Restarting}},'
                    '"oom_killed":{{.State.OOMKilled}},"exit_code":{{.State.ExitCode}},'
                    '"restart_count":{{.RestartCount}},'
                    '"health":{{if .State.Health}}{{json .State.Health.Status}}{{else}}"absent"{{end}},'
                    '"failing_streak":{{if .State.Health}}{{.State.Health.FailingStreak}}{{else}}0{{end}},'
                    '"health_checks":[{{if .State.Health}}{{range $index,$entry := .State.Health.Log}}'
                    '{{if $index}},{{end}}{"start":{{json $entry.Start}},"end":{{json $entry.End}},'
                    '"exit_code":{{$entry.ExitCode}}}{{end}}{{end}}]}')
        value = json.loads(probe(['docker', 'inspect', '--format', template, containers[0]]))
        keys = {'project', 'service', 'image', 'state', 'running', 'restarting', 'oom_killed',
                'exit_code', 'restart_count', 'health', 'failing_streak', 'health_checks'}
        if (not isinstance(value, dict) or set(value) != keys
                or (value['project'], value['service'], value['image']) != ('subyard-openlit', 'openlit', images[1])
                or value['state'] not in ('created', 'running', 'paused', 'restarting', 'removing', 'exited', 'dead')
                or value['health'] not in ('absent', 'starting', 'healthy', 'unhealthy')
                or any(type(value[key]) is not bool for key in ('running', 'restarting', 'oom_killed'))
                or any(type(value[key]) is not int or not 0 <= value[key] <= 2147483647
                       for key in ('exit_code', 'restart_count', 'failing_streak'))
                or value['exit_code'] > 255):
            raise ValueError('invalid health projection')
        for key in ('project', 'service', 'image'):
            del value[key]
        value['latest_healthcheck'] = health_execution(value.pop('health_checks'))
        value['category'] = 'observed'
    except (OSError, subprocess.TimeoutExpired, ValueError, UnicodeError, RecursionError) as error:
        return {'category': category(error)}
    if value['running'] and value['health'] != 'healthy':
        # Include the fixed login page beyond the UI's first redirect, without following
        # redirects, reading bodies, sending authentication or retrying.
        script = """const urls={ui:'http://localhost:3000',login:'http://localhost:3000/login',logs:'http://localhost:4318/v1/logs',metrics:'http://localhost:4318/v1/metrics',traces:'http://localhost:4318/v1/traces'};
Promise.all(Object.entries(urls).map(async ([name,url])=>{try{const r=await fetch(url,{signal:AbortSignal.timeout(1500),redirect:'manual'});await r.body?.cancel();return [name,r.status]}catch{return [name,0]}})).then(rows=>console.log(JSON.stringify(Object.fromEntries(rows))));"""
        try:
            endpoints = json.loads(probe(['docker', 'exec', containers[0], 'node', '-e', script]))
            if (not isinstance(endpoints, dict) or set(endpoints) != {'ui', 'login', 'logs', 'metrics', 'traces'}
                    or any(type(code) is not int or not (code == 0 or 100 <= code <= 599) for code in endpoints.values())):
                raise ValueError('invalid endpoint projection')
            value['endpoints'] = endpoints
        except (OSError, subprocess.TimeoutExpired, ValueError, UnicodeError, RecursionError) as error:
            value['endpoint_category'] = category(error)
    return value


def failure(state, images):
    owned = (state.is_dir() and not state.is_symlink() and state.stat().st_uid == 0
             and state.stat().st_mode & 0o777 == 0o700)
    names = ('marker', 'database.env', 'pull.log', 'compose.yaml', 'receipt.json')
    value = {'runtime_directory_owned': owned, 'files_present': {
        name: owned and (state/name).is_file() and not (state/name).is_symlink() for name in names}}
    log = state/'pull.log'
    if value['files_present']['pull.log']:
        value.update(pull_projection(log, images))
        # The owning native leaf still has no retained exit or timing record.
        value['pull_context'] = {'operation': 'docker compose pull --quiet',
                                 'exit_code': None, 'elapsed_seconds': None,
                                 'log_tail_truncated': value['pull_log_bytes'] > 65536}
        value['pull_category'] = next((row['category'] for row in value['pull_errors']
                                       if row['category'] != 'unknown'), 'unknown')
    value['registry_transport'] = registries()
    value['image_cache'] = cache(images)
    value['journal_categories'] = journals()
    if value['files_present']['marker']:
        try:
            with (state/'marker').open('rb') as marker:
                if marker.read(64) == b'subyard-openlit-v1\n':
                    value['openlit_health'] = openlit_health(images)
        except OSError:
            value['openlit_health'] = {'category': 'unavailable'}
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
