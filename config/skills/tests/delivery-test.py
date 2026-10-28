#!/usr/bin/env python3
"""Host-free shared delivery contracts use synthetic capability packages only."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SOURCE = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(SOURCE))
import deliver
spec = importlib.util.spec_from_file_location('preview_migration', SOURCE/'migrate-preview.py')
migration = importlib.util.module_from_spec(spec)
spec.loader.exec_module(migration)


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


class PreviewMigration(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='subyard-preview-migration-', dir='/tmp')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.home = self.root/'home'
        self.path = self.home/'.codex/AGENTS.md'
        self.path.parent.mkdir(parents=True)
        self.inventory = self.root/'inventory.json'
        self.data = b'Unmarked instructions.\n\n' + migration.LEGACY + b'\n'
        self.path.write_bytes(self.data)
        self.path.chmod(0o644)
        os.chown(self.path,-1,os.getuid())
        self.record = dict(id='codex',kind='file',path=str(self.path),digest=migration.hashlib.sha256(self.data).hexdigest())
        self.save()

    def save(self):
        self.inventory.write_text(json.dumps(dict(schema=1,entries=[self.record])))
        self.inventory.chmod(0o600)

    def test_strip_and_repeat_preserve_text_and_retirement_without_host_source(self):
        migration.migrate(self.home,os.getuid(),self.inventory)
        self.assertEqual(self.path.read_bytes(),b'Unmarked instructions.\n\n\n')
        once = self.inventory.read_bytes()
        migration.migrate(self.home,os.getuid(),self.inventory)
        self.assertEqual(self.inventory.read_bytes(),once)
        # Run the actual owning inventory program through full retirement: a
        # missing host source must not delete the retained unmarked guest text.
        state = self.root/'inventory'
        state.mkdir(mode=0o700)
        (state/'inventory.json').write_bytes(once)
        (state/'inventory.json').chmod(0o600)
        program = (SOURCE.parents[1]/'internal/adapters/reconcileruntime/integration_inventory.py').read_text().replace('@STATE_ROOT@',str(state)).replace('@STATE_UID@',str(os.getuid()))
        request = json.dumps(dict(home=str(self.home),uid=os.getuid(),entries=[]))
        for mode in ('observe','apply','commit'):
            result = subprocess.run([sys.executable,'-B','-c',program,mode],input=request,text=True,capture_output=True)
            self.assertEqual(result.returncode,0,mode+": "+result.stderr)
        self.assertEqual(self.path.read_bytes(),b'Unmarked instructions.\n\n\n')
        self.assertIn(str(self.path),json.loads((state/'inventory.json').read_bytes())['released'])

    def test_unsafe_inventory_directory_refuses_without_writes(self):
        before = self.inventory.read_bytes()
        self.root.chmod(0o777)
        try:
            with self.assertRaisesRegex(ValueError,'unsafe integration inventory directory'):
                migration.migrate(self.home,os.getuid(),self.inventory)
            self.assertEqual(self.path.read_bytes(),self.data)
            self.assertEqual(self.inventory.read_bytes(),before)
        finally:
            self.root.chmod(0o700)

    def test_current_block_all_three_destinations(self):
        entries = []
        for relative in ('.claude/CLAUDE.md','.codex/AGENTS.md','.config/opencode/AGENTS.md'):
            path = self.home/relative
            path.parent.mkdir(parents=True,exist_ok=True)
            data = b'Before\n'+migration.CURRENT+b'\nAfter\n'
            path.write_bytes(data)
            path.chmod(0o644)
            os.chown(path,-1,os.getuid())
            entries.append(dict(id='synthetic',kind='file',path=str(path),digest=migration.hashlib.sha256(data).hexdigest()))
        self.inventory.write_text(json.dumps(dict(schema=1,entries=entries)))
        self.inventory.chmod(0o600)
        migration.migrate(self.home,os.getuid(),self.inventory)
        for entry in entries:
            self.assertEqual(Path(entry['path']).read_bytes(),b'Before\n\nAfter\n')

    def test_drift_ambiguous_unowned_and_unknown_blocks_are_preserved(self):
        for changed in (self.data+b'Guest edit\n', self.data+migration.LEGACY, self.data.replace(b'For a static',b'For my static')):
            self.path.write_bytes(changed)
            with self.assertRaises(ValueError):
                migration.migrate(self.home,os.getuid(),self.inventory)
            self.assertEqual(self.path.read_bytes(),changed)
        self.inventory.unlink()
        migration.migrate(self.home,os.getuid(),self.inventory)
        self.assertEqual(self.path.read_bytes(),changed)


if __name__ == '__main__':
    unittest.main()
