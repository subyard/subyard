#!/usr/bin/env python3
"""Exercise boot registration and repair through the actual owner hook."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


PROFILE = Path(__file__).resolve().parents[1]
ROOT = PROFILE.parents[2]


class OwnerRegistrationTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='github-registration-', dir='/tmp')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        package = self.root/'runtime/config/profiles/github'
        for name in ('owner.sh', 'provision.sh', 'git_wiring.py', 'SKILL.md', 'skills.json',
                     'bin/subyard-github', 'bin/git-remote-subyard-github'):
            (package/name).parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(PROFILE/name, package/name)
        self.owner = package/'owner.sh'
        engine = package/'bin/broker'
        engine.write_bytes(b'fixture native engine\n')
        engine.chmod(0o700)
        self.digest = hashlib.sha256(engine.read_bytes()).hexdigest()
        for name in ('lib/engine-context.sh', 'lib/ui.sh', 'lib/host.sh',
                     'lib/download.sh', 'lib-power.sh'):
            target = self.root/'runtime/scripts'/name
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(ROOT/'scripts'/name, target)
        shared = self.root/'runtime/config/skills'
        shared.mkdir()
        shutil.copy2(ROOT/'config/skills/deliver.py', shared/'deliver.py')
        self.home = self.root/'home'
        self.home.mkdir()
        self.unit = self.home/'.config/systemd/user/subyard-github-fixture.service'
        self.link = self.unit.parent/'default.target.wants'/self.unit.name
        self.control = self.root/'control.json'
        self.log = self.root/'calls.jsonl'
        self.save({'active': False, 'enabled': False, 'graph': False, 'fragment': str(self.unit)})
        commands = self.root/'commands'
        commands.mkdir()
        shim = commands/'shim'
        shim.write_text('''#!/usr/bin/env python3
import hashlib,json,os,pathlib,sys
args=sys.argv[1:]; name=pathlib.Path(sys.argv[0]).name
control=pathlib.Path(os.environ['CONTROL']); state=json.loads(control.read_text())
with open(os.environ['CALLS'],'a') as log: log.write(json.dumps([name,*args])+'\\n')
unit=pathlib.Path(os.environ['UNIT']); link=unit.parent/'default.target.wants'/unit.name
def save(): control.write_text(json.dumps(state))
if name=='loginctl': print('yes'); sys.exit(0)
if name=='incus':
    sys.stdin.buffer.read()
    if 'sha256sum' in args: print(os.environ['ENGINE_DIGEST']+'  client')
    if 'status' in args: print(json.dumps(dict(protocol=1,broker_sha256=os.environ['ENGINE_DIGEST'],client_sha256=os.environ['ENGINE_DIGEST'])))
    sys.exit(0)
assert args.pop(0)=='--user'
action=args.pop(0)
if action=='is-enabled': sys.exit(0 if state['enabled'] else 1)
if action=='is-active': sys.exit(0 if state['active'] else 3)
if action=='show':
    if 'default.target' in args: print(unit.name if state['graph'] else '')
    elif '--property=FragmentPath' in args: print(state['fragment'])
    else: raise AssertionError(args)
elif action=='enable':
    link.parent.mkdir(parents=True,exist_ok=True)
    if not link.is_symlink(): link.symlink_to(unit)
    state['enabled']=True; state['graph']=True; save()
elif action=='daemon-reload': state['graph']=link.is_symlink(); save()
elif action in ('start','restart'): state['active']=True; save()
elif action!='show-environment': raise AssertionError([action,*args])
''')
        shim.chmod(0o755)
        for name in ('incus', 'systemctl', 'loginctl'):
            (commands/name).symlink_to(shim)
        self.environment = dict(os.environ, PATH=str(commands)+':'+os.environ['PATH'],
            CONTROL=str(self.control), CALLS=str(self.log), UNIT=str(self.unit), ENGINE_DIGEST=self.digest,
            SUBYARD_ENGINE_CONTEXT='1', SUBYARD_ENGINE_CONTEXT_SCHEMA='1',
            SUBYARD_OPERATOR_HOME=str(self.home), SUBYARD_CONFIG_DIR=str(self.root/'runtime/config'),
            SUBYARD_CONFIG_HOME=str(self.root/'config'), SUBYARD_HOME=str(self.root/'data'),
            STORAGE_PATH=str(self.root/'storage'), HOST_BASE=str(self.root/'host'),
            RESTRICTED_DISK_PATHS=str(self.root/'host'), ACCESS_KIND='local', YARD_KIND='container',
            YARD_INSTANCE_NAME='fixture', INCUS_PROJECT='fixture', INCUS_BRIDGE='fixture',
            SSH_HOST='fixture', SSH_PORT='2222', DEV_USER='dev', DEV_UID='1000', DEV_SUDO='0',
            FORWARD_SSH_AGENT='0', NESTED_E2E_VMS='0', SUBYARD_YARD='fixture', SUBYARD_PROFILE_SELECTED='1')
        self.run_owner('--yes')
        self.log.write_text('')

    def save(self, state):
        self.control.write_text(json.dumps(state))

    def drift(self, **changes):
        state = json.loads(self.control.read_text())
        state.update(changes)
        self.save(state)

    def run_owner(self, argument, success=True):
        result = subprocess.run(['bash', str(self.owner), argument], env=self.environment,
                                capture_output=True, text=True, timeout=10)
        if success:
            self.assertEqual(result.returncode, 0, result.stderr)
        else:
            self.assertEqual(result.returncode, 1, result.stderr)
        return result

    def calls(self):
        return [json.loads(line) for line in self.log.read_text().splitlines()]

    def test_read_only_check_requires_exact_link_fragment_and_loaded_dependency(self):
        for kind in ('missing', 'dangling', 'foreign', 'fragment', 'graph'):
            with self.subTest(kind=kind):
                self.link.unlink(missing_ok=True)
                self.link.parent.mkdir(parents=True, exist_ok=True)
                self.link.symlink_to(self.unit)
                self.drift(graph=True, fragment=str(self.unit))
                if kind == 'missing':
                    self.link.unlink()
                elif kind in ('dangling', 'foreign'):
                    self.link.unlink()
                    target = self.root/kind
                    if kind == 'foreign':
                        target.write_text('foreign unit')
                    self.link.symlink_to(target)
                else:
                    self.drift(**({'fragment': '/foreign/unit'} if kind == 'fragment' else {'graph': False}))
                self.log.write_text('')
                result = self.run_owner('--check', success=False)
                reason = {'missing': 'boot-link-missing', 'dangling': 'boot-link-target-mismatch',
                          'foreign': 'boot-link-target-mismatch', 'fragment': 'unit-fragment-mismatch',
                          'graph': 'boot-dependency-unloaded'}[kind]
                self.assertEqual(result.stderr, f'subyard-profile-readiness-v1 {reason}\n')
                self.assertFalse(any(call[0] == 'systemctl' and call[2] in
                    ('start', 'restart', 'enable', 'daemon-reload') for call in self.calls()))
                self.environment['SUBYARD_PROFILE_STOPPED'] = '1'
                self.run_owner('--check', success=False)
                self.environment.pop('SUBYARD_PROFILE_STOPPED')

    def test_registration_only_repair_preserves_payloads_and_healthy_process(self):
        protected = [self.unit, self.root/'data/github-broker/fixture.json',
                     self.root/'data/github-broker/fixture-engine']
        before = [(p.read_bytes(), p.stat().st_ino) for p in protected]
        self.drift(graph=False)
        self.run_owner('--yes')
        self.assertTrue(json.loads(self.control.read_text())['graph'])
        self.assertEqual(before, [(p.read_bytes(), p.stat().st_ino) for p in protected])
        self.assertFalse(any(call[0] == 'systemctl' and call[2] == 'restart' for call in self.calls()))
        self.log.write_text('')
        self.run_owner('--yes')
        self.assertFalse(any(call[0] == 'systemctl' and call[2] in
            ('start', 'restart', 'enable', 'daemon-reload') for call in self.calls()))

    def test_foreign_registration_is_preserved_and_refused(self):
        self.link.unlink()
        foreign = self.root/'foreign.service'
        foreign.write_text('foreign service')
        self.link.symlink_to(foreign)
        self.run_owner('--yes', success=False)
        self.assertEqual(self.link.readlink(), foreign)
        self.assertEqual(foreign.read_text(), 'foreign service')

    def test_inactive_broker_recovers_without_payload_reinstallation(self):
        before = self.unit.stat().st_ino
        self.drift(active=False, graph=False)
        self.run_owner('--yes')
        self.assertEqual(self.unit.stat().st_ino, before)
        self.assertTrue(json.loads(self.control.read_text())['active'])
        self.run_owner('--check')

    def test_payload_drift_still_reinstalls_and_restarts(self):
        installed = self.root/'data/github-broker/fixture-engine'
        installed.write_text('obsolete engine')
        self.run_owner('--yes')
        self.assertEqual(hashlib.sha256(installed.read_bytes()).hexdigest(), self.digest)
        self.assertTrue(any(call[0] == 'systemctl' and call[2] == 'restart' for call in self.calls()))

    def test_unrepairable_loaded_fragment_refuses_before_start(self):
        self.drift(active=False, fragment='/foreign/unit')
        self.run_owner('--yes', success=False)
        self.assertFalse(any(call[0] == 'systemctl' and call[2] in
            ('start', 'restart') for call in self.calls()))


if __name__ == '__main__':
    unittest.main()
