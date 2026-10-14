#!/usr/bin/env python3
"""Owning migration boundaries over private fixtures and real Compose parsing."""
import json
import contextlib
import io
import importlib.util
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

PROFILE = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(PROFILE))
sys.path.insert(0, str(PROFILE.parents[2]/'internal/adapters/configmaterial'))
import native
import tomli_w
import trace_cutover as cutover


class CutoverTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='subyard-openlit-cutover-', dir='/tmp')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.home = self.root/'home'
        self.state = self.root/'state'
        self.legacy = self.root/'legacy'
        for path in (self.home, self.state, self.legacy):
            path.mkdir(mode=0o700)
        self.addCleanup(setattr, cutover, 'LEGACY', cutover.LEGACY)
        cutover.LEGACY = self.legacy
        self.config = self.home/'.codex/config.toml'
        native.write(self.state/'marker', 'subyard-openlit-v1\n')
        native.write(self.config, tomli_w.dumps({'model': 'fixture', 'otel': {'environment': 'dev', 'trace_exporter': cutover.TEMP}}))
        # Authentic predecessor native reconciliation owns logs and metrics only.
        trace = native.CODEX.pop('trace_exporter')
        try:
            self.native()
        finally:
            native.CODEX['trace_exporter'] = trace
        self.recipe = self.legacy/'compose.yaml'
        self.receiver = self.legacy/'collector.yaml'
        collector = {'receivers': {'otlp': {'protocols': {'http': {'endpoint': '0.0.0.0:4318'}}}},
                     'processors': {'batch': {}},
                     'exporters': {'clickhouse': {'endpoint': 'tcp://clickhouse:9000', 'database': 'openlit', 'username': 'openlit', 'password': 'synthetic-secret'}},
                     'service': {'pipelines': {'traces': {'receivers': ['otlp'], 'processors': ['batch'], 'exporters': ['clickhouse']}}}}
        native.write(self.receiver, json.dumps(collector))
        self.receiver.chmod(0o644)
        self.definition = {'name': 'fixture-trace-compat', 'services': {'receiver': {
            'image': cutover.IMAGE, 'command': ['--config=/etc/otelcol-contrib/config.yaml'],
            'volumes': [str(self.receiver)+':/etc/otelcol-contrib/config.yaml:ro'],
            'ports': ['127.0.0.1:4319:4318'], 'networks': ['backend'], 'restart': 'unless-stopped'}},
            'networks': {'backend': {'external': True, 'name': 'subyard-openlit_default'}}}
        native.write(self.recipe, json.dumps(self.definition))
        self.hash = cutover.run(['docker', 'compose', '--env-file', '/dev/null', '-f', str(self.recipe), 'config', '--hash', 'receiver']).decode().split()[1]
        self.row = {'Id': 'a'*64, 'Image': 'sha256:'+'b'*64,
            'Config': {'Image': cutover.IMAGE, 'Cmd': ['--config=/etc/otelcol-contrib/config.yaml'], 'Env': ['DO_NOT_PRINT=synthetic-secret'],
                       'Labels': {'com.docker.compose.project': 'fixture-trace-compat', 'com.docker.compose.service': 'receiver',
                                  'com.docker.compose.project.working_dir': str(self.legacy), 'com.docker.compose.project.config_files': str(self.recipe),
                                  'com.docker.compose.config-hash': self.hash}},
            'HostConfig': {'Privileged': False, 'CapAdd': None, 'Devices': [],
                           'PortBindings': {'4318/tcp': [{'HostIp': '127.0.0.1', 'HostPort': '4319'}]}},
            'NetworkSettings': {'Networks': {'subyard-openlit_default': {}}},
            'Mounts': [{'Type': 'bind', 'Source': str(self.receiver), 'Destination': '/etc/otelcol-contrib/config.yaml', 'RW': False}],
            'State': {'Status': 'running'}}
        self.calls = []
        self.directory_extra_ids = []
        self.project_extra_ids = []
        self.events = b'0\n'
        self.active = True
        self.proc = self.root/'proc'
        (self.proc/'sys/kernel/random').mkdir(parents=True)
        (self.proc/'uptime').write_text('10.00 5.00\n')
        (self.proc/'sys/kernel/random/boot_id').write_text('12345678-1234-1234-1234-123456789abc\n')
        self.processes = {'100': 500}

    def native(self, agents=('codex',), mode='apply'):
        native.reconcile(self.home, self.state/'native.json', agents, mode, os.getuid(), os.getgid(), tomli_w)

    def runner(self, args, timeout=5, data=None):
        self.calls.append(args)
        if args[:2] == ['docker', 'compose'] and 'exec' not in args:
            return cutover.run(args, timeout, data)
        if args[:2] == ['docker', 'ps']:
            # Real Docker truncates quiet ps IDs unless explicitly disabled.
            selector = args[args.index('--filter')+1]
            labels = self.row['Config']['Labels']
            if selector.startswith('label=com.docker.compose.project.working_dir='):
                matches = selector == 'label=com.docker.compose.project.working_dir='+labels['com.docker.compose.project.working_dir']
                extra = self.directory_extra_ids
            else:
                matches = selector == 'label=com.docker.compose.project='+labels['com.docker.compose.project']
                extra = self.project_extra_ids
            ids = ([self.row['Id']] if matches else [])+extra
            return '\n'.join(identifier if '--no-trunc' in args else identifier[:12] for identifier in ids).encode()
        if args[:3] == ['docker', 'image', 'inspect']:
            return json.dumps([{'Id': self.row['Image']}]).encode()
        if args[:2] == ['docker', 'inspect']:
            return json.dumps([self.row]).encode()
        if args[:2] == ['systemctl', 'is-active']:
            return b'active\n' if self.active else b'inactive\n'
        if args[:2] == ['docker', 'compose'] and 'exec' in args:
            self.assertIn("ServiceName = 'codex_cli_rs'", args[-1])
            self.assertIn("ResourceAttributes['subyard.openlit.trace_cutover']", args[-1])
            self.assertIn('Timestamp > fromUnixTimestamp64Nano(', args[-1])
            return self.events
        if args[:2] == ['docker', 'stop']:
            self.assertEqual(args[-1], 'a'*64)
            self.row['State']['Status'] = 'exited'
            return b''
        raise AssertionError('unexpected probe category')

    def prepare(self):
        cutover.prepare(self.home, self.state, ['codex'], tomli_w, self.runner)

    def adopt(self):
        self.prepare()
        self.native()
        cutover.fence(self.home, self.state, os.getuid(), lambda uid: self.processes.copy(), self.proc)

    def finish(self):
        return cutover.finish(self.home, self.state, os.getuid(), self.runner, lambda uid: self.processes.copy(), self.proc)

    def test_exact_recognition_leaf_backup_adoption_and_default_restore(self):
        proof = cutover.recognize(runner=self.runner)
        self.assertIsNotNone(proof)
        self.assertNotIn('synthetic-secret', json.dumps(proof))
        self.prepare()
        self.assertNotIn('trace_exporter', native.load(self.config, 'codex')['otel'])
        backup = json.loads((self.state/'trace-cutover-backup.json').read_text())
        self.assertEqual(backup, {'schema': 1, 'trace_exporter': cutover.TEMP})
        self.assertEqual((self.state/'trace-cutover-backup.json').stat().st_mode & 0o777, 0o600)
        self.native()
        self.native(mode='check')
        owned = json.loads((self.state/'native.json').read_text())['agents']['codex']['trace_exporter']
        self.assertFalse(owned['present'])
        self.native(())
        self.assertNotIn('trace_exporter', native.load(self.config, 'codex')['otel'])
        self.native()
        self.assertEqual(native.load(self.config, 'codex')['otel']['trace_exporter'], native.CODEX['trace_exporter'])
        self.assertEqual(native.load(self.config, 'codex')['model'], 'fixture')

    def nameless_explicit_project(self):
        self.definition.pop('name', None)
        native.write(self.recipe, json.dumps(self.definition))
        project = 'subyard-openlit-traces-workaround'
        self.row['Config']['Labels']['com.docker.compose.project'] = project
        self.row['Config']['Labels']['com.docker.compose.config-hash'] = cutover.run(
            ['docker', 'compose', '--project-name', project, '--env-file', '/dev/null',
             '-f', str(self.recipe), 'config', '--hash', 'receiver']).decode().split()[1]
        return project

    def test_nameless_recipe_uses_actual_explicit_project_and_fixed_point(self):
        project = self.nameless_explicit_project()
        proof = cutover.recognize(runner=self.runner)
        self.assertIsNotNone(proof)
        self.assertEqual(proof['container']['labels']['com.docker.compose.project'], project)
        self.assertEqual(cutover.recognize(runner=self.runner), proof)
        rendered = [args for args in self.calls if args[:2] == ['docker', 'compose'] and str(self.recipe) in args]
        self.assertTrue(rendered)
        self.assertTrue(all(args[args.index('--project-name')+1] == project for args in rendered))
        self.adopt()
        self.assertEqual(self.finish(), 'pending-sessions')

    def test_explicit_project_rejects_multiple_or_foreign_resources(self):
        self.nameless_explicit_project()
        self.directory_extra_ids = ['c'*64]
        self.assertIsNone(cutover.recognize(runner=self.runner))
        self.directory_extra_ids = []
        self.project_extra_ids = ['c'*64]
        self.assertIsNone(cutover.recognize(runner=self.runner))
        self.project_extra_ids = []
        self.row['Config']['Labels']['com.docker.compose.project.config_files'] = '/foreign/compose.yaml'
        self.assertIsNone(cutover.recognize(runner=self.runner))
        self.row['Config']['Labels']['com.docker.compose.project.config_files'] = str(self.recipe)
        self.row['Config']['Image'] = 'foreign/image:latest'
        self.assertIsNone(cutover.recognize(runner=self.runner))

    def test_explicit_project_rejects_malformed_main_or_inconsistent_project(self):
        project = self.nameless_explicit_project()
        for invalid in ('subyard-openlit', 'INVALID/name'):
            self.row['Config']['Labels']['com.docker.compose.project'] = invalid
            self.assertIsNone(cutover.recognize(runner=self.runner))
        self.row['Config']['Labels']['com.docker.compose.project'] = project
        def mismatch(args, timeout=5, data=None):
            result = self.runner(args, timeout, data)
            if args[:2] == ['docker', 'compose'] and str(self.recipe) in args and '--format' in args:
                value = json.loads(result)
                value['name'] = 'foreign-project'
                return json.dumps(value).encode()
            return result
        self.assertIsNone(cutover.recognize(runner=mismatch))

    def test_explicit_project_adoption_stays_bound_to_original_resource(self):
        self.nameless_explicit_project()
        self.adopt()
        self.processes = {}
        self.events = b'1\n'
        self.row['Id'] = 'c'*64
        replacement = 'replacement-trace-project'
        self.row['Config']['Labels']['com.docker.compose.project'] = replacement
        self.row['Config']['Labels']['com.docker.compose.config-hash'] = cutover.run(
            ['docker', 'compose', '--project-name', replacement, '--env-file', '/dev/null',
             '-f', str(self.recipe), 'config', '--hash', 'receiver']).decode().split()[1]
        self.assertIsNotNone(cutover.recognize(runner=self.runner))
        self.assertEqual(self.finish(), 'pending-ownership')
        self.assertFalse(any(args[:2] == ['docker', 'stop'] for args in self.calls))

    def changed_during_recognition(self, field, value):
        count = 0
        edited = None
        def changing_runner(args, timeout=5, data=None):
            nonlocal count, edited
            if args[:2] == ['docker', 'compose'] and str(self.recipe) in args and '--format' in args:
                count += 1
                if count == 2:
                    obj = native.load(self.config, 'codex')
                    if field == 'model':
                        obj[field] = value
                    else:
                        obj['otel'][field] = value
                    native.write(self.config, tomli_w.dumps(obj))
                    edited = self.config.read_bytes()
            return self.runner(args, timeout, data)
        with self.assertRaises(ValueError):
            cutover.prepare(self.home, self.state, ['codex'], tomli_w, changing_runner)
        self.assertEqual(count, 2)
        self.assertEqual(self.config.read_bytes(), edited)
        self.assertEqual(cutover.record(self.state)['phase'], 'prepared')

    def test_concurrent_model_change_during_recognition_is_preserved(self):
        self.changed_during_recognition('model', 'new-user-model')

    def test_concurrent_trace_change_during_recognition_is_preserved(self):
        self.changed_during_recognition('trace_exporter', {'otlp-http': {'endpoint': 'http://user.invalid/v1/traces'}})

    def test_concurrent_edit_after_temporary_write_is_preserved_before_replace(self):
        original_fsync = os.fsync
        edited = None
        def changing_fsync(fd):
            nonlocal edited
            target = Path(os.readlink('/proc/self/fd/'+str(fd)))
            if edited is None and target.parent == self.config.parent and target.name.startswith('.openlit-'):
                obj = native.load(self.config, 'codex')
                obj['model'] = 'last-moment-user-model'
                edited = tomli_w.dumps(obj).encode()
                self.config.write_bytes(edited)
            original_fsync(fd)
        with patch.object(os, 'fsync', changing_fsync):
            with self.assertRaises(ValueError):
                self.prepare()
        self.assertIsNotNone(edited)
        self.assertEqual(self.config.read_bytes(), edited)
        self.assertEqual(cutover.record(self.state)['phase'], 'prepared')

    def test_pending_old_and_new_sessions_then_retirement_is_idempotent(self):
        self.adopt()
        self.events = b'1\n'
        self.processes['101'] = 2000  # New backend does not release the old fence.
        self.assertEqual(self.finish(), 'pending-sessions')
        self.assertFalse(any(args[:2] == ['docker', 'stop'] for args in self.calls))
        del self.processes['100']
        self.assertEqual(self.finish(), 'retired')
        stops = sum(args[:2] == ['docker', 'stop'] for args in self.calls)
        self.assertEqual(self.finish(), 'idle')
        self.assertEqual(sum(args[:2] == ['docker', 'stop'] for args in self.calls), stops)

    def test_no_events_no_agent_and_stopped_service_remain_pending(self):
        self.adopt()
        self.processes = {}
        self.assertEqual(self.finish(), 'pending-traces')
        self.events = b'1\n'
        self.active = False
        self.assertEqual(self.finish(), 'pending-service')
        self.assertFalse(any(args[:2] == ['docker', 'stop'] for args in self.calls))

    def test_disable_reenable_during_pending_and_after_complete(self):
        self.adopt()
        token = cutover.record(self.state)['token']
        self.native(())
        self.active = False
        self.assertEqual(self.finish(), 'pending-service')
        self.prepare()
        self.native()
        self.active = True
        self.assertEqual(cutover.record(self.state)['token'], token)
        self.assertEqual(self.finish(), 'pending-sessions')
        self.processes = {}
        self.events = b'1\n'
        self.assertEqual(self.finish(), 'retired')
        self.native(())
        self.prepare()
        self.native()
        self.assertEqual(self.finish(), 'idle')
        self.assertNotEqual(native.load(self.config, 'codex')['otel']['trace_exporter'], cutover.TEMP)

    def test_interruption_after_removal_and_after_adoption_resume_conservatively(self):
        self.prepare()
        self.assertEqual(cutover.record(self.state)['phase'], 'prepared')
        self.prepare()  # Interrupted before native adoption.
        self.native()
        self.processes = {'200': 2000}
        (self.proc/'uptime').write_text('30.00 0.00\n')
        self.events = b'1\n'
        self.assertEqual(self.finish(), 'pending-sessions')
        self.assertGreaterEqual(cutover.record(self.state)['barrier'], 3000)
        self.processes = {}
        self.assertEqual(self.finish(), 'retired')

    def test_foreign_recipe_live_identity_and_user_override_are_preserved(self):
        self.row['Config']['Labels']['com.docker.compose.project.working_dir'] = '/foreign'
        before = self.config.read_bytes()
        self.prepare()
        self.assertEqual(self.config.read_bytes(), before)
        self.assertIsNone(cutover.record(self.state))
        self.native()
        self.native(mode='check')
        self.assertEqual(native.load(self.config, 'codex')['otel']['trace_exporter'], cutover.TEMP)
        self.row['Config']['Labels']['com.docker.compose.project.working_dir'] = str(self.legacy)
        self.adopt()
        obj = native.load(self.config, 'codex')
        obj['otel']['trace_exporter'] = {'otlp-http': {'endpoint': 'http://custom.invalid/v1/traces'}}
        native.write(self.config, tomli_w.dumps(obj))
        self.processes = {}
        self.events = b'1\n'
        self.assertEqual(self.finish(), 'pending-settings')
        self.native(())
        self.assertEqual(native.load(self.config, 'codex')['otel']['trace_exporter'], obj['otel']['trace_exporter'])

    def test_replaced_sidecar_or_recipe_cannot_be_stopped(self):
        self.adopt()
        self.processes = {}
        self.events = b'1\n'
        self.row['Id'] = 'c'*64
        self.assertEqual(self.finish(), 'pending-ownership')
        self.row['Id'] = 'a'*64
        self.receiver.write_text(self.receiver.read_text()+'\n')
        self.assertEqual(self.finish(), 'pending-ownership')
        self.assertFalse(any(args[:2] == ['docker', 'stop'] for args in self.calls))

    def test_unsafe_private_record_backup_and_recipe_fail_before_leaf_mutation(self):
        before = self.config.read_bytes()
        self.recipe.chmod(0o644)
        self.prepare()
        self.assertEqual(self.config.read_bytes(), before)
        self.recipe.chmod(0o600)
        (self.state/'trace-cutover-backup.json').symlink_to(self.config)
        with self.assertRaises(ValueError):
            self.prepare()
        self.assertEqual(self.config.read_bytes(), before)
        self.assertIsNone(cutover.record(self.state))

    def test_unsafe_environment_is_rejected_before_backup_and_leaf_removal(self):
        before = self.config.read_bytes()
        native.write(self.state/'trace-cutover.env', 'OPENLIT_TRACE_CUTOVER_ID=none\n')
        (self.state/'trace-cutover.env').chmod(0o644)
        with self.assertRaises(ValueError):
            self.prepare()
        self.assertEqual(self.config.read_bytes(), before)
        self.assertFalse((self.state/'trace-cutover-backup.json').exists())
        self.assertIsNone(cutover.record(self.state))

    def test_pid_reuse_and_reboot_do_not_hold_defunct_sessions(self):
        self.adopt()
        self.events = b'1\n'
        self.processes = {'100': 2000}
        self.assertEqual(self.finish(), 'retired')

    def test_interruption_before_leaf_removal_and_after_stop_keeps_retry_state(self):
        before = self.config.read_bytes()
        with patch.object(cutover, 'save', side_effect=OSError('fixture interruption')):
            with self.assertRaises(OSError):
                self.prepare()
        self.assertEqual(self.config.read_bytes(), before)
        self.assertTrue((self.state/'trace-cutover-backup.json').exists())
        self.adopt()
        self.processes = {}
        self.events = b'1\n'
        with patch.object(cutover, 'save', side_effect=OSError('fixture interruption')):
            with self.assertRaises(OSError):
                self.finish()
        self.assertEqual(self.row['State']['Status'], 'exited')
        self.assertEqual(cutover.record(self.state)['phase'], 'adopted')
        self.assertEqual(self.finish(), 'retired')

    def test_missing_agent_and_read_only_check_do_not_create_migration(self):
        before = self.config.read_bytes()
        self.native(mode='validate')
        self.native(mode='check')
        self.assertEqual(self.config.read_bytes(), before)
        self.assertFalse((self.state/'trace-cutover.json').exists())
        cutover.prepare(self.home, self.state, [], tomli_w, self.runner)
        self.assertEqual(self.config.read_bytes(), before)
        self.assertIsNone(cutover.record(self.state))

    def test_unavailable_process_fence_retries_without_retirement(self):
        self.prepare()
        self.native()
        outcome = cutover.fence(self.home, self.state, os.getuid(), lambda uid: (_ for _ in ()).throw(ValueError('fixture scan unavailable')), self.proc)
        self.assertEqual(outcome, 'pending-process-metadata')
        self.native(mode='check')
        self.assertEqual(cutover.record(self.state)['phase'], 'prepared')
        self.assertEqual(self.row['State']['Status'], 'running')
        self.events = b'1\n'
        self.assertEqual(self.finish(), 'pending-sessions')

    def unavailable_admission(self):
        def unavailable(*args, **kwargs):
            raise cutover.ProbeUnavailable('fixture unavailable native probe')
        cutover.prepare(self.home, self.state, ['codex'], tomli_w, unavailable)
        self.native()
        self.native(mode='check')
        self.assertIsNone(cutover.record(self.state))
        self.assertEqual(cutover.admission(self.state)['status'], 'unavailable')

    def second_probe_unavailable(self):
        count = 0
        def unavailable(args, timeout=5, data=None):
            nonlocal count
            if args[:2] == ['docker', 'compose'] and str(self.recipe) in args and '--format' in args:
                count += 1
                if count == 2:
                    raise cutover.ProbeUnavailable('fixture second proof unavailable')
            return self.runner(args, timeout, data)
        before = self.config.read_bytes()
        cutover.prepare(self.home, self.state, ['codex'], tomli_w, unavailable)
        self.assertEqual(count, 2)
        self.assertEqual(self.config.read_bytes(), before)
        self.assertEqual(cutover.record(self.state)['phase'], 'prepared')
        self.assertEqual(cutover.admission(self.state)['status'], 'unavailable')
        token = cutover.record(self.state)['token']
        self.assertEqual((self.state/'trace-cutover.env').read_text(), 'OPENLIT_TRACE_CUTOVER_ID='+token+'\n')
        self.assertEqual(self.row['State']['Status'], 'running')
        self.native()
        self.native(mode='check')

    def test_second_probe_failure_keeps_service_current_and_prepared_retry_recovers(self):
        self.second_probe_unavailable()
        token = cutover.record(self.state)['token']
        config = self.config.read_bytes()
        spec = importlib.util.spec_from_file_location('restarted_prepared_cutover', PROFILE/'trace_cutover.py')
        restarted = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(restarted)
        restarted.LEGACY = self.legacy
        self.assertEqual(restarted.finish(self.home, self.state, os.getuid(), self.runner), 'admission-ready')
        self.assertEqual(self.config.read_bytes(), config)
        self.assertTrue(cutover.admission_ready(self.home, self.state, True, ['codex']))
        self.prepare()  # Ordinary freshly assessed profile reconciliation.
        self.assertEqual(cutover.record(self.state)['token'], token)
        self.assertIsNone(cutover.admission(self.state))
        self.native()
        self.native(mode='check')
        cutover.fence(self.home, self.state, os.getuid(), lambda uid: self.processes.copy(), self.proc)
        self.assertEqual(cutover.record(self.state)['phase'], 'adopted')
        self.assertEqual(self.finish(), 'pending-sessions')

    def test_prepared_retry_keeps_unavailable_proof_and_token_stable(self):
        self.second_probe_unavailable()
        config = self.config.read_bytes()
        environment = (self.state/'trace-cutover.env').read_bytes()
        token = cutover.record(self.state)['token']
        def unavailable(*args, **kwargs):
            raise cutover.ProbeUnavailable('fixture prepared proof unavailable')
        cutover.prepare(self.home, self.state, ['codex'], tomli_w, unavailable)
        self.native()
        self.native(mode='check')
        self.assertEqual(self.config.read_bytes(), config)
        self.assertEqual((self.state/'trace-cutover.env').read_bytes(), environment)
        self.assertEqual(cutover.record(self.state)['token'], token)
        self.assertEqual(cutover.admission(self.state)['status'], 'unavailable')
        self.assertFalse(cutover.admission_ready(self.home, self.state, True, ['codex']))
        self.assertEqual(self.row['State']['Status'], 'running')

    def test_prepared_deferred_admission_refuses_changed_trace_override(self):
        self.second_probe_unavailable()
        obj = native.load(self.config, 'codex')
        obj['otel']['trace_exporter'] = 'console'
        native.write(self.config, tomli_w.dumps(obj))
        before = self.config.read_bytes()
        with self.assertRaises(ValueError):
            self.prepare()
        self.assertEqual(self.config.read_bytes(), before)
        self.assertEqual(self.finish(), 'ignored-admission')
        self.assertFalse(cutover.admission_ready(self.home, self.state, True, ['codex']))
        self.assertEqual(self.row['State']['Status'], 'running')

    def test_prepared_admission_never_expands_to_replaced_receiver(self):
        self.second_probe_unavailable()
        self.row['Id'] = 'c'*64
        before = self.config.read_bytes()
        self.assertEqual(self.finish(), 'admission-mismatch')
        self.assertFalse(cutover.admission_ready(self.home, self.state, True, ['codex']))
        with self.assertRaises(ValueError):
            self.prepare()
        self.assertEqual(self.config.read_bytes(), before)
        self.assertEqual(self.row['State']['Status'], 'running')

    def test_deferred_admission_survives_restart_and_normal_reconcile_adopts(self):
        self.unavailable_admission()
        config = self.config.read_bytes()
        ownership = (self.state/'native.json').read_bytes()
        environment = (self.state/'trace-cutover.env').read_bytes()
        spec = importlib.util.spec_from_file_location('restarted_cutover', PROFILE/'trace_cutover.py')
        restarted = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(restarted)
        restarted.LEGACY = self.legacy
        self.assertEqual(restarted.finish(self.home, self.state, os.getuid(), self.runner), 'admission-ready')
        self.assertEqual(self.config.read_bytes(), config)
        self.assertEqual((self.state/'native.json').read_bytes(), ownership)
        self.assertEqual((self.state/'trace-cutover.env').read_bytes(), environment)
        self.assertIsNone(restarted.record(self.state))
        self.assertTrue(restarted.admission_ready(self.home, self.state, True, ['codex']))
        self.assertFalse(any(args[:2] == ['docker', 'stop'] or 'up' in args for args in self.calls))
        restarted.prepare(self.home, self.state, ['codex'], tomli_w, self.runner)
        self.native()
        self.native(mode='check')
        self.assertIsNone(restarted.admission(self.state))
        self.assertFalse(restarted.admission_ready(self.home, self.state, True, ['codex']))
        self.assertEqual(native.load(self.config, 'codex')['otel']['trace_exporter'], native.CODEX['trace_exporter'])

    def test_unavailable_then_foreign_proof_keeps_ready_native_settings_untouched(self):
        self.unavailable_admission()
        config = self.config.read_bytes()
        self.row['Config']['Labels']['com.docker.compose.project.working_dir'] = '/foreign'
        self.assertEqual(self.finish(), 'admission-mismatch')
        self.assertEqual(self.config.read_bytes(), config)
        self.native(mode='check')
        self.assertFalse(cutover.admission_ready(self.home, self.state, True, ['codex']))
        self.assertIsNone(cutover.record(self.state))

    def test_deferred_admission_stopped_service_never_wakes_or_probes_docker(self):
        self.unavailable_admission()
        self.active = False
        self.calls.clear()
        self.assertEqual(self.finish(), 'pending-service')
        self.assertEqual(self.calls, [['systemctl', 'is-active', 'subyard-openlit.service']])
        self.assertEqual(cutover.admission(self.state)['status'], 'unavailable')

    def test_deferred_admission_deselection_disable_and_user_override_are_ignored(self):
        self.unavailable_admission()
        cutover.prepare(self.home, self.state, [], tomli_w, self.runner)
        self.assertIsNone(cutover.admission(self.state))
        self.unavailable_admission()
        cutover.cancel_admission(self.state)
        self.assertEqual(self.finish(), 'idle')
        self.unavailable_admission()
        obj = native.load(self.config, 'codex')
        obj['otel']['trace_exporter'] = 'console'
        native.write(self.config, tomli_w.dumps(obj))
        current = self.config.read_bytes()
        self.calls.clear()
        self.assertEqual(self.finish(), 'ignored-admission')
        self.assertEqual(self.config.read_bytes(), current)
        self.assertIsNone(cutover.admission(self.state))
        self.assertEqual(self.calls, [])

    def test_fence_cli_defers_unreadable_metadata_but_refuses_unsafe_identity(self):
        self.prepare()
        self.native()
        original_fence = cutover.fence
        def deferred(home, state, uid):
            return original_fence(home, state, uid, lambda owner: (_ for _ in ()).throw(PermissionError('fixture unreadable process')), self.proc)
        argv = ['trace_cutover.py', 'fence', '--home', str(self.home), '--state', str(self.state), '--uid', str(os.getuid())]
        with patch.object(sys, 'argv', argv), patch.object(cutover, 'fence', deferred):
            cutover.main()  # Deferred metadata exits successfully.
        self.native(mode='check')
        self.assertEqual(cutover.record(self.state)['phase'], 'prepared')
        self.assertEqual(self.row['State']['Status'], 'running')
        argv[-1] = str(os.getuid()+1)
        stderr = io.StringIO()
        with patch.object(sys, 'argv', argv), contextlib.redirect_stderr(stderr):
            with self.assertRaises(SystemExit) as failure:
                cutover.main()
        self.assertEqual(failure.exception.code, 1)
        self.assertEqual(stderr.getvalue(), 'openlit: protected trace migration state conflict\n')

    def test_boot_change_releases_only_prior_boot_process_fence(self):
        self.adopt()
        self.events = b'1\n'
        (self.proc/'sys/kernel/random/boot_id').write_text('12345678-1234-1234-1234-123456789abd\n')
        self.assertEqual(self.finish(), 'retired')

    def test_scan_uses_only_backend_identity_not_payloads(self):
        for pid, name, exe, ticks in [('100', 'codex', '/fixture/codex', 123), ('101', 'other', '/fixture/other', 456)]:
            path = self.proc/pid
            path.mkdir()
            (path/'comm').write_text(name+'\n')
            (path/'exe').symlink_to(exe)
            # Fields3..22, with startticks last; comm may include whitespace.
            (path/'stat').write_text(pid+' ('+name+') S '+'0 '*18+str(ticks)+'\n')
            (path/'cmdline').write_text('synthetic-private-payload')
            (path/'environ').write_text('synthetic-private-secret')
            (path/'cmdline').chmod(0)
            (path/'environ').chmod(0)
        self.assertEqual(cutover.scan(os.getuid(), self.proc), {'100': 123})


class LockTest(unittest.TestCase):
    def test_private_lock_symlink_hardlink_replacement_and_contention(self):
        with tempfile.TemporaryDirectory(prefix='subyard-openlit-lock-', dir='/tmp') as temporary:
            path = Path(temporary)/'lock'
            owner = (os.getuid(), os.getgid())
            fd = cutover.acquire(path, wait=0, owner=owner)
            try:
                with self.assertRaises(ValueError):
                    cutover.acquire(path, wait=0, owner=owner)
                alias = path.with_name('alias')
                os.link(path, alias)
                with self.assertRaises(ValueError):
                    cutover.lock_identity(fd, path, owner)
                alias.unlink()
                path.unlink()
                native.write(path, '')
                with self.assertRaises(ValueError):
                    cutover.lock_identity(fd, path, owner)
            finally:
                os.close(fd)
            path.unlink()
            path.symlink_to(Path(temporary)/'target')
            with self.assertRaises(OSError):
                cutover.acquire(path, wait=0, owner=owner)


if __name__ == '__main__':
    unittest.main()
