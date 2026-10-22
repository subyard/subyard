#!/usr/bin/env python3
"""Strict artifact contract for disposable Veranda platform preparation."""
import hashlib
import contextlib
import json
import os
from pathlib import Path
import re
import stat
import struct
import socket
import ssl
import tarfile


def digest(path):
    with path.open('rb') as source:
        return hashlib.file_digest(source, 'sha256').hexdigest()


def engine_arch(engine):
    with engine.open('rb') as source:
        header = source.read(64)
    assert header[:4] == b'\x7fELF' and header[5] == 1, 'prepared engine is not a little-endian ELF'
    return {62: ('amd64', 'x86_64'), 183: ('arm64', 'aarch64')}[struct.unpack('<H', header[18:20])[0]]


def plain(path, directory=False):
    info = path.lstat()
    assert (stat.S_ISDIR(info.st_mode) if directory else stat.S_ISREG(info.st_mode)), 'prepared asset type'
    assert info.st_uid == os.getuid(), 'prepared asset owner'
    assert stat.S_IMODE(info.st_mode) == (0o700 if directory else 0o600), 'prepared asset mode'
    assert directory or info.st_nlink == 1, 'prepared asset links'


def validate(directory, engine, version, root):
    directory = Path(directory)
    assert directory.resolve() == directory, 'prepared asset directory must be canonical'
    plain(directory, True)
    plain(directory / 'prepared-assets.json')
    data = json.loads((directory / 'prepared-assets.json').read_text())
    assert data['schemaVersion'] == 1, 'prepared asset schema'
    arch, image_arch = engine_arch(engine)
    assert data['engine'] == {'version': version, 'sha256': digest(engine), 'architecture': arch}, 'prepared engine binding'
    assert re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._+-]*', version), 'prepared version'
    bundle = 'runtime/subyard-' + version + '-linux-' + arch + '.tar.gz'
    assert data['runtime'] == {'bundle': bundle}, 'prepared runtime selection'
    image = data['image']
    assert image['source'] == 'images:debian/13' and image['type'] == 'container', 'prepared image source/type'
    assert image['architecture'] == image_arch and re.fullmatch(r'[a-f0-9]{64}', image['fingerprint']), 'prepared image identity'
    assert image['parts'] in (['image/metadata.tar.xz'], ['image/metadata.tar.xz', 'image/rootfs']), 'prepared image parts'
    expected = {bundle, *(bundle + suffix for suffix in ('.sha256', '.manifest.json', '.provenance.json')), *image['parts']}
    files = data['files']
    assert len(files) == len(expected) and {entry['path'] for entry in files} == expected, 'prepared file allowlist'
    for entry in files:
        path = directory / entry['path']
        plain(path)
        assert entry['mode'] == 0o600 and path.stat().st_size == entry['bytes'], 'prepared file metadata'
        assert digest(path) == entry['sha256'], 'prepared asset digest'
    actual = set()
    for path in directory.rglob('*'):
        if path.is_dir():
            assert path.relative_to(directory).as_posix() in ('runtime', 'image'), 'prepared directory allowlist'
            plain(path, True)
        else:
            actual.add(path.relative_to(directory).as_posix())
    assert actual == expected | {'prepared-assets.json'}, 'unexpected prepared asset'
    archive = directory / bundle
    assert (directory / (bundle + '.sha256')).read_text().strip() == digest(archive) + '  ' + archive.name, 'prepared runtime checksum'
    metadata = json.loads((directory / (bundle + '.manifest.json')).read_text())
    assert (metadata['version'], metadata['kind'], metadata['os'], metadata['arch']) == (version, 'runtime', 'linux', arch), 'prepared runtime manifest'
    provenance = json.loads((directory / (bundle + '.provenance.json')).read_text())
    assert (provenance['version'], provenance['artifact'], provenance['sha256']) == (version, archive.name, digest(archive)), 'prepared runtime provenance'
    with tarfile.open(archive) as runtime:
        members = runtime.getmembers()
        assert all(not member.name.startswith('/') and '..' not in Path(member.name).parts
                   and (member.isfile() or member.isdir()) for member in members), 'prepared runtime archive type/path'
        names = [member.name.removeprefix('./') for member in members]
        assert len(names) == len(set(names)), 'prepared runtime duplicate entry'
        def member_digest(name):
            member = members[names.index(name)]
            assert member.isfile() and member.mode == 0o755, 'prepared runtime executable mode'
            with runtime.extractfile(member) as source:
                return hashlib.file_digest(source, 'sha256').hexdigest()
        assert member_digest('bin/yard-engine') == data['engine']['sha256'], 'prepared bundled engine'
        seen = set()
        for helper in data['helpers']:
            name = helper['path']
            assert re.fullmatch(r'\.build/profiles/[A-Za-z0-9_-]+/bin/[A-Za-z0-9_-]+', name) and name not in seen, 'prepared helper path'
            seen.add(name)
            path = root / name
            info = path.lstat()
            assert stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid() and info.st_nlink == 1 and stat.S_IMODE(info.st_mode) == 0o755, 'prepared helper mode/type'
            assert digest(path) == helper['sha256'] == member_digest(name.replace('.build/profiles/', 'config/profiles/', 1)), 'prepared helper binding'
        staged_helpers = {path.relative_to(root).as_posix() for path in (root / '.build/profiles').glob('*/bin/*')}
        assert seen == staged_helpers, 'prepared helper inventory'
    return data


def verify_image(rows, image):
    matches = [row for row in rows if row['fingerprint'] == image['fingerprint']]
    assert len(matches) == 1, 'prepared image fingerprint unavailable/ambiguous'
    assert matches[0]['type'] == image['type'] and matches[0]['architecture'] == image['architecture'], 'prepared image type/architecture'
    return matches[0]


@contextlib.contextmanager
def image_remote(directory, data, state, env, command):
    """Expose only our imported image over the allocated daemon's loopback TLS API.

    command(argv, timeout) is the owning guest's bounded, privately logged Incus
    invocation. The product still creates and owns its own isolated project.
    """
    token = state.name.removeprefix('veranda-wayland-').replace('_', '-')
    remote = 'veranda-assets-' + token + '-image'
    alias = remote
    image = data['image']; fingerprint = image['fingerprint']
    def call(*argv, timeout=30):
        return command(['incus', *argv], timeout)
    def rows():
        return json.loads(call('image', 'list', '--project', 'default', '--format=json'))
    baseline_remotes = json.loads(call('remote', 'list', '--format=json'))
    baseline_default = call('remote', 'get-default').strip()
    local = baseline_remotes[baseline_default]
    assert local['Addr'] in ('unix://', 'unix:///var/lib/incus/unix.socket') and not local['Public'], 'prepared default must be the guest Unix socket'
    # Native compatibility is checked before changing the daemon/client.
    version = call('version')
    assert 'Client version:' in version and 'Server version:' in version, 'prepared Incus version unavailable'
    assert '--public' in call('image', 'import', '--help'), 'prepared image import unsupported'
    remote_help = call('remote', 'add', '--help')
    assert '--public' in remote_help and '--protocol' in remote_help, 'prepared public remote unsupported'
    before = rows()
    assert not any(row['fingerprint'] == fingerprint for row in before), 'prepared image already exists; ownership unavailable'
    assert remote not in baseline_remotes, 'prepared remote already exists'
    assert not any(item['name'] == alias for row in before for item in (row.get('aliases') or [])), 'prepared alias already exists'
    assert call('config', 'get', 'core.https_address').strip() == '', 'prepared listener already configured'
    certificate = json.loads(call('query', '/1.0'))['environment']['certificate']
    expected_der = ssl.PEM_cert_to_DER_cert(certificate)
    # The native engine forwards operator HOME, not arbitrary INCUS_CONF. Use only
    # one exact fixture remote in that operator's normal client configuration.
    config = Path(env['HOME']) / '.config/incus'
    certificates = config / 'servercerts'
    for path in (config.parent, config, certificates):
        if not path.exists(): path.mkdir(mode=0o700)
        info = path.lstat()
        assert stat.S_ISDIR(info.st_mode) and info.st_uid == os.getuid(), 'prepared client directory ownership/type'
    pin = certificates / (remote + '.crt')
    client_config = config / 'config.yml'
    config_mode = None
    if client_config.exists() or client_config.is_symlink():
        info = client_config.lstat()
        assert stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid() and info.st_nlink == 1, 'prepared client config ownership/type'
        config_mode = stat.S_IMODE(info.st_mode)
    pin_identity = None
    listener = None
    def cleanup_pin():
        if pin_identity is not None and os.path.lexists(pin):
            plain(pin)
            assert (pin.stat().st_dev, pin.stat().st_ino) == pin_identity and pin.read_text() == certificate, 'prepared pin cleanup identity'
            pin.unlink()
    def cleanup_image():
        current = [row for row in rows() if row['fingerprint'] == fingerprint]
        if current:
            current = verify_image(current, image)
            assert current['public'] is True and {item['name'] for item in current['aliases']} == {alias}, 'prepared image cleanup ownership'
            call('image', 'delete', fingerprint, '--project', 'default')
            assert not any(row['fingerprint'] == fingerprint for row in rows()), 'prepared image cleanup incomplete'
    def cleanup_listener():
        current = call('config', 'get', 'core.https_address').strip()
        if current:
            assert current == listener, 'prepared listener cleanup identity'
            call('config', 'unset', 'core.https_address')
            assert call('config', 'get', 'core.https_address').strip() == '', 'prepared listener cleanup incomplete'
    def cleanup_remote():
        current = json.loads(call('remote', 'list', '--format=json')).get(remote)
        if current is not None:
            plain(pin)
            assert pin.read_text() == certificate, 'prepared remote cleanup pin'
            assert current['Addr'] == 'https://' + listener and current['Public'] is True, 'prepared remote cleanup identity'
            call('remote', 'remove', remote)
            assert json.loads(call('remote', 'list', '--format=json')) == baseline_remotes, 'prepared unrelated remotes changed'
            assert call('remote', 'get-default').strip() == baseline_default, 'prepared default remote changed'
            if config_mode is not None:
                info = client_config.lstat()
                assert stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid() and stat.S_IMODE(info.st_mode) == config_mode, 'prepared client config mode changed'
    cleanup = contextlib.ExitStack()
    original_failed = False
    try:
        cleanup.callback(cleanup_pin)
        fd = os.open(pin, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        pin_identity = (os.fstat(fd).st_dev, os.fstat(fd).st_ino)
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, 'w') as target: target.write(certificate)
        cleanup.callback(cleanup_image)
        call('image', 'import', *(str(Path(directory) / part) for part in image['parts']),
             '--alias', alias, '--public', '--project', 'default', timeout=180)
        actual = verify_image(rows(), image)
        assert actual['public'] is True and {item['name'] for item in actual['aliases']} == {alias}, 'prepared imported image ownership/publication'
        with socket.socket() as reservation:
            reservation.bind(('127.0.0.1', 0)); port = reservation.getsockname()[1]
        listener = '127.0.0.1:' + str(port)
        cleanup.callback(cleanup_listener)
        call('config', 'set', 'core.https_address', listener)
        assert call('config', 'get', 'core.https_address').strip() == listener, 'prepared listener binding'
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        context.check_hostname = False
        context.verify_mode = ssl.CERT_NONE  # Exact API-derived DER pin is checked below.
        with socket.create_connection(('127.0.0.1', port), timeout=5) as connection:
            with context.wrap_socket(connection, server_hostname='localhost') as tls:
                assert tls.getpeercert(binary_form=True) == expected_der, 'prepared TLS identity'
        cleanup.callback(cleanup_remote)
        call('remote', 'add', remote, 'https://' + listener, '--public', '--protocol=incus')
        plain(pin)
        assert pin.read_text() == certificate, 'prepared remote TLS pin changed'
        configured = json.loads(call('remote', 'list', '--format=json'))[remote]
        assert configured['Addr'] == 'https://' + listener and configured['Public'] is True, 'prepared remote identity'
        visible = json.loads(call('image', 'list', remote + ':', '--project', 'default', '--format=json'))
        verify_image(visible, image)
        yield remote + ':' + fingerprint
    except BaseException as error:
        original_failed = True
        detail = str(error) if isinstance(error, AssertionError) else None
        print('VERANDA_PREPARED_IMAGE_FAILURE ' + json.dumps({'error_type': type(error).__name__, 'detail': detail}), flush=True)
        raise
    finally:
        try:
            cleanup.close()
        except BaseException as error:
            print('VERANDA_PREPARED_IMAGE_CLEANUP ' + json.dumps({'status': 'failed', 'error_type': type(error).__name__}), flush=True)
            if not original_failed: raise
        else:
            print('VERANDA_PREPARED_IMAGE_CLEANUP {"status":"passed"}', flush=True)
