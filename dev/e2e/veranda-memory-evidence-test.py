#!/usr/bin/python3
"""Host-free completed-case retention and owned cleanup boundary checks."""
import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('memory_evidence', ROOT / 'dev/e2e/veranda-memory-research.py')
memory = importlib.util.module_from_spec(spec)
spec.loader.exec_module(memory)
fixture = memory.fixture


class EvidenceChecks(unittest.TestCase):
    def test_only_documented_singleton_vm1_reaches_artifact_validation(self):
        class ReachedArtifactValidation(Exception):
            pass
        base = {'SUBYARD_E2E_VM': '1', 'SUBYARD_E2E_VM_COUNT': '1',
                'SUBYARD_E2E_PURPOSE': 'veranda-memory-research',
                'SUBYARD_E2E_RUN_ID': '1234abcd', 'SUBYARD_E2E_SLOT': 'slot-001',
                'SUBYARD_E2E_CPU_PER_VM': '4', 'SUBYARD_E2E_MEMORY_PER_VM': '4GiB',
                'SUBYARD_E2E_DISK_PER_VM': '20GiB', 'SUBYARD_E2E_BASE_FINGERPRINT': 'a' * 64}
        for kind in ('subyard-pair', 'android-test'):
            for changes, valid in (({}, True), ({'SUBYARD_E2E_VM_COUNT': '2'}, False),
                                   ({'SUBYARD_E2E_VM_COUNT': None}, False),
                                   ({'SUBYARD_E2E_VM': '2'}, False),
                                   ({'SUBYARD_E2E_CPU_PER_VM': None}, False),
                                   ({'SUBYARD_E2E_MEMORY_PER_VM': 'private-sentinel'}, False),
                                   ({'SUBYARD_E2E_PURPOSE': 'other'}, False)):
                env = dict(base, SUBYARD_E2E_TYPE=kind)
                env.update(changes)
                env = {key: value for key, value in env.items() if value is not None}
                with self.subTest(kind=kind, changes=changes), patch.dict(os.environ, env, clear=True), \
                        patch.object(memory.os, 'getuid', return_value=1000), \
                        patch.object(sys, 'argv', ['memory', 'fixed.deb', 'fixed-engine']), \
                        patch.object(memory.Path, 'lstat', side_effect=ReachedArtifactValidation) as artifact, \
                        patch.object(memory.subprocess, 'run') as native, \
                        patch.object(memory.subprocess, 'Popen') as child:
                    with self.assertRaises(ReachedArtifactValidation if valid else ValueError):
                        memory.main()
                    self.assertEqual(artifact.call_count, 1 if valid else 0)
                    native.assert_not_called()
                    child.assert_not_called()

    def test_platform_names_with_underscore_suffix_keep_one_owned_lifecycle(self):
        for suffix in ('1_nd72r3', 'trailing_', '________'):
            for fail_body in (False, True):
                with self.subTest(suffix=suffix, fail_body=fail_body), \
                        tempfile.TemporaryDirectory(prefix='veranda-platform-name-', dir='/tmp') as temporary:
                    root = Path(temporary)
                    root.chmod(0o700)
                    home, state = root / 'home', root / ('veranda-wayland-' + suffix)
                    for directory in (home, home / '.cache', state):
                        directory.mkdir(mode=0o700)
                        directory.chmod(0o700)
                    platform = home / '.cache/subyard-e2e-platform'
                    commands, events, pending_markers = [], [], []
                    engine = root / 'yard'
                    fixture.probe.private_file(engine, 'not executed')
                    original = subprocess.CalledProcessError(17, ['fixed-body'])
                    def host_command(argv, **options):
                        # Simulate only owned install/marker operations; never invoke sudo.
                        self.assertEqual(argv[:2], ['sudo', '-n'])
                        if argv[2:4] == ['install', '-d']:
                            self.assertEqual(Path(argv[-1]), platform)
                            platform.mkdir(mode=0o711)
                            platform.chmod(0o711)
                        elif argv[2] == 'install':
                            self.assertEqual(argv[-4:-2], ['-m', '0600'])
                            pending_markers.append(Path(argv[-1]).name)
                            fixture.probe.private_file(Path(argv[-1]), Path(argv[-2]).read_text())
                        else:
                            self.assertEqual(argv[2:4], ['mv', '-fT'])
                            Path(argv[-2]).replace(argv[-1])
                        return SimpleNamespace(returncode=0)
                    def product(argv, env, log, **options):
                        self.assertEqual(argv[0], str(engine))
                        self.assertEqual(argv[1], '-Y')
                        self.assertEqual(argv[-1], '--yes')
                        self.assertIs(options['privileged'], True)
                        # The actual generated argv must satisfy the reported Incus boundary.
                        self.assertRegex('subyard-' + argv[2], r'^[A-Za-z0-9][A-Za-z0-9-]*[A-Za-z0-9]$')
                        commands.append(argv)
                        if argv[3] == 'init':
                            (platform / 'incus').mkdir(mode=0o700)
                            (platform / 'incus').chmod(0o700)
                        return 0
                    with patch.object(fixture.pwd, 'getpwuid', return_value=SimpleNamespace(pw_dir=str(home))), \
                            patch.object(fixture.subprocess, 'run', side_effect=host_command), \
                            patch.object(fixture, 'run_probe', side_effect=product), \
                            patch.object(fixture, 'product_stop_check'), \
                            patch.object(fixture.probe, 'isolated_defaults'):
                        if fail_body:
                            with self.assertRaises(subprocess.CalledProcessError) as caught:
                                with fixture.prepared_platform(state, engine, lambda *event: events.append(event)):
                                    raise original
                            self.assertIs(caught.exception, original)
                        else:
                            with fixture.prepared_platform(state, engine, lambda *event: events.append(event)):
                                pass
                    self.assertEqual([argv[3:-1] for argv in commands],
                                     [['init'], ['teardown', '--keep-data'], ['teardown']])
                    names = {argv[2] for argv in commands}
                    self.assertEqual(len(names), 1)
                    self.assertEqual([path.name for path in (state / 'prepare-config/yards').iterdir()], list(names))
                    self.assertEqual(pending_markers, ['.subyard-e2e-platform-marker.' + next(iter(names))])
                    self.assertEqual((platform / '.subyard-e2e-platform-marker').read_text(),
                                     'subyard-e2e-platform-v1\n')
                    self.assertEqual(events, [('start', 'platform', None, 'started'), ('end', 'platform', 0, 'passed')])

    def test_installer17_survives_end_phase_reporting_failure(self):
        args = SimpleNamespace(active_case='F', active_repeat=1, phase='case-install', child_exit=None)
        original, frames = subprocess.CalledProcessError(17, ['fixed-installer']), []
        def emit(text, **options):
            frame = json.loads(text.split(' ', 1)[1])
            frames.append(frame)
            if frame['state'] == 'end':
                raise BrokenPipeError('closed evidence stream')
        with patch.object(memory, 'print', side_effect=emit, create=True), \
                patch.object(memory.time, 'monotonic', side_effect=[10, 11]), \
                contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(subprocess.CalledProcessError) as caught:
                with memory.case_phase(args, 'install'):
                    raise original
        self.assertIs(caught.exception, original)
        self.assertEqual(frames, [{'schema_version': 1, 'case': 'F', 'repeat': 1, 'stage': 'install',
                                  'state': 'start', 'duration_seconds': 0, 'exit_code': None, 'status': 'started'},
                                 {'schema_version': 1, 'case': 'F', 'repeat': 1, 'stage': 'install',
                                  'state': 'end', 'duration_seconds': 1, 'exit_code': 17, 'status': 'failed'}])
        self.assertEqual((args.phase, args.child_exit), ('case-install', None))

    def test_successful_native_body_cannot_hide_failed_phase_reporting(self):
        args = SimpleNamespace(active_case='M', active_repeat=2, phase='case-probe', child_exit=None)
        original, calls = BrokenPipeError('closed evidence stream'), []
        def emit(text, **options):
            if json.loads(text.split(' ', 1)[1])['state'] == 'end':
                raise original
        with patch.object(memory, 'print', side_effect=emit, create=True):
            with self.assertRaises(BrokenPipeError) as caught:
                with memory.case_phase(args, 'probe'):
                    calls.append('native completed')
                    args.child_exit = 0
        self.assertIs(caught.exception, original)
        self.assertEqual(calls, ['native completed'])
        self.assertEqual((args.phase, args.child_exit), ('case-probe', 0))

    def test_failed_probe_keeps_actual_child_exit_separate_from_cleanup_failure(self):
        for child_exit in (17, 0):
            with self.subTest(child_exit=child_exit):
                args = SimpleNamespace(active_case='N', active_repeat=3, phase='cgroup-cleanup', child_exit=child_exit)
                original, capture = subprocess.CalledProcessError(124, ['fixed-cleanup']), io.StringIO()
                with contextlib.redirect_stdout(capture):
                    with self.assertRaises(subprocess.CalledProcessError) as caught:
                        with memory.case_phase(args, 'probe'):
                            raise original
                frame = json.loads(capture.getvalue().splitlines()[-1].split(' ', 1)[1])
                self.assertIs(caught.exception, original)
                self.assertEqual((frame['status'], frame['exit_code']), ('failed', child_exit))
                self.assertEqual((args.phase, args.child_exit), ('cgroup-cleanup', child_exit))

    def test_guard_and_unavailable_probe_exit_remain_null(self):
        for stage, child_exit in (('verification', 0), ('probe', None)):
            with self.subTest(stage=stage):
                args = SimpleNamespace(active_case='A', active_repeat=1, phase='case-metrics', child_exit=child_exit)
                original, capture = ValueError('guard failed'), io.StringIO()
                with contextlib.redirect_stdout(capture):
                    with self.assertRaises(ValueError) as caught:
                        with memory.case_phase(args, stage):
                            raise original
                frame = json.loads(capture.getvalue().splitlines()[-1].split(' ', 1)[1])
                self.assertIs(caught.exception, original)
                self.assertEqual((frame['status'], frame['exit_code']), ('failed', None))

    def test_legacy_comparison_cases_keep_successful_timing_frames(self):
        for case in ('A', 'D-image', 'C-on-A'):
            with self.subTest(case=case):
                args = SimpleNamespace(active_case=case, active_repeat=1, phase='case-install', child_exit=None)
                capture = io.StringIO()
                with contextlib.redirect_stdout(capture):
                    with memory.case_phase(args, 'install'):
                        pass
                frames = [json.loads(line.split(' ', 1)[1]) for line in capture.getvalue().splitlines()]
                self.assertEqual([frame['state'] for frame in frames], ['start', 'end'])
                self.assertEqual([(frame['case'], frame['stage']) for frame in frames], [(case, 'install')] * 2)
                self.assertEqual((frames[-1]['status'], frames[-1]['exit_code']), ('passed', 0))
                self.assertGreaterEqual(frames[-1]['duration_seconds'], 0)

    def test_same_deb_content_skips_all_repeated_installs(self):
        with tempfile.TemporaryDirectory(prefix='veranda-install-', dir='/tmp') as directory:
            Path(directory).chmod(0o700)
            a, alias = Path(directory) / 'a.deb', Path(directory) / 'alias.deb'
            for path in (a, alias):
                path.write_bytes(b'candidate A')
                path.chmod(0o600)
            original_read = Path.read_bytes
            executable = b'installed executable A'
            binary_sha = hashlib.sha256(executable).hexdigest()
            cache = hashlib.sha256(a.read_bytes()).hexdigest()
            def read(path):
                return executable if path == Path('/usr/bin/subyard-veranda') else original_read(path)
            with patch.object(memory.Path, 'read_bytes', read), \
                    patch.object(memory, 'installed_binary', return_value=binary_sha) as payload, \
                    patch.object(memory.subprocess, 'run') as installer:
                for index in range(9):
                    cache, actual = memory.install_candidate(a if index % 2 else alias, cache, io.StringIO())
                    self.assertEqual(actual, binary_sha)
                installer.assert_not_called()
                self.assertEqual(payload.call_count, 9)
            self.assertEqual(cache, hashlib.sha256(b'candidate A').hexdigest())

    def test_changed_deb_and_reversal_each_reinstall_once(self):
        with tempfile.TemporaryDirectory(prefix='veranda-install-', dir='/tmp') as directory:
            Path(directory).chmod(0o700)
            a, b = Path(directory) / 'a.deb', Path(directory) / 'b.deb'
            for path, content in ((a, b'candidate A'), (b, b'candidate B')):
                path.write_bytes(content)
                path.chmod(0o600)
            original_read, installed = Path.read_bytes, [b'executable A']
            expected = {a: b'executable A', b: b'executable B'}
            def read(path):
                return installed[0] if path == Path('/usr/bin/subyard-veranda') else original_read(path)
            def payload(path):
                return hashlib.sha256(expected[path]).hexdigest()
            def install(argv, **options):
                installed[0] = expected[Path(argv[-1])]
            log, cache = io.StringIO(), hashlib.sha256(a.read_bytes()).hexdigest()
            with patch.object(memory.Path, 'read_bytes', read), \
                    patch.object(memory, 'installed_binary', side_effect=payload), \
                    patch.object(memory.subprocess, 'run', side_effect=install) as installer:
                for path in (a, b, b, a, a):
                    cache, binary_sha = memory.install_candidate(path, cache, log)
                    self.assertEqual(cache, hashlib.sha256(original_read(path)).hexdigest())
                    self.assertEqual(binary_sha, payload(path))
                self.assertEqual([call.args[0][-1] for call in installer.call_args_list], [str(b), str(a)])
                for call in installer.call_args_list:
                    self.assertEqual(call.args[0][:-1], ['sudo', '-n', 'env', 'DEBIAN_FRONTEND=noninteractive',
                                                       'apt-get', 'install', '--reinstall', '-y', '--no-install-recommends'])
                    self.assertEqual(call.kwargs, {'stdout': log, 'stderr': subprocess.STDOUT, 'check': True, 'timeout': 180})

    def test_failed_installer_preserves_native17_and_cached_identity(self):
        original = subprocess.CalledProcessError(17, ['fixed-installer'])
        cache = ('a' * 64, 'b' * 64)
        with patch.object(memory.Path, 'read_bytes', return_value=b'changed candidate'), \
                patch.object(memory, 'installed_binary', return_value='c' * 64), \
                patch.object(memory.subprocess, 'run', side_effect=original):
            with self.assertRaises(subprocess.CalledProcessError) as caught:
                cache = memory.install_candidate(Path('changed.deb'), cache[0], io.StringIO())
        self.assertIs(caught.exception, original)
        self.assertEqual(caught.exception.returncode, 17)
        self.assertEqual(cache, ('a' * 64, 'b' * 64))

    def test_payload_executable_mismatch_cannot_advance_cache(self):
        content = b'candidate A'
        cache = (hashlib.sha256(content).hexdigest(), 'b' * 64)
        with patch.object(memory.Path, 'read_bytes', return_value=content), \
                patch.object(memory, 'installed_binary', return_value='c' * 64), \
                patch.object(memory.subprocess, 'run') as installer:
            with self.assertRaises(ValueError):
                cache = memory.install_candidate(Path('candidate.deb'), cache[0], io.StringIO())
            installer.assert_not_called()
        self.assertEqual(cache, (hashlib.sha256(content).hexdigest(), 'b' * 64))

    def test_fixture_watchdog_preserves_original_identity_deletion_script(self):
        calls = []
        with patch.object(fixture.subprocess, 'run', side_effect=lambda argv, **options: calls.append((argv, options))):
            fixture.cleanup_state(Path('/var/tmp/veranda-wayland-owned'), (1001, 2, 3))
        argv, options = calls[0]
        self.assertEqual(argv[:6], ['sudo', '-n', 'timeout', '--signal=TERM', '--kill-after=2s', '10s'])
        self.assertEqual(argv[6:8], [sys.executable, '-c'])
        self.assertEqual(hashlib.sha256(argv[8].encode()).hexdigest(),
                         '93182e864656a69a11da6a3f198551d8f83b2a23d780708c24743eb500068d59')
        self.assertEqual(argv[9:], ['/var/tmp/veranda-wayland-owned', '1001', '2', '3'])
        self.assertEqual(options['timeout'], 15)
        self.assertIs(options['check'], True)
        for name in ('stdin', 'stdout', 'stderr'):
            self.assertEqual(options[name], subprocess.DEVNULL)

    def test_original_body_error_survives_cleanup_failure(self):
        original = ValueError('native failure')
        events = []
        def fail():
            raise subprocess.CalledProcessError(124, ['fixed-cleanup'])
        with contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(ValueError) as caught:
                with fixture.cleanup_on_exit(fail, 'fixture', lambda *event: events.append(event)):
                    raise original
        self.assertIs(caught.exception, original)
        self.assertEqual(events, [('start', 'fixture', None, 'started'), ('end', 'fixture', 124, 'failed')])

    def test_failed_reporting_still_cleans_and_preserves_action_error(self):
        calls = []
        def observer(*_):
            raise BrokenPipeError()
        with self.assertRaises(BrokenPipeError):
            with fixture.cleanup_on_exit(lambda: calls.append('cleaned'), 'fixture', observer):
                pass
        self.assertEqual(calls, ['cleaned'])
        original = subprocess.CalledProcessError(17, ['fixed-cleanup'])
        def fail():
            raise original
        with self.assertRaises(subprocess.CalledProcessError) as caught:
            with fixture.cleanup_on_exit(fail, 'fixture', observer):
                pass
        self.assertIs(caught.exception, original)

    def test_faulting_stderr_cannot_replace_original_body_error(self):
        class FaultingStderr(io.StringIO):
            def write(self, _):
                raise BrokenPipeError('diagnostic destination closed')
        original = ValueError('native failure')
        calls = []
        def fail():
            calls.append('cleanup-attempted')
            raise subprocess.CalledProcessError(124, ['fixed-cleanup'])
        with contextlib.redirect_stderr(FaultingStderr()):
            with self.assertRaises(ValueError) as caught:
                with fixture.cleanup_on_exit(fail, 'fixture'):
                    raise original
        self.assertIs(caught.exception, original)
        self.assertEqual(calls, ['cleanup-attempted'])

    def run_main(self, fixture_failure=False, native_failure=False):
        with tempfile.TemporaryDirectory(prefix='veranda-memory-evidence-', dir='/tmp') as directory:
            state = Path(directory)
            state.chmod(0o700)
            deb, engine = state / 'candidate.deb', state / 'engine'
            for path in (deb, engine):
                fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
                os.fchmod(fd, 0o600)
                os.close(fd)
            setup = {'screen': {'width': 1920, 'height': 1080, 'refresh_millihertz': 60000, 'scale': 1}}
            case = {'case': 'A', 'repeat': 1, 'deb_sha256': 'a' * 64,
                    'installed_binary_sha256': 'b' * 64, 'allocation_mode': None,
                    'metrics': {'inputs': {'binary_sha256': 'b' * 64, 'engine_sha256': 'c' * 64}}}
            commands, order = [], []
            def command(argv, **options):
                commands.append((argv, options))
                if argv[0] == 'dpkg-query':
                    return SimpleNamespace(returncode=1)
                return SimpleNamespace(returncode=0)
            def output(argv):
                return 'Package: subyard-veranda\nVersion: 0.1.1\nArchitecture: amd64' if argv[0] == 'dpkg-deb' else 'engine 0.1.1'
            def cleanup_state(*_):
                order.append('fixture')
                if fixture_failure:
                    raise subprocess.CalledProcessError(124, ['fixed-fixture'])
            @contextlib.contextmanager
            def platform(state, engine, observer):
                with fixture.cleanup_on_exit(lambda: order.append('platform'), 'platform', observer):
                    yield
            def measure(state, args, cleanup):
                with fixture.cleanup_on_exit(lambda: cleanup.update(compositor='passed'), 'compositor', args.cleanup_observer):
                    with fixture.cleanup_on_exit(lambda: cleanup.update(cgroup='passed'), 'app cgroup', args.cleanup_observer):
                        args.child_exit = 0
                    memory.record_case(args, case, setup)
                    if native_failure:
                        args.child_exit = 9
                        raise ValueError('later native probe failure')
                return setup
            environment = {'SUBYARD_E2E_VM': '1', 'SUBYARD_E2E_TYPE': 'android-test',
                           'SUBYARD_E2E_VM_COUNT': '1', 'SUBYARD_E2E_CPU_PER_VM': '4',
                           'SUBYARD_E2E_MEMORY_PER_VM': '8GiB', 'SUBYARD_E2E_DISK_PER_VM': '40GiB',
                           'SUBYARD_E2E_BASE_FINGERPRINT': 'a' * 64,
                           'SUBYARD_E2E_PURPOSE': 'veranda-memory-research',
                           'SUBYARD_E2E_RUN_ID': '1234abcd', 'SUBYARD_E2E_SLOT': 'slot-001'}
            capture = io.StringIO()
            with patch.dict(os.environ, environment), patch.object(sys, 'argv', ['memory', str(deb), str(engine)]), \
                    patch.object(memory.os, 'getuid', return_value=1001), \
                    patch.object(memory.subprocess, 'run', side_effect=command), \
                    patch.object(memory.probe, 'command_output', side_effect=output), \
                    patch.object(memory.probe, 'resource_witness', return_value={'schema_version': 1}), \
                    patch.object(memory, 'group_command'), patch.object(memory, 'measure', side_effect=measure), \
                    patch.object(fixture, 'create_state', return_value=(state, (1001, 2, 3))), \
                    patch.object(fixture, 'cleanup_state', side_effect=cleanup_state), \
                    patch.object(fixture, 'prepared_platform', platform), \
                    contextlib.redirect_stdout(capture), contextlib.redirect_stderr(io.StringIO()):
                rc = memory.main()
            frames = [(line.split(' ', 1)[0], json.loads(line.split(' ', 1)[1])) for line in capture.getvalue().splitlines()]
            case_index = next(i for i, item in enumerate(frames) if item[0] == 'VERANDA_MEMORY_CASE')
            fixture_start = next(i for i, (prefix, value) in enumerate(frames)
                                 if prefix == 'VERANDA_MEMORY_CLEANUP' and value['label'] == 'fixture' and value['phase'] == 'start')
            self.assertLess(case_index, fixture_start)
            self.assertEqual(frames[case_index][1], {'schema_version': 1, 'case': case, 'setup': setup})
            self.assertEqual(frames[-1][0], 'VERANDA_MEMORY_RESULT')
            receipt = frames[-1][1]
            self.assertEqual(receipt['cases'], [case])
            events = receipt['cleanup_events']
            self.assertEqual({event['label'] for event in events}, {'cgroup', 'compositor', 'platform', 'package', 'fixture'})
            for label in {'cgroup', 'compositor', 'platform', 'package', 'fixture'}:
                self.assertEqual([event['phase'] for event in events if event['label'] == label], ['start', 'end'])
            self.assertEqual(order, ['platform', 'fixture'])
            package = next((argv, options) for argv, options in commands if 'remove' in argv)
            self.assertEqual(package[0], ['sudo', '-n', 'timeout', '--signal=TERM', '--kill-after=2s', '60s',
                                          'apt-get', 'remove', '-y', 'subyard-veranda'])
            self.assertEqual(package[1]['timeout'], 65)
            self.assertIs(package[1]['check'], True)
            return rc, receipt

    def test_case_survives_fixture_failure_without_false_measured_result(self):
        rc, receipt = self.run_main(fixture_failure=True)
        self.assertEqual(rc, 1)
        self.assertEqual(receipt['result'], 'failed')
        self.assertEqual(receipt['cleanup']['fixture'], 'failed')
        self.assertEqual(receipt['phase'], 'fixture-cleanup')
        self.assertEqual(receipt['cleanup_events'][-1],
                         {'phase': 'end', 'label': 'fixture', 'exit_code': 124, 'status': 'failed'})

    def test_original_native_failure_survives_later_failed_fixture(self):
        rc, receipt = self.run_main(fixture_failure=True, native_failure=True)
        self.assertEqual(rc, 1)
        self.assertEqual(receipt['child_exit'], 9)
        self.assertEqual(receipt['cleanup']['fixture'], 'failed')
        self.assertEqual(receipt['result'], 'failed')

    def test_measured_result_only_after_complete_owned_cleanup(self):
        rc, receipt = self.run_main()
        self.assertEqual(rc, 0)
        self.assertEqual(receipt['result'], 'measured')
        self.assertEqual(receipt['phase'], 'complete')
        self.assertTrue(all(value == 'passed' for value in receipt['cleanup'].values()))


if __name__ == '__main__':
    unittest.main()
