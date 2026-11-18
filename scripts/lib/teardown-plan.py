#!/usr/bin/env python3
"""Validate retained teardown identities; never print configuration or bindings."""
import datetime
import base64
import contextlib
import hashlib
import json
import os
import re
import stat
import subprocess
import sys
import urllib.parse


def read_plan():
    path = os.environ["SUBYARD_TEARDOWN_PLAN"]
    with artifact_parent(path) as (parent, name):
        fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW, dir_fd=parent)
        try:
            info = os.fstat(fd)
            with os.fdopen(fd, "rb", closefd=False) as handle:
                body = handle.read(64 * 1024 * 1024 + 1)
            plan = json.loads(body)
            if (len(body) > 64 * 1024 * 1024 or not stat.S_ISREG(info.st_mode)
                    or stat.S_IMODE(info.st_mode) != 0o600 or info.st_uid != plan["Installation"]["operatorUid"]
                    or hashlib.sha256(body).hexdigest() != os.environ["SUBYARD_TEARDOWN_PLAN_SHA256"]):
                raise ValueError("changed or unsafe full uninstall plan")
            return path, plan
        finally:
            os.close(fd)


def owner_artifacts(owners):
    original = {}
    for owner in owners:
        for artifact in owner.get("Artifacts") or ():
            previous = original.setdefault(artifact["Path"], artifact)
            if previous != artifact:
                raise ValueError("conflicting approved artifact facts")
    return original


def services(plan):
    approved = plan["Services"] if plan is not None else json.loads(os.environ["SUBYARD_TEARDOWN_SERVICES"])
    if not isinstance(approved, list) or len(approved) > 128:
        raise ValueError("invalid approved service inventory")
    for service in approved:
        if set(service) != {"scope", "units", "Artifacts"} or service["scope"] not in ("system", "user"):
            raise ValueError("invalid approved service scope")
        if not isinstance(service["units"], list) or len(service["units"]) > 4:
            raise ValueError("invalid approved service units")
        for unit in service["units"]:
            if not isinstance(unit, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.@:+-]*\.(?:service|timer|socket)", unit):
                raise ValueError("invalid approved service identity")
    return approved


def artifacts():
    plan = read_plan()[1] if os.environ.get("SUBYARD_TEARDOWN_PLAN") else None
    value = os.environ.get("SUBYARD_TEARDOWN_ARTIFACTS")
    if value is None and plan is not None:
        approved = list(owner_artifacts(services(plan)).values())
    else:
        if value is None or len(value.encode()) > 65536:
            raise ValueError("missing or oversized teardown artifacts")
        approved = json.loads(value)
    if not isinstance(approved, list) or len(approved) > 256:
        raise ValueError("invalid teardown artifacts")
    seen = set()
    for item in approved:
        if set(item) not in ({"Path", "Binding"}, {"Path", "Binding", "Entries"}):
            raise ValueError("invalid teardown artifact fields")
        path, binding = item["Path"], item["Binding"]
        if not isinstance(path, str) or not path.startswith("/") or path == "/" or os.path.normpath(path) != path:
            raise ValueError("invalid teardown artifact path")
        if not isinstance(binding, str) or (binding and not re.fullmatch(r"[a-f0-9]{64}", binding)) or path in seen:
            raise ValueError("invalid teardown artifact binding")
        seen.add(path)
    if plan is not None:
        original = owner_artifacts(list(plan["Services"]) + [yard["Snapshot"] for yard in plan["Yards"]])
        for index, item in enumerate(approved):
            saved = original.get(item["Path"])
            if saved is None or saved["Binding"] != item["Binding"] or "Entries" not in saved and item["Binding"]:
                raise ValueError("artifact is outside original full plan")
            approved[index] = saved
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
    result = fact_from_stat(observed, relative)
    result["Link"] = os.readlink(name, dir_fd=fd) if stat.S_ISLNK(observed.st_mode) else ""
    return result


def fact_from_stat(observed, relative):
    return {"Path": relative, "Mode": observed.st_mode, "Size": observed.st_size,
            "Modified": observed.st_mtime_ns, "Device": observed.st_dev,
            "Inode": observed.st_ino, "UID": observed.st_uid, "GID": observed.st_gid,
            "Changed": observed.st_ctime_ns,
            "Link": ""}


def artifact_entries(fd, name, limit=4096, exclusions=(), owned=None, preserved=None):
    entries = []

    def walk(parent, child, relative):
        if any(relative == excluded or relative.startswith(excluded + "/") for excluded in exclusions):
            return
        if owned is not None and not within_owned(relative, owned):
            if preserved is not None:
                preserved.append(relative)
            return
        if len(entries) >= limit:
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


def same_artifact_fact(approved, current, resumable=False):
    if resumable and approved and stat.S_ISDIR(approved["Mode"]):
        return all(approved[key] == current[key] for key in
                   ("Path", "Mode", "Device", "Inode", "UID", "GID", "Link"))
    return approved == current


def remove_entries(fd, name, entries, resumable=False, exclusions=(), retain_root=False, before_remove=None):
    approved = {entry["Path"]: entry for entry in entries}

    def remove(parent, child, relative):
        if any(relative == excluded or relative.startswith(excluded + "/") for excluded in exclusions):
            return
        try:
            fact = artifact_fact(parent, child, relative)
        except FileNotFoundError:
            return
        if not same_artifact_fact(approved.get(relative), fact, resumable):
            raise ValueError("teardown artifact changed before deletion")
        if not stat.S_ISDIR(fact["Mode"]):
            if before_remove:
                before_remove()
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
                if key not in approved and not any(key == x or key.startswith(x + "/") for x in exclusions):
                    raise ValueError("unapproved teardown directory entry")
            for entry in children:
                remove(directory, entry, entry if relative == "." else relative + "/" + entry)
            # Our unlinks change directory size/times. Ownership and identity must
            # still match; rmdir refuses any entries added during removal.
            current = artifact_fact(parent, child, relative)
            if any(current[key] != fact[key] for key in ("Mode", "Device", "Inode", "UID", "GID")):
                raise ValueError("teardown directory ownership changed before deletion")
            if not (relative == "." and retain_root) and not any(x.startswith(relative + "/") or relative == "." for x in exclusions):
                if before_remove:
                    before_remove()
                os.rmdir(child, dir_fd=parent)
        finally:
            os.close(directory)

    if entries:
        remove(fd, name, ".")


def guard_artifact(item, remove=False):
    try:
        with artifact_parent(item["Path"]) as (fd, name):
            entries = artifact_entries(fd, name)
            originals = item.get("Entries")
            if originals is not None:
                if (artifact_digest(originals) if originals else "") != item["Binding"]:
                    raise ValueError("original teardown artifact binding changed")
                check_tree(dict(Entries=originals), dict(Entries=entries))
            elif entries and artifact_digest(entries) != item["Binding"]:
                raise ValueError("teardown artifact changed after confirmation")
            if remove:
                if originals is None:
                    remove_entries(fd, name, entries)
                else:
                    remove_entries(fd, name, originals, True)
    except FileNotFoundError:
        # Only a missing ancestor is absence; disappearing entries while walking
        # an existing tree must fail closed.
        if os.path.lexists(item["Path"]):
            raise ValueError("teardown artifact changed during observation")


def within_owned(path, owned):
    return path == "." or any(path == item or path.startswith(item + "/") or item.startswith(path + "/") for item in owned)


def capture_tree(tree):
    root = tree["Path"]
    if not isinstance(root, str) or not root.startswith("/") or os.path.normpath(root) != root or os.path.dirname(root) == "/":
        raise ValueError("invalid installation artifact root")
    exclusions = []
    for relative in tree.get("Exclusions") or ():
        if os.path.isabs(relative):
            relative = os.path.relpath(relative, root)
        if relative in (".", "..") or relative.startswith("../") or os.path.normpath(relative) != relative:
            raise ValueError("invalid retained artifact subtree")
        exclusions.append(relative)
    preserved = []
    try:
        with artifact_parent(root) as (parent, name):
            entries = artifact_entries(parent, name, 131072, exclusions,
                                       tree.get("OwnedPaths") or () if tree.get("Scoped") else None, preserved)
    except FileNotFoundError:
        if os.path.lexists(root):
            raise ValueError("installation artifact changed during observation")
        entries = []
    if entries and any(entry["Device"] != entries[0]["Device"] for entry in entries):
        raise ValueError("installation artifact contains mounted descendants")
    return dict(tree, Entries=entries, Exclusions=exclusions + preserved, Preserved=preserved)


def check_tree(tree, current):
    originals = {}
    for entry in tree["Entries"]:
        relative = entry["Path"]
        if not isinstance(relative, str) or os.path.isabs(relative) or relative == ".." or relative.startswith("../") or os.path.normpath(relative) != relative or relative in originals:
            raise ValueError("invalid installation artifact descendant")
        originals[relative] = entry
    for entry in current["Entries"]:
        if not same_artifact_fact(originals.get(entry["Path"]), entry, True):
            raise ValueError("installation artifact changed after confirmation")


def file_tree(mode):
    body = sys.stdin.buffer.read(64 * 1024 * 1024 + 1)
    if len(body) > 64 * 1024 * 1024:
        raise ValueError("oversized installation artifact")
    tree = json.loads(body)
    if mode == "verify-tree":
        preserved = list(dict.fromkeys((tree.get("Exclusions") or []) + (tree.get("Preserved") or [])))
        current = capture_tree(dict(tree, Scoped=False, Exclusions=preserved))
        for entry in current["Entries"]:
            if not stat.S_ISDIR(entry["Mode"]) or not any(entry["Path"] == "." or path.startswith(entry["Path"] + "/") for path in preserved):
                raise ValueError("approved installation artifact remains")
        return
    current = capture_tree(tree)
    if mode == "capture-tree":
        print(json.dumps(current, separators=(",", ":")))
        return
    check_tree(tree, current)
    if mode == "remove-tree":
        if tree.get("Opaque"):
            raise ValueError("private installation artifact was not refined")
        try:
            with artifact_parent(tree["Path"]) as (parent, name):
                remove_entries(parent, name, tree["Entries"], True, tree.get("Exclusions") or (), False)
        except FileNotFoundError:
            if os.path.lexists(tree["Path"]):
                raise ValueError("installation artifact root changed")


def write_pending(parent, name, body, mode, uid):
    try:
        fd = os.open(name, os.O_RDWR | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=parent)
        if os.getuid() == 0:
            os.fchown(fd, uid, -1)
    except FileExistsError:
        fd = os.open(name, os.O_RDWR | os.O_NOFOLLOW, dir_fd=parent)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) not in (0o600, mode) or info.st_uid != uid or not body.startswith(os.read(fd, len(body) + 1)):
            raise ValueError("unsafe interrupted file publication")
        os.lseek(fd, 0, os.SEEK_SET)
        with os.fdopen(fd, "wb", closefd=False) as handle:
            handle.write(body); handle.flush()
        os.fchmod(fd, mode); os.fsync(fd)
        current = artifact_fact(parent, name, ".")
        if (info.st_dev, info.st_ino) != (current["Device"], current["Inode"]):
            raise ValueError("pending file changed during publication")
        return current
    finally:
        os.close(fd)


def startup_edit(mode):
    edit = json.loads(sys.stdin.buffer.read(2 * 1024 * 1024 + 1))
    before, pending = edit["Before"], edit.get("Pending")
    with artifact_parent(before["Path"]) as (parent, name):
        if mode == "prepare-startup":
            current = capture_tree(before)
            check_tree(before, current)
            if len(current["Entries"]) != 1:
                raise ValueError("managed shell file is missing")
            source = os.open(name, os.O_RDONLY | os.O_NOFOLLOW, dir_fd=parent)
            try:
                if fact_from_stat(os.fstat(source), ".") != before["Entries"][0]:
                    raise ValueError("managed shell file changed before reading")
                with os.fdopen(source, "rb", closefd=False) as handle:
                    content = handle.read(1024 * 1024 + 1)
                if len(content) > 1024 * 1024:
                    raise ValueError("managed shell file is oversized")
            finally:
                os.close(source)
            for block in edit["Blocks"]:
                block = block.encode()
                if content.count(block) != 1:
                    raise ValueError("managed shell block changed")
                content = content.replace(block, b"", 1)
            check_tree(before, capture_tree(before))
            planned = edit.get("Temporary")
            if not planned or os.path.dirname(planned) != os.path.dirname(before["Path"]) or not re.fullmatch(r"\.subyard-uninstall-[a-f0-9]{24}", os.path.basename(planned)):
                raise ValueError("original shell sidecar intent is missing")
            temporary = os.path.basename(planned)
            fact = write_pending(parent, temporary, content, before["Entries"][0]["Mode"] & 0o777, os.getuid())
            try:
                check_tree(before, capture_tree(before))
            except ValueError:
                remove_entries(parent, temporary, [fact])
                raise
            edit["Pending"] = dict(Path=os.path.join(os.path.dirname(before["Path"]), temporary), Entries=[fact])
            print(json.dumps(edit, separators=(",", ":")))
            return
        if not pending or pending["Path"] != edit.get("Temporary") or len(pending["Entries"]) != 1 or os.path.dirname(pending["Path"]) != os.path.dirname(before["Path"]):
            raise ValueError("invalid prepared shell edit")
        temporary = os.path.basename(pending["Path"])
        try:
            current = artifact_fact(parent, temporary, ".")
        except FileNotFoundError:
            original = dict(pending["Entries"][0], Changed=0)
            current = dict(artifact_fact(parent, name, "."), Changed=0)
            if original != current:
                raise ValueError("interrupted shell destination changed")
        else:
            if current != pending["Entries"][0]:
                raise ValueError("prepared shell edit changed")
            check_tree(before, capture_tree(before))
            os.rename(temporary, name, src_dir_fd=parent, dst_dir_fd=parent)
            os.fsync(parent)
        print(json.dumps(dict(Path=before["Path"], Entries=[artifact_fact(parent, name, ".")]), separators=(",", ":")))


def advance_epoch():
    request = json.loads(sys.stdin.buffer.read(65537))
    uid, before, after = request["UID"], request["Before"], request["After"]
    if os.getuid() != uid or after != before + 1 or after > 2**64 - 1:
        raise ValueError("invalid owner epoch advancement")
    with artifact_parent(request["Path"]) as (parent, name):
        directory = os.fstat(parent)
        if directory.st_uid != uid or stat.S_IMODE(directory.st_mode) != 0o700:
            raise ValueError("unsafe owner epoch directory")
        try:
            fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW, dir_fd=parent)
        except FileNotFoundError:
            current = 0
        else:
            try:
                info = os.fstat(fd)
                body = os.read(fd, 33)
                if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600 or info.st_uid != uid:
                    raise ValueError("unsafe owner epoch")
                current = int(body)
                if body != (str(current) + "\n").encode():
                    raise ValueError("invalid owner epoch")
            finally:
                os.close(fd)
        if current == after:
            return
        if current != before:
            raise ValueError("owner epoch changed outside approved cleanup")
        write_pending(parent, name + "-next", (str(after) + "\n").encode(), 0o600, uid)
        os.rename(name + "-next", name, src_dir_fd=parent, dst_dir_fd=parent)
        os.fsync(parent)


def publish_plan():
    request = json.loads(sys.stdin.buffer.read(96 * 1024 * 1024 + 1))
    body = base64.b64decode(request["Body"], validate=True)
    if len(body) > 64 * 1024 * 1024:
        raise ValueError("oversized recovery publication")
    path, uid, expected = request["Path"], request["UID"], request["Expected"]
    with artifact_parent(path) as (parent, name):
        directory = os.fstat(parent)
        if not stat.S_ISDIR(directory.st_mode) or stat.S_IMODE(directory.st_mode) != 0o700 or directory.st_uid != uid or os.getuid() != uid:
            raise ValueError("unsafe recovery publication parent")
        def unchanged():
            if expected is not None:
                if expected["Path"] != path:
                    raise ValueError("different recovery publication path")
                check_tree(expected, capture_tree(expected))
            else:
                try: os.stat(name, dir_fd=parent, follow_symlinks=False)
                except FileNotFoundError: return
                raise ValueError("recovery plan already exists")
        unchanged()
        write_pending(parent, ".plan-next", body, 0o600, uid)
        unchanged()
        os.rename(".plan-next", name, src_dir_fd=parent, dst_dir_fd=parent)
        os.fsync(parent)


def capture_assets():
    root = json.loads(sys.stdin.buffer.read(8192))
    manifest = os.path.join(root, "runtime-files.sha256")
    digests = {}
    try:
        with open(manifest, encoding="ascii") as handle:
            body = handle.read(4 * 1024 * 1024 + 1)
        if len(body) > 4 * 1024 * 1024:
            raise ValueError("oversized installed asset manifest")
        for line in body.splitlines():
            digest, relative = line.split("  ", 1)
            if not re.fullmatch("[a-f0-9]{64}", digest):
                raise ValueError("invalid installed asset digest")
            relative = relative.removeprefix("./")
            if relative in digests:
                raise ValueError("duplicate installed asset path")
            digests[relative] = digest
        paths = list(digests)
    except FileNotFoundError:
        body = subprocess.check_output(["rg", "--no-config", "--files", "--hidden", "--null", "--no-require-git", "--no-ignore-dot", "--no-ignore-global", "--no-ignore-parent", "--no-ignore-exclude", "-g", "!.git"], cwd=root)
        if len(body) > 4 * 1024 * 1024:
            raise ValueError("oversized public source selector")
        paths = [path.decode() for path in body.split(b"\0") if path]
    assets = []
    seen = set()
    for relative in paths:
        # Keep the core vendor pair shipped by package-engine.sh; retained
        # handlers use it after the original runtime has been removed.
        if not relative.startswith(("scripts/", "config/", "completions/")) and relative not in (
                "bin/yard", "internal/adapters/configmaterial/tomli_w.py",
                "internal/adapters/configmaterial/TOMLI_W_LICENSE"):
            continue
        if os.path.isabs(relative) or os.path.normpath(relative) != relative or relative == ".." or relative.startswith("../") or any(character in relative for character in "\r\n\t\0") or relative in seen or len(assets) >= 4096:
            raise ValueError("invalid public recovery asset")
        seen.add(relative)
        tree = capture_tree({"Path": os.path.join(root, relative), "Entries": []})
        if len(tree["Entries"]) != 1 or not stat.S_ISREG(tree["Entries"][0]["Mode"]):
            raise ValueError("public recovery asset is not one regular file")
        if relative in digests:
            with open(tree["Path"], "rb") as handle:
                digest = hashlib.sha256()
                for chunk in iter(lambda: handle.read(1024 * 1024), b""):
                    digest.update(chunk)
                if digest.hexdigest() != digests[relative]:
                    raise ValueError("public recovery asset differs from installed manifest")
            check_tree(tree, capture_tree(tree))
        assets.append(tree)
    required = {"bin/yard", "config/commands.registry", "scripts/teardown-physical.sh", "scripts/lib/teardown-services.sh", "scripts/lib/teardown-plan.py"}
    if not required.issubset(seen):
        raise ValueError("required public recovery assets are missing")
    print(json.dumps(assets, separators=(",", ":")))


def retain_runtime():
    request = json.loads(sys.stdin.buffer.read(64 * 1024 * 1024 + 1))
    plan, destination = request["Plan"], request["Destination"]
    uid = plan["Installation"]["operatorUid"]
    if os.getuid() != uid:
        raise ValueError("retained runtime must be copied by its owner")

    def directory(path):
        with artifact_parent(path) as (parent, name):
            try:
                os.mkdir(name, 0o700, dir_fd=parent)
                os.chmod(name, 0o700, dir_fd=parent, follow_symlinks=False)
            except FileExistsError:
                pass
            info = os.stat(name, dir_fd=parent, follow_symlinks=False)
            if not stat.S_ISDIR(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o700 or info.st_uid != uid:
                raise ValueError("unsafe retained runtime directory")
    directory(destination)
    for tree in plan["SourceAssets"]:
        if len(tree["Entries"]) != 1 or not stat.S_ISREG(tree["Entries"][0]["Mode"]):
            raise ValueError("retained public asset must be one approved file")
        relative = tree.get("Category") or os.path.relpath(tree["Path"], plan["SourceRoot"])
        if os.path.isabs(relative) or relative == ".." or relative.startswith("../") or os.path.normpath(relative) != relative:
            raise ValueError("retained public asset escapes its runtime")
        target = os.path.join(destination, relative)
        parents = relative.split("/")[:-1]
        for depth in range(1, len(parents) + 1):
            directory(os.path.join(destination, *parents[:depth]))
        approved = tree["Entries"][0]
        with artifact_parent(tree["Path"]) as (parent, name):
            source = os.open(name, os.O_RDONLY | os.O_NOFOLLOW, dir_fd=parent)
            try:
                if fact_from_stat(os.fstat(source), ".") != approved:
                    raise ValueError("retained public source changed before reading")
                with os.fdopen(source, "rb", closefd=False) as handle:
                    body = handle.read(approved["Size"] + 1)
                if len(body) != approved["Size"] or fact_from_stat(os.fstat(source), ".") != approved:
                    raise ValueError("retained public source changed while reading")
            finally:
                os.close(source)
        mode = 0o700 if approved["Mode"] & 0o111 else 0o600
        with artifact_parent(target) as (parent, name):
            def existing(candidate):
                fd = os.open(candidate, os.O_RDONLY | os.O_NOFOLLOW, dir_fd=parent)
                try:
                    info = os.fstat(fd)
                    if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != mode or info.st_uid != uid:
                        raise ValueError("unsafe retained runtime asset")
                    with os.fdopen(fd, "rb", closefd=False) as handle:
                        if handle.read(len(body) + 1) != body:
                            raise ValueError("retained runtime bytes differ from approved source")
                finally:
                    os.close(fd)
            try:
                existing(name)
                continue
            except FileNotFoundError:
                pass
            pending = name + ".copy-next"
            write_pending(parent, pending, body, mode, uid)
            try:
                os.stat(name, dir_fd=parent, follow_symlinks=False)
            except FileNotFoundError:
                os.rename(pending, name, src_dir_fd=parent, dst_dir_fd=parent)
                os.fsync(parent)
            else:
                raise ValueError("retained runtime destination appeared during publication")


def producer_template_matches(content, expected, publication):
    suffix = content[len(expected):] if content.startswith(expected) else b""
    return content == expected or bool(publication and re.fullmatch(rb"\n\[Service\]\nEnvironment=SUBYARD_PUBLICATION_CONTRACTS=[A-Za-z0-9+/=]+\n", suffix))


def capture_service(scope, units, files, helpers, uid):
    artifacts = []
    for path, expected in dict(files, **helpers).items():
        try:
            with artifact_parent(path) as (parent, name):
                facts = artifact_entries(parent, name)
                if not facts:
                    continue
                fact = facts[0]
                if not stat.S_ISREG(fact["Mode"]) or fact["UID"] != uid or stat.S_IMODE(fact["Mode"]) != (0o755 if path in helpers else 0o644):
                    raise ValueError("unsafe producer artifact")
                if path in helpers:
                    if not expected:
                        raise ValueError("unknown producer helper")
                else:
                    fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW, dir_fd=parent)
                    with os.fdopen(fd, "rb") as source:
                        content = source.read(1024 * 1024 + 1)
                    if not producer_template_matches(content, expected, bool(helpers)):
                        raise ValueError("changed or foreign producer template")
                artifacts.append(dict(Path=path, Entries=facts, Binding=artifact_digest(facts)))
        except FileNotFoundError:
            if os.path.lexists(path):
                raise ValueError("producer artifact changed")
    for path in files:
        for target in ("timers.target.wants", "sockets.target.wants", "multi-user.target.wants"):
            link = os.path.join(os.path.dirname(path), target, os.path.basename(path))
            try:
                with artifact_parent(link) as (parent, name):
                    facts = artifact_entries(parent, name)
                    destination = os.readlink(name, dir_fd=parent)
                    if os.path.normpath(os.path.join(os.path.dirname(link), destination)) != path:
                        raise ValueError("foreign producer enablement")
                    artifacts.append(dict(Path=link, Entries=facts, Binding=artifact_digest(facts)))
            except FileNotFoundError:
                if os.path.lexists(link):
                    raise ValueError("producer enablement changed")
    return dict(scope=scope, units=units, Artifacts=sorted(artifacts, key=lambda value: value["Path"]))


def core_producers(request):
    root, installation, environment = request["Root"], request["Installation"], request["Environment"]
    owners = request["Owners"]
    result = []
    original = request.get("Original") or {}
    original_templates = original.get("Templates") or {}
    def add(scope, units, files, helpers, uid):
        service = capture_service(scope, units, files, helpers, uid)
        if service["Artifacts"]:
            result.append(service)
    def template(name):
        with open(os.path.join(root, "config/systemd", name), "rb") as source:
            return source.read(1024 * 1024 + 1)
    runtime = installation["runtimeRoot"]
    if "power" in owners:
        helper = environment.get("SUBYARD_POWER_RECONCILER_PATH") or os.path.join(environment.get("SUBYARD_POWER_LIBEXEC_DIR") or "/usr/local/libexec/subyard", "yard-boot-reconcile")
        unit = environment.get("SUBYARD_POWER_UNIT_PATH") or "/etc/systemd/system/subyard-power-reconcile.service"
        if os.path.lexists(helper) or os.path.lexists(unit):
            import glob
            candidates = [environment.get("SUBYARD_DISPATCHER_PATH", ""), environment.get("SUBYARD_POWER_ENGINE_SOURCE", "")] + glob.glob(runtime + "/releases/*/bin/yard-engine")
            owned = False
            power_template = template("subyard-power-reconcile.service.in")
            original_helper = False
            candidate_helper = False
            if os.path.isfile(helper) and not os.path.islink(helper):
                with open(helper, "rb") as source:
                    digest = hashlib.sha256(source.read()).digest()
                original_helper = digest.hex() == original.get("EngineSHA256")
                candidate_helper = digest.hex() == original.get("ExecutingEngineSHA256")
                for candidate in candidates:
                    if os.path.isfile(candidate) and not os.path.islink(candidate):
                        with open(candidate, "rb") as source:
                            matches = hashlib.sha256(source.read()).digest() == digest
                            owned |= matches
            # Preserve the existing current-template proof first. A historical
            # fallback also covers interrupted replacement of unit and helper,
            # but only the sealed original or executing candidate helper.
            content = open(unit, "rb").read(1024 * 1024 + 1) if os.path.isfile(unit) else b""
            if (not producer_template_matches(content, power_template.replace(b"@SUBYARD_POWER_RECONCILER@", helper.encode()), True)
                    and (original_helper or candidate_helper) and original_templates):
                power_template = original_templates["subyard-power-reconcile.service.in"].encode()
            add("system", [os.path.basename(unit)], {unit: power_template.replace(b"@SUBYARD_POWER_RECONCILER@", helper.encode())}, {helper: owned}, 0)
    if "keys" in owners:
        directory = environment.get("SUBYARD_KEYS_SYSTEMD_DIR") or os.path.join(environment["SUBYARD_OPERATOR_HOME"], ".config/systemd/user")
        unit, timer = os.path.join(directory, "subyard-keys-sync.service"), os.path.join(directory, "subyard-keys-sync.timer")
        if os.path.lexists(unit) or os.path.lexists(timer):
            content = open(unit, "rb").read() if os.path.isfile(unit) else b""
            candidates = [os.path.join(root, ".build/yard"), os.path.join(root, "bin/yard"), os.path.join(runtime, "current/bin/yard")]
            service_template = original_templates.get("subyard-keys-sync.service.in")
            timer_template = original_templates.get("subyard-keys-sync.timer.in")
            expected = [(service_template.encode() if service_template is not None else template("subyard-keys-sync.service.in")).replace(b"@YARD_BIN@", path.encode()) for path in candidates]
            add("user", ["subyard-keys-sync.timer", "subyard-keys-sync.service"], {unit: next((value for value in expected if content == value), expected[-1]), timer: timer_template.encode() if timer_template is not None else template("subyard-keys-sync.timer.in")}, {}, installation["operatorUid"])
    if "relay" in owners:
        import glob
        paths = glob.glob("/etc/systemd/system/subyard-ssh-relay-*.service") + glob.glob("/etc/systemd/system/subyard-ssh-relay-*.socket")
        names = {os.path.basename(path).rsplit(".", 1)[0] for path in paths}
        for name in sorted(names):
            match = re.fullmatch(r"subyard-ssh-relay-([1-9][0-9]{0,4})", name)
            if not match or int(match[1]) > 65535:
                raise ValueError("unrecognized relay artifact")
            port = int(match[1])
            if request.get("Port") and port != request["Port"]:
                continue
            unit = "/etc/systemd/system/" + name + ".service"
            content = open(unit, "rb").read().decode()
            owner = re.search(r'^Environment="SUBYARD_RELAY_OWNER=([^"\r\n]+)"$', content, re.M)
            uid = re.search(r'^Environment="SUBYARD_RELAY_UID=([0-9]+)"$', content, re.M)
            marked = owner and uid
            if marked and (owner[1] != installation["dataHome"] or uid[1] != str(installation["operatorUid"])):
                continue
            target = re.search(r'^ExecStart=(/usr/lib/systemd/systemd-socket-proxyd|/lib/systemd/systemd-socket-proxyd) ([0-9.]+):22$', content, re.M)
            if not target:
                raise ValueError("unknown relay executable")
            import ipaddress
            address = str(ipaddress.IPv4Address(target[2]))
            if not marked and not any(yard["Context"]["yardKind"] == "vm" and yard["Context"]["sshPort"] == port and yard["Snapshot"]["State"]["Instance"]["devices"].get("eth0", {}).get("ipv4.address") == address for yard in request.get("Yards") or ()):
                raise ValueError("legacy relay has no exact yard/IP ownership")
            socket = f"[Unit]\nDescription=Subyard VM SSH loopback relay on port {port}\n\n[Socket]\nListenStream=127.0.0.1:{port}\nAccept=no\n\n[Install]\nWantedBy=sockets.target\n"
            expected = f"[Unit]\nDescription=Subyard VM SSH relay to {address}:22\nAfter=network-online.target\n\n[Service]\n"
            if marked:
                expected += f'Environment="SUBYARD_RELAY_OWNER={owner[1]}"\nEnvironment="SUBYARD_RELAY_UID={uid[1]}"\n'
            expected += f"ExecStart={target[1]} {address}:22\nNoNewPrivileges=yes\nPrivateTmp=yes\nProtectHome=yes\nProtectSystem=strict\nPrivateDevices=yes\nRestrictAddressFamilies=AF_INET AF_INET6\n"
            add("system", [name + ".socket", name + ".service"], {unit: expected.encode(), "/etc/systemd/system/" + name + ".socket": socket.encode()}, {}, 0)
    return result


def inventory():
    value = os.environ.get("SUBYARD_TEARDOWN_INVENTORY", "")
    if len(value.encode()) > 65536:
        raise ValueError("oversized teardown inventory")
    resources = json.loads(value)
    if not isinstance(resources, list) or len(resources) > 256:
        raise ValueError("invalid teardown inventory")
    seen = set()
    for resource in resources:
        if set(resource) - {"kind", "name", "pool", "binding", "path", "usedBy", "bindings", "owner"}:
            raise ValueError("invalid teardown resource fields")
        if resource.get("kind") not in ("instance", "profile", "image", "volume", "network", "pool", "project", "host-profile"):
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
    elif kind in ("profile", "host-profile"):
        path = "/1.0/profiles/" + name
        fields = ("name", "config", "devices", "description")
        if kind == "host-profile":
            project = "default"
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
    digest = hashlib.sha256(payload.encode()).hexdigest()
    variants = resource.get("bindings", []) if kind == "host-profile" else []
    if not isinstance(variants, list) or len(variants) > 3 or any(not isinstance(value, str) or not re.fullmatch(r"[a-f0-9]{64}", value) for value in variants):
        raise ValueError("invalid full default-profile retry bindings")
    if digest != resource["binding"] and digest not in variants:
        raise ValueError("teardown resource changed after confirmation")


def main():
    mode = sys.argv[1]
    if mode == "capture-producer":
        import pwd
        owner = os.environ.get("SUBYARD_USER") or os.environ.get("SUDO_USER") or pwd.getpwuid(os.getuid()).pw_name
        installation = {"operatorUid": pwd.getpwnam(owner).pw_uid, "dataHome": os.environ["SUBYARD_HOME"], "runtimeRoot": os.environ.get("YARD_RUNTIME_ROOT") or os.environ["SUBYARD_HOME"] + "/runtime"}
        print(json.dumps(core_producers({"Root": os.path.abspath(sys.argv[4]), "Installation": installation, "Environment": dict(os.environ), "Owners": [sys.argv[2]], "Port": int(sys.argv[3])})))
        return
    if mode == "producer-artifacts-json":
        print(json.dumps(list(owner_artifacts(services(None)).values())))
        return
    if mode == "capture-producers":
        print(json.dumps(core_producers(json.load(sys.stdin))))
        return
    if mode == "capture-artifact":
        item = json.loads(sys.stdin.buffer.read(65537))
        try:
            with artifact_parent(item["Path"]) as (fd, name):
                entries = artifact_entries(fd, name)
        except FileNotFoundError:
            if os.path.lexists(item["Path"]):
                raise ValueError("teardown artifact changed during observation")
            entries = []
        print(json.dumps(dict(Path=item["Path"], Binding=artifact_digest(entries) if entries else "", Entries=entries), separators=(",", ":")))
        return
    if mode == "service-inventory":
        for service in services(read_plan()[1] if os.environ.get("SUBYARD_TEARDOWN_PLAN") else None):
            for unit in service["units"]:
                print(service["scope"] + "\t" + unit)
        return
    if mode == "producer-artifacts":
        for path in owner_artifacts(services(read_plan()[1] if os.environ.get("SUBYARD_TEARDOWN_PLAN") else None)):
            print(path)
        return
    if mode == "service-fragment":
        if not sys.argv[2] or sys.argv[2] not in {item["Path"] for item in artifacts()}:
            raise ValueError("approved unit name resolves to an unapproved artifact")
        return
    if mode == "advance-epoch":
        advance_epoch()
        return
    if mode == "publish-plan":
        publish_plan()
        return
    if mode == "capture-assets":
        capture_assets()
        return
    if mode in ("prepare-startup", "commit-startup"):
        startup_edit(mode)
        return
    if mode == "retain-runtime":
        retain_runtime()
        return
    if mode in ("capture-tree", "check-tree", "remove-tree", "verify-tree"):
        file_tree(mode)
        return
    if mode in ("guard-artifacts", "guard-artifact", "remove-artifact"):
        approved = artifacts()
        if mode != "guard-artifacts":
            approved = [item for item in approved if item["Path"] == sys.argv[2]]
            if len(approved) != 1:
                raise ValueError("teardown artifact was not approved")
        for item in approved:
            guard_artifact(item, remove=mode == "remove-artifact")
        return
    if mode == "guard-all":
        stage, kind, ordinal = "inventory", "none", 0
        try:
            resources = inventory()
            stage = "project"
            project = sys.argv[2]
            if project in ("", "default"):
                raise ValueError("teardown requires a dedicated project")
            for ordinal, resource in enumerate(resources, 1):
                stage, kind = "resource", resource["kind"]
                guard(resource, project)
        except (ValueError, KeyError, TypeError, OSError, RecursionError, subprocess.CalledProcessError) as error:
            categories = (ValueError, KeyError, TypeError, OSError, PermissionError, FileNotFoundError, RecursionError, subprocess.CalledProcessError)
            details = dict(action=mode, stage=stage, kind=kind, ordinal=ordinal,
                           category=type(error).__name__ if type(error) in categories else "unknown")
            argument = error.args[0] if error.args else None
            # Only fixed structural labels cross this boundary, never native
            # query output, arbitrary exceptions, commands or resource values.
            if isinstance(error, KeyError) and isinstance(argument, str) and argument in (
                    "kind", "name", "pool", "binding", "config", "devices", "description", "profiles",
                    "type", "content_type", "created_at", "fingerprint", "managed", "driver"):
                details["field"] = argument
            if isinstance(error, ValueError) and isinstance(argument, str) and argument in (
                    "oversized teardown inventory", "invalid teardown inventory", "invalid teardown resource fields",
                    "invalid teardown resource kind", "invalid teardown resource identity", "invalid teardown binding",
                    "duplicate teardown resource", "teardown requires a dedicated project",
                    "invalid full default-profile retry bindings", "teardown resource changed after confirmation"):
                details["reason"] = argument
            if isinstance(error, subprocess.CalledProcessError) and isinstance(error.returncode, int):
                details["query_exit"] = error.returncode
            if isinstance(error, OSError) and isinstance(error.errno, int):
                details["errno"] = error.errno
            print("plan_stale: " + json.dumps(details, separators=(",", ":")), file=sys.stderr)
            raise
        return
    resources = inventory()
    if mode == "validate":
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
    except PermissionError:
        sys.exit(77 if sys.argv[1] in ("capture-tree", "capture-artifact") else 75)
    except (ValueError, KeyError, TypeError, OSError, RecursionError, subprocess.CalledProcessError):
        print("plan_stale: teardown inventory, artifact or resource binding is invalid", file=sys.stderr)
        sys.exit(75 if sys.argv[1] in ("guard-artifacts", "guard-artifact", "remove-artifact", "capture-tree", "check-tree", "remove-tree", "verify-tree", "publish-plan") else 1)
