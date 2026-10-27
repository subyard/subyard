#!/usr/bin/env python3
"""Generate the owning native-loss pause wrapper."""
from pathlib import Path
import os
import shlex
import sys


def write_wrapper(case_root, unit, *, native_executable="/usr/bin/systemctl"):
    if not Path(native_executable).is_absolute():
        raise ValueError('native executable must be absolute')
    case = os.fspath(case_root)
    script='#!/bin/bash\nset -euo pipefail\ncase='+shlex.quote(case)+'\nunit='+shlex.quote(unit)+'\n'+r'''
if [ "${1:-}" = --user ] && [ "${2:-}" = restart ] && [ "${3:-}" = "$unit" ]; then
  printf 'restart\n' >> "$case/calls"
  printf '%s\n' "$$" > "$case/paused"
  while [ ! -e "$case/release" ]; do sleep .05; done
fi
exec @NATIVE_EXECUTABLE@ "$@"
'''
    script = script.replace('@NATIVE_EXECUTABLE@', shlex.quote(os.fspath(native_executable)))
    path=Path(case)/'bin/systemctl';path.write_text(script);path.chmod(0o700)
    return path


if __name__ == "__main__":
    write_wrapper(*sys.argv[1:])
