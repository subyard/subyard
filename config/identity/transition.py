#!/usr/bin/env python3
"""Bounded developer login/home transition; numeric ownership never changes."""
from contextlib import contextmanager
import fcntl
import ctypes
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import tempfile
import time
from subids import IdentityRefusal

LINGER = '/var/lib/systemd/linger/'
MANAGER_UNITS = ('init.scope', 'dbus.service', 'dbus-broker.service')
USER_SERVICES = 'developer user services are running; stop them (for example with the owning profile\'s down command), then retry yard init'


def unique(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise IdentityRefusal('duplicate identity field')
        result[key] = value
    return result


def encoded(value):
    return (json.dumps(value, sort_keys=True, separators=(',', ':'))+'\n').encode()


def fingerprint(value):
    return hashlib.sha256(encoded(value)).hexdigest()


class Transition:
    # Tests inject a disposable filesystem and account-tool implementation. The
    # native entrypoint never accepts an alternate root or account tool.
    def __init__(self, root=Path('/'), owner=0, run=None, checkpoint=None, subid_lock=None, listener=None):
        self.root, self.owner = root, owner
        self.subid_lock = subid_lock
        self.run = run or self.command
        self.checkpoint = checkpoint or (lambda phase: None)
        self.listener = listener or self.listener_state
        self.directory = root/'var/lib/subyard/developer-identity'
        self.journal = self.directory/'transition.json'

    @staticmethod
    def listener_state():
        result = {}
        for unit in ('ssh.service','ssh.socket'):
            observed = subprocess.run(['systemctl','show',unit,'--property=LoadState,ActiveState,KillMode'],check=False,capture_output=True,text=True,timeout=15)
            fields = dict(line.split('=',1) for line in observed.stdout.splitlines() if '=' in line)
            if observed.returncode and not (observed.returncode in (1,4) and fields.get('LoadState')=='not-found' and fields.get('ActiveState')=='inactive'):
                raise IdentityRefusal('SSH listener inspection failed')
            if fields.get('LoadState') not in ('loaded','not-found') or fields.get('ActiveState') not in ('active','inactive','failed'):
                raise IdentityRefusal('SSH listener state cannot be safely captured')
            result[unit] = fields.get('ActiveState') == 'active'
            if unit=='ssh.service' and result[unit] and fields.get('KillMode')!='process':
                raise IdentityRefusal('SSH listener requires KillMode=process to retain established sessions')
        return result

    def fence_listener(self, plan):
        for unit in ('ssh.socket','ssh.service'):
            if self.listener().get(unit):
                self.run(['systemctl','stop',unit])
        if any(self.listener().values()):
            raise IdentityRefusal('SSH listener remains active')

    def restore_listener(self, plan):
        for unit in ('ssh.socket','ssh.service'):
            if plan['listener'][unit] and not self.listener().get(unit):
                self.run(['systemctl','start',unit])

    def sudo_grant(self, user):
        path=self.path('/etc/sudoers.d/90-subyard-'+user)
        self.safe(path)
        if not path.exists() and not path.is_symlink(): return None
        info=path.lstat()
        content=(user+' ALL=(ALL) NOPASSWD:ALL\n').encode()
        if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode)!=0o440 or info.st_uid!=self.owner or info.st_gid!=os.getgid() or info.st_nlink!=1 or path.read_bytes()!=content:
            raise IdentityRefusal('foreign developer sudo grant')
        return content

    def rebind_sudo(self, plan, retire=False):
        old,new=plan['old']['user'],plan['new']['user']
        previous,current=self.sudo_grant(old),self.sudo_grant(new)
        if not plan['sudo']:
            if previous is not None or current is not None: raise IdentityRefusal('unexpected developer sudo grant')
            return
        if retire:
            if previous is not None:
                self.path('/etc/sudoers.d/90-subyard-'+old).unlink()
                descriptor=os.open(self.path('/etc/sudoers.d'),os.O_DIRECTORY|os.O_NOFOLLOW)
                try: os.fsync(descriptor)
                finally: os.close(descriptor)
            return
        if previous is not None: raise IdentityRefusal('old developer sudo grant remains')
        if current is None:
            path=self.path('/etc/sudoers.d/90-subyard-'+new)
            fd,temporary=tempfile.mkstemp(prefix='.identity-',dir=path.parent)
            try:
                with os.fdopen(fd,'wb') as stream:
                    os.fchmod(stream.fileno(),0o440)
                    stream.write((new+' ALL=(ALL) NOPASSWD:ALL\n').encode())
                    stream.flush();os.fsync(stream.fileno())
                self.rename_home(temporary,path)
                descriptor=os.open(path.parent,os.O_DIRECTORY|os.O_NOFOLLOW)
                try:os.fsync(descriptor)
                finally:os.close(descriptor)
            finally:
                if os.path.exists(temporary):os.unlink(temporary)
        self.sudo_grant(new)

    @staticmethod
    def command(arguments):
        subprocess.run(arguments, check=True, stdin=subprocess.DEVNULL,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=30)

    def path(self, value):
        return self.root/value.lstrip('/')

    def safe(self, path):
        for parent in (path, *path.parents):
            if parent == self.root.parent:
                break
            try:
                info = parent.lstat()
            except FileNotFoundError:
                continue
            if parent != path and (not stat.S_ISDIR(info.st_mode) or info.st_uid != self.owner or info.st_mode & 0o022):
                raise IdentityRefusal('unsafe identity parent')
            if stat.S_ISLNK(info.st_mode):
                raise IdentityRefusal('symlinked identity path')

    def read(self, path, mode, maximum=1024*1024):
        self.safe(path)
        info = path.lstat()
        if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != mode or info.st_uid != self.owner or info.st_gid != os.getgid() or info.st_nlink != 1 or info.st_size > maximum:
            raise IdentityRefusal('unsafe identity metadata')
        return json.loads(path.read_bytes(), object_pairs_hook=unique)

    def publish(self, value):
        self.safe(self.directory)
        self.directory.mkdir(mode=0o700, parents=True, exist_ok=True)
        if self.directory.stat().st_uid != self.owner or stat.S_IMODE(self.directory.stat().st_mode) != 0o700:
            raise IdentityRefusal('unsafe identity journal directory')
        fd, temporary = tempfile.mkstemp(prefix='.identity-', dir=self.directory)
        try:
            with os.fdopen(fd, 'wb') as stream:
                os.fchmod(stream.fileno(), 0o600)
                stream.write(encoded(value))
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(temporary, self.journal)
            descriptor = os.open(self.directory, os.O_DIRECTORY | os.O_NOFOLLOW)
            try:
                os.fsync(descriptor)
            finally:
                os.close(descriptor)
        finally:
            if os.path.exists(temporary):
                os.unlink(temporary)

    def account_database(self, name):
        path = self.root/'etc'/name
        self.safe(path)
        info = path.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_uid != self.owner or info.st_gid != os.getgid() or stat.S_IMODE(info.st_mode) != 0o644 or info.st_nlink != 1 or info.st_size > 1024*1024:
            raise IdentityRefusal('unsafe account database')
        return path.read_text()

    def accounts(self):
        # Password fields and GECOS never enter a plan or journal.
        records = []
        for line in self.account_database('passwd').splitlines():
            fields = line.split(':')
            if len(fields) != 7:
                raise IdentityRefusal('invalid account database')
            records.append(dict(user=fields[0], uid=int(fields[2]), gid=int(fields[3]), home=fields[5], shell=fields[6]))
        return records

    def groups(self):
        result = []
        for line in self.account_database('group').splitlines():
            fields = line.split(':')
            if len(fields) != 4:
                raise IdentityRefusal('invalid group database')
            result.append(dict(name=fields[0], gid=int(fields[2]), members=fields[3].split(',') if fields[3] else []))
        return result

    def machine(self):
        value = (self.root/'etc/machine-id').read_text().strip()
        if not re.fullmatch('[0-9a-f]{32}', value):
            raise IdentityRefusal('invalid guest identity')
        return value

    def evidence(self, old):
        inventory = self.root/'var/lib/subyard/integrations/inventory.json'
        if inventory.exists() or inventory.is_symlink():
            value = self.read(inventory, 0o600)
            settings = value.get('applied_settings') if value.get('schema') == 1 else None
            if settings is not None:
                if settings.get('DEV_USER') != old['user'] or settings.get('DEV_UID') != str(old['uid']):
                    raise IdentityRefusal('applied developer identity changed')
                return fingerprint(value)
        core = self.root/'var/lib/subyard/agent-skills/core.json'
        value = self.read(core, 0o600)
        if value.get('schema') != 1 or value.get('home') != old['home'] or not value.get('skills'):
            raise IdentityRefusal('developer identity has no protected ownership evidence')
        return fingerprint(value)

    def home(self, value):
        path = self.path(value['home'])
        self.safe(path)
        info = path.lstat()
        if not stat.S_ISDIR(info.st_mode) or (info.st_uid, info.st_gid) != (value['uid'], value['gid']):
            raise IdentityRefusal('developer home ownership changed')
        return [info.st_dev, info.st_ino, stat.S_IMODE(info.st_mode), info.st_uid, info.st_gid]

    def parent(self, home):
        path = self.path(home).parent
        self.safe(path)
        info = path.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != self.owner or info.st_mode & 0o022:
            raise IdentityRefusal('unsafe home parent')
        return [info.st_dev, info.st_ino, info.st_uid, info.st_gid, stat.S_IMODE(info.st_mode)]

    @staticmethod
    def rename_home(old, new):
        library = ctypes.CDLL(None, use_errno=True)
        rename = getattr(library, 'renameat2', None)
        if rename is None:
            raise IdentityRefusal('atomic no-replace rename unavailable')
        if rename(-100, os.fsencode(old), -100, os.fsencode(new), 1) != 0:
            number = ctypes.get_errno()
            raise OSError(number, 'developer home rename refused')

    def no_mounts(self, home):
        for line in (self.root/'proc/self/mountinfo').read_text().splitlines():
            fields = line.split()
            if len(fields) < 6:
                raise IdentityRefusal('mount ownership unavailable')
            path = re.sub(r'\\([0-7]{3})', lambda m: chr(int(m[1], 8)), fields[4])
            if path == home or path.startswith(home+'/'):
                raise IdentityRefusal('developer home contains a mount; detach it before renaming')

    def no_processes(self, uid):
        for process in (self.root/'proc').iterdir():
            if not process.name.isdecimal():
                continue
            try:
                content = (process/'status').read_text()
            except (FileNotFoundError, ProcessLookupError):
                continue
            for line in content.splitlines():
                if line.startswith('Uid:') and uid in [int(item) for item in line.split()[1:]]:
                    raise IdentityRefusal('developer processes remain; close sessions and workloads before renaming')

    def wait_no_processes(self, uid):
        # Closing an owned SSH transport can leave logind's user manager alive
        # briefly. Wait for native teardown; never stop, ignore, or kill any UID
        # process. The same empty-UID predicate must pass before any mutation.
        deadline = time.monotonic() + 15
        while True:
            try:
                self.no_processes(uid)
                return
            except IdentityRefusal:
                if time.monotonic() >= deadline:
                    raise
                time.sleep(0.2)

    def lingering(self, user):
        path = self.path(LINGER+user)
        self.safe(path)
        return path.exists() or path.is_symlink()

    def user_services(self, uid):
        # Lingering keeps the user manager alive for its services. Stopping the
        # manager would end them, so only an otherwise idle manager may stop.
        manager = self.path('/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service' % (uid, uid))
        if not manager.is_dir() and not self.path('/sys/fs/cgroup/cgroup.controllers').exists():
            # Only the unified hierarchy has this layout; elsewhere absence proves nothing.
            raise IdentityRefusal('developer user manager cannot be inspected on this cgroup layout')
        for procs in sorted(manager.rglob('cgroup.procs')) if manager.is_dir() else ():
            if procs.parent == manager or procs.parent.name in MANAGER_UNITS:
                continue
            try:
                if procs.read_text().split():
                    return True
            except (FileNotFoundError, ProcessLookupError):
                continue
        return False

    def release_linger(self, plan):
        # Linger is keyed by login and keeps the user manager alive, so the UID
        # could never empty. Recorded in the plan, it moves to the new login.
        if not plan['linger']:
            return
        user, uid = plan['old']['user'], plan['old']['uid']
        if self.user_services(uid):
            # Planning refused running services; these started afterwards, while
            # the pending rename blocks profile commands.
            raise IdentityRefusal('developer user services started during the rename; stop them inside the yard as root, then retry yard init')
        if self.lingering(user):
            self.run(['loginctl', 'disable-linger', user])
        if self.lingering(user):
            raise IdentityRefusal('developer lingering could not be released')
        self.run(['systemctl', 'stop', 'user@%d.service' % uid])

    def restore_linger(self, plan):
        if plan['linger'] and not self.lingering(plan['new']['user']):
            self.run(['loginctl', 'enable-linger', plan['new']['user']])
            if not self.lingering(plan['new']['user']):
                raise IdentityRefusal('developer lingering did not move to the new account')

    def boot(self):
        value=self.path('/proc/sys/kernel/random/boot_id').read_text().strip()
        if not re.fullmatch('[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}',value):
            raise IdentityRefusal('native boot identity unavailable')
        return value

    def prepare(self, old_user, new_user, uid, source, yard, packages=None):
        failures = []
        def inspect(action):
            try:
                return action()
            except (ValueError, OSError, KeyError, TypeError) as error:
                message = str(error) if isinstance(error, IdentityRefusal) else 'developer identity metadata unavailable'
                if message not in failures:
                    failures.append(message)
                return None
        valid_target = bool(re.fullmatch('[a-z_][a-z0-9_-]{0,31}', new_user))
        if not valid_target:
            failures.append('invalid identity transition target')
        if not re.fullmatch('[0-9a-f]{64}', source):
            failures.append('invalid identity source')
        accounts = inspect(self.accounts)
        groups = inspect(self.groups)
        inspect(self.machine)
        old = None
        if accounts is not None:
            matches = [item for item in accounts if item['user'] == old_user]
            if len(matches) != 1 or matches[0]['uid'] != uid or uid == 0 or matches[0]['gid'] == 0 or sum(item['uid'] == uid for item in accounts) != 1:
                failures.append('developer account is not uniquely owned')
            else:
                old = matches[0]
            if any(item['user'] == new_user for item in accounts):
                failures.append('destination account already exists')
        if groups is not None:
            if any(item['name'] == new_user for item in groups):
                failures.append('destination group already exists')
            if old is not None:
                primary = [item for item in groups if item['gid'] == old['gid']]
                if len(primary) != 1 or primary[0]['name'] != old_user or any(name != old_user for name in primary[0]['members']) or any(item['gid'] == old['gid'] and item['user'] != old_user for item in accounts):
                    failures.append('developer primary group is shared or foreign')
        if valid_target:
            destination = self.path('/home/'+new_user)
            inspect(lambda: self.safe(destination))
            if destination.exists() or destination.is_symlink():
                failures.append('destination home already exists')
            if inspect(lambda: self.lingering(new_user)):
                failures.append('destination lingering already exists')
        if old is not None:
            if old['home'] != '/home/'+old_user:
                failures.append('canonical developer home transition required')
            else:
                inspect(lambda: self.home(old))
                inspect(lambda: self.no_mounts(old['home']))
                inspect(lambda: self.evidence(old))
            if inspect(lambda: self.lingering(old_user) and self.user_services(uid)):
                failures.append(USER_SERVICES)
        for relative in ('var/spool/cron/crontabs/'+old_user, 'var/spool/mail/'+old_user, 'var/mail/'+old_user):
            path = self.root/relative
            if path.exists() or path.is_symlink():
                failures.append('developer scheduled jobs or mailbox require an explicit owned transition')
                break
        if valid_target:
            import subids
            inspect(lambda: subids.SubordinateIDs(self.root,self.owner).observe(old_user,new_user,uid))
            inspect(lambda: self.sudo_grant(old_user))
            destination_grant=self.path('/etc/sudoers.d/90-subyard-'+new_user)
            if destination_grant.exists() or destination_grant.is_symlink():
                failures.append('destination sudo grant already exists')
            if old is not None:
                import accounts
                inspect(lambda: accounts.AccountRecovery(self.root,self.owner,self.subid_lock).observe(old,dict(old,user=new_user,home='/home/'+new_user)))
        inspect(self.listener)
        if failures:
            raise IdentityRefusal('; '.join(failures))
        return self.prepare_checked(old_user,new_user,uid,source,yard,packages or [])

    def prepare_checked(self, old_user, new_user, uid, source, yard, packages):
        if not re.fullmatch('[a-z_][a-z0-9_-]{0,31}', new_user) or not re.fullmatch('[0-9a-f]{64}', source):
            raise IdentityRefusal('invalid identity transition target')
        accounts = self.accounts()
        old = [item for item in accounts if item['user'] == old_user]
        if len(old) != 1 or old[0]['uid'] != uid or uid == 0 or old[0]['gid'] == 0 or sum(item['uid'] == uid for item in accounts) != 1:
            raise IdentityRefusal('developer account is not uniquely owned')
        old = old[0]
        if old['home'] != '/home/'+old_user or old_user == new_user:
            raise IdentityRefusal('canonical developer home transition required')
        if any(item['user'] == new_user for item in accounts):
            raise IdentityRefusal('destination account already exists')
        groups = self.groups()
        primary = [item for item in groups if item['gid'] == old['gid']]
        if len(primary) != 1 or primary[0]['name'] != old_user or any(name != old_user for name in primary[0]['members']) or any(item['gid'] == old['gid'] and item['user'] != old_user for item in accounts):
            raise IdentityRefusal('developer primary group is shared or foreign')
        if any(item['name'] == new_user for item in groups):
            raise IdentityRefusal('destination group already exists')
        new = dict(old, user=new_user, home='/home/'+new_user)
        destination = self.path(new['home'])
        self.safe(destination)
        if destination.exists() or destination.is_symlink():
            raise IdentityRefusal('destination home already exists')
        if self.lingering(new_user):
            raise IdentityRefusal('destination lingering already exists')
        home = self.home(old)
        if destination.parent.stat().st_dev != home[0]:
            raise IdentityRefusal('home transition requires the same filesystem')
        self.no_mounts(old['home'])
        for relative in ('var/spool/cron/crontabs/'+old_user, 'var/spool/mail/'+old_user, 'var/mail/'+old_user):
            auxiliary = self.root/relative
            if auxiliary.exists() or auxiliary.is_symlink():
                raise IdentityRefusal('developer scheduled jobs or mailbox require an explicit owned transition')
        import subids
        subordinate = subids.SubordinateIDs(self.root,self.owner).observe(old_user,new_user,uid)
        import accounts
        account_recipe=accounts.AccountRecovery(self.root,self.owner,self.subid_lock).observe(old,new)
        return dict(schema=1, machine=self.machine(), boot=self.boot(), yard=yard, source=source,
                    old=old, new=new, linger=self.lingering(old_user), group=primary[0], home=home, parent=self.parent(old['home']), evidence=self.evidence(old), subids=subordinate, accounts=account_recipe, packages=packages,receipts=self.receipts(old,new,packages),sudo=self.sudo_grant(old_user) is not None,listener=self.listener())

    @contextmanager
    def locked(self):
        self.safe(self.directory)
        self.directory.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.safe(self.directory/'lock')
        if self.directory.stat().st_uid != self.owner or stat.S_IMODE(self.directory.stat().st_mode) != 0o700:
            raise IdentityRefusal('unsafe identity journal directory')
        try:
            fd = os.open(self.directory/'lock', os.O_CREAT | os.O_EXCL | os.O_RDWR | os.O_NOFOLLOW, 0o600)
            os.fchmod(fd, 0o600)
        except FileExistsError:
            fd = os.open(self.directory/'lock', os.O_RDWR | os.O_NOFOLLOW)
        try:
            before = os.fstat(fd)
            if not stat.S_ISREG(before.st_mode) or before.st_uid != self.owner or before.st_gid != os.getgid() or stat.S_IMODE(before.st_mode) != 0o600 or before.st_nlink != 1:
                raise IdentityRefusal('unsafe identity lock')
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            after = (self.directory/'lock').lstat()
            if (before.st_dev, before.st_ino) != (after.st_dev, after.st_ino):
                raise IdentityRefusal('identity lock changed')
            yield
        finally:
            os.close(fd)

    def receipt_projection(self, value, kind, old, new, commit, packages=None):
        value = json.loads(json.dumps(value))
        if kind == 'skill':
            if set(value) != {'schema','home','extra','skills'} or value['schema'] != 1 or type(value['extra']) is not bool or not isinstance(value['skills'],dict) or value['home'] not in (old['home'],new['home']):
                raise IdentityRefusal('invalid owned skill identity receipt')
            value['home']=new['home']
            return value
        if value.get('schema') != 1 or not isinstance(value.get('entries'),list) or not isinstance(value.get('released',[]),list):
            raise IdentityRefusal('invalid owned integration identity receipt')
        def relocated(path, fixed=False):
            if not isinstance(path,str) or os.path.normpath(path)!=path:
                raise IdentityRefusal('invalid owned integration identity path')
            if fixed and path in ('/usr/local/libexec/subyard/projects-changed','/etc/subyard/agent-project-hooks'):
                return path
            if path.startswith(old['home']+'/'):
                return new['home']+path[len(old['home']):]
            if path.startswith(new['home']+'/'):
                return path
            raise IdentityRefusal('foreign owned integration identity path')
        recipes={}
        for recipe in packages or []:
            if (not isinstance(recipe,dict) or set(recipe)!={'id','before','after','check','command'}
                    or not isinstance(recipe['id'],str) or not re.fullmatch('[a-z][a-z0-9_-]*',recipe['id'])
                    or recipe['id'] in recipes
                    or any(not isinstance(recipe[key],str) or not re.fullmatch('[0-9a-f]{64}',recipe[key]) for key in ('before','after'))
                    or any(not isinstance(recipe[key],str) or (recipe[key] and not re.fullmatch('[A-Za-z0-9._-]+',recipe[key])) for key in ('check','command'))):
                raise IdentityRefusal('invalid package identity recipe')
            recipes[recipe['id']]=recipe
        seen=set()
        for entry in value['entries']:
            if not isinstance(entry,dict) or entry.get('kind') not in ('file','link','structured','package') or not entry.get('id'):
                raise IdentityRefusal('invalid owned integration identity record')
            if entry['kind']=='package':
                recipe=recipes.get(entry['id'])
                if (recipe is None or entry['id'] in seen or set(entry)!={'id','kind','digest'}
                        or entry['digest'] not in (recipe['before'],recipe['after'])):
                    raise IdentityRefusal('owned package identity receipt changed')
                seen.add(entry['id'])
                if commit: entry['digest']=recipe['after']
            else:
                entry['path']=relocated(entry['path'],True)
        if seen!=set(recipes):
            raise IdentityRefusal('owned package identity receipt set changed')
        value['released']=[relocated(path) for path in value.get('released',[])]
        settings=value.get('applied_settings')
        if settings is not None:
            if not isinstance(settings,dict) or settings.get('DEV_USER') not in (old['user'],new['user']) or settings.get('DEV_UID')!=str(old['uid']):
                raise IdentityRefusal('integration applied identity changed')
            if commit:
                settings['DEV_USER']=new['user']
        return value

    def skill_contents(self, value, home, old):
        adapters=('.agents/skills','.claude/skills','.config/opencode/skills')
        if value['extra']:
            adapters+=('.pi/agent/skills','.hermes/skills')
        for name,files in value['skills'].items():
            if not re.fullmatch('[a-z0-9]+(?:-[a-z0-9]+)*',name) or not isinstance(files,dict) or 'SKILL.md' not in files:
                raise IdentityRefusal('invalid owned skill registration')
            directory=self.root/'usr/local/share/subyard/skills'/name
            self.safe(directory)
            for relative,expected in files.items():
                if not isinstance(relative,str) or Path(relative).is_absolute() or '..' in Path(relative).parts or os.path.normpath(relative)!=relative or not isinstance(expected,str) or not re.fullmatch('[0-9a-f]{64}',expected):
                    raise IdentityRefusal('invalid owned skill file')
                path=directory/relative
                self.safe(path)
                info=path.lstat()
                if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode)!=0o644 or info.st_uid!=self.owner or info.st_gid!=os.getgid() or info.st_nlink!=1 or info.st_size>4*1024*1024 or hashlib.sha256(path.read_bytes()).hexdigest()!=expected:
                    raise IdentityRefusal('owned skill content changed')
            actual={str(path.relative_to(directory)) for path in directory.rglob('*') if not path.is_dir() or path.is_symlink()}
            if actual!=set(files):
                raise IdentityRefusal('unrecognized owned skill content')
            for adapter in adapters+('.codex/skills',):
                link=self.path(home)/adapter/name
                physical_home=self.path(home)
                parents=[]
                for parent in (link.parent,*link.parent.parents):
                    if parent==physical_home.parent:break
                    parents.append(parent)
                for parent in reversed(parents):
                    try: info=parent.lstat()
                    except FileNotFoundError: break
                    if not stat.S_ISDIR(info.st_mode) or info.st_uid not in (self.owner,old['uid']) or info.st_mode & 0o022:
                        raise IdentityRefusal('unsafe skill discovery parent')
                if adapter not in adapters:
                    if link.exists() or link.is_symlink():
                        raise IdentityRefusal('unexpected owned skill adapter')
                    continue
                info=link.lstat()
                if not stat.S_ISLNK(info.st_mode) or (info.st_uid,info.st_gid)!=(old['uid'],old['gid']) or os.readlink(link)!=str(directory):
                    raise IdentityRefusal('owned skill discovery link changed')

    def receipt_paths(self):
        result=[]
        inventory=self.root/'var/lib/subyard/integrations/inventory.json'
        if inventory.exists() or inventory.is_symlink():
            result.append(('inventory',inventory))
        directory=self.root/'var/lib/subyard/agent-skills'
        if directory.exists() or directory.is_symlink():
            self.safe(directory)
            info=directory.lstat()
            if not stat.S_ISDIR(info.st_mode) or stat.S_IMODE(info.st_mode)!=0o700 or info.st_uid!=self.owner or info.st_gid!=os.getgid():
                raise IdentityRefusal('unsafe skill identity directory')
            for path in sorted(directory.glob('*.json')):
                if not re.fullmatch('[a-z0-9_-]+',path.stem):
                    raise IdentityRefusal('invalid skill identity provider')
                result.append(('skill',path))
        return result

    def receipts(self, old, new, packages):
        result={}
        paths=self.receipt_paths()
        if packages and not any(kind=='inventory' for kind,_ in paths):
            raise IdentityRefusal('owned package inventory missing')
        for kind,path in paths:
            value=self.read(path,0o600,4*1024*1024)
            if kind=='inventory':
                expected={recipe['id']:recipe['before'] for recipe in packages}
                for entry in value.get('entries',[]):
                    if entry.get('kind')=='package' and entry.get('digest')!=expected.get(entry.get('id')):
                        raise IdentityRefusal('owned package identity source changed')
            moved=self.receipt_projection(value,kind,old,new,False,packages)
            final=self.receipt_projection(value,kind,old,new,True,packages)
            if kind=='skill':
                self.skill_contents(value,old['home'],old)
            result[str(path.relative_to(self.root))]=dict(kind=kind,before=hashlib.sha256(path.read_bytes()).hexdigest(),moved=hashlib.sha256(encoded(moved)).hexdigest(),after=hashlib.sha256(encoded(final)).hexdigest())
        return result

    def rebind_receipts(self, plan, commit=False):
        recipes=plan['receipts']
        if {str(path.relative_to(self.root)) for _,path in self.receipt_paths()}!=set(recipes):
            raise IdentityRefusal('owned identity receipt set changed')
        for relative,recipe in recipes.items():
            path=self.root/relative
            value=self.read(path,0o600,4*1024*1024)
            before=path.read_bytes()
            current=hashlib.sha256(before).hexdigest()
            if current not in (recipe['before'],recipe['moved'],recipe['after']):
                raise IdentityRefusal('owned identity receipt changed')
            if recipe['kind']=='skill':
                self.skill_contents(value,plan['new']['home'],plan['old'])
            if current==recipe['after']:
                continue
            projected=self.receipt_projection(value,recipe['kind'],plan['old'],plan['new'],commit,plan['packages'])
            payload=encoded(projected)
            if hashlib.sha256(payload).hexdigest()!=recipe['after' if commit else 'moved']:
                raise IdentityRefusal('owned identity receipt result changed')
            fd,temporary=tempfile.mkstemp(prefix='.identity-',dir=path.parent)
            try:
                with os.fdopen(fd,'wb') as stream:
                    os.fchmod(stream.fileno(),0o600)
                    os.fchown(stream.fileno(),self.owner,os.getgid())
                    stream.write(payload);stream.flush();os.fsync(stream.fileno())
                if path.read_bytes()!=before:
                    raise IdentityRefusal('owned identity receipt changed before publication')
                os.replace(temporary,path)
                descriptor=os.open(path.parent,os.O_DIRECTORY|os.O_NOFOLLOW)
                try: os.fsync(descriptor)
                finally: os.close(descriptor)
            finally:
                if os.path.exists(temporary):os.unlink(temporary)
            self.checkpoint('receipt:'+relative)

    def validate_pending(self, plan, receipts=True):
        if plan['machine'] != self.machine():
            raise IdentityRefusal('identity journal belongs to another guest')
        import accounts
        accounts.AccountRecovery(self.root,self.owner,self.subid_lock).validate(plan['accounts'])
        accounts = self.accounts()
        owned = [item for item in accounts if item['uid'] == plan['old']['uid'] or item['user'] in (plan['old']['user'],plan['new']['user'])]
        if len(owned) != 1 or owned[0] not in (plan['old'],plan['new']):
            raise IdentityRefusal('identity account state changed')
        homes = [record for record in (plan['old'],plan['new']) if self.path(record['home']).exists() or self.path(record['home']).is_symlink()]
        if len(homes) != 1 or self.home(homes[0]) != plan['home'] or self.parent(homes[0]['home']) != plan['parent']:
            raise IdentityRefusal('identity home state changed')
        for record in (plan['old'],plan['new']):
            self.no_mounts(record['home'])
        original = plan['group']
        renamed_members = [plan['new']['user'] if value == plan['old']['user'] else value for value in original['members']]
        allowed = (original,dict(original,members=renamed_members),dict(original,name=plan['new']['user'],members=renamed_members))
        groups = [value for value in self.groups() if value['gid'] == plan['old']['gid'] or value['name'] in (plan['old']['user'],plan['new']['user'])]
        if len(groups)!=1 or groups[0] not in allowed:
            raise IdentityRefusal('identity primary group state changed')
        import subids
        helper = subids.SubordinateIDs(self.root,self.owner,self.subid_lock)
        helper.provider()
        for name, expected in plan['subids'].items():
            payload=helper.payload(name)
            current=None if payload is None else hashlib.sha256(payload).hexdigest()
            if current not in (expected['before'],expected['after']):
                raise IdentityRefusal('identity subordinate state changed')
        for relative,recipe in (plan['receipts'].items() if receipts else ()):
            path=self.root/relative
            self.read(path,0o600,4*1024*1024)
            if hashlib.sha256(path.read_bytes()).hexdigest() not in (recipe['before'],recipe['moved'],recipe['after']):
                raise IdentityRefusal('owned identity receipt changed')
        return owned[0]['user']

    def prepared_matches(self,plan):
        observed=self.prepare(plan['old']['user'],plan['new']['user'],plan['old']['uid'],plan['source'],plan['yard'],plan['packages'])
        expected={name:value for name,value in plan.items() if name not in ('owners','settings')}
        if observed['boot']!=plan['boot']:
            # Native reboot may restart the same enabled listener. Original
            # listener/owner intent remains in the protected approved recipe.
            observed['boot']=plan['boot']
            observed['listener']=plan['listener']
        if observed!=expected:
            raise IdentityRefusal('identity native state changed after planning')

    def apply_account(self, plan, expected):
        with self.locked():
            return self.apply_account_locked(plan, expected)

    def apply_account_locked(self, plan, expected):
        if fingerprint(plan) != expected or plan['machine'] != self.machine():
            raise IdentityRefusal('identity plan changed')
        if self.journal.exists() or self.journal.is_symlink():
            journal = self.read(self.journal, 0o600)
            if set(journal) != {'schema', 'plan', 'phase'} or journal['schema'] != 1 or journal['phase'] not in ('prepared', 'quiesced', 'home', 'account', 'account-ready') or journal.get('plan') != plan:
                raise IdentityRefusal('another identity transition is pending')
        else:
            self.prepared_matches(plan)
            journal = dict(schema=1, plan=plan, phase='prepared')
            self.publish(journal)
        if plan.get('owners') and journal['phase'] == 'prepared':
            raise IdentityRefusal('identity owners must be quiesced before account mutation')
        self.release_linger(plan)
        self.wait_no_processes(plan['old']['uid'])
        if any(self.listener().values()):
            raise IdentityRefusal('SSH listener must be fenced before account mutation')
        self.rebind_sudo(plan,retire=True)
        self.no_mounts(plan['old']['home'])
        self.no_mounts(plan['new']['home'])
        old_path, new_path = self.path(plan['old']['home']), self.path(plan['new']['home'])
        if old_path.exists():
            if new_path.exists() or self.home(plan['old']) != plan['home']:
                raise IdentityRefusal('identity home changed')
            if self.parent(plan['old']['home']) != plan['parent'] or self.parent(plan['new']['home']) != plan['parent']:
                raise IdentityRefusal('developer home parent changed')
            self.rename_home(old_path, new_path)
            descriptor = os.open(new_path.parent, os.O_DIRECTORY | os.O_NOFOLLOW)
            try:
                os.fsync(descriptor)
            finally:
                os.close(descriptor)
        if self.home(plan['new']) != plan['home'] or old_path.exists() or old_path.is_symlink():
            raise IdentityRefusal('identity home transfer is incomplete')
        journal['phase'] = 'home'
        self.publish(journal)
        self.checkpoint('home')
        import accounts as account_recovery
        recovery=account_recovery.AccountRecovery(self.root,self.owner,self.subid_lock)
        if recovery.changed(plan['accounts']):
            recovery.recover(plan['accounts'],plan['old'],plan['new'],checkpoint=self.checkpoint)
        accounts = self.accounts()
        if plan['old'] in accounts and not any(item['user'] == plan['new']['user'] for item in accounts):
            self.run(['usermod', '--login', plan['new']['user'], '--home', plan['new']['home'], plan['old']['user']])
        recovery.recover(plan['accounts'],plan['old'],plan['new'],checkpoint=self.checkpoint)
        if plan['new'] not in self.accounts() or any(item['user'] == plan['old']['user'] for item in self.accounts()):
            raise IdentityRefusal('developer account transition is incomplete')
        journal['phase'] = 'account'
        self.publish(journal)
        self.checkpoint('account')
        groups = self.groups()
        old_group = dict(plan['group'], members=[plan['new']['user'] if item == plan['old']['user'] else item for item in plan['group']['members']])
        if old_group in groups and not any(item['name'] == plan['new']['user'] for item in groups):
            self.run(['groupmod', '--new-name', plan['new']['user'], plan['old']['user']])
        recovery.recover(plan['accounts'],plan['old'],plan['new'],final=True,checkpoint=self.checkpoint)
        if dict(plan['group'], name=plan['new']['user'], members=[plan['new']['user'] if item == plan['old']['user'] else item for item in plan['group']['members']]) not in self.groups():
            raise IdentityRefusal('developer group transition is incomplete')
        import subids
        subids.SubordinateIDs(self.root,self.owner,self.subid_lock).rebind(plan['subids'],plan['old']['user'],plan['new']['user'],plan['old']['uid'],self.checkpoint)
        self.rebind_sudo(plan)
        journal['phase'] = 'account-ready'
        self.publish(journal)
        self.checkpoint('account-ready')
        return journal


def main():
    import sys
    if os.geteuid() != 0:
        raise IdentityRefusal('guest root required')
    mode, = sys.argv[1:]
    request = json.load(sys.stdin, object_pairs_hook=unique)
    transition = Transition()
    if mode == 'pending':
        transition.safe(transition.journal)
        if not transition.journal.exists() and not transition.journal.is_symlink():
            print(json.dumps({'plan': None}))
            return
        value = transition.read(transition.journal, 0o600)
        if value['phase'] == 'complete':
            print(json.dumps({'plan': None}))
            return
        plan = value['plan']
        if plan['source'] != request['source'] or plan['machine'] != transition.machine() or plan['yard'] != request['yard'] or plan['new']['user'] != request['new_user'] or plan['new']['uid'] != request['uid']:
            raise IdentityRefusal('pending identity transition belongs to another scope')
        current_user=transition.validate_pending(plan, receipts=value['phase'] != 'ready')
        print(json.dumps(dict(plan=plan, pending=value['phase'] != 'complete', phase=value['phase'], current_user=current_user, fingerprint=fingerprint(plan))))
        return
    if mode == 'recover-observe':
        plan=request['plan']
        if plan['machine']!=transition.machine():raise IdentityRefusal('foreign identity recovery guest')
        if transition.journal.exists() or transition.journal.is_symlink():
            journal=transition.read(transition.journal,0o600)
            if journal.get('plan')!=plan:raise IdentityRefusal('identity recovery authority changed')
            current=transition.validate_pending(plan,receipts=journal['phase']!='ready')
            print(json.dumps(dict(plan=plan,pending=True,phase=journal['phase'],current_user=current,fingerprint=fingerprint(plan))))
        elif any(record==plan['new'] for record in transition.accounts()):
            transition.validate_pending(plan,receipts=False)
            print(json.dumps(dict(pending=False)))
        else:
            transition.prepared_matches(plan)
            print(json.dumps(dict(plan=plan,pending=True,phase='host-prepared',current_user=plan['old']['user'],fingerprint=fingerprint(plan))))
        return
    if mode == 'applied':
        matches=[record for record in transition.accounts() if record['user']==request['user'] or record['uid']==request['uid']]
        if len(matches)!=1 or matches[0]['user']!=request['user'] or matches[0]['uid']!=request['uid'] or matches[0]['home']!='/home/'+request['user']:
            raise IdentityRefusal('applied developer account changed')
        transition.home(matches[0])
        print(json.dumps(dict(current_user=request['user'])))
        return
    if mode == 'observe':
        plan = transition.prepare(request['old_user'], request['new_user'], request['uid'], request['source'], request['yard'], request.get('packages',[]))
        print(json.dumps(dict(plan=plan, pending=False, fingerprint=fingerprint(plan))))
        return
    if mode in ('receipts', 'commit'):
        with transition.locked():
            current=transition.read(transition.journal,0o600)
            if current['plan']!=request['plan'] or current['phase']!='account-ready':
                raise IdentityRefusal('identity receipt authority changed')
            transition.validate_pending(current['plan'])
            transition.rebind_receipts(current['plan'],commit=mode=='commit')
            # Account/home mutation and exact core receipt rebinding are done.
            # Restore the captured listener before owners reconnect transports;
            # applied identity is still published only by the later commit.
            transition.restore_listener(current['plan'])
            transition.restore_linger(current['plan'])
            if mode == 'receipts':
                transition.checkpoint('listener-restored')
            if mode == 'commit':
                current['phase']='ready'
                transition.publish(current)
        print(json.dumps(dict(plan=current['plan'],pending=True,phase=current['phase'],fingerprint=fingerprint(current['plan']))))
        return
    if mode == 'finished':
        plan=request['plan']
        transition.safe(transition.journal)
        if transition.journal.exists() or transition.journal.is_symlink() or plan['machine']!=transition.machine():
            raise IdentityRefusal('identity completion authority changed')
        if transition.validate_pending(plan,receipts=False)!=plan['new']['user']:
            raise IdentityRefusal('identity completion account changed')
        print(json.dumps(dict(plan=plan,pending=False,phase='complete',fingerprint=fingerprint(plan))))
        return
    if mode == 'finish':
        with transition.locked():
            current=transition.read(transition.journal,0o600)
            if current['plan']!=request['plan'] or current['phase'] not in ('ready','complete'):
                raise IdentityRefusal('identity completion authority changed')
            if transition.validate_pending(current['plan'],receipts=False)!=current['plan']['new']['user']:
                raise IdentityRefusal('identity completion account changed')
            current['phase']='complete'
            transition.journal.unlink()
            descriptor=os.open(transition.directory,os.O_DIRECTORY|os.O_NOFOLLOW)
            try:os.fsync(descriptor)
            finally:os.close(descriptor)
        print(json.dumps(dict(plan=current['plan'],pending=False,phase=current['phase'],fingerprint=fingerprint(current['plan']))))
        return
    if mode in ('fence', 'quiesced', 'account'):
        current = transition.read(transition.journal, 0o600)
        plan = current['plan']
        if plan != request['plan']:
            raise IdentityRefusal('identity authority changed')
        if mode == 'fence':
            with transition.locked():
                transition.fence_listener(plan)
        elif mode == 'quiesced':
            with transition.locked():
                current = transition.read(transition.journal,0o600)
                if current['plan'] != plan or current['phase'] not in ('prepared','quiesced'):
                    raise IdentityRefusal('identity quiesce phase changed')
                current['phase']='quiesced'
                transition.publish(current)
        else:
            current=transition.apply_account(plan,fingerprint(plan))
        print(json.dumps(dict(plan=plan,pending=True,phase=current['phase'],fingerprint=fingerprint(plan))))
        return
    if mode == 'begin':
        plan = request['plan']
        if request['expected'] not in (fingerprint(plan), fingerprint({name: value for name, value in plan.items() if name not in ('owners', 'settings')})):
            raise IdentityRefusal('identity plan changed')
        with transition.locked():
            if transition.journal.exists() or transition.journal.is_symlink():
                current = transition.read(transition.journal, 0o600)
                if current['phase'] == 'complete':
                    transition.prepared_matches(plan)
                    transition.publish(dict(schema=1,plan=plan,phase='prepared'))
                elif current.get('plan') != plan:
                    raise IdentityRefusal('another identity transition is pending')
            else:
                transition.prepared_matches(plan)
                transition.publish(dict(schema=1, plan=plan, phase='prepared'))
        current=transition.read(transition.journal,0o600)
        transition.validate_pending(plan)
        print(json.dumps(dict(plan=plan, pending=True, phase=current['phase'], fingerprint=fingerprint(plan))))
        return
    raise IdentityRefusal('unknown developer identity operation')


if __name__ == '__main__':
    try:
        main()
    except IdentityRefusal as error:
        import sys
        print(json.dumps({'errors':str(error).split('; ')}))
        sys.exit(1)
    except (ValueError, OSError, KeyError, TypeError, subprocess.SubprocessError):
        import sys
        print('developer identity: protected transition or native ownership could not be verified', file=sys.stderr)
        sys.exit(1)
