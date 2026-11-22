#!/usr/bin/env python3
"""rollback-apply prepares runtime format 1, which the unmodified v0.30 guest runtime reads."""
import json
import os
from pathlib import Path
import shutil
import sys
import tarfile
import tempfile
import types
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
sys.path.insert(0, str(Path(__file__).resolve().parents[4]/'internal/adapters/configmaterial'))
import guest

FIXTURES = Path(__file__).with_name('fixtures')
LIBRARY = ('native.py', 'launcher.py', 'processes.py', 'records.py', 'forward.py', 'resources.py', 'sources.py',
           'broker-correlation.py', 'tomli_w.py')


def fixture_source(archive, name):
    with tarfile.open(FIXTURES/archive) as files:
        return files.extractfile('config/profiles/telemetry/'+name).read()


def load_v030():
    """The v0.30 guest runtime and its writer, as modules beside this release's."""
    native = types.ModuleType('v030_native')
    exec(compile(fixture_source('v0.30.0-native.py.tar.gz', 'native.py'), 'v0.30.0/native.py', 'exec'), native.__dict__)
    module = types.ModuleType('v030_guest')
    current = sys.modules['native']
    sys.modules['native'] = native
    try:
        exec(compile(fixture_source('v0.30.0-guest.py.tar.gz', 'guest.py'), 'v0.30.0/guest.py', 'exec'), module.__dict__)
    finally:
        sys.modules['native'] = current
    return module


class Interrupted(Exception):
    pass


class Systemd:
    """Units load their file at daemon-reload; show reports the loaded definition."""
    def __init__(self, units, gid):
        self.units, self.gid = units, gid
        self.loaded, self.state, self.calls = {}, {}, []

    def __call__(self, *args):
        self.calls.append(args)
        action, names = args[0], [name for name in args[1:] if not name.startswith('--')]
        if action == 'show':
            unit = names[0]
            if unit not in self.loaded:
                return 'LoadState=not-found'
            values = dict(line.split('=', 1) for line in self.loaded[unit].decode().splitlines() if '=' in line)
            command = values['ExecStart']
            path = self.units/unit
            fields = dict(LoadState='loaded', FragmentPath=str(path), User=values['User'], Group=values['Group'], Type='simple',
                          UMask='0077', LimitCORE='0', NoNewPrivileges='yes', DropInPaths='', KillMode=values['KillMode'],
                          ExecStart='{ path='+command.split()[0]+' ; argv[]='+command+' ; ignore_errors=no ; start_time=x ; }',
                          Delegate='yes', MemoryMax=str(256*1024*1024), TasksMax='64', CPUQuotaPerSecUSec='250ms',
                          NeedDaemonReload='no' if path.exists() and path.read_bytes() == self.loaded[unit] else 'yes',
                          ActiveState='inactive', UnitFileState='disabled')
            fields.update(self.state.get(unit, {}))
            return '\n'.join(f'{key}={value}' for key, value in fields.items())
        if action == 'daemon-reload':
            for unit in (guest.COLLECTOR, guest.MANAGER):
                path = self.units/unit
                if path.exists():
                    self.loaded[unit] = path.read_bytes()
                else:
                    self.loaded.pop(unit, None)
        for unit in names:
            state = self.state.setdefault(unit, {})
            if action in ('enable', 'disable'):
                state['UnitFileState'] = action+'d'
            if action == 'start' or action == 'enable' and '--now' in args:
                state['ActiveState'] = 'active'
            if action == 'stop' or action == 'disable' and '--now' in args:
                state['ActiveState'] = 'inactive'
        return ''


@unittest.skipIf(os.getuid() == 0, 'the fixture installs as the developer account')
class RollbackTest(unittest.TestCase):
    def setUp(self):
        self.v030 = load_v030()
        scratch = tempfile.TemporaryDirectory(dir='/tmp')
        self.addCleanup(scratch.cleanup)
        self.root = root = Path(scratch.name)
        self.source, self.legacy_source = root/'source', root/'legacy-source'
        for directory in (self.source, self.legacy_source, root/'home', root/'units', root/'bin', root/'etc'):
            directory.mkdir()
        for path in Path(guest.__file__).parent.glob('*.py'):
            shutil.copyfile(path, self.source/path.name)
        shutil.copyfile(Path(guest.tomli_w.__file__), self.source/'tomli_w.py')
        (self.source/'subyard-telemetry').write_bytes(b'\x7fELF collector')
        # The retained release ships its own assets; only their delivery matters here.
        for name in LIBRARY:
            (self.legacy_source/name).write_bytes(b'# v0.30 '+name.encode()+b'\n')
        (self.legacy_source/'native.py').write_bytes(fixture_source('v0.30.0-native.py.tar.gz', 'native.py'))
        self.account = types.SimpleNamespace(pw_uid=os.getuid(), pw_gid=os.getgid(), pw_dir=str(root/'home'))
        self.systemd = Systemd(root/'units', self.account.pw_gid)
        registered = {'skill': False}

        def skill_delivery(_runtime, mode):
            if mode in ('apply', 'remove'):
                registered['skill'] = mode == 'apply'
            return registered['skill']
        for module in (guest, self.v030):
            for target in (patch.object(module, 'systemctl', side_effect=self.systemd),
                           patch.object(module.pwd, 'getpwnam', return_value=self.account),
                           patch.object(module.Runtime, 'running_no_new_privs', lambda _runtime, _fields: True)):
                target.start()
                self.addCleanup(target.stop)
        for target in (patch.object(guest.Runtime, 'prepare_store'), patch.object(guest.Runtime, 'policy_status', return_value='present'),
                       patch.object(guest.records, 'sanitize_state', side_effect=lambda _family, value, _binary: value),
                       patch.object(guest.Runtime, 'skill_delivery', skill_delivery)):
            target.start()
            self.addCleanup(target.stop)

    def runtime(self, selected):
        root = self.root
        return guest.Runtime(self.source, 'dev', selected, ['codex'], root/'control', root/'data', root/'library',
                             root/'units', root/'bin', etc=root/'etc')

    def legacy(self, selected):
        root = self.root
        source = self.legacy_source
        if selected:
            source = self.root/'legacy-selected'
            if not source.exists():
                shutil.copytree(self.legacy_source, source)
                (source/'subyard-telemetry').write_bytes(b'\x7fELF v0.30 collector')
        return self.v030.Runtime(source, 'dev', selected, ['codex'], root/'control', root/'data', root/'library',
                                 root/'units', root/'bin', account=self.account)

    def install_disabled(self):
        """Telemetry used and then disabled: this release's units stay installed."""
        self.runtime(True).apply()
        self.assertEqual(self.runtime(True).observe()['state'], 'current')
        self.runtime(False).apply()
        self.assertEqual(self.runtime(False).observe()['state'], 'current')
        self.assertTrue((self.root/'units'/guest.COLLECTOR).exists())
        self.assertTrue((self.root/'data-private/auth.token').exists())

    def test_format_one_units_are_the_v030_units(self):
        for selected in (False, True):
            with self.subTest(selected=selected):
                for path, content in self.runtime(selected).rollback_units.items():
                    self.assertEqual(content, self.legacy(selected).assets[path][0])
        self.assertNotEqual(self.runtime(False).rollback_units, {path: self.runtime(False).assets[path][0]
                                                                 for path in self.runtime(False).rollback_units})

    def test_disabled_runtime_is_prepared_for_and_accepted_by_v030(self):
        self.install_disabled()
        token = (self.root/'data-private/auth.token').read_bytes()
        # Without preparation v0.30 refuses this release's loaded collector unit.
        with self.assertRaisesRegex(ValueError, 'foreign loaded unit envelope'):
            self.legacy(False).observe()
        off = self.runtime(False)
        observed = off.rollback_observe()
        self.assertEqual(observed['state'], 'stale')
        self.assertEqual(observed['consequence'], guest.ROLLBACK_CONSEQUENCE)
        self.assertEqual({item['parameter'] for item in observed['diagnostics']},
                         {'telemetry.rollback.unit.'+guest.COLLECTOR, 'telemetry.rollback.loaded.'+guest.COLLECTOR,
                          'telemetry.rollback.token'})
        self.assertNotIn(token.strip().decode(), json.dumps(observed))
        off.rollback_apply()
        prepared = off.rollback_observe()
        self.assertEqual(prepared, {'state': 'current', 'actual': observed['desired'], 'desired': observed['desired']})
        self.assertEqual((self.root/'data/auth.token').read_bytes(), token)
        self.assertEqual((self.root/'data/auth.token').stat().st_mode & 0o777, 0o600)
        self.assertFalse((self.root/'data-private/auth.token').exists())
        # Idempotent: a repeated conversion changes nothing.
        calls = len(self.systemd.calls)
        off.rollback_apply()
        self.assertFalse([call for call in self.systemd.calls[calls:] if call[0] != 'show'])
        # This release now reports drift, and its own apply would undo the preparation.
        self.assertEqual(off.observe()['state'], 'stale')
        # The enabled check refuses before any read of the converted parts.
        with self.assertRaisesRegex(guest.native.Refusal, 'telemetry is enabled'):
            self.runtime(True).rollback_observe()
        # The unmodified v0.30 runtime accepts the state and converges it.
        legacy = self.legacy(False)
        self.assertEqual(legacy.observe()['state'], 'stale')
        legacy.apply()
        self.assertEqual(legacy.observe()['state'], 'current')
        self.assertEqual((self.root/'data/auth.token').read_bytes(), token)
        # The forward route from v0.30, yard update --version, converges this release again.
        off.apply()
        self.assertEqual(off.observe()['state'], 'current')
        self.assertEqual((self.root/'data-private/auth.token').read_bytes(), token)
        self.assertFalse((self.root/'data/auth.token').exists())

    def test_interrupted_preparation_resumes_or_reverts_from_every_write(self):
        effects = 0
        stop = 0
        publish, remove = guest.native.publish, guest.Runtime.remove_auth

        def effect(function):
            def run(*args, **kwargs):
                nonlocal effects
                result = function(*args, **kwargs)
                effects += 1
                if effects == stop:
                    raise Interrupted
                return result
            return run

        def reload(*args):
            return effect(self.systemd)(*args) if args == ('daemon-reload',) else self.systemd(*args)
        self.install_disabled()
        token = (self.root/'data-private/auth.token').read_bytes()
        snapshot = self.root.with_name(self.root.name+'-installed')
        shutil.copytree(self.root, snapshot, symlinks=True)
        self.addCleanup(shutil.rmtree, snapshot)
        loaded = dict(self.systemd.loaded)
        state = json.loads(json.dumps(self.systemd.state))
        completed = False
        while not completed:
            stop += 1
            for resume in (True, False):
                with self.subTest(stop=stop, resume=resume):
                    shutil.rmtree(self.root)
                    shutil.copytree(snapshot, self.root, symlinks=True)
                    self.systemd.loaded, self.systemd.state = dict(loaded), json.loads(json.dumps(state))
                    effects = 0
                    off = self.runtime(False)
                    try:
                        with patch.object(guest.native, 'publish', effect(publish)), \
                             patch.object(guest.Runtime, 'remove_auth', effect(remove)), \
                             patch.object(guest, 'systemctl', side_effect=reload):
                            off.rollback_apply()
                        completed = True
                    except Interrupted:
                        # Both observers name the interrupted state; neither refuses it.
                        self.assertEqual(off.observe()['state'], 'stale')
                        observed = off.rollback_observe()
                        self.assertEqual(observed['state'], 'current' if stop == 6 else 'stale')
                    if resume:
                        off.rollback_apply()
                        self.assertEqual(off.rollback_observe()['state'], 'current')
                        self.assertEqual(self.legacy(False).observe()['state'], 'stale')
                    else:
                        # yard migrate abandons the preparation from here.
                        off.apply()
                        self.assertEqual(off.observe()['state'], 'current')
                        self.assertEqual((self.root/'data-private/auth.token').read_bytes(), token)
                    if completed:
                        break
        # Token placement, its removal, both inventory records, the unit and daemon-reload.
        self.assertEqual(stop, 7)

    def test_unconverted_drift_and_foreign_state_refuse_preparation(self):
        self.install_disabled()
        off = self.runtime(False)
        records = self.root/'library/records.py'
        original = records.read_bytes()
        records.write_bytes(b'unmanaged edit')
        with self.assertRaisesRegex(guest.native.Refusal, 'run yard migrate'):
            off.rollback_observe()
        records.write_bytes(original)
        unit = self.root/'units'/guest.COLLECTOR
        current = unit.read_bytes()
        unit.write_bytes(current.replace(b'MemoryMax=256M', b'MemoryMax=512M'))
        with self.assertRaisesRegex(guest.native.Refusal, 'run yard migrate'):
            off.rollback_observe()
        unit.write_bytes(current)
        self.systemd.state[guest.COLLECTOR]['ActiveState'] = 'active'
        with self.assertRaisesRegex(guest.native.Refusal, 'run yard migrate'):
            off.rollback_observe()
        self.systemd.state[guest.COLLECTOR]['ActiveState'] = 'inactive'
        (self.root/'data/auth.token').write_text('f'*64+'\n')
        (self.root/'data/auth.token').chmod(0o600)
        with self.assertRaisesRegex(guest.native.Refusal, 'run yard migrate'):
            off.rollback_observe()
        (self.root/'data/auth.token').unlink()
        self.systemd.state[guest.COLLECTOR]['Environment'] = 'PRIVATE=1'
        with self.assertRaisesRegex(ValueError, 'foreign loaded unit envelope'):
            off.rollback_observe()
        del self.systemd.state[guest.COLLECTOR]['Environment']
        self.assertEqual(off.rollback_observe()['state'], 'stale')

    def test_uninstalled_runtime_needs_no_preparation(self):
        for selected in (False, True):
            self.assertEqual(self.runtime(selected).rollback_observe(), {'state': 'absent', 'actual': '', 'desired': ''})

    def test_guest_entry_binds_the_observation_and_reports_the_result(self):
        self.install_disabled()
        runtime = self.runtime(False)
        observed = runtime.rollback_observe()
        def main(*bindings):
            # Only the entry's guest root check sees root.
            output = []
            arguments = ['guest.py', 'rollback-apply', '0', 'dev', 'codex', *bindings]
            with patch.object(guest.os, 'getuid', side_effect=[0]+[os.getuid()]*64), patch.object(sys, 'argv', arguments), \
                 patch.object(guest, 'Runtime', lambda *_arguments: runtime), \
                 patch('builtins.print', side_effect=output.append):
                guest.main()
            return json.loads(output[0])
        with self.assertRaisesRegex(ValueError, 'telemetry observation changed'):
            main(observed['desired'], observed['desired'])
        self.assertEqual(self.runtime(False).rollback_observe(), observed)
        self.assertEqual(main(observed['actual'], observed['desired'])['state'], 'current')


if __name__ == '__main__':
    unittest.main()
