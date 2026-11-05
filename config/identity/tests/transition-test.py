#!/usr/bin/env python3
"""Native filesystem contracts; account tools are confined to fixture databases."""
import importlib.util
import json
import os
from pathlib import Path
import stat
import tempfile
import sys
import unittest
from contextlib import contextmanager
from unittest.mock import patch
from types import SimpleNamespace

sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
spec = importlib.util.spec_from_file_location('identity_transition', Path(__file__).resolve().parents[1]/'transition.py')
identity = importlib.util.module_from_spec(spec)
spec.loader.exec_module(identity)


class IdentityTransitionTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='subyard-identity-', dir='/tmp')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.uid = os.getuid() or 12345
        self.gid = os.getgid() or 12345
        for name in ('etc', 'proc/self', 'proc/sys/kernel/random', 'home/dev', 'var/lib/subyard/integrations'):
            (self.root/name).mkdir(mode=0o700, parents=True, exist_ok=True)
        os.chown(self.root/'home/dev', self.uid, self.gid)
        self.write('etc/passwd', f'dev:x:{self.uid}:{self.gid}:private:/home/dev:/bin/bash\n')
        self.write('etc/group', f'dev:x:{self.gid}:\n')
        self.write('etc/machine-id', 'a'*32+'\n')
        self.write('proc/self/mountinfo', '')
        self.write('proc/sys/kernel/random/boot_id','11111111-1111-1111-1111-111111111111',0o444)
        self.write('var/lib/subyard/integrations/inventory.json', json.dumps(dict(schema=1, entries=[], applied_settings=dict(DEV_USER='dev', DEV_UID=str(self.uid)))), 0o600)
        self.write('home/dev/data', 'retained user data', 0o600)
        self.before = (self.root/'home/dev/data').stat()
        @contextmanager
        def fixture_lock():yield
        self.transition = identity.Transition(self.root, owner=os.getuid(), run=self.account_tool, subid_lock=fixture_lock, listener=lambda: {"ssh.service":False,"ssh.socket":False})

    def write(self, name, content, mode=0o644):
        path = self.root/name
        path.write_text(content)
        path.chmod(mode)

    def account_tool(self, arguments):
        if arguments[0] == 'usermod':
            self.assertEqual(arguments, ['usermod', '--login', 'next', '--home', '/home/next', 'dev'])
            content = (self.root/'etc/passwd').read_text()
            self.write('etc/passwd', content.replace('dev:x:', 'next:x:').replace(':/home/dev:', ':/home/next:'))
        elif arguments[0] == 'groupmod':
            self.assertEqual(arguments, ['groupmod', '--new-name', 'next', 'dev'])
            self.write('etc/group', (self.root/'etc/group').read_text().replace('dev:x:', 'next:x:'))
        else:
            self.fail('unexpected account tool')

    def test_owner_reboot_allowance_requires_changed_native_boot(self):
        import authority
        journal={'plan':{'boot':self.transition.boot()}}
        self.assertFalse(authority.rebooted(journal,self.root))
        (self.root/'proc/sys/kernel/random/boot_id').unlink()
        self.write('proc/sys/kernel/random/boot_id','22222222-2222-2222-2222-222222222222',0o444)
        self.assertTrue(authority.rebooted(journal,self.root))
        self.assertTrue(authority.active_allowed('active','inactive','enabled',True))
        self.assertFalse(authority.active_allowed('active','inactive','disabled',True))
        self.assertFalse(authority.active_allowed('active','inactive','enabled',False))
        (self.root/'proc/sys/kernel/random/boot_id').unlink()
        self.write('proc/sys/kernel/random/boot_id','foreign',0o444)
        with self.assertRaises(ValueError):authority.rebooted(journal,self.root)
        (self.root/'proc/sys/kernel/random/boot_id').unlink()
        with self.assertRaises(FileNotFoundError):authority.rebooted(journal,self.root)

    def plan(self):
        return self.transition.prepare('dev', 'next', self.uid, 'b'*64, dict(name='sample', project='project', instance='yard'))

    def retained(self):
        path = self.root/'home/next/data'
        self.assertEqual(path.read_text(), 'retained user data')
        after = path.stat()
        self.assertEqual((self.before.st_dev, self.before.st_ino, self.before.st_uid, self.before.st_gid, stat.S_IMODE(self.before.st_mode)),
                         (after.st_dev, after.st_ino, after.st_uid, after.st_gid, stat.S_IMODE(after.st_mode)))
        self.assertFalse((self.root/'home/dev').exists())

    def test_rename_and_repeat_preserve_data_inode_and_numeric_identity(self):
        plan = self.plan()
        self.assertTrue((self.root/'home/dev/data').exists())
        self.assertFalse(self.transition.journal.exists())
        for _ in range(2):
            result = self.transition.apply_account(plan, identity.fingerprint(plan))
            self.assertEqual(result['phase'], 'account-ready')
            self.retained()

    def test_sudo_grant_is_retired_before_login_and_preserves_applied_intent(self):
        (self.root/'etc/sudoers.d').mkdir(mode=0o755)
        self.write('etc/sudoers.d/90-subyard-dev','dev ALL=(ALL) NOPASSWD:ALL\n',0o440)
        plan=self.plan()
        original_tool=self.transition.run
        def tool(arguments):
            self.assertFalse((self.root/'etc/sudoers.d/90-subyard-dev').exists())
            original_tool(arguments)
        self.transition.run=tool
        self.transition.apply_account(plan,identity.fingerprint(plan))
        self.assertEqual((self.root/'etc/sudoers.d/90-subyard-next').read_text(),'next ALL=(ALL) NOPASSWD:ALL\n')
        self.assertEqual(stat.S_IMODE((self.root/'etc/sudoers.d/90-subyard-next').stat().st_mode),0o440)
        self.transition.apply_account(plan,identity.fingerprint(plan))

    def test_partial_shadow_tool_write_recovers_only_captured_account_rows(self):
        self.write('etc/shadow','dev:locked-old-value:19800:0:99999:7:::\nforeign:!:19800:0:99999:7:::\n',0o640)
        self.write('etc/gshadow','dev:!::\nshared:!:dev,foreign:foreign,dev\n',0o640)
        self.write('etc/group',(self.root/'etc/group').read_text()+'shared:x:31337:dev,foreign\n')
        plan=self.plan()
        self.assertNotIn('locked-old-value',json.dumps(plan))
        original=self.transition.run
        def interrupted(arguments):
            original(arguments)
            if arguments[0]=='usermod':raise InterruptedError('cut after passwd publication')
        self.transition.run=interrupted
        with self.assertRaises(InterruptedError):self.transition.apply_account(plan,identity.fingerprint(plan))
        self.assertTrue((self.root/'etc/shadow').read_text().startswith('dev:'))
        self.transition.run=original
        self.transition.validate_pending(plan)
        self.transition.apply_account(plan,identity.fingerprint(plan))
        self.assertEqual((self.root/'etc/shadow').read_text(),'next:locked-old-value:19800:0:99999:7:::\nforeign:!:19800:0:99999:7:::\n')
        self.assertEqual((self.root/'etc/gshadow').read_text(),'next:!::\nshared:!:foreign,next:foreign,next\n')
        self.assertIn('shared:x:31337:foreign,next\n',(self.root/'etc/group').read_text())
        self.assertEqual(stat.S_IMODE((self.root/'etc/shadow').stat().st_mode),0o640)
        self.write('etc/shadow',(self.root/'etc/shadow').read_text().replace('locked-old-value','foreign-password'),0o640)
        with self.assertRaisesRegex(ValueError,'database changed'):
            self.transition.validate_pending(plan)

    def test_listener_is_fenced_before_account_tools_and_restores_intent(self):
        state={'ssh.service':True,'ssh.socket':True}
        self.transition.listener=lambda:dict(state)
        original_tool=self.transition.run
        def tool(arguments):
            if arguments[0]=='systemctl':
                state[arguments[2]]=arguments[1]=='start'
            else:
                self.assertFalse(any(state.values()))
                original_tool(arguments)
        self.transition.run=tool
        plan=self.plan()
        with self.assertRaisesRegex(ValueError,'listener must be fenced'):
            self.transition.apply_account(plan,identity.fingerprint(plan))
        self.transition.fence_listener(plan)
        self.transition.apply_account(plan,identity.fingerprint(plan))
        self.transition.restore_listener(plan)
        self.assertEqual(state,plan['listener'])

    def test_lingering_moves_to_the_new_login_only_from_an_idle_user_manager(self):
        linger=self.root/'var/lib/systemd/linger'
        linger.mkdir(parents=True)
        (linger/'dev').write_text('')
        work=self.root/('sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service/app.slice/agent.service'%(self.uid,self.uid))
        work.mkdir(parents=True)
        (work/'cgroup.procs').write_text('4242\n')
        (work.parent.parent/'init.scope').mkdir()
        (work.parent.parent/'init.scope'/'cgroup.procs').write_text('17\n')
        calls=[]
        original_tool=self.transition.run
        def tool(arguments):
            calls.append(arguments[:2])
            if arguments[0]=='loginctl':
                path=linger/arguments[2]
                path.unlink() if arguments[1]=='disable-linger' else path.write_text('')
            elif arguments[0]!='systemctl':
                original_tool(arguments)
        self.transition.run=tool
        with self.assertRaisesRegex(ValueError,'user services are running'):
            self.plan()
        (work/'cgroup.procs').write_text('')
        plan=self.plan()
        self.assertTrue(plan['linger'])
        (work/'cgroup.procs').write_text('4242\n')
        with self.assertRaisesRegex(ValueError,'user services'):
            self.transition.apply_account(plan,identity.fingerprint(plan))
        self.assertTrue((linger/'dev').exists())
        self.assertEqual(calls,[])
        (work/'cgroup.procs').write_text('')
        self.transition.apply_account(plan,identity.fingerprint(plan))
        self.assertEqual(calls[:2],[['loginctl','disable-linger'],['systemctl','stop']])
        self.assertFalse((linger/'dev').exists())
        self.transition.restore_linger(plan)
        self.transition.restore_linger(plan)
        self.assertTrue((linger/'next').exists())
        manager=work.parent.parent
        for path in sorted(manager.rglob('*'),reverse=True):
            path.rmdir() if path.is_dir() else path.unlink()
        manager.rmdir()
        with self.assertRaisesRegex(ValueError,'cgroup layout'):
            self.transition.user_services(self.uid)
        self.write('sys/fs/cgroup/cgroup.controllers','memory\n')
        self.assertFalse(self.transition.user_services(self.uid))
        self.assertEqual(sum(call==['loginctl','enable-linger'] for call in calls),1)

    def test_missing_ssh_socket_requires_an_exact_native_absence_observation(self):
        def show(arguments,**kwargs):
            if arguments[2]=='ssh.service':return SimpleNamespace(returncode=0,stdout='LoadState=loaded\nActiveState=active\nKillMode=process\n')
            return SimpleNamespace(returncode=4,stdout='LoadState=not-found\nActiveState=inactive\nKillMode=control-group\n')
        with patch.object(identity.subprocess,'run',side_effect=show):
            self.assertEqual(identity.Transition.listener_state(),{'ssh.service':True,'ssh.socket':False})
        with patch.object(identity.subprocess,'run',return_value=SimpleNamespace(returncode=1,stdout='')):
            with self.assertRaisesRegex(ValueError,'inspection failed'):identity.Transition.listener_state()

    def test_interruption_after_each_account_boundary_is_resumable(self):
        plan = self.plan()
        for boundary in ('home', 'account', 'account-ready'):
            def cut(phase):
                if phase == boundary:
                    raise InterruptedError('fixture interruption')
            self.transition.checkpoint = cut
            with self.assertRaises(InterruptedError):
                self.transition.apply_account(plan, identity.fingerprint(plan))
            self.retained()
        self.transition.checkpoint = lambda _: None
        self.transition.apply_account(plan, identity.fingerprint(plan))
        self.retained()

    def test_owned_receipts_move_then_commit_identity_and_resume_after_cut(self):
        import hashlib
        canonical=self.root/'usr/local/share/subyard/skills/sample'
        canonical.mkdir(mode=0o755,parents=True)
        content=b'owned static skill\n'
        (canonical/'SKILL.md').write_bytes(content)
        (canonical/'SKILL.md').chmod(0o644)
        state=self.root/'var/lib/subyard/agent-skills'
        state.mkdir(mode=0o700)
        self.write('var/lib/subyard/agent-skills/core.json',json.dumps(dict(schema=1,home='/home/dev',extra=False,skills={'sample':{'SKILL.md':hashlib.sha256(content).hexdigest()}})),0o600)
        for name in ('.agents/skills','.claude/skills','.config/opencode/skills'):
            directory=self.root/'home/dev'/name
            directory.mkdir(mode=0o700,parents=True)
            for parent in (directory,*directory.parents):
                if parent==self.root/'home':break
                os.chown(parent,self.uid,self.gid)
            link=directory/'sample'
            link.symlink_to(canonical)
            os.chown(link,self.uid,self.gid,follow_symlinks=False)
        path=self.root/'var/lib/subyard/integrations/inventory.json'
        original=json.loads(path.read_text())
        original['entries']=[dict(kind='file',id='sample',path='/home/dev/data',digest=hashlib.sha256(b'retained user data').hexdigest())]
        original['released']=['/home/dev/old-owned']
        self.write('var/lib/subyard/integrations/inventory.json',json.dumps(original),0o600)
        plan=self.plan()
        self.transition.apply_account(plan,identity.fingerprint(plan))
        cut='receipt:var/lib/subyard/integrations/inventory.json'
        self.transition.checkpoint=lambda phase: (_ for _ in ()).throw(InterruptedError('cut')) if phase==cut else None
        with self.assertRaises(InterruptedError):
            self.transition.rebind_receipts(plan)
        moved=json.loads(path.read_text())
        self.assertEqual(moved['entries'][0]['path'],'/home/next/data')
        self.assertEqual(moved['applied_settings']['DEV_USER'],'dev')
        self.transition.checkpoint=lambda _:None
        self.transition.rebind_receipts(plan)
        self.assertEqual(json.loads((state/'core.json').read_text())['home'],'/home/next')
        self.transition.rebind_receipts(plan,commit=True)
        self.transition.rebind_receipts(plan,commit=True)
        self.assertEqual(json.loads(path.read_text())['applied_settings']['DEV_USER'],'next')
        self.retained()
        for name in ('.agents/skills','.claude/skills','.config/opencode/skills'):
            self.assertEqual(os.readlink(self.root/'home/next'/name/'sample'),str(canonical))

    def test_assessment_reports_independent_destination_refusals(self):
        self.write('etc/passwd',(self.root/'etc/passwd').read_text()+f'next:x:{self.uid+1}:{self.gid+1}:private:/home/next:/bin/bash\n')
        self.write('etc/group',(self.root/'etc/group').read_text()+f'next:x:{self.gid+1}:\n')
        (self.root/'home/next').mkdir(mode=0o700)
        with self.assertRaises(ValueError) as result:
            self.plan()
        self.assertEqual(str(result.exception),'destination account already exists; destination group already exists; destination home already exists; account recovery owner row is not unique')
        self.assertFalse(self.transition.journal.exists())

    def test_foreign_destination_and_active_process_refuse_without_moving_data(self):
        (self.root/'etc/sudoers.d').mkdir(mode=0o755)
        self.write('etc/sudoers.d/90-subyard-dev','dev ALL=(ALL) NOPASSWD:ALL\n',0o440)
        plan = self.plan()
        (self.root/'home/next').mkdir(mode=0o700)
        with self.assertRaises(ValueError):
            self.transition.apply_account(plan, identity.fingerprint(plan))
        (self.root/'home/next').rmdir()
        (self.root/'proc/42').mkdir(mode=0o700)
        self.write('proc/42/status', f'Name: workload\nUid:\t{self.uid}\t{self.uid}\t{self.uid}\t{self.uid}\n')
        before=(self.root/'etc/passwd').read_bytes()
        with patch.object(identity.time,'monotonic',side_effect=[0,15]), patch.object(identity.time,'sleep') as sleep:
            with self.assertRaisesRegex(ValueError,'developer processes remain'):
                self.transition.apply_account(plan, identity.fingerprint(plan))
            sleep.assert_not_called()
        self.assertEqual((self.root/'etc/passwd').read_bytes(),before)
        self.assertEqual((self.root/'etc/sudoers.d/90-subyard-dev').read_text(),'dev ALL=(ALL) NOPASSWD:ALL\n')
        self.assertFalse((self.root/'etc/sudoers.d/90-subyard-next').exists())
        self.assertTrue((self.root/'home/dev/data').exists())
        self.assertFalse((self.root/'home/next').exists())

    def test_native_user_manager_drains_before_account_and_home_mutation(self):
        (self.root/'proc/42').mkdir(mode=0o700)
        self.write('proc/42/status',f'Name: systemd\nUid:\t{self.uid}\t{self.uid}\t{self.uid}\t{self.uid}\n')
        plan=self.plan()
        clock=[0]
        original=self.transition.run
        def tool(arguments):
            self.assertFalse((self.root/'proc/42').exists())
            self.assertGreaterEqual(clock[0],10)
            original(arguments)
        def sleep(seconds):
            self.assertTrue((self.root/'home/dev/data').exists())
            self.assertFalse((self.root/'home/next').exists())
            clock[0]+=seconds
            if clock[0]>=10:
                (self.root/'proc/42/status').unlink()
                (self.root/'proc/42').rmdir()
        self.transition.run=tool
        with patch.object(identity.time,'monotonic',side_effect=lambda:clock[0]), patch.object(identity.time,'sleep',side_effect=sleep):
            result=self.transition.apply_account(plan,identity.fingerprint(plan))
        self.assertEqual(result['phase'],'account-ready')
        self.assertLess(clock[0],15)
        self.retained()

    def test_process_inspection_failure_is_not_treated_as_drain(self):
        with patch.object(self.transition,'no_processes',side_effect=PermissionError('fixture read failure')), patch.object(identity.time,'sleep') as sleep:
            with self.assertRaises(PermissionError):
                self.transition.wait_no_processes(self.uid)
            sleep.assert_not_called()

    def test_borrowed_mount_and_foreign_group_refuse_during_assessment(self):
        self.write('proc/self/mountinfo', '1 2 0:1 / /home/dev/project rw - ext4 /dev/fixture rw\n')
        with self.assertRaises(ValueError):
            self.plan()
        self.write('proc/self/mountinfo', '')
        self.write('etc/group', f'dev:x:{self.gid}:foreign\n')
        with self.assertRaises(ValueError):
            self.plan()
        self.assertFalse(self.transition.journal.exists())


if __name__ == '__main__':
    unittest.main()
