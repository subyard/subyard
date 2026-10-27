#!/usr/bin/env python3
"""Exercise custody-refresh transport, guest pause and original native result."""
import json
import os
from pathlib import Path
import runpy
import subprocess
import tempfile
import time
import unittest

SOURCE = Path(__file__).parent/'e2e'
wrapper = runpy.run_path(str(SOURCE/'native-loss-wrapper.py'))['write_wrapper']


class WrapperTest(unittest.TestCase):
    def test_transport_and_guest_boundary(self):
        with tempfile.TemporaryDirectory(prefix="subyard-fetch-loss ' ", dir='/tmp') as temporary:
            root = Path(temporary)
            (root/'bin').mkdir()
            guest = root/"guest ' root"
            subprocess.run(['bash', '-se', '--', guest], input=(SOURCE/'native-loss-guest.sh').read_bytes(), check=True)
            environment = guest/'environment'
            self.assertEqual(environment.stat().st_mode & 0o777, 0o600)
            subprocess.run(['bash', '-n', environment], check=True)
            subprocess.run(['shellcheck', '-s', 'bash', '-S', 'warning', environment], check=True)
            native = root/"native ' incus"
            native.write_text(r'''#!/usr/bin/python3
import json, os, pathlib, sys, time
root = pathlib.Path(__file__).parent
guest = pathlib.Path(os.environ['GUEST_ROOT'])
arguments = sys.argv[1:]
with (root/'native-calls').open('a') as output: output.write(json.dumps(arguments)+'\n')
if 'custody-refresh' in arguments:
    (root/'stdin').write_bytes(sys.stdin.buffer.read())
    (guest/'paused').write_text('42 99\n')
    while not (guest/'release').exists(): time.sleep(.01)
    raise SystemExit(17)
if 'test' in arguments: raise SystemExit(0 if (guest/'paused').exists() else 1)
if 'touch' in arguments: (guest/'release').touch()
''')
            native.chmod(0o700)
            path = wrapper(root, guest, "fixture ' instance", "fixture ' project", native_executable=native)
            self.assertEqual(path.stat().st_mode & 0o777, 0o700)
            subprocess.run(['bash', '-n', path], check=True)
            subprocess.run(['shellcheck', '-S', 'warning', path], check=True)
            with subprocess.Popen([path, 'exec', 'instance', '--', 'python3', 'custody-refresh'], stdin=subprocess.PIPE,
                    env=dict(os.environ, GUEST_ROOT=str(guest))) as process:
                process.stdin.write(b'actual archive stdin'); process.stdin.close()
                try:
                    deadline = time.monotonic()+5
                    while not (root/'paused').exists() and time.monotonic() < deadline: time.sleep(.01)
                    self.assertTrue((root/'paused').exists())
                    self.assertEqual((root/'stdin').read_bytes(), b'actual archive stdin')
                    self.assertFalse((guest/'release').exists())
                    (root/'release').touch()
                    self.assertEqual(process.wait(timeout=5), 17)
                finally:
                    (guest/'release').touch()
                    if process.poll() is None: process.kill()
                    process.wait(timeout=5)

            calls = [json.loads(line) for line in (root/'native-calls').read_text().splitlines()]
            self.assertIn(['exec', 'instance', '--env', f'BASH_ENV={guest}/environment', '--', 'python3', 'custody-refresh'], calls)
            subprocess.run([path, 'list'], env=dict(os.environ, GUEST_ROOT=str(guest)), check=True, timeout=5)
            # Execute the real generated guest function with its native Python
            # dependency stubbed; it records PID/start and preserves terminal 17.
            (guest/'paused').unlink()
            python = root/'bin/python3'
            python.write_text('#!/bin/sh\nexit 17\n'); python.chmod(0o700)
            result = subprocess.run(['bash', '-c', '. "$1"; python3 custody-refresh', 'fixture', environment],
                env=dict(os.environ, PATH=str(root/'bin')+':/usr/bin:/bin'), timeout=5)
            self.assertEqual(result.returncode, 17)
            self.assertEqual(len((guest/'paused').read_text().split()), 2)
            self.assertEqual((guest/'terminal').read_text(), '17\n')


if __name__ == '__main__': unittest.main()
