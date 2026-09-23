#!/bin/sh
# Explicit operator cleanup; this does not establish integration ownership.
set -eu
exec python3 -B - "$@" <<'PY'
import ctypes
import fcntl
import hashlib
import json
import os
import re
import stat
import subprocess
import sys

SYSTEM_DIR = '/etc/systemd/system'
RECEIPT = '/var/lib/subyard/paseo-ownership'
SAFE_ROOT = '/'
OWNER_UID = 0
UNIT = SYSTEM_DIR + '/paseo.service'
BACKUP = UNIT + '.subyard-retired'


def fail(code, message):
    print(json.dumps({'code': code, 'message': message}))
    sys.exit(1)


def metadata(info):
    return [info.st_dev, info.st_ino, info.st_mode, info.st_uid,
            info.st_gid, info.st_size, info.st_mtime_ns, info.st_ctime_ns]


def parents(path):
    result = []
    current = os.path.dirname(path)
    while True:
        try:
            info = os.lstat(current)
        except FileNotFoundError:
            result.append(None)
        else:
            if (not stat.S_ISDIR(info.st_mode) or info.st_uid != OWNER_UID
                    or info.st_mode & 0o022):
                fail('cleanup_unsafe_path', 'Paseo cleanup requires a protected non-symlink directory: ' + current)
            result.append([info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_gid])
        if current == SAFE_ROOT:
            return result
        parent = os.path.dirname(current)
        if parent == current:
            fail('cleanup_unsafe_path', 'Paseo cleanup path is outside its protected root.')
        current = parent


def file_state(path):
    parent_state = parents(path)
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        return {'parents': parent_state, 'file': None}
    if (not stat.S_ISREG(info.st_mode) or info.st_uid != OWNER_UID
            or info.st_mode & 0o022 or info.st_nlink != 1):
        fail('cleanup_unsafe_file', 'Paseo cleanup requires a protected regular file without links: ' + path)
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, 'rb') as stream:
        before = metadata(os.fstat(stream.fileno()))
        if before != metadata(info):
            fail('cleanup_stale', 'Paseo cleanup state changed; inspect a fresh plan.')
        digest = hashlib.file_digest(stream, 'sha256').hexdigest()
        if metadata(os.fstat(stream.fileno())) != before:
            fail('cleanup_stale', 'Paseo cleanup state changed; inspect a fresh plan.')
    return {'parents': parent_state, 'file': before, 'sha256': digest}


def systemctl(*args):
    result = subprocess.run(['systemctl', *args], stdin=subprocess.DEVNULL,
                            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                            timeout=30, check=False)
    if result.returncode:
        fail('cleanup_service_failed', 'Paseo cleanup failed at systemctl ' + ' '.join(args) + '; inspect service state before retrying.')
    return result.stdout.decode('utf-8', errors='strict')


def observe():
    files = {name: file_state(path) for name, path in
             [('unit', UNIT), ('backup', BACKUP), ('receipt', RECEIPT)]}
    live = files['unit']['file'] is not None
    backup = files['backup']['file'] is not None
    if live and backup:
        fail('cleanup_backup_exists', 'Paseo service and retirement backup both exist; resolve the backup before cleanup.')
    service = {}
    steps = []
    if live or backup:
        output = systemctl('show', '--property=ActiveState,UnitFileState,FragmentPath,NeedDaemonReload,LoadState', 'paseo.service')
        for line in output.splitlines():
            key, separator, value = line.partition('=')
            if not separator or key in service:
                fail('cleanup_service_unknown', 'Paseo service state could not be verified.')
            service[key] = value
        if (set(service) != {'ActiveState', 'UnitFileState', 'FragmentPath', 'NeedDaemonReload', 'LoadState'}
                or service['ActiveState'] not in {'active', 'inactive', 'failed', 'activating', 'deactivating', 'reloading', 'maintenance', 'refreshing'}
                or service['UnitFileState'] not in {'', 'enabled', 'enabled-runtime', 'linked', 'linked-runtime', 'alias', 'masked', 'masked-runtime', 'static', 'disabled', 'indirect', 'generated', 'transient', 'bad'}
                or service['NeedDaemonReload'] not in {'yes', 'no'}):
            fail('cleanup_service_unknown', 'Paseo service state could not be verified.')
        if live:
            if service['FragmentPath'] not in {'', UNIT}:
                fail('cleanup_service_conflict', 'Paseo service resolves to a different unit; cleanup refused.')
            steps = [
                'Stop and disable paseo.service (active=' + service['ActiveState'] +
                ', enabled=' + (service['UnitFileState'] or 'unknown') + ').',
                'Preserve /etc/systemd/system/paseo.service as /etc/systemd/system/paseo.service.subyard-retired without overwriting a backup.',
                'Reload systemd; preserve Paseo binaries, configuration, authentication and history.',
            ]
        elif service['NeedDaemonReload'] == 'yes':
            steps = ['Finish interrupted Paseo cleanup by reloading systemd; preserve the retirement backup and all Paseo data.']
    state = {'files': files, 'service': service, 'developer': sys.argv[2:4]}
    fingerprint = hashlib.sha256(json.dumps(state, sort_keys=True, separators=(',', ':')).encode()).hexdigest()
    return state, {'fingerprint': fingerprint, 'changed': bool(steps), 'steps': steps}


def main():
    if (len(sys.argv) != 5 or sys.argv[1] not in {'observe', 'apply'}
            or not re.fullmatch(r'[A-Za-z0-9_][A-Za-z0-9_.-]*', sys.argv[2])
            or not re.fullmatch(r'[0-9]+', sys.argv[3])
            or (sys.argv[1] == 'apply' and not re.fullmatch(r'[0-9a-f]{64}', sys.argv[4]))
            or (sys.argv[1] == 'observe' and sys.argv[4])):
        fail('cleanup_arguments_invalid', 'Invalid Paseo cleanup request.')
    parents(UNIT)
    # The existing directory is a lock anchor, so observation creates no files.
    directory = os.open(SYSTEM_DIR, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        fcntl.flock(directory, fcntl.LOCK_EX if sys.argv[1] == 'apply' else fcntl.LOCK_SH)
        state, result = observe()
        if sys.argv[1] == 'apply':
            if result['fingerprint'] != sys.argv[4]:
                fail('cleanup_stale', 'Paseo cleanup state changed; inspect a fresh plan.')
            if state['files']['unit']['file'] is not None:
                systemctl('stop', 'paseo.service')
                systemctl('disable', 'paseo.service')
                # Service changes must not authorize changed files or a new backup.
                for name, path in [('unit', UNIT), ('backup', BACKUP), ('receipt', RECEIPT)]:
                    if file_state(path) != state['files'][name]:
                        fail('cleanup_stale', 'Paseo cleanup state changed; inspect a fresh plan.')
                # Linux renameat2 is atomic and refuses an existing destination.
                libc = ctypes.CDLL(None, use_errno=True)
                if libc.renameat2(directory, b'paseo.service', directory,
                                  b'paseo.service.subyard-retired', 1):
                    fail('cleanup_backup_failed', 'Paseo unit backup could not be created; the original unit is preserved.')
                os.fsync(directory)
            if result['changed']:
                systemctl('daemon-reload')
            _, result = observe()
        print(json.dumps(result))
    finally:
        os.close(directory)


try:
    main()
except (OSError, ValueError, UnicodeError, subprocess.SubprocessError, AttributeError):
    fail('cleanup_failed', 'Paseo cleanup could not complete safely; inspect a fresh plan before retrying.')
PY
