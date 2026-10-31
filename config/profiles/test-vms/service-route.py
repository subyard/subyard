#!/usr/bin/env python3
"""Resolve only the public owner service publication; never inspect owner config."""
import argparse
import base64
import ipaddress
import json
import os
import pathlib
import re
import stat
import sys


def resolve(root, owner="", pool=""):
    root = pathlib.Path(root)
    if not root.is_absolute() or ".." in root.parts:
        raise ValueError("service_unavailable: unsafe publication root")
    for parent in root.parents:
        if not stat.S_ISDIR(parent.lstat().st_mode):
            raise ValueError("service_unavailable: unsafe publication ancestor")
    base = root.lstat()
    if not stat.S_ISDIR(base.st_mode) or base.st_mode & 0o022:
        raise ValueError("service_unavailable: unsafe publication root")
    uid = base.st_uid
    if uid not in (0, os.getuid()):
        # Host owner UIDs can be unmapped inside an unprivileged consumer.
        # Only the exact read-only mount establishes authority in that case.
        mounts = pathlib.Path("/proc/self/mountinfo").read_text().splitlines()
        if not any(len(fields := line.split()) > 5 and fields[4] == str(root)
                   and "ro" in fields[5].split(",") for line in mounts):
            raise ValueError("service_unavailable: publication owner is untrusted")
    for path in (root / "services", root / "services/test-vms"):
        info = path.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != uid or stat.S_IMODE(info.st_mode) != 0o755:
            raise ValueError("service_unavailable: unsafe publication directory")
    records = []
    required = {"schema_version", "owner", "service", "pool", "yard", "project",
                "instance", "runtime_uuid", "registration_generation"}
    for path in sorted((root / "services/test-vms").iterdir()):
        if path.name.startswith(".pending-"):
            continue
        descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(descriptor, "rb") as source:
            info = os.fstat(source.fileno())
            if (not stat.S_ISREG(info.st_mode) or info.st_uid != uid or
                    stat.S_IMODE(info.st_mode) != 0o644 or info.st_nlink != 1 or info.st_size > 16384):
                raise ValueError("service_unavailable: unsafe service publication")
            record = json.loads(source.read(16385))
        if (set(record) != required or record["schema_version"] != 1 or record["service"] != "test-vms" or
                any(not isinstance(record[key], str) or not re.fullmatch(r"[a-z0-9][a-z0-9_-]*", record[key])
                    for key in ("pool", "yard", "project", "instance")) or
                not isinstance(record["owner"], str) or not re.fullmatch(r"[A-Za-z0-9_][A-Za-z0-9_.-]*", record["owner"]) or
                not isinstance(record["runtime_uuid"], str) or
                not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}", record["runtime_uuid"]) or
                not re.fullmatch(r"[0-9a-f]{64}", record["registration_generation"]) or
                path.name != record["pool"] + ".json"):
            raise ValueError("service_unavailable: invalid service publication")
        if owner and record["owner"] != owner:
            continue
        if pool and record["pool"] != pool:
            continue
        records.append(record)
    if not records:
        raise ValueError("service_absent: no published owner test VM service")
    if len(records) != 1:
        raise ValueError("service_ambiguous: select an exact --owner and --pool")
    return records[0]


def route_generation(root, record):
    root = pathlib.Path(root)
    uid = root.lstat().st_uid
    yard = root / record["yard"]
    info = yard.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != uid or stat.S_IMODE(info.st_mode) != 0o755:
        raise ValueError("service_unavailable: unsafe route directory")
    link = yard / "current"
    info = link.lstat()
    if not stat.S_ISLNK(info.st_mode) or info.st_uid != uid:
        raise ValueError("service_unavailable: unsafe route publication")
    generation = os.readlink(link)
    if not re.fullmatch(r"\.route-[A-Za-z0-9_-]+", generation):
        raise ValueError("service_unavailable: unsafe route generation")
    directory = yard / generation
    info = directory.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != uid or stat.S_IMODE(info.st_mode) != 0o755:
        raise ValueError("service_unavailable: unsafe route generation directory")
    payloads = {}
    for name in ("route.tsv", "known_hosts"):
        descriptor = os.open(directory / name, os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(descriptor, "r") as source:
            info = os.fstat(source.fileno())
            if (not stat.S_ISREG(info.st_mode) or info.st_uid != uid or info.st_nlink != 1 or
                    stat.S_IMODE(info.st_mode) != 0o644 or info.st_size > 16384):
                raise ValueError("service_unavailable: unsafe route artifact")
            payloads[name] = source.read(16385)
    lines = payloads["route.tsv"].splitlines()
    if len(lines) != 4 or lines[0] != "subyard-e2e-route-v1":
        raise ValueError("service_unavailable: invalid route schema")
    values = {}
    for line in lines[1:]:
        fields = line.split("\t")
        if len(fields) != 2 or fields[0] in values:
            raise ValueError("service_unavailable: invalid route fields")
        values[fields[0]] = fields[1]
    if (set(values) != {"hostname", "port", "host_key_alias"} or values["port"] != "22" or
            values["host_key_alias"] != "subyard-e2e-bastion"):
        raise ValueError("service_unavailable: invalid route values")
    ipaddress.IPv4Address(values["hostname"])
    fields = payloads["known_hosts"].split()
    if len(fields) != 3 or fields[:2] != ["subyard-e2e-bastion", "ssh-ed25519"]:
        raise ValueError("service_unavailable: invalid route host-key pin")
    key = base64.b64decode(fields[2], validate=True)
    prefix = b"\x00\x00\x00\x0bssh-ed25519\x00\x00\x00\x20"
    if len(key) != len(prefix) + 32 or not key.startswith(prefix):
        raise ValueError("service_unavailable: invalid route host-key pin")
    return generation


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--registry", required=True)
    parser.add_argument("--owner", default="")
    parser.add_argument("--pool", default="")
    args = parser.parse_args()
    try:
        try:
            record = resolve(args.registry, args.owner, args.pool)
        except FileNotFoundError:
            raise ValueError("service_absent (operator registration required)") from None
        generation = route_generation(args.registry, record)
    except (OSError, ValueError, TypeError, KeyError) as error:
        # Do not expose file payloads, paths, or parser excerpts.
        message = str(error) if isinstance(error, ValueError) and str(error).startswith("service_") else "service_unavailable: invalid publication"
        sys.exit("test environment unavailable: " + message)
    print("\t".join(record[key] for key in ("owner", "pool", "yard", "registration_generation", "runtime_uuid")) + "\t" + generation)


if __name__ == "__main__":
    main()
