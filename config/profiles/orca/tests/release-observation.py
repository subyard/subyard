"""Exact absent Orca registration transport for release verification."""
from pathlib import Path
import re

FIXED_PATH = re.compile(r'/[A-Za-z0-9._/-]+')


def parse_fixed_paths(text):
    """Parse the fixed-path list by the rule of resources/orca/fixed-paths.bash."""
    entries = []
    # The list is LF-only: a carriage return anywhere is invalid, never normalized.
    if '\r' in text:
        raise ValueError('invalid Orca fixed path list entry')
    for line in text.split('\n'):
        fields = line.split()
        if not fields or fields[0].startswith('#'):
            continue
        if (len(fields) != 2 or fields[0] not in ('unit', 'unit-helper', 'path')
                or not FIXED_PATH.fullmatch(fields[1]) or '..' in fields[1]):
            raise ValueError('invalid Orca fixed path list entry')
        entries.append((fields[0], fields[1]))
    return entries


def fixed_paths(resource):
    """The profile's one fixed-path list, as the observer receives it."""
    paths = [path for _, path in parse_fixed_paths((resource / 'fixed-paths').read_bytes().decode())]
    return [str(len(paths)), *paths]


def read_only_commands(package):
    return []


def read_only_stdin_commands(package):
    resource = package / 'resources/orca'
    contract = resource / 'registration-contract.sh'
    observer = resource / 'registration-observer.bash'
    current_resource = Path(__file__).parents[1] / 'resources/orca'
    current_handler = current_resource / 'handler.sh'
    if (resource / 'handler.sh').read_bytes() != current_handler.read_bytes():
        return []
    fixed = resource / 'fixed-paths'
    parser = resource / 'fixed-paths.bash'
    if any(path.is_symlink() or not path.is_file() or path.read_bytes() != (current_resource/path.name).read_bytes()
           for path in (contract, observer, fixed, parser)):
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
    version = re.search(r'^ORCA_CONTRACT_VERSION=(\d+)$', current_handler.read_text(), re.MULTILINE).group(1)
    script = script.replace('ORCA_CONTRACT_VERSION=5', f'ORCA_CONTRACT_VERSION={version}')
    # Orca installs the shared checkout discovery library beside its own modules.
    names = sorted([*(p.name for p in (resource / 'registration').glob('*.py')), 'discovery.py', 'inventory.py'])
    command = ['exec', 'yard', '--project', 'subyard', '--', 'bash', '-se', '--', None,
               '/usr/local/libexec/subyard/projects-changed.d/orca',
               '/usr/local/libexec/subyard/orca-registration',
               '/usr/local/libexec/subyard/orca-registration.sha256',
               '/usr/local/libexec/subyard/orca-registration.lock', '/usr/bin/orca-ide',
               '/etc/systemd/system/subyard-orca.service',
               '/etc/systemd/system/subyard-orca-discovery.service',
               '/etc/systemd/system/subyard-orca-discovery.timer',
               '/etc/profile.d/subyard-orca-codex.sh', '/usr/local/libexec/subyard/orca-codex/codex',
               *fixed_paths(resource), *names]
    return [(command, observer.read_bytes(), '{"state":"absent","actual":"","desired":""}\n', 0,
             ['bash', '-c', script, 'orca-observation', str(resource), str(contract)])]
