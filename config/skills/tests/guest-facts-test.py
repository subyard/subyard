#!/usr/bin/env python3
"""Guest snapshot state contract; physical owner/mode checks remain explicit."""
import importlib.util
import json
import os
from pathlib import Path
import stat
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
        patch.object(facts, 'safe', return_value=(0, 0)).start()
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

    def check(self, target):
        original_read = facts.read
        with patch.object(facts, 'read', side_effect=lambda: original_read(self.path, self.machine)), \
             patch.object(facts.sys, 'argv', ['facts.py', 'check']), \
             patch.object(facts.sys, 'stdin', io.StringIO(json.dumps(target))):
            return facts.main()

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
            self.assertEqual(facts.main(), 11)
        self.apply('begin', dict(self.target, version='0.23.0-test'))
        stdout, stderr = io.StringIO(), io.StringIO()
        with patch.object(facts, 'read', side_effect=lambda: original_read(self.path, self.machine)), patch.object(facts.sys, 'argv', ['facts.py']), contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            self.assertEqual(facts.main(), 1)
        self.assertEqual(json.loads(stdout.getvalue())['last_success']['version'], self.target['version'])
        self.assertIn('incomplete', stderr.getvalue())

    def test_check_distinguishes_pending_profiles_from_every_other_target_difference(self):
        self.apply('begin'); self.apply('commit')
        self.assertEqual(self.check(self.target), 0)
        before = self.path.read_bytes()
        pending = dict(self.target, selected_profiles=['synthetic'])
        self.assertEqual(self.check(pending), 11)
        self.assertEqual(self.path.read_bytes(), before, 'checking must not apply configuration')
        targets = [dict(self.target, version='0.23.0-test'), dict(pending, version='0.23.0-test')]
        for key in ('name', 'project', 'instance'):
            targets.append(dict(pending, yard=dict(self.target['yard'], **{key: 'different'})))
            targets.append(dict(self.target, yard=dict(self.target['yard'], **{key: 'different'})))
        for target in targets:
            with self.subTest(target=target):
                self.assertEqual(self.check(target), 10)
        self.apply('begin', pending)
        self.assertEqual(self.check(pending), 10)
        self.assertEqual(self.check(self.target), 10)
        self.assertEqual(self.read()['last_success'], self.target)
        self.apply('commit', pending)
        self.assertEqual(self.check(pending), 0)
        self.assertEqual(self.read()['last_success'], pending)
        self.assertEqual(self.path.stat().st_mode & 0o777, 0o644)

    def test_pending_profiles_never_hides_invalid_facts_or_invalid_target(self):
        self.apply('begin'); self.apply('commit')
        pending = dict(self.target, selected_profiles=['synthetic'])
        for target in (None, {}, dict(pending, version='invalid version'),
                       dict(pending, yard={}), dict(pending, selected_profiles=None),
                       dict(pending, selected_profiles=['synthetic', 'synthetic'])):
            with self.subTest(target=target), self.assertRaises(ValueError):
                self.check(target)
        self.path.chmod(0o666)
        with self.assertRaisesRegex(ValueError, 'unsafe'): self.check(pending)
        self.path.chmod(0o644)
        self.machine.write_text('b'*32)
        with self.assertRaisesRegex(ValueError, 'another guest'): self.check(pending)
        self.machine.write_text('a'*32)
        self.path.write_text('{broken')
        with self.assertRaises(ValueError): self.check(pending)

    def test_symlink_unsafe_mode_and_invalid_shape(self):
        self.apply('begin'); self.apply('commit')
        self.path.chmod(0o666)
        with self.assertRaisesRegex(ValueError, 'unsafe'): self.read()
        self.path.unlink()
        self.path.symlink_to(self.machine)
        with self.assertRaisesRegex(ValueError, 'symlink'): self.apply('begin')
        for invalid in (None, {}, dict(self.target, selected_profiles=None), dict(self.target, selected_profiles=['synthetic','synthetic'])):
            with self.assertRaises(ValueError): facts.snapshot(invalid)


class Ownership(unittest.TestCase):
    """Kernel metadata snapshots; native namespace checks own the real mounts."""

    def setUp(self):
        self.path = facts.FACTS
        self.parents = (self.path.parent, *self.path.parent.parents)
        self.metadata = {path: self.info(stat.S_IFDIR | 0o755) for path in self.parents}
        self.metadata[self.path] = self.info(stat.S_IFREG | 0o644)
        self.maps = {'uid': '1000 0 1\n', 'gid': '1001 0 1\n'}
        self.writable = set()
        target = dict(version='0.22.0-test', yard=dict(name='yard', project='project', instance='yard'), selected_profiles=[])
        value = dict(schema=1, machine_id='a'*32, status='current', target=target,
                     last_success=target, capabilities='unknown', readiness='unknown')
        stack = contextlib.ExitStack()
        self.addCleanup(stack.close)
        stack.enter_context(patch.object(Path, 'lstat', lambda path: self.metadata[path]))
        def read_map(path):
            value = self.maps[path.name.removesuffix('_map')]
            if isinstance(value, Exception):
                raise value
            return value
        self.map_read = stack.enter_context(patch.object(Path, 'read_text', side_effect=read_map, autospec=True))
        self.bytes_read = stack.enter_context(patch.object(Path, 'read_bytes', return_value=json.dumps(value).encode()))
        stack.enter_context(patch.object(facts, 'identity', return_value='a'*32))
        stack.enter_context(patch.object(facts.os, 'geteuid', return_value=1000))
        stack.enter_context(patch.object(facts.os, 'getegid', return_value=1001))
        self.access = stack.enter_context(patch.object(facts.os, 'access', side_effect=lambda path, mode, **kwargs: path in self.writable))

    @staticmethod
    def info(mode, uid=65534, gid=65534, links=1):
        return os.stat_result((mode, 1, 1, links, uid, gid, 1024, 0, 0, 0))

    def test_root_owned_facts_keep_normal_admission_without_namespace_reads(self):
        self.metadata = {path: self.info(info.st_mode, 0, 0) for path, info in self.metadata.items()}
        self.assertEqual(facts.read()['status'], 'current')
        self.map_read.assert_not_called()
        self.access.assert_not_called()

    def test_codex_single_user_namespace_accepts_protected_facts(self):
        self.assertEqual(facts.read()['status'], 'current')
        self.assertEqual({call.args[0] for call in self.access.call_args_list}, {*self.parents, self.path})
        self.assertTrue(all(call.kwargs == {'effective_ids': True} for call in self.access.call_args_list))

    def test_namespace_maps_fail_closed(self):
        for name in ('uid', 'gid'):
            for mapping in ('', 'invalid', '0 0 1', '1000 1000 1', '0 0 4294967295',
                            '1000 0 1\n2000 2000 1', OSError('map unreadable')):
                with self.subTest(name=name, mapping=mapping):
                    original = self.maps[name]
                    self.maps[name] = mapping
                    try:
                        with self.assertRaisesRegex(ValueError, 'unsafe yard facts directory'):
                            facts.read()
                    finally:
                        self.maps[name] = original
        self.bytes_read.assert_not_called()

    def test_namespace_exception_requires_fixed_path_and_unmapped_root_anchor(self):
        with self.assertRaisesRegex(ValueError, 'unsafe yard facts directory'):
            facts.read(Path('/tmp/guest-runtime.json'))
        for uid, gid in ((1000, 65534), (65534, 1001), (65534, 0)):
            with self.subTest(uid=uid, gid=gid):
                self.metadata[Path('/')] = self.info(stat.S_IFDIR | 0o755, uid, gid)
                with self.assertRaisesRegex(ValueError, 'unsafe yard facts directory'):
                    facts.read()
        self.bytes_read.assert_not_called()

    def test_mapped_developer_ownership_is_refused_even_on_read_only_mounts(self):
        # access(W_OK) is false throughout: readonly bind mounts do not establish
        # trust in a dev-owned directory, whose owner can later chmod/replace it.
        for path in (*self.parents, self.path):
            for uid, gid in ((1000, 65534), (65534, 1001)):
                with self.subTest(path=path, uid=uid, gid=gid):
                    original = self.metadata[path]
                    self.metadata[path] = self.info(original.st_mode, uid, gid)
                    try:
                        with self.assertRaisesRegex(ValueError, 'unsafe yard facts'):
                            facts.read()
                    finally:
                        self.metadata[path] = original
        self.bytes_read.assert_not_called()

    def test_writable_ancestors_and_file_are_refused(self):
        for path in (*self.parents, self.path):
            original = self.metadata[path]
            for permission in (0o002, 0o020):
                with self.subTest(path=path, permission=permission):
                    self.metadata[path] = self.info(original.st_mode | permission)
                    with self.assertRaisesRegex(ValueError, 'unsafe yard facts'):
                        facts.read()
                    self.metadata[path] = original
            with self.subTest(path=path, access='writable'):
                self.writable.add(path)
                with self.assertRaisesRegex(ValueError, 'unsafe yard facts'):
                    facts.read()
                self.writable.remove(path)
        self.bytes_read.assert_not_called()

    def test_links_and_foreign_visible_owners_remain_refused(self):
        for path in (*self.parents, self.path):
            original = self.metadata[path]
            invalid = (self.info(stat.S_IFLNK | 0o777), self.info(original.st_mode, 2000, 2000))
            if path == self.path:
                invalid += (self.info(original.st_mode, links=2),)
            for info in invalid:
                with self.subTest(path=path, mode=info.st_mode, uid=info.st_uid, links=info.st_nlink):
                    self.metadata[path] = info
                    with self.assertRaisesRegex(ValueError, 'unsafe yard facts'):
                        facts.read()
            self.metadata[path] = original
        self.bytes_read.assert_not_called()


if __name__ == '__main__': unittest.main()
