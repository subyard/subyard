#!/usr/bin/env python3
"""Bind repeatable activation to public runtime inputs and installed identities."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys


SOURCE = Path(__file__).resolve().parent


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def metadata(path):
    try:
        info = path.lstat()
        return [info.st_dev, info.st_ino, info.st_uid, info.st_gid, info.st_mode,
                info.st_size, info.st_mtime_ns, info.st_ctime_ns]
    except FileNotFoundError:
        return None


def observe():
    yard = os.environ.get('SUBYARD_YARD') or 'default'
    if not re.fullmatch('[a-zA-Z0-9_-]+', yard):
        raise ValueError('invalid GitHub runtime yard')
    descriptor = json.loads((SOURCE/'profile.json').read_text())
    selected = ('github' in os.environ['ENVIRONMENT_PROFILES'].split()
                if 'ENVIRONMENT_PROFILES' in os.environ else yard in descriptor.get('default_yards', []))
    selected = selected and not any(os.environ.get(key) == value
                                   for key, value in descriptor.get('disabled_when', {}).items())
    engine = SOURCE/'bin/broker'
    if not engine.exists():
        engine = SOURCE.parents[2]/'.build/profiles/github/bin/broker'
    inputs = {name: hashlib.sha256((SOURCE/name).read_bytes()).hexdigest()
              for name in ('profile.json', 'owner.sh', 'runtime.sh', 'runtime.py', 'provision.sh', 'SKILL.md', 'bin/subyard-github', 'bin/git-remote-subyard-github', 'git_wiring.py')}
    if engine.is_symlink():
        raise ValueError('unsafe native GitHub runtime source')
    inputs['engine'] = hashlib.sha256(engine.read_bytes()).hexdigest()
    # These are transport paths and settings; connection contents never enter a fingerprint.
    settings = {key: os.environ.get(key, '') for key in ('SUBYARD_YARD', 'INCUS_PROJECT',
        'YARD_INSTANCE_NAME', 'SUBYARD_HOME', 'SUBYARD_CONFIG_HOME', 'SUBYARD_OPERATOR_HOME',
        'SUBYARD_KEYS_CONSUMER_ROOT', 'SUBYARD_USER', 'DEV_USER', 'SSH_PORT', 'GITHUB_GIT_MODE')}
    desired = digest({'schema': 1, 'selected': selected, 'inputs': inputs, 'settings': settings})
    state = Path(os.environ['SUBYARD_HOME'])/'github-broker'
    unit = Path(os.environ['SUBYARD_OPERATOR_HOME'])/'.config/systemd/user'/f'subyard-github-{yard}.service'
    paths = [state, state/f'{yard}.json', state/f'{yard}-engine', unit.parent, unit]
    facts = {'paths': {str(path): metadata(path) for path in paths}, 'selected': selected}
    environment = dict(os.environ, SUBYARD_PROFILE_SELECTED='1' if selected else '0')
    try:
        result = subprocess.run([str(SOURCE/'owner.sh'), '--check'], env=environment,
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15)
        ready = result.returncode == 0
    except subprocess.TimeoutExpired:
        facts['check_timeout'] = True
        ready = False
    facts['ready'] = ready
    guest = ['incus', 'exec', os.environ['YARD_INSTANCE_NAME'], '--project', os.environ['INCUS_PROJECT'], '--']
    probe = "getent passwd \"$1\" >/dev/null || exit 2; for path in /usr/local/libexec/subyard/github-client /usr/local/bin/subyard-github /usr/local/bin/git-remote-subyard-github; do if [ -e \"$path\" ] || [ -L \"$path\" ]; then stat -c '%F|%a|%u|%g|%i|%s|%Y|%Z' \"$path\" 2>/dev/null && sha256sum \"$path\" 2>/dev/null || exit 2; fi; done; home=$(getent passwd \"$1\" | cut -d: -f6); if [ ! -L \"$home/.config/subyard/github-git.conf\" ] && [ \"$(head -n 1 \"$home/.config/subyard/github-git.conf\" 2>/dev/null)\" = '# subyard-managed-github-git-v1' ]; then for path in \"$home/.config/git/config\" \"$home/.config/subyard/github-git.conf\"; do if [ -e \"$path\" ] || [ -L \"$path\" ]; then stat -c '%F|%a|%u|%g|%i|%s|%Y|%Z' \"$path\" 2>/dev/null || exit 2; fi; done; fi"
    installed = subprocess.run(guest + ['sh', '-c', probe, 'subyard', os.environ.get('DEV_USER', 'dev')], capture_output=True, timeout=3)
    ready = ready and installed.returncode == 0
    facts['ready'] = ready
    facts['guest_available'] = installed.returncode == 0
    if installed.returncode == 0 and len(installed.stdout) < 4096:
        facts['guest'] = installed.stdout.decode().splitlines()
    loaded = subprocess.run(guest + ['timeout', '--kill-after=1', '2', 'runuser', '-u',
                            os.environ.get('DEV_USER', 'dev'), '--', '/usr/local/bin/subyard-github',
                            'status', '--runtime'], capture_output=True, timeout=4)
    if loaded.returncode == 0 and len(loaded.stdout) < 4096:
        try:
            identity = json.loads(loaded.stdout)
            if identity.get('protocol') == 1 and all(re.fullmatch('[a-f0-9]{64}', identity.get(key, ''))
                                                    for key in ('broker_sha256', 'client_sha256')):
                facts['loaded'] = {key: identity[key] for key in ('protocol', 'broker_sha256', 'client_sha256')}
        except (ValueError, TypeError, AttributeError):
            pass
    manager_environment = dict(environment, XDG_RUNTIME_DIR=f'/run/user/{os.getuid()}',
                               DBUS_SESSION_BUS_ADDRESS=f'unix:path=/run/user/{os.getuid()}/bus')
    manager = subprocess.run(['systemctl', '--user', 'show', unit.name,
                             '--property=ActiveState,UnitFileState,MainPID,LoadState'],
                            capture_output=True, env=manager_environment, timeout=3)
    if manager.returncode in (0, 4) and len(manager.stdout) < 4096:
        facts['manager'] = manager.stdout.decode().splitlines()
    # The ordinary disabled check proves owned skills, links and socket absent.
    # Also require both copied executables and every host path to be absent, and
    # no loaded service. Probe failure or a remaining guest client is not absence.
    manager_fields = dict(line.split('=', 1) for line in facts.get('manager', []) if '=' in line)
    if (not selected and ready and all(metadata(path) is None for path in paths[1:3]+[unit])
            and installed.returncode == 0 and installed.stdout == b''
            and manager_fields.get('LoadState') == 'not-found'
            and manager_fields.get('ActiveState') == 'inactive' and manager_fields.get('MainPID') == '0'):
        return {'state': 'current', 'actual': desired, 'desired': desired,
                'ownership_absent': True}, environment
    return {'state': 'current' if ready else 'stale',
            'actual': desired if ready else digest(facts), 'desired': desired}, environment


def main():
    arguments = sys.argv[1:]
    if arguments == ['observe']:
        result, _ = observe()
    elif len(arguments) == 4 and arguments[0] == 'apply':
        if not re.fullmatch('[a-zA-Z0-9_-]+', arguments[1]) or any(
                not re.fullmatch('[a-f0-9]{64}', item) for item in arguments[2:]):
            raise ValueError('invalid GitHub runtime activation arguments')
        result, environment = observe()
        if result['state'] == 'current' and result['actual'] == arguments[3]:
            print(json.dumps(result))
            return
        if result['actual'] != arguments[2] or result['desired'] != arguments[3]:
            raise ValueError('GitHub runtime assessment changed before apply')
        subprocess.run([str(SOURCE/'owner.sh'), '--yes'], env=environment,
                       stdout=sys.stderr, check=True, timeout=90)
        result, _ = observe()
        if result['state'] != 'current' or result['desired'] != arguments[3]:
            raise ValueError('GitHub runtime activation did not converge')
    else:
        raise ValueError('invalid GitHub runtime activation action')
    print(json.dumps(result))


if __name__ == '__main__':
    try:
        main()
    except (ValueError, OSError, KeyError, subprocess.SubprocessError):
        print('GitHub runtime activation failed; inspect the selected broker service', file=sys.stderr)
        sys.exit(1)
