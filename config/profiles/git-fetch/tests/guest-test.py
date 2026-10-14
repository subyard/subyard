#!/usr/bin/env python3
"""Lifecycle idempotency, deselection,  ownership contracts."""
import importlib.util
import os
import stat
from pathlib import Path
import pwd
import tempfile
import unittest
from unittest.mock import patch

PROFILE = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('guest', PROFILE/'guest.py')
guest = importlib.util.module_from_spec(spec)
spec.loader.exec_module(guest)


class GuestTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='subyard-git-fetch-guest-', dir='/tmp')
        self.addCleanup(self.temporary.cleanup)
        self.base = Path(self.temporary.name)
        self.units = self.base/'units'
        self.units.mkdir()
        self.state = self.base/'state'
        self.calls = []
        self.timer = ['inactive', 'disabled']
        self.service = ['inactive', 'static']
        self.loaded = set()
        self.fragments = {}
        self.dropins = {}
        self.overrides = {}
        self.addCleanup(patch.stopall)
        patch.object(guest, 'systemctl', self.systemctl).start()
        self.user = pwd.getpwuid(os.getuid()).pw_name

    def systemctl(self, *args):
        self.calls.append(args)
        state = self.timer if guest.TIMER in args else self.service
        if args[0] == 'show':
            unit = args[1]
            loaded = unit in self.loaded
            fragment = self.fragments.get(unit, str(self.units/unit)) if loaded else ''
            account = pwd.getpwnam(self.user)
            import grp
            fields = {'User':self.user,'Group':grp.getgrgid(account.pw_gid).gr_name, 'Type':'oneshot','KillMode':'control-group','UMask':'0077','LimitCORE':'0','TimeoutStartUSec':'infinity','TimeoutStopUSec':'5s','Environment':f'HOME={account.pw_dir} SSH_AUTH_SOCK={account.pw_dir}/.ssh/subyard-agent.sock','EnvironmentFiles':'','ExecStartPre':'','ExecStartPost':'','ExecStop':'','ExecStopPost':'','ExecStart':'{ path=/usr/bin/setpriv ; argv[]=/usr/bin/setpriv --no-new-privs /usr/bin/python3 -B /var/lib/subyard/git-fetch/fetch.py ; ignore_errors=no ; start_time=none ; }','TimersMonotonic':'{ OnBootUSec=5min ; next_elapse=123 }\nTimersMonotonic={ OnUnitInactiveUSec=5min ; next_elapse=456 }','AccuracyUSec':'1s'}
            fields.update(self.overrides.get(unit, {}))
            omitted_empty_arrays = {'EnvironmentFiles','ExecStartPre','ExecStartPost','ExecStop','ExecStopPost'}
            envelope = '\n'.join(key+'='+value for key,value in fields.items() if value or key not in omitted_empty_arrays)
            return envelope+f'\nActiveState={state[0]}\nUnitFileState={state[1]}\nLoadState={"loaded" if loaded else "not-found"}\nFragmentPath={fragment}\nNeedDaemonReload=no\nDropInPaths={self.dropins.get(unit, "")}'
        if args[0] == 'stop':
            if any(unit not in self.loaded for unit in args[1:]):
                raise ValueError('unit not loaded')
            self.timer[0] = self.service[0] = 'inactive'
        elif args[0] == 'start':
            state[0] = 'active'
        elif args[0] in ('enable', 'disable'):
            state[1] = 'enabled' if args[0] == 'enable' else 'disabled'
        elif args[0] == 'daemon-reload':
            self.loaded = {unit for unit in (guest.TIMER, guest.SERVICE) if (self.units/unit).exists()}
        return ''

    def runtime(self, selected=True, stopped=False):
        result = guest.Runtime(PROFILE, self.user, selected, stopped, state=self.state, units=self.units)
        original = result.safe
        def fixture_safe(path):
            # The private /tmp fixture substitutes only the production root ancestry.
            if path.is_symlink():
                raise ValueError('unsafe managed path')
            self.assertTrue(path.is_relative_to(self.base))
            ancestors = [ancestor for ancestor in path.parents if ancestor == self.base or ancestor.is_relative_to(self.base)]
            for ancestor in reversed(ancestors):
                try:
                    info = ancestor.lstat()
                except FileNotFoundError:
                    break
                if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o022:
                    raise ValueError('unsafe managed parent')
        result.safe = fixture_safe
        self.assertIsNotNone(original)
        return result

    def test_install_repeat_drift_and_disable(self):
        disabled = self.runtime(False)
        self.assertTrue(disabled.check())
        enabled = self.runtime()
        self.assertFalse(enabled.check())
        self.assertTrue(enabled.apply())
        self.assertEqual(self.timer, ['active', 'enabled'])
        unit = self.units/guest.TIMER
        inode = unit.stat().st_ino
        self.calls.clear()
        self.assertTrue(enabled.apply())
        self.assertEqual(unit.stat().st_ino, inode)
        self.assertTrue(all(c[0] == 'show' for c in self.calls))
        unit.write_text(guest.MARKER+'drift\n')
        self.assertFalse(enabled.check())
        self.assertTrue(enabled.apply())
        self.service[0] = 'activating'
        self.assertTrue(disabled.apply())
        self.assertEqual(self.timer, ['inactive', 'disabled'])
        self.assertEqual(self.service[0], 'inactive')
        self.assertTrue((self.state/'fetch.py').exists())
        self.assertTrue(disabled.apply())
        self.assertTrue(enabled.apply())

    def test_absent_parent_disabled_is_read_only_and_selected_creates_it(self):
        self.state = self.base/'missing-parent'/'state'
        disabled = self.runtime(False)
        self.assertTrue(disabled.check())
        self.assertTrue(disabled.apply())
        self.assertFalse(self.state.parent.exists())
        self.assertTrue(all(call[0]=='show' for call in self.calls))
        self.assertTrue(self.runtime().apply())
        self.assertEqual(self.state.parent.stat().st_mode & 0o777,0o755)
        self.assertTrue(self.state.exists())

    def test_existing_unsafe_parent_is_never_repaired_or_used(self):
        self.state = self.base/'unsafe'/'state'
        self.state.parent.mkdir()
        self.state.parent.chmod(0o777)
        for selected in (False,True):
            with self.assertRaisesRegex(ValueError,'unsafe managed parent'):
                self.runtime(selected).apply()
        self.assertEqual(self.calls,[])
        self.assertFalse(self.state.exists())
        self.assertEqual(self.state.parent.stat().st_mode & 0o777,0o777)

    def test_foreign_units_and_symlinks_refused_before_effects(self):
        (self.units/guest.TIMER).write_text('foreign\n')
        with self.assertRaisesRegex(ValueError, 'foreign service'):
            self.runtime().apply()
        self.assertEqual(self.calls, [])
        (self.units/guest.TIMER).unlink()
        (self.units/guest.TIMER).symlink_to(self.base/'elsewhere')
        with self.assertRaisesRegex(ValueError, 'unsafe managed'):
            self.runtime().apply()
        self.assertEqual(self.calls, [])

    def test_owned_partial_install_repairs_missing_units(self):
        self.state.mkdir(mode=0o755)
        self.state.chmod(0o755)
        marker = self.state/'marker'
        marker.write_text(guest.MARKER)
        marker.chmod(0o644)
        runtime = self.runtime()
        self.assertTrue(runtime.apply())
        self.assertEqual(self.loaded, {guest.TIMER, guest.SERVICE})

    def test_interrupted_marker_publication_leaves_no_foreign_runtime(self):
        runtime = self.runtime()
        with patch.object(runtime, 'publish', side_effect=OSError('interrupted publication')):
            with self.assertRaises(OSError):
                runtime.apply()
        self.assertFalse(self.state.exists())
        self.assertEqual(list(self.base.glob('.git-fetch.*')), [])
        self.assertTrue(runtime.apply())

    def test_foreign_loaded_unit_and_unmarked_state_refused(self):
        runtime = self.runtime()
        self.state.mkdir(mode=0o755)
        self.state.chmod(0o755)
        with self.assertRaisesRegex(ValueError, 'foreign runtime'):
            runtime.apply()
        marker = self.state/'marker'
        marker.write_text(guest.MARKER)
        marker.chmod(0o644)
        self.loaded.add(guest.TIMER)
        self.fragments[guest.TIMER] = '/usr/lib/systemd/system/foreign.timer'
        with self.assertRaisesRegex(ValueError, 'foreign loaded service'):
            runtime.apply()
        self.assertFalse(any(call[0] == 'stop' for call in self.calls))

    def test_pause_resume_preserves_verified_runtime_and_refuses_replacement(self):
        runtime = self.runtime()
        runtime.apply()
        self.assertTrue(runtime.pause())
        self.assertFalse(runtime.pause())
        runtime.resume()
        runtime.resume()
        self.assertEqual(self.timer, ['active', 'enabled'])
        self.assertTrue(runtime.pause())
        (self.units/guest.TIMER).write_text('foreign timer\n')
        self.calls.clear()
        with self.assertRaisesRegex(ValueError, 'foreign service'):
            runtime.resume()
        self.assertFalse(any(call[0] == 'start' for call in self.calls))

    def test_resume_refuses_foreign_loaded_fragment(self):
        runtime = self.runtime()
        runtime.apply()
        runtime.pause()
        self.fragments[guest.TIMER] = '/usr/lib/systemd/system/foreign.timer'
        self.calls.clear()
        with self.assertRaisesRegex(ValueError, 'foreign loaded'):
            runtime.resume()
        self.assertFalse(any(call[0] == 'start' for call in self.calls))

    def test_effective_override_guards_preserve_foreign_files(self):
        runtime = self.runtime()
        runtime.apply()
        self.dropins[guest.SERVICE] = '/run/systemd/system/service.d/zzz-lxc-service.conf'
        self.assertTrue(runtime.check())
        self.assertTrue(runtime.pause())
        runtime.resume()
        for unit, fields in ((guest.SERVICE, {'User':'root'}),
                             (guest.SERVICE, {'KillMode':'process'}),
                             (guest.TIMER, {'TimersMonotonic':'{ OnUnitInactiveUSec=1s ; next_elapse=0 }'})):
            with self.subTest(fields=fields):
                self.overrides[unit] = fields
                with self.assertRaisesRegex(ValueError, 'foreign loaded'):
                    runtime.check()
                for operation in (runtime.apply, runtime.pause, runtime.resume, self.runtime(False).apply):
                    self.calls.clear()
                    with self.assertRaises(ValueError):
                        operation()
                    self.assertFalse(any(call[0] in ('start','stop','enable','disable') for call in self.calls))
                self.overrides.clear()
        override = self.units/(guest.SERVICE+'.d')/'override.conf'
        override.parent.mkdir()
        override.write_text('[Service]\nUser=root\n')
        self.dropins[guest.SERVICE] = str(override)
        with self.assertRaises(ValueError):
            runtime.apply()
        self.assertTrue(override.exists())


    def test_stopped_intent_stops_live_worker_even_when_timer_is_inactive(self):
        running = self.runtime()
        running.apply()
        self.service[0] = 'activating'
        self.assertTrue(running.check())
        self.timer[0] = 'inactive'
        stopped = self.runtime(stopped=True)
        self.assertFalse(stopped.check())
        self.assertTrue(stopped.apply())
        self.assertEqual(self.timer,['inactive','enabled'])
        self.assertEqual(self.service[0],'inactive')
        self.assertTrue(any(call[0]=='stop' and guest.SERVICE in call for call in self.calls))

    def test_native_repeated_timer_lines_and_omitted_arrays_converge(self):
        runtime = self.runtime()
        self.assertTrue(runtime.apply())
        details = runtime.manager_details(guest.TIMER)
        self.assertIn('OnBootUSec=5min',details['TimersMonotonic'])
        self.assertIn('OnUnitInactiveUSec=5min',details['TimersMonotonic'])
        for key in ('EnvironmentFiles','ExecStartPre','ExecStartPost','ExecStop','ExecStopPost'):
            self.assertNotIn(key,runtime.manager_details(guest.SERVICE))
        self.assertTrue(runtime.check())

    def test_repeated_exec_and_nonempty_extra_arrays_are_refused(self):
        runtime = self.runtime()
        runtime.apply()
        fields = runtime.manager_details(guest.SERVICE)
        start = fields['ExecStart']
        fields.pop('User')
        self.assertFalse(runtime.loaded_owned(guest.SERVICE,fields))
        for key,value in (('ExecStart',start+'\nExecStart='+start),
                          ('ExecStartPre','{ path=/bin/true ; argv[]=/bin/true ; }'),
                          ('ExecStop','{ path=/bin/true ; argv[]=/bin/true ; }'),
                          ('EnvironmentFiles','/tmp/fixture (ignore_errors=no)'),
                          ('User','')):
            with self.subTest(key=key):
                self.overrides[guest.SERVICE] = {key:value}
                self.calls.clear()
                with self.assertRaisesRegex(ValueError,'foreign loaded'):
                    runtime.apply()
                self.assertFalse(any(call[0] in ('stop','start','enable','disable') for call in self.calls))
                self.overrides.clear()



if __name__ == '__main__':
    unittest.main()
