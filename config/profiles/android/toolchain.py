#!/usr/bin/env python3
"""Profile-owned pinned toolchain selection and verified staged installation."""
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import subprocess
import sys
import tarfile
import tempfile
import urllib.parse
import zipfile


def selected(path):
    manifest = json.loads(Path(path).read_text())
    if (type(manifest['schema']) is not int or manifest['schema'] != 1 or manifest['architecture'] != platform.machine()
            or manifest['architecture'] != 'x86_64' or manifest['image_policy'] != 'latest-stable'):
        raise ValueError('unsupported toolchain configuration')
    jdk = manifest['jdk']
    version = re.fullmatch(r'([1-9][0-9]*)(?:\.\d+)*\+\d+', jdk['version'])
    if (version is None or version.group(1) != jdk['major']
            or os.environ.get('JDK_VERSION', jdk['major']) != jdk['major']):
        raise ValueError('JDK version does not match selected major')
    api = os.environ.get('ANDROID_API', manifest['android_api'])
    build = os.environ.get('BUILD_TOOLS_VERSION', manifest['build_tools'])
    if not re.fullmatch(r'\d+', api) or not re.fullmatch(r'\d+\.\d+\.\d+', build):
        raise ValueError('invalid Android version selection')
    sdk = Path(os.environ.get('ANDROID_SDK_ROOT', '/srv/cache/android-sdk'))
    entries = {'jdk': dict(jdk, destination='/opt/jdk-' + jdk['major'], executable='bin/java')}
    for package, executable in [('cmdline-tools;latest', 'bin/sdkmanager'), ('platform-tools', 'adb'),
                                 ('platforms;android-' + api, 'android.jar'),
                                 ('build-tools;' + build, 'aapt2'), ('emulator', 'emulator')]:
        entry = manifest['sdk'][package]
        if not re.fullmatch(r'\d+\.\d+\.\d+', entry['version']):
            raise ValueError('invalid SDK revision')
        entries[package] = dict(entry, destination=str(sdk.joinpath(*package.split(';'))), executable=executable)
    for name, entry in entries.items():
        url = urllib.parse.urlsplit(entry['url'])
        if (url.scheme != 'https' or url.hostname not in ('github.com', 'dl.google.com')
                or url.username or url.password or url.query or url.fragment or any(c.isspace() for c in entry['url'])
                or entry['digest_algorithm'] not in ('sha1', 'sha256')
                or not re.fullmatch('[a-f0-9]{' + str(hashlib.new(entry['digest_algorithm']).digest_size * 2) + '}', entry['digest'])):
            raise ValueError('invalid toolchain archive identity')
        if not Path(entry['destination']).is_absolute():
            raise ValueError('toolchain path must be absolute')
    return entries


def digest(path, algorithm='sha256'):
    with open(path, 'rb') as stream:
        return hashlib.file_digest(stream, algorithm).hexdigest()


def payload_digest(directory):
    # Bind all reused bytes, including SDK/JDK supporting jars and native libraries.
    identity = hashlib.sha256()
    for path in sorted(directory.rglob('*')):
        relative = str(path.relative_to(directory))
        # SDK Manager regenerates root package.xml while scanning archive-installed packages.
        if relative in ('.subyard-toolchain.json', 'package.xml'):
            continue
        if path.is_symlink():
            value = ('link', relative, os.readlink(path))
        elif path.is_file():
            value = ('file', relative, digest(path))
        elif path.is_dir():
            value = ('directory', relative)
        else:
            raise ValueError('invalid toolchain payload')
        identity.update(json.dumps(value, separators=(',', ':')).encode())
    return identity.hexdigest()


def properties(path):
    return dict((k.strip(), v.strip().strip('"')) for line in path.read_text().splitlines()
                if '=' in line and not line.startswith('#') for k, v in [line.split('=', 1)])


def verify(directory, name, entry, receipt=True):
    """Verify selected metadata and the installed payload bytes; no network."""
    directory = Path(directory)
    executable = directory / entry['executable']
    if directory.is_symlink() or not executable.is_file() or executable.is_symlink():
        return False
    try:
        if receipt:
            identity = json.loads((directory / '.subyard-toolchain.json').read_text())
            if identity != dict(archive=entry['digest'], algorithm=entry['digest_algorithm'],
                                version=entry['version'], payload_sha256=payload_digest(directory)):
                return False
        if name == 'jdk':
            values = properties(directory / 'release')
            if values.get('JAVA_RUNTIME_VERSION') != entry['version'] or values.get('OS_ARCH') != 'x86_64':
                return False
            result = subprocess.run([str(executable), '-version'], capture_output=True, timeout=15, check=True)
            if entry['version'].encode() not in result.stdout + result.stderr:
                return False
        else:
            values = properties(directory / 'source.properties')
            if name.startswith('platforms;android-') and values.get('AndroidVersion.ApiLevel') != name.removeprefix('platforms;android-'):
                return False
            version = values.get('Pkg.Revision', '')
            if '.'.join((version.split('.') + ['0', '0'])[:3]) != entry['version']:
                return False
        if entry['executable'] != 'android.jar' and not os.access(executable, os.X_OK):
            return False
        return True
    except (OSError, ValueError, subprocess.SubprocessError):
        return False


def install(archive, destination, name, entry):
    if digest(archive, entry['digest_algorithm']) != entry['digest']:
        raise ValueError('toolchain archive checksum mismatch')
    destination = Path(destination)
    if destination.is_symlink() or (destination.exists() and not destination.is_dir()):
        raise ValueError('toolchain destination is not a managed directory')
    destination.parent.mkdir(parents=True, exist_ok=True)
    # Stage beside the destination: never expose partial extraction as an installed component.
    with tempfile.TemporaryDirectory(prefix='.toolchain-', dir=destination.parent) as temporary:
        stage = Path(temporary)
        unpack = stage / 'unpack'
        unpack.mkdir()
        if name == 'jdk':
            with tarfile.open(archive) as package:
                package.extractall(unpack, filter='data')
        else:
            with zipfile.ZipFile(archive) as package:
                for member in package.infolist():
                    path = Path(member.filename)
                    if path.is_absolute() or '..' in path.parts:
                        raise ValueError('unsafe toolchain archive path')
                    # Reject archive symlinks before invoking unzip.
                    if (member.external_attr >> 16) & 0o170000 == 0o120000:
                        raise ValueError('unsafe toolchain archive symlink')
            subprocess.run(['unzip', '-q', str(archive), '-d', str(unpack)], check=True,
                           stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, timeout=120)
        roots = list(unpack.iterdir())
        if len(roots) != 1 or not verify(roots[0], name, entry, receipt=False):
            raise ValueError('extracted toolchain does not match configuration')
        root = roots[0]
        (root / '.subyard-toolchain.json').write_text(json.dumps(dict(archive=entry['digest'],
            algorithm=entry['digest_algorithm'], version=entry['version'],
            payload_sha256=payload_digest(root))) + '\n')
        previous = stage / 'previous'
        if destination.exists():
            destination.rename(previous)
        try:
            root.rename(destination)
        except BaseException:
            if previous.exists():
                previous.rename(destination)
            raise


def main():
    action, manifest, *args = sys.argv[1:]
    entries = selected(manifest)
    if action == 'defaults':
        api = next(name.removeprefix('platforms;android-') for name in entries if name.startswith('platforms;'))
        print(api + '\t' + entries['jdk']['major'])
    elif action == 'plan':
        for name, entry in entries.items():
            print(name + '\t' + entry['url'])
    elif action == 'check':
        prefix = args[0] if args else ''
        return 0 if all(verify(prefix + e['destination'], n, e) for n, e in entries.items()) else 10
    elif action == 'component-check':
        return 0 if verify(entries[args[0]]['destination'], args[0], entries[args[0]]) else 10
    elif action == 'install':
        name, archive = args
        install(archive, entries[name]['destination'], name, entries[name])
    else:
        raise ValueError('unknown toolchain operation')
    return 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (OSError, ValueError, KeyError, TypeError, AttributeError, subprocess.SubprocessError, tarfile.TarError, zipfile.BadZipFile):
        # Do not disclose caller paths, URLs or native command output.
        print('android toolchain: invalid configuration, archive or installed component', file=sys.stderr)
        sys.exit(1)
