#!/usr/bin/env python3
"""Executable regression for configuration convergence and archive validation."""
import copy
import io
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
import time
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


def write_manifest(path, manifest):
    """A disposable manifest with the profile's vendored metadata beside it; a pin the profile
    does not vendor gets a synthetic entry for its archive digest."""
    path.write_text(json.dumps(manifest))
    text = (PROFILE / 'toolchain-metadata.xml').read_text()
    for name, entry in manifest['sdk'].items():
        if not toolchain.repository_blocks(text, name, entry['digest']):
            text = text.replace('</sdk:sdk-repository>', f'<remotePackage path="{name}"><revision><major>1</major>'
                                f'</revision><uses-license ref="android-sdk-license"/><archives><archive><complete>'
                                f'<checksum type="sha1">{entry["digest"]}</checksum></complete></archive></archives>'
                                '</remotePackage></sdk:sdk-repository>')
    path.with_name('toolchain-metadata.xml').write_text(text)


def rejected(call):
    try:
        call()
    except (ValueError, KeyError, TypeError, OSError, zipfile.BadZipFile):
        return
    raise AssertionError('invalid input accepted')


def selected_checks():
    entries = toolchain.selected(PROFILE / 'toolchain.json', environment={}, architecture='x86_64')
    assert 'platforms;android-35' in toolchain.selected(PROFILE / 'toolchain.json', environment={'ANDROID_API':'35'}, architecture='x86_64')
    rejected(lambda: toolchain.selected(PROFILE / 'toolchain.json', environment={'ANDROID_API':'abc'}, architecture='x86_64'))
    rejected(lambda: toolchain.selected(PROFILE / 'toolchain.json', environment={'JDK_VERSION':'21'}, architecture='x86_64'))
    with tempfile.TemporaryDirectory(prefix='subyard-android-toolchain-', dir='/tmp') as temporary:
        root = Path(temporary)
        # One SDK exercises shared receipt/payload checks; provision covers all components.
        name = 'platforms;android-35'
        entry = entries[name]
        directory = root / 'platform'
        fixture(directory, name, entry)
        (directory / 'package.xml').write_text('<generated-package-metadata/>')
        assert toolchain.verify(directory, name, entry)
        (directory / 'source.properties').write_text('Pkg.Revision=' + entry['version'] + '\nAndroidVersion.ApiLevel=34\n')
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
        write_manifest(updated_path, updated)
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
            write_manifest(next_path, changed_jdk)
            next_jdk = toolchain.selected(next_path, environment={'JDK_VERSION':major}, architecture='x86_64')['jdk']
            assert next_jdk['destination'] == '/opt/jdk-' + major
        path = root / 'invalid.json'
        for update in ({'digest': 'broken'}, {'url': 'https://invalid.example/archive.zip'}, {'version': '21.0.1+1'}):
            invalid = copy.deepcopy(manifest)
            invalid['jdk'].update(update)
            write_manifest(path, invalid)
            rejected(lambda: toolchain.selected(path, environment={}, architecture="x86_64"))
    print('ok: Android selected revisions, byte identity and verified staged replacement')


def default_package_checks():
    entries = toolchain.selected(PROFILE / 'toolchain.json', environment={}, architecture='x86_64')
    packages = [name for name in entries if name != 'jdk']
    manifest = json.loads((PROFILE / 'toolchain.json').read_text())
    # Every default is installed independently of the emulator API and compatibility build tools.
    assert packages == list(manifest['sdk'])
    assert packages == ['cmdline-tools;latest', 'platform-tools', 'emulator', 'platforms;android-35',
                        'platforms;android-37.2', 'build-tools;36.0.0', 'build-tools;37.0.0']
    assert entries['platforms;android-37.2']['destination'] == '/srv/cache/android-sdk/platforms/android-37.2'
    # The emulator default API needs a system image, not an installed build platform.
    assert toolchain.selected(PROFILE / 'toolchain.json', environment={'ANDROID_API': '36', 'BUILD_TOOLS_VERSION': '37.0.0'},
                              architecture='x86_64').keys() == entries.keys()
    for environment in ({'ANDROID_API': '36.1'}, {'BUILD_TOOLS_VERSION': '35.0.0'}):
        rejected(lambda: toolchain.selected(PROFILE / 'toolchain.json', environment=environment, architecture='x86_64'))
    with tempfile.TemporaryDirectory(prefix='subyard-android-defaults-', dir='/tmp') as temporary:
        root = Path(temporary)
        # A minor API level is verified against the platform's real ApiLevel metadata.
        name = 'platforms;android-37.2'
        directory = root / 'minor-platform'
        fixture(directory, name, entries[name])
        assert (directory / 'source.properties').read_text().count('AndroidVersion.ApiLevel=37.2\n') == 1
        assert toolchain.verify(directory, name, entries[name])
        (directory / 'source.properties').write_text('Pkg.Revision=2\nAndroidVersion.ApiLevel=37\n')
        assert not toolchain.verify(directory, name, entries[name], receipt=False)
        # Every independent manifest refusal is reported once, in stable manifest order.
        invalid = copy.deepcopy(manifest)
        invalid['sdk']['platforms;android-35']['version'] = 'r3'
        invalid['sdk']['build-tools;36.0.0']['license'] = 'android-sdk-preview-license'
        invalid['sdk']['ndk;27.0.1'] = dict(invalid['sdk']['build-tools;37.0.0'])
        invalid['repository'] = 'http://dl.google.com/android/repository/repository2-3.xml'
        del invalid['sdk']['emulator']
        path = root / 'invalid.json'
        write_manifest(path, invalid)
        try:
            toolchain.selected(path, environment={'ANDROID_API': '36x'}, architecture='x86_64')
        except toolchain.ConfigurationError as exc:
            message = str(exc)
        else:
            raise AssertionError('invalid default package list accepted')
        expected = ['ANDROID_API must be an integer emulator API level',
                    'repository must be the Google SDK repository metadata URL',
                    'default SDK package emulator is required',
                    'platforms;android-35: invalid SDK revision',
                    'build-tools;36.0.0: unsupported SDK license',
                    'ndk;27.0.1: unsupported default SDK package identifier']
        assert message.split('; ') == expected, message
        # Licenses are recorded by the installed SDK manager in private staging, then merged.
        sdk = root / 'sdk'
        sdk.mkdir()
        assert not toolchain.licenses_ready(sdk, entries)
        calls = []
        def sdk_manager(command, **kwargs):
            calls.append((command, kwargs))
            staged = Path(command[1].removeprefix('--sdk_root='))
            assert kwargs['stdin'] == toolchain.subprocess.DEVNULL and kwargs['check'] is True
            assert Path(kwargs['env']['HOME']).parent == staged.parent
            (staged / 'licenses').mkdir(parents=True)
            (staged / 'licenses/android-sdk-license').write_text('\n' + 'a' * 40)
        (sdk / 'licenses').mkdir()
        (sdk / 'licenses/android-sdk-license').write_text('not-a-hash\n' + 'b' * 40 + '\n')
        assert toolchain.licenses_ready(sdk, entries)
        (sdk / 'licenses/android-sdk-license').write_text('not-a-hash\n')
        with patch.object(toolchain.sys, 'stderr', new=io.StringIO()) as log:
            assert toolchain.accept_licenses(sdk, entries, '/opt/jdk-17', run=sdk_manager)
        assert [command[2] for command, _ in calls] == ['platform-tools']
        assert calls[0][0][0] == str(sdk / 'cmdline-tools/latest/bin/sdkmanager')
        assert log.getvalue() == 'Android: accepted Google Android SDK licenses for ' + ', '.join(packages) + '\n'
        assert toolchain.license_hashes(sdk / 'licenses/android-sdk-license') == ['a' * 40]
        assert not list(sdk.glob('.licenses-*'))
        (sdk / 'licenses/android-sdk-license').write_text('b' * 40 + '\n')
        stale = sdk / ('.subyard-licenses-' + 'e' * 24)
        stale.mkdir()
        (stale / '.subyard-owner.json').write_text(json.dumps(toolchain.LICENSE_STAGING))
        with patch.object(toolchain.sys, 'stderr', new=io.StringIO()) as log:
            assert not toolchain.accept_licenses(sdk, entries, '/opt/jdk-17', run=AssertionError)
        # Interrupted staging is swept even when the licenses are already recorded.
        assert not stale.exists()
        assert log.getvalue() == 'android provision: licenses state=reused\n'
        (sdk / 'licenses/android-sdk-license').unlink()
        def silent(command, **kwargs):
            Path(command[1].removeprefix('--sdk_root=')).mkdir(exist_ok=True)
        rejected(lambda: toolchain.accept_licenses(sdk, entries, '/opt/jdk-17', run=silent))
        assert not (sdk / 'licenses/android-sdk-license').exists()
        # The SDK manager's session is stopped, including helpers it leaves running.
        marker = root / 'late-helper'
        helper = ('import pathlib, time; time.sleep(1); pathlib.Path(' + repr(str(marker)) + ').touch()')
        quiet = dict(stdin=toolchain.subprocess.DEVNULL, stdout=toolchain.subprocess.DEVNULL,
                     stderr=toolchain.subprocess.DEVNULL, check=True)
        toolchain.run_session([sys.executable, '-c', 'import subprocess, sys; subprocess.Popen([sys.executable, "-c", '
                               + repr(helper) + '])'], timeout=10, **quiet)
        time.sleep(1.5)
        assert not marker.exists()
        for command, error in (([sys.executable, '-c', 'raise SystemExit(7)'], toolchain.subprocess.CalledProcessError),
                               ([sys.executable, '-c', 'import time; time.sleep(30)'], toolchain.subprocess.TimeoutExpired)):
            try:
                toolchain.run_session(command, timeout=1, **quiet)
            except error:
                pass
            else:
                raise AssertionError('failed SDK manager run accepted')
        # An interrupted license staging directory is marker-owned and removed by the next run.
        interrupted = sdk / ('.subyard-licenses-' + 'c' * 24)
        interrupted.mkdir()
        (interrupted / '.subyard-owner.json').write_text(json.dumps(toolchain.LICENSE_STAGING))
        (interrupted / 'home').mkdir()
        foreign = sdk / ('.subyard-licenses-' + 'd' * 24)
        foreign.mkdir()
        (sdk / 'licenses/android-sdk-license').unlink(missing_ok=True)
        with patch.object(toolchain.sys, 'stderr', new=io.StringIO()):
            assert toolchain.accept_licenses(sdk, entries, '/opt/jdk-17', run=sdk_manager)
        assert not interrupted.exists() and foreign.is_dir()
        assert sorted(path.name for path in sdk.glob('.subyard-licenses-*')) == [foreign.name]
    print('ok: Android default SDK packages, minor API levels, all refusals and license acceptance')


RETIRED = (('platforms;android-34', 'f09b07da8aea238a81b71d6fedb815e13aa13e58', '2.0.0'),
           ('platforms;android-34', '1f2e9478d6a7601425ceaa553311dc43191f103d', '3.0.0'),
           ('platforms;android-36', '2c1a80dd4d9f7d0e6dd336ec603d9b5c55a6f576', '2.0.0'),
           ('build-tools;35.0.0', '2cfaa0bbb2336e9ec18ed3ecea84fa2e2af607bc', '35.0.0'))


def metadata_checks():
    entries = toolchain.selected(PROFILE / 'toolchain.json', environment={}, architecture='x86_64')
    vendored = (PROFILE / 'toolchain-metadata.xml').read_bytes()
    text = vendored.decode()
    manifest = json.loads((PROFILE / 'toolchain.json').read_text())
    # Every pin and every retired earlier default has exactly one unmodified vendored entry, and the
    # generated package.xml declares exactly the pinned path and revision.
    pins = [(name, entry['digest'], entry['version']) for name, entry in manifest['sdk'].items()]
    with tempfile.TemporaryDirectory(prefix='subyard-android-vendored-', dir='/tmp') as temporary:
        probe = Path(temporary)
        for name, digest, version in pins + list(RETIRED):
            assert len(toolchain.repository_blocks(text, name, digest)) == 1, name
            (probe / 'package.xml').write_bytes(toolchain.package_xml(vendored, name, digest))
            assert toolchain.declared(probe) == (name, version), name
            output = (probe / 'package.xml').read_bytes()
            assert b'<channelRef' not in output and b'<archives>' not in output
            if name.startswith('platforms;'):
                assert b'xsi:type="sdk:platformDetailsType"' in output and b'xmlns:sdk=' in output
    rejected(lambda: toolchain.package_xml(vendored, 'platforms;android-35', 'f' * 40))
    with tempfile.TemporaryDirectory(prefix='subyard-android-metadata-', dir='/tmp') as temporary:
        root = Path(temporary)
        # A pin without a vendored entry is a configuration refusal before any side effect.
        unvendored = copy.deepcopy(manifest)
        unvendored['sdk']['build-tools;37.0.0']['digest'] = 'a' * 40
        path = root / 'unvendored.json'
        path.write_text(json.dumps(unvendored))
        (root / 'toolchain-metadata.xml').write_bytes(vendored)
        try:
            toolchain.selected(path, environment={}, architecture='x86_64')
        except toolchain.ConfigurationError as exc:
            assert str(exc) == ('build-tools;37.0.0: no unique vendored repository entry for the pinned archive; '
                                'run toolchain.py vendor-metadata'), str(exc)
        else:
            raise AssertionError('unvendored pin accepted')
        (root / 'toolchain-metadata.xml').unlink()
        rejected(lambda: toolchain.selected(path, environment={}, architecture='x86_64'))
        # The developer helper refreshes pins from the live repository and keeps retired entries.
        live = vendored.replace(b'</sdk:sdk-repository>', b'<remotePackage path="extras;unrelated"><archives/>'
                                b'</remotePackage></sdk:sdk-repository>')
        (root / 'toolchain.json').write_text(json.dumps(manifest))
        (root / 'toolchain-metadata.xml').write_text(text.split('\n', 2)[2].split('<remotePackage path="cmdline')[0]
                                                     + toolchain.repository_blocks(text, *RETIRED[2][:2])[0]
                                                     + '\n</sdk:sdk-repository>\n')
        toolchain.vendor_metadata(root / 'toolchain.json', fetch=lambda url: live)
        refreshed = (root / 'toolchain-metadata.xml').read_text()
        for name, digest, _ in pins + [RETIRED[2]]:
            assert len(toolchain.repository_blocks(refreshed, name, digest)) == 1, name
        assert 'extras;unrelated' not in refreshed and not toolchain.repository_blocks(refreshed, *RETIRED[0][:2])
        rejected(lambda: toolchain.vendor_metadata(root / 'unvendored.json', root / 'other.xml', fetch=lambda url: live))
        target = root / 'toolchain-metadata.xml'
        target.write_bytes(vendored)
        # A pin rotated out upstream keeps its vendored block; only pins with no block anywhere are refused.
        rotated = toolchain.repository_blocks(text, 'build-tools;37.0.0', manifest['sdk']['build-tools;37.0.0']['digest'])[0]
        upstream = vendored.replace(rotated.encode(), b'')
        toolchain.vendor_metadata(root / 'toolchain.json', fetch=lambda url: upstream)
        assert target.read_bytes().count(rotated.encode()) == 1
        unknown = copy.deepcopy(manifest)
        for name in ('platforms;android-35', 'build-tools;36.0.0'):
            unknown['sdk'][name]['digest'] = name[-1] * 40
        (root / 'unknown.json').write_text(json.dumps(unknown))
        before = target.read_bytes()
        try:
            toolchain.vendor_metadata(root / 'unknown.json', target, fetch=lambda url: upstream)
        except toolchain.ConfigurationError as exc:
            assert str(exc).endswith('pinned platforms;android-35, build-tools;36.0.0'), str(exc)
        else:
            raise AssertionError('pin without any entry accepted')
        assert target.read_bytes() == before
        # Google edits a still-pinned block: the upstream text replaces the vendored copy, no duplicate.
        pin35 = toolchain.repository_blocks(text, 'platforms;android-35', manifest['sdk']['platforms;android-35']['digest'])[0]
        edited = pin35.replace('<display-name>', '<display-name>Edited ')
        assert edited != pin35
        toolchain.vendor_metadata(root / 'toolchain.json', fetch=lambda url: vendored.replace(pin35.encode(), edited.encode()))
        refreshed = target.read_text()
        assert refreshed.count(edited) == 1 and pin35 not in refreshed
        toolchain.selected(root / 'toolchain.json', environment={}, architecture='x86_64')
        # Two differing retired copies of one archive would make metadata ambiguous: nothing is written.
        retired = toolchain.repository_blocks(text, *RETIRED[2][:2])[0]
        target.write_text(refreshed.replace(retired, retired + '\n  ' + retired.replace('<display-name>', '<display-name>Copy ')))
        before = target.read_bytes()
        try:
            toolchain.vendor_metadata(root / 'toolchain.json', fetch=lambda url: vendored)
        except toolchain.ConfigurationError as exc:
            assert str(exc) == 'vendored metadata would not be unique for platforms;android-36@' + RETIRED[2][1], str(exc)
        else:
            raise AssertionError('duplicate retired entry accepted')
        assert target.read_bytes() == before
        sdk = root / 'sdk'
        local = {}
        for package, value in entries.items():
            if package == 'jdk':
                continue
            value = dict(value, destination=str(sdk.joinpath(*package.split(';'))))
            local[package] = value
            fixture(Path(value['destination']), package, value)
            (Path(value['destination']) / 'package.xml').unlink()
        # Earlier defaults: unchanged payload with a vendored entry, changed payload, foreign directory.
        earlier = sdk / 'platforms/android-36'
        fixture(earlier, 'platforms;android-36', dict(entries['platforms;android-35'], version='2.0.0', digest=RETIRED[2][1]))
        (earlier / 'package.xml').unlink()
        changed = sdk / 'platforms/android-34'
        fixture(changed, 'platforms;android-34', dict(entries['platforms;android-35'], version='3.0.0', digest=RETIRED[1][1]))
        (changed / 'package.xml').unlink()
        (changed / 'supporting.jar').write_bytes(b'changed after installation')
        foreign = sdk / 'platforms/android-33'
        foreign.mkdir()
        (foreign / 'source.properties').write_text('Pkg.Revision=3\n')
        payloads = {package: toolchain.payload_digest(Path(value['destination'])) for package, value in local.items()}
        # Provisioning is offline for metadata.
        with patch.object(toolchain.urllib.request, 'urlopen', side_effect=AssertionError('network metadata fetch')), \
                patch.object(toolchain.sys, 'stderr', new=io.StringIO()) as log:
            written, skipped = toolchain.converge_metadata(sdk, local, PROFILE / 'toolchain-metadata.xml')
        assert written == list(local) + ['platforms;android-36'] and skipped == ['platforms;android-34'], (written, skipped)
        assert 'component=platforms;android-34 state=unavailable' in log.getvalue()
        for package, value in local.items():
            assert toolchain.metadata_ready(value['destination'], package, value['version'])
            # package.xml is outside the payload identity: pinned receipts remain valid.
            assert toolchain.payload_digest(Path(value['destination'])) == payloads[package]
            assert toolchain.verify(value['destination'], package, value)
        assert toolchain.declared(earlier) == ('platforms;android-36', '2.0.0')
        assert not (changed / 'package.xml').exists() and not (foreign / 'package.xml').exists()
        assert not list(sdk.glob('platforms/.*.new'))
        with patch.object(toolchain.sys, 'stderr', new=io.StringIO()):
            assert toolchain.converge_metadata(sdk, local, root / 'missing.xml') == ([], ['platforms;android-34'])
    print('ok: Android vendored package metadata, offline convergence and retired earlier defaults')


def main():
    # Production remains x86_64-only; synthetic coverage also runs on ARM CI.
    rejected(lambda: toolchain.selected(PROFILE / 'toolchain.json', environment={}, architecture='aarch64'))
    selected_checks()
    default_package_checks()
    metadata_checks()


if __name__ == '__main__':
    main()
