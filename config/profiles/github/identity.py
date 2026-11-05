"""Bounded GitHub broker and managed Git include identity owner."""
import hashlib
import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile

SOURCE=Path(__file__).resolve().parent
PREFIX='GitHub identity: '


def refusal(error):
    import importlib.util
    try:
        spec=importlib.util.spec_from_file_location('authority',SOURCE.parents[1]/'identity/authority.py')
        module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
    except OSError:
        return PREFIX+'owned state could not be verified; resolve it, then retry plain yard init'
    return module.refusal('GitHub',error)


def guest(arguments):
    program='import sys,types\n'
    for name,path in (('authority',SOURCE.parents[1]/'identity/authority.py'),('git_wiring',SOURCE/'git_wiring.py'),('identity_guest',SOURCE/'identity_guest.py')):
        program+=f"module=types.ModuleType({name!r});sys.modules[{name!r}]=module;exec({path.read_text()!r},module.__dict__)\n"
    program+='try:sys.modules["identity_guest"].main(sys.argv[1:])\n'
    program+='except Exception as error:print(sys.modules["authority"].refusal("GitHub",error),file=sys.stderr);raise SystemExit(1)\n'
    result=subprocess.run(['incus','exec',os.environ['YARD_INSTANCE_NAME'],'--project',os.environ['INCUS_PROJECT'],'--','python3','-B','-c',program,*arguments],capture_output=True,timeout=30)
    if result.returncode:
        # Relay only the guest's own refusal line; refusal() filters it again on the host.
        lines=[line[len(PREFIX):] for line in result.stderr.decode('utf-8','replace').splitlines() if line.startswith(PREFIX)]
        raise ValueError(lines[-1] if lines else 'Git identity guest operation failed')
    return json.loads(result.stdout)


def read(path,mode):
    for parent in path.parents:
        if parent.is_symlink():raise ValueError('symlinked broker identity parent')
        if parent.exists():
            info=parent.stat()
            if not stat.S_ISDIR(info.st_mode) or info.st_uid not in (0,os.getuid()) or info.st_mode&0o022 and not (info.st_uid==0 and info.st_mode&stat.S_ISVTX):
                raise ValueError('unsafe broker identity parent')
    if not path.exists() and not path.is_symlink():return None
    info=path.lstat()
    if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode)!=mode or info.st_uid!=os.getuid() or info.st_nlink!=1 or info.st_size>64*1024*1024:
        raise ValueError('unsafe broker identity artifact')
    return path.read_bytes()


def digest(value):return hashlib.sha256(value).hexdigest() if value is not None else None


def manager(unit,*arguments):
    environment=dict(os.environ,XDG_RUNTIME_DIR=f'/run/user/{os.getuid()}',DBUS_SESSION_BUS_ADDRESS=f'unix:path=/run/user/{os.getuid()}/bus')
    result=subprocess.run(['systemctl','--user',*arguments,unit],capture_output=True,env=environment,timeout=20,check=True)
    return result.stdout.decode()


def projected(content,user):
    value=json.loads(content)
    if set(value)!={'app_config','key_file','ssh_port','developer','identity_file','known_hosts_file'}:
        raise ValueError('unknown broker runtime contract')
    return subprocess.run(['jq','--arg','developer',user,'.developer = $developer'],input=content,capture_output=True,check=True).stdout


def run(arguments):
    action=arguments[0]
    result=guest(['identity-observe',os.environ['DEV_USER'],*arguments[1:]])
    recipe,phase=result['recipe'],result['phase']
    yard=os.environ.get('SUBYARD_YARD') or 'default'
    state=Path(os.environ['SUBYARD_HOME'])/'github-broker'
    path=state/(yard+'.json')
    unit='subyard-github-'+yard+'.service'
    unit_path=Path(os.environ['SUBYARD_OPERATOR_HOME'])/'.config/systemd/user'/unit
    engine=state/(yard+'-engine')
    before=read(path,0o600)
    unit_bytes=read(unit_path,0o644)
    if phase is None:
        if before is None and unit_bytes is not None:raise ValueError('partial broker identity ownership')
        if before is not None:
            import runtime
            observed,_=runtime.observe()
            if observed['state']!='current':raise ValueError('broker identity requires verified ownership')
            if json.loads(before)['developer']!=recipe['original']['user']:raise ValueError('broker developer changed')
        fields=dict(line.split('=',1) for line in manager(unit,'show','--property=ActiveState,UnitFileState,FragmentPath').splitlines() if '=' in line) if before is not None else {}
        recipe['broker']=dict(installed=before is not None,before=digest(before),after=digest(projected(before,recipe['target']['user'])) if before is not None else None,
                              unit=digest(unit_bytes),engine=digest(read(engine,0o700)),manager=fields)
    else:
        captured=recipe['broker']
        if digest(before) not in (captured['before'],captured['after']) or digest(unit_bytes)!=captured['unit'] or digest(read(engine,0o700))!=captured['engine']:
            raise ValueError('broker identity artifacts changed')
        if captured['installed']:
            fields=dict(line.split('=',1) for line in manager(unit,'show','--property=ActiveState,UnitFileState,FragmentPath').splitlines() if '=' in line)
            expected=captured['manager']
            active=fields.get('ActiveState')
            allowed=active in (expected.get('ActiveState'),'inactive')
            # The host broker survives a guest reboot, but its SSH child may
            # fail while the guest is down. Only the protected guest journal's
            # boot change permits that exact owned service to be re-quiesced.
            if not allowed and result.get('rebooted') is True and expected.get('ActiveState')=='active':
                allowed=active=='failed' or (active=='activating' and manager(unit,'show','--property=SubState','--value').strip()=='auto-restart')
            if fields.get('FragmentPath')!=str(unit_path) or fields.get('UnitFileState')!=expected.get('UnitFileState') or not allowed:
                raise ValueError('broker identity manager changed')
    if action=='identity-observe':
        print(json.dumps(recipe));return
    captured=recipe['broker']
    if not captured['installed']:
        if phase=='account-ready':guest(['identity-apply',os.environ['DEV_USER'],arguments[-1]])
        return
    if phase in ('prepared','quiesced','home','account'):
        manager(unit,'stop');return
    if phase!='account-ready':raise ValueError('invalid broker identity phase')
    guest(['identity-apply',os.environ['DEV_USER'],arguments[-1]])
    after=projected(before,recipe['target']['user'])
    if digest(after)!=captured['after']:raise ValueError('broker identity projection changed')
    if before!=after:
        fd,temporary=tempfile.mkstemp(prefix='.identity-',dir=path.parent)
        try:
            with os.fdopen(fd,'wb') as stream:
                os.fchmod(stream.fileno(),0o600);stream.write(after);stream.flush();os.fsync(stream.fileno())
            if read(path,0o600)!=before:raise ValueError('broker identity runtime changed')
            os.replace(temporary,path)
            descriptor=os.open(path.parent,os.O_DIRECTORY|os.O_NOFOLLOW)
            try:os.fsync(descriptor)
            finally:os.close(descriptor)
        finally:
            if os.path.exists(temporary):os.unlink(temporary)
    if captured['manager']['ActiveState']=='active':manager(unit,'start')

    fields=dict(line.split('=',1) for line in manager(unit,'show','--property=ActiveState,UnitFileState,FragmentPath').splitlines() if '=' in line)
    if fields != captured['manager'] or digest(read(path,0o600))!=captured['after']:
        raise ValueError('broker identity restoration failed')
