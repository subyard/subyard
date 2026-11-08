"""Read-only classification of Orca's native Claude installation."""
import json
import re
import stat
import sys
from pathlib import Path


def installed(home, uid, data=Path('/srv/agents/orca/data')):
    versions = data / 'claude/versions'
    # Do not execute an unselected program or follow an arbitrary native link.
    try:
        for directory in (data, data / 'claude', versions):
            info = directory.lstat()
            if not stat.S_ISDIR(info.st_mode) or info.st_uid != uid or info.st_mode & 0o022:
                return False
        for path in versions.iterdir():
            if not re.fullmatch(r'\d+\.\d+\.\d+', path.name):
                continue
            info = path.lstat()
            if (stat.S_ISREG(info.st_mode) and info.st_uid == uid
                    and info.st_mode & 0o111 and not info.st_mode & 0o022):
                return True
    except OSError:
        pass
    return False


if __name__ == '__main__':
    home, uid = Path(sys.argv[1]), int(sys.argv[2])
    print(json.dumps({'state': 'unmanaged' if installed(home, uid) else 'absent'}))
