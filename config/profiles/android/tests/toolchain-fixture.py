#!/usr/bin/env python3
"""Shared disposable toolchain component construction, without running tests."""
import importlib.util
import json
from pathlib import Path

PROFILE = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('toolchain', PROFILE / 'toolchain.py')
toolchain = importlib.util.module_from_spec(spec)
spec.loader.exec_module(toolchain)


def fixture(directory, name, entry):
    directory.mkdir(parents=True, exist_ok=True)
    executable = directory / entry['executable']
    executable.parent.mkdir(parents=True, exist_ok=True)
    executable.write_text('#!/bin/sh\nprintf "%s\\n" "' + entry['version'] + '"\n')
    executable.chmod(0o755)
    if name == 'jdk':
        (directory / 'release').write_text('JAVA_RUNTIME_VERSION="' + entry['version'] + '"\nOS_ARCH="x86_64"\n')
    else:
        metadata = 'Pkg.Revision=' + entry['version'] + '\n'
        if name.startswith('platforms;android-'):
            metadata += 'AndroidVersion.ApiLevel=' + name.removeprefix('platforms;android-') + '\n'
        (directory / 'source.properties').write_text(metadata)
    (directory / 'supporting.jar').write_bytes(b'verified supporting payload')
    (directory / '.subyard-toolchain.json').write_text(json.dumps(dict(archive=entry['digest'],
        algorithm=entry['digest_algorithm'], version=entry['version'],
        payload_sha256=toolchain.payload_digest(directory))))

