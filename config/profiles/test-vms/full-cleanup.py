#!/usr/bin/env python3
"""Assess the existing host-wide sink; never select a profile or mutate it."""
import glob
import hashlib
import importlib.util
import json
import os
import pathlib
import re
import stat
import sys
import subprocess


sys.dont_write_bytecode = True
ROOT = pathlib.Path(__file__).resolve().parents[3]
spec = importlib.util.spec_from_file_location("teardown_guard", ROOT / "scripts/lib/teardown-plan.py")
guard = importlib.util.module_from_spec(spec)
spec.loader.exec_module(guard)


def binding(path):
    with guard.artifact_parent(str(path)) as (parent, name):
        return guard.artifact_digest(guard.artifact_entries(parent, name))


def owned_regular(path, mode, uid):
    info = os.lstat(path)
    if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != mode or info.st_uid != uid:
        raise ValueError("unsafe sink artifact")
    return binding(path)


def evidence_roots(home):
    return [home + "/logs/.test-vms-broker-events", home + "/logs/test-vms-broker-incidents", home + "/logs/test-vms-broker.jsonl", home + "/.test-vms-broker-sink.lock"]


def producer_inventory(environment, inventory=None):
    if inventory is not None:
        return inventory
    if environment.get("SUBYARD_FULL_CLEANUP_INCUS_ABSENT") == "1":
        return []
    return json.loads(subprocess.check_output(["incus", "list", "--all-projects", "--format=json"], stderr=subprocess.DEVNULL, timeout=10))


def has_foreign_producer(environment, inventory):
    projects = set(json.loads(environment.get("SUBYARD_FULL_CLEANUP_PROJECTS", "[]")))
    return any((item.get("expanded_config") or item.get("config") or {}).get("user.subyard.test_vms_spool_schema") and item.get("project") not in projects for item in inventory)


def verify_evidence(environment):
    """Read bounded ownership fields only, including private host companions."""
    roots = evidence_roots(environment["SUBYARD_HOME"])
    sources = set(json.loads(environment.get("SUBYARD_FULL_CLEANUP_SOURCES", "[]")))
    allowed_uids = {0, int(environment.get("SUBYARD_FULL_CLEANUP_OPERATOR_UID", os.getuid()))}
    count = 0

    def read_record(path, maximum=2 * 1024 * 1024):
        nonlocal count
        count += 1
        info = path.lstat()
        if count > 131072 or not stat.S_ISREG(info.st_mode) or info.st_uid not in allowed_uids or stat.S_IMODE(info.st_mode) & 0o027 or info.st_size > maximum:
            raise ValueError("unsafe retained evidence")
        with path.open("rb") as stream:
            body = stream.read(maximum + 1)
            fresh = os.fstat(stream.fileno())
        after = path.lstat()
        facts = lambda item: (item.st_dev, item.st_ino, item.st_mode, item.st_uid, item.st_gid, item.st_size, item.st_mtime_ns, item.st_ctime_ns)
        if len(body) > maximum or facts(info) != facts(fresh) or facts(info) != facts(after):
            raise ValueError("retained evidence changed during ownership proof")
        return body

    incidents = set()
    for root_index, root in enumerate(roots[:2]):
        directory = pathlib.Path(root)
        if not os.path.lexists(directory):
            continue
        info = directory.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid not in allowed_uids or stat.S_IMODE(info.st_mode) & 0o027:
            raise ValueError("unsafe retained evidence directory")
        # os.walk's default suppresses EACCES: ownership checks must propagate it.
        paths = []
        def fail(error):
            raise error
        for parent, directories, files in os.walk(directory, followlinks=False, onerror=fail):
            for name in directories:
                child = pathlib.Path(parent) / name
                child_info = child.lstat()
                if root_index != 1 or child.relative_to(directory).as_posix() != "host" or not stat.S_ISDIR(child_info.st_mode) or child_info.st_uid not in allowed_uids or stat.S_IMODE(child_info.st_mode) & 0o027:
                    raise ValueError("unproven retained evidence descendant")
            paths.extend(pathlib.Path(parent) / name for name in files)
        for path in sorted(paths, key=lambda path: len(path.parts)):
            body = read_record(path)
            if path.name == ".host-evidence.lock" and path.parent.name == "host" and body == b"":
                continue
            if path.suffix != ".json":
                raise ValueError("unproven retained evidence file")
            record = json.loads(body)
            if path.parent.name == "host":
                if path.name not in incidents or not isinstance(record, dict) or "observed_at" not in record or "timing" not in record:
                    raise ValueError("host evidence has no approved parent incident")
            elif not isinstance(record, dict) or record.get("source") not in sources:
                raise ValueError("retained evidence has unproven producer ownership")
            elif root_index == 1:
                incidents.add(path.name)
    materialized = pathlib.Path(roots[2])
    if os.path.lexists(materialized):
        for line in read_record(materialized, 128 * 1024 * 1024).splitlines():
            if json.loads(line).get("source") not in sources:
                raise ValueError("materialized evidence has unproven producer ownership")
    lock = pathlib.Path(roots[3])
    if os.path.lexists(lock) and read_record(lock) != b"":
        raise ValueError("unsafe sink coordination record")


def verify_private(environment, inventory=None):
    if has_foreign_producer(environment, producer_inventory(environment, inventory)):
        raise ValueError("another producer now depends on this sink")
    verify_evidence(environment)


def assess(environment, system_uid=0, inventory=None):
    home = environment["SUBYARD_HOME"]
    libexec = environment.get("SUBYARD_TEST_VMS_SINK_LIBEXEC_DIR", "/usr/local/libexec/subyard")
    helper = pathlib.Path(environment.get("SUBYARD_TEST_VMS_SINK_PATH", libexec + "/test-vms-host-sink"))
    service = pathlib.Path(environment.get("SUBYARD_TEST_VMS_SINK_SERVICE_PATH", "/etc/systemd/system/subyard-test-vms-host-sink.service"))
    timer = pathlib.Path(environment.get("SUBYARD_TEST_VMS_SINK_TIMER_PATH", "/etc/systemd/system/subyard-test-vms-host-sink.timer"))
    result = {"artifacts": [], "services": [], "preserved": [], "managedRoots": [], "preservedRoots": [], "mutableRoots": [], "blockers": []}
    if not any(os.path.lexists(path) for path in (service, timer, helper)):
        return result
    if not service.exists():
        raise ValueError("sink artifacts lack a service establishing their installation owner")
    service_binding = owned_regular(service, 0o644, system_uid)
    content = service.read_bytes()
    match = re.search(rb'^Environment="SUBYARD_HOME=([^"\r\n]+)"$', content, re.MULTILINE)
    if not match:
        raise ValueError("sink service has no exact data-root identity")
    actual_home = match.group(1).decode()
    template = (ROOT / "config/systemd/subyard-test-vms-host-sink.service.in").read_bytes()
    expected = template.replace(b"@SUBYARD_TEST_VMS_HOST_SINK@", str(helper).encode())
    expected = expected.replace(b"@SUBYARD_HOME@", actual_home.encode())
    gid = re.search(rb'^Environment="SUBYARD_OPERATOR_GID=(\d+)"$', content, re.MULTILINE)
    if not gid:
        raise ValueError("sink service has no operator-group identity")
    expected = expected.replace(b"@SUBYARD_OPERATOR_GID@", gid.group(1))
    if content != expected or binding(service) != service_binding:
        raise ValueError("sink service differs from its exact owned template")
    if actual_home != home:
        result["preserved"].append("test VM host sink serving another installation")
        return result
    if gid.group(1).decode() != str(os.getgid()):
        raise ValueError("sink belongs to another operator group")
    artifacts = [(service, service_binding)]
    if os.path.lexists(timer):
        before = owned_regular(timer, 0o644, system_uid)
        if timer.read_bytes() != (ROOT / "config/systemd/subyard-test-vms-host-sink.timer.in").read_bytes() or binding(timer) != before:
            raise ValueError("sink timer differs from its exact owned template")
        artifacts.append((timer, before))
    if os.path.lexists(helper):
        before = owned_regular(helper, 0o755, system_uid)
        digest = hashlib.sha256(helper.read_bytes()).digest()
        runtime = environment.get("YARD_RUNTIME_ROOT", home + "/runtime")
        engines = [environment["SUBYARD_DISPATCHER_PATH"]] + glob.glob(runtime + "/releases/*/bin/yard-engine")
        if not any(path and os.path.isfile(path) and hashlib.sha256(pathlib.Path(path).read_bytes()).digest() == digest for path in engines):
            raise ValueError("sink helper does not match this installation's engine")
        if binding(helper) != before:
            raise ValueError("sink helper changed while assessing ownership")
        artifacts.append((helper, before))
    link = timer.parent / "timers.target.wants" / timer.name
    if os.path.lexists(link):
        destination = os.readlink(link)
        if not os.path.isabs(destination):
            destination = str(link.parent / destination)
        if os.path.normpath(destination) != str(timer):
            raise ValueError("sink timer enablement belongs to another producer")
        artifacts.append((link, binding(link)))
    roots = evidence_roots(home)
    if has_foreign_producer(environment, producer_inventory(environment, inventory)):
        result["preserved"].append("shared test VM host sink and retained evidence serving another producer")
        result["preservedRoots"] = roots
        return result
    try:
        verify_evidence(environment)
    except PermissionError:
        # Exact namespace and unit ownership are already established. The plan
        # authorizes only a bounded read-only proof after central confirmation.
        result["conditional"] = True
    except (ValueError, json.JSONDecodeError):
        result["blockers"].append("retained test VM evidence has unproven producer ownership; preserve logs and restore the producer inventory before full removal")
        result["preservedRoots"] = roots
        return result
    # Repeat after draining even when the first read was possible: an in-flight
    # producer may still publish evidence while the confirmation is displayed.
    result["conditional"] = True
    result["managedRoots"] = roots
    if os.path.isdir(home + "/logs"):
        result["mutableRoots"] = [home + "/logs"]
    result["artifacts"] = [{"Path": str(path), "Binding": identity} for path, identity in artifacts]
    result["services"] = [{"scope": "system", "units": [timer.name, service.name]}]
    return result


if __name__ == "__main__":
    try:
        if sys.argv[1:] == ["--assess"]:
            print(json.dumps(assess(os.environ)))
        elif sys.argv[1:] == ["--verify-private"]:
            verify_private(os.environ)
            print('{"verified": true}')
        else:
            raise SystemExit(2)
    except (ValueError, OSError, KeyError, subprocess.CalledProcessError, subprocess.TimeoutExpired):
        print("test-vms full cleanup ownership assessment failed; preserve sink artifacts", file=sys.stderr)
        raise SystemExit(1)
