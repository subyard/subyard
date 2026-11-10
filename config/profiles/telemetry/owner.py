#!/usr/bin/env python3
"""Existing assessed owner-to-guest transport; no host collector or dashboard."""
import io
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tarfile

FILES = ('guest.py', 'native.py', 'launcher.py', 'processes.py', 'records.py', 'forward.py',
         'resources.py', 'sources.py', 'broker-correlation.py')
GUEST_SCRIPT = 'stage="$(mktemp -d /tmp/subyard-telemetry.XXXXXX)"; trap \'rm -rf -- "$stage"\' EXIT; tar -xf - -C "$stage"; python3 -B "$stage/guest.py" "$@"'
SAFE_GUEST_FAILURE = re.compile(r'telemetry: local reconciliation failed \((?:OSError|PermissionError|FileNotFoundError|FileExistsError|NotADirectoryError|IsADirectoryError|BlockingIOError|TimeoutError|ValueError|KeyError|TimeoutExpired|CalledProcessError|SubprocessError|UnicodeDecodeError|JSONDecodeError) at (?:guest|native)\.py:[1-9][0-9]{0,4}\)\Z')


def run(arguments, **kwargs):
    result = subprocess.run(arguments, stdin=subprocess.DEVNULL if 'input' not in kwargs else None,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=90, **kwargs)
    if result.returncode:
        for line in result.stderr[-65536:].decode(errors='replace').splitlines()[-32:]:
            if SAFE_GUEST_FAILURE.fullmatch(line):
                print(line, file=sys.stderr)
                break
        raise ValueError('telemetry owner transport failed')
    return result.stdout.decode().strip()


class Refusal(ValueError):
    """Every independent refusal found before any effect, named for the operator."""
    def __init__(self, refusals):
        super().__init__('; '.join(refusals))


def collector_binary(source, selected):
    """Return the collector binary a selected run installs and every source refusal.

    An unselected run needs no binary contents; a symlinked candidate is unsafe
    whether or not the profile is selected."""
    packaged = source/'bin/subyard-telemetry'
    built = source.parents[2]/'.build/profiles/telemetry/bin/subyard-telemetry'
    refusals = []
    unsafe = [str(candidate) for candidate in (packaged, built) if candidate.is_symlink()]
    if unsafe:
        refusals.append('unsafe native telemetry source: symlinked collector ' + ', '.join(unsafe))
    if not selected or unsafe:
        return None, refusals
    found = [candidate for candidate in (packaged, built) if candidate.is_file()]
    usable = [candidate for candidate in found if os.access(candidate, os.X_OK)]
    if usable:
        return usable[0], refusals
    if found:
        refusals.append('native telemetry collector is not executable: ' + ', '.join(map(str, found)))
    else:
        refusals.append('native telemetry collector is missing: selected profile telemetry needs '
                        f'{packaged} or {built}; build it with make build')
    return None, refusals


def main():
    arguments = sys.argv[1:]
    mode = {'--yes': 'apply', '--check': 'check', '--remove': 'apply', '--pause': 'pause', '--resume': 'resume'}
    selected = os.environ.get('SUBYARD_PROFILE_SELECTED', '0') == '1'
    bindings = []
    if arguments[:1] == ['--runtime']:
        selected = 'telemetry' in os.environ.get('ENVIRONMENT_PROFILES', '').split()
        arguments = arguments[1:]
        if arguments[:1] == ['observe'] and len(arguments) == 1:
            operation = 'observe'
        elif arguments[:1] == ['apply'] and len(arguments) == 4:
            operation, bindings = 'refresh', arguments[2:]
        elif arguments[:1] == ['identity-observe'] and len(arguments) == 3:
            operation,bindings='identity-observe',arguments[1:]
        elif arguments[:1] == ['identity-apply'] and len(arguments) == 2:
            operation,bindings='identity-apply',arguments[1:]
        else:
            raise ValueError('invalid runtime request')
    elif len(arguments) == 1 and arguments[0] in mode:
        operation = mode[arguments[0]]
        if arguments[0] == '--remove':
            selected = False
    else:
        raise ValueError('invalid lifecycle request')
    project, instance = os.environ['INCUS_PROJECT'], os.environ['YARD_INSTANCE_NAME']
    # `incus list NAME` also lists every instance whose name starts with NAME.
    rows = run(['incus', 'list', instance, '--project', project, '-f', 'csv', '-c', 'ns']).splitlines()
    status = next((row[len(instance) + 1:] for row in rows if row.startswith(instance + ',')), '')
    if status in ('', 'STOPPED'):
        if operation == 'observe':
            print(json.dumps({'state': 'deferred' if status else 'absent', 'actual': '', 'desired': ''}))
        return 0
    if status != 'RUNNING':
        raise ValueError('yard unavailable')
    source = Path(__file__).resolve().parent
    binary, refusals = collector_binary(source, selected)
    agents = ' '.join(name for name in ('claude', 'codex') if name in os.environ.get('CODING_TOOL_INTEGRATIONS', '').split())
    for agent in agents.split():
        destination = os.environ.get('AGENT_'+agent+'_CONFIG_DEST')
        expected = '.claude/settings.json' if agent == 'claude' else '.codex/config.toml'
        if selected and destination != expected:
            refusals.append(f'standard native destination required: AGENT_{agent}_CONFIG_DEST must be {expected}')
    if refusals:
        raise Refusal(refusals)
    stream = io.BytesIO()
    with tarfile.open(fileobj=stream, mode='w') as archive:
        for name in FILES:
            archive.add(source/name, arcname=name, recursive=False)
        # Only a selected run installs the collector; an unselected one keeps its path only.
        if binary is not None:
            archive.add(binary, arcname='subyard-telemetry', recursive=False)
        archive.add(source.parents[2]/'internal/adapters/configmaterial/tomli_w.py', arcname='tomli_w.py', recursive=False)
        if operation.startswith('identity-'):
            archive.add(source.parents[1]/'identity/authority.py',arcname='authority.py',recursive=False)
    output = run(['incus', 'exec', instance, '--project', project, '--', 'bash', '-euo', 'pipefail', '-c',
                  GUEST_SCRIPT, 'subyard', operation, str(int(selected)), os.environ['DEV_USER'], agents, *bindings], input=stream.getvalue())
    if operation == 'check':
        if output == 'telemetry-drift':
            return 1
        if output:
            raise ValueError('unexpected telemetry check output')
        return 0
    if output and operation == 'observe' and os.environ.get('SUBYARD_PROFILE_DIAGNOSTICS') != '1':
        observation = json.loads(output)
        observation.pop('diagnostics', None)
        output = json.dumps(observation)
    if output:
        print(output)
    return 0


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except (OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
        if isinstance(error, Refusal):
            print(f'telemetry: refused: {error}', file=sys.stderr)
        print('telemetry: owner transport or native ownership unavailable', file=sys.stderr)
        raise SystemExit(2)
