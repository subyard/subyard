#!/usr/bin/env python3
"""Install only the marked local collector and native exporter ownership."""
import hashlib
import json
import os
from pathlib import Path
import pwd
import secrets
import stat
import subprocess
import sys

import native
import tomli_w

MARKER = b'subyard-telemetry-v1\n'
COLLECTOR = 'subyard-telemetry.service'
MANAGER = 'subyard-telemetry-runs.service'
BINARY_LIMIT = 64*1024*1024


def systemctl(*args):
    result = subprocess.run(['systemctl', *args], stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                            stderr=subprocess.DEVNULL, timeout=15)
    if result.returncode:
        raise ValueError('system manager unavailable')
    return result.stdout.decode().strip()


class Runtime:
    def __init__(self, source, user, selected, agents, control=Path('/var/lib/subyard/telemetry-control'),
                 data=Path('/var/lib/subyard/telemetry'), library=Path('/usr/local/lib/subyard-telemetry'),
                 units=Path('/etc/systemd/system'), commands=Path('/usr/local/bin'), legacy=Path('/var/lib/subyard/openlit/native.json')):
        self.source, self.user, self.selected, self.agents = source, user, selected, agents
        self.control, self.data, self.library, self.units, self.commands, self.legacy = control, data, library, units, commands, legacy
        account = pwd.getpwnam(user)
        self.uid, self.gid, self.home = account.pw_uid, account.pw_gid, Path(account.pw_dir)
        if self.uid == 0 or not user.replace('_', '').replace('-', '').isalnum():
            raise ValueError('developer account required')
        self.endpoint = 'http://127.0.0.1:14318'
        self.assets = {}
        for name in ('native.py', 'launcher.py', 'processes.py', 'records.py', 'forward.py',
                     'resources.py', 'sources.py', 'broker-correlation.py', 'tomli_w.py', 'subyard-telemetry'):
            content = native.snapshot(source/name, max_bytes=self.asset_limit(library/name))
            if content is None:
                raise ValueError('missing profile asset')
            self.assets[library/name] = (content, 0o755 if name in ('subyard-telemetry', 'broker-correlation.py') else 0o644)
        prefix = '# Managed by Subyard telemetry profile.\n'
        common = f'[Unit]\nDescription=Subyard local telemetry\n[Service]\nType=simple\nUser={user}\nGroup={self.gid}\nUMask=0077\nLimitCORE=0\nNoNewPrivileges=true\nRestart=on-failure\nRestartSec=5\nTimeoutStopSec=10\n'
        self.starts = {COLLECTOR: f'/usr/bin/setpriv --no-new-privs {library}/subyard-telemetry serve --data-dir {data} --listen 127.0.0.1:14318 --auth-file {data}/auth.token',
                       MANAGER: f'/usr/bin/setpriv --no-new-privs /usr/bin/python3 -B {library}/launcher.py manager --data-dir {data} --delegate'}
        collector = prefix+common+f'ExecStart={self.starts[COLLECTOR]}\nMemoryMax=256M\nCPUQuota=25%\nTasksMax=64\nKillMode=control-group\n[Install]\nWantedBy=multi-user.target\n'
        # Workloads inherit their caller's ceilings. Do not put a low collector
        # MemoryMax on this delegated ancestor or kill workloads on opt-out.
        manager = prefix+common+f'Slice=user-{self.uid}.slice\nExecStart={self.starts[MANAGER]}\nDelegate=cpu memory io pids\nKillMode=process\n[Install]\nWantedBy=multi-user.target\n'
        self.assets[units/COLLECTOR] = (collector.encode(), 0o644)
        self.assets[units/MANAGER] = (manager.encode(), 0o644)
        for command, target, extra in [('subyard-telemetry-run', 'launcher.py', 'run '), ('subyard-telemetry-forward', 'forward.py', '')]:
            self.assets[commands/command] = (('#!/bin/sh\n'+prefix+'exec /usr/bin/python3 -B '+str(library/target)+' '+extra+'"$@"\n').encode(), 0o755)

    def asset_limit(self, path):
        return BINARY_LIMIT if path == self.library/'subyard-telemetry' else native.CONFIG_LIMIT

    def asset_snapshot(self, path):
        return native.snapshot(path, max_bytes=self.asset_limit(path))

    def owned(self):
        if not self.control.exists():
            return False
        for directory in (self.control, self.library):
            if directory.is_symlink() or directory.stat().st_uid != os.getuid() or directory.stat().st_mode & 0o022:
                raise ValueError('foreign control directory')
        if native.snapshot(self.control/'marker') != MARKER:
            raise ValueError('foreign control state')
        return True

    def token(self):
        path = self.data/'auth.token'
        value = native.snapshot(path)
        if value is None or path.stat().st_uid != self.uid or stat.S_IMODE(path.stat().st_mode) != 0o600:
            raise ValueError('private collector token required')
        token = value.decode().strip()
        if len(token) != 64 or any(c not in '0123456789abcdef' for c in token):
            raise ValueError('invalid collector token')
        return token

    def manager(self, unit):
        raw = systemctl('show', unit, '--property=ActiveState,MainPID,UnitFileState,LoadState,FragmentPath,DropInPaths,User,Group,Type,ExecStart,KillMode,UMask,LimitCORE,NoNewPrivileges,AmbientCapabilities,ExecStartPre,ExecStartPost,ExecStop,ExecStopPost,Environment,EnvironmentFiles,MemoryMax,TasksMax,CPUQuotaPerSecUSec,Delegate')
        fields = dict(line.split('=', 1) for line in raw.splitlines() if '=' in line)
        if fields.get('LoadState') not in ('not-found', None):
            dropins = fields.get('DropInPaths', '').split()
            if any(Path(path).parent not in (Path('/run/systemd/system/service.d'), Path('/etc/systemd/system/service.d'), Path('/usr/lib/systemd/system/service.d')) for path in dropins):
                raise ValueError('foreign loaded unit')
            expected_mode = 'process' if unit == MANAGER else 'control-group'
            if fields.get('KillMode') != expected_mode:
                raise ValueError('foreign loaded unit stop policy')
            expected = {'FragmentPath': str(self.units/unit), 'User': self.user, 'Group': str(self.gid),
                        'Type': 'simple', 'UMask': '0077', 'LimitCORE': '0', 'AmbientCapabilities': '',
                        'ExecStartPre': '', 'ExecStartPost': '', 'ExecStop': '', 'ExecStopPost': '', 'Environment': '', 'EnvironmentFiles': ''}
            if unit == COLLECTOR:
                expected.update(MemoryMax=str(256*1024*1024), TasksMax='64', CPUQuotaPerSecUSec='250ms')
            else:
                expected['Delegate'] = 'yes'
            command = self.starts[unit]
            legacy = self.assets[self.units/unit][0].replace(b'/usr/bin/setpriv --no-new-privs ', b'', 1)
            legacy_command = command.removeprefix('/usr/bin/setpriv --no-new-privs ')
            start = fields.get('ExecStart', '')
            if start.startswith('{ path='+legacy_command.split()[0]+' ; argv[]='+legacy_command+' ; ignore_errors=no ; '):
                saved = json.loads(native.snapshot(self.control/'assets.json') or b'{}')
                if self.asset_snapshot(self.units/unit) == legacy and saved.get(str(self.units/unit)) == hashlib.sha256(legacy).hexdigest():
                    command = legacy_command  # Known pinned previous startup, stale until migrated.
            valid_start = start.startswith('{ path='+command.split()[0]+' ; argv[]='+command+' ; ignore_errors=no ; ') and start.count('argv[]=') == 1
            nnp = fields.get('NoNewPrivileges')
            if (any(fields.get(key, '') != value for key, value in expected.items()) or not valid_start
                    or nnp not in (('yes', 'no') if dropins else ('yes',))):
                raise ValueError('foreign loaded unit envelope')
        return fields

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

    def observe(self):
        installed = self.owned()
        if not installed:
            if any(path.exists() or path.is_symlink() for path in self.assets):
                raise ValueError('unmanaged collector assets')
            return {'state': 'stale' if self.selected else 'absent', 'actual': '', 'desired': self.desired() if self.selected else ''}
        files = {str(path): (hashlib.sha256(self.asset_snapshot(path) or b'').hexdigest(), stat.S_IMODE(path.stat().st_mode) if path.exists() else None)
                 for path in self.assets}
        ready = all(self.asset_snapshot(path) == content and stat.S_IMODE(path.stat().st_mode) == mode for path, (content, mode) in self.assets.items())
        states = {unit: self.manager(unit) for unit in (COLLECTOR, MANAGER)}
        active = states[COLLECTOR].get('ActiveState') == 'active'
        enabled = states[COLLECTOR].get('UnitFileState') == 'enabled'
        converged = ready and active == self.selected and enabled == self.selected
        converged = converged and all(self.running_no_new_privs(state) for state in states.values())
        if self.selected:
            converged = converged and states[MANAGER].get('ActiveState') == 'active' and states[MANAGER].get('UnitFileState') == 'enabled'
        converged = converged and native.reconcile(self.home, self.control/'native.json', self.agents if self.selected else [], self.endpoint, self.token(), tomli_w, self.uid, self.gid, check=True)
        actual = hashlib.sha256(json.dumps({'files': files, 'states': states, 'native_current': converged}, sort_keys=True).encode()).hexdigest()
        desired = self.desired()
        return {'state': 'current' if converged else 'stale', 'actual': desired if converged else actual, 'desired': desired}

    def desired(self):
        return hashlib.sha256(json.dumps({'assets': {str(path): [hashlib.sha256(data).hexdigest(), mode] for path, (data, mode) in self.assets.items()},
                                          'selected': self.selected, 'agents': self.agents}, sort_keys=True).encode()).hexdigest()

    def apply(self):
        installed = self.owned()
        if not self.selected and not installed:
            return
        before_states = {unit: self.manager(unit) for unit in (COLLECTOR, MANAGER)}
        if not installed:
            if self.library.exists():
                raise ValueError('unmanaged library')
            self.control.mkdir(mode=0o700, parents=True)
            self.library.mkdir(mode=0o755, parents=True)
            native.publish(self.control/'marker', MARKER, None)
            native.publish(self.control/'assets.json', (json.dumps({str(path): hashlib.sha256(data).hexdigest() for path, (data, _) in self.assets.items()})+'\n').encode(), None)
        if self.data.is_symlink():
            raise ValueError('unsafe data directory')
        if self.data.exists() and (self.data.stat().st_uid != self.uid or stat.S_IMODE(self.data.stat().st_mode) != 0o700):
            raise ValueError('foreign data directory')
        self.data.mkdir(mode=0o700, parents=True, exist_ok=True)
        os.chmod(self.data, 0o700)
        os.chown(self.data, self.uid, self.gid)
        if not (self.data/'auth.token').exists():
            native.publish(self.data/'auth.token', (secrets.token_hex(32)+'\n').encode(), None, self.uid, self.gid)
        token = self.token()
        saved = json.loads((self.control/'assets.json').read_text())
        restart = [unit for unit, fields in before_states.items() if fields.get('ActiveState') == 'active'
                   and not self.running_no_new_privs(fields) and (self.selected or unit == MANAGER)]
        for path in self.assets:
            before = self.asset_snapshot(path)
            if before is not None and saved.get(str(path)) != hashlib.sha256(before).hexdigest():
                raise ValueError('managed asset changed')
            if path in (self.units/COLLECTOR, self.units/MANAGER) and before != self.assets[path][0] and before is not None:
                unit = path.name
                if before_states[unit].get('ActiveState') == 'active' and unit not in restart and (self.selected or unit == MANAGER):
                    restart.append(unit)
        if self.selected:
            native.relinquish_openlit(self.home, self.legacy, tomli_w, self.uid, self.gid)
        native.reconcile(self.home, self.control/'native.json', self.agents if self.selected else [], self.endpoint, token, tomli_w, self.uid, self.gid)
        for path, (content, mode) in self.assets.items():
            before = self.asset_snapshot(path)
            if before != content:
                native.publish(path, content, before, max_bytes=self.asset_limit(path))
            os.chmod(path, mode)
        native.publish(self.control/'assets.json', (json.dumps({str(path): hashlib.sha256(data).hexdigest() for path, (data, _) in self.assets.items()})+'\n').encode(), native.snapshot(self.control/'assets.json'))
        systemctl('daemon-reload')
        if self.selected:
            drain = self.data/'drain.json'
            if drain.exists():
                drain.unlink()
            systemctl('enable', '--now', COLLECTOR, MANAGER)
        else:
            systemctl('disable', '--now', COLLECTOR)
            systemctl('disable', MANAGER)
            native.publish(self.data/'drain.json', b'{"drain":true}\n', native.snapshot(self.data/'drain.json'), self.uid, self.gid)
        if restart:
            systemctl('restart', *restart)


def main():
    if os.getuid() != 0:
        raise ValueError('guest root required')
    mode, selected, user, agents, *bindings = sys.argv[1:]
    runtime = Runtime(Path(__file__).resolve().parent, user, selected == '1', agents.split())
    if mode == 'pause':
        if runtime.owned() and runtime.manager(COLLECTOR).get('ActiveState') == 'active':
            systemctl('stop', COLLECTOR)
            print('paused')
        return
    if mode == 'resume':
        if runtime.selected and runtime.owned():
            systemctl('start', COLLECTOR, MANAGER)
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
    return 'telemetry: local reconciliation failed ('+type(error).__name__+' at '+location+')'


if __name__ == '__main__':
    try:
        main()
    except (OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
        print(failure_diagnostic(error), file=sys.stderr)
        raise SystemExit(2)
