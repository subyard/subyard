#!/usr/bin/env python3
"""Real scrcpy acceptance inside the fixture yard, as its unprivileged user."""
import importlib.util
import contextlib
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import tempfile
import threading
import time

CLIENT = '/usr/local/lib/subyard-android/client.py'
client = None


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
    for line in output.decode('ascii', 'replace').splitlines():
        if re.fullmatch(r'INFO: Texture: [0-9]{1,4}x[0-9]{1,4}', line):
            summary['rendered'] = True
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
    if summary['child_exit'] == 'unknown' and not timed_out:
        summary['child_exit'] = returncode
    if returncode == 0 and summary['rendered'] is True and not timed_out:
        summary['result'] = 'passed'
    return summary


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def view(arguments, phase, delay_server=False):
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
                     sys.executable, '/opt/subyard-e2e-capture.py', 'view', *arguments, '--', '--time-limit=30',
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
    passed = code == 0 and summary['rendered'] is True and (not delay_server or delayed)
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
    require(os.getuid() == 1000 and len(sys.argv) == 2, 'requires dev and its private lease file')
    lease = client.read_lease(Path(sys.argv[1]))
    before = client.rpc('allocation', token=lease['token'])
    require(before['state'] == 'held', 'borrowed lease is not held')
    # Exercise slow server startup deterministically, without another emulator or boot.
    view(['--lease-file', sys.argv[1]], 'attached-capture', delay_server=True)
    after = client.rpc('allocation', token=lease['token'])
    require(after['state'] == 'held' and after['expires_at'] == before['expires_at'],
            'attached viewer released or renewed the borrowed lease')
    print('android viewer: attached rendered without release or renewal', flush=True)
    # The fixture owns this now-unviewed lease; let its display sleep during the next boot.
    result = subprocess.run(
        ['/srv/cache/android-sdk/platform-tools/adb', 'shell', 'input', 'keyevent', 'KEYCODE_SLEEP'],
        env=dict(os.environ, ADB_SERVER_SOCKET='localfilesystem:' + lease['endpoint'],
                 ANDROID_SERIAL=lease['allocation']['android_serial']),
        stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=30)
    require(result.returncode == 0, 'could not idle the borrowed display')
    stopped, failed, worker = heartbeat_borrowed(lease['token'])
    try:
        view(['--device', 'phone', '--api', '35', '--purpose', 'viewer-acceptance'], 'standalone-capture')
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
    if type(value) is not dict or set(value) not in (required, required | {'stream'}):
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
