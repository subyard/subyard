#!/usr/bin/env python3
"""Small host-free checks for resource accounting and acknowledged window states."""
import importlib.util
import os
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('veranda_probe', Path(__file__).with_name('measure-veranda.py'))
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)


class MemoryProbeTest(unittest.TestCase):
    def test_native_readiness_does_not_claim_a_frontend_frame(self):
        marker = b'VERANDA_ALLOCATION {"mode":"native-only","readiness":"native-workload-complete; no frontend frame"}\n'
        self.assertEqual(probe.readiness_kind(b'VERANDA_READY\n'), 'frontend-two-frame-marker')
        self.assertEqual(probe.readiness_kind(marker + b'VERANDA_READY\n'),
                         'native-workload-complete; no frontend frame')
        for invalid in (marker + marker, b'VERANDA_ALLOCATION []\n',
                        b'VERANDA_ALLOCATION invalid\n', marker.replace(b'native-only', b'full')):
            with self.assertRaisesRegex(RuntimeError, 'invalid allocation readiness'):
                probe.readiness_kind(invalid)

    def test_privileged_attach_is_fixed_bounded_explicit_and_confirms_membership(self):
        group = probe.AppCgroup.__new__(probe.AppCgroup)
        group.path = Path('/sys/fs/cgroup/subyard-veranda-memory-fixture')
        group.identity = (12, 34)
        group.privileged_attach = True
        table = {10: {'start': 100, 'parent': 20}, 20: {'start': 200, 'parent': 1}}
        with patch.object(group, 'require_empty'), patch.object(group, 'members', return_value={10}), \
                patch.object(probe.os, 'getpid', return_value=20), \
                patch.object(probe, 'processes', return_value=table), \
                patch.object(probe.subprocess, 'run') as run:
            group.attach(10)
        run.assert_called_once_with(
            ['sudo', '-n', probe.sys.executable, str(probe.ROOT / 'dev/e2e/veranda-memory-research.py'),
             '--attach-cgroup', str(group.path), '10', '100', '20', '200', '12', '34'],
            stdout=probe.subprocess.DEVNULL, stderr=probe.subprocess.DEVNULL, timeout=5, check=True)
        with patch.object(group, 'require_empty'), patch.object(probe, 'processes', return_value={}), \
                patch.object(probe.subprocess, 'run') as run, self.assertRaises(RuntimeError):
            group.attach(10)
        run.assert_not_called()
        with patch.object(group, 'require_empty'), patch.object(group, 'members', return_value=set()), \
                patch.object(probe.os, 'getpid', return_value=20), \
                patch.object(probe, 'processes', return_value=table), \
                patch.object(probe.subprocess, 'run'), self.assertRaisesRegex(RuntimeError, 'migration was not confirmed'):
            group.attach(10)
        group.privileged_attach = False
        with patch.object(group, 'require_empty'), patch.object(probe.os, 'open', side_effect=PermissionError), \
                patch.object(probe.subprocess, 'run') as run, self.assertRaises(PermissionError):
            group.attach(10)
        run.assert_not_called()

    def test_metrics_use_exact_private_mode_and_refuse_links_before_truncation(self):
        with tempfile.TemporaryDirectory(prefix='veranda-output-test-', dir='/tmp') as temporary:
            root = Path(temporary)
            output = root / 'metrics.json'
            old_umask = os.umask(0)
            try:
                probe.write_metrics(output, 'first receipt')
            finally:
                os.umask(old_umask)
            self.assertEqual(output.stat().st_mode & 0o777, 0o600)
            output.chmod(0o644)
            probe.write_metrics(output, 'replacement receipt')
            self.assertEqual(output.stat().st_mode & 0o777, 0o600)
            self.assertEqual(output.read_text(), 'replacement receipt')
            link = root / 'symbolic'
            link.symlink_to(output)
            with self.assertRaises(OSError):
                probe.write_metrics(link, 'discard')
            hardlink = root / 'hardlink'
            os.link(output, hardlink)
            with self.assertRaises(RuntimeError):
                probe.write_metrics(output, 'discard')
            self.assertEqual(output.read_text(), 'replacement receipt')

    def test_launch_exec_is_gated_until_migration_and_keeps_starttime(self):
        with tempfile.TemporaryDirectory(prefix='veranda-gate-test-', dir='/tmp') as temporary:
            root = Path(temporary)
            marker = root / 'executed'
            binary = root / 'fixture'
            binary.write_text('#!/usr/bin/python3\nimport pathlib,time\n'
                              f'pathlib.Path({str(marker)!r}).touch()\n'
                              'print("VERANDA_READY",flush=True)\ntime.sleep(30)\n')
            binary.chmod(0o700)
            class GateCgroup:
                pid = None
                start = None
                def require_empty(self):
                    assert not self.members()
                def members(self):
                    return {self.pid} if self.pid in probe.processes() else set()
                def memory_current(self):
                    return None
                def attach(self, pid):
                    assert not marker.exists(), 'app executed before migration'
                    self.pid = pid
                    self.start = probe.processes()[pid]['start']
            group = GateCgroup()
            process, tree, _ = probe.launch(binary, {'PATH': '/usr/bin:/bin'}, 3, cgroup=group)
            try:
                self.assertTrue(marker.exists())
                self.assertEqual(probe.processes()[process.pid]['start'], group.start)
                self.assertEqual(tree.sample(pss=True)['process_count'], 1)
            finally:
                tree.stop(process)
                process.stdout.close()
            self.assertFalse(group.members())

    def test_cgroup_retains_reparented_identities_and_sums_current_memory(self):
        # PID 20 has detached before its first observation; neither ancestry nor
        # the launcher's group can discover it. PID 30 is unrelated and excluded.
        members = {10, 20}
        cgroup = SimpleNamespace(members=lambda: members, memory_current=lambda: 999)
        table = {pid: {'start': pid + 100, 'parent': 1, 'group': pid,
                       'ticks': pid, 'rss': pid, 'threads': 2, 'name': 'fixture'}
                 for pid in (10, 20, 30)}
        rollup = 'header\nRss: 100 kB\nPss: 70 kB\nPrivate_Clean: 5 kB\nPrivate_Dirty: 10 kB\nSwap: 3 kB\n'
        def read(path):
            if path.name == 'stat':
                return '1 (fixture) ' + ' '.join(['0'] * 19 + [str(table[int(path.parent.name)]['start'])])
            return rollup
        with patch.object(probe, 'processes', return_value=table), \
                patch.object(probe, 'process_role', return_value='fixture'), \
                patch.object(Path, 'read_text', read), \
                patch.object(Path, 'iterdir', return_value=iter(())):
            tree = probe.Tree(10, cgroup=cgroup)
            first = tree.sample(pss=True)
            self.assertEqual({entry['pid'] for entry in first['processes']}, members)
            self.assertEqual(first['rss_bytes'], 200 * 1024)
            self.assertEqual(first['pss_bytes'], 140 * 1024)
            self.assertEqual(first['private_bytes'], 30 * 1024)
            self.assertEqual(first['swap_bytes'], 6 * 1024)
            self.assertEqual(first['threads'], 4)
            self.assertEqual(first['roles']['fixture']['rss_bytes'], first['rss_bytes'])
            self.assertEqual(first['cgroup_memory_current_bytes'], 999)
            members.remove(20)
            table[20] = dict(table[20], start=9999, group=10)
            second = tree.sample(pss=True)
            self.assertEqual(second['process_count'], 1, 'recycled PID/group imported a foreign process')
            history = tree.process_history()
            self.assertEqual(len(history), 2)
            self.assertFalse(next(entry for entry in history if entry['pid'] == 20)['alive_at_end'])
            self.assertEqual(tree.cpu_ticks, 30, 'CPU must include newly observed owned children')

    def test_history_retains_a_role_observed_after_exec(self):
        table = {10: {'start': 100, 'parent': 1, 'group': 10,
                      'ticks': 0, 'rss': 123, 'threads': 1, 'name': 'fixture'}}
        with patch.object(probe, 'processes', return_value=table), \
                patch.object(probe, 'process_role', side_effect=['other-app-helper', 'webkit-web']):
            tree = probe.Tree(10)
            tree.sample()
            tree.sample()
            self.assertEqual(tree.process_history()[0]['observed_roles'], ['other-app-helper', 'webkit-web'])

    def test_missing_rollup_remains_explicit(self):
        table = {10: {'start': 100, 'parent': 1, 'group': 10,
                      'ticks': 0, 'rss': 123, 'threads': 1, 'name': 'fixture'}}
        with patch.object(probe, 'processes', return_value=table), \
                patch.object(probe, 'process_role', return_value='fixture'), \
                patch.object(Path, 'read_text', side_effect=PermissionError), \
                patch.object(Path, 'iterdir', side_effect=PermissionError):
            sample = probe.Tree(10).sample(pss=True)
        self.assertEqual(sample['rss_bytes'], 123)
        self.assertEqual(sample['pss_missing_processes'], 1)
        self.assertEqual(sample['fd_missing_processes'], 1)
        self.assertIsNone(sample['processes'][0]['pss_bytes'])

    def test_state_proof_requires_singleton_and_compositor_acknowledgement(self):
        control = probe.WlrToplevel('/synthetic/wayland', 'com.subyard.veranda')
        with patch.object(control, 'command', side_effect=['com.subyard.veranda: ignored title\n', '']) as command:
            proof = control.check('minimized')
        self.assertTrue(proof['minimized_acknowledged'])
        self.assertEqual(command.call_args.args, ('find', 'app_id:com.subyard.veranda', 'state:minimized', 'state:inactive'))
        self.assertNotIn('ignored title', str(proof))
        for output in ('', 'other: title\n', 'com.subyard.veranda: one\ncom.subyard.veranda: two\n'):
            with patch.object(control, 'command', return_value=output), self.assertRaises(RuntimeError):
                control.check('visible')
        with patch.object(control, 'command', side_effect=['com.subyard.veranda: title\n', RuntimeError('no ack')]), \
                self.assertRaisesRegex(RuntimeError, 'no ack'):
            control.check('minimized')

    def test_idle_statistics_use_simultaneous_sums_and_one_cpu(self):
        clock = [0.0]
        class SampleTree:
            cpu_ticks = 0
            count = 0
            def sample(self, **_):
                self.count += 1
                self.cpu_ticks += probe.TICKS // 10
                return {'rss_bytes': self.count * 100, 'pss_bytes': self.count * 70,
                        'pss_missing_processes': 0}
        def sleep(seconds):
            clock[0] += seconds
        with patch.object(probe.time, 'monotonic', side_effect=lambda: clock[0]), \
                patch.object(probe.time, 'sleep', side_effect=sleep):
            result = probe.measure_idle(SimpleNamespace(poll=lambda: None), SampleTree(), 2, 0)
        self.assertEqual([sample['elapsed_seconds'] for sample in result['samples']], [1, 2])
        self.assertEqual(result['statistics']['rss_bytes'], {'median': 250, 'p95': 300,
                                                            'observed_max': 300, 'sample_count': 2})
        expected_cpu = 100 * (probe.TICKS // 10) / probe.TICKS
        self.assertAlmostEqual(result['cpu_percent_one_core'], expected_cpu)
        self.assertAlmostEqual(result['samples'][0]['cpu_percent_one_core'], expected_cpu)


if __name__ == '__main__':
    unittest.main()
