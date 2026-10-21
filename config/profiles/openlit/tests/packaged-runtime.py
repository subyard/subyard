#!/usr/bin/env python3
"""Check actual runtime bundles without a source checkout or guest execution."""
import hashlib
import json
from pathlib import Path, PurePosixPath
import runpy
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[4]
HELPER = 'internal/adapters/configmaterial/tomli_w.py'


def extract(bundle, destination):
    with tarfile.open(bundle) as archive:
        for member in archive.getmembers():
            path = PurePosixPath(member.name)
            if path.is_absolute() or '..' in path.parts or not (member.isfile() or member.isdir()):
                raise ValueError('unsafe runtime bundle member')
            if path == PurePosixPath(HELPER):
                assert member.isfile() and member.mode & 0o777 == 0o644
    destination.mkdir(mode=0o700)
    subprocess.run(['tar', '-xzf', str(bundle), '-C', str(destination),
                    '--no-same-owner', '--same-permissions'], check=True)
    assert not any((destination/name).exists() for name in ('.git', '.build', 'go.mod'))


def observe(runtime, shims, rejected):
    environment = {'PATH': str(shims)+':/usr/bin:/bin', 'SUBYARD_ENGINE_CONTEXT': '1',
                   'SUBYARD_ENGINE_CONTEXT_SCHEMA': '1', 'ACCESS_KIND': 'local',
                   'YARD_KIND': 'container', 'YARD_INSTANCE_NAME': 'yard',
                   'INCUS_PROJECT': 'subyard', 'INCUS_BRIDGE': 'fixture',
                   'SSH_HOST': '127.0.0.1', 'DEV_USER': 'dev', 'DEV_UID': '1000',
                   'DEV_SUDO': '0', 'FORWARD_SSH_AGENT': '0', 'NESTED_E2E_VMS': '0',
                   'ENVIRONMENT_PROFILES': '', 'CODING_TOOL_INTEGRATIONS': 'codex'}
    for key in ('SUBYARD_OPERATOR_HOME', 'SUBYARD_CONFIG_HOME', 'SUBYARD_HOME',
                'STORAGE_PATH', 'HOST_BASE', 'RESTRICTED_DISK_PATHS'):
        environment[key] = str(runtime.parent)
    environment['SUBYARD_CONFIG_DIR'] = str(runtime/'config')
    result = subprocess.run(['bash', str(runtime/'config/profiles/openlit/runtime.sh'), 'observe'],
                            env=environment, capture_output=True, timeout=30)
    # Early native refusal may have no JSON; any returned observation stays exact.
    if result.returncode == 0 or result.stdout:
        assert json.loads(result.stdout) == {'state': 'absent', 'actual': '', 'desired': ''}
    assert not rejected.exists()
    return result.returncode


def main():
    if len(sys.argv) not in (2, 3):
        raise ValueError('expected fresh runtime bundle and optional missing-helper baseline bundle')
    fresh = Path(sys.argv[1]).resolve()
    baseline = Path(sys.argv[2]).resolve() if len(sys.argv) == 3 else None
    with tempfile.TemporaryDirectory(prefix='subyard-openlit-package-', dir='/tmp') as directory:
        state = Path(directory)
        shims = state/'commands'; shims.mkdir(mode=0o700)
        control, rejected = state/'control.json', state/'rejected'
        control.write_text(json.dumps({'instances': [{'info': {'status': 'Running'}}]}))
        control.chmod(0o600)
        harness = runpy.run_path(str(ROOT/'dev/verify-release-upgrades.py'))
        harness['write_observation_incus'](shims, control, rejected)
        runtime = state/'runtime'
        extract(fresh, runtime)
        helper = runtime/HELPER
        assert helper.is_file() and not helper.is_symlink()
        assert stat.S_IMODE(helper.stat().st_mode) == 0o644
        manifest = dict(line.split('  ', 1)[::-1] for line in (runtime/'runtime-files.sha256').read_text().splitlines())
        assert hashlib.sha256(helper.read_bytes()).hexdigest() == manifest['./'+HELPER]
        assert observe(runtime, shims, rejected) == 0
        missing = state/'missing-helper'
        shutil.copytree(runtime, missing)
        (missing/HELPER).unlink()
        assert observe(missing, shims, rejected) != 0
        if baseline:
            previous = state/'baseline'
            extract(baseline, previous)
            assert not (previous/HELPER).exists()
            assert observe(previous, shims, rejected) != 0
    print('ok: packaged OpenLIT observes absence; missing shared helper fails despite canned absence')


if __name__ == '__main__':
    main()
