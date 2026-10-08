#!/usr/bin/env python3
"""Real scrcpy acceptance inside the fixture yard, as its unprivileged user."""
import importlib.util
import contextlib
import json
import os
from pathlib import Path
import re
import runpy
import signal
import subprocess
import sys
import tempfile
import threading
import time

CLIENT = '/usr/local/lib/subyard-android/client.py'
client = None


def valid_capture_timing(value):
    if type(value) is not dict or set(value) != {'slot', 'generation', 'api', 'context_at_ms', 'metadata_at_ms', 'first_frame_at_ms', 'finished_at_ms'}:
        return False
    if not all(type(number) is int for number in value.values()):
        return False
    start, metadata, frame, end = (value[key] for key in ('context_at_ms', 'metadata_at_ms', 'first_frame_at_ms', 'finished_at_ms'))
    return (1 <= value['slot'] <= 999 and 1 <= value['generation'] <= 2147483647 and 1 <= value['api'] <= 99
            and 1 <= start <= end <= 4102444800000
            and (metadata == 0 or start <= metadata <= end)
            and (frame == 0 or metadata != 0 and metadata <= frame <= end))


SCRCPY_EVENTS = {
    'time_limit_reached': r'INFO: Time limit reached',
    'capture_encoding_error': r'\[server\] ERROR: Capture/encoding error: .{0,1024}',
    'video_encoding_error': r'\[server\] ERROR: Video encoding error',
    'screen_streaming_stopped': r'\[server\] DEBUG: Screen streaming stopped',
    'video_demuxer_error': r"ERROR: Demuxer 'video': stream (?:disabled due to connection error|configuration error on the device|disabled due to unsupported codec|disabled due to missing decoder)",
}

ENCODERS = {'c2.android.avc.encoder': 'android_avc', 'c2.goldfish.h264.encoder': 'goldfish_avc',
            'c2.ranchu.h264.encoder': 'ranchu_avc', 'OMX.google.h264.encoder': 'google_avc'}


def valid_framebuffer(value):
    if type(value) is not dict or set(value) != {'status', 'bytes', 'width', 'height', 'started_at_ms', 'finished_at_ms', 'memory_total_bytes', 'memory_available_bytes', 'swap_used_bytes'}:
        return False
    if value['status'] not in ('png_complete', 'malformed', 'command_failed', 'timeout', 'limit', 'unavailable'):
        return False
    if not all(type(value[key]) is int for key in value if key != 'status'):
        return False
    return (0 <= value['bytes'] <= 16 * 1024 * 1024 + 65536
            and all(-1 <= value[key] <= 1 << 60 for key in ('memory_total_bytes', 'memory_available_bytes', 'swap_used_bytes'))
            and 1 <= value['started_at_ms'] <= value['finished_at_ms'] <= 4102444800000
            and (value['status'] == 'png_complete' and value['bytes'] > 45
                 and 1 <= value['width'] <= 8192 and 1 <= value['height'] <= 8192
                 or value['status'] != 'png_complete' and value['width'] == value['height'] == 0))


_native_helper = None


def native_helper():
    global _native_helper
    if _native_helper is None:
        path = Path('/opt/subyard-e2e-native-debug.py')
        if not path.is_file():
            path = Path(__file__).resolve().parents[1] / 'helpers/scrcpy-native-debug.py'
        spec = importlib.util.spec_from_file_location('scrcpy_native_debug', path)
        _native_helper = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(_native_helper)
    return _native_helper


def valid_native_evidence(value):
    return (type(value) is dict and bool(value) and set(value) <= {'stack', 'result'}
            and ('stack' not in value or type(value['stack']) is dict
                 and set(value['stack']) == {'signal', 'thread', 'frames'}
                 and native_helper().valid_report(value['stack']))
            and ('result' not in value or native_helper().valid_native_result(value['result'])))


def native_passed(value):
    return 'result' in value and value['result'] == dict(
        inferior_returncode=0, wrapper_status=0, diagnostic='complete')


def capture_summary(output, returncode, timed_out=False):
    timed_out = timed_out or returncode in (124, 137)
    summary = dict(metadata='unknown', complete_frames='unknown', relay='unknown',
                   child_exit='unknown', viewer_exit=returncode,
                   rendered='unknown' if timed_out else False,
                   result='timeout' if timed_out else 'failed')
    if isinstance(output, str):
        output = output.encode()
    if len(output) > 262144:
        summary['output'] = 'limit'
        summary['rendered'] = 'unknown'
        return summary
    summary['output'] = 'bounded'
    native_invalid = False
    for line in output.decode('ascii', 'replace').splitlines():
        for prefix, field in (('scrcpy-native-stack ', 'stack'), ('scrcpy-native-result ', 'result')):
            if line.startswith(prefix):
                try:
                    native = json.loads(line[len(prefix):], object_pairs_hook=native_helper().unique_object)
                    if valid_native_evidence({field: native}):
                        summary.setdefault('native_debug', {})[field] = native
                    else:
                        native_invalid = True
                except (OSError, ValueError, TypeError, RecursionError):
                    native_invalid = True
        encoder = re.fullmatch(r"\[server\] DEBUG: Using video encoder: '([A-Za-z0-9_.-]{1,120})'", line)
        if encoder:
            summary['encoder'] = ENCODERS.get(encoder[1], 'other')
        if line.startswith('android-framebuffer '):
            try:
                framebuffer = json.loads(line[len('android-framebuffer '):])
                if valid_framebuffer(framebuffer):
                    summary['framebuffer'] = framebuffer
            except (ValueError, TypeError, RecursionError):
                pass
        for event, pattern in SCRCPY_EVENTS.items():
            if re.fullmatch(pattern, line):
                summary.setdefault('scrcpy_events', [])
                if event not in summary['scrcpy_events']:
                    summary['scrcpy_events'].append(event)
        if line.startswith('android-capture-timing '):
            try:
                timing = json.loads(line[len('android-capture-timing '):])
                if valid_capture_timing(timing):
                    summary['timing'] = timing
            except (ValueError, TypeError, RecursionError):
                pass
        if re.fullmatch(r'INFO: Texture: [0-9]{1,4}x[0-9]{1,4}', line):
            summary['rendered'] = True
        child = re.fullmatch(r'Android viewer: child returncode (-?[0-9]{1,3})', line)
        if child and -64 <= int(child[1]) <= 255:
            code = int(child[1])
            summary['child_returncode'] = code
            summary['child_exit'] = code if code >= 0 else 128 - code
        relay = re.fullmatch(r'Android viewer: (forward lookup|server socket|device transport|initial media byte) after [0-9]+\.[0-9]s: [A-Za-z0-9 :_-]{1,100}', line)
        if relay:
            summary['relay'] = relay[1].replace(' ', '_')
        if not line.startswith('android-capture '):
            continue
        try:
            value = json.loads(line[len('android-capture '):])
        except (ValueError, TypeError, RecursionError):
            continue
        if (type(value) is not dict or set(value) != {'metadata', 'complete_frames', 'stream', 'result', 'child_exit', 'relay'}
                or value['metadata'] not in ('pending', 'complete', 'malformed')
                or type(value['complete_frames']) is not int or not 0 <= value['complete_frames'] <= 10000000
                or value['stream'] not in ('observed', 'malformed')
                or value['result'] not in ('running', 'passed', 'failed')
                or not (value['child_exit'] == 'unknown' or type(value['child_exit']) is int and 0 <= value['child_exit'] <= 255)
                or value['relay'] not in ('unknown', 'no_failure_observed', 'forward_lookup', 'server_socket', 'device_transport', 'initial_media_byte')):
            continue
        for key in ('metadata', 'complete_frames', 'relay', 'child_exit'):
            summary[key] = value[key]
        summary['stream'] = value['stream']
    if returncode == 0 and summary['rendered'] is True and not timed_out:
        summary['result'] = 'passed'
    if (native_invalid or 'native_debug' in summary and not native_passed(summary['native_debug'])) and not timed_out:
        summary['result'] = 'failed'
        summary['viewer_exit'] = returncode or 1
    return summary


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def view(arguments, phase, delay_server=False, native_debug=False):
    started = time.monotonic()
    print(f"E2E_PHASE phase=fixture/viewer-{phase} state=start duration_seconds=0 exit_code=0", flush=True)
    with tempfile.TemporaryDirectory(prefix='subyard-viewer-check-') as directory:
        adb = '/srv/cache/android-sdk/platform-tools/adb'
        marker = Path(directory) / 'delayed'
        if delay_server:
            wrapper = Path(directory) / 'adb'
            wrapper.write_text('#!/usr/bin/python3\n'
                               'import os, sys, time\nfrom pathlib import Path\n'
                               "if any('com.genymobile.scrcpy.Server' in arg for arg in sys.argv[1:]):\n"
                               '    time.sleep(20)\n'
                               f'    Path({str(marker)!r}).touch()\n'
                               f'os.execv({adb!r}, [{adb!r}, *sys.argv[1:]])\n')
            wrapper.chmod(0o700)
            adb = str(wrapper)
        with tempfile.TemporaryFile() as output:
            timed_out = False
            try:
                process = subprocess.Popen(
                    ['xvfb-run', '-a', '-s', '-screen 0 1280x800x24 -nolisten tcp',
                     sys.executable, '/opt/subyard-e2e-capture.py', *(['--native-debug'] if native_debug else []), 'view', *arguments, '--', *(['--time-limit=30'] if native_debug else []),
                     '--max-size=640', '--no-audio', '--verbosity=debug'],
                    env=dict(os.environ, PATH='/opt/subyard-e2e-scrcpy:' + os.environ['PATH'],
                             ADB=adb, SDL_RENDER_DRIVER='software'),
                    stdin=subprocess.DEVNULL, stdout=output, stderr=subprocess.STDOUT, start_new_session=True)
                result = process.wait(timeout=1320)
                code = result if result >= 0 else 128 - result
            except subprocess.TimeoutExpired:
                timed_out, code = True, 124
                with contextlib.suppress(ProcessLookupError):
                    os.killpg(process.pid, signal.SIGTERM)
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    with contextlib.suppress(ProcessLookupError):
                        os.killpg(process.pid, signal.SIGKILL)
                    process.wait(timeout=5)
            output.seek(0)
            summary = capture_summary(output.read(262145), code, timed_out)
        delayed = marker.exists()
    passed = (code == 0 and summary['result'] == 'passed' and summary['rendered'] is True
              and (not delay_server or delayed)
              and (not native_debug or native_passed(summary.get('native_debug', {}))))
    exit_code = code if code else 0 if passed else 1
    summary['viewer_exit'] = exit_code
    if not passed and summary['result'] != 'timeout':
        summary['result'] = 'failed'
    print('android viewer evidence: ' + json.dumps(summary, sort_keys=True), flush=True)
    print(f"E2E_PHASE phase=fixture/viewer-{phase} state=end duration_seconds={int(time.monotonic() - started)} exit_code={exit_code}", flush=True)
    if not passed:
        raise SystemExit(exit_code)


def heartbeat_borrowed(token):
    stopped, failed = threading.Event(), threading.Event()

    def renew():
        while not stopped.wait(45):
            try:
                client.rpc('renew', timeout=30, token=token)
            except (client.Error, OSError, ValueError):
                failed.set()
                return

    # The attached-view assertion has already proved that the viewer itself did not renew.
    client.rpc('renew', timeout=30, token=token)
    worker = threading.Thread(target=renew, daemon=True)
    worker.start()
    return stopped, failed, worker


def main():
    global client
    spec = importlib.util.spec_from_file_location('android_client', CLIENT)
    client = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(client)
    native_debug = sys.argv[2:] == ['--native-debug']
    require(os.getuid() == 1000 and (len(sys.argv) == 2 or native_debug), 'requires dev and its private lease file')
    lease = client.read_lease(Path(sys.argv[1]))
    before = client.rpc('allocation', token=lease['token'])
    require(before['state'] == 'held', 'borrowed lease is not held')
    # Exercise slow server startup deterministically, without another emulator or boot.
    view(['--lease-file', sys.argv[1]], 'attached-capture', delay_server=True, native_debug=native_debug)
    after = client.rpc('allocation', token=lease['token'])
    require(after['state'] == 'held' and after['expires_at'] == before['expires_at'],
            'attached viewer released or renewed the borrowed lease')
    print('android viewer: attached rendered without release or renewal', flush=True)
    stopped, failed, worker = heartbeat_borrowed(lease['token'])
    try:
        runpy.run_path('/opt/subyard-e2e-lifecycle.py')['idle_display'](lease)
        require(client.rpc('allocation', token=lease['token'])['state'] == 'held',
                'idle display did not retain its borrowed lease')
        print('android viewer: idle display asleep; borrowed lease held', flush=True)
        view(['--device', 'phone', '--api', '35', '--purpose', 'viewer-acceptance'], 'standalone-capture', native_debug=native_debug)
    finally:
        stopped.set()
        worker.join(timeout=35)
    require(not worker.is_alive() and not failed.is_set(), 'borrowed lease renewal failed')
    require(client.rpc('allocation', token=lease['token'])['state'] == 'held',
            'standalone viewer affected the borrowed lease')
    slots = client.rpc('status')['slots']
    require(sum(slot['state'] == 'held' for slot in slots) == 1 and
            sum(slot['state'] == 'available' for slot in slots) == len(slots) - 1,
            'standalone viewer did not release its own lease')
    print('android viewer: standalone rendered and released only its own lease', flush=True)


def valid_viewer_summary(value):
    required = {'metadata', 'complete_frames', 'relay', 'child_exit', 'viewer_exit', 'rendered', 'result', 'output'}
    if type(value) is not dict or not required <= set(value) <= required | {'stream', 'child_returncode', 'timing', 'scrcpy_events', 'encoder', 'framebuffer', 'native_debug'}:
        return False
    return (value['metadata'] in ('unknown', 'pending', 'complete', 'malformed')
            and (value['complete_frames'] == 'unknown' or type(value['complete_frames']) is int and 0 <= value['complete_frames'] <= 10000000)
            and value['relay'] in ('unknown', 'no_failure_observed', 'forward_lookup', 'server_socket', 'device_transport', 'initial_media_byte')
            and (value['child_exit'] == 'unknown' or type(value['child_exit']) is int and 0 <= value['child_exit'] <= 255)
            and type(value['viewer_exit']) is int and 0 <= value['viewer_exit'] <= 255
            and (type(value['rendered']) is bool or value['rendered'] == 'unknown')
            and value['result'] in ('passed', 'failed', 'timeout')
            and value['output'] in ('bounded', 'limit')
            and value.get('stream', 'observed') in ('observed', 'malformed')
            and value.get('encoder', 'other') in (*ENCODERS.values(), 'other')
            and ('native_debug' not in value or valid_native_evidence(value['native_debug'])
                 and (value['result'] != 'passed' or native_passed(value['native_debug'])))
            and ('framebuffer' not in value or valid_framebuffer(value['framebuffer']))
            and ('timing' not in value or valid_capture_timing(value['timing']))
            and ('scrcpy_events' not in value or type(value['scrcpy_events']) is list
                 and len(value['scrcpy_events']) <= len(SCRCPY_EVENTS)
                 and all(type(event) is str and event in SCRCPY_EVENTS for event in value['scrcpy_events'])
                 and len(set(value['scrcpy_events'])) == len(value['scrcpy_events']))
            and ('child_returncode' not in value or type(value['child_returncode']) is int
                 and -64 <= value['child_returncode'] <= 255
                 and value['child_exit'] == (value['child_returncode'] if value['child_returncode'] >= 0
                                             else 128 - value['child_returncode']))
            and (value['result'] != 'passed' or value['viewer_exit'] == 0 and value['rendered'] is True)
            and (value['result'] != 'failed' or value['viewer_exit'] != 0)
            and (value['result'] != 'timeout' or value['viewer_exit'] in (124, 137)))


def report_log(path, code):
    with open(path, 'rb') as source:
        output = source.read(262145)
    lines = output.decode('ascii', 'replace').splitlines()
    limited = len(output) > 262144
    compound = any(line.startswith(('android viewer evidence: ', 'E2E_PHASE phase=fixture/viewer-')) for line in lines)
    if compound or limited:
        # Compound logs contain observations already projected by each capture.
        # The enclosing exit is independent; incomplete output proves no absence.
        print('android fixture evidence: ' + json.dumps(dict(fixture_exit=code,
              output='limit' if limited else 'bounded'), sort_keys=True), flush=True)
    else:
        print('android capture evidence: ' + json.dumps(capture_summary(output, code), sort_keys=True), flush=True)
    if limited:
        return
    for line in lines:
        if re.fullmatch(r'E2E_PHASE phase=fixture/viewer-[a-z0-9-]{1,40} state=(?:start|end) duration_seconds=[0-9]{1,6} exit_code=[0-9]{1,3}', line):
            print(line, flush=True)
        prefix = 'android viewer evidence: '
        if line.startswith(prefix):
            try:
                value = json.loads(line[len(prefix):])
                # Each record belongs to its own capture; the enclosing fixture
                # may fail later without changing an earlier successful capture.
                if valid_viewer_summary(value):
                    print('android viewer evidence: ' + json.dumps(value, sort_keys=True), flush=True)
            except (ValueError, TypeError, RecursionError):
                continue


if __name__ == '__main__':
    if len(sys.argv) == 4 and sys.argv[1] == '--summary' and re.fullmatch(r'[0-9]{1,3}', sys.argv[3]):
        report_log(sys.argv[2], int(sys.argv[3]))
        raise SystemExit(0)
    try:
        main()
    except Exception as exc:
        # No lease content or arbitrary process/OS error text is printed.
        detail = str(exc) if isinstance(exc, RuntimeError) else type(exc).__name__
        print(f'android viewer: FAIL ({detail})', file=sys.stderr)
        raise SystemExit(1)
