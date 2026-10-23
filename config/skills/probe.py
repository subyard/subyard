"""Read-only guest probe against source bytes captured by the owner runtime."""
import base64
import json
import os
from pathlib import Path
import pwd
import sys

request = json.load(sys.stdin)
files = {path: base64.b64decode(data) for path, data in request['files'].items()}
namespace = {'__name__': 'subyard_skill_probe'}
exec(compile(files['config/skills/deliver.py'], 'subyard-agent-skills', 'exec'), namespace)


def main():
    helper = Path('/usr/local/libexec/subyard/agent-skills')
    if helper.is_symlink() or not helper.is_file() or not namespace['metadata'](helper,0o755,0,0) or helper.read_bytes() != files['config/skills/deliver.py']:
        return False
    providers = {'core': {}}
    for path, data in files.items():
        parts = Path(path).parts
        if path.startswith('config/skills/core/'):
            providers['core'].setdefault(parts[3], {})['/'.join(parts[4:])] = data
        if len(parts) > 5 and parts[:2] == ('config','profiles') and parts[3] == 'skills':
            providers.setdefault('profile-'+parts[2], {}).setdefault(parts[4], {})['/'.join(parts[5:])] = data
        if path.startswith('config/profiles/') and parts[-1] == 'skills.json':
            package = str(Path(path).parent)
            providers['profile-'+parts[2]] = {name: {relative: files[package+'/'+origin] for relative, origin in declarations.items()} for name,declarations in json.loads(data).items()}
    namespace['sources'] = lambda source: providers[source]
    user = pwd.getpwnam(request['user'])
    selected = request['profiles'].split()
    args = type('Args', (), dict(root=Path('/'), home=Path(user.pw_dir), uid=user.pw_uid, gid=user.pw_gid, provider='core', mode='check' if request['coding'] else 'check-removed', source='core', registered=False, extra=False))()
    if namespace['delivery'](args):
        return False
    for receipt in Path('/var/lib/subyard/agent-skills').glob('profile-*.json'):
        args.provider = receipt.stem
        args.source = args.provider
        args.registered = True
        args.mode = 'check' if args.provider.removeprefix('profile-') in selected else 'check-removed'
        if namespace['delivery'](args):
            return False
    return True


try:
    print('skills-current' if main() else 'skills-stale')
except (OSError, ValueError, KeyError, TypeError):
    print('skills-stale')
