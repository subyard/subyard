#!/usr/bin/env python3
"""Install only the marked local collector and native exporter ownership."""
import fcntl
import hashlib
import json
import os
from pathlib import Path
import pwd
import re
import records
import select
import shlex
import shutil
import signal
import sqlite3
import grp
from types import SimpleNamespace
import secrets
import stat
import subprocess
import sys
import tempfile
import time

import native
from records import safefs
import tomli_w

MARKER = b'subyard-telemetry-v1\n'
CUTOVER = '.spool-cutover.json'
PENDING = '.telemetry-archive.pending'
COLLECTOR = 'subyard-telemetry.service'
MANAGER = 'subyard-telemetry-runs.service'
UNIT_PREFIX = '# Managed by Subyard telemetry profile.\n'
BINARY_LIMIT = 64*1024*1024
# The plan's stated effect of rollback-apply.
ROLLBACK_CONSEQUENCE = 'rewrite the collector unit for v0.30 and move the collector token to /var/lib/subyard/telemetry/auth.token'
# Inventory key for the other digest an interrupted asset update may leave on
# disk. Path entries stay single digests, which v0.30 can still read.
REPLACED = 'replaced'
# Codex reads otel.headers_helper from this release on; older clients export
# without Authorization, which the receiver rejects.
CODEX_LINK = Path('/opt/subyard/codex/codex')
CODEX_HELPER_VERSION = (0, 170, 0)


def headers_helper(auth):
    """POSIX shell builtins only; stderr is fixed text and never names the token."""
    return ('#!/bin/sh\n# Managed by Subyard telemetry profile.\n'
            "fail() { printf 'subyard-telemetry: %s\\n' \"$1\" >&2; exit 1; }\n"
            "[ \"$#\" -eq 0 ] || fail 'unexpected arguments'\n"
            'token=\n'
            f'IFS= read -r token 2>/dev/null <{shlex.quote(str(auth))} || [ -n "$token" ] || fail \'collector token unavailable\'\n'
            'case "$token" in *[!0-9a-f]*) fail \'invalid collector token\' ;; esac\n'
            '[ "${#token}" -eq 64 ] || fail \'invalid collector token\'\n'
            'printf \'{"Authorization":"Bearer %s"}\\n\' "$token"\n').encode()


def codex_helper_supported(link=None):
    """False only for a Subyard-provisioned Codex release older than the helper."""
    try:
        target = os.readlink(CODEX_LINK if link is None else link)
    except OSError:
        return True
    match = re.fullmatch(r'releases/([0-9]+)\.([0-9]+)\.([0-9]+)/bin/codex', target)
    return match is None or tuple(map(int, match.groups())) >= CODEX_HELPER_VERSION


# Drop-in directories systemd applies to every service, such as the LXC guest's.
TYPE_DROPINS = (Path('/run/systemd/system/service.d'), Path('/etc/systemd/system/service.d'), Path('/usr/lib/systemd/system/service.d'))
# Lines of the stock LXC guest drop-in after its [Service] header, the type-wide one a recognized
# previous unit may carry. The host's bwrap policy accepts the same set.
LXC_DROPIN = ('ProcSubset=all', 'ProtectProc=default', 'ProtectControlGroups=no', 'ProtectKernelTunables=no',
              'NoNewPrivileges=no', 'LoadCredential=', 'PrivateNetwork=no', 'ImportCredential=')


def systemctl(*args):
    result = subprocess.run(['systemctl', *args], stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                            stderr=subprocess.DEVNULL, timeout=15)
    if result.returncode:
        raise ValueError('system manager unavailable')
    return result.stdout.decode().strip()


def stock_dropin(path, before=None):
    """Whether a type-wide drop-in is the stock LXC guest's: a root-owned regular file, older than
    `before` when given, of a [Service] header and stock lines only."""
    try:
        native.safe(path)
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        with os.fdopen(fd, 'rb') as stream:
            info = os.fstat(stream.fileno())
            if (not stat.S_ISREG(info.st_mode) or (before is not None and info.st_mtime_ns >= before)
                    or any(entry.st_uid != os.getuid() or entry.st_mode & 0o022 for entry in (info, path.parent.stat()))):
                return False
            content = stream.read(native.CONFIG_LIMIT+1)
    except (OSError, ValueError):
        return False
    lines = content.removesuffix(b'\n').split(b'\n')
    return (len(content) <= native.CONFIG_LIMIT and lines[0] == b'[Service]'
            and all(line.decode('latin-1') in LXC_DROPIN for line in lines[1:]))


class Runtime:
    def __init__(self, source, user, selected, agents, control=Path('/var/lib/subyard/telemetry-control'),
                 data=Path('/var/lib/subyard/telemetry'), library=Path('/usr/local/lib/subyard-telemetry'),
                 units=Path('/etc/systemd/system'), commands=Path('/usr/local/bin'), account=None, etc=Path('/etc')):
        self.source, self.user, self.selected, self.agents = source, user, selected, agents
        self.etc = etc
        self.templates = {agent: content for agent in agents
                          if (content := native.snapshot(source/(agent+'.template'))) is not None}
        self.control, self.data, self.library, self.units, self.commands = control, data, library, units, commands
        self.operational = data.with_name(data.name+'-private')
        self.auth = self.operational/'auth.token'
        account = account or pwd.getpwnam(user)
        self.uid, self.gid, self.home = account.pw_uid, account.pw_gid, Path(account.pw_dir)
        if self.uid == 0 or not user.replace('_', '').replace('-', '').isalnum():
            raise ValueError('developer account required')
        self.endpoint = 'http://127.0.0.1:14318'
        self.assets = {}
        # Shared support is outside the telemetry inventory. An older runtime
        # can roll back its assets while leaving this file in place.
        self.safefs_path = library.parent/'subyard-python/safefs.py'
        self.safefs_content = native.snapshot(Path(safefs.__file__).resolve())
        if self.safefs_content is None:
            raise ValueError('missing profile asset')
        # Paths whose contents an unselected run neither needs nor installs. They
        # stay known so that leftovers are detected and kept unchanged.
        self.retained = {}
        # Running launchers can import records during publication.
        for name in ('records.py', 'native.py', 'launcher.py', 'processes.py', 'forward.py',
                     'resources.py', 'sources.py', 'broker-correlation.py', 'tomli_w.py', 'subyard-telemetry'):
            mode = 0o755 if name in ('subyard-telemetry', 'broker-correlation.py') else 0o644
            if name == 'subyard-telemetry' and not selected:
                self.retained[library/name] = mode
                continue
            content = native.snapshot(source/name, max_bytes=self.asset_limit(library/name))
            if content is None:
                raise ValueError('missing profile asset')
            self.assets[library/name] = (content, mode)
        # The profile's TELEMETRY_DISK_BUDGET; the collector default applies without it.
        budget = os.environ.get('SUBYARD_TELEMETRY_DISK_BYTES', '')
        budget = ' --disk-bytes '+budget if re.fullmatch('[1-9][0-9]{0,18}', budget) else ''
        self.starts = {COLLECTOR: f'/usr/bin/setpriv --no-new-privs {library}/subyard-telemetry serve --data-dir {data} --listen 127.0.0.1:14318 --auth-file {self.auth}{budget}',
                       MANAGER: f'/usr/bin/setpriv --no-new-privs /usr/bin/python3 -B {library}/launcher.py manager --data-dir {data} --delegate'}
        self.helper = library/'otlp-headers'
        self.assets[self.helper] = (headers_helper(self.auth), 0o755)
        for path, content in self.unit_files(self.starts).items():
            self.assets[path] = (content, 0o644)
        # Runtime format 1, which rollback-apply prepares for releases up to v0.30: the
        # collector reads its bearer from the analytics directory and has no disk budget.
        self.rollback_auth = data/'auth.token'
        self.rollback_starts = {**self.starts, COLLECTOR: f'/usr/bin/setpriv --no-new-privs {library}/subyard-telemetry serve --data-dir {data} --listen 127.0.0.1:14318 --auth-file {self.rollback_auth}'}
        self.rollback_units = self.unit_files(self.rollback_starts)
        for command, target, extra in [('subyard-telemetry-run', 'launcher.py', 'run '), ('subyard-telemetry-forward', 'forward.py', '')]:
            self.assets[commands/command] = (('#!/bin/sh\n'+UNIT_PREFIX+'exec /usr/bin/python3 -B '+str(library/target)+' '+extra+'"$@"\n').encode(), 0o755)

    def unit_files(self, starts):
        common = f'[Unit]\nDescription=Subyard local telemetry\n[Service]\nType=simple\nUser={self.user}\nGroup={self.gid}\nUMask=0077\nLimitCORE=0\nNoNewPrivileges=true\nRestart=on-failure\nRestartSec=5\nTimeoutStopSec=10\n'
        collector = UNIT_PREFIX+common+f'ExecStart={starts[COLLECTOR]}\nMemoryMax=256M\nCPUQuota=25%\nTasksMax=64\nKillMode=control-group\n[Install]\nWantedBy=multi-user.target\n'
        # Workloads inherit their caller's ceilings. Do not put a low collector
        # MemoryMax on this delegated ancestor or kill workloads on opt-out.
        manager = UNIT_PREFIX+common+f'Slice=user-{self.uid}.slice\nExecStart={starts[MANAGER]}\nDelegate=cpu memory io pids\nKillMode=process\n[Install]\nWantedBy=multi-user.target\n'
        return {self.units/COLLECTOR: collector.encode(), self.units/MANAGER: manager.encode()}

    def asset_limit(self, path):
        return BINARY_LIMIT if path == self.library/'subyard-telemetry' else native.CONFIG_LIMIT

    def identity_runtime(self,record):
        wanted = Runtime(self.source,record['user'],self.selected,self.agents,self.control,self.data,self.library,self.units,self.commands,
                       account=SimpleNamespace(pw_uid=record['uid'],pw_gid=record['gid'],pw_dir=record['home']), etc=self.etc)
        return wanted

    def identity_files(self,wanted):
        return {str(self.units/unit):[hashlib.sha256(self.assets[self.units/unit][0]).hexdigest(),hashlib.sha256(wanted.assets[self.units/unit][0]).hexdigest()] for unit in (COLLECTOR,MANAGER)}

    def identity_observe(self,target,source):
        observed=self.observe()
        if observed['state'] not in ('current','absent'):
            raise ValueError('identity requires converged telemetry ownership')
        original=dict(user=self.user,uid=self.uid,gid=self.gid,home=str(self.home),group=grp.getgrgid(self.gid).gr_name)
        wanted=self.identity_runtime(target)
        manager={unit:{key:self.manager(unit).get(key,'') for key in ('ActiveState','UnitFileState')} for unit in (COLLECTOR,MANAGER)} if self.owned() else {}
        # A failing unit cannot be restored to its recorded state after the rename.
        if any(fields['ActiveState']=='failed' for fields in manager.values()):
            raise ValueError('telemetry identity requires stable owner services')
        return dict(schema=1,source=source,original=original,target=target,installed=self.owned(),files=self.identity_files(wanted),manager=manager)

    def identity_check(self,recipe,phase):
        wanted=self.identity_runtime(recipe['target'])
        if recipe['original']['uid']!=recipe['target']['uid'] or recipe['original']['gid']!=recipe['target']['gid'] or recipe['files']!=self.identity_files(wanted):
            raise ValueError('telemetry identity scope changed')
        if self.owned()!=recipe['installed']:
            raise ValueError('telemetry identity ownership changed')
        if not recipe['installed']:return wanted
        for path,(content,mode) in self.assets.items():
            allowed=[content]
            if phase=='rebind' and path in wanted.assets:allowed.append(wanted.assets[path][0])
            if self.asset_snapshot(path) not in allowed or stat.S_IMODE(path.stat().st_mode)!=mode:
                raise ValueError('telemetry identity asset changed')
        if not self.retained_current(self.saved_inventory()):
            raise ValueError('telemetry identity asset changed')
        for unit in (COLLECTOR,MANAGER):
            try:fields=self.manager(unit)
            except ValueError:
                if phase!='rebind':raise
                fields=wanted.manager(unit)
            before=recipe['manager'][unit]
            if fields.get('UnitFileState','')!=before['UnitFileState'] or not (fields.get('ActiveState','') in (before['ActiveState'],'inactive') or (getattr(self,'identity_rebooted',False) and (fields.get('ActiveState')=='failed' or before['UnitFileState']=='enabled' and fields.get('ActiveState')=='active'))):
                raise ValueError('telemetry identity service intent changed')
        return wanted

    def identity_apply(self,recipe,phase):
        wanted=self.identity_check(recipe,phase)
        if not recipe['installed']:return
        if phase=='quiesce':
            for unit in (COLLECTOR,MANAGER):systemctl('stop',unit)
            return
        inventory_path=self.control/'assets.json'
        before=native.snapshot(inventory_path)
        inventory=json.loads(before)
        for unit in (COLLECTOR,MANAGER):
            path=self.units/unit
            if inventory.get(str(path)) not in recipe['files'][str(path)]:
                raise ValueError('telemetry asset inventory changed')
            content,mode=wanted.assets[path]
            if self.asset_snapshot(path)!=content:
                native.publish(path,content,native.snapshot(path),mode=mode)
            inventory[str(path)]=hashlib.sha256(content).hexdigest()
        after=(json.dumps(inventory)+'\n').encode()
        if before!=after:native.publish(inventory_path,after,before)
        systemctl('daemon-reload')
        for unit in (MANAGER,COLLECTOR):
            if recipe['manager'][unit]['ActiveState']=='active':systemctl('start',unit)
            elif wanted.manager(unit).get('ActiveState')!='inactive':systemctl('stop',unit)
            if wanted.manager(unit).get('ActiveState')!=recipe['manager'][unit]['ActiveState']:raise ValueError('telemetry identity intent was not restored')

    def asset_snapshot(self, path):
        return native.snapshot(path, max_bytes=self.asset_limit(path))

    def saved_inventory(self):
        return json.loads(native.snapshot(self.control/'assets.json') or b'{}')

    def skill_delivery(self, mode):
        result = subprocess.run([sys.executable, '-B', self.source/'agent-skills.py', mode,
                                 '--provider', 'profile-telemetry', '--source', self.source/'skills',
                                 '--home', self.home, '--uid', str(self.uid), '--gid', str(self.gid)],
                                stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15)
        if mode == 'check-registered' and result.returncode == 10:
            return False
        if result.returncode:
            raise ValueError('telemetry skill delivery failed')
        return True

    def recorded_digests(self, saved, path):
        replaced = saved.get(REPLACED)
        if not isinstance(replaced, dict) or str(path) not in replaced:
            return [saved.get(str(path))]
        return [saved.get(str(path)), replaced[str(path)]]

    def encode_inventory(self, recorded, replaced):
        return (json.dumps({**recorded, REPLACED: replaced} if replaced else recorded)+'\n').encode()

    def retained_current(self, saved):
        """A retained path is absent, or exactly what the inventory recorded with its mode."""
        for path, mode in self.retained.items():
            content = self.asset_snapshot(path)
            if content is None:
                continue
            if saved.get(str(path)) != hashlib.sha256(content).hexdigest() or stat.S_IMODE(path.stat().st_mode) != mode:
                return False
        return True

    def inventory(self):
        """Recorded hashes: delivered assets, and retained paths as they remain."""
        recorded = {str(path): hashlib.sha256(data).hexdigest() for path, (data, _) in self.assets.items()}
        for path in self.retained:
            content = self.asset_snapshot(path)
            if content is not None:
                recorded[str(path)] = hashlib.sha256(content).hexdigest()
        return (json.dumps(recorded)+'\n').encode()

    def owned(self):
        if not self.control.exists():
            return False
        for directory in (self.control, self.library):
            if directory.is_symlink() or directory.stat().st_uid != os.getuid() or directory.stat().st_mode & 0o022:
                raise ValueError('foreign control directory')
        if native.snapshot(self.control/'marker') != MARKER:
            raise ValueError('foreign control state')
        return True

    def token(self, path=None, strict=True):
        path = self.auth if path is None else path
        value = native.snapshot(path)
        if value is None or path.stat().st_uid != self.uid or stat.S_IMODE(path.stat().st_mode) != 0o600:
            raise ValueError('private collector token required')
        token = value.decode(errors='replace').strip()
        if len(token) != 64 or any(c not in '0123456789abcdef' for c in token):
            raise ValueError('invalid collector token')
        # The headers helper reads only an exact first line; the collector trims.
        if strict and value not in (token.encode(), token.encode()+b'\n'):
            raise ValueError('noncanonical collector token')
        return token

    def private_directory(self, path, refusal):
        try:
            parent = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            try:
                if self.uid != os.geteuid():
                    try:
                        os.stat(path.name, dir_fd=parent, follow_symlinks=False)
                    except FileNotFoundError:
                        self.create_developer_directory(parent, path.name)
                fd, _ = safefs.open_dir(path.name, dir_fd=parent, owner=(self.uid, self.gid))
                try:
                    info = os.fstat(fd)
                    if stat.S_IMODE(info.st_mode) != 0o700 or info.st_gid != self.gid:
                        raise ValueError(refusal)
                finally:
                    os.close(fd)
            finally:
                os.close(parent)
        except (OSError, safefs.Unsafe):
            raise ValueError(refusal) from None

    def create_developer_directory(self, parent, name):
        # The developer cannot write the destination parent. Give it access
        # through a descriptor to an empty staging directory on the same mount.
        # The outer directory keeps staging inaccessible by pathname.
        with tempfile.TemporaryDirectory(prefix='.telemetry-dir-', dir=f'/proc/self/fd/{parent}') as staging:
            path = Path(staging)/'create'
            os.mkdir(path, 0o700)
            fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            try:
                os.fchown(fd, -1, self.gid)
                os.fchmod(fd, 0o770)
                with native.developer_files(self.uid, self.gid):
                    safefs.ensure_dir(name, dir_fd=fd)
                try:
                    safefs.rename_noreplace(name, name, src_dir_fd=fd, dst_dir_fd=parent)
                except FileExistsError:
                    # Another apply created it. The caller validates its metadata.
                    pass
            finally:
                os.close(fd)

    def safefs_state(self):
        path = self.safefs_path
        native.safe(path)
        directory_mode = None
        if path.parent.exists():
            directory = path.parent.stat()
            if not stat.S_ISDIR(directory.st_mode) or directory.st_uid != os.geteuid() or directory.st_mode & 0o022:
                raise ValueError('foreign control directory')
            directory_mode = stat.S_IMODE(directory.st_mode)
        before = self.asset_snapshot(path)
        if before is None:
            return None, None
        info = path.stat()
        if info.st_uid != os.geteuid() or info.st_mode & 0o022:
            raise ValueError('managed asset changed')
        return before, stat.S_IMODE(info.st_mode) if directory_mode in (0o755, 0o1755) else None

    def publish_safefs(self):
        before, mode = self.safefs_state()
        if mode != 0o644 or before != self.safefs_content:
            safefs.ensure_dir(self.safefs_path.parent, mode=0o755)
            native.publish(self.safefs_path, self.safefs_content, before, mode=0o644)

    def prepare_auth(self):
        self.private_directory(self.operational, 'foreign telemetry operational directory')
        if not self.auth.exists():
            # Keep the v0.30 bearer: the analytics copy, else the one its home-config receipt records.
            old = self.data/'auth.token'
            token = (self.token(old, strict=False) if old.exists() or old.is_symlink()
                     else native.legacy_token(self.control/'native.json') or secrets.token_hex(32))
            native.publish(self.auth, (token+'\n').encode(), None, self.uid, self.gid)
        token = self.token(strict=False)
        current = native.snapshot(self.auth)
        if current != (token+'\n').encode():
            # A hand-restored copy may carry CRLF or blanks the helper rejects.
            native.publish(self.auth, (token+'\n').encode(), current, self.uid, self.gid)
        return self.token()

    def manager(self, unit, startup=False):
        raw = systemctl('show', unit, '--property=ActiveState,SubState,MainPID,UnitFileState,LoadState,FragmentPath,DropInPaths,User,Group,Type,ExecStart,KillMode,UMask,LimitCORE,NoNewPrivileges,AmbientCapabilities,ExecStartPre,ExecStartPost,ExecStop,ExecStopPost,Environment,EnvironmentFiles,NeedDaemonReload,MemoryMax,TasksMax,CPUQuotaPerSecUSec,Delegate'+(',Job' if startup else ''))
        fields = dict(line.split('=', 1) for line in raw.splitlines() if '=' in line)
        # A failed service waiting for Restart= has failed, e.g. after a reboot
        # during a rename, while its unit still names the previous account.
        if not startup and fields.get('ActiveState') == 'activating' and fields.get('SubState') == 'auto-restart':
            fields['ActiveState'] = 'failed'
        if fields.get('LoadState') not in ('not-found', None):
            dropins = fields.get('DropInPaths', '').split()
            if any(Path(path).parent not in TYPE_DROPINS for path in dropins):
                raise ValueError('foreign loaded unit')
            # Apply stops the loaded unit: the workloads the manager delegates must survive it.
            if unit == MANAGER and fields.get('KillMode') != 'process':
                raise ValueError('foreign loaded unit stop policy')
            if not self.loaded_as(unit, fields, self.starts[unit]):
                # Any other loaded unit is a previous one of ours, stale until apply rewrites it,
                # only when it is a file the inventory recorded: a previous file loaded from disk,
                # or one an interrupted apply replaced after recording both versions. Over the
                # loaded current file, any difference comes from a drop-in.
                path = self.units/unit
                content = self.asset_snapshot(path)
                saved = self.saved_inventory()
                replacing = isinstance(saved.get(REPLACED), dict) and str(path) in saved[REPLACED]
                if (fields.get('FragmentPath') != str(path) or content is None
                        or hashlib.sha256(content).hexdigest() not in self.recorded_digests(saved, path)
                        or not (replacing or fields.get('NeedDaemonReload') == 'no' and content != self.assets[path][0])):
                    raise ValueError('foreign loaded unit envelope')
                # Its own fields are ours; type-wide drop-ins may only be the stock LXC one. Before
                # daemon-reload, one changed after the unit file was replaced is not the loaded one.
                before = None if fields.get('NeedDaemonReload') == 'no' else path.stat().st_mtime_ns
                if not all(stock_dropin(Path(dropin), before) for dropin in dropins):
                    raise ValueError('foreign loaded unit drop-in')
        return fields

    def loaded_as(self, unit, fields, command):
        """Whether a loaded unit's own fields are exactly ours with this startup."""
        expected = {'FragmentPath': str(self.units/unit), 'User': self.user, 'Group': str(self.gid),
                    'Type': 'simple', 'UMask': '0077', 'LimitCORE': '0', 'AmbientCapabilities': '',
                    'ExecStartPre': '', 'ExecStartPost': '', 'ExecStop': '', 'ExecStopPost': '', 'Environment': '', 'EnvironmentFiles': ''}
        if unit == COLLECTOR:
            expected.update(KillMode='control-group', MemoryMax=str(256*1024*1024), TasksMax='64', CPUQuotaPerSecUSec='250ms')
        else:
            expected['Delegate'] = 'yes'
        start = fields.get('ExecStart', '')
        return (all(fields.get(key, '') == value for key, value in expected.items())
                and start.startswith('{ path='+command.split()[0]+' ; argv[]='+command+' ; ignore_errors=no ; ')
                and start.count('argv[]=') == 1
                and fields.get('NoNewPrivileges') in (('yes', 'no') if fields.get('DropInPaths', '').split() else ('yes',)))

    def running_no_new_privs(self, fields):
        if fields.get('ActiveState') != 'active':
            return True
        try:
            pid = int(fields.get('MainPID', '0'))
            if pid <= 1:
                return False
            status = dict(line.split(':', 1) for line in (Path('/proc')/str(pid)/'status').read_text().splitlines() if ':' in line)
            return status.get('NoNewPrivs', '').strip() == '1' and all(int(uid) == self.uid for uid in status.get('Uid', '').split()) and bool(status.get('Uid'))
        except (OSError, ValueError):
            return False

    def startup(self):
        if not self.selected or not self.owned():
            return {'state': 'drift'}
        states = [self.manager(unit, startup=True) for unit in (COLLECTOR, MANAGER)]
        anchored = bool(re.fullmatch('[0-9]{1,9}', os.environ.get('SUBYARD_STARTUP_REMAINING_MS', '')))
        def pending(state):
            if state.get('UnitFileState') != 'enabled' or state.get('ActiveState') == 'failed':
                return False
            if state.get('SubState') == 'auto-restart':
                return anchored
            job = state.get('Job', '').split()
            return bool(job and job[0].isdigit() and int(job[0]) > 0)
        # Queued jobs and this start's auto-restart are distinct from stable failure.
        if any(pending(state) for state in states):
            return {'state': 'starting', 'reason': 'collector services are starting',
                    'hint': 'run yard migrate to repair; inspect the guest collector services'}
        return {'state': 'ready' if all(state.get('ActiveState') == 'active' and state.get('UnitFileState') == 'enabled' for state in states) else 'drift'}

    def reconcile_native(self, check, token=None):
        receipt = self.control/'native.json'
        # Before the first apply after v0.30 the bearer is still in the analytics directory.
        for path in (self.auth, self.data/'auth.token'):
            if token is None and (self.selected or receipt.exists()) and native.snapshot(path) is not None:
                token = self.token(path, strict=False)
        return native.reconcile(self.etc, self.home, receipt, self.agents if self.selected else [],
                                self.endpoint, token, tomli_w, self.uid, self.gid, self.templates, check,
                                helper=str(self.helper))

    def observe(self):
        installed = self.owned()
        if not installed and any(path.exists() or path.is_symlink() for path in [*self.assets, *self.retained]):
            raise ValueError('unmanaged collector assets')
        native_current = self.reconcile_native(check=True) if installed or self.selected else True
        if not installed:
            result = {'state': 'stale' if self.selected else 'absent', 'actual': '', 'desired': self.desired() if self.selected else ''}
            if self.selected:
                result['diagnostics'] = [{'parameter': 'telemetry.runtime', 'installed': 'absent', 'desired': 'present'}]
            return result
        files = {str(path): (hashlib.sha256(self.asset_snapshot(path) or b'').hexdigest(), stat.S_IMODE(path.stat().st_mode) if path.exists() else None)
                 for path in [*self.assets, *self.retained]}
        saved = self.saved_inventory()
        inventory_current = saved == {path: digest for path, (digest, mode) in files.items() if mode is not None}
        ready = all(self.asset_snapshot(path) == content and stat.S_IMODE(path.stat().st_mode) == mode for path, (content, mode) in self.assets.items())
        support, support_mode = self.safefs_state()
        files[str(self.safefs_path)] = (hashlib.sha256(support or b'').hexdigest(), support_mode)
        ready = ready and support == self.safefs_content and support_mode == 0o644
        cutover_pending = self.selected and (self.data/CUTOVER).exists()
        policy_status = self.policy_status() if self.selected else 'present'
        policy_current = policy_status == 'present'
        ready = ready and not cutover_pending and not (self.data/'auth.token').exists() and inventory_current and all(files[str(path)][1] in (None, mode) for path, mode in self.retained.items())
        states = {unit: self.manager(unit) for unit in (COLLECTOR, MANAGER)}
        active = states[COLLECTOR].get('ActiveState') == 'active'
        enabled = states[COLLECTOR].get('UnitFileState') == 'enabled'
        converged = ready and active == self.selected and enabled == self.selected
        converged = converged and all(self.running_no_new_privs(state) for state in states.values())
        if self.selected:
            converged = converged and states[MANAGER].get('ActiveState') == 'active' and states[MANAGER].get('UnitFileState') == 'enabled'
        try:
            token = self.token()
        except (OSError, ValueError):
            token = None
            converged = False
        converged = converged and native_current
        skill_current = not self.selected or self.skill_delivery('check-registered')
        actual = hashlib.sha256(json.dumps({'files': files, 'inventory_current': inventory_current, 'cutover_pending': cutover_pending, 'states': states, 'native_current': native_current, 'skill_current': skill_current, 'policy_current': policy_current}, sort_keys=True).encode()).hexdigest()
        converged = converged and skill_current and policy_current
        desired = self.desired()
        result = {'state': 'current' if converged else 'stale', 'actual': desired if converged else actual, 'desired': desired}
        if not converged:
            diagnostics = []
            if cutover_pending:
                diagnostics.append({'parameter': 'telemetry.cutover', 'installed': 'pending', 'desired': 'complete'})
            if token is None:
                diagnostics.append({'parameter': 'telemetry.receiver-auth', 'installed': 'missing or invalid', 'desired': 'present'})
            if not policy_current:
                diagnostics.append({'parameter': 'telemetry.spool-policy', 'installed': policy_status, 'desired': 'present'})
            if not inventory_current:
                diagnostics.append({'parameter': 'telemetry.inventory', 'installed': 'changed', 'desired': 'current'})
            for path, (content, mode) in self.assets.items():
                snapshot = self.asset_snapshot(path)
                if snapshot != content:
                    diagnostics.append({'parameter': 'telemetry.asset.' + path.name, 'installed': 'missing' if snapshot is None else 'changed', 'desired': 'current'})
                elif stat.S_IMODE(path.stat().st_mode) != mode:
                    diagnostics.append({'parameter': 'telemetry.mode.' + path.name, 'installed': 'mismatch', 'desired': 'matched'})
            if support != self.safefs_content:
                diagnostics.append({'parameter': 'telemetry.asset.safefs.py', 'installed': 'missing' if support is None else 'changed', 'desired': 'current'})
            elif support_mode != 0o644:
                diagnostics.append({'parameter': 'telemetry.mode.safefs.py', 'installed': 'mismatch', 'desired': 'matched'})
            if not native_current:
                diagnostics.append({'parameter': 'telemetry.native', 'installed': 'changed', 'desired': 'current'})
            if not skill_current:
                diagnostics.append({'parameter': 'telemetry.skill', 'installed': 'absent', 'desired': 'registered'})
            if self.selected and 'codex' in self.agents and not codex_helper_supported():
                diagnostics.append({'parameter': 'telemetry.codex-headers-helper', 'installed': 'missing', 'desired': 'present'})
            for unit in (COLLECTOR, MANAGER):
                expected = self.selected if unit == COLLECTOR else True if self.selected else None
                if expected is None:
                    continue
                for field, wanted in [('ActiveState', 'active' if expected else 'inactive'), ('UnitFileState', 'enabled' if expected else 'disabled')]:
                    value = states[unit].get(field, '')
                    if (value == ('active' if field == 'ActiveState' else 'enabled')) != expected:
                        diagnostics.append({'parameter': 'telemetry.' + unit + '.' + field, 'installed': value if value in ('active', 'inactive', 'enabled', 'disabled', 'failed', 'not-found') else 'unknown', 'desired': wanted})
            if not diagnostics:
                diagnostics.append({'parameter': 'telemetry.runtime', 'installed': 'unknown', 'desired': 'current'})
            result['diagnostics'] = diagnostics
        return result

    def raw_refused(self):
        """Whether the collector's private marker says raw OTLP is refused for space."""
        try:
            directory = os.open(self.data, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        except FileNotFoundError:
            return False
        try:
            fd = os.open('raw-otlp-refused', os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory)
        except FileNotFoundError:
            return False
        finally:
            os.close(directory)
        try:
            info = os.fstat(fd)
            if (not stat.S_ISREG(info.st_mode) or info.st_uid != self.uid or info.st_nlink != 1
                    or stat.S_IMODE(info.st_mode) != 0o600 or info.st_size > 128):
                raise ValueError('foreign telemetry refusal marker')
            return re.fullmatch(rb'raw_otlp_refused_since=[0-9TZ:.-]{1,40}\n', os.pread(fd, 129, 0)) is not None
        finally:
            os.close(fd)

    def desired(self):
        value = {'assets': {str(path): [hashlib.sha256(data).hexdigest(), mode] for path, (data, mode) in self.assets.items()},
                 'safefs': hashlib.sha256(self.safefs_content).hexdigest(),
                 'selected': self.selected, 'agents': self.agents,
                 'templates': {agent: hashlib.sha256(content).hexdigest() for agent, content in self.templates.items()}}
        if self.retained:
            # Paths and modes only: packaged and source unselected runs agree.
            value['retained'] = {str(path): mode for path, mode in self.retained.items()}
        return hashlib.sha256(json.dumps(value, sort_keys=True).encode()).hexdigest()

    def as_developer(self, operation):
        # Create and change producer-owned paths with the producer's privileges.
        if os.getuid() == self.uid:
            operation()
            return
        child = os.fork()
        if child == 0:
            code = 1
            try:
                os.setgroups([])
                os.setgid(self.gid)
                os.setuid(self.uid)
                os.umask(0o077)
                operation()
                code = 0
            finally:
                os._exit(code)
        if os.waitpid(child, 0)[1] != 0:
            raise ValueError('telemetry developer operation failed')

    def retire_observer_lock(self, path):
        try:
            fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_RDWR | os.O_NOFOLLOW, 0)
        except FileExistsError:
            fd = os.open(path, os.O_PATH | os.O_NOFOLLOW)
        try:
            info = os.fstat(fd)
            if (not stat.S_ISREG(info.st_mode) or info.st_uid != self.uid or info.st_nlink != 1
                    or stat.S_IMODE(info.st_mode) not in (0, 0o600)):
                raise ValueError('foreign legacy observer lock')
            if stat.S_IMODE(info.st_mode) != 0:
                with open(f'/proc/self/fd/{fd}', 'rb') as handle:
                    os.fchmod(handle.fileno(), 0)
                    os.fsync(handle.fileno())
            self.stop_lock_users(info)
        finally:
            os.close(fd)

    def stop_lock_users(self, lock):
        script = os.fsencode(self.library/'launcher.py')
        for process in Path('/proc').iterdir():
            if not process.name.isdecimal():
                continue
            try:
                if process.stat().st_uid != self.uid or script not in (process/'cmdline').read_bytes().split(b'\0'):
                    continue
                pidfd = os.pidfd_open(int(process.name))
            except (FileNotFoundError, ProcessLookupError):
                continue
            except OSError:
                continue
            try:
                for entry in (process/'fd').iterdir():
                    try:
                        matches = os.path.samestat(entry.stat(), lock)
                    except FileNotFoundError:
                        continue
                    if matches:
                        signal.pidfd_send_signal(pidfd, signal.SIGKILL)
                        exited = select.poll()
                        exited.register(pidfd, select.POLLIN)
                        if not exited.poll(5000):
                            raise ValueError('legacy telemetry observer did not stop')
                        break
            except (FileNotFoundError, ProcessLookupError):
                pass
            except OSError:
                # Ptrace access to /proc/PID/fd can require a matching GID.
                # Leave an unidentified process alone. The fence still blocks
                # its legacy writes, even when it holds the retired lock open.
                pass
            finally:
                os.close(pidfd)

    def prepare_store(self):
        account = [] if self.uid == os.getuid() else ['--reuid', str(self.uid), '--regid', str(self.gid), '--clear-groups']
        subprocess.run(['/usr/bin/setpriv', '--no-new-privs', *account, self.library/'subyard-telemetry',
                        'prepare', '--data-dir', self.data], check=True, timeout=30,
                       stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.as_developer(lambda: records.load_producer_state(self.data, self.library/'subyard-telemetry'))

    def policy_status(self):
        try:
            self.as_developer(lambda: records.load_producer_state(self.data, self.library/'subyard-telemetry'))
        except (OSError, ValueError, KeyError, TypeError):
            return 'invalid' if (self.data/'spool-policy.json').exists() else 'missing'
        return 'present'

    def inspect_archive(self, archive):
        def inspect():
            directory = os.open(archive, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            try:
                info = os.fstat(directory)
                if info.st_uid != self.uid or stat.S_IMODE(info.st_mode) != 0o700:
                    raise ValueError('foreign archive directory')
                fd = os.open('telemetry.sqlite', os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory)
            finally:
                os.close(directory)
            with os.fdopen(fd, 'rb') as source:
                info = os.fstat(fd)
                if (not stat.S_ISREG(info.st_mode) or info.st_uid != self.uid or info.st_nlink != 1
                        or stat.S_IMODE(info.st_mode) != 0o400):
                    raise ValueError('foreign archive database')
                # SQLite resolves /proc/self/fd paths before opening them.
                # Inspect a private copy read from the validated descriptor.
                with tempfile.TemporaryDirectory(prefix='subyard-telemetry-archive-', dir='/tmp') as work:
                    copy = Path(work)/'archive.sqlite'
                    with copy.open('xb') as target:
                        shutil.copyfileobj(source, target)
                    db = sqlite3.connect(copy.as_uri()+'?mode=ro&immutable=1', uri=True)
                    try:
                        if not db.execute("SELECT value FROM metadata WHERE key='origin_id'").fetchone():
                            raise ValueError('missing archive origin')
                    finally:
                        db.close()

        try:
            self.as_developer(inspect)
        except (OSError, ValueError, sqlite3.Error):
            raise ValueError('legacy archive collision') from None

    def cutover_receipt(self):
        receipt = self.data/CUTOVER
        database = self.data/'telemetry.sqlite'
        existing = native.snapshot(receipt, max_bytes=4096)
        if existing is None:
            legacy = any((self.data/name).exists() for name in ('outbox', 'runs', 'runs-v4', 'manager.lock', 'auth.token'))
            try:
                fd = os.open(database, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
            except FileNotFoundError:
                info = None
            else:
                try:
                    info = os.fstat(fd)
                    if (not stat.S_ISREG(info.st_mode) or info.st_uid != self.uid or info.st_nlink != 1
                            or stat.S_IMODE(info.st_mode) != 0o600):
                        raise ValueError('foreign telemetry database')
                    version = int.from_bytes(os.pread(fd, 4, 60), 'big')
                finally:
                    os.close(fd)
                legacy = version in (1, 2, 3, 4) or (version == 0 and legacy)
            if not legacy:
                return False
            if (self.data/PENDING).exists() or (self.data/PENDING).is_symlink():
                raise ValueError('legacy archive staging collision')
            name = 'archive'
            for number in range(2, 4098):
                archive = self.data/name
                if not archive.exists() and not archive.is_symlink():
                    break
                self.inspect_archive(archive)
                name = f'archive-{number}'
            else:
                raise ValueError('legacy archive capacity')
            value = dict(device=info.st_dev if info else 0, inode=info.st_ino if info else 0,
                         archive=name, nonce=secrets.token_hex(16), started=records.timestamp(records.now()))
        else:
            info = receipt.lstat()
            if info.st_uid != self.uid or stat.S_IMODE(info.st_mode) != 0o600:
                raise ValueError('foreign telemetry cutover receipt')
            value = json.loads(existing)
        # The collector validates the receipt, staging and archive when it publishes the archive.
        return value

    def retire_legacy_producers(self):
        for name in ('runs', 'runs-v4'):
            path = self.data/name
            if path.is_symlink():
                raise ValueError('foreign legacy run directory')
            if not path.exists():
                continue
            with records.directory_lock(path, fcntl.LOCK_EX) as fd:
                info = os.fstat(fd)
                mode = stat.S_IMODE(info.st_mode)
                if info.st_uid != self.uid or mode not in (0o700, 0o500):
                    raise ValueError('foreign legacy run directory')
                if mode == 0o700 or (path/'manager.lock').exists():
                    self.retire_observer_lock(path/'manager.lock')
                os.fchmod(fd, 0o500)
                os.fsync(fd)
        self.retire_observer_lock(self.data/'manager.lock')
        path = self.data/'outbox'
        path.mkdir(mode=0o700, exist_ok=True)
        directory = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            info = os.fstat(directory)
            mode = stat.S_IMODE(info.st_mode)
            if info.st_uid != self.uid or mode not in (0o700, 0o1700, 0o1500):
                raise ValueError('foreign producer outbox')
            lock = path/'.lock'
            if lock.exists() and not lock.is_symlink() and stat.S_IMODE(lock.lstat().st_mode) == 0:
                info = lock.lstat()
                if not stat.S_ISREG(info.st_mode) or info.st_uid != self.uid or info.st_nlink != 1:
                    raise ValueError('foreign producer outbox lock')
                # Resume retirement if interrupted after the lock mode changed.
                os.fchmod(directory, 0o1500)
                os.fsync(directory)
                return
            fd = os.open('.lock', os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600, dir_fd=directory)
            try:
                records.checked_file(fd, 0)
                deadline = time.monotonic()+2
                while True:
                    try:
                        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                        break
                    except BlockingIOError:
                        if time.monotonic() >= deadline:
                            raise
                        time.sleep(0.01)
                os.fchmod(fd, 0)
                os.fsync(fd)
                # An already-open descriptor can still acquire flock after chmod.
                # Removing directory write permission also fences those waiters.
                os.fchmod(directory, 0o1500)
                os.fsync(directory)
            finally:
                os.close(fd)
        finally:
            os.close(directory)

    def remove_auth(self, path):
        with records.open_private(path):
            path.unlink()
        records.sync_directory(path.parent)

    def retire_operational_state(self):
        target = records.operational_directory(self.data)
        for name in ('sources', 'delegation.json'):
            old = self.data/name
            if not old.exists() and not old.is_symlink():
                continue
            # Reserve a generated destination so an interrupted retry cannot
            # overwrite the previous operational state.
            archive = Path(tempfile.mkdtemp(prefix='legacy-', dir=target))
            old.rename(archive/name)  # safefs: allow archive of existing state
            records.sync_directory(archive)
            records.sync_directory(target)
        records.sync_directory(self.data)

    def publish_asset(self, path, saved):
        content, mode = self.assets[path]
        before = self.asset_snapshot(path)
        if before != content:
            if before is not None and hashlib.sha256(before).hexdigest() not in self.recorded_digests(saved, path):
                raise ValueError('managed asset changed')
            native.publish(path, content, before, max_bytes=self.asset_limit(path), mode=mode)
        elif stat.S_IMODE(path.stat().st_mode) != mode:
            os.chmod(path, mode)

    def apply(self):
        installed = self.owned()
        if not self.selected and not installed:
            return
        self.reconcile_native(check=True)
        before_states = {unit: self.manager(unit) for unit in (COLLECTOR, MANAGER)}
        if not installed:
            if self.library.exists():
                raise ValueError('unmanaged library')
            self.control.mkdir(mode=0o700, parents=True)
            self.library.mkdir(mode=0o755, parents=True)
            native.publish(self.control/'marker', MARKER, None)
        if self.data.is_symlink():
            raise ValueError('unsafe data directory')
        self.data.parent.mkdir(parents=True, exist_ok=True)
        self.private_directory(self.data, 'foreign data directory')
        token = self.prepare_auth()
        inventory_path = self.control/'assets.json'
        inventory_before = native.snapshot(inventory_path)
        saved = json.loads(inventory_before or b'{}')
        pending = {key: value for key, value in saved.items() if key != REPLACED}
        replaced = dict(saved.get(REPLACED, {}))
        changed = False
        for path in [*self.assets, *self.retained]:
            before = self.asset_snapshot(path)
            before_digest = hashlib.sha256(before).hexdigest() if before is not None else None
            wanted_digest = hashlib.sha256(self.assets[path][0]).hexdigest() if path in self.assets else None
            # Content already equal to the delivery is never an unmanaged edit.
            if before_digest not in (None, wanted_digest, *self.recorded_digests(saved, path)):
                raise ValueError('managed asset changed')
            if path not in self.assets:
                continue
            if before is None:
                pending[str(path)] = wanted_digest
                replaced.pop(str(path), None)
            elif before_digest != wanted_digest:
                pending[str(path)] = before_digest
                replaced[str(path)] = wanted_digest
            changed |= before_digest != wanted_digest
        # Record both versions before changing any asset, so a retry accepts either.
        inventory_pending = self.encode_inventory(pending, replaced)
        if inventory_pending != inventory_before:
            native.publish(inventory_path, inventory_pending, inventory_before)
        saved = json.loads(inventory_pending)
        prepared = self.policy_status() == 'present'
        cutover = (installed and not prepared or (self.data/CUTOVER).exists()) and self.cutover_receipt()
        retiring = cutover and not prepared
        old_auth = self.data/'auth.token'
        # Policies name the helper; install it first so a stop between them leaves none dangling.
        self.publish_asset(self.helper, saved)
        self.reconcile_native(check=False, token=token)
        stop = [unit for unit, fields in before_states.items() if fields.get('LoadState') == 'loaded'
                and (retiring or changed or not self.running_no_new_privs(fields)
                     or (unit == COLLECTOR and (old_auth.exists() or (self.selected and not prepared))))]
        if retiring:
            for path in (self.commands/'subyard-telemetry-run', self.library/'broker-correlation.py'):
                if self.asset_snapshot(path) is not None:
                    os.chmod(path, 0o700)
            if stop:
                systemctl('disable', *stop)
        if stop:
            systemctl('stop', *stop)
        if retiring:
            self.as_developer(self.retire_legacy_producers)
            self.as_developer(self.retire_operational_state)
        if old_auth.exists() or old_auth.is_symlink():
            self.as_developer(lambda: self.remove_auth(old_auth))
        self.publish_safefs()
        if self.selected and not prepared:
            binary = self.library/'subyard-telemetry'
            self.publish_asset(binary, saved)
            if cutover:
                receipt = self.data/CUTOVER
                clean = records.sanitize_state('cutover', cutover, binary)
                if clean != cutover:
                    raise ValueError('invalid telemetry cutover receipt')
                if not receipt.exists():
                    native.publish(receipt, records.compact_json(clean), None, self.uid, self.gid)
            self.prepare_store()
        for path in self.assets:
            self.publish_asset(path, saved)
        for path, mode in self.retained.items():
            if self.asset_snapshot(path) is not None:
                os.chmod(path, mode)
        # Name the assets now on disk first; only the loaded units may differ.
        published = json.loads(self.inventory())
        previous = {path: digest for path, digest in [*replaced.items(), *pending.items()]
                    if published.get(path) not in (None, digest)}
        inventory_published = self.encode_inventory(published, previous)
        if inventory_published != inventory_pending:
            native.publish(inventory_path, inventory_published, inventory_pending)
        systemctl('daemon-reload')
        if self.selected:
            drain = self.data/'drain.json'
            if drain.exists():
                drain.unlink()
            systemctl('enable', '--now', COLLECTOR, MANAGER)
        else:
            systemctl('disable', '--now', COLLECTOR)
            systemctl('disable', MANAGER)
            if not retiring:
                drain = records.sanitize_state('drain', {'drain': True}, self.library/'subyard-telemetry')
                native.publish(self.data/'drain.json', records.compact_json(drain)+b'\n',
                               native.snapshot(self.data/'drain.json'), self.uid, self.gid)
                systemctl('start', MANAGER)
        # Keep the pair until the new units are loaded, so a retry also accepts the old ones.
        inventory = self.inventory()
        if inventory != inventory_published:
            native.publish(inventory_path, inventory, inventory_published)
        self.skill_delivery('apply' if self.selected else 'remove')
        if cutover and self.selected:
            (self.data/CUTOVER).unlink()
            with records.directory_lock(self.data, fcntl.LOCK_SH) as directory:
                os.fsync(directory)

    def rollback_state(self):
        """Observed facts and the parts not yet in runtime format 1.

        Each part is in this release's form or in format 1, and the inventory records
        every unit file on disk. Anything else, including drift that only apply
        converges, refuses: preparation converts a converged installation."""
        unconverged = native.Refusal([('rollback-unconverged',)])
        saved = self.saved_inventory()
        paths = [*self.assets, *self.retained]
        if not isinstance(saved.get(REPLACED, {}), dict) or set(saved)-{REPLACED}-{str(path) for path in paths}:
            raise unconverged
        facts, pending, on_disk = {}, [], {}
        for path in paths:
            content = self.asset_snapshot(path)
            if content is None:
                if path in self.retained:
                    continue
                raise unconverged
            digest, mode = hashlib.sha256(content).hexdigest(), stat.S_IMODE(path.stat().st_mode)
            on_disk[str(path)] = digest
            facts[str(path)] = [digest, mode]
            if digest not in self.recorded_digests(saved, path):
                raise unconverged
            if path in self.rollback_units:
                if content not in (self.assets[path][0], self.rollback_units[path]):
                    raise unconverged
                if (content, mode) != (self.rollback_units[path], 0o644):
                    pending.append({'parameter': 'telemetry.rollback.unit.'+path.name, 'installed': 'changed', 'desired': 'current'})
            elif path in self.assets and (content, mode) != self.assets[path] or path in self.retained and mode != self.retained[path]:
                raise unconverged
        facts['inventory'] = saved == on_disk
        if saved != on_disk:
            pending.append({'parameter': 'telemetry.rollback.inventory', 'installed': 'changed', 'desired': 'current'})
        for unit in (COLLECTOR, MANAGER):
            fields = self.manager(unit)
            loaded = (fields.get('LoadState') == 'loaded' and fields.get('NeedDaemonReload') == 'no'
                      and self.loaded_as(unit, fields, self.rollback_starts[unit]))
            facts[unit] = dict(loaded=loaded, **{key: fields.get(key, '') for key in ('LoadState', 'UnitFileState')})
            if not loaded:
                pending.append({'parameter': 'telemetry.rollback.loaded.'+unit, 'installed': 'changed', 'desired': 'current'})
            if not self.running_no_new_privs(fields) or unit == COLLECTOR and (
                    fields.get('ActiveState') == 'active' or fields.get('UnitFileState') == 'enabled'):
                raise unconverged
        tokens = {}
        for name, path in (('private', self.auth), ('analytics', self.rollback_auth)):
            if path.exists() or path.is_symlink():
                try:
                    tokens[name] = self.token(path)
                except (OSError, ValueError):
                    raise unconverged from None
        try:
            info = self.data.lstat()
        except FileNotFoundError:
            raise unconverged from None
        if (len(set(tokens.values())) != 1 or not stat.S_ISDIR(info.st_mode) or info.st_uid != self.uid
                or stat.S_IMODE(info.st_mode) != 0o700 or not self.reconcile_native(check=True)):
            raise unconverged
        facts['token'] = sorted(tokens)
        if 'private' in tokens:
            pending.append({'parameter': 'telemetry.rollback.token', 'installed': 'changed', 'desired': 'current'})
        return facts, pending

    def rollback_desired(self):
        value = {'format': 1, 'units': {str(path): hashlib.sha256(content).hexdigest() for path, content in self.rollback_units.items()},
                 'assets': {str(path): [hashlib.sha256(data).hexdigest(), mode] for path, (data, mode) in self.assets.items()
                            if path not in self.rollback_units},
                 'retained': {str(path): mode for path, mode in self.retained.items()}, 'auth': str(self.rollback_auth)}
        return hashlib.sha256(json.dumps(value, sort_keys=True).encode()).hexdigest()

    def rollback_observe(self):
        """How the installed state relates to runtime format 1, which v0.30 reads."""
        if not self.owned():
            if any(path.exists() or path.is_symlink() for path in [*self.assets, *self.retained]):
                raise ValueError('unmanaged collector assets')
            return {'state': 'absent', 'actual': '', 'desired': ''}
        # A v0.30 collector cannot open this release's store or its retired producer files.
        if self.selected:
            raise native.Refusal([('rollback-enabled',)])
        facts, pending = self.rollback_state()
        desired = self.rollback_desired()
        if not pending:
            return {'state': 'current', 'actual': desired, 'desired': desired}
        return {'state': 'stale', 'actual': hashlib.sha256(json.dumps(facts, sort_keys=True).encode()).hexdigest(),
                'desired': desired, 'consequence': ROLLBACK_CONSEQUENCE, 'diagnostics': pending}

    def rollback_apply(self):
        """Convert to runtime format 1. Every step is idempotent and leaves a state
        that both observers report stale, so a rerun resumes and apply reverts it."""
        if self.rollback_observe()['state'] != 'stale':
            return
        if self.auth.exists():
            token = self.token()
            if not self.rollback_auth.exists():
                self.as_developer(lambda: native.publish(self.rollback_auth, (token+'\n').encode(), None))
            self.as_developer(lambda: self.remove_auth(self.auth))
        inventory_path = self.control/'assets.json'
        before = native.snapshot(inventory_path)
        saved = json.loads(before)
        recorded = {key: value for key, value in saved.items() if key != REPLACED}
        replaced = dict(saved.get(REPLACED, {}))
        for path, content in self.rollback_units.items():
            current = self.asset_snapshot(path)
            if current != content:
                recorded[str(path)] = hashlib.sha256(current).hexdigest()
                replaced[str(path)] = hashlib.sha256(content).hexdigest()
        # Record both versions before replacing a unit, so every observer accepts either.
        pending = self.encode_inventory(recorded, replaced)
        if pending != before:
            native.publish(inventory_path, pending, before)
        for path, content in self.rollback_units.items():
            current = self.asset_snapshot(path)
            if current != content:
                native.publish(path, content, current, mode=0o644)
            elif stat.S_IMODE(path.stat().st_mode) != 0o644:
                os.chmod(path, 0o644)
        exact = {str(path): hashlib.sha256(content).hexdigest() for path in [*self.assets, *self.retained]
                 if (content := self.asset_snapshot(path)) is not None}
        final = self.encode_inventory(exact, {})
        if final != pending:
            native.publish(inventory_path, final, pending)
        systemctl('daemon-reload')


def main():
    if os.getuid() != 0:
        raise ValueError('guest root required')
    mode, selected, user, agents, *bindings = sys.argv[1:]
    if mode in ('identity-observe','identity-apply'):
        import authority
        source=bindings[-1]
        try:
            journal,recipe=authority.load(source,'telemetry')
            account=recipe['original']
            runtime=Runtime(Path(__file__).resolve().parent,account['user'],selected=='1',agents.split(),account=SimpleNamespace(pw_uid=account['uid'],pw_gid=account['gid'],pw_dir=account['home']))
            runtime.identity_rebooted=authority.rebooted(journal)
            phase='rebind' if journal['phase']=='account-ready' else 'quiesce'
            runtime.identity_check(recipe,phase)
        except FileNotFoundError:
            if mode!='identity-observe':raise
            runtime=Runtime(Path(__file__).resolve().parent,user,selected=='1',agents.split())
            target=dict(user=bindings[0],uid=runtime.uid,gid=runtime.gid,home='/home/'+bindings[0],group=bindings[0])
            recipe=runtime.identity_observe(target,source)
        if mode=='identity-observe':print(json.dumps(recipe))
        else:runtime.identity_apply(recipe,phase)
        return
    runtime = Runtime(Path(__file__).resolve().parent, user, selected == '1' and mode != 'startup', agents.split())
    runtime.selected = selected == '1'
    if mode == 'pause':
        if runtime.owned() and runtime.manager(COLLECTOR).get('ActiveState') == 'active':
            systemctl('stop', COLLECTOR)
            print('paused')
        return
    if mode == 'resume':
        if runtime.selected and runtime.owned():
            if (runtime.data/CUTOVER).exists() and not (runtime.data/'spool-policy.json').exists():
                raise ValueError('telemetry cutover pending')
            systemctl('start', COLLECTOR, MANAGER)
        return
    if mode == 'startup':
        print(json.dumps(runtime.startup()))
        return
    if mode in ('rollback-observe', 'rollback-apply'):
        observation = runtime.rollback_observe()
        if mode == 'rollback-apply':
            if bindings != [observation['actual'], observation['desired']]:
                raise ValueError('telemetry observation changed')
            runtime.rollback_apply()
            observation = runtime.rollback_observe()
            if observation['state'] != 'current':
                raise ValueError('telemetry did not converge')
        print(json.dumps(observation))
        return
    observation = runtime.observe()
    if mode == 'observe':
        print(json.dumps(observation))
        return
    if mode == 'check':
        if observation['state'] == 'stale':
            print('telemetry-drift')
        elif observation['state'] not in ('current', 'absent'):
            raise ValueError('telemetry drift')
        elif observation['state'] == 'current' and runtime.selected and runtime.raw_refused():
            print('telemetry-advisory storage-capacity')
        return
    if mode not in ('apply', 'refresh') or (mode == 'refresh' and bindings != [observation['actual'], observation['desired']]):
        raise ValueError('telemetry observation changed')
    runtime.apply()
    verified = runtime.observe()
    if verified['state'] not in ('current', 'absent'):
        raise ValueError('telemetry did not converge')
    if mode == 'refresh':
        print(json.dumps(verified))


def failure_diagnostic(error):
    location = 'runtime'
    trace = error.__traceback__
    while trace:
        filename = Path(trace.tb_frame.f_code.co_filename).name
        if filename in ('guest.py', 'native.py'):
            location = filename+':'+str(trace.tb_lineno)
        trace = trace.tb_next
    kind = 'ValueError' if isinstance(error, (native.Refusal, safefs.Unsafe)) else type(error).__name__
    return 'telemetry: local reconciliation failed ('+kind+' at '+location+')'


if __name__ == '__main__':
    try:
        main()
    except (OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
        if isinstance(error, native.Refusal):
            print('telemetry-native-refusal-v2 '+json.dumps(error.reasons), file=sys.stderr)
        if sys.argv[1:2] in (['identity-observe'], ['identity-apply']):
            import authority
            print(authority.refusal('Telemetry', error), file=sys.stderr)
        print(failure_diagnostic(error), file=sys.stderr)
        raise SystemExit(2)
