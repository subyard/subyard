#!/usr/bin/python3
"""Fixture-only tracing of original observer returns; never replace its functions."""
import hashlib
import json
import os
from pathlib import Path
import runpy
import stat
import subprocess
import sys


def append_record(value):
    path = os.environ['SUBYARD_TRACE_GITHUB_LOG']
    data = (json.dumps(value, sort_keys=True, separators=(',', ':'))+'\n').encode()
    if len(data) > 65536:
        raise ValueError('oversize public observation')
    fd = os.open(path, os.O_WRONLY | os.O_APPEND | os.O_CREAT | os.O_CLOEXEC | os.O_NOFOLLOW, 0o600)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600:
            raise ValueError('unsafe observation capture')
        if os.write(fd, data) != len(data):
            raise OSError('short observation capture')
    finally:
        os.close(fd)


def public_digest_input(value):
    # These exact schemas are public facts in the owning runtime contract.
    if set(value) == {'schema', 'selected', 'inputs', 'settings'}:
        settings = {'SUBYARD_YARD', 'INCUS_PROJECT', 'YARD_INSTANCE_NAME', 'SUBYARD_HOME',
                    'SUBYARD_CONFIG_HOME', 'SUBYARD_OPERATOR_HOME', 'SUBYARD_KEYS_CONSUMER_ROOT',
                    'SUBYARD_USER', 'DEV_USER', 'SSH_PORT', 'GITHUB_GIT_MODE'}
        if set(value['settings']) != settings:
            raise ValueError('unexpected desired settings schema')
        return 'desired', value
    facts = {'paths', 'selected', 'check_status', 'check_reason', 'check_timeout', 'ready', 'guest_available', 'guest', 'loaded', 'manager'}
    if {'paths', 'selected', 'ready'} <= set(value) <= facts:
        return 'actual-facts', value
    raise ValueError('unexpected observer digest schema')


CORE_FILES = r'''
for path in /usr/local/bin/subyard-preview /etc/subyard/preview.json /usr/local/bin/ccusage /usr/local/bin/ccusage.subyard-runtime.json /var/lib/subyard/ccusage-ownership; do
  printf '%s\n' "$path"
  if [ -e "$path" ] || [ -L "$path" ]; then
    stat -c '%F|%a|%u|%g|%d|%i|%s|%Y|%Z' "$path"
    if [ -f "$path" ] && [ ! -L "$path" ]; then sha256sum "$path"; fi
  else printf 'absent\n'; fi
done
'''


def core_files_hash():
    # Supporting read-only sample, not the complete core-guest Actual digest.
    try:
        result = subprocess.run(['incus', 'exec', os.environ['YARD_INSTANCE_NAME'], '--project',
                                 os.environ['INCUS_PROJECT'], '--', 'sh', '-eu', '-c', CORE_FILES],
                                capture_output=True, timeout=3)
    except (OSError, subprocess.SubprocessError):
        return {'available': False}
    if len(result.stdout) > 16384:
        return {'available': False}
    return {'available': result.returncode == 0, 'exit_code': result.returncode,
            'stdout_sha256': hashlib.sha256(result.stdout).hexdigest()}


def main():
    if len(sys.argv) == 4 and sys.argv[1] == '--verify':
        records = [json.loads(line) for line in Path(sys.argv[2]).read_text().splitlines()]
        groups = {}
        for record in records:
            if record['kind'] == 'capture-error':
                raise ValueError('native observation capture failed')
            groups.setdefault(record['pid'], []).append(record)
        named = 0
        for group in groups.values():
            desired = [item for item in group if item['kind'] == 'desired']
            results = [item for item in group if item['kind'] == 'result']
            if len(desired) != 1 or len(results) != 1:
                raise ValueError('incomplete native observation capture')
            result = results[0]
            if desired[0]['digest'] != result['public']['desired']:
                raise ValueError('captured desired digest differs from native result')
            if result['public']['state'] == 'stale':
                actual = [item for item in group if item['kind'] == 'actual-facts']
                if len(actual) != 1 or actual[0]['digest'] != result['public']['actual']:
                    raise ValueError('captured actual digest differs from native result')
            named += result['yard'] == sys.argv[3]
        if named < 2:
            raise ValueError('missing inspected and rechecked named-yard observations')
        print('release_activation_observation_capture observations='+str(len(groups))+' named='+str(named))
        return
    source = Path(os.environ['SUBYARD_TRACE_GITHUB_SOURCE']).resolve()
    args = sys.argv[1:]
    if len(args) != 2 or args[1] != 'observe' or Path(args[0]).resolve() != source:
        real = os.environ['SUBYARD_TRACE_REAL_PYTHON']
        os.execv(real, [real, *args])
    failed = False

    def trace(frame, event, returned):
        nonlocal failed
        if failed or event != 'return' or frame.f_code.co_name not in ('digest', 'observe') or Path(frame.f_code.co_filename).resolve() != source:
            return
        try:
            record = {'pid': os.getpid(), 'yard': os.environ.get('SUBYARD_YARD', 'default')}
            if frame.f_code.co_name == 'digest':
                kind, public = public_digest_input(frame.f_locals['value'])
                record.update(kind=kind, digest=returned, public=public)
            elif isinstance(returned, tuple) and len(returned) == 2:
                record.update(kind='result', public=returned[0], core_guest_files=core_files_hash())
            else:
                return
            append_record(record)
        except (KeyError, OSError, ValueError, TypeError, subprocess.SubprocessError):
            # Capture failures must not alter the native observer's status/output.
            # The fixture separately requires a complete, validated trace.
            failed = True
            try:
                append_record({'pid': os.getpid(), 'kind': 'capture-error'})
            except (KeyError, OSError, ValueError):
                pass

    # Preserve normal script argv, import path and original __file__/SOURCE. The
    # callback observes returns, leaving functions and digest inputs untouched.
    sys.argv = args
    sys.path[0] = str(Path(args[0]).parent)
    previous = sys.getprofile()
    sys.setprofile(trace)
    try:
        runpy.run_path(args[0], run_name='__main__')
    finally:
        sys.setprofile(previous)


if __name__ == '__main__':
    main()
