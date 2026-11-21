#!/usr/bin/env python3
import errno
import json
import hashlib
import importlib.util
import io
from contextlib import ExitStack, closing, redirect_stderr
import os
import re
from pathlib import Path
import select
import shutil
import signal
import socket
import time
import sqlite3
import stat
import subprocess
import sys
import tarfile
import tempfile
import types
import unittest
from unittest.mock import patch
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
sys.path.insert(0, str(Path(__file__).resolve().parents[4]/'internal/adapters/configmaterial'))
import guest
import records
spec = importlib.util.spec_from_file_location('profile_lifecycle', Path(__file__).parent/'e2e/profile-lifecycle.py')
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)


class LifecycleTest(unittest.TestCase):
    def test_disk_budget_reaches_the_collector_unit(self):
        profile = Path(guest.__file__).parent
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            source = Path(name)
            shutil.copytree(profile, source, dirs_exist_ok=True, ignore=shutil.ignore_patterns('tests', 'collector', 'cmd', 'bin', '__pycache__'))
            shutil.copyfile(guest.tomli_w.__file__, source/'tomli_w.py')
            shutil.copyfile(guest.safefs.__file__, source/'safefs.py')
            account = types.SimpleNamespace(pw_uid=1000, pw_gid=1000, pw_dir=name)
            for budget, flag in (('2147483648', ' --disk-bytes 2147483648'), ('', ''), ('2G', '')):
                with patch.dict(os.environ, SUBYARD_TELEMETRY_DISK_BYTES=budget):
                    unit = guest.Runtime(source, 'dev', False, [], account=account).assets[Path('/etc/systemd/system')/guest.COLLECTOR][0].decode()
                self.assertIn('--auth-file /var/lib/subyard/telemetry-private/auth.token'+flag+'\n', unit)

    def test_recorded_collector_unit_with_any_startup_is_stale_not_foreign(self):
        profile = Path(guest.__file__).parent
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            source, units = root/'source', root/'units'
            shutil.copytree(profile, source, ignore=shutil.ignore_patterns('tests', 'collector', 'cmd', 'bin', '__pycache__'))
            shutil.copyfile(guest.tomli_w.__file__, source/'tomli_w.py')
            shutil.copyfile(guest.safefs.__file__, source/'safefs.py')
            (source/'subyard-telemetry').write_bytes(b'\x7fELF')
            for directory in ('control', 'units'):
                (root/directory).mkdir()
            account = types.SimpleNamespace(pw_uid=os.getuid() or 1000, pw_gid=os.getgid(), pw_dir=name)
            path, inventory = units/guest.COLLECTOR, root/'control/assets.json'
            with patch.dict(os.environ, SUBYARD_TELEMETRY_DISK_BYTES='2147483648'):
                runtime = guest.Runtime(source, 'dev', True, ['codex'], root/'control', root/'data', root/'library', units, root/'bin',
                                        account=account, etc=root/'etc')
            desired = runtime.starts[guest.COLLECTOR]
            current = runtime.assets[path][0]
            v030 = desired.removesuffix(' --disk-bytes 2147483648').replace(str(runtime.auth), str(runtime.data/'auth.token'))
            # Previous and future releases may start the collector with any arguments and environment.
            for command, environment in ((v030, ''), (desired.replace('2147483648', '1073741824'), ''),
                                         (desired.replace(str(runtime.auth), '/var/lib/subyard/other/second.token'), ''),
                                         (desired+' --future-flag --budget-file /etc/x', ''), (desired, 'GOMEMLIMIT=200MiB'),
                                         (desired.removeprefix('/usr/bin/setpriv --no-new-privs '), '')):
                with self.subTest(command=command, environment=environment):
                    unit = current.replace(('ExecStart='+desired).encode(), ('ExecStart='+command).encode(), 1)
                    if environment:
                        unit = unit.replace(b'ExecStart=', ('Environment='+environment+'\nExecStart=').encode(), 1)
                    path.write_bytes(unit)
                    fields = dict(LoadState='loaded', FragmentPath=str(path), User='dev', Group=str(account.pw_gid), Type='simple',
                                  UMask='0077', LimitCORE='0', NoNewPrivileges='yes', DropInPaths='', KillMode='control-group',
                                  MemoryMax=str(256*1024*1024), TasksMax='64', CPUQuotaPerSecUSec='250ms', ActiveState='active',
                                  ExecStart='{ path='+command.split()[0]+' ; argv[]='+command+' ; ignore_errors=no ; start_time=x ; }',
                                  Environment=environment, NeedDaemonReload='no')
                    def show(fields=fields):
                        return '\n'.join(f'{key}={value}' for key, value in fields.items())
                    def refused():
                        with patch.object(guest, 'systemctl', side_effect=lambda *args: show()), \
                             self.assertRaisesRegex(ValueError, 'foreign loaded unit envelope'):
                            runtime.manager(guest.COLLECTOR)
                    recorded = {str(path): hashlib.sha256(unit).hexdigest()}
                    inventory.write_text(json.dumps(recorded))
                    with patch.object(guest, 'systemctl', side_effect=lambda *args: show()):
                        self.assertEqual(runtime.manager(guest.COLLECTOR)['ExecStart'], fields['ExecStart'])
                    # The inventory proves ownership; an unrecorded file or a drop-in is foreign.
                    inventory.write_text(json.dumps({str(path): hashlib.sha256(b'other').hexdigest()}))
                    refused()
                    inventory.write_text(json.dumps(recorded))
                    for changed in ({'DropInPaths': '/etc/systemd/system/'+guest.COLLECTOR+'.d/foreign.conf'},
                                    {'FragmentPath': '/etc/systemd/system/'+guest.COLLECTOR}, {'NeedDaemonReload': 'yes'}):
                        with self.subTest(changed=changed):
                            refused_fields = {**fields, **changed}
                            with patch.object(guest, 'systemctl', return_value=show(refused_fields)), self.assertRaises(ValueError):
                                runtime.manager(guest.COLLECTOR)
            # A recorded unit's own fields are ours too: a later release may change any of them.
            for changed in ({'User': 'other'}, {'Group': '0'}, {'KillMode': 'process'}, {'MemoryMax': '536870912'}, {'TasksMax': '128'},
                            {'CPUQuotaPerSecUSec': '500ms'}, {'Type': 'notify'}, {'UMask': '0027'}, {'NoNewPrivileges': 'no'},
                            {'ExecStartPre': '{ path=/bin/true ; argv[]=/bin/true ; ignore_errors=no ; }'}):
                with self.subTest(owned=changed), patch.object(guest, 'systemctl', return_value=show({**fields, **changed})):
                    self.assertEqual(runtime.manager(guest.COLLECTOR)['ExecStart'], fields['ExecStart'])
            # Also when the startup is this release's and only such a field differs.
            unit = current.replace(b'MemoryMax=256M', b'MemoryMax=512M', 1)
            path.write_bytes(unit)
            inventory.write_text(json.dumps({str(path): hashlib.sha256(unit).hexdigest()}))
            fields.update(ExecStart='{ path=/usr/bin/setpriv ; argv[]='+desired+' ; ignore_errors=no ; }', Environment='', MemoryMax='536870912')
            with patch.object(guest, 'systemctl', return_value=show(fields)):
                self.assertEqual(runtime.manager(guest.COLLECTOR)['MemoryMax'], '536870912')
            # Stopping the manager must leave its delegated workloads running.
            with patch.object(guest, 'systemctl', return_value=show({**fields, 'FragmentPath': str(units/guest.MANAGER)})), \
                 self.assertRaisesRegex(ValueError, 'foreign loaded unit stop policy'):
                runtime.manager(guest.MANAGER)
            # A recorded unit may carry only the stock LXC guest's type-wide drop-in.
            service = root/'service.d'
            service.mkdir(mode=0o755)
            service.chmod(0o755)
            dropin = service/'zzz-lxc-service.conf'
            def write_dropin(content, mode=0o644):
                dropin.unlink(missing_ok=True)
                dropin.write_bytes(content)
                dropin.chmod(mode)
                # Older than the replaced unit file, as loaded before it.
                os.utime(dropin, ns=(path.stat().st_mtime_ns-10**9,)*2, follow_symlinks=False)
            for state, loaded, saved in (
                    ('previous file loaded', unit, {str(path): hashlib.sha256(unit).hexdigest()}),
                    ('replaced file loaded', current, {str(path): hashlib.sha256(current).hexdigest(),
                                                      guest.REPLACED: {str(path): hashlib.sha256(unit).hexdigest()}})):
                path.write_bytes(loaded)
                inventory.write_text(json.dumps(saved))
                stale = {**fields, 'DropInPaths': str(dropin), 'NoNewPrivileges': 'no',
                         'NeedDaemonReload': 'no' if loaded == unit else 'yes'}
                with patch.object(guest, 'TYPE_DROPINS', (service,)), patch.object(guest, 'systemctl', return_value=show(stale)):
                    stock = b'[Service]\nProcSubset=all\nProtectProc=default\nProtectControlGroups=no\nProtectKernelTunables=no\n' \
                            b'NoNewPrivileges=no\nLoadCredential=\nPrivateNetwork=no\nImportCredential=\n'
                    for content in (stock, stock.removesuffix(b'\n'), b'[Service]\nProcSubset=all\n'):
                        write_dropin(content)
                        with self.subTest(state=state, dropin=content):
                            self.assertEqual(runtime.manager(guest.COLLECTOR)['MemoryMax'], '536870912')
                    # Only the stock lines, exactly: other values, keys, spellings and systemd syntax are refused.
                    for content in (b'[Service]\nLoadCredential=x:/etc/shadow\n', b'[Service]\nImportCredential=*\n',
                                    b'[Service]\nPrivateNetwork=yes\n', b'ProcSubset=all\n', b'', b'[Unit]\nProcSubset=all\n',
                                    b'[Service]\n\nProcSubset=all\n', b'[Service]\n# Generated\nProcSubset=all\n',
                                    b'[Service]\nProtectProc = default\n', b'[Service]\nProcSubset=all\n\n', b'[Service]\r\nProcSubset=all\r\n',
                                    b'[Service]\nEnvironment=GODEBUG=x\n', b'[Service]\nEnvironmentFile=-/etc/default/x\n',b'[Service]\nEnvironment=GODEBUG=x\n', b'[Service]\nEnvironmentFile=-/etc/default/x\n',
                                    b'[Service]\nUser=root\n', b'[Service]\nGroup=root\n', b'[Service]\nExecStart=\nExecStart=/bin/sh\n',
                                    b'[Service]\nExecStartPre=/bin/true\n', b'[Service]\n  Environment = A=1\n',
                                    b'[Service]\nEnvironment\\\n=A=1\n', b'[Service]\nProcSubset=all\n# note \\\nEnvironment=A=1\n',
                                    b'[Service]\nProcSubset=all\\\\\n', b'[Service]\nProcSubset=all\0Environment=A=1\n',
                                    b'[Service]\nLoadCredential=a \\\nProcSubset=all\n',
                                    b'[Service]\nProcSubset=all\rEnvironment=A=1\n', b'[Service]\nAmbientCapabilities=CAP_SYS_ADMIN\n',
                                    b'[Service]\nSupplementaryGroups=root\n', b'[Service]\nPassEnvironment=LD_PRELOAD\n',
                                    b'[Service]\nBindReadOnlyPaths=/tmp/x:/usr/local/lib\n', b'[Service]\nDynamicUser=yes\n',
                                    b'[Service]\nRootDirectory=/tmp/x\n', b'.include /tmp/x\n'):
                        write_dropin(content)
                        with self.subTest(state=state, dropin=content), self.assertRaisesRegex(ValueError, 'foreign loaded unit drop-in'):
                            runtime.manager(guest.COLLECTOR)
                    # A drop-in that cannot be read safely may hide any of these.
                    lxc = b'[Service]\nProcSubset=all\n'
                    for case in ('missing', 'writable', 'symlink', 'oversized', 'binary', 'writable directory'):
                        write_dropin(lxc)
                        if case == 'missing':
                            dropin.unlink()
                        elif case == 'writable':
                            dropin.chmod(0o666)
                        elif case == 'symlink':
                            dropin.unlink()
                            (root/'lxc.conf').write_bytes(lxc)
                            dropin.symlink_to(root/'lxc.conf')
                        elif case == 'oversized':
                            # Stock lines fill the read bound exactly; the rest is never read.
                            short, long = b'ProcSubset=all\n', b'NoNewPrivileges=no\n'
                            size = guest.native.CONFIG_LIMIT+1-len(b'[Service]\n')
                            count = next(count for count in range(len(short)) if (size-count*len(long)) % len(short) == 0)
                            write_dropin(b'[Service]\n'+long*count+short*((size-count*len(long))//len(short))+b'Environment=A=1\n')
                        elif case == 'binary':
                            write_dropin(lxc+b'\xff\n')
                        else:
                            service.chmod(0o775)
                        with self.subTest(state=state, unreadable=case), self.assertRaisesRegex(ValueError, 'foreign loaded unit drop-in'):
                            runtime.manager(guest.COLLECTOR)
                        service.chmod(0o755)
                        dropin.unlink(missing_ok=True)
                    # Until daemon-reload, a drop-in changed after the unit file was replaced is not the loaded one.
                    write_dropin(lxc)
                    os.utime(dropin, ns=(path.stat().st_mtime_ns,)*2)
                    with self.subTest(state=state, dropin='changed since load'):
                        if loaded == unit:
                            runtime.manager(guest.COLLECTOR)
                        else:
                            with self.assertRaisesRegex(ValueError, 'foreign loaded unit drop-in'):
                                runtime.manager(guest.COLLECTOR)
            # Over the current file, another loaded startup or environment comes from a drop-in.
            path.write_bytes(current)
            inventory.write_text(json.dumps({str(path): hashlib.sha256(current).hexdigest()}))
            fields.update(ExecStart='{ path=/bin/false ; argv[]=/bin/false ; ignore_errors=no ; }', Environment='', NeedDaemonReload='no',
                          MemoryMax=str(256*1024*1024))
            for changed in ({}, {'ExecStart': '{ path=/usr/bin/setpriv ; argv[]='+desired+' ; ignore_errors=no ; }', 'Environment': 'X=1'},
                            {'ExecStart': '{ path=/usr/bin/setpriv ; argv[]='+desired+' ; ignore_errors=no ; } '
                                          '{ path=/bin/false ; argv[]=/bin/false ; ignore_errors=no ; }'}):
                with self.subTest(over_current=changed), patch.object(guest, 'systemctl', return_value=show({**fields, **changed})), \
                     self.assertRaisesRegex(ValueError, 'foreign loaded unit envelope'):
                    runtime.manager(guest.COLLECTOR)
            # An apply interrupted before daemon-reload recorded both versions; the replaced one stays loaded.
            inventory.write_text(json.dumps({str(path): hashlib.sha256(current).hexdigest(), guest.REPLACED: {str(path): 'x'*64}}))
            with patch.object(guest, 'systemctl', return_value=show({**fields, 'NeedDaemonReload': 'yes'})):
                self.assertEqual(runtime.manager(guest.COLLECTOR)['ExecStart'], fields['ExecStart'])
            # A symlinked or missing unit file is not a regular unit this profile wrote.
            path.unlink()
            with patch.object(guest, 'systemctl', return_value=show({**fields, 'NeedDaemonReload': 'yes'})), \
                 self.assertRaisesRegex(ValueError, 'foreign loaded unit envelope'):
                runtime.manager(guest.COLLECTOR)
            (root/'elsewhere').write_bytes(current)
            path.symlink_to(root/'elsewhere')
            with patch.object(guest, 'systemctl', return_value=show({**fields, 'NeedDaemonReload': 'yes'})), self.assertRaises(ValueError):
                runtime.manager(guest.COLLECTOR)

    def test_stock_lxc_dropin_is_the_one_the_host_bwrap_policy_accepts(self):
        policy = (Path(guest.__file__).parents[3]/'internal/adapters/reconcileruntime/bwrap_policy.sh').read_text()
        header, = re.findall(r'NR == 1 \{ if \(\$0 != "(.*)"\) exit 1; next \}', policy)
        lines, = re.findall(r'!/\^\((.*)\)\$/ \{ exit 1 \}', policy)
        self.assertEqual(header, '[Service]')
        self.assertCountEqual(lines.split('|'), guest.LXC_DROPIN)

    def test_raw_refusal_is_read_from_a_private_marker_without_sqlite(self):
        profile = Path(guest.__file__).parent
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            source, data = Path(name), Path(name)/'store'
            shutil.copytree(profile, source, dirs_exist_ok=True, ignore=shutil.ignore_patterns('tests', 'collector', 'cmd', 'bin', '__pycache__'))
            shutil.copyfile(guest.tomli_w.__file__, source/'tomli_w.py')
            shutil.copyfile(guest.safefs.__file__, source/'safefs.py')
            account = types.SimpleNamespace(pw_uid=os.getuid() or 1000, pw_gid=os.getgid(), pw_dir=name)
            runtime = guest.Runtime(source, 'dev', False, [], data=data, account=account)
            marker = data/'raw-otlp-refused'
            with patch.object(guest.sqlite3, 'connect', side_effect=AssertionError('root opened SQLite')):
                self.assertFalse(runtime.raw_refused())
                data.mkdir()
                self.assertFalse(runtime.raw_refused())
                marker.write_bytes(b'raw_otlp_refused_since=2026-11-19T00:00:00.000000000Z\n')
                marker.chmod(0o600)
                self.assertTrue(runtime.raw_refused())
                marker.write_bytes(b'private-canary\n')
                self.assertFalse(runtime.raw_refused())
                for unsafe in ('mode', 'link', 'symlink'):
                    marker.unlink()
                    target = data/'target'
                    target.write_bytes(b'raw_otlp_refused_since=2026-11-19T00:00:00Z\n')
                    target.chmod(0o600 if unsafe != 'mode' else 0o644)
                    if unsafe == 'mode':
                        target.rename(marker)
                    elif unsafe == 'link':
                        os.link(target, marker)
                    else:
                        marker.symlink_to(target)
                    with self.subTest(unsafe=unsafe), self.assertRaises((ValueError, OSError)):
                        runtime.raw_refused()
                    target.unlink(missing_ok=True)

    def test_cutover_receipt_reads_the_validated_inode(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            runtime = object.__new__(guest.Runtime)
            runtime.data, runtime.uid, runtime.gid = Path(name), os.getuid(), os.getgid()
            database = runtime.data/'telemetry.sqlite'
            os.mkfifo(database, 0o600)
            with self.assertRaisesRegex(ValueError, 'foreign telemetry database'):
                runtime.cutover_receipt()
            database.unlink()
            database.write_bytes(bytes(60)+(4).to_bytes(4, 'big'))
            database.chmod(0o600)
            original = database.stat()
            pread = os.pread

            def replace_path(fd, size, offset):
                database.rename(runtime.data/'original.sqlite')
                database.write_bytes(bytes(60)+(5).to_bytes(4, 'big'))
                database.chmod(0o600)
                return pread(fd, size, offset)

            with patch.object(guest.os, 'pread', side_effect=replace_path):
                receipt = runtime.cutover_receipt()
            self.assertTrue(receipt)
            self.assertEqual((receipt['device'], receipt['inode']), (original.st_dev, original.st_ino))
            self.assertNotEqual(database.stat().st_ino, receipt['inode'])

    def test_existing_archive_inspection_reads_the_validated_inode(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            runtime = object.__new__(guest.Runtime)
            runtime.data, runtime.uid, runtime.gid = Path(name), os.getuid(), os.getgid()
            active = runtime.data/'telemetry.sqlite'
            active.write_bytes(bytes(60)+(4).to_bytes(4, 'big'))
            active.chmod(0o600)
            archive = runtime.data/'archive'
            archive.mkdir()
            archive.chmod(0o700)
            database = archive/'telemetry.sqlite'
            with closing(sqlite3.connect(database)) as db:
                db.execute('CREATE TABLE metadata(key TEXT, value TEXT)')
                db.execute("INSERT INTO metadata VALUES('origin_id','fixture-origin')")
                db.commit()
            database.chmod(0o400)
            copy = shutil.copyfileobj

            def replace_path(source, target):
                database.unlink()
                database.write_bytes(b'not a database')
                database.chmod(0o400)
                copy(source, target)

            with patch.object(guest.shutil, 'copyfileobj', side_effect=replace_path):
                receipt = runtime.cutover_receipt()
            self.assertTrue(receipt)
            self.assertEqual(receipt['archive'], 'archive-2')

    def test_installed_cutover_preserves_workloads_and_resumes(self):
        profile = Path(guest.__file__).parent
        with tempfile.TemporaryDirectory(dir='/tmp') as name, ExitStack() as children:
            root = Path(name)
            root.chmod(0o711)
            source = root/'source'
            shutil.copytree(profile, source, ignore=shutil.ignore_patterns('tests', 'collector', 'cmd', 'bin', '__pycache__'))
            shutil.copyfile(guest.tomli_w.__file__, source/'tomli_w.py')
            shutil.copyfile(guest.safefs.__file__, source/'safefs.py')
            binary = source/'subyard-telemetry'
            shutil.copyfile(records.collector_binary(), binary)
            binary.chmod(0o755)
            account = types.SimpleNamespace(pw_uid=os.getuid() or 1000, pw_gid=os.getgid(), pw_dir=str(root/'home'))
            runtime = guest.Runtime(source, 'dev', True, [], root/'control', root/'data',
                                    root/'library', root/'units', root/'bin', account)
            control, data = runtime.control, runtime.data
            for directory in (control, data, runtime.home, runtime.library, runtime.units, runtime.commands):
                directory.mkdir(mode=0o700 if directory == data else 0o755)
            os.chown(data, runtime.uid, runtime.gid)
            (control/'marker').write_bytes(guest.MARKER)
            launcher = runtime.library/'launcher.py'
            old_observer = b'''
import fcntl, os, pathlib, sys
with (pathlib.Path(sys.argv[1])/'manager.lock').open('rb') as lock:
    fcntl.flock(lock, fcntl.LOCK_EX)
    print('locked', flush=True)
    sys.stdin.read()
'''
            for path, (_, mode) in runtime.assets.items():
                path.write_bytes(old_observer if path == launcher else b'previous '+path.name.encode())
                path.chmod(mode)
            (control/'assets.json').write_text(json.dumps({
                str(path): hashlib.sha256(path.read_bytes()).hexdigest() for path in runtime.assets}))

            def dev(*command):
                if os.getuid() != runtime.uid:
                    return ['/usr/bin/setpriv', '--reuid', str(runtime.uid), '--regid', str(runtime.gid), '--clear-groups', *command]
                return command

            def child(*command):
                process = children.enter_context(subprocess.Popen(dev(*command), stdin=subprocess.PIPE, stdout=subprocess.PIPE))
                children.callback(lambda: process.kill() if process.poll() is None else None)
                return process

            subprocess.run(dev(binary, 'prepare', '--data-dir', data), check=True, timeout=10)
            initialization = '''
import contextlib, json, os, pathlib, sqlite3, sys
root = pathlib.Path(sys.argv[1])
(root/'spool-policy.json').unlink()
(root/'runs').mkdir(mode=0o700)
(root/'runs-v4').mkdir(mode=0o700)
(root/'outbox').mkdir(mode=0o700)
(root/'outbox/.lock').touch(mode=0o600)
(root/'manager.lock').touch(mode=0o600)
with contextlib.closing(sqlite3.connect(root/'telemetry.sqlite')) as db:
    db.execute("PRAGMA user_version=4")
    db.execute('DROP TABLE runs')
    db.execute('CREATE TABLE runs(agent_run_id TEXT PRIMARY KEY, body TEXT, source_time TEXT)')
    db.execute("INSERT INTO runs VALUES(?,?,'')", ('legacy-run', json.dumps({'state': 'running'})))
    db.commit()
for path, value in (
    ('runs/legacy-run.json', dict(agent_run_id='legacy-run', pid=2147483647,
        boot_id=pathlib.Path('/proc/sys/kernel/random/boot_id').read_text().strip())),
    ('outbox/.valid.json', dict(schema_version=1, event_id='.valid', source_id='old-producer',
         source_version='1', kind='gap', body=dict(reason='source_offline'))),
    ('outbox/.foreign.json', dict(schema_version=1, event_id='wrong-name', source_id='old-producer',
         source_version='1', kind='gap', body=dict(reason='source_offline'))),
):
    target = root/path
    target.write_text(json.dumps(value))
    target.chmod(0o600)
(root/'producer-health.json').touch(mode=0)
(root/'sources').mkdir(mode=0o700)
(root/'sources/observer-old.json').write_text('historical operational state')
(root/'auth.token').write_text('a'*64+chr(10))
(root/'auth.token').chmod(0o600)
'''
            subprocess.run(dev(sys.executable, '-Bc', initialization, data), check=True, timeout=5)
            children.callback(lambda: runtime.as_developer(lambda: ((data/'runs').chmod(0o700), (data/'runs-v4').chmod(0o700), (data/'outbox').chmod(0o700))))
            observer = child(sys.executable, '-B', launcher, data)
            self.assertTrue(select.select([observer.stdout], [], [], 5)[0])
            self.assertEqual(observer.stdout.readline(), b'locked\n')
            serializer = '''
import fcntl, os, pathlib, stat, sys
outbox = pathlib.Path(sys.argv[1])/'outbox'
with (outbox/'.lock').open('r+b') as lock:
    assert stat.S_IMODE(outbox.stat().st_mode) == 0o700
    print('loaded', flush=True)
    sys.stdin.read()
    fcntl.flock(lock, fcntl.LOCK_EX)
    try:
        mode = stat.S_IMODE(outbox.stat().st_mode)
        if mode in ((0o700,) if sys.argv[2]=='released' else (0o700,0o1700)):
            (outbox/(sys.argv[2]+'.json')).write_text('late')
        if sys.argv[2]=='predecessor':
            # This appender passed its lock and directory checks before cutover.
            (outbox/'late-open-fd.json').touch()
    except PermissionError:
        pass
'''
            writers = [child(sys.executable, '-Bc', serializer, data, mode) for mode in ('released', 'predecessor')]
            for writer in writers:
                self.assertTrue(select.select([writer.stdout], [], [], 5)[0])
                self.assertEqual(writer.stdout.readline(), b'loaded\n')
            with socket.socket() as port:
                port.bind(('127.0.0.1', 0))
                address = '127.0.0.1:'+str(port.getsockname()[1])
            descendant = '''
import os, sys, time, urllib.error, urllib.request
if os.fork():
    os._exit(0)
print('descendant', flush=True)
sys.stdin.read()
token = open(sys.argv[1]).read().strip()
req = urllib.request.Request('http://'+sys.argv[2]+'/v1/logs', b'{}',
    headers={'Content-Type':'application/json','Authorization':'Bearer '+token})
for _ in range(60):
    try:
        with urllib.request.urlopen(req, timeout=.5) as response:
            print(response.status, flush=True)
        break
    except OSError:
        time.sleep(.1)
'''
            workload = child(sys.executable, '-Bc', descendant, runtime.auth, address)
            self.assertTrue(select.select([workload.stdout], [], [], 5)[0])
            self.assertEqual(workload.stdout.readline(), b'descendant\n')
            self.assertEqual(workload.wait(timeout=5), 0)
            active = {guest.COLLECTOR: True, guest.MANAGER: True}
            enabled = dict(active)
            invoked = []
            collector = None

            def systemctl(action, *args):
                nonlocal collector
                invoked.append(action)
                for unit in (unit for unit in args if unit in active):
                    if action in ('stop', 'enable', 'disable'):
                        if action != 'stop':
                            enabled[unit] = action == 'enable'
                        if action == 'stop' or '--now' in args:
                            active[unit] = action == 'enable'
                    if action == 'stop' and unit == guest.COLLECTOR and collector is not None:
                        collector.terminate()
                        collector.wait(timeout=10)
                        collector = None
                    if action == 'enable' and unit == guest.COLLECTOR and collector is None:
                        collector = child(runtime.library/'subyard-telemetry', 'serve', '--data-dir', data,
                                          '--listen', address, '--auth-file', runtime.auth)
                return ''

            runtime.manager = lambda unit: dict(LoadState='loaded', ActiveState='active' if active[unit] else 'inactive',
                                                UnitFileState='enabled' if enabled[unit] else 'disabled')
            runtime.running_no_new_privs = lambda _state: True
            runtime.skill_delivery = lambda _action: True
            prepare = runtime.prepare_store

            def interrupt_after_prepare():
                prepare()
                raise RuntimeError('fixture interrupted after store preparation')

            with patch.object(guest, 'systemctl', side_effect=systemctl), patch.object(guest.native, 'reconcile', return_value=True):
                for path in ('archive', guest.PENDING):
                    collision = data/path
                    collision.mkdir(mode=0o700)
                    (collision/'foreign').touch()
                    with self.assertRaisesRegex(ValueError, 'archive.*collision'):
                        runtime.apply()
                    self.assertTrue((collision/'foreign').exists())
                    self.assertEqual(invoked, [])
                    (collision/'foreign').unlink()
                    collision.rmdir()
                with patch.object(runtime, 'prepare_store', side_effect=interrupt_after_prepare):
                    with self.assertRaisesRegex(RuntimeError, 'after store preparation'):
                        runtime.apply()
                self.assertEqual(observer.wait(timeout=5), -signal.SIGKILL)
                self.assertTrue((data/guest.CUTOVER).exists())
                self.assertEqual(stat.S_IMODE((data/'outbox/.lock').stat().st_mode), 0)
                for writer in writers:
                    writer.communicate(timeout=5)
                    self.assertEqual(writer.returncode, 0)
                self.assertCountEqual([p.name for p in (data/'outbox').glob('*.json')], ['.valid.json', '.foreign.json'])
                # A registration and exit both reach durable spool before OTLP is ready.
                code = '''
import sys, types
sys.path.insert(0, sys.argv[1])
import launcher, records
assert records.emit(sys.argv[2], 'run', {'agent_run_id':'before-ready', 'state':'running',
    'start_time':'2000-01-01T00:00:00Z'}, source_time='2000-01-01T00:00:00Z')
assert records.emit(sys.argv[2], 'run', {'agent_run_id':'before-ready', 'agent_exited':True,
    'end_time':'2000-01-01T00:00:01Z'}, source_time='2000-01-01T00:00:01Z')
launcher.MAX_RUNS = 0
args = types.SimpleNamespace(command=['/bin/true'], data_dir=sys.argv[2], agent='other',
    agent_version='1', scope_mode='partial', task_id=None, project_id=None, worktree_id=None, yard_id=None, host_id=None)
assert launcher.launch(args) == 0
'''
                offline = subprocess.run(dev(sys.executable, '-Bc', code, source, data), capture_output=True, check=True, timeout=10)
                offline_id = offline.stderr.decode().split('agent_run_id=')[1].strip()
                runtime.apply()
                self.assertEqual(runtime.observe()['state'], 'current')
                self.assertFalse((data/guest.CUTOVER).exists())
            workload.stdin.close()
            self.assertTrue(select.select([workload.stdout], [], [], 10)[0])
            self.assertEqual(workload.stdout.readline(), b'200\n')
            deadline = time.monotonic()+10
            self.assertFalse((data/'auth.token').exists())
            self.assertFalse((data/'sources/observer-old.json').exists())
            self.assertEqual(runtime.auth.read_bytes(), b'a'*64+b'\n')
            with closing(sqlite3.connect((data/'telemetry.sqlite').as_uri()+'?mode=ro', uri=True)) as db:
                reason = "json_extract(body,'$.reason')"
                # Both spooled runs drain once the collector is ready.
                while db.execute("SELECT count(*) FROM runs WHERE agent_run_id IN (?,?) AND (agent_run_id=? OR json_extract(body,'$.end_time') IS NOT NULL)",
                                 (offline_id, 'before-ready', offline_id)).fetchone() != (2,):
                    self.assertLess(time.monotonic(), deadline)
                    time.sleep(.05)
                self.assertEqual(db.execute("SELECT count(*) FROM runs WHERE agent_run_id='legacy-run'").fetchone(), (0,))
                self.assertEqual(db.execute(f"SELECT count(*) FROM gaps WHERE {reason}='source_offline'").fetchone(), (0,))
            archive = data/'archive/telemetry.sqlite'
            self.assertEqual(stat.S_IMODE(archive.stat().st_mode), 0o400)
            with closing(sqlite3.connect(archive.as_uri()+'?mode=ro', uri=True)) as db:
                self.assertCountEqual(db.execute("SELECT agent_run_id,json_extract(body,'$.reason') FROM gaps").fetchall(),
                                      [('legacy-run', 'interrupted_by_upgrade')])
            database = data/'telemetry.sqlite'
            active_inode = database.stat().st_ino
            producer = data/'producer.json'
            policy = data/'spool-policy.json'
            original_producer = json.loads(producer.read_bytes())
            original_policy = json.loads(policy.read_bytes())
            invoked.clear()
            cases = (
                (policy, 'missing', None, None),
                (policy, 'invalid', {}, None),
                (policy, 'invalid', {**original_policy, 'unknown': 'excess'}, None),
                (policy, 'invalid', {**original_policy, 'policy_version': 'unexpected'}, None),
                (policy, 'invalid', {**original_policy, 'attributes': ['user.email']}, None),
                (producer, 'invalid', {}, None),
                (producer, 'invalid', {**original_producer, 'source_id': 'not-a-producer'}, None),
                (producer, 'invalid', {**original_producer, 'origin_id': '1'*32}, None),
                (producer, 'invalid', original_producer, 0o644),
                (producer, 'invalid', {}, 0o600),
            )
            with patch.object(guest, 'systemctl', side_effect=systemctl), patch.object(guest.native, 'reconcile', return_value=True), \
                 patch.dict(os.environ, SUBYARD_TELEMETRY_COLLECTOR=str(runtime.library/'subyard-telemetry')):
                self.assertEqual(runtime.observe()['state'], 'current')
                runtime.as_developer(lambda: self.assertTrue(records.emit(data, 'gap', {}, event_id='ready-before-repair')))
                for snapshot, status, value, mode in cases:
                    with self.subTest(file=snapshot.name, value=value, mode=mode):
                        if value is None:
                            snapshot.unlink()
                        else:
                            runtime.as_developer(lambda: records.atomic_json(snapshot, value))
                        if mode is not None:
                            snapshot.chmod(mode)
                        observed = runtime.observe()
                        self.assertEqual(observed['state'], 'stale')
                        self.assertIn({'parameter': 'telemetry.spool-policy', 'installed': status, 'desired': 'present'},
                                      observed['diagnostics'])
                        runtime.as_developer(lambda: self.assertFalse(records.emit(data, 'gap', {})))
                        runtime.apply()
                        self.assertEqual(runtime.observe()['state'], 'current')
                        runtime.as_developer(lambda: self.assertTrue(records.emit(data, 'gap', {})))
                        self.assertEqual(json.loads(producer.read_bytes()), original_producer)
            self.assertEqual(database.stat().st_ino, active_inode)
            self.assertTrue((data/'spool-policy.json').is_file())
            self.assertFalse((data/guest.CUTOVER).exists())
            self.assertEqual(list(data.glob('archive*')), [archive.parent])
            self.assertNotIn('disable', invoked)

    def test_interrupted_private_directory_creation_is_finished_on_retry(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            runtime = object.__new__(guest.Runtime)
            runtime.uid, runtime.gid = os.getuid(), os.getgid()
            runtime.data, runtime.control = Path(name)/'data', Path(name)/'control'
            runtime.operational = Path(name)/'data-private'
            runtime.auth = runtime.operational/'auth.token'
            refusal = 'foreign telemetry operational directory'
            # mkdir under a restrictive umask stopped before chmod.
            runtime.operational.mkdir(mode=0o500)
            runtime.prepare_auth()
            self.assertEqual(stat.S_IMODE(runtime.operational.stat().st_mode), 0o700)
            before = runtime.auth.read_bytes()
            runtime.operational.chmod(0o500)
            runtime.prepare_auth()
            self.assertEqual(stat.S_IMODE(runtime.operational.stat().st_mode), 0o700)
            self.assertEqual(runtime.auth.read_bytes(), before)
            runtime.auth.unlink()
            runtime.operational.chmod(0o750)
            with self.assertRaisesRegex(ValueError, refusal):
                runtime.prepare_auth()
            runtime.operational.rmdir()
            runtime.operational.symlink_to(runtime.data)
            runtime.data.mkdir(mode=0o700)
            with self.assertRaisesRegex(ValueError, refusal):
                runtime.prepare_auth()
            runtime.uid += 1
            with self.assertRaisesRegex(ValueError, 'foreign data directory'):
                runtime.private_directory(runtime.data, 'foreign data directory')
            self.assertEqual(runtime.data.stat().st_uid, os.geteuid())

    def test_private_directory_preserves_storage_error(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            runtime = object.__new__(guest.Runtime)
            runtime.uid, runtime.gid = os.geteuid(), os.getegid()
            with patch.object(guest.safefs.os, 'mkdir', side_effect=OSError(errno.ENOSPC, 'no space')):
                try:
                    runtime.private_directory(Path(name)/'data', 'foreign data directory')
                except OSError as error:
                    self.assertEqual(error.errno, errno.ENOSPC)
                    self.assertIn('(OSError at guest.py:', guest.failure_diagnostic(error))
                else:
                    self.fail('directory creation ignored ENOSPC')

    @unittest.skipUnless(os.geteuid() == 0, 'creating a directory as another UID needs root')
    def test_staged_developer_directory_recovers_without_changing_a_directory_uid(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            runtime = object.__new__(guest.Runtime)
            runtime.uid, runtime.gid = 65534, 65534
            path = Path(name)/'developer'
            refusal = 'foreign data directory'

            def fault(point):
                if point == guest.safefs.AFTER_DIR_CREATE:
                    raise guest.safefs.SimulatedCrash(point)

            with patch.object(guest.safefs, 'FAULT', fault), self.assertRaises(guest.safefs.SimulatedCrash):
                runtime.private_directory(path, refusal)
            self.assertFalse(path.exists())

            runtime.private_directory(path, refusal)
            info = path.stat()
            self.assertEqual((info.st_uid, info.st_gid, stat.S_IMODE(info.st_mode)), (runtime.uid, runtime.gid, 0o700))
            path.rmdir()
            path.mkdir(mode=0o700)
            with self.assertRaisesRegex(ValueError, refusal):
                runtime.private_directory(path, refusal)
            self.assertEqual(path.stat().st_uid, 0)

    @unittest.skipUnless(os.geteuid() == 0, 'creating a directory as another UID needs root')
    def test_concurrent_developer_directory_creation_validates_the_winner(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            runtime = object.__new__(guest.Runtime)
            runtime.uid, runtime.gid = 65534, 65534
            publish = guest.safefs.rename_noreplace
            for mode in (0o700, 0o750):
                with self.subTest(mode=oct(mode)):
                    path = root/str(mode)
                    winner = None

                    def publish_with_winner(src, dst, *, src_dir_fd, dst_dir_fd):
                        nonlocal winner
                        with patch.object(guest.safefs, 'rename_noreplace', publish):
                            runtime.create_developer_directory(dst_dir_fd, dst)
                        path.chmod(mode)
                        winner = path.stat()
                        return publish(src, dst, src_dir_fd=src_dir_fd, dst_dir_fd=dst_dir_fd)

                    with patch.object(guest.safefs, 'rename_noreplace', side_effect=publish_with_winner):
                        if mode == 0o700:
                            runtime.private_directory(path, 'foreign data directory')
                        else:
                            with self.assertRaisesRegex(ValueError, 'foreign data directory'):
                                runtime.private_directory(path, 'foreign data directory')
                    info = path.stat()
                    self.assertTrue(os.path.samestat(info, winner))
                    self.assertEqual((info.st_uid, info.st_gid, stat.S_IMODE(info.st_mode)), (runtime.uid, runtime.gid, mode))
            self.assertEqual(list(root.glob('.telemetry-dir-*')), [])

    def test_apply_finishes_or_refuses_the_data_directory_before_the_bearer(self):
        class Reached(Exception):
            pass

        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            runtime = object.__new__(guest.Runtime)
            runtime.uid, runtime.gid, runtime.selected = os.getuid(), os.getgid(), True
            runtime.control, runtime.library, data = root/'control', root/'library', root/'state/data'
            runtime.data = data
            runtime.safefs_path, runtime.safefs_content = root/'support/safefs.py', b'support'
            runtime.reconcile_native = lambda check: True
            runtime.manager = lambda unit: {}

            def apply():
                with patch.object(runtime, 'prepare_auth', side_effect=Reached), self.assertRaises(Reached):
                    runtime.apply()

            apply()
            self.assertEqual(stat.S_IMODE(data.stat().st_mode), 0o700)
            data.rmdir()
            # mkdir under a restrictive umask stopped before chmod.
            data.mkdir(mode=0o500)
            apply()
            self.assertEqual(stat.S_IMODE(data.stat().st_mode), 0o700)
            history = data/'history'
            history.touch()
            data.chmod(0o500)
            apply()
            self.assertTrue(history.is_file())
            history.unlink()
            for refused in ('file', 'shared'):
                with self.subTest(refused=refused):
                    data.rmdir()
                    if refused == 'file':
                        data.touch(mode=0o700)
                    else:
                        data.mkdir(mode=0o750)
                        data.chmod(0o750)
                    with self.assertRaisesRegex(ValueError, 'foreign data directory'):
                        runtime.apply()
                    if data.is_dir():
                        data.chmod(0o700)
                        for entry in data.iterdir():
                            entry.unlink()
                    else:
                        data.unlink()
                        data.mkdir()
            data.rmdir()
            data.symlink_to(root)
            with self.assertRaisesRegex(ValueError, 'unsafe data directory'):
                runtime.apply()
            data.unlink()
            data.mkdir(mode=0o700)
            runtime.uid += 1
            with self.assertRaisesRegex(ValueError, 'foreign data directory'):
                apply()
            self.assertEqual(data.stat().st_uid, os.geteuid())

    def test_v030_and_bearer_policy_migrations_resume_after_every_interruption(self):
        profile = Path(guest.__file__).parent
        token = 'c'*64
        with tarfile.open(Path(__file__).with_name('fixtures')/'v0.30.0-native.py.tar.gz') as archive:
            legacy_native = archive.extractfile('config/profiles/telemetry/native.py').read()
        v030 = {}
        exec(compile(legacy_native, 'v0.30.0/native.py', 'exec'), v030)

        class Interrupted(Exception):
            pass

        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            base = Path(name)
            base.chmod(0o711)
            source = base/'source'
            shutil.copytree(profile, source, ignore=shutil.ignore_patterns('tests', 'collector', 'cmd', 'bin', '__pycache__'))
            shutil.copyfile(guest.tomli_w.__file__, source/'tomli_w.py')
            shutil.copyfile(guest.safefs.__file__, source/'safefs.py')
            binary = source/'subyard-telemetry'
            shutil.copyfile(records.collector_binary(), binary)
            binary.chmod(0o755)
            root, pristine = base/'yard', base/'pristine'
            account = types.SimpleNamespace(pw_uid=os.getuid() or 1000, pw_gid=os.getgid(), pw_dir=str(root/'home'))
            states = {}

            def systemctl(action, *args):
                for unit in (unit for unit in args if unit in states):
                    if action in ('enable', 'disable'):
                        states[unit]['UnitFileState'] = action+'d'
                    if action in ('stop', 'start') or '--now' in args:
                        states[unit]['ActiveState'] = 'active' if action in ('start', 'enable') else 'inactive'
                return ''

            def runtime():
                value = guest.Runtime(source, 'dev', True, ['claude', 'codex'], root/'control', root/'data', root/'library',
                                      root/'units', root/'bin', account, etc=root/'etc')
                value.manager = lambda unit: dict(LoadState='loaded', **states[unit])
                value.running_no_new_privs = lambda _state: True
                value.skill_delivery = lambda _action: True
                return value

            def dev(*command):
                if os.getuid() != account.pw_uid:
                    return ['/usr/bin/setpriv', '--reuid', str(account.pw_uid), '--regid', str(account.pw_gid), '--clear-groups', *command]
                return command

            def remove(path):
                if path.exists():
                    subprocess.run(['chmod', '-R', 'u+rwX', path], check=True)
                    shutil.rmtree(path)

            # One v0.30 yard: home agent configurations, their receipt and a schema 4 store.
            installed = runtime()
            for directory in (installed.control, installed.library, installed.units, installed.commands, installed.etc):
                directory.mkdir(mode=0o755, parents=True)
            for directory in (installed.home, installed.data):
                directory.mkdir(mode=0o700)
                os.chown(directory, installed.uid, installed.gid)
            (installed.control/'marker').write_bytes(guest.MARKER)
            for path, (_, mode) in installed.assets.items():
                path.write_bytes(b'v0.30 '+path.name.encode())
                path.chmod(mode)
            (installed.control/'assets.json').write_text(json.dumps({
                str(path): hashlib.sha256(path.read_bytes()).hexdigest() for path in installed.assets}))
            claude, codex = (installed.home/parts[1] for parts in guest.native.AGENTS.values())
            prior = {claude: b'{"env": {"UNRELATED": "keep"}, "theme": "dark"}\n', codex: b'[analytics]\nenabled = false\n'}
            for path, content in prior.items():
                path.parent.mkdir(mode=0o700)
                path.write_bytes(content)
                path.chmod(0o600)
                for owned in (path.parent, path):
                    os.chown(owned, installed.uid, installed.gid)
            v030['reconcile'](installed.home, installed.control/'native.json', ['claude', 'codex'], installed.endpoint, token,
                              guest.tomli_w, installed.uid, installed.gid)
            for path in prior:
                self.assertIn(token.encode(), path.read_bytes())
            subprocess.run(dev(binary, 'prepare', '--data-dir', installed.data), check=True, timeout=10)
            initialization = '''
import contextlib, json, pathlib, sqlite3, sys
root = pathlib.Path(sys.argv[1])
(root/'spool-policy.json').unlink()
for name in ('runs', 'runs-v4', 'outbox', 'sources'):
    (root/name).mkdir(mode=0o700)
(root/'outbox/.lock').touch(mode=0o600)
(root/'manager.lock').touch(mode=0o600)
with contextlib.closing(sqlite3.connect(root/'telemetry.sqlite')) as db:
    db.execute("PRAGMA user_version=4")
    db.execute('DROP TABLE runs')
    db.execute('CREATE TABLE runs(agent_run_id TEXT PRIMARY KEY, body TEXT, source_time TEXT)')
    db.execute("INSERT INTO runs VALUES(?,?,'')", ('legacy-run', json.dumps({'state': 'running'})))
    db.commit()
for path, value in (
    ('runs/legacy-run.json', json.dumps(dict(agent_run_id='legacy-run', pid=2147483647,
        boot_id=pathlib.Path('/proc/sys/kernel/random/boot_id').read_text().strip()))),
    ('outbox/.valid.json', json.dumps(dict(schema_version=1, event_id='.valid', source_id='old-producer',
         source_version='1', kind='gap', body=dict(reason='source_offline')))),
    ('sources/observer-old.json', 'historical operational state'),
    ('auth.token', sys.argv[2]+chr(10)),
):
    (root/path).write_text(value)
    (root/path).chmod(0o600)
'''
            subprocess.run(dev(sys.executable, '-Bc', initialization, installed.data, token), check=True, timeout=5)
            subprocess.run(['cp', '-a', root, pristine], check=True)
            private = [root/'home', root/'etc', root/'control', root/'data', root/'data-private']
            effects = [(guest.native, 'publish'), (guest, 'systemctl'), (subprocess, 'run'), *((os, name) for name in (
                'replace', 'rename', 'unlink', 'rmdir', 'mkdir', 'chmod', 'fchmod', 'chown', 'fchown'))]

            previous_policies = False

            def check():
                for top in private:
                    for path in (top.rglob('*') if top.exists() else ()):
                        info = path.lstat()
                        if stat.S_ISREG(info.st_mode) and info.st_mode & 0o004:
                            self.assertFalse(token.encode() in path.read_bytes(), path)
                for path in (root/'data/auth.token', root/'data-private/auth.token'):
                    if path.exists():
                        self.assertEqual(path.read_bytes(), token.encode()+b'\n')
                helper = root/'library/otlp-headers'
                for parts in guest.native.AGENTS.values():
                    policy = root/'etc'/parts[0]
                    if not policy.exists():
                        continue
                    content = policy.read_bytes()
                    if str(helper).encode() not in content:
                        # Only the previous candidate's own policy still holds the bearer.
                        self.assertTrue(previous_policies and token.encode() in content, policy)
                        continue
                    self.assertNotIn(token.encode(), content)
                    # A policy never names a helper that is not installed yet.
                    self.assertEqual(helper.read_bytes(), guest.headers_helper(root/'data-private/auth.token'))

            def apply(stop):
                calls, publishing = 0, None

                def wrap(name, original):
                    def call(*args, **kwargs):
                        nonlocal calls, publishing
                        # Inside a publish, a stop before the rename equals a stop before the publish.
                        if publishing is not False or name == 'replace':
                            calls += 1
                            check()
                            if calls == stop:
                                raise Interrupted
                        if name != 'publish':
                            result = original(*args, **kwargs)
                            if name == 'replace' and publishing is False:
                                publishing = True
                            return result
                        publishing = False
                        try:
                            return original(*args, **kwargs)
                        finally:
                            publishing = None
                    return call

                with ExitStack() as patches:
                    patches.enter_context(patch.object(guest, 'systemctl', side_effect=systemctl))
                    for owner, name in effects:
                        patches.enter_context(patch.object(owner, name, wrap(name, getattr(owner, name))))
                    runtime().apply()
                return calls

            def copy(source, target):
                # Retired v0.30 locks have no permissions; copy them with their modes.
                locked = {path: stat.S_IMODE(path.lstat().st_mode) for path in source.rglob('*')
                          if stat.S_ISREG(path.lstat().st_mode) and not path.lstat().st_mode & stat.S_IRUSR}
                for path in locked:
                    path.chmod(0o400)
                subprocess.run(['cp', '-a', source, target], check=True)
                for path, mode in locked.items():
                    for entry in (path, target/path.relative_to(source)):
                        entry.chmod(mode)

            def upgrade(stop):
                remove(root)
                copy(pristine, root)
                states.update({unit: dict(ActiveState='active', UnitFileState='enabled') for unit in (guest.COLLECTOR, guest.MANAGER)})
                if stop:
                    # A child failure as guest root surfaces as a failed developer operation.
                    with self.assertRaises(Interrupted if os.getuid() else (Interrupted, ValueError)):
                        apply(stop)
                completed = apply(0)
                check()
                current = runtime()
                self.assertEqual(current.observe()['state'], 'current')
                self.assertEqual(current.auth.read_bytes(), token.encode()+b'\n')
                self.assertFalse((root/'data/auth.token').exists())
                self.assertFalse((root/'control/native.json').exists())
                self.assertFalse((root/'data'/guest.CUTOVER).exists())
                for parts in guest.native.AGENTS.values():
                    policy = current.etc/parts[0]
                    self.assertIn(str(current.helper).encode(), policy.read_bytes())
                    self.assertEqual(stat.S_IMODE(policy.stat().st_mode), 0o640)
                # Only the private token file names the bearer.
                named = [path for top in private for path in (top.rglob('*') if top.exists() else ())
                         if path.is_file() and not path.is_symlink() and os.access(path, os.R_OK) and token.encode() in path.read_bytes()]
                self.assertEqual(named, [current.auth])
                self.assertEqual(json.loads(claude.read_bytes()), json.loads(prior[claude]))
                self.assertEqual(guest.native.parse('codex', codex.read_bytes()), {'analytics': {'enabled': False}})
                # The legacy store, run, outbox and operational state are kept, once.
                self.assertEqual(list((root/'data').glob('archive*')), [root/'data/archive'])
                with closing(sqlite3.connect((root/'data/archive/telemetry.sqlite').as_uri()+'?mode=ro', uri=True)) as db:
                    self.assertEqual(db.execute('SELECT agent_run_id FROM runs').fetchall(), [('legacy-run',)])
                    self.assertEqual(db.execute("SELECT agent_run_id,json_extract(body,'$.reason') FROM gaps").fetchall(),
                                     [('legacy-run', 'interrupted_by_upgrade')])
                self.assertTrue((root/'data/outbox/.valid.json').exists())
                self.assertTrue((root/'data/runs/legacy-run.json').exists())
                kept = list((root/'data-private').glob('legacy-*/sources/observer-old.json'))
                self.assertEqual([path.read_text() for path in kept], ['historical operational state'])
                return completed

            try:
                # The run without an interruption counts the steps; every retry completes.
                steps = upgrade(0)
                self.assertGreater(steps, 40)
                for stop in range(1, steps+1):
                    with self.subTest(stop=stop):
                        upgrade(stop)
                # The previous candidate kept the bearer in both policies and installed no helper.
                upgrade(0)
                current = runtime()
                for agent, parts in guest.native.AGENTS.items():
                    document = guest.native.desired(agent, current.endpoint)
                    if agent == 'claude':
                        del document['otelHeadersHelper']
                        for signal in ('LOGS', 'METRICS', 'TRACES'):
                            document['env']['OTEL_EXPORTER_OTLP_'+signal+'_HEADERS'] = 'Authorization=Bearer '+token
                    else:
                        del document['otel']['headers_helper'], document['otel']['headers_helper_refresh_interval_ms']
                        for key in ('exporter', 'metrics_exporter', 'trace_exporter'):
                            document['otel'][key]['otlp-http']['headers'] = {'Authorization': 'Bearer '+token}
                    path = current.etc/parts[0]
                    guest.native.publish(path, guest.native.policy(agent, document, guest.tomli_w), path.read_bytes(),
                                         mode=guest.native.POLICY_MODE, policy_gid=current.gid)
                current.helper.unlink()
                inventory = json.loads((current.control/'assets.json').read_bytes())
                del inventory[str(current.helper)]
                (current.control/'assets.json').write_text(json.dumps(inventory))
                self.assertEqual(current.observe()['state'], 'stale')
                remove(pristine)
                copy(root, pristine)
                previous_policies = True
                steps = upgrade(0)
                self.assertGreater(steps, 5)
                for stop in range(1, steps+1):
                    with self.subTest(previous=True, stop=stop):
                        upgrade(stop)
            finally:
                remove(root)
                remove(pristine)

    def test_runtime_diagnostics_name_asset_and_receipt_drift_without_contents(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            path = Path(name)/'collector.py'
            path.write_bytes(b'private observed content')
            runtime = object.__new__(guest.Runtime)
            runtime.assets = {path: (b'desired content', 0o644)}
            runtime.retained = {}
            runtime.selected = True
            runtime.home, runtime.agents = Path(name), []
            runtime.control = runtime.data = Path(name)
            runtime.reconcile_native = lambda check: True
            runtime.owned = lambda: True
            runtime.asset_snapshot = lambda path: path.read_bytes()
            runtime.safefs_path = Path(name)/'support/safefs.py'
            runtime.safefs_content = b'expected support'
            runtime.safefs_state = lambda: types.SimpleNamespace(before=runtime.safefs_content, mode=0o644,
                                                                 receipt=b'd', current=True, digest=b'd', wanted=b'd')
            runtime.saved_inventory = lambda: {str(path): hashlib.sha256(path.read_bytes()).hexdigest()}
            runtime.manager = lambda unit: {'ActiveState': 'active', 'UnitFileState': 'enabled'}
            runtime.running_no_new_privs = lambda state: True
            runtime.skill_delivery = lambda mode: True
            runtime.desired = lambda: 'a'*64
            runtime.policy_status = lambda: 'missing'
            runtime.token = lambda: 'a'*64
            observation = runtime.observe()
            self.assertIn({'parameter': 'telemetry.asset.collector.py', 'installed': 'changed', 'desired': 'current'}, observation['diagnostics'])
            self.assertNotIn('private observed content', str(observation))

            path.write_bytes(b'desired content')
            path.chmod(0o644)
            runtime.policy_status = lambda: 'present'
            runtime.manager = lambda unit: {'ActiveState': 'active', 'UnitFileState': 'enabled'}
            for receipt, status in ((None, 'missing'), (b'd\nd\n', 'pending')):
                with self.subTest(status=status), patch.object(runtime, 'safefs_state', return_value=types.SimpleNamespace(
                        before=runtime.safefs_content, mode=0o644, receipt=receipt, wanted=b'd', digest=b'd', current=False)):
                    self.assertEqual(runtime.observe()['diagnostics'], [
                        {'parameter': 'telemetry.receipt.safefs.py', 'installed': status, 'desired': 'current'},
                    ])

    def test_shared_module_refuses_unmanaged_bytes_and_adopts_exact_bytes(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            runtime = object.__new__(guest.Runtime)
            runtime.control = root/'control'
            runtime.safefs_path, runtime.safefs_content = root/'support/safefs.py', b'support'
            runtime.assets = runtime.retained = {}
            runtime.selected = True
            runtime.owned = lambda: False
            runtime.reconcile_native = lambda check: True
            runtime.safefs_path.parent.mkdir()
            runtime.safefs_path.parent.chmod(0o2755)
            runtime.safefs_path.write_bytes(b'unmanaged')
            runtime.safefs_path.chmod(0o644)
            for operation in (runtime.observe, runtime.apply):
                with self.assertRaisesRegex(ValueError, 'managed asset changed'):
                    operation()
            self.assertEqual(runtime.safefs_path.read_bytes(), b'unmanaged')
            self.assertFalse(runtime.control.exists())

            runtime.control.mkdir(mode=0o700)
            runtime.safefs_path.write_bytes(runtime.safefs_content)
            state = runtime.safefs_state()
            self.assertEqual(state.mode, 0o644)
            self.assertFalse(state.publish)
            pending = runtime.publish_safefs(state)
            guest.native.publish(state.receipt_path, state.wanted, pending)
            self.assertTrue(runtime.safefs_state().current)
            self.assertEqual(stat.S_IMODE(runtime.safefs_path.parent.stat().st_mode), 0o2755)

    def test_observe_and_apply_combine_module_and_native_refusals(self):
        runtime = object.__new__(guest.Runtime)
        runtime.selected = True
        runtime.owned = lambda: True
        policy = guest.native.Refusal([('unsafe-policy', 'codex'), ('unreadable-policy', 'claude')])
        for operation in (runtime.observe, runtime.apply):
            with self.subTest(operation=operation.__name__), \
                    patch.object(runtime, 'safefs_state', side_effect=ValueError('managed asset changed')) as support, \
                    patch.object(runtime, 'reconcile_native', side_effect=policy) as native_check:
                with self.assertRaises(guest.native.Refusal) as raised:
                    operation()
                support.assert_called_once_with()
                native_check.assert_called_once_with(check=True)
                self.assertEqual(str(raised.exception), 'managed asset changed; '+str(policy))
                self.assertEqual(raised.exception.reasons[1:], policy.reasons)

    def test_observe_and_apply_keep_each_combined_errno(self):
        runtime = object.__new__(guest.Runtime)
        runtime.selected = True
        runtime.owned = lambda: True
        native_io = OSError(errno.ENOTDIR, 'private error text', '/private/native')
        errno_reason = ('check-errno', 'native', errno.ENOTDIR)
        cases = (
            (ValueError('managed asset changed'), ('check-refusal', 'managed asset changed')),
            (OSError(errno.EACCES, 'private module error', '/private/module'), ('check-errno', 'safefs', errno.EACCES)),
        )
        for operation in (runtime.observe, runtime.apply):
            for module_error, module_reason in cases:
                with self.subTest(operation=operation.__name__, module=module_reason[0]), \
                        patch.object(runtime, 'safefs_state', side_effect=module_error), \
                        patch.object(runtime, 'reconcile_native', side_effect=native_io) as native_check:
                    with self.assertRaises(guest.native.Refusal) as raised:
                        operation()
                    native_check.assert_called_once_with(check=True)
                    self.assertEqual(raised.exception.reasons, [module_reason, errno_reason])
                    self.assertEqual(str(raised.exception), '; '.join(guest.native.refusal_messages(raised.exception.reasons)))
                    self.assertIn(f'[Errno {errno.ENOTDIR}]', str(raised.exception))
                    self.assertNotIn('/private/', str(raised.exception))

    def test_running_child_no_new_privs_uses_actual_kernel_status_and_uid(self):
        process=subprocess.Popen(['/usr/bin/setpriv','--no-new-privs',sys.executable,'-c',
            "import sys;print('ready',flush=True);sys.stdin.read()"],stdin=subprocess.PIPE,stdout=subprocess.PIPE)
        try:
            self.assertEqual(process.stdout.readline(),b'ready\n')
            runtime=types.SimpleNamespace(uid=os.getuid())
            fields=dict(ActiveState='active',MainPID=str(process.pid))
            self.assertTrue(guest.Runtime.running_no_new_privs(runtime,fields))
            runtime.uid+=1
            self.assertFalse(guest.Runtime.running_no_new_privs(runtime,fields))
            self.assertFalse(guest.Runtime.running_no_new_privs(runtime,dict(ActiveState='active',MainPID='0')))
        finally:
            process.communicate(timeout=3)

    def test_type_wide_incuseffective_envelope_admits_only_fixed_startup(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            units=Path(name)
            command='/usr/bin/setpriv --no-new-privs /usr/local/lib/subyard-telemetry/subyard-telemetry serve'
            runtime=types.SimpleNamespace(units=units,user='dev',gid=1000,starts={guest.COLLECTOR:command},
                data=units/'data',auth=units/'data-private/auth.token',
                assets={units/guest.COLLECTOR:(b'known unit',0o644)})
            runtime.loaded_as=lambda *arguments: guest.Runtime.loaded_as(runtime,*arguments)
            fields=dict(LoadState='loaded',FragmentPath=str(units/guest.COLLECTOR),User='dev',Group='1000',
                Type='simple',KillMode='control-group',UMask='0077',LimitCORE='0',NoNewPrivileges='no',
                DropInPaths='/run/systemd/system/service.d/zzz-lxc-service.conf',MemoryMax='268435456',TasksMax='64',
                CPUQuotaPerSecUSec='250ms',ExecStart='{ path=/usr/bin/setpriv ; argv[]='+command+' ; ignore_errors=no ; start_time=x ; }')
            with patch.object(guest,'systemctl',return_value='\n'.join(key+'='+value for key,value in fields.items())):
                observed=guest.Runtime.manager(runtime,guest.COLLECTOR)
            self.assertEqual(observed['NoNewPrivileges'],'no')
            fields.update(ActiveState='activating',SubState='auto-restart',NoNewPrivileges='yes')
            with patch.object(guest,'systemctl',return_value='\n'.join(key+'='+value for key,value in fields.items())):
                self.assertEqual(guest.Runtime.manager(runtime,guest.COLLECTOR)['ActiveState'],'failed')

    def test_unit_failure_projection_reports_predicates_without_private_native_fields(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            binary=Path(name)/'systemctl'
            binary.write_text('''#!/usr/bin/env python3
import sys
unit=sys.argv[2]
fields=dict(LoadState='loaded',FragmentPath='/etc/systemd/system/'+unit,User='dev',Group='1000',
    DropInPaths='/run/systemd/system/service.d/PRIVATE_FILE.conf',NoNewPrivileges='no',Type='simple',
    KillMode='process' if unit.endswith('-runs.service') else 'control-group',UMask='0077',LimitCORE='0',
    ExecStart='PRIVATE_COMMAND',Environment='PRIVATE_ENV',EnvironmentFiles='/private/config')
print('\\n'.join(key+'='+value for key,value in fields.items()))
''')
            binary.chmod(0o755)
            result=subprocess.run([sys.executable,'-B','-','dev','1000'],input=fixture.UNIT_FACTS.encode(),
                capture_output=True,check=True,timeout=5,env=dict(os.environ,PATH=name+os.pathsep+os.environ['PATH']))
            document=json.loads(result.stdout)
            self.assertEqual(len(document['units']),2)
            for unit in document['units']:
                self.assertEqual(unit['dropins'],{'type-wide':1,'unit-specific':0,'other':0})
                self.assertTrue(unit['expected_matches']['FragmentPath'])
                self.assertTrue(unit['expected_matches']['User'])
                self.assertFalse(unit['expected_matches']['NoNewPrivileges'])
                self.assertFalse(unit['expected_matches']['ExecStart'])
                self.assertFalse(unit['expected_matches']['Environment'])
                self.assertTrue(all(type(value) is bool for value in unit['expected_matches'].values()))
            self.assertNotIn(b'PRIVATE',result.stdout)
            self.assertNotIn(b'/private',result.stdout)

    def test_profile_cli_stderr_summary_preserves_error_words_and_redacts_values(self):
        private = ['SENSITIVE_HEADER','SENSITIVE_CAP','SENSITIVE_TOKEN','SENSITIVE_ENV','SENSITIVE_ARGV',
                   'SENSITIVE_CONFIG','SENSITIVE_USER','SENSITIVE_PASSWORD','SENSITIVE_QUERY','SENSITIVE_PATH',
                   '0123456789abcdef0123456789abcdef']
        output = ('yard: cannot enable profile because required setting is missing\n'
                  'Authorization: Bearer SENSITIVE_HEADER\nX-Private-Capability: SENSITIVE_CAP\n'
                  'token=SENSITIVE_TOKEN CUSTOM_ENV=SENSITIVE_ENV\nargv: [SENSITIVE_ARGV]\n'
                  'config: {private: SENSITIVE_CONFIG}\n'
                  'https://SENSITIVE_USER:SENSITIVE_PASSWORD@example.invalid/path?key=SENSITIVE_QUERY\n'
                  '/private/SENSITIVE_PATH 0123456789abcdef0123456789abcdef\n')
        argv = [sys.executable,'-c','import sys;sys.stdout.write(sys.argv[1]);sys.stderr.write(sys.argv[1]);sys.exit(7)',output]
        for operation in ('profile-enable','profile-disable','guest-command','installed-owner-observe'):
            with self.subTest(operation=operation), self.assertRaises(fixture.CommandFailure) as caught:
                fixture.command(argv,operation=operation)
            facts = caught.exception.facts
            if operation in ('profile-enable','profile-disable'):
                self.assertIn('cannot enable profile because required setting is missing',facts['stderr_summary'])
                self.assertLessEqual(len(facts['stderr_summary']),1024)
            else:
                self.assertNotIn('stderr_summary',facts)
            for value in private:
                self.assertNotIn(value,str(caught.exception))
        self.assertLessEqual(len(fixture.profile_stderr_summary(b'ordinary error '*1000)),1024)
        self.assertEqual(fixture.profile_stderr_summary(b'cannot enable profile\nconfig:\nshort-private-value\nmore-private-data'),
                         'cannot enable profile <details>')

    def test_failed_enable_observes_same_installed_owner_once_without_engine_guard(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            engine = root/'bin/yard'
            owner = root/'config/profiles/telemetry/owner.py'
            engine.parent.mkdir()
            engine.write_text('synthetic candidate')
            owner.parent.mkdir(parents=True)
            owner.write_text("import os,sys\nassert sys.argv[1:]==['--runtime','observe']\nassert os.environ['INCUS_PROJECT']=='fixture-project'\nassert os.environ['YARD_INSTANCE_NAME']=='fixture-instance'\nsys.stderr.write('PRIVATE_CREDENTIAL /private/config\\ntelemetry: local reconciliation failed (PermissionError at native.py:28)\\n');sys.exit(7)\n")
            context = dict(INCUS_PROJECT='fixture-project',YARD_INSTANCE_NAME='fixture-instance',DEV_USER='dev',
                           ENVIRONMENT_PROFILES='android',CODING_TOOL_INTEGRATIONS='')
            args = types.SimpleNamespace(engine=engine,project='fixture-project',instance='fixture-instance')
            stderr = io.StringIO()
            with patch.object(fixture,'command',wraps=fixture.command) as called, redirect_stderr(stderr):
                fixture.observe_enable_failure(args, context.__getitem__)
            called.assert_called_once()
            self.assertEqual(called.call_args.args[0], [sys.executable,'-B',str(owner),'--runtime','observe'])
            self.assertNotIn('SUBYARD_ENGINE_CONTEXT', {key:value for key,value in called.call_args.kwargs['env'].items()
                                                      if key not in os.environ})
            result = json.loads(stderr.getvalue())
            self.assertEqual(result['command']['stderr_classes'], ['PermissionError'])
            self.assertEqual(result['command']['stderr_locations'], ['native.py:28'])
            self.assertNotIn('PRIVATE', stderr.getvalue())

    def test_real_command_failure_is_redacted_and_cleanup_preserves_primary(self):
        secret = 'Bearer PRIVATE_CREDENTIAL /private/config PRIVATE_ARGV'
        argv = [sys.executable, '-c', 'import sys;sys.stderr.write(sys.argv[1]);sys.exit(7)',
                'permission denied '+secret]
        with self.assertRaises(fixture.CommandFailure) as caught:
            fixture.command(argv, operation='profile-enable')
        primary = caught.exception
        self.assertEqual(primary.facts['exit_code'], 7)
        self.assertEqual(primary.facts['stderr_signals'], ['permission denied'])
        self.assertNotIn(secret, str(primary))
        self.assertNotIn('PRIVATE', str(primary))
        stderr = io.StringIO()
        with redirect_stderr(stderr):
            fixture.cleanup([('disable', lambda: fixture.command(argv, operation='profile-disable'))], primary)
        diagnostic = json.loads(stderr.getvalue())
        self.assertEqual(diagnostic['operation'], 'disable')
        self.assertEqual(diagnostic['command']['exit_code'], 7)
        self.assertNotIn('PRIVATE', stderr.getvalue())
        self.assertEqual(primary.facts['operation'], 'profile-enable')
        with redirect_stderr(io.StringIO()), self.assertRaises(fixture.CommandFailure):
            fixture.cleanup([('disable', lambda: fixture.command(argv, operation='profile-disable'))], None)

    def test_apply_repeat_pause_disable_preserves_data_and_never_stops_workloads(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            root.chmod(0o755)  # Permit policy traversal for the fixture developer when run as root.
            source = root/'source'
            source.mkdir()
            for path in Path(guest.__file__).parent.glob('*.py'):
                shutil.copyfile(path, source/path.name)
            shutil.copyfile(Path(guest.tomli_w.__file__), source/'tomli_w.py')
            # Match the compiled collector's observed 25 MiB size, not a tiny stub.
            (source/'subyard-telemetry').write_bytes(b'\x7fELF'+b'\0'*(25_333_922-4))
            for directory in ('home', 'units', 'bin', 'etc'):
                (root/directory).mkdir()
            account = types.SimpleNamespace(pw_uid=os.getuid() or 1000, pw_gid=os.getgid(), pw_dir=str(root/'home'))
            states, calls = {}, []
            registered = False
            def skill_delivery(mode):
                nonlocal registered
                if mode == 'apply':
                    registered = True
                elif mode == 'remove':
                    registered = False
                return registered
            def manager(*args):
                calls.append(args)
                if args[0] == 'show':
                    unit = args[1]
                    if unit not in states:
                        return 'LoadState=not-found'
                    command=next(line.removeprefix('ExecStart=') for line in (root/'units'/unit).read_text().splitlines() if line.startswith('ExecStart='))
                    unit_user=next(line.removeprefix('User=') for line in (root/'units'/unit).read_text().splitlines() if line.startswith('User='))
                    fields=dict(LoadState='loaded',FragmentPath=str(root/'units'/unit),User=unit_user,Group=str(account.pw_gid),
                        Type='simple',UMask='0077',LimitCORE='0',NoNewPrivileges='yes',DropInPaths='',
                        ExecStart='{ path='+command.split()[0]+' ; argv[]='+command+' ; ignore_errors=no ; start_time=x ; }',
                        KillMode='process' if unit==guest.MANAGER else 'control-group',Delegate='yes',
                        MemoryMax=str(256*1024*1024),TasksMax='64',CPUQuotaPerSecUSec='250ms')
                    fields.update(states[unit])
                    # A startup other than the file's stays loaded until daemon-reload drops it.
                    fields.setdefault('NeedDaemonReload', 'no' if 'argv[]='+command+' ; ' in fields['ExecStart'] else 'yes')
                    return '\n'.join(f'{key}={value}' for key,value in fields.items())
                if args[0] == 'enable':
                    for unit in args[2:]:
                        # enable --now enables an already active unit without restarting it.
                        if states.get(unit, {}).get('ActiveState') != 'active':
                            states[unit] = dict(ActiveState='active')
                        states[unit]['UnitFileState'] = 'enabled'
                if args[0] == 'disable':
                    for unit in args[1:]:
                        if unit.startswith('--'):
                            continue
                        states[unit]['UnitFileState'] = 'disabled'
                        if '--now' in args:
                            states[unit]['ActiveState'] = 'inactive'
                if args[0] == 'daemon-reload':
                    for state in states.values():
                        state.pop('ExecStart', None)
                        state.pop('Environment', None)
                if args[0] in ('stop','start'):
                    for unit in args[1:]:states[unit]['ActiveState']='active' if args[0]=='start' else 'inactive'
                return ''
            # The LXC guest's type-wide drop-in leaves startup, account and environment alone.
            (root/'service.d').mkdir(mode=0o755)
            (root/'service.d').chmod(0o755)
            lxc = root/'service.d/zzz-lxc-service.conf'
            lxc.write_text('[Service]\nProcSubset=all\nNoNewPrivileges=no\n')
            lxc.chmod(0o644)
            with patch.object(guest.pwd, 'getpwnam', return_value=account), patch.object(guest, 'systemctl', side_effect=manager), \
                 patch.object(guest, 'TYPE_DROPINS', (lxc.parent,)), patch.object(guest.Runtime, 'prepare_store'), \
                 patch.object(guest.Runtime, 'policy_status', return_value='present'), \
                 patch.object(guest.records, 'sanitize_state', side_effect=lambda _family, value, _binary: value), \
                 patch.object(guest.Runtime, 'skill_delivery', side_effect=skill_delivery), \
                 patch.object(guest.Runtime,'running_no_new_privs',side_effect=lambda fields: fields.get('ProcessNNP','yes')=='yes'):
                runtime = guest.Runtime(source, 'dev', True, ['codex'], root/'control', root/'data', root/'library', root/'units', root/'bin', etc=root/'etc')
                off = guest.Runtime(source, 'dev', False, ['codex'], root/'control', root/'data', root/'library', root/'units', root/'bin', etc=root/'etc')
                foreign = root/'etc/codex/managed_config.toml'
                foreign.parent.mkdir()
                foreign.write_text('# Administrator policy\n')
                self.assertEqual(off.observe()['state'], 'absent')
                off.apply()
                self.assertEqual(foreign.read_text(), '# Administrator policy\n')
                with self.assertRaisesRegex(guest.native.Refusal, 'unsafe or foreign telemetry policy'):
                    runtime.observe()
                foreign.unlink()
                for command in runtime.starts.values():
                    self.assertEqual(command.split()[:2],['/usr/bin/setpriv','--no-new-privs'])
                    actual=subprocess.check_output(command.split()[:2]+[sys.executable,'-c',
                        "from pathlib import Path;print(next(line.split(':',1)[1].strip() for line in Path('/proc/self/status').read_text().splitlines() if line.startswith('NoNewPrivs:')))"])
                    self.assertEqual(actual.strip(),b'1')
                self.assertEqual(runtime.observe()['state'], 'stale')
                runtime.apply()
                self.assertTrue(registered)
                observed=runtime.observe()
                self.assertEqual(observed['state'], 'current')
                self.assertEqual(observed['actual'],observed['desired'],'current runtime must satisfy the generic activation digest contract')
                self.assertEqual(runtime.safefs_path.read_bytes(), Path(guest.safefs.__file__).read_bytes())
                self.assertNotIn(str(runtime.safefs_path), runtime.saved_inventory())
                subprocess.run([sys.executable, '-IBc', "import sys; sys.path.insert(0, sys.argv[1]); "
                                "import records; assert records.safefs.__file__ == sys.argv[2]",
                                str(runtime.library), str(runtime.safefs_path)], check=True, timeout=5)
                # Stop after publishing only the module. Both the receipt and
                # restart decision must survive a retry of the same update.
                original_support = runtime.safefs_content
                runtime.safefs_content += b'\n# replacement module\n'
                publish = guest.native.publish

                def interrupt_support(path, *args, **kwargs):
                    publish(path, *args, **kwargs)
                    if path == runtime.safefs_path:
                        raise InterruptedError('module published')

                inventory = runtime.saved_inventory()
                start = len(calls)
                with patch.object(guest.native, 'publish', side_effect=interrupt_support):
                    with self.assertRaisesRegex(InterruptedError, 'module published'):
                        runtime.apply()
                self.assertIn(('stop', guest.MANAGER), calls[start:])
                self.assertFalse(any(call[0] == 'stop' and guest.COLLECTOR in call for call in calls[start:]))
                support = runtime.safefs_state()
                self.assertFalse(support.current)
                self.assertFalse(support.publish)
                self.assertEqual(len(support.receipt.splitlines()), 2)
                start = len(calls)
                runtime.apply()
                self.assertIn(('enable', '--now', guest.COLLECTOR, guest.MANAGER), calls[start:])
                self.assertTrue(runtime.safefs_state().current)
                self.assertEqual(runtime.observe()['state'], 'current')
                self.assertEqual(runtime.saved_inventory(), inventory)
                runtime.safefs_content = original_support
                runtime.apply()
                installed_native = root/'library/native.py'
                receipt_path = root/'control/assets.json'
                legacy_fixture = Path(__file__).with_name('fixtures')/'v0.30.0-native.py.tar.gz'
                with tarfile.open(legacy_fixture) as archive:
                    legacy_native = archive.extractfile('config/profiles/telemetry/native.py').read()
                # A v0.30 receipt names the token its home documents carry; a
                # missing token file is restored from it rather than rotated.
                legacy = {}
                exec(compile(legacy_native, 'v0.30.0/native.py', 'exec'), legacy)
                legacy_token = 'c'*64
                runtime.auth.unlink()
                legacy['reconcile'](root/'home', root/'control/native.json', ['codex'], runtime.endpoint, legacy_token,
                                    guest.tomli_w, runtime.uid, runtime.gid)
                self.assertIn(legacy_token.encode(), (root/'home/.codex/config.toml').read_bytes())
                self.assertEqual(runtime.observe()['state'], 'stale')
                runtime.apply()
                self.assertEqual(runtime.observe()['state'], 'current')
                self.assertEqual(runtime.auth.read_text(), legacy_token+'\n')
                self.assertFalse((root/'control/native.json').exists())
                self.assertNotIn(legacy_token.encode(), (root/'home/.codex/config.toml').read_bytes())
                self.assertNotIn(legacy_token.encode(), (root/'etc/codex/managed_config.toml').read_bytes())
                # Rolled back, v0.30 mints a token in its own path; the private one stays.
                minted = 'd'*64
                (root/'data/auth.token').write_text(minted+'\n')
                (root/'data/auth.token').chmod(0o600)
                legacy['reconcile'](root/'home', root/'control/native.json', ['codex'], runtime.endpoint, minted,
                                    guest.tomli_w, runtime.uid, runtime.gid)
                self.assertEqual(runtime.observe()['state'], 'stale')
                runtime.apply()
                self.assertEqual(runtime.observe()['state'], 'current')
                self.assertEqual(runtime.auth.read_text(), legacy_token+'\n')
                self.assertFalse((root/'control/native.json').exists())
                self.assertFalse((root/'data/auth.token').exists())
                self.assertNotIn(minted.encode(), (root/'home/.codex/config.toml').read_bytes())
                # The collector trims a hand-restored token; the helper reads only an exact
                # first line. Observation reports that drift and apply rewrites the file.
                receiver_auth = {'parameter': 'telemetry.receiver-auth', 'installed': 'missing or invalid', 'desired': 'present'}
                for restored in (legacy_token+'\r\n', legacy_token+' \n', '\n'+legacy_token+'\n', legacy_token):
                    with self.subTest(restored=restored.encode()):
                        guest.native.publish(runtime.auth, restored.encode(), runtime.auth.read_bytes(), runtime.uid, runtime.gid)
                        helper_accepts = restored in (legacy_token, legacy_token+'\n')
                        observed = runtime.observe()
                        self.assertEqual(observed['state'], 'current' if helper_accepts else 'stale')
                        self.assertEqual(receiver_auth in observed.get('diagnostics', []), not helper_accepts)
                        runtime.apply()
                        self.assertEqual(runtime.observe()['state'], 'current')
                        self.assertEqual(runtime.auth.read_text(), legacy_token+'\n')
                shutil.rmtree(root/'home/.codex')
                publish = guest.native.publish

                def v030_accepts():
                    # v0.30 apply requires each path entry to be the digest on disk.
                    inventory = json.loads(receipt_path.read_bytes())
                    return all(inventory.get(str(path)) == hashlib.sha256(path.read_bytes()).hexdigest()
                               for path in [*runtime.assets, *runtime.retained] if path.exists())

                for stop in ('before-rename', 'after-rename', 'daemon-reload'):
                    after_rename = stop != 'before-rename'
                    with self.subTest(stop=stop):
                        installed_native.write_bytes(legacy_native)
                        inventory = json.loads(receipt_path.read_bytes())
                        inventory[str(installed_native)] = hashlib.sha256(legacy_native).hexdigest()
                        receipt_path.write_text(json.dumps(inventory))

                        def interrupt(path, *args, **kwargs):
                            if path == installed_native and stop != 'daemon-reload':
                                if after_rename:
                                    publish(path, *args, **kwargs)
                                raise InterruptedError('fixture stopped at native asset replacement')
                            publish(path, *args, **kwargs)

                        def reload(*args):
                            if args == ('daemon-reload',) and stop == 'daemon-reload':
                                raise InterruptedError('fixture stopped at daemon reload')
                            return manager(*args)

                        with patch.object(guest.native, 'publish', side_effect=interrupt), \
                             patch.object(guest, 'systemctl', side_effect=reload), self.assertRaises(InterruptedError):
                            runtime.apply()
                        # Only a stop between asset replacements is the accepted rollback residual.
                        self.assertEqual(v030_accepts(), stop != 'after-rename')
                        self.assertEqual(runtime.observe()['state'], 'stale')
                        retry = off if after_rename else runtime
                        retry.apply()
                        self.assertEqual(retry.observe()['state'], 'current')
                        self.assertEqual(installed_native.read_bytes(), runtime.assets[installed_native][0])
                        self.assertEqual(json.loads(receipt_path.read_bytes())[str(installed_native)],
                                         hashlib.sha256(installed_native.read_bytes()).hexdigest())
                        runtime.apply()
                self.assertEqual(runtime.startup(), {'state': 'ready'})
                registered = False
                self.assertIn({'parameter': 'telemetry.skill', 'installed': 'absent', 'desired': 'registered'},
                              runtime.observe()['diagnostics'])
                # A Codex without the headers helper is only named among drift diagnostics.
                old_codex = root/'codex'
                old_codex.symlink_to('releases/0.169.0/bin/codex')
                helper_diagnostic = {'parameter': 'telemetry.codex-headers-helper', 'installed': 'missing', 'desired': 'present'}
                with patch.object(guest, 'CODEX_LINK', old_codex):
                    self.assertIn(helper_diagnostic, runtime.observe()['diagnostics'])
                    runtime.apply()
                    self.assertEqual(runtime.observe()['state'], 'current')
                self.assertNotIn(helper_diagnostic, runtime.observe().get('diagnostics', []))
                self.assertTrue(registered)
                for unit in (guest.COLLECTOR, guest.MANAGER):
                    states[unit].update(ActiveState='inactive', Job='17')
                    self.assertEqual(runtime.startup()['state'], 'starting')
                    states[unit].update(ActiveState='active', Job='')
                states[guest.COLLECTOR]['ActiveState'] = 'inactive'
                self.assertEqual(runtime.startup(), {'state': 'drift'})
                states[guest.COLLECTOR]['ActiveState'] = 'failed'
                states[guest.MANAGER].update(ActiveState='inactive', Job='23')
                self.assertEqual(runtime.startup()['state'], 'starting')
                states[guest.MANAGER].update(ActiveState='active', Job='')
                self.assertEqual(runtime.startup(), {'state': 'drift'})
                states[guest.COLLECTOR].update(ActiveState='activating', SubState='auto-restart')
                self.assertEqual(runtime.startup(), {'state': 'drift'})
                for budget in ('30000', '0'):
                    with patch.dict(os.environ, {'SUBYARD_STARTUP_REMAINING_MS':budget}):
                        self.assertEqual(runtime.startup()['state'], 'starting')
                states[guest.COLLECTOR]['SubState'] = 'running'
                states[guest.COLLECTOR]['ActiveState'] = 'active'
                marker = root/'data/retained.db'
                marker.write_bytes(b'history')
                # Recover an active interrupted previous install. Unit text and
                # manifest are exactly the known pre-setpriv version.
                saved=json.loads((root/'control/assets.json').read_text())
                for unit in (guest.COLLECTOR,guest.MANAGER):
                    path=root/'units'/unit
                    legacy=runtime.assets[path][0].replace(b'/usr/bin/setpriv --no-new-privs ',b'',1)
                    path.write_bytes(legacy)
                    saved[str(path)]=hashlib.sha256(legacy).hexdigest()
                    states[unit].update(DropInPaths=str(lxc),NoNewPrivileges='no')
                    command = runtime.starts[unit].removeprefix('/usr/bin/setpriv --no-new-privs ')
                    states[unit]['ExecStart'] = '{ path='+command.split()[0]+' ; argv[]='+command+' ; ignore_errors=no ; }'
                (root/'control/assets.json').write_text(json.dumps(saved))
                self.assertEqual(runtime.observe()['state'],'stale')
                self.assertEqual(runtime.startup(), {'state': 'ready'})
                publish = guest.native.publish

                def interrupt_legacy_unit(path, *args, **kwargs):
                    publish(path, *args, **kwargs)
                    if path == runtime.units/guest.COLLECTOR:
                        raise InterruptedError

                with patch.object(guest.native, 'publish', side_effect=interrupt_legacy_unit), self.assertRaises(InterruptedError):
                    runtime.apply()
                self.assertEqual(runtime.observe()['state'], 'stale')
                runtime.apply()
                self.assertIn(('stop',guest.COLLECTOR,guest.MANAGER),calls)
                self.assertEqual(runtime.observe()['state'],'current')
                stops=sum(call[0]=='stop' for call in calls)
                runtime.apply()
                self.assertEqual(sum(call[0]=='stop' for call in calls),stops)
                # A restart interrupted after new files/manifest were committed
                # must still retry the old active NNP0 process on unchanged files.
                states[guest.COLLECTOR]['ProcessNNP']='no'
                stale=runtime.observe()
                self.assertEqual(stale['state'],'stale')
                self.assertNotEqual(stale['actual'],stale['desired'])
                runtime.apply()
                self.assertEqual(calls[-3:], [('stop',guest.COLLECTOR), ('daemon-reload',),
                                             ('enable','--now',guest.COLLECTOR,guest.MANAGER)])
                self.assertEqual(runtime.observe()['state'],'current')
                # Rolling back from a later release that changed the startup: its recorded
                # units are stale, not foreign, and apply rewrites them with this startup.
                saved=json.loads((root/'control/assets.json').read_text())
                for unit in (guest.COLLECTOR,guest.MANAGER):
                    path=root/'units'/unit
                    future=runtime.assets[path][0].replace(b' --data-dir ',b' --future-flag --data-dir ',1)
                    future=future.replace(b'ExecStart=',b'Environment=FUTURE=1\nExecStart=',1)
                    path.write_bytes(future)
                    saved[str(path)]=hashlib.sha256(future).hexdigest()
                    states[unit]['Environment']='FUTURE=1'
                (root/'control/assets.json').write_text(json.dumps(saved))
                self.assertEqual(runtime.observe()['state'],'stale')
                runtime.apply()
                self.assertIn(('stop',guest.COLLECTOR,guest.MANAGER),calls)
                self.assertEqual(runtime.observe()['state'],'current')
                for unit in (guest.COLLECTOR,guest.MANAGER):
                    self.assertEqual((root/'units'/unit).read_bytes(),runtime.assets[root/'units'/unit][0])
                for changed in ({'User':'root'},{'DropInPaths':'/etc/systemd/system/subyard-.service.d/foreign.conf'},
                                {'DropInPaths':'/etc/systemd/system/'+guest.COLLECTOR+'.d/foreign.conf'},
                                {'KillMode':'process'},{'MemoryMax':'infinity'},{'Environment':'PRIVATE=unsafe'},
                                {'ExecStart':'{ path=/bin/false ; argv[]=/bin/false ; ignore_errors=no ; }'}):
                    original=dict(states[guest.COLLECTOR])
                    states[guest.COLLECTOR].update(changed)
                    with self.assertRaises(ValueError):runtime.apply()
                    states[guest.COLLECTOR]=original
                # Replacing files used by running services must start new processes
                # even when the service definitions are unchanged.
                for asset in ('records.py', 'subyard-telemetry'):
                    installed = runtime.library/asset
                    before, mode = runtime.assets[installed]
                    (source/asset).write_bytes(before + b'\n')
                    runtime.assets[installed] = before + b'\n', mode
                start = len(calls)
                runtime.apply()
                upgrade_calls = calls[start:]
                self.assertLess(upgrade_calls.index(('stop', guest.COLLECTOR, guest.MANAGER)),
                                upgrade_calls.index(('enable', '--now', guest.COLLECTOR, guest.MANAGER)))
                self.assertEqual(runtime.observe()['state'], 'current')

                start = len(calls)
                runtime.selected = False
                runtime.apply()
                self.assertFalse(registered)
                observed=runtime.observe()
                self.assertEqual(observed['state'], 'current')
                self.assertEqual(observed['actual'],observed['desired'])
                self.assertEqual(marker.read_bytes(), b'history')
                self.assertTrue((root/'data/drain.json').exists())
                # A fresh unselected run, packaged or from plain source, needs no
                # collector binary and keeps the installed one unchanged.
                bare = root/'bare-source'
                shutil.copytree(source, bare)
                (bare/'subyard-telemetry').unlink()
                installed_binary = root/'library/subyard-telemetry'
                kept = installed_binary.read_bytes()
                runs = {name: guest.Runtime(path, 'dev', False, ['codex'], root/'control', root/'data', root/'library', root/'units', root/'bin', etc=root/'etc')
                        for name, path in (('source', bare), ('packaged', source))}
                for unselected in runs.values():
                    self.assertNotIn(installed_binary, unselected.assets)
                self.assertEqual(runs['source'].observe(), runs['packaged'].observe())
                inventory_path = root/'control/assets.json'
                saved = json.loads(inventory_path.read_text())
                stale = {**saved, str(root/'library/records.py'): '0'*64}
                inventory_path.write_text(json.dumps(stale))
                for unselected in runs.values():
                    observation = unselected.observe()
                    self.assertEqual(observation['state'], 'stale')
                    self.assertIn('telemetry.inventory', [item['parameter'] for item in observation['diagnostics']])
                installed_records = root/'library/records.py'
                candidate = installed_records.read_bytes()
                installed_records.write_bytes(b'unmanaged edit')
                with self.assertRaisesRegex(ValueError, 'managed asset changed'):
                    runs['source'].apply()
                installed_records.write_bytes(candidate)
                runs['source'].apply()
                observed = runs['source'].observe()
                self.assertEqual(observed['state'], 'current')
                self.assertEqual(observed['actual'], observed['desired'])
                self.assertEqual(observed, runs['packaged'].observe())
                self.assertEqual(installed_binary.read_bytes(), kept)
                self.assertEqual(json.loads((root/'control/assets.json').read_text())[str(installed_binary)],
                                 hashlib.sha256(kept).hexdigest())
                self.assertEqual(states[guest.MANAGER]['ActiveState'], 'active')
                self.assertFalse(any(call[0] == 'stop' and guest.MANAGER in call for call in calls[start:]))
                self.assertTrue((root/'bin/subyard-telemetry-run').read_text().startswith('#!/bin/sh\n'))
                self.assertFalse((root/'home/.codex').exists())
                self.assertFalse((root/'etc/codex/managed_config.toml').exists())
                token=(root/'data-private/auth.token').read_bytes()
                target=dict(user='next',uid=runtime.uid,gid=runtime.gid,home=str(runtime.home),group='next')
                recipe=runtime.identity_observe(target,'a'*64)
                runtime.identity_apply(recipe,'quiesce')
                self.assertEqual(states[guest.COLLECTOR]['ActiveState'],'inactive')
                self.assertEqual(states[guest.MANAGER]['ActiveState'],'inactive')
                original_replace=guest.native.os.replace
                def cut_after_unit_replace(source,path,**fds):
                    original_replace(source,path,**fds)
                    parent = Path(f"/proc/self/fd/{fds['dst_dir_fd']}").resolve()
                    if parent/path==runtime.units/guest.COLLECTOR:
                        raise InterruptedError('after first identity unit publication')
                with patch.object(guest.native.os,'replace',side_effect=cut_after_unit_replace):
                    with self.assertRaises(InterruptedError):runtime.identity_apply(recipe,'rebind')
                self.assertEqual((runtime.units/guest.COLLECTOR).stat().st_mode & 0o777,0o644)
                for _ in range(2):runtime.identity_apply(recipe,'rebind')
                self.assertEqual(states[guest.COLLECTOR]['ActiveState'],'inactive')
                self.assertEqual(states[guest.MANAGER]['ActiveState'],'active')
                self.assertEqual((root/'data-private/auth.token').read_bytes(),token)
                self.assertFalse((root/'control/native.json').exists())
                self.assertEqual(marker.read_bytes(),b'history')
                runtime=runtime.identity_runtime(target)
                # The binary exception must not relax configuration admission.
                with self.assertRaisesRegex(ValueError, 'unsafe native file'):
                    guest.native.snapshot(source/'subyard-telemetry')
                installed = root/'library/subyard-telemetry'
                with installed.open('r+b') as output:
                    output.write(b'X')
                with self.assertRaisesRegex(ValueError, 'managed asset changed'):
                    runtime.apply()
                self.assertEqual(installed.read_bytes()[:1], b'X')
                with (source/'subyard-telemetry').open('wb') as output:
                    output.truncate(guest.BINARY_LIMIT+1)
                with self.assertRaisesRegex(ValueError, 'unsafe native file'):
                    guest.Runtime(source, 'dev', True, [], root/'control', root/'data', root/'library', root/'units', root/'bin', etc=root/'etc')

    def test_unselected_guest_needs_only_the_collector_path(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            root = Path(name)
            source = root/'source'
            source.mkdir()
            for path in Path(guest.__file__).parent.glob('*.py'):
                shutil.copyfile(path, source/path.name)
            shutil.copyfile(Path(guest.tomli_w.__file__), source/'tomli_w.py')
            account = types.SimpleNamespace(pw_uid=os.getuid() or 1000, pw_gid=os.getgid(), pw_dir=str(root/'home'))
            def runtime(selected, origin=source):
                return guest.Runtime(origin, 'dev', selected, [], root/'control', root/'data', root/'library',
                                     root/'units', root/'bin', etc=root/'etc')
            with patch.object(guest.pwd, 'getpwnam', return_value=account):
                with self.assertRaisesRegex(ValueError, 'missing profile asset'):
                    runtime(True)
                source_run = runtime(False)
                self.assertEqual(source_run.retained, {root/'library/subyard-telemetry': 0o755})
                (source/'subyard-telemetry').write_bytes(b'\x7fELF synthetic collector')
                packaged_run = runtime(False)
                self.assertEqual(source_run.desired(), packaged_run.desired())
                self.assertEqual(source_run.observe(), {'state': 'absent', 'actual': '', 'desired': ''})
                self.assertEqual(packaged_run.observe(), source_run.observe())
                selected_run = runtime(True)
                self.assertNotEqual(selected_run.desired(), source_run.desired())
                self.assertEqual(selected_run.retained, {})
                # A leftover collector at the retained path is still detected.
                (root/'library').mkdir()
                (root/'library/subyard-telemetry').write_bytes(b'leftover')
                with self.assertRaisesRegex(ValueError, 'unmanaged collector assets'):
                    source_run.observe()

    def test_failure_diagnostic_omits_private_exception_payload(self):
        try:
            raise ValueError('Authorization=Bearer PRIVATE /private/config.json\nPRIVATE_ARGV')
        except ValueError as error:
            message = guest.failure_diagnostic(error)
        self.assertEqual(message, 'telemetry: local reconciliation failed (ValueError at runtime)')
        self.assertNotIn('PRIVATE', message)


if __name__ == '__main__':
    unittest.main()
