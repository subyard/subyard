#!/usr/bin/env python3
"""Android profile's private pool owner. No consumer can select paths or commands."""
import contextlib
import fcntl
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import platform
import pwd
import re
import secrets
import select
import shutil
import signal
import socket
import socketserver
import stat
import struct
import subprocess
import sys
import tempfile
import threading
import time
import traceback
import urllib.request
import xml.etree.ElementTree as ET

TTL = 600
HEARTBEAT = 60
BOOT_TIMEOUT = 1200
SCHEMA = 'subyard.android-pool.v1'
VERSIONS = {34: '14', 35: '15', 36: '16'}
# Emulator 37.1.11 raised the tested phone to 2.5GiB. The current upstream
# large-screen API 33+ floor is 4GiB, so keep tablet below three megapixels.
# Reserve effective guest RAM, not lower requested values. See
# https://android.googlesource.com/platform/external/qemu/+/refs/heads/emu-main-dev/android/android-emu/android/main-common.c
PRESETS = {'phone': (1080, 1920, 420, 2560), 'tablet': (800, 1280, 160, 2560)}
RUNTIME_OVERHEAD_MIB = 1024
VARIANTS = {'google_apis', 'google_apis_playstore'}


class PoolError(Exception):
    def __init__(self, code, message):
        self.code = code
        super().__init__(message)


def require(ok, code, message):
    if not ok:
        raise PoolError(code, message)


def atomic(path, value):
    path = Path(path)
    temporary = path.with_name(path.name + '.new')
    with open(temporary, 'w', encoding='utf-8') as stream:
        os.chmod(temporary, 0o600)
        json.dump(value, stream, separators=(',', ':'))
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temporary, path)
    fd = os.open(path.parent, os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def run(args, **kwargs):
    return subprocess.run(args, check=kwargs.pop('check', True), timeout=kwargs.pop('timeout', 60),
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE, **kwargs)


def utc(timestamp):
    return time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime(timestamp)) if timestamp else None


def report_failure(exc):
    # Log code locations, never exception values, requests or lease credentials.
    frames = traceback.extract_tb(exc.__traceback__)
    locations = ' -> '.join(f'{Path(f.filename).name}:{f.lineno}:{f.name}' for f in frames)
    print(f'Android pool: {type(exc).__name__} at {locations}', file=sys.stderr, flush=True)


class Images:
    """Only this private directory is pruneable; build SDK and user AVDs are external."""
    def __init__(self, root, sdk, java_home='/opt/jdk-17', disk_headroom=1024**3):
        self.root, self.sdk = Path(root), Path(sdk)
        self.java_home = java_home
        self.disk_headroom = disk_headroom
        self.root.mkdir(parents=True, exist_ok=True)
        os.chmod(self.root, 0o700)
        self.lock = threading.RLock()
        self.metadata_lock = threading.Lock()
        # Called only by the singleton owner, before any downloads start.
        for stage in self.root.glob('download-*'):
            if stage.is_symlink() or not re.fullmatch('download-[a-f0-9]{24}', stage.name):
                continue
            try:
                marker = json.loads((stage / '.download-owner.json').read_text())
            except (OSError, ValueError):
                # A crash can occur between mkdir and the atomic ownership marker.
                if stage.is_dir() and stage.stat().st_uid == os.geteuid() and not any(stage.iterdir()):
                    stage.rmdir()
                continue
            if marker == {'schema': SCHEMA, 'kind': 'image-download'}:
                shutil.rmtree(stage)

    def packages(self, variant):
        with self.metadata_lock:
            return self._packages(variant)

    def _packages(self, variant):
        manifest = self.root / (variant + '.xml')
        if not manifest.exists() or time.time() - manifest.stat().st_mtime > 86400:
            try:
                url = f'https://dl.google.com/android/repository/sys-img/{variant}/sys-img2-3.xml'
                with urllib.request.urlopen(url, timeout=30) as response:
                    data = response.read(8 * 1024 * 1024 + 1)
                require(len(data) <= 8 * 1024 * 1024, 'download', 'repository metadata too large')
                ET.fromstring(data)
                temporary = manifest.with_suffix('.new')
                temporary.write_bytes(data)
                temporary.replace(manifest)
            except (OSError, ET.ParseError) as exc:
                raise PoolError('download', 'Android image catalog download failed') from exc
        try:
            tree = ET.fromstring(manifest.read_bytes())
        except (OSError, ET.ParseError) as exc:
            raise PoolError('download', 'Android image catalog is invalid') from exc
        result = {}
        for node in tree.iter():
            if node.tag.rsplit('}', 1)[-1] != 'remotePackage':
                continue
            # Child elements are unqualified in Google's repository schema.
            revision = node.find('revision')
            if revision is None:
                continue
            revision = '.'.join(revision.findtext(part, '0') for part in ('major', 'minor', 'micro'))
            for archive in node.findall('./archives/archive'):
                if archive.findtext('host-os', 'linux') != 'linux':
                    continue
                complete = archive.find('complete')
                if complete is None:
                    continue
                filename = complete.findtext('url', '')
                if not re.fullmatch(r'[A-Za-z0-9_.-]+\.zip', filename):
                    continue
                package = node.attrib['path']
                digest = complete.findtext('checksum', '')
                if not re.fullmatch('[a-fA-F0-9]{40}', digest):
                    continue
                result[package] = dict(package=package, revision=revision,
                    url=f'https://dl.google.com/android/repository/sys-img/{variant}/{filename}',
                    sha1=digest.lower(), size=int(complete.findtext('size', '0')))
        return result

    def resolve(self, request):
        package = f"system-images;android-{request['api']};{request['variant']};{request['abi']}"
        entry = self.packages(request['variant']).get(package)
        require(entry is not None, 'unsupported', 'requested Android image is unavailable')
        key = hashlib.sha256((package + '@' + entry['revision']).encode()).hexdigest()
        return dict(entry, key=key)

    def cached(self, image):
        directory = self.root / image['key']
        if directory.is_symlink() or not (directory / 'owned.json').is_file():
            return False
        try:
            receipt = json.loads((directory / 'owned.json').read_text())
            return all(receipt.get(k) == image.get(k) for k in ('key', 'package', 'revision', 'sha1')) \
                and (directory / 'image/system.img').is_file()
        except (OSError, ValueError):
            return False

    @contextlib.contextmanager
    def download_lock(self, cancel):
        while not self.lock.acquire(timeout=0.2):
            require(not cancel.is_set(), 'cancelled', 'allocation cancelled while waiting for image cache')
        try:
            require(not cancel.is_set(), 'cancelled', 'allocation cancelled')
            yield
        finally:
            self.lock.release()

    def ensure(self, image, cancel):
        with self.download_lock(cancel):
            if self.cached(image):
                return self.root / image['key'] / 'image'
            require(shutil.disk_usage(self.root).free > max(image['size'] * 6, 8 * 1024**3) + self.disk_headroom,
                    'capacity', 'insufficient space for image download and extraction')
            stage = self.root / ('download-' + secrets.token_hex(12))
            stage.mkdir(mode=0o700)
            try:
                atomic(stage / '.download-owner.json', dict(schema=SCHEMA, kind='image-download'))
                sdk = stage / 'sdk'
                sdk.mkdir()
                home, temporary = stage / 'home', stage / 'tmp'
                home.mkdir()
                temporary.mkdir()
                if (self.sdk / 'licenses').is_dir():
                    shutil.copytree(self.sdk / 'licenses', sdk / 'licenses')
                # Use the installed SDK tool's licensing, integrity and extraction semantics.
                # Command-line Tools 23 replaced --licenses with Android CLI installation policy.
                process = subprocess.Popen([
                    str(self.sdk / 'cmdline-tools/latest/bin/sdkmanager'),
                    '--sdk_root=' + str(sdk), image['package']],
                    env=dict(os.environ, JAVA_HOME=self.java_home, HOME=str(home), TMPDIR=str(temporary),
                             ANDROID_USER_HOME=str(home / '.android'), XDG_CACHE_HOME=str(home / '.cache')),
                    stdin=subprocess.DEVNULL,
                    stdout=subprocess.PIPE, stderr=subprocess.STDOUT, start_new_session=True)
                deadline = time.monotonic() + 900
                prefix, tail, seen = b'', b'', 0
                nonblocking = False

                def drain():
                    nonlocal prefix, tail, seen
                    drained = 0
                    # At most 64 KiB per turn; noisy output cannot starve guards.
                    for _ in range(4):
                        try:
                            chunk = os.read(process.stdout.fileno(), 16384)
                        except BlockingIOError:
                            break
                        if not chunk:
                            break
                        drained += len(chunk)
                        seen += len(chunk)
                        take = min(len(chunk), 8192 - len(prefix))
                        prefix += chunk[:take]
                        tail = (tail + chunk[take:])[-8192:]
                    return drained

                try:
                    os.set_blocking(process.stdout.fileno(), False)
                    nonblocking = True
                    while process.poll() is None:
                        drained = drain()
                        require(not cancel.wait(0 if drained else 0.2), 'cancelled', 'allocation cancelled')
                        require(time.monotonic() < deadline, 'download', 'image installation exceeded 900 seconds')
                        require(shutil.disk_usage(self.root).free > self.disk_headroom, 'capacity',
                                'image installation stopped to preserve yard disk headroom')
                finally:
                    if process.poll() is None:
                        with contextlib.suppress(ProcessLookupError):
                            os.killpg(process.pid, signal.SIGKILL)
                    process.wait()
                    try:
                        if nonblocking:
                            drain()
                    finally:
                        process.stdout.close()
                if process.returncode != 0:
                    truncated = seen > len(prefix) + len(tail)
                    output = prefix + (b'\n' if truncated else b'') + tail
                    observed = [name for name, pattern in (
                        ('dns', rb'\bUnknownHostException\b'),
                        ('tls', rb'\bSSLHandshakeException\b'),
                        ('connect', rb'\bConnectException\b'),
                        ('network_timeout', rb'\bSocketTimeoutException\b'),
                        ('no_space', rb'No space left on device'),
                        ('license_rejected', rb'license[^\n]{0,256}(?:not accepted|not been accepted)'),
                        ('checksum', rb'checksum (?:mismatch|verification failed)'),
                        ('package_missing', rb'Failed to find package'),
                    ) if re.search(pattern, output, re.IGNORECASE)]
                    try:
                        free_bytes = shutil.disk_usage(self.root).free
                    except OSError:
                        free_bytes = -1
                    raise PoolError('download',
                        f'SDK image installation failed (exit {process.returncode}); '
                        'check licensing, disk space and download access; '
                        f'observed={",".join(observed) or "unknown"} free_bytes={free_bytes} '
                        f'truncated={int(truncated)}')
                installed = sdk.joinpath(*image['package'].split(';'))
                properties = dict(line.split('=', 1) for line in (installed / 'source.properties').read_text().splitlines()
                                  if '=' in line and not line.startswith('#'))
                properties = {key.strip(): value.strip() for key, value in properties.items()}
                package = image['package'].split(';')
                require(properties.get('AndroidVersion.ApiLevel') == package[1][8:]
                        and properties.get('SystemImage.Abi') == package[3]
                        and properties.get('SystemImage.TagId') == package[2],
                        'download', 'installed image does not match requested API, variant and ABI')
                revision = properties.get('Pkg.Revision', '').split('.')
                revision = '.'.join((revision + ['0', '0'])[:3])
                if revision != image['revision']:
                    with self.metadata_lock:
                        (self.root / (package[2] + '.xml')).unlink(missing_ok=True)
                    raise PoolError('revision', 'repository revision changed; retry with refreshed catalog')
                require((installed / 'system.img').is_file(), 'download', 'installed image has no system image')
                installed.rename(stage / 'image')
                shutil.rmtree(sdk)
                shutil.rmtree(home)
                shutil.rmtree(temporary)
                atomic(stage / 'owned.json', image)
                # Immutable image data is readable by the runtime account, never writable by dev.
                for directory, _, files in os.walk(stage):
                    os.chmod(directory, 0o755)
                    for name in files:
                        os.chmod(Path(directory) / name, 0o644)
                require(not cancel.is_set(), 'cancelled', 'allocation cancelled')
                destination = self.root / image['key']
                require(not destination.exists(), 'cache', 'unrecognized image cache entry; refusing replacement')
                stage.rename(destination)
                fd = os.open(self.root, os.O_DIRECTORY)
                try:
                    os.fsync(fd)
                finally:
                    os.close(fd)
                return destination / 'image'
            except PoolError:
                raise
            except (OSError, ValueError) as exc:
                raise PoolError('download', 'Android image download/extraction failed') from exc
            finally:
                if stage.exists():
                    shutil.rmtree(stage)

    def prune(self, pinned, dry_run):
        removed, skipped, freed = [], [], 0
        with self.lock:
            for directory in sorted(self.root.iterdir()):
                if directory.is_symlink() or not re.fullmatch('[a-f0-9]{64}', directory.name):
                    continue
                try:
                    receipt = json.loads((directory / 'owned.json').read_text())
                except (OSError, ValueError):
                    continue
                if receipt.get('key') != directory.name or not self.cached(receipt):
                    continue
                if directory.name in pinned:
                    skipped.append(receipt['package'] + '@' + receipt['revision'])
                    continue
                size = sum(p.stat().st_size for p in directory.rglob('*') if p.is_file())
                if not dry_run:
                    shutil.rmtree(directory)
                removed.append(dict(package=receipt['package'], revision=receipt['revision'], bytes=size))
                freed += size
        return dict(dry_run=dry_run, candidates=removed if dry_run else [],
                    removed=[] if dry_run else removed, skipped=skipped, bytes=freed)


class Runtime:
    NETNS_ROOT = Path('/run/netns')

    def __init__(self, config):
        self.config = config
        self.root = Path(config['state_root']) / 'runtimes'
        self.root.mkdir(parents=True, exist_ok=True)
        self.sdk = Path(config['sdk_root'])

    @staticmethod
    def unit(slot):
        return f"subyard-android-slot-{slot['slot_id']}-{slot['generation']}.service"

    @classmethod
    def network_unit(cls, slot):
        return cls.unit(slot).removesuffix('.service') + '-egress.service'

    @classmethod
    def namespace_name(cls, slot):
        name = cls.unit(slot).removesuffix('.service')
        require(re.fullmatch(r'subyard-android-slot-[0-9]{3}-[1-9][0-9]*', name) is not None,
                'ownership', 'invalid Android runtime namespace name')
        return name

    @classmethod
    def namespace_path(cls, slot):
        return cls.NETNS_ROOT / cls.namespace_name(slot)

    @classmethod
    def namespace_directory(cls):
        root = cls.NETNS_ROOT
        if os.path.lexists(root):
            info = os.lstat(root)
            require(stat.S_ISDIR(info.st_mode) and not stat.S_ISLNK(info.st_mode),
                    'ownership', 'Android namespace directory is unsafe')
        return root

    @classmethod
    def create_namespace(cls, slot, directory):
        root, name = cls.namespace_directory(), cls.namespace_name(slot)
        path = root / name
        require(not os.path.lexists(path), 'ownership', 'Android runtime namespace already exists')
        result = run(['ip', 'netns', 'add', name], check=False)
        root = cls.namespace_directory()
        path = root / name
        require(result.returncode == 0 and os.path.lexists(path), 'network',
                'cannot create isolated Android runtime namespace')
        info = os.lstat(path)
        require(stat.S_ISREG(info.st_mode) and not stat.S_ISLNK(info.st_mode),
                'ownership', 'Android runtime namespace path is unsafe')
        inode = cls.namespace_inode(path)
        require(inode != os.stat('/proc/self/ns/net').st_ino, 'network',
                'Android runtime namespace is not isolated from the pool owner')
        atomic(directory / 'network-namespace.json', dict(name=name, inode=inode))
        result = run(['ip', '-n', name, 'link', 'set', 'lo', 'up'], check=False)
        require(result.returncode == 0, 'network', 'cannot enable Android runtime loopback')
        return path

    @classmethod
    def namespace_inode(cls, path):
        try:
            return os.stat(path).st_ino
        except OSError as exc:
            raise PoolError('network', 'cannot inspect Android runtime namespace') from exc

    @classmethod
    def namespace_receipt(cls, slot, directory):
        receipt = Path(directory) / 'network-namespace.json'
        try:
            info = os.lstat(receipt)
            require(stat.S_ISREG(info.st_mode) and not stat.S_ISLNK(info.st_mode),
                    'ownership', 'Android runtime namespace receipt is unsafe')
            value = json.loads(receipt.read_text())
        except (OSError, ValueError, json.JSONDecodeError) as exc:
            raise PoolError('ownership', 'Android runtime namespace receipt is missing or invalid') from exc
        require(isinstance(value, dict) and set(value) == {'name', 'inode'}
                and value['name'] == cls.namespace_name(slot)
                and type(value['inode']) is int and value['inode'] > 0,
                'ownership', 'Android runtime namespace receipt is invalid')
        return value

    @classmethod
    def verify_namespace(cls, slot, pid, path, directory):
        receipt = cls.namespace_receipt(slot, directory)
        expected = cls.namespace_inode(path)
        require(expected == receipt['inode'], 'ownership', 'Android runtime namespace identity changed')
        try:
            actual = os.stat(f'/proc/{pid}/ns/net').st_ino
        except OSError as exc:
            raise PoolError('network', 'cannot inspect Android runtime network namespace') from exc
        require(actual == expected, 'network', 'Android runtime did not join its allocated network namespace')
        for other in cls.namespace_directory().iterdir():
            if other == path or not re.fullmatch(r'subyard-android-slot-[0-9]{3}-[1-9][0-9]*', other.name):
                continue
            info = os.lstat(other)
            require(stat.S_ISREG(info.st_mode) and not stat.S_ISLNK(info.st_mode),
                    'ownership', 'another Android runtime namespace path is unsafe')
            require(cls.namespace_inode(other) != expected, 'network',
                    'Android runtime namespace collides with another slot')

    @classmethod
    def delete_namespace(cls, slot, directory):
        root, name = cls.namespace_directory(), cls.namespace_name(slot)
        path = root / name
        if not os.path.lexists(path):
            return
        receipt = cls.namespace_receipt(slot, directory)
        info = os.lstat(path)
        require(stat.S_ISREG(info.st_mode) and not stat.S_ISLNK(info.st_mode),
                'ownership', 'Android runtime namespace path is unsafe')
        require(cls.namespace_inode(path) == receipt['inode'], 'ownership',
                'Android runtime namespace identity changed')
        result = run(['ip', 'netns', 'delete', name], check=False)
        require(result.returncode == 0 and not os.path.lexists(path), 'stop',
                'Android runtime namespace removal could not be confirmed')

    @staticmethod
    def nameservers():
        for path in ('/etc/resolv.conf', '/run/systemd/resolve/resolv.conf'):
            try:
                addresses = [line.split()[1] for line in Path(path).read_text().splitlines()
                             if line.startswith('nameserver ') and len(line.split()) >= 2]
                addresses = [ipaddress.ip_address(address) for address in addresses]
                addresses = [str(address) for address in addresses if address.version == 4
                             and not (address.is_loopback or address.is_unspecified or address.is_multicast
                                      or address.is_link_local or address.is_reserved)]
                if addresses:
                    return addresses
            except (OSError, ValueError):
                pass
        raise PoolError('preflight', 'Android egress requires a configured non-loopback IPv4 DNS server')

    def preflight(self, request, reserved, slot):
        gpu = self.config.get('gpu', 'host')
        require(gpu in ('host', 'software', 'software-gles'), 'config', 'EMULATOR_GPU must be host, software or software-gles')
        require(platform.machine() == 'x86_64', 'unsupported', 'Android pool currently requires x86_64 KVM')
        require(os.access('/dev/kvm', os.R_OK | os.W_OK), 'preflight', 'KVM is unavailable in this yard')
        require((self.sdk / 'emulator/emulator').is_file(), 'preflight', 'Android emulator is not installed')
        require(shutil.which('slirp4netns') and shutil.which('ip'), 'preflight', 'Android egress tools are not installed')
        self.nameservers()
        account = self.config['runtime_user'] + '-' + slot['slot_id']
        devices = ['/dev/kvm']
        if gpu == 'host':
            render_nodes = sorted(Path('/dev/dri').glob('renderD*'))
            require(render_nodes, 'preflight', 'no graphics render node in this yard')
            devices.append(str(render_nodes[0]))
        for device in devices:
            for permission in ('-r', '-w'):
                result = run(['runuser', '-u', account, '--', 'test', permission, device], check=False)
                require(result.returncode == 0, 'preflight',
                        f'pool runtime account cannot access {device}; converge yard device permissions')
        for binary in ('platform-tools/adb', 'emulator/emulator'):
            result = run(['runuser', '-u', account, '--', 'test', '-x', str(self.sdk / binary)], check=False)
            require(result.returncode == 0, 'preflight',
                    f'pool runtime account cannot execute SDK {binary}; rerun yard provisioning')
        if gpu == 'host':
            probe = Path(tempfile.mkdtemp(prefix='subyard-android-graphics-', dir='/run'))
            try:
                account_info = pwd.getpwnam(account)
                os.chown(probe, account_info.pw_uid, account_info.pw_gid)
                result = run(['timeout', '--kill-after=2', '10', 'runuser', '-u', account, '--', 'env',
                              'HOME=' + str(probe), 'XDG_RUNTIME_DIR=' + str(probe / 'xdg'),
                              'EMULATOR_GPU=host',
                              '/usr/local/lib/subyard-android/runtime.sh', '--check-graphics'],
                             check=False, timeout=15)
                require(result.returncode == 0, 'preflight',
                        'hardware graphics unavailable; converge the render device before allocation')
            except (OSError, subprocess.TimeoutExpired):
                raise PoolError('preflight', 'hardware graphics unavailable; converge the render device before allocation')
            finally:
                shutil.rmtree(probe, ignore_errors=True)

        info = {line.split(':')[0]: int(line.split()[1]) * 1024
                for line in Path('/proc/meminfo').read_text().splitlines() if line.startswith(('MemTotal:', 'MemAvailable:'))}
        budget = lambda item: (PRESETS[item['device']][3] + RUNTIME_OVERHEAD_MIB) * 1024**2
        required = budget(request)
        reserved_bytes = sum(budget(item) for item in reserved)
        require(required + reserved_bytes + 512 * 1024**2 <= info['MemTotal']
                and required <= info['MemAvailable'], 'capacity',
                'insufficient memory for this allocation and existing lease budgets; no slot was preempted')

    def start(self, slot, image, cancel):
        request = slot['request']
        width, height, density, memory = PRESETS[request['device']]
        available = int(next(line.split()[1] for line in Path('/proc/meminfo').read_text().splitlines()
                             if line.startswith('MemAvailable:'))) * 1024
        require(available >= (memory + RUNTIME_OVERHEAD_MIB) * 1024**2, 'capacity', 'insufficient memory for requested emulator')
        # Require initial free-space headroom for every slot; this is not a userdata quota.
        require(shutil.disk_usage(self.root).free > (2 * self.config['size'] + 1) * 1024**3,
                'capacity', 'insufficient space for disposable Android userdata and yard disk headroom')
        directory = self.root / self.unit(slot).removesuffix('.service')
        require(not directory.exists(), 'ownership', 'runtime directory already exists')
        directory.mkdir(mode=0o711)
        account = pwd.getpwnam(self.config['runtime_user'] + '-' + slot['slot_id'])
        home = directory / 'home'
        home.mkdir(mode=0o700)
        resolver = directory / 'resolv.conf'
        resolver.write_text(''.join(f'nameserver {address}\n' for address in self.nameservers()))
        resolver.chmod(0o444)
        base_image = directory / 'base-image'
        base_image.mkdir()
        avds = home / 'avd'
        avds.mkdir()
        avd = avds / 'managed.avd'
        avd.mkdir()
        (avds / 'managed.ini').write_text(f'avd.ini.encoding=UTF-8\npath={avd}\ntarget=android-{request["api"]}\n')
        config = {'AvdId': 'managed', 'avd.ini.displayname': 'Managed disposable emulator',
                  'abi.type': 'x86_64', 'hw.cpu.arch': 'x86_64', 'hw.cpu.ncore': '2',
                  'hw.lcd.width': width, 'hw.lcd.height': height, 'hw.lcd.density': density,
                  'hw.ramSize': memory, 'hw.keyboard': 'yes', 'hw.gpu.enabled': 'yes',
                  'hw.gpu.mode': 'host' if self.config.get('gpu', 'host') == 'host' else 'swiftshader', 'hw.mainKeys': 'no', 'hw.device.name': request['device'],
                  'image.sysdir.1': str(base_image) + '/', 'tag.id': request['variant'],
                  'disk.dataPartition.size': '2G', 'fastboot.forceColdBoot': 'yes',
                  'showDeviceFrame': 'no', 'skin.dynamic': 'yes'}
        (avd / 'config.ini').write_text(''.join(f'{key}={value}\n' for key, value in config.items()))
        for parent, dirs, files in os.walk(home):
            os.chown(parent, account.pw_uid, account.pw_gid)
            for name in files:
                os.chown(Path(parent) / name, account.pw_uid, account.pw_gid)
        require(not cancel.is_set(), 'cancelled', 'allocation cancelled')
        namespace = self.create_namespace(slot, directory)
        run(['systemd-run', '--quiet', '--collect', '--unit', self.unit(slot), '--service-type=exec',
             '--property=User=' + account.pw_name, '--property=KillMode=control-group',
             '--property=NetworkNamespacePath=' + str(namespace), '--property=PrivateTmp=yes', '--property=NoNewPrivileges=yes',
             '--property=ProtectHome=yes', '--property=ProtectSystem=strict',
             '--property=ReadWritePaths=' + str(home),
             '--property=BindReadOnlyPaths=' + str(image) + ':' + str(base_image)
             + ' ' + str(resolver) + ':/etc/resolv.conf',
             '--property=TimeoutStopSec=30',
             '--property=MemoryMax=' + str(memory + RUNTIME_OVERHEAD_MIB) + 'M', '--property=OOMPolicy=kill',
             '--property=Restart=no', '--property=BindsTo=subyard-android-pool.service',
             '--property=After=subyard-android-pool.service',
             '--setenv=HOME=' + str(home), '--setenv=ANDROID_AVD_HOME=' + str(avds),
             '--setenv=SUBYARD_NETWORK_READY=' + str(directory / 'network.ready'),
             '--setenv=ANDROID_HOME=' + str(self.sdk), '--setenv=ANDROID_SDK_ROOT=' + str(self.sdk),
             '--setenv=EMULATOR_GPU=' + self.config.get('gpu', 'host'),
             '/usr/local/lib/subyard-android/runtime.sh'])
        self.start_network(slot, directory, cancel, namespace)
        deadline = time.monotonic() + BOOT_TIMEOUT
        wifi_state = {}
        while time.monotonic() < deadline:
            require(not cancel.is_set(), 'cancelled', 'allocation cancelled')
            pid = self.pid(slot)
            require(pid > 0, 'boot', 'Android runtime exited before readiness')
            # Probe the existing ADB server directly. An adb CLI here could auto-start an
            # unowned root daemon outside the runtime cgroup when the server is not ready yet.
            process = None
            ready = False
            try:
                process = self.connect(slot)
                ready, _ = self.adb_ready(process, wifi_state=wifi_state)
            except (subprocess.SubprocessError, OSError, PoolError):
                pass
            finally:
                if process is not None:
                    with contextlib.suppress(OSError):
                        process.stdin.close()
                    if process.poll() is None:
                        with contextlib.suppress(OSError, ProcessLookupError):
                            process.kill()
                    with contextlib.suppress(OSError, subprocess.SubprocessError):
                        process.wait(timeout=5)
                    with contextlib.suppress(OSError):
                        process.stdout.close()
            if ready:
                return
            cancel.wait(1)
        raise PoolError('boot_timeout', f'Android boot did not complete within {BOOT_TIMEOUT} seconds')

    @staticmethod
    def adb_ready(process, timeout=5, wifi_state=None):
        """Return (ready, connection submitted) through the existing private ADB server."""
        if wifi_state is None:
            wifi_state = {}
        deadline = time.monotonic() + timeout
        buffered = bytearray()

        def read(size):
            while len(buffered) < size:
                remaining = deadline - time.monotonic()
                if remaining <= 0 or not select.select([process.stdout], [], [], remaining)[0]:
                    return None
                data = os.read(process.stdout.fileno(), 4096)
                if not data:
                    return None
                buffered.extend(data)
            result = bytes(buffered[:size])
            del buffered[:size]
            return result

        def request(command):
            payload = command.encode()
            process.stdin.write(f'{len(payload):04x}'.encode() + payload)
            process.stdin.flush()

        request('host:transport:emulator-5554')
        if read(4) != b'OKAY':
            return False, False
        # Android can report boot completion while its initial Wi-Fi setting is still off.
        # Initialize the guest radio explicitly, then wait for local IPv4 configuration.
        # A clean image can retain a disabled default network even after radio enable.
        # Submission is asynchronous, and boot-complete can survive a framework restart.
        # Submit once per observed framework/service lifetime, without interrupting pending DHCP.
        # wifi_on is the desired setting; wait for the radio to finish enabling.
        submitted_for = wifi_state.get('submitted_for', '')
        if not re.fullmatch(r'[1-9][0-9]{0,9}', str(submitted_for)):
            submitted_for = ''
        request('shell:if [ "$(getprop sys.boot_completed)" = 1 ]; then '
                'framework=$(pidof system_server); '
                'case "$framework" in ""|*[!0-9]*|0*) exit 1;; esac; '
                '[ "${#framework}" -le 10 ] || exit 1; '
                'case "$(service check wifi)" in '
                '"Service wifi: not found") printf "%s:missing\\n" "$framework"; exit;; '
                '"Service wifi: found") ;; *) exit 1;; esac; '
                'if [ "$(settings get global wifi_on)" = 0 ]; then svc wifi enable >/dev/null || exit 1; fi; '
                'if ip -4 addr show scope global | grep -q " inet "; then '
                'printf "%s:ready\\n" "$framework"; '
                f'elif [ "$framework" != "{submitted_for}" ] && '
                'cmd wifi status 2>/dev/null | { IFS= read -r radio; [ "$radio" = "Wifi is enabled" ]; } && '
                'cmd wifi connect-network AndroidWifi open >/dev/null 2>&1; then '
                'printf "%s:submitted\\n" "$framework"; '
                'else printf "%s:pending\\n" "$framework"; fi; fi')
        if read(4) != b'OKAY':
            return False, False
        line = bytearray()
        while len(line) < 1024:
            byte = read(1)
            if byte is None:
                return False, False
            line.extend(byte)
            if byte == b'\n':
                match = re.fullmatch(rb'([1-9][0-9]{0,9}):(ready|submitted|pending|missing)', line.strip())
                if not match:
                    return False, False
                framework, state = match.groups()
                if state == b'missing':
                    wifi_state.pop('submitted_for', None)
                elif state == b'submitted':
                    wifi_state['submitted_for'] = framework.decode('ascii')
                return state == b'ready', state == b'submitted'
        return False, False

    def start_network(self, slot, directory, cancel, namespace):
        pid = self.pid(slot)
        require(pid > 0, 'boot', 'Android runtime exited before network setup')
        require(self.unit(slot) in Path(f'/proc/{pid}/cgroup').read_text() and self.pid(slot) == pid,
                'boot', 'runtime identity changed before network setup')
        self.verify_namespace(slot, pid, namespace, directory)
        existing = run(['nsenter', '--net=' + str(namespace), 'ip', 'link', 'show', 'dev', 'tap0'], check=False)
        require(existing.returncode != 0, 'network', 'Android runtime namespace already has tap0')
        # The sibling starts outside the namespace and provides outbound-only traffic through this
        # generation-owned bind-mounted namespace, never the pool owner's descriptor table.
        run(['systemd-run', '--quiet', '--collect', '--unit', self.network_unit(slot),
             '--service-type=exec', '--property=KillMode=control-group',
             '--property=BindsTo=' + self.unit(slot), '--property=After=' + self.unit(slot),
             '--property=PartOf=' + self.unit(slot), '--property=Restart=no',
             '--property=TimeoutStopSec=15', '--property=NoNewPrivileges=yes',
             '--property=ProtectSystem=strict', '--property=ProtectHome=yes', '--property=PrivateTmp=yes',
             '/usr/bin/slirp4netns', '--configure', '--netns-type=path', '--disable-host-loopback',
             '--disable-dns', '--mtu=1500', str(namespace), 'tap0'])
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            require(not cancel.is_set(), 'cancelled', 'allocation cancelled during network setup')
            require(self.pid(slot) == pid, 'boot', 'runtime identity changed during network setup')
            self.verify_namespace(slot, pid, namespace, directory)
            egress = run(['systemctl', 'show', self.network_unit(slot),
                          '--property=ActiveState,MainPID'], check=False)
            route = run(['nsenter', '--net=' + str(namespace), 'ip', '-4', 'route', 'show', 'default'], check=False)
            values = dict(line.split('=', 1) for line in egress.stdout.decode(errors='replace').splitlines()
                          if '=' in line)
            active = (egress.returncode == 0 and values.get('ActiveState') == 'active'
                      and values.get('MainPID', '').isdigit() and int(values['MainPID']) > 0)
            if egress.returncode == 0 and values.get('ActiveState') == 'failed':
                raise PoolError('network', 'Android egress helper exited before readiness')
            if active and b'dev tap0' in route.stdout:
                (directory / 'network.ready').touch()
                return
            cancel.wait(0.1)
        raise PoolError('network', 'Android egress did not become ready')

    def pid(self, slot):
        result = run(['systemctl', 'show', self.unit(slot), '--property=MainPID', '--value'])
        return int(result.stdout.strip() or '0')

    def stop(self, slot):
        unit = self.unit(slot)
        self.stop_unit(self.network_unit(slot))
        self.stop_unit(unit)
        directory = self.root / unit.removesuffix('.service')
        self.delete_namespace(slot, directory)
        if directory.exists():
            require(not directory.is_symlink(), 'ownership', 'runtime ownership is ambiguous')
            shutil.rmtree(directory)

    @staticmethod
    def stop_unit(unit):
        def inspect():
            result = run(['systemctl', 'show', unit,
                          '--property=LoadState,ActiveState,MainPID,ControlGroup'], check=False)
            properties = dict(line.split('=', 1) for line in result.stdout.decode().splitlines() if '=' in line)
            require(result.returncode == 0 or properties.get('LoadState') == 'not-found',
                    'stop', 'cannot inspect runtime unit')
            return properties
        before = inspect()
        group = before.get('ControlGroup') or '/system.slice/' + unit
        if before.get('LoadState') != 'not-found':
            run(['systemctl', 'stop', unit], timeout=45)
            after = inspect()
            # --collect may unload a transient unit immediately after confirmed stop.
            require(after.get('LoadState') == 'not-found' or
                    (after.get('ActiveState') in ('inactive', 'failed') and after.get('MainPID') == '0'),
                    'stop', 'runtime stop could not be confirmed')
        events = Path('/sys/fs/cgroup') / group.lstrip('/') / 'cgroup.events'
        require(not events.exists() or 'populated 1' not in events.read_text(),
                'stop', 'runtime cgroup is still populated')

    def recover(self, slots):
        output = run(['systemctl', 'list-units', '--all', '--plain', '--no-legend',
                      'subyard-android-slot-*.service']).stdout.decode()
        known = {self.unit(slot): slot for slot in slots}
        known.update({self.network_unit(slot): slot for slot in slots})
        for line in output.splitlines():
            unit = line.split()[0]
            require(unit in known, 'ownership', 'unrecorded Android runtime requires operator resolution')
            slot = known[unit]
            if slot['state'] == 'available':
                self.stop(slot)
        expected = {self.unit(slot).removesuffix('.service') for slot in slots}
        require(all(path.name in expected for path in self.root.iterdir()),
                'ownership', 'unrecorded Android data requires operator resolution')

    def connect(self, slot):
        pid = self.pid(slot)
        require(pid > 0, 'unavailable', 'Android runtime is not running')
        directory = self.root / self.unit(slot).removesuffix('.service')
        namespace = self.namespace_path(slot)
        require(self.unit(slot) in Path(f'/proc/{pid}/cgroup').read_text() and self.pid(slot) == pid,
                'unavailable', 'runtime identity changed')
        self.verify_namespace(slot, pid, namespace, directory)
        return subprocess.Popen(['nsenter', '--net=' + str(namespace), 'socat', '-', 'TCP:127.0.0.1:5037'],
                                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)


class Pool:
    def __init__(self, config, runtime=None, images=None, clock=time.time):
        self.config, self.clock = config, clock
        self.root = Path(config['state_root'])
        self.root.mkdir(parents=True, exist_ok=True)
        # /run is cleared on yard reboot; service startup must recreate the maintenance lock directory.
        Path(config.get('sdk_lock', '/run/lock/subyard-android/sdk.lock')).parent.mkdir(
            mode=0o700, parents=True, exist_ok=True)
        self.state_path = self.root / 'pool.json'
        self.lock = threading.RLock()
        self.changed = threading.Condition(self.lock)
        self.closed = False
        self.cancelled = {}
        self.runtime = runtime or Runtime(config)
        self.images = images or Images(self.root / 'images', config['sdk_root'],
                                      config.get('java_home', '/opt/jdk-17'), (2 * config['size'] + 1) * 1024**3)
        self.cancel, self.workers, self.connections, self.sdk_locks = {}, {}, {}, {}
        self.prepare_cancel = threading.Event()
        if self.state_path.exists():
            data = json.loads(self.state_path.read_text())
            require(data.get('schema') == SCHEMA, 'state', 'unsupported Android pool state')
            self.slots = data['slots']
        else:
            self.slots = []
        size = config.get('size', 2)
        require(type(size) is int and 1 <= size <= 32, 'config', 'pool size must be between 1 and 32')
        require(config.get('gpu', 'host') in ('host', 'software', 'software-gles'), 'config',
                'EMULATOR_GPU must be host, software or software-gles')
        require(all(s['state'] == 'available' for s in self.slots[size:]),
                'busy', 'cannot shrink an occupied Android pool')
        self.slots = self.slots[:size]
        while len(self.slots) < size:
            self.slots.append(dict(slot_id=f'{len(self.slots)+1:03}', generation=0, state='available'))
        self.save()

    def save(self):
        atomic(self.state_path, dict(schema=SCHEMA, slots=self.slots))

    def normalize(self, request):
        require(isinstance(request, dict) and set(request) <= {'device', 'api', 'variant', 'abi'},
                'request', 'allocation accepts only device, api, variant and abi')
        defaults = self.config
        resolved = {name: request.get(name, defaults.get(name, value)) for name, value in
                    [('device', 'phone'), ('api', 36), ('variant', 'google_apis'), ('abi', 'x86_64')]}
        require(resolved['device'] in PRESETS, 'unsupported', 'device must be phone or tablet')
        require(type(resolved['api']) is int and resolved['api'] in VERSIONS, 'unsupported', 'supported APIs: 34, 35, 36')
        require(resolved['variant'] in VARIANTS and resolved['abi'] == 'x86_64',
                'unsupported', 'unsupported image variant or ABI')
        return dict(resolved, android_version=VERSIONS[resolved['api']])

    def public(self, slot):
        result = {key: slot[key] for key in ('slot_id', 'generation', 'state')}
        if slot['state'] != 'available':
            for key in ('owner', 'request', 'failure'):
                if key in slot:
                    result[key] = slot[key]
            for key in ('acquired_at', 'ready_at', 'last_heartbeat_at', 'expires_at'):
                result[key] = utc(slot.get(key))
        return result

    def status(self):
        with self.lock:
            return dict(schema=SCHEMA, schema_version=1, resource_type='android-emulator', resource_id='emulator',
                        defaults=self.normalize({}), graphics_mode=self.config.get('gpu', 'host'), heartbeat_seconds=HEARTBEAT,
                        ttl_seconds=TTL, slots=[self.public(s) for s in self.slots])

    def catalog(self):
        images = []
        for variant in sorted(VARIANTS):
            packages = self.images.packages(variant)
            for api, version in VERSIONS.items():
                package = f'system-images;android-{api};{variant};x86_64'
                if package in packages:
                    image = self.images.resolve(dict(api=api, variant=variant, abi='x86_64'))
                    images.append(dict(api=api, android_version=version, variant=variant, abi='x86_64',
                                       revision=image['revision'], cached=self.images.cached(image)))
        return dict(defaults=self.normalize({}), devices=list(PRESETS), images=images)

    @staticmethod
    def digest(token):
        require(isinstance(token, str) and re.fullmatch('[a-f0-9]{64}', token), 'credential', 'invalid lease credential')
        return hashlib.sha256(token.encode()).hexdigest()

    def find(self, token):
        digest = self.digest(token)
        for slot in self.slots:
            if secrets.compare_digest(slot.get('credential', ''), digest):
                return slot
        raise PoolError('stale', 'lease is expired, released or unknown')

    def reserve(self, request, token, owner):
        request = self.normalize(request)
        digest = self.digest(token)
        require(isinstance(owner, dict) and set(owner) == {'yard', 'project', 'run', 'purpose'},
                'owner', 'owner requires yard, project, run and purpose')
        require(all(isinstance(v, str) and re.fullmatch(r'[\w .:/-]{1,100}', v, re.ASCII)
                    for v in owner.values()), 'owner', 'invalid owner attribution')
        with self.lock:
            require(any(s['state'] == 'available' and s['slot_id'] not in self.workers for s in self.slots),
                    'busy', 'all Android emulator slots are occupied')
        # Resolve outside the state lock; no image is removed/downloaded before reservation pins it.
        image = self.images.resolve(request)
        with self.lock:
            self.cancelled = {key: expiry for key, expiry in self.cancelled.items() if expiry > self.clock()}
            require(digest not in self.cancelled, 'cancelled', 'allocation was cancelled before reservation')
            require(not self.closed, 'unavailable', 'Android pool is stopping')
            require(not any(s.get('credential') == digest for s in self.slots), 'credential', 'credential already used')
            slot = next((s for s in self.slots if s['state'] == 'available' and s['slot_id'] not in self.workers), None)
            require(slot is not None, 'busy', 'all Android emulator slots are occupied')
            self.runtime.preflight(request, [s['request'] for s in self.slots if 'request' in s], slot)
            sdk_lock = open(self.config.get('sdk_lock', '/run/lock/subyard-android/sdk.lock'), 'a')
            try:
                fcntl.flock(sdk_lock, fcntl.LOCK_SH | fcntl.LOCK_NB)
            except BlockingIOError:
                sdk_lock.close()
                raise PoolError('busy', 'Android SDK maintenance is in progress')
            self.sdk_locks[slot['slot_id']] = sdk_lock
            slot.update(state='provisioning', generation=slot['generation'] + 1,
                        credential=digest, request=request, image=image, owner=owner,
                        acquired_at=self.clock(), last_heartbeat_at=self.clock(), expires_at=self.clock() + TTL)
            slot.pop('failure', None)
            slot.pop('failure_message', None)
            self.save()
            cancel = self.cancel[slot['slot_id']] = threading.Event()
            worker = threading.Thread(target=self.provision, args=(slot, cancel), daemon=True)
            self.workers[slot['slot_id']] = worker
            worker.start()
            return self.public(slot)

    def provision(self, slot, cancel):
        try:
            image = self.images.ensure(slot['image'], cancel)
            self.runtime.start(slot, image, cancel)
            with self.lock:
                require(slot['state'] == 'provisioning' and not cancel.is_set(), 'cancelled', 'allocation cancelled')
                slot.update(state='held', ready_at=self.clock())
                self.save()
        except Exception as exc:
            report_failure(exc)
            with self.lock:
                slot['failure'] = exc.code if isinstance(exc, PoolError) else 'provisioning_failed'
                slot['failure_message'] = str(exc) if isinstance(exc, PoolError) else 'Android provisioning failed'
                if slot['state'] != 'draining':
                    slot['state'] = 'draining'
                    self.save()
            self.finish_stop(slot)
        finally:
            with self.lock:
                if self.workers.get(slot['slot_id']) is threading.current_thread():
                    self.workers.pop(slot['slot_id'], None)
                self.changed.notify_all()

    def allocation(self, token, renew=False):
        with self.lock:
            slot = self.find(token)
            if slot['state'] in ('draining', 'available') and slot.get('failure'):
                message = slot.get('failure_message', 'Android provisioning failed')
                if slot['state'] == 'available':
                    message += '; runtime was stopped'
                raise PoolError(slot['failure'], message)
            require(slot['state'] in ('provisioning', 'held') and self.clock() < slot['expires_at'],
                    'stale', 'lease is no longer active')
            if renew:
                slot.update(last_heartbeat_at=self.clock(), expires_at=self.clock() + TTL)
                self.save()
            result = self.public(slot)
            if slot['state'] == 'held':
                result.update(android_serial='emulator-5554', image_revision=slot['image']['revision'])
            return result

    def fence(self, slot):
        event = self.cancel.get(slot['slot_id'])
        if event:
            event.set()
        for connection, process in self.connections.get(slot['slot_id'], {}).copy().items():
            with contextlib.suppress(OSError):
                connection.shutdown(socket.SHUT_RDWR)
            with contextlib.suppress(ProcessLookupError):
                process.terminate()
            try:
                process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=3)
        slot['state'] = 'draining'
        self.save()

    def finish_stop(self, slot):
        try:
            self.runtime.stop(slot)
            with self.lock:
                # Keep last credential hash solely for idempotent release until next allocation.
                for key in list(slot):
                    if key not in ('slot_id', 'generation', 'credential', 'failure', 'failure_message'):
                        del slot[key]
                slot['state'] = 'available'
                self.save()
                sdk_lock = self.sdk_locks.pop(slot['slot_id'], None)
                if sdk_lock:
                    sdk_lock.close()
        except Exception:
            with self.lock:
                slot.update(state='quarantined', failure='stop_unconfirmed')
                self.save()

    def stop_slot(self, slot, expected_generation=None):
        with self.lock:
            if expected_generation is not None and slot['generation'] != expected_generation:
                return
            if slot['state'] == 'available':
                return
            generation = slot['generation']
            self.fence(slot)
            worker = self.workers.get(slot['slot_id'])
        if worker:
            worker.join(timeout=360)
            require(not worker.is_alive(), 'quarantined', 'provisioning cancellation is still draining')
        with self.lock:
            # Another release can finish before this waiter returns. Never stop its successor.
            if slot['generation'] != generation:
                return
            if slot['state'] != 'available':
                self.finish_stop(slot)
            require(slot['state'] == 'available', 'quarantined', 'runtime stop is unconfirmed; slot quarantined')

    def release(self, token):
        with self.lock:
            try:
                slot = self.find(token)
            except PoolError as exc:
                if exc.code == 'stale':
                    # A cancellation can overtake a reserve while it resolves repository metadata.
                    self.cancelled[self.digest(token)] = self.clock() + TTL
                    return dict(released=True)
                raise
            generation = slot['generation']
        self.stop_slot(slot, generation)
        return dict(released=True)

    def prepare(self, request):
        require(isinstance(request, dict) and set(request) <= {'api', 'variant', 'abi'},
                'request', 'cache preparation accepts only api, variant and abi')
        request = self.normalize(request)
        with self.lock:
            require(not self.closed and not self.prepare_cancel.is_set(), 'unavailable', 'Android pool is stopping')
            require(all(slot['state'] == 'available' for slot in self.slots), 'busy',
                    'Android cache preparation requires every slot to be idle')
            sdk_lock = open(self.config.get('sdk_lock', '/run/lock/subyard-android/sdk.lock'), 'a')
            try:
                fcntl.flock(sdk_lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                sdk_lock.close()
                raise PoolError('busy', 'Android SDK is in use by an emulator lease')
        try:
            image = self.images.resolve(request)
            self.images.ensure(image, self.prepare_cancel)
            return dict(api=request['api'], variant=request['variant'], abi=request['abi'], revision=image['revision'],
                        cached=True)
        finally:
            sdk_lock.close()

    def reap(self, recovery=False):
        with self.lock:
            victims = [(s, s['generation']) for s in self.slots if s['state'] != 'available'
                       and (recovery or s.get('expires_at', 0) <= self.clock())]
            for slot, _ in victims:
                self.fence(slot)
        for slot, generation in victims:
            with self.lock:
                if slot['generation'] != generation or slot['state'] == 'available':
                    continue
                if slot['slot_id'] in self.workers:
                    continue  # The cancelled worker owns its in-flight launch and stop.
                self.finish_stop(slot)

    def drain(self):
        with self.lock:
            self.closed = True
            slots = list(self.slots)
        try:
            for slot in slots:
                self.stop_slot(slot)
        finally:
            with self.lock:
                self.closed = False

    def shutdown(self):
        with self.lock:
            self.closed = True
            self.prepare_cancel.set()
        for slot in list(self.slots):
            with contextlib.suppress(PoolError):
                self.stop_slot(slot)

    def prune(self, dry_run):
        # Never wait for a download while holding the pool lock: renewals must stay responsive.
        if not self.images.lock.acquire(blocking=False):
            return dict(dry_run=dry_run, candidates=[], removed=[], skipped=['image cache busy'], bytes=0)
        try:
            # ponytail: pool-wide pin/prune transaction; shard only if measured contention matters.
            with self.lock:
                pins = {s['image']['key'] for s in self.slots if 'image' in s}
                return self.images.prune(pins, dry_run)
        finally:
            self.images.lock.release()

    def tunnel(self, token, connection):
        with self.lock:
            slot = self.find(token)
            require(slot['state'] == 'held' and self.clock() < slot['expires_at'], 'stale', 'lease is no longer active')
            process = self.runtime.connect(slot)
            connections = self.connections.setdefault(slot['slot_id'], {})
            connections[connection] = process
        try:
            relay(connection, process)
        finally:
            with self.lock:
                connections.pop(connection, None)


def relay(connection, process):
    def upstream():
        try:
            while data := connection.recv(65536):
                process.stdin.write(data)
                process.stdin.flush()
        except (OSError, ValueError):
            pass
        finally:
            with contextlib.suppress(OSError):
                process.stdin.close()
    thread = threading.Thread(target=upstream, daemon=True)
    thread.start()
    try:
        # Include the initial acknowledgement in the same transport teardown.
        connection.sendall(b'{"ok":true}\n')
        while data := os.read(process.stdout.fileno(), 65536):
            connection.sendall(data)
    except OSError:
        pass
    finally:
        with contextlib.suppress(OSError):
            connection.shutdown(socket.SHUT_RDWR)
        process.terminate()
        try:
            process.wait(timeout=3)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()
        thread.join(timeout=3)
        process.stdout.close()


class Server(socketserver.ThreadingUnixStreamServer):
    daemon_threads = True
    block_on_close = False


class Handler(socketserver.StreamRequestHandler):
    def handle(self):
        self.connection.settimeout(30)
        try:
            raw = self.rfile.readline(16385)
            require(len(raw) <= 16384 and raw.endswith(b'\n'), 'request', 'invalid request size')
            request = json.loads(raw)
            operation = request.get('operation')
            pool = self.server.pool
            if operation == 'tunnel':
                self.connection.settimeout(None)
                pool.tunnel(request.get('token'), self.connection)
                return
            if operation == 'status':
                result = pool.status()
            elif operation == 'catalog':
                result = pool.catalog()
            elif operation == 'reserve':
                result = pool.reserve(request.get('request', {}), request.get('token'), request.get('owner'))
            elif operation in ('allocation', 'renew'):
                result = pool.allocation(request.get('token'), operation == 'renew')
            elif operation == 'release':
                result = pool.release(request.get('token'))
            elif operation == 'prune':
                require(type(request.get('dry_run')) is bool, 'request', 'dry_run must be boolean')
                result = pool.prune(request['dry_run'])
            elif operation == 'prepare':
                result = pool.prepare(request.get('request', {}))
            elif operation in ('revoke', 'drain'):
                _, uid, _ = struct.unpack('3i', self.connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
                require(getattr(self.server, 'admin', False) and uid == 0, 'permission', 'operator authority required')
                if operation == 'drain':
                    pool.drain()
                else:
                    with pool.lock:
                        slots = [s for s in pool.slots if s['slot_id'] == request.get('slot')]
                        require(len(slots) == 1, 'request', 'unknown slot')
                    pool.stop_slot(slots[0])
                result = dict(stopped=True)
            else:
                raise PoolError('request', 'unknown Android pool operation')
            payload = dict(ok=True, result=result)
        except PoolError as exc:
            payload = dict(ok=False, error=exc.code, message=str(exc))
        except Exception as exc:
            report_failure(exc)
            payload = dict(ok=False, error='unavailable', message='Android pool operation failed')
        with contextlib.suppress(OSError):
            self.wfile.write(json.dumps(payload).encode() + b'\n')


def serve():
    require(os.getuid() == 0, 'permission', 'pool owner requires root')
    config = json.loads(Path('/etc/subyard-android.json').read_text())
    root = Path(config['state_root'])
    root.mkdir(mode=0o711, parents=True, exist_ok=True)
    os.chmod(root, 0o711)
    lock = open(root / 'owner.lock', 'a')
    os.chmod(root / 'owner.lock', 0o600)
    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    public = Path(config['public_root'])
    public.mkdir(mode=0o755, parents=True, exist_ok=True)
    address = public / 'control.sock'
    if address.exists():
        require(stat.S_ISSOCK(address.lstat().st_mode), 'ownership', 'pool socket path is occupied')
        address.unlink()
    pool = Pool(config)
    # A broker restart revokes old capabilities and cleans every recorded runtime before serving.
    pool.reap(recovery=True)
    pool.runtime.recover(pool.slots)
    server = Server(str(address), Handler)
    server.pool = pool
    os.chmod(address, 0o666)
    admin_path = root / 'admin.sock'
    admin_path.unlink(missing_ok=True)
    admin_server = Server(str(admin_path), Handler)
    admin_server.pool, admin_server.admin = pool, True
    os.chmod(admin_path, 0o600)
    threading.Thread(target=admin_server.serve_forever, daemon=True).start()
    stopping = threading.Event()
    def stop(_signum, _frame):
        stopping.set()
        pool.prepare_cancel.set()
    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    server.timeout = 1
    try:
        last = 0
        while not stopping.is_set():
            server.handle_request()
            if time.monotonic() - last >= 5:
                pool.reap()
                last = time.monotonic()
    finally:
        pool.shutdown()
        server.server_close()
        admin_server.shutdown()
        admin_server.server_close()
        admin_path.unlink(missing_ok=True)
        address.unlink(missing_ok=True)
        lock.close()


if __name__ == '__main__':
    try:
        serve()
    except (PoolError, OSError, ValueError):
        # Never emit state/credentials or subprocess arguments to the system journal.
        raise SystemExit('Android pool startup failed; check profile convergence and owned runtime state')
