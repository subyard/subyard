"""GitHub-owned include relocation under the fixed developer identity journal."""
import hashlib
import json
import os
from pathlib import Path
import pwd
import grp
import stat
import subprocess
import tempfile
import authority
import git_wiring


def digest(content):
    return hashlib.sha256(content).hexdigest()


def checked(path, home, uid, gid):
    git_wiring.safe(path,home)
    if not path.exists():return None
    info=path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid!=uid or info.st_gid!=gid or info.st_nlink!=1 or info.st_mode&0o022 or info.st_size>1024*1024:
        raise ValueError('unsafe Git identity artifact')
    return path.read_bytes()


def projection(content,old,new):
    with tempfile.TemporaryDirectory(prefix='subyard-git-identity-',dir='/tmp') as directory:
        path=Path(directory)/'config'
        path.write_bytes(content)
        query=subprocess.run(['git','config','--file',str(path),'--get-all','include.path'],capture_output=True,check=False)
        if query.returncode not in (0,1) or query.stdout.decode().splitlines().count(old)!=1 or new in query.stdout.decode().splitlines():
            raise ValueError('managed Git include is ambiguous')
        subprocess.run(['git','config','--file',str(path),'--fixed-value','--replace-all','include.path',new,old],check=True,capture_output=True)
        return path.read_bytes()


def observe(old_user,new_user,source):
    account=pwd.getpwnam(old_user)
    original=dict(user=old_user,uid=account.pw_uid,gid=account.pw_gid,home=account.pw_dir,group=grp.getgrgid(account.pw_gid).gr_name)
    target=dict(original,user=new_user,home='/home/'+new_user,group=new_user)
    home=Path(original['home'])
    fragment=home/'.config/subyard/github-git.conf'
    content=checked(fragment,home,account.pw_uid,account.pw_gid)
    recipe=dict(schema=1,source=source,original=original,target=target,git=None)
    if content is not None:
        if content.decode() not in (git_wiring.fragment('broker-first'),git_wiring.fragment('broker-last')):
            raise ValueError('foreign Git identity fragment')
        config=home/'.config/git/config'
        before=checked(config,home,account.pw_uid,account.pw_gid)
        if before is None:raise ValueError('managed Git include is missing')
        after=projection(before,str(fragment),target['home']+'/.config/subyard/github-git.conf')
        recipe['git']=dict(fragment=digest(content),before=digest(before),after=digest(after),mode=stat.S_IMODE(config.stat().st_mode))
    return recipe


def check(recipe,phase,apply=False):
    record=recipe['target'] if phase=='account-ready' else recipe['original']
    home=Path(record['home'])
    try:account=pwd.getpwnam(record['user'])
    except KeyError:
        if phase not in ('home','account'):raise
        account=pwd.getpwnam(recipe['target']['user'])
    if phase in ('home','account') and not home.exists():home=Path(recipe['target']['home'])
    if (account.pw_uid,account.pw_gid)!=(record['uid'],record['gid']) or account.pw_dir not in (recipe['original']['home'],recipe['target']['home']):
        raise ValueError('Git identity account changed')
    content=checked(home/'.config/subyard/github-git.conf',home,record['uid'],record['gid'])
    if recipe['git'] is None:
        if content is not None:raise ValueError('Git identity installation changed')
        return
    if content is None or digest(content)!=recipe['git']['fragment']:raise ValueError('Git identity fragment changed')
    path=home/'.config/git/config'
    before=checked(path,home,record['uid'],record['gid'])
    if before is None or digest(before) not in (recipe['git']['before'],recipe['git']['after']) or stat.S_IMODE(path.stat().st_mode)!=recipe['git']['mode']:
        raise ValueError('Git identity configuration changed')
    if not apply or phase!='account-ready' or digest(before)==recipe['git']['after']:return
    after=projection(before,recipe['original']['home']+'/.config/subyard/github-git.conf',recipe['target']['home']+'/.config/subyard/github-git.conf')
    if digest(after)!=recipe['git']['after']:raise ValueError('Git identity projection changed')
    fd,temporary=tempfile.mkstemp(prefix='.identity-',dir=path.parent)
    try:
        with os.fdopen(fd,'wb') as stream:
            os.fchmod(stream.fileno(),recipe['git']['mode']);os.fchown(stream.fileno(),record['uid'],record['gid'])
            stream.write(after);stream.flush();os.fsync(stream.fileno())
        if checked(path,home,record['uid'],record['gid'])!=before:raise ValueError('Git identity configuration changed')
        os.replace(temporary,path)
        directory=os.open(path.parent,os.O_DIRECTORY|os.O_NOFOLLOW)
        try:os.fsync(directory)
        finally:os.close(directory)
    finally:
        if os.path.exists(temporary):os.unlink(temporary)


def main(arguments):
    action,old_user,*extra=arguments
    source=extra[-1]
    try:
        journal,recipe=authority.load(source,'github')
    except FileNotFoundError:
        if action!='identity-observe':raise
        print(json.dumps(dict(phase=None,recipe=observe(old_user,extra[0],source))))
        return
    check(recipe,journal['phase'],apply=action=='identity-apply')
    print(json.dumps(dict(phase=journal['phase'],recipe=recipe,rebooted=authority.rebooted(journal))))
