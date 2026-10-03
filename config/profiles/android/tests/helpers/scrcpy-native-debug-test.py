#!/usr/bin/env python3
"""Real harmless native child: python3 TEST --gdb GDB (requires cc and GDB Python)."""
import argparse
import json
import os
import re
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--gdb', default='gdb')
args = parser.parse_args()
helper = Path(__file__).with_name('scrcpy-native-debug.py')
with tempfile.TemporaryDirectory(prefix='native-debug-test-') as directory:
    root = Path(directory)
    child = root / 'scrcpy'
    source = root / 'child.c'
    source.write_text(r'''
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>
void sc_native_crash(void) { raise(SIGSEGV); }
int main(int argc, char **argv) {
    puts("PRIVATE_SENTINEL /private/path token=secret 192.0.2.17");
    puts("INFO: Texture: 640x480");
    puts("[server] DEBUG: Using video encoder: 'c2.android.avc.encoder'");
    puts("[server] ERROR: Capture/encoding error: PRIVATE_SENTINEL");
    fflush(stdout);
    if (argv[1][0] == 's') sc_native_crash();
    if (argv[1][0] == 'c') {
        FILE *f = fopen(argv[2], "w"); fprintf(f, "%d", getpid()); fclose(f);
        sleep(60);
    }
    return atoi(argv[1]);
}
''')
    subprocess.run(['cc', '-g', '-O0', str(source), '-o', str(child)], check=True)
    base = [sys.executable, '-B', str(helper), '--gdb', args.gdb, '--', str(child)]
    def inspect(completed, expected):
        output = completed.stdout
        assert completed.returncode == expected, (completed.returncode, output)
        assert not completed.stderr, completed.stderr
        assert 'PRIVATE_SENTINEL' not in output and '/private' not in output and '192.0.2' not in output
        assert str(root) not in output and not re.search(r'\b0x[0-9a-fA-F]+', output)
        result = json.loads(next(line.split(' ', 1)[1] for line in output.splitlines()
                                 if line.startswith('scrcpy-native-result ')))
        assert result['wrapper_status'] == expected
        return result
    for value in ('0', '7', 's'):
        completed = subprocess.run([*base, value, 'PRIVATE_SENTINEL'], capture_output=True,
                                   text=True, timeout=15)
        result = inspect(completed, {'0': 0, '7': 7, 's': 139}[value])
        assert result['inferior_returncode'] == {'0': 0, '7': 7, 's': -11}[value]
        assert 'INFO: Texture: 640x480' in completed.stdout
        assert "[server] DEBUG: Using video encoder: 'c2.android.avc.encoder'" in completed.stdout
        if value == 's':
            stack = json.loads(next(line.split(' ', 1)[1] for line in completed.stdout.splitlines()
                                    if line.startswith('scrcpy-native-stack ')))
            assert set(stack) == {'signal', 'thread', 'frames'} and stack['signal'] == 11
            assert type(stack['thread']) is int and stack['thread'] > 0
            assert 1 <= len(stack['frames']) <= 12
            assert any(frame['symbol'] == 'sc_native_crash' for frame in stack['frames']), stack
            for frame in stack['frames']:
                assert set(frame) == {'module', 'symbol', 'offset'}
                assert frame['module'] in ('scrcpy', 'SDL', 'FFmpeg', 'X11', 'libc', 'other', 'unknown')
                assert type(frame['offset']) is int and -1 <= frame['offset'] < 1 << 64
        print('PASS real-child-' + value)
    fake = root / 'false-success'
    fake.write_text('#!/bin/sh\nexit 0\n')
    fake.chmod(0o700)
    completed = subprocess.run([sys.executable, '-B', str(helper), '--gdb', str(fake), '--', str(child), '0'],
                               capture_output=True, text=True, timeout=5)
    inspect(completed, 125)
    print('PASS debugger-zero-cannot-pass')
    stalled = root / 'stalled-debugger'
    stalled.write_text("#!/usr/bin/env python3\nimport os,re,sys,time\ns=open(sys.argv[sys.argv.index('-x')+1]).read()\nfd=int(re.search(r'fd = ([0-9]+)',s)[1])\nos.write(fd,b'{\"collecting\":11}\\n')\ntime.sleep(30)\n")
    stalled.chmod(0o700)
    started = time.monotonic()
    completed = subprocess.run([sys.executable, '-B', str(helper), '--gdb', str(stalled), '--', str(child), '0'],
                               capture_output=True, text=True, timeout=8)
    result = inspect(completed, 139)
    assert result['inferior_returncode'] is None
    assert result['diagnostic'] == 'stack_deadline' and time.monotonic() - started < 7
    print('PASS stack-collection-deadline')
    control = root / 'control-debugger'
    def controller(body):
        control.write_text("#!/usr/bin/env python3\nimport json,os,re,subprocess,sys,time\ns=open(sys.argv[sys.argv.index('-x')+1]).read()\nfd=int(re.search(r'fd = ([0-9]+)',s)[1])\n" + body)
        control.chmod(0o700)
    private_stack = {'signal': 11, 'thread': 1, 'frames': [], 'secret': 'PRIVATE_SENTINEL'}
    malformed = [private_stack, {'signal': 11, 'thread': True, 'frames': []},
                 {'result': True}, {'pid': True},
                 {'signal': 11, 'thread': 1, 'frames': [{'module': 'libc', 'symbol': 'PRIVATE_SENTINEL/private', 'offset': 1}]},
                 {'signal': 11, 'thread': 1, 'frames': [{'module': 'private', 'symbol': 'main', 'offset': 1}]},
                 {'signal': 11, 'thread': 1, 'frames': [{'module': 'libc', 'symbol': 'main', 'offset': True}]},
                 {'signal': 11, 'thread': 1, 'frames': [{'module': 'libc', 'symbol': 'main', 'offset': 1}] * 13}]
    for payload in malformed:
        controller('os.write(fd, ' + repr((json.dumps(payload) + '\n').encode()) + ')\ntime.sleep(30)\n')
        completed = subprocess.run([sys.executable, '-B', str(helper), '--gdb', str(control), '--', str(child), '0'],
                                   capture_output=True, text=True, timeout=5)
        result = inspect(completed, 125)
        assert result['diagnostic'] == 'invalid_control', result
        assert 'scrcpy-native-stack ' not in completed.stdout
    print('PASS invalid-control-privacy-and-schema')
    orphan_file = root / 'orphan.pid'
    controller("p=subprocess.Popen(['/usr/bin/sleep','60'], start_new_session=True)\n"
               + 'open(' + repr(str(orphan_file)) + ", 'w').write(str(p.pid))\n"
               + "os.write(fd,(json.dumps({'pid':p.pid})+chr(10)).encode())\ntime.sleep(.2)\nos._exit(9)\n")
    completed = subprocess.run([sys.executable, '-B', str(helper), '--gdb', str(control), '--', str(child), '0'],
                               capture_output=True, text=True, timeout=6)
    inspect(completed, 125)
    assert orphan_file.exists() and not Path('/proc/' + orphan_file.read_text()).exists()
    print('PASS unexpected-debugger-exit-reaped-orphan')
    foreign = subprocess.Popen(['/usr/bin/sleep', '60'])
    try:
        controller("os.write(fd,(json.dumps({'pid':" + str(foreign.pid) + "})+chr(10)).encode())\ntime.sleep(30)\n")
        completed = subprocess.run([sys.executable, '-B', str(helper), '--gdb', str(control), '--', str(child), '0'],
                                   capture_output=True, text=True, timeout=5)
        assert inspect(completed, 125)['diagnostic'] == 'invalid_control'
        assert foreign.poll() is None, 'unrelated process was signalled'
    finally:
        foreign.terminate()
        foreign.wait(timeout=3)
    print('PASS foreign-pid-untouched')
    for cancellation in (signal.SIGTERM, signal.SIGHUP):
        pidfile = root / ('child-%d.pid' % cancellation)
        process = subprocess.Popen([*base, 'c', str(pidfile)], stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, text=True)
        deadline = time.monotonic() + 5
        while not pidfile.exists() and time.monotonic() < deadline:
            time.sleep(.02)
        assert pidfile.exists(), 'inferior did not start'
        pid = int(pidfile.read_text())
        process.send_signal(cancellation)
        output, errors = process.communicate(timeout=5)
        inspect(subprocess.CompletedProcess([], process.returncode, output, errors), 128 + cancellation)
        assert not Path('/proc/%d' % pid).exists(), 'cancelled inferior remains'
        print('PASS cancellation-reaped-' + str(cancellation))
    assert not list(root.glob('core*')), 'core dump created'
print('PASS native-debug checks')
