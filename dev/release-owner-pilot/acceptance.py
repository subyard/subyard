#!/usr/bin/env python3
"""Real-process, marker-owned /tmp acceptance for the bounded native owner."""
import argparse,fcntl,json,os,pathlib,queue,shutil,signal,subprocess,tempfile,threading,time

P=argparse.ArgumentParser();P.add_argument('--owner',required=True);P.add_argument('--native',required=True);P.add_argument('--source',default=str(pathlib.Path(__file__).resolve().parents[2]));P.add_argument('--preflight',action='store_true');P.add_argument('--case');A=P.parse_args()
OWNER=str(pathlib.Path(A.owner).resolve());NATIVE=str(pathlib.Path(A.native).resolve());SOURCE=pathlib.Path(A.source).resolve()
assert pathlib.Path(OWNER).is_file() and pathlib.Path(NATIVE).is_file()
if A.preflight:
 print('preflight: native artifacts and source available; all effects require a fresh marker-owned /tmp root');raise SystemExit(0)

def fixture():
 root=pathlib.Path(tempfile.mkdtemp(prefix='subyard-owner-',dir='/tmp'));os.chmod(root,0o700)
 shutil.copytree(SOURCE/'config',root/'repository'/'config')
 for p in (root/'repository').rglob('*'):
  p.chmod(0o700 if p.is_dir() else 0o600|(p.stat().st_mode&0o100))
 (root/'repository').chmod(0o700)
 for name,content in {'owner.marker':'subyard-release-owner-pilot-v1\n','.sync/settings/config.env':'SSH_PORT=2240\n','.sync/settings/yards/named/config.env':'SSH_PORT=2241\n','yards/named/config.env':'# retained\nDEV_SUDO=0\n'}.items():
  p=root/name;p.parent.mkdir(parents=True,exist_ok=True);p.write_text(content);p.chmod(0o600)
 return root

PROCESSES=[]
NATIVE_PIDS=set()
def launch(root,mode='new',cut='none',yard='named',native=NATIVE):
 p=subprocess.Popen([OWNER,str(root),native,yard,mode,cut],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True,bufsize=1)
 p.events=queue.Queue();p.lines=[];PROCESSES.append(p)
 def read():
  for line in p.stdout:
   p.lines.append(line);value=json.loads(line)
   if value.get('event')=='native':NATIVE_PIDS.add(value['pid'])
   p.events.put(value)
  p.events.put(None)
 threading.Thread(target=read,daemon=True).start();return p

def event(p,kind,timeout=15):
 until=time.monotonic()+timeout
 while time.monotonic()<until:
  value=p.events.get(timeout=max(.001,until-time.monotonic()))
  if value is None:break
  if value.get('event')==kind:return value
 raise AssertionError(f'missing event {kind}; code={p.poll()} stderr={p.stderr.read() if p.poll() is not None else "running"}')

def consent(p,mutate=None):
 value=event(p,'prepared');
 if mutate:mutate()
 p.stdin.write(json.dumps({'version':1,'consent':value['plan']})+'\n');p.stdin.flush()

def finish(p,ok=True):
 if p.stdin is not None:p.stdin.close();p.stdin=None
 p.wait(timeout=20);err=p.stderr.read();out=''.join(p.lines)
 assert (p.returncode==0)==ok,(p.returncode,out,err)
 assert 'SSH_PORT=' not in out+err,(out,err)
 return out,err


def target(root):return root/'yards/named/config.env'
def preserved(root):
 assert (root/'.sync/settings/config.env').read_text()=='SSH_PORT=2240\n'
 assert (root/'.sync/settings/yards/named/config.env').read_text()=='SSH_PORT=2241\n'
 assert target(root).read_text()=="# retained\nDEV_SUDO=0\nSSH_PORT='2241'\n"
 assert target(root).stat().st_mode&0o777==0o600
 assert (root/'.owner/record.json').stat().st_mode&0o777==0o600
 assert '# retained' not in (root/'.owner/record.json').read_text()

def resume(root):
 before=target(root).stat().st_ino;p=launch(root,'resume');finish(p);preserved(root)
 if 'SSH_PORT=' in target(root).read_text():assert target(root).stat().st_ino==before

def run_case(name,fn):
 if A.case and name!=A.case:return
 root=fixture()
 try:fn(root);print('PASS',name,flush=True)
 finally:
  for p in PROCESSES:
   if p.poll() is None:p.kill();p.wait(timeout=5)
  for pid in NATIVE_PIDS:
   descriptors=pathlib.Path(f'/proc/{pid}/fd')
   try:owned=any(os.readlink(fd)==str(root) for fd in descriptors.iterdir())
   except (FileNotFoundError,ProcessLookupError,PermissionError):owned=False
   if owned:
    try:os.kill(pid,signal.SIGKILL)
    except ProcessLookupError:pass
    until=time.monotonic()+5
    while time.monotonic()<until:
     try:owned=any(os.readlink(fd)==str(root) for fd in descriptors.iterdir())
     except (FileNotFoundError,ProcessLookupError,PermissionError):owned=False
     if not owned:break
     time.sleep(.01)
    assert not owned,'native did not release fixture descriptors during teardown'
  PROCESSES.clear();NATIVE_PIDS.clear();shutil.rmtree(root)

def normal(root):
 p=launch(root);consent(p);finish(p);preserved(root);resume(root);finish(launch(root),False)
for mask in (0o077,0o022):
 previous=os.umask(mask)
 try:run_case(f'normal-umask-{mask:04o}',normal)
 finally:os.umask(previous)

def owner_cut(root,point):
 p=launch(root,cut=point);consent(p);event(p,'cut');p.kill();finish(p,False)
 if point=='before_intent':assert not (root/'.owner/record.json').exists();p=launch(root);consent(p);finish(p)
 else:
  p=launch(root,'resume');finish(p)
 preserved(root);resume(root)
for point in ('before_intent','after_intent','after_verify'):run_case(point,lambda r,p=point:owner_cut(r,p))

def native_cut(root,point,kill_owner):
 p=launch(root,cut=point);consent(p)
 while True:
  native=event(p,'native')
  if native['op']=='apply':break
 pid=native['pid']
 readiness=event(p,'native_cut')
 assert readiness['pid']==pid and readiness['point']==point,(native,readiness)
 if point=='after_cas':assert "SSH_PORT='2241'" in target(root).read_text()
 else:assert 'SSH_PORT=' not in target(root).read_text()

 if kill_owner:
  p.kill();p.wait(timeout=5)
  fd=os.open(root,os.O_RDONLY|os.O_DIRECTORY)
  try:
   try:fcntl.flock(fd,fcntl.LOCK_EX|fcntl.LOCK_NB)
   except BlockingIOError:pass
   else:raise AssertionError('orphan native lost root custody')
  finally:os.close(fd)
  p.stdin.write('c');p.stdin.flush();p.stdin.close();p.stdin=None;finish(p,False)
  # Fresh owner acquisition waits for the surviving exact native to exit.
  q=launch(root,'resume');finish(q)
 else:
  os.kill(pid,signal.SIGKILL);finish(p,False);q=launch(root,'resume');finish(q)
 preserved(root);resume(root)
for point in ('before_cas','after_cas'):
 for owner in (False,True):run_case(f'{point}-kill-{"owner" if owner else "native"}',lambda r,p=point,o=owner:native_cut(r,p,o))

def timeout(root):
 p=launch(root,cut='after_cas');consent(p);finish(p,False);q=launch(root,'resume');finish(q);preserved(root)
run_case('timeout-drains-native',timeout)

def drift(root,source):
 p=launch(root);consent(p,lambda:(root/'.sync/settings/config.env' if source else target(root)).write_text('SSH_PORT=3399\n'));finish(p,False)
 assert not (root/'.owner/record.json').exists()
run_case('late-source-drift',lambda r:drift(r,True));run_case('late-target-drift',lambda r:drift(r,False))

def durable_drift(root,source):
 p=launch(root,cut='after_intent');consent(p);assert event(p,'cut')['point']=='after_intent';p.kill();finish(p,False)
 record=root/'.owner/record.json';intent=record.read_bytes()
 assert json.loads(intent)['phase']=='intent'
 assert 'SSH_PORT=' not in target(root).read_text(),'fault cut was after the effect'
 changed=root/'.sync/settings/config.env' if source else target(root)
 changed.write_text('SSH_PORT=3399\n')
 before=target(root).read_bytes();inode=target(root).stat().st_ino
 q=launch(root,'resume');finish(q,False)
 assert not any(json.loads(line).get('op')=='apply' for line in q.lines),'conflict attempted an effect'
 assert record.read_bytes()==intent,'refusal changed durable intent'
 assert target(root).read_bytes()==before and target(root).stat().st_ino==inode,'refusal changed target'
 assert changed.read_text()=='SSH_PORT=3399\n','refusal changed drifted input'
run_case('durable-source-drift',lambda r:durable_drift(r,True));run_case('durable-target-drift',lambda r:durable_drift(r,False))

def fifo_refusal(root,kind):
 before=target(root).read_bytes();inode=target(root).stat().st_ino
 if kind=='record':
  (root/'.owner').mkdir(mode=0o700);leaf=root/'.owner/record.json'
 elif kind=='marker':
  leaf=root/'owner.marker';leaf.unlink()
 else:leaf=root/'native.fifo'
 os.mkfifo(leaf,0o600);leaf.chmod(0o600)
 p=launch(root,'resume',native=str(leaf) if kind=='helper' else NATIVE)
 p.wait(timeout=3);_,error=finish(p,False)
 assert error=='release-owner-pilot: protected-mode\n',error
 assert target(root).read_bytes()==before and target(root).stat().st_ino==inode
 if kind!='record':assert not (root/'.owner').exists(),'FIFO refusal created state'
for kind in ('record','marker','helper'):run_case(kind+'-fifo-refusal',lambda r,k=kind:fifo_refusal(r,k))

def disconnected(root):
 p=launch(root);event(p,'prepared')
 fd=os.open(root,os.O_RDONLY|os.O_DIRECTORY)
 try:fcntl.flock(fd,fcntl.LOCK_EX|fcntl.LOCK_NB);fcntl.flock(fd,fcntl.LOCK_UN)
 finally:os.close(fd)
 p.stdin.close();p.stdin=None;finish(p,False);assert not (root/'.owner/record.json').exists()
run_case('disconnect-before-consent',disconnected)

def bad_record(root,symlink):
 p=launch(root);consent(p);finish(p)
 record=root/'.owner/record.json';record.unlink()
 if symlink:record.symlink_to(root/'owner.marker')
 else:record.write_text('{}');record.chmod(0o600)
 finish(launch(root,'resume'),False)
run_case('corrupt-state',lambda r:bad_record(r,False));run_case('symlink-state',lambda r:bad_record(r,True))

def binding_refusal(root,kind):
 p=launch(root);consent(p);finish(p);inode=target(root).stat().st_ino
 if kind=='scope':finish(launch(root,'resume',yard='foreign'),False)
 elif kind=='mode':(root/'.owner/record.json').chmod(0o644);finish(launch(root,'resume'),False)
 else:
  fd,path=tempfile.mkstemp(prefix='subyard-owner-artifact-',dir='/tmp');os.close(fd)
  try:
   shutil.copyfile(NATIVE,path);os.chmod(path,0o700)
   with open(path,'ab') as file:file.write(b'changed')
   finish(launch(root,'resume',native=path),False)
  finally:os.unlink(path)
 assert target(root).stat().st_ino==inode
for kind in ('scope','mode','artifact'):run_case(kind+'-binding-refusal',lambda r,k=kind:binding_refusal(r,k))

def wrong_consent(root):
 p=launch(root);event(p,'prepared');p.stdin.write('{"version":1,"consent":"foreign"}\n');p.stdin.flush();finish(p,False);assert not (root/'.owner/record.json').exists()
run_case('wrong-consent',wrong_consent)

def codec_refusals(root):
 fd=os.open(root,os.O_RDONLY|os.O_DIRECTORY);fcntl.flock(fd,fcntl.LOCK_EX)
 try:
  for document in (b'{',b'{}{}',b'{"version":1.5}',b'{"version":1,"version":2}',b' {"op":"prepare","pause":"","recipe":null,"version":1,"yard":"named"}',b'{"version":2,"op":"prepare","yard":"named","recipe":null,"pause":""}',b'{"version":1,"op":"execute","yard":"named","recipe":null,"pause":""}',b'{"version":1,"op":"prepare","yard":"named","recipe":null,"pause":"","extra":true}'):
   result=subprocess.run([NATIVE,str(fd),'-1'],input=document,pass_fds=(fd,),capture_output=True,timeout=5)
   assert result.returncode!=0 and not result.stdout and result.stderr==b'release-owner-native: refused\n'
 finally:os.close(fd)
run_case('native-codec-refusals',codec_refusals)
def root_alias(root):
 alias=pathlib.Path(str(root)+'-alias');alias.symlink_to(root,target_is_directory=True)
 try:
  finish(launch(str(alias)+'/'),False)
  assert not (root/'.owner').exists(),'rejected root alias created state'
 finally:alias.unlink()
run_case('root-alias-before-state',root_alias)
print('PASS bounded native owner acceptance')
