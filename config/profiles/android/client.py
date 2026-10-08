#!/usr/bin/env python3
"""Unprivileged Android lease client; the same client runs in L1, L2 and on the owner."""
import argparse
import contextlib
import json
import os
from pathlib import Path
import re
import secrets
import select
import shutil
import signal
import socket
import socketserver
import stat
import subprocess
import sys
import tempfile
import threading
import time

CONTROL = os.environ.get('ANDROID_PUBLIC_ROOT', '/srv/cache/android-sdk/.subyard') + '/control.sock'
TTL = 1200
HEARTBEAT = 300
VIEWER_START_TIMEOUT = 60


class Error(Exception):
    def __init__(self, code, message):
        self.code = code
        super().__init__(message)


class AdbFailure(Error):
    pass


def line(stream):
    data = bytearray()
    while not data.endswith(b'\n'):
        part = stream.recv(1) if isinstance(stream, socket.socket) else stream.read(1)
        if not part or len(data) >= 1024 * 1024:
            raise Error('transport', 'Android pool response interrupted or oversized; verify the selected yard is running and its owner runtime is current')
        data.extend(part)
    return json.loads(data)


def transport():
    if command := os.environ.get('SUBYARD_RESOURCE_SESSION_TRANSPORT'):
        return json.loads(command)
    if os.environ.get('SUBYARD_EMU_INSTANCE'):
        return ['incus', 'exec', os.environ['SUBYARD_EMU_INSTANCE'], '--project',
                os.environ['SUBYARD_EMU_PROJECT'], '--', 'python3',
                '/usr/local/lib/subyard-android/client.py', '_wire']
    return None


def connection(request, timeout=400):
    command = transport()
    if command:
        process = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE)
        process.stdin.write(json.dumps(request).encode() + b'\n')
        process.stdin.flush()
        return process
    stream = socket.socket(socket.AF_UNIX)
    stream.settimeout(timeout)
    try:
        stream.connect(CONTROL)
        stream.sendall(json.dumps(request).encode() + b'\n')
    except OSError:
        stream.close()
        raise
    return stream


def response(channel):
    result = line(channel if isinstance(channel, socket.socket) else channel.stdout)
    if not result.get('ok'):
        raise Error(result.get('error', 'transport'), result.get('message', 'Android pool request failed'))
    return result.get('result')


def close(channel):
    if isinstance(channel, socket.socket):
        channel.close()
    else:
        channel.stdin.close()
        channel.stdout.close()
        if channel.poll() is None:
            channel.terminate()
        try:
            channel.wait(timeout=3)
        except subprocess.TimeoutExpired:
            channel.kill()
            channel.wait()


def rpc(operation, timeout=400, **values):
    channel = connection(dict(operation=operation, **values), timeout)
    try:
        return response(channel)
    finally:
        close(channel)


def bridge(local, channel):
    """Each authenticated stream is tied to the lease; owner drain closes both ends."""
    remote_read = channel if isinstance(channel, socket.socket) else channel.stdout
    def copy_to_remote():
        try:
            while data := local.recv(65536):
                if isinstance(channel, socket.socket):
                    channel.sendall(data)
                else:
                    channel.stdin.write(data)
                    channel.stdin.flush()
        except (OSError, ValueError):
            pass
        finally:
            with contextlib.suppress(OSError, ValueError):
                if isinstance(channel, socket.socket):
                    channel.shutdown(socket.SHUT_WR)
                else:
                    channel.stdin.close()
    worker = threading.Thread(target=copy_to_remote, daemon=True)
    worker.start()
    try:
        while True:
            data = channel.recv(65536) if isinstance(channel, socket.socket) else remote_read.read1(65536)
            if not data:
                break
            local.sendall(data)
    except OSError:
        pass
    finally:
        with contextlib.suppress(OSError):
            local.shutdown(socket.SHUT_RDWR)
        close(channel)
        worker.join(timeout=3)


class Relay(socketserver.ThreadingUnixStreamServer):
    daemon_threads = True
    block_on_close = False


class RelayHandler(socketserver.BaseRequestHandler):
    def handle(self):
        channel = None
        try:
            channel = connection(dict(operation='tunnel', token=self.server.token))
            response(channel)
            if isinstance(channel, socket.socket):
                channel.settimeout(None)
            bridge(self.request, channel)
            channel = None
        except (Error, OSError, ValueError):
            pass
        finally:
            if channel is not None:
                with contextlib.suppress(OSError, ValueError):
                    close(channel)


def adb_read(stream, size):
    data = bytearray()
    while len(data) < size:
        part = stream.recv(size - len(data))
        if not part:
            raise Error('transport', 'viewer ADB connection closed')
        data.extend(part)
    return bytes(data)


def adb_request(stream, command):
    payload = command.encode()
    stream.sendall(f'{len(payload):04x}'.encode() + payload)
    status = adb_read(stream, 4)
    if status == b'FAIL':
        raise AdbFailure('transport', 'viewer ADB service is unavailable')
    if status != b'OKAY':
        raise Error('transport', 'viewer ADB request failed')


class ViewerRelay(socketserver.ThreadingTCPServer):
    daemon_threads = True
    block_on_close = False

    def __init__(self, *args, **kwargs):
        self.clients, self.clients_lock = set(), threading.Lock()
        self.pending, self.setup_order = [], threading.Condition()
        self.closing = False
        super().__init__(*args, **kwargs)

    def process_request(self, request, client_address):
        # scrcpy identifies video/audio/control by connection order, not a header.
        # Record accept order before ThreadingMixIn schedules independent handlers.
        with self.setup_order:
            self.pending.append(request)
        try:
            super().process_request(request, client_address)
        except Exception:
            with self.setup_order:
                self.pending.remove(request)
                self.setup_order.notify_all()
            raise

    @contextlib.contextmanager
    def setup_turn(self, request):
        with self.setup_order:
            self.setup_order.wait_for(lambda: self.closing or self.pending[0] is request)
        try:
            if self.closing:
                raise Error('transport', 'viewer connection closed')
            yield
        finally:
            with self.setup_order:
                self.pending.remove(request)
                self.setup_order.notify_all()

    def server_close(self):
        super().server_close()
        with self.clients_lock:
            self.closing = True
            for stream in self.clients:
                with contextlib.suppress(OSError):
                    stream.shutdown(socket.SHUT_RDWR)
        with self.setup_order:
            self.setup_order.notify_all()


class ViewerHandler(socketserver.BaseRequestHandler):
    def handle(self):
        channel = None
        stage = 'forward lookup'
        started = time.monotonic()
        with self.server.clients_lock:
            self.server.clients.add(self.request)
        try:
            # scrcpy's ADB forward lives in the remote runtime namespace. Resolve its
            # exact device socket and carry media through the existing lease facade.
            # TCP connect returns before this handler runs. scrcpy may remove the
            # forward after opening audio/control, so retain the first verified target.
            with self.server.setup_turn(self.request):
                if self.server.target is None:
                    with socket.socket(socket.AF_UNIX) as query:
                        query.settimeout(10)
                        query.connect(self.server.endpoint)
                        adb_request(query, 'host:list-forward')
                        rows = adb_read(query, int(adb_read(query, 4), 16)).decode().splitlines()
                    prefix = [self.server.serial, f'tcp:{self.server.server_address[1]}']
                    targets = [parts[2] for row in rows if len(parts := row.split()) == 3
                               and parts[:2] == prefix
                               and re.fullmatch(r'localabstract:scrcpy_[0-9a-f]{8}', parts[2])]
                    if len(targets) != 1:
                        raise Error('transport', 'viewer forward was not found')
                    self.server.target = targets[0]
                    self.server.startup_deadline = time.monotonic() + VIEWER_START_TIMEOUT
                stage = 'server socket'
                retry_delay = 0.1
                while True:
                    remaining = self.server.startup_deadline - time.monotonic()
                    if self.server.closing or (not self.server.media_started and remaining <= 0):
                        raise Error('transport', 'viewer server socket did not become ready')
                    channel = socket.socket(socket.AF_UNIX)
                    channel.settimeout(10 if self.server.media_started else min(10, remaining))
                    channel.connect(self.server.endpoint)
                    stage = 'device transport'
                    adb_request(channel, 'host:transport:' + self.server.serial)
                    stage = 'server socket'
                    try:
                        adb_request(channel, self.server.target)
                        break
                    except AdbFailure:
                        close(channel)
                        channel = None
                        # A cold guest may not have opened scrcpy's device socket yet.
                        # Only its explicit FAIL is retryable; transport/EOF errors
                        # and failures on later media streams remain terminal.
                        remaining = self.server.startup_deadline - time.monotonic()
                        if self.server.media_started or self.server.closing or remaining <= 0:
                            raise
                        readable, _, _ = select.select([self.request], [], [], min(retry_delay, remaining))
                        retry_delay = min(1, retry_delay * 2)
                        if readable and not self.request.recv(1, socket.MSG_PEEK):
                            raise Error('transport', 'viewer connection closed')
                if not self.server.media_started:
                    stage = 'initial media byte'
                    remaining = self.server.startup_deadline - time.monotonic()
                    if remaining <= 0:
                        raise Error('transport', 'viewer server socket did not become ready')
                    channel.settimeout(remaining)
                    # ADB accepting the socket is not scrcpy readiness. Its initial
                    # dummy byte must arrive within the same window and reach the
                    # caller unchanged before later media streams can start.
                    self.request.sendall(adb_read(channel, 1))
                    self.server.media_started = True
            channel.settimeout(None)
            bridge(self.request, channel)
        except (Error, OSError, ValueError) as exc:
            # Report our bounded failure after scrcpy exits, without logging socket
            # paths, request payloads, lease credentials or arbitrary OS messages.
            if not self.server.closing and self.server.failure is None:
                detail = str(exc) if isinstance(exc, Error) else type(exc).__name__
                self.server.failure = f'{stage} after {time.monotonic() - started:.1f}s: {detail}'
        finally:
            if channel is not None:
                close(channel)
            with self.server.clients_lock:
                self.server.clients.discard(self.request)


def start_viewer_relay(endpoint, serial):
    server = ViewerRelay(('127.0.0.1', 0), ViewerHandler)
    server.endpoint, server.serial = str(endpoint), serial
    server.target = None
    server.media_started = False
    server.failure = None
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


def read_lease(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd) as source:
        info = os.fstat(source.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
            raise Error('credential', 'lease file must be a private regular file owned by the caller')
        return json.load(source)


def save_lease(path, value):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'w') as output:
        json.dump(value, output)
        output.flush()
        os.fsync(output.fileno())


def attribution(args):
    # Display labels never authorize a lease. Caller can explicitly select context outside a workspace.
    project = args.project or os.environ.get('SUBYARD_PROJECT_ID') or Path.cwd().name
    yard = args.yard or os.environ.get('SUBYARD_YARD_NAME') or os.environ.get('YARD_NAME', 'unknown')
    for directory in (Path.cwd(), *Path.cwd().parents):
        metadata = directory / '.subyard-meta.json'
        if metadata.is_file():
            try:
                if metadata.is_symlink() or metadata.stat().st_size > 65536:
                    raise ValueError('invalid metadata file')
                record = json.loads(metadata.read_text())
                if record.get('schema') != 1 or record.get('projectId') != directory.name:
                    raise ValueError('mismatched workspace identity')
                project = args.project or record.get('name', project)
                yard = args.yard or record.get('yard', yard)
            except (OSError, ValueError):
                raise Error('owner', 'workspace attribution metadata is invalid')
            break
    return dict(yard=yard, project=project, run=secrets.token_hex(8), purpose=args.purpose)


def requester_identity(pid):
    if pid <= 1:
        return None
    try:
        # comm can contain spaces and parentheses; starttime follows its final ')'.
        fields = Path(f'/proc/{pid}/stat').read_text().rsplit(')', 1)[1].split()
        return fields[19] if fields[0] not in ('Z', 'X') else None
    except (OSError, IndexError):
        return None


def acquire(args, token, requester_alive=lambda: True):
    request = {key: value for key in ('device', 'api', 'variant', 'abi')
               if (value := getattr(args, key, None)) is not None}
    deadline = time.monotonic() + args.wait
    owner = attribution(args)
    while True:
        if not requester_alive():
            raise Error('owner', 'requesting session ended')
        try:
            rpc('reserve', token=token, request=request, owner=owner)
            break
        except Error as exc:
            if exc.code != 'busy' or args.wait == 0:
                raise
            if time.monotonic() >= deadline:
                raise Error('timeout', 'timed out waiting for an Android emulator slot') from exc
            time.sleep(min(1, max(0, deadline - time.monotonic())))
    last_renew = args.last_renew = time.monotonic()
    while True:
        if not requester_alive():
            raise Error('owner', 'requesting session ended')
        result = rpc('allocation', token=token)
        if not requester_alive():
            raise Error('owner', 'requesting session ended')
        if time.monotonic() - last_renew >= HEARTBEAT:
            rpc('renew', token=token)
            last_renew = args.last_renew = time.monotonic()
        if result['state'] == 'held':
            return result
        time.sleep(0.5)


def start_relay(token, path):
    server = Relay(str(path), RelayHandler)
    server.token = token
    os.chmod(path, 0o600)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server


def context(allocation, endpoint):
    return dict(ANDROID_SERIAL=allocation['android_serial'], ADB_SERVER_SOCKET='localfilesystem:' + str(endpoint),
                SUBYARD_EMU_SLOT=allocation['slot_id'], SUBYARD_EMU_GENERATION=str(allocation['generation']),
                SUBYARD_EMU_DEVICE=allocation['request']['device'], SUBYARD_EMU_API=str(allocation['request']['api']),
                SUBYARD_EMU_ANDROID_VERSION=allocation['request']['android_version'],
                SUBYARD_EMU_VARIANT=allocation['request']['variant'], SUBYARD_EMU_ABI=allocation['request']['abi'])


def parser():
    result = argparse.ArgumentParser(description='Exclusive Android emulators; profile defaults: phone, API 36, google_apis/x86_64. '
                                     f'Heartbeat {HEARTBEAT}s; lease TTL {TTL}s. See catalog/status for configured defaults.',
                                     epilog='Agents: use run -- COMMAND for automatic renewal and release. '
                                     'To share your screen, acquire --lease-file ABSOLUTE_PATH and renew the lease while working; '
                                     'the operator opens yard -Y OWNER/YARD emu view --lease-file ABSOLUTE_PATH on their laptop. '
                                     'For a remote yard this path is inside the selected yard.')
    commands = result.add_subparsers(dest='verb', required=True)
    commands.add_parser('catalog')
    commands.add_parser('status')
    commands.add_parser('down', help='operator: drain all slots')
    commands.add_parser('revoke', help='operator: revoke one slot').add_argument('--slot', required=True)
    for name in ('run', 'acquire', 'view'):
        command = commands.add_parser(name)
        command.add_argument('--device', choices=('phone', 'tablet'))
        command.add_argument('--api', type=int, choices=(34, 35, 36))
        command.add_argument('--variant', choices=('google_apis', 'google_apis_playstore'))
        command.add_argument('--abi', choices=('x86_64',))
        command.add_argument('--wait', type=int, default=0, metavar='SECONDS')
        command.add_argument('--yard')
        command.add_argument('--project')
        command.add_argument('--purpose', default='android-development')
        if name in ('view', 'acquire'):
            command.add_argument('--lease-file', required=name == 'acquire')
        if name == 'view':
            command.add_argument('--control', action='store_true', help='enable viewer input (default view-only)')
        if name in ('view', 'run'):
            command.add_argument('command', nargs=argparse.REMAINDER)
    for name in ('renew', 'release'):
        commands.add_parser(name).add_argument('--lease-file', required=True)
    cache = commands.add_parser('cache').add_subparsers(dest='cache_verb', required=True)
    cache.add_parser('prune').add_argument('--dry-run', action='store_true')
    prepare = cache.add_parser('prepare')
    prepare.add_argument('--api', type=int, choices=(34, 35, 36), required=True)
    prepare.add_argument('--variant', choices=('google_apis', 'google_apis_playstore'))
    prepare.add_argument('--abi', choices=('x86_64',))
    return result


def relay_daemon(path):
    lease = read_lease(path)
    server = start_relay(lease['token'], lease['endpoint'])
    try:
        while True:
            try:
                rpc('allocation', token=lease['token'])
            except (Error, OSError):
                return
            time.sleep(2)
    finally:
        server.shutdown()
        server.server_close()
        Path(lease['endpoint']).unlink(missing_ok=True)
        with contextlib.suppress(OSError):
            Path(lease['endpoint']).parent.rmdir()


def wire():
    # Owner-side byte transport: first line is private control data, never an argument or log.
    request = json.loads(sys.stdin.buffer.readline(16385))
    if request.get('operation') == 'lease':
        try:
            path = request.get('lease_file', '')
            if not isinstance(path, str) or not Path(path).is_absolute():
                raise Error('credential', 'remote lease file requires an absolute path in the selected yard')
            lease = read_lease(path)
            payload = dict(ok=True, result=dict(token=lease['token']))
        except (OSError, ValueError, KeyError, Error):
            payload = dict(ok=False, error='credential',
                           message='remote lease file must exist in the selected yard and be a private regular file owned by the yard user')
        sys.stdout.buffer.write(json.dumps(payload).encode() + b'\n')
        sys.stdout.buffer.flush()
        return
    stream = socket.socket(socket.AF_UNIX)
    config = json.loads(Path('/etc/subyard-android.json').read_text())
    address = (Path(config['state_root']) / 'admin.sock' if request.get('operation') in ('drain', 'revoke')
               else Path(config['public_root']) / 'control.sock')
    stream.connect(str(address))
    stream.sendall(json.dumps(request).encode() + b'\n')
    if request.get('operation') != 'tunnel':
        payload = line(stream)
        sys.stdout.buffer.write(json.dumps(payload).encode() + b'\n')
        sys.stdout.buffer.flush()
        stream.close()
        return
    payload = line(stream)
    sys.stdout.buffer.write(json.dumps(payload).encode() + b'\n')
    sys.stdout.buffer.flush()
    if not payload.get('ok'):
        stream.close()
        return
    def send():
        try:
            while data := os.read(sys.stdin.fileno(), 65536):
                stream.sendall(data)
        finally:
            with contextlib.suppress(OSError):
                stream.shutdown(socket.SHUT_WR)
    thread = threading.Thread(target=send, daemon=True)
    thread.start()
    try:
        while data := stream.recv(65536):
            sys.stdout.buffer.write(data)
            sys.stdout.buffer.flush()
    finally:
        stream.close()


def validate(argv=None):
    args = parser().parse_args(argv)
    if hasattr(args, 'wait') and not 0 <= args.wait <= 3600:
        parser().error('--wait must be between 0 and 3600 seconds')
    if args.verb == 'view' and os.environ.get('SUBYARD_RESOURCE_SESSION_TRANSPORT') and args.lease_file and not Path(args.lease_file).is_absolute():
        parser().error('--lease-file requires an absolute path in the selected remote yard')
    if args.verb == 'run' and (not args.command or args.command[0] != '--' or len(args.command) == 1):
        parser().error('run requires -- COMMAND [ARG...]')
    return args


def assessment(argv):
    args = validate(argv)
    changed = args.verb not in ('status', 'catalog')
    consequences = []
    if args.verb == 'cache' and getattr(args, 'dry_run', False):
        changed = False
    if args.verb in ('down', 'revoke'):
        slots = rpc('status')['slots']
        if args.verb == 'revoke':
            slots = [slot for slot in slots if slot['slot_id'] == args.slot]
            if not slots:
                raise Error('request', 'unknown Android pool slot')
        changed = any(slot['state'] != 'available' for slot in slots)
        if changed:
            consequences = ['close selected Android leases and stop their runtimes']
    print(json.dumps(dict(schema='yard.resource-action-assessment.v1', action=args.verb,
                          changed=changed, consequences=consequences), separators=(',', ':')))


def main(argv=None):
    args = validate(argv)
    if args.verb in ('status', 'catalog'):
        print(json.dumps(rpc(args.verb), indent=2))
        return 0
    if args.verb == 'cache':
        if args.cache_verb == 'prune':
            result = rpc('prune', dry_run=args.dry_run)
        else:
            request = {key: value for key in ('api', 'variant', 'abi') if (value := getattr(args, key)) is not None}
            result = rpc('prepare', timeout=960, request=request)
        print(json.dumps(result, indent=2))
        return 0
    if args.verb in ('revoke', 'down'):
        print(json.dumps(rpc('revoke' if args.verb == 'revoke' else 'drain', slot=getattr(args, 'slot', None))))
        return 0
    if args.verb in ('renew', 'release'):
        lease = read_lease(args.lease_file)
        print(json.dumps(rpc(args.verb, token=lease['token']), indent=2))
        return 0
    if args.verb == 'view':
        for program in ('scrcpy', 'adb'):
            if shutil.which(program) is None:
                raise Error('viewer', f'{program} must be installed on the machine running emu view')
    attached = args.verb == 'view' and args.lease_file
    if attached and os.environ.get('SUBYARD_RESOURCE_SESSION_TRANSPORT'):
        try:
            token = rpc('lease', lease_file=args.lease_file)['token']
        except Error as exc:
            if exc.code == 'request' and str(exc) == 'unknown Android pool operation':
                raise Error('transport', 'remote Android client is outdated; run yard -Y OWNER/YARD provision android to refresh it') from exc
            raise
    else:
        token = read_lease(args.lease_file)['token'] if attached else secrets.token_hex(32)
    retained = False
    server = None
    viewer_server = None
    temporary = None
    child = None
    finished = threading.Event()
    heartbeat_failed = threading.Event()
    requester = os.getppid()
    identity = requester_identity(requester)
    def requester_alive():
        return identity is not None and os.getppid() == requester and requester_identity(requester) == identity
    def interrupted(signum, _frame):
        raise SystemExit(128 + signum)
    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGHUP, interrupted)
    try:
        allocation = rpc('allocation', token=token) if attached else acquire(args, token, requester_alive)
        if allocation['state'] != 'held':
            raise Error('unavailable', 'lease is not ready for a viewer')
        temporary = Path(tempfile.mkdtemp(prefix='subyard-emu-'))
        endpoint = temporary / 'adb.sock'
        environment = context(allocation, endpoint)
        if args.verb == 'acquire':
            save_lease(args.lease_file, dict(token=token, endpoint=str(endpoint), allocation=allocation))
            process = subprocess.Popen([sys.executable, str(Path(__file__).resolve()), '_relay', args.lease_file],
                                       stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                       start_new_session=True)
            deadline = time.monotonic() + 10
            while not endpoint.exists() and process.poll() is None and time.monotonic() < deadline:
                time.sleep(0.05)
            if not endpoint.exists():
                raise Error('transport', 'could not start allocation ADB relay')
            retained = True
            print(json.dumps(dict(allocation=allocation, environment=environment, lease_file=args.lease_file), indent=2))
            return 0
        server = start_relay(token, endpoint)
        def heartbeat():
            last_renew = args.last_renew
            while not finished.wait(1):
                try:
                    if not requester_alive():
                        raise Error('owner', 'requesting session ended')
                    if time.monotonic() - last_renew < HEARTBEAT:
                        continue
                    rpc('renew', token=token)
                    last_renew = time.monotonic()
                except (Error, OSError, ValueError):
                    heartbeat_failed.set()
                    if child is not None:
                        with contextlib.suppress(ProcessLookupError):
                            os.killpg(child.pid, signal.SIGTERM)
                        try:
                            child.wait(timeout=5)
                        except subprocess.TimeoutExpired:
                            with contextlib.suppress(ProcessLookupError):
                                os.killpg(child.pid, signal.SIGKILL)
                    return
        command = args.command[1:] if args.command and args.command[0] == '--' else args.command
        if args.verb == 'view':
            viewer_server = start_viewer_relay(endpoint, allocation['android_serial'])
            port = str(viewer_server.server_address[1])
            command = ['scrcpy', *(command or []), *([] if args.control else ['--no-control']),
                       '--port=' + port, '--tunnel-host=127.0.0.1', '--tunnel-port=' + port]
        child = subprocess.Popen(command, env=dict(os.environ, **environment), start_new_session=True)
        if not attached:
            threading.Thread(target=heartbeat, daemon=True).start()
        code = child.wait()
        if args.verb == 'view' and code:
            print(f'Android viewer: child returncode {code}', file=sys.stderr)
        if code and viewer_server and viewer_server.failure:
            print('Android viewer: ' + viewer_server.failure, file=sys.stderr)
        if heartbeat_failed.is_set():
            raise Error('heartbeat', 'lease renewal failed; command was stopped')
        return code if code >= 0 else 128 - code
    finally:
        finished.set()
        if child is not None:
            with contextlib.suppress(ProcessLookupError):
                os.killpg(child.pid, signal.SIGTERM)
            try:
                child.wait(timeout=5)
            except subprocess.TimeoutExpired:
                pass
            with contextlib.suppress(ProcessLookupError):
                os.killpg(child.pid, signal.SIGKILL)
            child.wait()
        if not retained and not attached:
            try:
                rpc('release', token=token)
            except (Error, OSError, ValueError):
                print('Android release unconfirmed; pool owner will expire/drain the lease', file=sys.stderr)
        if viewer_server:
            viewer_server.shutdown()
            viewer_server.server_close()
        if server:
            server.shutdown()
            server.server_close()
        if temporary and not retained:
            (temporary / 'adb.sock').unlink(missing_ok=True)
            temporary.rmdir()


if __name__ == '__main__':
    try:
        if sys.argv[1:2] == ['_validate']:
            validate(sys.argv[2:])
            code = 0
        elif sys.argv[1:2] == ['_assessment']:
            assessment(sys.argv[2:])
            code = 0
        elif sys.argv[1:2] == ['_ready']:
            rpc('status')
            code = 0
        elif sys.argv[1:2] == ['_wire']:
            wire()
            code = 0
        elif sys.argv[1:2] == ['_relay']:
            relay_daemon(sys.argv[2])
            code = 0
        else:
            code = main()
        raise SystemExit(code)
    except Error as exc:
        print(f'Android {exc.code}: {exc}', file=sys.stderr)
        raise SystemExit(3 if exc.code == 'busy' else 4 if exc.code == 'timeout' else 1)
    except (OSError, ValueError):
        print('Android pool unavailable; run yard provision android on the owner', file=sys.stderr)
        raise SystemExit(1)
