#!/usr/bin/env python3
"""Shared Cargo intermediates with caller-owned final artifacts (Cargo >= 1.91)."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import pwd
import re
import shlex
import shutil
import subprocess
import tempfile


def environment(target_dir, *, snapshot=False, toolchain=None, env=None, source_root=None, manifest_path=None, run_target=False):
    env = dict(os.environ if env is None else env)
    # HOME may be a disposable fixture; homes belong to the invoking developer.
    home = Path(pwd.getpwuid(os.getuid()).pw_dir)
    for key, value in [('CARGO_HOME', home/'.cargo'), ('RUSTUP_HOME', home/'.rustup')]:
        env.setdefault(key, str(value))
    selector = ['+'+toolchain] if toolchain else []
    cargo = subprocess.check_output(['cargo', *selector, '--version'], env=env, text=True).strip()
    version = re.match(r'cargo (\d+)\.(\d+)', cargo)
    if not version or tuple(map(int, version.groups())) < (1, 91):
        raise ValueError('separate Cargo build directories require Cargo >= 1.91')
    # Make Cargo use the compiler whose resolved identity we record, even when
    # a user Cargo config names another compiler. Explicit compiler env wins.
    env['RUSTC'] = env.get('RUSTC', env.get('CARGO_BUILD_RUSTC', shutil.which('rustc', path=env.get('PATH'))))
    compiler = subprocess.check_output([env['RUSTC'], *selector, '-vV'], env=env, text=True)
    wrapper = str(Path(source_root or Path(__file__).resolve().parents[1])/'dev/cargo-workspace-rustc.sh')
    previous = env.get('RUSTC_WORKSPACE_WRAPPER', '')
    if previous != wrapper:
        env['SUBYARD_RUSTC_WORKSPACE_WRAPPER'] = previous
    env['RUSTC_WORKSPACE_WRAPPER'] = wrapper
    if manifest_path is None:
        raise ValueError('supply the Cargo manifest path to validate local dependency safety')
    metadata = json.loads(subprocess.check_output(['cargo', *selector, 'metadata', '--offline', '--no-deps', '--format-version', '1', '--manifest-path', str(manifest_path)], env=env, text=True))
    members = {str(Path(package['manifest_path']).parent) for package in metadata['packages']}
    if any(dependency.get('path') not in members for package in metadata['packages'] for dependency in package['dependencies'] if dependency.get('path')):
        # Cargo does not apply workspace wrappers to non-member path crates.
        # Checkout-specific flags safely invalidate those crates, including their
        # built-in manifest paths, at the cost of dependency reuse for this mode.
        flags = env.get('CARGO_ENCODED_RUSTFLAGS')
        flags = flags.split('\x1f') if flags is not None else env.get('RUSTFLAGS', '').split()
        remap = '--remap-path-prefix='+str(Path(source_root or Path(__file__).resolve().parents[1]))+'=public-source'
        if remap not in flags:
            flags.append(remap)
        env.pop('RUSTFLAGS', None)
        env['CARGO_ENCODED_RUSTFLAGS'] = '\x1f'.join(flags)

    for key in ['RUSTC_WRAPPER', 'SUBYARD_RUSTC_WORKSPACE_WRAPPER']:
        if env.get(key):
            compiler += '\n'+key+'='+env[key]
            resolved = shutil.which(env[key], path=env.get('PATH'))
            if resolved:
                compiler += hashlib.sha256(Path(resolved).read_bytes()).hexdigest()

    identity = hashlib.sha256((cargo+'\n'+compiler).encode()).hexdigest()[:24]
    root = env.get('SUBYARD_CARGO_BUILD_ROOT', str(home/'.cache/subyard/cargo-build'))
    env.setdefault('CARGO_BUILD_BUILD_DIR', str(Path(root)/identity))
    if 'CARGO_TARGET_DIR' not in env:
        target = Path(target_dir).absolute()
        if run_target:
            target.mkdir(parents=True, exist_ok=True)
            target = Path(tempfile.mkdtemp(prefix='run-', dir=target))
        env['CARGO_TARGET_DIR'] = str(target)
    for key in ['CARGO_HOME', 'RUSTUP_HOME', 'CARGO_BUILD_BUILD_DIR', 'CARGO_TARGET_DIR']:
        if not Path(env[key]).is_absolute():
            raise ValueError(key+' must be absolute')
    if snapshot:
        env.setdefault('CARGO_INCREMENTAL', '0')
    return env


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--target-dir', required=True)
    parser.add_argument('--snapshot', action='store_true')
    parser.add_argument('--manifest-path', required=True)
    parser.add_argument('--run-target', action='store_true', help='allocate an invocation-owned final directory unless CARGO_TARGET_DIR is explicit')
    args = parser.parse_args()
    result = environment(args.target_dir, snapshot=args.snapshot, manifest_path=args.manifest_path, run_target=args.run_target)
    for key in ['CARGO_HOME', 'RUSTUP_HOME', 'CARGO_BUILD_BUILD_DIR', 'CARGO_TARGET_DIR', 'CARGO_INCREMENTAL', 'RUSTC', 'RUSTC_WORKSPACE_WRAPPER', 'SUBYARD_RUSTC_WORKSPACE_WRAPPER', 'CARGO_ENCODED_RUSTFLAGS']:
        if key in result:
            print('export '+key+'='+shlex.quote(result[key]))
