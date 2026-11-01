#!/usr/bin/env python3
"""Private bounded evidence for the proxy publication fixture; never a product API."""
import base64
import binascii
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import select
import signal
import stat
import subprocess
import sys
import tarfile
import tempfile
import time

MIB = 1024 * 1024
TOTAL = 80 * MIB
ARCHIVE = 96 * MIB
PREFIX = b"SUBYARD_PUBLICATION_CAPTURE_"
STATE_FILES = (".marker", "phase", "source-bundle.sha256", "runtime-artifact.sha256",
               "runtime-files.expected.sha256", "publication-physical.jsonl",
               "publication-physical.uuid", "publication-physical.foreign.uuid",
               "callback-continuation.jsonl")
LOG_LABELS = {f"state-{i}" for i, name in enumerate(STATE_FILES) if name.endswith(".jsonl")}
RESOURCES = ("/usr/local/libexec/subyard/yard-boot-reconcile",
             "/etc/systemd/system/subyard-power-reconcile.service",
             "/etc/systemd/system/subyard-publication-reconcile.service")
LABELS = {f"state-{i}" for i in range(len(STATE_FILES))}
LABELS |= {f"resource-{i}" for i in range(6)}
LABELS |= {"journal", "ledger", "links", "manager", "journal-output", "instance", "foreign"}
INSTALLED_LOGS = ("install-a.log", "migrate-a.log", "ready-a.json", "image-info-help.txt",
                  "image-list-help.txt", "image-info.txt", "image-catalog.json", "init-a.log",
                  "start-a.log", "check-a.log", "source-a-manifest.before", "update-b.log",
                  "ready-b.json", "source-b-complete.json", "source-b-manifest.before",
                  "normal-rust-graph.log", "check-target.log", "fixture-assets.json",
                  "partial-setup.log", "partial-init.log", "partial-start.log", "partial-check.log",
                  "partial-stop.log", "partial-import.log", "partial-restart.log", "partial-rust-graph.log",
                  "partial-mount.json", "partial-instance.uuid", "partial-config-source.toml", "partial-rules-source.rules",
                  "partial-pending-journal.json", "partial-pending-capsule.tar", "partial-pending-facts.json")
INSTALLED = False

def installed_profile():
    # A second closed fixture schema uses the existing framing and durable sink.
    global INSTALLED, LABELS, LOG_LABELS
    INSTALLED = True
    LOG_LABELS = {f"log-{i}" for i in range(len(INSTALLED_LOGS))}
    LABELS = LOG_LABELS | {"state-0", "state-1", "journal", "ledger", "links", "capsule"}

def digest(data):
    return hashlib.sha256(data).hexdigest()

def ancestry(path):
    path = Path(os.path.abspath(path))
    chain = []
    for part in reversed((path.parent, *path.parent.parents)):
        info = part.lstat()
        if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode):
            raise ValueError("unsafe ancestor")
        chain.append((part, info.st_dev, info.st_ino))
    return chain

def unchanged(chain):
    return all((p.lstat().st_dev, p.lstat().st_ino) == (dev, ino)
               and stat.S_ISDIR(p.lstat().st_mode) and not p.is_symlink()
               for p, dev, ino in chain)

def read_file(path, limit, reference=None):
    """Capture through one retained FD; reject namespace/content changes."""
    try:
        chain = ancestry(path)
        fd = os.open(path, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW | os.O_NONBLOCK)
    except FileNotFoundError:
        return {"status": "absent"}, None
    except (OSError, ValueError):
        return {"status": "refused"}, None
    try:
        before = os.fstat(fd)
        if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1 or before.st_size > limit:
            return {"status": "refused"}, None
        chunks, size = [], 0
        while True:
            block = os.read(fd, min(65536, limit + 1 - size))
            if not block:
                break
            chunks.append(block)
            size += len(block)
            if size > limit:
                return {"status": "oversized"}, None
        data = b"".join(chunks)
        # Metadata timestamps can have coarse resolution. Re-read the same FD
        # as well, so same-length in-place changes do not pass that guard.
        os.lseek(fd, 0, os.SEEK_SET)
        check, checked = hashlib.sha256(), 0
        while True:
            block = os.read(fd, min(65536, limit + 1 - checked))
            if not block:
                break
            checked += len(block)
            if checked > limit:
                return {"status": "changed"}, None
            check.update(block)
        after = os.fstat(fd)
        current = os.stat(path, follow_symlinks=False)
        identity = lambda s: (s.st_dev, s.st_ino, s.st_mode, s.st_size, s.st_nlink, s.st_uid, s.st_gid,
                              s.st_mtime_ns, s.st_ctime_ns)
        if (identity(before) != identity(after) or identity(after) != identity(current)
                or len(data) != before.st_size or check.hexdigest() != digest(data)
                or not unchanged(chain)):
            return {"status": "changed"}, None
        info = {"status": "present", "size": len(data), "sha256": digest(data),
                "mode": stat.S_IMODE(before.st_mode), "device": before.st_dev,
                "inode": before.st_ino, "uid": before.st_uid, "gid": before.st_gid}
        if reference == info["sha256"]:
            info["status"] = "retained-engine-reference"
            return info, None
        return info, data
    except (OSError, ValueError):
        return {"status": "changed"}, None
    finally:
        os.close(fd)

def command(args, limit=512 * 1024):
    """Bound both bytes and time, and reap only this known diagnostic child."""
    try:
        child = subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                 start_new_session=True)
    except OSError:
        return {"status": "refused"}, None
    data, status = bytearray(), "present"
    deadline = time.monotonic() + 10
    try:
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0 or not select.select([child.stdout], [], [], max(0, remaining))[0]:
                status = "timeout"
                break
            block = os.read(child.stdout.fileno(), min(65536, limit + 1 - len(data)))
            if not block:
                break
            data.extend(block)
            if len(data) > limit:
                status = "oversized"
                break
        if status != "present":
            os.killpg(child.pid, signal.SIGKILL)
        code = child.wait(timeout=2)
        if code and status == "present":
            status = "command-failed"
        return {"status": status, "exit": code, "size": len(data),
                "sha256": digest(data)}, bytes(data)
    except (OSError, subprocess.SubprocessError):
        return {"status": "timeout", "size": len(data), "sha256": digest(data)}, bytes(data)
    finally:
        if child.poll() is None:
            os.killpg(child.pid, signal.SIGKILL)
            child.wait()
        child.stdout.close()

def exact_query(endpoint):
    info, raw = command(["curl", "--unix-socket", "/var/lib/incus/unix.socket",
        "--max-time", "10", "--silent", "--show-error", "--write-out",
        "\\nSUBYARD_HTTP_STATUS%{http_code}", "http://localhost/1.0/" + endpoint], MIB)
    if info["status"] != "present":
        raise ValueError("query refused")
    body, code = raw.rsplit(b"\nSUBYARD_HTTP_STATUS", 1)
    if code == b"404":
        return None
    if code != b"200":
        raise ValueError("query status")
    return json.loads(body)["metadata"]

def make_archive(records):
    assert set(records) <= LABELS
    size = sum(len(data) for _, data in records.values() if data is not None)
    if size > TOTAL:
        raise ValueError("total limit")
    manifest = {name: info for name, (info, _) in records.items()}
    complete = all(x["status"] in ("present", "absent", "retained-engine-reference", "diagnostic-exit")
                   for x in manifest.values())
    raw = io.BytesIO()
    with tarfile.open(fileobj=raw, mode="w:gz") as bundle:
        for name, data in [("manifest.json", json.dumps(manifest).encode())] + [
                (name, data) for name, (_, data) in records.items() if data is not None]:
            entry = tarfile.TarInfo(name)
            entry.size, entry.mode = len(data), 0o600
            bundle.addfile(entry, io.BytesIO(data))
    data = raw.getvalue()
    if len(data) > ARCHIVE:
        raise ValueError("archive limit")
    return data, complete

def validate_archive(data, engine_sha=None, token=None):
    if len(data) > ARCHIVE:
        raise ValueError("archive limit")
    # Bound decompression, including tar headers/padding.
    with gzip.GzipFile(fileobj=io.BytesIO(data)) as zipped:
        raw = zipped.read(TOTAL + 2 * MIB + 1)
    if len(raw) > TOTAL + 2 * MIB:
        raise ValueError("expanded limit")
    with tarfile.open(fileobj=io.BytesIO(raw), mode="r:") as bundle:
        members = bundle.getmembers()
        if (len(members) > len(LABELS) + 1 or any(not m.isfile() for m in members)
                or len({m.name for m in members}) != len(members)
                or any(m.name not in LABELS | {"manifest.json"} for m in members)):
            raise ValueError("members")
        manifest_member = bundle.getmember("manifest.json")
        if manifest_member.size > 65536:
            raise ValueError("manifest limit")
        try:
            manifest = json.load(bundle.extractfile(manifest_member))
        except (json.JSONDecodeError, UnicodeDecodeError):
            raise ValueError("manifest decode") from None
        if not isinstance(manifest, dict) or set(manifest) != LABELS:
            raise ValueError("manifest")
        allowed = {"present", "absent", "retained-engine-reference", "refused", "changed",
                   "oversized", "timeout", "command-failed", "diagnostic-exit"}
        for name, info in manifest.items():
            if not isinstance(info, dict) or info.get("status") not in allowed:
                raise ValueError("status")
            if info["status"] in ("present", "diagnostic-exit", "retained-engine-reference"):
                if (type(info.get("size")) is not int or info["size"] < 0
                        or not isinstance(info.get("sha256"), str)
                        or not re.fullmatch(r"[0-9a-f]{64}", info["sha256"])):
                    raise ValueError("missing original bytes")
            if info["status"] == "absent" and ("size" in info or "sha256" in info):
                raise ValueError("contradictory absence")
            if info["status"] == "retained-engine-reference" and (
                    name != "resource-0" or not engine_sha or info.get("sha256") != engine_sha):
                raise ValueError("unbound engine reference")
        actual = {m.name for m in members} - {"manifest.json"}
        expected = {name for name, info in manifest.items()
                    if "size" in info and info.get("status") != "retained-engine-reference"}
        if actual != expected:
            raise ValueError("incomplete members")
        if manifest["state-0"]["status"] != "present":
            raise ValueError("missing marked identity")
        marker = bundle.extractfile("state-0").read(1025)
        prefix = "subyard-rust-owner-" if INSTALLED else "subyard-p0-power-systemd-"
        if not re.fullmatch(prefix.encode() + rb"[1-9][0-9]{0,19}\n", marker):
            raise ValueError("marked identity")
        if token and marker != (prefix + token + "\n").encode():
            raise ValueError("foreign marked identity")
        if INSTALLED and (manifest["state-1"]["status"] != "present"
                or bundle.extractfile("state-1").read(1025) != marker.rstrip(b"\n") + b":success\n"):
            raise ValueError("foreign fixture identity")
        total = 0
        for name in actual:
            limit = 64 * MIB if name == "resource-0" or INSTALLED and name == "capsule" else 4 * MIB if name in LOG_LABELS else MIB
            if bundle.getmember(name).size > limit:
                raise ValueError("file limit")
            content = bundle.extractfile(name).read()
            total += len(content)
            if (len(content) != manifest[name]["size"]
                    or digest(content) != manifest[name]["sha256"] or total > TOTAL):
                raise ValueError("content")
        complete = all(info.get("status") in ("present", "absent", "retained-engine-reference", "diagnostic-exit")
                       for info in manifest.values())
        if INSTALLED and complete:
            raw_journal = bundle.extractfile("journal").read() if manifest["journal"]["status"] == "present" else None
            journal = installed_journal(manifest["journal"], raw_journal)
            if journal is None:
                if manifest["capsule"]["status"] != "absent":
                    raise ValueError("unexpected transaction capsule")
            elif manifest["capsule"]["status"] != "present":
                raise ValueError("missing original transaction capsule")
            else:
                validate_installed_capsule(bundle.extractfile("capsule").read(), journal, manifest["journal"]["uid"])
        return complete

def emit(data, complete, stream):
    sha = digest(data)
    stream.write(PREFIX + f"BEGIN {len(data)} {sha} {int(complete)}\n".encode())
    encoded = base64.b64encode(data)
    for offset in range(0, len(encoded), 76):
        stream.write(PREFIX + b"DATA " + encoded[offset:offset + 76] + b"\n")
    stream.write(PREFIX + b"END " + sha.encode() + b"\n")
    stream.flush()

def receive(stream, output, passthrough, engine_sha=None, token=None):
    output = Path(output)
    chain = ancestry(output)
    parent = output.parent.stat()
    if parent.st_uid != os.getuid() or stat.S_IMODE(parent.st_mode) != 0o700:
        raise ValueError("private output directory required")
    frame, header, ended, ordinary_bytes, wire_bytes = bytearray(), None, False, 0, 0
    for line in iter(lambda: stream.readline(65537), b""):
        if len(line) > 65536:
            raise ValueError("line limit")
        if line.startswith(PREFIX):
            wire_bytes += len(line)
            if header and wire_bytes > 2 * header[0] + 4096:
                raise ValueError("encoded frame limit")
            if line.startswith(PREFIX + b"BEGIN "):
                if header is not None:
                    raise ValueError("duplicate frame")
                match = re.fullmatch(PREFIX + rb"BEGIN ([0-9]{1,9}) ([0-9a-f]{64}) ([01])\n", line)
                if not match or int(match[1]) > ARCHIVE:
                    raise ValueError("header")
                header = (int(match[1]), match[2].decode(), match[3] == b"1")
            elif line.startswith(PREFIX + b"DATA ") and header and not ended:
                try:
                    block = base64.b64decode(line[len(PREFIX) + 5:].strip(), validate=True)
                except binascii.Error:
                    raise ValueError("base64 decode") from None
                if not block or len(block) > 57 or len(frame) + len(block) > header[0]:
                    raise ValueError("data limit")
                frame.extend(block)
            elif line == PREFIX + b"END " + (header[1].encode() if header else b"") + b"\n" and not ended:
                ended = True
            else:
                raise ValueError("frame")
        elif header is not None and not ended:
            raise ValueError("interrupted frame")
        else:
            ordinary_bytes += len(line)
            if ordinary_bytes > 8 * MIB:
                raise ValueError("ordinary stdout limit")
            passthrough.write(line)
            passthrough.flush()
    if header is None:
        return False
    if not ended or len(frame) != header[0] or digest(frame) != header[1]:
        raise ValueError("truncated frame")
    complete = validate_archive(frame, engine_sha, token)
    if complete != header[2] or not unchanged(chain):
        raise ValueError("manifest status")
    directory = os.open(output.parent, os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC)
    try:
        retained = os.fstat(directory)
        if (retained.st_dev, retained.st_ino) != (parent.st_dev, parent.st_ino) or not unchanged(chain):
            raise ValueError("changed output directory")
        fd = os.open(output.name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                     0o600, dir_fd=directory)
        try:
            with os.fdopen(fd, "wb") as sink:
                os.fchmod(sink.fileno(), 0o600)
                sink.write(frame)
                sink.flush()
                os.fsync(sink.fileno())
            os.fsync(directory)
            if not unchanged(chain):
                raise ValueError("changed output ancestry")
        except BaseException:
            # A partial file never serves as acknowledgement.
            raise
    finally:
        os.close(directory)
    if not complete:
        raise ValueError("partial capture retained")
    return True

def collect(root, token, address_unit):
    if not re.fullmatch(r"[1-9][0-9]{0,19}", token) or os.geteuid() != 0:
        raise ValueError("allocated fixture required")
    marker = "subyard-p0-power-systemd-" + token
    state = Path("/var/tmp") / marker
    home = Path("/home") / ("subyardpower" + token)
    info, raw = read_file(state / ".marker", 1024)
    if info["status"] != "present" or raw != (marker + "\n").encode():
        raise ValueError("unowned state")
    records = {}
    for index, name in enumerate(STATE_FILES):
        records[f"state-{index}"] = read_file(state / name, 4 * MIB if name.endswith(".jsonl") else MIB)
    reference = read_file(Path(root) / ".subyard-acceptance/runtime/bin/yard-engine", 64 * MIB)
    reference_sha = reference[0].get("sha256") if reference[0]["status"] == "present" else None
    paths = list(map(Path, RESOURCES))
    paths += [p.parent / ("." + p.name + ".native-custody.json") for p in paths]
    for index, path in enumerate(paths):
        records[f"resource-{index}"] = read_file(path, 64 * MIB if index == 0 else MIB,
                                                reference_sha if index == 0 else None)
    home_info, home_marker = read_file(home / ".subyard-p0-power-systemd-home", 1024)
    home_owned = home_info["status"] == "present" and home_marker == (marker + "\n").encode()
    home_absent = not home.exists() and not home.is_symlink()
    for name in ("journal", "ledger"):
        records[name] = (read_file(home / ".config/subyard/release-transition/v2" / (name + ".json"), MIB)
                         if home_owned else ({"status": "absent" if home_absent else "refused"}, None))
    links = {}
    for name in ("current", "previous"):
        path = home / ".subyard/runtime" / name
        try:
            if not home_owned:
                links[name] = {"status": "absent" if home_absent else "refused"}
                continue
            ancestry(path)
            links[name] = {"status": "present", "target": os.readlink(path)}
        except FileNotFoundError:
            links[name] = {"status": "absent"}
        except OSError:
            links[name] = {"status": "refused"}
    data = json.dumps(links).encode()
    records["links"] = ({"status": "refused" if any(v["status"] == "refused" for v in links.values())
                         else "present", "size": len(data), "sha256": digest(data)}, data)
    units = ["subyard-publication-reconcile.service", "subyard-power-reconcile.service", address_unit]
    if address_unit != marker + "-address.service":
        raise ValueError("address unit")
    records["manager"] = command(["systemctl", "show", *units, "-p", "LoadState", "-p", "ActiveState",
        "-p", "SubState", "-p", "MainPID", "-p", "ControlGroup", "-p", "InvocationID",
        "-p", "Job", "-p", "Result", "-p", "ExecMainStatus"])
    records["journal-output"] = command(["journalctl", "-b", *sum((["-u", u] for u in units), []),
                                         "--no-pager", "-n", "100", "-o", "cat"])
    # A fully captured nonzero diagnostic exit is itself original evidence,
    # not a successful property observation. Timeouts/truncation still refuse.
    for label in ("manager", "journal-output"):
        if records[label][0]["status"] == "command-failed":
            records[label][0]["status"] = "diagnostic-exit"
    # Do not widen queries if the fixture's original UUID was never captured.
    for label, project, instance, key, uuid_index in [
            ("instance", "subyard", "yard", "user.subyard.p0-power-systemd", 6),
            ("foreign", "sypub-foreign-" + token, "control", "user.subyard.p0-publication-foreign", 7)]:
        try:
            project_info = exact_query("projects/" + project)
            if project_info is None:
                records[label] = ({"status": "absent"}, None)
                continue
            if project_info["config"].get(key) != marker:
                raise ValueError("project marker")
            uuid_info, uuid = records[f"state-{uuid_index}"]
            if uuid_info["status"] != "present" or not uuid.strip():
                raise ValueError("original UUID absent")
            value = exact_query("instances/" + instance + "?project=" + project)
            if value is None:
                records[label] = ({"status": "absent"}, None)
                continue
            config = value["config"]
            if not isinstance(config, dict) or not isinstance(value["devices"], dict):
                raise ValueError("metadata shape")
            if config.get("volatile.uuid") != uuid.decode().strip():
                raise ValueError("UUID")
            if label == "foreign" and config.get("user.subyard.e2e") != marker:
                raise ValueError("marker")
            selected_config = {k: v for k, v in config.items() if k == "volatile.uuid"
                or k == "user.subyard.desired_power" or k.startswith("user.subyard.publication_handoff.")
                or k.startswith("user.subyard.deferred_publication.")
                or k in ("user.subyard.preview_proxy", "user.subyard.preview_endpoint_sha256")}
            fields = {"type", "listen", "connect", "bind", "nat", "proxy_protocol"}
            devices = {name: {k: v for k, v in device.items() if k in fields}
                       for name, device in value["devices"].items() if device.get("type") == "proxy"}
            data = json.dumps({"name": instance, "config": selected_config, "devices": devices,
                               "status": value.get("status"), "type": value.get("type")}).encode()
            records[label] = ({"status": "present", "size": len(data), "sha256": digest(data)}, data)
        except (ValueError, KeyError, TypeError):
            records[label] = ({"status": "refused"}, None)
    return make_archive(records)

def installed_journal(info, raw, owner=None):
    if info["status"] == "absent":
        return None
    if (info["status"] != "present" or info.get("mode") != 0o600
            or type(info.get("uid")) is not int or info["uid"] < 0
            or owner is not None and info.get("uid") != owner):
        raise ValueError("unsafe journal")
    value = json.loads(raw)
    if not isinstance(value, dict):
        raise ValueError("journal object")
    safe = lambda s: isinstance(s, str) and re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}", s) and s not in (".", "..")
    token = lambda prefix, s: isinstance(s, str) and re.fullmatch(prefix + r"[0-9a-f]{64}", s)
    if (value.get("schemaVersion") != 2 or not safe(value.get("transaction"))
            or value.get("checkpoint") not in ("authorized", "migrating", "activation-intent", "target-active", "reconciling", "complete")
            or not isinstance(value.get("goal"), dict) or not isinstance(value.get("releases"), dict)
            or not safe(value["goal"].get("target")) or value["goal"]["target"] != value["releases"].get("target")
            or value["goal"].get("direction") not in ("activate-target", "activate-previous")
            or not safe(value["releases"].get("from"))
            or value["releases"].get("previous") is not None and not safe(value["releases"]["previous"])
            or not token("plan-v1-", value.get("authorizationPlan"))
            or not token("resume-v1-", value.get("resumePlan"))):
        raise ValueError("journal identity")
    for key in ("artifactDigest", "registryDigest", "catalogDigest", "observationScope", "authorizationDigest", "intentDigest"):
        if not token("", value.get(key)):
            raise ValueError("journal binding")
    steps = value.get("steps") or []
    if not isinstance(steps, list) or len(steps) > 256 or any(not isinstance(s, dict) or not safe(s.get("id")) for s in steps):
        raise ValueError("journal steps")
    if len({s["id"] for s in steps}) != len(steps):
        raise ValueError("duplicate journal step")
    return value

def installed_tx_name(kind, name, steps):
    # Both publishers stage only .<final>.pending; other dotted variants refuse.
    if name.startswith(".") and name.endswith(".pending"):
        name = name[1:-len(".pending")]
    if kind == "custody":
        part = re.fullmatch(r"bundle\.([0-9]{3})\.json", name)
        if name == "bundle.json" or part and int(part[1]) < 128:
            return True
        fact = re.fullmatch(r"([0-9]{3,4})\.(.+)\.json", name)
        if not fact or not 1 <= int(fact[1]) <= 4096 or fact[1] != f"{int(fact[1]):03}":
            return False
        leaf = fact[2]
        if leaf in ("intent", "accepted", "terminal", "reservation", "closure", "recovery",
                    "after-owner", "restoration", "host-power-installation", "physical"):
            return True
        nested = re.fullmatch(r"(command|operation)-([0-9]{3})-(intent|accepted|terminal|create-intent|create-result)", leaf)
        if nested:
            return (1 <= int(nested[2]) <= 256
                    and (nested[1] == "operation" or nested[3] != "accepted"))
        cache = re.fullmatch(r"image-cache-([0-9]{3})", leaf)
        return bool(cache and 1 <= int(cache[1]) <= 64)
    if kind == "evidence":
        return any(name == step + "." + suffix + ".json" for step in steps for suffix in ("captured", "applied", "verified"))
    return kind == "recovery" and any(name == step + ".json" for step in steps)

def validate_installed_capsule(data, journal, owner):
    with tarfile.open(fileobj=io.BytesIO(data), mode="r:") as archive:
        members = archive.getmembers()
        if len(members) > 513 or len({m.name for m in members}) != len(members) or any(not m.isfile() for m in members):
            raise ValueError("capsule members")
        member = archive.getmember("manifest.json")
        if member.size > 256 * 1024:
            raise ValueError("capsule manifest bound")
        value = json.load(archive.extractfile(member))
        if (not isinstance(value, dict) or not isinstance(value.get("directories"), dict)
                or value.get("transaction") != journal["transaction"] or set(value["directories"]) != {"custody", "evidence", "recovery"}
                or any(v not in ("present", "absent") for v in value["directories"].values())
                or not isinstance(value.get("files"), dict)):
            raise ValueError("capsule identity")
        if {m.name for m in members} != set(value["files"]) | {"manifest.json"}:
            raise ValueError("capsule missing bytes")
        steps = {s["id"] for s in journal.get("steps") or []}
        total = 0
        for name, info in value["files"].items():
            kind, separator, leaf = name.partition("/")
            if not separator or not installed_tx_name(kind, leaf, steps) or value["directories"].get(kind) != "present":
                raise ValueError("capsule file scope")
            entry = archive.getmember(name)
            if (entry.size > 4 * MIB or not isinstance(info, dict) or info.get("status") != "present" or info.get("mode") != 0o600
                    or info.get("uid") != owner):
                raise ValueError("capsule file identity")
            raw = archive.extractfile(entry).read()
            total += len(raw)
            if len(raw) != info.get("size") or digest(raw) != info.get("sha256") or total > 60 * MIB:
                raise ValueError("capsule content")

def installed_capsule(config, journal):
    """Only three closed directories in the journal-selected original transaction."""
    transaction = journal["transaction"]
    steps = {s["id"] for s in journal.get("steps") or []}
    directories = [
        ("custody", config / "migrations/native-custody/v1" / transaction),
        ("evidence", config / "release-transition/v2/transactions" / transaction / "evidence"),
        ("recovery", config / "release-transition/v2/transactions" / transaction / "recovery")]
    contents, facts, states, total = {}, {}, {}, 0
    for kind, directory in directories:
        try:
            chain = ancestry(directory / "probe")
            before = directory.lstat()
        except FileNotFoundError:
            states[kind] = "absent"
            continue
        if (not stat.S_ISDIR(before.st_mode) or before.st_uid != os.getuid()
                or stat.S_IMODE(before.st_mode) != 0o700):
            raise ValueError("private transaction required")
        names = sorted(os.listdir(directory))
        if len(names) + len(facts) > 512:
            raise ValueError("transaction count")
        for name in names:
            if not installed_tx_name(kind, name, steps):
                raise ValueError("unrecognized original transaction file")
            info, data = read_file(directory / name, 4 * MIB)
            if info["status"] != "present" or info["uid"] != os.getuid() or info["mode"] != 0o600:
                raise ValueError("unsafe original transaction file")
            total += len(data)
            if total > 60 * MIB:
                raise ValueError("transaction byte bound")
            key = kind + "/" + name
            facts[key], contents[key] = info, data
        after = directory.lstat()
        if ((before.st_dev, before.st_ino) != (after.st_dev, after.st_ino)
                or sorted(os.listdir(directory)) != names or not unchanged(chain)):
            raise ValueError("transaction changed")
        states[kind] = "present"
    raw = io.BytesIO()
    with tarfile.open(fileobj=raw, mode="w:") as archive:
        metadata = json.dumps({"transaction": transaction, "directories": states, "files": facts}).encode()
        for name, data in [("manifest.json", metadata)] + list(contents.items()):
            item = tarfile.TarInfo(name)
            item.size, item.mode = len(data), 0o600
            archive.addfile(item, io.BytesIO(data))
    data = raw.getvalue()
    if len(data) > 64 * MIB:
        raise ValueError("capsule bound")
    return {"status": "present", "size": len(data), "sha256": digest(data)}, data

def collect_installed(token):
    if not re.fullmatch(r"[1-9][0-9]{0,19}", token):
        raise ValueError("allocation token")
    root = Path("/var/tmp") / ("subyard-rust-owner-" + token)
    fixture = root / "success"
    for path in (root, fixture, fixture / "config"):
        ancestry(path / "probe")
        info = path.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700:
            raise ValueError("private fixture required")
    records = {name: ({"status": "absent"}, None) for name in LABELS}
    for label, path, wanted in (
            ("state-0", root / ".subyard-p0-marker", "subyard-rust-owner-" + token + "\n"),
            ("state-1", fixture / ".subyard-p0-marker", "subyard-rust-owner-" + token + ":success\n")):
        records[label] = read_file(path, 1024)
        if (records[label][0]["status"] != "present" or records[label][1] != wanted.encode()
                or records[label][0]["mode"] != 0o600 or records[label][0]["uid"] != os.getuid()):
            raise ValueError("fixture marker")
    for index, name in enumerate(INSTALLED_LOGS):
        records[f"log-{index}"] = read_file(fixture / name, 4 * MIB)
    config = fixture / "config"
    for label in ("journal", "ledger"):
        records[label] = read_file(config / "release-transition/v2" / (label + ".json"), MIB)
    try:
        journal = installed_journal(*records["journal"], owner=os.getuid())
        if journal is not None:
            records["capsule"] = installed_capsule(config, journal)
    except (OSError, ValueError, KeyError, TypeError, json.JSONDecodeError):
        records["capsule"] = ({"status": "refused"}, None)
    try:
        runtime = fixture / "data/runtime"
        links = {}
        for name in ("current", "previous"):
            path = runtime / name
            try:
                chain = ancestry(path)
                before = path.lstat()
            except FileNotFoundError:
                links[name] = {"status": "absent"}
                continue
            target = os.readlink(path)
            if not stat.S_ISLNK(before.st_mode) or not re.fullmatch(r"releases/[A-Za-z0-9][A-Za-z0-9_.+-]{0,160}", target):
                raise ValueError("runtime link")
            after = path.lstat()
            if (before.st_dev, before.st_ino, before.st_mtime_ns) != (after.st_dev, after.st_ino, after.st_mtime_ns) or not unchanged(chain):
                raise ValueError("runtime link changed")
            manifest_info, _ = read_file(runtime / target / "runtime-files.sha256", MIB)
            if manifest_info["status"] != "present":
                raise ValueError("linked runtime manifest unavailable")
            links[name] = {"status": "present", "target": target, "device": before.st_dev,
                           "inode": before.st_ino, "manifest": manifest_info}
        data = json.dumps(links).encode()
        records["links"] = ({"status": "present", "size": len(data), "sha256": digest(data)}, data)
    except (OSError, ValueError):
        records["links"] = ({"status": "refused"}, None)
    # A concurrent journal replacement cannot silently select a different capsule.
    if read_file(config / "release-transition/v2/journal.json", MIB) != records["journal"]:
        records["capsule"] = ({"status": "changed"}, None)
    return make_archive(records)


def private_directory(root, requested):
    if requested:
        result = Path(requested)
    else:
        base = Path(root) / ".build"
        ancestry(base)
        base.mkdir(exist_ok=True)
        ancestry(base / "probe")
        result = Path(tempfile.mkdtemp(prefix="publication-evidence-", dir=base))
        result.chmod(0o700)
    ancestry(result / "probe")
    info = result.stat()
    if info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700:
        raise ValueError("private directory")
    return result

def retained_engine(bundle):
    # The caller's existing build_bundle has already checked the transport SHA.
    with tarfile.open(bundle, "r:gz") as archive:
        try:
            entry = archive.getmember(".subyard-acceptance/candidate.json")
        except KeyError:
            return ""  # Ordinary reboot mode packages on the guest; capture actual bytes.
        if not entry.isfile() or entry.size > 4096:
            raise ValueError("candidate metadata")
        version = json.load(archive.extractfile(entry))["version"]
        if not re.fullmatch(r"[A-Za-z0-9._+-]{1,100}", version):
            raise ValueError("candidate version")
        entry = archive.getmember(f".subyard-acceptance/release/yard-{version}-linux-amd64")
        if not entry.isfile() or entry.size > 64 * MIB:
            raise ValueError("retained engine")
        return digest(archive.extractfile(entry).read())

# Fixed refusal codes: 1=other value, 2=OS, 3=key, 4=type, 5=tar, 6=EOF,
# 7=subprocess, 8=incomplete collection. Append literal reasons to preserve codes.
REFUSAL_REASONS = (
    'unsafe ancestor', 'query refused', 'query status',
    'total limit', 'archive limit', 'expanded limit',
    'members', 'manifest limit', 'manifest',
    'status', 'missing original bytes', 'contradictory absence',
    'unbound engine reference', 'incomplete members', 'missing marked identity',
    'marked identity', 'foreign marked identity', 'foreign fixture identity',
    'file limit', 'content', 'unexpected transaction capsule',
    'missing original transaction capsule', 'private output directory required', 'line limit',
    'encoded frame limit', 'duplicate frame', 'header',
    'data limit', 'frame', 'interrupted frame',
    'ordinary stdout limit', 'truncated frame', 'manifest status',
    'changed output directory', 'changed output ancestry', 'partial capture retained',
    'allocated fixture required', 'unowned state', 'address unit',
    'project marker', 'original UUID absent', 'metadata shape',
    'UUID', 'marker', 'unsafe journal',
    'journal object', 'journal identity', 'journal binding',
    'journal steps', 'duplicate journal step', 'capsule members',
    'capsule manifest bound', 'capsule identity', 'capsule missing bytes',
    'capsule file scope', 'capsule file identity', 'capsule content',
    'private transaction required', 'transaction count', 'unrecognized original transaction file',
    'unsafe original transaction file', 'transaction byte bound', 'transaction changed',
    'capsule bound', 'allocation token', 'private fixture required',
    'fixture marker', 'runtime link', 'runtime link changed',
    'linked runtime manifest unavailable', 'private directory', 'candidate metadata',
    'candidate version', 'retained engine', 'mode',
    'base64 decode', 'manifest decode',
)

def refusal_reason(error):
    if isinstance(error, ValueError):
        if len(error.args) == 1 and type(error.args[0]) is str and error.args[0] in REFUSAL_REASONS:
            return 100 + REFUSAL_REASONS.index(error.args[0])
        return 1
    for code, kind in ((2, OSError), (3, KeyError), (4, TypeError),
                       (5, tarfile.TarError), (6, EOFError), (7, subprocess.SubprocessError)):
        if isinstance(error, kind):
            return code
    return 0


def main():
    # Stages: 0=dispatch/profile, 1=receive, 2=directory, 3=engine, 4=collect, 5=emit.
    stage = 0
    try:
        if sys.argv[1] in ("installed-collect", "installed-receive"):
            installed_profile()
            sys.argv[1] = sys.argv[1].removeprefix("installed-")
        if sys.argv[1] == "receive":
            stage = 1
            try:
                receive(sys.stdin.buffer, sys.argv[2], sys.stdout.buffer, sys.argv[3] or None, sys.argv[4])
            except (OSError, ValueError, KeyError, TypeError, tarfile.TarError, EOFError):
                # Keep the existing SSH/workload exit; closing its stdout early
                # would manufacture SIGPIPE instead of the original outcome.
                while sys.stdin.buffer.read(65536):
                    pass
                raise
        elif sys.argv[1] == "directory":
            stage = 2
            print(private_directory(*sys.argv[2:]))
        elif sys.argv[1] == "engine-hash":
            stage = 3
            print(retained_engine(sys.argv[2]))
        elif sys.argv[1] == "collect":
            stage = 4
            data, complete = (collect_installed if INSTALLED else collect)(*sys.argv[2:])
            stage = 5
            emit(data, complete, sys.stdout.buffer)
            if not complete:
                print("publication capture refused or incomplete stage=5 reason=8", file=sys.stderr)
                return 3
        else:
            raise ValueError("mode")
        return 0
    except (OSError, ValueError, KeyError, TypeError, tarfile.TarError, EOFError, subprocess.SubprocessError) as error:
        print(f"publication capture refused or incomplete stage={stage} reason={refusal_reason(error)}", file=sys.stderr)
        return 3

if __name__ == "__main__":
    raise SystemExit(main())

