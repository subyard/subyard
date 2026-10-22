#!/usr/bin/env python3
"""Bind the profile's native runtime and exact owner dashboard route together."""
import hashlib
import io
import inspect
import json
import os
import re
import runpy
import selectors
import signal
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import time

ROOT = Path(__file__).absolute().parent
OWNER_BARRIER = ROOT.parents[2]/'scripts/lib/native-owner-barrier.py'
ENV = os.environ
PHASE = 'arguments'
KEY = 'user.subyard.openlit_dashboard'
DEVICE = 'openlit-dashboard'
DEFERRED = 'user.subyard.deferred_publication.'+DEVICE
GUEST_SCRIPT = 'stage="$(mktemp -d /tmp/subyard-openlit.XXXXXX)"; trap \'rm -rf -- "$stage"\' EXIT; tar -xf - -C "$stage"; bash "$stage/guest.sh" "$@"'
COLD_GUEST_SCRIPT = 'stage=$1; shift; tar -xf - -C "$stage"; bash "$stage/guest.sh" "$@"'
# Cold apply shares one deadline across archive delivery, first pinned delivery
# (900 seconds), the remaining guest phases and host publication.
GUEST_TIMEOUTS = {'apply':1560, 'refresh':110, 'observe':30, 'pause':120, 'resume':120}
COLD_CANCEL_SECONDS = 25
COLD_KILL_SECONDS = 5
COLD_DEADLINE = None
FILES = ('guest.sh', 'native.py', 'runtime.py', 'trace_cutover.py', 'account.py', 'compose.yaml', 'collector.yaml',
         'openlit-collector.yaml', 'subyard-openlit.service', 'subyard-openlit-trace-cutover.service', 'subyard-openlit-trace-cutover.timer')


def guest_error(error):
    # Guest command output may be private; project only static stage markers.
    stderr = error.stderr or b''
    if isinstance(stderr, bytes): stderr = stderr.decode(errors='replace')
    stages = re.findall(r'^openlit: guest stage=(guard|image-inventory|image-pull|assets|native-disable|compose-start|account-bootstrap|unit-start|native-enable|publication)$', stderr, re.MULTILINE)
    if stages: print('openlit: guest progress='+stages[-1], file=sys.stderr)
    if stages and stages[-1] == 'compose-start':
        reason = 'unknown'
        for category, pattern in (
                ('wait-deadline', r'^(?:application not healthy after [0-9a-z.]+|timed out waiting for .+)$'),
                ('unhealthy', r'^(?:dependency failed to start: )?container .+ is unhealthy$'),
                ('dependency-start', r'^dependency failed to start: .+$')):
            if re.search(pattern, stderr[-65536:], re.MULTILINE):
                reason = category
                break
        print('openlit: compose-start reason='+reason, file=sys.stderr)
    pulls = re.findall(r'^openlit: pinned image pull exit=([1-9][0-9]{0,2})$', stderr, re.MULTILINE)
    if pulls: print('openlit: guest image-pull exit='+pulls[-1], file=sys.stderr)
    for tool in ('Docker Engine', 'Docker Compose'):
        diagnostic = 'openlit: '+tool+' is missing; run yard init to reconcile core provisioning, then retry OpenLIT enable'
        if diagnostic in stderr.splitlines(): print(diagnostic, file=sys.stderr)


def owner_descriptors():
    if not os.environ.get('SUBYARD_NATIVE_OWNER_FD3'):
        return ()
    return runpy.run_path(str(OWNER_BARRIER))['inherited_owner_descriptors']()


def run(*args, input=None, timeout=240):
    if COLD_DEADLINE is not None:
        timeout = min(timeout, COLD_DEADLINE-time.monotonic())
        if timeout <= 0: raise subprocess.TimeoutExpired(args, 0)
    try:
        return subprocess.run(args, input=input, capture_output=True, check=True, timeout=timeout,
                              pass_fds=owner_descriptors()).stdout
    except (subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
        guest_error(error)
        raise


def supervise_cold(script, argv):
    # The framed archive has its own EOF. The still-open transport pipe is
    # exclusively a caller-liveness channel once that archive is consumed.
    child = None
    cancelled = False
    def cancel(signum, frame):
        nonlocal cancelled
        cancelled = True
    signal.signal(signal.SIGTERM, cancel)
    signal.signal(signal.SIGINT, cancel)
    with selectors.DefaultSelector() as reader, tempfile.TemporaryFile(dir='/tmp') as archive, tempfile.TemporaryDirectory(prefix='subyard-openlit.', dir='/tmp') as stage:
        os.fchmod(archive.fileno(), 0o600)
        # The supervisor remains outside the cancelled Bash group, so even a
        # stopped leader killed before its EXIT trap can run cannot leak assets.
        os.chmod(stage, 0o700)
        reader.register(0, selectors.EVENT_READ)
        header = b''
        header_deadline = time.monotonic()+1560
        while not header.endswith(b'\n') and len(header) < 80:
            if cancelled or time.monotonic() >= header_deadline: sys.exit(124)
            if not reader.select(min(0.1, max(0, header_deadline-time.monotonic()))): continue
            part = os.read(0, 1)
            if not part: sys.exit(124)
            header += part
        budget, grace, size = header.decode('ascii').split()
        budget, grace, size = float(budget), float(grace), int(size)
        if not (0 < budget <= 1560 and 0 < grace <= 5 and 0 <= size <= 16*1024*1024):
            sys.exit(124)
        deadline = time.monotonic()+budget
        while size:
            if cancelled or time.monotonic() >= deadline: sys.exit(124)
            if not reader.select(min(0.1, max(0, deadline-time.monotonic()))): continue
            part = os.read(0, min(size, 65536))
            if not part: sys.exit(124)
            archive.write(part); size -= len(part)
        archive.seek(0)
        child = subprocess.Popen(['bash', '-euo', 'pipefail', '-c', script, argv[0], stage, *argv[1:]],
                                 stdin=archive, start_new_session=True)
        status = 124
        try:
            while not cancelled and time.monotonic() < deadline:
                # Leave the owned leader unreaped until its whole group is
                # killed; an exited leader must not hide a surviving child.
                result = os.waitid(os.P_PID, child.pid, os.WEXITED|os.WNOHANG|os.WNOWAIT)
                if result:
                    status = result.si_status if result.si_code == os.CLD_EXITED else -result.si_status
                    break
                if reader.select(min(0.1, max(0, deadline-time.monotonic()))):
                    if not os.read(0, 1): break
            if status == 124:
                try: os.killpg(child.pid, signal.SIGTERM)
                except ProcessLookupError: pass
                end = time.monotonic()+grace
                while time.monotonic() < end: time.sleep(max(0, min(0.05, end-time.monotonic())))
        finally:
            try: os.killpg(child.pid, signal.SIGKILL)
            except ProcessLookupError: pass
            child.wait(timeout=1)
        sys.exit(status if status >= 0 else 128-status)


def cold_run(*args, input, timeout, deadline):
    remaining = deadline-time.monotonic()-COLD_CANCEL_SECONDS-COLD_KILL_SECONDS
    if remaining <= 0: raise subprocess.TimeoutExpired(args, timeout)
    framed = memoryview(f'{remaining} {COLD_KILL_SECONDS} {len(input)}\n'.encode()+input)
    child = subprocess.Popen(args, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                             stderr=subprocess.PIPE, bufsize=0, start_new_session=True,
                             pass_fds=owner_descriptors())
    output, errors = bytearray(), bytearray()
    previous = {s: signal.getsignal(s) for s in (signal.SIGINT, signal.SIGTERM)}
    def cancel(signum, frame): raise SystemExit(128+signum)
    for s in previous: signal.signal(s, cancel)
    status = None
    try:
        with selectors.DefaultSelector() as pipes:
            for pipe in (child.stdin, child.stdout, child.stderr): os.set_blocking(pipe.fileno(), False)
            pipes.register(child.stdin, selectors.EVENT_WRITE)
            pipes.register(child.stdout, selectors.EVENT_READ, output)
            pipes.register(child.stderr, selectors.EVENT_READ, errors)
            while True:
                result = os.waitid(os.P_PID, child.pid, os.WEXITED|os.WNOHANG|os.WNOWAIT)
                if result: status = result.si_status if result.si_code == os.CLD_EXITED else -result.si_status
                if status is not None and not any(k.events == selectors.EVENT_READ for k in pipes.get_map().values()): break
                now = time.monotonic()
                if now >= deadline-COLD_CANCEL_SECONDS-COLD_KILL_SECONDS and not child.stdin.closed:
                    if child.stdin in [k.fileobj for k in pipes.get_map().values()]: pipes.unregister(child.stdin)
                    child.stdin.close()
                if now >= deadline-COLD_KILL_SECONDS:
                    raise subprocess.TimeoutExpired(args, timeout, bytes(output), bytes(errors))
                for key, events in pipes.select(min(0.1, deadline-COLD_KILL_SECONDS-now)):
                    if events & selectors.EVENT_WRITE:
                        try: count = os.write(key.fd, framed[:65536])
                        except BrokenPipeError:
                            pipes.unregister(key.fileobj); key.fileobj.close(); continue
                        framed = framed[count:]
                        if not framed: pipes.unregister(key.fileobj)
                    else:
                        part = os.read(key.fd, 65536)
                        if part: key.data.extend(part)
                        else: pipes.unregister(key.fileobj)
        if status:
            raise subprocess.CalledProcessError(status, args, bytes(output), bytes(errors))
        return bytes(output)
    except (subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
        guest_error(error)
        raise
    finally:
        # EOF asks the guest supervisor to cancel its group, before falling
        # back to terminating the local transport within the same deadline.
        for s in previous: signal.signal(s, signal.SIG_IGN)
        try:
            if not child.stdin.closed: child.stdin.close()
            end = min(time.monotonic()+COLD_CANCEL_SECONDS, deadline-COLD_KILL_SECONDS)
            while status is None and time.monotonic() < end:
                result = os.waitid(os.P_PID, child.pid, os.WEXITED|os.WNOHANG|os.WNOWAIT)
                if result: status = result.si_status
                else: time.sleep(max(0, min(0.01, end-time.monotonic())))
            if status is None:
                try: os.killpg(child.pid, signal.SIGTERM)
                except ProcessLookupError: pass
                end = min(time.monotonic()+COLD_KILL_SECONDS, deadline)-min(1,COLD_KILL_SECONDS/5)
                while time.monotonic() < end:
                    if os.waitid(os.P_PID, child.pid, os.WEXITED|os.WNOHANG|os.WNOWAIT): break
                    time.sleep(max(0,min(0.01,end-time.monotonic())))
            try: os.killpg(child.pid, signal.SIGKILL)
            except ProcessLookupError: pass
            child.wait(timeout=max(0, min(1, deadline-time.monotonic())))
        finally:
            child.stdout.close(); child.stderr.close()
            for s, handler in previous.items(): signal.signal(s, handler)


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
        # Reservation needs configuration, not live state, snapshots or backups.
        instances = json.loads(run('incus', 'query', '/1.0/instances?recursion=1&all-projects=true'))
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
    deadline = time.monotonic()+GUEST_TIMEOUTS[mode]
    if mode == 'apply' and COLD_DEADLINE is not None: deadline = min(deadline, COLD_DEADLINE)
    stream = io.BytesIO()
    with tarfile.open(fileobj=stream, mode='w') as archive:
        for name in FILES:
            archive.add(ROOT/name, arcname=name, recursive=False)
        archive.add(ROOT.parents[2]/'internal/adapters/configmaterial/tomli_w.py', arcname='tomli_w.py', recursive=False)
    transport = ('incus', 'exec', ENV['YARD_INSTANCE_NAME'], '--project', ENV['INCUS_PROJECT'], '--')
    arguments = ('subyard', mode, str(int(selected)), user, agents, *bindings)
    if mode in ('apply', 'refresh'):
        program = 'import os,selectors,signal,subprocess,sys,tempfile,time\n'+inspect.getsource(supervise_cold)+'\nsupervise_cold(sys.argv[1],sys.argv[2:])\n'
        output = cold_run(*transport, 'python3', '-c', program, COLD_GUEST_SCRIPT, *arguments,
                          input=stream.getvalue(), timeout=GUEST_TIMEOUTS[mode], deadline=deadline)
    else:
        output = run(*transport, 'bash', '-euo', 'pipefail', '-c', GUEST_SCRIPT, *arguments,
                     input=stream.getvalue(), timeout=GUEST_TIMEOUTS[mode])
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


def operation():
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
    if mode == 'refresh' and verified['desired'] != bindings[1]:
        raise ValueError('dashboard/runtime desired assessment changed during refresh')
    if mode == 'refresh': print(json.dumps(verified))
    elif selected and desired.get('deferred'): print('openlit: native telemetry ready; dashboard publication deferred')
    else: print('openlit: native telemetry and dashboard are ready')


def main():
    global COLD_DEADLINE
    previous = COLD_DEADLINE
    # Only the original cold caller shares this deadline, including the fresh
    # host route read, publication and native verification after the guest.
    if sys.argv[1] == 'apply': COLD_DEADLINE = time.monotonic()+GUEST_TIMEOUTS['apply']
    try: return operation()
    finally: COLD_DEADLINE = previous


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
