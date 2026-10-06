#!/usr/bin/env python3
"""Owner-side physical leaf for the typed VPN resource operation."""
import hashlib
import ipaddress
import json
import os
import re
import subprocess
import sys

ADMIN = sys.argv[1:2] == ['--admin']
if ADMIN:
    sys.argv.pop(1)
DEVICE = 'amnezia-admin' if ADMIN else 'amnezia-vpn'
KEY = 'user.subyard.resource.' + DEVICE
STARTUP = 'user.subyard.startup.' + DEVICE
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
    port_setting = 'RESOURCE_VPN_ADMIN_PORT' if ADMIN else 'RESOURCE_VPN_PORT'
    port = os.environ.get(port_setting, '2226' if ADMIN else '51820')
    if not port.isdecimal() or str(int(port)) != port or not 1024 <= int(port) <= 65535:
        raise RuntimeError(port_setting + ' must be in 1024..65535')
    nic = instance['expanded_devices'].get('eth0', {})
    target = ipaddress.IPv4Address(nic.get('ipv4.address', ''))
    if target.is_unspecified or target.is_multicast or target.is_loopback or target.is_link_local:
        raise RuntimeError('VPN VM needs a pinned primary IPv4 address; rerun init')
    protocol, guest_port = ('tcp', '22') if ADMIN else ('udp', port)
    device = dict(type='proxy', bind='host', nat='true',
                  listen=f'{protocol}:{address}:{port}', connect=f'{protocol}:{target}:{guest_port}')
    return str(address), interface, port, device


def settings_valid(require_selected=True):
    if os.environ.get('EXCLUSIVE_ENVIRONMENT_PROFILE') != 'amnezia' or os.environ.get('YARD_KIND') != 'vm':
        raise RuntimeError('VPN requires the dedicated amnezia VM preset')
    if os.environ.get('YARD_NAME', 'default') == 'default':
        raise RuntimeError('VPN requires an explicit named yard')
    if require_selected and 'amnezia' not in os.environ.get('ENVIRONMENT_PROFILES', '').split():
        raise RuntimeError('amnezia is not selected in this yard')


def runtime_status(instance, management_only=False):
    if instance.get('status') != 'Running':
        return dict(ready=False, running=False, enabled=False, management_ready=False,
                    vpn_installed=False, vpn_running=False)
    return json.loads(guest('python3', RUNTIME, 'observe-management' if management_only else 'observe').stdout)


def ready(instance):
    settings_valid()
    if not owned(instance):
        return False
    _, _, _, want = endpoint(instance)
    status = runtime_status(instance, management_only=ADMIN)
    return (owned(instance) == want and instance['config'].get(KEY) == fingerprint(want)
            and (status['management_ready'] if ADMIN else status['ready'] and status['enabled']))


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


def binds_owner(host, address):
    if host == '*':
        return True
    try:
        parsed = ipaddress.ip_address(host.strip('[]'))
    except ValueError:
        return False
    return parsed.is_unspecified or str(getattr(parsed, 'ipv4_mapped', None) or parsed) == address


def collisions(want):
    listen = want['listen']
    protocol, address, port = listen.split(':')
    wanted_port = int(port)
    for line in run('ss', '-Hltn' if protocol == 'tcp' else '-Hlun', 'sport', '=', ':' + port).stdout.decode().splitlines():
        fields = line.split()
        host, _, listener_port = fields[-2].rpartition(':') if len(fields) >= 5 else ('', '', '')
        if listener_port == port and binds_owner(host, address):
            raise RuntimeError('owner ' + protocol.upper() + ' port is already bound')
    instances = query('/1.0/instances?recursion=1&all-projects=true')
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
            match = re.fullmatch(protocol + r':(\[[^]]+\]|[^:]+):(.+)', proxy_listen)
            if match and binds_owner(match.group(1), address) and port_spec_contains(match.group(2), wanted_port):
                raise RuntimeError('owner port conflicts with another Incus proxy')
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
                if rule.get('protocol') != protocol:
                    continue
                if port_spec_contains(rule.get('listen_port', ''), wanted_port):
                    raise RuntimeError('owner port conflicts with an Incus network forward')


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


def emit_native_plan(verb, instance, device, status=None, want=None, interface='', prestart=False):
    target = 'incus:' + PROJECT + '/' + YARD
    binding = dict(action=verb, project=PROJECT, instance=YARD)
    if ADMIN:
        present = bool(device or instance['config'].get(KEY))
        converged = (device == want and instance['config'].get(KEY) == fingerprint(want)) if verb == 'up' else not present
        if verb == 'up':
            binding.update(endpoint=want, interface=interface)
        consequence = (f'Publish administrative SSH access to the dedicated VM in {target}; AmneziaVPN manages the VPN'
                       if verb == 'up' else f'Close administrative SSH access to the dedicated VM in {target}')
        steps = [dict(id='ingress', target='owned administrative proxy ' + target,
                      observed='matching owned route' if converged and verb == 'up' else ('absent' if not present else 'owned route present'),
                      desired='matching owned route' if verb == 'up' else 'absent',
                      decision='skip' if converged else 'apply',
                      preconditions=['the administrative endpoint is exact and has no conflicting owner'],
                      verify='read the owned administrative proxy and matching ownership marker', consequence=consequence)]
        print(json.dumps(dict(schema='yard.resource-action-assessment.v2', action=verb, changed=not converged,
                              consequences=[] if converged else [consequence], steps=steps,
                              binding=hashlib.sha256(json.dumps(binding, sort_keys=True).encode()).hexdigest())))
        return
    if verb == 'up':
        binding.update(endpoint=want, interface=interface)
        service_ready = bool(status and status['ready'] and status['enabled'])
        service = dict(id='service', target='VPN environment network boundary ' + target,
                       observed='unknown' if prestart else ('enabled and ready' if service_ready else 'not ready'),
                       desired='enabled and ready', decision='conditional' if prestart else ('skip' if service_ready else 'apply'),
                       preconditions=['the dedicated VM is running and has no work projects'],
                       verify='read native VPN readiness and service enablement',
                       consequence=f'Enable the VPN network boundary in {target}; Amnezia retains ownership of its containers and clients')
        route_ready = device == want and instance['config'].get(KEY) == fingerprint(want)
        route = dict(id='ingress', target='owned VPN proxy ' + target,
                     observed=fingerprint(want) if route_ready else ('absent' if not device else fingerprint(device)),
                     desired=fingerprint(want), decision='skip' if route_ready else 'apply', dependsOn=['service'],
                     preconditions=['the exact owner address and interface are active and the UDP endpoint has no conflicting owner'],
                     verify='read the owned Incus proxy and matching ownership marker',
                     consequence=f'Publish UDP {os.environ.get("RESOURCE_VPN_IPV4")}:{os.environ.get("RESOURCE_VPN_PORT", "51820")} on {interface} to {want["connect"]}')
        steps = [service, route]
    else:
        ingress_present = bool(device or instance['config'].get(KEY))
        consequence = (f'Close the owned VPN ingress in incus:{PROJECT}/{YARD} and attempt guest shutdown; '
                       'retain pending cleanup if the guest remains unavailable; preserve keys and peers')
        route = dict(id='ingress', target='owned VPN proxy ' + target,
                     observed='owned ingress present' if ingress_present else 'absent', desired='absent',
                     decision='apply' if ingress_present else 'skip',
                     preconditions=['any current ingress device and marker have matching native ownership'],
                     verify='read the owned Incus proxy and marker and confirm both absent', consequence=consequence)
        service_stopped = bool(status and not status['running'] and not status['enabled'])
        service = dict(id='service', target='VPN environment network boundary ' + target,
                       observed='unknown' if status is None else ('disabled and stopped' if service_stopped else 'enabled or running'),
                       desired='disabled and stopped', decision='conditional' if status is None else ('skip' if service_stopped else 'apply'),
                       dependsOn=['ingress'], preconditions=['the owned ingress is closed before guest shutdown'],
                       verify='read guest runtime running and enabled state', consequence=consequence)
        steps = [route, service]
    changed = any(step['decision'] != 'skip' for step in steps)
    consequences = list(dict.fromkeys(step['consequence'] for step in steps if step['decision'] != 'skip'))
    print(json.dumps(dict(schema='yard.resource-action-assessment.v2', action=verb, changed=changed,
                          consequences=consequences, steps=steps,
                          binding=hashlib.sha256(json.dumps(binding, sort_keys=True).encode()).hexdigest())))


def prepare(verb, prestart=False):
    if verb not in ('up', 'down'):
        print(json.dumps(dict(schema='yard.resource-action-assessment.v1', action=verb,
                              changed=False, consequences=[])))
        return
    settings_valid(verb == 'up')
    instance = inspect()
    device = owned(instance)
    status, want, interface = None, None, ''
    if instance.get('type') != 'virtual-machine':
        raise RuntimeError('VPN target is not a VM')
    stopped = instance.get('status') == 'Stopped'
    if instance.get('status') != 'Running':
        if prestart and verb == 'up' and stopped:
            pass
        elif verb == 'down' and stopped and (ADMIN or (not device and not instance['config'].get(KEY) and instance['config'].get(STARTUP) in ('pending', 'disabled'))):
            emit_native_plan(verb, instance, device, status=dict(ready=False, running=False, enabled=False))
            return
        else:
            raise RuntimeError('start the dedicated VPN yard before changing its service')
    if verb == 'up':
        if not prestart:
            status = runtime_status(instance)
        _, interface, _, want = endpoint(instance)
        collisions(want)
        if ADMIN and (status is None or not status.get('management_ready')):
            raise RuntimeError('administrative SSH environment is not ready; rerun init --profile amnezia')
        if prestart:
            if device or instance['config'].get(KEY):
                raise RuntimeError('first-start VPN activation requires no existing ingress')
        else:
            projects = guest('find', '/srv/workspaces', '-mindepth', '1', '-maxdepth', '1', '-print', '-quit')
            if projects.stdout:
                raise RuntimeError('VPN cannot run in a yard containing work projects')
    elif not ADMIN and not device and not instance['config'].get(KEY):
        status = runtime_status(instance)
    emit_native_plan(verb, instance, device, status, want, interface, prestart)


def shutdown_guest():
    if ADMIN:
        return
    try:
        # Leave room in the engine's 30-second rollback for owner-side verification.
        guest('python3', RUNTIME, 'down', timeout=20)
    except (OSError, RuntimeError, subprocess.TimeoutExpired) as error:
        raise RuntimeError('owned VPN ingress closed; guest shutdown is unverified; retry vpn down') from error


def main():
    args = sys.argv[1:]
    if args in ([], ['help'], ['--help'], ['-h']):
        print('Usage: yard -Y <vpn-yard> ' + ('vpn-admin' if ADMIN else 'vpn') + ' <up|status|down>')
        return 0
    if len(args) != 1 or args[0] not in ('up', 'down', 'status', 'is-up', 'rollback-ingress'):
        print('vpn: expected up, status or down', file=sys.stderr)
        return 2
    if os.environ.get('SUBYARD_ENGINE_CONTEXT') != '1' or not YARD or not PROJECT:
        raise RuntimeError('typed owner context is required')
    verb = args[0]
    mode = os.environ.get('SUBYARD_RESOURCE_MODE', '')
    if mode in ('prepare', 'prepare-start', 'verify') and verb != 'rollback-ingress':
        prepare(verb, mode == 'prepare-start')
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
        print(json.dumps(dict(ready=ready(instance), network_enabled=status['enabled'],
                              ingress=bool(owned(instance)), vpn_installed=status.get('vpn_installed', False),
                              vpn_running=status.get('vpn_running', False),
                              management=dict(application='AmneziaVPN', host=os.environ.get('RESOURCE_VPN_IPV4', ''),
                                              port=int(os.environ.get('RESOURCE_VPN_ADMIN_PORT', '2226')), user='amnezia'),
                              vpn_port=int(os.environ.get('RESOURCE_VPN_PORT', '51820')))))
    elif verb == 'down':
        instance = inspect()
        if instance.get('status') == 'Stopped' and not owned(instance) and not instance['config'].get(KEY) and instance['config'].get(STARTUP) in ('pending', 'disabled'):
            pass
        else:
            remove_ingress(retain_pending=True)
            shutdown_guest()
            remove_ingress()
    else:
        settings_valid()
        instance = inspect()
        address, _, port, want = endpoint(instance)
        collisions(want)
        if not ADMIN:
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
