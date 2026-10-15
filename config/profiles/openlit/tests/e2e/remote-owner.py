#!/usr/bin/env python3
"""Real owner RPC and exact dashboard SSH instructions from an isolated synthetic client."""
import json
import os
from pathlib import Path
import re
import shlex
import signal
import socket
import subprocess
import sys
import time
import urllib.parse
import urllib.request


def run(*args, **options):
    options.setdefault('timeout',30)
    return subprocess.run(args, check=True, **options)


def dashboard_instruction(status):
    row = next((re.search(r'openlit\s+dashboard\s+(\S+)(.*)', line)
                for line in status.splitlines() if re.search(r'openlit\s+dashboard\s+', line)), None)
    state = row[1] if row and row[1] in ('up', 'down') else 'UNKNOWN'
    match = re.search(r'\(open (http://127\.0\.0\.1:[0-9]+/telemetry\?tab=logs) after: (.*)\)$', row[0]) if row else None
    observation = {'row_present': row is not None, 'state': state, 'instruction_present': match is not None}
    check = re.search(r'\(profile check reason=(deadline|cancelled|native-failure|unavailable) native_exit=(none|[1-9][0-9]{0,2}) elapsed_ms=([0-9]{1,7}) budget_ms=(5000|30000)\)\)$', row[0]) if row and state == 'down' else None
    if check:
        category, native, elapsed, budget = check.groups()
        native = None if native == 'none' else int(native)
        if int(elapsed) <= 1320000 and (native is None and category != 'native-failure' or native is not None and native <= 255 and category == 'native-failure'):
            observation['profile_check'] = {'category': category, 'native_exit': native, 'elapsed_ms': int(elapsed), 'budget_ms': int(budget)}
    print('openlit_e2e_remote_dashboard='+json.dumps(observation), flush=True)
    if state != 'up':
        raise RuntimeError('remote OpenLIT dashboard is not up: '+state)
    if not match:
        raise RuntimeError('remote status omitted the exact native dashboard tunnel instruction')
    url, command = match.groups()
    parsed = urllib.parse.urlsplit(url)
    return parsed.scheme+'://'+parsed.netloc+'/', shlex.split(command)


def client(root, engine, identity):
    root = Path(root)
    environment = dict(os.environ, SUBYARD_CONFIG_HOME=str(root/'client-config'),
                       SUBYARD_HOME=str(root/'client-data'), PATH=str(root/'bin')+':'+os.environ['PATH'])
    status = subprocess.check_output([engine, '-Y', 'remote', 'status'], env=environment, timeout=60, text=True)
    (root/'client-status').write_text(status)
    origin, arguments = dashboard_instruction(status)
    if arguments[:2] not in (['ssh', '-N'], ['ssh', '-T']):
        raise RuntimeError('unexpected remote dashboard transport')
    tunnel = subprocess.Popen(arguments, env=environment, start_new_session=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        for _ in range(80):
            if tunnel.poll() is not None:
                raise RuntimeError('rendered SSH tunnel exited')
            try:
                urllib.request.urlopen(origin+'api/auth/csrf', timeout=1).close()
                break
            except OSError:
                time.sleep(0.25)
        run(sys.executable, str(Path(__file__).with_name('ui-origin.py')), '--origin', origin,
            '--state', str(root/'client-cookies'), '--identity', identity+'-remote', timeout=60)
        print('ok: actual remote owner RPC and rendered dashboard SSH tunnel')
    finally:
        if tunnel.poll() is None:
            os.killpg(tunnel.pid, signal.SIGTERM)
        tunnel.wait(timeout=15)


def main(state, engine, yard, port, identity):
    root = Path(state)/'remote-owner'
    if (Path(state)/'.marker').read_text().strip() != 'openlit-profile-e2e-v1':
        raise RuntimeError('remote owner fixture requires its marked parent')
    root.mkdir(mode=0o700)
    (root/'.marker').write_text('openlit-remote-owner-v1')
    (root/'bin').mkdir(mode=0o700)
    (root/'client-config/yards/remote').mkdir(parents=True, mode=0o700)
    owner_pid = os.getpid()
    with socket.socket() as reserve:
        reserve.bind(('127.0.0.1', 0))
        ssh_port = reserve.getsockname()[1]
    for name in ('host-key', 'client-key'):
        run('ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', str(root/name))
    (root/'authorized_keys').write_bytes((root/'client-key.pub').read_bytes())
    os.chmod(root/'authorized_keys', 0o600)
    owner_environment = {name: os.environ[name] for name in ('SUBYARD_OPERATOR_HOME', 'SUBYARD_CONFIG_HOME', 'SUBYARD_HOME', 'STORAGE_PATH')}
    owner_environment.update(SUBYARD_NO_AUDIT='1', SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE='1', MIN_DISK_GIB='1')
    guest_alias = 'yard-'+yard
    nested = ['ssh', '-N', '-o', 'ExitOnForwardFailure=yes', '-L', port+':127.0.0.1:3000', guest_alias]
    program = '#!/usr/bin/python3\nimport os,pathlib,shlex,sys\n'
    program += 'ENGINE='+repr(engine)+'\nYARD='+repr(yard)+'\nENVIRONMENT='+repr(owner_environment)+'\nNESTED='+repr(nested)+'\nPID='+repr(str(root/'nested.pid'))+'\n'
    program += '''
request=shlex.split(os.environ.get('SSH_ORIGINAL_COMMAND',''))
if len(request)==3 and request[:2]==['bash','-lc']:
    request=shlex.split(request[2])
    if request[:1]==['exec']: request.pop(0)
if request==NESTED:
    pathlib.Path(PID).write_text(str(os.getpid()))
    os.execvp('/usr/bin/ssh',request)
if request not in (['yard','rpc','--stdio'],['yard','-Y',YARD,'rpc','--stdio']):
    sys.exit('openlit remote fixture: unsupported owner command')
environment=os.environ.copy()
for name in ('SUBYARD_ENGINE_CONTEXT','SUBYARD_CONFIG_LOADED','AGENTS','CODING_TOOL_INTEGRATIONS'):
    environment.pop(name,None)
environment.update(ENVIRONMENT)
os.execve(ENGINE,[ENGINE,'-Y',YARD,'rpc','--stdio'],environment)
'''
    (root/'owner-rpc').write_text(program)
    os.chmod(root/'owner-rpc', 0o700)
    config = f'''Port {ssh_port}
ListenAddress 127.0.0.1
HostKey {root}/host-key
PidFile {root}/sshd.pid
AuthorizedKeysFile {root}/authorized_keys
StrictModes no
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
AllowUsers dev
AllowTcpForwarding local
PermitOpen 127.0.0.1:{port}
X11Forwarding no
PermitTTY no
ForceCommand {root}/owner-rpc
LogLevel ERROR
'''
    (root/'sshd_config').write_text(config)
    run('sudo', '-n', '/usr/sbin/sshd', '-t', '-f', str(root/'sshd_config'))
    (root/'known_hosts').write_text('openlit-owner '+(root/'host-key.pub').read_text())
    # ProxyCommand runs only the TCP stream relay in the captured owner namespace.
    # SSH and its local browser port remain in the isolated client namespace.
    (root/'relay.py').write_text('''import os,selectors,socket,sys
sock=socket.create_connection(('127.0.0.1',int(sys.argv[1])),timeout=5)
sock.settimeout(None)
selector=selectors.DefaultSelector()
selector.register(0,selectors.EVENT_READ)
selector.register(sock,selectors.EVENT_READ)
while selector.get_map():
    for key,_ in selector.select():
        if key.fileobj==0:
            data=os.read(0,65536)
            if not data: selector.unregister(0); sock.shutdown(socket.SHUT_WR)
            else: sock.sendall(data)
        else:
            data=sock.recv(65536)
            if not data: sys.exit(0)
            sys.stdout.buffer.write(data)
            sys.stdout.buffer.flush()
''')
    proxy = shlex.join(['sudo','-n','nsenter','--net=/proc/'+str(owner_pid)+'/ns/net','--','/usr/bin/python3',str(root/'relay.py'),str(ssh_port)])
    (root/'ssh_config').write_text(f'''Host openlit-owner
  HostName openlit-owner
  User dev
  IdentityFile {root}/client-key
  IdentitiesOnly yes
  HostKeyAlias openlit-owner
  UserKnownHostsFile {root}/known_hosts
  StrictHostKeyChecking yes
  ProxyCommand {proxy}
  ConnectTimeout 5
  LogLevel ERROR
''')
    (root/'bin/ssh').write_text('#!/bin/sh\nexec /usr/bin/ssh -F '+shlex.quote(str(root/'ssh_config'))+' "$@"\n')
    os.chmod(root/'bin/ssh', 0o700)
    (root/'client-config/yards/remote/config.env').write_text('ACCESS_KIND=remote\nOWNER_ENDPOINT=openlit-owner\nOWNER_YARD_NAME='+yard+'\nSSH_HOST='+guest_alias+'\n')
    os.chmod(root/'client-config/yards/remote/config.env', 0o600)
    log = open(root/'sshd.log', 'wb')
    daemon = subprocess.Popen(['sudo','-n','/usr/sbin/sshd','-D','-e','-f',str(root/'sshd_config')],stdout=log,stderr=log)
    try:
        for _ in range(50):
            if daemon.poll() is not None: raise RuntimeError('synthetic owner sshd exited')
            if (root/'sshd.pid').exists(): break
            time.sleep(0.1)
        run('sudo','-n','unshare','--net','--fork','bash','-euc',
            'ip link set lo up; exec runuser -u dev -- python3 "$@"', 'subyard-client',
            str(Path(__file__).resolve()), '--client', str(root), engine, identity, timeout=180)
    finally:
        if (root/'nested.pid').exists():
            pid = int((root/'nested.pid').read_text())
            try:
                command=Path('/proc/'+str(pid)+'/cmdline').read_bytes().split(b'\0')
                if port.encode()+b':127.0.0.1:3000' in command and guest_alias.encode() in command:
                    os.kill(pid, signal.SIGTERM)
            except ProcessLookupError: pass
            except FileNotFoundError: pass
        if (root/'sshd.pid').exists():
            pid = int((root/'sshd.pid').read_text())
            command=Path('/proc/'+str(pid)+'/cmdline').read_bytes()
            if str(root/'sshd_config').encode() not in command:
                raise RuntimeError('synthetic owner daemon ownership changed')
            run('sudo','-n','kill','-TERM',str(pid))
        daemon.wait(timeout=15)
        log.close()


if __name__=='__main__':
    if sys.argv[1:2]==['--client']: client(*sys.argv[2:])
    else: main(*sys.argv[1:])
