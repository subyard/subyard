#!/usr/bin/env python3
"""Native Git HTTP/SSH fixtures; no external host, credentials or managed repo."""
import base64
import http.server
import importlib.util
import json
import os
from pathlib import Path
import shlex
import signal
import socket
import socketserver
import ssl
import subprocess
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[4]
spec = importlib.util.spec_from_file_location('wiring', Path(__file__).parents[1]/'git_wiring.py')
wiring = importlib.util.module_from_spec(spec)
spec.loader.exec_module(wiring)


class NativeGitTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory(prefix='subyard-github-native-', dir='/tmp')
        cls.base = Path(cls.temp.name)
        cls.engine = cls.base/'client'
        subprocess.run(['go', 'build', '-o', str(cls.engine), './config/profiles/github/cmd/broker'], cwd=ROOT, check=True)

    @classmethod
    def tearDownClass(cls):
        cls.temp.cleanup()

    def setUp(self):
        self.work = self.base/str(len(list(self.base.iterdir())))
        self.home = self.work/'home'
        self.bin = self.work/'bin'
        self.bin.mkdir(parents=True)
        self.home.mkdir()
        self.env = {key: value for key, value in os.environ.items() if not key.startswith(('GIT_', 'GH_', 'GITHUB_', 'SUBYARD_GITHUB_'))}
        self.env.update(HOME=str(self.home), XDG_CONFIG_HOME=str(self.home/'.config'), GIT_CONFIG_NOSYSTEM='1',
                        PATH=str(self.bin)+':'+os.environ['PATH'], GIT_TERMINAL_PROMPT='0',
                        GIT_AUTHOR_NAME='Fixture', GIT_AUTHOR_EMAIL='fixture@example.invalid',
                        GIT_COMMITTER_NAME='Fixture', GIT_COMMITTER_EMAIL='fixture@example.invalid')
        self.helper = self.bin/'git-remote-subyard-github'
        self.helper.write_text('#!/bin/sh\nexec '+shlex.quote(str(self.engine))+' _github-git-remote "$@"\n')
        self.helper.chmod(0o755)
        wiring.reconcile(self.home, 'broker-first', 'apply')
        self.remotes = self.work/'repos'
        self.remote = self.remotes/'owner/repo.git'
        self.remote.parent.mkdir(parents=True)
        self.git('init', '--bare', '--initial-branch=main', self.remote)
        self.seed = self.work/'seed'
        self.git('init', '--initial-branch=main', self.seed)
        (self.seed/'file').write_text('one\n')
        self.git('add', 'file', cwd=self.seed)
        self.git('commit', '-qm', 'Initial fixture', cwd=self.seed)
        self.git('push', self.remote, 'main', cwd=self.seed)
        self.sshlog = self.work/'ssh.log'
        ssh = self.bin/'fixture-ssh'
        ssh.write_text('#!/usr/bin/env python3\nimport os,shlex,subprocess,sys\n'
                       'args=shlex.split(sys.argv[-1])\n'
                       'assert args[0] in ("git-upload-pack","git-receive-pack") and args[1].lstrip("/")=="owner/repo.git"\n'
                       'with open('+repr(str(self.sshlog))+',"a") as out: out.write(args[0]+"\\n")\n'
                       'raise SystemExit(subprocess.call([args[0],'+repr(str(self.remote))+']))\n')
        ssh.chmod(0o755)
        self.env.update(GIT_SSH_COMMAND=str(ssh), GIT_SSH_VARIANT='ssh')
        self.access = 'unavailable'
        self.broker_requests = []
        self.auth = []
        self.discovery_failure = False
        self.lose_push_response = False
        self.push_posts = 0
        self.legacy_available = True
        fixture = self
        class Broker(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_): pass
            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                fixture.broker_requests.append(body)
                if self.path == '/token':
                    if not fixture.legacy_available:
                        self.send_error(503); return
                    payload = json.dumps(dict(token='synthetic-broker-token', expires_at='2099-01-01T00:00:00Z')).encode()
                    self.send_response(200); self.send_header('Content-Length', str(len(payload))); self.end_headers(); self.wfile.write(payload)
                    return
                response = dict(version=1, state=fixture.access)
                if fixture.access == 'available':
                    response['token'] = dict(token='synthetic-broker-token', expires_at='2099-01-01T00:00:00Z')
                payload = json.dumps(response).encode()
                self.send_response(200); self.send_header('Content-Length', str(len(payload))); self.end_headers(); self.wfile.write(payload)
        class UnixServer(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
            daemon_threads = True
        sock = self.home/'.local/share/subyard/github.sock'
        sock.parent.mkdir(parents=True)
        self.broker = UnixServer(str(sock), Broker)
        threading.Thread(target=self.broker.serve_forever, daemon=True).start()
        self.addCleanup(self.broker.server_close)
        self.addCleanup(self.broker.shutdown)

    def git(self, *args, cwd=None, expected=0, env=None):
        result = subprocess.run(['git', *map(str, args)], cwd=cwd, env=env or self.env, text=True,
                                capture_output=True, timeout=30)
        if expected is not None:
            diagnostics = result.stderr
            if result.returncode != expected and hasattr(self, 'native_ssh_server_log'):
                diagnostics += '\nFixture sshd evidence:\n'+'\n'.join(self.native_ssh_server_log.read_text().splitlines()[-32:])
            self.assertEqual(result.returncode, expected, diagnostics)
        return result

    def change(self, repo, value):
        (repo/'file').write_text(value+'\n')
        self.git('commit', '-qam', value, cwd=repo)

    def test_native_ssh_commands_options_hooks_and_no_replay(self):
        original = 'git@github.com:owner/repo.git'
        clone = self.work/'clone'
        self.git('clone', original, clone)
        self.assertEqual(self.git('config', '--get', 'remote.origin.url', cwd=clone).stdout.strip(), original)
        self.assertEqual((clone/'file').read_text(), 'one\n')
        hooklog = self.work/'hook.log'
        hook = clone/'.git/hooks/pre-push'
        hook.write_text('#!/bin/sh\nprintf "%s\\n" "$2" >> '+shlex.quote(str(hooklog))+'\n')
        hook.chmod(0o755)
        initial = self.git('rev-parse', 'HEAD', cwd=clone).stdout.strip()
        self.change(clone, 'two')
        self.git('push', '--atomic', 'origin', 'main', cwd=clone)
        self.assertEqual(hooklog.read_text().splitlines(), ['subyard-github::'+original])
        self.assertEqual(self.broker_requests[-1]['operation'], 'push')
        self.assertEqual(self.sshlog.read_text().splitlines()[-2:], ['git-receive-pack']*2)
        current = self.git('rev-parse', 'HEAD', cwd=clone).stdout.strip()
        self.change(clone, 'lease')
        self.git('push', '--force-with-lease=refs/heads/main:'+initial, 'origin', 'main', cwd=clone, expected=1)
        self.git('push', '--force-with-lease=refs/heads/main:'+current, 'origin', 'main', cwd=clone)
        self.git('--git-dir='+str(self.remote), 'config', 'receive.advertisePushOptions', 'true')
        pushhook = self.remote/'hooks/pre-receive'
        pushlog = self.work/'push-options'
        pushhook.write_text("#!/bin/sh\ncat >/dev/null\nprintf '%s:%s\\n' \"$GIT_PUSH_OPTION_COUNT\" \"$GIT_PUSH_OPTION_0\" > "+shlex.quote(str(pushlog))+"\n")
        pushhook.chmod(0o755)
        self.change(clone, 'push option')
        self.git('push', '--push-option=fixture=value', 'origin', 'main', cwd=clone)
        self.assertEqual(pushlog.read_text(), '1:fixture=value\n')
        self.git('tag', 'fixture-tag', cwd=clone)
        self.git('push', 'origin', 'refs/tags/fixture-tag', cwd=clone)
        self.git('push', 'origin', ':refs/tags/fixture-tag', cwd=clone)
        self.git('fetch', self.remote, 'main', cwd=self.seed)
        self.git('reset', '--hard', 'FETCH_HEAD', cwd=self.seed)
        self.change(self.seed, 'three')
        self.git('push', self.remote, 'main', cwd=self.seed)
        self.git('pull', '--ff-only', cwd=clone)
        self.assertEqual((clone/'file').read_text(), 'three\n')
        self.assertIn('refs/heads/main', self.git('ls-remote', 'origin', cwd=clone).stdout)
        self.git('config', 'remote.origin.pushurl', 'ssh://git@github.com/owner/repo.git', cwd=clone)
        self.change(clone, 'partial')
        reject = self.remote/'hooks/update'
        reject.write_text('#!/bin/sh\n[ "$1" != refs/heads/reject ]\n');reject.chmod(0o755)
        before = self.git('--git-dir='+str(self.remote), 'rev-parse', 'main').stdout
        result = self.git('push', '--atomic', 'origin', 'main', 'HEAD:reject', cwd=clone, expected=1)
        self.assertEqual(self.git('--git-dir='+str(self.remote), 'rev-parse', 'main').stdout, before)
        count = self.sshlog.read_text().count('git-receive-pack')
        self.git('push', 'origin', 'main', 'HEAD:reject', cwd=clone, expected=1)
        self.assertEqual(self.sshlog.read_text().count('git-receive-pack')-count, 2)
        self.assertEqual(self.git('--git-dir='+str(self.remote), 'rev-parse', 'main').stdout,
                         self.git('rev-parse', 'HEAD', cwd=clone).stdout)
        self.assertEqual(self.git('config', '--get', 'remote.origin.pushurl', cwd=clone).stdout.strip(),
                         'ssh://git@github.com/owner/repo.git')
        self.git('clone', '--depth=1', original, self.work/'shallow')
        self.assertEqual(self.git('rev-parse', '--is-shallow-repository', cwd=self.work/'shallow').stdout, 'true\n')

    def test_cancellation_drains_native_descendants(self):
        childpid = self.work/'child.pid'
        ssh = self.bin/'fixture-ssh'
        ssh.write_text('#!/usr/bin/env python3\nimport pathlib,subprocess,time\nchild=subprocess.Popen(["python3","-c","import signal,time;signal.signal(signal.SIGTERM,signal.SIG_IGN);time.sleep(60)"])\npathlib.Path('+repr(str(childpid))+').write_text(str(child.pid))\ntime.sleep(60)\n')
        process = subprocess.Popen([str(self.engine), '_github-git-remote', 'origin', 'git@github.com:owner/repo.git'],
                                   env=self.env, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        process.stdin.write(b'capabilities\nlist\n'); process.stdin.flush()
        deadline = time.monotonic()+10
        while not childpid.exists() and time.monotonic() < deadline:
            time.sleep(0.01)
        self.assertTrue(childpid.exists())
        process.send_signal(signal.SIGTERM)
        process.communicate(timeout=5)
        self.assertNotEqual(process.returncode, 0)
        state = Path('/proc')/childpid.read_text()/'stat'
        if state.exists():
            self.assertIn(state.read_text().rsplit(')', 1)[1].split()[0], ('Z', 'X'))

    def test_real_openssh_agent_config_worktree_and_submodules(self):
        sshd = '/usr/sbin/sshd'
        self.assertTrue(Path(sshd).is_file(), 'native OpenSSH acceptance requires sshd')
        user = subprocess.check_output(['id', '-un'], text=True).strip()
        hostkey, identity = self.work/'host-key', self.work/'user-key'
        for key in (hostkey, identity):
            subprocess.run(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', str(key)], check=True)
        dispatcher = self.bin/'ssh-service'
        dispatcher.write_text('#!/usr/bin/python3\nimport os,shlex,sys\nfrom pathlib import Path\n'
                              'args=shlex.split(os.environ["SSH_ORIGINAL_COMMAND"])\n'
                              'assert len(args)==2 and args[0] in ("git-upload-pack","git-receive-pack")\n'
                              'assert args[1].lstrip("/") in ("owner/repo.git","owner/sub.git")\n'
                              'with open('+repr(str(self.sshlog))+',"a") as out: out.write(args[0]+" "+args[1]+"\\n")\n'
                              'os.execvp(args[0],[args[0],str(Path('+repr(str(self.remotes))+')/args[1].lstrip("/"))])\n')
        dispatcher.chmod(0o755)
        authorized = self.work/'authorized_keys'
        authorized.write_text('restrict,command="'+str(dispatcher)+'" '+identity.with_suffix('.pub').read_text())
        authorized.chmod(0o600)
        with socket.socket() as reservation:
            reservation.bind(('127.0.0.1', 0)); port = reservation.getsockname()[1]
        server_config = self.work/'sshd_config'
        server_config.write_text(f'ListenAddress 127.0.0.1\nPort {port}\nHostKey {hostkey}\nPidFile {self.work}/sshd.pid\n'
                                 f'AuthorizedKeysFile {authorized}\nAllowUsers {user}\nUsePAM no\nPasswordAuthentication no\n'
                                 # StrictModes would reject /tmp as a parent. This
                                 # daemon uses only the protected generated key.
                                 f'StrictModes no\n'
                                 f'KbdInteractiveAuthentication no\nAuthenticationMethods publickey\nPermitUserRC no\n'
                                 f'SetEnv HOME={self.home} BASH_ENV=/dev/null ENV=/dev/null\nLogLevel VERBOSE\n')
        server_log = (self.work/'sshd.log').open('w')
        self.native_ssh_server_log = self.work/'sshd.log'
        self.addCleanup(server_log.close)
        server = subprocess.Popen([sshd, '-D', '-e', '-f', str(server_config)], stdout=server_log, stderr=server_log)
        def stop(process):
            if process.poll() is None: process.terminate()
            process.wait(timeout=5)
        self.addCleanup(stop, server)
        deadline = time.monotonic()+5
        while True:
            if server.poll() is not None:
                self.fail('unprivileged sshd failed: '+(self.work/'sshd.log').read_text())
            try:
                with socket.create_connection(('127.0.0.1', port), timeout=0.1): break
            except OSError:
                if time.monotonic()>deadline: self.fail('loopback sshd did not become ready')
                time.sleep(0.01)
        agent_socket = self.work/'agent.sock'
        agent = subprocess.Popen(['ssh-agent', '-D', '-a', str(agent_socket)], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.addCleanup(stop, agent)
        deadline = time.monotonic()+5
        while not agent_socket.exists():
            self.assertIsNone(agent.poll())
            if time.monotonic()>deadline: self.fail('fixture agent did not become ready')
            time.sleep(0.01)
        self.env['SSH_AUTH_SOCK'] = str(agent_socket)
        subprocess.run(['ssh-add', str(identity)], env=self.env, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        known = self.work/'known_hosts'
        known.write_text('fixture-github '+hostkey.with_suffix('.pub').read_text())
        client_config = self.work/'ssh_config'
        client_config.write_text(f'Host github.com\n HostName 127.0.0.1\n Port {port}\n HostKeyAlias fixture-github\n'
                                 f' UserKnownHostsFile {known}\n GlobalKnownHostsFile /dev/null\n StrictHostKeyChecking yes\n'
                                 f' IdentityFile none\n IdentityAgent {agent_socket}\n BatchMode yes\n')
        native = 'ssh -F '+shlex.quote(str(client_config))+' -l '+shlex.quote(user)
        self.env['GIT_SSH_COMMAND'] = native+' -o ConnectTimeout=3'
        clone = self.work/'openssh-clone'
        self.git('clone', 'git@github.com:owner/repo.git', clone)
        self.assertIn('Accepted publickey', (self.work/'sshd.log').read_text())
        del self.env['GIT_SSH_COMMAND']
        self.git('config', 'core.sshCommand', native+' -o ConnectionAttempts=1', cwd=clone)
        linked = self.work/'linked'
        self.git('worktree', 'add', '-b', 'linked', linked, cwd=clone)
        self.change(linked, 'native worktree push')
        self.git('push', 'origin', 'HEAD:main', cwd=linked)
        self.change(self.seed, 'native worktree fetch')
        self.git('push', '--force', self.remote, 'main', cwd=self.seed)
        self.git('fetch', 'origin', cwd=linked)
        self.assertEqual(self.git('rev-parse', 'origin/main', cwd=linked).stdout,
                         self.git('rev-parse', 'HEAD', cwd=self.seed).stdout)
        trusted = known.read_text()
        known.write_text('fixture-github '+identity.with_suffix('.pub').read_text())
        denied = self.git('ls-remote', 'origin', cwd=linked, expected=None)
        self.assertNotEqual(denied.returncode, 0)
        self.assertIn('HOST IDENTIFICATION HAS CHANGED', denied.stderr)
        known.write_text(trusted)
        # Submodule URLs go through only the managed, strictly validating helper.
        self.env['GIT_SSH_COMMAND'] = native
        self.git('clone', '--bare', self.remote, self.remotes/'owner/sub.git')
        self.git('config', '-f', '.gitmodules', 'submodule.child.path', 'child', cwd=self.seed)
        self.git('config', '-f', '.gitmodules', 'submodule.child.url', 'git@github.com:owner/sub.git', cwd=self.seed)
        oid = self.git('rev-parse', 'HEAD', cwd=self.seed).stdout.strip()
        self.git('update-index', '--add', '--cacheinfo', '160000,'+oid+',child', cwd=self.seed)
        self.git('add', '.gitmodules', cwd=self.seed)
        self.git('commit', '-qm', 'Canonical routed submodule', cwd=self.seed)
        self.git('push', self.remote, 'main', cwd=self.seed)
        recursive = self.work/'recursive'
        self.git('clone', '--recurse-submodules', 'git@github.com:owner/repo.git', recursive)
        self.assertTrue((recursive/'child/file').is_file())
        self.assertTrue(any(row['repository']=='owner/sub' for row in self.broker_requests))
        self.assertEqual(self.git('config', '--get', 'protocol.subyard-github.allow').stdout, 'always\n')
        self.assertNotEqual(self.git('config', '--get', 'protocol.allow', expected=None).returncode, 0)
        self.assertNotEqual(self.git('-c', 'protocol.file.allow=never', 'submodule', 'add', str(self.remote), 'forbidden', cwd=recursive, expected=None).returncode, 0)

    def test_git_fetch_worker_observes_router_in_shared_worktree(self):
        log = self.start_https()
        self.access = 'available'
        clone, linked = self.work/'worker-clone', self.work/'worker-linked'
        self.git('clone', 'https://github.com/owner/repo.git', clone)
        self.git('worktree', 'add', '-b', 'linked', linked, cwd=clone)
        original = self.git('rev-parse', 'HEAD', cwd=linked).stdout
        self.change(self.seed, 'background route')
        self.git('push', self.remote, 'main', cwd=self.seed)
        spec = importlib.util.spec_from_file_location('native_fetch_worker', ROOT/'config/profiles/git-fetch/fetch.py')
        worker = importlib.util.module_from_spec(spec); spec.loader.exec_module(worker)
        wrapper = self.bin/'must-not-run'
        wrapper.write_text('#!/bin/sh\nexit 99\n'); wrapper.chmod(0o755)
        before = len(self.broker_requests)
        with patch.dict(os.environ, self.env, clear=True), patch.object(worker, 'GITHUB_CLIENT', wrapper):
            self.assertEqual(worker.fetch_repositories([clone, linked]), (1, 0))
        self.assertEqual(self.broker_requests[before:], [{'repository':'owner/repo', 'operation':'read'}])
        self.assertEqual(self.git('rev-parse', 'origin/main', cwd=linked).stdout,
                         self.git('rev-parse', 'HEAD', cwd=self.seed).stdout)
        self.assertEqual(self.git('rev-parse', 'HEAD', cwd=linked).stdout, original)
        self.assertFalse(log.exists())

    def start_https(self):
        certificate = self.work/'cert.pem'
        key = self.work/'key.pem'
        subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1',
                        '-subj', '/CN=github.com', '-keyout', str(key), '-out', str(certificate)],
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True)
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(certificate, key)
        fixture = self
        class HTTPS(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_): pass
            def do_CONNECT(self):
                self.send_response(200); self.end_headers()
                self.connection = context.wrap_socket(self.connection, server_side=True)
                self.rfile = self.connection.makefile('rb')
                self.wfile = self.connection.makefile('wb')
                self.handle_one_request()
                self.close_connection = True
            def finish(self):
                super().finish()
                self.connection.close()
            def do_GET(self): self.backend()
            def do_POST(self): self.backend()
            def backend(self):
                authorization = self.headers.get('Authorization', '')
                if not authorization:
                    self.send_response(401); self.send_header('WWW-Authenticate', 'Basic realm="fixture"'); self.end_headers(); return
                decoded = base64.b64decode(authorization.split()[1]).decode()
                fixture.auth.append(decoded)
                if fixture.discovery_failure and 'synthetic-broker-token' in decoded:
                    self.send_response(403); self.end_headers(); return
                path, _, query = self.path.partition('?')
                environment = dict(os.environ, GIT_PROJECT_ROOT=str(fixture.remotes), GIT_HTTP_EXPORT_ALL='1',
                                   PATH_INFO=path, QUERY_STRING=query, REQUEST_METHOD=self.command,
                                   CONTENT_TYPE=self.headers.get('Content-Type', ''), REMOTE_USER='fixture')
                body = self.rfile.read(int(self.headers.get('Content-Length', '0')))
                result = subprocess.run(['git', 'http-backend'], input=body, env=environment, capture_output=True)
                if self.command == 'POST' and path.endswith('/git-receive-pack'):
                    fixture.push_posts += 1
                    if fixture.lose_push_response:
                        self.close_connection = True
                        return
                headers, data = result.stdout.split(b'\r\n\r\n', 1)
                self.send_response(200)
                for header in headers.decode().splitlines():
                    name, value = header.split(':', 1)
                    if name.lower() != 'status': self.send_header(name, value.strip())
                self.send_header('Content-Length', str(len(data))); self.end_headers(); self.wfile.write(data)
        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), HTTPS)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close); self.addCleanup(server.shutdown)
        self.git('config', '--global', 'http.proxy', 'http://127.0.0.1:'+str(server.server_port))
        self.git('config', '--global', 'http.sslVerify', 'false')
        log = self.work/'credentials.log'
        credential = self.bin/'original-credential'
        credential.write_text('#!/usr/bin/env python3\nimport sys\ndata=sys.stdin.read()\n'
                              'with open('+repr(str(log))+',"a") as out: out.write(sys.argv[1]+" "+data.replace("\\n"," ")+"\\n")\n'
                              'if sys.argv[1]=="get": print("username=original\\npassword=original-password\\n")\n')
        credential.chmod(0o755)
        self.git('config', '--global', 'credential.helper', str(credential))
        return log

    def test_broker_https_isolation_and_original_https_credentials(self):
        log = self.start_https()
        self.access = 'available'
        (self.home/'.netrc').write_text('machine github.com login netrc-user password netrc-password\n')
        (self.home/'.netrc').chmod(0o600)
        self.git('config', '--global', 'credential.https://github.com.helper', str(self.bin/'original-credential'))
        clone = self.work/'broker-clone'
        self.git('clone', 'https://github.com/owner/repo.git', clone)
        self.assertTrue(self.auth and all('synthetic-broker-token' in value for value in self.auth))
        self.assertFalse(log.exists(), 'broker token reached an ordinary helper')
        self.change(clone, 'broker push')
        self.git('push', '--atomic', 'origin', 'main', cwd=clone)
        self.assertFalse(log.exists())
        (self.home/'.netrc').unlink()
        self.discovery_failure = True
        self.git('ls-remote', 'https://github.com/owner/repo.git')
        self.assertIn('original-password', self.auth[-1])
        self.assertNotIn('synthetic-broker-token', log.read_text())

        self.discovery_failure = False
        log.unlink()
        self.change(clone, 'lost response')
        self.lose_push_response = True
        result = self.git('push', 'origin', 'main', cwd=clone, expected=None)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.push_posts, 2)  # one normal push and one unreplayed lost reply
        self.assertFalse(log.exists())
        self.assertEqual(self.git('--git-dir='+str(self.remote), 'rev-parse', 'main').stdout, self.git('rev-parse', 'HEAD', cwd=clone).stdout)
        self.lose_push_response = False
        self.access = 'insufficient-permissions'
        self.auth.clear()
        inherited = dict(self.env, GIT_CONFIG_PARAMETERS="'credential.useHttpPath=true'", GIT_CONFIG_COUNT='1',
                         GIT_CONFIG_KEY_0='credential.username', GIT_CONFIG_VALUE_0='inherited-user')
        self.git('ls-remote', 'https://github.com/owner/repo.git', env=inherited)
        self.assertTrue(self.auth and all('original-password' in value for value in self.auth))
        self.assertIn('path=owner/repo.git', log.read_text())
        self.assertNotIn('synthetic-broker-token', log.read_text())

    def test_command_configuration_isolation_and_original_auth_preservation(self):
        log = self.start_https()
        self.access = 'available'
        store = self.work/'ordinary-store'
        store_helper = 'store --file='+shlex.quote(str(store))
        user_header = 'Authorization: Basic '+base64.b64encode(b'original-header:user-auth').decode()
        path = 'http.https://github.com/owner/repo.git.'
        self.git('config', '--global', path+'extraHeader', user_header)
        self.git('config', '--global', path+'proactiveAuth', 'none')
        (self.home/'.netrc').write_text('machine github.com login x-access-token password ordinary-netrc-password\n')
        (self.home/'.netrc').chmod(0o600)
        inherited = dict(self.env, GIT_CONFIG_COUNT='2', GIT_CONFIG_KEY_0='credential.helper',
                         GIT_CONFIG_VALUE_0=str(self.bin/'original-credential'),
                         GIT_CONFIG_KEY_1=path+'extraHeader', GIT_CONFIG_VALUE_1=user_header,
                         GIT_CONFIG_PARAMETERS="'credential.helper="+store_helper+"' 'http.emptyAuth=true'")
        args = ('-c', 'credential.helper='+store_helper, '-c', path+'extraHeader='+user_header,
                '-c', path+'proactiveAuth=none', 'ls-remote', 'https://github.com/owner/repo.git')
        self.git(*args, env=inherited)
        self.assertTrue(self.auth and all(value=='x-access-token:synthetic-broker-token' for value in self.auth))
        self.assertFalse(store.exists(), 'broker token entered ordinary store')
        self.assertFalse(log.exists(), 'ordinary helper ran for broker authorization')
        self.access = 'insufficient-permissions'
        self.auth.clear()
        result = self.git(*args, env=inherited)
        self.assertIn('permissions are insufficient', result.stderr)
        self.assertTrue(self.auth and all(value=='original-header:user-auth' for value in self.auth))
        if store.exists(): self.assertNotIn('synthetic-broker-token', store.read_text())
        if log.exists(): self.assertNotIn('synthetic-broker-token', log.read_text())

    def test_explicit_run_isolates_legacy_token_with_custom_socket_and_no_fallback(self):
        log = self.start_https()
        custom = self.work/'custom.sock'
        (self.home/'.local/share/subyard/github.sock').rename(custom)
        store = self.work/'legacy-store'
        helper = 'store --file='+shlex.quote(str(store))
        inherited = dict(self.env, GIT_CONFIG_PARAMETERS="'credential.helper="+helper+"'",
                         GIT_CONFIG_COUNT='1', GIT_CONFIG_KEY_0='credential.helper', GIT_CONFIG_VALUE_0=helper)
        command = [str(self.engine), '_github-client', '--socket', str(custom), 'run', '--',
                   'git', '-c', 'credential.helper='+helper, 'ls-remote', 'https://github.com/owner/repo.git']
        result = subprocess.run(command, env=inherited, capture_output=True, text=True, timeout=20)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(self.auth and all(value=='x-access-token:synthetic-broker-token' for value in self.auth))
        self.assertEqual(self.broker_requests, [{'repository':'owner/repo'}])
        self.assertFalse(store.exists())
        self.assertFalse(log.exists())
        self.legacy_available = False
        before = len(self.auth)
        result = subprocess.run(command, env=inherited, capture_output=True, text=True, timeout=20)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('explicit GitHub broker authorization failed', result.stderr)
        self.assertNotIn('using original authentication', result.stderr)
        self.assertEqual(len(self.auth), before)
        self.assertFalse(store.exists())
        self.assertFalse(log.exists())
        command[-1] = 'git@github.com:owner/repo.git'
        before = len(self.broker_requests)
        result = subprocess.run(command, env=inherited, capture_output=True, text=True, timeout=20)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(self.broker_requests), before)

    def test_fallback_reasons_and_selected_native_errors(self):
        log = self.start_https()
        reasons = {'no-repo-access':'has no repository access', 'insufficient-permissions':'permissions are insufficient',
                   'unknown':'permissions are unknown', 'unavailable':'broker is unavailable'}
        for state, reason in reasons.items():
            self.access = state
            result = self.git('ls-remote', 'https://github.com/owner/repo.git')
            self.assertIn(reason+'; using original authentication', result.stderr)
            failed = self.git('ls-remote', 'git@github.com:owner/repo.git',
                              env=dict(self.env, GIT_SSH_COMMAND='false'), expected=None)
            self.assertNotEqual(failed.returncode, 0)
            self.assertIn(reason+'; using original authentication', failed.stderr)
        self.access = 'available'
        self.discovery_failure = True
        result = self.git('ls-remote', 'https://github.com/owner/repo.git')
        self.assertIn('broker ref discovery failed; using original authentication', result.stderr)
        self.assertNotIn('403', result.stderr)
        self.discovery_failure = False
        if log.exists(): log.unlink()
        clone = self.work/'native-errors'
        self.git('clone', 'https://github.com/owner/repo.git', clone)
        progress = self.remote/'hooks/post-receive'
        progress.write_text('#!/bin/sh\ncat >/dev/null\nprintf "fixture-selected-native-progress\\n" >&2\n')
        progress.chmod(0o755)
        self.change(clone, 'native progress')
        result = self.git('push', 'origin', 'main', cwd=clone)
        self.assertIn('fixture-selected-native-progress', result.stderr)
        hook = self.remote/'hooks/pre-receive'
        hook.write_text('#!/bin/sh\ncat >/dev/null\nprintf "fixture-selected-native-error synthetic-broker-token\\n" >&2\nexit 1\n')
        hook.chmod(0o755)
        self.change(clone, 'reject with native diagnostic')
        before = self.push_posts
        result = self.git('push', 'origin', 'main', cwd=clone, expected=None)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('fixture-selected-native-error', result.stderr)
        self.assertIn('[redacted]', result.stderr)
        self.assertNotIn('synthetic-broker-token', result.stderr)
        self.assertEqual(self.push_posts-before, 1)
        self.assertFalse(log.exists())


if __name__ == '__main__':
    unittest.main()
