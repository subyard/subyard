#!/usr/bin/env python3
"""Real ENOSPC and durable acknowledgement checks on one leased stage only."""
import argparse
from contextlib import closing, contextmanager
import errno
import hashlib
import http.client
import json
import os
from pathlib import Path
import secrets
import signal
import socket
import sqlite3
import stat
import subprocess
import sys
import tempfile
import time

MARKER = b'subyard-telemetry-storage-v1\n'
SOURCE = 'subyard-telemetry-storage'
SIZE = 16*1024*1024
HOOK = Path('/usr/local/lib/subyard-telemetry/broker-correlation.py')

# Executed as root only by the leased-stage fixture. Local checks run this same
# atomic boundary on their own private file, without sudo or system writes.
REPLACE_HOOK = '''import hashlib, json, os, pathlib, stat, sys, tempfile
value = json.load(sys.stdin)
path = pathlib.Path(value['path'])
info = path.lstat()
assert stat.S_ISREG(info.st_mode) and info.st_uid == os.geteuid() and info.st_nlink == 1
assert stat.S_IMODE(info.st_mode) == 0o755
assert hashlib.sha256(path.read_bytes()).hexdigest() == value['expected']
content = bytes.fromhex(value['content'])
assert len(content) <= 65536
fd, name = tempfile.mkstemp(prefix='.broker-correlation-probe.', dir=path.parent)
try:
    with os.fdopen(fd, 'wb') as stream:
        os.fchmod(stream.fileno(), 0o755)
        os.fchown(stream.fileno(), info.st_uid, info.st_gid)
        stream.write(content); stream.flush(); os.fsync(stream.fileno())
    os.replace(name, path)
finally:
    if os.path.exists(name): os.unlink(name)
'''


def private_file(path, content):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'wb') as output:
        os.fchmod(output.fileno(), 0o600)
        output.write(content)
        output.flush()
        os.fsync(output.fileno())


def owned_root(root):
    info = root.lstat()
    assert root.parent == Path('/tmp') and root.name.startswith('subyard-telemetry-storage.')
    assert stat.S_ISDIR(info.st_mode) and info.st_uid == os.getuid() and stat.S_IMODE(info.st_mode) == 0o700
    marker = root/'marker'
    info = marker.lstat()
    assert stat.S_ISREG(info.st_mode) and info.st_nlink == 1 and info.st_uid == os.getuid() and stat.S_IMODE(info.st_mode) == 0o600
    assert marker.read_bytes() == MARKER


def mount_entry(path, contents=None):
    contents = Path('/proc/self/mountinfo').read_text() if contents is None else contents
    matches = []
    for line in contents.splitlines():
        before, _, after = line.partition(' - ')
        fields, kind = before.split(), after.split()
        if len(fields) >= 6 and len(kind) >= 2 and fields[4] == str(path):
            assert kind[:2] == ['tmpfs', SOURCE], 'foreign filesystem at fixture mountpoint'
            matches.append(int(fields[0]))
    assert len(matches) == 1, 'fixture mount identity is not unique'
    return matches[0]


def verify_mount(root, identity, require_marker=True):
    owned_root(root)
    mount = root/'filesystem'
    assert not mount.is_symlink()
    info = mount.stat()
    assert [mount_entry(mount), info.st_dev, info.st_ino] == identity, 'fixture mount generation changed'
    if require_marker:
        marker = mount/'marker'
        assert not marker.is_symlink() and marker.stat().st_uid == os.getuid() and marker.read_bytes() == MARKER


def stop(process, hard=False):
    if process is not None and process.poll() is None:
        process.send_signal(signal.SIGKILL if hard else signal.SIGTERM)
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)


def healthy(data, process):
    deadline = time.monotonic()+15
    while time.monotonic() < deadline:
        assert process.poll() is None, 'isolated collector exited before readiness'
        connection = http.client.HTTPConnection('localhost', timeout=1)
        try:
            connection.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            connection.sock.settimeout(1)
            connection.sock.connect(str(data/'control.sock'))
            connection.request('GET', '/healthz')
            response = connection.getresponse()
            if response.status == 200 and json.loads(response.read(1024)).get('ok'):
                return
        except (OSError, ValueError, http.client.HTTPException):
            pass
        finally:
            connection.close()
        time.sleep(0.1)
    raise AssertionError('isolated collector readiness deadline')


def count(data):
    with closing(sqlite3.connect('file:'+str(data/'telemetry.sqlite')+'?mode=ro', uri=True, timeout=1)) as db:
        return db.execute("SELECT count(*) FROM records WHERE kind='otlp/logs'").fetchone()[0]


def send(port, token, serial):
    payload = json.dumps({'resourceLogs': [{'resource': {'attributes': [
        {'key': 'service.name', 'value': {'stringValue': 'subyard-storage-fixture'}}]},
        'scopeLogs': [{'logRecords': [{'timeUnixNano': str(time.time_ns()), 'severityNumber': 9,
                                     'body': {'stringValue': 'synthetic-probe-'+str(serial)}}]}]}]}).encode()
    connection = http.client.HTTPConnection('127.0.0.1', port, timeout=3)
    try:
        connection.request('POST', '/v1/logs', payload, {'Content-Type': 'application/json', 'Authorization': 'Bearer '+token})
        response = connection.getresponse()
        response.read(65536)
        return response.status, hashlib.sha256(payload).hexdigest()
    finally:
        connection.close()


def fill(path):
    written = 0
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    try:
        os.fchmod(fd, 0o600)
        while written <= 2*SIZE:
            try:
                written += os.write(fd, b'\0'*65536)
            except OSError as failure:
                if failure.errno != errno.ENOSPC:
                    raise
                return written
    finally:
        os.close(fd)
    raise AssertionError('real filesystem did not reach ENOSPC within its fixed bound')


def correlation_probe(directory, helper):
    """Trace only the real optional hook; retain fixed failure booleans, never inputs."""
    helper = helper.resolve(strict=True)
    code = '''import errno, os, sys
HELPER = %r
RECORDS = %r
SOURCES = %r
PROOF = %r
held_enospc = False
def trace(frame, event, value):
    if event != 'call':
        return None
    filename = os.path.realpath(frame.f_code.co_filename)
    if frame.f_code.co_name == 'atomic_json' and filename == RECORDS and frame.f_back.f_code.co_name == 'store_intent' and os.path.realpath(frame.f_back.f_code.co_filename) == SOURCES:
        path = os.fspath(frame.f_locals.get('path', ''))
        if os.path.basename(os.path.dirname(path)) != 'intents' or not os.path.basename(path).startswith('intent-'):
            return None
        def observe_intent(frame, event, value):
            global held_enospc
            if event == 'exception' and isinstance(value[1], OSError) and value[1].errno == errno.ENOSPC:
                held_enospc = True
            return observe_intent
        return observe_intent
    direct_emit = frame.f_code.co_name == 'emit' and filename == RECORDS and frame.f_back.f_code.co_name == 'register' and os.path.realpath(frame.f_back.f_code.co_filename) == HELPER
    register = frame.f_code.co_name == 'register' and filename == HELPER
    if not (direct_emit or register):
        return None
    phase = (frame.f_back if direct_emit else frame).f_locals.get('request', {}).get('phase')
    if not (direct_emit and phase == 'request' or register and phase == 'held'):
        return None
    failed = False
    def observe(frame, event, value):
        nonlocal failed
        if event == 'exception' and isinstance(value[1], OSError) and value[1].errno == errno.ENOSPC:
            failed = True
        if event == 'return' and value is False and failed and (phase == 'request' or held_enospc):
            try:
                fd = os.open(PROOF+'/'+phase+'.json', os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW, 0o600)
                with os.fdopen(fd, 'wb') as output:
                    os.fchmod(output.fileno(), 0o600)
                    output.write(b'{"enospc":true,"'+(b'emit_failed' if phase == 'request' else b'intent_failed')+b'":true}\\n')
            except FileExistsError:
                pass
        return observe
    return observe
if os.path.realpath(sys.argv[0]) == HELPER:
    sys.settrace(trace)
''' % (str(helper), str(helper.parent/'records.py'), str(helper.parent/'sources.py'), str(directory))
    private_file(directory/'sitecustomize.py', code.encode())


def probe_wrapper(directory, helper, original):
    # No loader environment is needed; argv, __file__, import path and the real
    # register code filename remain those of the installed optional hook.
    return ('exec(compile(open(%r, "rb").read(), %r, "exec"))\n'
            'exec(compile(%r, %r, "exec"))\n' %
            (str(directory/'sitecustomize.py'), str(directory/'sitecustomize.py'), original, str(helper))).encode()


def replace_hook(helper, expected, content):
    assert helper == HOOK, 'only the installed native correlation hook may be instrumented'
    subprocess.run(['sudo', '-n', sys.executable, '-B', '-c', REPLACE_HOOK],
                   input=json.dumps(dict(path=str(helper), expected=expected, content=content.hex())).encode(),
                   check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10)


@contextmanager
def installed_probe(directory, helper, cleanup_errors):
    info = helper.lstat()
    assert helper == HOOK and helper.resolve(strict=True) == HOOK
    assert stat.S_ISREG(info.st_mode) and info.st_uid == 0 and info.st_nlink == 1
    assert stat.S_IMODE(info.st_mode) == 0o755
    original = helper.read_bytes()
    original_hash = hashlib.sha256(original).hexdigest()
    wrapper = probe_wrapper(directory, helper, original)
    wrapper_hash = hashlib.sha256(wrapper).hexdigest()
    try:
        replace_hook(helper, original_hash, wrapper)
        yield
    finally:
        try:
            current = helper.lstat()
            assert stat.S_ISREG(current.st_mode) and current.st_uid == info.st_uid and current.st_gid == info.st_gid
            assert current.st_nlink == 1 and stat.S_IMODE(current.st_mode) == stat.S_IMODE(info.st_mode)
            if hashlib.sha256(helper.read_bytes()).hexdigest() != original_hash:
                replace_hook(helper, wrapper_hash, original)
            assert helper.read_bytes() == original and stat.S_IMODE(helper.stat().st_mode) == 0o755
        except BaseException as error:
            cleanup_errors.append(error)


def preflight():
    with tempfile.TemporaryDirectory(prefix='subyard-telemetry-storage.', dir='/tmp') as name:
        root = Path(name)
        os.chmod(root, 0o700)
        private_file(root/'marker', MARKER)
        owned_root(root)
        path = root/'filesystem'
        sample = f'81 1 0:19 / {path} rw,nodev,nosuid,noexec - tmpfs {SOURCE} rw,size=16m\n'
        assert mount_entry(path, sample) == 81
        for bad in (sample.replace(SOURCE, 'foreign'), sample+sample):
            try:
                mount_entry(path, bad)
            except AssertionError:
                continue
            raise AssertionError('foreign or duplicate mount accepted')
        (root/'marker').unlink()
        (root/'marker').symlink_to('/does-not-exist')
        try:
            owned_root(root)
        except AssertionError:
            pass
        else:
            raise AssertionError('symlink marker accepted')
        hook = root/'hook'
        hook.mkdir(mode=0o700)
        helper = hook/'broker-correlation.py'
        source = Path(__file__).resolve().parents[2]
        for name in ('broker-correlation.py', 'records.py', 'sources.py', 'resources.py'):
            private_file(hook/name, (source/name).read_bytes())
        records_source = (hook/'records.py').read_bytes()
        (hook/'records.py').unlink()
        private_file(hook/'records.py', records_source+b'''
import errno
original_atomic_json = atomic_json
def atomic_json(path, value):
    if Path(path).name.startswith('intent-'):
        raise OSError(errno.ENOSPC, 'synthetic intent storage fault')
    return original_atomic_json(path, value)
def emit(*args, **kwargs):
    try:
        raise OSError(errno.ENOSPC, 'synthetic storage fault')
    except OSError:
        return False
''')
        probe = root/'probe'
        probe.mkdir(mode=0o700)
        correlation_probe(probe, helper)
        environment = dict(os.environ, SUBYARD_AGENT_RUN_ID='fixture-run', SUBYARD_TELEMETRY_DATA_DIR=str(root/'data'))
        environment.pop('PYTHONPATH', None)  # Match the resource dispatcher's loader-env filtering.
        original = helper.read_bytes()
        helper.chmod(0o755)
        def invoke(phase):
            request = dict(kind='vm', phase=phase, owner_id='fixture-owner', correlation_id='fixture-acquire',
                           slot_id='slot-001', resource_generation=1, lease_epoch=1,
                           transport=dict(script='/tmp/fixture/dev/agent-e2e.sh', yard='test-yard'))
            subprocess.run([sys.executable, '-B', str(helper)], input=json.dumps(request).encode(), check=True,
                           env=environment, timeout=5)
        for phase in ('request', 'held'):
            invoke(phase)
        assert {path.name for path in probe.iterdir()} == {'sitecustomize.py'}, 'unfiltered loader environment reached hook'
        wrapper = probe_wrapper(probe, helper, original)
        def replace(expected, content):
            subprocess.run([sys.executable, '-B', '-c', REPLACE_HOOK],
                           input=json.dumps(dict(path=str(helper), expected=hashlib.sha256(expected).hexdigest(), content=content.hex())).encode(),
                           check=True, timeout=5)
        replace(original, wrapper)
        for phase in ('request', 'held'):
            invoke(phase)
            assert json.loads((probe/(phase+'.json')).read_text()) == {'enospc': True,
                ('emit_failed' if phase == 'request' else 'intent_failed'): True}
        pending = root/'data/sources/intents'
        assert not list(pending.glob('intent-*.json')), 'failed intent write was reported as durable'
        assert json.loads((pending/'registration-health').read_text())['reason'] == 'correlation_registration_unavailable'
        assert {path.name for path in probe.iterdir()} == {'sitecustomize.py', 'request.json', 'held.json'}
        for phase in ('request', 'held'):
            (probe/(phase+'.json')).unlink()
        (hook/'records.py').unlink()
        private_file(hook/'records.py', records_source+b'''
import errno
original_atomic_json = atomic_json
def atomic_json(path, value):
    if Path(path).name.startswith('intent-'):
        raise OSError(errno.EACCES, 'synthetic non-storage fault')
    return original_atomic_json(path, value)
def emit(*args, **kwargs):
    return False
''')
        for phase in ('request', 'held'):
            invoke(phase)
        assert {path.name for path in probe.iterdir()} == {'sitecustomize.py'}, 'false without ENOSPC passed the probe'
        assert not list(pending.glob('intent-*.json')), 'failed non-ENOSPC intent was reported as durable'
        (hook/'records.py').unlink()
        private_file(hook/'records.py', records_source+b'\ndef emit(*args, **kwargs):\n    return False\n')
        invoke('held')
        assert len(list(pending.glob('intent-*.json'))) == 1, 'healthy held registration did not save its intent'
        replace(wrapper, original)
        assert helper.read_bytes() == original and stat.S_IMODE(helper.stat().st_mode) == 0o755
    print('ok: storage fixture exact root/mount/source/generation guards; no mount or collector executed')


def run(args):
    assert os.environ.get('SUBYARD_E2E_VM') == '1', 'storage fixture requires a leased disposable stage'
    subprocess.run(['sudo', '-n', 'test', '-f', '/run/subyard-e2e-lease.json'], check=True, timeout=5)
    binary = args.binary.resolve(strict=True)
    assert binary.is_file() and os.access(binary, os.X_OK)
    args.evidence.mkdir(mode=0o700, parents=True, exist_ok=False)
    os.chmod(args.evidence, 0o700)
    root = Path(tempfile.mkdtemp(prefix='subyard-telemetry-storage.', dir='/tmp'))
    os.chmod(root, 0o700)
    private_file(root/'marker', MARKER)
    mount = root/'filesystem'
    mount.mkdir(mode=0o700)
    process, identity, failure, cleanup_failure = None, None, None, None
    mounted, inner_marked = False, False
    result = {'schema': 1, 'binary_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(),
              'filesystem_bytes': SIZE, 'broker_failure_physical': 'not_exercised'}
    logs, probe_cleanup_errors = [], []
    def interrupted(_signum, _frame):
        raise InterruptedError('storage fixture interrupted')
    previous_term = signal.signal(signal.SIGTERM, interrupted)
    try:
        owned_root(root)
        subprocess.run(['sudo', '-n', 'mount', '-t', 'tmpfs', '-o',
            f'size={SIZE},mode=0700,uid={os.getuid()},gid={os.getgid()},nodev,nosuid,noexec', SOURCE, str(mount)], check=True, timeout=10)
        mounted = True
        info = mount.stat()
        identity = [mount_entry(mount), info.st_dev, info.st_ino]
        private_file(mount/'marker', MARKER)
        inner_marked = True
        verify_mount(root, identity)
        data = mount/'data'
        data.mkdir(mode=0o700)
        os.chmod(data, 0o700)
        token = secrets.token_hex(32)
        private_file(data/'auth.token', (token+'\n').encode())
        with socket.socket() as port_socket:
            port_socket.bind(('127.0.0.1', 0))
            port = port_socket.getsockname()[1]
        def start(name):
            nonlocal process
            fd = os.open(args.evidence/name, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
            os.fchmod(fd, 0o600)
            stream = os.fdopen(fd, 'wb')
            logs.append(stream)
            process = subprocess.Popen([str(binary), 'serve', '--data-dir', str(data), '--listen', f'127.0.0.1:{port}',
                                      '--auth-file', str(data/'auth.token')], stdin=subprocess.DEVNULL,
                                     stdout=subprocess.DEVNULL, stderr=stream)
            healthy(data, process)
            return process
        process = start('collector-before.log')
        initial, payload_hash = send(port, token, 0)
        assert initial == 200 and count(data) == 1, 'initial OTLP acknowledgement was not committed'
        result.update(initial_status=initial, initial_committed=1, initial_payload_sha256=payload_hash)
        if args.native_command:
            # Reach the durable intent write after filling, rather than failing
            # earlier while admitting its directory or registration lock.
            pending = data/'sources/intents'
            pending.mkdir(mode=0o700, parents=True)
            os.chmod(pending.parent, 0o700)
            os.chmod(pending, 0o700)
            private_file(pending/'.lock', b'')
        result['filled_bytes'] = fill(mount/'fill')
        result['fill_errno'] = errno.ENOSPC
        result['free_bytes_after_fill'] = os.statvfs(mount).f_bavail*os.statvfs(mount).f_frsize
        acknowledged, statuses = 1, []
        deadline = time.monotonic()+30
        for serial in range(1, 129):
            assert time.monotonic() < deadline, 'ENOSPC rejection deadline'
            status, _ = send(port, token, serial)
            statuses.append(status)
            committed = count(data)
            if status == 200:
                acknowledged += 1
                assert committed == acknowledged, 'OTLP acknowledged an uncommitted record'
                continue
            assert status == 429 or 500 <= status <= 599, 'storage rejection is not retryable'
            assert committed == acknowledged, 'rejected OTLP unexpectedly committed'
            break
        else:
            raise AssertionError('full filesystem never rejected OTLP')
        result.update(full_statuses=statuses, acknowledged_before_crash=acknowledged, rejection_status=statuses[-1])
        proof = args.evidence/'native-proof'
        native = subprocess.run([sys.executable, '-B', str(Path(__file__).resolve().parents[2]/'launcher.py'), 'run',
            '--data-dir', str(data), '--agent', 'other', '--agent-version', 'storage-fixture', '--scope-mode', 'partial', '--',
            sys.executable, '-c', 'import os,sys; fd=os.open(sys.argv[1],os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600); os.fchmod(fd,0o600); os.write(fd,b"native-completed"); os.close(fd)', str(proof)],
            capture_output=True, timeout=15)
        private_file(args.evidence/'managed-command.stderr', native.stderr)
        assert native.returncode == 0 and proof.read_bytes() == b'native-completed', 'storage failure changed native command result'
        result.update(native_exit=native.returncode, native_proof_sha256=hashlib.sha256(proof.read_bytes()).hexdigest(),
                      native_scope_quality='partial', native_telemetry_coverage='unavailable_due_to_enospc')
        stop(process, hard=True)
        result['collector_crash_exit'] = process.returncode
        if args.native_command:
            command = args.native_command[1:] if args.native_command[:1] == ['--'] else args.native_command
            assert command, 'native broker command is empty'
            helper = HOOK
            assert helper.is_file(), 'installed native correlation hook is required'
            assert helper.read_bytes() == (Path(__file__).resolve().parents[2]/'broker-correlation.py').read_bytes(), \
                'installed native correlation hook differs from the frozen fixture source'
            probe = args.evidence/'hook-probe'
            probe.mkdir(mode=0o700)
            correlation_probe(probe, helper)
            with installed_probe(probe, helper, probe_cleanup_errors):
                callback = subprocess.run(['timeout', '--signal=TERM', '--kill-after=15s', '1800s',
                    sys.executable, '-B', str(Path(__file__).resolve().parents[2]/'launcher.py'),
                    'run', '--data-dir', str(data), '--agent', 'other', '--agent-version', 'storage-broker-fixture',
                    '--scope-mode', 'partial', '--', *command], capture_output=True, timeout=1830)
            private_file(args.evidence/'broker-command.stderr', callback.stderr)
            assert callback.returncode == 0, 'storage failure changed native broker lifecycle result'
            for phase in ('request', 'held'):
                assert json.loads((probe/(phase+'.json')).read_text()) == {'enospc': True,
                    ('emit_failed' if phase == 'request' else 'intent_failed'): True}, \
                    'actual native correlation hook did not encounter ENOSPC'
            assert process.poll() is not None, 'isolated collector unexpectedly restarted'
            result.update(broker_failure_physical='pass', broker_native_exit=callback.returncode,
                          broker_hook_request_enospc=True, broker_hook_held_enospc=True,
                          broker_collector_unavailable=True)
        verify_mount(root, identity)
        (mount/'fill').unlink()
        process = start('collector-restarted.log')
        assert count(data) == acknowledged, 'acknowledged records did not survive collector crash'
        result['committed_after_restart'] = count(data)
        recovered, _ = send(port, token, 1000)
        assert recovered == 200 and count(data) == acknowledged+1, 'OTLP did not recover after freeing storage'
        result.update(recovered_status=recovered, committed_after_recovery=count(data), status='pass')
    except BaseException as error:
        failure = error
        result.update(status='fail', failure={'type': type(error).__name__, 'message': str(error)})
    finally:
        try:
            stop(process)
            for stream in logs:
                stream.close()
            if mounted:
                if identity is None:
                    owned_root(root)
                    info = mount.stat()
                    identity = [mount_entry(mount), info.st_dev, info.st_ino]
                verify_mount(root, identity, require_marker=inner_marked)
                subprocess.run(['sudo', '-n', 'umount', '--', str(mount)], check=True, timeout=10)
                assert not any(line.split()[4] == str(mount) for line in Path('/proc/self/mountinfo').read_text().splitlines())
            result['cleanup'] = 'pass'
        except BaseException as error:
            cleanup_failure = error
            result['cleanup'] = {'type': type(error).__name__, 'message': str(error)}
        if probe_cleanup_errors:
            cleanup_failure = cleanup_failure or probe_cleanup_errors[0]
            result['cleanup'] = {'type': type(cleanup_failure).__name__, 'hook_restore_failed': True}
        private_file(args.evidence/'result.json', (json.dumps(result, indent=2)+'\n').encode())
        signal.signal(signal.SIGTERM, previous_term)
    print(json.dumps({'schema': 'subyard.telemetry.storage-failure/v1', 'status': result['status'],
                      'original_failure': type(failure).__name__ if failure else None,
                      'cleanup_status': 'fail' if cleanup_failure else 'pass',
                      'cleanup_failure': type(cleanup_failure).__name__ if cleanup_failure else None,
                      'binary_sha256': result['binary_sha256'], 'enospc': result.get('fill_errno'),
                      'http_status': result.get('rejection_status'), 'acknowledged': result.get('acknowledged_before_crash'),
                      'committed_after_restart': result.get('committed_after_restart'),
                      'committed_after_recovery': result.get('committed_after_recovery'), 'native_exit': result.get('native_exit'),
                      'broker_failure_physical': result['broker_failure_physical'],
                      'broker_native_exit': result.get('broker_native_exit')}), flush=True)
    if failure is not None:
        raise failure
    if cleanup_failure is not None:
        raise cleanup_failure


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--preflight', action='store_true')
    parser.add_argument('--binary', type=Path)
    parser.add_argument('--evidence', type=Path)
    parser.add_argument('--native-command', action='store_true', help='Run all following argv as the native lifecycle command')
    argv = list(sys.argv[1:] if argv is None else argv)
    boundary = argv.index('--native-command') if '--native-command' in argv else len(argv)
    arguments = parser.parse_args(argv[:boundary])
    arguments.native_command = argv[boundary+1:] if boundary < len(argv) else None
    if boundary < len(argv) and (not arguments.native_command or arguments.native_command == ['--']):
        parser.error('--native-command requires a command')
    if arguments.preflight:
        preflight()
    else:
        if not arguments.binary or not arguments.evidence:
            parser.error('--binary and --evidence are required')
        run(arguments)


if __name__ == '__main__':
    main()
