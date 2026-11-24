#!/usr/bin/env python3
"""Internal init hook: clone missing skill repositories and link their skills."""
import ctypes
import errno
import functools
import hashlib
import os
from pathlib import Path
import re
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import time

ADAPTERS = ('.agents/skills', '.claude/skills', '.config/opencode/skills',
            '.pi/agent/skills', '.hermes/skills')
ROOT = '.local/share/subyard-user-skills'
CANONICAL = Path('/usr/local/share/subyard/skills')
GITHUB_WRAPPER = Path('/usr/local/bin/subyard-github')
SAFE_PATH = '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
ATTEMPT_TIMEOUT = 60
CLONE_BUDGET = 300
SKILL_NAME = re.compile(r'[a-z0-9]+(?:-[a-z0-9]+)*')
SOURCE_NAME = re.compile(r'[a-z0-9]+(?:-[a-z0-9]+)*-[0-9a-f]{8}')
TEMP_NAME = re.compile(r'\.tmp-' + SOURCE_NAME.pattern + r'-[a-z0-9_]{8}')


def source_name(normalized):
    base = re.split(r'[/:]', normalized)[-1].lower()
    base = re.sub(r'[^a-z0-9]+', '-', base).strip('-')[:40].strip('-') or 'source'
    return base + '-' + hashlib.sha256(normalized.encode()).hexdigest()[:8]


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
    for adapter in ADAPTERS:
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


def temporary_clones(root):
    if not root.is_dir():
        return []
    # Init and scoped profile operations already hold the core's per-yard lock.
    return [path for path in sorted(root.iterdir()) if TEMP_NAME.fullmatch(path.name)
            and stat.S_ISDIR((info := path.lstat()).st_mode) and info.st_uid == os.geteuid()]


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
        if not any(clone.iterdir()):
            missing.append(source)
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
    return {'missing': missing, 'wanted': wanted, 'remove': remove, 'create': create, 'skipped': skipped,
            'temporary': temporary_clones(root)}


def _die_with_parent(parent):
    # PR_SET_PDEATHSIG: a killed helper takes its Git child with it. A helper that
    # died before prctl took effect leaves a reparented child, which exits at once.
    libc = ctypes.CDLL(None, use_errno=True)
    libc.prctl(1, signal.SIGKILL, 0, 0, 0)
    if os.getppid() != parent:
        os._exit(1)


def _exit_on_signal(signum, _frame):
    # A repeated signal must not interrupt the unwinding that kills the Git group.
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
    signal.signal(signal.SIGHUP, signal.SIG_IGN)
    raise SystemExit(128 + signum)


def run_bounded(command, home, cwd, timeout):
    """Run one git command in its own process group; the group never outlives it."""
    process = subprocess.Popen(command, cwd=cwd, env={'HOME': str(home), 'PATH': SAFE_PATH},
                               stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                               stderr=subprocess.DEVNULL, start_new_session=True,
                               preexec_fn=functools.partial(_die_with_parent, os.getpid()))
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
    if source['url'].startswith('https://github.com/') and os.access(GITHUB_WRAPPER, os.X_OK):
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
                try:
                    target.rmdir()  # Only an empty target can be retired before publication.
                except OSError as error:
                    if error.errno not in (errno.ENOENT, errno.ENOTEMPTY, errno.EEXIST, errno.ENOTDIR):
                        raise
                rename_noreplace(temporary, target)
                return True
        except FileExistsError:
            return True
        finally:
            if os.path.lexists(temporary):
                remove_tree(temporary, report)
    return False


def run(mode, urls, home, post_apply=False):
    root = home / ROOT
    if os.path.lexists(root) and (os.path.islink(root) or not root.is_dir()
                                 or root.stat().st_uid != os.geteuid()):
        raise OSError('skill source root is not a directory owned by the developer')
    # URLs have already passed the profile setting's url-list validator.
    sources = [{'url': url, 'name': source_name(url), 'dir': str(root / source_name(url))}
               for url in urls]
    plan = inspect(home, root, sources)
    if mode == 'check':
        return int(bool(plan['remove'] or plan['create']
                        or ((plan['missing'] or plan['temporary']) and not post_apply)))
    report = []
    for temporary in plan['temporary']:
        remove_tree(temporary, report)
    if sources:
        root.mkdir(parents=True, exist_ok=True)
        deadline = time.monotonic() + CLONE_BUDGET
        for source in plan['missing']:
            if not clone(source, home, root, deadline, report):
                report.append(f"warning: source {source['name']} could not be cloned; authorize Git access "
                              "or correct USER_SKILL_SOURCES, then run yard init")
    plan = inspect(home, root, sources)
    for link in plan['remove']:
        link.unlink()
    for link, target in plan['create']:
        link.parent.mkdir(parents=True, exist_ok=True)
        link.symlink_to(target)
    for line in report + plan['skipped']:
        print('user-skills: ' + line, file=sys.stderr)
    return 0


def main():
    mode, post_apply, *urls = sys.argv[1:]
    try:
        code = run(mode, urls, Path(os.environ['HOME']), post_apply == '1')
    except OSError:
        print('user-skills: clone or link operation failed', file=sys.stderr)
        return 2
    if mode == 'check':
        print('work' if code else 'current')
        return 0
    return code


if __name__ == '__main__':
    signal.signal(signal.SIGTERM, _exit_on_signal)
    signal.signal(signal.SIGHUP, _exit_on_signal)
    raise SystemExit(main())
