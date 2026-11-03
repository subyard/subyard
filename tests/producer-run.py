#!/usr/bin/env python3
"""Local lifecycle contracts; real Incus and reboot remain owning VM checks."""
import importlib.util
import contextlib
import hashlib
import io
import json
import os
from pathlib import Path
import signal
import shutil
import subprocess
import tarfile
import tempfile
import time
from types import SimpleNamespace
from unittest import TestCase, mock

checks = TestCase()

ROOT = Path(__file__).resolve().parents[1]
HELPER = ROOT / "scripts/lib/producer-run.py"
LEGACY = ROOT / "tests/fixtures/producer-run-v3.py"
# Byte-identical 74a9c07e source; retain the actual old executable contract.
assert hashlib.sha256(LEGACY.read_bytes()).hexdigest() == "905df6f88fd33f3cb9138010cf2232985c32efbae303c4d56663334400df756d"
spec = importlib.util.spec_from_file_location("producer", HELPER)
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)
(ROOT / ".build").mkdir(exist_ok=True)

# The normal helper boundary retains its failure status without exposing input.
result = subprocess.run(["python3", str(HELPER), "capture", "check", "private-secret/path"],
                        env=dict(os.environ, SUBYARD_METADATA_FD="private-secret/path"), capture_output=True)
assert result.returncode == 125 and not result.stdout
assert result.stderr == b"producer: operation failed; inspect preserved storage before retrying\n"


class Profile:
    """Run an exact helper with only its operation parent moved to a test root."""

    def __init__(self, helper, parent, script, prefix="", shared=()):
        self.helper, self.parent, self.script, self.prefix, self.shared = helper, Path(parent), script, prefix, shared

    def replace(self, **changes):
        return Profile(*[changes.get(name, getattr(self, name)) for name in ("helper", "parent", "script", "prefix", "shared")])

    def arguments(self):
        source = self.helper.read_text()
        moved = source.replace('Path("/var/lib/subyard/profile-operation")', f"Path({str(self.parent / 'profile-operation')!r})")
        assert moved != source
        source = self.prefix + moved
        program = f"__file__={str(self.helper)!r}; _SOURCE={source!r}; exec(_SOURCE)"
        return ["python3", "-c", program, "launch", "profile", "", json.dumps(list(self.shared)), "apply"]

    def start(self, **options):
        archive = io.BytesIO()
        with tarfile.open(fileobj=archive, mode="w") as tar:
            info = tarfile.TarInfo("provision.sh")
            payload = self.script.encode()
            info.size, info.mode = len(payload), 0o700
            tar.addfile(info, io.BytesIO(payload))
        # The controller keeps stdin open: its loss is the disconnect signal.
        process = subprocess.Popen(self.arguments(), stdin=subprocess.PIPE, **options)
        try:
            process.stdin.write(archive.getvalue())
            process.stdin.flush()
        except BrokenPipeError:
            pass
        return process

    def run(self):
        with tempfile.TemporaryFile() as out, tempfile.TemporaryFile() as err:
            process = self.start(stdout=out, stderr=err)
            status = process.wait(timeout=30)
            try:
                process.stdin.close()
            except BrokenPipeError:
                pass
            out.seek(0), err.seek(0)
            return SimpleNamespace(returncode=status, stdout=out.read(), stderr=err.read())


def until(predicate):
    deadline = time.monotonic() + 8
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(.02)
    raise AssertionError("lifecycle deadline")


with tempfile.TemporaryDirectory(prefix="producer-identity-", dir="/tmp") as temporary:
    # Pure identity checks use real private FDs; mock the host disk classifier.
    with mock.patch.object(runner.subprocess, "check_output", return_value="ext4\n"):
        storage = runner.Storage(Path(temporary)/"operation", "profile")
    try:
        state = storage.read()
        identity = runner.durable_inode(storage.control)
        live = runner.inode(storage.control)
        real_fstat = os.fstat
        def changed_device(fd):
            info = real_fstat(fd)
            return SimpleNamespace(st_dev=info.st_dev+1, st_ino=info.st_ino)
        with mock.patch.object(runner.os, "fstat", side_effect=changed_device):
            assert storage.matches(state) and runner.durable_inode(storage.control) == identity
            assert runner.inode(storage.control) != live
        with mock.patch.object(runner.os, "fstatvfs", return_value=SimpleNamespace(f_fsid=identity[0]+1)):
            assert not storage.matches(state)
        for value in (0, -1, True, None, "private-canary"):
            with mock.patch.object(runner.os, "fstatvfs", return_value=SimpleNamespace(f_fsid=value)):
                with checks.assertRaises(ValueError):
                    runner.durable_inode(storage.control)
        with mock.patch.object(runner.os, "fstatvfs", return_value=SimpleNamespace()):
            with checks.assertRaises(ValueError):
                runner.durable_inode(storage.control)
    finally:
        for fd in (storage.parent, storage.control, storage.lock, storage.data):
            os.close(fd)


with tempfile.TemporaryDirectory(prefix="producer-bridge-", dir="/tmp") as temporary:
    with mock.patch.object(runner.subprocess, "check_output", return_value="ext4\n"):
        legacy = runner.Storage(Path(temporary)/"legacy", "profile", legacy=True)
        current = runner.Storage(Path(temporary)/"current", "profile")
    try:
        legacy_canary, current_canary = Path(temporary)/"legacy/data/private", Path(temporary)/"current/data/private"
        legacy_canary.write_text("preserve")
        current_canary.write_text("preserve")
        ticks = Path('/proc/self/stat').read_text().rsplit(') ', 1)[1].split()[19]
        identity = [os.getpid(), ticks, Path('/proc/sys/kernel/random/boot_id').read_text().strip()]
        runner.write_bridge_active(legacy.bridge(), "profile", identity)
        state = current.read()
        state['active'] = identity
        runner.write_state(current.lock, state)
        before = [(Path(temporary)/name/'.owner').read_bytes() for name in ('legacy', 'current')]
        with contextlib.redirect_stderr(io.StringIO()):
            assert runner.finish(current, legacy, 23) == 23
            assert runner.finish(current, legacy, 0) == 125
        assert before == [(Path(temporary)/name/'.owner').read_bytes() for name in ('legacy', 'current')]
        assert current_canary.read_text() == 'preserve'
        writes = []
        real_write = runner.write_state
        def record_write(fd, value):
            writes.append(fd)
            real_write(fd, value)
        with mock.patch.object(runner, 'active', return_value=False), mock.patch.object(runner, 'write_state', side_effect=record_write):
            assert runner.finish(current, legacy, 0) == 0
        assert writes == [current.lock, legacy.lock] and not current_canary.exists()
        assert legacy_canary.read_text() == 'preserve'
        # Named-path replacement must refuse before any mirrored write.
        receipt = Path(temporary)/'legacy/.owner'
        receipt.rename(receipt.with_name('saved-owner'))
        receipt.write_bytes(before[0])
        receipt.chmod(0o600)
        with checks.assertRaises(ValueError):
            runner.write_bridge_active(legacy.bridge(), 'profile', None)
        assert receipt.read_bytes() == before[0]
        receipt.unlink()
        receipt.with_name('saved-owner').rename(receipt)
        os.link(receipt, receipt.with_name('linked-owner'))
        with checks.assertRaises(ValueError):
            runner.write_bridge_active(legacy.bridge(), 'profile', None)
        receipt.with_name('linked-owner').unlink()
        real_fstat = os.fstat
        def foreign_uid(fd):
            info = real_fstat(fd)
            return SimpleNamespace(st_dev=info.st_dev, st_ino=info.st_ino, st_mode=info.st_mode,
                                   st_nlink=info.st_nlink, st_uid=info.st_uid+(fd == legacy.lock))
        with mock.patch.object(runner.os, 'fstat', side_effect=foreign_uid):
            with checks.assertRaises(ValueError):
                runner.write_bridge_active(legacy.bridge(), 'profile', None)
        receipt.chmod(0o666)
        with checks.assertRaises(ValueError):
            runner.write_bridge_active(legacy.bridge(), 'profile', None)
    finally:
        for value in (legacy, current):
            for fd in (value.parent, value.control, value.lock, value.data):
                os.close(fd)


with tempfile.TemporaryDirectory(prefix="producer-test-", dir=ROOT / ".build") as temporary:
    root = Path(temporary)
    marker = root / "started"
    operation = root / "profile-operation-v4"
    legacy = root / "profile-operation"
    child = "import os,json,time; from pathlib import Path; f=open('/tmp/open-deleted','wb'); os.unlink(f.name); f.write(b'x'*1048576); f.flush(); fields=Path('/proc/self/stat').read_text().rsplit(') ',1)[1].split(); pid=Path('/proc/self/stat').read_text().split(' ',1)[0]; Path(__import__('sys').argv[1]).write_text(json.dumps([pid,fields[19]])); time.sleep(300)"
    import shlex
    script = 'setsid python3 -c ' + shlex.quote(child) + ' "$1" & wait\n'
    command = Profile(HELPER, root, 'set -- ' + shlex.quote(str(marker)) + '\n' + script)
    old_command = command.replace(helper=LEGACY)
    recovery = command.replace(script="exit 7\n")
    old_recovery = recovery.replace(helper=LEGACY)
    for invocation in (old_recovery, recovery, old_recovery):
        result = invocation.run()
        assert result.returncode == 7, result
    for cancellation in (signal.SIGINT, signal.SIGTERM, signal.SIGKILL):
        with (root / "out").open("wb") as out, (root / "err").open("wb") as err:
            process = command.start(stdout=out, stderr=err)
            try:
                until(lambda: marker.exists() and marker.stat().st_size > 0)
                pid, ticks = json.loads(marker.read_text())
                busy = command.run()
                assert busy.returncode == 125 and b"busy" in busy.stderr, busy
                old_busy = old_command.run()
                assert old_busy.returncode == 125 and b"busy" in old_busy.stderr, old_busy
                assert json.loads((legacy/".owner").read_text())["active"] == json.loads((operation/".owner").read_text())["active"]
                os.kill(process.pid, cancellation)
                status = process.wait(timeout=8)
                assert status == (-9 if cancellation == signal.SIGKILL else 128 + cancellation), status
                marker.unlink()
                def gone():
                    try:
                        fields = Path(f"/proc/{pid}/stat").read_text().rsplit(") ",1)[1].split()
                        return fields[19] != ticks or fields[0] in ("Z", "X")
                    except FileNotFoundError:
                        return True
                until(gone)
                # A next invocation cleans stale owned scratch after SIGKILL.
                until(lambda: not runner.active(json.loads((operation / ".owner").read_text()).get("active")))
                result = recovery.run()
                assert result.returncode == 7, result
                assert not list((operation / "data").iterdir())
                assert json.loads((legacy/".owner").read_text())["active"] is None
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait()
    # An old running helper also fences the new helper through the shared slot.
    process = old_command.start(stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        until(lambda: marker.exists() and marker.stat().st_size > 0)
        busy = command.run()
        assert busy.returncode == 125 and b"busy" in busy.stderr, busy
    finally:
        process.send_signal(signal.SIGTERM)
        assert process.wait(timeout=8) == 143
    marker.unlink()
    until(lambda: not runner.active(json.loads((legacy/".owner").read_text()).get("active")))
    original_legacy = (legacy/".owner").read_bytes()
    # With every flock released, a still-live durable identity fences both
    # actual versions through the exit_files / PID-namespace teardown interval.
    fence = json.loads(original_legacy)
    fence['active'] = [os.getpid(), Path('/proc/self/stat').read_text().rsplit(') ', 1)[1].split()[19],
                       Path('/proc/sys/kernel/random/boot_id').read_text().strip()]
    (legacy/'.owner').write_text(json.dumps(fence))
    for invocation in (old_recovery, recovery):
        result = invocation.run()
        assert result.returncode == 125 and b'busy' in result.stderr, result
    (legacy/'.owner').write_bytes(original_legacy)
    changed = json.loads(original_legacy)
    for key in ("root", "owner", "data"):
        changed[key][0] += 1
    (legacy/".owner").write_text(json.dumps(changed))
    legacy_canary = legacy/"data/preserved"
    legacy_canary.write_text("private-canary")
    changed_bytes = (legacy/".owner").read_bytes()
    result = recovery.run()
    assert result.returncode == 7 and (legacy/".owner").read_bytes() == changed_bytes, result
    assert legacy_canary.read_text() == "private-canary"
    result = old_recovery.run()
    assert result.returncode == 125 and b"foreign" in result.stderr, result
    (legacy/".owner").write_bytes(original_legacy)
    result = old_recovery.run()
    assert result.returncode == 7 and not legacy_canary.exists(), result
    # An interrupted mkdir is not permission to complete or bypass old custody.
    partial = root/'partial'
    (partial/'profile-operation').mkdir(parents=True, mode=0o700)
    partial_command = recovery.replace(parent=partial)
    result = partial_command.run()
    assert result.returncode == 125 and not (partial/'profile-operation-v4').exists(), result
    shutil.rmtree(partial)
    assert partial_command.replace(helper=LEGACY).run().returncode == 7
    partial_receipt = partial/'profile-operation/.owner'
    partial_before = partial_receipt.read_bytes()
    partial_canary = partial_receipt.parent/'data/preserved'
    partial_canary.write_text('preserve')
    wrong = json.loads(partial_before)
    wrong['active'] = ['private-canary']
    for payload in (b'', b'{broken', json.dumps(wrong).encode()):
        partial_receipt.write_bytes(payload)
        result = partial_command.run()
        assert result.returncode == 125 and partial_receipt.read_bytes() == payload, result
        assert partial_canary.read_text() == 'preserve'
        assert not (partial/'profile-operation-v4').exists()
    partial_receipt.write_bytes(partial_before)
    # Kill the real supervisor between durable publication and work, and between
    # current cleanup and the final legacy write. Hook only the fsync boundary.
    for stage in ('publication', 'cleanup'):
        fault_root = root/('fault-'+stage)
        fault_root.mkdir()
        reached, armed = fault_root/'reached', fault_root/'armed'
        prefix = f'''import os,json,time
from pathlib import Path
_real_fsync = os.fsync
def _fault_fsync(fd):
    _real_fsync(fd)
    value = json.loads(os.pread(fd, 2048, 0))
    if value.get('active') is not None:
        Path({str(armed)!r}).touch()
    selected = (value.get('version') == 'subyard-producer-v3' and value.get('active') is not None) if {stage!r} == 'publication' else (value.get('version') == 'subyard-producer-v4' and value.get('active') is None and Path({str(armed)!r}).exists())
    if selected:
        Path({str(reached)!r}).touch()
        while True: time.sleep(.02)
os.fsync = _fault_fsync
'''
        faulted = Profile(HELPER, fault_root, 'exit 7\n', prefix=prefix)
        process = faulted.start(stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        try:
            until(reached.exists)
            if stage == 'publication':
                current_state = json.loads((fault_root/'profile-operation-v4/.owner').read_text())
                assert current_state['active'] is None
            process.kill()
            assert process.wait(timeout=8) == -9
        finally:
            if process.poll() is None:
                process.kill()
                process.wait()
        legacy_receipt = fault_root/'profile-operation/.owner'
        until(lambda: not runner.active(json.loads(legacy_receipt.read_text()).get('active')))
        for executable in (HELPER, LEGACY):
            result = faulted.replace(helper=executable, prefix='').run()
            assert result.returncode == 7, result
    # Receipts and data paths refuse FIFO/symlink substitution without deletion.
    receipt = operation / ".owner"
    original = receipt.read_bytes()
    receipt.rename(operation / "saved-owner")
    receipt.write_bytes(original)
    receipt.chmod(0o600)
    result = command.run()
    assert result.returncode == 125 and b"foreign" in result.stderr
    receipt.unlink()
    (operation / "saved-owner").rename(receipt)
    (operation / "data").rename(operation / "saved-data")
    (operation / "data").mkdir(mode=0o700)
    canary = operation / "data/foreign"
    canary.write_text("preserve")
    result = command.run()
    assert result.returncode == 125 and canary.read_text() == "preserve"
    canary.unlink()
    (operation / "data").rmdir()
    (operation / "saved-data").rename(operation / "data")
    receipt.unlink()
    os.mkfifo(receipt, 0o600)
    result = command.run()
    assert result.returncode == 125 and receipt.is_fifo()
    receipt.unlink()
    receipt.symlink_to(root / "out")
    result = command.run()
    assert result.returncode == 125 and receipt.is_symlink()
    receipt.unlink()
    canary = operation / "data/preserved"
    canary.write_text("preserve")
    for content in (None, b"", b"{broken", b"[]"):
        if content is not None:
            receipt.write_bytes(content)
            receipt.chmod(0o600)
        result = command.run()
        assert result.returncode == 125 and result.stderr == b"producer: operation failed; inspect preserved storage before retrying\n"
        assert str(operation).encode() not in result.stderr
        assert canary.read_text() == "preserve"
        if content is not None:
            assert receipt.read_bytes() == content
            receipt.unlink()
        else:
            assert not receipt.exists()
    canary.unlink()
    (operation / "data").rmdir()
    receipt.write_bytes(original)
    receipt.chmod(0o600)
    result = command.run()
    assert result.returncode == 125 and result.stderr == b"producer: operation failed; inspect preserved storage before retrying\n" and not (operation / "data").exists()
    # Profile scratch stays private; only an explicit migration directory is shared.
    direct = Path("/tmp/subyard-producer-direct-write")
    fresh = root / "fresh"
    result = recovery.replace(parent=fresh, script=f"printf scratch >{direct}\nexit 7\n").run()
    assert result.returncode == 7 and result.stdout == b"", result
    assert not direct.exists()
    # Loss of the controller's lifetime pipe ends the namespace.
    process = recovery.replace(parent=fresh, script=f"printf started >{root / 'disconnect'}\nsleep 60\n").start(stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    until((root / "disconnect").exists)
    process.stdin.close()
    assert process.wait(timeout=8) == 130
    until(lambda: not runner.active(json.loads((fresh / "profile-operation-v4/.owner").read_text()).get("active")))

with tempfile.TemporaryDirectory(prefix="producer-shared-", dir="/var/tmp") as shared, tempfile.TemporaryDirectory(prefix="producer-test-", dir=ROOT / ".build") as temporary:
    canary = Path(shared) / "canary"
    canary.write_text("visible")
    result = Profile(HELPER, temporary, f'test "$(cat {shlex.quote(str(canary))})" = visible || exit 96\nprintf updated >{shlex.quote(str(canary))}\nexit 7\n', shared=[shared]).run()
    assert result.returncode == 7 and canary.read_text() == "updated", result
    assert Profile(HELPER, temporary, "exit 0\n", shared=["/var/tmp/../etc"]).run().returncode == 125
print("ok: PID namespace cancellation, busy, stale recovery and receipt refusal")
