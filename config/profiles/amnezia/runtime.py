#!/usr/bin/env python3
"""Root-only runtime inside the dedicated VPN VM. Never print credential material."""
import argparse
import fcntl
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import secrets
import shutil
import stat
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parent
STATE = Path('/srv/amnezia')
UNIT = 'subyard-amnezia.service'
CONTAINER = 'subyard-amnezia'
TABLE = 'subyard_amnezia'
MARKER = 'subyard-amnezia-v1'
FIREWALL_STATE = Path('/run/subyard-amnezia-firewall.json')
IMAGE = next(line.split('=', 1)[1] for line in (ROOT / 'release.env').read_text().splitlines()
             if line.startswith('AMNEZIA_IMAGE='))
SUBNET = '10.90.0.0/24'
# Decrypted client traffic never reaches local, private, metadata or special-use networks.
DENIED = ('0.0.0.0/8', '10.0.0.0/8', '100.64.0.0/10', '127.0.0.0/8',
          '169.254.0.0/16', '172.16.0.0/12', '192.0.0.0/24', '192.0.2.0/24',
          '192.168.0.0/16', '198.18.0.0/15', '198.51.100.0/24',
          '203.0.113.0/24', '224.0.0.0/4', '240.0.0.0/4')


def run(*args, data=None, check=True):
    result = subprocess.run(args, input=data, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    if check and result.returncode:
        raise RuntimeError('runtime command failed: ' + args[0])
    return result


def protected(path, directory=False):
    info = path.lstat()
    if info.st_uid != 0 or stat.S_IMODE(info.st_mode) != (0o700 if directory else 0o600):
        raise RuntimeError('VPN state has unsafe ownership or permissions')
    if not (stat.S_ISDIR(info.st_mode) if directory else stat.S_ISREG(info.st_mode)):
        raise RuntimeError('VPN state has an unsafe file type')


def atomic(path, content):
    if path.exists() or path.is_symlink():
        protected(path)
    descriptor, temporary = tempfile.mkstemp(prefix='.amnezia-', dir=path.parent)
    try:
        with os.fdopen(descriptor, 'w') as output:
            os.fchmod(output.fileno(), 0o600)
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


def awg(command, data=None):
    return run('docker', 'run', '--rm', '-i', '--network', 'none', '--entrypoint', 'awg',
               IMAGE, command, data=data).stdout.decode().strip()


def initialize(endpoint, port):
    if STATE.exists() or STATE.is_symlink():
        protected(STATE, True)
        for name in ('awg0.conf', 'client.conf', 'settings.json'):
            protected(STATE / name)
        return
    staging = Path(tempfile.mkdtemp(prefix='.amnezia-', dir=STATE.parent))
    os.chmod(staging, 0o700)
    try:
        server_key, client_key, psk = awg('genkey'), awg('genkey'), awg('genpsk')
        server_public = awg('pubkey', (server_key + '\n').encode())
        client_public = awg('pubkey', (client_key + '\n').encode())
        headers = secrets.SystemRandom().sample(range(1, 2**32), 4)
        parameters = 'Jc = 4\nJmin = 40\nJmax = 70\nS1 = 0\nS2 = 0\nS3 = 0\nS4 = 0\n'
        parameters += ''.join(f'H{i + 1} = {value}\n' for i, value in enumerate(headers))
        atomic(staging / 'awg0.conf',
               f'[Interface]\nPrivateKey = {server_key}\nAddress = 10.90.0.1/24\n'
               f'ListenPort = 51820\nMTU = 1280\n{parameters}\n[Peer]\n'
               f'PublicKey = {client_public}\nPresharedKey = {psk}\nAllowedIPs = 10.90.0.2/32\n')
        atomic(staging / 'client.conf',
               f'[Interface]\nPrivateKey = {client_key}\nAddress = 10.90.0.2/32\n'
               f'DNS = 1.1.1.1, 9.9.9.9\nMTU = 1280\n{parameters}\n[Peer]\n'
               f'PublicKey = {server_public}\nPresharedKey = {psk}\n'
               f'Endpoint = {endpoint}:{port}\nAllowedIPs = 0.0.0.0/0, ::/0\nPersistentKeepalive = 25\n')
        atomic(staging / 'settings.json', json.dumps(dict(endpoint=endpoint, port=port)) + '\n')
        os.rename(staging, STATE)
    finally:
        if staging.exists():
            shutil.rmtree(staging)


def container():
    result = run('docker', 'container', 'inspect', CONTAINER, check=False)
    if result.returncode:
        # An unavailable daemon is not evidence that the container is absent.
        run('docker', 'info')
        return None
    value = json.loads(result.stdout)[0]
    if value['Config'].get('Labels', {}).get('subyard.component') != MARKER:
        raise RuntimeError('refusing a foreign VPN container')
    return value


def firewall_exists():
    result = run('nft', '-j', 'list', 'tables')
    return any(item.get('table', {}).get('name') == TABLE and
               item.get('table', {}).get('family') == 'inet'
               for item in json.loads(result.stdout)['nftables'])


def firewall_owned():
    if not firewall_exists():
        return False
    value = json.loads(run('nft', '-j', 'list', 'table', 'inet', TABLE).stdout)
    if not any(item.get('table', {}).get('comment') == MARKER for item in value['nftables']):
        raise RuntimeError('refusing a foreign VPN firewall table')
    return True


def firewall_policy():
    protected(STATE, True)
    protected(STATE / 'settings.json')
    endpoint = str(ipaddress.IPv4Address(json.loads((STATE / 'settings.json').read_text())['endpoint']))
    blocked = ', '.join((*DENIED, endpoint + '/32'))
    return f'''table inet {TABLE} {{
 comment "{MARKER}"
 chain input {{
  type filter hook input priority -200; policy accept;
  iifname "awg0" ip daddr 10.90.0.1 ip protocol icmp icmp type echo-request accept
  iifname "awg0" drop
 }}
 chain forward {{
  type filter hook forward priority -200; policy accept;
  iifname "awg0" meta nfproto ipv6 drop
  iifname "awg0" ip daddr {{ {blocked} }} drop
  iifname "awg0" accept
  oifname "awg0" ct state established,related accept
  oifname "awg0" drop
 }}
 chain nat {{
  type nat hook postrouting priority 100; policy accept;
  ip saddr {SUBNET} oifname != "awg0" masquerade
 }}
}}
'''


def firewall_digest():
    value = json.loads(run('nft', '-j', 'list', 'table', 'inet', TABLE).stdout)
    # Handles are assigned by the kernel and are not policy. Rule order remains significant.
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


def firewall(remove=False):
    present = firewall_owned()
    if remove:
        if present:
            run('nft', 'delete', 'table', 'inet', TABLE)
        if FIREWALL_STATE.exists() or FIREWALL_STATE.is_symlink():
            protected(FIREWALL_STATE)
            FIREWALL_STATE.unlink()
        return
    policy = firewall_policy()
    batch = (f'delete table inet {TABLE}\n' if present else '') + policy
    run('nft', '-f', '-', data=batch.encode())
    # nft installs this batch atomically; remember the effective rules to detect later drift.
    atomic(FIREWALL_STATE, json.dumps(dict(
        policy=hashlib.sha256(policy.encode()).hexdigest(), effective=firewall_digest())) + '\n')
    if not firewall_ready():
        raise RuntimeError('VPN firewall did not converge')


def observe():
    value = container()
    running = bool(value and value['State']['Running'])
    expected_mounts = {'/etc/amnezia/awg0.conf': str(STATE / 'awg0.conf'),
                       '/opt/amnezia/start.sh': str(ROOT / 'container.sh')}
    mounts = value.get('Mounts', []) if running else []
    mounts_ready = len(mounts) == len(expected_mounts) and all(
        any(mount.get('Type') == 'bind' and mount.get('Destination') == target and
            mount.get('Source') == source and mount.get('RW') is False for mount in mounts)
        for target, source in expected_mounts.items())
    config = value.get('Config', {}) if running else {}
    ready = (running and config.get('Image') == IMAGE and
             config.get('Entrypoint') == ['/bin/bash'] and
             config.get('Cmd') == ['/opt/amnezia/start.sh'] and
             value.get('HostConfig', {}).get('NetworkMode') == 'host' and mounts_ready and
             run('systemctl', 'is-active', '--quiet', UNIT, check=False).returncode == 0 and
             firewall_ready())
    if ready:
        ready = run('docker', 'exec', CONTAINER, 'awg', 'show', 'awg0', 'listen-port',
                    check=False).stdout.strip() == b'51820'
    return dict(running=running, ready=ready,
                enabled=run('systemctl', 'is-enabled', '--quiet', UNIT, check=False).returncode == 0,
                state_present=STATE.exists(), image=IMAGE)


def up(endpoint, port):
    initialize(endpoint, port)
    settings = json.dumps(dict(endpoint=endpoint, port=port)) + '\n'
    changed = (STATE / 'settings.json').read_text() != settings
    if changed:
        client = (STATE / 'client.conf').read_text()
        client = '\n'.join(f'Endpoint = {endpoint}:{port}' if line.startswith('Endpoint = ') else line
                           for line in client.splitlines()) + '\n'
        atomic(STATE / 'client.conf', client)
        atomic(STATE / 'settings.json', settings)
    status = observe()
    run('systemctl', 'enable', UNIT)
    if changed or not status['ready']:
        run('systemctl', 'restart', UNIT)
    for _ in range(30):
        if observe()['ready']:
            return
        time.sleep(1)
    raise RuntimeError('VPN runtime failed readiness; state is preserved')


def start():
    protected(STATE, True)
    protected(STATE / 'awg0.conf')
    value = container()
    if value:
        run('docker', 'rm', '-f', CONTAINER)
    # This namespace is the dedicated VM, never the owner host.
    run('sysctl', '-q', '-w', 'net.ipv4.ip_forward=1')
    firewall()
    try:
        return subprocess.call(['docker', 'run', '--rm', '--init', '--name', CONTAINER,
                                '--label', 'subyard.component=' + MARKER,
                                '--log-driver', 'none', '--network', 'host',
                                '--cap-add', 'NET_ADMIN', '--device', '/dev/net/tun',
                                '--mount', f'type=bind,src={STATE}/awg0.conf,dst=/etc/amnezia/awg0.conf,readonly',
                                '--mount', f'type=bind,src={ROOT}/container.sh,dst=/opt/amnezia/start.sh,readonly',
                                '--entrypoint', '/bin/bash', IMAGE, '/opt/amnezia/start.sh'],
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    finally:
        firewall(remove=True)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('command', choices=['observe', 'up', 'down', 'start', 'stop'])
    parser.add_argument('--endpoint')
    parser.add_argument('--port', type=int, default=51820)
    args = parser.parse_args()
    if os.geteuid() != 0:
        raise RuntimeError('root access inside the VPN VM is required')
    if args.command == 'observe':
        print(json.dumps(observe()))
    elif args.command == 'start':
        return start()
    elif args.command == 'stop':
        if container():
            run('docker', 'stop', '--time', '15', CONTAINER)
    else:
        with open('/run/subyard-amnezia.lock', 'a', opener=lambda path, flags: os.open(
                path, flags | os.O_NOFOLLOW, 0o600)) as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            if args.command == 'up':
                endpoint = ipaddress.IPv4Address(args.endpoint)
                if endpoint.is_unspecified or endpoint.is_multicast or endpoint.is_loopback or not 1024 <= args.port <= 65535:
                    raise RuntimeError('invalid VPN endpoint')
                up(str(endpoint), args.port)
            else:
                run('systemctl', 'disable', '--now', UNIT)
                if container():
                    run('docker', 'rm', '-f', CONTAINER)
                firewall(remove=True)
    return 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (OSError, ValueError, KeyError, RuntimeError):
        print('amnezia: runtime operation failed; VPN state is preserved', file=sys.stderr)
        sys.exit(1)
