#!/usr/bin/env python3
"""Bind the profile's native runtime and exact owner dashboard route together."""
import hashlib
import io
import json
import os
import re
from pathlib import Path
import subprocess
import sys
import tarfile

ROOT = Path(__file__).absolute().parent
ENV = os.environ
PHASE = 'arguments'
KEY = 'user.subyard.openlit_dashboard'
DEVICE = 'openlit-dashboard'
DEFERRED = 'user.subyard.deferred_publication.'+DEVICE
GUEST_SCRIPT = 'stage="$(mktemp -d /tmp/subyard-openlit.XXXXXX)"; trap \'rm -rf -- "$stage"\' EXIT; tar -xf - -C "$stage"; bash "$stage/guest.sh" "$@"'
# Cold apply retains the existing guest budgets: Compose config240 + pull240 +
# startup240 + unit90 + guard/publication60 + lock5 + transport25 =900 seconds.
GUEST_TIMEOUTS = {'apply':900, 'refresh':110, 'observe':30, 'pause':120, 'resume':120}
FILES = ('guest.sh', 'native.py', 'runtime.py', 'trace_cutover.py', 'account.py', 'compose.yaml', 'collector.yaml',
         'openlit-collector.yaml', 'subyard-openlit.service', 'subyard-openlit-trace-cutover.service', 'subyard-openlit-trace-cutover.timer')


def run(*args, input=None, timeout=240):
    try:
        return subprocess.run(args, input=input, capture_output=True, check=True, timeout=timeout).stdout
    except (subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
        # Guest command output may be private; project only static stage markers.
        stderr = error.stderr or b''
        if isinstance(stderr, bytes): stderr = stderr.decode(errors='replace')
        stages = re.findall(r'^openlit: guest stage=(guard|image-inventory|image-pull|assets|native-disable|compose-start|account-bootstrap|unit-start|native-enable|publication)$', stderr, re.MULTILINE)
        if stages: print('openlit: guest progress='+stages[-1], file=sys.stderr)
        pulls = re.findall(r'^openlit: pinned image pull exit=([1-9][0-9]{0,2})$', stderr, re.MULTILINE)
        if pulls: print('openlit: guest image-pull exit='+pulls[-1], file=sys.stderr)
        # Only fixed public diagnostics cross the guest boundary; never relay raw stderr.
        for tool in ('Docker Engine', 'Docker Compose'):
            diagnostic = 'openlit: '+tool+' is missing; run yard init to reconcile core provisioning, then retry OpenLIT enable'
            if diagnostic in stderr.splitlines(): print(diagnostic, file=sys.stderr)
        raise


def incus(*args):
    return run('incus', *args, '--project', ENV['INCUS_PROJECT'])


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def teardown_deletes_instance():
    # Generic teardown owns exact instance deletion; hooks must preserve the
    # approved config/device identity until that boundary deletes its routes.
    if ENV.get('SUBYARD_TEARDOWN_KEEP_DATA') not in ('0', '1'):
        return False
    inventory = json.loads(ENV.get('SUBYARD_TEARDOWN_INVENTORY', '[]'))
    return any(item.get('kind') == 'instance' and item.get('name') == ENV['YARD_INSTANCE_NAME']
               and re.fullmatch(r'[a-f0-9]{64}', item.get('binding', '')) for item in inventory)


def owned_route(snapshot):
    config, devices = snapshot.get('config', {}), snapshot.get('devices', {})
    marker, device, saved = config.get(KEY, ''), devices.get(DEVICE), config.get(DEFERRED, '')
    expected = None
    if any(snapshot.get('expanded_config', config).get(key, '') != config.get(key, '') for key in (KEY, DEFERRED)) or snapshot.get('expanded_devices', devices).get(DEVICE) != device:
        raise ValueError('inherited or ambiguous dashboard ownership')
    if marker:
        version, value = marker.split(':', 1)
        value = value.removeprefix('pending:')
        host, port = ('127.0.0.1', value) if version == 'v1' else value.rsplit(':', 1)
        import ipaddress
        if version not in ('v1', 'v2') or (version == 'v2' and ipaddress.ip_address(host) not in ipaddress.ip_network('100.64.0.0/10')) or not port.isdecimal() or str(int(port)) != port or not 1024 <= int(port) <= 65535:
            raise ValueError('invalid dashboard ownership receipt')
        expected = {'type': 'proxy', 'bind': 'host', 'listen': f'tcp:{host}:{port}', 'connect': 'tcp:127.0.0.1:3000'}
        if device is not None and device != expected:
            raise ValueError('foreign or divergent dashboard device')
    elif device is not None:
        raise ValueError('foreign dashboard device')
    if saved:
        def unique(pairs):
            result = {}
            for key, value in pairs:
                if key in result: raise ValueError('duplicate deferred publication field')
                result[key] = value
            return result
        deferred = json.loads(saved, object_pairs_hook=unique)
        if (not marker or version != 'v2' or not isinstance(deferred, dict)
            or set(deferred) != {'version','ownership','device'} or type(deferred['version']) is not int
            or deferred['version'] != 1 or deferred['ownership'] != marker or deferred['device'] != expected):
            raise ValueError('invalid deferred dashboard ownership')
    return marker, device, saved, expected


def route(selected):
    snapshot = json.loads(run('incus', 'query', f"/1.0/instances/{ENV['YARD_INSTANCE_NAME']}?project={ENV['INCUS_PROJECT']}"))
    marker, device, saved, expected = owned_route(snapshot)
    host = '127.0.0.1'
    if selected and ENV.get('YARD_KIND', 'container') != 'vm':
        host = run('bash', '-c', '. "$SUBYARD_REPOSITORY_ROOT/scripts/lib/owner-address.sh"; subyard_owner_host').decode().strip()
        if saved:
            host = expected['listen'].split(':')[1]
    port = ENV.get('OPENLIT_HOST_PORT', '')
    if selected and (not port.isdecimal() or not 1024 <= int(port) <= 65535):
        raise ValueError('OPENLIT_HOST_PORT must be within 1024..65535')
    desired = None
    receipt = ''
    if selected and ENV.get('YARD_KIND', 'container') != 'vm':
        desired = {'type': 'proxy', 'bind': 'host', 'listen': f'tcp:{host}:{port}', 'connect': 'tcp:127.0.0.1:3000'}
        receipt = f'v1:{port}' if host == '127.0.0.1' else f'v2:{host}:{port}'
        # Reserve the exact tuple against every other instance's inherited routes.
        instances = json.loads(run('incus', 'list', '--all-projects', '--format', 'json'))
        for instance in instances:
            same_instance = (instance.get('project') == ENV['INCUS_PROJECT']
                             and instance.get('name') == ENV['YARD_INSTANCE_NAME'])
            for name, peer in instance.get('expanded_devices', instance.get('devices', {})).items():
                if same_instance and name == DEVICE:
                    continue
                if peer.get('type') == 'proxy' and peer.get('listen') == desired['listen']:
                    raise ValueError('dashboard listener is reserved by another device')
            # A detached route still owns its exact future listener. Accept
            # only this profile's validated local marker/journal provenance.
            peer_config = instance.get('expanded_config', instance.get('config', {}))
            if not same_instance and peer_config.get(DEFERRED):
                _, _, _, peer = owned_route(instance)
                if peer['listen'] == desired['listen']:
                    raise ValueError('dashboard listener is reserved by a deferred device')
        if device != desired and not saved:
            import socket
            with socket.socket() as probe:
                probe.bind((host, int(port)))
    actual = {'marker': marker, 'device': device}
    if saved: actual['deferred'] = saved
    wanted = actual if not selected and teardown_deletes_instance() else {'marker': receipt, 'device': desired}
    if selected and saved and receipt == marker.replace(':pending:', ':', 1):
        wanted = actual
    elif selected and saved:
        raise ValueError('deferred listener recipe changed; cancel the owned route first')
    return actual, wanted, f'http://{host}:{port}'


def guest(mode, selected, user, agents, origin, *bindings):
    global PHASE
    PHASE = 'guest-'+mode
    stream = io.BytesIO()
    with tarfile.open(fileobj=stream, mode='w') as archive:
        for name in FILES:
            archive.add(ROOT/name, arcname=name, recursive=False)
        archive.add(ROOT.parents[2]/'internal/adapters/configmaterial/tomli_w.py', arcname='tomli_w.py', recursive=False)
    output = run('incus', 'exec', ENV['YARD_INSTANCE_NAME'], '--project', ENV['INCUS_PROJECT'], '--',
                 'bash', '-euo', 'pipefail', '-c', GUEST_SCRIPT,
                 'subyard', mode, str(int(selected)), user, agents, *bindings, input=stream.getvalue(), timeout=GUEST_TIMEOUTS[mode])
    return output.decode().strip()


def observe(selected, user, agents):
    actual, desired, origin = route(selected)
    native = json.loads(guest('observe', selected, user, agents, origin))
    current = actual == desired and native['state'] in ('current', 'absent') and (not selected or native['state'] == 'current')
    sources = {name: hashlib.sha256((ROOT/name).read_bytes()).hexdigest() for name in ('owner.sh', 'runtime.sh', 'dashboard.py')}
    wanted = digest({'source': sources, 'route': desired, 'runtime': native['desired'], 'origin': origin if selected else ''})
    state = 'current' if current else 'stale'
    if current and not selected and native['state'] == 'absent':
        return {'state': 'absent', 'actual': '', 'desired': ''}, native, actual, desired, origin
    return {'state': state, 'actual': wanted if current else digest({'route': actual, 'runtime': native}), 'desired': wanted}, native, actual, desired, origin


def publish(actual, desired):
    if actual == desired:
        return
    instance = ENV['YARD_INSTANCE_NAME']
    if actual.get('deferred'):
        # Native unset invalidates an in-flight core restore's ETag. A restore
        # committed before revocation is caught by the fresh ownership read.
        incus('config', 'unset', instance, DEFERRED)
        fresh, _, _ = route(False)
        if fresh['marker'] != actual['marker'] or fresh.get('deferred'):
            raise ValueError('dashboard ownership changed during cancellation')
        actual = fresh
    if actual['device'] is not None:
        incus('config', 'device', 'remove', instance, DEVICE)
    if desired['device'] is not None:
        marker = desired['marker']
        version, value = marker.split(':', 1)
        incus('config', 'set', instance, KEY, version+':pending:'+value)
        incus('config', 'device', 'add', instance, DEVICE, 'proxy', 'bind=host', 'listen='+desired['device']['listen'], 'connect=tcp:127.0.0.1:3000')
        incus('config', 'set', instance, KEY, marker)
    elif actual['marker']:
        incus('config', 'unset', instance, KEY)


def main():
    mode, selected, user, agents, *bindings = sys.argv[1:]
    selected = selected == '1'
    global PHASE
    if mode in ('pause', 'resume'):
        PHASE = 'existing-route'
        # Service lifecycle runs under elevated teardown context, which carries
        # existing identity but need not carry a desired listener recipe.
        _, _, origin = route(False)
        PHASE = 'guest-'+mode
        output = guest(mode, selected, user, agents, origin)
        if output: print(output)
        return
    PHASE = 'observe'
    observation, native, actual, desired, origin = observe(selected, user, agents)
    if mode == 'observe':
        print(json.dumps(observation)); return
    if mode == 'check':
        # An exact deferred publication is converged service state. The owning
        # receipt remains intact; status separately withholds the unavailable URL.
        if observation['state'] not in ('current', 'absent'):
            raise ValueError('dashboard or native runtime requires reconciliation')
        return
    if mode == 'refresh' and bindings != [observation['actual'], observation['desired']]:
        raise ValueError('dashboard/runtime assessment changed')
    # Recheck owned route before changing guest assets, and publish only after readiness.
    if native['state'] == 'stale':
        guest('refresh' if mode == 'refresh' else 'apply', selected, user, agents, origin, native['actual'], native['desired'])
    fresh, wanted, _ = route(selected)
    if fresh != actual or wanted != desired:
        raise ValueError('dashboard assessment changed during guest reconciliation')
    publish(actual, desired)
    verified, *_ = observe(selected, user, agents)
    if verified['state'] not in ('current', 'absent'):
        raise ValueError('dashboard verification failed')
    if mode == 'refresh': print(json.dumps(verified))
    elif selected and desired.get('deferred'): print('openlit: native telemetry ready; dashboard publication deferred')
    else: print('openlit: native telemetry and dashboard are ready')


if __name__ == '__main__':
    try:
        main()
    except (ValueError, KeyError, OSError, subprocess.SubprocessError) as error:
        diagnostic = 'openlit: failure stage='+PHASE+' category='+type(error).__name__
        if isinstance(error, subprocess.TimeoutExpired): diagnostic += ' timeout_seconds='+str(error.timeout)
        if isinstance(error, subprocess.CalledProcessError): diagnostic += ' exit='+str(error.returncode)
        print(diagnostic, file=sys.stderr)
        print('openlit: dashboard/runtime conflict or failed verification; retry a fresh assessment', file=sys.stderr)
        sys.exit(1)
