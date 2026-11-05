#!/usr/bin/env python3
"""Idempotent owned XDG wiring, drift, conflict and removal contracts."""
import importlib.util
import contextlib
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
import os
import sys
from types import SimpleNamespace
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('wiring', Path(__file__).parents[1]/'git_wiring.py')
wiring = importlib.util.module_from_spec(spec)
spec.loader.exec_module(wiring)
sys.modules['git_wiring']=wiring
sys.path.insert(0,str(Path(__file__).resolve().parents[3]/'identity'))
identity_spec=importlib.util.spec_from_file_location('identity_guest',Path(__file__).parents[1]/'identity_guest.py')
identity=importlib.util.module_from_spec(identity_spec)
identity_spec.loader.exec_module(identity)


class WiringTest(unittest.TestCase):
    def test_identity_reboot_fact_is_separate_from_approved_recipe(self):
        recipe={'source':'bound-source'}
        journal={'phase':'home','plan':{'boot':'original'}}
        with patch.object(identity.authority,'load',return_value=(journal,recipe)), patch.object(identity.authority,'rebooted',return_value=True) as rebooted, patch.object(identity,'check'), contextlib.redirect_stdout(io.StringIO()) as output:
            identity.main(['identity-observe','old','new','bound-source'])
        rebooted.assert_called_once_with(journal)
        self.assertEqual(json.loads(output.getvalue()),dict(phase='home',recipe=recipe,rebooted=True))
        self.assertNotIn('rebooted',recipe)

    def test_identity_rebind_preserves_unrelated_git_configuration_and_retries(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            root=Path(temporary)
            home,target=root/'dev',root/'next'
            config=home/'.config/git/config'
            fragment=home/'.config/subyard/github-git.conf'
            config.parent.mkdir(parents=True)
            fragment.parent.mkdir(parents=True)
            fragment.write_text(wiring.fragment('broker-first'));fragment.chmod(0o644)
            foreign='[credential]\n\thelper = retained\n[include]\n\tpath = /foreign/include\n'
            config.write_text(foreign+'\tpath = '+str(fragment)+'\n');config.chmod(0o644)
            before=config.read_bytes()
            after=identity.projection(before,str(fragment),str(target/'.config/subyard/github-git.conf'))
            original=dict(user='dev',uid=os.getuid(),gid=os.getgid(),home=str(home),group='dev')
            wanted=dict(original,user='next',home=str(target),group='next')
            recipe=dict(original=original,target=wanted,git=dict(fragment=identity.digest(fragment.read_bytes()),before=identity.digest(before),after=identity.digest(after),mode=0o644))
            home.rename(target)
            account=SimpleNamespace(pw_uid=os.getuid(),pw_gid=os.getgid(),pw_dir=str(target))
            with patch.object(identity.pwd,'getpwnam',return_value=account):
                for _ in range(2):identity.check(recipe,'account-ready',apply=True)
                result=(target/'.config/git/config').read_text()
                self.assertTrue(result.startswith(foreign))
                self.assertNotIn(str(home),result)
                self.assertIn(str(target/'.config/subyard/github-git.conf'),result)
                (target/'.config/git/config').write_text(result+'# concurrent user edit\n')
                with self.assertRaisesRegex(ValueError,'configuration changed'):
                    identity.check(recipe,'account-ready',apply=True)

    def test_install_repair_disable_preserves_user_config(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            home = Path(temporary)
            config = home/'.config/git/config'
            config.parent.mkdir(parents=True)
            foreign = '[credential]\n\thelper = foreign\n[include]\n\tpath = /foreign/path\n'
            config.write_text(foreign)
            (home/'.gitconfig').write_text('[user]\n\tname = Delivered user\n')
            self.assertFalse(wiring.reconcile(home, 'broker-first', 'check'))
            wiring.reconcile(home, 'broker-first', 'apply')
            first = config.read_bytes()
            wiring.reconcile(home, 'broker-first', 'apply')
            self.assertEqual(first, config.read_bytes())
            self.assertTrue(wiring.reconcile(home, 'broker-first', 'check'))
            owned = home/'.config/subyard/github-git.conf'
            owned.write_text(wiring.MARKER+'drift\n')
            self.assertFalse(wiring.reconcile(home, 'broker-first', 'check'))
            wiring.reconcile(home, 'broker-first', 'apply')
            self.assertEqual(owned.read_text(), wiring.fragment('broker-first'))
            owned.chmod(0o777)
            self.assertFalse(wiring.reconcile(home, 'broker-first', 'check'))
            wiring.reconcile(home, 'broker-first', 'apply')
            self.assertEqual(owned.stat().st_mode & 0o777, 0o644)
            for mode in ('broker-last', 'broker-first', 'broker-last'):
                self.assertFalse(wiring.reconcile(home, mode, 'check'))
                wiring.reconcile(home, mode, 'apply')
                self.assertTrue(wiring.reconcile(home, mode, 'check'))
                self.assertEqual(owned.read_text(), wiring.fragment(mode))
                owned.write_text(wiring.MARKER+'drift\n')
                wiring.reconcile(home, mode, 'apply')
                self.assertTrue(wiring.reconcile(home, mode, 'check'))
            wiring.reconcile(home, 'off', 'apply')
            self.assertTrue(wiring.reconcile(home, 'off', 'check'))
            self.assertEqual(config.read_text(), foreign)
            self.assertIn('Delivered user', (home/'.gitconfig').read_text())

    def test_foreign_fragment_and_conflicting_rewrite_are_refused(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            home = Path(temporary)
            (home/'.gitconfig').write_text('[url "ssh://elsewhere/"]\n\tinsteadOf = https://github.com/\n')
            with self.assertRaises(ValueError):
                wiring.reconcile(home, 'broker-first', 'apply')
            self.assertFalse((home/'.config/git/config').exists())
            (home/'.gitconfig').unlink()
            owned = home/'.config/subyard/github-git.conf'
            owned.parent.mkdir(parents=True)
            owned.write_text('foreign\n')
            self.assertTrue(wiring.reconcile(home, 'off', 'remove'))
            self.assertEqual(owned.read_text(), 'foreign\n')

    def test_default_off_preserves_symlinked_foreign_config(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            home = Path(temporary)
            foreign = home/'delivered-config'
            foreign.write_text('[credential]\n\thelper = foreign\n')
            config = home/'.config/git/config'
            config.parent.mkdir(parents=True)
            config.symlink_to(foreign)
            self.assertTrue(wiring.reconcile(home, 'off', 'check'))
            self.assertTrue(wiring.reconcile(home, 'off', 'apply'))
            with self.assertRaises(ValueError):
                wiring.reconcile(home, 'broker-first', 'apply')
            self.assertTrue(config.is_symlink())
            self.assertIn('foreign', foreign.read_text())


if __name__ == '__main__':
    unittest.main()
