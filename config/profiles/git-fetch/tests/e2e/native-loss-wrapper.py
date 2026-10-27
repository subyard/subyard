#!/usr/bin/env python3
"""Generate the owning native-loss pause wrapper."""
from pathlib import Path
import os
import shlex
import sys


def write_wrapper(case_root, guest_root, instance, project, *, native_executable="/usr/bin/incus"):
    if not Path(native_executable).is_absolute():
        raise ValueError('native executable must be absolute')
    case = os.fspath(case_root)
    guest = os.fspath(guest_root)
    script='#!/bin/bash\nset -euo pipefail\n'+''.join(key+'='+shlex.quote(value)+'\n' for key,value in
    [('case',case),('guest',guest),('instance',instance),('project',project)])+r'''
refresh=0
for argument in "$@"; do [ "$argument" != custody-refresh ] || refresh=1; done
if [ "$refresh" = 1 ]; then
  printf 'custody-refresh\n' >> "$case/calls"
  if [ ! -e "$case/paused" ]; then
    arguments=("$@")
    for index in "${!arguments[@]}"; do
      if [ "${arguments[$index]}" = -- ]; then
        arguments=("${arguments[@]:0:$index}" --env "BASH_ENV=$guest/environment" "${arguments[@]:$index}")
        break
      fi
    done
    @NATIVE_EXECUTABLE@ "${arguments[@]}" <&0 & client=$!
    while ! @NATIVE_EXECUTABLE@ exec "$instance" --project "$project" -- test -f "$guest/paused" </dev/null; do sleep .05; done
    printf '%s\n' "$$" > "$case/paused"
    while [ ! -e "$case/release" ]; do sleep .05; done
    @NATIVE_EXECUTABLE@ exec "$instance" --project "$project" -- touch "$guest/release" </dev/null
    wait "$client"
    exit "$?"
  fi
fi
exec @NATIVE_EXECUTABLE@ "$@"
'''
    script = script.replace('@NATIVE_EXECUTABLE@', shlex.quote(os.fspath(native_executable)))
    path=Path(case)/'bin/incus';path.write_text(script);path.chmod(0o700)
    return path


if __name__ == "__main__":
    write_wrapper(*sys.argv[1:])
