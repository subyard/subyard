#!/usr/bin/env python3
"""Bootstrap the pinned OpenLIT seed account through its normal password API.

Call prepare before Compose creates volumes, apply after the UI starts, and check
from readiness after runtime verifies Docker inventory and mounted volume ownership.
Mutation callers hold the profile lifecycle lock. Credentials are only
written to root-private operator.json, never returned on stdout or in diagnostics.
Existing volumes keep their accounts and passwords; only a recorded fresh install
may rotate the stock seed. The seed email stays unchanged so upstream restart
seeding retains its existing account, organisation, project and database links.
"""
import argparse
import http.cookiejar
import json
import os
from pathlib import Path
import re
import secrets
import stat
import subprocess
import sys
import tempfile
import urllib.error
import urllib.parse
import urllib.request

EMAIL = 'user@openlit.io'
ORIGIN = 'http://127.0.0.1:3000'
VOLUME = 'subyard-openlit_openlit-data'
STOCK_HASH = '$2a$10$gh6Odw7fhLRrE1A1OxaHfeWOWKiZEEQpkOAhhCQ.RHx8VWOngwlHO'
# Read-only Prisma access is limited to linkage/identity and a boolean stock check.
# Password hashes and database connection fields never leave the container.
PROBE = r'''
const {PrismaClient}=require('/app/client/node_modules/@prisma/client');
const p=new PrismaClient();
(async()=>{
 const u=await p.user.findUnique({where:{email:'user@openlit.io'}});
 const o=await p.organisation.findUnique({where:{slug:'default'}});
 const j=o&&await p.project.findUnique({where:{organisationId_slug:{organisationId:o.id,slug:'default'}}});
 const d=j&&await p.databaseConfig.findUnique({where:{name_projectId_environment:{name:'Default DB',projectId:j.id,environment:'production'}}});
 if(!u||!o||!j||!d)throw Error();
 const m=await p.organisationUser.findUnique({where:{organisationId_userId:{organisationId:o.id,userId:u.id}}});
 const a=await p.databaseConfigUser.findUnique({where:{databaseConfigId_userId:{databaseConfigId:d.id,userId:u.id}}});
 if(!m||!a||o.createdByUserId!==u.id||d.createdByUserId!==u.id)throw Error();
 const counts=await Promise.all([p.user.count(),p.organisation.count(),p.project.count(),p.databaseConfig.count()]);
 const fresh=counts.every(n=>n===1)&&m.role==='owner'&&m.isCurrent&&m.currentProjectId===j.id&&a.isCurrent&&a.canEdit&&a.canDelete&&a.canShare&&u.hasCompletedOnboarding;
 console.log(JSON.stringify({ids:{user:u.id,organisation:o.id,project:j.id,database:d.id},stock:u.password==='STOCK',fresh:Boolean(fresh)}));
})().catch(()=>{process.exitCode=1}).finally(()=>p.$disconnect());
'''.replace('STOCK', STOCK_HASH)


def private(path, mode, directory=False):
    for parent in (path, *path.parents):
        if parent.is_symlink():
            raise ValueError('unsafe account path')
    info = path.lstat()
    kind = stat.S_ISDIR if directory else stat.S_ISREG
    if not kind(info.st_mode) or info.st_uid != os.getuid() or info.st_gid != os.getgid() or stat.S_IMODE(info.st_mode) != mode:
        raise ValueError('account state must be root-private')
    if not directory and (info.st_nlink != 1 or info.st_size > 65536):
        raise ValueError('unsafe account file')


def guard(state):
    private(state, 0o700, True)
    private(state/'marker', 0o600)
    if (state/'marker').read_text() != 'subyard-openlit-v1\n':
        raise ValueError('unmanaged account state')
    for name in ('account.json', 'operator.json', 'database.env'):
        path = state/name
        if path.exists() or path.is_symlink():
            private(path, 0o600)


def write(path, value):
    fd, temporary = tempfile.mkstemp(prefix='.account-', dir=path.parent)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, 'w') as out:
            json.dump(value, out, sort_keys=True)
            out.write('\n')
            out.flush()
            os.fsync(out.fileno())
        os.replace(temporary, path)
        directory = os.open(path.parent, os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def load(state):
    guard(state)
    if not (state/'account.json').exists():
        return None
    value = json.loads((state/'account.json').read_text())
    if not isinstance(value, dict) or set(value) != {'schema', 'kind', 'phase', 'ids'} or type(value['schema']) is not int or value['schema'] != 1 or value['kind'] not in ('fresh', 'existing') or value['phase'] not in ('pending', 'ready'):
        raise ValueError('invalid account intent')
    ids = value['ids']
    if ids is not None and (not isinstance(ids, dict) or set(ids) != {'user', 'organisation', 'project', 'database'} or any(not isinstance(v, str) or not re.fullmatch('[a-zA-Z0-9_-]{1,128}', v) for v in ids.values())):
        raise ValueError('invalid account identities')
    if value['phase'] == 'ready' and ids is None:
        raise ValueError('missing account identities')
    if value['kind'] == 'fresh':
        credentials(state)
    elif (state/'operator.json').exists():
        raise ValueError('unexpected account credentials')
    return value


def credentials(state):
    private(state/'operator.json', 0o600)
    value = json.loads((state/'operator.json').read_text())
    if not isinstance(value, dict) or set(value) != {'email', 'password'} or value['email'] != EMAIL or not isinstance(value['password'], str) or not re.fullmatch('Openlit-7-[a-f0-9]{64}', value['password']):
        raise ValueError('invalid operator credentials')
    return value


def run(argv, timeout=20):
    result = subprocess.run(argv, capture_output=True, timeout=timeout, check=False)
    if result.returncode:
        raise ValueError('account native verification failed')
    return result.stdout


def volume_exists():
    names = run(['docker', 'volume', 'ls', '-q']).decode().splitlines()
    if VOLUME not in names:
        return False
    value = json.loads(run(['docker', 'volume', 'inspect', VOLUME]))
    if not isinstance(value, list) or len(value) != 1 or not isinstance(value[0], dict) or value[0].get('Name') != VOLUME or (value[0].get('Labels') or {}).get('com.docker.compose.project') != 'subyard-openlit' or (value[0].get('Labels') or {}).get('com.docker.compose.volume') != 'openlit-data':
        raise ValueError('unmanaged account volume')
    return True


def prepare(state):
    value = load(state)
    exists = volume_exists()
    if value is not None:
        if (value['phase'] == 'ready' or value['kind'] == 'existing') and not exists:
            raise ValueError('initialized account volume is missing')
        return value
    if (state/'operator.json').exists() and exists:
        raise ValueError('missing fresh account intent; retain credentials and recover ownership')
    kind = 'existing' if exists else 'fresh'
    if kind == 'fresh':
        if (state/'operator.json').exists():
            credentials(state)
        else:
            write(state/'operator.json', {'email': EMAIL, 'password': 'Openlit-7-'+secrets.token_hex(32)})
    value = {'schema': 1, 'kind': kind, 'phase': 'pending', 'ids': None}
    write(state/'account.json', value)
    return value


def inspect(state, source):
    private(state/'database.env', 0o600)
    if not volume_exists():
        raise ValueError('account volume is missing')
    compose = ['docker', 'compose', '--project-directory', str(state), '--env-file', str(state/'database.env'), '-f', str(source/'compose.yaml')]
    identity = run([*compose, 'ps', '-q', 'openlit']).decode().strip()
    if not re.fullmatch('[a-f0-9]{64}', identity):
        raise ValueError('unexpected account container')
    containers = json.loads(run(['docker', 'inspect', identity]))
    if not isinstance(containers, list) or len(containers) != 1:
        raise ValueError('unexpected account container')
    container = containers[0]
    labels = container.get('Config', {}).get('Labels') or {}
    mounts = [m for m in container.get('Mounts', []) if m.get('Destination') == '/app/client/data']
    if container.get('Id') != identity or labels.get('com.docker.compose.project') != 'subyard-openlit' or labels.get('com.docker.compose.service') != 'openlit' or len(mounts) != 1 or mounts[0].get('Type') != 'volume' or mounts[0].get('Name') != VOLUME:
        raise ValueError('account container uses an unmanaged data volume')
    return linkage(state, source)


def linkage(state, source, identity=None):
    # Runtime readiness already verifies container/volume ownership. Its account
    # projection uses one bounded process instead of repeating Docker inventory.
    private(state/'database.env', 0o600)
    if identity is None:
        command = ['docker', 'compose', '--project-directory', str(state), '--env-file', str(state/'database.env'), '-f', str(source/'compose.yaml'), 'exec', '-T', 'openlit']
    else:
        if not isinstance(identity, str) or not re.fullmatch('[a-f0-9]{64}', identity):
            raise ValueError('invalid verified account container')
        command = ['docker', 'exec', identity]
    value = json.loads(run([*command, 'node', '-e', PROBE], timeout=8))
    if not isinstance(value, dict) or set(value) != {'ids', 'stock', 'fresh'} or type(value['stock']) is not bool or type(value['fresh']) is not bool:
        raise ValueError('invalid native account observation')
    ids = value['ids']
    if not isinstance(ids, dict) or set(ids) != {'user', 'organisation', 'project', 'database'} or any(not isinstance(v, str) or not re.fullmatch('[a-zA-Z0-9_-]{1,128}', v) for v in ids.values()):
        raise ValueError('invalid native account identities')
    return value


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, message, headers, newurl):
        return None


class Client:
    def __init__(self):
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()), NoRedirect())

    def request(self, path, payload=None, form=False):
        data = None if payload is None else (urllib.parse.urlencode(payload).encode() if form else json.dumps(payload).encode())
        headers = {'Origin': ORIGIN, 'Content-Type': 'application/x-www-form-urlencoded' if form else 'application/json'}
        try:
            with self.opener.open(urllib.request.Request(ORIGIN+path, data=data, headers=headers), timeout=10) as response:
                return json.loads(response.read(65536))
        except (urllib.error.URLError, ValueError):
            raise ValueError('account API verification failed') from None

    def login(self, password):
        csrf = self.request('/api/auth/csrf')['csrfToken']
        result = self.request('/api/auth/callback/login', {'csrfToken': csrf, 'email': EMAIL, 'password': password, 'callbackUrl': ORIGIN+'/dashboard', 'json': 'true'}, form=True)
        target = urllib.parse.urlsplit(result.get('url', ''))
        if target.scheme != 'http' or target.netloc != '127.0.0.1:3000' or target.path != '/dashboard' or target.query:
            raise ValueError('account login failed')
        user = self.request('/api/auth/session').get('user', {})
        if user.get('email') != EMAIL:
            raise ValueError('account login failed')
        return self.request('/api/user/profile')['id']


def apply(state, source, probe=inspect, client_factory=Client):
    value = load(state)
    if value is None:
        raise ValueError('account preparation is required before starting Compose')
    actual = probe(state, source)
    if value['ids'] is not None and actual['ids'] != value['ids']:
        raise ValueError('account identities changed')
    if value['kind'] == 'fresh':
        if value['phase'] == 'pending':
            if not actual['fresh']:
                raise ValueError('fresh account bootstrap conflicts with existing user state')
            # Save identity before rotation. A lost HTTP response or interrupted write
            # resumes with the same private password and verifies it before completion.
            value['ids'] = actual['ids']
            write(state/'account.json', value)
            client = client_factory()
            password = credentials(state)['password']
            if actual['stock']:
                if client.login('openlituser') != actual['ids']['user']:
                    raise ValueError('seed account identity changed')
                client.request('/api/user/profile', {'currentPassword': 'openlituser', 'newPassword': password})
                client = client_factory()
            if client.login(password) != actual['ids']['user']:
                raise ValueError('operator account identity changed')
            actual = probe(state, source)
        if actual['stock'] or actual['ids'] != value['ids']:
            raise ValueError('private operator account is not verified')
    value.update(phase='ready', ids=actual['ids'])
    write(state/'account.json', value)
    return value


def check(state, source, probe=linkage):
    value = load(state)
    if value is None or value['phase'] != 'ready':
        raise ValueError('account bootstrap is incomplete')
    actual = probe(state, source)
    if actual['ids'] != value['ids'] or (value['kind'] == 'fresh' and actual['stock']):
        raise ValueError('account readiness changed')
    return value


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('mode', choices=('prepare', 'apply', 'check'))
    parser.add_argument('--state', type=Path, required=True)
    parser.add_argument('--source', type=Path, required=True)
    args = parser.parse_args()
    try:
        if os.getuid() != 0:
            raise ValueError('guest root required')
        if args.mode == 'check':
            # Only runtime readiness calls this mode, after checking the exact
            # container's ownership, image, configuration, mounts and health.
            identity = sys.stdin.buffer.read(65)
            if not re.fullmatch(b'[a-f0-9]{64}', identity):
                raise ValueError('invalid verified account container')
            value = check(args.state, args.source, lambda state, source: linkage(state, source, identity.decode('ascii')))
        else:
            value = prepare(args.state) if args.mode == 'prepare' else apply(args.state, args.source)
        print(json.dumps({'kind': value['kind'], 'phase': value['phase']}))
    except (ValueError, OSError, KeyError, TypeError, subprocess.TimeoutExpired):
        print('openlit: unsafe or incomplete account bootstrap; retain private state and retry', file=sys.stderr)
        sys.exit(1)


if __name__ == '__main__':
    main()
