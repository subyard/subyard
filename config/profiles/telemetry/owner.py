#!/usr/bin/env python3
"""Existing assessed owner-to-guest transport; no host collector or dashboard."""
import io
import json
import os
from pathlib import Path
import re
import shlex
import stat
import subprocess
import sys
import tarfile

from native import refusal_messages

FILES = ('guest.py', 'native.py', 'launcher.py', 'processes.py', 'records.py', 'forward.py',
         'resources.py', 'sources.py', 'broker-correlation.py', 'skills/subyard-telemetry/SKILL.md')
GUEST_SCRIPT = 'stage="$(mktemp -d /tmp/subyard-telemetry.XXXXXX)"; trap \'rm -rf -- "$stage"\' EXIT; tar -xf - -C "$stage"; python3 -B "$stage/guest.py" "$@"'
SAFE_GUEST_FAILURE = re.compile(r'telemetry: local reconciliation failed \((?P<kind>OSError|PermissionError|FileNotFoundError|FileExistsError|NotADirectoryError|IsADirectoryError|BlockingIOError|TimeoutError|ValueError|KeyError|TimeoutExpired|CalledProcessError|SubprocessError|UnicodeDecodeError|JSONDecodeError) at (?P<location>(?:guest|native)\.py:[1-9][0-9]{0,4})\)\Z')


def run(arguments, **kwargs):
    result = subprocess.run(arguments, stdin=subprocess.DEVNULL if 'input' not in kwargs else None,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=90, **kwargs)
    if result.returncode:
        for line in result.stderr[-65536:].decode(errors='replace').splitlines()[-32:]:
            if line.startswith('telemetry-native-refusal-v2 '):
                try:
                    reasons = json.loads(line.removeprefix('telemetry-native-refusal-v2 '))
                    messages = refusal_messages(reasons)
                except (ValueError, TypeError, RecursionError):
                    continue
                raise Refusal(messages, rollback_retry(reasons))
            match = SAFE_GUEST_FAILURE.fullmatch(line)
            if match:
                print(f'telemetry: local reconciliation failed ({match["kind"]} at {match["location"]})', file=sys.stderr)
                print('subyard-profile-observation-diagnostic-v1 '+match['kind']+' '+match['location'], file=sys.stderr)
                break
        raise ValueError('telemetry owner transport failed')
    return result.stdout.decode().strip()


def yard_name():
    return os.environ.get('YARD_NAME') or os.environ.get('SUBYARD_YARD') or 'default'


def rollback_retry(reasons):
    """The next step of a rollback preparation refusal; other refusals keep config paths."""
    for reason, command in ((['rollback-enabled'], ['profile', 'disable', 'telemetry']), (['rollback-unconverged'], ['migrate'])):
        if reason in reasons:
            return 'run '+shlex.join(['yard', '-Y', yard_name(), *command])
    return None


class Refusal(ValueError):
    """Every independent refusal found before any effect, named for the operator."""
    def __init__(self, refusals, retry=None):
        super().__init__('; '.join(refusals))
        self.retry = retry if retry is not None else 'run '+shlex.join(['yard', '-Y', yard_name(), 'config', 'paths'])


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


def disk_budget(value):
    """Bytes for a size setting as core validates it, None when unset, 0 when invalid."""
    if value == '':
        return None
    match = re.fullmatch('([+-]?[0-9]+)(MiB|GiB)', value)
    number = int(match[1]) if match else 0
    if number < 1 or number >= 1 << 63:
        return 0
    # The collector reads a signed 64-bit count; larger budgets are unbounded anyway.
    return min(number << (20 if match[2] == 'MiB' else 30), (1 << 63) - 1)


def main():
    arguments = sys.argv[1:]
    mode = {'--startup-check': 'startup', '--yes': 'apply', '--check': 'check', '--remove': 'apply', '--pause': 'pause', '--resume': 'resume'}
    selected = os.environ.get('SUBYARD_PROFILE_SELECTED', '0') == '1'
    bindings = []
    if arguments[:1] == ['--runtime']:
        selected = 'telemetry' in os.environ.get('ENVIRONMENT_PROFILES', '').split()
        arguments = arguments[1:]
        if arguments[:1] == ['observe'] and len(arguments) == 1:
            operation = 'observe'
        elif arguments[:1] == ['apply'] and len(arguments) == 4:
            operation, bindings = 'refresh', arguments[2:]
        elif arguments[:1] == ['rollback-observe'] and len(arguments) == 1:
            operation = 'rollback-observe'
        elif arguments[:1] == ['rollback-apply'] and len(arguments) == 4:
            operation, bindings = 'rollback-apply', arguments[2:]
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
        if operation == 'startup':
            print(json.dumps({'state': 'drift'}))
        elif operation in ('observe', 'rollback-observe'):
            print(json.dumps({'state': 'deferred' if status else 'absent', 'actual': '', 'desired': ''}))
        return 0
    if status != 'RUNNING':
        raise ValueError('yard unavailable')
    # The collector lives only in the yard; a guest init never provisioned has
    # none to remove, pause or resume.
    if os.environ.get('SUBYARD_GUEST_NEVER_PROVISIONED') == '1' and arguments in (['--remove'], ['--pause'], ['--resume']):
        return 0
    source = Path(__file__).resolve().parent
    binary, refusals = collector_binary(source, selected and operation != 'startup')
    agents = ' '.join(name for name in ('claude', 'codex') if name in os.environ.get('CODING_TOOL_INTEGRATIONS', '').split())
    templates = {}
    if selected and operation in ('observe', 'refresh', 'check', 'apply'):
        for agent in agents.split():
            path = os.environ.get('AGENT_'+agent+'_CONFIG', '').strip()
            if not path:
                continue
            try:
                # A retained release may anchor the source directory by descriptor.
                if path.startswith('/proc/self/fd/'):
                    path = path.replace('/proc/self/', f'/proc/{os.getppid()}/', 1)
                with os.fdopen(os.open(path, os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW), 'rb') as template:
                    if not stat.S_ISREG(os.fstat(template.fileno()).st_mode):
                        raise ValueError('not regular')
                    content = template.read(4*1024*1024+1)
                    if len(content) > 4*1024*1024:
                        raise ValueError('size bound')
                templates[agent] = content
            except (OSError, ValueError):
                refusals.append('missing, unsafe or unreadable telemetry template for '+agent)
    budget = disk_budget(os.environ.get('TELEMETRY_DISK_BUDGET', ''))
    if budget == 0:
        refusals.append('TELEMETRY_DISK_BUDGET must be a positive MiB or GiB value')
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
        archive.add(source.parents[2]/'config/skills/deliver.py', arcname='agent-skills.py', recursive=False)
        for agent, content in templates.items():
            entry = tarfile.TarInfo(agent+'.template')
            entry.size, entry.mode = len(content), 0o600
            archive.addfile(entry, io.BytesIO(content))
        if operation.startswith('identity-'):
            archive.add(source.parents[1]/'identity/authority.py',arcname='authority.py',recursive=False)
    remaining = os.environ.get('SUBYARD_STARTUP_REMAINING_MS', '')
    startup_environment = ['--env', 'SUBYARD_STARTUP_REMAINING_MS='+remaining] if operation == 'startup' and re.fullmatch('[0-9]{1,9}', remaining) else []
    if budget is not None:
        startup_environment += ['--env', 'SUBYARD_TELEMETRY_DISK_BYTES='+str(budget)]
    output = run(['incus', 'exec', instance, '--project', project, *startup_environment, '--', 'bash', '-euo', 'pipefail', '-c',
                  GUEST_SCRIPT, 'subyard', operation, str(int(selected)), os.environ['DEV_USER'], agents, *bindings], input=stream.getvalue())
    if operation == 'check':
        if output == 'telemetry-drift':
            return 1
        if output == 'telemetry-advisory storage-capacity':
            print('subyard-profile-advisory-v1 storage-capacity', file=sys.stderr)
            return 0
        if output:
            raise ValueError('unexpected telemetry check output')
        return 0
    if output and operation in ('observe', 'rollback-observe') and os.environ.get('SUBYARD_PROFILE_DIAGNOSTICS') != '1':
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
            print('subyard-profile-observation-refusal-v1 '+json.dumps({'message': str(error), 'retry': error.retry}), file=sys.stderr)
        print('telemetry: owner transport or native ownership unavailable', file=sys.stderr)
        raise SystemExit(2)
