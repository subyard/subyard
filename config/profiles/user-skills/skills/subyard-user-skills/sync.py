#!/usr/bin/env python3
"""Clone configured skill sources once and link their skills for agent harnesses.

Runs as the developer user. Subyard never updates, rewrites or deletes a clone;
it owns only `sources.json` and links whose target is exactly a skill directory
inside its root. Exit codes: 0 done, 2 unsafe or invalid, 3 done but a source is
unavailable, 4 another sync holds the lock.
"""

import argparse
import ctypes
import errno
import fcntl
import hashlib
import json
import os
import re
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from urllib.parse import urlsplit

# Harness directories of core skill delivery (config/skills/deliver.py).
ADAPTERS = ('.agents/skills', '.claude/skills', '.config/opencode/skills')
EXTRA = ('.pi/agent/skills', '.hermes/skills')
ROOT = '.local/share/subyard-user-skills'
CANONICAL = Path('/usr/local/share/subyard/skills')
GITHUB_WRAPPER = Path('/usr/local/bin/subyard-github')
SAFE_PATH = '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
LOCK_WAIT = 330
ATTEMPT_TIMEOUT = 60
SYNC_BUDGET = 300
MAX_SOURCES = 8
# Host-free tests use local bare repositories; production refuses local paths.
ALLOW_LOCAL = False

SKILL_NAME = re.compile(r'[a-z0-9]+(?:-[a-z0-9]+)*')
SOURCE_NAME = re.compile(r'[a-z0-9]+(?:-[a-z0-9]+)*-[0-9a-f]{8}')
URL_CHARACTERS = re.compile(r'[A-Za-z0-9._~:/@%+-]+')
SCP_LIKE = re.compile(r'([A-Za-z0-9._-]+)@([A-Za-z0-9.-]+):([^/:][^:]*|/[^:]+)')
HOST = re.compile(r'[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?')
DEFAULT_PORTS = {'https': 443, 'ssh': 22}


class Unsafe(Exception):
    pass


class Busy(Exception):
    pass


def _trim(path):
    return path.rstrip('/').removesuffix('.git')


def normalize(url):
    """Mirror of domain.NormalizeRemoteURL; refusals never echo the value."""
    if ALLOW_LOCAL and (url.startswith('/') or url.startswith('file:///')):
        return url
    if url.startswith('-'):
        raise ValueError("starts with '-'")
    if '?' in url or '#' in url:
        raise ValueError('has a query or fragment')
    if not URL_CHARACTERS.fullmatch(url):
        raise ValueError('contains unsupported characters')
    match = SCP_LIKE.fullmatch(url)
    if match and '://' not in url:
        if not HOST.fullmatch(match[2]):
            raise ValueError('has an invalid host')
        path = _trim(match[3])
        if path in ('', '/'):
            raise ValueError('has no repository path')
        return match[1] + '@' + match[2].lower() + ':' + path
    if '://' not in url:
        raise ValueError('is not an https, ssh or user@host:path URL')
    parts = urlsplit(url)
    scheme = parts.scheme.lower()
    if scheme not in DEFAULT_PORTS:
        raise ValueError('uses an unsupported scheme')
    user = None
    if '@' in parts.netloc:
        if parts.password is not None or scheme == 'https' or not parts.username:
            raise ValueError('contains credentials')
        user = parts.username
    host = (parts.hostname or '').lower()
    if not HOST.fullmatch(host):
        raise ValueError('has an invalid host')
    if parts.netloc.endswith(':'):
        raise ValueError('has an invalid port')
    try:
        port = parts.port
    except ValueError:
        raise ValueError('has an invalid port') from None
    path = _trim(parts.path)
    if path in ('', '/'):
        raise ValueError('has no repository path')
    authority = host if port in (None, DEFAULT_PORTS[scheme]) else f'{host}:{port}'
    if user is not None:
        authority = user + '@' + authority
    return f'{scheme}://{authority}{path}'


def source_name(normalized):
    base = re.split(r'[/:]', normalized)[-1].lower()
    base = re.sub(r'[^a-z0-9]+', '-', base).strip('-')[:40].strip('-') or 'source'
    return base + '-' + hashlib.sha256(normalized.encode()).hexdigest()[:8]


def sources_for(urls, root):
    """Validate a configured list; every refusal is reported by position."""
    problems, result, seen = [], [], {}
    if len(urls) > MAX_SOURCES:
        problems.append(f'more than {MAX_SOURCES} sources')
    for index, url in enumerate(urls, 1):
        try:
            normalized = normalize(url)
        except ValueError as error:
            problems.append(f'source {index} {error}')
            continue
        if normalized in seen:
            problems.append(f'source {index} repeats source {seen[normalized]}')
            continue
        seen[normalized] = index
        name = source_name(normalized)
        result.append({'url': url, 'normalized': normalized, 'name': name, 'dir': str(root / name)})
    if problems:
        raise Unsafe('invalid sources: ' + '; '.join(problems))
    return result


def encoded(sources):
    return (json.dumps(sources, indent=2) + '\n').encode()


def home_directory():
    home = os.environ.get('HOME', '')
    if not home.startswith('/'):
        raise Unsafe('developer home is missing')
    try:
        status = os.stat(home)
    except FileNotFoundError:
        raise Unsafe('developer home is missing') from None
    if not stat.S_ISDIR(status.st_mode):
        raise Unsafe('developer home is missing')
    return Path(home)


def skill_root(home, create):
    root = home / ROOT
    try:
        status = os.lstat(root)
    except FileNotFoundError:
        if not create:
            return root
        os.makedirs(root.parent, 0o755, exist_ok=True)
        os.mkdir(root, 0o755)
        status = os.lstat(root)
    if not stat.S_ISDIR(status.st_mode) or status.st_uid != os.geteuid():
        raise Unsafe('skill source root is not a directory owned by the developer user')
    if create:
        os.chmod(root, 0o755)
    return root


def read_sources(root):
    path = root / 'sources.json'
    if not os.path.lexists(path):
        return []
    if not stat.S_ISREG(os.lstat(path).st_mode):
        raise Unsafe('sources.json is not a regular file')
    try:
        records = json.loads(path.read_bytes())
        urls = [record['url'] for record in records]
    except (ValueError, TypeError, KeyError) as error:
        raise Unsafe('sources.json is invalid') from error
    return sources_for(urls, root)


def write_sources(root, sources):
    descriptor, temporary = tempfile.mkstemp(prefix='.sources-', dir=root)
    try:
        with os.fdopen(descriptor, 'wb') as stream:
            stream.write(encoded(sources))
            os.fchmod(stream.fileno(), 0o644)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, root / 'sources.json')
    except BaseException:
        Path(temporary).unlink(missing_ok=True)
        raise
    directory = os.open(root, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(directory)
    finally:
        os.close(directory)


def owned(link, root):
    """A link is ours only when it targets exactly ROOT/<source>/skills/<its name>."""
    try:
        target = os.readlink(link)
    except OSError:
        return False
    prefix = str(root) + '/'
    if not target.startswith(prefix):
        return False
    parts = target[len(prefix):].split('/')
    return len(parts) == 3 and SOURCE_NAME.fullmatch(parts[0]) is not None and parts[1] == 'skills' and parts[2] == link.name


def reserved(name):
    return name.startswith('subyard-') or os.path.lexists(CANONICAL / name)


def harness_directories(home):
    """Usable harness directories plus a skip report for unusable ones."""
    usable, skipped = [], []
    for adapter in ADAPTERS + EXTRA:
        path = home / adapter
        parent = path
        unsafe = False
        while parent != home:
            if os.path.islink(parent) or (os.path.lexists(parent) and not parent.is_dir()):
                unsafe = True
                break
            parent = parent.parent
        if unsafe:
            skipped.append(f'skipped {path}: not a real directory')
        else:
            usable.append(path)
    return usable, skipped


def inspect(home, root, sources):
    """Plan without effects: missing clones, wanted links, removals and skips."""
    missing, wanted, skipped = [], {}, []
    for source in sources:
        clone = Path(source['dir'])
        if not os.path.lexists(clone):
            missing.append(source)
            continue
        if os.path.islink(clone) or not clone.is_dir():
            skipped.append(f"skipped {clone}: not a directory")
            continue
        skills = clone / 'skills'
        if os.path.islink(skills) or not skills.is_dir():
            continue
        for entry in sorted(skills.iterdir()):
            name = entry.name
            manifest = entry / 'SKILL.md'
            if (os.path.islink(entry) or not entry.is_dir() or not SKILL_NAME.fullmatch(name)
                    or not os.path.lexists(manifest) or not stat.S_ISREG(os.lstat(manifest).st_mode)):
                skipped.append(f'skipped {entry}: not a valid skill directory')
            elif reserved(name):
                skipped.append(f'skipped {entry}: reserved skill name')
            elif name in wanted:
                skipped.append(f'skipped {entry}: skill name already provided by an earlier source')
            else:
                wanted[name] = str(entry)
    harnesses, unusable = harness_directories(home)
    skipped += unusable
    remove, create = [], []
    for directory in harnesses:
        if directory.is_dir():
            for link in sorted(directory.iterdir()):
                if os.path.islink(link) and owned(link, root) and os.readlink(link) != wanted.get(link.name):
                    remove.append(link)
        for name, target in wanted.items():
            link = directory / name
            if not os.path.lexists(link) or link in remove:
                create.append((link, target))
            elif not os.path.islink(link) or os.readlink(link) != target:
                skipped.append(f'skipped {link}: path is occupied')
    return {'missing': missing, 'wanted': wanted, 'remove': remove, 'create': create, 'skipped': skipped}


def run_bounded(command, home, cwd, timeout):
    """Run one git command in its own process group; the group never outlives it."""
    process = subprocess.Popen(command, cwd=cwd, env={'HOME': str(home), 'PATH': SAFE_PATH},
                               stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                               stderr=subprocess.DEVNULL, start_new_session=True)
    try:
        return process.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        return None
    finally:
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.wait()


def remove_tree(path, report):
    def failed(_function, failed_path, error):
        error = error[1] if isinstance(error, tuple) else error
        report.append(f'cleanup {failed_path}: {error.strerror or error}')
    if sys.version_info >= (3, 12):
        shutil.rmtree(path, onexc=failed)
    else:
        shutil.rmtree(path, onerror=failed)


def rename_noreplace(source, target):
    libc = ctypes.CDLL(None, use_errno=True)
    renameat2 = getattr(libc, 'renameat2', None)
    if renameat2 is not None:
        at_fdcwd, noreplace = -100, 1
        if renameat2(at_fdcwd, os.fsencode(source), at_fdcwd, os.fsencode(target), noreplace) == 0:
            return
        code = ctypes.get_errno()
        if code not in (errno.EINVAL, errno.ENOSYS):
            raise OSError(code, os.strerror(code), str(target))
    if os.path.lexists(target):
        raise FileExistsError(errno.EEXIST, 'target exists', str(target))
    os.rename(source, target)


def clone(source, home, root, deadline, report):
    git = shutil.which('git', path=SAFE_PATH) or 'git'
    attempts = [[git, 'clone', '--']]
    if source['normalized'].startswith('https://github.com/') and os.access(GITHUB_WRAPPER, os.X_OK):
        attempts.append([str(GITHUB_WRAPPER), 'run', '--', 'git', 'clone', '--'])
    target = Path(source['dir'])
    for prefix in attempts:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            break
        temporary = Path(tempfile.mkdtemp(prefix=f".tmp-{source['name']}-", dir=root))
        try:
            if run_bounded(prefix + [source['url'], str(temporary)], home, root, min(ATTEMPT_TIMEOUT, remaining)) == 0:
                os.chmod(temporary, 0o755)
                rename_noreplace(temporary, target)
                return True
        except FileExistsError:
            return True
        finally:
            if os.path.lexists(temporary):
                remove_tree(temporary, report)
    return False


class Lock:
    def __init__(self, root):
        self.root = root

    def __enter__(self):
        path = self.root / '.lock'
        self.descriptor = os.open(path, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW | os.O_CLOEXEC, 0o644)
        deadline = time.monotonic() + LOCK_WAIT
        while True:
            try:
                fcntl.flock(self.descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
                return self
            except BlockingIOError:
                if time.monotonic() >= deadline:
                    os.close(self.descriptor)
                    raise Busy('another user-skills sync holds the lock; retry') from None
                time.sleep(0.2)

    def __exit__(self, *_):
        os.close(self.descriptor)


def synchronize(home, root, sources, out):
    report = []
    for leftover in sorted(root.glob('.tmp-*')):
        if os.path.isdir(leftover) and not os.path.islink(leftover):
            remove_tree(leftover, report)
    deadline = time.monotonic() + SYNC_BUDGET
    unavailable = []
    for source in inspect(home, root, sources)['missing']:
        if clone(source, home, root, deadline, report):
            report.append(f"cloned {source['name']}")
        else:
            unavailable.append(source['name'])
            report.append(f"unavailable {source['name']}")
    plan = inspect(home, root, sources)
    for link in plan['remove']:
        link.unlink()
        report.append(f'removed link {link}')
    for link, target in plan['create']:
        os.makedirs(link.parent, 0o755, exist_ok=True)
        os.symlink(target, link)
        report.append(f'linked {link}')
    report += plan['skipped']
    for line in report:
        print(line, file=out)
    return 3 if unavailable else 0


def unlink_owned(home, root, out):
    harnesses, _ = harness_directories(home)
    for directory in harnesses:
        if directory.is_dir():
            for link in sorted(directory.iterdir()):
                if os.path.islink(link) and owned(link, root):
                    link.unlink()
                    print(f'removed link {link}', file=out)
    (root / 'sources.json').unlink(missing_ok=True)
    return 0


def check(home, root, urls, out):
    sources = sources_for(urls, root)
    path = root / 'sources.json'
    plan = inspect(home, root, sources)
    if urls:
        current = os.path.isfile(path) and not os.path.islink(path) and path.read_bytes() == encoded(sources)
    else:
        current = not os.path.lexists(path)
    if not current or plan['remove'] or plan['create']:
        print('work', file=out)
    elif plan['missing']:
        print('unavailable', file=out)
    else:
        print('current', file=out)
    return 0


def list_sources(home, root, out):
    sources = read_sources(root) if os.path.isdir(root) else []
    if not sources:
        print('no skill sources configured', file=out)
        return 0
    plan = inspect(home, root, sources)
    for source in sources:
        state = 'missing' if source in plan['missing'] else 'present'
        print(f"{source['url']}\n  dir: {source['dir']} ({state})", file=out)
        for name, target in sorted(plan['wanted'].items()):
            if target.startswith(source['dir'] + '/'):
                print(f'  skill: {name}', file=out)
    for line in plan['skipped']:
        print(line, file=out)
    if plan['remove'] or plan['create']:
        print('links need a sync', file=out)
    return 0


def main(argv=None, out=sys.stdout):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument('command', choices=('apply', 'sync', 'list', 'check', 'unlink'))
    parser.add_argument('urls', nargs='*')
    args = parser.parse_args(argv)
    if args.urls and args.command not in ('apply', 'check'):
        parser.error('only apply and check take source URLs')
    try:
        home = home_directory()
        if args.command == 'check':
            return check(home, skill_root(home, False), args.urls, out)
        if args.command == 'list':
            return list_sources(home, skill_root(home, False), out)
        if args.command == 'unlink' and not os.path.lexists(skill_root(home, False)):
            return unlink_owned(home, home / ROOT, out)
        root = skill_root(home, True)
        with Lock(root):
            if args.command == 'apply':
                sources = sources_for(args.urls, root)
                write_sources(root, sources)
                return synchronize(home, root, sources, out)
            if args.command == 'sync':
                return synchronize(home, root, read_sources(root), out)
            return unlink_owned(home, root, out)
    except Busy as error:
        print('user skills: ' + str(error), file=sys.stderr)
        return 4
    except (Unsafe, OSError) as error:
        print('user skills: ' + str(error), file=sys.stderr)
        return 2


if __name__ == '__main__':
    raise SystemExit(main())
