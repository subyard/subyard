#!/usr/bin/env python3
"""Profile-owned pinned toolchain selection and verified staged installation."""
import contextlib
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import signal
import subprocess
import sys
import tarfile
import secrets
import shutil
import tempfile
import time
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET
import zipfile


# Default SDK packages: every manifest entry is installed and kept at its pinned revision.
# Provisioning never removes other packages; it replaces a directory only when it converges a
# default with the same identifier (for example a broker-added copy of a later default).
PACKAGE = re.compile(r'platforms;android-[1-9][0-9]*(?:\.[0-9]+)?|build-tools;[0-9]+\.[0-9]+\.[0-9]+'
                     r'|cmdline-tools;latest|platform-tools|emulator')
BASE_PACKAGES = ('cmdline-tools;latest', 'platform-tools', 'emulator')
# License identifiers whose acceptance the installed SDK manager records under licenses/.
LICENSES = ('android-sdk-license',)


class ConfigurationError(ValueError):
    """Every independently determinable manifest/selection refusal, without paths or URLs."""


def executable(package):
    if package.startswith('platforms;'):
        return 'android.jar'
    if package.startswith('build-tools;'):
        return 'aapt2'
    return {'cmdline-tools;latest': 'bin/sdkmanager', 'platform-tools': 'adb', 'emulator': 'emulator'}[package]


def sdk_root(environment):
    return Path(environment.get('ANDROID_SDK_ROOT', '/srv/cache/android-sdk'))


def selected(path, *, environment, architecture, metadata=None):
    manifest = json.loads(Path(path).read_text())
    if (type(manifest['schema']) is not int or manifest['schema'] != 1 or manifest['architecture'] != architecture
            or manifest['architecture'] != 'x86_64' or manifest['image_policy'] != 'latest-stable'):
        raise ValueError('unsupported toolchain configuration')
    errors = []
    jdk = manifest['jdk']
    version = re.fullmatch(r'([1-9][0-9]*)(?:\.\d+)*\+\d+', jdk['version'])
    if (version is None or version.group(1) != jdk['major']
            or environment.get('JDK_VERSION', jdk['major']) != jdk['major']):
        errors.append('JDK version does not match selected major')
    packages = manifest['sdk']
    # ANDROID_API is the emulator default API; BUILD_TOOLS_VERSION is kept for compatibility.
    # Both must name installed defaults; neither narrows the default package list.
    api = environment.get('ANDROID_API', manifest['android_api'])
    build = environment.get('BUILD_TOOLS_VERSION', manifest['build_tools'])
    # The emulator API selects a system image, not a build platform.
    if not re.fullmatch(r'[1-9][0-9]{0,2}', api):
        errors.append('ANDROID_API must be an integer emulator API level')
    repository = urllib.parse.urlsplit(manifest.get('repository', ''))
    if (repository.scheme != 'https' or repository.hostname != 'dl.google.com' or repository.query
            or repository.fragment or repository.username or repository.password):
        errors.append('repository must be the Google SDK repository metadata URL')
    if not re.fullmatch(r'\d+\.\d+\.\d+', build) or 'build-tools;' + build not in packages:
        errors.append('BUILD_TOOLS_VERSION must name a default build-tools package')
    for package in BASE_PACKAGES:
        if package not in packages:
            errors.append(f'default SDK package {package} is required')
    sdk = sdk_root(environment)
    entries = {'jdk': dict(jdk, destination='/opt/jdk-' + jdk['major'], executable='bin/java')}
    for package, entry in packages.items():
        if not PACKAGE.fullmatch(package):
            errors.append(f'{package[:80]}: unsupported default SDK package identifier')
            continue
        if not re.fullmatch(r'\d+\.\d+\.\d+', entry['version']):
            errors.append(f'{package}: invalid SDK revision')
        if entry.get('license') not in LICENSES:
            errors.append(f'{package}: unsupported SDK license')
        entries[package] = dict(entry, destination=str(sdk.joinpath(*package.split(';'))), executable=executable(package))
    for name, entry in entries.items():
        url = urllib.parse.urlsplit(entry['url'])
        if (url.scheme != 'https' or url.hostname not in ('github.com', 'dl.google.com')
                or url.username or url.password or url.query or url.fragment or any(c.isspace() for c in entry['url'])
                or entry['digest_algorithm'] not in ('sha1', 'sha256')
                or not re.fullmatch('[a-f0-9]{' + str(hashlib.new(entry['digest_algorithm']).digest_size * 2) + '}', entry['digest'])):
            errors.append(f'{name}: invalid toolchain archive identity')
        if not Path(entry['destination']).is_absolute():
            errors.append(f'{name}: toolchain path must be absolute')
    # Provisioning writes package.xml offline from the vendored entry for each pinned archive.
    try:
        vendored = Path(metadata or Path(path).with_name('toolchain-metadata.xml')).read_text()
        repository_root(vendored)
    except (OSError, ValueError):
        vendored = None
        errors.append('vendored SDK repository metadata is missing or invalid')
    if vendored is not None:
        for package, entry in packages.items():
            if package in entries:
                try:
                    unique = len(repository_blocks(vendored, package, entry['digest'])) == 1
                except ET.ParseError:
                    unique = False
                if not unique:
                    errors.append(f'{package}: no unique vendored repository entry for the pinned archive; '
                                  'run toolchain.py vendor-metadata')
    if errors:
        raise ConfigurationError('; '.join(errors))
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


COMMON = 'http://schemas.android.com/repository/android/common/02'
# Trimmed, unmodified repository entries for every pin and for retired earlier defaults.
METADATA = Path(__file__).with_name('toolchain-metadata.xml')
CATEGORIES = ('platforms', 'build-tools')


def declared(directory):
    """(path, revision) recorded in a component's package.xml, or None."""
    try:
        path = Path(directory) / 'package.xml'
        if path.is_symlink() or not path.is_file():
            return None
        tree = ET.fromstring(path.read_bytes())
    except (OSError, ET.ParseError):
        return None
    if tree.tag != '{' + COMMON + '}repository':
        return None
    packages = [node for node in tree if node.tag == 'localPackage']
    if len(packages) != 1:
        return None
    revision = packages[0].find('revision')
    if revision is None:
        return None
    return packages[0].get('path'), '.'.join(revision.findtext(part, '0') for part in ('major', 'minor', 'micro'))


def metadata_ready(directory, name, version):
    return declared(directory) == (name, version)


def repository_root(text):
    root = re.search(r'<sdk:sdk-repository\s([^>]*)>', text)
    if root is None:
        raise ValueError('unsupported SDK repository metadata')
    return root


def repository_blocks(text, name, digest):
    """Unmodified <remotePackage> blocks for one package path whose archive has the digest."""
    root = repository_root(text)
    blocks = []
    for block in re.findall(r'<remotePackage\b[^>]*\bpath="' + re.escape(name) + r'"[^>]*>.*?</remotePackage>',
                            text, re.S):
        element = ET.fromstring('<wrap ' + root.group(1) + '>' + block + '</wrap>')[0]
        if digest in [node.text.strip().lower() for node in element.iter('checksum') if node.text]:
            blocks.append(block)
    return blocks


def package_xml(document, name, digest):
    """The installed-package metadata the SDK manager writes, built from Google's repository
    entry for exactly the verified archive (path and archive checksum must both match)."""
    text = document.decode('utf-8')
    root = repository_root(text)
    declarations = dict(re.findall(r'xmlns:([A-Za-z0-9_-]+)="([^"]*)"', root.group(1)))
    prefixes = [prefix for prefix, uri in declarations.items() if uri == COMMON]
    if len(prefixes) != 1:
        raise ValueError('unsupported SDK repository metadata')
    candidates = repository_blocks(text, name, digest)
    if len(candidates) != 1:
        raise ValueError('SDK repository metadata has no unique entry for the verified archive')
    block = candidates[0]
    license = re.search(r'<uses-license ref="([^"]+)"', block)
    if license is None:
        raise ValueError('SDK repository entry has no license')
    licenses = re.findall(r'<license id="' + re.escape(license.group(1)) + r'"[^>]*>.*?</license>', text, re.S)
    if len(licenses) != 1:
        raise ValueError('SDK repository license is missing')
    body = re.sub(r'\s*<channelRef [^>]*/>', '', block)
    body = re.sub(r'\s*<archives>.*?</archives>', '', body, flags=re.S)
    body = re.sub(r'^<remotePackage [^>]*>', '<localPackage path="' + name + '" obsolete="false">', body)
    body = body[:body.rindex('</remotePackage>')] + '</localPackage>'
    output = ('<?xml version="1.0" encoding="UTF-8" standalone="yes"?>\n<' + prefixes[0] + ':repository '
              + root.group(1).strip() + '>\n  ' + licenses[0] + '\n  ' + body + '\n</' + prefixes[0] + ':repository>\n')
    ET.fromstring(output.encode())  # Well-formed, self-contained namespaces.
    return output.encode()


def fetch_repository(url):
    with urllib.request.urlopen(url, timeout=60) as response:
        data = response.read(32 * 1024 * 1024 + 1)
    if len(data) > 32 * 1024 * 1024:
        raise ValueError('SDK repository metadata too large')
    ET.fromstring(data)
    return data


def block_identity(root, block):
    """(package path, archive checksums) of one <remotePackage> block."""
    element = ET.fromstring('<wrap ' + root + '>' + block + '</wrap>')[0]
    return element.get('path'), {node.text.strip().lower() for node in element.iter('checksum') if node.text}


def vendor_metadata(manifest, metadata=None, fetch=fetch_repository):
    """Developer helper: rewrite the vendored metadata from the live repository for every manifest
    pin, keeping existing retired blocks for earlier defaults that may remain on disk. A pin that
    has rotated out upstream keeps its vendored block; nothing is written unless every pin and
    retired archive ends up with exactly one block."""
    manifest = Path(manifest)
    metadata = Path(metadata or manifest.with_name('toolchain-metadata.xml'))
    pins = json.loads(manifest.read_text())
    live = fetch(pins['repository']).decode('utf-8')
    root = repository_root(live)
    licenses = re.findall(r'<license id="android-sdk-license"[^>]*>.*?</license>', live, re.S)
    if len(licenses) != 1:
        raise ValueError('SDK repository license is missing')
    current = metadata.read_text() if metadata.exists() else ''
    current_root = repository_root(current).group(1) if current else root.group(1)
    blocks, sources, missing = [], [], []
    for name, entry in pins['sdk'].items():
        found, source = repository_blocks(live, name, entry['digest']), root.group(1)
        if not found and current:
            found, source = repository_blocks(current, name, entry['digest']), current_root  # Rotated out upstream.
        if len(found) != 1:
            missing.append(name)
        blocks += found[:1]
        sources += [source] * len(found[:1])
    if missing:
        raise ConfigurationError('no unique repository or vendored entry for pinned ' + ', '.join(missing))
    pinned = {entry['digest'] for entry in pins['sdk'].values()}
    for block in re.findall(r'<remotePackage\b[^>]*>.*?</remotePackage>', current, re.S):
        # Retired: an earlier default may still be installed. A block for a current pin's archive
        # is superseded by the chosen (upstream when listed) block.
        if block not in blocks and not block_identity(current_root, block)[1] & pinned:
            blocks.append(block)
            sources.append(current_root)
    identities = [block_identity(source, block) for source, block in zip(sources, blocks)]
    duplicates = sorted({f'{path}@{digest}' for index, (path, digests) in enumerate(identities) for digest in digests
                         if any(other == path and digest in others
                                for position, (other, others) in enumerate(identities) if position != index)})
    if duplicates:
        raise ConfigurationError('vendored metadata would not be unique for ' + ', '.join(duplicates))
    output = ('<?xml version="1.0" encoding="utf-8"?>\n<!-- Trimmed from ' + pins['repository']
              + ' by toolchain.py vendor-metadata; blocks are unmodified. -->\n<sdk:sdk-repository '
              + root.group(1).strip() + '>\n  ' + licenses[0] + '\n  ' + '\n  '.join(blocks) + '\n</sdk:sdk-repository>\n')
    ET.fromstring(output.encode())
    metadata.write_text(output)


def write_metadata(directory, content):
    directory = Path(directory)
    temporary = directory.parent / ('.' + directory.name + '.package.xml.new')
    temporary.write_bytes(content)
    temporary.chmod(0o644)
    os.replace(temporary, directory / 'package.xml')


def component_layout(sdk):
    """Profile-receipted SDK components by package identifier, defaults or earlier defaults."""
    sdk = Path(sdk)
    found = {}
    for name, relative in (('cmdline-tools;latest', 'cmdline-tools/latest'), ('platform-tools', 'platform-tools'),
                           ('emulator', 'emulator')):
        found[name] = sdk / relative
    for category in CATEGORIES:
        root = sdk / category
        if root.is_dir() and not root.is_symlink():
            for child in sorted(root.iterdir()):
                if not child.name.startswith('.'):
                    found[category + ';' + child.name] = child
    return found


def converge_metadata(sdk, entries, metadata=METADATA):
    """Give every default, and every earlier profile default kept on disk, the package.xml AGP
    needs to see it on a read-only SDK, from the vendored metadata only (no network). Pinned
    archives and payload digests are unchanged."""
    document = None
    def repository():
        nonlocal document
        if document is None:
            document = Path(metadata).read_bytes()
        return document
    written, skipped = [], []
    for name, entry in entries.items():
        if name == 'jdk' or metadata_ready(entry['destination'], name, entry['version']):
            continue
        write_metadata(entry['destination'], package_xml(repository(), name, entry['digest']))
        written.append(name)
    for name, directory in component_layout(sdk).items():
        if name in entries or directory.is_symlink() or not directory.is_dir() or declared(directory):
            continue
        try:
            receipt = json.loads((directory / '.subyard-toolchain.json').read_text())
        except (OSError, ValueError):
            continue  # Not profile-installed: never modified.
        try:
            if (receipt.get('algorithm') != 'sha1' or not re.fullmatch('[a-f0-9]{40}', str(receipt.get('archive')))
                    or receipt.get('payload_sha256') != payload_digest(directory)):
                raise ValueError('earlier default changed since installation')
            write_metadata(directory, package_xml(repository(), name, receipt['archive']))
            written.append(name)
        except (OSError, ValueError, ET.ParseError):
            skipped.append(name)
    for name in written:
        print(f'android provision: package metadata component={name} state=written', file=sys.stderr)
    for name in skipped:
        print(f'android provision: package metadata component={name} state=unavailable', file=sys.stderr)
    return written, skipped


def license_hashes(path):
    """Accepted-license hashes in one SDK license file; an unsafe or missing file has none."""
    path = Path(path)
    if path.is_symlink() or not path.is_file():
        return []
    return [line.strip() for line in path.read_text().splitlines() if re.fullmatch('[a-f0-9]{40}', line.strip())]


def licenses_ready(sdk, entries):
    required = sorted({entry['license'] for name, entry in entries.items() if name != 'jdk'})
    return all(license_hashes(Path(sdk) / 'licenses' / license) for license in required)


def run_session(command, **kwargs):
    """Run a command in its own session and stop anything it leaves there (the Android CLI
    starts a background metrics upload) before its staging directory is removed."""
    timeout = kwargs.pop('timeout')
    kwargs.pop('check', None)
    process = subprocess.Popen(command, start_new_session=True, **kwargs)
    deadline = time.monotonic() + timeout
    try:
        while os.waitid(os.P_PID, process.pid, os.WEXITED | os.WNOHANG | os.WNOWAIT) is None:
            if time.monotonic() >= deadline:
                raise subprocess.TimeoutExpired(command, timeout)
            time.sleep(0.2)
    finally:
        # The unreaped leader keeps the process group id reserved for this signal.
        with contextlib.suppress(ProcessLookupError, PermissionError):
            os.killpg(process.pid, signal.SIGKILL)
        process.wait()
    if process.returncode:
        raise subprocess.CalledProcessError(process.returncode, command)


LICENSE_STAGING = {'schema': 1, 'kind': 'subyard-android-license-staging'}


def sweep_license_staging(sdk):
    """Remove only marker-owned license staging an interrupted run left in the SDK root."""
    for stage in Path(sdk).glob('.subyard-licenses-*'):
        if stage.is_symlink() or not stage.is_dir() or not re.fullmatch(r'\.subyard-licenses-[a-f0-9]{24}', stage.name):
            continue
        try:
            owned = json.loads((stage / '.subyard-owner.json').read_text()) == LICENSE_STAGING
        except (OSError, ValueError):
            owned = False
        if owned:
            shutil.rmtree(stage, ignore_errors=True)


def accept_licenses(sdk, entries, java_home, run=run_session):
    """Record Google's SDK licenses the supported way: the installed SDK manager accepts them
    while installing one default package into a private staging root; only its license files
    are kept. Existing accepted hashes are preserved."""
    sdk = Path(sdk)
    packages = [name for name in entries if name != 'jdk']
    required = sorted({entries[name]['license'] for name in packages})
    # An interrupted run's staging is removed even when the licenses are already recorded.
    sweep_license_staging(sdk)
    if licenses_ready(sdk, entries):
        print('android provision: licenses state=reused', file=sys.stderr)
        return False
    licenses = sdk / 'licenses'
    if licenses.is_symlink() or (licenses.exists() and not licenses.is_dir()):
        raise ValueError('SDK license directory is not a managed directory')
    stage = sdk / ('.subyard-licenses-' + secrets.token_hex(12))
    stage.mkdir(mode=0o700)
    try:
        (stage / '.subyard-owner.json').write_text(json.dumps(LICENSE_STAGING) + '\n')
        for directory in ('sdk', 'home', 'tmp'):
            (stage / directory).mkdir()
        for license in required:
            # Prefer the small platform-tools archive as the staging package for each license.
            probe = next(name for name in sorted(packages, key=lambda name: name != 'platform-tools')
                         if entries[name]['license'] == license)
            run([str(sdk / 'cmdline-tools/latest/bin/sdkmanager'), '--sdk_root=' + str(stage / 'sdk'), probe],
                env=dict(os.environ, JAVA_HOME=str(java_home), HOME=str(stage / 'home'), TMPDIR=str(stage / 'tmp'),
                         ANDROID_USER_HOME=str(stage / 'home/.android'), XDG_CACHE_HOME=str(stage / 'home/.cache')),
                stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                timeout=900, check=True)
        licenses.mkdir(mode=0o755, exist_ok=True)
        for license in required:
            accepted = license_hashes(stage / 'sdk/licenses' / license)
            if not accepted:
                raise ValueError('SDK manager did not record the required license')
            target = licenses / license
            merged = license_hashes(target)
            merged += [value for value in accepted if value not in merged]
            staged = stage / (license + '.new')
            staged.write_text(''.join('\n' + value for value in merged))
            staged.chmod(0o644)
            staged.replace(target)
    finally:
        shutil.rmtree(stage, ignore_errors=True)
    print('Android: accepted Google Android SDK licenses for ' + ', '.join(packages), file=sys.stderr)
    return True


def main():
    action, manifest, *args = sys.argv[1:]
    if action == 'vendor-metadata':
        # Developer-only: needs the network; provisioning never fetches metadata.
        vendor_metadata(manifest)
        return 0
    entries = selected(manifest, environment=os.environ, architecture=platform.machine())
    if action == 'defaults':
        api = os.environ.get('ANDROID_API', json.loads(Path(manifest).read_text())['android_api'])
        print(api + '\t' + entries['jdk']['major'])
    elif action == 'plan':
        for name, entry in entries.items():
            print(name + '\t' + entry['url'])
    elif action == 'check':
        prefix = args[0] if args else ''
        ready = all(verify(prefix + e['destination'], n, e) and
                    (n == 'jdk' or metadata_ready(prefix + e['destination'], n, e['version']))
                    for n, e in entries.items())
        return 0 if ready and licenses_ready(prefix + str(sdk_root(os.environ)), entries) else 10
    elif action == 'component-check':
        return 0 if verify(entries[args[0]]['destination'], args[0], entries[args[0]]) else 10
    elif action == 'install':
        name, archive = args
        install(archive, entries[name]['destination'], name, entries[name])
    elif action == 'metadata':
        converge_metadata(sdk_root(os.environ), entries, Path(manifest).with_name('toolchain-metadata.xml'))
    elif action == 'licenses':
        accept_licenses(sdk_root(os.environ), entries, os.environ.get('JAVA_HOME', entries['jdk']['destination']))
    else:
        raise ValueError('unknown toolchain operation')
    return 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except ConfigurationError as exc:
        # Manifest/selection refusals name only profile fields and package identifiers.
        print(f'android toolchain: invalid configuration: {exc}', file=sys.stderr)
        sys.exit(1)
    except (OSError, ValueError, KeyError, TypeError, AttributeError, StopIteration,
            subprocess.SubprocessError, tarfile.TarError, zipfile.BadZipFile):
        # Do not disclose caller paths, URLs or native command output.
        print('android toolchain: invalid configuration, archive or installed component', file=sys.stderr)
        sys.exit(1)
