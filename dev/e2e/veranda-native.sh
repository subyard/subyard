#!/usr/bin/env bash
# Native client acceptance inside the already allocated Veranda owner fixture.
set -Eeuo pipefail
umask 077
die() { printf 'veranda-native: %s\n' "$*" >&2; exit 1; }
packet_loss_program() {
  cat <<'PYLOSS'
import hashlib,json,os,pathlib,re,stat,subprocess,sys
def owned_table(value,name,comment):
    tables=[item['table'] for item in value['nftables'] if 'table' in item and item['table'].get('family')=='ip' and item['table'].get('name')==name]
    if not tables: return False
    if len(tables)!=1 or tables[0].get('comment')!=comment: raise ValueError('packet loss table ownership mismatch')
    return True
def drop_count(value,name,comment):
    if not owned_table(value,name,comment): raise ValueError('packet loss table missing')
    rules=[item['rule'] for item in value['nftables'] if 'rule' in item]
    if len(rules)!=2: raise ValueError('packet loss rules changed')
    total=0
    for rule in rules:
        if rule.get('family')!='ip' or rule.get('table')!=name or rule.get('chain')!='output' or rule['expr'][-1]!={'drop':None}:
            raise ValueError('packet loss rule ownership mismatch')
        counters=[expr['counter'] for expr in rule['expr'] if 'counter' in expr]
        if len(counters)!=1: raise ValueError('packet loss counter missing')
        count=counters[0]['packets']
        if type(count) is not int or not 0<=count<=18446744073709551615: raise ValueError('packet loss counter invalid')
        total+=count
    if not 0<total<=18446744073709551615: raise ValueError('packet loss not observed')
    return total
if sys.argv[1:]==['--self-test']:
    import copy
    value={'nftables':[{'table':{'family':'ip','name':'owned','comment':'marker'}},
        *[{'rule':{'family':'ip','table':'owned','chain':'output','expr':[{'counter':{'packets':n}},{'drop':None}]}} for n in [2,3]]]}
    assert drop_count(value,'owned','marker')==5
    assert not owned_table({'nftables':[]},'owned','marker')
    for change in ['comment','duplicate','family','chain','verdict','missing','zero','negative','bool','string','overflow']:
        bad=copy.deepcopy(value)
        if change=='comment': bad['nftables'][0]['table']['comment']='foreign'
        elif change=='duplicate': bad['nftables'].append(bad['nftables'][0])
        elif change=='family': bad['nftables'][1]['rule']['family']='inet'
        elif change=='chain': bad['nftables'][1]['rule']['chain']='foreign'
        elif change=='verdict': bad['nftables'][1]['rule']['expr'][-1]={'accept':None}
        elif change=='missing': bad['nftables'][1]['rule']['expr'].pop(0)
        else:
            for item in bad['nftables'][1:]: item['rule']['expr'][0]['counter']['packets']={'zero':0,'negative':-1,'bool':True,'string':'2','overflow':18446744073709551616}[change]
        try: drop_count(bad,'owned','marker')
        except (ValueError,KeyError,IndexError): pass
        else: raise AssertionError('unsafe packet loss evidence accepted')
    print('ok: packet loss ownership and counter contracts'); raise SystemExit(0)
def private_read(path,uid,limit=4096):
    fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK)
    try:
        info=os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode)!=0o600 or info.st_uid!=uid or info.st_nlink!=1 or info.st_size>limit:
            raise ValueError('unsafe packet loss fixture file')
        return os.read(fd,limit+1)
    finally: os.close(fd)
def guard():
    root=pathlib.Path(__file__).resolve().parent.parent; uid,device,inode,lease_expected,shell_pid,shell_start=IDENTITY
    if os.geteuid()!=0 or not re.fullmatch(r'/var/tmp/subyard-veranda-owner\.[A-Za-z0-9]+/native\.[A-Za-z0-9]+',str(root)):
        raise ValueError('packet loss requires allocated guest root')
    info=root.lstat(); owner=root.parent; parent=owner.lstat()
    if not stat.S_ISDIR(info.st_mode) or stat.S_IMODE(info.st_mode)!=0o700 or (info.st_uid,info.st_dev,info.st_ino)!=(uid,device,inode) or root.resolve()!=root:
        raise ValueError('packet loss fixture identity changed')
    if not stat.S_ISDIR(parent.st_mode) or stat.S_IMODE(parent.st_mode)!=0o700 or parent.st_uid!=uid or owner.resolve()!=owner:
        raise ValueError('unsafe packet loss owner fixture')
    if private_read(root/'.marker',uid)!=b'subyard-veranda-native-v1\n' or private_read(owner/'.marker',uid)!=('subyard-veranda-owner-v1-'+owner.name.rsplit('.',1)[1].lower()+'\n').encode():
        raise ValueError('packet loss fixture marker mismatch')
    lease=pathlib.Path('/run/subyard-e2e-lease.json'); info=lease.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid!=0 or stat.S_IMODE(info.st_mode)!=0o444 or info.st_nlink!=1 or info.st_size>4096:
        raise ValueError('unsafe packet loss lease marker')
    context=json.loads(lease.read_bytes())
    if {key:context.get(key) for key in lease_expected}!=lease_expected: raise ValueError('packet loss lease identity changed')
    port=private_read(root/'owner.port',uid)
    if not port.isdigit() or not 1<=int(port)<=65535: raise ValueError('invalid packet loss port')
    name='veranda_loss_'+hashlib.sha256(str(root).encode()).hexdigest()[:24]
    return root,uid,int(port),name,'subyard-veranda-packet-loss-v1-'+name,shell_pid,shell_start
def nft(*args,input=None):
    result=subprocess.run(['/usr/sbin/nft',*args],input=input,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,timeout=5)
    if result.returncode or len(result.stdout)>65536: raise ValueError('packet loss nft command failed')
    return json.loads(result.stdout) if '-j' in args else None
def main():
    root,uid,port,name,comment,shell_pid,shell_start=guard()
    action=sys.argv[1]
    if action in ['interface-run','interface-cleanup']:
        import runpy
        program=runpy.run_path(SOURCE+'/dev/e2e/veranda-native-network.py')
        program['main'](action,guard(),NATIVE,SSHD,GID,GROUPS,private_read)
        return
    if action=='cleanup' and not any((root/('packet-loss.drop.'+suffix)).exists() for suffix in ['request','done']):
        return  # No DROP attempt; a prerequisite collision is never adopted.
    if action in ['drop','restore'] and private_read(root/('packet-loss.'+action+'.request'),uid)!=('packet-loss-'+action+'-v1\n').encode():
        raise ValueError('invalid privileged packet loss request')
    if action=='prepare' and not pathlib.Path('/usr/sbin/nft').is_file():
        with (root/'packet-loss-apt.log').open('xb') as log:
            os.fchmod(log.fileno(),0o600); os.fchown(log.fileno(),uid,-1)
            for args in [['apt-get','update','-qq'],['apt-get','install','--no-install-recommends','-y','nftables']]:
                subprocess.run(args,stdout=log,stderr=log,check=True,timeout=180,env={'PATH':'/usr/sbin:/usr/bin:/sbin:/bin','DEBIAN_FRONTEND':'noninteractive'})
    # A successful root query proves nf_tables access before any rule mutation.
    present=owned_table(nft('-j','list','tables','ip'),name,comment)
    if action=='prepare':
        if present: raise ValueError('packet loss table collision')
        return
    if action in ['restore','cleanup']:
        count=None
        if present:
            detail=nft('-j','list','table','ip',name)
            if not owned_table(detail,name,comment): raise ValueError('packet loss table ownership changed')
            try:
                if action=='restore': count=drop_count(detail,name,comment)
            finally:
                nft('delete','table','ip',name)
                if owned_table(nft('-j','list','tables','ip'),name,comment): raise ValueError('packet loss cleanup failed')
        elif action=='restore': raise ValueError('packet loss table missing')
        if count is not None:
            with (root/'packet-loss.drops').open('x') as output:
                os.fchmod(output.fileno(),0o600); os.fchown(output.fileno(),uid,-1)
                output.write(str(count)); output.flush(); os.fsync(output.fileno())
            print('veranda-native-packet-loss: dropped='+str(count),flush=True)
        return
    if action!='drop' or present: raise ValueError('packet loss table collision or invalid action')
    identity=json.loads(private_read(root/'listener.identity',uid)); pid=identity['pid']
    if type(pid) is not int or not 1<pid<=2147483647 or not isinstance(identity['start'],str) or not re.fullmatch(r'[0-9]{1,20}',identity['start']): raise ValueError('invalid packet loss listener identity')
    entry=pathlib.Path('/proc',str(pid)); fields=(entry/'stat').read_text().rsplit(')',1)[1].split(); parent=int(fields[1])
    shell=pathlib.Path('/proc',str(shell_pid)); shell_fields=(shell/'stat').read_text().rsplit(')',1)[1].split()
    if shell.stat().st_uid!=uid or shell_fields[19]!=shell_start: raise ValueError('packet loss fixture parent changed')
    if parent!=shell_pid:
        supervisor=pathlib.Path('/proc',str(parent)); parent_fields=(supervisor/'stat').read_text().rsplit(')',1)[1].split()
        if supervisor.stat().st_uid!=uid or int(parent_fields[1])!=shell_pid or str(root/'bin/run-native').encode() not in (supervisor/'cmdline').read_bytes().split(b'\0'):
            raise ValueError('packet loss listener parent changed')
    if entry.stat().st_uid!=uid or fields[0]=='Z' or int(fields[2])!=pid or fields[19]!=identity['start'] or (entry/'exe').resolve()!=pathlib.Path(SSHD).resolve():
        raise ValueError('packet loss listener identity changed')
    sockets=set()
    for fd in (entry/'fd').iterdir():
        try: target=os.readlink(fd)
        except FileNotFoundError: continue
        match=re.fullmatch(r'socket:\[([0-9]+)\]',target)
        if match: sockets.add(match[1])
    if not any(fields[1]==f'0100007F:{port:04X}' and fields[3]=='0A' and fields[9] in sockets
               for fields in [line.split() for line in pathlib.Path('/proc/net/tcp').read_text().splitlines()[1:]]):
        raise ValueError('packet loss port is not owned by listener')
    with socket_connection(port): pass
    # One atomic batch; create rejects collisions instead of adopting a table.
    rules=f'create table ip {name} {{ comment "{comment}"; }}\nadd chain ip {name} output {{ type filter hook output priority 0; }}\n'
    for direction in ['sport','dport']:
        rules+=f'add rule ip {name} output ip saddr 127.0.0.1 ip daddr 127.0.0.1 tcp {direction} {port} counter drop\n'
    nft('-f','-',input=rules.encode())
def socket_connection(port):
    import socket
    return socket.create_connection(('127.0.0.1',port),timeout=1)
try: main()
except BaseException:
    print('veranda-native-packet-loss: failed',file=sys.stderr); raise SystemExit(1)
PYLOSS
}
native_failure_diagnostic() {
  python3 - "$@" <<'PY'
import base64,importlib.util,json,os,pathlib,re,stat,sys
MAX_READ=131072
MAX_LINE=2048
SOURCES={'client_tests.rs','transport_tests.rs','client.rs','transport.rs','connections.rs',
         'sessions.rs','ssh.rs','local_fleet.rs','lib.rs','desktop.rs','main.rs'}
NETWORK_MARKERS={
    'ok: owned SSH byte-flow stall fails bounded reads and autonomously restores pinned subscribed state',
    'ok: owned IPv4 packet loss fails bounded reads and autonomously restores pinned subscribed state'}
INTERFACE_MARKER='ok: owned interface replacement fails bounded reads and autonomously restores pinned subscribed state'
MARKERS=NETWORK_MARKERS | {f'ok: real terminal context {index}' for index in range(3)} | {
    INTERFACE_MARKER,
    'ok: editor fixture public-key authorization and narrow owner forwarding',
    'ok: real VS Code Remote SSH project terminal and window cleanup',
    'ok: separate-process repair preserves old authenticated RPC transports',
    'ok: separate-process repair rejects stale cached RPC, held execute and bound launch',
    'ok: actual SSH key rotation, explicit repair, saved-store reload and autonomous monitor reconnect',
    'ok: owned listener outage preserves authenticated RPC and autonomously restores subscribed state',
    'ok: 100 SSH assess/connect/disconnect handshakes; unchanged store and no provisional pins'}
STAGES={f'native-stage: {stage}' for stage in ['fleet','yard','host','session.prepare','plan','discard','execute','cancel','requery','restore','ssh.churn','terminal.window-wait','terminal.focus','terminal.type','terminal.submit']}
STAGES|={f'native-stage: ssh.interface.{stage}' for stage in ['setup','remove','restore','cleanup','finish']}
STAGES|={f'native-stage: editor.{stage}' for stage in ['accessibility','accessibility-ready','palette-open','palette-ready','choice','create-choice','trust-choice','trust-accepted','focus-choice','terminal-focused','input-sent','marker-wait','deadline','accessibility-missing','palette-missing','choice-missing','trust-missing','terminal-missing','input-missing']}
STAGES|={f'native-stage: editor.{stage}' for stage in ['accessibility-import','accessibility-init','window-binding','window-bound','application-seen','document-ready','accessibility-import-missing','accessibility-init-missing','window-binding-missing','application-missing','document-missing']}
STAGES|={f'native-stage: editor.{stage}' for stage in ['activation-start','activation-ready','activation-missing']}
STAGES|={f'native-stage: editor.{stage}' for stage in ['palette-window-bound','palette-window-focused','palette-key-sent','palette-ambiguous','palette-rpc-exception','palette-binding-failed','palette-focus-failed','palette-key-failed','palette-type-failed','palette-observation-failed']}
CHECKPOINTS={f'native-stage: editor.checkpoint.{name}' for name in ['arguments','owned-uid','owned-executable','owned-start','application-desktop','application-count','application-child','application-pid','application-unique','walk-bounds','walk-state','walk-children','predicate-role','predicate-name','predicate-state','predicate-unique','gesture-focus','gesture-key','gesture-type']}
CHECKPOINTS|={f'native-stage: editor.checkpoint.{name}' for name in ['walk-identity','walk-node-limit','walk-depth-limit','predicate-relations']}
CHECKPOINTS|={f'native-stage: editor.checkpoint.{name}' for name in ['choice-indices','choice-order','choice-adjacent']}
CHECKPOINTS|={f'native-stage: editor.checkpoint.predicate-{point}-{kind}-{state}' for point in ['role','name'] for kind in ['application-gone','ipc','other-glib'] for state in ['defunct','live','recheck-failed']}
EXCEPTIONS={f'native-stage: editor.exception.{name}' for name in ['TimeoutError','TimeoutExpired','CalledProcessError','AmbiguousSelection','GLibError','AssertionError','AttributeError','TypeError','ValueError','OSError','Other']}
REPEATS={f'native-stage: editor.walk-repeat-{name}' for name in ['self','ancestor','duplicate']}
PALETTE_STATES={f'native-stage: editor.palette-state.{name}' for name in ['observe-start','input-scan','input-absent','input-present','direct-focus','relation-scan','owned-listbox-present','owned-listbox-absent','focused-selected-row-absent','active-descendant-ready']}
PALETTE_STATES|={f'native-stage: editor.palette-state.choice-{phase}{outcome}' for phase in ['create','focus'] for outcome in ['', '-target-absent', '-nonadjacent', '-selection-check']}
SSH_DROP=r'drop connection #[0-9]{1,10} from \[[0-9a-fA-F:.]{1,64}\]:[0-9]{1,5} on \[[0-9a-fA-F:.]{1,64}\]:[0-9]{1,5} '
SSH_FAILURES={
    'authorized_keys_unsafe_ancestor':r'Authentication refused: bad ownership or modes for directory [ -~]{1,1024}',
    'account_locked':r'User [A-Za-z0-9_.-]{1,128} not allowed because account is locked(?: \[preauth\])?',
    'authentication_failed':r'Failed publickey for (?:invalid user )?[A-Za-z0-9_.-]{1,128} from [0-9a-fA-F:.]{1,64} port [0-9]{1,5} ssh2(?:: [A-Za-z0-9_@+.-]{1,64} [A-Za-z0-9+/=:]{1,256})?(?: \[preauth\])?',
    'connection_throttled':SSH_DROP+r'Maxstartups|Maxstartups logging rate-limited: additional [0-9]{1,10} connections dropped',
    'source_penalized':SSH_DROP+r'penalty: connections without attempting authentication|PerSourcePenalties logging rate-limited: additional [0-9]{1,10} connections dropped'}
def parse(raw,supervisor=b'',ssh=b''):
    locations=[]; codes=[]; markers=[]; last_stage=None; last_churn=None; ssh_codes=[]
    last_editor_stage=None; editor_deadline=False
    last_editor_checkpoint=None; last_editor_exception=None
    editor_repeats=[]; palette_states=[]
    for line in raw[:MAX_READ].splitlines():
        if len(line)>MAX_LINE: continue
        try: line=line.decode('ascii')
        except UnicodeDecodeError: continue
        if line in MARKERS and line not in markers: markers.append(line)
        if line in CHECKPOINTS: last_editor_checkpoint=line
        if line in EXCEPTIONS: last_editor_exception=line
        if line in REPEATS and line not in editor_repeats: editor_repeats.append(line)
        if line in PALETTE_STATES and line not in palette_states: palette_states.append(line)
        if line in STAGES:
            last_stage=line
            if line=='native-stage: editor.deadline': editor_deadline=True
            elif line.startswith('native-stage: editor.'): last_editor_stage=line
        churn=re.fullmatch(r'native-churn: (100|[1-9][0-9]?) (assessment|connect|completed)',line)
        if churn: last_churn={'iteration':int(churn[1]),'stage':churn[2]}
        location=re.fullmatch(r"thread '[^\r\n]{1,256}'(?: \([0-9]{1,10}\))? panicked at ([A-Za-z0-9_./-]{1,512}):([0-9]{1,8}):([0-9]{1,6}):?",line)
        if location:
            source=pathlib.PurePosixPath(location[1]).name
            if source in SOURCES and int(location[2])>0 and int(location[3])>0 and len(locations)<4:
                locations.append({'source':source,'line':int(location[2]),'column':int(location[3])})
        code=re.match(r'^(?:called `Result::(?:unwrap|expect)\(\)` on an `Err` value: | *(?:left|right): )?NativeError \{ code: "([a-z][a-z0-9_]{0,63})"(?:,| \})',line)
        if code and code[1] not in codes and len(codes)<4: codes.append(code[1])
    for line in ssh[:MAX_READ].splitlines():
        if len(line)>MAX_LINE: continue
        try: line=line.decode('ascii')
        except UnicodeDecodeError: continue
        for code,pattern in SSH_FAILURES.items():
            if code not in ssh_codes and re.fullmatch(pattern,line): ssh_codes.append(code)
    lines=[line for line in supervisor[:MAX_READ].splitlines() if len(line)<=MAX_LINE]
    drops=None
    for line in lines+raw[:MAX_READ].splitlines():
        count=re.fullmatch(rb'veranda-native-packet-loss: dropped=([0-9]{1,20})',line)
        if count and 0<int(count[1])<=18446744073709551615: drops=int(count[1])
    category='native_test_failed'
    if b'veranda-native-supervisor: timeout' in lines: category='native_timeout'
    elif b'Traceback (most recent call last):' in lines: category='supervisor_failure'
    elif locations: category='native_panic'
    return {'category':category,'panicLocations':locations,'nativeErrorCodes':codes,'completedStages':markers,'lastStage':last_stage,'lastEditorStage':last_editor_stage,'lastEditorCheckpoint':last_editor_checkpoint,'lastEditorException':last_editor_exception,'editorTraversalRepeats':editor_repeats,'editorPaletteStates':palette_states,'editorDeadlineReached':editor_deadline,'lastChurn':last_churn,'sshFailureCodes':ssh_codes,'packetLossDrops':drops}
def network_summary(result):
    count=result.get('packetLossDrops')
    if not NETWORK_MARKERS.issubset(result.get('completedStages',[])) or type(count) is not int or not 0<count<=18446744073709551615:
        raise ValueError('incomplete native network evidence')
    return {'schema_version':1,'packet_loss':'passed','byte_flow_stall':'passed','dropped_packets':count}
def private_root(path):
    info=path.lstat()
    if not stat.S_ISDIR(info.st_mode) or stat.S_IMODE(info.st_mode)!=0o700 or info.st_uid!=os.geteuid():
        raise ValueError('unsafe diagnostic root')
def private_read(path,limit=MAX_READ):
    fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK)
    try:
        info=os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode)!=0o600 or info.st_uid!=os.geteuid() or info.st_nlink!=1:
            raise ValueError('unsafe diagnostic file')
        # Panics are at the end; never read more than this fixed tail budget.
        os.lseek(fd,max(0,info.st_size-limit),os.SEEK_SET)
        return os.read(fd,limit)
    finally: os.close(fd)
if sys.argv[1:]==['--self-test']:
    unsafe=b'private-value ssh-ed25519 key-material\nNativeError { code: "bad-code", message: "secret" }\n'
    raw=b"thread 'test' panicked at /private/secret/client_tests.rs:963:28:\n"+unsafe
    raw+=b'NativeError { code: "rpc_unavailable", message: "private-value" }\n'
    raw+=b'private-value NativeError { code: "embedded_payload", message: "secret" }\n'
    raw+=b'NativeError { code: "'+b'x'*65+b'", message: "secret" }\n'
    raw+=b'x'*(MAX_LINE+1)+b'NativeError { code: "hidden", message: "secret" }\n'
    raw+=b"thread 'test' (12345) panicked at src/transport.rs:123:4:\n"
    raw+=b'native-stage: fleet\nnative-stage: yard\nnative-stage: private-value\n'
    raw+=b'ok: real terminal context 0\nok: real terminal context private-value\n'
    result=parse(raw)
    assert result['panicLocations']==[{'source':'client_tests.rs','line':963,'column':28},{'source':'transport.rs','line':123,'column':4}]
    assert result['nativeErrorCodes']==['rpc_unavailable']
    assert result['completedStages']==['ok: real terminal context 0']
    assert result['category']=='native_panic'
    assert result['lastStage']=='native-stage: yard'
    assert not any(value in json.dumps(result) for value in ['private','secret','key-material','ssh-ed25519','bad-code','hidden'])
    assert parse(raw,b'veranda-native-supervisor: timeout\n')['category']=='native_timeout'
    assert parse(raw,b'Traceback (most recent call last):\n')['category']=='supervisor_failure'
    assert parse(b'',b'veranda-native-packet-loss: dropped=12\n')['packetLossDrops']==12
    for count in [b'0',b'-1',b'18446744073709551616',b'1 private-value',b'private-value']:
        assert parse(b'',b'veranda-native-packet-loss: dropped='+count+b'\n')['packetLossDrops'] is None
    network_raw=('\n'.join(sorted(NETWORK_MARKERS))+'\n').encode()
    counter=b'veranda-native-packet-loss: dropped=12\n'
    assert network_summary(parse(network_raw,counter))=={'schema_version':1,'packet_loss':'passed','byte_flow_stall':'passed','dropped_packets':12}
    for raw,supervisor in [(network_raw.replace(marker.encode(),b''),counter) for marker in NETWORK_MARKERS]+[(network_raw,b''),(network_raw,b'veranda-native-packet-loss: dropped=0\n'),(network_raw,b'veranda-native-packet-loss: dropped=malformed\n')]:
        try: network_summary(parse(raw,supervisor))
        except ValueError: pass
        else: raise AssertionError('incomplete native network evidence accepted')
    assert not parse(b'x'*MAX_READ+raw)['panicLocations']
    for header in [
        b"thread 'test' (private-value) panicked at src/client_tests.rs:1:1:",
        b"thread 'test' (-1) panicked at src/client_tests.rs:1:1:",
        b"thread 'test' (12345678901) panicked at src/client_tests.rs:1:1:",
        b"thread 'test' (1) panicked at src/private.rs:1:1:",
        b"thread 'test' (1) panicked at src/client_tests.rs:0:1:",
        b"thread 'test' (1) panicked at src/client_tests.rs:1:0:",
        b"thread 'test' (1) panicked at src/client_tests.rs:123456789:1:",
        b"thread 'test' (1) panicked at src/client_tests.rs:1:1234567:",
        b"private-value thread 'test' (1) panicked at src/client_tests.rs:1:1:",
        b"thread 'test' (1) panicked at src/client_tests.rs:1:1: private-value",
        b"thread 'test' (1) panicked at src/client_tests.rs:1:1:\xff",
        b"thread '"+b'x'*257+b"' (1) panicked at src/client_tests.rs:1:1:",
        b'x'*(MAX_LINE+1)]:
        assert not parse(header)['panicLocations']
    assert parse(b'native-stage: host private-value\n')['lastStage'] is None
    editor=parse(b'native-stage: editor.palette-ready\nnative-stage: editor.deadline\n')
    assert editor['lastEditorStage']=='native-stage: editor.palette-ready' and editor['editorDeadlineReached']
    editor=parse(b'native-stage: editor.palette-ready private-value\nnative-stage: editor.deadline private-value\n')
    assert editor['lastEditorStage'] is None and not editor['editorDeadlineReached']
    for phase in ['activation-start','activation-ready','activation-missing','accessibility-import','accessibility-init','window-binding','window-bound','application-seen','document-ready','accessibility-import-missing','accessibility-init-missing','window-binding-missing','application-missing','document-missing']:
        marker=('native-stage: editor.'+phase).encode()
        assert parse(marker+b'\nnative-stage: editor.deadline')['lastEditorStage']==marker.decode()
        for invalid in [b'private-value '+marker,marker+b' private-value',marker+b'\x00',marker+b'\xff',marker+b'x'*(MAX_LINE+1)]:
            assert parse(invalid)['lastEditorStage'] is None
    for phase in ['choice','palette-window-bound','palette-window-focused','palette-key-sent','palette-ambiguous','palette-rpc-exception','palette-binding-failed','palette-focus-failed','palette-key-failed','palette-type-failed','palette-observation-failed']:
        marker=('native-stage: editor.'+phase).encode()
        assert parse(marker)['lastEditorStage']==marker.decode()
        for invalid in [marker+b' private-value',b'private-value '+marker,marker+b'\x00',marker+b'\xff',marker+b'x'*(MAX_LINE+1)]:
            assert parse(invalid)['lastEditorStage'] is None
    for field,markers in [('lastEditorCheckpoint',CHECKPOINTS),('lastEditorException',EXCEPTIONS)]:
        for value in markers:
            marker=value.encode()
            result=parse(marker+b'\nnative-stage: editor.palette-observation-failed\nnative-stage: editor.deadline')
            assert result[field]==value and result['lastEditorStage']=='native-stage: editor.palette-observation-failed'
            for invalid in [marker+b' private-value',b'private-value '+marker,marker+b'\x00',marker+b'\xff',marker+b'x'*(MAX_LINE+1)]:
                assert parse(invalid)[field] is None
        assert parse(('native-stage: editor.'+('checkpoint.' if field=='lastEditorCheckpoint' else 'exception.')+'private-value').encode())[field] is None
    for marker in REPEATS:
        assert parse((marker+'\n'+marker).encode())['editorTraversalRepeats']==[marker]
        for invalid in [marker+' private-value','private-value '+marker,marker+'\x00',marker+'\u00ff',marker+'x'*(MAX_LINE+1)]:
            assert not parse(invalid.encode())['editorTraversalRepeats']
    assert not parse(b'native-stage: editor.walk-repeat-private-value')['editorTraversalRepeats']
    for marker in PALETTE_STATES:
        raw=(marker+'\n'+marker+'\nnative-stage: editor.deadline').encode()
        assert parse(raw)['editorPaletteStates']==[marker]
        for invalid in [marker+' private-value','private-value '+marker,marker+'\x00',marker+'\u00ff',marker+'x'*(MAX_LINE+1)]:
            assert not parse(invalid.encode())['editorPaletteStates']
    assert not parse(b'native-stage: editor.palette-state.private-value')['editorPaletteStates']
    for marker in MARKERS:
        assert parse(marker.encode())['completedStages']==[marker]
        assert not parse((marker+' private-value').encode())['completedStages']
        assert parse((marker+'\n'+marker).encode())['completedStages']==[marker]
        for invalid in ['private-value '+marker,marker+'\x00',marker+'\u00ff',marker+'x'*(MAX_LINE+1)]:
            assert not parse(invalid.encode())['completedStages']
    for stage in ['terminal.window-wait','terminal.focus','terminal.type','terminal.submit']:
        marker='native-stage: '+stage
        assert parse(marker.encode())['lastStage']==marker
        assert parse((marker+' private-value').encode())['lastStage'] is None
    ssh=b'Authentication refused: bad ownership or modes for directory /private/secret\nUser private-user not allowed because account is locked [preauth]\nFailed publickey for private-user from 127.0.0.1 port 12345 ssh2: ED25519 SHA256:privatekey [preauth]\n'
    result=parse(b'',ssh=ssh+ssh)
    assert result['sshFailureCodes']==['authorized_keys_unsafe_ancestor','account_locked','authentication_failed']
    assert not any(value in json.dumps(result) for value in ['private','secret','127.0.0.1','12345','ED25519','SHA256'])
    for unsafe in [b'private-value '+ssh, b'User private-user not allowed because account is locked private-value',
                   b'Authentication refused: bad ownership or modes for directory /private/secret\xff',
                   b'Authentication refused: bad ownership or modes for directory /private/secret\x00',
                   b'Authentication refused: bad ownership or modes for directory '+b'x'*(MAX_LINE+1)]:
        assert not parse(b'',ssh=unsafe.splitlines()[0])['sshFailureCodes']
    assert not parse(b'',ssh=b'x'*MAX_READ+ssh)['sshFailureCodes']
    drop=b'drop connection #12 from [127.0.0.1]:12345 on [127.0.0.1]:54321 '
    admission=drop+b'Maxstartups\n'+drop+b'penalty: connections without attempting authentication\n'
    summaries=b'Maxstartups logging rate-limited: additional 12 connections dropped\nPerSourcePenalties logging rate-limited: additional 15 connections dropped\n'
    for sample in [admission,summaries]:
        result=parse(b'',ssh=sample)
        assert result['sshFailureCodes']==['connection_throttled','source_penalized']
        assert not any(value in json.dumps(result) for value in ['127.0.0.1','12345','54321','12','15','drop connection'])
    for unsafe in [b'private-value '+admission.splitlines()[0],drop+b'MaxStartups',drop+b'Maxstartups private-value',
                   drop+b'Maxstartups\x00',drop+b'Maxstartups\xff',b'x'*(MAX_LINE+1)+b'Maxstartups']:
        assert not parse(b'',ssh=unsafe)['sshFailureCodes']
    result=parse(b'native-stage: ssh.churn\nnative-churn: 1 assessment\nnative-churn: 1 connect\nnative-churn: 100 completed\n')
    assert result['lastStage']=='native-stage: ssh.churn'
    assert result['lastChurn']=={'iteration':100,'stage':'completed'}
    for unsafe in [b'native-churn: 0 assessment',b'native-churn: 101 connect',b'native-churn: 01 assessment',
                   b'private-value native-churn: 1 connect',b'native-churn: 1 private-value',b'native-churn: 1 completed\x00',
                   b'native-churn: 1 completed\xff',b'x'*(MAX_LINE+1)+b'native-churn: 1 assessment']:
        assert parse(unsafe)['lastChurn'] is None
    assert parse(b'x'*MAX_READ+b'native-churn: 1 connect')['lastChurn'] is None
    assert parse(b'native-stage: editor.palette-ready\nnative-stage: editor.trust-choice\nnative-stage: editor.terminal-missing\n')['lastStage']=='native-stage: editor.terminal-missing'
    for unsafe in [b'native-stage: editor.private-value',b'private-value native-stage: editor.input-sent',
                   b'native-stage: editor.input-sent private-value',b'native-stage: editor.input-sent\x00',
                   b'native-stage: editor.input-sent\xff',b'x'*(MAX_LINE+1)+b'native-stage: editor.input-sent']:
        assert parse(unsafe)['lastStage'] is None
    print('ok: bounded native diagnostics redact unsafe log content')
else:
    phase='unobserved'
    network_mode=sys.argv[1:2]==['--network-summary']
    interface_mode=sys.argv[1:2]==['--interface-summary']
    try:
        owner,native,phase,source=sys.argv[2:] if network_mode or interface_mode else sys.argv[1:]
        if phase not in {'local','remote'}: raise ValueError('invalid diagnostic phase')
        owner=pathlib.Path(owner); native=pathlib.Path(native)
        if not re.fullmatch(r'/var/tmp/subyard-veranda-owner\.[a-zA-Z0-9]+',str(owner)) or native.parent!=owner or not re.fullmatch(r'native\.[a-zA-Z0-9]+',native.name):
            raise ValueError('invalid diagnostic roots')
        private_root(owner); private_root(native)
        expected='subyard-veranda-owner-v1-'+owner.name.rsplit('.',1)[1].lower()+'\n'
        if private_read(owner/'.marker',128)!=expected.encode('ascii') or private_read(native/'.marker',128)!=b'subyard-veranda-native-v1\n':
            raise ValueError('invalid diagnostic markers')
        ssh=private_read(native/'owner-sshd.log') if phase=='remote' else b''
        result=parse(private_read(owner/('native-'+phase+'.log')),private_read(native/(phase+'-supervisor.log')),ssh)
        if interface_mode:
            if phase!='remote' or INTERFACE_MARKER not in result['completedStages']: raise ValueError('interface assertions missing')
            sys.dont_write_bytecode=True
            spec=importlib.util.spec_from_file_location('network',pathlib.Path(source)/'dev/e2e/veranda-native-network.py')
            program=importlib.util.module_from_spec(spec); spec.loader.exec_module(program)
            summary=program.receipt(json.loads(private_read(native/'interface.proof',4096)))
            print('VERANDA_NATIVE_INTERFACE_RESULT '+json.dumps(summary,separators=(',',':')))
            raise SystemExit(0)
        if network_mode:
            if phase!='remote': raise ValueError('network summary requires remote phase')
            print('VERANDA_NATIVE_NETWORK_RESULT '+json.dumps(network_summary(result),separators=(',',':')))
            raise SystemExit(0)
    except (OSError,ValueError):
        if network_mode or interface_mode:
            print('veranda-native network summary: evidence unavailable',file=sys.stderr)
            raise SystemExit(1)
        result={'category':'diagnostic_unavailable'}
    result['phase']=phase if phase in {'local','remote'} else 'unobserved'
    print('veranda-native diagnostic: '+json.dumps(result,separators=(',',':')))
    if result['phase']=='remote' and result['category']!='diagnostic_unavailable':
        try:
            # The supervisor captured only this marked synthetic Code/Xvfb fixture.
            private_root(native/'editor')
            if private_read(native/'editor/.marker',128)!=b'subyard-veranda-editor-v1\n': raise ValueError('invalid editor marker')
            path=native/'editor.failure.png'
            if path.lstat().st_size>512*1024: raise ValueError('oversized image')
            value=private_read(path,512*1024)
            sys.dont_write_bytecode=True
            spec=importlib.util.spec_from_file_location('release',pathlib.Path(source)/'dev/e2e/veranda-release-gui.py')
            release=importlib.util.module_from_spec(spec); spec.loader.exec_module(release)
            if release.valid_failure_png(value): print('veranda-native-screenshot: '+base64.b64encode(value).decode('ascii'))
        except BaseException: pass  # Optional evidence never changes the failed result.
PY
}
if [ "$#" = 1 ] && [ "$1" = --diagnostic-self-test ]; then
  native_failure_diagnostic --self-test
  exit 0
fi
if [ "$#" = 1 ] && [ "$1" = --packet-loss-self-test ]; then
  packet_loss_program | python3 - --self-test
  exit 0
fi
[ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'run only inside an allocated Veranda owner fixture'
[ "$#" = 4 ] || die 'expected native test binary, owner binary, yard name and owner fixture root'
NATIVE="$1" ENGINE="$2" YARD_NAME="$3" OWNER_STATE="$4"
[[ "$YARD_NAME" =~ ^[a-z0-9][a-z0-9_-]{0,127}$ ]] || die 'invalid fixture yard'
[[ "$OWNER_STATE" =~ ^/var/tmp/subyard-veranda-owner\.[a-zA-Z0-9]+$ ]] || die 'invalid owner fixture root'
[ -d "$OWNER_STATE" ] && [ ! -L "$OWNER_STATE" ] || die 'owner fixture root must be plain'
[ "$(stat -c '%a:%u' "$OWNER_STATE")" = "700:$(id -u)" ] || die 'owner fixture root must be private and owned'
[ -f "$OWNER_STATE/.marker" ] && [ ! -L "$OWNER_STATE/.marker" ] || die 'missing owner fixture marker'
token="$(printf '%s' "${OWNER_STATE##*.}" | tr '[:upper:]' '[:lower:]')"
[ "$(cat "$OWNER_STATE/.marker")" = "subyard-veranda-owner-v1-$token" ] || die 'owner fixture marker mismatch'
[ "${SUBYARD_CONFIG_HOME:-}" = "$OWNER_STATE/config" ] && [ "${SUBYARD_HOME:-}" = "$OWNER_STATE/data" ] || die 'owner fixture configuration mismatch'
for command in python3 ssh-keygen ssh-agent ssh-add setsid; do command -v "$command" >/dev/null || die "$command is required"; done
SSHD="$(command -v sshd || true)"
if [ -z "$SSHD" ] && [ -x /usr/sbin/sshd ]; then SSHD=/usr/sbin/sshd; fi
[ -n "$SSHD" ] || die 'the allocated VM needs OpenSSH server'
[ -x "$NATIVE" ] && [ -x "$ENGINE" ] || die 'candidate native test and owner executables are required'
NATIVE="$(realpath "$NATIVE")" ENGINE="$(realpath "$ENGINE")"
FIXTURE="$(mktemp -d "$OWNER_STATE/native.XXXXXX")"
chmod 0700 "$FIXTURE"
printf '%s\n' subyard-veranda-native-v1 > "$FIXTURE/.marker"
chmod 0600 "$FIXTURE/.marker"
agent_pid='' owner_pid='' bad_pid='' xvfb_pid=''
cleanup() {
  local rc=$?
  local packet_clean=1
  trap - EXIT INT TERM ERR
  set +e
  if [ -x "$FIXTURE/bin/packet-loss" ]; then
    sudo -n /usr/bin/python3 "$FIXTURE/bin/packet-loss" interface-cleanup || { packet_clean=0; [ "$rc" -ne 0 ] || rc=1; }
    sudo -n /usr/bin/python3 "$FIXTURE/bin/packet-loss" cleanup || { packet_clean=0; [ "$rc" -ne 0 ] || rc=1; }
  fi
  # Keep the listener reservation and guarded fixture if packet cleanup failed.
  # Disposable allocation teardown remains responsible for the failed guest.
  if [ "$packet_clean" -ne 1 ]; then owner_pid=''; fi
  for pid in "$owner_pid" "$bad_pid" "$agent_pid" "$xvfb_pid"; do
    if [ -n "$pid" ]; then kill "$pid" 2>/dev/null; wait "$pid" 2>/dev/null; fi
  done
  # Remove only this marked, private, synthetic key fixture, even on failure.
  if [ "$packet_clean" = 1 ] && [ -d "$FIXTURE" ] && [ ! -L "$FIXTURE" ] && [ "$(cat "$FIXTURE/.marker" 2>/dev/null)" = subyard-veranda-native-v1 ]; then
    find "$FIXTURE" -depth -delete
  fi
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
install -d -m 0700 "$FIXTURE/bin"
ssh-keygen -q -t ed25519 -N '' -f "$FIXTURE/identity" > /dev/null 2>&1
ssh-keygen -q -t ed25519 -N '' -f "$FIXTURE/host-key" > /dev/null 2>&1
ssh-keygen -q -t ed25519 -N '' -f "$FIXTURE/host-key-next" > /dev/null 2>&1
chmod 0600 "$FIXTURE/identity" "$FIXTURE/identity.pub" "$FIXTURE/host-key" "$FIXTURE/host-key.pub" "$FIXTURE/host-key-next" "$FIXTURE/host-key-next.pub"
cp "$FIXTURE/identity.pub" "$FIXTURE/authorized-keys"
chmod 0600 "$FIXTURE/authorized-keys"
export SSH_AUTH_SOCK="$FIXTURE/agent.sock"
ssh-agent -D -a "$SSH_AUTH_SOCK" > "$FIXTURE/agent.log" 2>&1 &
agent_pid=$!
for _ in {1..100}; do
  [ -S "$SSH_AUTH_SOCK" ] && break
  kill -0 "$agent_pid" 2>/dev/null || die 'isolated SSH agent could not start'
  sleep 0.05
done
[ -S "$SSH_AUTH_SOCK" ] || die 'isolated SSH agent socket did not become ready'
ssh-add "$FIXTURE/identity" > /dev/null 2>&1 || die 'could not load the synthetic agent key'
# The wrapper preserves explicit context selection and fixes the default only
# for this native smoke. No operator configuration or default SSH keys are used.
python3 - "$ENGINE" "$YARD_NAME" "$FIXTURE" "$SSHD" "$(dirname -- "${BASH_SOURCE[0]}")/../.." "$NATIVE" <<'PY'
import json, os, shlex, socket, stat, sys
engine, yard, root, sshd, source, native = sys.argv[1:]
info=os.lstat(root)
if not stat.S_ISDIR(info.st_mode) or stat.S_IMODE(info.st_mode)!=0o700 or info.st_uid!=os.geteuid():
    raise RuntimeError('unsafe private native fixture root')
for name in ['.marker','authorized-keys']:
    info=os.lstat(root+'/'+name)
    if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode)!=0o600 or info.st_uid!=os.geteuid() or info.st_nlink!=1:
        raise RuntimeError('unsafe private native fixture file')
with open(root+'/.marker') as marker:
    if marker.read(128)!='subyard-veranda-native-v1\n': raise RuntimeError('invalid native fixture marker')
lease_path='/run/subyard-e2e-lease.json'; lease_info=os.lstat(lease_path)
if not stat.S_ISREG(lease_info.st_mode) or lease_info.st_uid!=0 or stat.S_IMODE(lease_info.st_mode)!=0o444 or lease_info.st_nlink!=1 or lease_info.st_size>4096:
    raise RuntimeError('unsafe allocated lease marker')
with open(lease_path) as lease_file: lease=json.load(lease_file)
expected={'schema_version':2,**{key:os.environ['SUBYARD_E2E_'+env] for key,env in [('run','RUN_ID'),('slot','SLOT'),('purpose','PURPOSE')]}}
if any(not value for value in expected.values()) or {key:lease.get(key) for key in expected}!=expected:
    raise RuntimeError('allocated lease identity mismatch')
shell_pid=os.getppid(); shell_start=open('/proc/'+str(shell_pid)+'/stat').read().rsplit(')',1)[1].split()[19]
with open(root+'/bin/packet-loss','x') as output:
    root_info=os.lstat(root)
    output.write('IDENTITY='+repr((root_info.st_uid,root_info.st_dev,root_info.st_ino,expected,shell_pid,shell_start))+'\nSSHD='+repr(sshd)+'\n')
    output.write('NATIVE='+repr(native)+'\nSOURCE='+repr(os.path.realpath(source))+'\nGID='+repr(os.getgid())+'\nGROUPS='+repr(os.getgroups())+'\n')
os.chmod(root+'/bin/packet-loss',0o700)
wrapper = '#!/usr/bin/python3\nimport os,sys\nengine=' + repr(engine) + '\nyard=' + repr(yard) + '\nargs=sys.argv[1:]\nif "-Y" not in args: args=["-Y",yard]+args\nos.execv(engine,[engine]+args)\n'
with open(root+'/bin/yard', 'w') as output: output.write(wrapper)
os.chmod(root+'/bin/yard', 0o700)
# sshd resets its child environment. Reintroduce only the known fixture paths.
exports = {name: os.environ[name] for name in ['SUBYARD_CONFIG_HOME','SUBYARD_HOME','SUBYARD_OPERATOR_HOME','STORAGE_PATH']}
exports.update(SUBYARD_NO_AUDIT='1', SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE='1', MIN_DISK_GIB='1')
terminal_enabled = os.environ.get('SUBYARD_E2E_VERANDA_TERMINAL') == '1'
forced = '#!/usr/bin/python3\nimport os,pathlib,shlex,sys\n'
forced += 'engine='+repr(engine)+'\nyard='+repr(yard)+'\nroot='+repr(root)+'\n'
forced += 'os.environ.update('+repr(exports)+')\n'
forced += 'args=shlex.split(os.environ.get("SSH_ORIGINAL_COMMAND",""))\n'
forced += 'rpc=args in [["yard","rpc","--stdio"],["yard","-Y",yard,"rpc","--stdio"]]\n'
forced += 'if rpc: os.execv(engine,[engine,"-Y",yard,"rpc","--stdio"])\n'
if terminal_enabled:
    forced += 'if args == ["bash","-l"]: os.execv("/bin/bash",["bash","-l"])\n'
    forced += 'project=pathlib.Path(root,"terminal-project-id")\n'
    forced += 'allowed=[["yard","-Y",yard,"shell"]]\n'
    forced += 'if project.is_file() and not project.is_symlink():\n value=project.read_text().strip()\n if value and len(value)<=128 and all(c.isascii() and (c.isalnum() or c in "._-") for c in value): allowed.append(["yard","-Y",yard,"shell",value])\n'
    forced += 'if args in allowed: os.execv(engine,[engine]+args[1:])\n'
forced += 'sys.exit(65)\n'
with open(root+'/bin/owner-rpc', 'w') as output: output.write(forced)
with open(root+'/bin/incompatible-rpc', 'w') as output: output.write('#!/usr/bin/env bash\nexit 65\n')
for name in ['owner-rpc','incompatible-rpc']: os.chmod(root+'/bin/'+name, 0o700)
# This script is sent as an explicit python3 argument through the fixture's real
# yard shell. It touches only that disposable developer account and marked root.
guest_script=r'''import hashlib,json,os,pathlib,re,shutil,signal,stat,sys,time
mode,token,key=sys.argv[1:]
if not re.fullmatch(r'[a-zA-Z0-9]{1,32}',token): raise RuntimeError('invalid editor guest token')
if not re.fullmatch(r'ssh-ed25519 [A-Za-z0-9+/=]{32,512}',key): raise RuntimeError('invalid fixture public key')
root=pathlib.Path('/tmp/veranda-editor.'+token); uid=os.geteuid(); auth=pathlib.Path.home()/'.ssh/authorized_keys'
def private(path,directory=False):
    info=path.lstat()
    if (not stat.S_ISDIR(info.st_mode) if directory else not stat.S_ISREG(info.st_mode)) or info.st_uid!=uid or stat.S_IMODE(info.st_mode)!=(0o700 if directory else 0o600):
        raise RuntimeError('unsafe editor guest path')
    if not directory and info.st_nlink!=1: raise RuntimeError('unsafe editor guest link')
def write(path,data):
    descriptor=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
    with os.fdopen(descriptor,'wb') as output:
        os.fchmod(output.fileno(),0o600); output.write(data); output.flush(); os.fsync(output.fileno())
def processes():
    found={}
    for entry in pathlib.Path('/proc').iterdir():
        if not entry.name.isdecimal(): continue
        try:
            if entry.stat().st_uid!=uid: continue
            executable=(entry/'exe').resolve(strict=True)
            if not executable.is_relative_to(root/'server'): continue
            fields=(entry/'stat').read_text().rsplit(')',1)[1].split()
            found[int(entry.name)]=fields[19]
        except (OSError,ValueError,IndexError): pass
    if len(found)>32: raise RuntimeError('editor guest process bound')
    return found
if mode=='setup':
    root.mkdir(mode=0o700); root.chmod(0o700)
    write(root/'.marker',json.dumps({'token':token,'uid':uid,'project':os.getcwd(),'key':hashlib.sha256(key.encode()).hexdigest()}).encode())
    (root/'server').mkdir(mode=0o700); (root/'server').chmod(0o700)
    if not auth.parent.exists(): auth.parent.mkdir(mode=0o700); auth.parent.chmod(0o700)
    private(auth.parent,True)
    existed=auth.exists() or auth.is_symlink()
    if existed: private(auth)
    before=auth.read_bytes() if existed else b''
    if len(before)>65536 or key.encode() in before: raise RuntimeError('unexpected guest authorization')
    write(root/'authorized.before',before); write(root/'authorized.existed',b'1' if existed else b'0')
    after=before+(b'\n' if before and not before.endswith(b'\n') else b'')+key.encode()+b'\n'
    write(root/'authorized.after',after)
    temporary=auth.parent/('.veranda-editor-'+token)
    write(temporary,after)
    if (auth.read_bytes() if existed else b'')!=before: raise RuntimeError('guest authorization baseline changed')
    os.replace(temporary,auth)
    print('editor-guest-setup-v1')
else:
    private(root,True); private(root/'.marker')
    marker=json.loads((root/'.marker').read_text())
    if marker!={'token':token,'uid':uid,'project':os.getcwd(),'key':hashlib.sha256(key.encode()).hexdigest()}:
        raise RuntimeError('editor guest marker mismatch')
    if mode=='ready':
        if not processes(): raise SystemExit(1)
        print('editor-guest-ready-v1')
    elif mode=='pwd':
        try: private(root/'pwd')
        except FileNotFoundError: print(json.dumps({'status':'pending'}))
        else:
            data=(root/'pwd').read_bytes()
            if len(data)>4096: raise RuntimeError('editor terminal marker bound')
            print(json.dumps({'status':'ready','pwd':data.decode('utf-8')}))
    elif mode=='cleanup':
        # Capture only executable paths inside this exact server install root.
        owned=processes()
        for sig in [signal.SIGTERM,signal.SIGKILL]:
            current=processes()
            for pid,start in owned.items():
                if current.get(pid)==start:
                    try: os.kill(pid,sig)
                    except ProcessLookupError: pass
            until=time.monotonic()+3
            while time.monotonic()<until and any(processes().get(pid)==start for pid,start in owned.items()): time.sleep(0.05)
        if any(processes().get(pid)==start for pid,start in owned.items()): raise RuntimeError('editor guest process cleanup failed')
        for name in ['authorized.before','authorized.after','authorized.existed']: private(root/name)
        private(auth)
        if auth.read_bytes()!=(root/'authorized.after').read_bytes(): raise RuntimeError('guest authorization changed after setup')
        if (root/'authorized.existed').read_bytes()==b'1':
            temporary=auth.parent/('.veranda-editor-'+token); write(temporary,(root/'authorized.before').read_bytes()); os.replace(temporary,auth)
        else: auth.unlink()
        shutil.rmtree(root)
        print('editor-guest-cleanup-v1')
    else: raise RuntimeError('invalid editor guest action')
'''
with open(root+'/bin/editor-guest.py','w') as output: output.write(guest_script)
os.chmod(root+'/bin/editor-guest.py',0o700)
# Become a subreaper so a timed-out test cannot leave orphaned RPC descendants.
# Keep only PID/start-time identities descended from this exact test child.
supervisor = r'''#!/usr/bin/python3
import ctypes,importlib.util,json,os,pathlib,re,signal,socket,stat,subprocess,sys,time
sys.dont_write_bytecode=True
if ctypes.CDLL(None).prctl(36,1,0,0,0) != 0: raise SystemExit(1)
def table():
    result={}
    for path in pathlib.Path('/proc').iterdir():
        if not path.name.isdigit(): continue
        try:
            fields=(path/'stat').read_text().rsplit(')',1)[1].split()
            result[int(path.name)]=(int(fields[1]),fields[19])
        except (OSError,ValueError,IndexError): pass
    return result
def descendants(pid, snapshot):
    owned={pid}; changed=True
    while changed:
        changed=False
        for child,(parent,_) in snapshot.items():
            if parent in owned and child not in owned: owned.add(child); changed=True
        if len(owned)>128: raise RuntimeError('native child capacity exceeded')
    return owned
root=pathlib.Path(__file__).resolve().parent.parent
def image_read(path,limit=4096):
    fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK)
    try:
        info=os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode)!=0o600 or info.st_uid!=os.geteuid() or info.st_nlink!=1 or info.st_size>limit:
            raise ValueError('unsafe image fixture file')
        return os.read(fd,limit+1)
    finally: os.close(fd)
def image_module(name,path):
    spec=importlib.util.spec_from_file_location(name,path)
    module=importlib.util.module_from_spec(spec); spec.loader.exec_module(module)
    return module
def editor_failure_image(code_pid,code_start,shell_pid):
    # Only the fixture-created Xvfb and private Code launch can be captured.
    for directory in [root,root/'editor',*[root/'editor'/name for name in ['home','config','cache','runtime']]]:
        info=directory.lstat()
        if not stat.S_ISDIR(info.st_mode) or stat.S_IMODE(info.st_mode)!=0o700 or info.st_uid!=os.geteuid() or directory.resolve()!=directory:
            raise ValueError('unsafe image fixture root')
    if image_read(root/'.marker')!=b'subyard-veranda-native-v1\n' or image_read(root/'editor/.marker')!=b'subyard-veranda-editor-v1\n':
        raise ValueError('invalid image fixture marker')
    if image_read(root/'editor.failure.request')!=b'editor-palette-readiness-v1\n': raise ValueError('invalid image request')
    display=image_read(root/'display')
    if not re.fullmatch(rb'[0-9]{1,5}\n',display) or int(display)>65535: raise ValueError('invalid image display')
    display=':'+display.decode('ascii').strip()
    if os.environ.get('DISPLAY')!=display: raise ValueError('image display changed')
    identity=json.loads(image_read(root/'xvfb.identity')); pid=identity['pid']
    entry=pathlib.Path('/proc',str(pid)); fields=(entry/'stat').read_text().rsplit(')',1)[1].split()
    if entry.stat().st_uid!=os.geteuid() or fields[0]=='Z' or int(fields[1])!=shell_pid or int(fields[2])!=pid or int(fields[19])!=identity['start'] or (entry/'exe').resolve()!=pathlib.Path('/usr/bin/Xvfb').resolve():
        raise ValueError('image server identity changed')
    entry=pathlib.Path('/proc',str(code_pid)); fields=(entry/'stat').read_text().rsplit(')',1)[1].split()
    if entry.stat().st_uid!=os.geteuid() or fields[0]=='Z' or fields[19]!=code_start or (entry/'exe').resolve()!=root/'editor/VSCode-linux-x64/code':
        raise ValueError('image editor identity changed')
    environment=dict(value.split(b'=',1) for value in (entry/'environ').read_bytes().split(b'\0') if b'=' in value)
    for key,name in [('HOME','home'),('XDG_CONFIG_HOME','config'),('XDG_CACHE_HOME','cache'),('XDG_RUNTIME_DIR','runtime')]:
        if environment.get(key.encode())!=os.fsencode(root/'editor'/name): raise ValueError('image editor environment changed')
    if environment.get(b'DISPLAY')!=display.encode(): raise ValueError('image editor display changed')
    measure=image_module('measure',pathlib.Path(SOURCE)/'dev/measure-veranda.py')
    release=image_module('release',pathlib.Path(SOURCE)/'dev/e2e/veranda-release-gui.py')
    path=root/'editor.failure.png'
    fd=os.open(path,os.O_RDWR|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
    try:
        os.fchmod(fd,0o600)
        if not measure.screenshot(display,pathlib.Path('/proc/self/fd',str(fd))): return
        os.lseek(fd,0,os.SEEK_SET); value=os.read(fd,release.MAX_FAILURE_PNG+1)
        if not release.valid_failure_png(value): raise ValueError('invalid image encoding')
        info=os.fstat(fd); current=path.lstat()
        if (info.st_dev,info.st_ino)!=(current.st_dev,current.st_ino) or info.st_nlink!=1: raise ValueError('image file identity changed')
    finally: os.close(fd)
if sys.argv[1:2]==['--editor-failure-image']:
    try: editor_failure_image(int(sys.argv[2]),sys.argv[3],int(sys.argv[4]))
    except BaseException: raise SystemExit(1)  # Optional evidence never exports private exception text.
    raise SystemExit(0)
def private_file(path):
    info=path.lstat()
    if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode)!=0o600 or info.st_uid!=os.geteuid() or info.st_nlink!=1:
        raise RuntimeError('unsafe native fixture file')
def rotate_host_key():
    info=root.lstat()
    if not stat.S_ISDIR(info.st_mode) or stat.S_IMODE(info.st_mode)!=0o700 or info.st_uid!=os.geteuid():
        raise RuntimeError('unsafe native fixture root')
    private_file(root/'.marker')
    if (root/'.marker').read_text()!='subyard-veranda-native-v1\n':
        raise RuntimeError('native fixture marker mismatch')
    request=root/'rotate.request'
    private_file(request)
    if request.read_text()!='rotate-host-key-v1\n':
        raise RuntimeError('invalid native fixture request')
    private_file(root/'listener.identity')
    identity=json.loads((root/'listener.identity').read_text())
    pid=identity['pid']; snapshot=table()
    if pid not in snapshot or snapshot[pid][1]!=identity['start'] or snapshot[pid][0]!=os.getppid():
        raise RuntimeError('native SSH listener identity changed')
    for name in ['host-key','host-key.pub']:
        private_file(root/name); private_file(root/(name.replace('host-key','host-key-next')))
    # Only this marked fixture listener reloads these pre-generated synthetic keys.
    for name in ['host-key','host-key.pub']:
        os.replace(root/(name.replace('host-key','host-key-next')),root/name)
        os.chmod(root/name,0o600)
    os.kill(pid,signal.SIGHUP)
    expected=(root/'host-key.pub').read_text().split()[1]
    private_file(root/'owner.port')
    port=int((root/'owner.port').read_text())
    deadline=time.monotonic()+8
    while time.monotonic()<deadline:
        scan=subprocess.Popen(['ssh-keyscan','-T','1','-t','ed25519','-p',str(port),'127.0.0.1'],stdout=subprocess.PIPE,stderr=subprocess.DEVNULL)
        try: output,_=scan.communicate(timeout=2)
        except subprocess.TimeoutExpired:
            scan.kill(); scan.wait(); output=b''
        if len(output)<=16384 and any(len(parts)==3 and parts[1]=='ssh-ed25519' and parts[2]==expected for parts in (line.split() for line in output.decode('ascii',errors='ignore').splitlines())):
            with open(root/'rotate.done.tmp','x',encoding='ascii') as done:
                done.write('rotate-host-key-v1\n'); done.flush(); os.fsync(done.fileno())
            os.chmod(root/'rotate.done.tmp',0o600)
            os.replace(root/'rotate.done.tmp',root/'rotate.done')
            request.unlink()
            return
        time.sleep(0.1)
    raise RuntimeError('native SSH host-key reload was not observed')
def editor_forwarding(restore=False):
    info=root.lstat()
    if not stat.S_ISDIR(info.st_mode) or stat.S_IMODE(info.st_mode)!=0o700 or info.st_uid!=os.geteuid():
        raise RuntimeError('unsafe editor fixture root')
    private_file(root/'.marker')
    if (root/'.marker').read_text()!='subyard-veranda-native-v1\n' or os.environ.get('VERANDA_TEST_EDITOR')!='1':
        raise RuntimeError('invalid editor fixture')
    request=root/('editor.cleanup.request' if restore else 'editor.prepare.request')
    private_file(request)
    if request.stat().st_size>256: raise RuntimeError('oversized editor request')
    private_file(root/'listener.identity')
    identity=json.loads((root/'listener.identity').read_text()); snapshot=table(); pid=identity['pid']
    if pid not in snapshot or snapshot[pid][1]!=identity['start'] or snapshot[pid][0]!=os.getppid():
        raise RuntimeError('editor listener identity changed')
    config=root/'owner.conf'; original=root/'owner.conf.editor-original'; private_file(config)
    if restore:
        if request.read_text()!='editor-cleanup-v1\n': raise RuntimeError('invalid editor cleanup request')
        private_file(original); text=original.read_text()
    else:
        value=json.loads(request.read_text())
        if set(value)!={'port'} or type(value['port']) is not int or not 1<=value['port']<=65535:
            raise RuntimeError('invalid editor port')
        text=config.read_text()
        if text.count('AllowTcpForwarding no\n')!=1: raise RuntimeError('unexpected editor listener configuration')
        with original.open('x') as output: output.write(text); os.fchmod(output.fileno(),0o600)
        text=text.replace('AllowTcpForwarding no\n','AllowTcpForwarding local\n')
        text+='AllowStreamLocalForwarding no\nPermitOpen 127.0.0.1:'+str(value['port'])+' 127.0.0.1:8765\n'
    temporary=root/'owner.conf.editor-tmp'
    with temporary.open('x') as output:
        output.write(text); output.flush(); os.fchmod(output.fileno(),0o600); os.fsync(output.fileno())
    check=subprocess.run([SSHD,'-t','-f',str(temporary)],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=5)
    if check.returncode: raise RuntimeError('editor listener configuration refused')
    snapshot=table()
    if pid not in snapshot or snapshot[pid][1]!=identity['start']: raise RuntimeError('editor listener identity changed before reload')
    os.replace(temporary,config); os.kill(pid,signal.SIGHUP)
    # Confirm this same listener remains alive after reload before acknowledging.
    time.sleep(0.2); snapshot=table()
    if pid not in snapshot or snapshot[pid][1]!=identity['start']: raise RuntimeError('editor listener reload failed')
    done=root/('editor.cleanup.done' if restore else 'editor.prepare.done')
    temporary=done.with_name(done.name+'.tmp')
    with temporary.open('x') as output:
        output.write('editor-forwarding-v1\n'); output.flush(); os.fchmod(output.fileno(),0o600); os.fsync(output.fileno())
    os.replace(temporary,done)
    request.unlink()
listener_baseline={}
def listener_outage(restore=False):
    global restarted_listener
    info=root.lstat()
    if not stat.S_ISDIR(info.st_mode) or stat.S_IMODE(info.st_mode)!=0o700 or info.st_uid!=os.geteuid() or root.resolve()!=root:
        raise RuntimeError('unsafe listener fixture root')
    def read(name,limit=4096):
        fd=os.open(root/name,os.O_RDONLY|os.O_NOFOLLOW)
        try:
            info=os.fstat(fd)
            if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode)!=0o600 or info.st_uid!=os.geteuid() or info.st_nlink!=1 or info.st_size>limit:
                raise RuntimeError('unsafe listener fixture file')
            value=os.read(fd,limit+1)
            if len(value)>limit: raise RuntimeError('oversized listener fixture file')
            return value
        finally: os.close(fd)
    if read('.marker')!=b'subyard-veranda-native-v1\n': raise RuntimeError('invalid listener fixture marker')
    action='restore' if restore else 'stop'; request='listener.'+action+'.request'
    payload=('listener-'+action+'-v1\n').encode()
    if read(request,128)!=payload: raise RuntimeError('invalid listener control request')
    files={name:read(name,65536 if name=='owner.conf' else 4096) for name in ['owner.conf','owner.port','host-key','host-key.pub']}
    port=files['owner.port']
    if not port.isdigit() or not 1<=int(port)<=65535: raise RuntimeError('invalid listener port')
    port=int(port)
    def listening():
        try:
            with socket.create_connection(('127.0.0.1',port),timeout=0.2): return True
        except OSError: return False
    child=None
    if not restore:
        if not listening(): raise RuntimeError('listener was not accepting connections')
        identity=json.loads(read('listener.identity'))
        if set(identity)!={'pid','start'} or type(identity['pid']) is not int or not 1<identity['pid']<=2147483647 or not isinstance(identity['start'],str) or not 1<=len(identity['start'])<=20 or not identity['start'].isdigit():
            raise RuntimeError('invalid listener identity')
        pid=identity['pid']; entry=pathlib.Path('/proc',str(pid)); fields=(entry/'stat').read_text().rsplit(')',1)[1].split()
        if entry.stat().st_uid!=os.geteuid() or fields[0]=='Z' or int(fields[1])!=os.getppid() or int(fields[2])!=pid or fields[19]!=identity['start'] or (entry/'exe').resolve()!=pathlib.Path(SSHD).resolve():
            raise RuntimeError('listener identity changed')
        listener_baseline.update(files)
        os.kill(pid,signal.SIGTERM)  # Listener only; accepted SSH streams must survive.
        until=time.monotonic()+3
        while listening():
            if time.monotonic()>=until: raise RuntimeError('listener stop deadline')
            time.sleep(0.05)
    else:
        if not listener_baseline or files!=listener_baseline or listening(): raise RuntimeError('listener restore baseline changed')
        check=subprocess.run([SSHD,'-t','-f',str(root/'owner.conf')],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=5)
        if check.returncode: raise RuntimeError('listener restore configuration refused')
        read('owner-sshd.log',131072)
        fd=os.open(root/'owner-sshd.log',os.O_WRONLY|os.O_APPEND|os.O_NOFOLLOW)
        with os.fdopen(fd,'ab') as log:
            child=subprocess.Popen([SSHD,'-D','-e','-f',str(root/'owner.conf')],stdout=log,stderr=log,start_new_session=True)
        restarted_listener=child
        try:
            until=time.monotonic()+5
            while not listening():
                if child.poll() is not None or time.monotonic()>=until: raise RuntimeError('listener restore deadline')
                time.sleep(0.05)
            entry=pathlib.Path('/proc',str(child.pid)); fields=(entry/'stat').read_text().rsplit(')',1)[1].split()
            if child.poll() is not None or entry.stat().st_uid!=os.geteuid() or int(fields[1])!=os.getpid() or int(fields[2])!=child.pid or (entry/'exe').resolve()!=pathlib.Path(SSHD).resolve():
                raise RuntimeError('restored listener identity changed')
            tracked[child.pid]=fields[19]
            with (root/'listener.identity.tmp').open('x') as output:
                os.fchmod(output.fileno(),0o600); json.dump({'pid':child.pid,'start':fields[19]},output); output.flush(); os.fsync(output.fileno())
            read('listener.identity'); os.replace(root/'listener.identity.tmp',root/'listener.identity')
        except BaseException:
            child.kill(); child.wait(); raise
    temporary=root/('listener.'+action+'.done.tmp')
    with temporary.open('x') as output:
        os.fchmod(output.fileno(),0o600); output.write(payload.decode()); output.flush(); os.fsync(output.fileno())
    os.replace(temporary,root/('listener.'+action+'.done')); (root/request).unlink()
    return child
def packet_loss(action):
    request=root/('packet-loss.'+action+'.request'); payload=('packet-loss-'+action+'-v1\n').encode()
    if image_read(request,128)!=payload: raise RuntimeError('invalid packet loss request')
    subprocess.run(['sudo','-n','/usr/bin/python3',str(root/'bin/packet-loss'),action],check=True,timeout=20)
    temporary=root/('packet-loss.'+action+'.done.tmp')
    with temporary.open('x') as output:
        os.fchmod(output.fileno(),0o600); output.write(payload.decode()); output.flush(); os.fsync(output.fileno())
    os.replace(temporary,root/('packet-loss.'+action+'.done')); request.unlink()
tracked={}; result=1; rotated=False; editor_prepared=False; editor_cleaned=False; editor_deadline=None; editor_finished=False
packet_started=False; packet_restored=False; interface_job=None; interface_deadline=None
listener_stopped=False; restarted_listener=None; image_child=None; image_deadline=None
with open(sys.argv[2],'wb') as log:
    os.fchmod(log.fileno(),0o600)
    command=[sys.argv[1],'native_owner_smoke','--ignored','--nocapture']
    environment=dict(os.environ)
    if environment.get('VERANDA_TEST_EDITOR')=='1':
        for name in ['DBUS_SESSION_BUS_ADDRESS','DBUS_STARTER_ADDRESS','DBUS_STARTER_BUS_TYPE',
                     'DCONF_PROFILE','GSETTINGS_BACKEND','GSETTINGS_SCHEMA_DIR','GIO_EXTRA_MODULES','GIO_MODULE_DIR']:
            environment.pop(name,None)
        paths=['HOME','XDG_CONFIG_HOME','XDG_CACHE_HOME','XDG_RUNTIME_DIR']
        restored=[name+'='+environment[name] for name in paths if name in environment]
        unset=[argument for name in paths for argument in ['-u',name]]
        command=['dbus-run-session','--',str(root/'bin/editor-ui.py'),'--activate',str(root),
                 '/usr/bin/env',*unset,*restored]+command
        environment.update({'HOME':str(root/'editor/home'),'XDG_CONFIG_HOME':str(root/'editor/config'),
                            'XDG_CACHE_HOME':str(root/'editor/cache'),'XDG_RUNTIME_DIR':str(root/'editor/runtime')})
    process=subprocess.Popen(command,env=environment,stdout=log,stderr=log,start_new_session=True)
    try:
        deadline=time.monotonic()+int(sys.argv[3])
        while True:
            snapshot=table()
            for pid in descendants(process.pid,snapshot):
                if pid in snapshot: tracked[pid]=snapshot[pid][1]
            if restarted_listener is not None and restarted_listener.pid in snapshot and snapshot[restarted_listener.pid][1]==tracked.get(restarted_listener.pid):
                for pid in descendants(restarted_listener.pid,snapshot):
                    if pid in snapshot: tracked[pid]=snapshot[pid][1]
            if os.environ.get('VERANDA_TEST_EDITOR')=='1':
                # Electron may detach from its --wait CLI. Its verified private
                # fixture executable still binds cleanup to this exact launch.
                editor_executable=root/'editor/VSCode-linux-x64/code'; editor_owned=0; editor_pids=[]
                for pid in snapshot:
                    try:
                        entry=pathlib.Path('/proc',str(pid))
                        if entry.stat().st_uid==os.geteuid() and (entry/'exe').resolve(strict=True)==editor_executable:
                            tracked[pid]=snapshot[pid][1]; editor_owned+=1; editor_pids.append(pid)
                    except OSError: pass
                if editor_owned>32: raise RuntimeError('editor process capacity exceeded')
                if image_child is None and editor_pids and (root/'editor.failure.request').exists():
                    try: ready=image_read(root/'editor.failure.request')==b'editor-palette-readiness-v1\n'
                    except (OSError,ValueError): ready=False
                    if ready:
                        pid=min(editor_pids)
                        image_child=subprocess.Popen([sys.executable,__file__,'--editor-failure-image',str(pid),snapshot[pid][1],str(os.getppid())],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
                        image_deadline=min(deadline,editor_deadline or deadline,time.monotonic()+5)
                if image_child is not None and image_child.poll() is None:
                    for pid in descendants(image_child.pid,snapshot):
                        if pid in snapshot: tracked[pid]=snapshot[pid][1]
                    if time.monotonic()>=image_deadline: image_child.kill()
            result=process.poll()
            if interface_job is not None:
                status=interface_job.poll()
                if status is not None and status!=0:
                    print('veranda-native-interface: failed',flush=True); result=1; break
                if status is None and time.monotonic()>=interface_deadline:
                    print('veranda-native-interface: timeout',flush=True); result=1; break
            if result is not None:
                # Preserve an already-started frame within the original bounds.
                if image_child is not None and image_child.poll() is None and time.monotonic()<image_deadline:
                    time.sleep(0.05); continue
                break
            if time.monotonic()>=deadline:
                print('veranda-native-supervisor: timeout',flush=True)
                result=1; break
            if editor_deadline is None and (root/'editor.start').exists():
                private_file(root/'editor.start')
                if os.environ.get('VERANDA_TEST_EDITOR')!='1' or (root/'editor.start').read_text()!='editor-segment-v1\n':
                    raise RuntimeError('invalid editor segment marker')
                editor_deadline=time.monotonic()+300
            if not editor_finished and (root/'editor.finish').exists():
                private_file(root/'editor.finish')
                if editor_deadline is None or not editor_cleaned or (root/'editor.finish').read_text()!='editor-segment-v1\n':
                    raise RuntimeError('invalid editor completion marker')
                editor_finished=True
            if editor_deadline is not None and time.monotonic()>=editor_deadline and not editor_finished:
                print('veranda-native-supervisor: timeout',flush=True)
                result=1; break
            if not rotated and (root/'rotate.request').exists():
                rotate_host_key(); rotated=True
            if not editor_prepared and (root/'editor.prepare.request').exists():
                editor_forwarding(); editor_prepared=True
            if editor_prepared and not editor_cleaned and (root/'editor.cleanup.request').exists():
                editor_forwarding(True); editor_cleaned=True
            if not listener_stopped and (root/'listener.stop.request').exists():
                listener_outage(); listener_stopped=True
            if listener_stopped and restarted_listener is None and (root/'listener.restore.request').exists():
                restarted_listener=listener_outage(True)
            if not packet_started and (root/'packet-loss.drop.request').exists():
                packet_started=True; packet_loss('drop')
            if packet_started and not packet_restored and (root/'packet-loss.restore.request').exists():
                packet_loss('restore'); packet_restored=True
            if interface_job is None and (root/'interface.run.request').exists():
                if image_read(root/'interface.run.request',128)!=b'interface-run-v1\n': raise RuntimeError('invalid interface segment request')
                interface_job=subprocess.Popen(['sudo','-n','/usr/bin/python3',str(root/'bin/packet-loss'),'interface-run'],stdout=log,stderr=log)
                interface_deadline=min(deadline,time.monotonic()+65)
            time.sleep(0.05)
    finally:
        # Restore packet flow before terminating any process or releasing the port.
        packet_clean=True
        if interface_job is not None:
            try:
                subprocess.run(['sudo','-n','/usr/bin/python3',str(root/'bin/packet-loss'),'interface-cleanup'],check=True,timeout=20)
                interface_job.wait(timeout=2)
            except BaseException:
                packet_clean=False
                print('veranda-native-interface: cleanup-failed',flush=True)
                if result in [None,0]: result=1
        if packet_started:
            try: subprocess.run(['sudo','-n','/usr/bin/python3',str(root/'bin/packet-loss'),'cleanup'],check=True,timeout=20)
            except BaseException:
                packet_clean=False
                print('veranda-native-packet-loss: cleanup-failed',flush=True)
                if result in [None,0]: result=1
        snapshot=table()
        if image_child is not None and image_child.poll() is None and image_child.pid in snapshot:
            tracked[image_child.pid]=snapshot[image_child.pid][1]
        if restarted_listener is not None and restarted_listener.pid in snapshot and snapshot[restarted_listener.pid][1]==tracked.get(restarted_listener.pid):
            for pid in descendants(restarted_listener.pid,snapshot):
                if pid in snapshot: tracked[pid]=snapshot[pid][1]
        # Check identities again before each signal; PIDs may have been reused.
        for sig in [signal.SIGTERM,signal.SIGKILL]:
            snapshot=table()
            for pid,start in tracked.items():
                if not packet_clean and restarted_listener is not None and pid==restarted_listener.pid: continue
                if pid in snapshot and snapshot[pid][1]==start:
                    try:
                        if pathlib.Path('/proc',str(pid)).stat().st_uid==os.geteuid(): os.kill(pid,sig)
                    except (ProcessLookupError,FileNotFoundError): pass
            time.sleep(0.1)
        process.wait()
        if image_child is not None:
            if image_child.poll() is None: image_child.terminate()
            try: image_child.wait(timeout=2)
            except subprocess.TimeoutExpired:
                image_child.kill(); image_child.wait()
        if restarted_listener is not None and packet_clean:
            # Popen retains this exact unreaped child identity; never signal a stale PID.
            if restarted_listener.poll() is None: restarted_listener.terminate()
            try: restarted_listener.wait(timeout=2)
            except subprocess.TimeoutExpired:
                restarted_listener.kill(); restarted_listener.wait()
        while True:
            try:
                if os.waitpid(-1,os.WNOHANG)[0]==0: break
            except ChildProcessError: break
raise SystemExit(result)
'''
supervisor='#!/usr/bin/python3\nSSHD='+repr(sshd)+'\nSOURCE='+repr(os.path.realpath(source))+'\n'+supervisor.split('\n',1)[1]
with open(root+'/bin/run-native','w') as output: output.write(supervisor)
os.chmod(root+'/bin/run-native',0o700)
sockets = []
for name in ['owner','incompatible']:
    sock=socket.socket(); sock.bind(('127.0.0.1',0)); sockets.append(sock)
    port=sock.getsockname()[1]
    with open(root+'/'+name+'.port','w') as output: output.write(str(port))
    os.chmod(root+'/'+name+'.port',0o600)
    forced_command = 'owner-rpc' if name == 'owner' else 'incompatible-rpc'
    def quoted(value): return '"'+value.replace('\\','\\\\').replace('"','\\"')+'"'
    # StrictModes walks world-writable /var/tmp ancestors; this disposable
    # fixture instead checks its own private root/files, ownership and links.
    # The hundred explicit keyscans open three unauthenticated connections each;
    # keep finite admission capacity without penalizing these intentional scans.
    config = '\n'.join(['Port '+str(port), 'ListenAddress 127.0.0.1',
        'HostKey '+quoted(root+'/host-key'), 'PidFile '+quoted(root+'/'+name+'.pid'),
        'AuthorizedKeysFile '+quoted(root+'/authorized-keys'),
        'ForceCommand '+quoted(root+'/bin/'+forced_command),
        'AllowUsers '+os.environ['USER'], 'PubkeyAuthentication yes',
        'AuthenticationMethods publickey', 'PasswordAuthentication no',
        'KbdInteractiveAuthentication no', 'UsePAM no', 'StrictModes no',
        'PermitRootLogin no', 'AllowAgentForwarding no', 'AllowTcpForwarding no',
        'X11Forwarding no', 'PermitTTY '+('yes' if terminal_enabled and name == 'owner' else 'no'), 'PermitUserRC no', 'PermitUserEnvironment no',
        'UseDNS no', 'PrintMotd no', 'PrintLastLog no',
        'LogLevel INFO', 'MaxStartups 16', 'PerSourcePenalties no', 'MaxSessions 4', 'LoginGraceTime 10'])+'\n'
    with open(root+'/'+name+'.conf','w') as output: output.write(config)
    os.chmod(root+'/'+name+'.conf',0o600)
PY
packet_loss_program >> "$FIXTURE/bin/packet-loss"
sudo -n /usr/bin/python3 "$FIXTURE/bin/packet-loss" prepare || die 'allocated VM packet loss prerequisites failed'
"$SSHD" -t -f "$FIXTURE/owner.conf" > "$FIXTURE/sshd-check.log" 2>&1 || die 'isolated SSH server configuration failed'
for name in owner incompatible; do
  install -m 0600 /dev/null "$FIXTURE/$name-sshd.log"
done
setsid "$SSHD" -D -e -f "$FIXTURE/owner.conf" > "$FIXTURE/owner-sshd.log" 2>&1 &
owner_pid=$!
setsid "$SSHD" -D -e -f "$FIXTURE/incompatible.conf" > "$FIXTURE/incompatible-sshd.log" 2>&1 &
bad_pid=$!
python3 - "$FIXTURE" <<'PY' || die 'isolated SSH servers did not become ready'
import pathlib,socket,sys,time
root=pathlib.Path(sys.argv[1]); deadline=time.monotonic()+5
for name in ['owner','incompatible']:
    port=int((root/(name+'.port')).read_text())
    while True:
        try:
            with socket.create_connection(('127.0.0.1',port),timeout=0.2): break
        except OSError:
            if time.monotonic() >= deadline: raise SystemExit(1)
            time.sleep(0.05)
PY
python3 - "$FIXTURE" "$owner_pid" <<'PY'
import json,os,pathlib,sys
root=pathlib.Path(sys.argv[1]); pid=int(sys.argv[2])
fields=pathlib.Path('/proc',str(pid),'stat').read_text().rsplit(')',1)[1].split()
if int(fields[1])!=os.getppid(): raise SystemExit('native listener parent mismatch')
with open(root/'listener.identity','x',encoding='ascii') as output:
    json.dump({'pid':pid,'start':fields[19]},output)
os.chmod(root/'listener.identity',0o600)
PY
unset VERANDA_TEST_TERMINAL VERANDA_TEST_TERMINAL_CONTROL_ROOT VERANDA_TEST_EDITOR VERANDA_TEST_EDITOR_CONTROL_ROOT
if [ "${SUBYARD_E2E_VERANDA_TERMINAL:-0}" = 1 ] || [ "${SUBYARD_E2E_VERANDA_EDITOR:-0}" = 1 ]; then
  sudo -n test -f /run/subyard-e2e-lease.json || die 'terminal tools require an allocated VM'
  # Logs belong to the unprivileged fixture owner.
  # shellcheck disable=SC2024
  sudo -n apt-get update -qq > "$FIXTURE/terminal-apt.log" 2>&1
  # shellcheck disable=SC2024
  sudo -n apt-get install -y xvfb xterm xdotool >> "$FIXTURE/terminal-apt.log" 2>&1
  if [ "${SUBYARD_E2E_VERANDA_EDITOR:-0}" = 1 ]; then
    # shellcheck disable=SC2024
    sudo -n apt-get install -y libnss3 libgbm1 libasound2t64 libatk-bridge2.0-0 libgtk-3-0 libcups2 dbus python3-gi gir1.2-atspi-2.0 at-spi2-core >> "$FIXTURE/terminal-apt.log" 2>&1
  fi
  install -m 0600 /dev/null "$FIXTURE/display"
  install -m 0600 /dev/null "$FIXTURE/xvfb.log"
  setsid Xvfb -displayfd 3 -screen 0 1280x800x24 -nolisten tcp 3> "$FIXTURE/display" > "$FIXTURE/xvfb.log" 2>&1 &
  xvfb_pid=$!
  for _ in {1..100}; do
    [ -s "$FIXTURE/display" ] && break
    kill -0 "$xvfb_pid" || die 'terminal Xvfb exited'
    sleep 0.05
  done
  [ -s "$FIXTURE/display" ] || die 'terminal Xvfb readiness timeout'
  python3 - "$FIXTURE" "$xvfb_pid" <<'PY'
import json,os,pathlib,re,stat,sys
root=pathlib.Path(sys.argv[1]); pid=int(sys.argv[2]); uid=os.geteuid()
info=root.lstat()
if not stat.S_ISDIR(info.st_mode) or stat.S_IMODE(info.st_mode)!=0o700 or info.st_uid!=uid or root.resolve()!=root:
    raise SystemExit('invalid Xvfb fixture root')
def private_read(path):
    fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW)
    try:
        info=os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode)!=0o600 or info.st_uid!=uid or info.st_nlink!=1 or info.st_size>4096:
            raise SystemExit('invalid Xvfb fixture file')
        return os.read(fd,4097)
    finally: os.close(fd)
if private_read(root/'.marker')!=b'subyard-veranda-native-v1\n': raise SystemExit('invalid Xvfb fixture marker')
display=private_read(root/'display')
if not re.fullmatch(rb'[0-9]{1,5}\n',display) or int(display)>65535: raise SystemExit('invalid Xvfb fixture display')
private_read(root/'xvfb.log')
process=pathlib.Path('/proc',str(pid)); fields=(process/'stat').read_text().rsplit(')',1)[1].split()
if process.stat().st_uid!=uid or fields[0]=='Z' or int(fields[1])!=os.getppid() or int(fields[2])!=pid or (process/'exe').resolve()!=pathlib.Path('/usr/bin/Xvfb').resolve():
    raise SystemExit('invalid Xvfb fixture process')
fd=os.open(root/'xvfb.identity',os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
with os.fdopen(fd,'w',encoding='ascii') as output:
    os.fchmod(output.fileno(),0o600)
    json.dump({'pid':pid,'start':int(fields[19])},output)
PY
  DISPLAY=":$(cat "$FIXTURE/display")"
  export DISPLAY
  if [ "${SUBYARD_E2E_VERANDA_TERMINAL:-0}" = 1 ]; then
    export VERANDA_TEST_TERMINAL=1 VERANDA_TEST_TERMINAL_CONTROL_ROOT="$FIXTURE"
  fi
fi
if [ "${SUBYARD_E2E_VERANDA_EDITOR:-0}" = 1 ]; then
  install -m 0700 "$(dirname -- "${BASH_SOURCE[0]}")/veranda-editor-ui.py" "$FIXTURE/bin/editor-ui.py"
  # Preparation runs only in the marked allocated VM. Downloads and CLI output
  # remain private; no default HOME, existing Code profile or personal key is read.
  if ! timeout --signal=TERM --kill-after=5s 180s python3 - "$FIXTURE" > "$FIXTURE/editor-tools.log" 2>&1 <<'PY'
import gzip,hashlib,json,os,pathlib,posixpath,subprocess,sys,tarfile,time,urllib.request
root=pathlib.Path(sys.argv[1]); editor=root/'editor'; editor.mkdir(mode=0o700); editor.chmod(0o700)
(editor/'.marker').write_text('subyard-veranda-editor-v1\n'); (editor/'.marker').chmod(0o600)
for name in ['downloads','home','extensions','tool-userdata','config','cache','runtime']:
    (editor/name).mkdir(mode=0o700); (editor/name).chmod(0o700)
downloads=[
 ('code.tar.gz','https://vscode.download.prss.microsoft.com/dbazure/download/stable/07f806f999227108933c2e30515b26eecc1fda74/code-stable-x64-1790759436.tar.gz','d32031e9e213d59532af3cf32fcb8b357a1cdd10417967b4f5b5ba30436dc0dc',500*1024*1024,False),
 ('remote-ssh-edit.vsix','https://marketplace.visualstudio.com/_apis/public/gallery/publishers/ms-vscode-remote/vsextensions/remote-ssh-edit/0.87.0/vspackage','c9e5fa440265d3b77e4ae63219014b64d714cd5280b263c5c932849fecb71ae3',32*1024*1024,True),
 ('remote-explorer.vsix','https://marketplace.visualstudio.com/_apis/public/gallery/publishers/ms-vscode/vsextensions/remote-explorer/0.5.0/vspackage','04db277ada5d771bf763d323644454e633aad48da0e81345175ac3ec08299abf',32*1024*1024,True),
 ('remote-ssh.vsix','https://marketplace.visualstudio.com/_apis/public/gallery/publishers/ms-vscode-remote/vsextensions/remote-ssh/0.128.0/vspackage','458bfdea562840a3a4d9cb246cfe9a212c67a0c9317a8fdd0d22aeaedc82bf2c',32*1024*1024,True)]
deadline=time.monotonic()+180
for name,url,expected,limit,is_extension in downloads:
    path=editor/'downloads'/name
    with urllib.request.urlopen(url,timeout=30) as response, path.open('xb') as output:
        count=0
        while True:
            chunk=response.read(512*1024)
            if not chunk: break
            count+=len(chunk)
            if count>limit or time.monotonic()>deadline: raise RuntimeError('editor download bound')
            output.write(chunk)
    path.chmod(0o600)
    if is_extension and path.read_bytes()[:2]==b'\x1f\x8b':
        with gzip.open(path,'rb') as compressed: data=compressed.read(limit+1)
        if len(data)>limit: raise RuntimeError('editor extension bound')
        path.write_bytes(data)
    digest=hashlib.sha256()
    with path.open('rb') as source:
        while chunk:=source.read(512*1024): digest.update(chunk)
    if digest.hexdigest()!=expected: raise RuntimeError('official editor digest mismatch')
with tarfile.open(editor/'downloads/code.tar.gz') as archive:
    members=archive.getmembers()
    if len(members)>100000 or sum(item.size for item in members)>2*1024**3: raise RuntimeError('editor archive bound')
    for item in members:
        path=pathlib.PurePosixPath(item.name)
        if path.is_absolute() or '..' in path.parts or not path.parts or path.parts[0]!='VSCode-linux-x64': raise RuntimeError('unsafe editor archive')
        if not (item.isfile() or item.isdir() or item.issym() or item.islnk()): raise RuntimeError('unsafe editor archive type')
        if item.issym() and not posixpath.normpath(posixpath.join(str(path.parent),item.linkname)).startswith('VSCode-linux-x64/'):
            raise RuntimeError('unsafe editor archive link')
    archive.extractall(editor,filter='data')
environment={'PATH':'/usr/local/bin:/usr/bin:/bin','LANG':'C.UTF-8','HOME':str(editor/'home'),
             'XDG_CONFIG_HOME':str(editor/'config'),'XDG_CACHE_HOME':str(editor/'cache'),'XDG_RUNTIME_DIR':str(editor/'runtime')}
code=str(editor/'VSCode-linux-x64/bin/code')
args=[code,'--user-data-dir',str(editor/'tool-userdata'),'--extensions-dir',str(editor/'extensions')]
version=subprocess.check_output(args+['--version'],env=environment,timeout=30).decode().splitlines()
if version!=['1.140.0','07f806f999227108933c2e30515b26eecc1fda74','x64']: raise RuntimeError('editor identity mismatch')
for name,_,_,_,is_extension in downloads:
    if is_extension:
        subprocess.run(args+['--install-extension',str(editor/'downloads'/name)],env=environment,check=True,timeout=60)
installed=subprocess.check_output(args+['--list-extensions','--show-versions'],env=environment,timeout=30).decode().splitlines()
if set(installed)!={'ms-vscode-remote.remote-ssh@0.128.0','ms-vscode-remote.remote-ssh-edit@0.87.0','ms-vscode.remote-explorer@0.5.0'}:
    raise RuntimeError('editor extension identities mismatch')
# Keep Electron's normal sandbox. A missing VM prerequisite is an explicit failure.
subprocess.run(['unshare','--user','--map-root-user','/usr/bin/true'],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=5)
wrapper='#!/usr/bin/python3\nimport os,sys\nroot='+repr(str(root))+'\neditor='+repr(str(editor))+'\n'
wrapper+="env={'PATH':'/usr/local/bin:/usr/bin:/bin','LANG':'C.UTF-8','HOME':editor+'/home','XDG_CONFIG_HOME':editor+'/config','XDG_CACHE_HOME':editor+'/cache','XDG_RUNTIME_DIR':editor+'/runtime','DISPLAY':os.environ['DISPLAY'],'SSH_AUTH_SOCK':root+'/agent.sock','DBUS_SESSION_BUS_ADDRESS':os.environ['DBUS_SESSION_BUS_ADDRESS']}\n"
wrapper+='os.execve('+repr(code)+',['+repr(code)+',"--force-renderer-accessibility","--extensions-dir",editor+"/extensions"]+sys.argv[1:],env)\n'
(root/'bin/code').write_text(wrapper); (root/'bin/code').chmod(0o700)
print('ok: pinned official editor tools and normal sandbox prerequisites')
PY
  then
    die 'editor tools unavailable: verify official download access, Code runtime libraries and unprivileged user namespaces in the allocated VM'
  fi
  printf 'veranda-native: pinned official VS Code and Remote SSH tools prepared\n'
fi
export PATH="$FIXTURE/bin:$PATH" VERANDA_TEST_DISPOSABLE_OWNER=1
unset VERANDA_TEST_SSH_DESTINATION VERANDA_TEST_INCOMPATIBLE_DESTINATION VERANDA_TEST_NATIVE_CONTROL_ROOT
# Interface replacement adds 45 seconds plus bounded setup/cleanup (65 total).
local_timeout=150 remote_timeout=350
if [ "${SUBYARD_E2E_VERANDA_TERMINAL:-0}" = 1 ]; then local_timeout=240; remote_timeout=440; fi
if [ "${SUBYARD_E2E_VERANDA_EDITOR:-0}" = 1 ]; then remote_timeout=$((remote_timeout + 300)); fi
printf 'veranda-native: local native client against the disposable owner\n'
install -m 0600 /dev/null "$FIXTURE/local-supervisor.log"
if ! "$FIXTURE/bin/run-native" "$NATIVE" "$OWNER_STATE/native-local.log" "$local_timeout" > "$FIXTURE/local-supervisor.log" 2>&1; then
  native_failure_diagnostic "$OWNER_STATE" "$FIXTURE" local "$(dirname -- "${BASH_SOURCE[0]}")/../.."
  die 'local native owner smoke failed; bounded diagnostic emitted'
fi
VERANDA_TEST_SSH_DESTINATION="$(id -un)@127.0.0.1:$(cat "$FIXTURE/owner.port")"
VERANDA_TEST_INCOMPATIBLE_DESTINATION="$(id -un)@127.0.0.1:$(cat "$FIXTURE/incompatible.port")"
export VERANDA_TEST_SSH_DESTINATION VERANDA_TEST_INCOMPATIBLE_DESTINATION VERANDA_TEST_NATIVE_CONTROL_ROOT="$FIXTURE"
if [ "${SUBYARD_E2E_VERANDA_EDITOR:-0}" = 1 ]; then
  export VERANDA_TEST_EDITOR=1 VERANDA_TEST_EDITOR_CONTROL_ROOT="$FIXTURE"
fi
printf 'veranda-native: remote native client with isolated SSH agent and public pins\n'
install -m 0600 /dev/null "$FIXTURE/remote-supervisor.log"
if ! "$FIXTURE/bin/run-native" "$NATIVE" "$OWNER_STATE/native-remote.log" "$remote_timeout" > "$FIXTURE/remote-supervisor.log" 2>&1; then
  native_failure_diagnostic "$OWNER_STATE" "$FIXTURE" remote "$(dirname -- "${BASH_SOURCE[0]}")/../.."
  die 'remote native owner smoke failed; bounded diagnostic emitted'
fi
native_failure_diagnostic --network-summary "$OWNER_STATE" "$FIXTURE" remote "$(dirname -- "${BASH_SOURCE[0]}")/../.." \
  || die 'native network acceptance lacks complete bounded evidence'
native_failure_diagnostic --interface-summary "$OWNER_STATE" "$FIXTURE" remote "$(dirname -- "${BASH_SOURCE[0]}")/../.." \
  || die 'native interface acceptance lacks complete bounded evidence'
printf 'ok: local and pinned SSH native owner acceptance\n'
