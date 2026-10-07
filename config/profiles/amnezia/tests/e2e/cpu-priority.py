#!/usr/bin/env python3
"""Measure controlled VM/container CPU contention on an allocated owner."""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time


def instance(project, name):
    value = json.loads(subprocess.check_output([
        'incus', 'query', f'/1.0/instances/{name}?project={project}']))
    assert value['config']['user.subyard.managed'] == 'true'
    state = json.loads(subprocess.check_output([
        'incus', 'query', f'/1.0/instances/{name}/state?project={project}']))
    assert state['status'] == 'Running' and state['pid'] > 0
    return state['pid']


def cgroup(pid):
    value = Path(f'/proc/{pid}/cgroup').read_text().strip()
    assert value.startswith('0::/') and '\n' not in value
    return Path('/sys/fs/cgroup') / value[4:]


def usage(group):
    return int(dict(line.split() for line in (group / 'cpu.stat').read_text().splitlines())['usage_usec'])


def write_result(path, value):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, 'w') as output:
        json.dump(value, output)


def interrupted(_signal, _frame):
    raise RuntimeError('CPU contention probe interrupted')


def main():
    assert os.geteuid() == 0 and len(sys.argv) == 3
    assert Path('/run/subyard-e2e-lease.json').is_file(), 'allocated owner required'
    fixture = Path(sys.argv[1])
    assert fixture == Path('/var/tmp/subyard-amnezia-profile-e2e') and not fixture.is_symlink()
    assert (fixture / '.marker').read_text().strip() == 'subyard-amnezia-e2e-v1'
    weight = int(sys.argv[2])
    assert weight in (100, 1000)
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    vm_pid = instance('subyard-vpn-e2e', 'yard-vpn-e2e')
    work_pid = instance('subyard-work-e2e', 'yard-work-e2e')
    vm_group, work_group = cgroup(vm_pid), cgroup(work_pid)
    # The init PID may live in init.scope. Resolve the container's namespace
    # root rather than including unrelated host services in the measurement.
    visible = subprocess.check_output(['incus', 'exec', 'yard-work-e2e', '--project',
        'subyard-work-e2e', '--', 'cat', '/proc/1/cgroup'], text=True).strip()
    assert visible.startswith('0::/') and '\n' not in visible
    relative = Path(visible[4:]).parts
    assert not relative or work_group.parts[-len(relative):] == relative
    for _ in relative:
        work_group = work_group.parent
    assert work_group != Path('/sys/fs/cgroup'), 'private yard cgroup required'
    assert int((vm_group / 'cpu.weight').read_text()) == weight
    cpu = min(os.sched_getaffinity(vm_pid) & os.sched_getaffinity(work_pid))
    affinities, jobs = {}, []
    marker = 'subyard-amnezia-priority-workload'
    workload = ("import os,sys,time; from pathlib import Path; "
                "Path(sys.argv[2]).write_text(str(os.getpid())); end=time.monotonic()+180\n"
                "while time.monotonic()<end and not Path(sys.argv[3]).exists(): sum(range(10000))")
    markers = []
    try:
        for task in Path(f'/proc/{vm_pid}/task').iterdir():
            tid = int(task.name)
            affinities[tid] = os.sched_getaffinity(tid)
            os.sched_setaffinity(tid, {cpu})
        for project, name in [('subyard-vpn-e2e', 'yard-vpn-e2e'),
                              ('subyard-work-e2e', 'yard-work-e2e')]:
            for index in range(2):
                ready = f'/run/{marker}-{weight}-{index}.pid'
                stop = ready + '.stop'
                markers.append((project, name, ready, stop))
                jobs.append(subprocess.Popen(['incus', 'exec', name, '--project', project, '--',
                    'python3', '-c', workload, marker, ready, stop], stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL, start_new_session=True))
        deadline = time.monotonic() + 25
        while True:
            workers = []
            for path in Path('/proc').iterdir():
                if not path.name.isdigit():
                    continue
                try:
                    if (path / 'comm').read_text().strip() == 'python3' \
                            and marker.encode() in (path / 'cmdline').read_bytes() \
                            and cgroup(int(path.name)).is_relative_to(work_group):
                        workers.append(int(path.name))
                except (FileNotFoundError, ProcessLookupError):
                    continue
            if len(workers) == 2:
                break
            assert time.monotonic() < deadline, 'ordinary yard workload did not start'
            time.sleep(0.2)
        for pid in workers:
            os.sched_setaffinity(pid, {cpu})
        for project, name, ready, _ in markers:
            subprocess.run(['timeout', '30', 'incus', 'exec', name, '--project', project,
                            '--', 'sh', '-ec',
                            'for attempt in $(seq 1 50); do test -s "$1" && exit 0; sleep 0.2; done; exit 1',
                            'sh', ready], check=True,
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        time.sleep(3)
        before = usage(vm_group), usage(work_group)
        write_result(fixture / f'priority-{weight}.ready', {'ready': True})
        time.sleep(30)
        delta = usage(vm_group) - before[0], usage(work_group) - before[1]
        assert all(job.poll() is None for job in jobs), 'CPU workload ended before measurement'
        assert min(delta) > 10000, 'both yards must receive CPU time'
        write_result(fixture / f'priority-{weight}.json', {
            'weight': weight, 'vm_cpu_usec': delta[0], 'neighbor_cpu_usec': delta[1]})
        deadline = time.monotonic() + 60
        while not (fixture / f'priority-{weight}.client-done').exists():
            assert time.monotonic() < deadline, 'VPN client probe did not finish under CPU contention'
            assert all(job.poll() is None for job in jobs), 'CPU contention ended before VPN probe'
            time.sleep(0.2)
    finally:
        for project, name, _, stop in markers:
            try:
                subprocess.run(['timeout', '5', 'incus', 'exec', name, '--project', project,
                                '--', 'touch', stop], timeout=8, stdout=subprocess.DEVNULL,
                               stderr=subprocess.DEVNULL)
            except subprocess.TimeoutExpired:
                pass
        for job in jobs:
            try:
                job.wait(timeout=8)
            except subprocess.TimeoutExpired:
                os.killpg(job.pid, signal.SIGKILL)
                job.wait()
        for tid, affinity in affinities.items():
            try:
                os.sched_setaffinity(tid, affinity)
            except ProcessLookupError:
                pass
    assert all(job.returncode == 0 for job in jobs), 'CPU workload failed'


if __name__ == '__main__':
    main()
