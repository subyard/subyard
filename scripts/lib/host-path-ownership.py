#!/usr/bin/env python3
"""Record install-created host namespaces without claiming existing user data."""
import hashlib
import json
import os
import pwd
import stat
import sys
import tempfile


def identity(path):
    value = os.lstat(path)
    if not stat.S_ISDIR(value.st_mode):
        raise ValueError("host namespace is not a real directory")
    return {"Device": value.st_dev, "Inode": value.st_ino, "Mode": value.st_mode, "UID": value.st_uid, "GID": value.st_gid}


def main():
    action, root, *names = sys.argv[1:]
    uid = pwd.getpwnam(os.environ["SUBYARD_USER"]).pw_uid if os.environ.get("SUBYARD_USER") else os.getuid()
    data, config = os.environ["SUBYARD_HOME"], os.environ["SUBYARD_CONFIG_HOME"]
    if not os.path.isabs(root) or os.path.normpath(root) != root or root in ("/", "/srv", "/home", "/tmp"):
        raise ValueError("invalid host namespace root")
    directory = data + "/ownership/host-paths"
    os.makedirs(directory, mode=0o700, exist_ok=True)
    for path in (data + "/ownership", directory):
        info = os.lstat(path)
        if not stat.S_ISDIR(info.st_mode) or info.st_uid not in (0, uid):
            raise ValueError("unsafe ownership ledger parent")
        os.chmod(path, 0o700)
        os.chown(path, uid, -1)
    path = directory + "/" + hashlib.sha256(root.encode()).hexdigest() + ".json"
    if os.path.lexists(path):
        info = os.lstat(path)
        if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600 or info.st_uid != uid:
            raise ValueError("unsafe host ownership ledger")
        with open(path, encoding="utf-8") as handle:
            plan = json.load(handle)
        if plan.get("schema") != 1 or plan.get("root") != root or plan.get("dataHome") != data or plan.get("configHome") != config or plan.get("uid") != uid:
            raise ValueError("foreign host ownership ledger")
    else:
        plan = {"schema": 1, "root": root, "dataHome": data, "configHome": config, "uid": uid, "paths": []}
    entries = {entry["name"]: entry for entry in plan["paths"]}
    if action == "begin":
        for name in names:
            if not name or name in (".", "..") or "/" in name:
                raise ValueError("invalid owned host namespace")
            if name not in entries:
                target = root + "/" + name
                # Existing unrecorded namespaces are foreign, even when named
                # like a default mount. Setup may use them; full cannot adopt them.
                entries[name] = {"name": name, "created": not os.path.lexists(target), "identity": None}
    elif action == "finish":
        for entry in entries.values():
            if not entry["created"]:
                continue
            observed = identity(root + "/" + entry["name"])
            if entry["identity"] and entry["identity"] != observed:
                raise ValueError("owned host namespace identity changed")
            entry["identity"] = observed
    else:
        raise ValueError("invalid host ownership action")
    plan["paths"] = sorted(entries.values(), key=lambda value: value["name"])
    fd, temporary = tempfile.mkstemp(prefix=".host-paths-", dir=directory)
    try:
        os.fchmod(fd, 0o600)
        os.fchown(fd, uid, -1)
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            json.dump(plan, handle, separators=(",", ":"))
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        directory_fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, TypeError):
        print("host namespace ownership could not be verified; preserve its data and ledger", file=sys.stderr)
        sys.exit(1)
