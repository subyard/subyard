#!/usr/bin/env python3
"""Run the actual skill wrapper against the nonexecuting release transport fake."""

import hashlib
import io
import json
import os
from pathlib import Path
import runpy
import shutil
import subprocess
import sys
import tarfile
import tempfile


root = Path(sys.argv[1])
verifier = runpy.run_path(str(root / 'dev/verify-release-upgrades.py'))
with tempfile.TemporaryDirectory(prefix='subyard-skills-transport-', dir='/tmp') as temporary:
    base = Path(temporary)
    source = base / 'source'
    # This independent tiny inventory makes omissions visible even if producer
    # and release fake share the same inventory helper.
    payloads = {
        'config/skills/core/sample/SKILL.md': b'core skill\n',
        'config/profiles/synthetic/skills/local/SKILL.md': b'profile skill\n',
        'config/profiles/synthetic/payload.txt': b'declared content\n',
        'config/profiles/synthetic/skills.json': json.dumps({'declared': {'SKILL.md': 'payload.txt', 'copy.txt': 'payload.txt'}}).encode(),
    }
    for name in ('package.py', 'refresh.sh', 'deliver.py', 'probe.py', 'migrate-preview.py'):
        payloads['config/skills/'+name] = (root / 'config/skills' / name).read_bytes()
    for name, payload in payloads.items():
        path = source / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(payload)
    for name in ('refresh-agent-skills.sh', 'lib/engine-context.sh'):
        path = source / 'scripts' / name
        path.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(root / 'scripts' / name, path)
    for name in ('config/skills/tests/ignored', 'config/skills/__pycache__/ignored', 'config/skills/cache.pyc', 'config/profiles/synthetic/skills/tests/ignored'):
        path = source / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text('ignored')
    package = runpy.run_path(str(source / 'config/skills/package.py'))
    assert package['source_files'](source) == sorted(['scripts/refresh-agent-skills.sh', *payloads])
    expected = {name: ('file', 0o600, hashlib.sha256(payload).hexdigest()) for name, payload in payloads.items()}
    for name in payloads:
        for parent in Path(name).parents:
            if parent != Path('.'):
                expected[parent.as_posix()] = ('directory', 0o700, None)
    stream = io.BytesIO()
    package['write_archive'](source, stream)
    original = stream.getvalue()
    actual = {}
    with tarfile.open(fileobj=io.BytesIO(original)) as archive:
        for member in archive:
            assert member.name not in actual
            actual[member.name] = ('directory', member.mode, None) if member.isdir() else ('file', member.mode, hashlib.sha256(archive.extractfile(member).read()).hexdigest())
    assert actual == expected, (actual.keys(), expected.keys())
    shims = base / 'commands'
    shims.mkdir()
    unexpected = base / 'unexpected'
    verifier['write_observation_incus'](shims, base/'control', unexpected, skills_roots=[source])
    environment = dict(os.environ, PATH=str(shims)+':/usr/bin:/bin', ENVIRONMENT_PROFILES='', ALLOWS_CODING_TOOLS='true', PYTHONDONTWRITEBYTECODE='1')
    command = ['bash', '-c', '. "$1/tests/helpers/test-context.sh"; setup_test_context "$2"; bash "$3/scripts/refresh-agent-skills.sh" "$4"', 'skills-test', str(root), str(base/'context'), str(source)]
    for directory_mode, script_mode, file_mode in ((0o755, 0o755, 0o644), (0o700, 0o700, 0o600), (0o500, 0o500, 0o500)):
        for path in source.rglob('*'):
            path.chmod(directory_mode if path.is_dir() else script_mode if path.suffix == '.sh' else file_mode)
        source.chmod(directory_mode)
        for mask in (0o022, 0o002, 0o077):
            for operation in ('apply', 'check'):
                result = subprocess.run(command+[operation], env=environment, capture_output=True, umask=mask, timeout=20)
                assert result.returncode == 0, (directory_mode, mask, operation, result.stderr)
                assert not unexpected.exists()
    # Restore write permission only for fixture mutation/cleanup after frozen runs.
    source.chmod(0o700)
    for path in source.rglob('*'):
        path.chmod(0o700 if path.is_dir() else 0o600)
    argv = list(verifier['skills_transport_contract'](source)[0][0])
    def reject(arguments, archive):
        result = subprocess.run([str(shims/'incus'), *arguments], input=archive, capture_output=True, timeout=10)
        assert result.returncode == 99, (arguments[:4], result.returncode, result.stderr)
        assert unexpected.exists()
        unexpected.unlink()
    def mutant(kind):
        result = io.BytesIO()
        with tarfile.open(fileobj=io.BytesIO(original)) as archive, tarfile.open(fileobj=result, mode='w') as output:
            for member in archive:
                payload = archive.extractfile(member).read() if member.isfile() else None
                if member.name == 'config/skills/core/sample/SKILL.md':
                    if kind == 'bytes': payload = b'foreign\n'; member.size = len(payload)
                    if kind == 'mode': member.mode = 0o644
                    if kind == 'type': member.type = tarfile.SYMTYPE; member.linkname = '/foreign'; payload = None
                    output.addfile(member, io.BytesIO(payload) if payload is not None else None)
                    if kind == 'duplicate': output.addfile(member, io.BytesIO(payload))
                else:
                    output.addfile(member, io.BytesIO(payload) if payload is not None else None)
            if kind == 'extra': output.addfile(tarfile.TarInfo('foreign'))
        return result.getvalue()
    for kind in ('bytes', 'mode', 'type', 'duplicate', 'extra'):
        reject(argv, mutant(kind))
    changed = argv.copy(); changed[-3] += '\n# foreign'
    reject(changed, original)
    journal, failure, marker = base/'journal', base/'failure', base/'failed'
    journal.write_text(json.dumps({'checkpoint': 'reconciling'}))
    failure.write_text(json.dumps({'journal': str(journal), 'marker': str(marker)}))
    verifier['write_observation_incus'](shims, base/'control', unexpected,
                                       activation_failure=failure, skills_roots=[source])
    result = subprocess.run(command+['apply'], env=environment, capture_output=True, timeout=20)
    assert result.returncode == 65 and marker.exists() and not unexpected.exists()
    failure.unlink()
    manifest = source/'config/profiles/synthetic/skills.json'
    saved = manifest.read_bytes()
    for relative in ('../outside', '/outside', 'missing'):
        manifest.write_text(json.dumps({'unsafe': {'SKILL.md': relative}}))
        result = subprocess.run(command+['apply'], env=environment, capture_output=True, timeout=20)
        assert result.returncode != 0, relative
        if unexpected.exists(): unexpected.unlink()
    manifest.write_bytes(saved)
    payload = source/'config/profiles/synthetic/payload.txt'
    payload.unlink(); payload.symlink_to('/etc/passwd')
    result = subprocess.run(command+['apply'], env=environment, capture_output=True, timeout=20)
    assert result.returncode != 0
print('ok: actual skills wrapper, independent inventory, canonical modes and strict transport refusal')
