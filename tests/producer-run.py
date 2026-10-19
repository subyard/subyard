#!/usr/bin/env python3
"""Local lifecycle contracts; real Incus and reboot remain owning VM checks."""
import importlib.util
import contextlib
import json
import os
from pathlib import Path
import signal
import shutil
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
HELPER = ROOT / "scripts/lib/producer-run.py"
spec = importlib.util.spec_from_file_location("producer", HELPER)
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)
(ROOT / ".build").mkdir(exist_ok=True)


def until(predicate):
    deadline = time.monotonic() + 8
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(.02)
    raise AssertionError("lifecycle deadline")


with tempfile.TemporaryDirectory(prefix="producer-test-", dir=ROOT / ".build") as temporary:
    root = Path(temporary)
    marker = root / "started"
    operation = root / f".build/paseo-operation-{os.getuid()}"
    child = "import os,json,time; from pathlib import Path; f=open('/tmp/open-deleted','wb'); os.unlink(f.name); f.write(b'x'*1048576); f.flush(); fields=Path('/proc/self/stat').read_text().rsplit(') ',1)[1].split(); pid=Path('/proc/self/stat').read_text().split(' ',1)[0]; Path(__import__('sys').argv[1]).write_text(json.dumps([pid,fields[19]])); time.sleep(300)"
    import shlex
    command = ["python3", str(HELPER), "launch", "paseo", str(root), "bash", "-c",
               'setsid python3 -c ' + shlex.quote(child) + ' "$1" & wait',
               "fixture", str(marker), "--output-dir", str(root / "output")]
    for cancellation in (signal.SIGINT, signal.SIGTERM, signal.SIGKILL):
        with (root / "out").open("wb") as out, (root / "err").open("wb") as err:
            process = subprocess.Popen(command, stdout=out, stderr=err)
            try:
                until(lambda: marker.exists() and marker.stat().st_size > 0)
                pid, ticks = json.loads(marker.read_text())
                busy = subprocess.run(command, capture_output=True)
                assert busy.returncode == 125 and b"busy" in busy.stderr, busy
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
                recovery = command[:command.index("bash")] + ["bash", "-c", "exit 7", "--output-dir", str(root / "output")]
                until(lambda: not runner.active(json.loads((operation / ".owner").read_text()).get("active")))
                result = subprocess.run(recovery, capture_output=True)
                assert result.returncode == 7, result
                assert not list((operation / "data").iterdir())
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait()
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
        assert result.returncode == 125 and str(operation).encode() in result.stderr
        assert b"preserved" in result.stderr and b"inspect" in result.stderr
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
    assert result.returncode == 125 and str(operation).encode() in result.stderr and not (operation / "data").exists()
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
