#!/usr/bin/env python3
"""Generate the owning native-loss pause wrapper."""
from pathlib import Path
import os
import shlex
import sys


def write_wrapper(case_root, instance, *, native_executable="/usr/bin/incus"):
    if not Path(native_executable).is_absolute():
        raise ValueError('native executable must be absolute')
    case = os.fspath(case_root)
    script='#!/bin/bash\nset -euo pipefail\ncase='+shlex.quote(case)+'\ninstance='+shlex.quote(instance)+'\n'+r'''
if [ "${1:-}" = file ] && [ "${2:-}" = push ] && [[ "${4:-}" = "$instance/tmp/subyard-orca."* ]]; then
  printf 'registration-push\n' >> "$case/calls"
  @NATIVE_EXECUTABLE@ "$@"
  if [ ! -e "$case/paused" ]; then
    printf '%s\n' "$$" > "$case/paused"
    while [ ! -e "$case/release" ]; do sleep .05; done
  fi
  exit 0
fi
exec @NATIVE_EXECUTABLE@ "$@"
'''
    script = script.replace('@NATIVE_EXECUTABLE@', shlex.quote(os.fspath(native_executable)))
    path=Path(case)/'bin/incus';path.write_text(script);path.chmod(0o700)
    return path


if __name__ == "__main__":
    write_wrapper(*sys.argv[1:])
