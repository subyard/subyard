#!/usr/bin/env python3
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import processes


def proc(root, pid, parent, start, cpu=10):
    path = root/str(pid)
    path.mkdir(exist_ok=True)
    fields = ['0']*22
    for index, value in {0: 'S', 1: parent, 11: cpu, 12: 3, 19: start, 21: 2}.items():
        fields[index] = str(value)
    (path/'stat').write_text(str(pid)+' (fixture) '+' '.join(fields))
    (path/'ns').mkdir(exist_ok=True)
    for namespace in ('pid', 'cgroup'):
        if not (path/'ns'/namespace).is_symlink():
            (path/'ns'/namespace).symlink_to(namespace+':[1]')


def cg(path, cpu, populated):
    path.mkdir(exist_ok=True)
    (path/'cpu.stat').write_text(f'usage_usec {cpu}\nuser_usec {cpu-1}\nsystem_usec 1\n')
    (path/'memory.current').write_text('100')
    (path/'memory.peak').write_text('200')
    (path/'memory.swap.current').write_text('0')
    (path/'io.stat').write_text('8:1 rbytes=20 wbytes=30 rios=2 wios=3\n')
    (path/'cgroup.procs').write_text('')
    (path/'cgroup.events').write_text('populated '+str(populated))


class ProcessTest(unittest.TestCase):
    def test_start_identity_reads_stat_but_detail_requires_namespaces(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            proc(root,77,1,123)
            with patch.object(processes.os,'readlink',side_effect=PermissionError(13,'namespace read denied')) as namespace:
                self.assertEqual(processes.start_ticks(77,root),123)
                namespace.assert_not_called()
                with self.assertRaises(PermissionError):
                    processes.process(77,root)
                self.assertNotIn(77,processes.inventory(root))

    def test_external_delegation_root_generation_is_validated(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            group = root/'run'
            cg(group, 1, 0)
            cg(group/'workload', 1, 0)
            info, parent = group.stat(), root.stat()
            run = dict(agent_run_id='run', host_id='host', boot_id='boot', cgroup=str(group),
                       cgroup_identity=[info.st_dev, info.st_ino], scope_root=str(root),
                       scope_root_identity=[parent.st_dev, parent.st_ino])
            with patch.object(processes.records, 'emit', return_value=True):
                sampler = processes.Sampler(root, run, root)
                sampler.group_identity()
                run['scope_root_identity'][1] += 1
                with self.assertRaisesRegex(ValueError, 'delegation generation'):
                    sampler.group_identity()

    def test_sibling_migration_requires_writable_common_ancestor(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            (root/'proc/self').mkdir(parents=True)
            (root/'proc/self/cgroup').write_text('0::/caller')
            (root/'cgroups/caller').mkdir(parents=True)
            (root/'cgroups/target').mkdir()
            with self.assertRaisesRegex(ValueError, 'migration delegation'):
                processes.safe_placement(root/'cgroups/target', root/'proc', root/'cgroups')
            (root/'cgroups/cgroup.procs').write_text('')
            processes.safe_placement(root/'cgroups/target', root/'proc', root/'cgroups')

    def test_preexec_kernel_baselines_replay_before_short_run_final_cleanup(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            group = root/'group'
            cg(group, 11, 0)
            cg(group/'workload', 7, 0)
            info = group.stat()
            run = dict(agent_run_id='run', host_id='host', boot_id='boot', pid=10, start_ticks=100,
                       cgroup=str(group), cgroup_identity=[info.st_dev, info.st_ino], scope_start_time='2026-10-24T00:00:00.000000+00:00')
            events, blocked = [], [True]
            def emit(directory, kind, body, **options):
                if kind == 'sample' and options.get('event_id', '').endswith(':baseline') and blocked[0]:
                    return False
                events.append((kind, body, options))
                return True
            with patch.object(processes.records, 'emit', side_effect=emit), patch.object(Path, 'rmdir') as remove:
                sampler = processes.Sampler(root, run, root)
                self.assertFalse(sampler.capture_baseline())
                self.assertEqual(len(run['scope_baselines']), 2)
                self.assertFalse(sampler.poll())
                remove.assert_not_called()
                cg(group, 25, 0)
                cg(group/'workload', 18, 0)
                blocked[0] = False
                # A restarted observer's first poll is already after all work.
                self.assertTrue(processes.Sampler(root, run, root).poll())
                self.assertEqual(remove.call_count, 2)
            for suffix, initial, delta in ((':scope', 11000, 14000), (':workload', 7000, 11000)):
                samples = [(body, options) for kind, body, options in events if kind == 'sample' and body['object_id'] == 'run'+suffix]
                self.assertEqual(samples[0][0]['cpu_ns'], initial)
                self.assertFalse(samples[0][0]['final'])
                self.assertGreater(samples[0][1]['source_time'], run['scope_start_time'])
                self.assertEqual(samples[-1][0]['cpu_ns']-samples[0][0]['cpu_ns'], delta)
                for key in ('host_id', 'boot_id', 'generation'):
                    self.assertEqual(samples[0][0][key], samples[-1][0][key])
                self.assertLess(samples[0][0]['monotonic_ns'], samples[-1][0]['monotonic_ns'])

    def test_partial_descendants_continue_after_parent_and_reject_pid_reuse(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            proc(root, 10, 1, 100)
            proc(root, 11, 10, 200)
            run = dict(agent_run_id='run', host_id='host', boot_id='boot', pid=10, start_ticks=100, shared=True)
            emitted = []
            with patch.object(processes.records, 'emit', side_effect=lambda directory, kind, body: emitted.append((kind, body)) or True):
                sampler = processes.Sampler(root, run, root)
                self.assertFalse(sampler.poll())
                (root/'10/stat').unlink()
                proc(root, 11, 1, 200, 30)
                self.assertFalse(sampler.poll())
                proc(root, 11, 1, 300, 999)
                self.assertTrue(sampler.poll())
            self.assertTrue(all(body.get('quality') != 'complete' for _, body in emitted))
            self.assertFalse(any(body.get('start_ticks') == 300 for _, body in emitted))
            self.assertTrue(any(body.get('reason') == 'process_final_counter_unavailable' for _, body in emitted))
            self.assertTrue(all(body['shared'] for kind, body in emitted if kind == 'association'))
            self.assertTrue(all(body['shared'] for kind, body in emitted if kind == 'scope'))

    def test_managed_final_counter_reset_direct_and_inclusive(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            group = root/'group'
            cg(group, 20, 1)
            cg(group/'workload', 10, 1)
            info = group.stat()
            run = dict(agent_run_id='run', host_id='host', boot_id='boot', pid=10, start_ticks=100,
                       cgroup=str(group), cgroup_identity=[info.st_dev, info.st_ino])
            emitted = []
            with patch.object(processes.records, 'emit', side_effect=lambda directory, kind, body: emitted.append((kind, body)) or True), patch.object(Path, 'rmdir') as remove:
                sampler = processes.Sampler(root, run, root)
                self.assertFalse(sampler.poll())
                cg(group, 5, 0)
                cg(group/'workload', 4, 0)
                self.assertTrue(sampler.poll())
                self.assertEqual(remove.call_count, 2)
            final = [body for kind, body in emitted if kind == 'sample' and body.get('final') and body['object_id'] == 'run:scope'][0]
            self.assertEqual(final['cpu_ns'], 5000)
            self.assertEqual(final['memory_peak_bytes'], 200)
            self.assertEqual(final['io_read_bytes'], 20)
            self.assertTrue(final['counter_reset'])
            self.assertEqual(final['accounting'], 'inclusive')
            self.assertTrue(any(kind == 'scope' and body['object_id'] == 'run:workload' and body['accounting'] == 'direct' for kind, body in emitted))

    def test_source_ceiling_cannot_be_escaped(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            (root/'proc/self').mkdir(parents=True)
            (root/'proc/self/cgroup').write_text('0::/caller')
            (root/'cgroups/caller').mkdir(parents=True)
            (root/'cgroups/target').mkdir()
            (root/'cgroups/caller/memory.max').write_text('4096')
            with self.assertRaisesRegex(ValueError, 'inherited ceiling'):
                processes.safe_placement(root/'cgroups/target', root/'proc', root/'cgroups')


if __name__ == '__main__':
    unittest.main()
