#!/usr/bin/env python3
"""Read-only assertions called inside existing marker-owned resource fixtures."""
import argparse
import contextlib
import datetime
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import time

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
import records
import resources
import sources


class Failure(Exception):
    pass


def require(condition, reason):
    if not condition:
        raise Failure(reason)


def failure_details(error):
    """Expose only types and public code locations, never exception payloads."""
    public = {str(Path(module.__file__).resolve()): Path(module.__file__).name
              for module in (records, resources, sources)}
    public[str(Path(__file__).resolve())] = Path(__file__).name
    details = dict(check='resource-owner-observer', passed=False,
                   error_class=type(error).__name__ if type(error).__module__ in
                   ('builtins', 'sqlite3', 'json.decoder', 'subprocess') else 'Error')
    trace = error.__traceback__
    while trace:
        name = public.get(trace.tb_frame.f_code.co_filename)
        if name:
            details['source'] = name
            details['line'] = trace.tb_lineno
            if name == 'resource-observer.py':
                details['callsite_line'] = trace.tb_lineno
        trace = trace.tb_next
    if isinstance(error, OSError) and isinstance(error.errno, int) and 0 <= error.errno <= 4095:
        details['errno'] = error.errno
    if isinstance(error, KeyError) and error.args and error.args[0] in (
            'kind', 'transport', 'source_id', 'owner_id', 'cursor', 'events', 'more',
            'observation', 'observed_at', 'next_cursor', 'object_id', 'body', 'associations'):
        details['field'] = error.args[0]
    return details


def configs(data_dir, kind):
    selected = []
    for path in (Path(data_dir) / 'sources').glob('*.json'):
        config = records.read_private(path)
        if config['kind'] == kind:
            selected.append(config)
    require(selected, 'native client did not register an observer source')
    return selected


def drain_outbox(data_dir):
    data = Path(data_dir)
    source_ids = {records.read_private(path)['source_id']
                  for path in (data/'sources').glob('*.json')}
    if (data/'producer.json').exists():
        source_ids.add(records.read_private(data/'producer.json')['source_id'])
    for path in list((data/'outbox').glob('*.json'))[:4096]:
        try:
            record = records.read_private(path)
        except FileNotFoundError:
            # The collector commits before unlinking; absence alone is not ACK.
            with contextlib.closing(sqlite3.connect((data/'telemetry.sqlite').as_uri()+'?mode=ro', uri=True, timeout=2)) as db:
                db.execute('PRAGMA query_only=ON')
                rows = db.execute("SELECT source_id FROM records WHERE event_id=? AND encoding='json' AND origin_id='producer:'||source_id LIMIT 2",
                                  (path.stem,)).fetchall()
            require(len(rows) == 1 and rows[0][0] in source_ids,
                    'removed outbox evidence lacks canonical SQLite commit')
        else:
            require(records.deliver(data_dir, record), 'collector did not commit retained evidence')


def capture(data_dir, kind):
    """Merge real held intents, then replay without writing the replay cursor."""
    root = records.private_directory(Path(data_dir)/'sources')
    deadline = time.monotonic()+15
    while True:
        try:
            # Use the ordinary reader lock and production intent merger. An
            # immediate post-grant check may precede the manager's next poll.
            with sources.intent_lock(root):
                sources.merge_intents(data_dir,root)
                selected = configs(data_dir,kind)
            break
        except BlockingIOError:
            require(time.monotonic()<deadline,'pending attribution reader-lock deadline')
            time.sleep(.05)
    count = 0
    for config in selected:
        config['cursor'] = '-'
        for _ in range(32):
            page = sources.fetch(config)
            require(sources.consume(data_dir, config, page), 'owner records were not durably admitted')
            count += len(page['events'])
            if not page['more']:
                break
        else:
            raise Failure('native owner journal exceeded physical assertion bound')
    # A successful outbox admission alone does not prove SQLite commit.
    drain_outbox(data_dir)
    require(count > 0, 'native owner lifecycle archive was empty')


def read_rows(data_dir, table):
    require(table in ('allocations', 'samples', 'gaps', 'associations'), 'invalid assertion table')
    with contextlib.closing(sqlite3.connect((Path(data_dir) / 'telemetry.sqlite').as_uri() + '?mode=ro', uri=True, timeout=2)) as db:
        db.execute('PRAGMA query_only=ON')
        return [json.loads(row[0]) for row in db.execute('SELECT body FROM ' + table + ' LIMIT 10000')]


def assert_records(data_dir, kind, stage):
    allocation = [row for row in read_rows(data_dir, 'allocations') if row.get('object_kind') == kind]
    require(allocation, 'SQLite has no native allocation')
    objects = {row['object_id'] for row in allocation}
    agent_run = os.environ.get('SUBYARD_AGENT_RUN_ID')
    edges = [row for row in read_rows(data_dir, 'associations') if row.get('object_id') in objects
             and agent_run and row.get('agent_run_id') == agent_run and row.get('quality') == 'explicit']
    require(edges,'native allocation lacks explicit managed-run association')
    objects = {row['object_id'] for row in edges}
    allocation = [row for row in allocation if row['object_id'] in objects]
    samples = [row for row in read_rows(data_dir, 'samples') if row.get('object_id') in objects]
    require(samples, 'SQLite has no resource counter sample for the managed allocation')
    measured = [row for row in samples if row.get('cpu_ns', 0) is not None and row.get('cpu_ns', 0) > 0
                and ((row.get('memory_bytes') or 0) > 0 or (row.get('rss_bytes') or 0) > 0)]
    require(measured, 'native CPU and memory were not measured')
    require(any(row.get('counter_generation') for row in measured), 'native counter generation missing')
    require(any(all(row.get(key) is not None for key in ('io_read_bytes', 'io_write_bytes'))
                for row in samples), 'native block-I/O counters unavailable')
    require(any(row.get('allocated_memory_bytes', 0) for row in allocation), 'native allocated-memory contract missing')
    if stage == 'final':
        require(all(row.get('state') == 'available' for row in allocation), 'resource actual stop was not retained')
        gaps = read_rows(data_dir, 'gaps')
        require(any(row.get('reason') == 'final_tail_missing' and row.get('object_id') in objects for row in gaps),
                'lost post-stop counter tail was not explicit')
        require(any(row.get('phase') == 'pre_stop' and row.get('final_complete') is False for row in samples),
                'native pre-stop boundary missing')
    print(json.dumps(dict(check='resource-owner-observer', kind=kind, stage=stage, passed=True,
                          native_counters=True, explicit_association=True, sqlite_commit=True), sort_keys=True))


def offline_reader(data_dir, kind):
    # Fault a private reader copy, never its native owner or the service's cursor.
    original = configs(data_dir, kind)[-1]
    before = json.dumps(original, sort_keys=True)
    sources.fetch(original)
    with tempfile.TemporaryDirectory(prefix='telemetry-offline-reader-', dir='/tmp') as name:
        root = Path(name)
        root.chmod(0o700)
        probe = json.loads(json.dumps(original))
        probe['source_id'] = 'offline-probe-' + hashlib.sha256(name.encode()).hexdigest()
        probe['last_seen'] = datetime.datetime.now(datetime.timezone.utc).isoformat()
        probe['offline'] = False
        probe['associations'] = {}
        seen, cursor = probe['last_seen'], probe['cursor']
        if kind == 'android':
            probe['transport'] = dict(control=str(root/'control.sock'), capability=original['transport']['capability'])
        else:
            # A missing public half makes the real --observe helper fail locally,
            # even when its yard has a higher-priority route registry entry.
            key = root/'unavailable-key'
            key.write_bytes(b'unavailable fixture identity\n')
            key.chmod(0o600)
            probe['transport']['identity'] = str(key)
        path = records.private_directory(root/'sources')/'probe.json'
        records.atomic_json(path, probe)
        require(sources.poll(root), 'isolated offline reader failed to retain state')
        failed = records.read_private(path)
        require(failed['offline'] and failed['cursor'] == cursor and failed['last_seen'] == seen,
                'offline observer changed its cursor or last successful observation')
        rows = [records.read_private(path) for path in (root/'outbox').glob('*.json')]
        require(len(rows) == 1, 'offline observer did not emit exactly one gap')
        gap = rows[0]
        require(gap['kind'] == 'gap' and gap['body'].get('reason') == 'source_offline'
                and gap['body'].get('stale') is True and gap['body'].get('start_time') == seen,
                'offline gap lost its last-seen boundary')
        require(records.deliver(data_dir, gap), 'offline observer gap did not commit')
        with contextlib.closing(sqlite3.connect((Path(data_dir)/'telemetry.sqlite').as_uri()+'?mode=ro', uri=True, timeout=2)) as db:
            row = db.execute('SELECT body FROM records WHERE source_id=? AND event_id=?',
                             (probe['source_id'], gap['event_id'])).fetchone()
            stored = json.loads(row[0]) if row else {}
            require(stored.get('reason') == 'source_offline' and stored.get('stale') is True
                    and datetime.datetime.fromisoformat(stored['start_time']) == datetime.datetime.fromisoformat(seen),
                    'offline gap or last-seen boundary missing in SQLite')
        failed['transport'] = original['transport']
        records.atomic_json(path, failed)
        require(sources.poll(root), 'restored isolated reader could not poll its real owner')
        recovered = records.read_private(path)
        require(not recovered['offline'] and recovered['last_seen'] != seen, 'real reader did not recover')
    require(json.dumps(original, sort_keys=True) == before, 'offline probe changed the real reader configuration')
    print(json.dumps(dict(check='resource-observer-offline', kind=kind, passed=True,
                          cursor_preserved=True, last_seen_preserved=True, gap_committed=True, real_reader_recovered=True)))


def android_exercise(args):
    # The owning fixture already prepared an image. This uses the ordinary native
    # client so the managed context traverses the actual acquire hook.
    workload = 'import time; time.sleep(15)'
    command = [args.engine, '-Y', args.yard, 'emu', 'run', '--purpose', 'telemetry-resource-proof',
               '--', sys.executable, '-c', workload]
    process = subprocess.Popen(command, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        deadline = time.monotonic() + 1500
        observed = False
        while process.poll() is None and time.monotonic() < deadline:
            sources.poll(args.data_dir)
            measured = False
            try:
                assert_records(args.data_dir, 'android', 'held')
                measured = True
            except (Failure, OSError, ValueError, sqlite3.Error):
                pass
            if measured and not observed:
                offline_reader(args.data_dir, 'android')
                observed = True
            time.sleep(1)
        require(process.poll() is not None, 'native Android exercise deadline')
        require(process.returncode == 0, 'native Android exercise failed')
        require(observed, 'Android exercise never produced measured counters')
    finally:
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--kind', choices=['vm', 'android'], required=True)
    parser.add_argument('--stage', choices=['held', 'final', 'exercise', 'storage-failure'], required=True)
    parser.add_argument('--data-dir', required=True)
    parser.add_argument('--engine')
    parser.add_argument('--yard')
    parser.add_argument('--source')
    parser.add_argument('--evidence', type=Path)
    parser.add_argument('--native-command', action='store_true', help='Run all following argv as the native lifecycle command')
    argv = list(sys.argv[1:] if argv is None else argv)
    boundary = argv.index('--native-command') if '--native-command' in argv else len(argv)
    args = parser.parse_args(argv[:boundary])
    args.native_command = argv[boundary+1:] if boundary < len(argv) else None
    if boundary < len(argv) and (not args.native_command or args.native_command == ['--']):
        parser.error('--native-command requires a command')
    try:
        require(os.environ.get('SUBYARD_E2E_VM') == '1' and Path('/run/subyard-e2e-lease.json').is_file(),
                'requires allocated disposable guest')
        require(bool(os.environ.get('SUBYARD_AGENT_RUN_ID')), 'requires explicit managed-run context')
        if args.stage == 'storage-failure':
            require(args.evidence and args.native_command, 'storage proof needs evidence and native lifecycle argv')
            require(bool(os.environ.get('SUBYARD_TELEMETRY_FIXTURE_ASSETS')), 'storage proof needs prepared fixture assets')
            binary = Path(os.environ['SUBYARD_TELEMETRY_FIXTURE_ASSETS'])/'subyard-telemetry'
            require(binary.is_file() and os.access(binary, os.X_OK), 'prepared telemetry executable is missing')
            subprocess.run([sys.executable, '-B', str(Path(__file__).with_name('storage-failure.py')),
                            '--binary', str(binary),
                            '--evidence', str(args.evidence), '--native-command', *args.native_command], check=True)
            print(json.dumps(dict(check='resource-owner-storage-failure', kind=args.kind, passed=True)))
            return 0
        if args.stage == 'exercise':
            require(args.kind == 'android' and args.engine and args.yard, 'Android exercise needs native candidate and yard')
            android_exercise(args)
        capture(args.data_dir, args.kind)
        assert_records(args.data_dir, args.kind, 'final' if args.stage == 'exercise' else args.stage)
        if args.stage == 'held':
            offline_reader(args.data_dir, args.kind)
        return 0
    except Failure as exc:
        print('resource-owner-observer: ' + str(exc), file=sys.stderr)
        return 1
    except (OSError, ValueError, KeyError, TypeError, sqlite3.Error, subprocess.SubprocessError) as error:
        print('resource-owner-observer: required native evidence unavailable ' +
              json.dumps(failure_details(error), sort_keys=True), file=sys.stderr)
        return 1


if __name__ == '__main__':
    raise SystemExit(main())
