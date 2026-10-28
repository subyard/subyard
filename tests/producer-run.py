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
        storage = runner.Storage(Path(temporary)/"operation", "paseo")
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
        legacy = runner.Storage(Path(temporary)/"legacy", "paseo", legacy=True)
        current = runner.Storage(Path(temporary)/"current", "paseo")
    try:
        legacy_canary, current_canary = Path(temporary)/"legacy/data/private", Path(temporary)/"current/data/private"
        legacy_canary.write_text("preserve")
        current_canary.write_text("preserve")
        ticks = Path('/proc/self/stat').read_text().rsplit(') ', 1)[1].split()[19]
        identity = [os.getpid(), ticks, Path('/proc/sys/kernel/random/boot_id').read_text().strip()]
        runner.write_bridge_active(legacy.bridge(), "paseo", identity)
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
            runner.write_bridge_active(legacy.bridge(), 'paseo', None)
        assert receipt.read_bytes() == before[0]
        receipt.unlink()
        receipt.with_name('saved-owner').rename(receipt)
        os.link(receipt, receipt.with_name('linked-owner'))
        with checks.assertRaises(ValueError):
            runner.write_bridge_active(legacy.bridge(), 'paseo', None)
        receipt.with_name('linked-owner').unlink()
        real_fstat = os.fstat
        def foreign_uid(fd):
            info = real_fstat(fd)
            return SimpleNamespace(st_dev=info.st_dev, st_ino=info.st_ino, st_mode=info.st_mode,
                                   st_nlink=info.st_nlink, st_uid=info.st_uid+(fd == legacy.lock))
        with mock.patch.object(runner.os, 'fstat', side_effect=foreign_uid):
            with checks.assertRaises(ValueError):
                runner.write_bridge_active(legacy.bridge(), 'paseo', None)
        receipt.chmod(0o666)
        with checks.assertRaises(ValueError):
            runner.write_bridge_active(legacy.bridge(), 'paseo', None)
    finally:
        for value in (legacy, current):
            for fd in (value.parent, value.control, value.lock, value.data):
                os.close(fd)


with tempfile.TemporaryDirectory(prefix="producer-test-", dir=ROOT / ".build") as temporary:
    root = Path(temporary)
    marker = root / "started"
    operation = root / f".build/paseo-operation-v4-{os.getuid()}"
    legacy = root / f".build/paseo-operation-{os.getuid()}"
    child = "import os,json,time; from pathlib import Path; f=open('/tmp/open-deleted','wb'); os.unlink(f.name); f.write(b'x'*1048576); f.flush(); fields=Path('/proc/self/stat').read_text().rsplit(') ',1)[1].split(); pid=Path('/proc/self/stat').read_text().split(' ',1)[0]; Path(__import__('sys').argv[1]).write_text(json.dumps([pid,fields[19]])); time.sleep(300)"
    import shlex
    command = ["python3", str(HELPER), "launch", "paseo", str(root), "bash", "-c",
               'setsid python3 -c ' + shlex.quote(child) + ' "$1" & wait',
               "fixture", str(marker), "--output-dir", str(root / "output")]
    old_command = [*command]
    old_command[1] = str(LEGACY)
    recovery = command[:command.index("bash")] + ["bash", "-c", "exit 7", "--output-dir", str(root / "output")]
    old_recovery = [*recovery]
    old_recovery[1] = str(LEGACY)
    for invocation in (old_recovery, recovery, old_recovery):
        result = subprocess.run(invocation, capture_output=True)
        assert result.returncode == 7, result
    for cancellation in (signal.SIGINT, signal.SIGTERM, signal.SIGKILL):
        with (root / "out").open("wb") as out, (root / "err").open("wb") as err:
            process = subprocess.Popen(command, stdout=out, stderr=err)
            try:
                until(lambda: marker.exists() and marker.stat().st_size > 0)
                pid, ticks = json.loads(marker.read_text())
                busy = subprocess.run(command, capture_output=True)
                assert busy.returncode == 125 and b"busy" in busy.stderr, busy
                old_busy = subprocess.run(old_command, capture_output=True)
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
                result = subprocess.run(recovery, capture_output=True)
                assert result.returncode == 7, result
                assert not list((operation / "data").iterdir())
                assert json.loads((legacy/".owner").read_text())["active"] is None
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait()
    # An old running helper also fences the new helper through the shared slot.
    process = subprocess.Popen(old_command, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        until(lambda: marker.exists() and marker.stat().st_size > 0)
        busy = subprocess.run(command, capture_output=True)
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
        result = subprocess.run(invocation, capture_output=True)
        assert result.returncode == 125 and b'busy' in result.stderr, result
    (legacy/'.owner').write_bytes(original_legacy)
    changed = json.loads(original_legacy)
    for key in ("root", "owner", "data"):
        changed[key][0] += 1
    (legacy/".owner").write_text(json.dumps(changed))
    legacy_canary = legacy/"data/preserved"
    legacy_canary.write_text("private-canary")
    changed_bytes = (legacy/".owner").read_bytes()
    result = subprocess.run(recovery, capture_output=True)
    assert result.returncode == 7 and (legacy/".owner").read_bytes() == changed_bytes, result
    assert legacy_canary.read_text() == "private-canary"
    result = subprocess.run(old_recovery, capture_output=True)
    assert result.returncode == 125 and b"foreign" in result.stderr, result
    (legacy/".owner").write_bytes(original_legacy)
    result = subprocess.run(old_recovery, capture_output=True)
    assert result.returncode == 7 and not legacy_canary.exists(), result
    # An interrupted mkdir is not permission to complete or bypass old custody.
    partial = root/'partial'
    (partial/f'.build/paseo-operation-{os.getuid()}').mkdir(parents=True, mode=0o700)
    partial_command = [*recovery]
    partial_command[4] = str(partial)
    result = subprocess.run(partial_command, capture_output=True)
    assert result.returncode == 125 and not (partial/f'.build/paseo-operation-v4-{os.getuid()}').exists(), result
    shutil.rmtree(partial)
    old_partial = [*partial_command]
    old_partial[1] = str(LEGACY)
    assert subprocess.run(old_partial, capture_output=True).returncode == 7
    partial_receipt = partial/f'.build/paseo-operation-{os.getuid()}/.owner'
    partial_before = partial_receipt.read_bytes()
    partial_canary = partial_receipt.parent/'data/preserved'
    partial_canary.write_text('preserve')
    wrong = json.loads(partial_before)
    wrong['active'] = ['private-canary']
    for payload in (b'', b'{broken', json.dumps(wrong).encode()):
        partial_receipt.write_bytes(payload)
        result = subprocess.run(partial_command, capture_output=True)
        assert result.returncode == 125 and partial_receipt.read_bytes() == payload, result
        assert partial_canary.read_text() == 'preserve'
        assert not (partial/f'.build/paseo-operation-v4-{os.getuid()}').exists()
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
        source = prefix+HELPER.read_text()
        program = f'__file__={str(HELPER)!r}; _SOURCE={source!r}; exec(_SOURCE)'
        invocation = ['python3', '-c', program, 'launch', 'paseo', str(fault_root),
                      'bash', '-c', 'exit 7', '--output-dir', str(fault_root/'output')]
        process = subprocess.Popen(invocation, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        try:
            until(reached.exists)
            if stage == 'publication':
                current_state = json.loads((fault_root/f'.build/paseo-operation-v4-{os.getuid()}/.owner').read_text())
                assert current_state['active'] is None
            process.kill()
            assert process.wait(timeout=8) == -9
        finally:
            if process.poll() is None:
                process.kill()
                process.wait()
        legacy_receipt = fault_root/f'.build/paseo-operation-{os.getuid()}/.owner'
        until(lambda: not runner.active(json.loads(legacy_receipt.read_text()).get('active')))
        for executable in (HELPER, LEGACY):
            result = subprocess.run(['python3', str(executable), 'launch', 'paseo', str(fault_root),
                                     'bash', '-c', 'exit 7', '--output-dir', str(fault_root/'output')], capture_output=True)
            assert result.returncode == 7, result
    # Receipts and data paths refuse FIFO/symlink substitution without deletion.
    receipt = operation / ".owner"
    original = receipt.read_bytes()
    receipt.rename(operation / "saved-owner")
    receipt.write_bytes(original)
    receipt.chmod(0o600)
    result = subprocess.run(command, capture_output=True, timeout=3)
    assert result.returncode == 125 and b"foreign" in result.stderr
    receipt.unlink()
    (operation / "saved-owner").rename(receipt)
    (operation / "data").rename(operation / "saved-data")
    (operation / "data").mkdir(mode=0o700)
    canary = operation / "data/foreign"
    canary.write_text("preserve")
    result = subprocess.run(command, capture_output=True, timeout=3)
    assert result.returncode == 125 and canary.read_text() == "preserve"
    canary.unlink()
    (operation / "data").rmdir()
    (operation / "saved-data").rename(operation / "data")
    receipt.unlink()
    os.mkfifo(receipt, 0o600)
    result = subprocess.run(command, capture_output=True, timeout=3)
    assert result.returncode == 125 and receipt.is_fifo()
    receipt.unlink()
    receipt.symlink_to(root / "out")
    result = subprocess.run(command, capture_output=True, timeout=3)
    assert result.returncode == 125 and receipt.is_symlink()
    receipt.unlink()
    canary = operation / "data/preserved"
    canary.write_text("preserve")
    for content in (None, b"", b"{broken", b"[]"):
        if content is not None:
            receipt.write_bytes(content)
            receipt.chmod(0o600)
        result = subprocess.run(command, capture_output=True, timeout=3)
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
    result = subprocess.run(command, capture_output=True, timeout=3)
    assert result.returncode == 125 and result.stderr == b"producer: operation failed; inspect preserved storage before retrying\n" and not (operation / "data").exists()
    # Invoke the real build boundary, faking only its expensive download.
    binary = root / "bin"
    binary.mkdir()
    curl = binary / "curl"
    curl.write_text('#!/bin/sh\nprintf scratch >/tmp/subyard-producer-direct-write\nexit 7\n')
    curl.chmod(0o755)
    result = subprocess.run(["bash", str(ROOT / "dev/build-paseo-headless.sh"), "--output-dir", str(root / "artifact")],
                            env=dict(os.environ, PATH=str(binary) + ":" + os.environ["PATH"]), capture_output=True)
    assert result.returncode == 7 and result.stdout == b"", result
    assert not Path("/tmp/subyard-producer-direct-write").exists()

# Neither checkout nor output is a Git worktree; both are short-lived disk fixtures.
with tempfile.TemporaryDirectory(prefix="producer hidden % $ : ", dir="/var/tmp") as temporary:
    base = Path(temporary)
    source, output = base / "source % $ :", base / "output % $ :"
    output.mkdir()
    (output / "canary").write_text("visible")
    for name in ("dev/build-paseo-headless.sh", "scripts/lib/producer-run.py",
                 "config/agents/paseo/bundle/package.json", "config/agents/paseo/bundle/package-lock.json",
                 "config/agents/paseo/config.json", "config/agents/paseo/paseo.service.in", "config/agents/paseo/sync-projects.mjs"):
        target = source / name
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(ROOT / name, target)
    # A relative PATH entry supports a colon-containing checkout and stays within
    # its exact bind, including when the transported candidate is under /var/tmp.
    binary = source / "bin"
    binary.mkdir()
    curl = binary / "curl"
    curl.write_text('#!/bin/sh\ntest -f "$PRODUCER_SOURCE/config/agents/paseo/config.json" || exit 95\ntest "$(cat "$PRODUCER_OUTPUT/canary")" = visible || exit 96\nprintf updated >"$PRODUCER_OUTPUT/canary"\nprintf scratch >/tmp/subyard-producer-hidden-write\nexit 7\n')
    curl.chmod(0o755)
    fixture_path = "./bin:" + os.environ["PATH"]
    with contextlib.chdir(source):
        assert Path(shutil.which("curl", path=fixture_path)).resolve() == curl
        result = subprocess.run(["bash", str(source / "dev/build-paseo-headless.sh"), "--output-dir", str(output)],
                                env=dict(os.environ, PATH=fixture_path,
                                         PRODUCER_SOURCE=str(source), PRODUCER_OUTPUT=str(output)), capture_output=True)
    (ROOT / ".build/producer-hidden.stderr").write_bytes(result.stderr)
    (ROOT / ".build/producer-hidden-result.json").write_text(json.dumps({"status": result.returncode, "canary": (output / "canary").read_text()}))
    assert result.returncode == 7 and result.stdout == b"", (result.returncode, result.stderr[:512])
    assert (output / "canary").read_text() == "updated" and not Path("/tmp/subyard-producer-hidden-write").exists()
print("ok: PID namespace cancellation, busy, stale recovery and receipt refusal")
