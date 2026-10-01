"""Periodic, resumable discovery, independent of registration readiness.

Linux directory cookies let a new process continue a huge flat directory without
replaying its earlier files. A FIFO of directory portions gives other trees a
turn. Cookies are hints in a changing directory: every completed pass is followed
by a fresh pass, and omission is never evidence for deleting an Orca record.
Only directory entries and Git markers are inspected; no per-file watches exist.
"""

import ctypes
import errno
import fcntl
import json
import os
from pathlib import Path
import sqlite3
import stat
import sys
import time

from discovery import ScanLimit, _git_kind, _open_dir, _same_directory, verify_missing


class InventoryError(Exception):
    pass


class Directory:
    """The Linux 64-bit libc dirent ABI, shared by the shipped amd64/arm64 yards."""
    class Entry(ctypes.Structure):
        _fields_ = [("inode", ctypes.c_ulong), ("offset", ctypes.c_long),
                    ("length", ctypes.c_ushort), ("kind", ctypes.c_ubyte),
                    ("name", ctypes.c_char * 256)]

    def __init__(self, path, cookie=0):
        if sys.platform != "linux" or ctypes.sizeof(ctypes.c_void_p) != 8:
            raise InventoryError("Resumable discovery requires a 64-bit Linux yard")
        self.lib = ctypes.CDLL(None, use_errno=True)
        self.lib.fdopendir.argtypes = [ctypes.c_int]
        self.lib.fdopendir.restype = ctypes.c_void_p
        self.lib.readdir.argtypes = [ctypes.c_void_p]
        self.lib.readdir.restype = ctypes.POINTER(self.Entry)
        self.lib.telldir.argtypes = [ctypes.c_void_p]
        self.lib.telldir.restype = ctypes.c_long
        self.lib.seekdir.argtypes = [ctypes.c_void_p, ctypes.c_long]
        self.lib.closedir.argtypes = [ctypes.c_void_p]
        self.fd = _open_dir(path)
        if not _same_directory(path, self.fd):
            os.close(self.fd)
            raise OSError("Directory identity changed")
        self.handle = self.lib.fdopendir(self.fd)
        if not self.handle:
            os.close(self.fd)
            raise OSError(ctypes.get_errno(), "Cannot open directory stream")
        self.identity = os.fstat(self.fd)
        self.seek(cookie)

    def seek(self, cookie):
        self.lib.seekdir(self.handle, cookie)

    def next(self):
        ctypes.set_errno(0)
        entry = self.lib.readdir(self.handle)
        if not entry:
            if ctypes.get_errno():
                raise OSError(ctypes.get_errno(), "Cannot read directory stream")
            return None
        value = entry.contents
        # dirent records are variable-length; do not copy the declared 256-byte
        # name array past a short record at the end of libc's buffer.
        length = value.length - self.Entry.name.offset
        if not 1 <= length <= 264:
            raise OSError("Invalid directory entry")
        raw = ctypes.string_at(ctypes.addressof(value) + self.Entry.name.offset, length)
        name = os.fsdecode(raw.split(b"\0", 1)[0])
        cookie = self.lib.telldir(self.handle)
        if cookie < 0:
            raise OSError("Cannot save directory position")
        is_dir = value.kind == 4
        if value.kind == 0:
            is_dir = stat.S_ISDIR(os.stat(name, dir_fd=self.fd, follow_symlinks=False).st_mode)
        return name, is_dir, cookie

    def close(self):
        self.lib.closedir(self.handle)


class Inventory:
    def __init__(self, directory, apply=False):
        self.directory = Path(directory)
        self.path = self.directory / "subyard-discovery.sqlite3"
        self.apply = apply
        self.db = None
        self.lock = None

    def __enter__(self):
        try:
            if os.path.realpath(self.directory) != str(self.directory.absolute()):
                raise InventoryError("Discovery state directory is a symlink")
            if self.apply:
                self.directory.mkdir(mode=0o700, parents=True, exist_ok=True)
                info = self.directory.stat(follow_symlinks=False)
                if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o022:
                    raise InventoryError("Discovery state directory is unsafe")
                self.lock = os.open(self.directory / "subyard-discovery.lock",
                                    os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600)
                if not stat.S_ISREG(os.fstat(self.lock).st_mode):
                    raise InventoryError("Discovery lock is not a regular file")
                os.fchmod(self.lock, 0o600)
                try:
                    fcntl.flock(self.lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                except BlockingIOError:
                    raise InventoryError("Another discovery portion is running") from None
            try:
                fd = os.open(self.path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
            except FileNotFoundError:
                if not self.apply:
                    return self
                fd = os.open(self.path, os.O_RDWR | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
            try:
                info = os.fstat(fd)
                if (not stat.S_ISREG(info.st_mode) or info.st_nlink != 1
                        or info.st_uid != os.getuid() or info.st_mode & 0o022):
                    raise InventoryError("Discovery database is unsafe")
                if self.apply:
                    os.fchmod(fd, 0o600)
                elif info.st_size == 0:
                    # A concurrently starting worker has not installed its schema yet.
                    return self
            finally:
                os.close(fd)
            uri = self.path.absolute().as_uri() + ("?mode=rw" if self.apply else "?mode=ro")
            self.db = sqlite3.connect(uri, uri=True, timeout=0.1)
            if self.apply:
                self.db.executescript("""
                    PRAGMA journal_mode=WAL;
                    CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT);
                    CREATE TABLE IF NOT EXISTS roots (path TEXT PRIMARY KEY);
                    CREATE TABLE IF NOT EXISTS queue (
                        turn INTEGER PRIMARY KEY AUTOINCREMENT, path TEXT UNIQUE, project TEXT,
                        device INTEGER, inode INTEGER, cookie INTEGER NOT NULL DEFAULT 0);
                """)
            return self
        except (OSError, sqlite3.Error, InventoryError):
            self.__exit__(None, None, None)
            raise InventoryError("Discovery state is unavailable, unsafe or invalid") from None

    def __exit__(self, *_):
        if self.db is not None:
            self.db.close()
            self.db = None
        if self.lock is not None:
            os.close(self.lock)
            self.lock = None

    def paths(self):
        return [row[0] for row in self.db.execute("SELECT path FROM roots")] if self.db else []

    def progress(self):
        if self.db is None:
            return {"state": "pending", "generation": 0, "errors": []}
        values = dict(self.db.execute("SELECT key, value FROM metadata"))
        return {"state": "scanning" if self.db.execute("SELECT 1 FROM queue LIMIT 1").fetchone() else "idle",
                "generation": int(values.get("generation", "0")),
                "errors": json.loads(values.get("errors", "[]"))}

    def _set(self, key, value):
        self.db.execute("INSERT OR REPLACE INTO metadata VALUES (?, ?)", (key, value))

    def advance(self, scan, deadline, max_entries=50000, portion=256):
        """Commit one directory portion at a time; killed work repeats only that portion."""
        if not self.apply or not scan.identity or scan.errors or scan.warnings:
            return
        identity = json.dumps([scan.workspaces, *scan.identity])
        values = dict(self.db.execute("SELECT key, value FROM metadata"))
        projects = {project.root: project for project in scan.projects if project.roots}
        with self.db:
            if values.get("identity") != identity:
                self.db.execute("DELETE FROM queue")
                self.db.execute("DELETE FROM roots")
                self._set("identity", identity)
                values["seeded"] = "0"
                values["projects"] = "[]"
            # A new project gets a turn immediately, even during an enormous pass.
            previous = set(json.loads(values.get("projects", "[]")))
            for root in projects.keys() - previous:
                self.db.execute("INSERT OR IGNORE INTO queue(path, project) VALUES (?, ?)", (root, root))
            self._set("projects", json.dumps(sorted(projects)))
            # Revisit completed trees even while another tree's enormous cache
            # is still queued. Existing rows keep their directory cookies.
            if time.time() - float(values.get("seeded", "0")) >= 60:
                self._set("seeded", str(time.time()))
                self._set("generation", str(int(values.get("generation", "0")) + 1))
                self._set("errors", "[]")
                for root in projects:
                    self.db.execute("INSERT OR IGNORE INTO queue(path, project) VALUES (?, ?)", (root, root))
            elif not self.db.execute("SELECT 1 FROM queue LIMIT 1").fetchone():
                return
        count = 0
        errors = self.progress()["errors"]
        while count < max_entries and time.monotonic() < deadline:
            row = self.db.execute("SELECT turn, path, project, device, inode, cookie FROM queue ORDER BY turn LIMIT 1").fetchone()
            if row is None:
                break
            turn, path, project_root, device, inode, cookie = row
            stream = None
            try:
                if project_root not in projects or not path.startswith(project_root + "/") and path != project_root:
                    with self.db:
                        self.db.execute("DELETE FROM queue WHERE turn=?", (turn,))
                    continue
                stream = Directory(path)
                actual = (stream.identity.st_dev, stream.identity.st_ino)
                if actual == (device, inode):
                    stream.seek(cookie)
                else:
                    cookie = 0
                with self.db:
                    if cookie == 0:
                        try:
                            os.stat(".git", dir_fd=stream.fd, follow_symlinks=False)
                        except FileNotFoundError:
                            pass
                        else:
                            kind = _git_kind(path, stream.fd, True, deadline)
                            if kind != "folder":
                                self.db.execute("INSERT OR IGNORE INTO roots VALUES (?)", (path,))
                    more = True
                    for _ in range(min(portion, max_entries - count)):
                        if time.monotonic() >= deadline:
                            break
                        item = stream.next()
                        if item is None:
                            more = False
                            break
                        name, is_dir, cookie = item
                        count += 1
                        if is_dir and name not in (".", "..", ".git"):
                            self.db.execute("INSERT OR IGNORE INTO queue(path, project) VALUES (?, ?)",
                                            (os.path.join(path, name), project_root))
                    if not _same_directory(path, stream.fd):
                        raise OSError("Directory identity changed")
                    self.db.execute("DELETE FROM queue WHERE turn=?", (turn,))
                    if more:
                        self.db.execute("INSERT INTO queue(path, project, device, inode, cookie) VALUES (?, ?, ?, ?, ?)",
                                        (path, project_root, *actual, cookie))
            except ScanLimit:
                # The uncommitted portion remains queued at its previous cookie.
                break
            except OSError as error:
                if error.errno != errno.ENOENT:
                    diagnostic = "Directory discovery is unavailable: " + path
                    if diagnostic not in errors and len(errors) < 100:
                        errors.append(diagnostic)
                with self.db:
                    self.db.execute("DELETE FROM queue WHERE turn=?", (turn,))
            finally:
                if stream is not None:
                    stream.close()
            with self.db:
                self._set("errors", json.dumps(errors))
        # Proven absence, never a partial pass's omissions, retires cached facts.
        with self.db:
            for path in self.paths():
                if verify_missing(scan, path):
                    self.db.execute("DELETE FROM roots WHERE path=?", (path,))
