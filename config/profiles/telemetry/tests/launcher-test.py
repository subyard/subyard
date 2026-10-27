#!/usr/bin/env python3
import concurrent.futures
import contextlib
import fcntl
import json
import os
import sqlite3
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import types
import unittest
from unittest.mock import patch
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import launcher


class LauncherTest(unittest.TestCase):
    def test_final_wait_preserves_original_failure_when_diagnostic_fails(self):
        owner = (Path(__file__).parent/'e2e/owner.sh').read_text()
        function = 'wait_for_finals() {\n'+owner.split('wait_for_finals() {\n',1)[1].split('\n}\n',1)[0]+'\n}\n'
        for code in (0, 124):
            script = ('set -eu\nROOT=/unused\n'
                      'timeout() { [ "$1" = 45 ]; return '+str(code)+'; }\n'
                      'finals_diagnostic() { printf "diagnostic-attempted\\n"; return 7; }\n'
                      +function+'wait_for_finals\n')
            result = subprocess.run(['bash','-c',script],capture_output=True,text=True,timeout=5)
            self.assertEqual(result.returncode,code,result.stderr)
            self.assertEqual(result.stdout.strip(), 'diagnostic-attempted' if code else '')

    def test_held_command_failure_is_distinguished_before_final_wait(self):
        owner = (Path(__file__).parent/'e2e/owner.sh').read_text()
        held = 'if [ "$mode" = --held-diagnostic ]; then\n'+owner.split('if [ "$mode" = --held-diagnostic ]; then\n',1)[1].split('\nfi',1)[0]+'\nfi'
        for managed, final in ((124, 0), (0, 124), (0, 0)):
            script = ('set -eu\nmode=--held-diagnostic; ROOT=/unused; state=/unused\n'
                      'SUBYARD_TELEMETRY_DATA_DIR=/unused\n'
                      'subyard-telemetry-run() { return '+str(managed)+'; }\n'
                      'wait_for_finals() { printf "wait\\n"; return '+str(final)+'; }\n'
                      'finals_diagnostic() { printf "diagnostic\\n"; return 7; }\n'
                      'sudo() { return 0; }; test() { return 0; }\n'+held)
            result = subprocess.run(['bash','-c',script],capture_output=True,text=True,timeout=5)
            self.assertEqual(result.returncode, managed or final, result.stderr)
            marker=json.loads(result.stderr.strip())
            self.assertEqual(marker['stage'],'managed-held')
            self.assertEqual(marker['exit_code'],managed)
            if managed:
                self.assertEqual(result.stdout.strip(),'diagnostic')
            else:
                self.assertIn('wait',result.stdout.splitlines())

    def test_final_diagnostic_reports_only_bounded_identified_metadata(self):
        owner = (Path(__file__).parent/'e2e/owner.sh').read_text()
        code = owner.split("<<'PY_FINALS_DIAGNOSTIC'",1)[1].split('\n',1)[1].split('\nPY_FINALS_DIAGNOSTIC',1)[0]
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            data = launcher.records.private_directory(root/'data')
            runs = launcher.records.private_directory(data/'runs')
            outbox = launcher.records.private_directory(data/'outbox')
            cgroups, proc = root/'cgroups', root/'proc'
            group = cgroups/'run-active'
            (group/'workload').mkdir(parents=True)
            (group/'cgroup.events').write_text('populated 1\n')
            (group/'cgroup.procs').write_text('')
            (group/'workload/cgroup.procs').write_text('42\n')
            def process(pid, comm):
                directory = proc/str(pid)
                directory.mkdir(parents=True)
                fields = ['0']*22
                fields[0], fields[19] = 'S', '123'
                (directory/'stat').write_text(str(pid)+' ('+comm+') '+' '.join(fields))
            process(42,'ssh')
            process(44,'PRIVATE_SECRET_PROCESS')
            info=group.stat()
            launcher.records.atomic_json(runs/'active.json',dict(agent_run_id='active',pid=43,start_ticks=123,
                cgroup=str(group),cgroup_identity=[info.st_dev,info.st_ino], private='PRIVATE_SECRET_MANIFEST'))
            launcher.records.atomic_json(runs/'reserved.json',dict(agent_run_id='reserved',manifest_state='reserved',launcher_pid=44,launcher_start_ticks=123))
            launcher.records.atomic_json(runs/'invalid.json',dict(private='PRIVATE_SECRET_INVALID'))
            (runs/'invalid.json').chmod(0o644)
            launcher.records.atomic_json(outbox/'event.json',dict(private='PRIVATE_SECRET_OUTBOX'))
            with contextlib.closing(sqlite3.connect(data/'telemetry.sqlite')) as db:
                db.execute('CREATE TABLE runs (agent_run_id TEXT, body TEXT)')
                db.execute('INSERT INTO runs VALUES (?,?)',('active',json.dumps(dict(state='main_exited',main_exited=True,agent_exited=True,resource_completed=False,private='PRIVATE_SECRET_DB'))))
                db.commit()
            namespace={'__name__':'fixture'}
            with patch.object(sys,'argv',['probe',str(Path(launcher.__file__).parent),str(data)]):
                exec(compile(code,'owner.sh:finals-diagnostic','exec'),namespace)
            document=namespace['diagnose'](data,cgroups,proc)
            encoded=json.dumps(document)
            self.assertNotIn('PRIVATE_SECRET',encoded)
            self.assertNotIn(str(root),encoded)
            self.assertLess(len(encoded),8192)
            self.assertEqual(document['outbox_count'],1)
            active=next(item for item in document['runs'] if item.get('scope')=='identified')
            self.assertTrue(active['populated'])
            self.assertFalse(active['main_identity_present'])
            self.assertTrue(active['main_exited'])
            self.assertFalse(active['resource_completed'])
            self.assertEqual(active['processes'],[dict(pid=42,start_ticks=123,state='S',comm_class='ssh')])
            reserved=next(item for item in document['runs'] if item['manifest']=='reserved')
            self.assertEqual(reserved['main_process']['comm_class'],'other')
            self.assertTrue(any(item['manifest']=='invalid' for item in document['runs']))
            self.assertTrue((runs/'active.json').exists())
            self.assertTrue((outbox/'event.json').exists())

    def test_manifest_admission_is_bounded_private_and_recovers(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            first = {'agent_run_id': 'first', 'boot_id': launcher.boot_id()}
            with patch.object(launcher, 'MAX_RUNS', 1):
                self.assertTrue(launcher.admit_manifest(root, first))
                self.assertFalse(launcher.admit_manifest(root, {**first, 'agent_run_id': 'second'}))
                marker = root/'runs/health'
                self.assertEqual(marker.stat().st_mode & 0o777, 0o600)
                self.assertLess(marker.stat().st_size, 1024)
                (root/'runs/first.json').unlink()
                self.assertTrue(launcher.admit_manifest(root, {**first, 'agent_run_id': 'second'}))
                self.assertFalse(marker.exists())
                events = [json.loads(path.read_text()) for path in (root/'outbox').glob('*.json')]
                self.assertTrue(any(event['kind']=='gap' and event['body']['reason']=='run_manifest_capacity' for event in events))
            payload = {'agent_run_id': 'third', 'metadata': 'x'*100}
            with patch.object(launcher, 'MAX_RUN_BYTES', 1):
                self.assertFalse(launcher.admit_manifest(root, payload))
            with patch.object(launcher, 'MAX_RUN_PAYLOAD', 10):
                self.assertFalse(launcher.admit_manifest(root, payload))
            self.assertEqual(len(list((root/'runs').glob('*.json'))), 1)

    def test_concurrent_manifest_admission_never_exceeds_count(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            with patch.object(launcher, 'MAX_RUNS', 1), concurrent.futures.ThreadPoolExecutor(2) as pool:
                results = list(pool.map(lambda run: launcher.admit_manifest(root, {'agent_run_id': run}), ('one', 'two')))
            self.assertEqual(sorted(results), [False, True])
            self.assertEqual(len(list((root/'runs').glob('*.json'))), 1)

    def test_manifest_admission_preserves_foreign_and_permission_guards(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            runs = launcher.records.private_directory(root/'runs')
            foreign = root/'foreign'
            foreign.write_text('do not overwrite')
            (runs/'foreign.json').symlink_to(foreign)
            self.assertFalse(launcher.admit_manifest(root, {'agent_run_id':'new'}))
            self.assertEqual(foreign.read_text(), 'do not overwrite')
            (runs/'foreign.json').unlink()
            launcher.records.atomic_json(runs/'foreign.json', {'agent_run_id':'foreign'})
            (runs/'foreign.json').chmod(0o644)
            self.assertFalse(launcher.admit_manifest(root, {'agent_run_id':'new'}))
            self.assertTrue((runs/'foreign.json').exists())

    def test_observer_recovers_preexisting_overflow_in_bounded_batches(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            runs = launcher.records.private_directory(root/'runs')
            for number in range(3):
                launcher.records.atomic_json(runs/(str(number)+'.json'), {'agent_run_id':str(number), 'boot_id':launcher.boot_id()})
            sampler = types.SimpleNamespace(poll=lambda: True)
            with patch.object(launcher, 'MAX_RUNS', 2), patch.object(launcher.processes, 'Sampler', return_value=sampler) as observed:
                self.assertEqual(launcher.observe(root, once=True), 0)
                self.assertEqual(observed.call_count, 2)
                self.assertEqual(len(list(runs.glob('*.json'))), 1)
                self.assertEqual(launcher.observe(root, once=True), 0)
                self.assertFalse(list(runs.glob('*.json')))

    def test_observer_byte_budget_retains_later_manifest_while_first_stays_active(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            directory = launcher.records.private_directory(root/'runs')
            for run in ('one', 'two'):
                launcher.records.atomic_json(directory/(run+'.json'), {'agent_run_id':run, 'boot_id':launcher.boot_id()})
            first, second = list(directory.glob('*.json'))
            sampled, ticks = [], []
            def sampler(_directory, run):
                def poll():
                    sampled.append(run['agent_run_id'])
                    return run['agent_run_id'] == second.stem
                return types.SimpleNamespace(poll=poll)
            class Finished(Exception):
                pass
            def tick(_interval):
                ticks.append(None)
                if not second.exists() or len(ticks) >= 3:
                    raise Finished
            with patch.object(launcher, 'MAX_RUN_BYTES', max(first.stat().st_size, second.stat().st_size)), patch.object(launcher.processes, 'Sampler', side_effect=sampler), patch.object(launcher.time, 'sleep', side_effect=tick), patch.dict(sys.modules, {'sources': types.SimpleNamespace(poll=lambda _directory: None)}):
                with self.assertRaises(Finished):
                    launcher.observe(root)
            self.assertTrue(first.exists(), 'active first run must remain tracked')
            self.assertFalse(second.exists(), 'later run must finalize despite the earlier active run')
            self.assertIn(first.stem, sampled)
            self.assertIn(second.stem, sampled)
            self.assertLessEqual(len(ticks), 2)

    def test_manifest_capacity_keeps_default_native_success_and_strict_refusal(self):
        for mode in ('auto', 'partial', 'require'):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory(dir='/tmp') as name:
                root = Path(name)
                target = root/'executed'
                args = types.SimpleNamespace(command=[sys.executable, '-c', 'from pathlib import Path; Path('+repr(str(target))+').touch(); raise SystemExit(19)'],
                    data_dir=str(root/'data'), agent='other', agent_version='fixture', scope_mode=mode,
                    task_id=None, project_id=None, worktree_id=None, yard_id=None, host_id=None)
                with patch.object(launcher, 'MAX_RUN_BYTES', 0), patch.object(launcher, 'create_scope') as create, patch.object(launcher, 'detached_observer') as observer:
                    self.assertEqual(launcher.launch(args), 2 if mode == 'require' else 19)
                create.assert_not_called()
                observer.assert_not_called()
                self.assertEqual(target.exists(), mode != 'require')
                self.assertFalse(list((root/'data/runs').glob('*.json')))
                self.assertTrue((root/'data/runs/health').exists())

    def test_reserved_capacity_is_charged_and_failed_placement_releases_it(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            run = {'agent_run_id':'one', 'boot_id':launcher.boot_id()}
            with patch.object(launcher, 'MAX_RUN_BYTES', launcher.MAX_RUN_PAYLOAD):
                self.assertTrue(launcher.admit_manifest(root, run, reserve=True))
                self.assertFalse(launcher.admit_manifest(root, {**run,'agent_run_id':'two'}, reserve=True))
                launcher.finish_manifest(root, run)
                self.assertTrue(launcher.admit_manifest(root, {**run,'agent_run_id':'two'}))
            args = types.SimpleNamespace(command=['/bin/true'], data_dir=str(root), agent='other', agent_version='fixture', scope_mode='require',
                task_id=None, project_id=None, worktree_id=None, yard_id=None, host_id=None)
            before = set((root/'runs').glob('*.json'))
            with patch.object(launcher, 'create_scope', side_effect=ValueError('safe caller delegation unavailable')):
                self.assertEqual(launcher.launch(args), 2)
            self.assertEqual(set((root/'runs').glob('*.json')), before)

    def test_observer_recovers_stale_reservation_only_after_durable_gap(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            self.assertTrue(launcher.admit_manifest(root, {'agent_run_id':'stale','boot_id':'old-boot'}, reserve=True))
            path = root/'runs/stale.json'
            with patch.object(launcher.records, 'emit', return_value=False):
                self.assertEqual(launcher.observe(root, once=True), 0)
            self.assertTrue(path.exists())
            self.assertEqual(launcher.observe(root, once=True), 0)
            self.assertFalse(path.exists())
            gap = next(json.loads(item.read_text()) for item in (root/'outbox').glob('*.json'))
            self.assertEqual(gap['body']['reason'], 'run_reservation_interrupted')

    def test_observer_overflow_foreign_prefix_does_not_starve_valid_run(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            directory = launcher.records.private_directory(root/'runs')
            # scandir's actual order supplies the retained unsafe prefix and then
            # the valid snapshot; no filesystem ordering assumption is needed.
            for number in range(3):
                launcher.records.atomic_json(directory/(str(number)+'.json'), {'agent_run_id':str(number),'boot_id':launcher.boot_id()})
            paths = list(directory.glob('*.json'))
            for path in paths[:2]:
                path.chmod(0o644)
            sampler = types.SimpleNamespace(poll=lambda: True)
            class Finished(Exception):
                pass
            def tick(_interval):
                if not paths[2].exists():
                    raise Finished
            with patch.object(launcher,'MAX_RUNS',2), patch.object(launcher.processes,'Sampler',return_value=sampler), patch.object(launcher.time,'sleep',side_effect=tick), patch.dict(sys.modules, {'sources': types.SimpleNamespace(poll=lambda _data: None)}):
                with self.assertRaises(Finished):
                    launcher.observe(root)
            self.assertFalse(paths[2].exists())
            self.assertTrue(all(path.exists() for path in paths[:2]))

    def test_snapshot_write_failure_executes_native_once_or_strictly_refuses(self):
        for mode in ('auto', 'require'):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory(dir='/tmp') as name:
                root = Path(name)
                workload = root/'scope/workload'
                workload.mkdir(parents=True)
                target = root/'executed'
                code = 'from pathlib import Path; p=Path('+repr(str(target))+'); p.write_text((p.read_text() if p.exists() else "")+"once"); raise SystemExit(19)'
                args = types.SimpleNamespace(command=[sys.executable,'-c',code], data_dir=str(root/'data'),
                    agent='other', agent_version='fixture', scope_mode=mode, task_id=None,
                    project_id=None, worktree_id=None, yard_id=None, host_id=None)
                original_write, original_remove = launcher.records.atomic_json, Path.rmdir
                def write(path, value):
                    if path.parent.name == 'runs' and path.suffix == '.json' and 'manifest_state' not in value:
                        raise OSError(28, 'simulated snapshot fsync failure')
                    return original_write(path, value)
                def remove(path):
                    # Kernel cgroup pseudo-files do not prevent rmdir; the host-
                    # free cgroup fixture uses one ordinary file instead.
                    if path == workload and (path/'cgroup.procs').exists():
                        (path/'cgroup.procs').unlink()
                    return original_remove(path)
                with patch.object(launcher, 'create_scope',return_value=workload), patch.object(launcher.processes,'Sampler',return_value=types.SimpleNamespace(capture_baseline=lambda:None)), patch.object(launcher.records,'atomic_json',side_effect=write), patch.object(Path,'rmdir',remove):
                    self.assertEqual(launcher.launch(args), 2 if mode == 'require' else 19)
                self.assertEqual(target.read_text() if target.exists() else '', '' if mode == 'require' else 'once')
                self.assertFalse(workload.parent.exists())
                self.assertFalse(list((root/'data/runs').glob('*.json')))

    def test_manifest_busy_lock_is_finite_and_preserves_owner_snapshot(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            self.assertTrue(launcher.admit_manifest(root, {'agent_run_id':'old'}))
            with (root/'runs/.lock').open('rb') as lock:
                fcntl.flock(lock, fcntl.LOCK_EX)
                started = time.monotonic()
                self.assertFalse(launcher.admit_manifest(root, {'agent_run_id':'new'}))
                self.assertLess(time.monotonic()-started, .5)
            self.assertEqual(len(list((root/'runs').glob('*.json'))), 1)

    def test_manager_identity_does_not_require_namespace_permission(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            proc, data, delegated = root/'proc', root/'data', root/'delegated'
            (proc/'sys/kernel/random').mkdir(parents=True)
            (proc/'sys/kernel/random/boot_id').write_text('boot')
            (proc/'77').mkdir()
            fields = ['0']*22
            fields[0], fields[19] = 'S', '123'
            (proc/'77/stat').write_text('77 (manager fixture) '+' '.join(fields))
            (delegated/'manager').mkdir(parents=True)
            (delegated/'manager/cgroup.procs').write_text('77')
            launcher.records.private_directory(data)
            info = delegated.stat()
            valid = {'boot_id':'boot','path':str(delegated),'identity':[info.st_dev,info.st_ino],'pid':77,'start_ticks':123}
            launcher.records.atomic_json(data/'delegation.json',valid)
            original = Path.mkdir
            def mkdir(path, *args, **kwargs):
                result = original(path, *args, **kwargs)
                if path.name == 'run-child':
                    (path/'cgroup.controllers').write_text('cpu memory io pids')
                    (path/'cgroup.subtree_control').write_text('')
                return result
            with patch.object(launcher.processes.os,'readlink',side_effect=PermissionError(13,'namespace read denied')) as namespace:
                with patch.object(launcher,'scope_parent',return_value=delegated), patch.object(Path,'mkdir',mkdir):
                    self.assertEqual(launcher.create_scope(data,{'agent_run_id':'child'},proc,root),delegated/'run-child/workload')
                namespace.assert_not_called()
                for key, value, reason in (('start_ticks',124,'manager unavailable'),('identity',[info.st_dev,info.st_ino+1],'delegation identity'),('boot_id','other','stale delegation')):
                    launcher.records.atomic_json(data/'delegation.json',{**valid,key:value})
                    with self.assertRaisesRegex(ValueError,reason):
                        launcher.create_scope(data,{'agent_run_id':'other'},proc,root)
                launcher.records.atomic_json(data/'delegation.json',valid)
                (delegated/'manager/cgroup.procs').write_text('88')
                with self.assertRaisesRegex(ValueError,'manager membership'):
                    launcher.create_scope(data,{'agent_run_id':'other'},proc,root)

    def test_scope_failure_diagnostic_never_echoes_error_payload(self):
        for error in (PermissionError(13, 'PRIVATE_ENV=secret', '/private/operator/config'),
                      ValueError('PRIVATE_PROMPT'), KeyError('PRIVATE_KEY')):
            value = launcher.scope_failure(error)
            self.assertNotIn('PRIVATE', value)
            self.assertNotIn('/private', value)
            self.assertIn('reason=unclassified', value)
            self.assertLess(len(value), 200)
        self.assertIn('reason=parent_scope_not_delegated', launcher.scope_failure(ValueError('parent scope not delegated')))

    def test_nested_scope_parent_stays_below_real_parent_group(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            cgroups, proc = root/'cgroups', root/'proc'
            delegated = cgroups/'fixture'
            parent = delegated/'run-parent'
            workload = parent/'workload'
            workload.mkdir(parents=True)
            (proc/'self').mkdir(parents=True)
            (proc/'self/cgroup').write_text('0::/fixture/run-parent/workload')
            for directory in (delegated, parent):
                for filename, value in {'cgroup.controllers':'cpu memory io pids',
                                        'cgroup.subtree_control':'cpu memory io pids', 'cgroup.procs':''}.items():
                    (directory/filename).write_text(value)
            (workload/'cgroup.procs').write_text('123')
            (delegated/'memory.max').write_text('4096')
            (delegated/'pids.max').write_text('32')
            launcher.records.private_directory(root/'runs')
            info = parent.stat()
            launcher.records.atomic_json(root/'runs/parent.json', {'cgroup':str(parent), 'boot_id':'boot',
                'cgroup_identity':[info.st_dev,info.st_ino]})
            run = {'agent_run_id':'child','parent_run_id':'parent','boot_id':'boot'}
            self.assertEqual(launcher.scope_parent(root,run,cgroups/'manager',proc,cgroups),parent)
            self.assertEqual(run['parent_scope_id'],'parent:scope')
            (workload/'pids.max').write_text('8')
            with self.assertRaisesRegex(ValueError, 'safe caller delegation'):
                launcher.scope_parent(root,run,cgroups/'manager',proc,cgroups)
            self.assertEqual((delegated/'memory.max').read_text(),'4096')
            self.assertEqual((delegated/'pids.max').read_text(),'32')

    def test_removed_scope_finishes_only_after_durable_gap_and_unknown_state(self):
        for failure in ('root_missing', 'root_replaced', 'group_missing', 'transient', 'gap_not_durable', 'run_not_durable'):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory(dir='/tmp') as name:
                root = Path(name)
                delegated = root/'delegated'
                group = delegated/'run'
                group.mkdir(parents=True)
                parent_info, group_info = delegated.stat(), group.stat()
                data = root/'data'
                launcher.records.private_directory(data/'runs')
                manifest = data/'runs/run.json'
                run = {'agent_run_id': 'run', 'boot_id': launcher.boot_id(), 'scope_root': str(delegated),
                       'scope_root_identity': [parent_info.st_dev, parent_info.st_ino], 'cgroup': str(group),
                       'cgroup_identity': [group_info.st_dev, group_info.st_ino]}
                if failure == 'root_replaced':
                    run['scope_root_identity'][1] += 1
                elif failure != 'transient':
                    group.rmdir()
                    if failure == 'root_missing':
                        delegated.rmdir()
                launcher.records.atomic_json(manifest, run)
                events = []
                def emit(directory, kind, body, **options):
                    events.append((kind, body))
                    return not ((failure == 'gap_not_durable' and kind == 'gap') or (failure == 'run_not_durable' and kind == 'run'))
                sampler = types.SimpleNamespace(poll=lambda: (_ for _ in ()).throw(PermissionError('transient read')))
                original = Path.lstat
                def lstat(path, *args, **kwargs):
                    if failure == 'transient' and path == delegated:
                        raise PermissionError('transient identity read')
                    return original(path, *args, **kwargs)
                with patch.object(launcher.records, 'emit', side_effect=emit), patch.object(launcher.processes, 'Sampler', return_value=sampler), patch.object(Path, 'lstat', lstat):
                    self.assertEqual(launcher.observe(data, once=True), 0)
                self.assertEqual(manifest.exists(), failure in ('transient', 'gap_not_durable', 'run_not_durable'))
                if failure == 'transient':
                    self.assertFalse(any(body.get('state') == 'resources_unknown' for _, body in events))
                else:
                    self.assertTrue(any(kind == 'gap' and body['reason'] == 'scope_removed_before_final' and body['missing'] == 'final_tail' for kind, body in events))
                    self.assertFalse(any(body.get('final') or body.get('resource_completed') or 'cpu_ns' in body for _, body in events))
                    if failure != 'gap_not_durable':
                        self.assertTrue(any(body.get('state') == 'resources_unknown' for _, body in events))
                if failure in ('gap_not_durable', 'run_not_durable'):
                    with patch.object(launcher.records, 'emit', return_value=True):
                        self.assertEqual(launcher.observe(data, once=True), 0)
                    self.assertFalse(manifest.exists())

    def test_caller_delegation_preserves_common_caps_and_nested_parent(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            cgroups, proc = root/'cgroups', root/'proc'
            caller = cgroups/'fixture/caller'
            caller.mkdir(parents=True)
            (proc/'self').mkdir(parents=True)
            (proc/'self/cgroup').write_text('0::/fixture/caller')
            delegated = caller.parent
            for filename, value in {'cgroup.controllers': 'cpu memory io pids', 'cgroup.subtree_control': '',
                                    'cgroup.procs': '', 'memory.max': '4096', 'pids.max': '32'}.items():
                (delegated/filename).write_text(value)
            (caller/'cgroup.procs').write_text('123')
            manager = cgroups/'manager'
            manager.mkdir()
            (cgroups/'cgroup.procs').write_text('')
            run = {'agent_run_id': 'run', 'boot_id': 'boot'}
            self.assertEqual(launcher.scope_parent(root, run, manager, proc, cgroups), delegated)
            self.assertEqual((delegated/'memory.max').read_text(), '4096')
            self.assertEqual((delegated/'pids.max').read_text(), '32')
            self.assertEqual(set((delegated/'cgroup.subtree_control').read_text().split()), {'+cpu', '+io', '+memory'})
            # A nested caller stays below its explicit parent's inclusive scope.
            (delegated/'cgroup.subtree_control').write_text('cpu memory io')
            (delegated/'workload').mkdir()
            (proc/'self/cgroup').write_text('0::/fixture/workload')
            launcher.records.private_directory(root/'runs')
            info = delegated.stat()
            launcher.records.atomic_json(root/'runs/parent.json', {'cgroup': str(delegated), 'boot_id': 'boot',
                'cgroup_identity': [info.st_dev, info.st_ino]})
            run['parent_run_id'] = 'parent'
            self.assertEqual(launcher.scope_parent(root, run, manager, proc, cgroups), delegated)
            self.assertEqual(run['parent_scope_id'], 'parent:scope')
            # A narrower caller leaf budget may never be escaped to a sibling.
            (delegated/'workload/pids.max').write_text('8')
            with self.assertRaisesRegex(ValueError, 'safe caller delegation'):
                launcher.scope_parent(root, run, manager, proc, cgroups)

    def test_auto_scope_refusal_preserves_native_success_and_reports_partial(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            args = types.SimpleNamespace(command=[sys.executable, '-c', 'raise SystemExit(19)'], data_dir=str(root/'data'),
                agent='other', agent_version='fixture', scope_mode='auto', task_id=None,
                project_id=None, worktree_id=None, yard_id=None, host_id=None)
            with patch.object(launcher, 'create_scope', side_effect=ValueError('safe refusal')), patch.object(launcher, 'detached_observer'):
                self.assertEqual(launcher.launch(args), 19)
            events = [json.loads(path.read_text()) for path in (root/'data/outbox').glob('*.json')]
            self.assertTrue(any(event['kind']=='run' and event['body'].get('scope_quality')=='partial' for event in events))

    def test_native_exec_waits_for_baseline_publication(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            workload = root/'scope/workload'
            workload.mkdir(parents=True)
            baseline = root/'baseline-durable'
            target = root/'executed'
            code = 'from pathlib import Path; assert Path('+repr(str(baseline))+').read_bytes()==b"durable"; Path('+repr(str(target))+').touch()'
            args = types.SimpleNamespace(command=[sys.executable, '-c', code], data_dir=str(root/'data'),
                agent='other', agent_version='fixture', scope_mode='require', task_id=None,
                project_id=None, worktree_id=None, yard_id=None, host_id=None)
            sampler = types.SimpleNamespace(capture_baseline=lambda: baseline.write_bytes(b'durable'))
            with patch.object(launcher, 'create_scope', return_value=workload), patch.object(launcher.processes, 'Sampler', return_value=sampler):
                self.assertEqual(launcher.launch(args), 0)
            self.assertTrue(target.exists())

    def test_required_scope_never_executes_after_placement_failure(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            workload = root/'scope/workload'
            workload.mkdir(parents=True)
            target = root/'executed'
            args = types.SimpleNamespace(command=[sys.executable, '-c', 'from pathlib import Path; Path('+repr(str(target))+').touch()'],
                data_dir=str(root/'data'), agent='other', agent_version='fixture', scope_mode='require',
                task_id=None, project_id=None, worktree_id=None, yard_id=None, host_id=None)
            original = Path.write_text
            def write(path, *values, **kwargs):
                if path == workload/'cgroup.procs':
                    raise PermissionError('synthetic placement refusal')
                return original(path, *values, **kwargs)
            with patch.object(launcher, 'create_scope', return_value=workload), patch.object(Path, 'write_text', write):
                self.assertEqual(launcher.launch(args), 2)
            self.assertFalse(target.exists())
            self.assertFalse(workload.exists())

    def test_partial_launch_keeps_native_exit_and_descendant_context(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            target = root/'context.json'
            command = [sys.executable, '-B', str(Path(launcher.__file__)), 'run', '--data-dir', str(root/'data'),
                       '--agent', 'other', '--scope-mode', 'partial', '--task-id', 'task', '--', sys.executable, '-c',
                       'import json,os,sys; json.dump({k:os.environ.get(k) for k in ("SUBYARD_AGENT_RUN_ID","SUBYARD_TASK_ID")},open(sys.argv[1],"w")); raise SystemExit(23)', str(target)]
            result = subprocess.run(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10)
            self.assertEqual(result.returncode, 23, result.stderr)
            value = json.loads(target.read_text())
            self.assertEqual(value['SUBYARD_TASK_ID'], 'task')
            self.assertTrue(value['SUBYARD_AGENT_RUN_ID'])
            events = [json.loads(path.read_text()) for path in (root/'data/outbox').glob('*.json')]
            self.assertTrue(any(event['kind'] == 'run' and event['body'].get('state') == 'starting' for event in events))
            deadline = time.monotonic()+12
            while list((root/'data/runs').glob('*.json')) and time.monotonic() < deadline:
                time.sleep(0.05)
            self.assertFalse(list((root/'data/runs').glob('*.json')))

    def test_native_binding_does_not_persist_argv_or_claim_shared_backend(self):
        arguments = launcher.native_arguments('codex', ['codex', 'exec', 'PRIVATE_PROMPT'], 'run')
        self.assertEqual(arguments[-2:], ['exec', 'PRIVATE_PROMPT'])
        self.assertEqual(arguments.count('-c'), 3)
        env = launcher.environment(Path('/tmp/data'), {'agent': 'claude', 'agent_run_id': 'run'}, {'OTEL_RESOURCE_ATTRIBUTES': 'team=local,subyard.agent_run.id=old',
            'OTEL_EXPORTER_OTLP_LOGS_HEADERS': 'Authorization=Bearer private-fixture,x-subyard-agent-run-id=old'})
        self.assertEqual(env['OTEL_RESOURCE_ATTRIBUTES'], 'team=local,subyard.agent_run.id=run')
        self.assertEqual(env['OTEL_EXPORTER_OTLP_HEADERS'], 'x-subyard-agent-run-id=run')
        self.assertEqual(env['OTEL_EXPORTER_OTLP_LOGS_HEADERS'], 'Authorization=Bearer private-fixture,x-subyard-agent-run-id=run')


if __name__ == '__main__':
    unittest.main()
