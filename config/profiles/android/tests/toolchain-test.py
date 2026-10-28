#!/usr/bin/env python3
"""Executable regression for configuration convergence and archive validation."""
import copy
import importlib.util
import json
from pathlib import Path
import tempfile
import zipfile
from unittest.mock import patch

PROFILE = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('toolchain', PROFILE / 'toolchain.py')
toolchain = importlib.util.module_from_spec(spec)
spec.loader.exec_module(toolchain)


spec = importlib.util.spec_from_file_location('toolchain_fixture', PROFILE / 'tests/toolchain-fixture.py')
fixture_module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture_module)
fixture = fixture_module.fixture


def rejected(call):
    try:
        call()
    except (ValueError, KeyError, TypeError, OSError, zipfile.BadZipFile):
        return
    raise AssertionError('invalid input accepted')


def selected_checks():
    entries = toolchain.selected(PROFILE / 'toolchain.json', environment={}, architecture='x86_64')
    assert 'platforms;android-35' in toolchain.selected(PROFILE / 'toolchain.json', environment={'ANDROID_API':'35'}, architecture='x86_64')
    rejected(lambda: toolchain.selected(PROFILE / 'toolchain.json', environment={'ANDROID_API':'99'}, architecture='x86_64'))
    rejected(lambda: toolchain.selected(PROFILE / 'toolchain.json', environment={'JDK_VERSION':'21'}, architecture='x86_64'))
    with tempfile.TemporaryDirectory(prefix='subyard-android-toolchain-', dir='/tmp') as temporary:
        root = Path(temporary)
        # One SDK exercises shared receipt/payload checks; provision covers all components.
        name = 'platforms;android-36'
        entry = entries[name]
        directory = root / 'platform'
        fixture(directory, name, entry)
        (directory / 'package.xml').write_text('<generated-package-metadata/>')
        assert toolchain.verify(directory, name, entry)
        (directory / 'source.properties').write_text('Pkg.Revision=' + entry['version'] + '\nAndroidVersion.ApiLevel=35\n')
        assert not toolchain.verify(directory, name, entry, receipt=False)
        entry = entries['platform-tools']
        # Cached Java must not execute before either executable or library drift is rejected.
        jdk = entries['jdk']
        directory = root / 'cached-jdk'
        marker = root / 'java-executed'
        fixture(directory, 'jdk', jdk)
        assert toolchain.verify(directory, 'jdk', jdk)
        (directory / 'bin/java').write_text('#!/bin/sh\ntouch "' + str(marker)
            + '"\nprintf "%s\\n" "' + jdk['version'] + '"\n')
        assert not toolchain.verify(directory, 'jdk', jdk)
        assert not marker.exists()
        fixture(directory, 'jdk', jdk)
        with patch.object(toolchain.subprocess, 'run', side_effect=AssertionError('untrusted Java executed')):
            (directory / 'supporting.jar').write_bytes(b'corrupt library')
            assert not toolchain.verify(directory, 'jdk', jdk)
        destination = root / 'installed'
        fixture(destination, 'platform-tools', entry)
        original = (destination / 'adb').read_bytes()
        source = root / 'replacement'
        replacement = dict(entry, version='98.0.0')
        fixture(source, 'platform-tools', replacement)
        archive = root / 'archive.zip'
        with zipfile.ZipFile(archive, 'w') as package:
            for path in source.iterdir():
                package.write(path, 'platform-tools/' + path.name)
        replacement.update(digest_algorithm='sha256', digest=toolchain.digest(archive))
        rejected(lambda: toolchain.install(archive, destination, 'platform-tools', entry))
        assert (destination / 'adb').read_bytes() == original
        updated = json.loads((PROFILE / 'toolchain.json').read_text())
        updated['sdk']['platform-tools'].update({key: replacement[key] for key in ('version', 'digest', 'digest_algorithm')})
        updated_path = root / 'updated.json'
        updated_path.write_text(json.dumps(updated))
        configured = toolchain.selected(updated_path, environment={}, architecture="x86_64")['platform-tools']
        toolchain.install(archive, destination, 'platform-tools', configured)
        assert toolchain.verify(destination, 'platform-tools', replacement)
        assert not toolchain.verify(destination, 'platform-tools', entry)
        # A matching digest cannot authorize a different installed revision.
        rejected(lambda: toolchain.install(archive, destination, 'platform-tools', dict(replacement, version='97.0.0')))
        assert toolchain.verify(destination, 'platform-tools', replacement)
        with zipfile.ZipFile(archive, 'w') as package:
            package.writestr('../escape', 'unsafe')
        unsafe = dict(replacement, digest=toolchain.digest(archive))
        rejected(lambda: toolchain.install(archive, destination, 'platform-tools', unsafe))
        assert not (root / 'escape').exists()
        archive.write_bytes(b'corrupt archive')
        rejected(lambda: toolchain.install(archive, destination, 'platform-tools',
                                           dict(replacement, digest=toolchain.digest(archive))))
        assert toolchain.verify(destination, 'platform-tools', replacement)
        manifest = json.loads((PROFILE / 'toolchain.json').read_text())
        for major, version in [('17', '17.0.20.1+1'), ('21', '21+35')]:
            changed_jdk = copy.deepcopy(manifest)
            changed_jdk['jdk'].update(major=major, version=version)
            next_path = root / 'jdk-next.json'
            next_path.write_text(json.dumps(changed_jdk))
            next_jdk = toolchain.selected(next_path, environment={'JDK_VERSION':major}, architecture='x86_64')['jdk']
            assert next_jdk['destination'] == '/opt/jdk-' + major
        path = root / 'invalid.json'
        for update in ({'digest': 'broken'}, {'url': 'https://invalid.example/archive.zip'}, {'version': '21.0.1+1'}):
            invalid = copy.deepcopy(manifest)
            invalid['jdk'].update(update)
            path.write_text(json.dumps(invalid))
            rejected(lambda: toolchain.selected(path, environment={}, architecture="x86_64"))
    print('ok: Android selected revisions, byte identity and verified staged replacement')


def main():
    # Production remains x86_64-only; synthetic coverage also runs on ARM CI.
    rejected(lambda: toolchain.selected(PROFILE / 'toolchain.json', environment={}, architecture='aarch64'))
    selected_checks()


if __name__ == '__main__':
    main()
