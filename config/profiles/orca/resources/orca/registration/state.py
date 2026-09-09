"""Subyard's registration identities, separate from Orca's own persistence.

An fsynced intent precedes every non-idempotent create. The instance lock covers
the whole apply, including reads and atomic identity-file replacement.
"""

import fcntl
import json
import os
from pathlib import Path
import stat
import tempfile
import time


class StateError(Exception):
    pass


class State:
    def __init__(self, directory, apply, deadline):
        self.directory = Path(directory)
        self.path = self.directory / "subyard-registration.json"
        self.apply = apply
        self.deadline = deadline
        self.lock = None
        self.data = {"schema": 1, "projects": {}}

    def __enter__(self):
        try:
            if self.apply:
                self.directory.mkdir(parents=True, exist_ok=True)
            flags = os.O_NOFOLLOW | os.O_NONBLOCK | (os.O_RDWR | os.O_CREAT if self.apply else os.O_RDONLY)
            try:
                self.lock = os.open(self.directory / "subyard-registration.lock", flags, 0o600)
            except FileNotFoundError:
                if self.apply:
                    raise
            if self.lock is not None:
                if not stat.S_ISREG(os.fstat(self.lock).st_mode):
                    raise StateError("Registration lock is not a regular file")
                end = min(self.deadline, time.monotonic() + 10)
                while True:
                    try:
                        fcntl.flock(self.lock, (fcntl.LOCK_EX if self.apply else fcntl.LOCK_SH) | fcntl.LOCK_NB)
                        break
                    except BlockingIOError:
                        if time.monotonic() >= end:
                            raise StateError("Another Orca registration invocation holds the lock") from None
                        time.sleep(0.05)
            try:
                fd = os.open(self.path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
            except FileNotFoundError:
                return self
            with os.fdopen(fd, "rb") as source:
                if not stat.S_ISREG(os.fstat(source.fileno()).st_mode):
                    raise StateError("Registration identity state is not a regular file")
                raw = source.read(8 * 1024 * 1024 + 1)
            if len(raw) > 8 * 1024 * 1024:
                raise ValueError()
            self.data = json.loads(raw)
            self._validate()
            return self
        except (OSError, ValueError, TypeError, KeyError, RecursionError, StateError) as error:
            self.__exit__(None, None, None)
            if isinstance(error, StateError):
                raise
            raise StateError("Registration identity state is unavailable or invalid") from None

    def _validate(self):
        if (not isinstance(self.data, dict) or self.data.get("schema") != 1
                or not isinstance(self.data.get("projects"), dict)):
            raise ValueError()
        ids = set()
        for project_id, entry in self.data["projects"].items():
            if (not isinstance(project_id, str) or not project_id or not isinstance(entry, dict)
                    or not isinstance(entry.get("root"), str) or not entry["root"].startswith("/")
                    or not isinstance(entry.get("pending_repos", {}), dict)):
                raise ValueError()
            group_id = entry.get("group_id")
            if group_id is not None:
                if not isinstance(group_id, str) or not group_id or group_id in ids:
                    raise ValueError()
                ids.add(group_id)
            if "pending_group" in entry:
                pending = entry["pending_group"]
                if (not isinstance(pending, dict) or not isinstance(pending.get("before_ids"), list)
                        or not all(isinstance(value, str) for value in pending["before_ids"])
                        or not isinstance(pending.get("runtime_id"), str)
                        or not isinstance(pending.get("name"), str)):
                    raise ValueError()
            for path, pending in entry.get("pending_repos", {}).items():
                if (not isinstance(path, str) or not isinstance(pending, dict)
                        or not isinstance(pending.get("before_ids"), list)
                        or not all(isinstance(value, str) for value in pending["before_ids"])
                        or not isinstance(pending.get("name"), str)
                        or ("runtime_id" in pending and not isinstance(pending["runtime_id"], str))):
                    raise ValueError()

    def save(self):
        if not self.apply:
            raise StateError("Read-only registration cannot save state")
        temporary = None
        try:
            fd, temporary = tempfile.mkstemp(prefix=".subyard-registration-", dir=self.directory)
            with os.fdopen(fd, "w", encoding="utf-8") as output:
                json.dump(self.data, output, sort_keys=True, separators=(",", ":"))
                output.write("\n")
                output.flush()
                os.fsync(output.fileno())
            os.replace(temporary, self.path)
            temporary = None
            directory_fd = os.open(self.directory, os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(directory_fd)
            finally:
                os.close(directory_fd)
        except OSError:
            raise StateError("Registration identity state could not be saved durably") from None
        finally:
            if temporary is not None:
                os.unlink(temporary)

    def __exit__(self, *_):
        if self.lock is not None:
            os.close(self.lock)
            self.lock = None
