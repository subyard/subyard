#!/usr/bin/env python3
"""Give a fixture one separate, fully disclosed migration confirmation."""
import argparse
import errno
import math
import os
import pty
import select
import signal
import sys
import time

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--yard', required=True)
parser.add_argument('--answer', choices=('yes', 'no', 'eof'), default='yes')
parser.add_argument('--timeout', type=float, default=900)
parser.add_argument('--allow-no-prompt', action='store_true')
parser.add_argument('command', nargs=argparse.REMAINDER)
args = parser.parse_args()
command = args.command[1:] if args.command[:1] == ['--'] else args.command
if not command or not math.isfinite(args.timeout) or args.timeout <= 0:
    parser.error('a command and positive timeout are required')
disclosure = (f'yard {args.yard}: restart to finish disk-backed /tmp migration\nThis will:\n'
              '  - stop and start this yard; interrupt its active processes and connections; reset /tmp on boot\n'
              '  - retain persistent data and instance UUID; restore desired power running\n\nProceed? [y/N]').encode()
pid, terminal = pty.fork()
if pid == 0:
    os.environ.pop('SUBYARD_MIGRATION_OPERATOR_INPUT_FD', None)
    os.execvp(command[0], command)
status, tail, answered, eof = None, b'', False, False
signal.signal(signal.SIGTERM, lambda number, _frame: sys.exit(128 + number))
try:
    deadline = time.monotonic() + args.timeout
    while status is None or not eof:
        if time.monotonic() >= deadline:
            sys.exit(124)
        if select.select([] if eof else [terminal], [], [], 0.1)[0]:
            try:
                data = os.read(terminal, 65536)
            except OSError as error:
                if error.errno != errno.EIO:
                    raise
                data = b''
            if not data:
                eof = True
            sys.stdout.buffer.write(data)
            sys.stdout.buffer.flush()
            tail = (tail + data)[-65536:]
            rendered = tail.replace(b'\r\n', b'\n')
            prompt = rendered.rfind(b'Proceed? [')
            if prompt >= 0 and b']' in rendered[prompt:]:
                if answered or disclosure not in rendered or rendered.count(b'Proceed? [') != 1:
                    print("migration fixture: unexpected or repeated confirmation", file=sys.stderr)
                    sys.exit(97)
                os.write(terminal, {'yes': b'y\n', 'no': b'n\n', 'eof': b'\x04'}[args.answer])
                answered, tail = True, b''
        if status is None:
            exited, observed = os.waitpid(pid, os.WNOHANG)
            if exited:
                status = observed
except KeyboardInterrupt:
    sys.exit(130)
finally:
    if status is not None and not eof:
        try:
            os.killpg(pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    if status is None:
        try:
            os.killpg(pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        until = time.monotonic() + 2
        while status is None and time.monotonic() < until:
            exited, observed = os.waitpid(pid, os.WNOHANG)
            if exited:
                status = observed
            else:
                time.sleep(0.02)
        if status is None:
            try:
                os.killpg(pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            _, status = os.waitpid(pid, 0)
    os.close(terminal)
code = os.waitstatus_to_exitcode(status)
sys.exit((128 - code if code < 0 else code) or (0 if answered or args.allow_no_prompt else 97))
