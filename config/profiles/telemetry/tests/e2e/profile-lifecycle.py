#!/usr/bin/env python3
"""Supported profile lifecycle in an existing marker-owned Android Incus yard."""
import argparse
import ast
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys

NAME = re.compile(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,127}\Z')

SNAPSHOT = '''import hashlib,json,pathlib,pwd,subprocess
account=pwd.getpwnam('dev')
assert account.pw_uid>0
raw=subprocess.check_output(['systemctl','show','subyard-android-pool.service','--property=MainPID,ControlGroup,FragmentPath,ActiveState'],text=True)
unit=dict(line.split('=',1) for line in raw.splitlines())
assert unit['ActiveState']=='active' and int(unit['MainPID'])>1
unit['start_ticks']=pathlib.Path('/proc',unit['MainPID'],'stat').read_text().rsplit(') ',1)[1].split()[19]
unit['unit_sha256']=hashlib.sha256(pathlib.Path(unit['FragmentPath']).read_bytes()).hexdigest()
print(json.dumps({'uid':account.pw_uid,'gid':account.pw_gid,'home':account.pw_dir,'pool':unit}))
'''

UNIT_FACTS = '''import json,pathlib,subprocess,sys
user,gid=sys.argv[1:]
units=[]
for unit in ('subyard-telemetry.service','subyard-telemetry-runs.service'):
    manager=unit=='subyard-telemetry-runs.service'
    properties='LoadState,FragmentPath,User,Group,DropInPaths,Type,KillMode,NoNewPrivileges,MainPID,UMask,LimitCORE,MemoryMax,TasksMax,CPUQuotaPerSecUSec,Delegate,ExecStart,ExecStartPre,ExecStartPost,ExecStop,ExecStopPost,Environment,EnvironmentFiles'
    result=subprocess.run(['systemctl','show',unit,'--property='+properties],capture_output=True,text=True,timeout=10)
    fields=dict(line.split('=',1) for line in result.stdout.splitlines() if '=' in line)
    dropins=fields.get('DropInPaths','').split()
    classes={'type-wide':0,'unit-specific':0,'other':0}
    for path in dropins:
        parent=pathlib.Path(path).parent.name
        classes['type-wide' if parent=='service.d' else 'unit-specific' if parent==unit+'.d' else 'other']+=1
    command='/usr/bin/setpriv --no-new-privs '+('/usr/bin/python3 -B /usr/local/lib/subyard-telemetry/launcher.py manager --data-dir /var/lib/subyard/telemetry --delegate' if manager else '/usr/local/lib/subyard-telemetry/subyard-telemetry serve --data-dir /var/lib/subyard/telemetry --listen 127.0.0.1:14318 --auth-file /var/lib/subyard/telemetry/auth.token')
    start=fields.get('ExecStart','')
    path=command.split()[0]
    expected={'User':user,'Group':gid,'Type':'simple','KillMode':'process' if manager else 'control-group',
              'NoNewPrivileges':'yes','UMask':'0077','LimitCORE':'0','ExecStartPre':'','ExecStartPost':'','ExecStop':'','ExecStopPost':'','Environment':'','EnvironmentFiles':''}
    checks={key:fields.get(key,'')==value for key,value in expected.items()}
    checks['FragmentPath']=fields.get('FragmentPath')=='/etc/systemd/system/'+unit
    checks['ExecStart']=start.startswith('{ path='+path+' ; argv[]='+command+' ; ignore_errors=no ; ') and start.count('argv[]=')==1
    if not manager:
        checks.update(MemoryMax=fields.get('MemoryMax')=='268435456',TasksMax=fields.get('TasksMax')=='64',CPUQuota=fields.get('CPUQuotaPerSecUSec')=='250ms')
    else: checks['Delegate']=fields.get('Delegate')=='yes'
    process_nnp=None
    try:
        pid=int(fields.get('MainPID','0'))
        if pid>1:
            status=pathlib.Path('/proc',str(pid),'status').read_text()
            process_nnp=next(line.split(':',1)[1].strip() for line in status.splitlines() if line.startswith('NoNewPrivs:'))=='1'
    except (OSError,ValueError,StopIteration): pass
    units.append(dict(manager=manager,systemctl_exit=result.returncode,load_state=fields.get('LoadState') if fields.get('LoadState') in ('loaded','not-found','error','masked') else 'other',dropins=classes,expected_matches=checks,process_no_new_privs=process_nnp))
print(json.dumps(dict(schema='subyard.telemetry.profile-unit-facts/v1',units=units)),flush=True)
'''

PREPARE = '''from contextlib import closing
import json,os,pathlib,sqlite3,subprocess,sys,tempfile,time
data=pathlib.Path('/var/lib/subyard/telemetry')
deadline=time.monotonic()+30
while not (data/'delegation.json').is_file():
    assert time.monotonic()<deadline,'Incus yard cgroup delegation unavailable'
    time.sleep(.1)
state=pathlib.Path(tempfile.mkdtemp(prefix='subyard-telemetry-incus.',dir='/tmp'))
os.chmod(state,0o700)
(state/'marker').write_bytes(b'subyard-telemetry-incus-v1')
os.chmod(state/'marker',0o600)
sys.argv=['process-scope.py','--state',str(state),'--data-dir',str(data)]
exec(compile(FIXTURE_SOURCE,'process-scope.py','exec'),{'__name__':'__main__'})
if NATIVE_STAGE:
    prepared=pathlib.Path(NATIVE_STAGE)/'native'
    os.environ['SUBYARD_TELEMETRY_NATIVE_STATE']=str(prepared)
    os.environ['SUBYARD_YARD_NAME']=FIXTURE_YARD
    subprocess.run(['git','init','--quiet',str(prepared/'project')],check=True,timeout=10)
    exercise=pathlib.Path(NATIVE_STAGE)/'source/config/profiles/telemetry/tests/e2e/native-signals.py'
    subprocess.run([sys.executable,'-B',str(exercise),'--exercise','--state',str(prepared/'exercise'),
        '--data-dir',str(data),'--source',str(pathlib.Path(NATIVE_STAGE)/'source'),'--kind','android','--',
        '/srv/cache/android-sdk/.subyard/bin/android-broker','run','--api','36','--yard',FIXTURE_YARD,'--project','telemetry-fixture',
        '--purpose','telemetry-incus-native','--','python3','-c','import time;time.sleep(2)'],check=True,timeout=650)
    bindings=json.loads((prepared/'exercise/native-resource-bindings.json').read_text())
    claude=next(run for run in bindings['runs'] if run['agent']=='claude')
    document=json.loads(subprocess.check_output(['/usr/local/lib/subyard-telemetry/subyard-telemetry','show-run',
        '--data-dir',str(data),'--run',claude['agent_run_id']],timeout=8))
    assert document['sessions'] and document['selected_nonoverlapping'],'inside-yard native pipeline has no linked evidence'
    print(json.dumps({'schema':'subyard.telemetry.acceptance-example/v1','boundary':'inside-incus-yard','document':document}),flush=True)
parent="""import json,os,pathlib,subprocess,sys
state=pathlib.Path(sys.argv[1])
run=os.environ['SUBYARD_AGENT_RUN_ID']
assert '/run-'+run+'/workload' in pathlib.Path('/proc/self/cgroup').read_text()
code='import pathlib,sys,time; state=pathlib.Path(sys.argv[1]); end=time.monotonic()+90'+chr(10)+'while not (state/"release").exists() and time.monotonic()<end: time.sleep(.05)'+chr(10)+'(state/"done").write_bytes(b"completed")'
child=subprocess.Popen([sys.executable,'-c',code,str(state)],stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
start=pathlib.Path('/proc',str(child.pid),'stat').read_text().rsplit(') ',1)[1].split()[19]
print(json.dumps({'agent_run_id':run,'pid':child.pid,'start_ticks':start}))
"""
result=subprocess.run(['subyard-telemetry-run','--agent','other','--agent-version','fixture','--scope-mode','require','--project-id','telemetry-incus-fixture','--',sys.executable,'-c',parent,str(state)],capture_output=True,check=True,timeout=15)
run=json.loads(result.stdout)
run['state']=str(state)
info=(data/'telemetry.sqlite').stat()
run['database_identity']=[info.st_dev,info.st_ino]
with closing(sqlite3.connect('file:'+str(data/'telemetry.sqlite')+'?mode=ro',uri=True,timeout=1)) as db:
    run['records_before']=db.execute('SELECT count(*) FROM records').fetchone()[0]
print(json.dumps(run),flush=True)
'''

SETUP = '''import json,os,pathlib,pwd,tempfile
account=pwd.getpwnam('dev')
root=pathlib.Path(tempfile.mkdtemp(prefix='subyard-telemetry-delegated.',dir='/tmp'))
os.chmod(root,0o700)
os.chown(root,account.pw_uid,account.pw_gid)
marker=root/'marker'
marker.write_bytes(b'subyard-telemetry-delegated-v1')
os.chmod(marker,0o600)
os.chown(marker,account.pw_uid,account.pw_gid)
print(json.dumps({'root':str(root),'unit':'subyard-telemetry-fixture-'+root.name.rsplit('.',1)[1]+'.service'}))
'''

START = '''import json,pathlib,subprocess,sys,time
fixture=json.loads(sys.argv[1])
root=pathlib.Path(fixture['root'])
assert root.parent==pathlib.Path('/tmp') and root.name.startswith('subyard-telemetry-delegated.') and not root.is_symlink()
assert (root/'marker').read_bytes()==b'subyard-telemetry-delegated-v1'
subprocess.run(['systemd-run','--quiet','--unit',fixture['unit'],'--property=User=dev','--property=Group='+sys.argv[2],
    '--property=Type=exec','--property=ExitType=cgroup','--property=RemainAfterExit=yes','--property=Delegate=cpu memory io pids',
    '--property=DelegateSubgroup=caller','--property=MemoryMax=2G','--property=TasksMax=128',
    '--property=UMask=0077','--property=LimitCORE=0','--property=NoNewPrivileges=true',
    '--setenv=HOME='+sys.argv[3],'--setenv=PATH=/usr/local/bin:/usr/bin:/bin','--setenv=LANG=C.UTF-8',
    '/usr/bin/python3','-B',str(root/'payload.py')],check=True,timeout=20)
deadline=time.monotonic()+800
while time.monotonic()<deadline:
    if (root/'result').is_file():
        print((root/'result').read_text(),end='',flush=True)
        break
    if (root/'failure').is_file():
        raise RuntimeError('delegated fixture failed: '+(root/'failure').read_text())
    time.sleep(.2)
else:
    raise AssertionError('delegated fixture did not report its result')
'''

FINISH = '''from contextlib import closing
import json,os,pathlib,sqlite3,sys,time
run=json.loads(sys.argv[1])
state=pathlib.Path(run['state'])
assert state.parent==pathlib.Path('/tmp') and state.name.startswith('subyard-telemetry-incus.')
assert not state.is_symlink() and (state/'marker').read_bytes()==b'subyard-telemetry-incus-v1'
proc=pathlib.Path('/proc',str(run['pid']))
fields=(proc/'stat').read_text().rsplit(') ',1)[1].split()
assert fields[0]!='Z' and fields[19]==run['start_ticks'],'disable killed the running descendant'
assert '/run-'+run['agent_run_id']+'/workload' in (proc/'cgroup').read_text()
data=pathlib.Path('/var/lib/subyard/telemetry')
info=(data/'telemetry.sqlite').stat()
assert [info.st_dev,info.st_ino]==run['database_identity'],'disable replaced the history database'
with closing(sqlite3.connect('file:'+str(data/'telemetry.sqlite')+'?mode=ro',uri=True,timeout=1)) as db:
    retained=db.execute('SELECT count(*) FROM records').fetchone()[0]
assert retained>=run['records_before']>0,'disable lost history'
(state/'release').touch(mode=0o600)
deadline=time.monotonic()+40
sys.path.insert(0,'/usr/local/lib/subyard-telemetry')
import records
while time.monotonic()<deadline:
    final=[]
    for path in (data/'outbox').glob('*.json'):
        record=records.read_private(path)
        body=record.get('body',{})
        if record.get('kind')=='sample' and body.get('object_id')==run['agent_run_id']+':scope' and body.get('final'):
            final.append(body)
    if (state/'done').is_file() and not (data/'runs'/(run['agent_run_id']+'.json')).exists() and final:
        assert final[-1]['populated'] is False
        print(json.dumps({'retained_records':retained,'descendant_completed':True,'final_counter_durable':True}),flush=True)
        break
    time.sleep(.2)
else:
    raise AssertionError('disabled profile did not preserve and finalize the descendant')
'''


def validate(args):
    for value in (args.yard, args.project, args.instance):
        if not value or not NAME.fullmatch(value) or value == 'default':
            raise ValueError('exact non-default fixture yard/project/instance required')


def profile_stderr_summary(output):
    """Bounded CLI error wording; never used for guest/native output or stdout."""
    text = output[:8192].decode(errors='replace')
    text = re.sub(r'\x1b\[[0-?]*[ -/]*[@-~]', '', text)
    text = re.sub(r'(?im)^(?:authorization|proxy-authorization|cookie|set-cookie|x-[a-z0-9-]+)\s*:.*$', '<header>', text)
    text = re.sub(r'(?i)\b[a-z][a-z0-9+.-]*://[^\s<>]+', '<url>', text)
    text = re.sub(r'(?is)\b(?:argv|arguments|command|cmd|environment|env|configuration|config|headers|payload|prompt|response)\s*[:=].*', '<details>', text)
    text = re.sub(r'(?i)\b(?:bearer|basic)\s+[^\s,;]+', '<credential>', text)
    text = re.sub(r'''(?i)\b(?:[a-z0-9_-]*(?:token|secret|password|credential|capability|authorization|api[_-]?key)[a-z0-9_-]*)\s*[:=]\s*(?:"[^"]*"|'[^']*'|[^\s,;]+)''', '<credential>', text)
    text = re.sub(r'''\b[a-zA-Z_][a-zA-Z0-9_.-]*\s*=\s*(?:"[^"]*"|'[^']*'|[^\s,;]+)''', '<assignment>', text)
    text = re.sub(r'''"[^"\n]*"|'[^'\n]*'|\{[^\n]*\}|\[[^\n]*\]''', '<value>', text)
    text = re.sub(r'(?:[A-Za-z]:\\|/|\./|\.\./)[^\s<>:,;]*', '<path>', text)
    text = re.sub(r'\b(?:[0-9a-fA-F-]{16,}|[A-Za-z0-9_+=.-]{32,}|[A-Z][A-Z0-9_]{2,})\b', '<identifier>', text)
    return ' '.join(''.join(character if character.isprintable() else ' ' for character in text).split())[:1024]


class CommandFailure(RuntimeError):
    def __init__(self, operation, result=None):
        facts = dict(schema='subyard.telemetry.profile-command-failure/v1', operation=operation,
                     exit_code=result.returncode if result else None, timed_out=result is None)
        signals = ('native source metadata is unavailable', 'unsafe prepared profile native source',
                   'prepared profile owner unavailable', 'profile runtime owner is unavailable',
                   'profile commands require the owner host', 'profile connection is unavailable',
                   'profile changes require an existing running yard', 'profile changes require a running yard',
                   'profile changes require an initialized managed instance', 'profile settings are',
                   'profile runtime did not converge', 'profile selection saved; runtime remains pending',
                   'profile runtime assessment changed', 'profile configuration changed',
                   'observe profile runtime', 'profile runtime handler observe failed',
                   'profile runtime handler apply failed', 'profile runtime handler output exceeded limit',
                   'profile runtime handler cancelled',
                   'remove the temporary ENVIRONMENT_PROFILES override', 'profile is disabled',
                   'telemetry: owner transport or native ownership unavailable',
                   'telemetry: local reconciliation failed', 'no such file or directory',
                   'permission denied', 'operation not permitted', 'read-only file system',
                   'owner identity', 'ownership', 'source changed', 'plan is stale',
                   'missing profile asset', 'system manager unavailable', 'foreign loaded unit',
                   'unmanaged collector assets', 'managed asset changed', 'unsafe native file',
                   'cgroup', 'unit not found', 'failed to start')
        for name in ('stdout', 'stderr'):
            output = getattr(result, name, b'') if result else b''
            facts[name+'_bytes'] = len(output)
            facts[name+'_sha256'] = hashlib.sha256(output).hexdigest()
            text = output[-65536:].decode(errors='replace').lower()
            facts[name+'_signals'] = [signal for signal in signals if signal.lower() in text]
            facts[name+'_classes'] = re.findall(r'\b(?:PermissionError|FileNotFoundError|ValueError|KeyError|RuntimeError|TimeoutExpired|CalledProcessError)\b', output[-65536:].decode(errors='replace'))[-4:]
            facts[name+'_locations'] = re.findall(r'\b(?:guest|native|owner)\.py:[0-9]{1,5}\b', text)[-4:]
        if result is not None and operation in ('profile-enable', 'profile-disable'):
            facts['stderr_summary'] = profile_stderr_summary(result.stderr)
        self.facts = facts
        super().__init__(json.dumps(facts, sort_keys=True))


def command(argv, payload=None, timeout=120, operation='guest-command', env=None):
    try:
        result = subprocess.run(argv, input=payload, capture_output=True, timeout=timeout, env=env)
    except subprocess.TimeoutExpired:
        raise CommandFailure(operation) from None
    if result.returncode:
        raise CommandFailure(operation, result)
    return result.stdout.decode()


def observe_enable_failure(args, effective):
    try:
        owner = args.engine.resolve().parents[1]/'config/profiles/telemetry/owner.py'
        assert owner.is_file(), 'installed telemetry diagnostic owner missing'
        environment = dict(os.environ)
        for key in ('INCUS_PROJECT', 'YARD_INSTANCE_NAME', 'DEV_USER', 'ENVIRONMENT_PROFILES', 'CODING_TOOL_INTEGRATIONS'):
            environment[key] = effective(key)
        assert environment['INCUS_PROJECT'] == args.project and environment['YARD_INSTANCE_NAME'] == args.instance
        for agent in environment['CODING_TOOL_INTEGRATIONS'].split():
            if agent in ('claude', 'codex'):
                key = 'AGENT_'+agent+'_CONFIG_DEST'
                environment[key] = effective(key)
        output = json.loads(command([sys.executable, '-B', str(owner), '--runtime', 'observe'], env=environment,
                                    operation='installed-owner-observe', timeout=100))
        state = output.get('state')
        print(json.dumps(dict(schema='subyard.telemetry.profile-owner-observe/v1',
              state=state if state in ('current', 'absent', 'stale', 'deferred', 'blocked') else 'unknown')), file=sys.stderr, flush=True)
    except Exception as error:
        print(json.dumps(dict(schema='subyard.telemetry.profile-owner-observe/v1', error_class=type(error).__name__,
              command=error.facts if isinstance(error, CommandFailure) else None)), file=sys.stderr, flush=True)


def cleanup(actions, primary):
    failures = []
    for operation, action in actions:
        try:
            action()
        except Exception as error:
            failures.append(error)
            print(json.dumps(dict(schema='subyard.telemetry.profile-cleanup/v1', operation=operation,
                  status='failed', error_class=type(error).__name__,
                  command=error.facts if isinstance(error, CommandFailure) else None)), file=sys.stderr, flush=True)
    if failures and primary is None:
        raise failures[0]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--engine', type=Path)
    parser.add_argument('--source', type=Path)
    parser.add_argument('--native-state', type=Path)
    for name in ('yard', 'project', 'instance'):
        parser.add_argument('--'+name)
    parser.add_argument('--preflight', action='store_true')
    args = parser.parse_args()
    source = Path(__file__).with_name('process-scope.py').read_text()
    if args.preflight:
        for script in (SNAPSHOT, UNIT_FACTS, 'FIXTURE_SOURCE='+repr(source)+'\n'+PREPARE, FINISH, SETUP, START, source):
            ast.parse(script)
        parent = next(node.value for node in ast.parse(PREPARE).body
                      if isinstance(node, ast.Assign) and any(isinstance(target, ast.Name) and target.id == 'parent' for target in node.targets))
        ast.parse(ast.literal_eval(parent))
        tree = ast.parse(Path(__file__).read_text())
        for node in ast.walk(tree):
            if isinstance(node, ast.Assign) and any(isinstance(target, ast.Name) and target.id == 'payload' for target in node.targets):
                ast.parse(ast.literal_eval(node.value))
        try:
            validate(argparse.Namespace(yard='default', project='fixture', instance='fixture'))
        except ValueError:
            pass
        else:
            raise AssertionError('default yard admitted')
        print('ok: profile lifecycle fixture syntax and exact owned-yard guard; no live commands')
        return
    validate(args)
    assert os.environ.get('SUBYARD_E2E_VM') == '1'
    command(['sudo', '-n', 'test', '-f', '/run/subyard-e2e-lease.json'], timeout=5)
    assert args.engine and args.engine.is_file() and os.access(args.engine, os.X_OK)
    prefix = [str(args.engine), '-Y', args.yard]
    def cli(*argv):
        return command([*prefix, *argv], operation='profile-'+argv[1] if argv[0] == 'profile' else 'config-show')
    def guest(script, *argv, user=None, timeout=120):
        base = ['/usr/bin/incus', 'exec', args.instance, '--project', args.project]
        if user:
            base += ['--user', str(user['uid']), '--group', str(user['gid']), '--env', 'HOME='+user['home']]
        return command([*base, '--', 'python3', '-B', '-', *argv], script.encode(), timeout=timeout)
    def effective(name):
        return next(line.removeprefix('effective: ') for line in cli('config', 'show', name).splitlines() if line.startswith('effective: '))
    assert effective('INCUS_PROJECT') == args.project and effective('YARD_INSTANCE_NAME') == args.instance
    profiles = effective('ENVIRONMENT_PROFILES').split()
    assert 'android' in profiles and 'telemetry' not in profiles
    before = json.loads(guest(SNAPSHOT))
    assert args.source and args.native_state
    fixture = json.loads(guest(SETUP))
    stage = Path(fixture['root'])
    files = {'source/config/profiles/telemetry/tests/e2e/native-signals.py': args.source/'config/profiles/telemetry/tests/e2e/native-signals.py',
             'source/config/profiles/telemetry/native.py': args.source/'config/profiles/telemetry/native.py',
             'source/internal/adapters/configmaterial/tomli_w.py': args.source/'internal/adapters/configmaterial/tomli_w.py',
             'source/config/profiles/orca/tests/e2e/codex-permissions.py': args.source/'config/profiles/orca/tests/e2e/codex-permissions.py',
             'native/bin/codex': (args.native_state/'bin/codex').resolve(), 'native/bin/claude': args.native_state/'bin/claude'}
    guest('import pathlib,sys\nfor name in sys.argv[1:]: pathlib.Path(name).parent.mkdir(parents=True,exist_ok=True,mode=0o700)\n',
          *[str(stage/name) for name in files], user=before)
    for name, path in files.items():
        assert path.is_file()
        command(['/usr/bin/incus', 'file', 'push', str(path), args.instance+str(stage/name), '--project', args.project,
                 '--uid', str(before['uid']), '--gid', str(before['gid']), '--mode', '0755' if name.startswith('native/bin/') else '0600'])
    body = 'FIXTURE_SOURCE='+repr(source)+'\nNATIVE_STAGE='+repr(str(stage))+'\nFIXTURE_YARD='+repr(args.yard)+'\n'+PREPARE
    # Keep the delegated caller alive until the external observer has durably
    # finalized every child scope; systemd must not remove empty counters first.
    payload = '''import contextlib,io,json,os,pathlib,time,traceback
root=pathlib.Path(__file__).parent
try:
    membership=pathlib.Path('/proc/self/cgroup').read_text().strip().split('::',1)[1]
    delegated=pathlib.Path('/sys/fs/cgroup')/membership.lstrip('/')
    assert delegated.name=='caller'
    delegated=delegated.parent
    limits={name:(delegated/name).read_text() for name in ('memory.max','pids.max')}
    assert limits['memory.max'].strip()==str(2*1024**3) and limits['pids.max'].strip()=='128'
    output=io.StringIO()
    with contextlib.redirect_stdout(output):
        exec(compile(BODY,'incus-profile-payload','exec'),{'__name__':'__main__'})
    assert limits=={name:(delegated/name).read_text() for name in limits}
    (root/'result.tmp').write_text(output.getvalue())
    os.chmod(root/'result.tmp',0o600)
    os.replace(root/'result.tmp',root/'result')
    deadline=time.monotonic()+150
    while list(pathlib.Path('/var/lib/subyard/telemetry/runs').glob('*.json')):
        assert time.monotonic()<deadline,'fixture scopes did not drain'
        time.sleep(.2)
except BaseException as error:
    frame=traceback.extract_tb(error.__traceback__)[-1]
    (root/'failure').write_text(type(error).__name__+' at '+pathlib.Path(frame.filename).name+':'+str(frame.lineno))
    os.chmod(root/'failure',0o600)
    raise SystemExit(1)
'''
    ast.parse(payload)
    guest('import os,pathlib,sys\np=pathlib.Path(sys.argv[1]);p.write_text(sys.argv[2]);os.chmod(p,0o600)\n',
          str(stage/'payload.py'), 'BODY='+repr(body)+'\n'+payload, user=before)
    enabled = unit_requested = False
    primary = None
    try:
        enabled = True
        try:
            cli('profile', 'enable', 'telemetry', '--yes')
        except CommandFailure:
            try:
                status = json.loads(cli('profile', 'status', 'telemetry', '--json'))
                facts = {key: status[key] for key in ('enabled', 'configured', 'ready') if type(status.get(key)) is bool}
                if status.get('runtime') in ('unavailable', 'pending', 'removed', 'ready'):
                    facts['runtime'] = status['runtime']
                print(json.dumps(dict(schema='subyard.telemetry.profile-enable-status/v1', **facts)), file=sys.stderr, flush=True)
            except Exception as error:
                print(json.dumps(dict(schema='subyard.telemetry.profile-enable-status/v1', error_class=type(error).__name__,
                      command=error.facts if isinstance(error, CommandFailure) else None)), file=sys.stderr, flush=True)
            observe_enable_failure(args, effective)
            try:
                print(guest(UNIT_FACTS, effective('DEV_USER'), str(before['gid']), timeout=25).strip(),file=sys.stderr,flush=True)
            except Exception as error:
                print(json.dumps(dict(schema='subyard.telemetry.profile-unit-facts/v1',error_class=type(error).__name__)),file=sys.stderr,flush=True)
            raise
        status = json.loads(cli('profile', 'status', 'telemetry', '--json'))
        assert status['enabled'] and status['ready'] and status['runtime'] == 'ready'
        unit_requested = True
        output = guest(START, json.dumps(fixture), str(before['gid']), before['home'], timeout=840)
        for line in output.splitlines():
            if line.startswith('{') and json.loads(line).get('schema') == 'subyard.telemetry.acceptance-example/v1':
                print(line, flush=True)
        run = json.loads(output.splitlines()[-1])
        cli('profile', 'disable', 'telemetry', '--yes')
        enabled = False
        status = json.loads(cli('profile', 'status', 'telemetry', '--json'))
        assert not status['enabled'] and not status['ready'] and status['runtime'] == 'removed'
        finished = json.loads(guest(FINISH, json.dumps(run), user=before))
        assert effective('ENVIRONMENT_PROFILES').split() == profiles
        assert json.loads(guest(SNAPSHOT)) == before, 'telemetry changed the existing Android pool'
        print(json.dumps({'schema': 'subyard.telemetry.incus-profile/v1', 'status': 'pass', 'supported_profile_lifecycle': True,
                          'managed_scope_inside_incus': True, 'android_pool_unchanged': True, **finished}), flush=True)
    except BaseException as error:
        primary = error
        raise
    finally:
        actions = [('disable', lambda: cli('profile', 'disable', 'telemetry', '--yes'))] if enabled else []
        if unit_requested:
            actions.append(('stop-unit', lambda: guest('import subprocess,sys\nstate=subprocess.run(["systemctl","show",sys.argv[1],"--property=LoadState","--value"],capture_output=True,text=True,timeout=5)\nif state.stdout.strip()!="not-found": subprocess.run(["systemctl","stop",sys.argv[1]],check=True,timeout=20)\n', fixture['unit'])))
        cleanup(actions, primary)


if __name__ == '__main__':
    main()
