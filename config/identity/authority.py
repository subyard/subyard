"""Read the fixed protected authority for one developer-identity owner."""
import json
import os
from pathlib import Path
import re
import stat

JOURNAL = Path('/var/lib/subyard/developer-identity/transition.json')
# Owner-authored refusals are fixed prose; anything else (parser errors,
# quoted values) could carry file contents and stays generic.
PUBLIC_REFUSAL = re.compile('[A-Za-z][A-Za-z ,;/-]{0,160}')


def refusal(owner, error):
    """Return the one stderr line core shows the operator for a refused owner."""
    if type(error) is ValueError and PUBLIC_REFUSAL.fullmatch(str(error)):
        return owner+' identity: '+str(error)
    return owner+' identity: owned state could not be verified; resolve it, then retry plain yard init'


def unique(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise ValueError('duplicate identity authority field')
        value[key] = item
    return value


def load(source, name, path=JOURNAL, machine=Path('/etc/machine-id'), owner=0, root=Path('/')):
    if not re.fullmatch('[0-9a-f]{64}', source):
        raise ValueError('invalid identity source authority')
    for parent in (path.parent, *path.parent.parents):
        if parent == root.parent:
            break
        info = parent.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != owner or info.st_mode & 0o022:
            raise ValueError('unsafe identity authority parent')
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600 or info.st_uid != owner or info.st_gid != os.getgid() or info.st_nlink != 1 or info.st_size > 1024*1024:
        raise ValueError('unsafe identity authority')
    value = json.loads(path.read_bytes(), object_pairs_hook=unique)
    if not isinstance(value, dict) or set(value) != {'schema', 'plan', 'phase'} or value['schema'] != 1 or value['phase'] not in ('prepared', 'quiesced', 'home', 'account', 'account-ready', 'ready', 'rebind', 'complete'):
        raise ValueError('invalid identity authority')
    plan = value['plan']
    if plan['source'] != source or plan['machine'] != machine.read_text().strip() or name not in plan.get('owners', {}):
        raise ValueError('foreign identity authority')
    recipe = plan['owners'][name]
    # Account records carry the shell for core postconditions; owners address
    # just the account identity and never authorize a shell change.
    expected_old = {key: plan['old'][key] for key in ('user', 'uid', 'gid', 'home')}
    expected_new = {key: plan['new'][key] for key in ('user', 'uid', 'gid', 'home')}
    if recipe['original'] != {**expected_old, 'group': plan['group']['name']} or recipe['target'] != {**expected_new, 'group': plan['new']['user']}:
        raise ValueError('identity owner scope changed')
    if plan['old']['uid'] != plan['new']['uid'] or plan['old']['gid'] != plan['new']['gid'] or plan['new']['home'] != '/home/'+plan['new']['user']:
        raise ValueError('identity numeric ownership changed')
    return value, recipe


def rebooted(value, root=Path('/')):
    """Only a native boot change permits enabled owned services to restart.

    It never changes the approved active intent or permits changed unit bytes.
    """
    before=value['plan'].get('boot')
    if before is None:return False
    after=(root/'proc/sys/kernel/random/boot_id').read_text().strip()
    pattern='[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}'
    if not re.fullmatch(pattern,before) or not re.fullmatch(pattern,after):
        raise ValueError('native boot identity unavailable')
    return before!=after


def active_allowed(active,expected,enabled,restarted=False):
    return active in ('inactive',expected) or (restarted and (active=='failed' or enabled=='enabled' and active=='active'))
