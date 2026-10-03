#!/usr/bin/env python3
"""Fixture-only stock scrcpy observation window; never alter native failures."""
import contextlib
import errno
import json
import math
import os
import pty
import re
import selectors
import signal
import subprocess
import sys
import termios
import time

# Official v4.1 starts its timeout thread before publishing its callbacks.
# Keep stock rendering, but request SDL quit externally after connection.
# https://github.com/Genymobile/scrcpy/blob/v4.1/app/src/util/timeout.c
MARKER = b'DEBUG: Server connected'


def safe_line(line):
    if line in (MARKER, b'DEBUG: User requested to quit',
                b'[server] ERROR: Video encoding error',
                b'[server] DEBUG: Screen streaming stopped'):
        return line.decode('ascii')
    if re.fullmatch(rb'INFO: Texture: [0-9]{1,4}x[0-9]{1,4}', line):
        return line.decode('ascii')
    if re.fullmatch(rb"\[server\] DEBUG: Using video encoder: '(?:c2\.android\.avc\.encoder|c2\.goldfish\.h264\.encoder|c2\.ranchu\.h264\.encoder|OMX\.google\.h264\.encoder)'", line):
        return line.decode('ascii')
    if line.startswith(b'[server] ERROR: Capture/encoding error:'):
        return '[server] ERROR: Capture/encoding error: classified'
    return None


def run(command, seconds=30, shutdown_seconds=5):
    if (type(command) is not list or not command or
            any(type(arg) is not str or not arg or '\0' in arg for arg in command) or
            not os.path.isabs(command[0]) or
            any(type(value) not in (int, float) or not math.isfinite(value) or value <= 0
                for value in (seconds, shutdown_seconds))):
        return 125
    cancelled = [0]
    def cancel(number, frame):
        cancelled[0] = number
    handlers = {number: signal.signal(number, cancel)
                for number in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP)}
    master = slave = pidfd = -1
    process = None
    connected = stopping = None
    requested = False
    native = None
    status = 125
    failed = False
    pending = b''
    dropping = False
    try:
        master, slave = pty.openpty()
        attrs = termios.tcgetattr(slave)
        attrs[1] &= ~termios.ONLCR
        termios.tcsetattr(slave, termios.TCSANOW, attrs)
        process = subprocess.Popen(command, stdin=subprocess.DEVNULL,
                                   stdout=slave, stderr=slave, start_new_session=True)
        os.close(slave)
        slave = -1
        if hasattr(os, 'pidfd_open'):
            try:
                pidfd = os.pidfd_open(process.pid)
            except ProcessLookupError:
                pass  # A fast native exit is classified by wait/reap below.
            except OSError as exc:
                if exc.errno not in (errno.ENOSYS, errno.EINVAL):
                    raise
        os.set_blocking(master, False)
        with selectors.DefaultSelector() as selector:
            selector.register(master, selectors.EVENT_READ)
            while True:
                # Keep the child unreaped until group cleanup: its PID/PGID cannot
                # be reused for an unrelated process while it remains our zombie.
                if os.waitid(os.P_PID, process.pid, os.WEXITED | os.WNOHANG | os.WNOWAIT):
                    break
                now = time.monotonic()
                if cancelled[0] and stopping is None:
                    try:
                        os.killpg(process.pid, signal.SIGTERM)
                    except ProcessLookupError:
                        continue
                    stopping = now
                elif connected is not None and not requested and stopping is None and now >= connected + seconds:
                    try:
                        if pidfd >= 0 and hasattr(signal, 'pidfd_send_signal'):
                            signal.pidfd_send_signal(pidfd, signal.SIGTERM)
                        else:
                            os.kill(process.pid, signal.SIGTERM)
                    except ProcessLookupError:
                        continue  # Native exit won the race; preserve its signed result.
                    requested, stopping = True, now
                if stopping is not None and now >= stopping + shutdown_seconds:
                    status = 128 + cancelled[0] if cancelled[0] else 124
                    break
                if not selector.select(0.02):
                    continue
                for _ in range(4):  # At most 64 KiB before checking timers/cancellation.
                    try:
                        data = os.read(master, 16384)
                    except BlockingIOError:
                        break
                    except OSError as exc:
                        if exc.errno != errno.EIO:
                            raise
                        data = b''
                    if not data:
                        selector.unregister(master)
                        break
                    parts = data.split(b'\n')
                    for index, part in enumerate(parts):
                        if not dropping:
                            if len(pending) + len(part) <= 4096:
                                pending += part
                            else:
                                pending, dropping = b'', True
                        if index == len(parts) - 1:
                            continue
                        if not dropping:
                            if pending == MARKER and connected is None:
                                connected = time.monotonic()
                            line = safe_line(pending)
                            if line is not None:
                                print(line, flush=True)
                        pending, dropping = b'', False
    except (OSError, ValueError):
        failed = True
        status = 125
    finally:
        if process is not None:
            # Our unreaped session leader fences this group identity even if it
            # exited before its descendants. Only this newly created group dies.
            with contextlib.suppress(ProcessLookupError):
                os.killpg(process.pid, signal.SIGKILL)
            native = process.wait()
            if cancelled[0]:
                status = 128 + cancelled[0]
            elif not failed and status != 124:
                status = native if native > 0 else 128 - native if native < 0 else 0 if requested else 1
        for fd in (master, slave, pidfd):
            if fd >= 0:
                os.close(fd)
        for number, handler in handlers.items():
            signal.signal(number, handler)
    print('scrcpy-view-window ' + json.dumps(dict(native_returncode=native,
          window_completed=requested and native == 0 and not cancelled[0] and status == 0,
          wrapper_status=status), sort_keys=True), flush=True)
    return status


def main():
    if len(sys.argv) < 3 or sys.argv[1] != '--':
        return 125
    return run(sys.argv[2:])


if __name__ == '__main__':
    raise SystemExit(main())
