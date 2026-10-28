#!/usr/bin/env python3
"""Real Incus profile boundary in a marker-owned disposable container."""
import json
import hashlib
import importlib.util
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("producer", ROOT / "scripts/lib/producer-run.py")
producer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(producer)


def call(*args, **kwargs):
    result = subprocess.run(args, capture_output=True, text=True, **kwargs)
    if result.returncode:
        # Preserve native failure bytes in the controller log before guest cleanup.
        sys.stdout.write(result.stdout)
        sys.stderr.write(result.stderr)
        result.check_returncode()
    return result.stdout.strip()


def wait_for(predicate):
    until = time.monotonic() + 15
    while time.monotonic() < until:
        if predicate():
            return
        time.sleep(.1)
    raise AssertionError("profile lifecycle deadline")


assert os.getuid() == 0 and os.environ.get("SUBYARD_E2E_VM") == "1"
with tempfile.TemporaryDirectory(prefix="producer-lifecycle-", dir="/var/tmp") as temporary:
    fixture = Path(temporary)
    name = "producer-" + fixture.name.rsplit("-", 1)[1]
    call("incus", "launch", "images:debian/13/cloud", name, "-c", "security.nesting=true",
         "-c", "limits.memory=1GiB", "-c", "user.subyard-producer-fixture=" + name)
    def guest(*command):
        return call("incus", "exec", name, "--", *command)
    try:
        call("incus", "exec", name, "--", "sh", "-s", "--", "wait-boot",
             input=(ROOT / "scripts/guest-disk-tmp.sh").read_text())
        print("evidence: guest boot complete; tmp=" + guest("findmnt", "-n", "-o", "FSTYPE", "-T", "/tmp"), flush=True)
        guest("python3", "-c", "import os,tarfile")
        guest("sh", "-c", "mkdir -p /tmp/producer-legacy; echo old >/tmp/producer-legacy/canary; setsid sleep 293 >/dev/null 2>&1 </dev/null &")
        runtime = fixture / "runtime"
        (runtime / "scripts/lib").mkdir(parents=True)
        for path in ("scripts/provision-profile.sh", "scripts/lib/engine-context.sh", "scripts/lib/producer-run.py"):
            shutil.copyfile(ROOT / path, runtime / path)
        profile = runtime / "config/profiles/synthetic"
        profile.mkdir(parents=True)
        (profile / "provision-tmp-paths").write_text("/tmp/producer-legacy\n")
        (profile / "provision.sh").write_text('''#!/bin/bash
# subyard-provision-check-v1
set -eu
if [ "$ACTION" = hang ]; then
  setsid python3 -c 'import json,time; from pathlib import Path; s=Path("/proc/self/stat").read_text(); Path("/root/producer-child").write_text(json.dumps([s.split(" ",1)[0],s.rsplit(") ",1)[1].split()[19]])); f=open("/tmp/deleted","wb"); import os; os.unlink(f.name); f.write(b"x"*1048576); f.flush(); time.sleep(300)' &
  wait
fi
if [ "$ACTION" = compat ]; then
  pgrep -f '^sleep 293$' >/dev/null
  runuser -u nobody -- id -u >/dev/null
  systemctl show --property=Version >/dev/null
  systemctl daemon-reload
  test "$(cat /tmp/producer-legacy/canary)" = old
  echo updated >/tmp/producer-legacy/canary
  python3 -c 'from pathlib import Path; assert Path("/tmp").stat().st_dev == Path("/var/lib/subyard/profile-operation-v4/data/tmp").stat().st_dev'
fi
printf 'hook output\\n'
printf 'hook error\\n' >&2
if [ "$ACTION" = noisy ]; then head -c 200000 /dev/zero | tr '\\0' x >&2; fi
exit "$STATUS"
''')
        env = dict(os.environ, SUBYARD_ENGINE_CONTEXT="1", SUBYARD_ENGINE_CONTEXT_SCHEMA="1",
                   SUBYARD_OPERATOR_HOME=str(fixture), SUBYARD_CONFIG_DIR=str(runtime / "config"),
                   SUBYARD_CONFIG_HOME=str(fixture), SUBYARD_HOME=str(runtime), STORAGE_PATH=str(fixture),
                   HOST_BASE=str(fixture), RESTRICTED_DISK_PATHS="", ACCESS_KIND="local", YARD_KIND="container",
                   YARD_INSTANCE_NAME=name, INCUS_PROJECT="default", INCUS_BRIDGE="incusbr0", SSH_HOST=name,
                   DEV_USER="nobody", DEV_UID="65534", DEV_SUDO="0", FORWARD_SSH_AGENT="0", NESTED_E2E_VMS="0")
        def adapter(status=0, action="status", check=False):
            (profile / "profile.conf").write_text(f"STATUS={status}\nACTION={action}\n")
            entries = {}
            for path in [profile, *sorted(profile.rglob("*"))]:
                digest = None
                if path.is_file():
                    with path.open("rb") as source:
                        digest = hashlib.file_digest(source, "sha256").hexdigest()
                entries[str(path.relative_to(profile))] = producer.source_identity(path.lstat(), digest)
            parents = {}
            for path in profile.parents:
                identity = producer.source_identity(path.lstat())
                identity.update(size=0, modified=0, changed=0, links=0)
                parents[str(path)] = identity
            binding = dict(root=str(profile), entries=entries, parents=parents,
                           environment={"STATUS": str(status), "ACTION": action})
            with tempfile.NamedTemporaryFile(mode="w", prefix="metadata-", dir=fixture, delete=False) as metadata:
                os.fchmod(metadata.fileno(), 0o600)
                json.dump({"input": {"profileBinding": binding}}, metadata)
            return ["bash", "-c", 'set -e; exec 3<"$1"; shift; export SUBYARD_METADATA_FD=3; exec "$@"',
                    "producer-fixture", metadata.name, "bash", str(runtime / "scripts/provision-profile.sh"),
                    *(["--check"] if check else []), "synthetic"]
        for status, expected in ((0, "converged\n"), (10, "changed\n"), (7, "")):
            result = subprocess.run(adapter(status, check=True), env=env, capture_output=True, text=True, timeout=30)
            assert result.returncode == (7 if status == 7 else 0), result
            assert result.stdout == expected and result.stderr == ("hook error\n" if status == 7 else ""), result
            assert guest("cat", "/tmp/producer-legacy/canary") == "old"
        result = subprocess.run(adapter(7, "compat"), env=env, capture_output=True, text=True, timeout=30)
        assert result.returncode == 7 and result.stderr == "hook error\n", result
        assert guest("cat", "/tmp/producer-legacy/canary") == "updated"
        guest("sh", "-c", "printf preserve >/var/lib/subyard/profile-operation/data/preserved")
        for invalid in ("/tmp/.", "/tmp//", "/var/tmp/.", "/dev/shm/."):
            (profile / "provision-tmp-paths").write_text(invalid + "\n")
            result = subprocess.run(adapter(), env=env, capture_output=True, text=True, timeout=30)
            assert result.returncode == 125 and not result.stdout and result.stderr == "producer: invalid shared temporary directory\n", result
        (profile / "provision-tmp-paths").write_text("/tmp/producer-legacy\n")
        result = subprocess.run(adapter(7, "noisy"), env=env, capture_output=True, timeout=30)
        assert result.returncode == 7 and len(result.stderr) < 65700 and b"truncated" in result.stderr, result
        for sig in (signal.SIGTERM, signal.SIGKILL):
            command = adapter(action="hang")
            with (fixture / "cancel-out").open("wb") as out, (fixture / "cancel-err").open("wb") as err:
                process = subprocess.Popen(command, env=env, stdout=out, stderr=err)
                try:
                    wait_for(lambda: guest("sh", "-c", "test ! -s /root/producer-child || cat /root/producer-child"))
                    pid, ticks = json.loads(guest("cat", "/root/producer-child"))
                    busy = subprocess.run(command, env=env, capture_output=True, timeout=10)
                    assert busy.returncode == 125 and b"busy" in busy.stderr, busy
                    process.send_signal(sig)
                    assert process.wait(timeout=15) == (-9 if sig == signal.SIGKILL else 143)
                    def gone():
                        return guest("python3", "-c", f"from pathlib import Path; p=Path('/proc/{pid}/stat'); print(not p.exists() or p.read_text().rsplit(') ',1)[1].split()[19] != {ticks!r})") == "True"
                    wait_for(gone)
                    guest("rm", "/root/producer-child")
                    recovery = subprocess.run(adapter(), env=env, capture_output=True, timeout=30)
                    assert recovery.returncode == 0, recovery
                    assert guest("find", "/var/lib/subyard/profile-operation-v4/data", "-mindepth", "1", "-print", "-quit") == ""
                    assert guest("cat", "/var/lib/subyard/profile-operation/data/preserved") == "preserve"
                finally:
                    if process.poll() is None:
                        process.kill()
                        process.wait()
        command = adapter(action="hang")
        with (fixture / "restart-out").open("wb") as out, (fixture / "restart-err").open("wb") as err:
            process = subprocess.Popen(command, env=env, stdout=out, stderr=err)
            wait_for(lambda: guest("sh", "-c", "test ! -s /root/producer-child || cat /root/producer-child"))
            before = call("incus", "query", f"/1.0/instances/{name}/state")
            call("incus", "restart", "--force", name)
            process.wait(timeout=15)
            after = call("incus", "query", f"/1.0/instances/{name}/state")
            assert json.loads(before)["pid"] != json.loads(after)["pid"]
            recovery = subprocess.run(adapter(), env=env, capture_output=True, timeout=30)
            assert recovery.returncode == 0, recovery
            assert guest("find", "/var/lib/subyard/profile-operation-v4/data", "-mindepth", "1", "-print", "-quit") == ""
            assert guest("cat", "/var/lib/subyard/profile-operation/data/preserved") == "preserve"
        print("ok: real Incus status, runuser, external proc, legacy tmp, stderr, busy, controller TERM/SIGKILL and recovery")
    finally:
        assert call("incus", "config", "get", name, "user.subyard-producer-fixture") == name
        call("incus", "delete", "--force", name)
