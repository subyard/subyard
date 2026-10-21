"""Private per-lifecycle scalar observation collection; no product or GUI imports."""
import base64
import hashlib
import json
import os
from pathlib import Path
import stat
import time

def _private_dir(path):
    path.mkdir(mode=0o700)
    path.chmod(0o700)

TRACE_ENV = 'SUBYARD_VERANDA_TRACE'
CORRELATION_ENV = 'SUBYARD_VERANDA_CORRELATION_CONFIG'
OBSERVATION_ENV = 'SUBYARD_VERANDA_OBSERVATION_CONFIG'
TRACE_LIMIT = 8 * 1024 * 1024
TRACE_SCENARIOS = {'local', 'connect', 'remote-restore', 'local-restore', 'owner-focus',
                   'owner-focus-connect', 'owner-focus-restore', 'ime', 'ime-restore', 'profile-diagnostic'}
TRACE_COUNTS = {'session_sources', 'received', 'eligible', 'inventory_eligible', 'operation_eligible',
                'view_starts', 'gui_start_attempts', 'ignored', 'retired', 'unknown',
                'rpc_admission_attempts', 'cache_reads', 'overflow', 'duplicate', 'unknown_retirement',
                'rejected_batch_tickets', 'dropped_records', 'completed', 'duplicate_completion',
                'dropped_terminals', 'pending'}


def validate_trace(value):
    """Validate the sealed R74 scalar schema, not arbitrary JSON or UI strings."""
    if type(value) is dict and value.get('schema_version') == 3:
        return validate_usage_trace(value)
    if type(value) is dict and value.get('schema_version') == 2:
        return validate_correlation_trace(value)
    def shape(row, keys):
        assert type(row) is dict and set(row) == set(keys)
    def number(value, signed=False):
        assert type(value) is int and (-(1 << 63) <= value < (1 << 63) if signed else 0 <= value < (1 << 64))
    def boolean(value): assert type(value) is bool
    def enum(value, choices): assert type(value) is str and value in choices
    def optional(value, check):
        if value is not None: check(value)
    shape(value, {'schema_version', 'app_run_returned', 'exit_success', 'run_lifecycle', 'snapshot'})
    assert type(value['schema_version']) is int and value['schema_version'] == 1
    assert value['app_run_returned'] is True and value['exit_success'] is True
    assert value['run_lifecycle'] == 'ObservedClosed'
    snapshot = value['snapshot']
    shape(snapshot, {'schema_version', 'context', 'trace_window', 'lifecycle', 'worker_cleanup',
                     'producer_quiescence', 'counts', 'records', 'terminals'})
    assert type(snapshot['schema_version']) is int and snapshot['schema_version'] == 1
    number(snapshot['context']); assert snapshot['context'] > 0
    assert snapshot['trace_window'] == 'Sealed' and snapshot['worker_cleanup'] == 'ObservedClosed'
    assert snapshot['producer_quiescence'] == 'Unknown'  # Sealing is not a producer-drain claim.
    lifecycle = snapshot['lifecycle']
    shape(lifecycle, {'activations', 'workers_started', 'workers_closed', 'unmatched_closed', 'untracked_workers'})
    for item in lifecycle.values(): number(item)
    assert lifecycle['activations'] > 0 and lifecycle['workers_started'] > 0
    assert lifecycle['workers_started'] == lifecycle['workers_closed']
    assert lifecycle['unmatched_closed'] == lifecycle['untracked_workers'] == 0
    counts = snapshot['counts']; shape(counts, TRACE_COUNTS)
    for item in counts.values(): number(item)
    assert counts['pending'] == 0
    records = snapshot['records']; terminals = snapshot['terminals']
    assert type(records) is list and len(records) <= 8192
    assert type(terminals) is list and len(terminals) <= 8192
    for row in records:
        shape(row, {'source', 'ticket', 'request', 'elapsed_micros', 'phase'})
        for name in ('source', 'ticket', 'request', 'elapsed_micros'): number(row[name])
        phase = row['phase']
        if type(phase) is str: enum(phase, {'Overflow', 'Duplicate'})
        else:
            assert type(phase) is dict and len(phase) == 1
            name, item = next(iter(phase.items()))
            choices = {'Source': {'Local', 'Remote'}, 'Received': {'RpcEvent', 'LocalRequest', 'LoadedSelection'},
                       'Eligible': {'Inventory', 'Operation', 'View'}, 'Read': {'Rpc', 'InitialCache', 'Sealed'},
                       'Retired': {'Ignored', 'Unknown', 'SessionClosed', 'Superseded'}}
            assert name in choices; enum(item, choices[name])
    proof = []; seen = set(); painted = 0; event_samples = 0
    for row in terminals:
        shape(row, {'context', 'source', 'source_kind', 'origin', 'gui_view', 'gui_remote', 'coverage',
                    'ticket', 'kind', 'effect', 'elapsed_micros', 'started_micros', 'outcome'})
        for name in ('context', 'source', 'ticket'): number(row[name])
        assert row['context'] == snapshot['context'] and row['ticket'] > 0 and row['ticket'] not in seen
        seen.add(row['ticket'])
        enum(row['origin'], {'RpcSession', 'Gui'}); enum(row['kind'], {'RpcEvent', 'LocalRequest', 'LoadedSelection'})
        optional(row['effect'], lambda item: enum(item, {'Inventory', 'Operation', 'View'}))
        optional(row['source_kind'], lambda item: enum(item, {'Local', 'Remote'}))
        optional(row['gui_view'], number); optional(row['gui_remote'], boolean)
        optional(row['elapsed_micros'], number); optional(row['started_micros'], number)
        if row['origin'] == 'Gui':
            assert row['source'] == 0 and row['source_kind'] is None
            assert row['gui_view'] is not None and row['gui_view'] > 0 and type(row['gui_remote']) is bool
        else:
            assert row['source'] > 0 and row['source_kind'] in ('Local', 'Remote')
            assert row['gui_view'] is None and row['gui_remote'] is None
        coverage = row['coverage']
        if coverage is not None:
            shape(coverage, {'view', 'scope', 'generation', 'invalidation', 'parts', 'whole', 'cached', 'remote'})
            for name in ('view', 'scope', 'generation', 'invalidation', 'parts'): number(coverage[name])
            assert coverage['parts'] <= 255
            for name in ('whole', 'cached', 'remote'): boolean(coverage[name])
        outcome = row['outcome']; assert type(outcome) is dict and len(outcome) == 1
        if set(outcome) == {'Retired'}:
            enum(outcome['Retired'], {'Ignored', 'Unknown', 'SessionClosed', 'Superseded'})
            assert row['elapsed_micros'] is None
        else:
            shape(outcome, {'Paint'}); paint = outcome['Paint']; shape(paint, {'revision', 'clock', 'frame'})
            number(paint['revision']); number(paint['clock']); number(paint['frame'], signed=True)
            assert paint['revision'] > 0 and paint['clock'] > 0 and paint['frame'] >= 0
            assert row['elapsed_micros'] is not None and row['started_micros'] is not None
            painted += 1
            if row['origin'] == 'RpcSession' and row['kind'] == 'RpcEvent' and row['effect'] == 'Inventory': event_samples += 1
            if (row['origin'] == 'Gui' and row['kind'] == 'LocalRequest' and row['effect'] == 'View'
                    and coverage is not None and coverage['whole'] is True and coverage['cached'] is False
                    and coverage['parts'] & 1 and coverage['view'] == row['gui_view']
                    and coverage['remote'] == row['gui_remote']): proof.append(row['ticket'])
    assert painted <= counts['completed'] and proof
    return {'painted_gui_requests': len(proof), 'event_paint_samples': event_samples,
            'records': len(records), 'terminals': len(terminals), 'retired_unknown': counts['unknown'],
            'dropped_records': counts['dropped_records'], 'dropped_terminals': counts['dropped_terminals'],
            'worker_cleanup': snapshot['worker_cleanup'], 'producer_quiescence': snapshot['producer_quiescence'],
            'event_latency_claimed': False}


def strict_json(data):
    def pairs(items):
        value = {}
        for key, item in items:
            assert key not in value; value[key] = item
        return value
    return json.loads(data.decode('utf-8'), object_pairs_hook=pairs,
                      parse_constant=lambda _: (_ for _ in ()).throw(ValueError()))


def scalar_number(value, positive=False):
    assert type(value) is int and (1 if positive else 0) <= value < (1 << 64)


def validate_ready(value):
    assert type(value) is dict and set(value) == {'schema_version', 'context', 'freeze_micros', 'source_frontier', 'bindings'}
    assert type(value['schema_version']) is int and value['schema_version'] == 1
    scalar_number(value['context'], True); scalar_number(value['freeze_micros']); scalar_number(value['source_frontier'], True)
    assert type(value['bindings']) is list and 0 < len(value['bindings']) <= 128
    roles = set()
    for row in value['bindings']:
        assert type(row) is dict and set(row) == {'cohort', 'context', 'source', 'role'}
        for key in ('cohort', 'context', 'source'): scalar_number(row[key], True)
        assert row['context'] == value['context'] and row['role'] in ('Fleet', 'Details')
        assert row['source'] <= value['source_frontier']
        identity = (row['cohort'], row['role']); assert identity not in roles; roles.add(identity)
    return value


def validate_correlation_trace(value):
    assert set(value) == {'schema_version', 'app_run_returned', 'exit_success', 'run_lifecycle', 'snapshot', 'correlation_run'}
    assert type(value['schema_version']) is int and value['schema_version'] == 2
    core = value['snapshot']; assert type(core) is dict and 'correlation' in core
    plain = dict(value, schema_version=1, snapshot={key: item for key, item in core.items() if key != 'correlation'})
    del plain['correlation_run']; result = validate_trace(plain)
    correlation = core['correlation']
    assert type(correlation) is dict and set(correlation) == {'counts', 'index'}
    counts = correlation['counts']
    assert type(counts) is dict and set(counts) == {'valid_raw', 'tagged', 'other', 'unclassifiable', 'duplicate', 'admitted', 'admission_drops', 'index_drops', 'classification_pending', 'classification_unknown'}
    for number in counts.values(): scalar_number(number)
    assert counts['classification_pending'] == 0
    assert type(correlation['index']) is list and len(correlation['index']) <= 8192
    terminals = {row['ticket']: row for row in core['terminals']}
    for row in correlation['index']:
        assert type(row) is dict and set(row) == {'context', 'source', 'source_kind', 'sequence', 'started_micros', 'ticket', 'tag', 'admission', 'classification'}
        for key in ('context', 'source', 'sequence'): scalar_number(row[key], True)
        assert row['context'] == core['context'] and row['source_kind'] in ('Local', 'Remote')
        assert row['source'] <= core['counts']['session_sources']
        for key in ('started_micros', 'ticket'):
            if row[key] is not None: scalar_number(row[key], key == 'ticket')
        assert row['admission'] in ('Admitted', 'Duplicate', 'Overflow') and row['classification'] in ('Complete', 'Unknown')
        if row['admission'] == 'Admitted':
            assert row['ticket'] is not None
            if row['ticket'] in terminals:
                terminal = terminals[row['ticket']]
                assert terminal['origin'] == 'RpcSession' and terminal['source'] == row['source'] and terminal['source_kind'] == row['source_kind']
            else: assert core['counts']['dropped_terminals'] > 0
        else: assert row['ticket'] is None
        tag = row['tag']
        if type(tag) is dict:
            assert set(tag) == {'Matched'}; scalar_number(tag['Matched'], True)
        else: assert tag in ('Other', 'Unclassifiable')
    run = value['correlation_run']
    assert type(run) is dict and set(run) == {'schema_version', 'phase', 'bindings', 'writer', 'ready', 'cohorts_complete', 'replacement_unknown'}
    assert type(run['schema_version']) is int and run['schema_version'] == 1
    assert run['writer'] in ('Pending', 'Written', 'Failed', 'Stopped')
    assert run['phase'] in ('Cold', 'Warming', 'Frozen')
    assert type(run['bindings']) is list and len(run['bindings']) <= 128
    if run['bindings']:
        validate_ready({'schema_version': 1, 'context': core['context'], 'freeze_micros': 0, 'source_frontier': core['counts']['session_sources'], 'bindings': run['bindings']})
    if run['phase'] == 'Cold': assert not run['bindings']
    assert type(run['cohorts_complete']) is bool and type(run['replacement_unknown']) is bool
    if run['ready'] is not None:
        validate_ready(run['ready']); assert run['ready']['context'] == core['context']
        assert run['phase'] == 'Frozen' and run['ready']['bindings'] == run['bindings']
    qualified = run['writer'] == 'Written' and run['ready'] is not None and run['cohorts_complete'] and not run['replacement_unknown'] and run['ready']['source_frontier'] == core['counts']['session_sources']
    return dict(result, correlation_readiness=qualified, correlation_counts=dict(counts), event_latency_claimed=False)


def validate_correlation_template(value):
    assert type(value) is dict and set(value) == {'schema', 'offers', 'cohorts'} and type(value['schema']) is int and value['schema'] == 2
    def text(value, limit): assert type(value) is str and 0 < len(value) <= limit and all(33 <= ord(c) <= 126 for c in value)
    assert type(value['offers']) is list and 0 < len(value['offers']) <= 2048
    offers = set(); mappings = set()
    for row in value['offers']:
        assert type(row) is dict and set(row) == {'offer', 'action', 'source'}
        scalar_number(row['offer'], True); text(row['action'], 128); text(row['source'], 2048); assert row['source'].startswith('/')
        assert row['offer'] not in offers and (row['action'], row['source']) not in mappings
        offers.add(row['offer']); mappings.add((row['action'], row['source']))
    assert type(value['cohorts']) is list and 0 < len(value['cohorts']) <= 64
    cohorts = set(); targets = set()
    for row in value['cohorts']:
        assert type(row) is dict and set(row) == {'ordinal', 'owner', 'connection', 'yard'}
        scalar_number(row['ordinal'], True); text(row['owner'], 128)
        for key in ('connection', 'yard'):
            if row[key] is not None: text(row[key], 128)
        assert row['ordinal'] not in cohorts and (row['connection'], row['yard']) not in targets
        cohorts.add(row['ordinal']); targets.add((row['connection'], row['yard']))
    return value


def validate_usage_trace(value):
    fields = {'schema_version', 'app_run_returned', 'exit_success', 'run_lifecycle', 'snapshot', 'correlation_run'}
    assert type(value) is dict and set(value) in (fields, fields | {'renderer_observation'})
    assert type(value['schema_version']) is int and value['schema_version'] == 3
    core = value['snapshot']; assert type(core) is dict and 'child_usage' in core
    plain = dict(value, schema_version=2 if value['correlation_run'] is not None else 1,
                 snapshot={key: item for key, item in core.items() if key != 'child_usage'})
    plain.pop('renderer_observation', None)
    if value['correlation_run'] is None: del plain['correlation_run']
    summary = validate_trace(plain)
    usage = core['child_usage']
    assert type(usage) is dict and set(usage) == {'attempts', 'dropped', 'overflow', 'records'}
    for key in ('attempts', 'dropped'): scalar_number(usage[key])
    causes = ('SpawnFailed', 'CaptureFailed', 'WaitFailed', 'Unsupported', 'Unfinished', 'MissingCapture', 'Overflow')
    assert usage['overflow'] is None or usage['overflow'] in causes
    assert type(usage['records']) is list and len(usage['records']) <= 8192
    assert usage['attempts'] == len(usage['records']) + usage['dropped']
    fields = {'context', 'source', 'source_kind', 'provenance', 'kind', 'attempt', 'pre_spawn_micros', 'post_owning_wait_micros',
              'interval_start_micros', 'interval_end_micros', 'owned_pid', 'terminal_pid', 'terminal_code', 'terminal_status',
              'usage', 'raw_wait_status', 'returncode', 'signal', 'captures', 'reaps', 'state', 'unknown', 'error_number'}
    kernel = set('utime_sec utime_usec stime_sec stime_usec maxrss_kib ixrss idrss isrss minflt majflt nswap inblock oublock msgsnd msgrcv nsignals nvcsw nivcsw'.split())
    attempts = set(); complete = usage['dropped'] == 0 and usage['overflow'] is None
    for row in usage['records']:
        assert type(row) is dict and set(row) == fields
        for key in ('context', 'provenance', 'attempt'): scalar_number(row[key], True)
        assert row['context'] == core['context'] and row['attempt'] not in attempts and row['attempt'] <= usage['attempts']; attempts.add(row['attempt'])
        if row['source'] is None: assert row['source_kind'] is None
        else:
            scalar_number(row['source'], True); assert row['source'] <= core['counts']['session_sources'] and row['source_kind'] in ('Local', 'Remote')
        assert row['kind'] in ('RpcSession', 'ProvisionalRpc', 'HostKeyScan', 'HostKeyFingerprint', 'AgentReadiness', 'EditorExtensionProbe', 'ShortTool')
        assert row['state'] in ('Pending', 'Captured', 'Unknown') and (row['unknown'] is None or row['unknown'] in causes)
        for key in ('pre_spawn_micros', 'post_owning_wait_micros', 'interval_start_micros', 'interval_end_micros', 'owned_pid', 'terminal_pid'):
            if row[key] is not None: scalar_number(row[key], key.endswith('pid'))
        for key in ('terminal_code', 'terminal_status', 'raw_wait_status', 'returncode', 'signal', 'error_number'):
            assert row[key] is None or (type(row[key]) is int and -(1 << 31) <= row[key] < (1 << 31))
        for key in ('captures', 'reaps'): scalar_number(row[key]); assert row[key] <= 1
        if row['usage'] is not None:
            assert type(row['usage']) is dict and set(row['usage']) == kernel
            for number in row['usage'].values(): assert type(number) is int and 0 <= number < (1 << 63)
        qualified = (row['state'] == 'Captured' and row['unknown'] is None and row['captures'] == row['reaps'] == 1
                     and row['owned_pid'] is not None and row['owned_pid'] == row['terminal_pid'] and row['usage'] is not None
                     and row['interval_start_micros'] is not None and row['interval_end_micros'] is not None
                     and row['interval_start_micros'] <= row['interval_end_micros']
                     and row['interval_start_micros'] == row['pre_spawn_micros'] and row['post_owning_wait_micros'] is not None
                     and row['post_owning_wait_micros'] <= row['interval_end_micros'] <= row['post_owning_wait_micros'] + 1
                     and row['error_number'] is None)
        complete = complete and qualified
    renderer = value.get('renderer_observation'); renderer_status = 'UNKNOWN'
    if renderer is not None:
        assert type(renderer) is dict and set(renderer) == {'records', 'dropped'}
        scalar_number(renderer['dropped']); assert type(renderer['records']) is list and len(renderer['records']) <= 64
        ordinals = set()
        for row in renderer['records']:
            assert type(row) is dict and set(row) == {'ordinal', 'renderer', 'backend'}
            scalar_number(row['ordinal'], True); assert row['ordinal'] not in ordinals; ordinals.add(row['ordinal'])
            assert row['renderer'] in ('Cairo', 'GL', 'Vulkan', 'Other', 'Unknown') and row['backend'] in ('Wayland', 'X11', 'Unknown')
        if renderer['records'] and renderer['dropped'] == 0 and all(row['renderer'] != 'Unknown' and row['backend'] != 'Unknown' for row in renderer['records']): renderer_status = 'observed'
    return dict(summary, child_usage_status='recorded' if complete else 'UNKNOWN', child_usage_attempts=usage['attempts'], child_usage_dropped=usage['dropped'], child_usage_clock='RustObservationRelativeMicros', family_coverage='UNKNOWN', renderer_observation_status=renderer_status, hardware_acceleration='unmeasured')


def python_usage_status(value):
    assert type(value) is dict and set(value) == {'attempts','overflow','clock','origin_ns','coverage','records'}
    for key in ('attempts','overflow','origin_ns'): scalar_number(value[key])
    assert value['clock'] == 'PythonMonotonicNs' and value['coverage'] in ('UNKNOWN','RECORDED_ATTEMPTS')
    assert type(value['records']) is list and len(value['records']) <= 64
    fields = set('attempt pre_spawn_ns owned_pid terminal_pid terminal_code terminal_status raw_wait_status returncode post_owning_wait_ns capture_count reap_count state reason capture_errno interval_start_micros interval_end_micros'.split())
    fields |= {'ru_'+key for key in 'utime_sec utime_usec stime_sec stime_usec maxrss_kib ixrss idrss isrss minflt majflt nswap inblock oublock msgsnd msgrcv nsignals nvcsw nivcsw'.split()}
    complete = value['coverage'] == 'RECORDED_ATTEMPTS' and value['overflow'] == 0 and value['attempts'] == len(value['records']) and value['attempts'] > 0
    attempts = set()
    for row in value['records']:
        assert type(row) is dict and set(row) == fields
        scalar_number(row['attempt'],True); assert row['attempt'] not in attempts and row['attempt'] <= value['attempts']; attempts.add(row['attempt'])
        assert row['state'] in ('UNKNOWN','CAPTURED') and row['reason'] in ('not_spawned','spawn_or_exec_failed_internal_child_unknown','awaiting_terminal','capture_failed','terminal_captured','owning_wait_failed','unsupported_runtime_or_abi')
        for key,number in row.items():
            if key in ('state','reason') or number is None: continue
            assert type(number) is int and -(1<<63) <= number < (1<<64)
        start=row['interval_start_micros'];end=row['interval_end_micros'];before=row['pre_spawn_ns'];after=row['post_owning_wait_ns']
        good = (row['state']=='CAPTURED' and row['capture_count']==row['reap_count']==1 and row['owned_pid'] is not None and row['owned_pid']==row['terminal_pid']
                and type(start) is int and type(end) is int and type(after) is int and type(before) is int and start>=0 and end>=start
                and value['origin_ns']+1000*start <= before < value['origin_ns']+1000*(start+1)
                and value['origin_ns']+1000*(end-1) < after <= value['origin_ns']+1000*end
                and all(type(number) is int and number >= 0 for key,number in row.items() if key.startswith('ru_'))
                and row['capture_errno'] is None and type(row['raw_wait_status']) is int)
        complete = complete and good
    return 'recorded' if complete else 'UNKNOWN'


class TraceObservations:
    """Declared lifecycle destinations and post-exit scalar evidence only."""
    def __init__(self, state, scenarios, correlation=None, usage_capacity=None):
        assert 0 < len(scenarios) <= 3 and len(scenarios) == len(set(scenarios)) and set(scenarios) <= TRACE_SCENARIOS
        self.parent = state / 'observations'; _private_dir(self.parent)
        self.identity = self.parent.stat(); self.scenarios = tuple(scenarios)
        self.destinations = {}; self.collected = set(); self.payloads = {}
        self.correlation = None; self.ready_destinations = {}; self.ready_payloads = {}; self.ready_failures = set()
        assert usage_capacity is None or (type(usage_capacity) is int and 0 < usage_capacity <= 8192)
        self.usage_capacity = usage_capacity
        if correlation is not None:
            path = Path(correlation)
            assert path.is_absolute() and '..' not in path.parts and path.parent.resolve() == path.parent
            self.correlation = validate_correlation_template(strict_json(self._read(path, 512 * 1024)))

    def app_env(self, env, scenario):
        assert scenario in self.scenarios and scenario not in self.destinations
        path = self.parent / ('trace-' + scenario + '.json')
        assert not path.exists() and not path.is_symlink()
        self.destinations[scenario] = path
        result = dict(env, **{TRACE_ENV: str(path)})
        if self.correlation is not None or self.usage_capacity is not None:
            config = self.parent / ('config-' + scenario + '.json')
            correlation = None
            if self.correlation is not None:
                ready = self.parent / ('ready-' + scenario + '.json')
                assert not ready.exists() and not ready.is_symlink()
                self.ready_destinations[scenario] = ready
                correlation = dict(offers=self.correlation['offers'], cohorts=self.correlation['cohorts'], ready=str(ready))
            value = dict(schema=3, child_usage={'capacity': self.usage_capacity}, correlation=correlation) if self.usage_capacity is not None else dict(schema=2, **correlation)
            data = json.dumps(value, separators=(',', ':')).encode('utf-8')
            assert len(data) <= 512 * 1024
            fd = os.open(config, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
            with os.fdopen(fd, 'wb') as output:
                os.fchmod(output.fileno(), 0o600); assert output.write(data) == len(data); output.flush(); os.fsync(output.fileno())
            result[OBSERVATION_ENV if self.usage_capacity is not None else CORRELATION_ENV] = str(config)
        return result

    def poll_ready(self, scenario, app, deadline):
        if self.correlation is None or scenario in self.ready_payloads or scenario in self.ready_failures: return None
        path = self.ready_destinations[scenario]
        if not path.exists() and not path.is_symlink(): return None
        return self.wait_ready(scenario, app, deadline)

    def _ready(self, scenario):
        assert scenario in self.ready_destinations
        ready = validate_ready(strict_json(self._read(self.ready_destinations[scenario], 65536, observation=True)))
        expected = {(row['ordinal'], 'Fleet') for row in self.correlation['cohorts']}
        expected |= {(row['ordinal'], 'Details') for row in self.correlation['cohorts'] if row['yard'] is not None}
        assert {(row['cohort'], row['role']) for row in ready['bindings']} == expected
        return ready

    def wait_ready(self, scenario, app, deadline):
        """Only a successful live handshake may release a future producer. No producer here."""
        if scenario in self.ready_payloads or scenario in self.ready_failures:
            return {'status': 'UNKNOWN', 'reason': 'readiness_duplicate_or_failed'}
        try:
            while time.monotonic() < deadline:
                assert app.poll() is None
                try: ready = self._ready(scenario)
                except FileNotFoundError: time.sleep(min(0.02, max(0, deadline - time.monotonic()))); continue
                assert time.monotonic() < deadline and app.poll() is None
                self.ready_payloads[scenario] = ready
                return {'status': 'ready', 'context': ready['context'], 'freeze_micros': ready['freeze_micros']}
        except (OSError, AssertionError, ValueError, TypeError, KeyError, RecursionError): pass
        self.ready_failures.add(scenario)
        return {'status': 'UNKNOWN', 'reason': 'readiness_unavailable'}

    def _read(self, path, limit, observation=False):
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            parent = os.fstat(directory)
            assert stat.S_ISDIR(parent.st_mode) and parent.st_uid == os.getuid() and stat.S_IMODE(parent.st_mode) == 0o700
            if observation: assert os.path.samestat(parent, self.identity)
            fd = os.open(path.name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory)
            try:
                before = os.fstat(fd)
                assert stat.S_ISREG(before.st_mode) and before.st_uid == os.getuid() and before.st_nlink == 1
                assert stat.S_IMODE(before.st_mode) == 0o600 and before.st_size <= limit
                data = bytearray()
                while len(data) <= limit:
                    block = os.read(fd, min(65536, limit + 1 - len(data)))
                    if not block: break
                    data.extend(block)
                after = os.fstat(fd)
                assert len(data) == before.st_size and len(data) <= limit
                assert (before.st_dev, before.st_ino, before.st_mode, before.st_uid, before.st_nlink,
                        before.st_size, before.st_mtime_ns, before.st_ctime_ns) == (
                       after.st_dev, after.st_ino, after.st_mode, after.st_uid, after.st_nlink,
                       after.st_size, after.st_mtime_ns, after.st_ctime_ns)
                return bytes(data)
            finally: os.close(fd)
        finally: os.close(directory)

    def collect(self, scenario, lifetime, app, app_log):
        if scenario not in self.destinations or scenario in self.collected:
            return {'status': 'UNKNOWN', 'reason': 'undeclared_or_duplicate'}
        if lifetime.get('gui_original_exit_code') != 0 or app.returncode != 0 or lifetime.get('input_exit_code') != 0:
            return {'status': 'UNKNOWN', 'reason': 'not_successfully_closed'}
        self.collected.add(scenario)
        try:
            log = self._read(app_log, 2 * 1024 * 1024)
            if b'Veranda trace: UNKNOWN' in log.splitlines():
                return {'status': 'UNKNOWN', 'reason': 'application_output_unknown'}
            data = self._read(self.destinations[scenario], TRACE_LIMIT, observation=True)
        except (OSError, AssertionError, ValueError):
            return {'status': 'UNKNOWN', 'reason': 'missing_or_unsafe'}
        try:
            def object_pairs(pairs):
                value = {}
                for key, item in pairs:
                    assert key not in value; value[key] = item
                return value
            value = json.loads(data.decode('utf-8'), object_pairs_hook=object_pairs,
                               parse_constant=lambda _: (_ for _ in ()).throw(ValueError()))
            assert type(value) is dict and value.get('schema_version') == (3 if self.usage_capacity is not None else 2 if self.correlation is not None else 1)
            summary = validate_trace(value)
            if self.usage_capacity is not None:
                assert (value['correlation_run'] is not None) == (self.correlation is not None)
            if self.correlation is not None:
                assert scenario in self.ready_payloads and scenario not in self.ready_failures
                ready = self._ready(scenario)
                assert ready == value['correlation_run']['ready'] and summary['correlation_readiness']
                assert self.ready_payloads[scenario] == ready
        except (OSError, ValueError, AssertionError, TypeError, KeyError, RecursionError):
            return {'status': 'UNKNOWN', 'reason': 'malformed_or_no_gui_paint'}
        digest = hashlib.sha256(data).hexdigest()
        self.payloads[scenario] = {'sha256': digest, 'bytes': len(data), 'base64': base64.b64encode(data).decode('ascii')}
        if self.usage_capacity is not None and summary['child_usage_status'] != 'recorded':
            return dict(status='UNKNOWN', reason='child_usage_incomplete', raw_retained=True,
                        app_lifecycle_status='passed', sha256=digest, bytes=len(data), **summary)
        return dict(status='passed', sha256=digest, bytes=len(data), **summary)

