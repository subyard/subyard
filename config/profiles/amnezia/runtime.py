#!/usr/bin/env python3
"""Prepare the dedicated VPN environment; Amnezia owns VPN containers and peers."""
import argparse
import fcntl
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import pwd
import re
import shutil
import stat
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parent
STATE = Path('/srv/amnezia')
UNIT = 'subyard-amnezia.service'
ADMIN = 'amnezia'
MARKER = 'subyard-amnezia-native-v1'
SSH_POLICY = (f'Match User {ADMIN}\n AuthenticationMethods publickey\n PasswordAuthentication no\n'
              ' KbdInteractiveAuthentication no\n DisableForwarding yes\nMatch all\n')
TABLE = 'subyard_amnezia'
FIREWALL_STATE = Path('/run/subyard-amnezia-firewall.json')
DOCKER_CONFIG = Path('/etc/docker/daemon.json')
PUBLIC_DNS = ['1.1.1.1', '1.0.0.1']
DENIED = ('0.0.0.0/8', '10.0.0.0/8', '100.64.0.0/10', '127.0.0.0/8',
          '169.254.0.0/16', '172.16.0.0/12', '192.0.0.0/24', '192.0.2.0/24',
          '192.168.0.0/16', '198.18.0.0/15', '198.51.100.0/24',
          '203.0.113.0/24', '224.0.0.0/4', '240.0.0.0/4')


def run(*args, data=None, check=True):
    result = subprocess.run(args, input=data, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    if check and result.returncode:
        raise RuntimeError('environment command failed: ' + args[0])
    return result


def protected(path, directory=False, modes=(0o600,)):
    info = path.lstat()
    if info.st_uid != 0 or stat.S_IMODE(info.st_mode) not in ((0o700,) if directory else modes):
        raise RuntimeError('VPN environment state has unsafe ownership or permissions')
    if not (stat.S_ISDIR(info.st_mode) if directory else stat.S_ISREG(info.st_mode)):
        raise RuntimeError('VPN environment state has an unsafe file type')


def atomic(path, content, mode=0o600):
    if path.exists() or path.is_symlink():
        protected(path, modes=(0o600, mode))
    descriptor, temporary = tempfile.mkstemp(prefix='.amnezia-', dir=path.parent)
    try:
        with os.fdopen(descriptor, 'w') as output:
            os.fchmod(output.fileno(), mode)
            os.fchown(output.fileno(), 0, 0)
            output.write(content)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def require_state_mount():
    if not stat.S_ISDIR(STATE.parent.lstat().st_mode) or not os.path.ismount(STATE.parent):
        raise RuntimeError('VPN state volume is not mounted at /srv')


def check_state():
    require_state_mount()
    if STATE.exists() or STATE.is_symlink():
        protected(STATE, True)
        if not (STATE / 'environment').exists():
            raise RuntimeError('legacy or unowned VPN state; use a fresh dedicated VPN yard')
        protected(STATE / 'environment')
        if (STATE / 'environment').read_text() != MARKER + '\n':
            raise RuntimeError('VPN environment ownership does not match')
        protected(STATE / 'settings.json')
        return True
    return False


def initialize():
    if check_state():
        return
    staging = Path(tempfile.mkdtemp(prefix='.amnezia-', dir=STATE.parent))
    os.chmod(staging, 0o700)
    try:
        atomic(staging / 'environment', MARKER + '\n')
        atomic(staging / 'settings.json', json.dumps(dict(enabled=False, endpoint='', port=0)) + '\n')
        os.rename(staging, STATE)
    finally:
        if staging.exists():
            shutil.rmtree(staging)


def settings():
    require_state_mount()
    protected(STATE, True)
    protected(STATE / 'environment')
    if (STATE / 'environment').read_text() != MARKER + '\n':
        raise RuntimeError('VPN environment ownership does not match')
    protected(STATE / 'settings.json')
    value = json.loads((STATE / 'settings.json').read_text())
    if set(value) != {'enabled', 'endpoint', 'port'} or not isinstance(value['enabled'], bool):
        raise RuntimeError('invalid VPN environment settings')
    if value['endpoint']:
        value['endpoint'] = str(ipaddress.IPv4Address(value['endpoint']))
    if type(value['port']) is not int or not 0 <= value['port'] <= 65535:
        raise RuntimeError('invalid VPN environment port')
    if value['enabled'] and (not value['endpoint'] or not value['port']):
        raise RuntimeError('enabled VPN route has no endpoint')
    return value


def storage_bound(service, name):
    result = run('systemctl', 'show', service, '--property=MainPID', '--value', check=False)
    pid = result.stdout.decode().strip()
    if result.returncode or not pid.isdecimal() or int(pid) == 0:
        return False
    try:
        target = (STATE / name).stat()
        mounted = Path(f'/proc/{pid}/root/var/lib/{name}').stat()
        return (target.st_dev, target.st_ino) == (mounted.st_dev, mounted.st_ino)
    except OSError:
        return False


def storage_policy(name):
    dropin = Path('/etc/systemd/system') / (name + '.service.d') / '90-subyard-amnezia-storage.conf'
    content = ('# Managed by the Subyard Amnezia environment.\n'
               '[Unit]\nRequiresMountsFor=/srv\nAssertPathIsMountPoint=/srv\n'
               f'[Service]\nBindPaths={STATE / name}:/var/lib/{name}\n'
               + ('ExecStartPre=/usr/bin/python3 /usr/local/lib/subyard-amnezia/runtime.py start\n'
                  if name == 'docker' else ''))
    return dropin, content


def storage_policy_ready(name):
    dropin, content = storage_policy(name)
    try:
        info = dropin.lstat()
        return (not dropin.parent.is_symlink() and stat.S_ISREG(info.st_mode)
                and info.st_uid == 0 and info.st_gid == 0 and stat.S_IMODE(info.st_mode) == 0o644
                and dropin.read_text() == content)
    except OSError:
        return False


def docker_settings():
    if DOCKER_CONFIG.parent.is_symlink():
        raise RuntimeError('Docker configuration directory must not be a symlink')
    if not DOCKER_CONFIG.exists() and not DOCKER_CONFIG.is_symlink():
        return {}
    protected(DOCKER_CONFIG, modes=(0o600, 0o644))
    if DOCKER_CONFIG.lstat().st_gid != 0:
        raise RuntimeError('Docker configuration has unsafe ownership')
    value = json.loads(DOCKER_CONFIG.read_text())
    if not isinstance(value, dict):
        raise RuntimeError('Docker configuration must be a JSON object')
    return value


def docker_dns_ready():
    try:
        return docker_settings().get('dns') == PUBLIC_DNS
    except (OSError, ValueError, RuntimeError):
        return False


def prepare_docker_dns():
    value = docker_settings()
    if value.get('dns') == PUBLIC_DNS:
        return False
    if DOCKER_CONFIG.parent.is_symlink():
        raise RuntimeError('Docker configuration directory must not be a symlink')
    DOCKER_CONFIG.parent.mkdir(mode=0o755, exist_ok=True)
    value['dns'] = PUBLIC_DNS
    mode = stat.S_IMODE(DOCKER_CONFIG.lstat().st_mode) if DOCKER_CONFIG.exists() else 0o644
    atomic(DOCKER_CONFIG, json.dumps(value, sort_keys=True) + '\n', mode=mode)
    return True


def storage_initialized():
    seeded = STATE / 'storage-seeded'
    try:
        protected(seeded)
        return seeded.read_text() == MARKER + '\n'
    except (OSError, RuntimeError):
        return False


def prepare_storage(force_restart=False):
    # Private service mounts retain both classic Docker and containerd image stores.
    # RequiresMountsFor and AssertPathIsMountPoint forbid a fallback onto the root disk.
    seeded = STATE / 'storage-seeded'
    if not seeded.exists() and not seeded.is_symlink():
        for name in ('docker', 'containerd'):
            destination = STATE / name
            if destination.is_symlink() or (destination.exists() and not destination.is_dir()):
                raise RuntimeError('unseeded persistent container storage has an unsafe file type')
            if destination.exists() and any(destination.iterdir()):
                raise RuntimeError('persistent container storage has data but no ownership marker; preserve it for recovery')
        run('systemctl', 'stop', 'docker.service', 'docker.socket', 'containerd.service')
        for name in ('docker', 'containerd'):
            destination = STATE / name
            if destination.is_symlink():
                raise RuntimeError('persistent container storage must not be a symlink')
            destination.mkdir(mode=0o700, exist_ok=True)
            source = Path('/var/lib') / name
            if source.exists():
                if source.is_symlink() or not source.is_dir():
                    raise RuntimeError('container storage has an unsafe file type')
                shutil.copytree(source, destination, dirs_exist_ok=True, symlinks=True)
        atomic(seeded, MARKER + '\n')
    else:
        protected(seeded)
        if seeded.read_text() != MARKER + '\n':
            raise RuntimeError('container storage ownership does not match')
    changed = False
    for name in ('docker', 'containerd'):
        dropin, content = storage_policy(name)
        destination = dropin.parent
        if destination.is_symlink():
            raise RuntimeError('container service directory must not be a symlink')
        destination.mkdir(mode=0o755, exist_ok=True)
        if dropin.is_symlink():
            raise RuntimeError('container service configuration must not be a symlink')
        if dropin.exists() and not stat.S_ISREG(dropin.lstat().st_mode):
            raise RuntimeError('container service configuration must be a regular file')
        if not storage_policy_ready(name):
            run('install', '-o', '0', '-g', '0', '-m', '0644', '/dev/stdin', str(dropin),
                data=content.encode())
            changed = True
    if force_restart or changed or not all(storage_bound(name + '.service', name) for name in ('docker', 'containerd')):
        run('systemctl', 'daemon-reload')
        run('systemctl', 'restart', 'containerd.service', 'docker.service')
    if not all(storage_policy_ready(name) and storage_bound(name + '.service', name)
               for name in ('docker', 'containerd')):
        raise RuntimeError('container storage is not bound to the persistent state volume')


def admin_ready():
    key = STATE / 'admin.key'
    if not key.exists():
        return False
    protected(key)
    public = run('ssh-keygen', '-y', '-f', str(key)).stdout.decode().strip()
    home = Path('/home') / ADMIN
    authorized = home / '.ssh' / 'authorized_keys'
    grant = Path('/etc/sudoers.d/91-subyard-amnezia-admin')
    policy = Path('/etc/ssh/sshd_config.d/01-subyard-amnezia-admin.conf')
    try:
        uid = pwd.getpwnam(ADMIN).pw_uid
        for path, owner, mode, directory in ((home / '.ssh', uid, 0o700, True),
                                              (authorized, uid, 0o600, False),
                                              (grant, 0, 0o440, False), (policy, 0, 0o644, False)):
            info = path.lstat()
            if info.st_uid != owner or stat.S_IMODE(info.st_mode) != mode or not (
                    stat.S_ISDIR(info.st_mode) if directory else stat.S_ISREG(info.st_mode)):
                return False
    except (OSError, KeyError):
        return False
    return (authorized.read_text() == 'restrict ' + public + '\n'
            and grant.read_text() == ADMIN + ' ALL=(ALL) NOPASSWD:ALL\n'
            and policy.read_text() == SSH_POLICY
            and run('sshd', '-t', check=False).returncode == 0
            and run('systemctl', 'is-active', '--quiet', 'ssh', check=False).returncode == 0)


def prepare_admin():
    key = STATE / 'admin.key'
    if not key.exists() and not key.is_symlink():
        run('ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', str(key))
        os.chmod(key, 0o600)
        os.chmod(Path(str(key) + '.pub'), 0o600)
    protected(key)
    public = run('ssh-keygen', '-y', '-f', str(key)).stdout.decode().strip()
    if run('id', '-u', ADMIN, check=False).returncode:
        run('useradd', '--create-home', '--shell', '/bin/bash', ADMIN)
    home = Path('/home') / ADMIN
    if home.is_symlink() or (home / '.ssh').is_symlink():
        raise RuntimeError('administrative home must not be a symlink')
    run('install', '-d', '-o', ADMIN, '-g', ADMIN, '-m', '0700', str(home / '.ssh'))
    authorized = home / '.ssh' / 'authorized_keys'
    if authorized.is_symlink():
        raise RuntimeError('administrative authorization must not be a symlink')
    run('install', '-o', ADMIN, '-g', ADMIN, '-m', '0600', '/dev/stdin', str(authorized),
        data=('restrict ' + public + '\n').encode())
    grant = Path('/etc/sudoers.d/91-subyard-amnezia-admin')
    if grant.is_symlink():
        raise RuntimeError('administrative sudo policy must not be a symlink')
    run('install', '-o', '0', '-g', '0', '-m', '0440', '/dev/stdin', str(grant),
        data=(ADMIN + ' ALL=(ALL) NOPASSWD:ALL\n').encode())
    run('visudo', '-cf', str(grant))
    dropin = Path('/etc/ssh/sshd_config.d/01-subyard-amnezia-admin.conf')
    if dropin.is_symlink():
        raise RuntimeError('administrative SSH policy must not be a symlink')
    run('install', '-o', '0', '-g', '0', '-m', '0644', '/dev/stdin', str(dropin), data=SSH_POLICY.encode())
    run('sshd', '-t')
    run('systemctl', 'reload', 'ssh')
    if not admin_ready():
        raise RuntimeError('administrative SSH preparation did not converge')


def firewall_exists():
    value = json.loads(run('nft', '-j', 'list', 'tables').stdout)
    return any(item.get('table', {}).get('name') == TABLE and
               item.get('table', {}).get('family') == 'inet' for item in value['nftables'])


def firewall_owned():
    if not firewall_exists():
        return False
    value = json.loads(run('nft', '-j', 'list', 'table', 'inet', TABLE).stdout)
    if not any(item.get('table', {}).get('comment') == MARKER for item in value['nftables']):
        raise RuntimeError('refusing a foreign VPN boundary table')
    return True


def uplink():
    routes = json.loads(run('ip', '-4', '-j', 'route', 'get', '1.1.1.1').stdout)
    if not isinstance(routes, list) or len(routes) != 1 or not isinstance(routes[0], dict):
        raise RuntimeError('VPN VM requires one IPv4 uplink route')
    interface = routes[0].get('dev', '')
    if not isinstance(interface, str) or not re.fullmatch(r'[A-Za-z0-9_.-]{1,15}', interface) or interface == 'lo':
        raise RuntimeError('VPN VM uplink interface is invalid')
    links = json.loads(run('ip', '-j', 'link', 'show', 'dev', interface).stdout)
    if not isinstance(links, list) or len(links) != 1 or not isinstance(links[0], dict) or \
            links[0].get('ifname') != interface or not isinstance(links[0].get('flags'), list) or \
            'UP' not in links[0]['flags']:
        raise RuntimeError('VPN VM uplink interface is not active')
    return interface


def firewall_policy():
    value = settings()
    interface = uplink()
    outbound = f'iifname != "{interface}" oifname "{interface}"'
    blocked = ', '.join((*DENIED, *((value['endpoint'] + '/32',) if value['endpoint'] else ())))
    port = value['port']
    closed = f'iifname "{interface}" udp dport {port} drop\n' if port and not value['enabled'] else ''
    # Encrypted replies may reach a private test peer; decrypted new flows may not.
    replies = (f'{outbound} udp sport {port} ct direction reply '
               f'ct original proto-dst {port} accept\n') if port and value['enabled'] else ''
    return f'''table inet {TABLE} {{
 comment "{MARKER}"
 chain input {{
  type filter hook input priority -200; policy accept;
  {closed}
  iifname "lo" accept
  iifname "{interface}" accept
  drop
 }}
 chain forward {{
  type filter hook forward priority -200; policy accept;
  {closed}
  {outbound} meta nfproto ipv6 drop
  {replies}
  {outbound} ip daddr {{ {blocked} }} drop
 }}
}}
'''


def firewall_digest():
    value = json.loads(run('nft', '-j', 'list', 'table', 'inet', TABLE).stdout)
    records = [{kind: {key: value for key, value in record.items() if key != 'handle'}}
               for item in value['nftables'] for kind, record in item.items() if kind != 'metainfo']
    return hashlib.sha256(json.dumps(records, sort_keys=True).encode()).hexdigest()


def firewall_ready():
    if not firewall_owned() or not FIREWALL_STATE.exists():
        return False
    protected(FIREWALL_STATE)
    signature = json.loads(FIREWALL_STATE.read_text())
    return (signature.get('policy') == hashlib.sha256(firewall_policy().encode()).hexdigest()
            and signature.get('effective') == firewall_digest())


def firewall():
    present = firewall_owned()
    policy = firewall_policy()
    batch = (f'delete table inet {TABLE}\n' if present else '') + policy
    run('nft', '-f', '-', data=batch.encode())
    atomic(FIREWALL_STATE, json.dumps(dict(
        policy=hashlib.sha256(policy.encode()).hexdigest(), effective=firewall_digest())) + '\n')
    if not firewall_ready():
        raise RuntimeError('VPN environment boundary did not converge')


def native_status(port):
    installed, running = False, False
    for name in ('amnezia-awg2', 'amnezia-awg'):
        result = run('docker', 'container', 'inspect', name, check=False)
        if result.returncode:
            continue
        container = json.loads(result.stdout)[0]
        installed = True
        if container['State']['Running']:
            probe = run('docker', 'exec', name, 'awg', 'show', 'awg0', 'listen-port', check=False)
            running = running or (bool(port) and probe.returncode == 0 and probe.stdout.strip() == str(port).encode())
    return dict(vpn_installed=installed, vpn_running=running)


def observe():
    value = settings()
    management_ready = storage_initialized() and admin_ready() and docker_dns_ready() and all(
        storage_policy_ready(name) and storage_bound(name + '.service', name)
        for name in ('docker', 'containerd'))
    boundary_ready = firewall_ready()
    enabled = value['enabled']
    return dict(ready=management_ready and boundary_ready, running=enabled and boundary_ready,
                enabled=enabled, management_ready=management_ready, **native_status(value['port']))


def up(endpoint, port):
    settings()
    address = str(ipaddress.IPv4Address(endpoint))
    if not 1024 <= port <= 65535:
        raise RuntimeError('VPN port must be in 1024..65535')
    atomic(STATE / 'settings.json', json.dumps(dict(enabled=True, endpoint=address, port=port)) + '\n')
    run('systemctl', 'restart', UNIT)
    if not observe()['ready']:
        raise RuntimeError('VPN environment is not ready')


def down():
    value = settings()
    value['enabled'] = False
    atomic(STATE / 'settings.json', json.dumps(value) + '\n')
    run('systemctl', 'restart', UNIT)
    if observe()['running'] or observe()['enabled']:
        raise RuntimeError('VPN network shutdown did not converge')


def provision():
    initialize()
    prepare_storage(force_restart=prepare_docker_dns())
    prepare_admin()
    run('systemctl', 'enable', UNIT)
    run('systemctl', 'restart', UNIT)
    if not observe()['ready']:
        raise RuntimeError('VPN environment preparation did not converge')


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('command', choices=['observe', 'provision', 'up', 'down', 'start', 'check-state'])
    parser.add_argument('--endpoint')
    parser.add_argument('--port', type=int, default=51820)
    args = parser.parse_args()
    if os.geteuid() != 0:
        raise RuntimeError('run inside the dedicated VM as root')
    if args.command == 'check-state':
        check_state()
        return
    if args.command == 'start':
        # systemd serializes starts. Lifecycle callers already hold this lock
        # while waiting for their child unit; taking it here would deadlock.
        firewall()
        return
    with open('/run/subyard-amnezia.lock', 'a', opener=lambda path, flags: os.open(
            path, flags | os.O_NOFOLLOW, 0o600)) as lock:
        info = os.fstat(lock.fileno())
        if info.st_uid != 0 or stat.S_IMODE(info.st_mode) != 0o600 or not stat.S_ISREG(info.st_mode):
            raise RuntimeError('VPN environment lock has unsafe ownership or permissions')
        fcntl.flock(lock, fcntl.LOCK_EX)
        if args.command == 'observe':
            print(json.dumps(observe()))
        elif args.command == 'provision':
            provision()
        elif args.command == 'up':
            up(args.endpoint, args.port)
        elif args.command == 'down':
            down()


if __name__ == '__main__':
    try:
        main()
    except RuntimeError as error:
        # Runtime errors contain bounded diagnostics, never configuration contents.
        print('amnezia: ' + str(error), file=sys.stderr)
        sys.exit(1)
    except (OSError, ValueError, KeyError):
        print('amnezia: environment operation failed', file=sys.stderr)
        sys.exit(1)
