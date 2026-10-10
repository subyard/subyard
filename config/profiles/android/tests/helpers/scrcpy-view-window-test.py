#!/usr/bin/env python3
"""Real local process checks for the fixture's stock viewer window."""
import contextlib
import importlib.util
import inspect
import io
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

HELPER = Path(__file__).with_name('scrcpy-view-window.py')
spec = importlib.util.spec_from_file_location('window', HELPER)
window = importlib.util.module_from_spec(spec)
spec.loader.exec_module(window)
BASE = "import os,signal,sys,time,resource; resource.setrlimit(resource.RLIMIT_CORE,(0,0)); "
GRACEFUL = "signal.signal(signal.SIGTERM,lambda *_: sys.exit(0)); "
CONNECTED = "print('DEBUG: Server connected',flush=True); "
FRAME = "print('INFO: Texture: 320x640',flush=True); "
MARKER = CONNECTED + FRAME


class WindowTest(unittest.TestCase):
    def run_child(self, body, seconds=0.12, shutdown=0.12, args=(), startup=None):
        output = io.StringIO()
        start = time.monotonic()
        with contextlib.redirect_stdout(output):
            options = {} if startup is None else {'startup_seconds': startup}
            code = window.run([sys.executable, '-I', '-B', '-c', BASE + body, *args], seconds, shutdown, **options)
        lines = output.getvalue().splitlines()
        evidence = json.loads(lines[-1].removeprefix('scrcpy-view-window '))
        self.assertEqual(code, evidence['wrapper_status'])
        self.assertEqual(set(evidence), {'native_returncode','window_completed','wrapper_status'})
        return code, evidence, lines, time.monotonic() - start

    def test_exact_marker_graceful(self):
        code, value, lines, elapsed = self.run_child("assert sys.stdout.isatty() and sys.stderr.isatty() and not sys.stdin.isatty(); " + GRACEFUL + MARKER + "time.sleep(10)")
        self.assertEqual((code,value['native_returncode'],value['window_completed']), (0,0,True))
        self.assertIn('INFO: Texture: 320x640', lines)
        self.assertGreaterEqual(elapsed, 0.12)

    def test_fragmented_delayed_marker(self):
        code, value, _, elapsed = self.run_child(GRACEFUL + "time.sleep(.1); os.write(1,b'DEBUG: Server '); time.sleep(.08); os.write(1,b'connected\\n'); " + FRAME + "time.sleep(10)")
        self.assertEqual(code, 0)
        self.assertGreaterEqual(elapsed, .3)

    def test_delayed_first_frame_starts_observation_window(self):
        # Connection can precede a cold decoder's first frame by more than the window.
        code, value, lines, elapsed = self.run_child(GRACEFUL + CONNECTED + "time.sleep(.3); " + FRAME + "time.sleep(10)")
        self.assertEqual((code,value['native_returncode'],value['window_completed']), (0,0,True))
        self.assertIn('INFO: Texture: 320x640', lines)
        self.assertGreaterEqual(elapsed, .42)

    def test_no_first_frame_is_bounded_without_completed_window(self):
        code, value, _, elapsed = self.run_child(GRACEFUL + CONNECTED + 'time.sleep(10)', startup=.12)
        self.assertEqual((code,value['native_returncode'],value['window_completed']), (124,0,False))
        self.assertLess(elapsed, 2)

    def test_exit_tail_preserves_safe_lines_and_native_status(self):
        original_waitid = os.waitid
        def exited_before_read(kind, pid, flags):
            return original_waitid(kind, pid, os.WEXITED | os.WNOWAIT)
        for ending, status, native in [('pass',1,0), ('sys.exit(7)',7,7), ('os.kill(os.getpid(),signal.SIGSEGV)',139,-11)]:
            with self.subTest(native=native), mock.patch.object(window.os, 'waitid', side_effect=exited_before_read):
                code, value, lines, _ = self.run_child(FRAME + "print('private-sentinel',flush=True); " + ending)
                self.assertEqual((code,value['native_returncode'],value['window_completed']), (status,native,False))
                self.assertIn('INFO: Texture: 320x640', lines)
                self.assertNotIn('private-sentinel', '\n'.join(lines))

    def test_no_marker_and_early_zero_fail(self):
        for body in ("pass", MARKER + 'pass', "print('prefix DEBUG: Server connected',flush=True)", "print('DEBUG: Server connected\\r',flush=True)"):
            with self.subTest(body=body):
                code, value, _, _ = self.run_child(body)
                self.assertEqual((code,value['native_returncode'],value['window_completed']), (1,0,False))

    def test_nonzero_and_signed_crash(self):
        for body, status, native in [('sys.exit(7)',7,7), ('os.kill(os.getpid(),signal.SIGSEGV)',139,-11)]:
            code, value, _, _ = self.run_child(body)
            self.assertEqual((code,value['native_returncode']), (status,native))

    def test_native_crash_between_live_check_and_signal(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory)/'pid'
            body = GRACEFUL + f"__import__('pathlib').Path({str(target)!r}).write_text(str(os.getpid())); " + MARKER + 'time.sleep(10)'
            def exited_before_signal(fd, number):
                pid = int(target.read_text())
                os.kill(pid, signal.SIGSEGV)
                deadline = time.monotonic() + 2
                while not os.waitid(os.P_PID, pid, os.WEXITED | os.WNOHANG | os.WNOWAIT):
                    if time.monotonic() >= deadline:
                        self.fail('controlled native crash did not exit')
                    time.sleep(.005)
                raise ProcessLookupError('native already exited')
            with mock.patch.object(window.signal, 'pidfd_send_signal', side_effect=exited_before_signal):
                code, value, _, _ = self.run_child(body)
            self.assertEqual((code,value['native_returncode'],value['window_completed']), (139,-11,False))

    def test_fast_native_exit_before_pidfd_open(self):
        def vanished(pid):
            os.kill(pid, signal.SIGSEGV)
            deadline = time.monotonic()+2
            while not os.waitid(os.P_PID, pid, os.WEXITED | os.WNOHANG | os.WNOWAIT):
                if time.monotonic() >= deadline:
                    self.fail('controlled native crash did not exit')
                time.sleep(.005)
            raise ProcessLookupError('native already exited')
        with mock.patch.object(window.os, 'pidfd_open', side_effect=vanished):
            code, value, _, _ = self.run_child('time.sleep(10)')
        self.assertEqual((code,value['native_returncode']), (139,-11))

    def test_cancel_group_vanishes_before_signal(self):
        original_open, original_killpg = os.pidfd_open, os.killpg
        def cancel_after_open(pid):
            fd = original_open(pid)
            os.kill(os.getpid(), signal.SIGTERM)
            return fd
        def vanished_group(pid, number):
            if number != signal.SIGTERM:
                return original_killpg(pid, number)
            os.kill(pid, signal.SIGSEGV)
            deadline = time.monotonic()+2
            while not os.waitid(os.P_PID, pid, os.WEXITED | os.WNOHANG | os.WNOWAIT):
                if time.monotonic() >= deadline:
                    self.fail('controlled native crash did not exit')
                time.sleep(.005)
            raise ProcessLookupError('owned group already exited')
        with mock.patch.object(window.os, 'pidfd_open', side_effect=cancel_after_open), mock.patch.object(window.os, 'killpg', side_effect=vanished_group):
            code, value, _, _ = self.run_child('time.sleep(10)')
        self.assertEqual((code,value['native_returncode'],value['window_completed']), (143,-11,False))

    def test_missing_signal_handler_never_passes(self):
        code, value, _, _ = self.run_child(MARKER + 'time.sleep(10)')
        self.assertEqual((code,value['native_returncode'],value['window_completed']), (143,-15,False))

    def test_ignored_term_is_bounded_timeout(self):
        code, value, _, elapsed = self.run_child("signal.signal(signal.SIGTERM,signal.SIG_IGN); " + MARKER + 'time.sleep(10)')
        self.assertEqual((code,value['native_returncode'],value['window_completed']), (124,-9,False))
        self.assertLess(elapsed, 2)

    def test_noise_long_line_privacy_and_timer(self):
        body = GRACEFUL + "os.write(1,b'SECRET-'*10000+b'DEBUG: Server connected\\n'); " + "print('[server] ERROR: Capture/encoding error: private-value',flush=True); " + MARKER + "\nwhile True: os.write(1,b'private-value\\n'*4000)\n"
        code, value, lines, elapsed = self.run_child(body)
        self.assertEqual(code, 0)
        self.assertLess(elapsed, 2)
        self.assertEqual(lines[:-1], ['[server] ERROR: Capture/encoding error: classified', 'DEBUG: Server connected', 'INFO: Texture: 320x640'])
        self.assertNotIn('SECRET', '\n'.join(lines))
        self.assertNotIn('private-value', '\n'.join(lines))

    def test_argv_unchanged(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory)/'argv.json'
            args = ('--max-size=640', '--no-audio', 'literal value')
            code, _, _, _ = self.run_child(GRACEFUL + f"__import__('pathlib').Path({str(target)!r}).write_text(__import__('json').dumps(sys.argv[1:])); " + MARKER + 'time.sleep(10)', args=args)
            self.assertEqual(code, 0)
            self.assertEqual(json.loads(target.read_text()), list(args))

    def test_defaults_and_no_cli_override(self):
        self.assertEqual(inspect.signature(window.run).parameters['seconds'].default, 30)
        self.assertEqual(inspect.signature(window.run).parameters['shutdown_seconds'].default, 5)
        result = subprocess.run([sys.executable,'-I','-B',str(HELPER),'--seconds=0.01','--',sys.executable], capture_output=True, timeout=2)
        self.assertEqual((result.returncode,result.stdout,result.stderr), (125,b'',b''))
        for command in ([], ('/bin/true',), ['/bin/true', '\0'], ['relative']):
            self.assertEqual(window.run(command), 125)

    def test_cancellation_cleans_owned_group_not_foreign(self):
        foreign = subprocess.Popen([sys.executable,'-I','-B','-c','import time; time.sleep(30)'], start_new_session=True)
        try:
            for number in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
                with self.subTest(signal=number), tempfile.TemporaryDirectory() as directory:
                    target = Path(directory)/'pids.json'
                    body = BASE + GRACEFUL + "import subprocess,json,pathlib; child=subprocess.Popen([sys.executable,'-I','-B','-c','import time; time.sleep(30)']); " + f"pathlib.Path({str(target)!r}).write_text(json.dumps([os.getpid(),child.pid])); " + MARKER + 'time.sleep(30)'
                    command = [sys.executable,'-I','-B',str(HELPER),'--',sys.executable,'-I','-B','-c',body]
                    proc = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                    try:
                        deadline = time.monotonic()+3
                        while not target.exists() and time.monotonic()<deadline:
                            time.sleep(.01)
                        self.assertTrue(target.exists())
                        pids = json.loads(target.read_text())
                        proc.send_signal(number)
                        output, errors = proc.communicate(timeout=7)
                        self.assertEqual(proc.returncode, 128+number)
                        self.assertEqual(errors,b'')
                        value = json.loads(output.decode().splitlines()[-1].removeprefix('scrcpy-view-window '))
                        self.assertFalse(value['window_completed'])
                        self.assertIsNone(foreign.poll())
                        for pid in pids:
                            path = Path(f'/proc/{pid}/stat')
                            self.assertTrue(not path.exists() or path.read_text().split(') ',1)[1].startswith('Z '))
                    finally:
                        if proc.poll() is None:
                            proc.kill(); proc.wait()
        finally:
            foreign.terminate(); foreign.wait()


if __name__ == '__main__':
    unittest.main()
