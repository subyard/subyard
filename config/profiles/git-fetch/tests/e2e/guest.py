#!/usr/bin/env python3
"""Direct installed-component acceptance on one disposable systemd guest."""
import importlib.util
import json
import os
from pathlib import Path
import pwd
import shutil
import subprocess
import tempfile
import time

PROFILE = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('guest', PROFILE/'guest.py')
guest = importlib.util.module_from_spec(spec)
spec.loader.exec_module(guest)


def run(*args, capture=False):
    result = subprocess.run(list(map(str, args)), check=True, timeout=20,
                            stdout=subprocess.PIPE if capture else subprocess.DEVNULL,
                            stderr=subprocess.DEVNULL)
    return result.stdout.decode().strip() if capture else ''


def wait_for(predicate, seconds=20):
    deadline = time.monotonic()+seconds
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(.2)
    raise AssertionError('installed component wait expired')


def main():
    assert os.geteuid() == 0
    user = 'dev'
    account = pwd.getpwnam(user)
    runtime = guest.Runtime(PROFILE, user, True)
    assert not runtime.state.exists()
    assert all(not path.exists() and not path.is_symlink() for path in runtime.assets)
    assert all(runtime.manager_details(unit).get('LoadState') == 'not-found' for unit in (guest.SERVICE, guest.TIMER))
    workspaces = Path('/srv/workspaces')
    assert not list(workspaces.glob('*/.subyard-meta.json')), 'guest must have no managed projects'
    for directory in (workspaces,runtime.state.parent):
        if not directory.exists():
            directory.mkdir(mode=0o755, parents=True)
            directory.chmod(0o755)
        assert directory.stat().st_uid == 0 and directory.stat().st_mode & 0o777 == 0o755
    fixture = Path(tempfile.mkdtemp(prefix='git-fetch-e2e-', dir='/tmp'))
    project = workspaces/fixture.name
    events = fixture/'events'
    dropin = runtime.units/(guest.TIMER+'.d')/'fixture.conf'
    installed = mounted = False
    try:
        os.chown(fixture, account.pw_uid, account.pw_gid)
        project.mkdir(mode=0o755)
        os.chown(project, account.pw_uid, account.pw_gid)
        descriptor = project/'.subyard-meta.json'
        descriptor.write_text(json.dumps({'schema':1,'projectId':project.name,'name':'Fixture'}))
        descriptor.chmod(0o600)
        os.chown(descriptor, account.pw_uid, account.pw_gid)
        def git(*args):
            return run('runuser','-u',user,'--','git',*args,capture=True)
        source = fixture/'source'
        git('init','-q',source)
        git('-C',source,'-c','user.name=Fixture','-c','user.email=fixture@example.invalid','commit','-qm','first','--allow-empty')
        mainrepo = fixture/'main'
        git('clone','-q',source,mainrepo)
        git('clone','-q',source,mainrepo/'private')
        git('clone','-q',source,mainrepo/'other')
        git('-C',mainrepo,'remote','add','broken',fixture/'absent-remote')
        original = git('-C',mainrepo,'rev-parse','HEAD')
        (mainrepo/'.gitignore').write_text('private/\n')
        upload = fixture/'upload'
        upload.write_text('#!/bin/sh\nprintf "start %s " "$$" >> '+str(events)+'\nawk \'/^Uid:|^NoNewPrivs:/ {printf "%s ", $2} END {print ""}\' /proc/self/status >> '+str(events)+'\nsleep 3\ngit-upload-pack "$@"\nprintf "end %s\\n" "$$" >> '+str(events)+'\n')
        upload.chmod(0o755)
        for repo in (mainrepo, mainrepo/'private', mainrepo/'other'):
            git('-C',repo,'config','remote.origin.uploadpack',upload)
        git('-C',source,'-c','user.name=Fixture','-c','user.email=fixture@example.invalid','commit','-qm','second','--allow-empty')
        target = git('-C',source,'rev-parse','HEAD')
        (project/'src').mkdir(mode=0o755)
        run('mount','--bind',mainrepo,project/'src')
        mounted = True
        installed = True
        assert runtime.apply()
        inode = (runtime.units/guest.SERVICE).stat().st_ino
        assert runtime.apply()
        assert (runtime.units/guest.SERVICE).stat().st_ino == inode
        assert 'OnUnitInactiveSec=5min' in (runtime.units/guest.TIMER).read_text()
        # Shorten only this disposable fixture's timer; remove before lifecycle checks.
        dropin.parent.mkdir()
        dropin.write_text('[Timer]\nOnBootSec=\nOnUnitInactiveSec=\nOnUnitInactiveSec=1s\n')
        dropin.chmod(0o644)
        run('systemctl','daemon-reload')
        run('systemctl','restart',guest.TIMER)
        run('systemctl','start','--no-block',guest.SERVICE)
        wait_for(lambda: events.exists() and len(events.read_text().splitlines()) >= 12, 35)
        run('systemctl','stop',guest.TIMER,guest.SERVICE)
        lines = events.read_text().splitlines()
        for start,end in zip(lines[:12:2],lines[1:12:2]):
            fields = start.split()
            assert fields[0] == 'start' and fields[2:] == [str(account.pw_uid),'1']
            assert end == 'end '+fields[1], 'fetches overlapped'
        for repo in (mainrepo,mainrepo/'private',mainrepo/'other'):
            assert git('-C',repo,'rev-parse','origin/HEAD') == target
            assert git('-C',repo,'rev-parse','HEAD') == original
        dropin.unlink()
        dropin.parent.rmdir()
        run('systemctl','daemon-reload')
        assert runtime.apply()
        previous = len(events.read_text().splitlines())
        run('systemctl','start','--no-block',guest.SERVICE)
        wait_for(lambda: len(events.read_text().splitlines()) > previous)
        last_pid = events.read_text().splitlines()[-1].split()[1]
        group = run('systemctl','show',guest.SERVICE,'--property=ControlGroup','--value',capture=True)
        assert group.startswith('/') and Path(group).name == guest.SERVICE
        members = Path('/sys/fs/cgroup')/group.lstrip('/')/'cgroup.procs'
        assert guest.Runtime(PROFILE,user,False).apply()
        assert runtime.manager(guest.SERVICE)[0] in ('inactive','failed')
        assert run('systemctl','show',guest.SERVICE,'--property=MainPID','--value',capture=True) == '0'
        wait_for(lambda: not members.exists() or not members.read_text().strip(),5)
        process = Path('/proc')/last_pid/'stat'
        assert not process.exists() or process.read_text().rsplit(')',1)[1].split()[0] in ('Z','X')
        assert runtime.manager(guest.TIMER) == ('inactive','disabled')
        print('PASS: install/repeat, native NNP/dev UID, serial main+nested fetch, recurrence after slow cycle, disable')
    finally:
        if dropin.exists():
            dropin.unlink()
            dropin.parent.rmdir()
            run('systemctl','daemon-reload')
        if installed:
            # The fixture proved names absent before publishing. Verify original
            # files and loaded fragments before cleanup, even if policy rejected.
            assert runtime.owned()
            for path,content in runtime.assets.items():
                assert runtime.fact(path) == [0o644,content]
            loaded = []
            for unit in (guest.TIMER,guest.SERVICE):
                fields = runtime.manager_details(unit)
                if fields.get('LoadState') != 'not-found':
                    assert fields.get('LoadState') == 'loaded'
                    assert fields.get('FragmentPath') == str(runtime.units/unit)
                    loaded.append(unit)
            if loaded:
                run('systemctl','stop',*loaded)
            run('systemctl','disable',guest.TIMER)
            for path in runtime.assets:
                path.unlink()
            (runtime.state/'marker').unlink()
            runtime.state.rmdir()
            run('systemctl','daemon-reload')
        if mounted:
            run('umount',project/'src')
        if project.exists():
            assert descriptor.read_text() == json.dumps({'schema':1,'projectId':project.name,'name':'Fixture'})
            shutil.rmtree(project)
        shutil.rmtree(fixture)
        print('git-fetch component fixture cleanup complete')


if __name__ == '__main__':
    main()
