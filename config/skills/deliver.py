#!/usr/bin/env python3
# subyard-agent-skills-v1
"""Guest-side package skill delivery. Receipts own exact files and client links."""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import tempfile

ADAPTERS = ('.agents/skills', '.claude/skills', '.config/opencode/skills')
SKILL_STAGING = '.subyard-skill-staging-'
# Earlier writers staged with mkstemp inside the skill directory itself.
OLD_STAGING = re.compile(r'\.skill-[a-z0-9_]{8}')
EXTRA = ('.pi/agent/skills', '.hermes/skills')


def digest(data):
    return hashlib.sha256(data).hexdigest()


def safe(path):
    for parent in (path, *path.parents):
        if parent.is_symlink():
            raise ValueError('symlinked managed parent: ' + str(parent))
        if parent.exists() and parent != path and not parent.is_dir():
            raise ValueError('non-directory managed parent: ' + str(parent))


def mkdir(path, mode, uid, gid):
    safe(path)
    if path.exists():
        if not path.is_dir():
            raise ValueError('managed directory collision: ' + str(path))
        return
    missing = []
    current = path
    while not current.exists():
        missing.append(current)
        current = current.parent
    for current in reversed(missing):
        current.mkdir(mode=mode)
        current.chmod(mode)
        os.chown(current, uid, gid)


def write(path, data, mode, uid, gid):
    """Replace a receipt or instruction file; stages next to it."""
    stage(path, data, mode, uid, gid, path.parent, '.subyard-staging-')


def write_skill(canonical, path, data, uid, gid):
    """Replace managed skill content; stages in the canonical root, outside skill
    directories, so a leftover from a killed run never becomes unmanaged content."""
    if canonical not in path.parents:
        raise ValueError('skill content outside canonical root: ' + str(path))
    stage(path, data, 0o644, uid, gid, canonical, SKILL_STAGING)


def stage(path, data, mode, uid, gid, staging_root, prefix):
    safe(path)
    # mkstemp creates a fresh random name with O_EXCL and never opens an existing file.
    fd, temporary = tempfile.mkstemp(prefix=prefix, dir=staging_root)
    try:
        with os.fdopen(fd, 'wb') as stream:
            os.fchmod(stream.fileno(), mode)
            os.fchown(stream.fileno(), uid, gid)
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def owned_file(path, uid):
    st = path.lstat()
    return stat.S_ISREG(st.st_mode) and st.st_uid == uid


def metadata(path, mode, uid, gid):
    st = path.lstat()
    return stat.S_ISREG(st.st_mode) and stat.S_IMODE(st.st_mode) == mode and (st.st_uid, st.st_gid) == (uid, gid)


def restore_metadata(path, mode, uid, gid, repair):
    """Restore the mode and group of our own root policy entry.

    Another owner, or group or other write that may have changed the entry, is left
    for the caller to refuse.
    """
    if not repair:
        return
    # Change the pinned inode, never a name that could be swapped meanwhile.
    fd = os.open(path, os.O_PATH | os.O_NOFOLLOW | os.O_CLOEXEC)
    try:
        st = os.fstat(fd)
        if (st.st_uid == uid and (stat.S_ISDIR(st.st_mode) or stat.S_ISREG(st.st_mode) and st.st_nlink == 1)
                and not stat.S_IMODE(st.st_mode) & 0o022 & ~mode
                and (stat.S_IMODE(st.st_mode), st.st_gid) != (mode, gid)):
            os.chown(f'/proc/self/fd/{fd}', uid, gid)
            os.chmod(f'/proc/self/fd/{fd}', mode)
    finally:
        os.close(fd)


def validate_sources(result):
    for name, files in result.items():
        if not re.fullmatch(r'[a-z0-9]+(?:-[a-z0-9]+)*', name):
            raise ValueError('invalid skill name')
        text = files.get('SKILL.md', b'')
        if not text.startswith(b'---\nname: ' + name.encode() + b'\n') or b'\ndescription: ' not in text.split(b'\n---',1)[0]:
            raise ValueError('invalid skill metadata')
    return result


def sources(source):
    result = {}
    safe(source)
    if source.is_file():
        manifest = json.loads(source.read_bytes())
        for name, files in manifest.items():
            result[name] = {}
            for relative, origin in files.items():
                if Path(relative).is_absolute() or ".." in Path(relative).parts or Path(origin).is_absolute() or ".." in Path(origin).parts:
                    raise ValueError("invalid skill manifest path")
                path = source.parent / origin
                safe(path)
                result[name][relative] = path.read_bytes()
        return validate_sources(result)
    if not source.is_dir():
        raise ValueError('skill source missing: ' + str(source))
    for skill in sorted(source.iterdir()):
        if not re.fullmatch(r'[a-z0-9]+(?:-[a-z0-9]+)*', skill.name) or not skill.is_dir() or skill.is_symlink():
            raise ValueError('invalid skill source')
        files = {}
        for path in sorted(skill.rglob('*')):
            if path.is_symlink() or not (path.is_file() or path.is_dir()):
                raise ValueError('invalid skill source file')
            if path.is_file():
                files[str(path.relative_to(skill))] = path.read_bytes()
        result[skill.name] = files
    return validate_sources(result)


def delivery(args):
    state = args.root / 'var/lib/subyard/agent-skills'
    canonical = args.root / 'usr/local/share/subyard/skills'
    receipt_path = state / (args.provider + '.json')
    owner = os.geteuid()
    old = {'schema': 1, 'skills': {}, 'extra': False}
    safe(receipt_path)
    repair = not args.mode.startswith('check')
    if state.exists():
        restore_metadata(state, 0o700, owner, os.getegid(), repair)
        st = state.stat()
        if (st.st_uid,st.st_gid) != (owner,os.getegid()) or stat.S_IMODE(st.st_mode) != 0o700:
            raise ValueError('unsafe skill receipt directory')
    if receipt_path.exists():
        restore_metadata(receipt_path, 0o600, owner, os.getegid(), repair)
        if not metadata(receipt_path, 0o600, owner, os.getegid()):
            raise ValueError('unsafe skill receipt')
        old = json.loads(receipt_path.read_bytes())
        if old.get('schema') != 1 or old.get('home') != str(args.home):
            raise ValueError('invalid skill receipt')
    if args.mode == 'check-registered':
        if not receipt_path.exists():
            return 10
        skills = old.get('skills')
        if not isinstance(skills, dict) or not skills:
            raise ValueError('invalid provider registration')
        for name, files in skills.items():
            if not re.fullmatch(r'[a-z0-9]+(?:-[a-z0-9]+)*', name) or not isinstance(files,dict) or 'SKILL.md' not in files:
                raise ValueError('invalid provider registration')
            for relative, fingerprint in files.items():
                if Path(relative).is_absolute() or '..' in Path(relative).parts or not isinstance(fingerprint,str) or not re.fullmatch('[0-9a-f]{64}',fingerprint):
                    raise ValueError('invalid provider registration')
        return 0
    safe(canonical)
    if canonical.exists():
        restore_metadata(canonical, 0o755, owner, os.getegid(), repair)
        st = canonical.stat()
        if not stat.S_ISDIR(st.st_mode) or (st.st_uid,st.st_gid) != (owner,os.getegid()) or stat.S_IMODE(st.st_mode) != 0o755:
            raise ValueError('unsafe canonical skill root')
    if args.registered and not receipt_path.exists():
        return 0
    wanted = {} if args.mode in ('remove', 'check-removed') else sources(args.source)
    extra = args.extra or old.get('extra', False)
    desired = {'schema': 1, 'home': str(args.home), 'extra': extra,
               'skills': {name: {p: digest(b) for p, b in files.items()} for name, files in wanted.items()}}
    adapters = ADAPTERS + (EXTRA if extra else ())
    names = set(old['skills']) | set(wanted)
    for receipt in state.glob('*.json'):
        if receipt != receipt_path:
            safe(receipt)
            if set(json.loads(receipt.read_bytes())['skills']) & set(wanted):
                raise ValueError('skill name already belongs to another provider')
    changed = old != desired if wanted else bool(old['skills'])
    leftovers = []
    # Validate the whole operation before changing any artifacts.
    for name in names:
        if not re.fullmatch(r'[a-z0-9]+(?:-[a-z0-9]+)*', name):
            raise ValueError('invalid receipt skill')
        directory = canonical / name
        safe(directory)
        known = set(old['skills'].get(name, {}))
        for relative in known | set(wanted.get(name, {})):
            if Path(relative).is_absolute() or '..' in Path(relative).parts:
                raise ValueError('invalid skill file path')
        if directory.exists():
            st = directory.stat()
            if (st.st_uid,st.st_gid) != (owner,os.getegid()) or st.st_mode & 0o022:
                raise ValueError('unsafe canonical skill directory')
            changed |= stat.S_IMODE(st.st_mode) != 0o755 or (st.st_uid,st.st_gid) != (owner,os.getegid())
            actual = set()
            for p in directory.rglob('*'):
                if p.is_dir() and not p.is_symlink():
                    continue
                # Owned regular files named like the old in-place staging file are
                # leftovers of a killed write; every directory is checked below.
                relative = str(p.relative_to(directory))
                if (OLD_STAGING.fullmatch(p.name) and relative not in known | set(wanted.get(name, {}))
                        and owned_file(p, owner)):
                    leftovers.append(p)
                    changed = True
                else:
                    actual.add(relative)
            # Legacy package installers used this explicit managed-content marker.
            legacy = name not in old['skills'] and actual == {'SKILL.md'} and b'<!-- subyard-managed-v1 -->' in (directory / 'SKILL.md').read_bytes()
            if (name not in old['skills'] or actual - known) and not legacy:
                raise ValueError('unmanaged skill content: ' + str(directory))
            for p in directory.rglob('*'):
                if p.is_dir() and not p.is_symlink():
                    st = p.stat()
                    if (st.st_uid,st.st_gid) != (owner,os.getegid()) or st.st_mode & 0o022:
                        raise ValueError('unsafe canonical skill directory')
                    changed |= stat.S_IMODE(st.st_mode) != 0o755 or (st.st_uid,st.st_gid) != (owner,os.getegid())
                if p.is_symlink():
                    raise ValueError('symlinked skill content')
        for adapter in adapters + ('.codex/skills',):
            link = args.home / adapter / name
            safe(link.parent)
            if os.path.lexists(link) and (not link.is_symlink() or os.readlink(link) != str(directory)):
                raise ValueError('unmanaged skill link: ' + str(link))
            expected = name in wanted and adapter in adapters
            changed |= os.path.lexists(link) != expected
            if expected and link.is_symlink():
                st = link.lstat()
                changed |= (st.st_uid,st.st_gid) != (args.uid,args.gid)
        for relative, data in wanted.get(name, {}).items():
            path = directory / relative
            changed |= not path.exists() or not metadata(path, 0o644, owner, os.getegid()) or path.read_bytes() != data
        changed |= bool(known - set(wanted.get(name, {})))
    if args.mode in ('check', 'check-removed'):
        return 10 if changed else 0
    mkdir(state, 0o700, owner, os.getegid())
    mkdir(canonical, 0o755, owner, os.getegid())
    # The delivery lock excludes any concurrent writer, so staging files that are
    # still present belong to killed runs.
    for path in leftovers + [p for p in canonical.glob(SKILL_STAGING + '*') if owned_file(p, owner)]:
        path.unlink()
    # Record the union before writes; retries recognize files installed just
    # before an interruption without claiming any unrecognized path.
    pending = dict(desired, skills={name: dict(old['skills'].get(name, {}), **desired['skills'].get(name, {})) for name in names})
    if names:
        write(receipt_path, (json.dumps(pending, sort_keys=True) + '\n').encode(), 0o600, owner, os.getegid())
    for name in sorted(names):
        directory = canonical / name
        for adapter in adapters + ('.codex/skills',):
            link = args.home / adapter / name
            if os.path.lexists(link) and (name not in wanted or adapter not in adapters):
                link.unlink()
        for relative in old['skills'].get(name, {}):
            path = directory / relative
            if relative not in wanted.get(name, {}) and path.exists():
                safe(path)
                path.unlink()
        if name in wanted:
            mkdir(directory, 0o755, owner, os.getegid())
            directory.chmod(0o755)
            os.chown(directory,owner,os.getegid())
            for relative, data in wanted[name].items():
                path = directory / relative
                mkdir(path.parent, 0o755, owner, os.getegid())
                for parent in (path.parent, *path.parent.parents):
                    if parent == canonical:
                        break
                    parent.chmod(0o755)
                    os.chown(parent,owner,os.getegid())
                if not path.exists() or not metadata(path, 0o644, owner, os.getegid()) or path.read_bytes() != data:
                    write_skill(canonical, path, data, owner, os.getegid())
            for adapter in adapters:
                link = args.home / adapter / name
                mkdir(link.parent, 0o755, args.uid, args.gid)
                if not os.path.lexists(link):
                    link.symlink_to(directory)
                os.chown(link, args.uid, args.gid, follow_symlinks=False)
        elif directory.exists():
            for child in sorted(directory.rglob('*'), reverse=True):
                if child.is_dir():
                    child.rmdir()
            directory.rmdir()
    if wanted:
        write(receipt_path, (json.dumps(desired, sort_keys=True) + '\n').encode(), 0o600, owner, os.getegid())
    elif receipt_path.exists():
        receipt_path.unlink()
    return 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('mode', choices=('apply', 'check', 'remove', 'check-removed', 'check-registered'))
    parser.add_argument('--provider', required=True)
    parser.add_argument('--source', type=Path)
    parser.add_argument('--home', required=True, type=Path)
    parser.add_argument('--uid', required=True, type=int)
    parser.add_argument('--gid', required=True, type=int)
    parser.add_argument('--root', type=Path, default=Path('/'))
    parser.add_argument('--registered', action='store_true', help='refresh only a successfully provisioned provider')
    parser.add_argument('--extra', action='store_true', help='retain pi and Hermes adapters for an existing provider')
    args = parser.parse_args()
    if not re.fullmatch(r'[a-z0-9_-]+', args.provider) or not args.home.is_absolute():
        parser.error('invalid provider or home')
    if args.mode in ('apply', 'check') and args.source is None:
        parser.error('source required')
    # Serialize receipts and links across concurrent profile/static refreshes.
    state = args.root / 'var/lib/subyard'
    if args.mode in ('apply', 'remove'):
        mkdir(state, 0o755, os.geteuid(), os.getegid())
        lock = state / '.agent-skills.lock'
        safe(lock)
        fd = os.open(lock, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX)
            return delivery(args)
        finally:
            os.close(fd)
    return delivery(args)


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except (OSError, ValueError, KeyError, TypeError) as error:
        print('agent skills: ' + str(error), file=__import__('sys').stderr)
        raise SystemExit(1)
