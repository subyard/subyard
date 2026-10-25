#!/usr/bin/env python3
"""Physical pre-exec placement and final counters after main-process exit."""
import argparse
from contextlib import closing
import grp
import json
import os
from pathlib import Path
import sqlite3
import shlex
import subprocess
import sys
import time


def nested_run(args):
    context = args.state/'nested-context.json'
    code = r'''import json,os,subprocess,sys
from pathlib import Path
parent=os.environ['SUBYARD_AGENT_RUN_ID']
child='import json,os,sys; from pathlib import Path; Path(sys.argv[1]).write_text(json.dumps({"parent":os.environ["SUBYARD_PARENT_AGENT_RUN_ID"],"child":os.environ["SUBYARD_AGENT_RUN_ID"],"cgroup":Path("/proc/self/cgroup").read_text().strip()})); os.execv("/bin/true",["/bin/true"])'
env=dict(os.environ,SUBYARD_PARENT_AGENT_RUN_ID=parent)
subprocess.run(['subyard-telemetry-run','--agent','other','--agent-version','fixture','--scope-mode','require','--project-id','telemetry-fixture','--',sys.executable,'-c',child,sys.argv[1]],env=env,check=True,timeout=15)
document=json.loads(Path(sys.argv[1]).read_text())
assert document['parent']==parent
assert '/run-'+parent+'/run-'+document['child']+'/workload' in document['cgroup']
'''
    subprocess.run(['subyard-telemetry-run', '--data-dir', str(args.data_dir), '--agent', 'other',
                    '--agent-version', 'fixture', '--scope-mode', 'require', '--project-id', 'telemetry-fixture',
                    '--', sys.executable, '-c', code, str(context)], check=True, timeout=25)
    run = json.loads(context.read_text())
    deadline = time.monotonic()+40
    with closing(sqlite3.connect((args.data_dir/'telemetry.sqlite').as_uri()+'?mode=ro', uri=True, timeout=1)) as db:
        while time.monotonic() < deadline:
            finals = {}
            for name in ('parent', 'child'):
                samples = [json.loads(row[0]) for row in db.execute('SELECT body FROM samples WHERE object_id=?', (run[name]+':scope',))]
                finals[name] = any(sample.get('final') and sample.get('populated') is False for sample in samples)
            if all(finals.values()) and not any((args.data_dir/'runs'/(run[name]+'.json')).exists() for name in ('parent', 'child')):
                parent = db.execute('SELECT parent_object_id FROM objects WHERE object_id=?', (run['child']+':scope',)).fetchone()
                child_run = db.execute('SELECT body FROM runs WHERE agent_run_id=?', (run['child'],)).fetchone()
                if not parent or not child_run:
                    time.sleep(.2)
                    continue
                assert parent and parent[0] == run['parent']+':scope'
                assert child_run and json.loads(child_run[0])['parent_run_id'] == run['parent']
                print(json.dumps({'schema': 'subyard.telemetry.nested-scope/v1', 'nested_placement': True,
                                  'parent_relation': True, 'parent_final': True, 'child_final': True}), flush=True)
                return
            time.sleep(.2)
    raise AssertionError('nested scope final counters were not committed')


def short_run(args):
    context = args.state/'short-context.json'
    code = r'''import json,os,subprocess,sys,time
from pathlib import Path
child='import time; start=time.process_time_ns()\nwhile time.process_time_ns()-start<250000000: sum(range(10000))\nprint(time.process_time_ns()-start)'
cpu_ns=int(subprocess.check_output([sys.executable,'-c',child],timeout=4))
Path(sys.argv[1]).write_text(json.dumps({'agent_run_id':os.environ['SUBYARD_AGENT_RUN_ID'],'cpu_ns':cpu_ns}))
'''
    started = time.monotonic()
    subprocess.run(['subyard-telemetry-run', '--data-dir', str(args.data_dir), '--agent', 'other',
                    '--agent-version', 'fixture', '--scope-mode', 'require', '--project-id', 'telemetry-fixture',
                    '--', sys.executable, '-c', code, str(context)], check=True, timeout=15)
    elapsed = time.monotonic()-started
    assert elapsed < 5, 'short-run fixture exceeded the sampling interval'
    run = json.loads(context.read_text())
    deadline = time.monotonic()+40
    with closing(sqlite3.connect('file:'+str(args.data_dir/'telemetry.sqlite')+'?mode=ro', uri=True, timeout=1)) as db:
        while time.monotonic() < deadline:
            rows = db.execute('SELECT body FROM samples WHERE object_id=? ORDER BY source_time', (run['agent_run_id']+':scope',)).fetchall()
            samples = [json.loads(row[0]) for row in rows]
            if any(sample.get('final') for sample in samples):
                break
            time.sleep(0.2)
        else:
            raise AssertionError('short-run final sample was not committed')
    binary = '/usr/local/lib/subyard-telemetry/subyard-telemetry'
    result = subprocess.run([binary, 'show-run', '--data-dir', str(args.data_dir), '--run', run['agent_run_id']],
                            capture_output=True, check=True, timeout=8)
    document = json.loads(result.stdout)
    entries = [entry for key in ('selected_nonoverlapping', 'shared', 'unknown', 'excluded_overlapping_detail') for entry in document[key]]
    deltas = {}
    for suffix in (':scope', ':workload'):
        entry = next(entry for entry in entries if entry['object']['object_id'] == run['agent_run_id']+suffix)
        first, last = entry['samples'][0], entry['samples'][-1]
        assert first['populated'] is False and first['final'] is False and last['final'] is True
        assert entry['counter_coverage'] == 'observed_interval'
        assert entry['cpu_ns_delta'] >= run['cpu_ns'] > 0, 'reported CPU dropped work before the first sampling tick'
        assert first['monotonic_ns'] < last['monotonic_ns']
        assert first['generation'] == last['generation'] and first['boot_id'] == last['boot_id']
        deltas[suffix] = entry['cpu_ns_delta']
    print(json.dumps({'schema': 'subyard.telemetry.short-scope/v1', 'elapsed_seconds': elapsed,
                      'native_cpu_ns': run['cpu_ns'], 'reported_cpu_ns': deltas}), flush=True)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--state', type=Path, required=True)
    parser.add_argument('--data-dir', type=Path, required=True)
    parser.add_argument('--nested-only', action='store_true')
    parser.add_argument('--different-gid', action='store_true')
    parser.add_argument('--expect-manager-namespace-denied', action='store_true')
    args = parser.parse_args()
    if args.different_gid:
        assert args.nested_only and os.environ.get('SUBYARD_E2E_VM') == '1' and Path('/run/subyard-e2e-lease.json').is_file()
        choices = sorted(set(os.getgroups())-{os.getgid()})
        assert choices, 'different-primary-GID proof requires an existing supplementary group'
        group = grp.getgrgid(choices[0]).gr_name
        command = [sys.executable, '-B', str(Path(__file__).resolve()), '--nested-only', '--expect-manager-namespace-denied',
                   '--state', str(args.state), '--data-dir', str(args.data_dir)]
        subprocess.run(['sg', group, '-c', 'exec '+shlex.join(command)], check=True, timeout=90)
        return
    if args.expect_manager_namespace_denied:
        delegation = json.loads((args.data_dir/'delegation.json').read_text())
        manager = Path('/proc')/str(delegation['pid'])
        groups = next(line.split()[1:] for line in (manager/'status').read_text().splitlines() if line.startswith('Gid:'))
        assert int(groups[0]) != os.getgid(), 'fixture did not change the manager/caller primary GID'
        denied = False
        try:
            os.readlink(manager/'ns/pid')
        except PermissionError:
            denied = True
        assert denied, 'fixture did not reproduce the manager namespace permission boundary'
        print(json.dumps({'schema': 'subyard.telemetry.manager-stat-regression/v1', 'different_primary_gid': True,
                          'manager_namespace_read_denied': True}), flush=True)
    nested_run(args)
    if args.nested_only:
        return
    short_run(args)
    context = args.state/'physical-context.json'
    code = r'''import json,os,subprocess,sys
from pathlib import Path
run=os.environ['SUBYARD_AGENT_RUN_ID']
cg=Path('/proc/self/cgroup').read_text().strip()
assert '/run-'+run+'/workload' in cg,cg
child=subprocess.Popen([sys.executable,'-c','import time; end=time.monotonic()+7; data=bytearray(8*1024*1024);\nwhile time.monotonic()<end: sum(range(10000))'])
Path(sys.argv[1]).write_text(json.dumps({'agent_run_id':run,'child_pid':child.pid,'cgroup':cg}))
'''
    subprocess.run(['subyard-telemetry-run', '--data-dir', str(args.data_dir), '--agent', 'other',
                    '--agent-version', 'fixture', '--scope-mode', 'require', '--project-id', 'telemetry-fixture',
                    '--', sys.executable, '-c', code, str(context)], check=True, timeout=15)
    run = json.loads(context.read_text())
    assert Path('/proc/'+str(run['child_pid'])).exists(), 'background child did not outlive main'
    deadline = time.monotonic()+40
    with closing(sqlite3.connect('file:'+str(args.data_dir/'telemetry.sqlite')+'?mode=ro', uri=True, timeout=1)) as db:
        while time.monotonic() < deadline:
            samples = [json.loads(row[0]) for row in db.execute('SELECT body FROM samples WHERE object_id=?', (run['agent_run_id']+':scope',))]
            finals = [sample for sample in samples if sample.get('final')]
            if finals:
                assert finals[-1]['cpu_ns'] > 0
                assert finals[-1]['populated'] is False
                assert finals[-1]['memory_peak_bytes'] >= 8*1024*1024
                assert not list((args.data_dir/'runs').glob(run['agent_run_id']+'.json'))
                print('ok: pre-exec managed placement; child survived main; final cumulative counters durable')
                return
            time.sleep(0.2)
    raise AssertionError('final descendant counters were not committed')


if __name__ == '__main__':
    main()
