"""Recover only fingerprinted partial shadow-utils developer rename writes.

Normal writes remain owned by usermod/groupmod. The recovery projection follows
shadow src/usermod.c update_group/update_gshadow: remove the old member, then
append the new member. Password fields and all unrelated bytes stay opaque.
https://github.com/shadow-maint/shadow/blob/master/src/usermod.c
"""
from contextlib import ExitStack
import hashlib
import os
from pathlib import Path
import stat
import tempfile

from subids import IdentityRefusal

DATABASES=('passwd','shadow','group','gshadow')


def digest(payload):return None if payload is None else hashlib.sha256(payload).hexdigest()


class AccountRecovery:
    def __init__(self,root=Path('/'),owner=0,password_lock=None):
        import subids
        self.root,self.owner=root,owner
        self.locks=subids.SubordinateIDs(root,owner,password_lock)

    def read(self,name):
        self.locks.directory_safe()
        path=self.root/'etc'/name
        if not path.exists() and not path.is_symlink():
            if name in ('passwd','group'):raise IdentityRefusal('account database missing')
            return None,None
        info=path.lstat()
        allowed=(0o644,) if name in ('passwd','group') else (0o600,0o640)
        if not stat.S_ISREG(info.st_mode) or info.st_uid!=self.owner or stat.S_IMODE(info.st_mode) not in allowed or info.st_nlink!=1 or info.st_size>1024*1024:
            raise IdentityRefusal('unsafe account recovery database')
        metadata=dict(mode=stat.S_IMODE(info.st_mode),uid=info.st_uid,gid=info.st_gid,attributes=self.locks.attributes(path))
        return path.read_bytes(),metadata

    @staticmethod
    def project(payload,name,old,new,group=False):
        if payload is None:return None
        result=[]
        for line in payload.splitlines(keepends=True):
            body=line.rstrip(b'\r\n');ending=line[len(body):]
            fields=body.split(b':')
            expected={'passwd':7,'shadow':9,'group':4,'gshadow':4}[name]
            if len(fields)!=expected:raise IdentityRefusal('unsupported account recovery row')
            if name in ('passwd','shadow') and fields[0]==old['user'].encode():
                fields[0]=new['user'].encode()
                if name=='passwd':
                    if fields[5]!=old['home'].encode():raise IdentityRefusal('account recovery home changed')
                    fields[5]=new['home'].encode()
            if name in ('group','gshadow'):
                for index in ((3,) if name=='group' else (2,3)):
                    members=fields[index].split(b',') if fields[index] else []
                    if old['user'].encode() in members:
                        members=[value for value in members if value!=old['user'].encode()]
                        if new['user'].encode() not in members:members.append(new['user'].encode())
                        fields[index]=b','.join(members)
                if group and fields[0]==old['user'].encode():fields[0]=new['user'].encode()
            result.append(b':'.join(fields)+ending)
        return b''.join(result)

    def observe(self,old,new):
        recipe={}
        for name in DATABASES:
            payload,metadata=self.read(name)
            if payload is not None:
                rows=[line.split(b':',1)[0] for line in payload.splitlines()]
                if new['user'].encode() in rows or rows.count(old['user'].encode())!=1:
                    raise IdentityRefusal('account recovery owner row is not unique')
            login=self.project(payload,name,old,new)
            final=self.project(payload,name,old,new,True)
            recipe[name]=dict(before=digest(payload),login=digest(login),final=digest(final),metadata=metadata)
        return recipe

    def validate(self,recipe):
        for name in DATABASES:
            payload,metadata=self.read(name)
            saved=recipe[name]
            if metadata!=saved['metadata'] or digest(payload) not in (saved['before'],saved['login'],saved['final']):
                raise IdentityRefusal('account recovery database changed outside the captured rename')

    def changed(self,recipe):
        self.validate(recipe)
        return any(digest(self.read(name)[0])!=recipe[name]['before'] for name in DATABASES)

    def publish(self,name,payload,expected,metadata):
        path=self.root/'etc'/name
        attrs={key:os.getxattr(path,key,follow_symlinks=False) for key in metadata['attributes']}
        fd,temporary=tempfile.mkstemp(prefix='.'+name+'-identity-',dir=path.parent)
        try:
            with os.fdopen(fd,'wb') as stream:
                os.fchmod(stream.fileno(),metadata['mode']);os.fchown(stream.fileno(),metadata['uid'],metadata['gid'])
                for key,value in attrs.items():os.setxattr(stream.fileno(),key,value)
                stream.write(payload);stream.flush();os.fsync(stream.fileno())
            if self.read(name)!=(expected,metadata):raise IdentityRefusal('account recovery publication changed')
            os.replace(temporary,path)
            directory=os.open(path.parent,os.O_DIRECTORY|os.O_NOFOLLOW)
            try:os.fsync(directory)
            finally:os.close(directory)
        finally:
            if os.path.exists(temporary):os.unlink(temporary)

    def recover(self,recipe,old,new,final=False,checkpoint=lambda _:None):
        # Fixed DB set and shadow-compatible PID locks; no caller-supplied paths.
        with self.locks.password_lock(),ExitStack() as stack:
            for name in DATABASES:stack.enter_context(self.locks.file_lock(name))
            self.validate(recipe)
            for name in DATABASES:
                payload,metadata=self.read(name)
                saved=recipe[name]
                desired=saved['final' if final else 'login']
                actual=digest(payload)
                if actual==desired or not final and actual==saved['final']:continue
                updated=self.project(payload,name,old,new,final)
                if digest(updated)!=desired:raise IdentityRefusal('account recovery projection changed')
                self.publish(name,updated,payload,metadata)
                checkpoint('database:'+name)
