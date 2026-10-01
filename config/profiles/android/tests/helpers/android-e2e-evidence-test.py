#!/usr/bin/env python3
"""Small owning checks for bounded Android fixture evidence."""
import contextlib
import importlib.util
import io
import json
from pathlib import Path
import struct
import subprocess
import sys
import tempfile
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[5]
E2E = ROOT / 'config/profiles/android/tests/e2e'


def load(name):
    spec = importlib.util.spec_from_file_location(name, E2E / (name + '.py'))
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value


capture = load('android-pool-capture')
viewer = load('android-pool-viewer')
monitor = load('android-pool-monitor')

# Exact pinned v4.1 layout: device64, codec4, session12, then packets.
# Session/config/keyframe bits are 63/62/61, respectively.
session = struct.pack('>III', 0x80000000, 640, 360)
resized_session = struct.pack('>III', 0x80000001, 360, 640)
metadata = b'private-device-name'.ljust(64, b'\0') + b'h264' + session
configuration = struct.pack('>QI', 1 << 62, 3) + b'cfg'
frame = struct.pack('>QI', (1 << 61) | 42, 7) + b'private'
with contextlib.redirect_stdout(io.StringIO()) as output:
    observed = capture.VideoObservation()
    for byte in metadata + configuration + frame + resized_session + configuration + frame[:-1]:
        observed.feed(bytes([byte]))
    assert observed.metadata == 'complete' and observed.frames == 1
    assert observed.dimensions == (360, 640) and observed.stage == 'payload' and observed.remaining == 1
    assert len(observed.header) <= 12
    observed.emit('failed', 23, 'server_socket')
assert 'private' not in output.getvalue()
summary = viewer.capture_summary(output.getvalue(), 23)
assert summary['metadata'] == 'complete' and summary['complete_frames'] == 1
assert summary['child_exit'] == 23 and summary['relay'] == 'server_socket'
assert summary['result'] == 'failed' and not summary['rendered']

for bad in (b'leak', b'h264' + struct.pack('>III', 0x80000000, 0, 360),
            b'h264' + struct.pack('>III', 0x80000002, 640, 360)):
    observed = capture.VideoObservation()
    observed.feed(b'\0' * 64 + bad)
    assert observed.stage == 'malformed' and observed.frames == 0
with contextlib.redirect_stdout(io.StringIO()):
    for length in (0, 16 * 1024 * 1024 + 1):
        observed = capture.VideoObservation()
        observed.feed(metadata + struct.pack('>QI', 42, length))
        assert observed.stage == 'malformed' and not observed.header and observed.frames == 0
    observed = capture.VideoObservation()
    observed.feed(metadata[:-1])
    assert observed.metadata == 'pending' and observed.dimensions is None and len(observed.header) == 11
    observed = capture.VideoObservation()
    observed.feed(metadata + struct.pack('>QI', 42, 16 * 1024 * 1024))
    for _ in range(16):
        observed.feed(b'x' * 65536)
    assert observed.stage == 'payload' and observed.frames == 0 and not observed.header
    observed = capture.VideoObservation()
    observed.feed(metadata[:-12] + frame)
    assert observed.stage == 'malformed' and observed.metadata == 'pending'


class Sink:
    def sendall(self, _data):
        raise OSError('private failure')

observed = capture.VideoObservation()
try:
    capture.ObservingSocket(Sink(), observed).sendall(metadata)
    raise AssertionError('failed forwarding accepted')
except OSError:
    pass
assert observed.metadata == 'pending'

class RecordingSink:
    def __init__(self):
        self.sent = bytearray()
    def sendall(self, data):
        self.sent.extend(data)

sink, observed = RecordingSink(), capture.VideoObservation()
with contextlib.redirect_stdout(io.StringIO()):
    wrapped = capture.ObservingSocket(sink, observed)
    wrapped.sendall(metadata[:5])
    wrapped.sendall(metadata[5:] + frame)
assert bytes(sink.sent) == metadata + frame and observed.frames == 1

secret = 'private-token-and-endpoint'
malicious = {'metadata': secret, 'complete_frames': 2, 'stream': 'observed',
             'result': 'passed', 'child_exit': 0, 'relay': secret}
for raw in ('android-capture {malformed', 'android-capture ' + json.dumps(malicious),
            'INFO: Texture: ' + secret, secret, 'x' * 262145):
    summary = viewer.capture_summary(raw, 23)
    assert secret not in json.dumps(summary) and summary['result'] == 'failed'
    assert summary['complete_frames'] == 'unknown'
for code in (0, 23, 124):
    summary = viewer.capture_summary(b'x' * 262145, code)
    assert summary['output'] == 'limit' and summary['rendered'] == 'unknown'
    assert summary['viewer_exit'] == code and summary['result'] != 'passed'
    assert summary['complete_frames'] == 'unknown' and summary['metadata'] == 'unknown'

summary = viewer.capture_summary('INFO: Texture: 640x360\n', 124)
assert summary['result'] == 'timeout' and summary['child_exit'] == 'unknown'
assert viewer.capture_summary('INFO: Texture: 640x360\n', 0)['result'] == 'passed'
assert viewer.capture_summary('Android viewer: initial media byte after 60.0s: TimeoutError\n', 23)['relay'] == 'initial_media_byte'

valid = b'  mScreenState=OFF\nprivate-token-and-endpoint\nSUBYARD_QUERY_EXIT=0\n'
assert monitor.display_values('ok', valid) == {'query': 'ok', 'screen_state': 'OFF'}
for status, raw in [('timeout', valid), ('limit', valid), ('transport_unavailable', valid),
                    ('ok', valid.replace(b'=0\n', b'=1\n')), ('ok', b'mScreenState=OFF\n'),
                    ('ok', valid + b'x'), ('ok', valid.replace(b'OFF', secret.encode())),
                    ('ok', valid.replace(b'OFF', b'OFF\xff')), ('ok', b'x' * 65537), (secret, valid),
                    ('ok', b'mScreenState=OFF\nmScreenState=ON\nSUBYARD_QUERY_EXIT=0\n')]:
    result = monitor.display_values(status, raw)
    assert result['screen_state'] == 'unknown' and secret not in json.dumps(result)

with patch.object(monitor, 'initialize'), patch.object(monitor, 'slot_states', return_value=[]), \
        patch.object(monitor.Path, 'exists', return_value=True), \
        patch.object(sys, 'argv', ['monitor', '/run/subyard-e2e-android-monitor-test.stop']), \
        contextlib.redirect_stdout(io.StringIO()) as output:
    monitor.main()
assert '"state": "idle"' in output.getvalue()

# Exercise real cleanup bodies with host operations replaced by bounded local stubs.
with tempfile.TemporaryDirectory() as directory:
    for initial, teardown, expected in ((23, 1, 23), (0, 1, 3), (0, 0, 0)):
        source = (E2E / 'android-pool-runtime.sh').read_text()
        cleanup = source[source.index('cleanup() {'):source.index('\ntrap cleanup EXIT')]
        script = ('set -euo pipefail\n. "$1/android-pool-phases.sh"\n'
                  'android_fixture=runtime\nandroid_phase_begin check\n'
                  'state="$2"; project=""; instance=""; monitor_pid=""; monitor_stop=""; monitor_host_stop="$2/stop"\n'
                  'incus_binary=(false); hold_seconds=0; teardown_status="$3"\n'
                  'yard() { return "$teardown_status"; }; sudo() { return 0; }\n' + cleanup +
                  '\ntrap cleanup EXIT\nexit "$4"\n')
        result = subprocess.run(['bash', '-c', script, '_', str(E2E), directory, str(teardown), str(initial)], capture_output=True, text=True)
        assert result.returncode == expected, result.stderr
        assert f'exit_code={initial}' in result.stdout
        if teardown:
            assert 'exit_code=3' in result.stdout
    log = Path(directory) / 'viewer.log'
    log.write_text(secret + '\nINFO: Texture: 640x360\n')
    with contextlib.redirect_stdout(io.StringIO()) as output:
        viewer.report_log(log, 23)
    assert secret not in output.getvalue() and '"child_exit": 23' in output.getvalue()

# A later fixture failure preserves each completed capture's own result.
attached = dict(metadata='complete', complete_frames=3, relay='no_failure_observed',
                child_exit=0, viewer_exit=0, rendered=True, result='passed', output='bounded', stream='observed')
standalone = dict(attached, complete_frames=0, viewer_exit=1, rendered=False, result='failed')
with tempfile.TemporaryDirectory() as directory:
    log = Path(directory) / 'viewer.log'
    log.write_text('android viewer evidence: ' + json.dumps(attached) + '\n'
                   'E2E_PHASE phase=fixture/viewer-attached-capture state=end duration_seconds=72 exit_code=0\n'
                   'android viewer evidence: ' + json.dumps(standalone) + '\n'
                   'E2E_PHASE phase=fixture/viewer-standalone-capture state=end duration_seconds=936 exit_code=1\n')
    with contextlib.redirect_stdout(io.StringIO()) as output:
        viewer.report_log(log, 1)
    reported = [json.loads(line.removeprefix('android viewer evidence: '))
                for line in output.getvalue().splitlines() if line.startswith('android viewer evidence: ')]
    assert reported == [attached, standalone]
    assert 'android fixture evidence: {"fixture_exit": 1, "output": "bounded"}' in output.getvalue()
    assert 'android capture evidence: ' not in output.getvalue()
    assert 'duration_seconds=72 exit_code=0' in output.getvalue()
    assert 'duration_seconds=936 exit_code=1' in output.getvalue()
    log.write_text('android viewer evidence: ' + json.dumps(attached) + '\n'
                   'android viewer evidence: ' + json.dumps(dict(attached, complete_frames=5)) + '\n')
    with contextlib.redirect_stdout(io.StringIO()) as output:
        viewer.report_log(log, 0)
    reported = [json.loads(line.removeprefix('android viewer evidence: '))
                for line in output.getvalue().splitlines() if line.startswith('android viewer evidence: ')]
    assert reported == [attached, dict(attached, complete_frames=5)]
    assert 'android fixture evidence: {"fixture_exit": 0, "output": "bounded"}' in output.getvalue()
    assert 'android capture evidence: ' not in output.getvalue()
    assert '"rendered": false' not in output.getvalue() and '"result": "failed"' not in output.getvalue()
    for code in (0, 23, 124):
        log.write_bytes(b'x' * 262145 + secret.encode())
        with contextlib.redirect_stdout(io.StringIO()) as output:
            viewer.report_log(log, code)
        assert output.getvalue().strip() == 'android fixture evidence: ' + json.dumps(dict(fixture_exit=code, output='limit'), sort_keys=True)
    for invalid in (dict(attached, extra=secret), dict(attached, metadata=secret),
                    dict(attached, child_exit=256), dict(attached, viewer_exit=True),
                    dict(attached, complete_frames=True), dict(attached, result=secret),
                    dict(attached, rendered=False), dict(attached, output=secret)):
        assert not viewer.valid_viewer_summary(invalid)
        log.write_text('android viewer evidence: ' + json.dumps(invalid))
        with contextlib.redirect_stdout(io.StringIO()) as output:
            viewer.report_log(log, 1)
        assert secret not in output.getvalue() and 'android viewer evidence: ' not in output.getvalue()

# The timeout path emits evidence and keeps the original failure exit.
class FakeViewer:
    pid = 999999
    def __init__(self, stream, exit_code, timeout, rendered=True):
        if rendered:
            stream.write(b'INFO: Texture: 640x360\n')
        stream.flush()
        self.exit_code, self.timeout = exit_code, timeout
    def wait(self, timeout):
        if self.timeout:
            self.timeout = False
            raise subprocess.TimeoutExpired('private-command', timeout)
        return self.exit_code

for code, timed_out, rendered, delay, expected in ((23, False, True, False, 23), (0, True, True, False, 124),
                                                (0, False, False, False, 1), (0, False, True, True, 1)):
    def spawn(*args, **kwargs):
        return FakeViewer(kwargs['stdout'], code, timed_out, rendered)
    with patch.object(viewer.subprocess, 'Popen', side_effect=spawn), \
            patch.object(viewer.os, 'killpg'), contextlib.redirect_stdout(io.StringIO()) as output:
        try:
            viewer.view([], 'attached-capture', delay_server=delay)
            raise AssertionError('viewer failure accepted')
        except SystemExit as exc:
            assert exc.code == expected
    assert f'exit_code={expected}' in output.getvalue()
    record = next(json.loads(line.removeprefix('android viewer evidence: '))
                  for line in output.getvalue().splitlines() if line.startswith('android viewer evidence: '))
    assert record['viewer_exit'] == expected and record['result'] == ('timeout' if timed_out else 'failed')
    assert record['child_exit'] == ('unknown' if timed_out else code)
    assert 'private-command' not in output.getvalue()
    if timed_out:
        assert '"result": "timeout"' in output.getvalue()

def spawn_limited(*args, **kwargs):
    process = FakeViewer(kwargs['stdout'], 0, False)
    kwargs['stdout'].write(b'x' * 262145)
    return process

with patch.object(viewer.subprocess, 'Popen', side_effect=spawn_limited), contextlib.redirect_stdout(io.StringIO()) as output:
    try:
        viewer.view([], 'attached-capture')
        raise AssertionError('oversized capture accepted')
    except SystemExit as exc:
        assert exc.code == 1
record = next(json.loads(line.removeprefix('android viewer evidence: '))
              for line in output.getvalue().splitlines() if line.startswith('android viewer evidence: '))
assert record['rendered'] == 'unknown' and record['viewer_exit'] == 1 and record['result'] == 'failed'

# Invalid direct helper invocations reject before fixture or VM access.
for arguments in ([], ['r', 's', 'y', 'p'], ['r', 's', 'y', 'p', 'i', '--unknown'],
                  ['r', 's', 'y', 'p', 'i', '--viewer-only', '--recovery-only']):
    result = subprocess.run(['bash', str(E2E / 'android-pool-recovery.sh'), *arguments], capture_output=True, text=True)
    assert result.returncode != 0 and 'usage:' in result.stderr
    assert 'E2E_PHASE' not in result.stdout and 'requires the allocated' not in result.stderr
for arguments in (['--lane'], ['--lane', 'unknown'], ['--lane', 'viewer', 'extra'], ['--unknown']):
    result = subprocess.run(['bash', str(E2E / 'android-pool-runtime.sh'), *arguments], capture_output=True, text=True)
    assert result.returncode == 2
    assert 'requires VM1' not in result.stderr and 'E2E_PHASE' not in result.stdout

# Execute the existing mode/prelude/dispatch blocks without guest access.
runtime_source = (E2E / 'android-pool-runtime.sh').read_text()
recovery_source = (E2E / 'android-pool-recovery.sh').read_text()
mode_block = recovery_source[recovery_source.index('viewer_only=0'):recovery_source.index('root="$1"')]
for arguments, expected in ((['r', 's', 'y', 'p', 'i'], '0 0'),
                            (['r', 's', 'y', 'p', 'i', '--viewer-only'], '1 0'),
                            (['r', 's', 'y', 'p', 'i', '--recovery-only'], '0 1'),
                            (['r', 's', 'y', 'p', 'i', '--viewer-only', '--recovery-only'], None),
                            (['r', 's', 'y', 'p', 'i', '--unknown'], None)):
    script = 'set -euo pipefail\nfail() { exit 2; }\n' + mode_block + '\nprintf "%s %s\\n" "$viewer_only" "$recovery_only"'
    result = subprocess.run(['bash', '-c', script, '_', *arguments], capture_output=True, text=True)
    assert result.returncode == (0 if expected is not None else 2)
    if expected is not None:
        assert result.stdout.strip() == expected

remote_start = runtime_source.index('if [ "$lane" != viewer ]; then\nandroid_phase_begin remote-owner-route')
remote_end = runtime_source.index('android_phase_begin images', remote_start)
images_start = remote_end
images_end = runtime_source.index('android_phase_end 0', images_start)
dispatch_start = runtime_source.index('viewer_args=()')
dispatch_end = runtime_source.index('android_phase_end 0', dispatch_start)
for lane, mode in (('full', ''), ('viewer', '--viewer-only'), ('recovery', '--recovery-only')):
    prelude = ('set -euo pipefail\nlane="$1"; root=/fixture; state=/fixture; YARD_NAME=fixture; project=fixture; instance=fixture\n'
               'android_phase_begin() { :; }; timeout() { printf "%s\\n" "$*"; }; yard() { printf "%s\\n" "$*"; }\n')
    result = subprocess.run(['bash', '-c', prelude + runtime_source[remote_start:remote_end], '_', lane], capture_output=True, text=True, check=True)
    assert ('android-pool-remote.sh' in result.stdout) == (lane != 'viewer')
    result = subprocess.run(['bash', '-c', prelude + runtime_source[images_start:images_end], '_', lane], capture_output=True, text=True, check=True)
    assert result.stdout.count('emu cache prepare --api 35') == (1 if lane == 'viewer' else 2)
    assert result.stdout.count('emu cache prepare --api 36') == 1
    result = subprocess.run(['bash', '-c', prelude + runtime_source[dispatch_start:dispatch_end], '_', lane], capture_output=True, text=True, check=True)
    assert 'android-pool-recovery.sh' in result.stdout
    assert ('--viewer-only' in result.stdout) == (mode == '--viewer-only')
    assert ('--recovery-only' in result.stdout) == (mode == '--recovery-only')

with tempfile.TemporaryDirectory() as directory:
    log = Path(directory) / 'failure.log'
    log.write_text(secret)
    for initial, guest_code, expected in ((23, 1, 23), (0, 1, 3), (0, 0, 0)):
        cleanup = recovery_source[recovery_source.index('cleanup() {'):recovery_source.index('\ntrap cleanup EXIT')]
        script = ('set -euo pipefail\n. "$1/config/profiles/android/tests/e2e/android-pool-phases.sh"\n'
                  'root="$1"; work="$2"; phase_log="$2/failure.log"; lease_dir=/fixture; guest_status="$3"\n'
                  'android_fixture=recovery; android_phase_begin check\n'
                  'guest() { return "$guest_status"; }; incus_exec() { return 124; }\n' + cleanup +
                  '\ntrap cleanup EXIT\nexit "$4"\n')
        result = subprocess.run(['bash', '-c', script, '_', str(ROOT), directory, str(guest_code), str(initial)], capture_output=True, text=True)
        assert result.returncode == expected, result.stderr
        assert secret not in result.stdout + result.stderr
        if guest_code:
            assert 'exit_code=3' in result.stdout
print('ok: Android capture, display, privacy, bounds, lane and cleanup evidence')
