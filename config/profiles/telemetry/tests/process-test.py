#!/usr/bin/env python3
from contextlib import closing
import errno
import importlib.util
import json
from pathlib import Path
import sqlite3
import sys
import tempfile
import unittest
from unittest.mock import patch
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import processes
from spool_fixture import prepare
spec = importlib.util.spec_from_file_location('process_scope', Path(__file__).parent/'e2e/process-scope.py')
scope_fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(scope_fixture)


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
    def test_parent_cleanup_waits_for_nested_scope_retirement(self):
        for restart in (False, True):
            with self.subTest(restart=restart), tempfile.TemporaryDirectory(dir='/tmp') as name:
                root = Path(name)
                group = root/'parent'
                cg(group, 20, 0)
                workload = group/'workload'
                cg(workload, 10, 0)
                child = group/'run-child'
                child.mkdir()
                info = group.stat()
                run = dict(agent_run_id='parent', host_id='host', boot_id='boot', pid=10, start_ticks=100,
                           cgroup=str(group), cgroup_identity=[info.st_dev, info.st_ino])
                events = []
                def emit(directory, kind, body, **options):
                    events.append((kind, body))
                    return True
                def kernel_rmdir(path):
                    # Cgroup pseudo-files do not prevent removal, child groups do.
                    if any(entry.is_dir() for entry in path.iterdir()):
                        raise OSError(errno.ENOTEMPTY, 'nested scope remains')
                    path.rename(root/('retired-'+path.name))
                with patch.object(processes.records, 'emit', side_effect=emit), patch.object(Path, 'rmdir', kernel_rmdir):
                    sampler = processes.Sampler(root, run, root)
                    self.assertFalse(sampler.poll())
                    self.assertTrue(workload.is_dir(), 'parent must remain sampleable while child finalization retries')
                    self.assertFalse(any(body.get('resource_completed') for _, body in events))
                    child.rename(root/'retired-child')
                    if restart:
                        sampler = processes.Sampler(root, run, root)
                    self.assertTrue(sampler.poll())
                    self.assertFalse(group.exists())
                finals = {body['object_id']: body for kind, body in events if kind == 'sample' and body.get('final')}
                self.assertEqual(finals['parent:scope']['cpu_ns'], 20000)
                self.assertEqual(finals['parent:workload']['cpu_ns'], 10000)
                self.assertTrue(any(body.get('resource_completed') for _, body in events))

    def test_nested_final_diagnostic_identifies_each_missing_condition(self):
        conditions = ('parent_final', 'parent_journal_removed', 'child_final',
                      'child_journal_removed', 'child_scope_present', 'child_run_present')
        cases = [(), *[(condition,) for condition in conditions], conditions]
        for missing in cases:
            with self.subTest(missing=missing), tempfile.TemporaryDirectory(dir='/tmp') as name:
                data = Path(name)
                journals = data/'runs-v5'
                journals.mkdir(mode=0o700)
                with closing(sqlite3.connect(data/'telemetry.sqlite')) as writer:
                    writer.executescript('''CREATE TABLE samples(object_id TEXT,source_time INTEGER,body TEXT);
                        CREATE TABLE objects(object_id TEXT,parent_object_id TEXT);
                        CREATE TABLE runs(agent_run_id TEXT,body TEXT);''')
                    for role in ('parent', 'child'):
                        writer.execute('INSERT INTO samples VALUES(?,?,?)', (role+':scope', 1, json.dumps(dict(
                            final=True, populated=role+'_final' in missing, cpu_ns=123, monotonic_ns=456,
                            private='PRIVATE_SAMPLE'))))
                        if role+'_journal_removed' in missing:
                            (journals/(role+'.json')).write_text('{"private":"PRIVATE_JOURNAL"}')
                    if 'child_scope_present' not in missing:
                        writer.execute('INSERT INTO objects VALUES(?,?)', ('child:scope', 'parent:scope'))
                    if 'child_run_present' not in missing:
                        writer.execute('INSERT INTO runs VALUES(?,?)', ('child', json.dumps(dict(
                            parent_run_id='parent', private='PRIVATE_RUN'))))
                    writer.commit()
                clock = [0]
                def sleep(_seconds):
                    clock[0] += 40
                with patch.object(scope_fixture.time, 'monotonic', side_effect=lambda: clock[0]), \
                     patch.object(scope_fixture.time, 'sleep', side_effect=sleep) as pause:
                    if not missing:
                        scope_fixture.wait_for_nested_finals(data, dict(parent='parent', child='child'))
                        pause.assert_not_called()
                        continue
                    with self.assertRaisesRegex(AssertionError, 'nested scope final counters were not committed: ') as raised:
                        scope_fixture.wait_for_nested_finals(data, dict(parent='parent', child='child'))
                message = str(raised.exception)
                self.assertNotIn('PRIVATE_', message)
                report = json.loads(message.split(': ', 1)[1])
                self.assertEqual(report['missing'], list(missing))
                self.assertEqual(report['elapsed_seconds'], 40)
                self.assertEqual(report['budget_seconds'], 40)
                self.assertEqual(report['polls'], 1)
                self.assertEqual(report['first_observed_seconds'], {key: 0 for key in conditions if key not in missing})
                for role in ('parent', 'child'):
                    state = report['runs'][role]
                    self.assertEqual(state['sample_count'], 1)
                    self.assertEqual(state['last_sample']['cpu_ns'], 123)
                    self.assertEqual(state['journal_present'], role+'_journal_removed' in missing)

    def test_nested_wait_observes_separate_final_commit_and_journal_cleanup(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            data = Path(name)
            journals = data/'runs-v5'
            journals.mkdir(mode=0o700)
            manifest = journals/'child.json'
            manifest.write_text('{}')
            with closing(sqlite3.connect(data/'telemetry.sqlite')) as writer:
                writer.executescript('''CREATE TABLE samples(object_id TEXT,source_time INTEGER,body TEXT);
                    CREATE TABLE objects(object_id TEXT,parent_object_id TEXT);
                    CREATE TABLE runs(agent_run_id TEXT,body TEXT);
                    INSERT INTO objects VALUES('child:scope','parent:scope');
                    INSERT INTO runs VALUES('child','{"parent_run_id":"parent"}');''')
                def commit_final(role):
                    writer.execute('INSERT INTO samples VALUES(?,?,?)',
                                   (role+':scope', 1, json.dumps(dict(final=True, populated=False))))
                    writer.commit()
                commit_final('parent')
                clock = [0]
                def progress(_seconds):
                    clock[0] += 5
                    if clock[0] == 5:
                        commit_final('child')
                    else:
                        manifest.unlink()
                with patch.object(scope_fixture.time, 'monotonic', side_effect=lambda: clock[0]), \
                     patch.object(scope_fixture.time, 'sleep', side_effect=progress) as pause:
                    scope_fixture.wait_for_nested_finals(data, dict(parent='parent', child='child'))
                self.assertEqual(pause.call_count, 2)

    def test_short_run_waits_for_both_objects_baseline_and_final_samples(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            data = prepare(Path(name))
            database = data/'telemetry.sqlite'
            database.unlink()
            with closing(sqlite3.connect(database)) as writer:
                database.chmod(0o600)
                writer.execute('CREATE TABLE samples(object_id TEXT,source_time INTEGER,body TEXT)')
                def sample(suffix, at, final):
                    writer.execute('INSERT INTO samples VALUES(?,?,?)',
                                   ('run'+suffix, at, json.dumps(dict(populated=False, final=final))))
                    writer.commit()
                sample(':scope', 0, False)
                sample(':scope', 1, True)
                pending = iter(((1, True), (0, False)))
                def commit_workload(_seconds):
                    sample(':workload', *next(pending))
                with patch.object(scope_fixture.time, 'monotonic', side_effect=(0, 0, 1, 2)), \
                     patch.object(scope_fixture.time, 'sleep', side_effect=commit_workload) as sleep:
                    scope_fixture.wait_for_short_run_samples(data, 'run')
                self.assertEqual(sleep.call_count, 2, 'scope final must not admit a missing workload endpoint')
                writer.execute("DELETE FROM samples WHERE object_id=? AND source_time=1", ("run:workload",))
                writer.commit()
                with patch.object(scope_fixture.time, 'monotonic', side_effect=(0, 0, 40)), \
                     patch.object(scope_fixture.time, 'sleep') as sleep:
                    with self.assertRaisesRegex(AssertionError, 'short-run baseline and final samples were not committed'):
                        scope_fixture.wait_for_short_run_samples(data, 'run')
                sleep.assert_called_once_with(0.2)

    def test_detached_final_waits_for_observer_to_retire_the_manifest(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            data = prepare(Path(name))
            database = data/'telemetry.sqlite'
            database.unlink()
            runs = data/'runs-v5'
            runs.mkdir(mode=0o700)
            manifest = runs/'run.json'
            with closing(sqlite3.connect(database)) as writer:
                database.chmod(0o600)
                writer.execute('CREATE TABLE samples(object_id TEXT,source_time INTEGER,body TEXT)')
                writer.execute('INSERT INTO samples VALUES(?,?,?)', ('run:scope', 1, json.dumps(
                    dict(populated=False, final=True, cpu_ns=1, memory_peak_bytes=8*1024*1024))))
                writer.commit()
            # A failed completion emit keeps the manifest for one more observer
            # tick after the final sample is already durable.
            manifest.write_text('{}')
            with patch.object(scope_fixture.time, 'monotonic', side_effect=(0, 0, 1)), \
                 patch.object(scope_fixture.time, 'sleep', side_effect=lambda _seconds: manifest.unlink()) as sleep:
                scope_fixture.wait_for_detached_final(data, 'run')
            sleep.assert_called_once_with(0.2)
            manifest.write_text('{}')
            with patch.object(scope_fixture.time, 'monotonic', side_effect=(0, 0, 40)), \
                 patch.object(scope_fixture.time, 'sleep'):
                with self.assertRaisesRegex(AssertionError, 'run manifest remained after final descendant counters'):
                    scope_fixture.wait_for_detached_final(data, 'run')

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
                self.assertFalse(any(kind == 'run' for kind, _, _ in events))
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
