#!/usr/bin/env python3
"""Bounded device watchers for the Gradle lane's leased connected stage.

stream PATH CAP -- COMMAND...            copy COMMAND's output records into PATH
sample PATH CAP INTERVAL -- COMMAND...   run COMMAND every INTERVAL seconds into PATH
section NAME PATH                        print the newest whole records of one report section

PATH and PATH.1 rotate, so each watcher keeps at most 2 * CAP bytes of its newest records.
A wedged device no longer answers captures taken after the failure.
"""
import os
import re
import signal
import subprocess
import sys
import time

# The capture helper bounds redacted output to 64 KiB; redaction may lengthen records.
SECTION_BYTES = 49152
NOTABLE_TAGS = {'vold', 'lmkd', 'lowmemorykiller', 'Watchdog', 'StorageManagerService',
                'StorageUserConnection', 'FuseDaemon'}
EVENTS = re.compile(rb' (am_kill|am_crash|am_anr|am_proc_died|am_low_memory|am_wtf|killinfo|watchdog)[: ]')
DIVIDER = re.compile(rb'^--------- (?:beginning of|switch to) (\S+)$')


class Stop(Exception):
    pass


def stop(_signum, _frame):
    raise Stop


class Rotating:
    def __init__(self, path, cap):
        self.path, self.cap = path, cap
        self.output = open(path, 'ab')

    def write(self, data):
        data = data[:self.cap]
        if self.output.tell() + len(data) > self.cap:
            self.output.close()
            os.replace(self.path, self.path + '.1')
            self.output = open(self.path, 'ab')
        self.output.write(data)
        self.output.flush()


def stream(output, command):
    child = subprocess.Popen(command, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    try:
        for record in child.stdout:
            output.write(record)
        output.write(b'watch: stream ended exit=%d\n' % child.wait())
    finally:
        if child.poll() is None:
            child.kill()
            child.wait()


def sample(output, interval, command):
    while True:
        started = time.time()
        # A sample that does not answer within its interval is recorded, not waited for.
        try:
            result = subprocess.run(command, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                    stderr=subprocess.STDOUT, timeout=interval)
            data, status = result.stdout, b'%d' % result.returncode
        except subprocess.TimeoutExpired as expired:
            data, status = expired.stdout or b'', b'timeout'
        output.write(b'--- sample time=%.3f\n' % started + data[:16384] + b'\n--- exit=%s\n' % status)
        time.sleep(max(0.0, interval - (time.time() - started)))


def selected(name, records):
    if name == 'logcat-notable':
        for record in records:
            fields = record.split()
            if len(fields) > 5 and (fields[4] in (b'W', b'E', b'F')
                                    or fields[5].rstrip(b':').decode('ascii', 'replace') in NOTABLE_TAGS):
                yield record
    elif name == 'logcat-crash':
        # logcat -D marks every change of buffer, so only crash-buffer records follow a crash divider.
        crash = False
        for record in records:
            divider = DIVIDER.match(record.rstrip(b'\n'))
            if divider:
                crash = divider[1] == b'crash'
            elif crash:
                yield record
    elif name == 'events-notable':
        yield from (record for record in records if EVENTS.search(record))
    elif name in ('logcat', 'kernel', 'memory'):
        yield from records
    else:
        raise SystemExit(f'watch: unknown section {name}')


def section(name, path):
    # A watcher stopped between rotation and reopening leaves only PATH.1.
    if not os.path.isfile(path) and not os.path.isfile(path + '.1'):
        print(f'watch: no records file for {name}', file=sys.stderr)
        raise SystemExit(3)
    data = b''
    for part in (path + '.1', path):
        if os.path.isfile(part):
            with open(part, 'rb') as source:
                data += source.read()
    # Only whole records are kept, so redaction never sees part of one.
    records = list(selected(name, data.splitlines(keepends=True)))
    kept, size, omitted = [], 0, 0
    for index in range(len(records) - 1, -1, -1):
        record = records[index]
        if len(record) > SECTION_BYTES:
            omitted += 1
            continue
        if size + len(record) > SECTION_BYTES:
            omitted += index + 1
            break
        kept.append(record)
        size += len(record)
    output = b''.join(reversed(kept))
    if output and not output.endswith(b'\n'):
        output += b'\n'
    if omitted:
        sys.stdout.buffer.write(b'watch: %d older or oversized records omitted\n' % omitted)
    sys.stdout.buffer.write(output)
    return 0


def main(argv):
    if len(argv) == 3 and argv[0] == 'section':
        return section(argv[1], argv[2])
    if len(argv) >= 5 and argv[0] == 'stream' and argv[3] == '--':
        mode, path, cap, interval, command = 'stream', argv[1], int(argv[2]), 0, argv[4:]
    elif len(argv) >= 6 and argv[0] == 'sample' and argv[4] == '--':
        mode, path, cap, interval, command = 'sample', argv[1], int(argv[2]), int(argv[3]), argv[5:]
    else:
        raise SystemExit(__doc__)
    signal.signal(signal.SIGTERM, stop)
    output = Rotating(path, cap)
    try:
        if mode == 'stream':
            stream(output, command)
        else:
            sample(output, interval, command)
    except Stop:
        pass
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
