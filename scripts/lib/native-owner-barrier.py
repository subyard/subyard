"""Retain the one fixed native account OFD through fixed child transports."""
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
    validate_owner_descriptor(3, os.getuid(), os.environ.get('SUBYARD_HOME', ''))
    return (3,)


def validate_owner_descriptor(fd, uid, home):
    if not home.startswith('/') or os.path.normpath(home) != home:
        raise ValueError('invalid native owner scope')
    parent = Path('/tmp')/('subyard-owner-locks-'+str(uid))
    parent_info = parent.lstat()
    if not stat.S_ISDIR(parent_info.st_mode) or parent_info.st_uid != uid or stat.S_IMODE(parent_info.st_mode) != 0o700:
        raise ValueError('unsafe native owner barrier parent')
    named = (parent/(hashlib.sha256(os.fsencode(home)).hexdigest()+'.lock')).lstat()
    actual = os.fstat(fd)
    if not stat.S_ISREG(actual.st_mode) or actual.st_uid != uid or actual.st_nlink != 1 or stat.S_IMODE(actual.st_mode) != 0o600 or (named.st_dev, named.st_ino, named.st_mode, named.st_uid, named.st_nlink) != (actual.st_dev, actual.st_ino, actual.st_mode, actual.st_uid, actual.st_nlink):
        raise ValueError('native owner barrier descriptor changed')


if __name__ == '__main__':
    import ipaddress
    import sys
    action, uid, home, binary, *arguments = sys.argv[1:]
    if action != 'conntrack' or not uid.isdecimal() or str(int(uid)) != uid:
        raise ValueError('invalid native conntrack owner')
    validate_owner_descriptor(0, int(uid), home)
    if (not os.path.isabs(binary) or os.path.basename(binary) != 'conntrack' or
            len(arguments) != 11 or arguments[0] not in ('-L', '-D') or
            arguments[1:6] != ['-f', 'ipv4', '-p', 'udp', '--orig-dst'] or
            arguments[7] != '--orig-port-dst' or arguments[9] != '--reply-src' or
            arguments[6] != arguments[10]):
        raise ValueError('invalid native conntrack tuple')
    address = ipaddress.IPv4Address(arguments[6])
    port = int(arguments[8])
    if str(address) != arguments[6] or not 0 < port <= 65535 or str(port) != arguments[8]:
        raise ValueError('invalid native conntrack endpoint')
    # stdin is the original OFD after sudo, and remains open across exec.
    os.execv(binary, [binary, *arguments])
