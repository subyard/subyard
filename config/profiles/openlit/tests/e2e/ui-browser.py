#!/usr/bin/env python3
"""Render native telemetry on a disposable OpenLIT stage through the normal login form."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import socket
import stat
import struct
import subprocess
import sys
import tempfile
import time
import urllib.parse
import urllib.request


class Failure(Exception):
    pass


def protected(path, directory=False):
    for item in (path, *path.parents):
        if item.is_symlink():
            raise Failure('unsafe-path')
    info = path.lstat()
    if info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != (0o700 if directory else 0o600):
        raise Failure('private-permissions')
    if directory:
        if not stat.S_ISDIR(info.st_mode):
            raise Failure('private-directory')
    elif not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_size > 65536:
        raise Failure('private-file')


def write_private(path, content):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'wb') as out:
        os.fchmod(out.fileno(), 0o600)
        out.write(content)


class CDP:
    """Small RFC6455 transport for local Chromium; no third-party dependency."""
    def __init__(self, url, deadline):
        self.deadline = deadline
        self.next_id = 0
        target = urllib.parse.urlsplit(url)
        if target.scheme != 'ws' or target.hostname != '127.0.0.1' or not target.path.startswith('/devtools/page/'):
            raise Failure('devtools-origin')
        self.socket = socket.create_connection(('127.0.0.1', target.port), timeout=self.remaining())
        self.buffer = b''
        key = base64.b64encode(os.urandom(16)).decode()
        headers = f'GET {target.path} HTTP/1.1\r\nHost: 127.0.0.1:{target.port}\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n'
        self.socket.sendall(headers.encode())
        while b'\r\n\r\n' not in self.buffer:
            self.socket.settimeout(self.remaining())
            block = self.socket.recv(4096)
            if not block:
                raise Failure('devtools-handshake')
            self.buffer += block
            if len(self.buffer) > 65536:
                raise Failure('devtools-handshake')
        response, self.buffer = self.buffer.split(b'\r\n\r\n', 1)
        expected = base64.b64encode(hashlib.sha1((key+'258EAFA5-E914-47DA-95CA-C5AB0DC85B11').encode()).digest())
        if not response.startswith(b'HTTP/1.1 101 ') or expected not in response:
            raise Failure('devtools-handshake')

    def remaining(self):
        remaining = self.deadline-time.monotonic()
        if remaining <= 0:
            raise Failure('deadline')
        return remaining

    def take(self, size):
        while len(self.buffer) < size:
            self.socket.settimeout(self.remaining())
            block = self.socket.recv(65536)
            if not block:
                raise Failure('devtools-closed')
            self.buffer += block
        result, self.buffer = self.buffer[:size], self.buffer[size:]
        return result

    def send(self, payload, opcode=1):
        mask = os.urandom(4)
        size = len(payload)
        length = bytes([size|128]) if size < 126 else bytes([126|128])+struct.pack('!H', size) if size < 65536 else bytes([127|128])+struct.pack('!Q', size)
        data = bytes(v ^ mask[i % 4] for i, v in enumerate(payload))
        self.socket.settimeout(self.remaining())
        self.socket.sendall(bytes([128|opcode])+length+mask+data)

    def receive(self):
        chunks = []
        while True:
            first, second = self.take(2)
            opcode, size = first & 15, second & 127
            if size == 126:
                size = struct.unpack('!H', self.take(2))[0]
            elif size == 127:
                size = struct.unpack('!Q', self.take(8))[0]
            if size > 32*1024*1024:
                raise Failure('devtools-frame-size')
            mask = self.take(4) if second & 128 else None
            data = self.take(size)
            if mask:
                data = bytes(v ^ mask[i % 4] for i, v in enumerate(data))
            if opcode == 8:
                raise Failure('devtools-closed')
            if opcode == 9:
                self.send(data, 10)
                continue
            if opcode == 10:
                continue
            chunks.append(data)
            if first & 128:
                return json.loads(b''.join(chunks))

    def call(self, method, params=None):
        self.next_id += 1
        identity = self.next_id
        self.send(json.dumps({'id': identity, 'method': method, 'params': params or {}}).encode())
        while True:
            response = self.receive()
            if response.get('id') == identity:
                if 'error' in response:
                    raise Failure('devtools-command')
                return response.get('result', {})

    def evaluate(self, expression):
        result = self.call('Runtime.evaluate', {'expression': expression, 'awaitPromise': True, 'returnByValue': True})
        if 'exceptionDetails' in result:
            raise Failure('browser-script')
        return result.get('result', {}).get('value')


def wait(cdp, expression, deadline, category):
    while time.monotonic() < deadline:
        try:
            value = cdp.evaluate(expression)
            if value:
                return value
        except Failure as failure:
            if str(failure) != 'browser-script':
                raise
        time.sleep(0.2)
    raise Failure(category)


def discover(profile, deadline):
    while time.monotonic() < deadline:
        try:
            port = int((profile/'DevToolsActivePort').read_text().splitlines()[0])
            with urllib.request.urlopen(f'http://127.0.0.1:{port}/json/list', timeout=2) as response:
                targets = json.load(response)
            for target in targets:
                if target.get('type') == 'page':
                    return target['webSocketDebuggerUrl']
        except (OSError, ValueError, KeyError):
            pass
        time.sleep(0.1)
    raise Failure('chromium-startup')


def screenshot(cdp, output, name):
    result = cdp.call('Page.captureScreenshot', {'format': 'png', 'captureBeyondViewport': False})
    path = output/(name+'.png')
    write_private(path, base64.b64decode(result['data']))
    return str(path)


OBSERVE = r'''(() => {
 const visible=e=>!!(e.offsetWidth||e.offsetHeight||e.getClientRects().length);
 const rows=[...document.querySelectorAll('section .grid.gap-2 > button.w-full.text-left.border')].filter(visible);
 const service=(row,name)=>[...row.querySelectorAll('span')].some(e=>e.innerText.trim().toLowerCase()===name);
 const alerts=[...document.querySelectorAll('[role="alert"], [data-sonner-toast][data-type="error"]')].filter(visible).map(e=>e.innerText.toLowerCase());
 const errors=[];
 if(alerts.some(t=>/sql|clickhouse|unknown identifier|syntax error|db::exception|code: \d+/.test(t))) errors.push('sql-query');
 else if(alerts.some(t=>/unauthori|forbidden|credential/.test(t))) errors.push('authentication');
 else if(alerts.length) errors.push('rendered-alert');
 const labels=[...document.querySelectorAll('button')].filter(visible).map(e=>e.innerText.trim());
 return {path:location.pathname,tab:new URLSearchParams(location.search).get('tab'),title:document.title.includes('OpenLIT')?'OpenLIT':'rendered',rows:rows.length,codexRows:rows.filter(e=>service(e,'codex_cli_rs')).length,claudeRows:rows.filter(e=>service(e,'claude-code')).length,errors,tabs:['Logs','Metrics','Traces'].every(t=>labels.includes(t))};
})()'''


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--origin', required=True)
    parser.add_argument('--credentials', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--timeout', type=float, default=120)
    args = parser.parse_args()
    process = cdp = None
    summary = {'pages': [], 'authenticated': False}
    try:
        if not 15 <= args.timeout <= 120:
            raise Failure('invalid-timeout')
        parsed = urllib.parse.urlsplit(args.origin)
        if parsed.scheme != 'http' or parsed.hostname not in ('127.0.0.1', 'localhost') or not parsed.port or parsed.username or parsed.password or parsed.path not in ('', '/') or parsed.query or parsed.fragment:
            raise Failure('loopback-origin-required')
        origin = args.origin.rstrip('/')
        protected(args.credentials)
        protected(args.credentials.parent, True)
        credentials = json.loads(args.credentials.read_text())
        if set(credentials) != {'email', 'password'} or not all(isinstance(v, str) and v for v in credentials.values()):
            raise Failure('invalid-credentials')
        if not args.output.exists():
            for parent in (args.output, *args.output.parents):
                if parent.is_symlink():
                    raise Failure('unsafe-output')
            args.output.mkdir(mode=0o700)
            os.chmod(args.output, 0o700)
        protected(args.output, True)
        binary = shutil.which('chromium') or shutil.which('chromium-browser') or shutil.which('google-chrome')
        if not binary:
            raise Failure('chromium-missing')
        deadline = time.monotonic()+args.timeout-10  # Reserve bounded browser cleanup.
        with tempfile.TemporaryDirectory(prefix='openlit-browser-', dir='/tmp') as temporary:
            profile = Path(temporary)
            os.chmod(profile, 0o700)
            with (profile/'chromium.log').open('wb') as log:
                os.chmod(profile/'chromium.log', 0o600)
                command = [binary, '--headless=new', '--remote-debugging-address=127.0.0.1', '--remote-debugging-port=0', '--user-data-dir='+temporary, '--disable-background-networking', '--disable-component-update', '--disable-sync', '--no-first-run', '--no-default-browser-check', '--disable-breakpad', '--disable-crash-reporter', '--password-store=basic', '--disable-dev-shm-usage', '--window-size=1600,1000', '--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE localhost, EXCLUDE 127.0.0.1', 'about:blank']
                if os.getuid() == 0:
                    command.insert(1, '--no-sandbox')
                process = subprocess.Popen(command, stdout=log, stderr=log, start_new_session=True, env={'PATH': os.defpath, 'HOME': temporary, 'LANG': 'C.UTF-8'})
                try:
                    cdp = CDP(discover(profile, min(deadline, time.monotonic()+20)), deadline)
                    cdp.call('Page.enable')
                    cdp.call('Emulation.setDeviceMetricsOverride', {'width': 1600, 'height': 1000, 'deviceScaleFactor': 1, 'mobile': False})
                    cdp.call('Page.navigate', {'url': origin+'/dashboard'})
                    wait(cdp, 'location.pathname==="/login"&&!!document.querySelector("form input[name=email]")&&!!document.querySelector("form input[name=password]")', min(deadline, time.monotonic()+25), 'anonymous-login-redirect')
                    screenshot(cdp, args.output, 'anonymous-login')
                    # Native input setter + events update React, then click the actual
                    # submit control. No auth endpoint is called by the driver.
                    expression = '''(creds=>{const f=document.querySelector('form:has(input[name="password"])');const set=Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set;for(const name of ['email','password']){const e=f.querySelector('[name="'+name+'"]');set.call(e,creds[name]);e.dispatchEvent(new Event('input',{bubbles:true}));e.dispatchEvent(new Event('change',{bubbles:true}));}f.querySelector('[type="submit"]').click();return true;})(CREDENTIALS)'''.replace('CREDENTIALS', json.dumps(credentials))
                    cdp.evaluate(expression)
                    del expression
                    wait(cdp, 'location.pathname!=="/login"&&location.pathname!=="/register"&&location.pathname!=="/onboarding"&&document.readyState==="complete"', min(deadline, time.monotonic()+30), 'rendered-login')
                    active = cdp.evaluate('(async()=>{const r=await fetch("/api/auth/session",{credentials:"same-origin"});const v=await r.json();return r.ok&&!!v.user&&v.user.email===EMAIL;})()'.replace('EMAIL', json.dumps(credentials['email'])))
                    if not active:
                        raise Failure('active-session')
                    summary['authenticated'] = True
                    del credentials
                    for tab in ('logs', 'metrics', 'traces'):
                        cdp.call('Page.navigate', {'url': origin+'/telemetry?tab='+tab+'&tr=7D'})
                        tab_deadline = min(deadline, time.monotonic()+30)
                        wait(cdp, 'location.pathname==="/telemetry"&&document.readyState==="complete"&&[...document.querySelectorAll("button")].some(b=>b.innerText.trim()==="'+tab.title()+'")', tab_deadline, 'telemetry-navigation')
                        cdp.evaluate('(()=>{const b=[...document.querySelectorAll("button")].find(b=>b.innerText.trim()==="Clear Filters"&&!b.disabled);if(b)b.click();return true;})()')
                        observation = None
                        ready_since = None
                        while time.monotonic() < tab_deadline:
                            observation = cdp.evaluate(OBSERVE)
                            if observation.get('errors'):
                                break
                            if observation.get('rows', 0)>0 and observation.get('codexRows',0)>0:
                                ready_since = ready_since or time.monotonic()
                                if time.monotonic()-ready_since >= 1.5:
                                    break
                            else:
                                ready_since = None
                            time.sleep(0.3)
                        observation = observation or {}
                        observation['screenshot'] = screenshot(cdp, args.output, tab)
                        summary['pages'].append(observation)
                        if observation.get('errors'):
                            raise Failure(observation['errors'][0])
                        if not observation.get('tabs') or observation.get('tab') != tab or not observation.get('rows') or not observation.get('codexRows'):
                            raise Failure('native-service-rows-missing-'+tab)
                    write_private(args.output/'summary.json', (json.dumps(summary, sort_keys=True)+'\n').encode())
                    print(json.dumps(summary, sort_keys=True))
                finally:
                    if cdp:
                        cdp.socket.close()
                    if process and process.poll() is None:
                        os.killpg(process.pid, signal.SIGTERM)
                        try:
                            process.wait(timeout=5)
                        except subprocess.TimeoutExpired:
                            os.killpg(process.pid, signal.SIGKILL)
                            process.wait(timeout=5)
    except Exception as error:
        # Never print browser protocol, page text, URLs, API bodies or credentials.
        summary['error_category'] = str(error) if isinstance(error, Failure) else 'browser-driver'
        print(json.dumps(summary, sort_keys=True), file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
