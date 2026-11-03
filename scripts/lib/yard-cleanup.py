#!/usr/bin/env python3
"""Delete only approved directory contents, without following links or mounts."""
import errno
import json
import os
from pathlib import Path
import stat
import sys

def mount_id(fd):
    for line in Path('/proc/self/fdinfo/' + str(fd)).read_text().splitlines():
        if line.startswith('mnt_id:'):
            return int(line.split()[1])
    raise ValueError('mount identity is unavailable')


def open_directory(path):
    current = os.open('/', os.O_RDONLY | os.O_DIRECTORY)
    try:
        for component in Path(path).parts[1:]:
            child = os.open(component, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=current)
            os.close(current)
            current = child
        return current
    except BaseException:
        os.close(current)
        raise


def identity(fd):
    info = os.fstat(fd)
    return dict(device=info.st_dev, inode=info.st_ino, mode=info.st_mode,
                uid=info.st_uid, gid=info.st_gid, mount=mount_id(fd))


def mount_path(fd):
    wanted = str(mount_id(fd))
    for line in Path('/proc/self/mountinfo').read_text().splitlines():
        fields = line.split()
        if fields[0] == wanted:
            return fields[4]
    raise ValueError('mount ownership is unavailable')


def assess(paths, allowed_mounts=('/', '/srv')):
    result = []
    for path in paths:
        try:
            fd = open_directory(path)
        except FileNotFoundError:
            result.append(dict(path=path, identity=None))
            continue
        try:
            if mount_path(fd) not in allowed_mounts:
                raise ValueError('cache is on an external or nested mount: ' + path)
            result.append(dict(path=path, identity=identity(fd)))
        finally:
            os.close(fd)
    return result


def remove_contents(fd, root_mount, report):
    for name in os.listdir(fd):
        try:
            info = os.stat(name, dir_fd=fd, follow_symlinks=False)
            if stat.S_ISDIR(info.st_mode):
                child = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
                try:
                    if mount_id(child) != root_mount:
                        report['mounts_preserved'] += 1
                        continue
                    remove_contents(child, root_mount, report)
                finally:
                    os.close(child)
                try:
                    os.rmdir(name, dir_fd=fd)
                    report['entries_removed'] += 1
                except OSError as error:
                    if error.errno not in (errno.ENOENT, errno.ENOTEMPTY, errno.EBUSY):
                        raise
            else:
                try:
                    os.unlink(name, dir_fd=fd)
                    report['entries_removed'] += 1
                except OSError as error:
                    if error.errno == errno.EBUSY:
                        report['mounts_preserved'] += 1
                    elif error.errno != errno.ENOENT:
                        raise
        except FileNotFoundError:
            pass
    # Active builds may recreate contents; empty-at-return is not a postcondition.


def apply(roots):
    opened = []
    try:
        # Open and bind every target before the first deletion. Root directories
        # and their exact permissions survive.
        for root in roots:
            try:
                fd = open_directory(root['path'])
            except FileNotFoundError:
                if root['identity'] is not None:
                    raise ValueError('cleanup directory disappeared after assessment')
                continue
            if identity(fd) != root['identity']:
                os.close(fd)
                raise ValueError('cleanup directory identity changed after assessment')
            opened.append((root, fd))
        report = dict(entries_removed=0, mounts_preserved=0, filesystems=[])
        filesystems = {}
        for root, fd in opened:
            device = os.fstat(fd).st_dev
            if device not in filesystems:
                space = os.fstatvfs(fd)
                filesystems[device] = (fd, space.f_bavail * space.f_frsize, root['path'])
        for root, fd in opened:
            remove_contents(fd, root['identity']['mount'], report)
            if identity(fd) != root['identity']:
                raise ValueError('cleanup root metadata changed during deletion')
        for device, (fd, before, path) in filesystems.items():
            space = os.fstatvfs(fd)
            after = space.f_bavail * space.f_frsize
            report['filesystems'].append(dict(device=device, path=path, available_bytes_before=before,
                                             available_bytes_after=after, change=after-before))
        return report
    finally:
        for _, fd in opened:
            os.close(fd)


if __name__ == '__main__':
    if len(sys.argv) < 2 or sys.argv[1] not in ('assess', 'apply'):
        raise ValueError('invalid yard cleanup action')
    if sys.argv[1] == 'assess':
        print(json.dumps(assess(sys.argv[2:]), separators=(',', ':')))
    else:
        print(json.dumps(apply(json.load(sys.stdin)), separators=(',', ':')))
