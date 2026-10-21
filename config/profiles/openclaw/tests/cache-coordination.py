#!/usr/bin/env python3
"""Host-free outcomes for the profile's shared-cache process contract."""
import fcntl
import json
import os
import re
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time

PROFILE = Path(__file__).resolve().parents[1]
HELPER = PROFILE / "cache/sy-cache.sh"


def run(*args, **kwargs):
    return subprocess.run(args, check=True, text=True, capture_output=True, **kwargs)


def wait_file(path, process):
    deadline = time.monotonic() + 10
    while not path.exists():
        if process.poll() is not None or time.monotonic() > deadline:
            raise AssertionError(f"command did not start: {process.communicate()}")
        time.sleep(0.01)


with tempfile.TemporaryDirectory(prefix="subyard-cache-", dir="/tmp") as directory:
    root = Path(directory)
    lock = root / ".sy-cache.lock"
    lock.touch(mode=0o644)
    lock.chmod(0o644)
    env = {k: v for k, v in os.environ.items() if not k.startswith("_SY_CACHE_")}
    env.pop("PIP_NO_CACHE_DIR", None)
    env.update(SY_CACHE_LOCK=str(lock), SY_CACHE_TOOL=str(HELPER), SY_CACHE_WAIT_SECONDS="2")
    worker = root / "worker.py"
    worker.write_text('''import os, pathlib, sys, time
pathlib.Path(sys.argv[1]).write_text(str(os.getpid())+" "+os.environ["_SY_CACHE_FD"]+"\\n")
while not pathlib.Path(sys.argv[2]).exists(): time.sleep(.01)
''')
    children = []

    def start(command, marker, release, extra=None):
        proc = subprocess.Popen([*command, sys.executable, str(worker), str(marker), str(release)],
                                env=env | (extra or {}), stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, text=True, start_new_session=True)
        children.append(proc)
        return proc

    def finish(proc, release):
        release.touch()
        output = proc.communicate(timeout=10)
        assert proc.returncode == 0, output

    def exclude(first, second, label):
        a, b, ra, rb = (root / f"{label}-{suffix}" for suffix in ("a", "b", "ra", "rb"))
        holder = start(first, a, ra)
        wait_file(a, holder)
        waiter = start(second, b, rb)
        time.sleep(.15)
        assert not b.exists() and waiter.poll() is None, "exclusive/shared operations overlapped"
        finish(holder, ra)
        wait_file(b, waiter)
        finish(waiter, rb)

    try:
        shared = [str(HELPER), "shared", "--"]
        exclusive = [str(HELPER), "exclusive", "--"]
        exclude(shared, exclusive, "shared-first")
        exclude(exclusive, shared, "exclusive-first")
        exclude([str(HELPER), "browser", "1.50.0", "--"],
                [str(HELPER), "browser", "1.50.0", "--"], "browser-installs")
        browser = run(str(HELPER), "browser", "1.50.0", "--", "sh", "-c",
                      'printf "%s:%s" "$PLAYWRIGHT_BROWSERS_PATH" "$PLAYWRIGHT_SKIP_BROWSER_GC"', env=env).stdout
        assert "browser/linux-" in browser and browser.endswith("/1.50.0:1")
        a, b, ra, rb = (root / x for x in ("sa", "sb", "sra", "srb"))
        pa = start(shared, a, ra)
        pb = start(shared, b, rb)
        wait_file(a, pa)
        wait_file(b, pb)
        finish(pa, ra)
        finish(pb, rb)

        # A matching open descriptor is insufficient: native held mode must match the marker.
        def reject_unheld_or_wrong_mode(command, marker, test_env):
            for operation, declared in ((None, "shared"), (None, "exclusive"),
                                        (fcntl.LOCK_SH, "exclusive"), (fcntl.LOCK_EX, "shared")):
                with lock.open("rb") as opened:
                    if operation is not None:
                        fcntl.flock(opened, operation)
                    result = subprocess.run(command, env=test_env | {
                        "_SY_CACHE_FD": str(opened.fileno()), "_SY_CACHE_MODE": declared},
                        pass_fds=(opened.fileno(),), capture_output=True, timeout=10)
                    assert result.returncode != 0 and not marker.exists(), "unheld/wrong-mode FD bypassed coordination"

        reject_unheld_or_wrong_mode([*shared, "touch", str(root / "fd-bypass")], root / "fd-bypass", env)
        reject_unheld_or_wrong_mode([*exclusive, "touch", str(root / "fd-bypass")], root / "fd-bypass", env)
        run(*shared, *shared, "true", env=env)
        run(*exclusive, *shared, "true", env=env)
        run(*exclusive, *exclusive, "true", env=env)

        # Failed acquisition, absent locks, stale inherited markers and cancellation never execute.
        missing = run(str(HELPER), "key", "pnpm", "11.2.2", "node-137", env=env).stdout
        assert "11.2.2-node-137" in missing
        assert missing != run(str(HELPER), "key", "pnpm", "12", "node-137", env=env).stdout
        assert missing != run(str(HELPER), "key", "pnpm", "11.2.2", "node-999", env=env).stdout
        bindir = root / "bin"
        bindir.mkdir()
        (bindir / "uname").write_text('#!/bin/sh\ncase "$1" in -s) echo OtherOS;; -m) echo otherarch;; esac\n')
        (bindir / "uname").chmod(0o755)
        assert missing != run(str(HELPER), "key", "pnpm", "11.2.2", "node-137",
                              env=env | {"PATH": str(bindir) + ":" + env["PATH"]}).stdout
        for overrides in ({"SY_CACHE_LOCK": str(root / "missing")},
                          {"_SY_CACHE_FD": "999", "_SY_CACHE_MODE": "exclusive"}):
            result = subprocess.run([*shared, "touch", str(root / "bypass")], env=env | overrides,
                                    capture_output=True)
            assert result.returncode != 0 and not (root / "bypass").exists()
        (bindir / "flock").write_text("#!/bin/sh\nexit 1\n")
        (bindir / "flock").chmod(0o755)
        result = subprocess.run([*shared, "touch", str(root / "bypass")],
                                env=env | {"PATH": str(bindir) + ":" + env["PATH"]}, capture_output=True)
        assert result.returncode != 0 and not (root / "bypass").exists()
        a, ra = root / "cancel-a", root / "cancel-ra"
        holder = start(shared, a, ra)
        wait_file(a, holder)
        waiter = start(exclusive, root / "cancel-b", root / "cancel-rb")
        time.sleep(.1)
        os.killpg(waiter.pid, signal.SIGTERM)
        waiter.communicate(timeout=10)
        assert waiter.returncode != 0 and not (root / "cancel-b").exists()
        timed = subprocess.run([*exclusive, "touch", str(root / "bypass")],
                               env=env | {"SY_CACHE_WAIT_SECONDS": "0.05"}, capture_output=True)
        assert timed.returncode != 0 and not (root / "bypass").exists()
        finish(holder, ra)
        run(*exclusive, "true", env=env)
        # Ordinary build tools close extra FDs when starting nested tools: shared work reacquires.
        run(*shared, sys.executable, "-c",
            "import subprocess,sys; subprocess.run(sys.argv[1:],check=True)", *shared, "true", env=env)
        nested = subprocess.run([*shared, *exclusive, "touch", str(root / "bypass")],
                                env=env, capture_output=True)
        assert nested.returncode != 0 and not (root / "bypass").exists()

        # Provision repairs ownership/mode while preserving the lock inode and waiting for readers.
        source = (PROFILE / "provision.sh").read_text()
        repair = source.split("repair_cache_lock() {", 1)[1].split("\n}\n", 1)[0]
        repair_script = root / "repair.sh"
        repair_script.write_text("set -eu\nrepair_cache_lock() {" + repair + '\n}\nrepair_cache_lock "$1" "$2"\n')
        before = lock.stat().st_ino
        lock.chmod(0o600)
        a, ra = root / "repair-a", root / "repair-ra"
        holder = start(shared, a, ra)
        wait_file(a, holder)
        repair_proc = subprocess.Popen(["bash", str(repair_script), str(lock), f"{os.getuid()}:{os.getgid()}"],
                                       env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, start_new_session=True)
        children.append(repair_proc)
        time.sleep(.15)
        assert repair_proc.poll() is None and (lock.stat().st_mode & 0o777) == 0o600
        finish(holder, ra)
        assert repair_proc.wait(timeout=10) == 0
        run("bash", str(repair_script), str(lock), f"{os.getuid()}:{os.getgid()}", env=env)
        assert lock.stat().st_ino == before and (lock.stat().st_mode & 0o777) == 0o644

        # Execute actual generated pnpm/npm wrappers with cache-writing fixture children.
        render = root / "render.sh"
        functions = []
        for name in ("render_browser_policy", "render_pnpm_wrapper", "render_npm_wrapper"):
            body = source.split(name + "() {", 1)[1].split("\n}\n", 1)[0]
            functions.append(name + "() {" + body + "\n}\n")
        render.write_text("".join(functions) + '\nCACHE_PNPM_STORE=/srv/cache/pnpm PNPM_VERSION=11.2.2 render_pnpm_wrapper > "$1/pnpm"\nrender_npm_wrapper fixture-npm > "$1/npm"\nrender_npm_wrapper /fixture/npx-cli.js > "$1/npx"\n')
        run("bash", str(render), str(bindir), env=env)
        for name in ("pnpm", "npm", "npx"):
            (bindir / name).chmod(0o755)
        (bindir / "flock").unlink()
        (bindir / "uname").unlink()
        (bindir / "node").write_text('''#!/bin/sh
if [ "$1" = -p ]; then
  case "$2" in *require*)
    [ "${CACHE_TEST_REQUIRE_FAIL:-}" != 1 ] || exit 1
    printf '%s' "${3:-}" > "$CACHE_TEST_PROJECT"
    echo 1.50.0 ;;
    *) echo 137 ;;
  esac
  exit
fi
printf '%s\\0' "$@" > "$CACHE_TEST_RAW_ARGS"
printf '%s\\n' "$_SY_CACHE_MODE" "${PLAYWRIGHT_BROWSERS_PATH:-}" "${PLAYWRIGHT_SKIP_BROWSER_GC:-}" > "$CACHE_TEST_MODE"
exec "$CACHE_TEST_PYTHON" "$CACHE_TEST_WORKER" "$CACHE_TEST_MARKER" "$CACHE_TEST_RELEASE"
''')
        (bindir / "corepack").write_text('''#!/bin/sh
printf '%s\\n' "$@" > "$CACHE_TEST_ARGS"
printf '%s\\0' "$@" > "$CACHE_TEST_RAW_ARGS"
printf '%s\\n' "$_SY_CACHE_MODE" "${PLAYWRIGHT_BROWSERS_PATH:-}" "${PLAYWRIGHT_SKIP_BROWSER_GC:-}" > "$CACHE_TEST_MODE"
exec "$CACHE_TEST_PYTHON" "$CACHE_TEST_WORKER" "$CACHE_TEST_MARKER" "$CACHE_TEST_RELEASE"
''')
        for name in ("node", "corepack"):
            (bindir / name).chmod(0o755)
        toolenv = env | {"PATH": str(bindir) + ":" + env["PATH"], "CACHE_TEST_PYTHON": sys.executable,
                         "CACHE_TEST_WORKER": str(worker), "CACHE_TEST_ARGS": str(root / "args"),
                         "CACHE_TEST_RAW_ARGS": str(root / "raw-args"), "CACHE_TEST_MODE": str(root / "mode"),
                         "CACHE_TEST_PROJECT": str(root / "browser-project")}

        def tool(command, marker, release, extra=None):
            proc = subprocess.Popen(command, env=toolenv | {"CACHE_TEST_MARKER": str(marker),
                                                           "CACHE_TEST_RELEASE": str(release)} | (extra or {}),
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
                                    start_new_session=True)
            children.append(proc)
            return proc

        for writer, mutation, label in (([str(bindir / "pnpm"), "install"], [str(bindir / "pnpm"), "store", "prune"], "pnpm"),
                                         ([str(bindir / "npm"), "install"], [str(bindir / "npm"), "cache", "clean", "--force"], "npm"),
                                         ([str(bindir / "pnpm"), "install"], [str(bindir / "pnpm"), "exec", "playwright", "install"], "pnpm-browser"),
                                         ([str(bindir / "npm"), "install"], [str(bindir / "npm"), "exec", "playwright", "install"], "npm-browser")):
            for reverse in (False, True):
                a, b, ra, rb = (root / f"{label}-{reverse}-{x}" for x in ("a", "b", "ra", "rb"))
                first, second = (mutation, writer) if reverse else (writer, mutation)
                pa = tool(first, a, ra)
                wait_file(a, pa)
                pb = tool(second, b, rb)
                time.sleep(.15)
                assert not b.exists() and pb.poll() is None
                finish(pa, ra)
                wait_file(b, pb)
                finish(pb, rb)
        assert "pnpm/linux-" in (root / "args").read_text()
        assert "11.2.2-node-137" in (root / "args").read_text()

        # Fresh dependency installs must not resolve an already-installed Playwright module.
        # Exact argv tokens survive the wrapper, including a single argument containing spaces.
        package_installs = [
            ("npm", ["install", "playwright"]),
            ("npm", ["install", "@playwright/test"]),
            ("npm", ["install", "playwright@1.50.0"]),
            ("npm", ["install", "@playwright/test@1.50.0"]),
            ("pnpm", ["install", "playwright"]),
            ("pnpm", ["add", "@playwright/test"]),
            ("pnpm", ["install", "playwright@1.50.0"]),
            ("pnpm", ["install", "playwright install"]),
            ("npm", ["exec", "playwright install"]),
            ("npx", ["playwright install"]),
            ("pnpm", ["--dir", "directory with spaces", "install", "playwright@1.50.0"]),
            ("pnpm", ["--filter", "workspace", "install", "@playwright/test"]),
            ("npm", ["--prefix=directory with spaces", "install", "playwright"]),
            ("npm", ["--workspace", "workspace", "install", "playwright@1.50.0"]),
            ("npm", ["install", "--cache", "/tmp/download cache", "playwright@1.50.0"]),
            ("npm", ["--global", "install", "playwright@1.49.0"]),
            ("pnpm", ["-g", "add", "playwright@1.49.0"]),
        ]
        for index, (manager, args) in enumerate(package_installs):
            marker, release = root / f"dependency-{index}", root / f"dependency-release-{index}"
            proc = tool([str(bindir / manager), *args], marker, release, {"CACHE_TEST_REQUIRE_FAIL": "1"})
            wait_file(marker, proc)
            assert (root / "mode").read_text().splitlines()[0] == "shared"
            actual = [part.decode() for part in (root / "raw-args").read_bytes().split(b"\0")[:-1]]
            assert any(actual[i:i + len(args)] == args for i in range(len(actual))), actual
            # A second normal installer must enter while this one still holds its shared lock.
            second_marker, second_release = root / f"dependency-peer-{index}", root / f"dependency-peer-release-{index}"
            peer = tool([str(bindir / "npm"), "install"], second_marker, second_release)
            wait_file(second_marker, peer)
            finish(proc, release)
            finish(peer, second_release)
        browser_commands = [
            ("pnpm", ["exec", "playwright", "install", "chromium"]),
            ("pnpm", ["exec", "--", "playwright", "uninstall"]),
            ("npm", ["exec", "--", "playwright", "install"]),
            ("npm", ["x", "--", "playwright", "install"]),
            ("npm", ["exec", "--cache", "/tmp/download-cache", "--", "playwright", "install"]),
            ("npm", ["exec", "--cache", "/tmp/download cache", "--", "playwright", "install"]),
            ("npm", ["exec", "--cache", "/tmp/download cache", "playwright", "install"]),
            ("npm", ["exec", "--cache=/tmp/download cache", "playwright", "install"]),
            ("npm", ["exec", "--", "playwright", "install", "--package=playwright@1.49.0"]),
            ("npx", ["--yes", "playwright", "install"]),
            ("pnpm", ["--dir", "directory with spaces", "exec", "playwright", "install"]),
            ("pnpm", ["-C", "directory with spaces", "exec", "playwright", "install"]),
            ("pnpm", ["--dir=directory with spaces", "exec", "playwright", "install"]),
            ("pnpm", ["-Cdirectory with spaces", "exec", "playwright", "install"]),
            ("npm", ["--prefix", "directory with spaces", "exec", "--", "playwright", "install"]),
            ("npm", ["--prefix=directory with spaces", "exec", "--", "playwright", "install"]),
        ]
        for index, (manager, args) in enumerate(browser_commands):
            marker, release = root / f"browser-shape-{index}", root / f"browser-shape-release-{index}"
            proc = tool([str(bindir / manager), *args], marker, release)
            wait_file(marker, proc)
            declared, path, skip_gc = (root / "mode").read_text().splitlines()
            assert declared == "exclusive" and path.endswith("/1.50.0") and skip_gc == "1"
            expected_project = "directory with spaces" if any("directory with spaces" in arg for arg in args) else str(Path.cwd())
            assert (root / "browser-project").read_text() == expected_project
            actual = [part.decode() for part in (root / "raw-args").read_bytes().split(b"\0")[:-1]]
            assert any(actual[i:i + len(args)] == args for i in range(len(actual))), actual
            finish(proc, release)
        ambiguous_commands = [
            ("pnpm", ["--filter", "workspace", "exec", "playwright", "install"]),
            ("pnpm", ["--filter=workspace", "exec", "playwright", "install"]),
            ("pnpm", ["-Fworkspace", "exec", "playwright", "install"]),
            ("pnpm", ["-F", "workspace", "exec", "playwright", "install"]),
            ("pnpm", ["exec", "--workspace-root", "playwright", "install"]),
            ("pnpm", ["exec", "-w", "playwright", "install"]),
            ("pnpm", ["-w", "exec", "playwright", "install"]),
            ("npm", ["--workspace", "workspace", "exec", "--", "playwright", "install"]),
            ("npm", ["--workspace=workspace", "exec", "--", "playwright", "install"]),
            ("npm", ["-w", "workspace", "exec", "--", "playwright", "install"]),
            ("npm", ["exec", "--workspace=workspace", "--", "playwright", "install"]),
            ("npm", ["--unknown-option", "value", "exec", "playwright", "install"]),
            ("npm", ["exec", "--unknown-option", "value", "--", "playwright", "install"]),
        ]
        for index, (manager, args) in enumerate(ambiguous_commands):
            marker, release = root / f"ambiguous-browser-{index}", root / f"ambiguous-browser-release-{index}"
            proc = tool([str(bindir / manager), *args], marker, release)
            output = proc.communicate(timeout=10)
            assert proc.returncode != 0 and not marker.exists() and "sy-cache browser VERSION" in output[1]
            proc = tool([str(HELPER), "browser", "1.50.0", "--", str(bindir / manager), *args], marker, release)
            wait_file(marker, proc)
            assert (root / "mode").read_text().splitlines()[0] == "exclusive"
            finish(proc, release)

        # A selected registry/global CLI can differ from the locally probed 1.50.0 package.
        # Refuse before invoking the native command; explicit helper1.49 supplies the actual key.
        version_selected_commands = [
            ("npx", ["--package", "playwright@1.49.0", "playwright", "install"]),
            ("npx", ["--package=playwright@1.49.0", "playwright", "install"]),
            ("npx", ["-p", "playwright@1.49.0", "playwright", "install"]),
            ("npx", ["-pplaywright@1.49.0", "playwright", "install"]),
            ("npm", ["exec", "--package", "playwright@1.49.0", "--", "playwright", "install"]),
            ("npm", ["exec", "--package=playwright@1.49.0", "--", "playwright", "install"]),
            ("npm", ["exec", "-p", "playwright@1.49.0", "playwright", "install"]),
            ("npm", ["exec", "-pplaywright@1.49.0", "playwright", "install"]),
            ("npm", ["exec", "--package", "playwright", "--", "playwright", "install"]),
            ("npm", ["exec", "playwright", "install", "--package=playwright@1.49.0"]),
            ("npm", ["x", "playwright", "install", "--package=playwright@1.49.0"]),
            ("npm", ["exec", "playwright", "install", "--package", "playwright@1.49.0"]),
            ("npm", ["exec", "playwright", "install", "-g"]),
            ("npm", ["--global", "exec", "playwright", "install"]),
            ("npm", ["exec", "-g", "playwright", "install"]),
            ("pnpm", ["-g", "exec", "playwright", "install"]),
            ("pnpm", ["dlx", "playwright", "install"]),
            ("pnpm", ["dlx", "playwright@1.49.0", "install"]),
        ]
        for index, (manager, args) in enumerate(version_selected_commands):
            marker, release = root / f"selected-version-{index}", root / f"selected-version-release-{index}"
            proc = tool([str(bindir / manager), *args], marker, release)
            output = proc.communicate(timeout=10)
            assert proc.returncode != 0 and not marker.exists() and "sy-cache browser VERSION" in output[1]
            proc = tool([str(HELPER), "browser", "1.49.0", "--", str(bindir / manager), *args], marker, release)
            wait_file(marker, proc)
            declared, path, skip_gc = (root / "mode").read_text().splitlines()
            assert declared == "exclusive" and path.endswith("/1.49.0") and skip_gc == "1"
            finish(proc, release)

        # Package-qualified execution requires the explicit helper's version instead of guessing.
        marker, release = root / "browser-qualified", root / "browser-qualified-release"
        proc = tool([str(bindir / "npx"), "playwright@1.50.0", "install"], marker, release)
        assert proc.wait(timeout=10) != 0 and not marker.exists()
        proc = tool([str(HELPER), "browser", "1.50.0", "--", str(bindir / "pnpm"), "dlx",
                     "playwright@1.50.0", "install"], marker, release)
        wait_file(marker, proc)
        assert (root / "mode").read_text().splitlines()[0] == "exclusive"
        finish(proc, release)

        # Real venv startup, launcher, -m pip and -I -m pip; no downloads or interpreter relocation.
        venv = root / "venv"
        run(sys.executable, "-m", "venv", "--without-pip", str(venv))
        python = venv / "bin/python"
        site = Path(run(str(python), "-c", 'import sysconfig; print(sysconfig.get_path("purelib"))').stdout.strip())
        (site / "subyard_cache.py").write_bytes((PROFILE / "cache/subyard_cache.py").read_bytes())
        (site / "subyard_cache.pth").write_text("import subyard_cache\n")
        (site / "pip").mkdir()
        (site / "pip/__init__.py").touch()
        (site / "pip/__main__.py").write_text('''import json, os, pathlib, sys, time
print(json.dumps({"prefix": sys.prefix, "cache": os.environ.get("PIP_CACHE_DIR")}), flush=True)
pathlib.Path(os.environ["CACHE_TEST_MARKER"]).touch()
while not pathlib.Path(os.environ["CACHE_TEST_RELEASE"]).exists(): time.sleep(.01)
''')
        configured_root = root / "configured pip cache"
        (site / "subyard_cache_root").write_text(str(configured_root) + "\n")
        launcher = venv / "bin/pip"
        launcher.write_text(f"#!{python}\nimport pip.__main__\n")
        launcher.chmod(0o755)
        for invocation, label in (([str(python), "-m", "pip"], "module"),
                                  ([str(python), "-I", "-m", "pip"], "isolated"),
                                  ([str(launcher)], "launcher")):
            for reverse in (False, True):
                a, b, ra, rb = (root / f"pip-{label}-{reverse}-{x}" for x in ("a", "b", "ra", "rb"))
                args = ("cache", "purge") if reverse else ("install",)
                other = ("install",) if reverse else ("cache", "purge")
                pa = tool([*invocation, *args], a, ra)
                wait_file(a, pa)
                pb = tool([*invocation, *other], b, rb)
                time.sleep(.15)
                assert not b.exists() and pb.poll() is None, "pip destructive cache race"
                ra.touch()
                output = pa.communicate(timeout=10)
                assert pa.returncode == 0, output
                info = json.loads(output[0])
                assert info["prefix"] == str(venv)
                assert sys.implementation.cache_tag in info["cache"]
                wait_file(b, pb)
                finish(pb, rb)
        # Provisioned defaults survive removal of the setup environment; explicit overrides win.
        for index, override in enumerate((None, str(root / "explicit pip cache"))):
            marker, release = root / f"pip-root-{index}", root / f"pip-root-release-{index}"
            release.touch()
            pip_env = toolenv | {"CACHE_TEST_MARKER": str(marker), "CACHE_TEST_RELEASE": str(release)}
            pip_env.pop("PIP_CACHE_DIR", None)
            if override is not None:
                pip_env["PIP_CACHE_DIR"] = override
            result = run(str(python), "-m", "pip", "install", env=pip_env)
            cache = json.loads(result.stdout)["cache"]
            expected_root = override or str(configured_root)
            assert cache == f"{expected_root}/{sys.platform}-{os.uname().machine}/{sys.implementation.cache_tag}", cache
        pip_fault_marker = root / "pip-fd-bypass"
        pip_fault_release = root / "pip-fd-release"
        pip_fault_release.touch()
        pip_fault_env = toolenv | {"CACHE_TEST_MARKER": str(pip_fault_marker),
                                   "CACHE_TEST_RELEASE": str(pip_fault_release)}
        reject_unheld_or_wrong_mode([str(python), "-m", "pip", "install"], pip_fault_marker, pip_fault_env)
        reject_unheld_or_wrong_mode([str(python), "-m", "pip", "cache", "purge"], pip_fault_marker, pip_fault_env)
        for timeout in ("NaN", "Infinity", "-1"):
            result = subprocess.run([str(python), "-m", "pip", "install"],
                                    env=pip_fault_env | {"SY_CACHE_WAIT_SECONDS": timeout}, capture_output=True)
            assert result.returncode != 0 and not pip_fault_marker.exists(), "invalid timeout entered pip"
        # Valid inherited native READ/WRITE locks work without changing the interpreter prefix.
        for outer, action, label in ((shared, ["install"], "shared"), (exclusive, ["cache", "purge"], "exclusive")):
            marker = root / f"pip-valid-{label}"
            result = run(*outer, str(python), "-m", "pip", *action,
                         env=toolenv | {"CACHE_TEST_MARKER": str(marker), "CACHE_TEST_RELEASE": str(pip_fault_release)})
            assert marker.exists() and json.loads(result.stdout)["prefix"] == str(venv)

        # A cancelled real interpreter waiting for cache maintenance cannot enter pip later.
        a, ra = root / "pip-cancel-a", root / "pip-cancel-ra"
        holder = start(exclusive, a, ra)
        wait_file(a, holder)
        marker = root / "pip-cancel-b"
        waiting_pip = tool([str(python), "-m", "pip", "install"], marker, root / "pip-cancel-rb")
        time.sleep(.1)
        os.killpg(waiting_pip.pid, signal.SIGTERM)
        waiting_pip.communicate(timeout=10)
        assert waiting_pip.returncode != 0 and not marker.exists()
        finish(holder, ra)
        result = subprocess.run([str(python), "-m", "pip", "--isolated", "install"],
                                env=toolenv, capture_output=True)
        assert result.returncode != 0
        result = subprocess.run([str(python), "-m", "pip", "install"],
                                env=toolenv | {"SY_CACHE_LOCK": str(root / "missing")}, capture_output=True)
        assert result.returncode != 0
        # The physical fixture must reject a negative probe whose holder expired.
        source = (PROFILE / 'tests/e2e/project-isolation.sh').read_text()
        guards = '\n'.join(re.search(r'^  '+name+r'\(\) \{\n.*?^  \}', source, re.M | re.S)[0]
                           for name in ('cache_holder_live', 'cache_reject'))
        guards = guards.replace('/srv/cache/.sy-cache.lock', str(lock))
        marker, release = root/'reader-ready', root/'release'
        holder = start(shared, marker, release)
        wait_file(marker, holder)
        script = '''set -euo pipefail
instance=fixture project=fixture shared=$1 cache_holder=reader cache_mode=READ
cache_transport=()
incus() { shift 5; "$@"; }
die() { printf '%s\\n' "$*" >&2; exit 2; }
expire_holder() {
  touch "$shared/release"
  for attempt in $(seq 1 1000); do
    cache_holder_live || return 0
    sleep .01
  done
  return 98
}
'''+guards+'\ncache_reject l1-exclusive bypass expire_holder\n'
        expired = subprocess.run(['bash', '-c', script, 'fixture', str(root)],
                                 env=env, capture_output=True, text=True, timeout=15)
        finish(holder, release)
        assert expired.returncode == 2 and 'holder expired during l1-exclusive' in expired.stderr, expired
        assert 'bypass' not in expired.stderr
        print("ok: cache shared/exclusive exclusion, wrappers, cancellation, inode repair, keys and pip interpreter hook")
    finally:
        for proc in children:
            if proc.poll() is None:
                os.killpg(proc.pid, signal.SIGKILL)
            proc.communicate()
