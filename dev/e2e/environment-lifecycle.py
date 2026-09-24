#!/usr/bin/env python3
"""Exercise disposable environments through independent agent-e2e wrappers.

Usage: python3 dev/e2e/environment-lifecycle.py --slot N --peer-slot M \
    --output-dir .build/environment-lifecycle-RUN [--require-cold]
Single slot: --slot N --pair-only --output-dir .build/pair-reuse-RUN

Only VM1 is probed. Pair VM2 readiness/resources are broker evidence. No Android
SDK/GPU, outer-host lifecycle, raw SSH, or broker credentials are involved.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import queue
import re
import signal
import subprocess
import sys
import threading
import time
import uuid


ROOT = Path(__file__).resolve().parents[2]
RUNNER = ROOT / "dev/agent-e2e.sh"
PREFIX = "ENVIRONMENT_PROBE "
LOG_LIMIT = 1024 * 1024
TIMEOUT = 3600

# This trusted code runs as guest root; only the cache/KVM child drops to dev.
GUEST = r'''
import base64, fcntl, hashlib, json, os, pathlib, pwd, re, select, socket
import errno, stat, subprocess, sys, tempfile, time
slot, kind, purpose, token = sys.argv[1:]
def require(ok, message):
    if not ok:
        raise RuntimeError(message)
def identity():
    context = json.loads(pathlib.Path('/run/subyard-e2e-lease.json').read_text())
    require(context['slot'] == slot and context['purpose'] == purpose, 'lease context mismatch')
    require(bool(context['run']), 'missing public run identity')
    hostname = socket.gethostname()
    require(hostname.endswith('-1'), 'expected VM1 hostname')
    manifest = pathlib.Path('/var/lib/subyard/base-manifest.txt').read_text()
    require(manifest.splitlines()[0] == 'environment=' + kind, 'base environment mismatch')
    machine = pathlib.Path('/etc/machine-id').read_bytes().strip()
    require(re.fullmatch(b'[a-f0-9]{32}', machine), 'invalid machine identity')
    keys = {}
    for path in sorted(pathlib.Path('/etc/ssh').glob('ssh_host_*_key.pub')):
        fields = path.read_text().split()
        keys[path.name] = hashlib.sha256(base64.b64decode(fields[1], validate=True)).hexdigest()
    require(bool(keys), 'missing SSH public host keys')
    return {'slot': slot, 'type': kind, 'vm': 1, 'run': context['run'],
            'purpose': purpose, 'machine_id_sha256': hashlib.sha256(machine).hexdigest(),
            'ssh_public_key_sha256': keys}
require(os.geteuid() == 0, 'guest probe requires root')
first = identity()
marker = pathlib.Path('/var/tmp/subyard-environment-' + token)
with marker.open('x') as stream:
    stream.write('disposable environment acceptance\n')
dev = pwd.getpwnam('dev')
cache = {}
for name in ['/home/dev/.cache', '/home/dev/.cache/subyard-e2e-platform']:
    metadata = pathlib.Path(name).stat()
    require(metadata.st_uid == dev.pw_uid and metadata.st_gid == dev.pw_gid,
            'cache directory not owned by dev')
    cache[name] = {'uid': metadata.st_uid, 'gid': metadata.st_gid}
child = subprocess.run([sys.executable, '-c', """
import fcntl, json, os, tempfile
for directory in ['/home/dev/.cache', '/home/dev/.cache/subyard-e2e-platform']:
    with tempfile.TemporaryFile(dir=directory) as stream:
        stream.write(b'cache writable by dev')
with open('/dev/kvm', 'r+b', buffering=0) as device:
    version = fcntl.ioctl(device, 0xAE00, 0)
    assert version == 12, 'unexpected KVM API version'
    vm = fcntl.ioctl(device, 0xAE01, 0)
    os.close(vm)
print(json.dumps({'uid': os.getuid(), 'api_version': version, 'create_vm': True}))
"""], user=dev.pw_uid, group=dev.pw_gid, extra_groups=os.getgrouplist('dev', dev.pw_gid),
    capture_output=True, text=True, timeout=30)
require(child.returncode == 0, 'dev cache write or KVM ioctl failed')
memory = int(re.search(r'^MemTotal:\s+(\d+)', pathlib.Path('/proc/meminfo').read_text(), re.M)[1]) * 1024
device = os.stat('/').st_dev
block = pathlib.Path('/sys/dev/block/%s:%s' % (os.major(device), os.minor(device))).resolve()
if (block / 'partition').exists():
    block = block.parent
disk = int((block / 'size').read_text()) * 512
def emit(event, value):
    print('ENVIRONMENT_PROBE ' + json.dumps({'event': event, **value}), flush=True)
emit('ready', {**first, 'marker_absent_before_create': True, 'memory_bytes': memory,
    'root_disk_bytes': disk, 'cpu_count': os.cpu_count(), 'cache': cache,
    'cache_writable_by_dev': True, 'kvm': json.loads(child.stdout)})
deadline = time.monotonic() + 7200
fill = None
while time.monotonic() < deadline:
    readable, _, _ = select.select([sys.stdin], [], [], max(0, min(30, deadline - time.monotonic())))
    if not readable:
        continue
    command = sys.stdin.readline()
    require(bool(command), 'controller closed stdin without release')
    if command.strip() == 'fill':
        require(fill is None, 'disk fill already active')
        # An unlinked file is reclaimed even if the guest process is killed.
        fill = tempfile.TemporaryFile(dir='/var/tmp', prefix=token, buffering=0)
        info = os.fstat(fill.fileno())
        require(stat.S_ISREG(info.st_mode) and info.st_dev == device, 'fill must use guest root filesystem')
        block = os.urandom(1024 * 1024)
        written, fill_deadline = 0, time.monotonic() + 300
        try:
            while written < disk:
                require(time.monotonic() < fill_deadline, 'disk fill timed out')
                written += fill.write(block[:min(len(block), disk - written)])
                if written % (32 * len(block)) == 0:
                    os.fdatasync(fill.fileno())
            os.fdatasync(fill.fileno())
            raise RuntimeError('disk fill reached device capacity without ENOSPC')
        except OSError as error:
            require(error.errno == errno.ENOSPC, 'disk fill failed without ENOSPC')
        emit('full', {'written_bytes': written, 'allocated_bytes': os.fstat(fill.fileno()).st_blocks * 512,
                      'errno': errno.ENOSPC, 'root_disk_bytes': disk})
        continue
    if command.strip() == 'clear':
        require(fill is not None, 'no disk fill to clear')
        fill.close()
        fill = None
        emit('cleared', {'free_bytes': os.statvfs('/').f_bavail * os.statvfs('/').f_frsize})
        continue
    if command.strip() == 'release':
        if fill is not None:
            fill.close()
        emit('released', first)
        sys.exit(0)
    require(command.strip() == 'probe', 'unknown controller request')
    with tempfile.TemporaryFile(dir='/var/tmp') as writable:
        writable.write(b'neighbor remains writable\n')
        writable.flush()
        os.fsync(writable.fileno())
    emit('identity', {**identity(), 'root_writable': True})
raise RuntimeError('controller deadline exceeded')
'''


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def utc():
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def size_bytes(value):
    match = re.fullmatch(r"([1-9][0-9]*)(MiB|GiB)", value)
    require(match, "invalid broker resource size")
    return int(match[1]) * (2 ** (20 if match[2] == "MiB" else 30))


def current_bases(status):
    return {base["type"]: base["fingerprint"] for base in status["resources"]["bases"]
            if base["current"] and not base["unusable"] and not base["expired"]}


def slot_status(status, slot):
    return next(value for value in status["pool"]["slots"] if value["slot_id"] == slot)


def same_identity(a, b):
    return all(a[field] == b[field] for field in ("run", "machine_id_sha256", "ssh_public_key_sha256"))


def distinct_identity(a, b):
    require(a["machine_id_sha256"] != b["machine_id_sha256"], "machine identity reused")
    require(set(a["ssh_public_key_sha256"].values()).isdisjoint(b["ssh_public_key_sha256"].values()),
            "SSH host identity reused")


class Lease:
    def __init__(self, controller, name, slot, kind):
        self.name, self.slot, self.kind = name, slot, kind
        self.events = queue.Queue()
        self.ready = None
        self.record = {"name": name, "slot": slot, "type": kind, "started_at": utc(),
                       "log": name + ".log", "log_truncated": False}
        purpose = "environment-" + name + "-" + controller.token[:8]
        self.process = subprocess.Popen(
            [str(RUNNER), "--slot", str(int(slot[5:])), "--type", kind,
             "--purpose", purpose, "--ssh-stdin", "1", "--", "python3", "-u", "-c",
             GUEST, slot, kind, purpose, controller.token], cwd=ROOT,
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
            start_new_session=True)
        self.reader = threading.Thread(target=self.read_output, args=(controller.output,), daemon=True)
        self.reader.start()

    def read_output(self, output):
        written = 0
        with (output / self.record["log"]).open("wb") as log:
            for line in iter(self.process.stdout.readline, b""):
                remaining = max(0, LOG_LIMIT - written)
                log.write(line[:remaining])
                log.flush()
                written += len(line[:remaining])
                self.record["log_truncated"] |= len(line) > remaining
                if line.startswith(PREFIX.encode()):
                    try:
                        self.events.put(json.loads(line[len(PREFIX):]))
                    except (ValueError, UnicodeDecodeError):
                        self.events.put({"event": "invalid"})

    def send(self, command):
        self.process.stdin.write((command + "\n").encode())
        self.process.stdin.flush()


class Controller:
    def __init__(self, output, slots):
        self.output, self.slots = output, slots
        self.token = uuid.uuid4().hex
        self.leases = []
        self.snapshots = []
        self.summary = {"started_at": utc(), "token": self.token, "slots": slots,
                        "measured_guests": "VM1 only; VM2 composition/readiness from broker",
                        "leases": [], "status_snapshots": self.snapshots}
        self.last_snapshot = 0

    def snapshot(self, label):
        result = subprocess.run([str(RUNNER), "--status", "--json"], cwd=ROOT,
                                capture_output=True, text=True, timeout=45)
        require(result.returncode == 0, "redacted broker status failed")
        value = json.loads(result.stdout)
        require(value["status"] == "ok", "broker status not ok")
        # The public facade is the redaction boundary. Never read its private state.
        name = "%03d-%s.json" % (len(self.snapshots), label)
        (self.output / name).write_text(json.dumps(value, indent=2) + "\n")
        self.snapshots.append({"at": utc(), "label": label, "path": name,
                               "builder": value["resources"].get("builder")})
        self.last_snapshot = time.monotonic()
        print("environment-lifecycle: " + label, flush=True)
        return value

    def wait(self, predicate, label, timeout=TIMEOUT):
        deadline = time.monotonic() + timeout
        while not predicate():
            require(time.monotonic() < deadline, label + " timed out")
            if time.monotonic() - self.last_snapshot >= 10:
                self.snapshot(label + "-waiting")
            threading.Event().wait(0.2)

    def acquire(self, name, slot, kind):
        lease = Lease(self, name, slot, kind)
        self.leases.append(lease)
        self.summary["leases"].append(lease.record)
        return lease

    def event(self, lease, expected):
        def arrived():
            if not lease.events.empty():
                return True
            require(lease.process.poll() is None, lease.name + " exited before " + expected)
            return False
        self.wait(arrived, lease.name + "-" + expected)
        result = lease.events.get_nowait()
        require(result["event"] == expected, lease.name + " unexpected probe event")
        return result

    def ready(self, *leases):
        for lease in leases:
            lease.ready = self.event(lease, "ready")
            lease.record.update(ready_at=utc(), probe=lease.ready)
        status = self.snapshot("held-" + "-".join(lease.name for lease in leases))
        for lease in leases:
            held = slot_status(status, lease.slot)
            spec = held["environment"]
            require(held["state"] == "held" and held["run"] == lease.ready["run"],
                    lease.name + " guest/status identity mismatch")
            require(spec["type"] == lease.kind and spec["vm_count"] ==
                    (2 if lease.kind == "subyard-pair" else 1), "wrong environment composition")
            require(spec["lifecycle"] == "disposable-v1", "wrong lifecycle")
            ram, disk = size_bytes(spec["memory_per_vm"]), size_bytes(spec["disk_per_vm"])
            require(0.85 * ram <= lease.ready["memory_bytes"] <= ram,
                    lease.name + " guest RAM differs from held contract")
            require(lease.ready["root_disk_bytes"] == disk, "guest disk differs from held contract")
            require(lease.ready["cpu_count"] == spec["cpu_per_vm"], "guest CPU differs from held contract")
            resource = next(item for item in status["resources"]["slots"] if item["slot_id"] == lease.slot)
            require(resource["virtual_disk_capacity_bytes"] == spec["vm_count"] * disk,
                    "broker virtual disk reservation differs from actual environment")
            require(resource["memory_commitment_bytes"] == spec["vm_count"] *
                    (ram + status["resources"]["budgets"]["vm_overhead_bytes"]),
                    "broker RAM reservation differs from actual environment")
            lease.record.update(environment=spec, base_fingerprint=held["base_fingerprint"], resources=resource)
        return status

    def release(self, *leases):
        for lease in leases:
            lease.send("release")
        for lease in leases:
            self.event(lease, "released")
            lease.process.stdin.close()
            self.wait(lambda: lease.process.poll() is not None, lease.name + "-release", 180)
            lease.reader.join(timeout=5)
            lease.record.update(exit_code=lease.process.returncode, released_at=utc())
            require(lease.process.returncode == 0, lease.name + " wrapper release failed")
        status = self.snapshot("released-" + "-".join(lease.name for lease in leases))
        for lease in leases:
            held = slot_status(status, lease.slot)
            require(held["state"] == "available" and not held.get("environment") and
                    not held.get("reserved") and not held.get("base_fingerprint"),
                    lease.name + " residual working allocation")
            require(not any(item["slot_id"] == lease.slot for item in status["resources"]["slots"]),
                    lease.name + " residual working reservation")
        return status

    def cleanup(self):
        active = [lease for lease in self.leases if lease.process.poll() is None]
        for lease in active:
            try:
                lease.process.stdin.close()
                os.killpg(lease.process.pid, signal.SIGTERM)
            except (BrokenPipeError, ProcessLookupError):
                pass
        deadline = time.monotonic() + 30
        for lease in active:
            try:
                lease.process.wait(timeout=max(0.1, deadline - time.monotonic()))
            except subprocess.TimeoutExpired:
                try:
                    os.killpg(lease.process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                lease.process.wait(timeout=5)
            lease.reader.join(timeout=5)
            lease.record.update(exit_code=lease.process.returncode, interrupted=True)


def pair_reuse(controller, slot, require_cold):
    """Check reuse across sequential leases without reserving a second slot."""
    first = controller.acquire("first-pair", slot, "subyard-pair")
    controller.ready(first)
    fingerprint = first.record["base_fingerprint"]
    idle = controller.release(first)
    warm = controller.acquire("warm-pair", slot, "subyard-pair")
    controller.ready(warm)
    require(warm.record["base_fingerprint"] == fingerprint, "warm pair base changed")
    distinct_identity(first.ready, warm.ready)
    final = controller.release(warm)
    require(current_bases(final).get("subyard-pair") == fingerprint,
            "pair base not retained after release")
    summary = controller.summary
    changed = fingerprint != summary["initial_bases"].get("subyard-pair")
    if require_cold:
        require(changed, "cold pair fingerprint not observed")
    summary.update(published_bases={"subyard-pair": fingerprint},
                   fingerprint_changed={"subyard-pair": changed},
                   idle_storage={"before_warm": idle["resources"].get("storage"),
                                 "after_warm": final["resources"].get("storage")})
    require(all("storage" in status["resources"] for status in (idle, final)),
            "storage telemetry missing")
    summary["idle_budget_delta_bytes"] = (final["resources"]["storage"]["budget_used_bytes"] -
                                          idle["resources"]["storage"]["budget_used_bytes"])
    summary["all_slots_available_at_end"] = all(
        item["state"] == "available" for item in final["pool"]["slots"])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--slot", required=True, type=int)
    parser.add_argument("--peer-slot", type=int)
    parser.add_argument("--pair-only", action="store_true",
                        help="check two sequential pair leases in one slot; no Android allocation")
    parser.add_argument("--output-dir", required=True, type=Path)
    parser.add_argument("--require-cold", action="store_true",
                        help="require tested types to publish fingerprints different from initial current bases")
    parser.add_argument("--disk-isolation", action="store_true",
                        help="fill peer VM1 root disk to ENOSPC while checking a held neighbor")
    args = parser.parse_args()
    if not 1 <= args.slot <= 999:
        parser.error("choose a slot from 1 to 999")
    if args.pair_only:
        if args.peer_slot is not None or args.disk_isolation:
            parser.error("--pair-only does not use --peer-slot or --disk-isolation")
        numbers = (args.slot,)
    else:
        if args.peer_slot is None or not 1 <= args.peer_slot <= 999 or args.slot == args.peer_slot:
            parser.error("choose distinct --slot and --peer-slot from 1 to 999")
        numbers = (args.slot, args.peer_slot)
    args.output_dir.mkdir(parents=True, exist_ok=False)
    slots = ["slot-%03d" % number for number in numbers]
    controller = Controller(args.output_dir, slots)
    summary = controller.summary
    started = time.monotonic()
    def interrupted(signum, frame):
        raise InterruptedError("controller interrupted by signal %d" % signum)
    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    try:
        head = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
        dirty = subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT)
        diff = subprocess.check_output(["git", "diff", "HEAD", "--binary"], cwd=ROOT)
        summary["source"] = {"head": head, "dirty": bool(dirty),
                             "tracked_diff_sha256": hashlib.sha256(diff).hexdigest(),
                             "fixture_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest()}
        summary["require_cold"] = args.require_cold
        summary["pair_only"] = args.pair_only
        initial = controller.snapshot("initial")
        for slot in slots:
            require(slot_status(initial, slot)["state"] == "available", slot + " is not available")
        summary["initial_bases"] = current_bases(initial)
        if args.pair_only:
            pair_reuse(controller, slots[0], args.require_cold)
        else:
            first = controller.acquire("first-pair", slots[0], "subyard-pair")
            peer = controller.acquire("peer-pair", slots[1], "subyard-pair")
            controller.ready(first, peer)
            require(first.record["base_fingerprint"] == peer.record["base_fingerprint"],
                    "concurrent same-type requests used different bases")
            distinct_identity(first.ready, peer.ready)
            if args.disk_isolation:
                controller.snapshot("before-disk-fill")
                peer.send("fill")
                full = controller.event(peer, "full")
                require(0 < full["written_bytes"] <= peer.ready["root_disk_bytes"], "invalid disk fill size")
                controller.snapshot("disk-full")
                first.send("probe")
                neighbor = controller.event(first, "identity")
                require(same_identity(first.ready, neighbor) and neighbor["root_writable"],
                        "neighbor failed while peer disk was full")
                peer.send("clear")
                cleared = controller.event(peer, "cleared")
                require(cleared["free_bytes"] > 0, "guest space not reclaimed after closing fill file")
                controller.snapshot("disk-cleared")
                summary["disk_isolation"] = {"full": full, "cleared": cleared, "neighbor_writable": True}
            controller.release(peer)
            if args.disk_isolation:
                first.send("probe")
                neighbor = controller.event(first, "identity")
                require(same_identity(first.ready, neighbor) and neighbor["root_writable"],
                        "neighbor failed after peer disk release")
            android = controller.acquire("first-android", slots[1], "android-test")
            mixed = controller.ready(android)
            require(slot_status(mixed, first.slot)["state"] == "held" and
                    slot_status(mixed, first.slot)["run"] == first.ready["run"],
                    "pair neighbor not held during Android acquisition")
            first.send("probe")
            require(same_identity(first.ready, controller.event(first, "identity")),
                    "held neighbor changed identity")
            idle = controller.release(first, android)
            bases = {lease.kind: lease.record["base_fingerprint"] for lease in (first, android)}
            summary["published_bases"] = bases
            summary["fingerprint_changed"] = {kind: fingerprint != summary["initial_bases"].get(kind)
                                              for kind, fingerprint in bases.items()}
            if args.require_cold:
                require(all(summary["fingerprint_changed"].values()), "cold build fingerprints not observed")
            warm_pair = controller.acquire("warm-pair", slots[0], "subyard-pair")
            warm_android = controller.acquire("warm-android", slots[1], "android-test")
            controller.ready(warm_pair, warm_android)
            for fresh in (warm_pair, warm_android):
                require(fresh.record["base_fingerprint"] == bases[fresh.kind], "warm base changed")
                for prior in (first, peer, android):
                    distinct_identity(fresh.ready, prior.ready)
            distinct_identity(warm_pair.ready, warm_android.ready)
            final = controller.release(warm_pair, warm_android)
            require(all(current_bases(final).get(kind) == fingerprint for kind, fingerprint in bases.items()),
                    "current bases not retained after release")
            summary["idle_storage"] = {"before_warm": idle["resources"].get("storage"),
                                       "after_warm": final["resources"].get("storage")}
            before = idle["resources"].get("storage", {}).get("budget_used_bytes")
            after = final["resources"].get("storage", {}).get("budget_used_bytes")
            require(before is not None and after is not None, "dedicated storage budget telemetry missing")
            summary["idle_budget_delta_bytes"] = after - before
            summary["all_slots_available_at_end"] = all(item["state"] == "available" for item in final["pool"]["slots"])
        summary["result"] = "passed"
    except (Exception, KeyboardInterrupt) as error:
        summary.update(result="failed", error=str(error))
    finally:
        try:
            controller.cleanup()
            if summary.get("result") != "passed":
                controller.snapshot("failure-cleanup")
        except Exception as error:
            summary.update(result="failed", cleanup_error=str(error))
        summary["builder_samples"] = [sample for sample in controller.snapshots if sample["builder"]]
        summary["singleflight_evidence_limit"] = ("sequential leases only" if args.pair_only else
            "same published fingerprint plus sampled builder state; samples cannot count unobserved builds")
        summary.update(finished_at=utc(), duration_seconds=round(time.monotonic() - started, 2))
        (args.output_dir / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    print("environment-lifecycle: %s; %s" % (summary["result"], args.output_dir / "summary.json"))
    return 0 if summary["result"] == "passed" else 1


if __name__ == "__main__":
    sys.exit(main())
