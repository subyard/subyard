#!/usr/bin/env python3
"""Exec a host-free runner with inherited directory ownership; no supervisor."""
import fcntl
import json
import os
from pathlib import Path
import stat
import sys
import tempfile

# Keep Unix socket fixtures below Linux's 108-byte pathname limit.
PREFIX = "s"
RECEIPT = ".test-run.json"


def identity(fd):
    info = os.fstat(fd)
    return [info.st_dev, info.st_ino]


def mount(fd):
    return next(line for line in Path(f"/proc/self/fdinfo/{fd}").read_text().splitlines()
                if line.startswith("mnt_id:"))


def valid(fd):
    info = os.fstat(fd)
    if info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700:
        return False
    receipt = os.open(RECEIPT, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=fd)
    try:
        info = os.fstat(receipt)
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600
                or not 0 < info.st_size <= 1024):
            return False
        state = json.loads(os.read(receipt, 1024))
        return (state == {"version": 1, "root": identity(fd), "uid": os.getuid()}
                and type(state["version"]) is int and type(state["uid"]) is int
                and all(type(value) is int for value in state["root"]))
    finally:
        os.close(receipt)


def clear(fd, root_mount, keep_receipt=False):
    # Like producer-run.clear, remove through pinned fds, with an extra mount guard.
    for name in os.listdir(fd):
        if keep_receipt and name == RECEIPT:
            continue  # Keep the authorization if an interrupted cleanup needs a retry.
        info = os.stat(name, dir_fd=fd, follow_symlinks=False)
        if stat.S_ISDIR(info.st_mode):
            child = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            try:
                if identity(child) != [info.st_dev, info.st_ino] or mount(child) != root_mount:
                    raise ValueError("temporary directory replaced or mounted")
                clear(child, root_mount)
                current = os.stat(name, dir_fd=fd, follow_symlinks=False)
                if identity(child) != [current.st_dev, current.st_ino]:
                    raise ValueError("temporary directory replaced")
                os.rmdir(name, dir_fd=fd)
            finally:
                os.close(child)
        else:
            os.unlink(name, dir_fd=fd)


def remove(parent, name, fd):
    current = os.stat(name, dir_fd=parent, follow_symlinks=False)
    if identity(fd) != [current.st_dev, current.st_ino] or mount(fd) != mount(parent):
        raise ValueError("temporary root replaced or mounted")
    clear(fd, mount(fd), keep_receipt=True)
    current = os.stat(name, dir_fd=parent, follow_symlinks=False)
    if identity(fd) != [current.st_dev, current.st_ino]:
        raise ValueError("temporary root replaced")
    os.unlink(RECEIPT, dir_fd=fd)
    os.rmdir(name, dir_fd=parent)


def sweep(parent):
    for name in sorted(os.listdir(parent)):
        if not name.startswith(PREFIX):
            continue
        try:
            fd = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent)
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                if valid(fd):
                    remove(parent, name, fd)
            finally:
                os.close(fd)
        except (OSError, ValueError):
            # Unknown, live, replaced, mounted, and malformed roots are preserved.
            continue


def inherited():
    fd = int(os.environ["SUBYARD_RUN_TEMP_FD"])
    path = Path(os.environ["SUBYARD_RUN_TEMP"])
    if path.parent != Path("/tmp") or not path.name.startswith(PREFIX) or not valid(fd):
        raise ValueError("invalid inherited test root")
    info = path.lstat()
    if identity(fd) != [info.st_dev, info.st_ino]:
        raise ValueError("inherited test root replaced")
    # The same open file description retains its lock through exec and nested runners.
    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    return fd, path


def main():
    if sys.argv[1] == "retain":
        parent = os.open(sys.argv[2], os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            runs = [(os.stat(name, dir_fd=parent, follow_symlinks=False).st_mtime_ns, name)
                    for name in os.listdir(parent) if name.startswith("run.")
                    and stat.S_ISDIR(os.stat(name, dir_fd=parent, follow_symlinks=False).st_mode)]
            for _, name in sorted(runs, reverse=True)[10:]:
                fd = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent)
                try:
                    if mount(fd) != mount(parent):
                        raise ValueError("mounted log directory")
                    clear(fd, mount(fd))
                    os.rmdir(name, dir_fd=parent)
                finally:
                    os.close(fd)
        finally:
            os.close(parent)
        return
    if sys.argv[1] == "clean":
        if os.environ.get("SUBYARD_RUN_TEMP_OWNER") == str(os.getppid()):
            fd, path = inherited()
            parent = os.open("/tmp", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            try:
                remove(parent, path.name, fd)
            finally:
                os.close(parent)
        return
    if sys.argv[1] != "enter":
        raise ValueError("expected enter or clean")
    if "SUBYARD_RUN_TEMP_FD" in os.environ:
        fd, path = inherited()
    else:
        parent = os.open("/tmp", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            sweep(parent)
        finally:
            os.close(parent)
        path = Path(tempfile.mkdtemp(prefix=PREFIX, dir="/tmp"))
        fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        os.fchmod(fd, 0o700)
        fcntl.flock(fd, fcntl.LOCK_EX)
        # Publish the receipt last: a root interrupted before this stays unknown.
        receipt = os.open(path / RECEIPT, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(receipt, "w") as output:
            os.fchmod(output.fileno(), 0o600)
            json.dump({"version": 1, "root": identity(fd), "uid": os.getuid()}, output)
        os.environ["SUBYARD_RUN_TEMP_OWNER"] = str(os.getpid())
    # Bash reserves low fds for scripts, redirections, and process substitutions.
    inherited_fd = fcntl.fcntl(fd, fcntl.F_DUPFD, 200)
    os.close(fd)
    os.set_inheritable(inherited_fd, True)
    os.environ.update(SUBYARD_RUN_TEMP=str(path), SUBYARD_RUN_TEMP_FD=str(inherited_fd),
                      SUBYARD_RUN_TEMP_ENTERED=str(os.getpid()), TMPDIR=str(path), GOTMPDIR=str(path),
                      PYTHONDONTWRITEBYTECODE="1")
    os.execvp(sys.argv[2], sys.argv[2:])


if __name__ == "__main__":
    main()
