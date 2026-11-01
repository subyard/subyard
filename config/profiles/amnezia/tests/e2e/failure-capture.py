#!/usr/bin/env python3
"""Stream bounded, private diagnostics for the exact disposable VPN instance."""
import json
import os
from pathlib import Path
import re
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
    'qmp-log': ['incus', 'query', f'/1.0/instances/{INSTANCE}/logs/qemu.qmp.log?project={PROJECT}'],
    'info-log': ['incus', 'info', '--show-log', INSTANCE, '--project', PROJECT],
    'incus-journal': ['journalctl', '-u', 'incus.service', '-b', '--no-pager', '-n', '100', '-o', 'short-monotonic'],
    'image-facts': [sys.executable, str(Path(__file__).resolve()), 'image-facts'],
    'cpu-runtime': [sys.executable, str(Path(__file__).resolve()), 'cpu-runtime'],
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


def read_bounded(path):
    with Path(path).open('rb') as source:
        body = source.read(LIMIT + 1)
    if len(body) > LIMIT:
        raise ValueError('diagnostic input exceeds bound')
    return body


def query_json(path):
    # The temporary output and native RLIMIT bound nested Incus responses too.
    with tempfile.TemporaryFile(dir='/tmp') as output:
        os.fchmod(output.fileno(), 0o600)
        subprocess.run(['incus', 'query', path], stdin=subprocess.DEVNULL, stdout=output,
                       stderr=subprocess.DEVNULL, timeout=3, check=True, preexec_fn=child_limits)
        output.seek(0)
        body = output.read(LIMIT + 1)
        if len(body) > LIMIT:
            raise ValueError('diagnostic response exceeds bound')
        return json.loads(body)


def image_facts():
    instance = query_json(f'/1.0/instances/{INSTANCE}?project={PROJECT}')
    config = instance.get('config', {})
    fingerprint = config.get('volatile.base_image', '')
    facts = dict(architecture=instance.get('architecture') if instance.get('architecture') in ('x86_64', 'aarch64') else None,
                 type=instance.get('type') if instance.get('type') in ('virtual-machine', 'container') else None,
                 base_fingerprint=fingerprint if re.fullmatch('[a-f0-9]{64}', fingerprint) else None)
    if facts['base_fingerprint']:
        try:
            image = query_json('/1.0/images/' + fingerprint + '?project=default')
            public = lambda value: value if isinstance(value, str) and re.fullmatch(r'[A-Za-z0-9 .:/+_-]{0,128}', value) else None
            facts['image'] = {key: public(image.get(key)) for key in ('fingerprint', 'architecture', 'created_at', 'uploaded_at')}
            facts['properties'] = {key: public(image.get('properties', {}).get(key))
                                   for key in ('os', 'release', 'architecture', 'serial')}
        except (OSError, ValueError, subprocess.SubprocessError):
            facts['image_lookup'] = 'unavailable'
    return facts


def cpu_runtime():
    instance = query_json(f'/1.0/instances/{INSTANCE}?project={PROJECT}')
    state = query_json(f'/1.0/instances/{INSTANCE}/state?project={PROJECT}')
    pid = state.get('pid')
    if type(pid) is not int or pid <= 1 or instance.get('type') != 'virtual-machine':
        return dict(status=instance.get('status'), pid=pid, qemu='unavailable')
    uuid = instance.get('config', {}).get('volatile.uuid', '')
    if not re.fullmatch('[a-fA-F0-9-]{1,64}', uuid):
        raise ValueError('QEMU UUID missing')
    proc = Path('/proc') / str(pid)
    before = read_bounded(proc / 'stat')
    arguments = read_bounded(proc / 'cmdline').split(b'\0')
    if (not arguments or not Path(os.fsdecode(arguments[0])).name.startswith('qemu-system-') or
            b'-uuid' not in arguments or arguments[arguments.index(b'-uuid') + 1] != uuid.encode()):
        raise ValueError('QEMU identity changed')
    cgroup = read_bounded(proc / 'cgroup').decode().splitlines()
    unified = [line[3:] for line in cgroup if line.startswith('0::')]
    if len(unified) != 1 or not re.fullmatch(r'/[A-Za-z0-9_.@:/-]{1,512}', unified[0]) or '..' in unified[0].split('/'):
        raise ValueError('QEMU cgroup unavailable')
    weight = read_bounded(Path('/sys/fs/cgroup' + unified[0]) / 'cpu.weight').decode().strip()
    after = read_bounded(proc / 'stat')
    if (not weight.isdecimal() or not 1 <= int(weight) <= 10000 or
            before.rsplit(b')', 1)[1].split()[19] != after.rsplit(b')', 1)[1].split()[19]):
        raise ValueError('QEMU identity or CPU weight changed')
    return dict(pid=pid, uuid=uuid, starttime=before.rsplit(b')', 1)[1].split()[19].decode(),
                cgroup=unified[0], scope=unified[0].rsplit('/', 1)[-1], cpu_weight=int(weight))


def redact_diagnostic_log(body):
    text = body[:LIMIT].decode('utf-8', errors='replace')
    text = re.sub(r'-----BEGIN [^-\n]*PRIVATE KEY-----[\s\S]*?(?:-----END [^-\n]*PRIVATE KEY-----|\Z)',
                  '[redacted-private-key]', text)
    # These new logs are diagnostic only: discard entire credential-shaped lines.
    text = re.sub(r'(?im)^.*(?:password|passwd|token|secret|credential|authorization|private[-_ ]?key|api[-_ ]?key|user\.user[-_]data).*$',
                  '[redacted-credential-line]', text)
    text = re.sub(r'https?://[^\s"<>]+', '[redacted-url]', text)
    return text.encode()[:LIMIT]


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
            raw_sizes = [path.stat().st_size for path in paths]
            if name in ('qmp-log', 'info-log', 'incus-journal'):
                for path in paths:
                    path.write_bytes(redact_diagnostic_log(path.read_bytes()))
            sizes = [path.stat().st_size for path in paths]
            results[name] = dict(exit_code=code, timed_out=timed_out,
                                 duration_seconds=round(time.monotonic() - started, 3),
                                 stdout_bytes=sizes[0], stderr_bytes=sizes[1],
                                 truncated=any(size >= LIMIT for size in raw_sizes))
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
        elif sys.argv[1:] == ['image-facts']:
            print(json.dumps(image_facts()))
        elif sys.argv[1:] == ['cpu-runtime']:
            print(json.dumps(cpu_runtime()))
        else:
            sys.exit(2)
    except (OSError, ValueError, KeyError, TypeError, IndexError, subprocess.SubprocessError, tarfile.TarError):
        sys.exit(1)
