#!/usr/bin/env python3
"""Stream bounded, private diagnostics for the exact disposable VPN instance."""
import json
import os
from pathlib import Path
import resource
import signal
import stat
import subprocess
import sys
import tarfile
import tempfile
import time

INSTANCE = 'yard-vpn-e2e'
PROJECT = 'subyard-vpn-e2e'
LIMIT = 65536
PROBES = {
    'managed': ['incus', 'config', 'get', INSTANCE, 'user.subyard.managed', '--project', PROJECT],
    'state': ['incus', 'query', f'/1.0/instances/{INSTANCE}/state?project={PROJECT}'],
    'cpu': ['incus', 'config', 'get', INSTANCE, 'limits.cpu', '--project', PROJECT],
    'memory': ['incus', 'config', 'get', INSTANCE, 'limits.memory', '--project', PROJECT],
    'secureboot': ['incus', 'config', 'get', INSTANCE, 'security.secureboot', '--project', PROJECT],
    'startup': ['incus', 'config', 'get', INSTANCE, 'user.subyard.startup.amnezia-vpn', '--project', PROJECT],
    'ingress': ['incus', 'config', 'get', INSTANCE, 'user.subyard.resource.amnezia-vpn', '--project', PROJECT],
    'observe': ['incus', 'exec', INSTANCE, '--project', PROJECT, '--', 'python3',
                '/usr/local/lib/subyard-amnezia/runtime.py', 'observe'],
    'services': ['incus', 'exec', INSTANCE, '--project', PROJECT, '--', 'systemctl', 'show',
                 'incus-agent.service', 'docker.service', 'containerd.service', 'subyard-amnezia.service',
                 '-p', 'Id', '-p', 'LoadState', '-p', 'ActiveState', '-p', 'SubState',
                 '-p', 'Result', '-p', 'ExecMainStatus', '-p', 'NRestarts'],
    'container': ['incus', 'exec', INSTANCE, '--project', PROJECT, '--', 'docker', 'inspect',
                  '--format', '{{.State.Status}}|{{.State.Running}}|{{.State.Restarting}}|{{.State.ExitCode}}',
                  'amnezia-awg2'],
    'console': ['incus', 'console', INSTANCE, '--project', PROJECT, '--show-log'],
    'owner-memory': ['free', '-b'],
    'owner-power': ['systemctl', 'show', 'subyard-power-reconcile.service', '-p', 'LoadState',
                    '-p', 'ActiveState', '-p', 'SubState', '-p', 'Result', '-p', 'ExecMainStatus', '-p', 'NRestarts'],
    'owner-oom': ['bash', '-o', 'pipefail', '-c', 'journalctl -k -b --no-pager -n 200 | '
                  '{ grep -iE "out of memory|oom-kill|killed process" || test "$?" = 1; } | tail -n 30'],
    'steps': ['cat', '/var/tmp/subyard-amnezia-profile-e2e/recovery.steps'],
    'down': ['cat', '/var/tmp/subyard-amnezia-profile-e2e/recovery-state-mount.down'],
}
MEMBERS = sorted(['result.json', *[name + suffix for name in PROBES for suffix in ('.stdout', '.stderr')]])


def child_limits():
    resource.setrlimit(resource.RLIMIT_FSIZE, (LIMIT, LIMIT))


def collect():
    # Each probe has a native process-group deadline and separate byte caps.
    with tempfile.TemporaryDirectory(prefix='subyard-amnezia-capture-', dir='/tmp') as temporary:
        directory = Path(temporary)
        os.chmod(directory, 0o700)
        results = {}
        owned = False
        for name, command in PROBES.items():
            paths = [directory / (name + suffix) for suffix in ('.stdout', '.stderr')]
            files = [path.open('wb') for path in paths]
            for path in paths:
                os.chmod(path, 0o600)
            started = time.monotonic()
            code = None
            timed_out = False
            try:
                allowed = name == 'managed' or owned
                if name in ('steps', 'down'):
                    path = Path(command[1])
                    info = path.lstat()
                    allowed = (owned and stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode) == 0o600
                               and not path.parent.is_symlink()
                               and (path.parent / '.marker').read_text() == 'subyard-amnezia-e2e-v1\n')
                if allowed:
                    with subprocess.Popen(command, stdin=subprocess.DEVNULL, stdout=files[0], stderr=files[1],
                                          start_new_session=True, preexec_fn=child_limits) as child:
                        try:
                            code = child.wait(timeout=5)
                        except subprocess.TimeoutExpired:
                            timed_out = True
                            os.killpg(child.pid, signal.SIGKILL)
                            code = child.wait()
            except OSError:
                code = 127
            finally:
                for output in files:
                    output.close()
            sizes = [path.stat().st_size for path in paths]
            results[name] = dict(exit_code=code, timed_out=timed_out,
                                 duration_seconds=round(time.monotonic() - started, 3),
                                 stdout_bytes=sizes[0], stderr_bytes=sizes[1],
                                 truncated=any(size >= LIMIT for size in sizes))
            if name == 'managed':
                owned = code == 0 and paths[0].read_bytes().strip() == b'true'
        result = directory / 'result.json'
        result.write_text(json.dumps(dict(owned=owned, probes=results)) + '\n')
        os.chmod(result, 0o600)
        with tarfile.open(fileobj=sys.stdout.buffer, mode='w|') as archive:
            for name in MEMBERS:
                archive.add(directory / name, arcname=name, recursive=False)


def validate(path):
    if Path(path).stat().st_size > 3145728:
        return 1
    with tarfile.open(path) as archive:
        members = archive.getmembers()
        if sorted(item.name for item in members) != MEMBERS or any(
                not item.isfile() or item.mode != 0o600 or item.size > LIMIT for item in members):
            return 1
        result = json.load(archive.extractfile('result.json'))
        if not isinstance(result, dict) or result.get('owned') is not True or not isinstance(result.get('probes'), dict):
            return 1
        if set(result['probes']) != set(PROBES) or any(not isinstance(item, dict) for item in result['probes'].values()):
            return 1
        # Failed probes are useful evidence, but capture completeness stays explicit.
        return int(any(item['exit_code'] != 0 or item['timed_out'] or item['truncated']
                       for item in result['probes'].values()))


if __name__ == '__main__':
    try:
        if sys.argv[1:] == ['collect']:
            collect()
        elif len(sys.argv) == 3 and sys.argv[1] == 'validate':
            sys.exit(validate(sys.argv[2]))
        else:
            sys.exit(2)
    except (OSError, ValueError, KeyError, TypeError, tarfile.TarError):
        sys.exit(1)
