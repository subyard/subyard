#!/usr/bin/env python3
"""Validate retained teardown identities; never print configuration or bindings."""
import datetime
import contextlib
import hashlib
import json
import os
import re
import stat
import subprocess
import sys
import urllib.parse


def artifacts():
    value = os.environ.get("SUBYARD_TEARDOWN_ARTIFACTS", "")
    if len(value.encode()) > 65536:
        raise ValueError("oversized teardown artifacts")
    approved = json.loads(value)
    if not isinstance(approved, list) or len(approved) > 256:
        raise ValueError("invalid teardown artifacts")
    seen = set()
    for item in approved:
        if set(item) != {"Path", "Binding"}:
            raise ValueError("invalid teardown artifact fields")
        path, binding = item["Path"], item["Binding"]
        if not isinstance(path, str) or not path.startswith("/") or path == "/" or os.path.normpath(path) != path:
            raise ValueError("invalid teardown artifact path")
        if not isinstance(binding, str) or (binding and not re.fullmatch(r"[a-f0-9]{64}", binding)) or path in seen:
            raise ValueError("invalid teardown artifact binding")
        seen.add(path)
    return approved


@contextlib.contextmanager
def artifact_parent(path):
    # Pin ancestors without following symlinks; deletion stays in that directory.
    fd = os.open("/", os.O_RDONLY | os.O_DIRECTORY)
    try:
        for part in path.split("/")[1:-1]:
            next_fd = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = next_fd
        yield fd, os.path.basename(path)
    finally:
        os.close(fd)


def artifact_fact(fd, name, relative):
    observed = os.stat(name, dir_fd=fd, follow_symlinks=False)
    return {"Path": relative, "Mode": observed.st_mode, "Size": observed.st_size,
            "Modified": observed.st_mtime_ns, "Device": observed.st_dev,
            "Inode": observed.st_ino, "UID": observed.st_uid, "GID": observed.st_gid,
            "Changed": observed.st_ctime_ns,
            "Link": os.readlink(name, dir_fd=fd) if stat.S_ISLNK(observed.st_mode) else ""}


def artifact_entries(fd, name):
    entries = []

    def walk(parent, child, relative):
        if len(entries) >= 4096:
            raise ValueError("teardown artifact exceeds bounded metadata inventory")
        fact = artifact_fact(parent, child, relative)
        entries.append(fact)
        if stat.S_ISDIR(fact["Mode"]):
            directory = os.open(child, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent)
            try:
                pinned = os.fstat(directory)
                if (pinned.st_dev, pinned.st_ino) != (fact["Device"], fact["Inode"]):
                    raise ValueError("teardown directory replaced during observation")
                for entry in sorted(os.listdir(directory)):
                    walk(directory, entry, entry if relative == "." else relative + "/" + entry)
                if artifact_fact(parent, child, relative) != fact:
                    raise ValueError("teardown directory changed during observation")
            finally:
                os.close(directory)

    try:
        os.stat(name, dir_fd=fd, follow_symlinks=False)
    except FileNotFoundError:
        return entries
    walk(fd, name, ".")
    return entries


def artifact_digest(entries):
    if not entries:
        return ""
    payload = json.dumps(entries, ensure_ascii=False, separators=(",", ":"))
    payload = payload.replace("<", "\\u003c").replace(">", "\\u003e").replace("&", "\\u0026").replace("\u2028", "\\u2028").replace("\u2029", "\\u2029")
    return hashlib.sha256(payload.encode()).hexdigest()


def remove_entries(fd, name, entries):
    approved = {entry["Path"]: entry for entry in entries}

    def remove(parent, child, relative):
        try:
            fact = artifact_fact(parent, child, relative)
        except FileNotFoundError:
            return
        if fact != approved.get(relative):
            raise ValueError("teardown artifact changed before deletion")
        if not stat.S_ISDIR(fact["Mode"]):
            os.unlink(child, dir_fd=parent)
            return
        directory = os.open(child, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent)
        try:
            pinned = os.fstat(directory)
            if (pinned.st_dev, pinned.st_ino) != (fact["Device"], fact["Inode"]):
                raise ValueError("teardown directory replaced before deletion")
            children = sorted(os.listdir(directory))
            for entry in children:
                key = entry if relative == "." else relative + "/" + entry
                if key not in approved:
                    raise ValueError("unapproved teardown directory entry")
            for entry in children:
                remove(directory, entry, entry if relative == "." else relative + "/" + entry)
            # Our unlinks change directory size/times. Ownership and identity must
            # still match; rmdir refuses any entries added during removal.
            current = artifact_fact(parent, child, relative)
            if any(current[key] != fact[key] for key in ("Mode", "Device", "Inode", "UID", "GID")):
                raise ValueError("teardown directory ownership changed before deletion")
            os.rmdir(child, dir_fd=parent)
        finally:
            os.close(directory)

    if entries:
        remove(fd, name, ".")


def guard_artifact(item, remove=False):
    try:
        with artifact_parent(item["Path"]) as (fd, name):
            entries = artifact_entries(fd, name)
            if entries and artifact_digest(entries) != item["Binding"]:
                raise ValueError("teardown artifact changed after confirmation")
            if remove:
                remove_entries(fd, name, entries)
    except FileNotFoundError:
        # Only a missing ancestor is absence; disappearing entries while walking
        # an existing tree must fail closed.
        if os.path.lexists(item["Path"]):
            raise ValueError("teardown artifact changed during observation")


def inventory():
    value = os.environ.get("SUBYARD_TEARDOWN_INVENTORY", "")
    if len(value.encode()) > 65536:
        raise ValueError("oversized teardown inventory")
    resources = json.loads(value)
    if not isinstance(resources, list) or len(resources) > 256:
        raise ValueError("invalid teardown inventory")
    seen = set()
    for resource in resources:
        if set(resource) - {"kind", "name", "pool", "binding"}:
            raise ValueError("invalid teardown resource fields")
        if resource.get("kind") not in ("instance", "profile", "image", "volume", "network", "pool", "project"):
            raise ValueError("invalid teardown resource kind")
        for field in ("name", "pool"):
            value = resource.get(field, "")
            if (field == "name" or value) and not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*", value):
                raise ValueError("invalid teardown resource identity")
        if not re.fullmatch(r"[a-f0-9]{64}", resource.get("binding", "")):
            raise ValueError("invalid teardown binding")
        key = (resource["kind"], resource.get("pool", ""), resource["name"])
        if key in seen:
            raise ValueError("duplicate teardown resource")
        seen.add(key)
    return resources


def query(path, project):
    return json.loads(subprocess.check_output(
        ["incus", "query", path + "?project=" + urllib.parse.quote(project, safe="")],
        stderr=subprocess.DEVNULL))


def guard(resource, project):
    kind, name = resource["kind"], resource["name"]
    if kind == "instance":
        path = "/1.0/instances/" + name
        fields = ("name", "type", "created_at", "config", "devices", "profiles")
    elif kind == "profile":
        path = "/1.0/profiles/" + name
        fields = ("name", "config", "devices", "description")
    elif kind == "volume":
        path = "/1.0/storage-pools/" + resource["pool"] + "/volumes/custom/" + name
        fields = ("name", "type", "content_type", "config", "description")
    elif kind == "project":
        path = "/1.0/projects/" + name
        fields = ("name", "config", "description")
        project = "default"
    elif kind == "network":
        path = "/1.0/networks/" + name
        fields = ("name", "type", "managed", "config", "description")
        project = "default"
    elif kind == "pool":
        path = "/1.0/storage-pools/" + name
        fields = ("name", "driver", "config", "description")
        project = "default"
    else:
        value = query("/1.0/images/" + name, project)["fingerprint"]
        fields = ()
    if fields:
        observed = query(path, project)
        value = {key: observed[key] for key in fields}
        if kind in ("instance", "volume"):
            value["snapshots"] = sorted(urllib.parse.unquote(urllib.parse.urlsplit(item).path.rsplit("/", 1)[-1])
                                        for item in query(path + "/snapshots", project))
        if kind == "instance":
            timestamp = value["created_at"]
            fraction = re.search(r"\.(\d+)", timestamp)
            stamp = datetime.datetime.fromisoformat(re.sub(r"\.\d+", "", timestamp).replace("Z", "+00:00"))
            suffix = fraction.group(1).rstrip("0") if fraction else ""
            value["created_at"] = stamp.astimezone(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S") + ("." + suffix if suffix else "") + "Z"

    payload = json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    payload = payload.replace("<", "\\u003c").replace(">", "\\u003e").replace("&", "\\u0026").replace("\u2028", "\\u2028").replace("\u2029", "\\u2029")
    if hashlib.sha256(payload.encode()).hexdigest() != resource["binding"]:
        raise ValueError("teardown resource changed after confirmation")


def main():
    mode = sys.argv[1]
    if mode in ("guard-artifacts", "guard-artifact", "remove-artifact"):
        approved = artifacts()
        if mode != "guard-artifacts":
            approved = [item for item in approved if item["Path"] == sys.argv[2]]
            if len(approved) != 1:
                raise ValueError("teardown artifact was not approved")
        for item in approved:
            guard_artifact(item, remove=mode == "remove-artifact")
        return
    resources = inventory()
    if mode == "validate":
        return
    if mode == "guard-all":
        project = sys.argv[2]
        if project in ("", "default"):
            raise ValueError("teardown requires a dedicated project")
        for resource in resources:
            guard(resource, project)
        return
    if mode == "volumes":
        for resource in resources:
            if resource["kind"] == "volume":
                print(resource["pool"] + "\t" + resource["name"])
        return
    kind = sys.argv[2]
    if mode == "list":
        for resource in resources:
            if resource["kind"] == kind:
                print(resource["name"])
        return
    if mode == "guard":
        name, project = sys.argv[3:5]
        pool = sys.argv[5] if len(sys.argv) > 5 else ""
        if project in ("", "default"):
            raise ValueError("teardown requires a dedicated project")
        matches = [item for item in resources if item["kind"] == kind and item["name"] == name and item.get("pool", "") == pool]
        if len(matches) != 1:
            raise ValueError("teardown resource was not approved")
        guard(matches[0], project)
        return
    raise ValueError("invalid teardown guard mode")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, TypeError, OSError, RecursionError, subprocess.CalledProcessError):
        print("plan_stale: teardown inventory, artifact or resource binding is invalid", file=sys.stderr)
        sys.exit(75 if sys.argv[1] in ("guard-artifacts", "guard-artifact", "remove-artifact") else 1)
