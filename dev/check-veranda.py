#!/usr/bin/env python3
"""Run Veranda's shared local and CI host-free checks."""
import argparse
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile


ROOT = Path(__file__).resolve().parent.parent


def check(name, command, env):
    print(f"Checking {name}", flush=True)
    try:
        code = subprocess.run(command, cwd=ROOT, env=env).returncode
    except OSError as error:
        print(f"{name}: could not start ({error})", file=sys.stderr)
        return 1
    if code:
        print(f"{name}: failed (exit {code})", file=sys.stderr)
    return code if code >= 0 else 128 - code


def rust_check(env):
    command = ["cargo", "test", "--manifest-path", "veranda/src-tauri/Cargo.toml",
               "--no-default-features", "--locked"]
    if sys.platform != "linux":
        print("Linux symlink TMPDIR probe: skipped on this platform", flush=True)
        return check("native Rust tests", command, env)
    print("Native Rust tests include the Linux symlink TMPDIR probe", flush=True)
    code = 0
    try:
        with tempfile.TemporaryDirectory(prefix="veranda-check-", dir="/tmp") as temporary:
            root = Path(temporary)
            root.chmod(0o700)
            real = root / "real"
            real.mkdir(mode=0o700)
            real.chmod(0o700)
            alias = root / "alias"
            alias.symlink_to(real, target_is_directory=True)
            code = check("native Rust tests", command, dict(env, TMPDIR=str(alias)))
    except Exception as error:
        print(f"Native Rust temporary-state setup/cleanup failed ({type(error).__name__})",
              file=sys.stderr)
        return code or 1
    return code


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--rust-only", action="store_true", help="run only native Rust tests")
    args = parser.parse_args()
    env = dict(os.environ, VERANDA_TEST_PYTHON=sys.executable)
    if not args.rust_only:
        code = check("runner temporary-path contract", [sys.executable, "dev/check-veranda-test.py"], env)
        if code:
            return code
        if sys.platform == "linux":
            code = check("Linux resource probe isolation", [sys.executable, "dev/measure-veranda-test.py"], env)
            if code:
                return code
        else:
            print("Linux resource probe isolation: skipped on this platform", flush=True)
        npm = shutil.which("npm.cmd" if os.name == "nt" else "npm")
        if not npm:
            print("Veranda checks require npm on PATH", file=sys.stderr)
            return 1
        for name, command in [
            ("frontend types", [npm, "--prefix", "veranda", "run", "check"]),
            ("frontend tests", [npm, "--prefix", "veranda", "test"]),
            ("packaging contract", ["node", "--test", "dev/build-veranda.test.mjs"]),
            ("frontend build", [npm, "--prefix", "veranda", "run", "build"]),
        ]:
            code = check(name, command, env)
            if code:
                return code
    code = rust_check(env)
    if not code:
        print("Veranda native Rust checks passed" if args.rust_only else "Veranda host-free checks passed", flush=True)
    return code


if __name__ == "__main__":
    sys.exit(main())
