#!/usr/bin/env python3
"""Owning activation CAS, selection and credential-preservation regressions."""
import importlib.util
import contextlib
import ctypes
import io
import json
import os
import shlex
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

NATIVE_RUN = subprocess.run

spec = importlib.util.spec_from_file_location('github_runtime', Path(__file__).parents[1]/'runtime.py')
runtime = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runtime)
identity_spec = importlib.util.spec_from_file_location('github_identity', Path(__file__).parents[1]/'identity.py')
identity = importlib.util.module_from_spec(identity_spec)
identity_spec.loader.exec_module(identity)


class IdentityRecoveryTest(unittest.TestCase):
    def test_guest_reboot_requiesces_only_exact_previously_active_broker(self):
        for state, substate, rebooted, foreign, accepted in (
                ('failed', 'failed', True, '', True),
                ('activating', 'auto-restart', True, '', True),
                ('failed', 'failed', False, '', False),
                ('activating', 'auto-restart', False, '', False),
                ('activating', 'start', True, '', False),
                ('failed', 'failed', True, 'unit', False),
                ('failed', 'failed', True, 'intent', False)):
            with self.subTest(state=state, substate=substate, rebooted=rebooted, foreign=foreign), tempfile.TemporaryDirectory(dir='/tmp') as temporary:
                root = Path(temporary)
                state_dir = root/'data/github-broker'; state_dir.mkdir(parents=True)
                path = state_dir/'sample.json'; engine = state_dir/'sample-engine'
                unit = root/'home/.config/systemd/user/subyard-github-sample.service'
                unit.parent.mkdir(parents=True)
                before = json.dumps(dict(app_config='retained-app', key_file='retained-key', ssh_port=2222,
                                         developer='old', identity_file='retained-id', known_hosts_file='retained-hosts')).encode()
                for artifact, content, mode in ((path,before,0o600),(engine,b'owned-engine',0o700),(unit,b'owned-unit',0o644)):
                    artifact.write_bytes(content); artifact.chmod(mode)
                expected = dict(ActiveState='active', UnitFileState='enabled', FragmentPath=str(unit))
                recipe = dict(original={'user':'old'},target={'user':'new'},broker=dict(installed=True,
                    before=identity.digest(before),after=identity.digest(identity.projected(before,'new')),
                    unit=identity.digest(unit.read_bytes()),engine=identity.digest(engine.read_bytes()),manager=expected))
                response = dict(recipe=recipe,phase='prepared',rebooted=rebooted)
                fields = dict(expected,ActiveState=state)
                if foreign == 'unit': unit.write_bytes(b'foreign-unit')
                if foreign == 'intent': fields['UnitFileState']='disabled'
                calls=[]
                def manager(name,*args):
                    if args[0]=='show':
                        if '--property=SubState' in args:return substate+'\n'
                        return ''.join(key+'='+value+'\n' for key,value in fields.items())
                    calls.append(args[0]); fields['ActiveState']='inactive' if args[0]=='stop' else 'active'
                    return ''
                environment=dict(SUBYARD_HOME=str(root/'data'),SUBYARD_OPERATOR_HOME=str(root/'home'),SUBYARD_YARD='sample',DEV_USER='old')
                with patch.dict(os.environ,environment), patch.object(identity,'guest',return_value=response), patch.object(identity,'manager',manager), contextlib.redirect_stdout(io.StringIO()):
                    if not accepted:
                        with self.assertRaises(ValueError): identity.run(['identity-apply','source'])
                        self.assertEqual(calls,[]);self.assertEqual(path.read_bytes(),before)
                        continue
                    identity.run(['identity-observe','new','source'])
                    self.assertEqual(calls,[])
                    identity.run(['identity-apply','source'])
                    self.assertEqual(calls,['stop'])
                    self.assertEqual(path.read_bytes(),before)
                    response['phase']='account-ready'
                    identity.run(['identity-apply','source'])
                    identity.run(['identity-apply','source'])
                    self.assertEqual(fields,expected)
                    self.assertEqual(json.loads(path.read_bytes()),dict(json.loads(before),developer='new'))
                    self.assertEqual(engine.read_bytes(),b'owned-engine')


class RuntimeTest(unittest.TestCase):
    def test_identity_observation_does_not_write_into_published_source(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            root=Path(temporary)
            profile=root/'config/profiles/github'
            profile.mkdir(parents=True)
            for name in ('runtime.sh','runtime.py','identity.py'):
                (profile/name).write_bytes((Path(__file__).parents[1]/name).read_bytes())
            before={str(path.relative_to(root)):path.read_bytes() for path in root.rglob('*') if path.is_file()}
            # The omitted authority module stops the actual identity import path
            # before any Incus command. Source immutability still applies on failure.
            result=NATIVE_RUN(['bash',str(profile/'runtime.sh'),'identity-observe','next','a'*64],
                                  env={'PATH':os.defpath,'DEV_USER':'dev'},capture_output=True)
            self.assertEqual(result.returncode,1)
            self.assertIn(b'GitHub runtime activation failed',result.stderr)
            after={str(path.relative_to(root)):path.read_bytes() for path in root.rglob('*') if path.is_file()}
            self.assertEqual(after,before)
            self.assertFalse(list(root.rglob('__pycache__')))


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
        self.commands = []
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
        self.commands.append(argv)
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

    def test_selected_ready_uses_only_the_complete_owner_check(self):
        self.ready = True
        observed, _ = runtime.observe()
        self.assertEqual(observed, {'state': 'current', 'actual': observed['desired'],
                                    'desired': observed['desired']})
        self.assertEqual(self.commands, [[str(self.source/'owner.sh'), '--check']])
        # A later observation must still run the authoritative live check.
        self.ready = False
        self.check_reason = 'transport-unavailable'
        self.assertEqual(runtime.observe()[0]['state'], 'stale')
        self.assertEqual(sum(command[0] == str(self.source/'owner.sh') for command in self.commands), 2)

    def test_stopped_selected_owner_does_not_bypass_guest_transport_check(self):
        self.ready = True
        os.environ['SUBYARD_PROFILE_STOPPED'] = '1'
        self.probe_code = 1
        observed, _ = runtime.observe()
        self.assertEqual(observed['state'], 'stale')
        self.assertTrue(any(command[0] == 'incus' for command in self.commands))

    def test_failed_or_malformed_native_diagnostics_cannot_make_stale_owner_ready(self):
        for status, output in ((1, b''), (0, b'{broken'), (0, b'[]'),
                               (0, b'{"protocol":1,"broker_sha256":"wrong"}')):
            with self.subTest(status=status, output=output):
                def native(argv, **kwargs):
                    if argv[0] == 'incus' and 'status' in argv:
                        return subprocess.CompletedProcess(argv, status, output)
                    return self.fake_run(argv, **kwargs)
                with patch.object(subprocess, 'run', native):
                    self.assertEqual(runtime.observe()[0]['state'], 'stale')

    def test_slow_probes_are_unavailable_rather_than_an_observation_failure(self):
        # A ready owner shows what each timeout decides: only the guest probe gates readiness.
        self.ready = True
        self.guest = ''
        os.environ['SUBYARD_PROFILE_STOPPED'] = '1'
        for selected in ('github', ''):
            for slow, match, state in (('guest', lambda argv: argv[0] == 'incus' and 'sh' in argv, 'stale'),
                                       ('manager', lambda argv: argv[0] == 'systemctl', 'current'),
                                       ('loaded', lambda argv: argv[0] == 'incus' and 'status' in argv, 'current')):
                with self.subTest(selected=selected, slow=slow):
                    def native(argv, **kwargs):
                        if match(argv):
                            raise subprocess.TimeoutExpired(argv, kwargs['timeout'])
                        return self.fake_run(argv, **kwargs)
                    os.environ['ENVIRONMENT_PROFILES'] = selected
                    with patch.object(subprocess, 'run', native):
                        observed, _ = runtime.observe()
                    self.assertEqual(observed['state'], state)
                    if slow != 'loaded':
                        self.assertFalse(observed.get('ownership_absent', False))

    def test_unselected_absence_requires_all_guest_and_loaded_owner_artifacts_absent(self):
        os.environ['ENVIRONMENT_PROFILES'] = ''
        self.ready = True  # ordinary removal check: no owned skills/links/socket
        self.guest = ''
        self.manager = b'ActiveState=inactive\nMainPID=0\nLoadState=not-found\n'
        absent, _ = runtime.observe()
        self.assertEqual(absent['state'], 'current')
        self.assertEqual(absent['actual'], absent['desired'])
        self.assertTrue(absent['ownership_absent'])
        self.assertEqual(len(self.commands), 3)
        self.assertFalse(any('status' in command for command in self.commands))
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
        with patch.object(sys, 'argv', ['runtime.py', 'apply', 'op', old['actual'], old['desired']]):
            with self.assertRaises(ValueError):
                runtime.main()
        self.assertEqual(self.applies, 0)

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
