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
import zlib
from types import SimpleNamespace
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
assert viewer.capture_summary('', 139)['child_exit'] == 'unknown'
for child_status in (-11, 139, 23):
    summary = viewer.capture_summary(f'Android viewer: child returncode {child_status}\n', 139)
    assert summary['child_returncode'] == child_status
    assert summary['child_exit'] == (child_status if child_status >= 0 else 128 - child_status)
    assert viewer.valid_viewer_summary(summary)
for invalid in ('-65', '256', secret, '11 ' + secret):
    summary = viewer.capture_summary('Android viewer: child returncode ' + invalid + '\n', 139)
    assert summary['child_exit'] == 'unknown' and 'child_returncode' not in summary
    assert secret not in json.dumps(summary)

# Epoch observations identify the selected lease and complete packets, never media.
public_context = dict(SUBYARD_EMU_SLOT='002', SUBYARD_EMU_GENERATION='7', SUBYARD_EMU_API='35',
                      ANDROID_SERIAL=secret, ADB_SERVER_SOCKET=secret)
with patch.object(capture.time, 'time_ns', side_effect=[1000000000000, 1001000000000, 1002000000000, 1003000000000]), \
        contextlib.redirect_stdout(io.StringIO()) as output:
    observed = capture.VideoObservation()
    observed.observe_context(public_context)
    observed.feed(metadata[:-1])
    assert observed.timing['metadata_at_ms'] == 0
    observed.feed(metadata[-1:] + configuration + frame[:-1])
    assert observed.timing['metadata_at_ms'] == 1001000 and observed.timing['first_frame_at_ms'] == 0
    observed.feed(frame[-1:] + resized_session + frame)
    assert observed.timing['first_frame_at_ms'] == 1002000
    observed.finish()
timing = dict(slot=2, generation=7, api=35, context_at_ms=1000000,
              metadata_at_ms=1001000, first_frame_at_ms=1002000, finished_at_ms=1003000)
summary = viewer.capture_summary(output.getvalue(), 23)
assert summary['timing'] == timing and viewer.valid_viewer_summary(summary)
assert secret not in output.getvalue() and 'private' not in output.getvalue()
for key, bad in [('SUBYARD_EMU_SLOT', secret), ('SUBYARD_EMU_SLOT', '000'),
                 ('SUBYARD_EMU_GENERATION', '2147483648'), ('SUBYARD_EMU_API', '100'),
                 ('SUBYARD_EMU_API', True)]:
    observed = capture.VideoObservation()
    observed.observe_context(dict(public_context, **{key: bad}))
    assert observed.timing is None
for invalid in (dict(timing, extra=secret), dict(timing, slot=True), dict(timing, slot=0),
                dict(timing, generation=2147483648), dict(timing, api=100),
                dict(timing, finished_at_ms=4102444800001), dict(timing, metadata_at_ms=secret),
                dict(timing, metadata_at_ms=999999), dict(timing, first_frame_at_ms=1004000),
                dict(timing, metadata_at_ms=0), dict(timing, finished_at_ms=999999)):
    assert not viewer.valid_capture_timing(invalid)
    summary = viewer.capture_summary('android-capture-timing ' + json.dumps(invalid), 23)
    assert 'timing' not in summary and secret not in json.dumps(summary)
    assert not viewer.valid_viewer_summary(dict(summary, timing=invalid))
assert viewer.valid_capture_timing(dict(timing, metadata_at_ms=0, first_frame_at_ms=0))
assert viewer.valid_capture_timing(dict(timing, first_frame_at_ms=0))

# Exercise the helper's actual context/relay wrapping without installed runtime access.
server = SimpleNamespace(process_request=lambda request, address: None, failure=None)
fake_client = SimpleNamespace(context=lambda *args: public_context,
                              start_relay=lambda *args: None, rpc=lambda *args, **kwargs: None,
                              start_viewer_relay=lambda *args: server,
                              bridge=lambda local, channel: local.sendall(metadata + frame), Error=RuntimeError,
                              subprocess=subprocess)
def run_client(_arguments):
    assert fake_client.context({}, secret) is public_context
    fake_client.start_relay(secret, secret)
    relay = fake_client.start_viewer_relay(secret, secret)
    sink = RecordingSink()
    relay.process_request(sink, None)
    fake_client.bridge(sink, None)
    fake_client.rpc('release', token=secret)
    assert bytes(sink.sent) == metadata + frame
    return 0
fake_client.main = run_client
spec = SimpleNamespace(loader=SimpleNamespace(exec_module=lambda module: None))
with patch.object(capture.importlib.util, 'spec_from_file_location', return_value=spec), \
        patch.object(capture.importlib.util, 'module_from_spec', return_value=fake_client), \
        contextlib.redirect_stdout(io.StringIO()) as output:
    assert capture.main() == 0
summary = viewer.capture_summary(output.getvalue(), 0)
assert summary['complete_frames'] == 1 and summary['child_exit'] == 0
assert summary['timing']['slot'] == 2 and summary['timing']['api'] == 35
assert summary['timing']['first_frame_at_ms'] >= summary['timing']['metadata_at_ms'] > 0
assert secret not in output.getvalue() and 'private' not in output.getvalue()

scrcpy_log = ('INFO: Time limit reached\n'
              '[server] ERROR: Capture/encoding error: ' + secret + '\n'
              '[server] ERROR: Video encoding error\n'
              '[server] DEBUG: Screen streaming stopped\n'
              "ERROR: Demuxer 'video': stream configuration error on the device\n"
              'INFO: Time limit reached\n' + secret)
summary = viewer.capture_summary(scrcpy_log, 23)
assert summary['scrcpy_events'] == list(viewer.SCRCPY_EVENTS)
assert secret not in json.dumps(summary) and viewer.valid_viewer_summary(summary)
for events in ([secret], ['time_limit_reached'] * 2, [True], [{}], secret):
    assert not viewer.valid_viewer_summary(dict(summary, scrcpy_events=events))
assert 'scrcpy_events' not in viewer.capture_summary('INFO: Time limit reached ' + secret, 23)

for name, classification in [*viewer.ENCODERS.items(), (secret, 'other')]:
    summary = viewer.capture_summary(f"[server] DEBUG: Using video encoder: '{name}'\n", 23)
    assert summary['encoder'] == classification and secret not in json.dumps(summary)
    assert viewer.valid_viewer_summary(summary)
assert not viewer.valid_viewer_summary(dict(summary, encoder=secret))

# Stock screencap uses a streamed PNG. Only its header/end, byte count and exit
# are observed; pixels and arbitrary command errors never enter evidence.
def png_chunk(kind, data):
    return struct.pack('>I', len(data)) + kind + data + struct.pack('>I', zlib.crc32(kind + data))
def png(width=2):
    return (b'\x89PNG\r\n\x1a\n' + png_chunk(b'IHDR', struct.pack('>IIBBBBB', width, 1, 8, 6, 0, 0, 0))
            + png_chunk(b'IDAT', zlib.compress(b'\0' + b'\x01\x02\x03\x04' * 2)) + png_chunk(b'IEND', b''))
real_popen = subprocess.Popen
for data, code, expected in ((png(), 0, 'png_complete'), (png()[:-1], 0, 'malformed'),
                             (png(9000), 0, 'malformed'), (secret.encode(), 0, 'malformed'),
                             (png()[:29] + b'bad!' + png()[33:], 0, 'malformed'),
                             (png(), 23, 'command_failed')):
    def spawn(command, **kwargs):
        assert command == ['/srv/cache/android-sdk/platform-tools/adb', 'exec-out', 'screencap', '-p']
        assert kwargs['env']['ADB_SERVER_SOCKET'] == secret
        script = f'import os; os.write(1, {data!r}); raise SystemExit({code})'
        return real_popen([sys.executable, '-B', '-c', script], **kwargs)
    with patch.object(capture.subprocess, 'Popen', side_effect=spawn):
        evidence = capture.framebuffer_observation(public_context)
    assert evidence['status'] == expected and evidence['bytes'] == len(data)
    assert evidence['width'] == (2 if expected == 'png_complete' else 0)
    assert viewer.valid_framebuffer(evidence) and secret not in json.dumps(evidence)
def overflow(command, **kwargs):
    return real_popen([sys.executable, '-B', '-c', 'import os\nfor _ in range(260): os.write(1, b"x" * 65536)'], **kwargs)
with patch.object(capture.subprocess, 'Popen', side_effect=overflow):
    evidence = capture.framebuffer_observation(public_context)
assert evidence['status'] == 'limit' and viewer.valid_framebuffer(evidence)
def stalled(command, **kwargs):
    return real_popen([sys.executable, '-B', '-c', 'import time; time.sleep(60)'], **kwargs)
with patch.object(capture.subprocess, 'Popen', side_effect=stalled), \
        patch.object(capture.time, 'monotonic', side_effect=[0, 11]):
    evidence = capture.framebuffer_observation(public_context)
assert evidence['status'] == 'timeout' and viewer.valid_framebuffer(evidence)
with patch.object(capture.subprocess, 'Popen', side_effect=OSError(secret)):
    evidence = capture.framebuffer_observation(public_context)
assert evidence['status'] == 'unavailable' and secret not in json.dumps(evidence)

framebuffer = dict(status='png_complete', bytes=75, width=2, height=1,
                   memory_total_bytes=8589934592, memory_available_bytes=1073741824, swap_used_bytes=0,
                   started_at_ms=1000000, finished_at_ms=1001000)
summary = viewer.capture_summary('android-framebuffer ' + json.dumps(framebuffer), 23)
assert summary['framebuffer'] == framebuffer and summary['result'] == 'failed'
assert viewer.valid_viewer_summary(summary)
for invalid in (dict(framebuffer, extra=secret), dict(framebuffer, status=secret),
                dict(framebuffer, width=True), dict(framebuffer, bytes=45),
                dict(framebuffer, width=9000), dict(framebuffer, bytes=16 * 1024 * 1024 + 65537),
                dict(framebuffer, swap_used_bytes=secret), dict(framebuffer, memory_available_bytes=True),
                dict(framebuffer, memory_total_bytes=(1 << 60) + 1), dict(framebuffer, swap_used_bytes=-2),
                dict(framebuffer, finished_at_ms=999999), dict(framebuffer, status='timeout')):
    assert not viewer.valid_framebuffer(invalid)
    assert 'framebuffer' not in viewer.capture_summary('android-framebuffer ' + json.dumps(invalid), 23)
    assert not viewer.valid_viewer_summary(dict(summary, framebuffer=invalid))
with patch.object(capture.Path, 'read_text', return_value='MemTotal: 8388608 kB\nMemAvailable: 1048576 kB\nSwapTotal: 1024 kB\nSwapFree: 512 kB\n' + secret), \
        patch.object(capture.subprocess, 'Popen', side_effect=OSError(secret)):
    evidence = capture.framebuffer_observation(public_context)
assert evidence['memory_total_bytes'] == 8589934592 and evidence['memory_available_bytes'] == 1073741824
assert evidence['swap_used_bytes'] == 524288 and secret not in json.dumps(evidence)
with patch.object(capture.Path, 'read_text', side_effect=OSError(secret)), \
        patch.object(capture.subprocess, 'Popen', side_effect=OSError(secret)):
    evidence = capture.framebuffer_observation(public_context)
assert evidence['memory_total_bytes'] == evidence['memory_available_bytes'] == evidence['swap_used_bytes'] == -1
assert viewer.valid_framebuffer(evidence)

# Only one selected owned release is observed, while its relay is still alive.
# A failing observation never skips or changes the original release operation.
for payload, failure, calls in ((metadata, False, 1), (metadata, True, 1), (metadata + frame, False, 0)):
    releases = []
    relay = SimpleNamespace(process_request=lambda request, address: None, failure=None)
    fake_client = SimpleNamespace(context=lambda *args: public_context, start_relay=lambda *args: None,
                                 rpc=lambda operation, **values: releases.append((operation, values['token'])),
                                 start_viewer_relay=lambda *args: relay,
                                 bridge=lambda local, channel: local.sendall(payload), Error=RuntimeError,
                                 subprocess=subprocess)
    def main(_arguments):
        fake_client.context({}, secret)
        fake_client.start_relay(secret, secret)
        server = fake_client.start_viewer_relay(secret, secret)
        sink = RecordingSink(); server.process_request(sink, None); fake_client.bridge(sink, None)
        fake_client.rpc('release', token='other')
        fake_client.rpc('release', token=secret)
        fake_client.rpc('release', token=secret)
        return 0
    fake_client.main = main
    with patch.object(capture.importlib.util, 'spec_from_file_location', return_value=spec), \
            patch.object(capture.importlib.util, 'module_from_spec', return_value=fake_client), \
            patch.object(capture, 'framebuffer_observation', side_effect=RuntimeError(secret) if failure else None,
                         return_value=framebuffer) as probe, contextlib.redirect_stdout(io.StringIO()) as output:
        assert capture.main() == 0
    assert probe.call_count == calls
    assert releases == [('release', 'other'), ('release', secret), ('release', secret)]
    assert secret not in output.getvalue()
    summary = viewer.capture_summary(output.getvalue(), 0)
    assert ('framebuffer' in summary) == bool(calls and not failure)

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
    assert secret not in output.getvalue() and '"child_exit": "unknown"' in output.getvalue()

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
                    dict(attached, child_returncode=True), dict(attached, child_returncode=-65),
                    dict(attached, child_returncode=secret), dict(attached, child_returncode=-11),
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
    assert record['child_exit'] == 'unknown'
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
                            (['r', 's', 'y', 'p', 'i', '--viewer-native-debug'], '1 0'),
                            (['r', 's', 'y', 'p', 'i', '--recovery-only'], '0 1'),
                            (['r', 's', 'y', 'p', 'i', '--viewer-only', '--recovery-only'], None),
                            (['r', 's', 'y', 'p', 'i', '--unknown'], None)):
    script = 'set -euo pipefail\nfail() { exit 2; }\n' + mode_block + '\nprintf "%s %s\\n" "$viewer_only" "$recovery_only"'
    result = subprocess.run(['bash', '-c', script, '_', *arguments], capture_output=True, text=True)
    assert result.returncode == (0 if expected is not None else 2)
    if expected is not None:
        assert result.stdout.strip() == expected

remote_start = runtime_source.index('if [ "$lane" != viewer ] && [ "$lane" != viewer-native-debug ] && [ "$lane" != sdk-images ]; then\nandroid_phase_begin remote-owner-route')
remote_end = runtime_source.index('android_phase_begin images', remote_start)
images_start = remote_end
images_end = runtime_source.index('android_phase_end 0', images_start)
dispatch_start = runtime_source.index('viewer_args=()')
dispatch_end = runtime_source.index('android_phase_end 0', dispatch_start)
for lane, mode in (('full', ''), ('viewer', '--viewer-only'), ('recovery', '--recovery-only'), ('viewer-native-debug', '--viewer-native-debug'), ('sdk-images', '')):
    prelude = ('set -euo pipefail\nlane="$1"; root=/fixture; state=/fixture; YARD_NAME=fixture; project=fixture; instance=fixture\n'
               'android_phase_begin() { :; }; timeout() { printf "%s\\n" "$*"; }; yard() { printf "%s\\n" "$*"; }\n')
    result = subprocess.run(['bash', '-c', prelude + runtime_source[remote_start:remote_end], '_', lane], capture_output=True, text=True, check=True)
    assert ('android-pool-remote.sh' in result.stdout) == (lane not in ('viewer', 'viewer-native-debug', 'sdk-images'))
    result = subprocess.run(['bash', '-c', prelude + runtime_source[images_start:images_end], '_', lane], capture_output=True, text=True, check=True)
    assert result.stdout.count('emu cache prepare --api 35') == (0 if lane == 'sdk-images' else 1 if lane in ('viewer', 'viewer-native-debug') else 2)
    assert result.stdout.count('emu cache prepare --api 36') == 1
    if lane == 'sdk-images':
        # Execute the real early exit before any monitor/emulator/capture dispatch.
        end = runtime_source.index('# Monitor all viewer', images_end)
        result = subprocess.run(['bash', '-c', prelude + 'android_phase_end() { :; };\n'
                                 + runtime_source[images_start:end] + '\nprintf reached-after-images\n', '_', lane],
                                capture_output=True, text=True, check=True)
        assert result.stdout.splitlines() == ['android phase=prepare-images', 'emu cache prepare --api 36']
        continue
    result = subprocess.run(['bash', '-c', prelude + runtime_source[dispatch_start:dispatch_end], '_', lane], capture_output=True, text=True, check=True)
    assert 'android-pool-recovery.sh' in result.stdout
    assert ('--viewer-only' in result.stdout) == (mode == '--viewer-only')
    assert ('--recovery-only' in result.stdout) == (mode == '--recovery-only')
    assert ('--viewer-native-debug' in result.stdout) == (mode == '--viewer-native-debug')

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

# Diagnostic wrapping occurs at the actual client's Popen boundary, only for scrcpy.
dispatched = []
def dispatch(command, *args, **kwargs):
    dispatched.append((command, args, kwargs))
    return SimpleNamespace(returncode=139)
process_module = SimpleNamespace(Popen=dispatch)
native_client = SimpleNamespace(context=lambda *args: public_context,
                                start_relay=lambda *args: None, rpc=lambda *args, **kwargs: None,
                                start_viewer_relay=lambda *args: server,
                                bridge=lambda *args: None, Error=RuntimeError,
                                subprocess=process_module)
stock = ['scrcpy', '--time-limit=30', '--max-size=640', '--no-audio', '--verbosity=debug',
         '--no-control', '--port=1337', '--tunnel-host=127.0.0.1', '--tunnel-port=1337']
def run_native(arguments):
    assert arguments == ['view', '--lease-file', secret]
    values = native_client.context({}, secret)
    native_client.subprocess.Popen(stock, env=values, start_new_session=True)
    native_client.subprocess.Popen(['adb', 'version'], env=values)
    return 139
native_client.main = run_native
with patch.object(capture.importlib.util, 'spec_from_file_location', return_value=spec), \
        patch.object(capture.importlib.util, 'module_from_spec', return_value=native_client), \
        patch.object(capture.sys, 'argv', ['capture', '--native-debug', 'view', '--lease-file', secret]), \
        contextlib.redirect_stdout(io.StringIO()) as output:
    assert capture.main() == 139
assert process_module.Popen is dispatch
assert dispatched[0][0] == [sys.executable, capture.NATIVE_HELPER, '--gdb', '/usr/bin/gdb', '--', *stock]
assert dispatched[0][2]['env'] is public_context and dispatched[0][2]['start_new_session'] is True
assert dispatched[1][0] == ['adb', 'version'] and dispatched[1][2]['env'] is public_context
assert stock[0] == 'scrcpy' and secret not in output.getvalue()

# Ordinary captures use the bounded stock-client window, while non-viewer calls
# and the native-debug reproducer retain their original dispatch.
dispatched.clear()
stock_window = [part for part in stock if part != '--time-limit=30']
def run_window(arguments):
    assert arguments == ['view', '--lease-file', secret]
    native_client.subprocess.Popen(stock_window, env=public_context, start_new_session=True)
    native_client.subprocess.Popen(['adb', 'version'], env=public_context)
    return 23
native_client.main = run_window
with patch.object(capture.importlib.util, 'spec_from_file_location', return_value=spec), \
        patch.object(capture.importlib.util, 'module_from_spec', return_value=native_client), \
        patch.object(capture.sys, 'argv', ['capture', 'view', '--lease-file', secret]), \
        contextlib.redirect_stdout(io.StringIO()) as output:
    assert capture.main() == 23
assert process_module.Popen is dispatch
assert dispatched[0][0] == [sys.executable, capture.WINDOW_HELPER, '--', '/opt/subyard-e2e-scrcpy/scrcpy', *stock_window[1:]]
assert dispatched[0][2]['env'] is public_context and dispatched[0][2]['start_new_session'] is True
assert dispatched[1][0] == ['adb', 'version'] and secret not in output.getvalue()

native_stack = dict(signal=11, thread=42, frames=[dict(module='scrcpy', symbol='sc_native_crash', offset=1234)])
for native_result in (dict(inferior_returncode=-11, wrapper_status=139, diagnostic='complete'),
                      dict(inferior_returncode=None, wrapper_status=125, diagnostic='invalid_control')):
    raw = ('scrcpy-native-stack ' + json.dumps(native_stack) + '\n'
           + 'scrcpy-native-result ' + json.dumps(native_result) + '\n'
           + 'Android viewer: child returncode ' + str(native_result['wrapper_status']) + '\n')
    summary = viewer.capture_summary(raw, native_result['wrapper_status'])
    assert summary['native_debug'] == dict(stack=native_stack, result=native_result)
    assert summary['child_returncode'] == native_result['wrapper_status'] and viewer.valid_viewer_summary(summary)
    with tempfile.TemporaryDirectory() as directory:
        log = Path(directory) / 'native.log'
        log.write_text('android viewer evidence: ' + json.dumps(summary) + '\n' + secret)
        with contextlib.redirect_stdout(io.StringIO()) as projected:
            viewer.report_log(log, 1)
        assert secret not in projected.getvalue()
        record = json.loads(next(line.split(': ', 1)[1] for line in projected.getvalue().splitlines()
                                 if line.startswith('android viewer evidence: ')))
        assert record['native_debug'] == summary['native_debug']
for malformed in (dict(native_stack, secret=secret), dict(native_stack, thread=True),
                  dict(native_stack, frames=[dict(module='scrcpy', symbol=secret, offset=1)])):
    raw = 'scrcpy-native-stack ' + json.dumps(malformed) + '\nINFO: Texture: 640x360\n'
    summary = viewer.capture_summary(raw, 0)
    assert 'native_debug' not in summary and summary['result'] == 'failed'
    assert secret not in json.dumps(summary)
for malformed in (dict(inferior_returncode=None, wrapper_status=0, diagnostic='complete'),
                  dict(inferior_returncode=True, wrapper_status=0, diagnostic='complete'),
                  dict(inferior_returncode=-11, wrapper_status=0, diagnostic='complete'),
                  dict(inferior_returncode=0, wrapper_status=0, diagnostic='invalid_control'),
                  dict(inferior_returncode=None, wrapper_status=125, diagnostic=secret),
                  dict(inferior_returncode=-65, wrapper_status=193, diagnostic='complete')):
    summary = viewer.capture_summary('scrcpy-native-result ' + json.dumps(malformed) + '\nINFO: Texture: 640x360\n', 0)
    assert summary['result'] == 'failed' and 'native_debug' not in summary
    assert secret not in json.dumps(summary)
complete = dict(inferior_returncode=0, wrapper_status=0, diagnostic='complete')
summary = viewer.capture_summary('scrcpy-native-result ' + json.dumps(complete) + '\n', 0)
assert summary['result'] != 'passed'  # GDB success still requires the original Texture assertion.
summary = viewer.capture_summary('scrcpy-native-result ' + json.dumps(complete) + '\nINFO: Texture: 640x360\n', 0)
assert summary['result'] == 'passed' and viewer.valid_viewer_summary(summary)


# Run the actual owner PATH-shim setup with harmless executables and no VM calls.
owner_start = recovery_source.index('owner_viewer_path=')
owner_end = recovery_source.index('owner_lease=', owner_start)
with tempfile.TemporaryDirectory() as directory:
    fixture = Path(directory)
    work = fixture / 'work'
    tools = work / 'subyard-e2e-scrcpy'
    tools.mkdir(parents=True)
    stock_script = tools / 'scrcpy'
    stub = "#!/usr/bin/python3\nimport json,os,sys\nprint(json.dumps(dict(argv=sys.argv[1:], adb=os.environ['ADB'], renderer=os.environ['SDL_RENDER_DRIVER'])))\n"
    stock_script.write_text(stub)
    stock_script.chmod(0o755)
    fixture_helper = fixture / 'config/profiles/android/tests/helpers/scrcpy-native-debug.py'
    fixture_helper.parent.mkdir(parents=True)
    fixture_helper.write_text(stub)
    fixture_helper.with_name('scrcpy-view-window.py').write_text(stub)
    for diagnostic in ('0', '1'):
        script = ('set -euo pipefail\nroot="$1"; work="$2"; native_debug="$3"\n'
                  + recovery_source[owner_start:owner_end]
                  + '\nenv PATH="$owner_viewer_path" ADB=private-test-value SDL_RENDER_DRIVER=software scrcpy "${viewer_window_args[@]}" --max-size=640 --no-audio --verbosity=debug\n')
        result = subprocess.run(['bash', '-c', script, '_', str(fixture), str(work), diagnostic],
                                capture_output=True, text=True, check=True)
        record = json.loads(result.stdout)
        argv = (['--time-limit=30'] if diagnostic == '1' else []) + ['--max-size=640', '--no-audio', '--verbosity=debug']
        if diagnostic == '1':
            assert record['argv'][:3] == ['--gdb', '/usr/bin/gdb', '--']
            assert Path(record['argv'][3]).resolve() == stock_script
            assert record['argv'][4:] == argv
        else:
            assert record['argv'][0] == '--'
            assert Path(record['argv'][1]).resolve() == stock_script
            assert record['argv'][2:] == argv
        assert record['adb'] == 'private-test-value' and record['renderer'] == 'software'

# The viewer's real launch path preserves its deadline and fails without native completion.
for diagnostic, native_output, code, expected in (
        (False, '', 0, 0), (True, '', 0, 1),
        (True, 'scrcpy-native-result ' + json.dumps(complete) + '\n', 0, 0),
        (True, 'scrcpy-native-result ' + json.dumps(dict(inferior_returncode=-11, wrapper_status=139, diagnostic='complete')) + '\n', 139, 139)):
    def launch(command, **kwargs):
        assert command[:6] == ['xvfb-run', '-a', '-s', '-screen 0 1280x800x24 -nolisten tcp', sys.executable, '/opt/subyard-e2e-capture.py']
        assert command[6:] == ([ '--native-debug'] if diagnostic else []) + ['view', '--lease-file', secret, '--'] + (['--time-limit=30'] if diagnostic else []) + ['--max-size=640', '--no-audio', '--verbosity=debug']
        assert kwargs['env']['SDL_RENDER_DRIVER'] == 'software'
        kwargs['stdout'].write(('INFO: Texture: 640x360\n' + native_output).encode())
        def wait(timeout):
            assert timeout == 1320
            return code
        return SimpleNamespace(wait=wait)
    with patch.object(viewer.subprocess, 'Popen', side_effect=launch), contextlib.redirect_stdout(io.StringIO()) as projected:
        try:
            viewer.view(['--lease-file', secret], 'local-dispatch', native_debug=diagnostic)
            assert expected == 0
        except SystemExit as error:
            assert error.code == expected
    assert secret not in projected.getvalue()

print('ok: Android capture, display, privacy, bounds, lane and cleanup evidence')
