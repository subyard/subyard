#!/usr/bin/env python3
"""Real scrcpy acceptance inside the fixture yard, as its unprivileged user."""
import importlib.util
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import threading

CLIENT = '/usr/local/lib/subyard-android/client.py'
spec = importlib.util.spec_from_file_location('android_client', CLIENT)
client = importlib.util.module_from_spec(spec)
spec.loader.exec_module(client)


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def view(arguments, delay_server=False):
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
        result = subprocess.run(
            ['xvfb-run', '-a', '-s', '-screen 0 1280x800x24 -nolisten tcp',
             sys.executable, CLIENT, 'view', *arguments, '--', '--time-limit=30',
             '--max-size=640', '--no-audio', '--verbosity=debug'],
            env=dict(os.environ, PATH='/opt/subyard-e2e-scrcpy:' + os.environ['PATH'],
                     ADB=adb, SDL_RENDER_DRIVER='software'),
            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
            text=True, timeout=1320)
        delayed = marker.exists()
    if result.returncode != 0 or 'Texture:' not in result.stdout:
        # Keep private raw output for a retained-VM investigation. The controller
        # receives only the filtered excerpt, never raw commands or connection data.
        fd, diagnostic = tempfile.mkstemp(prefix='subyard-viewer-failure-', suffix='.log')
        with os.fdopen(fd, 'w') as output:
            output.write(result.stdout)
        print('android viewer: private diagnostic=' + diagnostic, file=sys.stderr, flush=True)
        diagnostics = []
        for line in result.stdout.splitlines():
            if re.match(r'^(?:Android [a-z_]+:|ERROR:|WARN:|INFO:|'
                        r'DEBUG: (?:Remaining connection attempts:|Server terminated)|'
                        r'\[server\] (?:ERROR|WARN|INFO):)', line):
                line = re.sub(r'(?i)(token|credential|secret|password|authorization)[=: ]+\S+',
                              '[redacted]', line)
                line = re.sub(r'\b[0-9a-fA-F]{64}\b', '[redacted]', line)
                diagnostics.append(line[:500])
        # Keep initial server errors as well as the final relay failure; scrcpy's
        # connection retry messages must not displace the actual cause.
        if len(diagnostics) > 60:
            diagnostics = diagnostics[:20] + diagnostics[-40:]
        for line in diagnostics:
            print('android viewer diagnostic: ' + line, file=sys.stderr, flush=True)
    require(result.returncode == 0, f'viewer exited {result.returncode}')
    require('Texture:' in result.stdout, 'viewer did not render an Android video frame')
    require(not delay_server or delayed, 'server startup delay was not exercised')


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
    require(os.getuid() == 1000 and len(sys.argv) == 2, 'requires dev and its private lease file')
    lease = client.read_lease(Path(sys.argv[1]))
    before = client.rpc('allocation', token=lease['token'])
    require(before['state'] == 'held', 'borrowed lease is not held')
    # Exercise slow server startup deterministically, without another emulator or boot.
    view(['--lease-file', sys.argv[1]], delay_server=True)
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
        view(['--device', 'phone', '--api', '35', '--purpose', 'viewer-acceptance'])
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


if __name__ == '__main__':
    try:
        main()
    except (RuntimeError, client.Error, OSError, subprocess.TimeoutExpired) as exc:
        # No lease content or viewer output containing connection details is printed.
        detail = str(exc) if isinstance(exc, RuntimeError) else type(exc).__name__
        print(f'android viewer: FAIL ({detail})', file=sys.stderr)
        raise SystemExit(1)
