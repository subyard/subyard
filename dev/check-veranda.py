#!/usr/bin/env python3
"""Run Veranda's shared local and CI host-free checks."""
import argparse
import os
from pathlib import Path
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
    command = ["cargo", "+stable", "test", "--manifest-path", "veranda/client/Cargo.toml", "--locked"]
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


def rust_lint(env):
    manifest = ["--manifest-path", "veranda/client/Cargo.toml"]
    commands = [
        ("Rust formatting", ["cargo", "+stable", "fmt", *manifest, "--check"]),
        ("Rust Clippy", ["cargo", "+stable", "clippy", *manifest,
                         "--locked", "--all-targets", "--", "-D", "warnings"]),
    ]
    code = 0
    for name, command in commands:
        result = check(name, command, env)
        code = code or result
    return code


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--rust-only", action="store_true", help="run only native Rust checks")
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
    code = rust_lint(env)
    test_code = rust_check(env)
    code = code or test_code
    if not code:
        print("Veranda native Rust checks passed" if args.rust_only else "Veranda host-free checks passed", flush=True)
    return code


if __name__ == "__main__":
    sys.exit(main())
