#!/usr/bin/env python3
"""Host-free checks for explicit Wayland selection and isolated probe state."""
import contextlib
import importlib.util
import io
import itertools
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('veranda_probe', Path(__file__).with_name('measure-veranda.py'))
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)
wayland_spec = importlib.util.spec_from_file_location('wayland_fixture', Path(__file__).parent / 'e2e/veranda-wayland-resources.py')
wayland_fixture = importlib.util.module_from_spec(wayland_spec)
wayland_spec.loader.exec_module(wayland_fixture)


class WaylandProbeTest(unittest.TestCase):
    def test_launch_stderr_target_is_optional_and_default_is_unchanged(self):
        binary = self.root / 'launch-fixture'
        binary.write_text('#!/usr/bin/python3\nimport sys,time\nprint("private-stderr-fixture",file=sys.stderr,flush=True)\nprint("VERANDA_READY",flush=True)\ntime.sleep(30)\n')
        binary.chmod(0o700)
        target = self.root / 'stderr.log'
        target.write_bytes(b''); target.chmod(0o600)
        original_popen = subprocess.Popen
        with target.open('ab') as stream:
            for stderr in [subprocess.DEVNULL, stream]:
                with self.subTest(explicit=stderr is stream), patch.object(probe.subprocess, 'Popen', wraps=original_popen) as popen:
                    options = {'stderr': stderr} if stderr is stream else {}
                    process, tree, _ = probe.launch(binary, {'PATH': '/usr/bin:/bin'}, 3, **options)
                    try: self.assertIs(popen.call_args.kwargs['stderr'], stderr)
                    finally:
                        tree.stop(process); process.stdout.close()
        self.assertEqual(target.read_bytes(), b'private-stderr-fixture\n')

    def test_fixture_cleanup_preserves_targets_and_checks_marker_identity(self):
        original_run = subprocess.run
        def unprivileged(command, **kwargs):
            self.assertEqual(command[:2], ['sudo', '-n'])
            return original_run(command[2:], **kwargs)
        with patch.dict(os.environ, {'TMPDIR': str(probe.ROOT / '.build')}):
            state, identity = wayland_fixture.create_state()
        self.assertEqual(state.parent, Path('/var/tmp'))
        self.assertFalse(state.is_relative_to(probe.ROOT))
        marker = state / '.marker'; original = marker.read_text()
        with patch.object(wayland_fixture.subprocess, 'run', side_effect=unprivileged):
            try:
                marker.write_text('wrong marker\n')
                with self.assertRaises(subprocess.CalledProcessError):
                    wayland_fixture.cleanup_state(state, identity)
                self.assertTrue(state.is_dir())
                marker.write_text(original)
                with self.assertRaises(subprocess.CalledProcessError):
                    wayland_fixture.cleanup_state(state, (identity[0], identity[1], identity[2] + 1))
                with tempfile.TemporaryDirectory(prefix='veranda-cleanup-target-') as target:
                    kept = Path(target) / 'kept'; kept.write_text('keep\n')
                    (state / 'link').symlink_to(target, target_is_directory=True)
                    wayland_fixture.cleanup_state(state, identity)
                    self.assertFalse(state.exists())
                    self.assertEqual(kept.read_text(), 'keep\n')
            finally:
                if state.exists():
                    marker.write_text(original)
                    wayland_fixture.cleanup_state(state, identity)

    def test_cleanup_failure_preserves_the_primary_error(self):
        for error_type in (RuntimeError, KeyboardInterrupt):
            with self.subTest(cleanup=error_type):
                def fail(): raise error_type('cleanup failed')
                stderr = io.StringIO()
                with contextlib.redirect_stderr(stderr), self.assertRaisesRegex(ValueError, '^original failure$'):
                    with wayland_fixture.cleanup_on_exit(fail, 'fixture'):
                        raise ValueError('original failure')
                self.assertIn('original error preserved', stderr.getvalue())
                with self.assertRaisesRegex(error_type, '^cleanup failed$'):
                    with wayland_fixture.cleanup_on_exit(fail, 'fixture'):
                        pass

    def test_product_signal_requires_the_recorded_process_identity(self):
        process = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(30)'], start_new_session=True)
        original_run = subprocess.run
        # Exercise the signal helper without sudo; this host-free child is ours.
        def unprivileged(command, **kwargs):
            self.assertEqual(command[:2], ['sudo', '-n'])
            return original_run(command[2:], **kwargs)
        try:
            identity = probe.processes()[process.pid]
            with patch.object(wayland_fixture.subprocess, 'run', side_effect=unprivileged):
                wayland_fixture.signal_product(signal.SIGTERM, {process.pid: dict(identity, start=identity['start'] + 1)})
                self.assertIsNone(process.poll(), 'a different process identity was signalled')
                wayland_fixture.signal_product(signal.SIGTERM, {process.pid: identity})
            self.assertEqual(process.wait(timeout=3), -signal.SIGTERM)
        finally:
            if process.poll() is None:
                process.kill()
                process.wait(timeout=3)

    def test_configured_workload_requires_a_complete_rendered_fleet_marker(self):
        expected = probe.FLEET_COUNTS
        marker = b'VERANDA_FLEET ' + json.dumps(expected).encode() + b'\nVERANDA_READY\n'
        probe.ready_fleet(marker, expected)
        probe.ready_fleet(b'unrelated output\n' + marker, expected)
        for counts in (None, {}, dict(expected, owners=0), dict(expected, projects=199),
                       dict(expected, owners=True), dict(expected, projects=200.0),
                       dict(expected, extra=1)):
            with self.subTest(counts=counts), self.assertRaises(RuntimeError):
                probe.ready_fleet(b'VERANDA_FLEET ' + json.dumps(counts).encode() + b'\nVERANDA_READY\n', expected)
        for incomplete in (b'VERANDA_READY\n', marker[:-len(b'\nVERANDA_READY\n')],
                           b'VERANDA_FLEET {invalid}\nVERANDA_READY\n',
                           b'VERANDA_FLEET ' + b'x' * 513 + b'\nVERANDA_READY\n'):
            with self.subTest(marker=incomplete), self.assertRaises(RuntimeError):
                probe.ready_fleet(incomplete, expected)

    def test_owned_independent_child_group_is_reaped_before_cleanup_success(self):
        code = """import signal, subprocess, sys, time
child = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(30)'], start_new_session=True)
def stop(*_):
    child.wait(timeout=3)
    sys.exit(0)
signal.signal(signal.SIGTERM, stop)
print('ready', flush=True)
time.sleep(30)
"""
        process = subprocess.Popen([sys.executable, '-c', code], start_new_session=True,
                                   stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        tree = probe.Tree(process.pid)
        try:
            with probe.selectors.DefaultSelector() as selector:
                selector.register(process.stdout, probe.selectors.EVENT_READ)
                self.assertTrue(selector.select(3), 'fixture readiness timed out')
                self.assertEqual(process.stdout.readline(), b'ready\n')
            tree.sample()
            self.assertEqual(len(tree.known), 2)
            tree.stop(process)
            self.assertFalse(set(tree.known) & set(probe.processes()))
        finally:
            if process.poll() is None:
                tree.stop(process)
            process.stdout.close()

    def test_recycled_group_does_not_import_unrelated_processes(self):
        tree = probe.Tree(999999)
        tree.known[999999] = 1
        foreign = {'start': 2, 'group': 999999, 'parent': 1}
        with patch.object(probe, 'processes', return_value={999999: foreign}):
            self.assertEqual(tree.sample()['process_count'], 0)

    def test_wayland_control_requires_the_current_pinned_output(self):
        output = "interface: 'wl_output', version: 4\n x: 0, y: 0, scale: 1,\n mode:\n width: 1920 px, height: 1080 px, refresh: 60.000 Hz,\n flags: current preferred\n"
        self.assertEqual(wayland_fixture.output_geometry(output)['refresh_millihertz'], 60000)
        for invalid in (output.replace('current preferred', 'preferred'), output.replace('60.000', '59.000'),
                        output.replace('1920', '1280'), output.replace('scale: 1', 'scale: 2'), output + output):
            with self.subTest(output=invalid), self.assertRaises(RuntimeError):
                wayland_fixture.output_geometry(invalid)

    def test_runtime_only_webkit_metadata_and_missing_metadata(self):
        missing = FileNotFoundError('pkg-config')
        with patch.object(probe, 'command_output', side_effect=[missing, '2.50.6-1~deb13u1']) as output:
            self.assertEqual(probe.webkit_version(), ('2.50.6-1~deb13u1', 'Debian runtime package'))
            self.assertEqual(output.call_args.args[0][-1], 'libwebkit2gtk-4.1-0')
        with patch.object(probe, 'command_output', side_effect=missing), self.assertRaisesRegex(RuntimeError, 'metadata is unavailable'):
            probe.webkit_version()

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='veranda-wayland-test-')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.root.chmod(0o700)
        self.runtime = self.root / 'desktop-runtime'
        self.runtime.mkdir(mode=0o700)
        self.runtime.chmod(0o700)
        self.endpoint = self.runtime / 'wayland-fixture'
        self.socket = socket.socket(socket.AF_UNIX)
        self.addCleanup(self.socket.close)
        self.socket.bind(str(self.endpoint))
        self.ambient = {'WAYLAND_DISPLAY': self.endpoint.name, 'XDG_RUNTIME_DIR': str(self.runtime),
                        'DISPLAY': ':ambient', 'SSH_AUTH_SOCK': 'credential-sentinel',
                        'WAYLAND_SOCKET': '123', 'SECRET_TOKEN': 'credential-sentinel',
                        'GDK_GL': 'credential-sentinel'}

    def test_missing_or_invalid_wayland_fails_before_any_launch_without_echoing_environment(self):
        cases = [({key: value for key, value in self.ambient.items() if key != missing},
                  'requires WAYLAND_DISPLAY and XDG_RUNTIME_DIR')
                 for missing in ['WAYLAND_DISPLAY', 'XDG_RUNTIME_DIR']]
        cases.append((dict(self.ambient, WAYLAND_DISPLAY='missing'), 'compositor socket or runtime directory is unavailable'))
        cases.append((dict(self.ambient, WAYLAND_DISPLAY='credential-sentinel\n'), 'valid compositor endpoint names'))
        for ambient, expected in cases:
            with self.subTest(expected=expected):
                error = io.StringIO()
                with patch.dict(os.environ, ambient, clear=True), patch('sys.argv', ['probe', '--wayland']), \
                        patch.object(probe.subprocess, 'Popen') as popen, patch.object(probe, 'launch') as launch, \
                        contextlib.redirect_stderr(error), self.assertRaises(SystemExit) as stopped:
                    probe.main()
                self.assertEqual(stopped.exception.code, 2)
                popen.assert_not_called()
                launch.assert_not_called()
                self.assertIn(expected, error.getvalue())
                self.assertNotIn(str(self.runtime), error.getvalue())
                self.assertNotIn('credential-sentinel', error.getvalue())

    def test_unsafe_runtime_and_socket_are_rejected(self):
        plain_file = self.runtime / 'plain'
        plain_file.write_text('not a socket')
        socket_link = self.runtime / 'linked'
        socket_link.symlink_to(self.endpoint)
        runtime_link = self.root / 'linked-runtime'
        runtime_link.symlink_to(self.runtime, target_is_directory=True)
        for display, runtime in [(plain_file.name, self.runtime), (socket_link.name, self.runtime),
                                 ('missing', self.runtime), ('../wayland-fixture', self.runtime),
                                 (self.endpoint.name, runtime_link)]:
            with self.subTest(display=display), patch.dict(os.environ, {'WAYLAND_DISPLAY': display, 'XDG_RUNTIME_DIR': str(runtime)}, clear=True):
                with self.assertRaises(RuntimeError):
                    probe.wayland_display()
        self.runtime.chmod(0o755)
        with patch.dict(os.environ, self.ambient, clear=True), self.assertRaises(RuntimeError):
            probe.wayland_display()
        self.runtime.chmod(0o700)
        with patch.dict(os.environ, self.ambient, clear=True), patch.object(probe.os, 'getuid', return_value=os.getuid() + 1), \
                self.assertRaises(RuntimeError):
            probe.wayland_display()
        original_lstat = Path.lstat

        def foreign_socket(path):
            values = list(original_lstat(path))
            if path == self.endpoint:
                values[4] += 1  # Socket UID; runtime remains owned by this user.
            return os.stat_result(values)

        with patch.dict(os.environ, self.ambient, clear=True), patch.object(Path, 'lstat', foreign_socket), \
                self.assertRaises(RuntimeError):
            probe.wayland_display()

    def test_wayland_environment_is_isolated_and_default_api_is_preserved(self):
        for name in [self.endpoint.name, str(self.endpoint)]:
            with self.subTest(name=name), patch.dict(os.environ, dict(self.ambient, WAYLAND_DISPLAY=name), clear=True):
                display = probe.wayland_display()
                directory = self.root / ('absolute' if Path(name).is_absolute() else 'relative')
                directory.mkdir(mode=0o700)
                env = probe.environment(directory, display, Path('/synthetic/engine'), wayland=True)
                self.assertEqual(env['WAYLAND_DISPLAY'], str(self.endpoint))
                self.assertEqual(env['GDK_BACKEND'], 'wayland')
                self.assertEqual(env['XDG_RUNTIME_DIR'], str(directory / 'runtime'))
                self.assertEqual(env['YARD_ENGINE_PATH'], '/synthetic/engine')
                for credential in ['DISPLAY', 'SSH_AUTH_SOCK', 'WAYLAND_SOCKET', 'SECRET_TOKEN', 'GDK_GL']:
                    self.assertNotIn(credential, env)
                for path in directory.iterdir():
                    self.assertEqual(path.stat().st_mode & 0o777, 0o700)
        directory = self.root / 'xvfb'
        directory.mkdir(mode=0o700)
        env = probe.environment(directory, ':fixture', Path('/synthetic/engine'))
        self.assertEqual(env['DISPLAY'], ':fixture')
        self.assertNotIn('WAYLAND_DISPLAY', env)

    def test_wayland_main_never_launches_xvfb_or_captures_desktop(self):
        class FakeTree:
            peak_rss = peak_pss = max_count = 1
            cpu_ticks = 0
            peak_processes = []
            names = {'fixture'}

            def sample(self, **kwargs):
                return {'rss_bytes': 1, 'pss_bytes': 1, 'pss_missing_processes': 0}

            def stop(self, process):
                pass

        binary = self.root / 'binary'
        binary.write_bytes(b'fixture')
        (self.root / '.build').mkdir(mode=0o700)
        output = self.root / 'metrics.json'
        args = ['probe', '--wayland', '--binary', str(binary), '--engine', str(binary),
                '--starts', '1', '--idle-seconds', '1', '--output', str(output)]
        original_read_text = Path.read_text
        cpuinfo = 'processor\t: 0\nCPU implementer\t: 0x41\nCPU architecture: 8\n'

        def read_text(path, *args, **kwargs):
            return cpuinfo if path == Path('/proc/cpuinfo') else original_read_text(path, *args, **kwargs)

        with patch.dict(os.environ, self.ambient, clear=True), patch('sys.argv', args), \
                patch.object(probe, 'ROOT', self.root), \
                patch.object(Path, 'read_text', read_text), \
                patch.object(probe.subprocess, 'Popen') as popen, patch.object(probe, 'screenshot') as screenshot, \
                patch.object(probe, 'launch', return_value=(SimpleNamespace(poll=lambda: None), FakeTree(), 0.1)) as launch, \
                patch.object(probe.time, 'monotonic', side_effect=itertools.count(0, 0.5)), \
                patch.object(probe.time, 'sleep'), patch.object(probe, 'command_output', return_value='fixture'), \
                contextlib.redirect_stdout(io.StringIO()):
            probe.main()
        popen.assert_not_called()
        screenshot.assert_not_called()
        self.assertEqual(launch.call_args.args[1]['GDK_BACKEND'], 'wayland')
        self.assertNotIn('DISPLAY', launch.call_args.args[1])
        metrics = json.loads(output.read_text())
        self.assertEqual(metrics['system']['cpu'], 'not measured')
        self.assertFalse(metrics['screenshot_captured'])
        self.assertEqual(metrics['display']['server'], 'Wayland')
        self.assertEqual(metrics['display']['gpu'], 'not measured')
        self.assertNotIn(str(self.endpoint), output.read_text())
        self.assertNotIn('credential-sentinel', output.read_text())
        self.assertFalse(metrics['diagnostic_disable_gdk_gl'])
        cpuinfo = 'processor\t: 0\nmodel name\t: Fixture CPU\n'
        with patch.dict(os.environ, self.ambient, clear=True), patch('sys.argv', args + ['--disable-gdk-gl']), \
                patch.object(probe, 'ROOT', self.root), patch.object(probe.subprocess, 'Popen'), \
                patch.object(Path, 'read_text', read_text), \
                patch.object(probe, 'launch', return_value=(SimpleNamespace(poll=lambda: None), FakeTree(), 0.1)) as launch, \
                patch.object(probe.time, 'monotonic', side_effect=itertools.count(0, 0.5)), \
                patch.object(probe.time, 'sleep'), patch.object(probe, 'command_output', return_value='fixture'), \
                contextlib.redirect_stdout(io.StringIO()):
            probe.main()
            self.assertEqual(launch.call_args.args[1]['GDK_GL'], 'disable')
            self.assertEqual(os.environ['GDK_GL'], 'credential-sentinel')
        self.assertTrue(json.loads(output.read_text())['diagnostic_disable_gdk_gl'])
        self.assertEqual(json.loads(output.read_text())['system']['cpu'], 'Fixture CPU')
        output.unlink()
        with patch.dict(os.environ, self.ambient, clear=True), patch('sys.argv', args), \
                patch.object(probe, 'ROOT', self.root), patch.object(probe.subprocess, 'Popen'), \
                patch.object(probe, 'launch', return_value=(SimpleNamespace(poll=lambda: 1), FakeTree(), 0.1)), \
                patch.object(probe.time, 'monotonic', side_effect=itertools.count(0, 0.5)), \
                patch.object(probe.time, 'sleep'), patch.object(probe, 'command_output', return_value='fixture'), \
                self.assertRaisesRegex(RuntimeError, 'app exited'):
            probe.main()
        self.assertFalse(output.exists(), 'an exited app must not produce successful idle metrics')


if __name__ == '__main__':
    unittest.main()
