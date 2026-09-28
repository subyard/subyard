#!/usr/bin/env python3
"""Owner-side physical leaf for the typed VPN resource operation."""
import hashlib
import ipaddress
import json
import os
import re
import subprocess
import sys

DEVICE = 'amnezia-vpn'
KEY = 'user.subyard.resource.' + DEVICE
RUNTIME = '/usr/local/lib/subyard-amnezia/runtime.py'
YARD = os.environ.get('YARD_INSTANCE_NAME', '')
PROJECT = os.environ.get('INCUS_PROJECT', '')


def run(*args, check=True, timeout=None):
    result = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=timeout)
    if check and result.returncode:
        raise RuntimeError('owner or guest command failed: ' + args[0])
    return result


def incus(*args, check=True):
    return run('incus', *args, '--project', PROJECT, check=check)


def guest(*args, check=True, timeout=None):
    return run('incus', 'exec', YARD, '--project', PROJECT, '--', *args, check=check, timeout=timeout)


def query(path):
    return json.loads(run('incus', 'query', path).stdout)


def inspect():
    return query(f'/1.0/instances/{YARD}?project={PROJECT}')


def fingerprint(device):
    data = 'v1\n' + ''.join(f'{name}={device[name]}\n' for name in sorted(device))
    return 'v1:' + hashlib.sha256(data.encode()).hexdigest()


def owned(instance):
    device = instance.get('devices', {}).get(DEVICE)
    marker = instance.get('config', {}).get(KEY, '')
    if device and marker not in (fingerprint(device), fingerprint(device).replace('v1:', 'v1:pending:', 1)):
        raise RuntimeError('refusing a foreign or modified VPN ingress device')
    if not device and marker and not re.fullmatch(r'v1:pending:[0-9a-f]{64}', marker):
        raise RuntimeError('refusing ambiguous VPN ingress ownership metadata')
    if DEVICE in instance.get('expanded_devices', {}) and not device:
        raise RuntimeError('VPN ingress must not be inherited from an Incus profile')
    return device


def endpoint(instance):
    address = ipaddress.IPv4Address(os.environ.get('RESOURCE_VPN_IPV4', ''))
    if not address.is_global and (address.is_unspecified or address.is_multicast or address.is_loopback or address.is_link_local):
        raise RuntimeError('RESOURCE_VPN_IPV4 must be the exact owner IPv4')
    interface = os.environ.get('RESOURCE_VPN_INTERFACE', '')
    if not re.fullmatch(r'[A-Za-z0-9_.-]{1,15}', interface):
        raise RuntimeError('RESOURCE_VPN_INTERFACE is required')
    links = json.loads(run('ip', '-4', '-j', 'address', 'show', 'dev', interface).stdout)
    if len(links) != 1 or 'UP' not in links[0]['flags'] or not any(
            info.get('local') == str(address) for info in links[0]['addr_info']):
        raise RuntimeError('configured IPv4 is not active on the selected owner interface')
    port = os.environ.get('RESOURCE_VPN_PORT', '51820')
    if not port.isdecimal() or str(int(port)) != port or not 1024 <= int(port) <= 65535:
        raise RuntimeError('RESOURCE_VPN_PORT must be in 1024..65535')
    nic = instance['expanded_devices'].get('eth0', {})
    target = ipaddress.IPv4Address(nic.get('ipv4.address', ''))
    if target.is_unspecified or target.is_multicast or target.is_loopback or target.is_link_local:
        raise RuntimeError('VPN VM needs a pinned primary IPv4 address; rerun init')
    device = dict(type='proxy', bind='host', nat='true',
                  listen=f'udp:{address}:{port}', connect=f'udp:{target}:51820')
    return str(address), interface, port, device


def settings_valid(require_selected=True):
    if os.environ.get('EXCLUSIVE_ENVIRONMENT_PROFILE') != 'amnezia' or os.environ.get('YARD_KIND') != 'vm':
        raise RuntimeError('VPN requires the dedicated amnezia VM preset')
    if os.environ.get('YARD_NAME', 'default') == 'default':
        raise RuntimeError('VPN requires an explicit named yard')
    if require_selected and 'amnezia' not in os.environ.get('ENVIRONMENT_PROFILES', '').split():
        raise RuntimeError('amnezia is not selected in this yard')


def runtime_status(instance):
    if instance.get('status') != 'Running':
        return dict(ready=False, running=False, enabled=False)
    return json.loads(guest('python3', RUNTIME, 'observe').stdout)


def ready(instance):
    settings_valid()
    if not owned(instance):
        return False
    _, _, _, want = endpoint(instance)
    status = runtime_status(instance)
    return (owned(instance) == want and instance['config'].get(KEY) == fingerprint(want)
            and status['ready'] and status['enabled'])


def port_spec_contains(spec, port):
    if not isinstance(spec, str) or not spec:
        raise RuntimeError('invalid Incus UDP port specification')
    found = False
    for item in spec.split(','):
        bounds = item.split('-')
        if len(bounds) not in (1, 2) or not all(value.isdecimal() for value in bounds):
            raise RuntimeError('invalid Incus UDP port specification')
        first, last = int(bounds[0]), int(bounds[-1])
        if not 1 <= first <= last <= 65535:
            raise RuntimeError('invalid Incus UDP port specification')
        found = found or first <= port <= last
    return found


def collisions(want):
    listen = want['listen']
    _, address, port = listen.split(':')
    wanted_port = int(port)
    for line in run('ss', '-Hlun', 'sport', '=', ':' + port).stdout.decode().splitlines():
        fields = line.split()
        if len(fields) >= 5 and fields[-2] in (f'{address}:{port}', f'0.0.0.0:{port}', f'*:{port}', f'[::]:{port}'):
            raise RuntimeError('owner UDP port is already bound')
    instances = json.loads(run('incus', 'list', '--all-projects', '--format=json').stdout)
    for instance in instances:
        for name, device in instance.get('expanded_devices', {}).items():
            if instance['name'] == YARD and instance.get('project', 'default') == PROJECT and name == DEVICE:
                continue
            if device.get('type') != 'proxy':
                continue
            if device.get('bind', 'host') == 'instance':
                continue
            proxy_listen = device.get('listen', '')
            if not isinstance(proxy_listen, str):
                raise RuntimeError('invalid Incus proxy listener')
            match = re.fullmatch(r'udp:(\[[^]]+\]|[^:]+):(.+)', proxy_listen)
            if match and match.group(1) in (address, '0.0.0.0', '[::]') and port_spec_contains(match.group(2), wanted_port):
                raise RuntimeError('owner UDP port conflicts with another Incus proxy')
    # Native network forwards do not appear as listening sockets.
    for network in query('/1.0/networks?recursion=1'):
        if not network.get('managed'):
            continue
        for forward in query('/1.0/networks/' + network['name'] + '/forwards?recursion=1'):
            if forward.get('listen_address') != address:
                continue
            if forward.get('config', {}).get('target_address'):
                raise RuntimeError('owner address already belongs to an Incus network forward')
            for rule in forward.get('ports', []):
                if rule.get('protocol') != 'udp':
                    continue
                if port_spec_contains(rule.get('listen_port', ''), wanted_port):
                    raise RuntimeError('owner UDP port conflicts with an Incus network forward')


def remove_ingress(retain_pending=False):
    instance = inspect()
    device = owned(instance)
    marker = instance['config'].get(KEY, '')
    pending = marker
    if device and retain_pending:
        pending = fingerprint(device).replace('v1:', 'v1:pending:', 1)
        if marker != pending:
            incus('config', 'set', YARD, KEY, pending)
    if device:
        incus('config', 'device', 'remove', YARD, DEVICE)
    if marker and not retain_pending:
        incus('config', 'unset', YARD, KEY)
    current = inspect()
    if owned(current) or (retain_pending and current['config'].get(KEY, '') != pending):
        raise RuntimeError('VPN ingress removal did not converge')


def ensure_ingress(want):
    instance = inspect()
    if owned(instance) == want and instance['config'].get(KEY) == fingerprint(want):
        return
    remove_ingress(retain_pending=True)
    incus('config', 'set', YARD, KEY, fingerprint(want).replace('v1:', 'v1:pending:', 1))
    try:
        incus('config', 'device', 'add', YARD, DEVICE, 'proxy',
              *(f'{name}={value}' for name, value in want.items() if name != 'type'))
        if owned(inspect()) != want:
            raise RuntimeError('VPN ingress publication did not converge')
        incus('config', 'set', YARD, KEY, fingerprint(want))
    except RuntimeError:
        remove_ingress(retain_pending=True)
        raise


def prepare(verb):
    changed, consequences = False, []
    if verb in ('up', 'down'):
        settings_valid(verb == 'up')
        instance = inspect()
        device = owned(instance)
        if instance.get('type') != 'virtual-machine':
            raise RuntimeError('VPN target is not a VM')
        if instance.get('status') != 'Running':
            raise RuntimeError('start the dedicated VPN yard before changing its service')
        if verb == 'up':
            status = runtime_status(instance)
            address, interface, port, want = endpoint(instance)
            collisions(want)
            projects = guest('find', '/srv/workspaces', '-mindepth', '1', '-maxdepth', '1', '-print', '-quit')
            if projects.stdout:
                raise RuntimeError('VPN cannot run in a yard containing work projects')
            changed = device != want or instance['config'].get(KEY) != fingerprint(want) or not status['ready'] or not status['enabled']
            if changed:
                consequences = [f'Enable the pinned AmneziaWG service in {YARD}; preserve existing keys and peers',
                                f'Publish UDP {address}:{port} on {interface} to {want["connect"]}']
        else:
            changed = bool(device or instance['config'].get(KEY))
            if not changed:
                status = runtime_status(instance)
                changed = bool(status['running'] or status['enabled'])
            if changed:
                consequences = [f'Close the owned VPN ingress in {YARD} and attempt guest shutdown; '
                                'retain pending cleanup if the guest remains unavailable; preserve keys and peers']
    print(json.dumps(dict(schema='yard.resource-action-assessment.v1', action=verb,
                          changed=changed, consequences=consequences)))


def shutdown_guest():
    try:
        # Docker receives 15 seconds to stop; leave room in the engine's 30-second rollback.
        guest('python3', RUNTIME, 'down', timeout=20)
    except (OSError, RuntimeError, subprocess.TimeoutExpired) as error:
        raise RuntimeError('owned VPN ingress closed; guest shutdown is unverified; retry vpn down') from error


def main():
    args = sys.argv[1:]
    if args in ([], ['help'], ['--help'], ['-h']):
        print('Usage: yard -Y <vpn-yard> vpn <up|status|down>')
        return 0
    if len(args) != 1 or args[0] not in ('up', 'down', 'status', 'is-up', 'rollback-ingress'):
        print('vpn: expected up, status or down', file=sys.stderr)
        return 2
    if os.environ.get('SUBYARD_ENGINE_CONTEXT') != '1' or not YARD or not PROJECT:
        raise RuntimeError('typed owner context is required')
    verb = args[0]
    mode = os.environ.get('SUBYARD_RESOURCE_MODE', '')
    if mode == 'prepare' and verb != 'rollback-ingress':
        prepare(verb)
        return 0
    if verb == 'is-up':
        return 0 if ready(inspect()) else 1
    if mode != 'apply' or not os.environ.get('SUBYARD_OPERATION_ID'):
        raise RuntimeError('typed resource apply operation is required')
    expected = 'up' if verb == 'rollback-ingress' else verb
    if os.environ.get('SUBYARD_RESOURCE_ACTION') != expected:
        raise RuntimeError('prepared resource action does not match')
    if verb == 'rollback-ingress':
        remove_ingress(retain_pending=True)
        shutdown_guest()
        remove_ingress()
    elif verb == 'status':
        settings_valid()
        instance = inspect()
        status = runtime_status(instance)
        print(json.dumps(dict(ready=ready(instance), running=status['running'], enabled=status['enabled'],
                              ingress=bool(owned(instance)))))
    elif verb == 'down':
        remove_ingress(retain_pending=True)
        shutdown_guest()
        remove_ingress()
    else:
        settings_valid()
        instance = inspect()
        address, _, port, want = endpoint(instance)
        collisions(want)
        guest('python3', RUNTIME, 'up', '--endpoint', address, '--port', port)
        ensure_ingress(want)
        if not ready(inspect()):
            remove_ingress(retain_pending=True)
            raise RuntimeError('VPN runtime or ingress did not converge')
    return 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (OSError, ValueError, KeyError, RuntimeError) as error:
        # RuntimeError messages are bounded constants plus validated public identifiers.
        message = str(error) if isinstance(error, RuntimeError) else 'invalid or unavailable VPN state'
        print('vpn: ' + message, file=sys.stderr)
        sys.exit(1)
