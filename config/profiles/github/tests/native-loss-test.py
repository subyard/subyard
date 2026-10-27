#!/usr/bin/env python3
"""Exercise the GitHub pause before the actual service restart."""
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
    def test_restart_boundary(self):
        with tempfile.TemporaryDirectory(prefix="subyard-github-loss ' ", dir='/tmp') as temporary:
            root = Path(temporary)
            (root/'bin').mkdir()
            native = root/"native ' systemctl"
            native.write_text('#!/usr/bin/python3\nimport json, pathlib, sys\npathlib.Path(__file__).with_name("native-call").write_text(json.dumps(sys.argv[1:]))\nraise SystemExit(17)\n')
            native.chmod(0o700)
            path = wrapper(root, "fixture ' service", native_executable=native)
            self.assertEqual(path.stat().st_mode & 0o777, 0o700)
            subprocess.run(['bash', '-n', path], check=True)
            subprocess.run(['shellcheck', '-S', 'warning', path], check=True)
            result = subprocess.run([path, '--version'], timeout=5)
            self.assertEqual(result.returncode, 17)
            (root/'native-call').unlink()
            with subprocess.Popen([path, '--user', 'restart', "fixture ' service"]) as process:
                try:
                    deadline = time.monotonic()+5
                    while not (root/'paused').exists() and time.monotonic() < deadline: time.sleep(.01)
                    self.assertTrue((root/'paused').exists())
                    self.assertFalse((root/'native-call').exists(), 'pause moved after restart')
                    (root/'release').touch()
                    self.assertEqual(process.wait(timeout=5), 17)
                finally:
                    if process.poll() is None: process.kill()
                    process.wait(timeout=5)

            self.assertEqual(json.loads((root/'native-call').read_text()), ['--user', 'restart', "fixture ' service"])
            self.assertEqual((root/'calls').read_text(), 'restart\n')


if __name__ == '__main__': unittest.main()
