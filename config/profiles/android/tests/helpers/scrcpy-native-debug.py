#!/usr/bin/env python3
"""Fixture-only: --gdb GDB -- scrcpy ARGS; no raw debugger output or core files."""
import argparse
import ctypes
import errno
import json
import os
from pathlib import Path
import pty
import re
import resource
import selectors
import signal
import subprocess
import sys
import tempfile
import time

# Runs inside GDB. No evaluation of inferior expressions, arguments or locals.
GDB_PYTHON = r'''
import gdb, json, os, re
fd = CONTROL_FD
last_signal = 0
result = None

def emit(value):
    os.write(fd, (json.dumps(value, separators=(',', ':')) + '\n').encode())

def running(event):
    emit({'pid': gdb.selected_inferior().pid})

def exited(event):
    global result
    result = getattr(event, 'exit_code', -last_signal if last_signal else 125)

def stopped(event):
    global last_signal
    if not isinstance(event, gdb.SignalEvent):
        return
    last_signal = {'SIGSEGV': 11, 'SIGABRT': 6, 'SIGBUS': 7, 'SIGILL': 4,
                   'SIGFPE': 8, 'SIGTERM': 15, 'SIGINT': 2, 'SIGKILL': 9}.get(event.stop_signal, 0)
    emit({'collecting': last_signal})
    if last_signal != 11:
        emit({'collected': True})
        return
    thread = gdb.selected_thread()
    frames = []
    mappings = []
    with open('/proc/%d/maps' % gdb.selected_inferior().pid) as source:
        for line in source:
            fields = line.split(None, 5)
            if len(fields) == 6 and fields[5].startswith('/'):
                lo, hi = (int(v, 16) for v in fields[0].split('-'))
                mappings.append((lo, hi, int(fields[2], 16), fields[5].strip()))
    frame = gdb.newest_frame()
    while frame is not None and len(frames) < 12:
        pc = frame.pc()
        module, offset = 'unknown', -1
        for lo, hi, file_offset, path in mappings:
            if lo <= pc < hi:
                base = min(a - c for a, b, c, d in mappings if d == path)
                offset = pc - base
                name = os.path.basename(path)
                module = ('scrcpy' if name == 'scrcpy' else 'SDL' if name.startswith('libSDL')
                          else 'FFmpeg' if name.startswith(('libav', 'libsw'))
                          else 'X11' if name.startswith(('libX11', 'libxcb'))
                          else 'libc' if name.startswith('libc.so') else 'other')
                break
        name = frame.name() or 'unknown'
        if not re.fullmatch(r'(?:sc_|SDL_|av_|avcodec_|avformat_|sws_|swr_|net_|__libc_|__GI_)[A-Za-z0-9_]{1,100}|main|raise|unknown', name):
            name = 'unknown'
        frames.append({'module': module, 'symbol': name, 'offset': offset})
        frame = frame.older()
    emit({'signal': 11, 'thread': thread.ptid[1] or thread.ptid[0], 'frames': frames})
    emit({'collected': True})

gdb.events.cont.connect(running)
gdb.events.stop.connect(stopped)
gdb.events.exited.connect(exited)
try:
    gdb.execute('run', to_string=True)
    while result is None and gdb.selected_inferior().pid:
        gdb.execute('continue', to_string=True)
except gdb.error:
    pass
emit({'result': result if result is not None else 125})
'''


def safe_line(line):
    if re.fullmatch(r'INFO: Texture: [0-9]{1,4}x[0-9]{1,4}', line):
        return line
    if line in ('INFO: Time limit reached', '[server] ERROR: Video encoding error',
                '[server] DEBUG: Screen streaming stopped'):
        return line
    encoder = re.fullmatch(r"\[server\] DEBUG: Using video encoder: '(c2\.android\.avc\.encoder|c2\.goldfish\.h264\.encoder|c2\.ranchu\.h264\.encoder|OMX\.google\.h264\.encoder)'", line)
    if encoder:
        return line
    if line.startswith('[server] ERROR: Capture/encoding error:'):
        return '[server] ERROR: Capture/encoding error: classified'
    if re.fullmatch(r"ERROR: Demuxer 'video': stream (?:disabled due to connection error|configuration error on the device|disabled due to unsupported codec|disabled due to missing decoder)", line):
        return line
    return None


SYMBOL = r'(?:sc_|SDL_|av_|avcodec_|avformat_|sws_|swr_|net_|__libc_|__GI_)[A-Za-z0-9_]{1,100}|main|raise|unknown'
MODULES = ('scrcpy', 'SDL', 'FFmpeg', 'X11', 'libc', 'other', 'unknown')


def valid_report(value):
    if type(value) is not dict:
        return False
    def integer(number, low, high):
        return type(number) is int and low <= number <= high
    keys = set(value)
    if keys == {'pid'}:
        return integer(value['pid'], 2, 2147483647)
    if keys == {'collecting'}:
        return integer(value['collecting'], 1, 64)
    if keys == {'collected'}:
        return value['collected'] is True
    if keys == {'result'}:
        return integer(value['result'], -64, 255)
    return (keys == {'signal', 'thread', 'frames'} and type(value['signal']) is int
            and value['signal'] == 11 and integer(value['thread'], 1, 2147483647)
            and type(value['frames']) is list and len(value['frames']) <= 12
            and all(type(frame) is dict and set(frame) == {'module', 'symbol', 'offset'}
                    and frame['module'] in MODULES and type(frame['symbol']) is str
                    and re.fullmatch(SYMBOL, frame['symbol'])
                    and integer(frame['offset'], -1, (1 << 64) - 1)
                    for frame in value['frames']))


def valid_native_result(value):
    if type(value) is not dict or set(value) != {'inferior_returncode', 'wrapper_status', 'diagnostic'}:
        return False
    code, status, diagnostic = (value[key] for key in ('inferior_returncode', 'wrapper_status', 'diagnostic'))
    if type(status) is not int or not 0 <= status <= 255 or diagnostic not in (
            'complete', 'cancelled', 'stack_deadline', 'debugger_unavailable',
            'debugger_exit', 'missing_result', 'invalid_control'):
        return False
    if code is None:
        return status != 0 and diagnostic != 'complete'
    if type(code) is not int or not -64 <= code <= 255:
        return False
    return (status == (code if code >= 0 else 128 - code) if diagnostic == 'complete' else status != 0)


def unique_object(pairs):
    value = dict(pairs)
    if len(value) != len(pairs):
        raise ValueError('duplicate control field')
    return value


def identity(pid):
    # Only PPID/start time; never read command lines or export proc contents.
    fields = Path('/proc/%d/stat' % pid).read_text().rsplit(') ', 1)[1].split()
    return int(fields[1]), int(fields[19])


def owned_pidfd(pid, debugger, born):
    before = identity(pid)
    if pid == debugger.pid or before[1] < born or before[0] not in (debugger.pid, os.getpid()):
        raise ValueError('unowned control PID')
    fd = os.pidfd_open(pid)
    try:
        if identity(pid) != before:
            raise ValueError('changed control PID')
        return fd
    except Exception:
        os.close(fd)
        raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--gdb', default='gdb')
    parser.add_argument('command', nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command[1:] if args.command[:1] == ['--'] else args.command
    if not command:
        parser.error('a command is required')
    cancelled = [0]
    def cancel(number, frame):
        cancelled[0] = number
    for number in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
        signal.signal(number, cancel)
    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
    master, slave = pty.openpty()
    read_fd, write_fd = os.pipe()
    inferior = 0
    inferior_fd = -1
    result = None
    inferior_result = None
    observed_signal = 0
    stack_emitted = False
    collection_deadline = None
    failure = None
    process = None
    try:
        # This process alone adopts/reaps its orphaned debugger descendants.
        if ctypes.CDLL(None, use_errno=True).prctl(36, 1, 0, 0, 0) != 0:
            raise OSError('subreaper unavailable')
        with tempfile.TemporaryDirectory(prefix='scrcpy-native-debug-') as directory:
            script = Path(directory) / 'commands.gdb'
            script.write_text('set pagination off\nset confirm off\nset auto-load off\n'
                              'set debuginfod enabled off\nset disable-randomization off\n'
                              'set startup-with-shell off\nset inferior-tty ' + os.ttyname(slave) + '\n'
                              'handle SIGSEGV noprint stop pass\npython\n' +
                              GDB_PYTHON.replace('CONTROL_FD', str(write_fd)) + '\nend\n')
            script.chmod(0o600)
            process = subprocess.Popen([args.gdb, '-nx', '-nh', '-batch',
                                        '-iex', 'set auto-load off', '-iex', 'set debuginfod enabled off', '-x', str(script),
                                        '--args', *command], stdin=subprocess.DEVNULL,
                                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                       pass_fds=(write_fd,), start_new_session=True)
            born = identity(process.pid)[1]
            os.close(write_fd)
            write_fd = -1
            # Keep the slave open until launch ends: a pre-launch PTY EIO is not EOF.
            pending = {master: b'', read_fd: b''}
            output_bytes = 0
            with selectors.DefaultSelector() as selector:
                for fd in pending:
                    os.set_blocking(fd, False)
                    selector.register(fd, selectors.EVENT_READ)
                while selector.get_map():
                    if cancelled[0]:
                        failure = 'cancelled'
                        break
                    if collection_deadline is not None and time.monotonic() >= collection_deadline:
                        failure = 'stack_deadline'
                        break
                    for key, mask in selector.select(0.05):
                        fd = key.fd
                        try:
                            data = os.read(fd, 4096)
                        except OSError as error:
                            if error.errno != errno.EIO:
                                raise
                            data = b''
                        if not data:
                            selector.unregister(fd)
                            continue
                        pending[fd] += data
                        while b'\n' in pending[fd]:
                            line, pending[fd] = pending[fd].split(b'\n', 1)
                            if fd == master:
                                safe = safe_line(line.decode('ascii', 'replace').rstrip('\r'))
                                if safe and output_bytes + len(safe) < 65536:
                                    print(safe, flush=True)
                                    output_bytes += len(safe) + 1
                            else:
                                try:
                                    value = json.loads(line, object_pairs_hook=unique_object)
                                    if not valid_report(value):
                                        raise ValueError('invalid control record')
                                except (ValueError, TypeError, RecursionError):
                                    failure = 'invalid_control'
                                    raise ValueError('invalid control record')
                                if 'pid' in value:
                                    if inferior and inferior != value['pid']:
                                        failure = 'invalid_control'
                                        raise ValueError('changed control PID')
                                    if inferior_fd < 0:
                                        try:
                                            inferior_fd = owned_pidfd(value['pid'], process, born)
                                        except FileNotFoundError:
                                            continue  # A fast normal inferior may already be reaped.
                                        except (OSError, ValueError):
                                            failure = 'invalid_control'
                                            raise ValueError('unowned control PID')
                                    inferior = value['pid']
                                elif 'collecting' in value:
                                    observed_signal = value['collecting']
                                    collection_deadline = time.monotonic() + 5
                                elif 'collected' in value:
                                    collection_deadline = None
                                elif 'signal' in value and not stack_emitted:
                                    print('scrcpy-native-stack ' + json.dumps(value, sort_keys=True), flush=True)
                                    stack_emitted = True
                                elif 'result' in value:
                                    result = inferior_result = value['result']
                        if len(pending[fd]) > 8192:
                            if fd == read_fd:
                                failure = 'invalid_control'
                                raise ValueError('oversized control record')
                            pending[fd] = b''
                    if process.poll() is not None and not selector.select(0):
                        break
            if not failure and process.poll() not in (None, 0):
                failure = 'debugger_exit'
            if failure:
                result = -cancelled[0] if cancelled[0] else -observed_signal if observed_signal else 125
            elif result is None:
                result, failure = 125, 'missing_result'
    except (OSError, ValueError):
        result, failure = 125, failure or 'debugger_unavailable'
    finally:
        # Stable handles prevent PID reuse; adopted children can also be reaped.
        if process is not None:
            for number in (signal.SIGTERM, signal.SIGKILL):
                if inferior_fd >= 0:
                    try:
                        signal.pidfd_send_signal(inferior_fd, number)
                    except ProcessLookupError:
                        pass
                if process.poll() is None:
                    try:
                        if number == signal.SIGKILL or inferior_fd < 0:
                            os.killpg(process.pid, number)
                        process.wait(timeout=1)
                    except (ProcessLookupError, subprocess.TimeoutExpired):
                        pass
                if process.poll() is not None and inferior_fd >= 0:
                    deadline = time.monotonic() + 1
                    while time.monotonic() < deadline:
                        try:
                            if os.waitpid(inferior, os.WNOHANG)[0]:
                                break
                        except ChildProcessError:
                            break  # GDB already reaped its child.
                        time.sleep(.01)
            if process.poll() is None:
                process.wait(timeout=1)
        if inferior_fd >= 0:
            os.close(inferior_fd)
        for fd in (master, slave, read_fd, write_fd):
            if fd >= 0:
                os.close(fd)
    status = result if 0 <= result <= 255 else 128 - result if -64 <= result < 0 else 125
    print('scrcpy-native-result ' + json.dumps({'inferior_returncode': inferior_result,
          'wrapper_status': status, 'diagnostic': failure or 'complete'}, sort_keys=True), flush=True)
    return status


if __name__ == '__main__':
    sys.exit(main())
