#!/usr/bin/env python3
"""Host-free shared delivery contracts use synthetic capability packages only."""
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import unittest

SOURCE = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(SOURCE))
import deliver


class Delivery(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='subyard-skills-', dir='/tmp')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.home = self.root/'home'
        self.home.mkdir()
        self.source = self.root/'package'
        self.skill = self.source/'synthetic-capability'
        self.skill.mkdir(parents=True)
        (self.skill/'SKILL.md').write_text('---\nname: synthetic-capability\ndescription: Use the synthetic capability.\n---\nContent.\n')
        self.args = type('Args', (), dict(root=self.root, home=self.home, uid=os.getuid(), gid=os.getgid(), provider='synthetic', mode='apply', source=self.source, registered=False, extra=False))()
        self.installed = self.root/'usr/local/share/subyard/skills/synthetic-capability'

    def run_mode(self, mode):
        self.args.mode = mode
        return deliver.delivery(self.args)

    def test_fresh_rerun_stale_repair_remove_and_late_client(self):
        self.assertEqual(self.run_mode('check'), 10)
        self.run_mode('apply')
        self.assertEqual(self.run_mode('check'), 0)
        for adapter in deliver.ADAPTERS:
            self.assertEqual((self.home/adapter/'synthetic-capability').resolve(), self.installed)
        self.assertFalse((self.home/'.codex/skills/synthetic-capability').exists())
        # Discovery paths are ready before any client is installed/enabled.
        self.run_mode('apply')
        (self.installed/'SKILL.md').write_text('stale')
        self.assertEqual(self.run_mode('check'), 10)
        self.run_mode('apply')
        self.assertEqual(self.run_mode('check'), 0)
        user_skill = self.home/'.agents/skills/user-owned'
        user_skill.mkdir()
        (user_skill/'SKILL.md').write_text('keep')
        self.run_mode('remove')
        self.assertEqual(self.run_mode('check-removed'), 0)
        self.assertEqual((user_skill/'SKILL.md').read_text(), 'keep')

    def test_apply_recovers_after_kill_before_skill_rename(self):
        target = self.installed/'SKILL.md'
        state = self.root/'var/lib/subyard/agent-skills'
        reserved = (self.installed.parent/'.subyard-skill-staging', state/'.subyard-skill-staging')
        for path in reserved:
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text('operator staging file')
            path.chmod(0o644)
        self.installed.parent.chmod(0o755)
        state.chmod(0o700)
        inodes = [path.stat().st_ino for path in reserved]
        command = ['apply', '--provider', self.args.provider, '--root', str(self.root),
                   '--home', str(self.home), '--uid', str(self.args.uid), '--gid', str(self.args.gid),
                   '--source', str(self.source)]
        code = """
import os, runpy, signal, sys
script, target, *args = sys.argv[1:]
def interrupt(event, values):
    if event == 'os.rename' and values[1] == target:
        os.kill(os.getpid(), signal.SIGKILL)
sys.addaudithook(interrupt)
sys.argv = [script, *args]
runpy.run_path(script, run_name='__main__')
"""
        script = str(SOURCE/'deliver.py')
        result = subprocess.run([sys.executable, '-B', '-c', code, script, str(target), *command], timeout=5)
        self.assertEqual(result.returncode, -signal.SIGKILL)
        unfinished, = self.installed.parent.glob('.subyard-skill-staging-*')
        self.assertEqual(unfinished.read_bytes(), (self.skill/'SKILL.md').read_bytes())
        self.assertFalse(target.exists())
        self.assertEqual(self.run_mode('check'), 10)
        subprocess.run([sys.executable, '-B', script, *command], check=True, timeout=5)
        self.assertEqual(target.read_bytes(), (self.skill/'SKILL.md').read_bytes())
        self.assertFalse(unfinished.exists())
        self.assertEqual(list(self.installed.parent.glob('.subyard-skill-staging-*')), [])
        for path, inode in zip(reserved, inodes):
            self.assertEqual(path.stat().st_ino, inode)
            self.assertEqual(path.read_text(), 'operator staging file')
        self.assertEqual(self.run_mode('check'), 0)

    def test_old_in_place_staging_leftover_recovers_and_unknown_file_refuses(self):
        self.run_mode('apply')
        leftover = self.installed/'.skill-a1_z9xq0'
        leftover.write_text('interrupted write')
        (self.installed/'SKILL.md').write_text('stale')
        self.assertEqual(self.run_mode('check'), 10)
        self.run_mode('apply')
        self.assertFalse(leftover.exists())
        self.assertEqual((self.installed/'SKILL.md').read_bytes(), (self.skill/'SKILL.md').read_bytes())
        self.assertEqual(self.run_mode('check'), 0)
        for name in ('.skill-a1_z9xq', '.skill-A1_Z9XQ0', '.skill-a1_z9xq0x', 'notes'):
            unknown = self.installed/name
            unknown.write_text('operator content')
            for mode in ('check', 'apply', 'remove'):
                with self.assertRaisesRegex(ValueError, 'unmanaged skill content'):
                    self.run_mode(mode)
                self.assertEqual(unknown.read_text(), 'operator content')
            unknown.unlink()
        link = self.installed/'.skill-b2_y8wp1'
        link.symlink_to(self.installed/'SKILL.md')
        with self.assertRaisesRegex(ValueError, 'unmanaged skill content'):
            self.run_mode('apply')
        self.assertTrue(link.is_symlink())

    def test_shipped_file_named_like_old_staging_settles(self):
        (self.skill/'.skill-shipped0').write_text('shipped content')
        self.run_mode('apply')
        shipped = self.installed/'.skill-shipped0'
        inode = shipped.stat().st_ino
        self.assertEqual(self.run_mode('check'), 0)
        self.run_mode('apply')
        self.assertEqual(shipped.stat().st_ino, inode)
        self.assertEqual(shipped.read_text(), 'shipped content')
        self.assertEqual(self.run_mode('check'), 0)

    def test_core_native_extra_links_and_repair(self):
        self.args.provider = 'core'
        self.args.extra = True
        self.run_mode('apply')
        for adapter in deliver.ADAPTERS + deliver.EXTRA:
            self.assertEqual((self.home/adapter/'synthetic-capability').resolve(), self.installed)
        (self.home/'.hermes/skills/synthetic-capability').unlink()
        self.assertEqual(self.run_mode('check'), 10)
        self.run_mode('apply')
        self.assertEqual(self.run_mode('check'), 0)

    def test_permissions_and_broken_links(self):
        self.run_mode('apply')
        path = self.installed/'SKILL.md'
        self.assertEqual(path.stat().st_mode & 0o777, 0o644)
        self.assertEqual(self.installed.stat().st_mode & 0o777, 0o755)
        path.chmod(0o600)
        self.assertEqual(self.run_mode('check'), 10)
        self.run_mode('apply')
        self.assertEqual(path.stat().st_mode & 0o777, 0o644)
        receipt = self.root/'var/lib/subyard/agent-skills/synthetic.json'
        self.assertEqual(receipt.stat().st_mode & 0o777, 0o600)
        link = self.home/'.claude/skills/synthetic-capability'
        link.unlink()
        self.assertEqual(self.run_mode('check'), 10)
        self.run_mode('apply')
        link.unlink()
        link.symlink_to('/missing/foreign-skill')
        with self.assertRaisesRegex(ValueError, 'unmanaged skill link'):
            self.run_mode('apply')
        self.assertEqual(os.readlink(link), '/missing/foreign-skill')

    def test_preflight_collision_preserves_everything(self):
        collision = self.home/'.claude/skills/synthetic-capability'
        collision.mkdir(parents=True)
        (collision/'SKILL.md').write_text('user data')
        with self.assertRaises(ValueError):
            self.run_mode('apply')
        self.assertFalse(self.installed.exists())
        self.assertEqual((collision/'SKILL.md').read_text(), 'user data')

    def test_unregistered_provider_never_advertises_selected_capability(self):
        self.args.registered = True
        self.run_mode('apply')
        self.assertFalse(self.installed.exists())

    def test_missing_source_and_symlinked_parent(self):
        self.args.source = self.root/'missing'
        with self.assertRaises(ValueError):
            self.run_mode('apply')
        self.args.source = self.source
        (self.home/'.claude').symlink_to(self.root/'outside')
        with self.assertRaises(ValueError):
            self.run_mode('apply')
        self.assertFalse(self.installed.exists())

    def test_registration_probe_separates_success_from_static_drift(self):
        self.assertEqual(self.run_mode('check-registered'),10)
        self.run_mode('apply')
        (self.installed/'SKILL.md').write_text('stale')
        (self.home/'.agents/skills/synthetic-capability').unlink()
        self.args.source = None
        self.assertEqual(self.run_mode('check-registered'),0)
        receipt = self.root/'var/lib/subyard/agent-skills/synthetic.json'
        receipt.chmod(0o644)
        with self.assertRaisesRegex(ValueError,'unsafe skill receipt'):
            self.run_mode('check-registered')

    def test_unsafe_system_directories_refuse_without_clobber(self):
        self.run_mode('apply')
        content = (self.installed/'SKILL.md').read_bytes()
        for directory in (self.root/'var/lib/subyard/agent-skills',self.installed.parent,self.installed):
            original = directory.stat().st_mode & 0o777
            directory.chmod(0o777)
            for mode in ('check','apply','remove'):
                with self.assertRaisesRegex(ValueError,'unsafe'):
                    self.run_mode(mode)
                self.assertEqual((self.installed/'SKILL.md').read_bytes(),content)
            directory.chmod(original)
        self.installed.chmod(0o700)
        self.assertEqual(self.run_mode('check'),10)
        self.run_mode('apply')
        self.assertEqual(self.installed.stat().st_mode & 0o777,0o755)

    def test_provider_name_conflict_and_extra_file(self):
        self.run_mode('apply')
        self.args.provider = 'another'
        with self.assertRaisesRegex(ValueError, 'another provider'):
            self.run_mode('apply')
        self.args.provider = 'synthetic'
        (self.installed/'foreign').write_text('keep')
        with self.assertRaisesRegex(ValueError, 'unmanaged skill content'):
            self.run_mode('remove')
        self.assertEqual((self.installed/'foreign').read_text(), 'keep')

    def test_new_package_files_and_removed_files_converge(self):
        self.run_mode('apply')
        (self.skill/'reference.md').write_text('new supporting reference')
        self.run_mode('apply')
        self.assertEqual(self.run_mode('check'), 0)
        (self.skill/'reference.md').unlink()
        self.run_mode('apply')
        self.assertFalse((self.installed/'reference.md').exists())


if __name__ == '__main__':
    unittest.main()
