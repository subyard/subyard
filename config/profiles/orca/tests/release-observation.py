"""Exact absent Orca registration transport for release verification."""
import hashlib
import json
from pathlib import Path
import re

FIXTURES = Path(__file__).parent / 'fixtures/release-observation'


def read_only_commands(package):
    return []


def read_only_stdin_commands(package):
    resource = package / 'resources/orca'
    contract = resource / 'registration-contract.sh'
    observer = resource / 'registration-observer.bash'
    historical = None
    if contract.is_file() and observer.is_file():
        current_handler = Path(__file__).parents[1] / 'resources/orca/handler.sh'
        if (resource / 'handler.sh').read_bytes() != current_handler.read_bytes():
            return []
    if not contract.is_file() or not observer.is_file():
        handler = resource / 'handler.sh'
        if not handler.is_file():
            return []
        identities = json.loads((FIXTURES / 'identities.json').read_text())
        for version, identity in identities.items():
            if hashlib.sha256(handler.read_bytes()).hexdigest() != identity['handler']:
                continue
            actual = resource / 'registration'
            expected = identity['registration']
            if sorted(p.name for p in actual.glob('*.py')) != sorted(expected) or any(
                    hashlib.sha256((actual / name).read_bytes()).hexdigest() != digest
                    for name, digest in expected.items()):
                continue
            historical = version
            contract = FIXTURES / f"contract-{identity['contract']}.txt"
            observer = FIXTURES / 'observer.txt'
            break
        if historical is None:
            return []
    script = '''set -euo pipefail
RESOURCE_DIR=$1
ORCA_CONTRACT_VERSION=5
ORCA_UNIT=subyard-orca.service
ORCA_REGISTRATION=/usr/local/libexec/subyard/orca-registration
ORCA_REGISTRATION_LOCK=/usr/local/libexec/subyard/orca-registration.lock
ORCA_REGISTRATION_DIGEST=/usr/local/libexec/subyard/orca-registration.sha256
ORCA_DISCOVERY_UNIT=subyard-orca-discovery.service
ORCA_DISCOVERY_TIMER=subyard-orca-discovery.timer
ORCA_CODEX_PROFILE=/etc/profile.d/subyard-orca-codex.sh
ORCA_CODEX_BIN=/usr/local/libexec/subyard/orca-codex/codex
. "$2"
registration_desired_digest
'''
    if not historical:
        version = re.search(r'^ORCA_CONTRACT_VERSION=(\d+)$', current_handler.read_text(), re.MULTILINE).group(1)
        script = script.replace('ORCA_CONTRACT_VERSION=5', f'ORCA_CONTRACT_VERSION={version}')
    names = sorted(p.name for p in (resource / 'registration').glob('*.py'))
    if historical:
        inputs = [Path('handler.sh'), *(Path('registration') / name for name in names)]
        checks = ['set -- "$RESOURCE_DIR"/registration/*.py', f'[ "$#" -eq {len(names)} ]']
        for name in inputs:
            digest = hashlib.sha256((resource / name).read_bytes()).hexdigest()
            checks.append(f'sum="$(sha256sum \"$RESOURCE_DIR/{name}\")"; [ "${{sum%% *}}" = {digest} ]')
        script = script.replace('registration_desired_digest', '\n'.join(checks) + '\nregistration_desired_digest')
    command = ['exec', 'yard', '--project', 'subyard', '--', 'bash', '-se', '--', None,
               '/usr/local/libexec/subyard/projects-changed.d/orca',
               '/usr/local/libexec/subyard/orca-registration',
               '/usr/local/libexec/subyard/orca-registration.sha256',
               '/usr/local/libexec/subyard/orca-registration.lock', '/usr/bin/orca-ide',
               '/etc/systemd/system/subyard-orca.service',
               '/etc/systemd/system/subyard-orca-discovery.service',
               '/etc/systemd/system/subyard-orca-discovery.timer',
               *([] if historical else ['/etc/profile.d/subyard-orca-codex.sh',
                                       '/usr/local/libexec/subyard/orca-codex/codex']), *names]
    return [(command, observer.read_bytes(), '{"state":"absent","actual":"","desired":""}\n', 0,
             ['bash', '-c', script, 'orca-observation', str(resource), str(contract)])]
