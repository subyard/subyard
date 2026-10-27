#!/usr/bin/env python3
"""Exercise Orca's first successful registration-push boundary."""
import json
import os
from pathlib import Path
import runpy
import subprocess
import tempfile
import time
import unittest

wrapper = runpy.run_path(str(Path(__file__).parent/'e2e/native-loss-wrapper.py'))['write_wrapper']


class WrapperTest(unittest.TestCase):
    def test_registration_boundary(self):
        with tempfile.TemporaryDirectory(prefix="subyard-orca-loss ' ", dir='/tmp') as temporary:
            root = Path(temporary)
            (root/'bin').mkdir()
            native = root/"native ' incus"
            native.write_text('#!/usr/bin/python3\nimport json, os, pathlib, sys\nwith pathlib.Path(__file__).with_name("native-calls").open("a") as output: output.write(json.dumps(sys.argv[1:])+"\\n")\nraise SystemExit(int(os.environ["NATIVE_EXIT"]))\n')
            native.chmod(0o700)
            path = wrapper(root, "fixture ' instance", native_executable=native)
            self.assertEqual(path.stat().st_mode & 0o777, 0o700)
            subprocess.run(['bash', '-n', path], check=True)
            subprocess.run(['shellcheck', '-S', 'warning', path], check=True)
            arguments = ['file', 'push', 'payload', "fixture ' instance/tmp/subyard-orca.fixture"]
            result = subprocess.run([path, *arguments], env=dict(os.environ, NATIVE_EXIT='17'), timeout=5)
            self.assertEqual(result.returncode, 17)
            self.assertFalse((root/'paused').exists())
            with subprocess.Popen([path, *arguments], env=dict(os.environ, NATIVE_EXIT='0')) as process:
                try:
                    deadline = time.monotonic()+5
                    while not (root/'paused').exists() and time.monotonic() < deadline: time.sleep(.01)
                    self.assertTrue((root/'paused').exists())
                    self.assertEqual(len((root/'native-calls').read_text().splitlines()), 2, 'pause moved before push')
                    paused = (root/'paused').read_bytes()
                    (root/'release').touch()
                    self.assertEqual(process.wait(timeout=5), 0)
                finally:
                    if process.poll() is None: process.kill()
                    process.wait(timeout=5)

            self.assertEqual(subprocess.run([path, *arguments], env=dict(os.environ, NATIVE_EXIT='0'), timeout=5).returncode, 0)
            self.assertEqual((root/'paused').read_bytes(), paused)
            self.assertEqual(subprocess.run([path, 'list'], env=dict(os.environ, NATIVE_EXIT='19'), timeout=5).returncode, 19)
            self.assertEqual(json.loads((root/'native-calls').read_text().splitlines()[-1]), ['list'])


if __name__ == '__main__': unittest.main()
