"""Retain the one fixed native account OFD through unprivileged child transports."""
import hashlib
import os
from pathlib import Path
import stat


def inherited_owner_descriptors():
    enabled = os.environ.get('SUBYARD_NATIVE_OWNER_FD3', '')
    if enabled == '':
        return ()
    if enabled != '1':
        raise ValueError('invalid native owner barrier flag')
    home = os.environ.get('SUBYARD_HOME', '')
    if not home.startswith('/') or os.path.normpath(home) != home:
        raise ValueError('invalid native owner scope')
    uid = os.getuid()
    parent = Path('/tmp')/('subyard-owner-locks-'+str(uid))
    parent_info = parent.lstat()
    if not stat.S_ISDIR(parent_info.st_mode) or parent_info.st_uid != uid or stat.S_IMODE(parent_info.st_mode) != 0o700:
        raise ValueError('unsafe native owner barrier parent')
    named = (parent/(hashlib.sha256(home.encode()).hexdigest()+'.lock')).lstat()
    actual = os.fstat(3)
    if not stat.S_ISREG(actual.st_mode) or actual.st_uid != uid or actual.st_nlink != 1 or stat.S_IMODE(actual.st_mode) != 0o600 or (named.st_dev, named.st_ino, named.st_mode, named.st_uid, named.st_nlink) != (actual.st_dev, actual.st_ino, actual.st_mode, actual.st_uid, actual.st_nlink):
        raise ValueError('native owner barrier descriptor changed')
    return (3,)
