#!/usr/bin/env python3
"""Owned yard-local timer installation, readiness and opt-out."""
import fcntl
import grp
import hashlib
import json
import os
from pathlib import Path
import pwd
import re
import shutil
import stat
import subprocess
import sys
import tempfile

MARKER = '# Managed by Subyard git-fetch profile.\n'
SERVICE = 'subyard-git-fetch.service'
TIMER = 'subyard-git-fetch.timer'


def systemctl(*args):
    result = subprocess.run(['systemctl', *args], stdin=subprocess.DEVNULL,
                            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=15)
    if result.returncode:
        raise ValueError('system manager operation failed')
    return result.stdout.decode().strip()


class Runtime:
    def __init__(self, source, user, selected, stopped=False,
                 state=Path('/var/lib/subyard/git-fetch'), units=Path('/etc/systemd/system')):
        self.state, self.units = state, units
        self.selected, self.stopped = selected, stopped
        account = pwd.getpwnam(user)
        if not re.fullmatch(r'[a-zA-Z_][a-zA-Z0-9_-]*', user) or account.pw_uid == 0:
            raise ValueError('invalid developer account')
        home = account.pw_dir
        if not re.fullmatch(r'/[a-zA-Z0-9_./-]+', home):
            raise ValueError('unsupported developer home')
        group = grp.getgrgid(account.pw_gid).gr_name
        if not re.fullmatch(r'[a-zA-Z_][a-zA-Z0-9_-]*', group):
            raise ValueError('unsupported developer group')
        service = (source/(SERVICE+'.in')).read_text()
        for key, value in (('USER', user), ('GROUP', group), ('HOME', home)):
            service = service.replace('@'+key+'@', value)
        self.user, self.group, self.home = user, group, home
        self.assets = {state/'fetch.py': (source/'fetch.py').read_bytes(),
                       units/SERVICE: service.encode(), units/TIMER: (source/TIMER).read_bytes()}

    def safe(self, path):
        # Parent traversal must never turn an installed root asset into a symlink write.
        for ancestor in reversed(path.parents):
            try:
                info = ancestor.lstat()
            except FileNotFoundError:
                break
            if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o022:
                raise ValueError('unsafe managed parent')
        if path.is_symlink():
            raise ValueError('unsafe managed path')

    def fact(self, path):
        self.safe(path)
        if not path.exists():
            return None
        info = path.stat()
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1 or info.st_mode & 0o022:
            raise ValueError('unsafe managed file')
        return [info.st_mode & 0o777, path.read_bytes()]

    def owned(self):
        self.safe(self.state)
        if not self.state.exists():
            return False
        info = self.state.stat()
        if not self.state.is_dir() or info.st_uid != os.getuid() or info.st_mode & 0o777 != 0o755:
            raise ValueError('unsafe runtime directory')
        marker = self.state/'marker'
        if self.fact(marker) != [0o644, MARKER.encode()]:
            raise ValueError('foreign runtime directory')
        return True

    def manager_details(self, unit):
        raw = systemctl('show', unit, '--property=ActiveState,UnitFileState,LoadState,FragmentPath,NeedDaemonReload,DropInPaths,User,Group,Type,KillMode,UMask,LimitCORE,TimeoutStartUSec,TimeoutStopUSec,ExecStart,ExecStartPre,ExecStartPost,ExecStop,ExecStopPost,Environment,EnvironmentFiles,TimersMonotonic,AccuracyUSec')
        fields = {}
        for line in raw.splitlines():
            if '=' in line:
                key, value = line.split('=', 1)
                fields.setdefault(key, []).append(value)
        return {key: ' '.join(values) for key, values in fields.items()}

    def loaded_owned(self, unit, fields):
        if fields.get('LoadState') != 'loaded' or fields.get('FragmentPath') != str(self.units/unit):
            return False
        dropins = fields.get('DropInPaths', '').split()
        # Container images may generate type-wide service policy. The required
        # effective envelope and setpriv preserve our worker's security boundary.
        if any(unit != SERVICE or Path(path).parent.name != 'service.d' for path in dropins):
            return False
        if unit == TIMER:
            return (sorted(re.findall(r'(On\w+USec)=([^ ;}]+)', fields.get('TimersMonotonic', ''))) == [('OnBootUSec', '5min'), ('OnUnitInactiveUSec', '5min')]
                    and fields.get('AccuracyUSec') == '1s')
        expected = {'User': self.user, 'Group': self.group, 'Type': 'oneshot',
                    'KillMode': 'control-group', 'UMask': '0077', 'LimitCORE': '0',
                    'TimeoutStartUSec': 'infinity', 'TimeoutStopUSec': '5s',
                    'Environment': f'HOME={self.home} SSH_AUTH_SOCK={self.home}/.ssh/subyard-agent.sock',
                    'EnvironmentFiles': '', 'ExecStartPre': '', 'ExecStartPost': '',
                    'ExecStop': '', 'ExecStopPost': ''}
        command = '/usr/bin/setpriv --no-new-privs /usr/bin/python3 -B /var/lib/subyard/git-fetch/fetch.py'
        start = fields.get('ExecStart', '')
        return (all(fields.get(key, '') == value for key, value in expected.items())
                and start.startswith('{ path=/usr/bin/setpriv ; argv[]='+command+' ; ignore_errors=no ; ')
                and start.count('argv[]=') == 1)

    def manager(self, unit):
        fields = self.manager_details(unit)
        return fields.get('ActiveState'), fields.get('UnitFileState')

    def check(self, stopped=None, refresh=False):
        installed = self.owned()
        ready = installed
        present = False
        for path, content in self.assets.items():
            actual = self.fact(path) if path.parent.exists() else None
            if path.parent == self.units and actual is not None and not path.read_text().startswith(MARKER):
                raise ValueError('foreign service unit')
            present = present or actual is not None
            ready = ready and actual == [0o644, content]
        details = {unit: self.manager_details(unit) for unit in (TIMER, SERVICE)}
        for unit, fields in details.items():
            if fields.get('LoadState') != 'not-found' and not self.loaded_owned(unit, fields):
                raise ValueError('foreign loaded service unit')
            ready = ready and fields.get('LoadState') == 'loaded' and fields.get('NeedDaemonReload') == 'no'
        if not installed and not self.selected and not present:
            return True
        active, enabled = details[TIMER].get('ActiveState'), details[TIMER].get('UnitFileState')
        if refresh and self.selected and enabled == 'disabled':
            return ready and active == 'inactive' and details[SERVICE].get('ActiveState') in ('inactive', 'failed')
        if self.selected:
            stopped = self.stopped if stopped is None else stopped
            return (ready and enabled == 'enabled' and active == ('inactive' if stopped else 'active')
                    and (not stopped or details[SERVICE].get('ActiveState') in ('inactive', 'failed')))
        return ready and enabled == 'disabled' and active == 'inactive' and details[SERVICE].get('ActiveState') in ('inactive', 'failed')

    def publish(self, path, content):
        self.safe(path)
        fd, temporary = tempfile.mkstemp(prefix='.'+path.name+'.', dir=path.parent)
        try:
            with os.fdopen(fd, 'wb') as output:
                os.fchmod(output.fileno(), 0o644)
                output.write(content)
            os.replace(temporary, path)
        finally:
            if os.path.exists(temporary):
                os.unlink(temporary)

    def stop_owned_units(self, installed=True):
        units = []
        for unit in (TIMER, SERVICE):
            fields = self.manager_details(unit)
            if fields.get('LoadState') == 'not-found':
                continue
            if (not installed or fields.get('LoadState') != 'loaded'
                    or not self.loaded_owned(unit, fields)):
                raise ValueError('foreign loaded service unit')
            units.append(unit)
        if units:
            systemctl('stop', *units)

    def pause(self):
        if not self.owned():
            return False
        for unit in (TIMER, SERVICE):
            path = self.units/unit
            if self.fact(path) != [0o644, self.assets[path]]:
                raise ValueError('installed unit identity changed')
        active, _ = self.manager(TIMER)
        service_active, _ = self.manager(SERVICE)
        if active == 'active' or service_active in ('active', 'activating'):
            self.stop_owned_units()
            return True
        return False

    def resume(self):
        if not self.owned() or not self.selected:
            return
        # Resume accepts both a verified paused timer and an already restored one.
        # Every source/loaded identity check still applies before systemctl start.
        if not self.check(stopped=self.manager(TIMER)[0] == 'inactive'):
            raise ValueError('paused runtime identity changed')
        systemctl('start', TIMER)

    def create_state(self):
        # Publish the directory and marker together. A crash before rename leaves
        # only an unpublished staging directory, never an unowned final runtime.
        if not self.state.parent.exists():
            self.safe(self.state.parent)
            self.state.parent.mkdir(mode=0o755)
            self.state.parent.chmod(0o755)
        staging = Path(tempfile.mkdtemp(prefix='.git-fetch.', dir=self.state.parent))
        try:
            self.publish(staging/'marker', MARKER.encode())
            os.chmod(staging, 0o755)
            self.safe(self.state)
            if self.state.exists():
                raise ValueError('runtime directory appeared during publication')
            os.rename(staging, self.state)
        finally:
            if staging.exists():
                shutil.rmtree(staging)

    def apply(self, refresh=False):
        if self.check(refresh=refresh):
            return True
        installed = self.owned()
        if refresh and not installed:
            raise ValueError('release refresh requires owned installation')
        disabled = refresh and self.manager(TIMER)[1] == 'disabled'
        self.stop_owned_units(installed)
        if not installed:
            self.create_state()
        for path, content in self.assets.items():
            self.publish(path, content)
        systemctl('daemon-reload')
        if self.selected and not disabled:
            systemctl('enable', TIMER)
            if not self.stopped:
                systemctl('start', TIMER)
        else:
            systemctl('disable', TIMER)
        if not self.check(refresh=refresh):
            raise ValueError('runtime verification failed')
        return True

    def observe(self, binding):
        if not re.fullmatch('[a-f0-9]{64}', binding):
            raise ValueError('invalid release source binding')
        ready = self.check(refresh=True)
        installed = self.owned()
        if not installed:
            # check() also rejects orphan/foreign units before absence is trusted.
            if any(self.fact(path) is not None for path in self.assets):
                raise ValueError('unowned runtime artifacts')
            return {'state': 'absent'}
        details = {unit: self.manager_details(unit) for unit in (TIMER, SERVICE)}
        desired = digest({'source': binding, 'selected': self.selected,
                          'disabled': self.selected and details[TIMER].get('UnitFileState') == 'disabled',
                          'assets': {str(path): hashlib.sha256(content).hexdigest()
                                     for path, content in self.assets.items()}})
        facts = {}
        for path in (self.state, self.state/'marker', *self.assets):
            self.safe(path)
            try:
                info = path.lstat()
                facts[str(path)] = [info.st_dev, info.st_ino, info.st_uid, info.st_gid,
                                    info.st_mode, info.st_size, info.st_mtime_ns, info.st_ctime_ns]
                if path != self.state:
                    facts[str(path)].append(hashlib.sha256(path.read_bytes()).hexdigest())
            except FileNotFoundError:
                facts[str(path)] = None
        # check() validates the effective security envelope. Only public manager
        # readiness enters the CAS; never export or hash arbitrary environment.
        facts['manager'] = {unit: {key: fields.get(key, '') for key in
                            ('ActiveState', 'UnitFileState', 'LoadState', 'NeedDaemonReload')}
                            for unit, fields in details.items()}
        return {'state': 'current' if ready else 'stale',
                'actual': desired if ready else digest(facts), 'desired': desired}

    def refresh(self, binding, operation, actual, desired):
        if not re.fullmatch('[a-zA-Z0-9_-]+', operation) or any(
                not re.fullmatch('[a-f0-9]{64}', value) for value in (actual, desired)):
            raise ValueError('invalid release activation request')
        observed = self.observe(binding)
        if observed.get('desired') != desired:
            raise ValueError('release inputs changed before apply')
        if observed['state'] == 'current' and observed['actual'] == desired:
            return observed
        if observed.get('actual') != actual:
            raise ValueError('runtime changed before apply')
        self.apply(refresh=True)
        verified = self.observe(binding)
        if verified != {'state': 'current', 'actual': desired, 'desired': desired}:
            raise ValueError('release runtime verification failed')
        return verified


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def main():
    mode, selected, user, stopped, *extra = sys.argv[1:]
    if mode not in ('check', 'apply', 'pause', 'resume', 'observe', 'refresh') or selected not in ('0', '1') or stopped not in ('0', '1'):
        raise ValueError('invalid lifecycle request')
    if len(extra) != {'observe': 1, 'refresh': 4}.get(mode, 0):
        raise ValueError('invalid lifecycle arguments')
    if os.geteuid() != 0:
        raise ValueError('invalid lifecycle authority')
    runtime = Runtime(Path(__file__).resolve().parent, user, selected == '1', stopped == '1')
    if mode == 'observe':
        print(json.dumps(runtime.observe(*extra)))
        return
    if mode == 'check':
        if not runtime.check():
            print('git-fetch-drift')
        return
    # Serialize profile reconciliation against concurrent profile operations.
    lock = Path('/run/subyard-git-fetch-install.lock')
    actual = runtime.fact(lock)
    if actual is not None and actual[0] != 0o600:
        raise ValueError('unsafe lifecycle lock')
    fd = os.open(lock, os.O_CREAT | os.O_WRONLY | os.O_NOFOLLOW, 0o600)
    try:
        os.fchmod(fd, 0o600)
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if mode == 'refresh':
            print(json.dumps(runtime.refresh(*extra)))
            return
        if mode in ('pause', 'resume'):
            if mode == 'pause' and runtime.pause():
                print('paused')
            elif mode == 'resume':
                runtime.resume()
            return
        runtime.apply()
    finally:
        os.close(fd)


if __name__ == '__main__':
    try:
        main()
    except (OSError, ValueError, KeyError, subprocess.SubprocessError):
        print('git-fetch: profile reconciliation failed; inspect owned timer state', file=sys.stderr)
        sys.exit(2)
