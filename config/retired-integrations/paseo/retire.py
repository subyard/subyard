#!/usr/bin/env python3
"""Remove a proven Paseo installation; leave and report anything else."""
import hashlib, json, os, shlex, subprocess, sys

PROJECT = os.environ['INCUS_PROJECT']
INSTANCE = os.environ['YARD_INSTANCE_NAME']
DEV_USER = os.environ.get('DEV_USER', 'dev')
STATE = '/srv/agents/paseo'
NOTICE_LIMIT = 440
UNIT_PATHS = ('/etc/systemd/system/paseo.service', '/run/systemd/system/paseo.service',
              '/usr/lib/systemd/system/paseo.service', '/lib/systemd/system/paseo.service')

# Runs as root in the guest. Ownership comes from the provisioning receipt, or, for
# releases before 0.15 that wrote none, from the exact bytes of a published template.
GUEST = r'''
import fcntl, glob, hashlib, json, os, pwd, re, stat, subprocess, sys
ROOT = ''
OWNER = 0
UNIT_PATH = '/etc/systemd/system/paseo.service'
UNIT_PATHS = ('/etc/systemd/system/paseo.service', '/run/systemd/system/paseo.service',
              '/usr/lib/systemd/system/paseo.service', '/lib/systemd/system/paseo.service')
UNIT = ROOT + UNIT_PATH
RECEIPT = ROOT + '/var/lib/subyard/paseo-ownership'
RUNTIME = ROOT + '/opt/subyard/paseo'
STATE = ROOT + '/srv/agents/paseo'
BIN = ROOT + '/usr/local/bin/'
RECEIPT_HEADER = '# subyard-paseo-ownership-v1'
UNIT_TEMPLATE = '7e99be3463d058f14234b1b11f1f942f7f2e542cc61252f30a2d52a7a46b5677'
HELPER_TEMPLATES = {
    'paseo': {'667ca70a94d091461f82cc5cc5f1d5036c9292dd7eda8986c52116e5d655e2ef'},
    'paseo-check': {'0121f3c8de4aa80c4ee3e38e7b8044e8da50c782997d152660b9035b801ce76c',
                    'f9d60f7897a377e0c6234a14aff4cdbe2818a8f21c8f360230625af6fd223d61',
                    '803c7c7acfc652fe83e501733287094f5b3043bd2d217c8ac1a8c4e5e37f6843'},
    'paseo-pair': {'67be0bac888cb708b4a4bff4fba3218067b2e0f9c5746338324f71a9438e5c7d'},
    'paseo-rotate-identity': {'36e3bd8f1eb2d7a3655e46eaadc10b0b90faa2d2aa22cb6c017d17060e3369f0'},
    'paseo-sync-projects': {'27e3f0daee797653c5fe6984b7c44b98ec63bcbf86e13b6bab421a10e3f9690f'},
}
RELEASE = re.compile(r'[0-9]+\.[0-9]+\.[0-9]+-[0-9a-f]{16}')
SCRATCH = re.compile(r'\.(candidate|replaced)\.[A-Za-z0-9]+')
CURRENT_SCRATCH = re.compile(r'\.current\.[A-Za-z0-9]+')

def sha256(data):
    return hashlib.sha256(data).hexdigest()

def safe_parents(path):
    current = os.path.dirname(path)
    while True:
        try:
            info = os.lstat(current)
        except FileNotFoundError:
            info = None
        # Missing ancestors contain nothing to prove or remove.
        if info is not None and (not stat.S_ISDIR(info.st_mode) or info.st_uid != OWNER or info.st_mode & 0o002):
            return False
        if current == (ROOT or '/'):
            return True
        current = os.path.dirname(current)

def read_owned(path, exact_mode=None):
    """Return None if absent, otherwise (safe, bytes)."""
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        return None
    if (not safe_parents(path) or not stat.S_ISREG(info.st_mode) or info.st_uid != OWNER
            or info.st_mode & 0o022 or info.st_nlink != 1
            or (exact_mode is not None and stat.S_IMODE(info.st_mode) != exact_mode)):
        return False, b''
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(descriptor, 'rb') as stream:
        return True, stream.read()

def unit_template_hash(data, user, home):
    # Reverse only the exact templated lines; any other change alters the hash.
    text = data
    for line, value, name in (('User=@\n', user, 'USER'), ('Group=@\n', user, 'USER'),
                              ('Environment=HOME=@\n', home, 'HOME'),
                              ('ReadWritePaths=@ /srv/agents/paseo /srv/workspaces\n', home, 'HOME')):
        text = text.replace(line.replace('@', value).encode(),
                            line.replace('@', '@DEV_' + name + '@').encode(), 1)
    return sha256(text)

def helper_template_hash(data, user, home):
    text = data
    for name, value in (('USER', user), ('HOME', home)):
        line = 'PASEO_DEV_%s="${PASEO_DEV_%s:-%%s}"\n' % (name, name)
        text = text.replace((line % value).encode(), (line % ('@DEV_' + name + '@')).encode(), 1)
    return sha256(text)

def inspect_receipt():
    content = read_owned(RECEIPT, 0o600)
    if content is None:
        return {'state': 'absent'}, ''
    safe, data = content
    lines = data.decode(errors='replace').split('\n')
    if (not safe or len(lines) != 3 or lines[0] != RECEIPT_HEADER or lines[2] != ''
            or not re.fullmatch('[0-9a-f]{64}  /etc/systemd/system/paseo.service', lines[1])):
        return {'state': 'invalid'}, ''
    return {'state': 'valid', 'sha256': sha256(data)}, lines[1][:64]

def loaded_unit():
    # Only an ownership decision reads this, so service activity stays outside the binding.
    output = subprocess.run(['systemctl', 'show', 'paseo.service', '--property=ActiveState,FragmentPath,LoadState'],
                            stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=30, check=True).stdout
    return dict(line.split('=', 1) for line in output.splitlines() if '=' in line)

def inspect_unit(receipt, recorded, user, home):
    loaded = loaded_unit()
    fragment = loaded.get('FragmentPath', '')
    foreign = {'state': 'foreign', 'fragment': fragment}
    if loaded.get('LoadState', 'not-found') != 'not-found' and fragment != UNIT_PATH:
        return foreign
    content = read_owned(UNIT)
    if content is None:
        # disable --now precedes the unlink, so a retry never finds its own unit still running.
        return foreign if loaded.get('ActiveState', 'inactive') not in ('inactive', 'failed') else {'state': 'absent'}
    # No release shipped a drop-in, and one can replace what this proven file runs.
    if any(glob.glob(ROOT + path + '.d/*.conf') for path in UNIT_PATHS):
        return foreign
    safe, data = content
    digest = sha256(data)
    if not safe or receipt['state'] == 'invalid':
        return {'state': 'foreign'}
    if receipt['state'] == 'valid':
        return {'state': 'proven' if digest == recorded else 'foreign', 'sha256': digest}
    return {'state': 'proven' if unit_template_hash(data, user, home) == UNIT_TEMPLATE else 'foreign', 'sha256': digest}

def inspect_helpers(user, home):
    helpers = {}
    for name, templates in HELPER_TEMPLATES.items():
        content = read_owned(BIN + name)
        if content is None:
            continue
        safe, data = content
        proven = safe and helper_template_hash(data, user, home) in templates
        helpers[name] = {'state': 'proven' if proven else 'unverified', 'sha256': sha256(data)}
    return helpers

def inspect_runtime():
    try:
        root = os.lstat(RUNTIME)
    except FileNotFoundError:
        return {'state': 'absent'}
    unverified = {'state': 'unverified'}
    if (not stat.S_ISDIR(root.st_mode) or root.st_uid != OWNER or root.st_mode & 0o022
            or not safe_parents(RUNTIME) or os.path.ismount(RUNTIME)):
        return unverified
    names = sorted(os.listdir(RUNTIME))
    if any(name not in ('releases', 'current') and not CURRENT_SCRATCH.fullmatch(name) for name in names):
        return unverified
    releases = RUNTIME + '/releases'
    entries = []
    if 'releases' in names:
        info = os.lstat(releases)
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != OWNER or info.st_mode & 0o022:
            return unverified
        entries = sorted(os.listdir(releases))
    for name in entries:
        path = releases + '/' + name
        info = os.lstat(path)
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != OWNER:
            return unverified
        if RELEASE.fullmatch(name):
            try:
                with open(path + '/manifest.json', 'rb') as stream:
                    if json.load(stream).get('kind') != 'paseo-headless':
                        return unverified
            except (OSError, ValueError, AttributeError):
                return unverified
        elif not SCRATCH.fullmatch(name):
            return unverified
    if 'current' in names:
        current = RUNTIME + '/current'
        if not stat.S_ISLNK(os.lstat(current).st_mode):
            return unverified
        target = os.readlink(current)
        if not target.startswith('releases/') or target[len('releases/'):] not in entries:
            return unverified
    return {'state': 'proven', 'entries': entries}

def worktrees_empty():
    # A worktree can hold work that exists nowhere else, so anything but a readable
    # plain directory without entries counts as holding one.
    path = STATE + '/worktrees'
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        return True
    except OSError:
        return False
    if not stat.S_ISDIR(info.st_mode) or os.path.ismount(path):
        return False
    try:
        return not os.listdir(path)
    except OSError:
        return False

def mount_inside_state():
    prefix = STATE.encode() + b'/'
    with open('/proc/self/mountinfo', 'rb') as stream:
        return any(line.split()[4] == STATE.encode() or line.split()[4].startswith(prefix) for line in stream)

def inspect_state(user, receipt, unit):
    """Delete only with receipt ownership; contents stay outside the binding."""
    try:
        info = os.lstat(STATE)
    except FileNotFoundError:
        return {'state': 'absent'}
    try:
        owners = (OWNER, pwd.getpwnam(user).pw_uid)
    except KeyError:
        owners = (OWNER,)
    try:
        removable = (receipt['state'] == 'valid' and unit['state'] != 'foreign' and stat.S_ISDIR(info.st_mode)
                     and info.st_uid in owners and not info.st_mode & 0o002 and safe_parents(STATE)
                     and not os.path.ismount(STATE) and not mount_inside_state() and worktrees_empty())
    except OSError:
        removable = False
    return {'state': 'remove' if removable else 'keep'}

def inspect(user):
    try:
        home = pwd.getpwnam(user).pw_dir
    except KeyError:
        home = '/home/' + user
    receipt, recorded = inspect_receipt()
    unit = inspect_unit(receipt, recorded, user, home)
    return {'unit': unit, 'receipt': receipt, 'helpers': inspect_helpers(user, home), 'runtime': inspect_runtime(),
            'state': inspect_state(user, receipt, unit)}

def systemctl(*arguments):
    subprocess.run(['systemctl', *arguments], stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                   stderr=subprocess.DEVNULL, timeout=90, check=True)

def apply(user, expected):
    lock = os.open(os.path.dirname(UNIT), os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    fcntl.flock(lock, fcntl.LOCK_EX)
    facts = inspect(user)
    if sha256(json.dumps(facts, sort_keys=True, separators=(',', ':')).encode()) != expected:
        sys.exit(3)
    if facts['unit']['state'] == 'foreign' or facts['receipt']['state'] == 'invalid':
        sys.exit(4)
    # The receipt goes last so an interrupted run keeps its proof for the retry.
    if facts['unit']['state'] == 'proven':
        systemctl('disable', '--now', 'paseo.service')
        os.unlink(UNIT)
    systemctl('daemon-reload')
    for name, helper in facts['helpers'].items():
        if helper['state'] == 'proven':
            os.unlink(BIN + name)
    if facts['runtime']['state'] == 'proven':
        subprocess.run(['rm', '-rf', '--one-file-system', '--', RUNTIME], stdin=subprocess.DEVNULL,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=300, check=True)
    # The daemon could add a worktree until it stopped, so decide again now.
    if facts['state']['state'] == 'remove' and inspect_state(user, facts['receipt'], {'state': 'absent'})['state'] == 'remove':
        subprocess.run(['rm', '-rf', '--one-file-system', '--', STATE], stdin=subprocess.DEVNULL,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=300, check=True)
    if facts['receipt']['state'] == 'valid':
        os.unlink(RECEIPT)

if sys.argv[1] == 'observe':
    print(json.dumps(inspect(sys.argv[2]), sort_keys=True, separators=(',', ':')))
else:
    apply(sys.argv[2], sys.argv[3])
'''


def guest(*arguments, timeout=30):
    return subprocess.run(['incus', 'exec', INSTANCE, '--project', PROJECT, '--', 'python3', '-B', '-c', GUEST, *arguments],
                          stdin=subprocess.DEVNULL, capture_output=True, timeout=timeout)


def digest(payload):
    return hashlib.sha256(payload).hexdigest()


def manual_command(fragment=''):
    # The path comes from the guest, so only a known unit location reaches this command.
    unit = fragment if fragment in UNIT_PATHS else UNIT_PATHS[0]
    script = (f'systemctl disable --now paseo.service; rm -f -- {unit} /var/lib/subyard/paseo-ownership; '
              'systemctl daemon-reload')
    return ' '.join(shlex.quote(part) for part in ('incus', 'exec', INSTANCE, '--project', PROJECT, '--', 'sh', '-c', script))


def state_command():
    return ' '.join(shlex.quote(part) for part in ('incus', 'exec', INSTANCE, '--project', PROJECT, '--', 'rm', '-rf', '--', STATE))


def bounded(notice, fallback):
    # An invalid notice would fail observation and so block the update.
    return notice if len(notice) <= NOTICE_LIMIT else fallback


def state_notice():
    return ('/srv/agents/paseo stays because it may hold worktrees. Removing it also deletes chats, schedules, loops and '
            'uploads; a compatible rollback must re-pair. Run on the owner host: ' + state_command())


def observe():
    result = guest('observe', DEV_USER)
    if result.returncode:
        raise ValueError('native-unavailable')
    raw = result.stdout.strip()
    facts = json.loads(raw)
    fallback = 'integration paseo is retired and left files in place; see yard integration status on the owner host.'
    if facts['unit']['state'] == 'foreign' or facts['receipt']['state'] == 'invalid':
        return {'state': 'absent', 'actual': '', 'desired': '',
                'notice': bounded('paseo.service is not proven Subyard-managed and stays. To remove it, run on the owner host: '
                                  + manual_command(facts['unit'].get('fragment', '')) + '; then run yard init.', fallback)}, raw
    helpers = facts['helpers']
    retained = sorted(name for name, helper in helpers.items() if helper['state'] != 'proven')
    if facts['runtime']['state'] == 'unverified':
        retained.append('/opt/subyard/paseo')
    if (facts['unit']['state'] == 'proven' or facts['receipt']['state'] == 'valid'
            or facts['runtime']['state'] == 'proven' or len(retained) != len(helpers) + (facts['runtime']['state'] == 'unverified')):
        if facts['state']['state'] == 'remove':
            notice = ('retire integration paseo: remove paseo.service, its helpers, /opt/subyard/paseo and /srv/agents/paseo, '
                      'including chats, schedules, loops and uploads; paired clients disconnect and a compatible rollback needs re-pairing.')
        elif facts['state']['state'] == 'keep':
            notice = 'retire integration paseo: remove paseo.service, its helpers and /opt/subyard/paseo. ' + state_notice()
        else:
            notice = 'retire integration paseo: remove paseo.service, its helpers and /opt/subyard/paseo; paired clients disconnect.'
        return {'state': 'stale', 'actual': digest(raw), 'desired': digest(json.dumps(
                    {'retired': True, 'source': os.environ.get('SUBYARD_INTEGRATION_SOURCE_BINDING', '')}, sort_keys=True).encode()),
                'notice': bounded(notice, 'retire integration paseo: remove what Subyard installed; see yard integration status.')}, raw
    observation = {'state': 'absent', 'actual': '', 'desired': ''}
    parts = [state_notice()] if facts['state']['state'] == 'keep' else []
    if retained:
        parts.append('Integration paseo is retired; unverified files stay in place: ' + ', '.join(retained) + '.')
    if parts:
        notice = ' '.join(parts)
        observation['notice'] = bounded(notice, parts[0] if len(parts[0]) <= NOTICE_LIMIT else fallback)
    return observation, raw


def main():
    observed, _ = observe()
    if sys.argv[1:] == ['observe']:
        print(json.dumps(observed))
        return
    if len(sys.argv) != 5 or sys.argv[1] != 'apply' or observed['state'] != 'stale' or [observed['actual'], observed['desired']] != sys.argv[3:]:
        raise ValueError('observation-changed')
    result = guest('apply', DEV_USER, sys.argv[3], timeout=900)
    if result.returncode:
        raise ValueError({3: 'observation-changed', 4: 'foreign-ownership'}.get(result.returncode, 'operation-failed'))
    verified, _ = observe()
    if verified['state'] != 'absent':
        raise ValueError('convergence-failed')
    print(json.dumps(verified))


if __name__ == '__main__':
    phase = 'apply' if sys.argv[1:2] == ['apply'] else 'inspect'
    try:
        main()
    except (ValueError, KeyError, TypeError, OSError, subprocess.SubprocessError) as error:
        reasons = ('native-unavailable', 'foreign-ownership', 'observation-changed', 'convergence-failed', 'operation-failed')
        reason = str(error) if str(error) in reasons else 'ambiguous-ownership'
        print(f'subyard-lifecycle-diagnostic-v1 {phase} {reason}', file=sys.stderr)
        sys.exit(1)
