#!/usr/bin/env python3
"""Durably refine approved private roots and remove runtime last."""
import errno
import fcntl
import importlib.util
import json
import os
import stat
import sys

spec = importlib.util.spec_from_file_location("guard", os.path.join(os.path.dirname(__file__), "teardown-plan.py"))
guard = importlib.util.module_from_spec(spec)
spec.loader.exec_module(guard)


class CallerBarrier:
    """An orphaned physical leaf must retain the same approved owner barrier."""
    def __init__(self, plan):
        self.plan = plan
        self.identity = os.environ.get("SUBYARD_TEARDOWN_PARENT_ID")
        self.handles = []

    def check(self):
        if not self.identity or self.handles:
            return
        pid, birth = self.identity.split(":", 1)
        try:
            # Comm may contain spaces/parentheses; fields after its final ')' start at state.
            fields = open("/proc/" + pid + "/stat", encoding="ascii").read().rsplit(")", 1)[1].split()
            if fields[0] != "Z" and fields[19] == birth:
                return
        except FileNotFoundError:
            pass
        lock = self.plan.get("OwnerLock")
        if not lock or len(lock["Entries"]) != 1:
            raise ValueError("approved owner barrier is missing after caller interruption")
        expected = lock["Entries"][0]
        fd = None
        try:
            with guard.artifact_parent(lock["Path"]) as (parent, name):
                fd = os.open(name, os.O_RDWR | os.O_NOFOLLOW, dir_fd=parent)
            info = os.fstat(fd)
            if (not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600
                    or info.st_uid != self.plan["Installation"]["operatorUid"]
                    or (info.st_dev, info.st_ino, info.st_gid) != (expected["Device"], expected["Inode"], expected["GID"])):
                raise ValueError("owner barrier changed after caller interruption")
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except Exception:
            if fd is not None:
                os.close(fd)
            raise
        self.handles = [fd]


def capture(tree):
    return guard.capture_tree(tree)["Entries"]


def check(tree, current, owned_git):
    guard.check_tree(tree, {"Entries": current})
    for entry in current:
        if os.path.basename(entry["Path"]) == ".git" and os.path.join(tree["Path"], entry["Path"]) not in owned_git:
            raise ValueError("full uninstall refuses user or source Git repositories")


def publish(path, plan):
    body = json.dumps(plan, separators=(",", ":")).encode()
    with guard.artifact_parent(path) as (parent, name):
        pending = ".refined-plan-next"
        guard.write_pending(parent, pending, body, 0o600, plan["Installation"]["operatorUid"])
        os.rename(pending, name, src_dir_fd=parent, dst_dir_fd=parent)
        os.fsync(parent)


def main():
    path, plan = guard.read_plan()
    barrier = CallerBarrier(plan)
    barrier.check()
    changed = False
    owned_git = plan.get("OwnedGitDirectories") or []
    for tree in plan["Trees"]:
        if os.path.lexists(tree["Path"] + "/.git") and tree["Path"] + "/.git" not in owned_git:
            raise ValueError("full uninstall refuses a user Git repository root")
        resource = tree.get("Resource")
        if resource:
            if resource["kind"] != "pool":
                raise ValueError("invalid backing storage owner")
            values = guard.query("/1.0/storage-pools", "default")
            if any(value.split("?")[0].endswith("/" + resource["name"]) for value in values):
                raise ValueError("backing storage pool remains; preserve its data")
        if tree.get("Opaque") and tree.get("Scoped"):
            with guard.artifact_parent(tree["Path"]) as (parent, name):
                all_entries = guard.artifact_entries(parent, name, 131072, tree.get("Exclusions") or ())
            retained = []
            for entry in all_entries:
                if not guard.within_owned(entry["Path"], tree.get("OwnedPaths") or ()) and not any(entry["Path"] == value or entry["Path"].startswith(value + "/") for value in retained):
                    retained.append(entry["Path"])
            tree["Preserved"] = list(dict.fromkeys((tree.get("Preserved") or []) + retained))
            tree["Exclusions"] = list(dict.fromkeys((tree.get("Exclusions") or []) + retained))
            plan["Preserved"] = (plan.get("Preserved") or []) + ["foreign content " + tree["Path"] + "/" + value for value in retained]
        current = capture(tree)
        facts = {entry["Path"]: entry for entry in current}
        for namespace in tree.get("Namespaces") or ():
            if namespace["Path"] in facts and not guard.same_artifact_fact(namespace, facts[namespace["Path"]], True):
                raise ValueError("installation-created host namespace changed")
        if tree.get("Opaque"):
            original = tree["Entries"]
            if len(original) != 1 or original[0]["Path"] != ".":
                raise ValueError("invalid conditional private root")
            before = original[0]
            after = dict(current[0]) if current else None
            if tree.get("Mutable") and after:
                before = dict(before)
                for field in ("Size", "Modified", "Changed"):
                    before[field] = after[field] = 0
            if current and not guard.same_artifact_fact(before, after, True):
                raise ValueError("private full uninstall root changed")
            if tree.get("Category") == "agent":
                allowed = {".", "manager.lock", "worker.lock", "worker.json", "control.sock", "private.sock"}
                if any(entry["Path"] not in allowed or entry["UID"] != plan["Installation"]["operatorUid"] for entry in current):
                    raise ValueError("SSH signing runtime acquired foreign entries")
            tree["Entries"], tree["Opaque"] = current, False
            changed = True
        check(tree, current, owned_git)
    # Publish metadata before the first unlink; interruption never adopts new
    # descendants of a private root when retried by a fresh process.
    if changed:
        barrier.check()
        publish(path, plan)
    ordered = sorted(plan["Trees"], key=lambda tree: tree.get("Category") == "runtime")
    for tree in ordered:
        current = capture(tree)
        check(tree, current, owned_git)
        try:
            with guard.artifact_parent(tree["Path"]) as (parent, name):
                guard.remove_entries(parent, name, tree["Entries"], True,
                                     tree.get("Exclusions") or (), bool(tree.get("Exclusions")), barrier.check)
        except FileNotFoundError:
            if os.path.lexists(tree["Path"]):
                raise ValueError("full uninstall root changed")
    # Only approved empty containers may remain after separately captured
    # descendants are removed. Identity-pinned rmdir never deletes new content.
    for tree in sorted(ordered, key=lambda tree: len(tree["Path"]), reverse=True):
        preserved = tree.get("Preserved") or ()
        for original in reversed(tree["Entries"]):
            relative = original["Path"]
            if not stat.S_ISDIR(original["Mode"]):
                continue
            candidate = tree["Path"] if relative == "." else os.path.join(tree["Path"], relative)
            try:
                with guard.artifact_parent(candidate) as (parent, name):
                    if not guard.same_artifact_fact(original, guard.artifact_fact(parent, name, relative), True):
                        raise ValueError("approved container identity changed")
                    barrier.check()
                    os.rmdir(name, dir_fd=parent)
            except FileNotFoundError:
                if os.path.lexists(candidate):
                    raise ValueError("approved container changed")
            except OSError as error:
                if error.errno != errno.ENOTEMPTY:
                    raise
    for tree in ordered:
        preserved = list(dict.fromkeys((tree.get("Exclusions") or []) + (tree.get("Preserved") or [])))
        for entry in guard.capture_tree(dict(tree, Scoped=False, Exclusions=preserved))["Entries"]:
            if not stat.S_ISDIR(entry["Mode"]) or not any(entry["Path"] == "." or path.startswith(entry["Path"] + "/") for path in preserved):
                raise ValueError("approved full uninstall artifact remains")


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, TypeError, RecursionError, guard.subprocess.CalledProcessError):
        print("full uninstall could not verify exact file removal; retain the saved recovery plan and retry", file=sys.stderr)
        sys.exit(75)
