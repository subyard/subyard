#!/usr/bin/env python3
"""Keep genuine Codex app-server sessions alive across a normal owner update.

Only the declared compatibility recipe is a fixture. Telemetry comes exclusively
from native Codex; this driver never submits manufactured OTLP events. Its JSONL
control replies contain counts/process identities, never user config or secrets.
"""
import http.server
import importlib.util
import json
import os
from pathlib import Path
import queue
import re
import subprocess
import sys
import tempfile
import threading
import time
import tomllib

STATE = Path('/var/lib/subyard/openlit')
LEGACY = Path('/var/lib/subyard/openlit-traces-workaround')
HOME = Path('/home/dev')
IMAGE = 'otel/opentelemetry-collector-contrib:0.162.0@sha256:39923a8e431bd1f57be82411999d389fcfe40857492e4365456d97a4c1f74be6'
TEMP = {'otlp-http': {'endpoint': 'http://127.0.0.1:4319/v1/traces', 'protocol': 'binary'}}
NATIVE = {'otlp-http': {'endpoint': 'http://127.0.0.1:4318/v1/traces', 'protocol': 'binary'}}
ATTRIBUTE = 'subyard.openlit.trace_cutover'
TIMEOUT = 45
CODEX_SERVICES = "('codex_cli_rs', 'codex_exec', 'codex-app-server')"


def run(arguments, timeout=15):
    result = subprocess.run(arguments, capture_output=True, text=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError('fixture command failed: '+arguments[0])
    return result.stdout.strip()


class Provider(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_POST(self):
        length = int(self.headers.get('Content-Length', '0'))
        if not 0 < length <= 1024*1024:
            self.send_error(413)
            return
        json.loads(self.rfile.read(length))
        item = {'id': 'msg_fixture', 'type': 'message', 'role': 'assistant',
                'content': [{'type': 'output_text', 'text': 'Fixture complete.', 'annotations': []}]}
        response = {'id': 'resp_fixture', 'status': 'completed', 'output': [item],
                    'usage': {'input_tokens': 10, 'output_tokens': 1, 'total_tokens': 11}}
        self.send_response(200)
        self.send_header('Content-Type', 'text/event-stream')
        self.send_header('Connection', 'close')
        self.end_headers()
        for event in [{'type': 'response.created', 'response': {'id': 'resp_fixture'}},
                      {'type': 'response.output_item.done', 'output_index': 0, 'item': item},
                      {'type': 'response.completed', 'response': response}]:
            self.wfile.write(('data: '+json.dumps(event)+'\n\n').encode())
        self.wfile.flush()


class App:
    def __init__(self, argv, env, cwd, **identity):
        self.stderr = tempfile.TemporaryFile(mode='w+')
        self.process = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                        stderr=self.stderr, text=True, env=env, cwd=cwd, **identity)
        self.frames = queue.Queue()
        self.serial = 0
        def reader():
            try:
                for line in self.process.stdout:
                    self.frames.put(json.loads(line))
            except (ValueError, OSError):
                self.frames.put({'invalid': True})
            finally:
                self.frames.put({'closed': True})
        threading.Thread(target=reader, daemon=True).start()
        try:
            self.request('initialize', {'clientInfo': {'name': 'subyard-migration-fixture', 'version': '1'},
                                        'capabilities': {'experimentalApi': True}})
            self.send({'method': 'initialized', 'params': {}})
            self.thread = self.request('thread/start', {'cwd': str(cwd)})['thread']['id']
        except BaseException:
            self.abort()
            raise

    def send(self, frame):
        self.process.stdin.write(json.dumps(frame)+'\n')
        self.process.stdin.flush()

    def read(self, deadline):
        remaining = deadline-time.monotonic()
        if remaining <= 0:
            raise RuntimeError('native app-server deadline expired')
        try:
            frame = self.frames.get(timeout=remaining)
        except queue.Empty:
            raise RuntimeError('native app-server deadline expired') from None
        if frame.get('closed') or frame.get('invalid'):
            raise RuntimeError('native app-server closed or emitted invalid JSON')
        return frame

    def request(self, method, params):
        self.serial += 1
        self.send({'id': self.serial, 'method': method, 'params': params})
        deadline = time.monotonic()+TIMEOUT
        while True:
            frame = self.read(deadline)
            if frame.get('id') == self.serial and 'method' not in frame:
                if 'error' in frame:
                    raise RuntimeError('native app-server rejected '+method)
                return frame['result']

    def turn(self):
        self.request('turn/start', {'threadId': self.thread,
                                  'input': [{'type': 'text', 'text': 'Say fixture complete.'}]})
        deadline = time.monotonic()+TIMEOUT
        while True:
            frame = self.read(deadline)
            if frame.get('method') == 'turn/completed':
                if frame['params']['turn']['status'] != 'completed':
                    raise RuntimeError('native turn did not complete')
                return

    def identity(self):
        if self.process.poll() is not None:
            raise RuntimeError('native backend exited unexpectedly')
        pid = self.process.pid
        start = int(Path(f'/proc/{pid}/stat').read_text().rsplit(')', 1)[1].split()[19])
        if Path(os.readlink(f'/proc/{pid}/exe')).name != 'codex':
            raise RuntimeError('backend is not the genuine native Codex executable')
        return {'pid': pid, 'start': start}

    def close(self):
        # EOF is normal app-server closure. A forced kill can only fail the proof.
        self.process.stdin.close()
        try:
            code = self.process.wait(timeout=20)
        except subprocess.TimeoutExpired:
            self.abort()
            raise RuntimeError('native backend did not close normally') from None
        self.close_streams()
        if code:
            raise RuntimeError('native backend failed on normal closure')

    def abort(self):
        if self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=5)
        self.close_streams()

    def close_streams(self):
        for stream in (self.process.stdin, self.process.stdout, self.stderr):
            if not stream.closed:
                stream.close()


class Proof:
    def __init__(self):
        self.old = None
        self.old_identity = None
        self.foreign = None
        self.container = None
        self.workspace = None
        self.fixed_point = None
        self.server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Provider)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def app(self):
        binary = '/usr/local/bin/codex'
        version = run(['runuser', '-u', 'dev', '--', binary, '--version'])
        if not re.fullmatch(r'codex-cli \d+\.\d+\.\d+(?:[+-][\w.-]+)?', version):
            raise RuntimeError('genuine Codex version is unavailable')
        base = 'http://127.0.0.1:'+str(self.server.server_port)+'/v1'
        argv = [binary, '-c', 'model_provider="fixture"', '-c', 'model="fixture"',
                '-c', 'model_providers.fixture.name="Local fixture"',
                '-c', 'model_providers.fixture.base_url="'+base+'"',
                '-c', 'model_providers.fixture.wire_api="responses"',
                '-c', 'model_providers.fixture.requires_openai_auth=false',
                # App-server defaults metrics off; keep this synthetic session's
                # default enabled without overriding explicit user configuration.
                'app-server', '--analytics-default-enabled']
        env = {'HOME': str(HOME), 'PATH': '/usr/local/bin:/usr/bin:/bin',
               'OTEL_METRIC_EXPORT_INTERVAL': '1000', 'OTEL_LOGS_EXPORT_INTERVAL': '1000'}
        return App(argv, env, self.workspace, user='dev', group='dev', extra_groups=[])

    def query(self, sql, deadline=None):
        timeout = 15 if deadline is None else min(15, deadline-time.monotonic())
        if timeout <= 0:
            raise subprocess.TimeoutExpired('signal query', 0)
        return run(['docker', 'compose', '--env-file', str(STATE/'database.env'), '-f', str(STATE/'compose.yaml'),
                    'exec', '-T', 'clickhouse', 'bash', '-c',
                    'exec clickhouse-client --user openlit --password "$CLICKHOUSE_PASSWORD" --database openlit --query "$1"',
                    'fixture', sql], timeout=timeout)

    def trace_count(self, native=False, deadline=None):
        predicate = "ResourceAttributes['"+ATTRIBUTE+"']=''"
        if native:
            record = json.loads((STATE/'trace-cutover.json').read_text())
            if not re.fullmatch('[0-9a-f]{32}', record['token']) or not isinstance(record['cutover_ns'], int):
                raise RuntimeError('invalid adoption correlation')
            predicate = "ResourceAttributes['"+ATTRIBUTE+"']='"+record['token']+"' AND Timestamp>fromUnixTimestamp64Nano("+str(record['cutover_ns'])+")"
        return int(self.query("SELECT count() FROM otel_traces WHERE ServiceName IN "+CODEX_SERVICES+" AND TraceId!='' AND "+predicate,
                              deadline=deadline))

    def wait_trace(self, previous, native=False):
        deadline = time.monotonic()+90
        while time.monotonic() < deadline:
            value = self.trace_count(native)
            if value > previous:
                return value
            time.sleep(2)
        raise RuntimeError('genuine native trace ingestion deadline expired')

    def running(self, container):
        return run(['docker', 'inspect', '--format', '{{.State.Running}}', container]) == 'true'

    def setup(self):
        if os.getuid() != 0 or LEGACY.exists() or LEGACY.is_symlink():
            raise RuntimeError('fixture requires root and an absent compatibility directory')
        ownership = json.loads((STATE/'native.json').read_text())
        if 'trace_exporter' in ownership['agents']['codex']:
            raise RuntimeError('predecessor already owns native trace configuration')
        path = HOME/'.codex/config.toml'
        obj = tomllib.loads(path.read_text())
        if 'trace_exporter' in obj.get('otel', {}):
            raise RuntimeError('refusing existing user trace leaf')
        spec = importlib.util.spec_from_file_location('fixture_tomli_w', STATE/'tomli_w.py')
        writer = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(writer)
        obj['model'] = 'migration-fixture-preserved'
        obj['otel']['trace_exporter'] = TEMP
        info = path.stat()
        path.write_text(writer.dumps(obj))
        os.chmod(path, 0o600)
        os.chown(path, info.st_uid, info.st_gid)
        # The baseline ownership ledger is produced by the real predecessor and
        # remains untouched; only this exact unowned leaf is fixture setup.
        if json.loads((STATE/'native.json').read_text()) != ownership:
            raise RuntimeError('predecessor native ownership changed during setup')
        LEGACY.mkdir(mode=0o700)
        password = (STATE/'database.env').read_text().removeprefix('OPENLIT_DB_PASSWORD=').strip()
        if not re.fullmatch('[0-9a-f]{64}', password):
            raise RuntimeError('unsupported private predecessor database binding')
        collector = {'receivers': {'otlp': {'protocols': {'http': {'endpoint': '0.0.0.0:4318'}}}},
                     'processors': {'batch': {}}, 'exporters': {'clickhouse': {
                         'endpoint': 'tcp://clickhouse:9000', 'database': 'openlit', 'username': 'openlit',
                         'password': password, 'traces_table_name': 'otel_traces'}},
                     'service': {'pipelines': {'traces': {'receivers': ['otlp'], 'processors': ['batch'], 'exporters': ['clickhouse']}}}}
        recipe = {'services': {'receiver': {
            'image': IMAGE, 'command': ['--config=/etc/otelcol-contrib/config.yaml'],
            'volumes': ['./collector.yaml:/etc/otelcol-contrib/config.yaml:ro'],
            'ports': ['127.0.0.1:4319:4318'], 'networks': ['backend'], 'restart': 'unless-stopped'}},
            'networks': {'backend': {'external': True, 'name': 'subyard-openlit_default'}}}
        for name, value, mode in [('compose.yaml', recipe, 0o600), ('collector.yaml', collector, 0o644)]:
            (LEGACY/name).write_text(json.dumps(value)+'\n')
            os.chmod(LEGACY/name, mode)
        compose = ['docker', 'compose', '-p', 'subyard-openlit-traces-workaround',
                   '--env-file', '/dev/null', '-f', str(LEGACY/'compose.yaml')]
        run([*compose, 'up', '-d', '--pull', 'never'], timeout=60)
        self.container = run([*compose, 'ps', '-q', 'receiver'])
        if not re.fullmatch('[a-f0-9]{64}', self.container) or not self.running(self.container):
            raise RuntimeError('compatibility receiver failed to start')
        self.workspace = Path(tempfile.mkdtemp(prefix='subyard-openlit-migration-', dir='/tmp'))
        os.chown(self.workspace, info.st_uid, info.st_gid)
        # A second same-image collector belongs to this fixture, but is outside
        # the reserved recipe/project. Automatic retirement must leave it alone.
        foreign = {'receivers': {'otlp': {'protocols': {'http': {'endpoint': '0.0.0.0:4318'}}}},
                   'exporters': {'debug': {}},
                   'service': {'pipelines': {'traces': {'receivers': ['otlp'], 'exporters': ['debug']}}}}
        foreign_config = self.workspace/'foreign.yaml'
        foreign_config.write_text(json.dumps(foreign))
        os.chmod(foreign_config, 0o644)
        self.foreign = run(['docker', 'run', '-d', '--pull=never', '--memory', '256m', '--network', 'none', '--mount',
                            'type=bind,src='+str(foreign_config)+',dst=/etc/otelcol-contrib/config.yaml,readonly',
                            IMAGE, '--config=/etc/otelcol-contrib/config.yaml'])
        deadline = time.monotonic()+30
        while time.monotonic() < deadline:
            try:
                self.trace_count()
                break
            except RuntimeError:
                time.sleep(1)
        else:
            raise RuntimeError('compatibility trace schema did not initialize')
        self.old = self.app()
        self.old_identity = self.old.identity()
        return {'backend': self.old_identity, 'compatibility_layout': 'declared-contract-only'}

    def old_turn(self):
        if self.old.identity() != self.old_identity or not self.running(self.container):
            raise RuntimeError('old native session or compatibility receiver changed')
        before = self.trace_count()
        self.old.turn()
        count = self.wait_trace(before)
        return {'old_backend_unchanged': True, 'unmarked_codex_traces': count}

    def adoption(self):
        value = json.loads((STATE/'trace-cutover.json').read_text())
        obj = tomllib.loads((HOME/'.codex/config.toml').read_text())
        owned = json.loads((STATE/'native.json').read_text())['agents']['codex']['trace_exporter']
        if value['phase'] != 'adopted' or value['container']['id'] != self.container or str(self.old_identity['pid']) not in value['processes'] or value['processes'][str(self.old_identity['pid'])] != self.old_identity['start']:
            raise RuntimeError('normal update did not fence the authentic old backend')
        if obj['otel']['trace_exporter'] != NATIVE or owned['managed'] != NATIVE or owned['present'] or obj['model'] != 'migration-fixture-preserved':
            raise RuntimeError('normal adoption lost native ownership or user customization')
        if not self.running(self.foreign):
            raise RuntimeError('foreign fixture container changed')
        return {'adopted': True, 'old_backend': self.old.identity()}

    def fresh_turn(self):
        before = self.trace_count(native=True)
        app = self.app()
        try:
            if app.identity() == self.old_identity:
                raise RuntimeError('fresh backend reused the old process')
            app.turn()
            app.close()
        except BaseException:
            app.abort()
            raise
        # Normal closure flushes the native exporters; ClickHouse ingestion can
        # still lag. All three signals share one deadline, including query time.
        deadline = time.monotonic()+90
        # Negative counts distinguish an unobserved signal from an observed zero.
        count, logs, metrics = before, -1, -1
        predicate = 'ServiceName IN '+CODEX_SERVICES
        while time.monotonic() < deadline:
            try:
                count = self.trace_count(native=True, deadline=deadline)
                logs = int(self.query('SELECT count() FROM otel_logs WHERE '+predicate, deadline=deadline))
                metrics = int(self.query('SELECT (SELECT count() FROM otel_metrics_sum WHERE '+predicate+
                                         ') + (SELECT count() FROM otel_metrics_histogram WHERE '+predicate+')',
                                         deadline=deadline))
            except subprocess.TimeoutExpired:
                pass
            if count > before and logs > 0 and metrics > 0 and time.monotonic() < deadline:
                return {'marked_codex_traces': count, 'codex_logs': logs, 'codex_metrics': metrics}
            remaining = deadline-time.monotonic()
            if remaining > 0:
                time.sleep(min(2, remaining))
        raise RuntimeError('genuine Codex signal ingestion deadline expired: marked_codex_traces='+str(count)+
                           ' previous_marked_codex_traces='+str(before)+' codex_logs='+str(logs)+' codex_metrics='+str(metrics))

    def retain(self):
        # Observe an actual automatic retry while old process is alive. No manual
        # start or direct cutover command substitutes for timer execution.
        deadline = time.monotonic()+150
        unit = 'subyard-openlit-trace-cutover.service'
        properties = ['Type', 'ActiveState', 'SubState', 'Result', 'InvocationID',
                      'ExecMainStartTimestampMonotonic', 'ExecMainExitTimestampMonotonic',
                      'ExecMainCode', 'ExecMainStatus']
        def observation():
            value = dict(line.split('=', 1) for line in run(
                ['systemctl', 'show', unit, '--property='+','.join(properties)]).splitlines())
            if set(value) != set(properties) or value['Type'] != 'oneshot':
                raise RuntimeError('automatic retention oneshot observation unavailable')
            for name in ('ExecMainStartTimestampMonotonic', 'ExecMainExitTimestampMonotonic', 'ExecMainCode', 'ExecMainStatus'):
                if not value[name].isdigit():
                    raise RuntimeError('invalid automatic retention invocation identity')
                value[name] = int(value[name])
            return value
        before = observation()['ExecMainStartTimestampMonotonic']
        invocation = None
        while time.monotonic() < deadline:
            if self.old.identity() != self.old_identity or not self.running(self.container):
                raise RuntimeError('timer retired compatibility receiver while old backend was alive')
            now = observation()
            start = now['ExecMainStartTimestampMonotonic']
            if invocation is None and start > before:
                invocation = (start, now['InvocationID'])
            if invocation is not None:
                # The nonzero monotonic start binds this service invocation in
                # this boot across state changes. Also compare InvocationID
                # whenever systemd exposes it.
                if start != invocation[0] or (invocation[1] and now['InvocationID'] and now['InvocationID'] != invocation[1]):
                    raise RuntimeError('automatic retention invocation changed before verified completion')
                if now['ActiveState'] == 'failed':
                    raise RuntimeError('automatic retention invocation failed')
                if now['ActiveState'] == 'inactive' and now['SubState'] == 'dead' and now['ExecMainExitTimestampMonotonic'] >= start:
                    if now['Result'] != 'success' or now['ExecMainCode'] != 1 or now['ExecMainStatus'] != 0:
                        raise RuntimeError('automatic retention invocation failed')
                    # Completion is required before this final check: observing
                    # a fresh start alone can race an asynchronous Docker stop.
                    if self.old.identity() != self.old_identity or not self.running(self.container):
                        raise RuntimeError('timer retired compatibility receiver while old backend was alive')
                    return {'automatic_retry_completed': True, 'old_backend_retained': True}
            time.sleep(2)
        raise RuntimeError('automatic retention invocation did not complete within deadline')

    def retire(self):
        self.old.close()
        self.old = None
        deadline = time.monotonic()+200
        while time.monotonic() < deadline:
            value = json.loads((STATE/'trace-cutover.json').read_text())
            if value['phase'] == 'retired' and not self.running(self.container):
                if not self.running(self.foreign):
                    raise RuntimeError('automatic retirement stopped a foreign container')
                return {'automatic_retirement': True, 'foreign_receiver_retained': True}
            time.sleep(2)
        raise RuntimeError('automatic retirement deadline expired after old backend closure')

    def disabled(self):
        obj = tomllib.loads((HOME/'.codex/config.toml').read_text())
        if 'trace_exporter' in obj.get('otel', {}) or obj['model'] != 'migration-fixture-preserved':
            raise RuntimeError('disable restored temporary4319 leaf or lost customization')
        if self.running(self.container) or not self.running(self.foreign):
            raise RuntimeError('disable changed receiver ownership')
        return {'temporary_leaf_not_restored': True, 'user_customization_retained': True}

    def snapshot(self):
        paths = [STATE/'native.json', STATE/'trace-cutover.json', HOME/'.codex/config.toml']
        return ([path.read_bytes() for path in paths], sorted(run(['docker', 'ps', '-aq', '--no-trunc',
                 '--filter', 'label=com.docker.compose.project=subyard-openlit']).split()))

    def before_repeat(self):
        self.fixed_point = self.snapshot()
        return {'repeat_update_baseline_captured': True}

    def after_repeat(self):
        if self.fixed_point is None or self.snapshot() != self.fixed_point:
            raise RuntimeError('repeat update changed native settings, journal or owned containers')
        return {'repeat_update_fixed_point': True}

    def reenabled(self):
        obj = tomllib.loads((HOME/'.codex/config.toml').read_text())
        if obj['otel']['trace_exporter'] != NATIVE or obj['model'] != 'migration-fixture-preserved' or self.running(self.container) or not self.running(self.foreign):
            raise RuntimeError('re-enable regressed adoption or foreign ownership')
        return {'native4318_reenabled': True}

    def cleanup(self):
        if self.old is not None:
            self.old.abort()
        if self.foreign is not None:
            run(['docker', 'rm', '-f', self.foreign])
        self.server.shutdown()


def main():
    proof = Proof()
    actions = {'setup': proof.setup, 'old-turn': proof.old_turn, 'adoption': proof.adoption,
               'fresh-turn': proof.fresh_turn, 'retain': proof.retain, 'retire': proof.retire,
               'disabled': proof.disabled, 'reenabled': proof.reenabled,
               'before-repeat': proof.before_repeat, 'after-repeat': proof.after_repeat}
    try:
        for line in sys.stdin:
            action = json.loads(line)['action']
            if action == 'close':
                print(json.dumps({'ok': True}), flush=True)
                break
            if action not in actions:
                raise RuntimeError('unsupported fixture action')
            print(json.dumps({'ok': True, 'proof': actions[action]()}), flush=True)
    except Exception as error:
        # Exception messages above contain only fixed diagnostics and command
        # names. Parser/OS exceptions may include payloads, so expose type only.
        message = str(error) if isinstance(error, RuntimeError) else type(error).__name__
        print(json.dumps({'ok': False, 'error': message}), flush=True)
        return 1
    finally:
        proof.cleanup()
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
