#!/usr/bin/env python3
"""Exercise the real handler and guest program against an isolated guest tree."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import pwd
import shutil
import subprocess
import sys
import tempfile
import unittest

PACKAGE = Path(__file__).resolve().parents[1]
TEMPLATES = Path(__file__).resolve().parent / 'fixtures' / 'templates'
HELPERS = ('paseo', 'paseo-check', 'paseo-pair', 'paseo-rotate-identity', 'paseo-sync-projects')
RELEASE = '0.2.1-0123456789abcdef'
DIAGNOSTIC_LIMIT = 440
GUEST_DIRECTORIES = ('etc/systemd/system', 'var/lib/subyard', 'usr/local/bin', 'opt/subyard', 'srv/agents')

# Runs the exact guest program with only its filesystem root and owner replaced.
INCUS_SHIM = r'''#!/usr/bin/env python3
import os, sys
arguments = sys.argv[1:]
separator = arguments.index('--')
if arguments[:2] != ['exec', os.environ['YARD_INSTANCE_NAME']] or arguments[2:separator] != ['--project', os.environ['INCUS_PROJECT']]:
    sys.exit(90)
if os.path.exists(os.environ['FIXTURE_INCUS_DOWN']):
    sys.exit(1)
command = arguments[separator + 1:]
if command[:3] != ['python3', '-B', '-c']:
    sys.exit(91)
program = command[3].replace("ROOT = ''", 'ROOT = %r' % os.environ['FIXTURE_ROOT'], 1).replace('OWNER = 0', 'OWNER = %d' % os.getuid(), 1)
os.execv(sys.executable, [sys.executable, '-B', '-c', program, *command[4:]])
'''
SYSTEMCTL_SHIM = r'''#!/bin/sh
case "$*" in
  show*)
    [ -z "$FIXTURE_SYSTEMCTL_SHOW_FAILS" ] || exit 1
    if [ -n "$FIXTURE_LOADED_FRAGMENT" ]; then load=loaded fragment=$FIXTURE_LOADED_FRAGMENT
    elif [ -e "$FIXTURE_ROOT/etc/systemd/system/paseo.service" ]; then load=loaded fragment=/etc/systemd/system/paseo.service
    else load=not-found fragment=
    fi
    printf 'ActiveState=%s\nFragmentPath=%s\nLoadState=%s\n' "${FIXTURE_ACTIVE_STATE:-inactive}" "$fragment" "$load"
    exit 0 ;;
esac
printf '%s\n' "$*" >>"$FIXTURE_SYSTEMCTL_LOG"
case "$*" in
  "$FIXTURE_SYSTEMCTL_FAIL") exit 1 ;;
esac
exit 0
'''


def sha256(data):
    return hashlib.sha256(data).hexdigest()


class RetirementTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix='paseo-retire-', dir='/tmp')
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name)
        self.root = self.base / 'root'
        self.bin = self.base / 'bin'
        self.bin.mkdir()
        for name, program in (('incus', INCUS_SHIM), ('systemctl', SYSTEMCTL_SHIM)):
            path = self.bin / name
            path.write_text(program)
            path.chmod(0o755)
        for directory in GUEST_DIRECTORIES:
            (self.root / directory).mkdir(parents=True)
        entry = pwd.getpwuid(os.getuid())
        self.user, self.home = entry.pw_name, entry.pw_dir
        self.unit = self.root / 'etc/systemd/system/paseo.service'
        self.receipt = self.root / 'var/lib/subyard/paseo-ownership'
        self.runtime = self.root / 'opt/subyard/paseo'
        self.state = self.root / 'srv/agents/paseo'
        self.log = self.base / 'systemctl.log'
        self.environment = {
            'PATH': f'{self.bin}:{os.environ["PATH"]}', 'INCUS_PROJECT': 'subyard', 'YARD_INSTANCE_NAME': 'yard',
            'DEV_USER': self.user, 'SUBYARD_INTEGRATION_SOURCE_BINDING': 'source', 'FIXTURE_ROOT': str(self.root),
            'FIXTURE_INCUS_DOWN': str(self.base / 'incus-down'), 'FIXTURE_SYSTEMCTL_LOG': str(self.log),
            'FIXTURE_SYSTEMCTL_FAIL': '', 'FIXTURE_SYSTEMCTL_SHOW_FAILS': '', 'FIXTURE_LOADED_FRAGMENT': '',
        }

    def render(self, name):
        text = (TEMPLATES / (name + '.in')).read_bytes()
        return text.replace(b'@DEV_USER@', self.user.encode()).replace(b'@DEV_HOME@', self.home.encode())

    def write(self, path, data, mode=0o755):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)
        path.chmod(mode)

    def install_runtime(self):
        release = self.runtime / 'releases' / RELEASE
        self.write(release / 'manifest.json', json.dumps({'kind': 'paseo-headless'}).encode(), 0o644)
        (self.runtime / 'current').symlink_to('releases/' + RELEASE)
        (self.runtime / 'releases' / '.candidate.AbC123').mkdir()
        (self.runtime / '.current.XyZ789').symlink_to('releases/' + RELEASE)

    def install(self, receipt=True, worktree=True):
        self.write(self.unit, self.render('paseo.service'), 0o644)
        for helper in HELPERS:
            self.write(self.root / 'usr/local/bin' / helper, self.render(helper))
        self.install_runtime()
        self.write(self.state / 'config.json', b'{}', 0o644)
        self.write(self.state / 'chat/rooms.json', b'{}', 0o644)
        if worktree:
            self.write(self.state / 'worktrees/hash/slug/file', b'unpushed work', 0o644)
        (self.root / 'etc/systemd/system/paseo.service.subyard-retired').write_bytes(b'retired')
        if receipt:
            self.write_receipt()

    def write_receipt(self, digest=None):
        digest = digest or sha256(self.unit.read_bytes())
        self.write(self.receipt, f'# subyard-paseo-ownership-v1\n{digest}  /etc/systemd/system/paseo.service\n'.encode(), 0o600)

    def run_handler(self, *arguments, **environment):
        return subprocess.run([sys.executable, '-B', str(PACKAGE / 'retire.py'), *arguments], capture_output=True, text=True,
                              env={**self.environment, **environment}, timeout=60)

    def observe(self):
        result = self.run_handler('observe')
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(result.stdout)

    def apply(self, observed=None, **environment):
        observed = observed or self.observe()
        return self.run_handler('apply', 'op-retire', observed['actual'], observed['desired'], **environment)

    def reset_guest(self):
        shutil.rmtree(self.root)
        for directory in GUEST_DIRECTORIES:
            (self.root / directory).mkdir(parents=True)
        if self.log.exists():
            self.log.unlink()

    def tree(self):
        return sorted(str(path.relative_to(self.root)) for path in self.root.rglob('*'))

    def assert_removed(self):
        for path in (self.unit, self.receipt, self.runtime):
            self.assertFalse(os.path.lexists(path), path)
        for helper in HELPERS:
            self.assertFalse(os.path.lexists(self.root / 'usr/local/bin' / helper), helper)

    def assert_user_data_kept(self):
        self.assertEqual((self.state / 'worktrees/hash/slug/file').read_bytes(), b'unpushed work')
        self.assertEqual((self.state / 'chat/rooms.json').read_bytes(), b'{}')
        self.assertEqual((self.root / 'etc/systemd/system/paseo.service.subyard-retired').read_bytes(), b'retired')

    def assert_failure(self, result, phase, reason):
        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stderr.strip().splitlines()[-1], f'subyard-lifecycle-diagnostic-v1 {phase} {reason}')

    def test_nothing_installed_is_absent_without_notice(self):
        self.assertEqual(self.observe(), {'state': 'absent', 'actual': '', 'desired': ''})

    def test_receipted_installation_is_removed_and_repeat_is_a_no_op(self):
        self.install()
        observed = self.observe()
        self.assertEqual(observed['state'], 'stale')
        self.assertEqual(self.observe(), observed)
        result = self.apply(observed)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assert_removed()
        self.assert_user_data_kept()
        self.assertEqual(self.log.read_text().splitlines(), ['disable --now paseo.service', 'daemon-reload'])
        repeated = self.observe()
        self.assertEqual(json.loads(result.stdout), repeated)
        self.assertEqual(self.observe(), repeated)

    def assert_state_kept_with_warning(self, observed):
        for text in ('may hold worktrees', 'chats, schedules, loops and uploads', 're-pair',
                     'incus exec yard --project subyard -- rm -rf -- /srv/agents/paseo'):
            self.assertIn(text, observed['notice'])

    def retire(self):
        observed = self.observe()
        self.assertEqual(observed['state'], 'stale')
        result = self.apply(observed)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assert_removed()
        return observed, json.loads(result.stdout)

    def test_state_without_worktrees_is_removed_with_receipt_proof(self):
        for name, prepare in (('absent', lambda: None), ('empty', lambda: (self.state / 'worktrees').mkdir())):
            with self.subTest(name):
                self.install(worktree=False)
                prepare()
                observed, applied = self.retire()
                self.assertIn('/srv/agents/paseo, including chats, schedules, loops and uploads', observed['notice'])
                self.assertIn('re-pair', observed['notice'])
                self.assertFalse(os.path.lexists(self.state))
                self.assertEqual(applied, {'state': 'absent', 'actual': '', 'desired': ''})
                self.assertEqual(self.observe(), applied)

    def test_state_with_a_worktree_stays_and_warns_until_removed(self):
        self.install()
        observed, applied = self.retire()
        self.assert_state_kept_with_warning(observed)
        self.assert_user_data_kept()
        self.assertEqual((applied['state'], applied['actual']), ('absent', ''))
        self.assert_state_kept_with_warning(applied)
        self.assertEqual(self.observe(), applied)
        shutil.rmtree(self.state)
        self.assertEqual(self.observe(), {'state': 'absent', 'actual': '', 'desired': ''})

    def test_state_with_unreadable_or_unplain_worktrees_stays(self):
        target = self.base / 'elsewhere'
        target.mkdir()
        cases = {
            'symlink': lambda path: path.symlink_to(target),
            'dangling symlink': lambda path: path.symlink_to(self.base / 'missing'),
            'file': lambda path: path.write_bytes(b''),
            'fifo': os.mkfifo,
            'unreadable': lambda path: (path.mkdir(), path.chmod(0)),
        }
        for name, prepare in cases.items():
            with self.subTest(name):
                self.install(worktree=False)
                path = self.state / 'worktrees'
                prepare(path)
                observed, applied = self.retire()
                self.assert_state_kept_with_warning(observed)
                self.assert_state_kept_with_warning(applied)
                self.assertTrue((self.state / 'chat/rooms.json').exists())
                self.assertTrue(target.is_dir())
                if not path.is_symlink() and path.is_dir():
                    path.chmod(0o700)
                shutil.rmtree(self.state)

    def test_symlinked_state_is_never_followed(self):
        target = self.base / 'elsewhere'
        self.write(target / 'file', b'keep', 0o644)
        self.install(worktree=False)
        shutil.rmtree(self.state)
        self.state.symlink_to(target)
        observed, applied = self.retire()
        self.assert_state_kept_with_warning(observed)
        self.assertTrue(self.state.is_symlink())
        self.assertEqual((target / 'file').read_bytes(), b'keep')
        self.assert_state_kept_with_warning(applied)

    def test_already_removed_state_retires_the_rest(self):
        self.install(worktree=False)
        shutil.rmtree(self.state)
        observed, applied = self.retire()
        self.assertNotIn('/srv/agents/paseo', observed['notice'])
        self.assertEqual(applied, {'state': 'absent', 'actual': '', 'desired': ''})

    def test_state_without_receipt_proof_stays(self):
        self.install(receipt=False, worktree=False)
        observed, applied = self.retire()
        self.assert_state_kept_with_warning(observed)
        self.assert_state_kept_with_warning(applied)
        self.assertTrue(self.state.is_dir())
        # A leftover directory alone stays a warning and never becomes a removal.
        self.assertEqual(self.observe(), applied)

    def test_worktree_added_after_observation_is_refused(self):
        self.install(worktree=False)
        observed = self.observe()
        self.write(self.state / 'worktrees/hash/slug/file', b'new', 0o644)
        self.assert_failure(self.apply(observed), 'apply', 'observation-changed')
        self.assertTrue(self.unit.exists())

    def test_worktree_added_while_the_service_stops_is_kept(self):
        self.install(worktree=False)
        shim = self.bin / 'systemctl'
        shim.write_text(SYSTEMCTL_SHIM.replace('exit 0\n', 'case "$*" in disable*) mkdir -p "$FIXTURE_ROOT/srv/agents/paseo/worktrees/late" ;; esac\nexit 0\n'))
        observed, applied = self.retire()
        self.assertIn('including chats', observed['notice'])
        self.assertTrue((self.state / 'worktrees/late').is_dir())
        self.assert_state_kept_with_warning(applied)

    def test_interrupted_state_removal_retries_from_the_kept_receipt(self):
        self.install(worktree=False)
        fake = self.bin / 'rm'
        fake.write_text('#!/bin/sh\ncase "$*" in *srv/agents/paseo*) exit 1 ;; esac\nexec /bin/rm "$@"\n')
        fake.chmod(0o755)
        self.assert_failure(self.apply(), 'apply', 'operation-failed')
        self.assertTrue(self.receipt.exists())
        fake.unlink()
        self.assertEqual(self.observe()['state'], 'stale')
        self.assertEqual(self.apply().returncode, 0)
        self.assertFalse(os.path.lexists(self.state))
        self.assert_removed()

    def test_state_contents_and_service_activity_do_not_change_the_binding(self):
        self.install()
        before = self.observe()
        self.write(self.state / 'chat/other.json', b'{}', 0o644)
        self.assertEqual(self.observe(), before)
        self.environment['FIXTURE_ACTIVE_STATE'] = 'active'
        self.assertEqual(self.observe(), before)

    def test_exact_published_template_proves_a_unit_without_receipt(self):
        self.install(receipt=False)
        self.assertEqual(self.observe()['state'], 'stale')
        result = self.apply()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assert_removed()
        self.assert_user_data_kept()

    def assert_foreign(self):
        before = self.tree()
        observed = self.observe()
        self.assertEqual((observed['state'], observed['actual'], observed['desired']), ('absent', '', ''))
        self.assertIn("incus exec yard --project subyard -- sh -c 'systemctl disable --now paseo.service;", observed['notice'])
        self.assertEqual(self.tree(), before)
        self.assertFalse(self.log.exists())

    def test_edited_unit_without_a_receipt_is_foreign_and_preserved(self):
        self.install(receipt=False)
        self.write(self.unit, self.unit.read_bytes() + b'# edited\n', 0o644)
        self.assert_foreign()

    def test_unit_that_differs_from_its_receipt_is_foreign(self):
        self.install()
        self.write(self.unit, self.unit.read_bytes() + b'# edited\n', 0o644)
        self.assert_foreign()

    def test_changed_receipt_is_foreign(self):
        for name, mutate in (
                ('digest', lambda: self.write_receipt('0' * 64)),
                ('mode', lambda: self.receipt.chmod(0o640)),
                ('header', lambda: self.write(self.receipt, b'# other\n' + self.receipt.read_bytes().split(b'\n', 1)[1], 0o600)),
                ('path', lambda: self.write(self.receipt, self.receipt.read_bytes().replace(b'paseo.service', b'other.service'), 0o600))):
            with self.subTest(name):
                self.install()
                mutate()
                self.assert_foreign()
                shutil.rmtree(self.runtime)

    def test_symlinked_ownership_files_are_never_followed(self):
        decoy = self.base / 'decoy'
        decoy.write_bytes(self.render('paseo.service'))
        self.install()
        self.unit.unlink()
        self.unit.symlink_to(decoy)
        self.assert_foreign()
        self.assertTrue(decoy.exists())
        self.unit.unlink()
        self.write(self.unit, decoy.read_bytes(), 0o644)
        target = self.base / 'receipt'
        target.write_bytes(self.receipt.read_bytes())
        target.chmod(0o600)
        self.receipt.unlink()
        self.receipt.symlink_to(target)
        self.assert_foreign()
        self.assertTrue(target.exists())

    def test_unproven_helper_is_kept_and_reported_without_blocking(self):
        self.install()
        foreign = self.root / 'usr/local/bin/paseo-pair'
        self.write(foreign, b'#!/bin/sh\necho operator\n')
        symlink = self.root / 'usr/local/bin/paseo'
        symlink.unlink()
        symlink.symlink_to(foreign)
        result = self.apply()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(foreign.read_bytes(), b'#!/bin/sh\necho operator\n')
        self.assertTrue(symlink.is_symlink())
        self.assertFalse((self.root / 'usr/local/bin/paseo-check').exists())
        self.assertFalse(self.unit.exists())
        observed = self.observe()
        self.assertEqual((observed['state'], observed['actual']), ('absent', ''))
        self.assertIn('paseo-pair', observed['notice'])
        self.assertEqual(json.loads(result.stdout), observed)

    def test_unproven_runtime_is_kept_and_reported(self):
        for name, mutate in (
                ('extra', lambda: self.write(self.runtime / 'operator-file', b'keep', 0o644)),
                ('manifest', lambda: self.write(self.runtime / 'releases' / RELEASE / 'manifest.json', b'{"kind":"other"}', 0o644)),
                ('release', lambda: (self.runtime / 'releases' / 'other').mkdir()),
                ('current', lambda: ((self.runtime / 'current').unlink(), (self.runtime / 'current').symlink_to('/etc')))):
            with self.subTest(name):
                self.install()
                mutate()
                result = self.apply()
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertTrue(self.runtime.is_dir())
                self.assertFalse(self.unit.exists())
                observed = self.observe()
                self.assertEqual(observed['state'], 'absent')
                self.assertIn('/opt/subyard/paseo', observed['notice'])
                shutil.rmtree(self.runtime)
                for helper in HELPERS:
                    self.assertFalse((self.root / 'usr/local/bin' / helper).exists())

    def test_leftovers_without_unit_are_removed(self):
        self.install(receipt=False)
        self.unit.unlink()
        self.assertEqual(self.observe()['state'], 'stale')
        result = self.apply()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assert_removed()
        self.assertEqual(self.log.read_text().splitlines(), ['daemon-reload'])
        self.assert_user_data_kept()

    def test_interrupted_removal_fails_then_retries_from_kept_proof(self):
        for failing in ('disable --now paseo.service', 'daemon-reload'):
            with self.subTest(failing):
                self.install()
                result = self.apply(FIXTURE_SYSTEMCTL_FAIL=failing)
                self.assert_failure(result, 'apply', 'operation-failed')
                self.assertTrue(self.receipt.exists())
                self.assertEqual(self.observe()['state'], 'stale')
                retried = self.apply()
                self.assertEqual(retried.returncode, 0, retried.stderr)
                self.assert_removed()
                self.assert_user_data_kept()

    def test_retry_after_the_unit_unlink_converges_without_a_stop(self):
        self.install()
        os.unlink(self.unit)
        # systemd still lists the unlinked unit until daemon-reload, and it is already stopped.
        self.environment['FIXTURE_LOADED_FRAGMENT'] = '/etc/systemd/system/paseo.service'
        result = self.apply()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.log.read_text().splitlines(), ['daemon-reload'])
        self.assert_removed()

    def assert_service_untouched(self, expected_rm):
        before = self.tree()
        observed = self.observe()
        self.assertEqual({key: observed[key] for key in ('state', 'actual', 'desired')}, {'state': 'absent', 'actual': '', 'desired': ''})
        self.assertIn('is not proven Subyard-managed and stays', observed['notice'])
        self.assertIn(f'rm -f -- {expected_rm} /var/lib/subyard/paseo-ownership', observed['notice'])
        self.assertLessEqual(len(observed['notice']), DIAGNOSTIC_LIMIT)
        self.assertEqual(self.tree(), before)
        self.assertFalse(self.log.exists())
        self.assertEqual(json.loads(self.run_handler('observe').stdout), observed)
        refused = self.run_handler('apply', 'op-retire', 'a' * 64, 'b' * 64)
        self.assert_failure(refused, 'apply', 'observation-changed')
        self.assertEqual(self.tree(), before)
        self.assertFalse(self.log.exists())

    def test_foreign_loaded_units_keep_every_leftover_and_the_state(self):
        for location in ('/usr/lib/systemd/system/paseo.service', '/lib/systemd/system/paseo.service',
                         '/run/systemd/system/paseo.service'):
            for active in ('active', 'inactive'):
                with self.subTest(location=location, active=active):
                    # A valid receipt and empty worktrees would otherwise delete the state it may use.
                    self.install(worktree=False)
                    self.unit.unlink()
                    self.environment.update(FIXTURE_LOADED_FRAGMENT=location, FIXTURE_ACTIVE_STATE=active)
                    self.assert_service_untouched(location)
                    self.assertTrue(self.receipt.exists() and self.runtime.exists() and self.state.exists())
                    self.assertTrue((self.root / 'usr/local/bin/paseo').exists())
                    self.reset_guest()

    def test_transient_unit_shadowing_a_proven_file_is_foreign(self):
        self.install(worktree=False)
        self.environment['FIXTURE_LOADED_FRAGMENT'] = '/run/systemd/transient/paseo.service'
        self.assert_service_untouched('/etc/systemd/system/paseo.service')
        self.assertTrue(self.unit.exists() and self.state.exists())

    def test_running_service_without_a_unit_file_is_foreign(self):
        self.install(worktree=False)
        self.unit.unlink()
        self.environment['FIXTURE_ACTIVE_STATE'] = 'active'
        self.assert_service_untouched('/etc/systemd/system/paseo.service')
        self.assertTrue(self.state.exists() and self.runtime.exists())

    def test_foreign_fragment_outside_known_locations_stays_out_of_the_command(self):
        self.environment['FIXTURE_LOADED_FRAGMENT'] = '/dev/null'
        self.assert_service_untouched('/etc/systemd/system/paseo.service')

    def test_unreadable_systemd_state_is_a_normal_diagnostic_without_effects(self):
        self.install(worktree=False)
        before = self.tree()
        self.environment['FIXTURE_SYSTEMCTL_SHOW_FAILS'] = '1'
        self.assert_failure(self.run_handler('observe'), 'inspect', 'native-unavailable')
        self.assertEqual(self.tree(), before)
        self.assertFalse(self.log.exists())

    def test_changed_observation_is_refused_without_effects(self):
        self.install()
        observed = self.observe()
        self.write(self.root / 'usr/local/bin/paseo', self.render('paseo') + b'# edited\n')
        self.assert_failure(self.apply(observed), 'apply', 'observation-changed')
        self.assertTrue(self.unit.exists())
        self.assertFalse(self.log.exists())
        desired = dict(observed, actual='0' * 64)
        self.assert_failure(self.apply(desired), 'apply', 'observation-changed')
        self.assertTrue(self.unit.exists())

    def test_guest_refuses_a_digest_that_no_longer_matches(self):
        self.install()
        spec = importlib.util.spec_from_file_location('retirement', PACKAGE / 'retire.py')
        previous = dict(os.environ)
        os.environ.update(self.environment)
        self.addCleanup(lambda: (os.environ.clear(), os.environ.update(previous)))
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        result = module.guest('apply', self.user, '0' * 64)
        self.assertEqual(result.returncode, 3)
        self.assertTrue(self.unit.exists())

    def test_unavailable_guest_is_a_normal_diagnostic(self):
        (self.base / 'incus-down').write_text('')
        self.assert_failure(self.run_handler('observe'), 'inspect', 'native-unavailable')

    def test_notices_fit_the_frozen_diagnostic_limit_for_longest_names(self):
        names = {'YARD_INSTANCE_NAME': 'y' * 63, 'INCUS_PROJECT': 'p' * 63}
        self.environment.update(names)
        self.install()
        stale = self.observe()
        self.assertLessEqual(len(stale['notice']), DIAGNOSTIC_LIMIT)
        self.assert_notice_keeps_command(stale)
        self.write(self.root / 'usr/local/bin/paseo-pair', b'#!/bin/sh\n')
        for helper in HELPERS[:2]:
            self.write(self.root / 'usr/local/bin' / helper, b'#!/bin/sh\n')
        retained = self.apply()
        self.assertEqual(retained.returncode, 0, retained.stderr)
        retained = json.loads(retained.stdout)
        self.assertLessEqual(len(retained['notice']), DIAGNOSTIC_LIMIT)
        self.assert_notice_keeps_command(retained)
        self.write(self.unit, b'foreign\n', 0o644)
        foreign = self.observe()
        self.assertEqual(foreign['state'], 'absent')
        self.assertLessEqual(len(foreign['notice']), DIAGNOSTIC_LIMIT)
        self.assertEqual(self.observe(), foreign)
        self.unit.unlink()
        self.environment['FIXTURE_LOADED_FRAGMENT'] = '/usr/lib/systemd/system/paseo.service'
        self.assertIn('rm -f -- /usr/lib/systemd/system/paseo.service', self.observe()['notice'])

    def assert_notice_keeps_command(self, observed):
        self.assertIn('rm -rf -- /srv/agents/paseo', observed['notice'])

    def test_published_templates_match_the_recognized_digests(self):
        program = self.run_guest_constants()
        self.assertEqual(sha256((TEMPLATES / 'paseo.service.in').read_bytes()), program['UNIT_TEMPLATE'])
        for helper in HELPERS:
            self.assertIn(sha256((TEMPLATES / (helper + '.in')).read_bytes()), program['HELPER_TEMPLATES'][helper])

    def run_guest_constants(self):
        spec = importlib.util.spec_from_file_location('retirement', PACKAGE / 'retire.py')
        previous = dict(os.environ)
        os.environ.update(self.environment)
        self.addCleanup(lambda: (os.environ.clear(), os.environ.update(previous)))
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        namespace = {}
        exec(module.GUEST[:module.GUEST.index("if sys.argv[1] == 'observe'")], namespace)
        return namespace

    def test_release_observation_fixture_matches_the_empty_guest(self):
        spec = importlib.util.spec_from_file_location('observation', Path(__file__).resolve().parent / 'release-observation.py')
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        (arguments, expected), = module.read_only_commands(PACKAGE)
        self.assertEqual(arguments[:5], ['exec', 'yard', '--project', 'subyard', '--'])
        self.environment['DEV_USER'] = 'dev'
        output = subprocess.run([str(self.bin / 'incus'), *arguments[:-1], self.user], capture_output=True, text=True,
                                env=self.environment, check=True).stdout
        self.assertEqual(output, expected)


if __name__ == '__main__':
    unittest.main()
