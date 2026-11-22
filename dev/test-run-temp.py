#!/usr/bin/env python3
"""Exec a host-free runner with inherited directory ownership; no supervisor."""
import fcntl
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import time

# Subyard run roots: a distinct prefix within the Unix socket pathname budget.
PREFIX = "syr."
RECEIPT = ".test-run.json"
# Interrupted descendants get this long to exit before the root is left to the next sweep.
GRACE = 5


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
            child = os.open(name, os.O_PATH | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            try:
                if identity(child) != [info.st_dev, info.st_ino] or mount(child) != root_mount:
                    raise ValueError("temporary directory replaced or mounted")
                # Pin before chmod: crashed permission fixtures can leave 000 directories.
                if info.st_uid == os.getuid():
                    os.chmod(f"/proc/self/fd/{child}", stat.S_IMODE(info.st_mode) | 0o700)
                readable = os.open(".", os.O_RDONLY | os.O_DIRECTORY, dir_fd=child)
                try:
                    clear(readable, root_mount)
                finally:
                    os.close(readable)
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
                    try:
                        remove(parent, name, fd)
                    except (OSError, ValueError) as error:
                        print(f"test temporary sweep: could not remove /tmp/{name}: {error}", file=sys.stderr)
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


def retain(path):
    try:
        parent = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    except OSError:
        return  # Log retention must never prevent tests from running.
    try:
        runs = []
        for name in os.listdir(parent):
            if not name.startswith("run."):
                continue
            try:
                info = os.stat(name, dir_fd=parent, follow_symlinks=False)
                if stat.S_ISDIR(info.st_mode):
                    runs.append((info.st_mtime_ns, name))
            except OSError:
                continue
        for _, name in sorted(runs, reverse=True)[10:]:
            try:
                fd = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent)
                try:
                    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    current = os.stat(name, dir_fd=parent, follow_symlinks=False)
                    if identity(fd) != [current.st_dev, current.st_ino] or mount(fd) != mount(parent):
                        raise ValueError("log directory replaced or mounted")
                    clear(fd, mount(fd))
                    os.rmdir(name, dir_fd=parent)
                finally:
                    os.close(fd)
            except (OSError, ValueError):
                continue  # Active, concurrently removed or undeletable logs are preserved.
    except OSError:
        pass
    finally:
        os.close(parent)


def main():
    if sys.argv[1] == "check":
        try:
            _, path = inherited()
            return_code = not (os.environ.get("SUBYARD_RUN_TEMP_ENTERED") == str(os.getppid())
                               and os.environ.get("TMPDIR") == str(path)
                               and os.environ.get("GOTMPDIR") == str(path))
        except (KeyError, OSError, ValueError):
            return_code = 1
        sys.exit(return_code)
    if sys.argv[1] == "retain":
        retain(sys.argv[2])
        return
    if sys.argv[1] == "clean":
        if os.environ.get("SUBYARD_RUN_TEMP_OWNER") == str(os.getppid()):
            path = Path(os.environ["SUBYARD_RUN_TEMP"])
            if path.parent != Path("/tmp") or not path.name.startswith(PREFIX):
                raise ValueError("invalid inherited test root")
            parent = os.open("/tmp", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            fd = None
            try:
                # The wrapper closed its descriptor; the lock frees when the last user exits.
                fd = os.open(path.name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent)
                deadline = time.monotonic() + GRACE
                while True:
                    try:
                        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                        break
                    except BlockingIOError:
                        if time.monotonic() >= deadline:
                            print(f"test temporary root {path} is still in use; the next run removes it",
                                  file=sys.stderr)
                            return
                        time.sleep(.05)
                if not valid(fd):
                    raise ValueError("invalid test root")
                remove(parent, path.name, fd)
            except FileNotFoundError:
                pass  # A concurrent run's sweep already removed the released root.
            finally:
                if fd is not None:
                    os.close(fd)
                os.close(parent)
        return
    if sys.argv[1] != "enter":
        raise ValueError("expected enter or clean")
    try:
        fd, path = inherited()
    except (KeyError, OSError, ValueError):
        parent = os.open("/tmp", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            sweep(parent)
        finally:
            os.close(parent)
        # GNU mktemp supplies collision-safe creation with a shorter random suffix.
        path = Path(subprocess.check_output(["mktemp", "-d", f"/tmp/{PREFIX}XXXXX"], text=True).strip())
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
