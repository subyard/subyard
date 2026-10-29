#!/usr/bin/env python3
"""Owning activation CAS, selection and credential-preservation regressions."""
import importlib.util
import contextlib
import ctypes
import io
import json
import os
import runpy
import shlex
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location('github_runtime', Path(__file__).parents[1]/'runtime.py')
runtime = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runtime)


class RuntimeTest(unittest.TestCase):
    def test_release_fixture_proves_unselected_native_absence_without_apply(self):
        profile = Path(__file__).parents[1]
        fixture = runpy.run_path(str(profile/'tests/release-observation.py'))
        commands = [(['incus', *argv], output, code) for argv, output, code in
                    [(entry[0], entry[1], entry[2] if len(entry) == 3 else 0)
                     for entry in fixture['read_only_commands'](profile, project='project')]]
        commands += [([program, *arguments], output, code) for program, arguments, output, code in
                     fixture['read_only_host_commands'](profile, yard='test')]
        responses = {tuple(argv): (output, code) for argv, output, code in commands}
        observed = []
        def exact_read(argv, **kwargs):
            if argv == [str(self.source/'owner.sh'), '--check']:
                return subprocess.CompletedProcess(argv, 0)
            self.assertIn(tuple(argv), responses)
            observed.append(argv)
            output, code = responses[tuple(argv)]
            return subprocess.CompletedProcess(argv, code, output.encode())
        os.environ['ENVIRONMENT_PROFILES'] = ''
        with patch.object(subprocess, 'run', exact_read):
            observation, _ = runtime.observe()
        self.assertEqual(observation['state'], 'current')
        self.assertEqual(observation['actual'], observation['desired'])
        self.assertTrue(observation['ownership_absent'])
        self.assertEqual(len(observed), 3)

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(dir='/tmp')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.source = self.root/'config/profiles/github'
        (self.source/'bin').mkdir(parents=True)
        for name in ('owner.sh', 'runtime.sh', 'runtime.py', 'provision.sh', 'SKILL.md', 'skills.json', 'bin/subyard-github', 'bin/git-remote-subyard-github', 'git_wiring.py', 'bin/broker'):
            (self.source/name).write_text(name)
        (self.source/'profile.json').write_text('{"default_yards":["default"],"disabled_when":{"NESTED_E2E_VMS":"1"}}')
        shared = self.root/'config/skills'
        shared.mkdir()
        (shared/'deliver.py').write_text('shared skill delivery fixture')
        self.ready = False
        self.check_status = 1
        self.check_reason = ''
        self.guest = 'installed-old'
        self.loaded = {'protocol': 1, 'broker_sha256': 'a'*64, 'client_sha256': 'b'*64}
        self.manager = b'ActiveState=active\nMainPID=22\nLoadState=loaded\n'
        self.probe_code = 0
        self.applies = 0
        self.remove_on_apply = False
        self.environment = {'SUBYARD_YARD': 'test', 'SUBYARD_HOME': str(self.root/'data'),
            'SUBYARD_OPERATOR_HOME': str(self.root/'home'), 'SUBYARD_CONFIG_HOME': str(self.root/'config'),
            'YARD_INSTANCE_NAME': 'yard', 'INCUS_PROJECT': 'project', 'ENVIRONMENT_PROFILES': 'github'}
        self.addCleanup(patch.stopall)
        patch.object(runtime, 'SOURCE', self.source).start()
        patch.dict(os.environ, self.environment, clear=True).start()
        patch.object(subprocess, 'run', self.fake_run).start()
        patch.object(runtime, 'apply_owner', self.fake_apply).start()

    def fake_apply(self, environment):
        self.fake_run([str(self.source/'owner.sh'), '--yes'], env=environment)

    def fake_run(self, argv, **kwargs):
        if argv[0] == str(self.source/'owner.sh'):
            if argv[1] == '--yes':
                self.applies += 1
                self.ready = True
                if self.remove_on_apply:
                    self.guest = ''
                    self.manager = b'ActiveState=inactive\nMainPID=0\nLoadState=not-found\n'
                    (self.root/'data/github-broker/test-engine').unlink()
            return subprocess.CompletedProcess(argv, 0 if self.ready or argv[1] == '--yes' else self.check_status,
                stderr=(f'subyard-profile-readiness-v1 {self.check_reason}\n'.encode() if self.check_reason else b''))
        if argv[0] == 'incus':
            if 'status' in argv:
                return subprocess.CompletedProcess(argv, 0, json.dumps(self.loaded).encode())
            return subprocess.CompletedProcess(argv, self.probe_code, self.guest.encode())
        return subprocess.CompletedProcess(argv, 0, self.manager)

    def test_unselected_absence_requires_all_guest_and_loaded_owner_artifacts_absent(self):
        os.environ['ENVIRONMENT_PROFILES'] = ''
        self.ready = True  # ordinary removal check: no owned skills/links/socket
        self.guest = ''
        self.manager = b'ActiveState=inactive\nMainPID=0\nLoadState=not-found\n'
        absent, _ = runtime.observe()
        self.assertEqual(absent['state'], 'current')
        self.assertEqual(absent['actual'], absent['desired'])
        self.assertTrue(absent['ownership_absent'])
        self.guest = 'remaining-copied-guest-engine'
        self.assertFalse(runtime.observe()[0].get('ownership_absent', False))
        self.guest = ''
        self.probe_code = 1
        self.assertEqual(runtime.observe()[0]['state'], 'stale')
        self.probe_code = 0
        self.manager = b'ActiveState=active\nMainPID=44\nLoadState=loaded\n'
        self.assertFalse(runtime.observe()[0].get('ownership_absent', False))
        self.manager = b'ActiveState=inactive\nMainPID=0\nLoadState=not-found\n'
        self.ready = False  # an owned skill/link/socket remains
        self.assertFalse(runtime.observe()[0].get('ownership_absent', False))
        self.ready = True
        state = self.root/'data/github-broker'
        state.mkdir(parents=True)
        (state/'test-engine').write_text('remaining owner executable')
        self.assertFalse(runtime.observe()[0].get('ownership_absent', False))
        (state/'test-engine').unlink()
        os.environ['ENVIRONMENT_PROFILES'] = 'github'
        self.assertFalse(runtime.observe()[0].get('ownership_absent', False))

    def test_deselected_cleanup_apply_retains_target_digest_and_proves_absence(self):
        os.environ['ENVIRONMENT_PROFILES'] = ''
        self.remove_on_apply = True
        state = self.root/'data/github-broker'
        state.mkdir(parents=True)
        (state/'test-engine').write_text('owned predecessor executable')
        before, _ = runtime.observe()
        self.assertEqual(before['state'], 'stale')
        arguments = ['runtime.py', 'apply', 'cleanup', before['actual'], before['desired']]
        with patch.object(sys, 'argv', arguments), contextlib.redirect_stdout(io.StringIO()) as output:
            runtime.main()
        after = json.loads(output.getvalue())
        self.assertEqual(after, {'state': 'current', 'actual': before['desired'],
                                 'desired': before['desired'], 'ownership_absent': True})
        with patch.object(sys, 'argv', arguments), contextlib.redirect_stdout(io.StringIO()):
            runtime.main()
        self.assertEqual(self.applies, 1)

    def test_delivery_change_invalidates_stale_assessment_before_apply(self):
        old, _ = runtime.observe()
        self.guest = 'delivered-new-but-not-restarted'
        changed, _ = runtime.observe()
        self.assertNotEqual(old['actual'], changed['actual'])
        with patch.object(sys, 'argv', ['runtime.py', 'apply', 'op', old['actual'], old['desired']]):
            with self.assertRaises(ValueError):
                runtime.main()
        self.assertEqual(self.applies, 0)

    def test_loaded_process_change_binds_assessment_with_same_installed_files(self):
        old, _ = runtime.observe()
        self.loaded['broker_sha256'] = 'c'*64
        changed, _ = runtime.observe()
        self.assertNotEqual(old['actual'], changed['actual'])

    def test_boot_registration_reason_binds_assessment_without_extending_wire(self):
        self.check_reason = 'boot-link-missing'
        old, _ = runtime.observe()
        self.check_reason = 'boot-dependency-unloaded'
        changed, _ = runtime.observe()
        self.assertNotEqual(old['actual'], changed['actual'])
        self.assertEqual(set(changed), {'state', 'actual', 'desired'})
        with patch.object(sys, 'argv', ['runtime.py', 'apply', 'op', old['actual'], old['desired']]):
            with self.assertRaises(ValueError):
                runtime.main()
        self.assertEqual(self.applies, 0)

    def test_unknown_check_diagnostics_do_not_enter_runtime_facts(self):
        old, _ = runtime.observe()
        self.check_reason = 'private-sentinel-do-not-copy'
        changed, _ = runtime.observe()
        self.assertEqual(old, changed)

    def test_retry_then_noop_preserves_credentials(self):
        protected = self.root/'private-connection'
        protected.write_text('private-fixture-data')
        old, _ = runtime.observe()
        with patch.object(sys, 'argv', ['runtime.py', 'apply', 'op', old['actual'], old['desired']]):
            runtime.main()
            runtime.main()
        self.assertEqual(self.applies, 1)
        self.assertEqual(protected.read_text(), 'private-fixture-data')

    def test_shared_skill_helper_is_a_desired_runtime_input(self):
        before, _ = runtime.observe()
        (self.source.parents[1]/'skills/deliver.py').write_text('updated delivery implementation')
        after, _ = runtime.observe()
        self.assertNotEqual(before['desired'],after['desired'])

    def test_selection_and_native_recipe_are_desired_inputs(self):
        old, _ = runtime.observe()
        os.environ['ENVIRONMENT_PROFILES'] = ''
        disabled, _ = runtime.observe()
        self.assertNotEqual(old['desired'], disabled['desired'])
        os.environ['ENVIRONMENT_PROFILES'] = 'github'
        (self.source/'bin/broker').write_text('new-native-runtime')
        changed, _ = runtime.observe()
        self.assertNotEqual(old['desired'], changed['desired'])

    def test_git_mode_binds_runtime_assessment(self):
        old, _ = runtime.observe()
        for mode in ('broker-first', 'broker-last', 'off'):
            os.environ['GITHUB_GIT_MODE'] = mode
            changed, _ = runtime.observe()
            self.assertNotEqual(old['desired'], changed['desired'])
            old = changed

    def test_implicit_default_and_disabled_nested_selection_match_descriptor(self):
        os.environ['SUBYARD_YARD'] = 'default'
        os.environ.pop('ENVIRONMENT_PROFILES')
        implicit, environment = runtime.observe()
        self.assertEqual(environment['SUBYARD_PROFILE_SELECTED'], '1')
        os.environ['ENVIRONMENT_PROFILES'] = ''
        empty, environment = runtime.observe()
        self.assertEqual(environment['SUBYARD_PROFILE_SELECTED'], '0')
        self.assertNotEqual(implicit['desired'], empty['desired'])
        os.environ['ENVIRONMENT_PROFILES'] = 'github'
        os.environ['NESTED_E2E_VMS'] = '1'
        _, environment = runtime.observe()
        self.assertEqual(environment['SUBYARD_PROFILE_SELECTED'], '0')

    def test_default_yard_contexts_observe_apply_and_retry(self):
        os.environ.pop('ENVIRONMENT_PROFILES')
        for value in (None, '', 'default'):
            with self.subTest(yard=value):
                if value is None:
                    os.environ.pop('SUBYARD_YARD', None)
                else:
                    os.environ['SUBYARD_YARD'] = value
                self.ready = False
                before, environment = runtime.observe()
                self.assertEqual(before['state'], 'stale')
                self.assertEqual(environment['SUBYARD_PROFILE_SELECTED'], '1')
                applies = self.applies
                arguments = ['runtime.py', 'apply', 'default-yard', before['actual'], before['desired']]
                with patch.object(sys, 'argv', arguments), contextlib.redirect_stdout(io.StringIO()) as output:
                    runtime.main()
                after = json.loads(output.getvalue())
                self.assertEqual(after, {'state': 'current', 'actual': before['desired'],
                                         'desired': before['desired']})
                with patch.object(sys, 'argv', arguments), contextlib.redirect_stdout(io.StringIO()):
                    runtime.main()
                self.assertEqual(self.applies, applies + 1)

    def test_invalid_yard_context_is_rejected(self):
        for value in ('../default', 'yard/name', 'bad yard'):
            with self.subTest(yard=value):
                os.environ['SUBYARD_YARD'] = value
                with self.assertRaisesRegex(ValueError, 'invalid GitHub runtime yard'):
                    runtime.observe()
        self.assertEqual(self.applies, 0)


class OwnerProcessTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='subyard-github-owner-', dir='/tmp')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.pids = self.root/'children'
        self.published = self.root/'published'
        self.owner = self.root/'owner.sh'
        self.owner.write_text('#!/bin/bash\nset -euo pipefail\n'
                              'python3 -c '+shlex.quote('import os,signal,time; signal.signal(signal.SIGTERM, signal.SIG_IGN); '
                              'open('+repr(str(self.pids))+', "w").write(str(os.getpid())); time.sleep(30)')+' &\n'
                              'wait\ntouch '+str(self.published)+'\n')
        self.owner.chmod(0o700)
        self.libc = ctypes.CDLL(None, use_errno=True)
        self.previous = ctypes.c_int()
        self.assertEqual(self.libc.prctl(37, ctypes.byref(self.previous), 0, 0, 0), 0)
        self.assertEqual(self.libc.prctl(36, 1, 0, 0, 0), 0)
        self.addCleanup(lambda: self.libc.prctl(36, self.previous.value, 0, 0, 0))
        self.addCleanup(self.reap_child)
        for patcher in (patch.object(runtime, 'SOURCE', self.root),
                        patch.object(runtime, 'OWNER_TIMEOUT', 0.3),
                        patch.object(runtime, 'OWNER_CANCEL_SECONDS', 0.1)):
            patcher.start()
            self.addCleanup(patcher.stop)

    def reap_child(self):
        if not self.pids.exists():
            return
        pid = int(self.pids.read_text())
        end = time.monotonic()+1
        while time.monotonic() < end:
            try:
                if os.waitpid(pid, os.WNOHANG)[0]:
                    return
            except ChildProcessError:
                return
            time.sleep(0.01)
        os.kill(pid, 9)
        os.waitpid(pid, 0)
        self.fail('GitHub owner cancellation left a live foreground child')

    def test_timeout_drains_owner_children_before_return(self):
        with self.assertRaises(subprocess.TimeoutExpired):
            runtime.apply_owner(os.environ.copy())
        self.assertTrue(self.pids.exists())
        self.assertFalse(self.published.exists())
        self.reap_child()

    def test_exited_owner_cannot_hide_a_surviving_child(self):
        self.owner.write_text(self.owner.read_text().replace('wait\ntouch',
            'for attempt in {1..100}; do [ ! -e '+str(self.pids)+' ] || break; sleep 0.005; done\nexit 0\ntouch'))
        runtime.apply_owner(os.environ.copy())
        self.assertTrue(self.pids.exists())
        self.reap_child()

    def test_signal_cancellation_drains_owner_children(self):
        done = threading.Event()
        def cancel():
            end = time.monotonic()+1
            while not done.wait(0.005) and time.monotonic() < end:
                if self.pids.exists():
                    os.kill(os.getpid(), 15)
                    return
        thread = threading.Thread(target=cancel)
        thread.start()
        try:
            with self.assertRaises(SystemExit) as caught:
                runtime.apply_owner(os.environ.copy())
            self.assertEqual(caught.exception.code, 143)
        finally:
            done.set()
            thread.join(timeout=1)
        self.assertFalse(self.published.exists())
        self.reap_child()


if __name__ == '__main__':
    unittest.main()
