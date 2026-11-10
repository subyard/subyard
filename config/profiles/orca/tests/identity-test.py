#!/usr/bin/env python3
"""Owner-level retry proof with real unit files and mocked native manager."""
import importlib.util
import os
from pathlib import Path
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[4]
sys.path.insert(0, str(ROOT/'config/identity'))
spec = importlib.util.spec_from_file_location('orca_identity', Path(__file__).resolve().parents[1]/'resources/orca/identity.py')
m = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)


class Identity(unittest.TestCase):
    def test_full_runtime_retry_preserves_data_and_intent(self):
        self.installed_case(True)

    def test_registration_only_retry_keeps_application_absent(self):
        self.installed_case(False)

    def installed_case(self, full):
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            root = Path(temporary); directory = root/'units'; directory.mkdir()
            marker = root/'contract'; asset = root/'helper'; retained = root/'persisted-grant'
            retained.write_text('retained'); inode = retained.stat().st_ino
            def write(path, data): path.write_bytes(data); path.chmod(0o644)
            before = ['User=old\nGroup=old\nEnvironment=HOME=/home/old\n']*2
            after = [value.replace('old', 'new') for value in before]
            for name, data in zip(m.UNITS, [*before, 'timer\n']):
                if full or name!=m.UNITS[0]:write(directory/name, data.encode())
            write(asset, b'helper\n')
            manifest = dict(full=full, before=before, after=after, files=[str(asset), *([str(directory/m.UNITS[0])] if full else [])], version='6:source', oldDigest='old', newDigest='new', observation=dict(state='current', actual='old'))
            if full: write(marker, m.contract_digest(manifest, [asset.read_bytes(), before[0].encode()]))
            states = ['active' if full else 'inactive', 'inactive', 'active']; enabled = ['enabled', 'static', 'enabled']; calls = []
            def manager(name):
                index = m.UNITS.index(name); user = 'new' if b'User=new' in (directory/name).read_bytes() else 'old'
                return dict(LoadState='loaded', FragmentPath=str(directory/name), DropInPaths='', ActiveState=states[index], UnitFileState=enabled[index], User=user, Group=user, Environment='HOME=/home/'+user)
            def run(*args):
                calls.append(args)
                if args[1] == 'show': return ''
                elif len(args) > 2 and args[2] in m.UNITS: states[m.UNITS.index(args[2])] = 'inactive' if args[1] == 'stop' else 'active'
            def account(user): return SimpleNamespace(pw_uid=1001, pw_gid=1002, pw_dir='/home/'+user)
            with patch.object(m, 'DIRECTORY', directory), patch.object(m, 'MARKER', marker), patch.object(m, 'manager', manager), patch.object(m, 'run', run), patch.object(m.pwd, 'getpwnam', account), patch.object(m.grp, 'getgrgid', return_value=SimpleNamespace(gr_name='old')):
                with patch.object(m.authority, 'load', side_effect=FileNotFoundError):
                    recipe = m.execute('identity-observe', 'source', 'old', 'new', manifest)
                journal = dict(phase='prepared',plan={})
                with patch.object(m.authority, 'load', return_value=(journal, recipe)):
                    m.execute('identity-apply', 'source', 'old', 'new', manifest)
                    self.assertEqual(states, ['inactive']*3)
                    journal['phase'] = 'account-ready'
                    publish = m.publish; counter = 0
                    def cut(path, old, new):
                        nonlocal counter
                        publish(path, old, new); counter += 1
                        if counter == 1: raise RuntimeError('injected cut')
                    with patch.object(m, 'publish', cut), self.assertRaises(RuntimeError): m.execute('identity-apply', 'source', 'old', 'new', manifest)
                    m.execute('identity-apply', 'source', 'old', 'new', manifest)
                    manifest['observation']['actual'] = 'new'
                    m.execute('identity-apply', 'source', 'old', 'new', manifest)
                    self.assertEqual(states, ['active' if full else 'inactive', 'inactive', 'active'])
                    self.assertEqual((directory/m.UNITS[0]).exists(),full)
                    self.assertEqual(marker.exists(),full)
                    self.assertEqual(enabled, ['enabled', 'static', 'enabled'])
                    self.assertEqual(retained.read_text(), 'retained'); self.assertEqual(retained.stat().st_ino, inode)
                    self.assertTrue(all(call[1] in ('show', 'stop', 'start', 'daemon-reload') for call in calls))
                    write(asset, b'foreign\n'); prior = list(calls)
                    with self.assertRaises(ValueError): m.execute('identity-apply', 'source', 'old', 'new', manifest)
                    self.assertEqual(calls, prior)

    def test_type_wide_container_dropin_is_accepted_and_unit_dropin_refused(self):
        fields = 'LoadState=loaded\nActiveState=inactive\nUnitFileState=static\nFragmentPath=/units/%s\nDropInPaths=%s\n'
        with patch.object(m, 'DIRECTORY', Path('/units')):
            with patch.object(m, 'run', return_value=fields % (m.UNITS[1], '/run/systemd/system/service.d/zzz-lxc-service.conf')):
                self.assertEqual(m.manager(m.UNITS[1])['ActiveState'], 'inactive')
            with patch.object(m, 'run', return_value=fields % (m.UNITS[1], '/etc/systemd/system/%s.d/override.conf' % m.UNITS[1])), self.assertRaises(ValueError):
                m.manager(m.UNITS[1])

    def test_discovery_run_in_progress_is_not_a_refusal(self):
        fields = 'LoadState=loaded\nActiveState=activating\nUnitFileState=static\nFragmentPath=/units/%s\nDropInPaths=\n'
        with patch.object(m, 'DIRECTORY', Path('/units')):
            with patch.object(m, 'run', return_value=fields % m.UNITS[1]):
                self.assertEqual(m.manager(m.UNITS[1])['ActiveState'], 'inactive')
            with patch.object(m, 'run', return_value=fields % m.UNITS[0]), self.assertRaises(ValueError):
                m.manager(m.UNITS[0])

    def test_service_waiting_to_restart_is_failed(self):
        fields = 'LoadState=loaded\nActiveState=activating\nSubState=auto-restart\nUnitFileState=enabled\nFragmentPath=/units/%s\nDropInPaths=\n'
        with patch.object(m, 'DIRECTORY', Path('/units')), patch.object(m, 'run', return_value=fields % m.UNITS[0]):
            self.assertEqual(m.manager(m.UNITS[0])['ActiveState'], 'failed')

    def test_stopping_a_failed_unit_clears_its_failed_state(self):
        state = {'value': 'failed'}; calls = []
        def run(*args):
            calls.append(args[1])
            if args[1] == 'reset-failed': state['value'] = 'inactive'
        with patch.object(m, 'run', run), patch.object(m, 'manager', lambda name: dict(ActiveState=state['value'])):
            m.stop(m.UNITS[1])
            self.assertEqual((calls, state['value']), (['stop', 'reset-failed'], 'inactive'))
            m.stop(m.UNITS[1])
            self.assertEqual(calls[2:], ['stop'])

    def test_agent_inside_the_service_refuses(self):
        pid = str(os.getpid())
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            root = Path(temporary); (root/'service').mkdir(); (root/'service/cgroup.procs').write_text(pid+'\n')
            def run(*args): return '/service\n' if args[2] == m.UNITS[0] else ''
            with patch.object(m, 'CGROUP', root), patch.object(m, 'run', run), patch.object(m, 'hosts_agent', return_value=True):
                with self.assertRaisesRegex(ValueError, 'agent terminals'): m.refuse_running_work(True)
                m.refuse_running_work(False)

    def test_older_main_unit_names_the_repair(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            root = Path(temporary); directory = root/'units'; directory.mkdir(); marker = root/'contract'
            for name, data in zip(m.UNITS, ['older\n', 'User=old\n', 'timer\n']):
                (directory/name).write_text(data); (directory/name).chmod(0o644)
            marker.write_text('old\n'); marker.chmod(0o644)
            manifest = dict(full=True, before=['current\n', 'User=old\n'], after=['new\n', 'User=new\n'], files=[], version='7:source', oldDigest='old', newDigest='new', observation=dict(state='current', actual='old'))
            account = SimpleNamespace(pw_uid=1001, pw_gid=1002, pw_dir='/home/old')
            with patch.object(m, 'DIRECTORY', directory), patch.object(m, 'MARKER', marker), patch.object(m, 'refuse_running_work'), patch.object(m.pwd, 'getpwnam', return_value=account), patch.object(m.grp, 'getgrgid', return_value=SimpleNamespace(gr_name='old')), patch.object(m.authority, 'load', side_effect=FileNotFoundError):
                with self.assertRaisesRegex(ValueError, 'run yard orca up'): m.execute('identity-observe', 'source', 'old', 'new', manifest)


if __name__ == '__main__': unittest.main()
