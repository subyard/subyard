#!/usr/bin/env python3
"""Observe verified installed stack identity without exporting private configuration."""
import argparse
import hashlib
import io
import json
import os
import re
from pathlib import Path
import subprocess
import sys
import tarfile

import native

FILES = ('guest.sh', 'native.py', 'runtime.py', 'tomli_w.py', 'compose.yaml', 'collector.yaml')
SERVICES = {'clickhouse', 'openlit', 'collector'}
MARKER = 'subyard-openlit-v1'


def run(arguments):
    result = subprocess.run(arguments, capture_output=True, timeout=2, check=False)
    if result.returncode or len(result.stdout) > 4 * 1024 * 1024:
        raise ValueError('native runtime probe failed')
    return result.stdout


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def metadata(path):
    native.safe(path)
    if not path.exists():
        return None
    info = path.stat()
    return [info.st_dev, info.st_ino, info.st_uid, info.st_gid, info.st_mode & 0o777,
            info.st_size, info.st_mtime_ns, info.st_ctime_ns]


def owned_file(path, mode):
    info = metadata(path)
    return info is not None and info[2:4] == [os.getuid(), os.getgid()] and info[4] == mode


def recipe(source, selected, agents, user):
    for name in (*FILES, 'subyard-openlit.service'):
        if (source/name).is_symlink():
            raise ValueError('unsafe shipped runtime source')
    files = {name: hashlib.sha256((source/name).read_bytes()).hexdigest()
             for name in (*FILES, 'subyard-openlit.service')}
    return digest({'schema': 1, 'files': files, 'selected': selected,
                   'agents': sorted(agents) if selected else [], 'user': user})


def compose(state, source, *arguments):
    return ['docker', 'compose', '--project-directory', str(state), '--env-file', str(state/'database.env'),
            '-f', str(source/'compose.yaml'), *arguments]


def capture(state, source, unit, home, selected, agents, user):
    """Return readiness and a nonsecret CAS projection; raw inspect/config stays local."""
    desired = recipe(source, selected, agents, user)
    if not state.exists():
        return {'state': 'absent', 'actual': '', 'desired': ''}, {}
    if state.is_symlink() or not state.is_dir() or state.stat().st_uid != os.getuid() or state.stat().st_mode & 0o777 != 0o700:
        raise ValueError('unsafe runtime directory')
    if not owned_file(state/'marker', 0o600) or (state/'marker').read_text().strip() != MARKER:
        raise ValueError('foreign runtime directory')
    facts = {'selected': selected, 'files': {}, 'native': {}, 'containers': []}
    ready = True
    for name in FILES:
        path = state/name
        facts['files'][name] = metadata(path)
        mode = 0o644 if name == 'collector.yaml' else 0o600
        ready = ready and owned_file(path, mode) and path.read_bytes() == (source/name).read_bytes()
    facts['unit'] = metadata(unit)
    ready = ready and owned_file(unit, 0o644) and unit.read_bytes() == (source/'subyard-openlit.service').read_bytes()
    for name, path in [('claude', home/'.claude/settings.json'), ('codex', home/'.codex/config.toml')]:
        facts['native'][name] = metadata(path)
    facts['ownership'] = metadata(state/'native.json')
    # Bind private environment replacement through filesystem identity only,
    # without fingerprinting its password or the rendered Compose environment.
    facts['database_environment'] = metadata(state/'database.env')
    try:
        # Parsing and comparing native values is local; no values enter the CAS projection.
        native.reconcile(home, state/'native.json', agents if selected else [], 'check', None, None, None)
    except (ValueError, OSError, KeyError, TypeError):
        ready = False
    try:
        manager = run(['systemctl', 'show', 'subyard-openlit.service', '--property=ActiveState,UnitFileState']).decode()
        statuses = dict(line.split('=', 1) for line in manager.splitlines() if '=' in line)
        if statuses.get('ActiveState') not in ('active', 'inactive', 'failed', 'activating', 'deactivating', 'reloading') or statuses.get('UnitFileState') not in ('enabled', 'disabled', 'static', 'masked', 'not-found', 'indirect', 'enabled-runtime'):
            raise ValueError('unexpected service state')
        facts['service'] = statuses
        ready = ready and (statuses.get('ActiveState') == 'active' and statuses.get('UnitFileState') == 'enabled'
                           if selected else statuses.get('ActiveState') != 'active' and statuses.get('UnitFileState') != 'enabled')
        ids = run(['docker', 'ps', '-aq', '--filter', 'label=com.docker.compose.project=subyard-openlit']).decode().split()
        rows = json.loads(run(['docker', 'inspect', *ids])) if ids else []
        for row in rows:
            identity = {'id': row['Id'], 'image': row['Image'],
                'service': row['Config']['Labels'].get('com.docker.compose.service'),
                'state': row['State']['Status'], 'health': row['State'].get('Health', {}).get('Status', '')}
            if not valid_receipt({'schema': 1, 'desired': desired, 'containers': [identity]}):
                raise ValueError('unexpected container identity')
            facts['containers'].append(identity)
        facts['containers'].sort(key=lambda row: row['id'])
        if selected:
            if not owned_file(state/'database.env', 0o600):
                raise ValueError('unsafe private database environment')
            config = json.loads(run(compose(state, source, 'config', '--format', 'json')))
            definitions = config['services']
            if set(definitions) != SERVICES:
                raise ValueError('unexpected Compose service inventory')
            # Compose hashes can depend on the private database environment. Compare locally;
            # never put them, environment fields or raw inspect JSON in fingerprints/receipts.
            hashes = dict(line.split() for line in run(compose(state, source, 'config', '--hash', '*')).decode().splitlines())
            images = json.loads(run(['docker', 'image', 'inspect', *[definitions[name]['image'] for name in sorted(SERVICES)]]))
            expected_images = {name: image['Id'] for name, image in zip(sorted(SERVICES), images)}
            ready = ready and len(rows) == 3 and {r['Config']['Labels'].get('com.docker.compose.service') for r in rows} == SERVICES
            for row in rows:
                name = row['Config']['Labels'].get('com.docker.compose.service')
                matches = name in SERVICES and row['Image'] == expected_images.get(name) and row['Config']['Labels'].get('com.docker.compose.config-hash') == hashes.get(name)
                facts.setdefault('configuration_matches', {})[row['Id']] = matches
                ready = ready and matches and row['State']['Status'] == 'running'
                if name != 'collector':
                    ready = ready and row['State'].get('Health', {}).get('Status') == 'healthy'
                if name == 'collector':
                    # Docker reads the mounted inode inside the container, detecting an old bind
                    # after atomic host publication even when the host path has desired bytes.
                    archive = run(['docker', 'cp', row['Id']+':/etc/otelcol-contrib/config.yaml', '-'])
                    with tarfile.open(fileobj=io.BytesIO(archive), mode='r:*') as tar:
                        members = tar.getmembers()
                        if len(members) != 1 or not members[0].isfile() or members[0].size > 1024 * 1024:
                            raise ValueError('unexpected collector asset')
                        mounted_matches = tar.extractfile(members[0]).read() == (source/'collector.yaml').read_bytes()
                    facts['collector_mount_matches'] = mounted_matches
                    ready = ready and mounted_matches
            for endpoint in ('http://127.0.0.1:13133/', 'http://127.0.0.1:3000/'):
                run(['curl', '-fsS', '--max-time', '1', endpoint])
        else:
            ready = ready and not rows
    except (ValueError, OSError, KeyError, TypeError, subprocess.TimeoutExpired):
        ready = False
        facts['probe_failed'] = True
    # This result is independent of the durable receipt, allowing verified publication.
    return {'state': 'current' if ready else 'stale', 'actual': desired if ready else digest(facts), 'desired': desired}, facts


def valid_receipt(value):
    if not isinstance(value, dict) or set(value) != {'schema', 'desired', 'containers'} or value['schema'] != 1:
        return False
    if not isinstance(value['desired'], str) or not re.fullmatch('[a-f0-9]{64}', value['desired']) or not isinstance(value['containers'], list):
        return False
    for row in value['containers']:
        if not isinstance(row, dict) or set(row) != {'id', 'image', 'service', 'state', 'health'}:
            return False
        if not isinstance(row['id'], str) or not isinstance(row['image'], str) or not re.fullmatch('[a-f0-9]{64}', row['id']) or not re.fullmatch('sha256:[a-f0-9]{64}', row['image']):
            return False
        if row['service'] not in SERVICES or row['state'] not in ('running', 'created', 'paused', 'restarting', 'removing', 'exited', 'dead') or row['health'] not in ('', 'healthy', 'unhealthy', 'starting'):
            return False
    return True


def observe(state, source, unit, home, selected, agents, user):
    observation, facts = capture(state, source, unit, home, selected, agents, user)
    if observation['state'] == 'absent':
        return observation
    receipt_path = state/'receipt.json'
    facts['receipt_metadata'] = metadata(receipt_path)
    receipt = json.loads(receipt_path.read_text()) if owned_file(receipt_path, 0o600) else None
    expected = {'schema': 1, 'desired': observation['desired'], 'containers': facts['containers']}
    if observation['state'] == 'current' and receipt == expected and valid_receipt(receipt):
        return observation
    facts['receipt'] = receipt if valid_receipt(receipt) else 'invalid'
    return {'state': 'stale', 'actual': digest(facts), 'desired': observation['desired']}


def publish(state, source, unit, home, selected, agents, user):
    observation, facts = capture(state, source, unit, home, selected, agents, user)
    if observation['state'] != 'current':
        raise ValueError('runtime is not ready for receipt publication')
    native.write(state/'receipt.json', json.dumps({'schema': 1, 'desired': observation['desired'], 'containers': facts['containers']})+'\n')
    return observation


def guard(values, actual, desired):
    observation = observe(*values)
    if observation['actual'] != actual or observation['desired'] != desired or observation['state'] != 'stale':
        raise ValueError('runtime assessment changed')
    return observation


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('mode', choices=('observe', 'publish', 'guard'))
    parser.add_argument('--source', type=Path, required=True)
    parser.add_argument('--state', type=Path, required=True)
    parser.add_argument('--unit', type=Path, required=True)
    parser.add_argument('--home', type=Path, required=True)
    parser.add_argument('--user', required=True)
    parser.add_argument('--selected', choices=('0', '1'), required=True)
    parser.add_argument('--agents', default='')
    parser.add_argument('--actual')
    parser.add_argument('--desired')
    args = parser.parse_args()
    values = (args.state, args.source, args.unit, args.home, args.selected == '1', args.agents.split(), args.user)
    try:
        if args.mode == 'publish':
            observation = publish(*values)
        elif args.mode == 'guard':
            observation = guard(values, args.actual, args.desired)
        else:
            observation = observe(*values)
        print(json.dumps(observation))
    except (ValueError, OSError, KeyError, TypeError, subprocess.TimeoutExpired):
        print('openlit: runtime conflict or failed native verification; retry a fresh assessment', file=sys.stderr)
        sys.exit(1)


if __name__ == '__main__':
    main()
