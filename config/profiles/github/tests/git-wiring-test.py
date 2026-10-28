#!/usr/bin/env python3
"""Idempotent owned XDG wiring, drift, conflict and removal contracts."""
import importlib.util
from pathlib import Path
import subprocess
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('wiring', Path(__file__).parents[1]/'git_wiring.py')
wiring = importlib.util.module_from_spec(spec)
spec.loader.exec_module(wiring)


class WiringTest(unittest.TestCase):
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
