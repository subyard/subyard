#!/usr/bin/env python3
"""Native diagnostic failures remain bounded and never expose private output."""
import importlib.util
import io
import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import types
import unittest
from unittest.mock import patch

NODE = shutil.which('node')
NODE = str(Path(NODE).absolute()) if NODE else None

spec = importlib.util.spec_from_file_location('facts', Path(__file__).parent/'e2e/failure-facts.py')
facts = importlib.util.module_from_spec(spec)
spec.loader.exec_module(facts)


class FailureFactsTest(unittest.TestCase):
    def health(self, *, changed=None, endpoints=None, ids=None, timeout=False, malformed=False, cost=0):
        images = [name+':1@sha256:'+character*64 for name, character in zip(facts.SERVICES, 'abc')]
        container = 'd'*64
        snapshot = {'project': 'subyard-openlit', 'service': 'openlit', 'image': images[1],
                    'state': 'running', 'running': True, 'restarting': False, 'oom_killed': False,
                    'exit_code': 0, 'restart_count': 0, 'health': 'starting', 'failing_streak': 7,
                    'health_checks': [{'start': '2026-10-18T12:00:00.123456789Z',
                                       'end': '2026-10-18T12:00:03.123456789Z', 'exit_code': -1}]}
        snapshot.update(changed or {})
        calls = []
        clock = 0
        def native(argv, **options):
            nonlocal clock
            calls.append(argv)
            self.assertIs(options['stderr'], subprocess.DEVNULL)
            self.assertLessEqual(options['timeout'], 5)
            self.assertGreater(options['timeout'], 0)
            if cost and len(calls) == 3:
                self.assertEqual(options['timeout'], 4)
            if timeout:
                clock += options['timeout']
                raise subprocess.TimeoutExpired('PRIVATE_SENTINEL', options['timeout'])
            if argv[1] == 'ps':
                self.assertIn('label=com.docker.compose.project=subyard-openlit', argv)
                self.assertIn('label=com.docker.compose.service=openlit', argv)
                output = (container+'\n') if ids is None else ids
            elif argv[1] == 'inspect':
                self.assertEqual(argv[-1], container)
                self.assertNotIn('{{json .State}}', argv)
                self.assertNotIn('.Output', argv[3])
                output = 'PRIVATE_SENTINEL' if malformed else json.dumps(snapshot)
            else:
                self.assertEqual(argv[:4], ['docker', 'exec', container, 'node'])
                self.assertNotIn('logs', argv[:2])
                output = json.dumps(endpoints if endpoints is not None else {'ui': 200, 'login': 200, 'logs': 0, 'metrics': 405, 'traces': 405})
            clock += cost
            return subprocess.CompletedProcess(argv, 0, output.encode(), None)
        with patch.object(facts.subprocess, 'run', side_effect=native), \
             patch.object(facts.time, 'monotonic', side_effect=lambda: clock):
            result = facts.openlit_health(images)
        self.assertNotIn('PRIVATE_SENTINEL', json.dumps(result))
        self.assertNotIn(container, json.dumps(result))
        self.assertNotIn(images[1], json.dumps(result))
        return result, calls

    def test_owned_openlit_health_projects_only_fixed_state_and_endpoint_statuses(self):
        result, calls = self.health()
        self.assertEqual(result, {'category': 'observed', 'state': 'running', 'running': True,
            'restarting': False, 'oom_killed': False, 'exit_code': 0, 'restart_count': 0,
            'health': 'starting', 'failing_streak': 7,
            'latest_healthcheck': {'category': 'observed', 'exit_code': -1, 'duration_ms': 3000},
            'endpoints': {'ui': 200, 'login': 200, 'logs': 0, 'metrics': 405, 'traces': 405}})
        self.assertNotIn('2026-10-18', json.dumps(result))
        self.assertNotIn('health_checks', result)
        self.assertEqual(len(calls), 3)
        result, calls = self.health(changed={'health': 'healthy'})
        self.assertNotIn('endpoints', result)
        self.assertEqual(len(calls), 2)

    @unittest.skipUnless(NODE, 'Node is needed to exercise the native endpoint probe')
    def test_endpoint_program_emits_statuses_without_bodies_or_error_messages(self):
        _, calls = self.health(cost=4)
        script = calls[-1][-1]
        fake = """globalThis.fetch=async(url,options)=>{
if(options.redirect!=='manual')throw Error('PRIVATE_SENTINEL');
const codes={'http://localhost:3000':307,'http://localhost:3000/login':500,'http://localhost:4318/v1/metrics':405,'http://localhost:4318/v1/traces':405};
if(!(url in codes))throw Error('PRIVATE_SENTINEL');
return {status:codes[url],body:{cancel:async()=>{}}};};"""
        result = subprocess.run([NODE, '-e', fake+script], capture_output=True, timeout=3,
                                text=True, env={'PATH': '/usr/bin:/bin'})
        self.assertEqual(result.returncode, 0)
        self.assertEqual(json.loads(result.stdout), {'ui': 307, 'login': 500, 'logs': 0, 'metrics': 405, 'traces': 405})
        self.assertNotIn('PRIVATE_SENTINEL', result.stdout+result.stderr)

    def test_latest_native_health_execution_projects_only_bounded_execution(self):
        row = {'start': '2026-10-18T12:00:00Z', 'end': '2026-10-18T12:00:03Z', 'exit_code': 1}
        for rows in (None, [], [row]*6, ['PRIVATE_SENTINEL'], [{**row, 'exit_code': True}],
                     [{**row, 'start': 'PRIVATE_SENTINEL'}], [{**row, 'start': '0001-01-01T00:00:00Z'}],
                     [{**row, 'end': '2026-10-18T11:59:59Z'}], [{**row, 'end': '2026-11-18T12:00:00Z'}],
                     [{**row, 'output': 'PRIVATE_SENTINEL'}]):
            self.assertEqual(facts.health_execution(rows), {'category': 'unknown'})
        self.assertEqual(facts.health_execution([row, {**row, 'exit_code': 0}]),
                         {'category': 'observed', 'exit_code': 0, 'duration_ms': 3000})
        self.assertEqual(facts.health_execution([{**row,
            'start': '2026-10-18T12:00:00.000000999Z', 'end': '2026-10-18T12:00:00.001000001Z'}])['duration_ms'], 0)

    def test_oversized_or_truncated_health_inspection_cannot_be_observed(self):
        for output in (b'x'*65537, b'{"project":'):
            calls = 0
            def native(argv, **options):
                nonlocal calls
                calls += 1
                return subprocess.CompletedProcess(argv, 0, b'd'*64+b'\n' if calls == 1 else output, None)
            with patch.object(facts.subprocess, 'run', side_effect=native):
                images = [name+':1@sha256:'+character*64 for name, character in zip(facts.SERVICES, 'abc')]
                self.assertEqual(facts.openlit_health(images), {'category': 'invalid-projection'})
            self.assertEqual(calls, 2)

    def test_openlit_health_refuses_foreign_ambiguous_or_malformed_projection(self):
        for changed in ({'project': 'PRIVATE_SENTINEL'}, {'service': 'collector'}, {'image': 'foreign'},
                        {'running': 1}, {'extra': 'PRIVATE_SENTINEL'}):
            with self.subTest(changed=changed):
                result, calls = self.health(changed=changed)
                self.assertEqual(result, {'category': 'invalid-projection'})
                self.assertEqual(len(calls), 2)
        for ids in ('', 'd'*64+'\n'+'e'*64+'\n', 'PRIVATE_SENTINEL\n'):
            result, calls = self.health(ids=ids)
            self.assertEqual(result['category'], 'absent' if not ids else 'invalid-projection')
            self.assertEqual(len(calls), 1)
        result, _ = self.health(endpoints={'ui': 'PRIVATE_SENTINEL', 'login': 200, 'logs': 405, 'metrics': 405, 'traces': 405})
        self.assertEqual(result['endpoint_category'], 'invalid-projection')
        self.assertNotIn('endpoints', result)
        result, calls = self.health(malformed=True)
        self.assertEqual(result, {'category': 'invalid-projection'})
        self.assertEqual(len(calls), 2)

    def test_openlit_health_timeout_is_safe_and_does_not_retry(self):
        result, calls = self.health(timeout=True)
        self.assertEqual(result, {'category': 'deadline'})
        self.assertEqual(len(calls), 1)

    def test_pull_context_requires_real_api_and_cold_cache_and_keeps_raw_journals_private(self):
        for info_exit, image_state in ((0, 'absent'), (1, 'absent'), (0, 'present'), (0, 'unknown')):
            with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
                directory = Path(temporary)/'diagnostic'
                output = io.StringIO()
                original_stat = Path.stat
                def owned_stat(path, **options):
                    value = original_stat(path, **options)
                    if path in (directory, directory/'marker'):
                        return types.SimpleNamespace(st_uid=0, st_mode=value.st_mode, st_nlink=value.st_nlink)
                    return value
                def native(argv, **options):
                    if argv[:3] == ['docker', 'image', 'inspect']:
                        if image_state == 'present':
                            return subprocess.CompletedProcess(argv, 0, b'[{}]', b'')
                        return subprocess.CompletedProcess(argv, 1, b'[]\n',
                            (('Error response from daemon: No such image: '+argv[3]+'\n').encode()
                         if image_state == 'absent' else b'API unavailable PRIVATE_SENTINEL'))
                    if 'info' in argv:
                        self.assertEqual(argv[:5], ['timeout', '--kill-after=1', '10', 'docker', 'info'])
                        self.assertEqual(options['timeout'], 12)
                        return subprocess.CompletedProcess(argv, info_exit, b'PRIVATE_SENTINEL', b'PRIVATE_SENTINEL')
                    if argv[0] == 'systemctl':
                        self.assertEqual(options['timeout'], 3)
                        return subprocess.CompletedProcess(argv, 0,
                            ('MainPID=18\nExecMainStartTimestampMonotonic=1234\nInvocationID='+'c'*32+'\nEnvironment=PRIVATE_SENTINEL\n').encode(), b'')
                    self.assertEqual(argv[0], 'journalctl')
                    self.assertEqual(options['timeout'], 5)
                    self.assertIn('--lines=200', argv)
                    return subprocess.CompletedProcess(argv, 0, b'PRIVATE_SENTINEL raw journal\n', b'PRIVATE_SENTINEL stderr\n')
                with patch.object(facts.os, 'getuid', return_value=0), patch.object(Path, 'stat', owned_stat), \
                     patch.object(facts.subprocess, 'run', side_effect=native), \
                     patch.object(facts, 'failure', return_value={'pull_category':'timeout'}), \
                     patch.dict(facts.os.environ, {'HOME':'PRIVATE_SENTINEL','DOCKER_HOST':'PRIVATE_SENTINEL','PRIVATE_SENTINEL':'secret'}), \
                     patch('sys.stdout', output):
                    code = facts.pull_context('before', ['a','b','c'], directory)
                    cold = info_exit == 0 and image_state == 'absent'
                    self.assertEqual(code, 0 if cold else 2)
                    # Recovery after an ambiguous inspection must not rewrite
                    # the initial cold admission or permit ordinary enable.
                    image_state = 'present'
                    self.assertEqual(facts.pull_context('after-ordinary', ['a','b','c'], directory), 0)
                rows = [json.loads(line.split('=', 1)[1]) for line in output.getvalue().splitlines()]
                self.assertEqual(rows[0]['docker_info_exit'], info_exit)
                self.assertEqual(rows[0]['cold_cache_observed'], cold)
                self.assertTrue(all(row['state'] == 'present' for row in rows[1]['image_cache'].values()))
                self.assertEqual(rows[0]['daemon']['MainPID'], 18)
                self.assertNotIn('PRIVATE_SENTINEL', output.getvalue())
                self.assertEqual(directory.stat().st_mode & 0o777, 0o700)
                for path in directory.glob('*.log'):
                    self.assertEqual(path.stat().st_mode & 0o777, 0o600)
                    self.assertEqual(path.read_bytes(), b'PRIVATE_SENTINEL raw journal\nPRIVATE_SENTINEL stderr\n')



    def test_cache_accepts_only_exact_known_absence_and_projects_unknown_errors(self):
        image = 'otel/collector:1@sha256:'+'a'*64
        absent = ('Error response from daemon: No such image: '+image).encode()
        for code, stdout, stderr, expected in (
                (0, b'[{}]', b'', 'present'), (1, b'[]\n', absent+b'\n', 'absent'),
                (1, b'[]\n', b'API transport PRIVATE_SENTINEL', 'unknown'),
                (1, b'[]\n', absent+b' PRIVATE_SENTINEL', 'unknown'),
                (1, b'[]\n', absent.replace(b'otel/collector', b'foreign/image'), 'unknown'),
                (1, b'[{}]', absent, 'unknown'), (124, b'[]', absent, 'unknown')):
            with self.subTest(expected=expected, code=code), patch.object(facts.subprocess, 'run',
                    return_value=subprocess.CompletedProcess([], code, stdout, stderr)):
                value = facts.cache([image])
            self.assertEqual(value['clickhouse']['state'], expected)
            self.assertNotIn('PRIVATE_SENTINEL', json.dumps(value))

    def test_pull_context_refuses_wrong_mode_user_or_symlink_before_any_probe(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            directory = Path(temporary)/'link'
            directory.symlink_to(Path(temporary), target_is_directory=True)
            for uid, phase in ((1000,'before'), (0,'unsupported'), (0,'before')):
                with self.subTest(uid=uid, phase=phase), patch.object(facts.os, 'getuid', return_value=uid), \
                     patch.object(facts.subprocess, 'run') as native:
                    with self.assertRaises(ValueError):
                        facts.pull_context(phase, ['a','b','c'], directory)
                    native.assert_not_called()

    def test_plain_progress_is_allowlisted_and_aggregates_layer_bytes(self):
        images = ['clickhouse:1@sha256:'+'a'*64, 'openlit:1@sha256:'+'b'*64, 'collector:1@sha256:'+'c'*64]
        with tempfile.TemporaryDirectory(prefix='openlit-pull-progress-', dir='/tmp') as directory:
            log = Path(directory)/'pull.log'
            log.write_text('PRIVATE_SENTINEL Bearer secret\n'
                'https://PRIVATE_SENTINEL/collector Pulling\n'
                ' Image '+images[2]+' Pulling\n'
                ' clickhouse Pulling\n'
                ' abcdef123456 Pulling fs layer\n'
                ' abcdef123456 Downloading [===>] 1.5MB/3MB PRIVATE_SENTINEL\n'
                ' abcdef123456 Downloading [====>] 2MB/3MB\n'
                ' abcdef123456 Pull complete\n'
                ' 123456abcdef Downloading 2KiB/4KiB\n'
                ' aabbccddeeff Downloading 9MB/1MB\n'
                ' layer-private-id PRIVATE_SENTINEL\n')
            value = facts.pull_projection(log, images)
        self.assertEqual(value['service_progress']['collector']['last_status'], 'Pulling')
        self.assertEqual(value['layer_progress'], {'observed_count': 3,
            'status_counts': {'Pull complete': 1, 'Downloading': 2},
            'byte_pairs_observed_count': 2,
            'downloaded_bytes': 2000000+2048, 'total_bytes': 3000000+4096})
        encoded = json.dumps(value)
        for forbidden in ('PRIVATE_SENTINEL', 'Bearer', 'secret', 'https://', 'abcdef123456', images[2]):
            self.assertNotIn(forbidden, encoded)


    def test_unparsed_byte_progress_is_unknown_and_measured_zero_remains_zero(self):
        with tempfile.TemporaryDirectory(prefix='openlit-pull-bytes-', dir='/tmp') as directory:
            log = Path(directory)/'pull.log'
            for progress, expected in (('unrecognized PRIVATE_SENTINEL', (0, None, None)),
                                       ('0B/0B PRIVATE_SENTINEL', (1, 0, 0)), ('0B/4MB', (1, 0, 4000000))):
                log.write_text(' abcdef123456 Downloading '+progress+'\n')
                value = facts.pull_projection(log, [])
                layer = value['layer_progress']
                self.assertEqual((layer['byte_pairs_observed_count'], layer['downloaded_bytes'], layer['total_bytes']), expected)
                self.assertNotIn('PRIVATE_SENTINEL', json.dumps(value))

    def test_native_timeout_projects_before_private_cleanup_and_preserves124(self):
        compose = (Path(__file__).parent.parent/'compose.yaml').read_bytes()
        images = [line.strip().split(': ', 1)[1] for line in compose.decode().splitlines() if line.startswith('    image: ')]
        original_run, original_remove = subprocess.run, facts.shutil.rmtree
        output = io.StringIO()
        directories = []
        def native(argv, **options):
            self.assertEqual(argv[:7], ['timeout', '--signal=TERM', '--kill-after=30', '900', 'docker', 'compose', '--ansi'])
            self.assertIn('plain', argv)
            self.assertNotIn('--quiet', argv)
            log = Path(options['stdout'].name)
            directories.append(log.parent)
            self.assertEqual(log.stat().st_mode & 0o777, 0o600)
            self.assertEqual(log.parent.stat().st_mode & 0o777, 0o700)
            self.assertEqual((log.parent/'compose.yaml').read_bytes(), compose)
            # Execute a real short timeout, preserving the original subprocess
            # capture options. The native124 is not a manufactured return object.
            return original_run(['timeout', '--signal=TERM', '--kill-after=30', '0.05', 'sh', '-c',
                'printf " collector Pulling\\n abcdef123456 Downloading 1MB/4MB\\nPRIVATE_SENTINEL\\n"; exec sleep 2'], **options)
        def remove(directory):
            self.assertIn('openlit_e2e_dependency_pull=', output.getvalue())
            self.assertTrue((directory/'pull.log').exists())
            original_remove(directory)
        with patch.object(facts.os, 'getuid', return_value=0), patch.object(facts.subprocess, 'run', side_effect=native), \
             patch.object(facts.shutil, 'rmtree', side_effect=remove), patch('sys.stdout', output):
            code = facts.dependency(compose, images)
        self.assertEqual(code, 124)
        value = json.loads(output.getvalue().split('=', 1)[1])
        self.assertEqual(value['exit_code'], 124)
        self.assertEqual(value['pull_category'], 'timeout')
        self.assertEqual(value['layer_progress']['total_bytes'], 4000000)
        self.assertGreaterEqual(value['elapsed_seconds'], 0.05)
        self.assertNotIn('PRIVATE_SENTINEL', output.getvalue())
        self.assertTrue(all(not directory.exists() for directory in directories))

    def test_dependency_rejects_nonroot_or_wrong_pins_before_pull(self):
        for uid, compose in ((1000, b''), (0, b'    image: foreign\n')):
            with self.subTest(uid=uid), patch.object(facts.os, 'getuid', return_value=uid), patch.object(facts.subprocess, 'run') as run:
                with self.assertRaises(ValueError):
                    facts.dependency(compose, ['a', 'b', 'c'])
                run.assert_not_called()

    def test_nonzero_native_exit_with_unknown_progress_is_preserved(self):
        compose = (Path(__file__).parent.parent/'compose.yaml').read_bytes()
        images = [line.strip().split(': ', 1)[1] for line in compose.decode().splitlines() if line.startswith('    image: ')]
        original_run = subprocess.run
        output = io.StringIO()
        def native(argv, **options):
            return original_run(['sh', '-c', 'printf "PRIVATE_SENTINEL unknown progress\\n"; exit 23'], **options)
        with patch.object(facts.os, 'getuid', return_value=0), patch.object(facts.subprocess, 'run', side_effect=native), patch('sys.stdout', output):
            code = facts.dependency(compose, images)
        value = json.loads(output.getvalue().split('=', 1)[1])
        self.assertEqual(code, 23)
        self.assertEqual(value['exit_code'], 23)
        self.assertEqual(value['progress_state'], 'unknown')
        self.assertEqual(value['pull_category'], 'other')
        self.assertNotIn('PRIVATE_SENTINEL', output.getvalue())

    def test_docker_errors_retain_bounded_cause_without_private_values(self):
        private = 'PRIVATE_SENTINEL'
        messages = (
            ('authorization', 'failed to fetch anonymous token: unexpected status from GET request to https://private.invalid/token: 401 Unauthorized', '401 Unauthorized'),
            ('manifest', 'Error response from daemon: manifest unknown: manifest unknown', 'manifest unknown'),
            ('rate-limit', 'toomanyrequests: You have reached your unauthenticated pull rate limit.', 'toomanyrequests'),
            ('dns', 'failed to do request: Head "https://private.invalid/v2/": dial tcp: lookup private.invalid: no such host', 'no such host'),
            ('tls', 'failed to do request: x509: certificate signed by unknown authority', 'x509: certificate signed by unknown authority'),
            ('timeout', 'failed to do request: net/http: TLS handshake timeout', 'TLS handshake timeout'),
            ('deadline', 'failed to resolve reference: context deadline exceeded', 'context deadline exceeded'),
            ('network', 'failed to do request: dial tcp private.invalid:443: connect: connection refused', 'connection refused'),
            ('oom', 'Error response from daemon: cannot allocate memory', 'cannot allocate memory'),
        )
        for category, message, excerpt in messages:
            with self.subTest(category=category):
                text = 'collector Error ' + message + '\nBearer ' + private + '\nHTTP/1.1 500 Private\n' + private
                value = facts.pull_errors(text)
                self.assertEqual(value[0]['service'], 'collector')
                self.assertEqual(value[0]['category'], category)
                self.assertEqual(value[0]['message'], excerpt)
                encoded = json.dumps(value)
                for forbidden in (private, 'private.invalid', 'https://', 'Bearer', 'HTTP/1.1', '500 Private'):
                    self.assertNotIn(forbidden, encoded)
        repeated = '\n'.join('collector Error failed to do request: connection refused' for _ in range(10000))
        value = facts.pull_errors(repeated)
        self.assertEqual(len(value), 1)
        self.assertLess(len(json.dumps(value)), 1024)
        value = facts.pull_errors('\n'.join('collector Error ' + message for _, message, _ in messages))
        self.assertEqual(len(value), 3)
        self.assertLess(len(json.dumps(value)), 1024)

    def test_unknown_error_does_not_invent_pull_cause(self):
        text = 'openlit Error PRIVATE_SENTINEL https://private.invalid/no%20such%20host/429\nError response from daemon: PRIVATE_SENTINEL\n'
        value = facts.pull_errors(text)
        self.assertTrue(all(error['category'] == 'unknown' for error in value))
        self.assertEqual(value[-1]['message'], 'Error response from daemon')
        self.assertNotIn('PRIVATE_SENTINEL', json.dumps(value))

    def test_registry_codes_and_timeouts_do_not_expose_credentials(self):
        calls = []
        def run(argv, **options):
            calls.append((argv, options))
            if 'ghcr.io/token' in argv[-1]:
                raise subprocess.TimeoutExpired(argv, 12, output=b'PRIVATE_SENTINEL', stderr=b'PRIVATE_SENTINEL')
            return subprocess.CompletedProcess(argv, 28 if len(calls) == 1 else 0,
                b'000' if len(calls) == 1 else b'401', b'PRIVATE_SENTINEL')
        with patch.object(facts.subprocess, 'run', side_effect=run):
            value = facts.registries()
        self.assertEqual(value['dockerhub'], {'exit_code': 28, 'http_status': 0})
        self.assertEqual(value['ghcr'], {'exit_code': 0, 'http_status': 401})
        self.assertEqual(value['ghcr-auth'], {'category': 'TimeoutExpired'})
        self.assertNotIn('PRIVATE_SENTINEL', json.dumps(value))
        self.assertEqual(len(calls), 4)
        self.assertTrue(all(options['timeout'] == 12 and '--max-time' in argv for argv, options in calls))

    def test_failure_projects_fixed_progress_cache_and_oom_facts(self):
        with tempfile.TemporaryDirectory(prefix='openlit-facts-', dir='/tmp') as directory:
            state = Path(directory)
            state.chmod(0o700)
            (state/'marker').write_text('subyard-openlit-v1\n')
            (state/'pull.log').write_text('PRIVATE_SENTINEL\n clickhouse Pulling\n openlit Pulling\n collector Error\nTLS handshake timeout\n')
            original = Path.stat
            def stat(path, **options):
                value = original(path, **options)
                return types.SimpleNamespace(st_uid=0, st_mode=value.st_mode) if path == state else value
            def run(argv, **options):
                if argv[0] == 'journalctl':
                    return subprocess.CompletedProcess(argv, 0, b'PRIVATE_SENTINEL\nOut of memory: Killed process\n', b'')
                return subprocess.CompletedProcess(argv, 1, b'PRIVATE_SENTINEL', b'PRIVATE_SENTINEL')
            with patch.object(Path, 'stat', stat), patch.object(facts, 'registries', return_value={}), \
                 patch.object(facts.subprocess, 'run', side_effect=run), \
                 patch.object(facts, 'openlit_health', return_value={'category': 'observed', 'health': 'starting'}) as health:
                value = facts.failure(state, ['a', 'b', 'c'])
                health.assert_called_once_with(['a', 'b', 'c'])
            self.assertEqual(value['openlit_health'], {'category': 'observed', 'health': 'starting'})
            self.assertEqual(value['pull_category'], 'timeout')
            self.assertEqual(value['pull_errors'][-1]['message'], 'TLS handshake timeout')
            self.assertEqual(value['pull_context'], {'operation': 'docker compose pull --quiet',
                'exit_code': None, 'elapsed_seconds': None, 'log_tail_truncated': False})
            self.assertEqual(value['pull_progress'], [('clickhouse', 'Pulling'), ('collector', 'Error'), ('openlit', 'Pulling')])
            self.assertEqual(value['image_cache']['openlit'], {'inspect_exit': 1, 'state': 'unknown'})
            self.assertTrue(value['journal_categories']['kernel']['oom_present'])
            self.assertFalse(value['files_present']['receipt.json'])
            self.assertNotIn('PRIVATE_SENTINEL', json.dumps(value))
            # A symlinked runtime directory must not expose even pull-log facts.
            link = state/'link'
            link.symlink_to(state, target_is_directory=True)
            with patch.object(facts, 'registries', return_value={}), patch.object(facts, 'cache', return_value={}), \
                 patch.object(facts, 'journals', return_value={}), patch.object(facts, 'openlit_health') as health:
                unsafe = facts.failure(link, [])
                health.assert_not_called()
            self.assertFalse(unsafe['runtime_directory_owned'])
            self.assertNotIn('pull_log_bytes', unsafe)


if __name__ == '__main__':
    unittest.main()
