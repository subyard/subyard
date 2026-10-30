#!/usr/bin/env python3
"""Guest snapshot state contract; physical owner/mode checks remain explicit."""
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
import io
import contextlib
from unittest.mock import patch

SOURCE = Path(__file__).resolve().parents[1]/'core/subyard-environment/facts.py'
spec = importlib.util.spec_from_file_location('facts', SOURCE)
facts = importlib.util.module_from_spec(spec)
spec.loader.exec_module(facts)


class Facts(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir='/tmp', prefix='subyard-facts-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.path = self.root/'guest-runtime.json'
        self.machine = self.root/'machine-id'
        self.machine.write_text('a'*32+'\n')
        self.target = dict(version='0.22.0-test', yard=dict(name='default', project='project', instance='yard'), selected_profiles=[])
        # The host-free worker is unprivileged. Keep real file modes/fsync/replace;
        # substitute only ownership and the root-only ancestor admission.
        self.addCleanup(patch.stopall)
        patch.object(facts, 'safe').start()
        patch.object(facts.os, 'fchown').start()
        self.real_lstat = Path.lstat
        def root_stat(path):
            st = self.real_lstat(path)
            fields = list(st)
            fields[4] = fields[5] = 0
            return os.stat_result(fields)
        patch.object(Path, 'lstat', root_stat).start()

    def read(self):
        return facts.read(self.path, self.machine)

    def apply(self, mode, target=None):
        facts.apply(mode, target or self.target, self.path, self.machine)

    def test_empty_success_noop_and_profile_selection_snapshot(self):
        self.apply('begin')
        self.assertIsNone(self.read()['last_success'])
        self.assertEqual(self.read()['status'], 'incomplete')
        self.apply('commit')
        self.assertEqual(self.read()['last_success'], self.target)
        self.assertEqual(self.path.stat().st_mode & 0o777, 0o644)
        self.apply('begin'); self.apply('commit')
        selected = dict(self.target, selected_profiles=['synthetic'])
        self.apply('begin', selected); self.apply('commit', selected)
        value = self.read()
        self.assertEqual(value['target']['selected_profiles'], ['synthetic'])
        self.assertEqual((value['capabilities'], value['readiness']), ('unknown', 'unknown'))

    def test_selected_profile_names_follow_domain_safe_name_contract(self):
        selected = dict(self.target, selected_profiles=['fixture_profile', 'fixture--profile', 'fixture-', '9'])
        self.apply('begin', selected); self.apply('commit', selected)
        self.assertEqual(self.read()['last_success']['selected_profiles'], selected['selected_profiles'])
        for name in ('', '_fixture', '-fixture', 'Fixture', 'fixture/profile', 'fixture.profile', 'fixture\n'):
            with self.subTest(name=name), self.assertRaisesRegex(ValueError, 'selected profiles'):
                facts.snapshot(dict(self.target, selected_profiles=[name]))

    def test_failed_refresh_retains_success_and_retry_does_not_promote_target(self):
        self.apply('begin'); self.apply('commit')
        newer = dict(self.target, version='0.23.0-test', selected_profiles=['synthetic'])
        self.apply('begin', newer)
        self.assertEqual(self.read()['last_success'], self.target)
        self.assertEqual(self.read()['status'], 'incomplete')
        self.apply('begin', newer)
        self.assertEqual(self.read()['last_success'], self.target)
        with self.assertRaisesRegex(ValueError, 'target changed'):
            self.apply('commit')
        self.apply('commit', newer)
        self.assertEqual(self.read()['last_success'], newer)

    def test_missing_corrupt_copied_and_wrong_yard_are_unknown(self):
        with self.assertRaises(FileNotFoundError): self.read()
        self.path.write_text('{broken')
        with self.assertRaises(ValueError): self.read()
        self.apply('begin'); self.apply('commit')
        self.machine.write_text('b'*32)
        with self.assertRaisesRegex(ValueError, 'another guest'): self.read()
        self.apply('begin')
        self.assertIsNone(self.read()['last_success'])
        self.apply('commit')
        other = dict(self.target, yard=dict(name='other', project='other', instance='other'))
        self.apply('begin', other)
        self.assertIsNone(self.read()['last_success'])

    def test_atomic_commit_failure_keeps_incomplete_and_successful_version(self):
        self.apply('begin'); self.apply('commit')
        newer = dict(self.target, version='0.23.0-test')
        self.apply('begin', newer)
        before = self.path.read_bytes()
        with patch.object(facts.os, 'replace', side_effect=OSError('fixture cut')):
            with self.assertRaises(OSError): self.apply('commit', newer)
        self.assertEqual(self.path.read_bytes(), before)
        self.assertEqual(self.read()['last_success'], self.target)
        self.assertEqual(list(self.root.glob('.guest-runtime-*')), [])

    def test_reader_and_owner_check_report_incomplete_and_stale_selection(self):
        self.apply('begin'); self.apply('commit')
        original_read = facts.read
        with patch.object(facts, 'read', side_effect=lambda: original_read(self.path, self.machine)), patch.object(facts.sys, 'argv', ['facts.py', 'check']), patch.object(facts.json, 'load', return_value=dict(self.target, selected_profiles=['synthetic'])):
            self.assertEqual(facts.main(), 10)
        self.apply('begin', dict(self.target, version='0.23.0-test'))
        stdout, stderr = io.StringIO(), io.StringIO()
        with patch.object(facts, 'read', side_effect=lambda: original_read(self.path, self.machine)), patch.object(facts.sys, 'argv', ['facts.py']), contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            self.assertEqual(facts.main(), 1)
        self.assertEqual(json.loads(stdout.getvalue())['last_success']['version'], self.target['version'])
        self.assertIn('incomplete', stderr.getvalue())

    def test_symlink_unsafe_mode_and_invalid_shape(self):
        self.apply('begin'); self.apply('commit')
        self.path.chmod(0o666)
        with self.assertRaisesRegex(ValueError, 'unsafe'): self.read()
        self.path.unlink()
        self.path.symlink_to(self.machine)
        with self.assertRaisesRegex(ValueError, 'symlink'): self.apply('begin')
        for invalid in (None, {}, dict(self.target, selected_profiles=None), dict(self.target, selected_profiles=['synthetic','synthetic'])):
            with self.assertRaises(ValueError): facts.snapshot(invalid)


if __name__ == '__main__': unittest.main()
