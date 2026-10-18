#!/usr/bin/env python3
"""Bind the activation fixture to current frozen metadata and authentic predecessors."""
import ast
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest
from unittest import mock


GUEST = Path(__file__).parent/'e2e/release-activation-guest.sh'
INSTALLER_SHA = 'eeacda9ded1cc4c095ecc1e0569cf6fd96a737b3af6ab30ce7804833e3d8f1d1'


class ActivationIdentityTest(unittest.TestCase):
    def identity(self, version='0.21.0', baseline='0.20.0', **changes):
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            metadata = Path(temporary)/'candidate.json'
            value = dict(version=version,
                runtime_file=f'subyard-{version}-linux-amd64.tar.gz',
                runtime_sha256='a'*64)
            value.update(changes)
            metadata.write_text(json.dumps(value))
            return subprocess.run(['bash', str(GUEST), '--candidate-identity', str(metadata), baseline],
                capture_output=True, text=True, check=False)

    def test_current_and_original_candidate_select_greater_synthetic_successor(self):
        for candidate, baseline, successor in (('0.21.0', '0.20.0', '0.21.1-activation-fixture'),
                ('0.19.1-activation-e2e', '0.19.0', '0.19.2-activation-fixture')):
            with self.subTest(candidate=candidate, baseline=baseline):
                result = self.identity(candidate, baseline)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout, f'{candidate}\t{successor}\t{INSTALLER_SHA}\n')

    def test_mismatched_archive_checksum_or_predecessor_is_refused(self):
        for changes in ({'runtime_file': 'subyard-0.19.1-activation-e2e-linux-amd64.tar.gz'},
                {'runtime_sha256': 'invalid'}, {'baseline': '0.21.0'}, {'version': '0.20.0'}):
            with self.subTest(changes=changes):
                self.assertNotEqual(self.identity(**changes).returncode, 0)


class ActivationTimingTest(unittest.TestCase):
    def test_actual_readiness_and_cleanup_preserve_exit_and_privacy(self):
        source = GUEST.read_text()
        functions = [next(line for line in source.splitlines() if line.startswith('stage() {'))]
        functions.append(next(line for line in source.splitlines() if line.startswith('READY_STATE=not-started ')))
        for name in ('ready', 'cleanup'):
            start = source.index(name+'() {')
            functions.append(source[start:source.index('\n}\n', start)+3])
        with tempfile.TemporaryDirectory(prefix='subyard-release-activation.', dir='/tmp') as temporary:
            root = Path(temporary)
            canary = 'PRIVATE_READINESS_CANARY'
            malformed_exit = subprocess.run(['jq', '-e', '.outcome.status == "ready"'],
                                            input=canary, capture_output=True, text=True).returncode
            script = '\n'.join(functions) + r'''
set -euo pipefail
STATE=$1/state calls=$1/calls case_name=$2
mkdir -m 0700 "$STATE"
MARKER=subyard-release-activation-v1
printf '%s\n' "$MARKER" > "$STATE/.marker"
STARTED=$(date +%s)
SECONDS=9
UNIT=fixture NAMED_UNIT=named UNIT_DIR=$STATE/units UNITS_AUTHORIZED=0
CREATED_YARDS=()
cleanup_image_probe() { printf 'owned-cleanup\n' >> "$calls"; }
yard() {
  [ "$*" = 'migrate --check --json' ] || exit 99
  printf 'readiness\n' >> "$calls"
  case "$case_name" in
    failure) printf '{"outcome":{"status":"PRIVATE_READINESS_CANARY"}}'; return 23 ;;
    timeout) printf 'PRIVATE_READINESS_CANARY'; return 124 ;;
    signal) kill -TERM "$$"; return 99 ;;
    malformed) printf 'PRIVATE_READINESS_CANARY' ;;
    unready) printf '{"outcome":{"status":"PRIVATE_READINESS_CANARY"}}' ;;
    *) printf '{"outcome":{"status":"ready","code":"ready","warnings":null},"message":"PRIVATE_READINESS_CANARY"}' ;;
  esac
}
trap cleanup EXIT
trap 'exit 143' TERM
trap 'exit 130' INT
if [ "$case_name" = closed-stderr ]; then exec 2>&-; exit 7; fi
stage fixture-boundary
ready
'''
            for case, expected in (('success', 0), ('failure', 23), ('timeout', 124),
                                   ('signal', 143), ('malformed', malformed_exit), ('unready', 1),
                                   ('closed-stderr', 7)):
                with self.subTest(case=case):
                    calls = root/'calls'
                    calls.write_text('')
                    result = subprocess.run(['bash', '-c', script, 'fixture', str(root), case],
                                            env={**os.environ, 'READY_STATE':canary,
                                                 'READY_EXIT':canary, 'READY_STARTED':canary},
                                            capture_output=True, text=True, timeout=5)
                    self.assertEqual(result.returncode, expected)
                    self.assertNotIn(canary, result.stdout+result.stderr)
                    self.assertIn('owned-cleanup', calls.read_text())
                    if case == 'closed-stderr':
                        self.assertEqual(result.stderr, '')
                        self.assertEqual(calls.read_text(), 'owned-cleanup\n')
                        continue
                    self.assertEqual(calls.read_text(), 'readiness\nowned-cleanup\n')
                    self.assertRegex(result.stdout, r'release_activation_stage=fixture-boundary elapsed_seconds=[0-9]+')
                    self.assertRegex(result.stdout, r'release_activation_ready state=start elapsed_seconds=[0-9]+')
                    if case == 'success':
                        self.assertRegex(result.stdout, r'release_activation_ready state=returned elapsed_seconds=[0-9]+ native_exit=0')
                        self.assertNotIn('release_activation_ready_failure', result.stderr)
                    else:
                        self.assertIn('release_activation_ready_failure', result.stderr)
                        self.assertIn('trigger_exit='+str(expected), result.stderr)
                        self.assertIn('state='+('running' if case == 'signal' else 'returned'), result.stderr)
                        self.assertIn('native_exit='+('unknown' if case == 'signal' else
                                      str(expected) if case in ('failure', 'timeout') else '0'), result.stderr)


class ConnectionFixtureInitTest(unittest.TestCase):
    def test_auto_sync_failure_capture_preserves_start_exit_and_privacy(self):
        source = (Path(__file__).parent/'e2e/connection-sync-guest.sh').read_text()
        start = source.index('start_auto_sync() {')
        function = source[start:source.index('\n}\n', start)+3]
        with tempfile.TemporaryDirectory(prefix='subyard-connection-auto-', dir='/tmp') as temporary:
            root = Path(temporary)
            calls = root/'calls'
            due = root/'peer.json'
            canary = 'PRIVATE_AUTOSYNC_CANARY'
            categories = {
                'release-inspection-unavailable': 'release transition inspection failed',
                'journal-store-unavailable': 'the journal-selected release store cannot be inspected safely',
                'journal-assets-unverified': 'the journal-selected release is unavailable or unverified',
                'journal-identity-inconsistent': 'the journal-selected release identity is inconsistent',
                'transition-inspection-unavailable': 'the journal-selected release cannot provide a valid transition inspection',
                'source-verification-unavailable': 'verify source support:',
                'materialized-config-unavailable': 'cannot inspect materialized config for release activation',
                'release-dependency-unavailable': 'dependency-unavailable',
                'release-migration-required': 'migration-required',
                'release-transition-required': 'transition-required',
            }
            for name in ('systemctl', 'journalctl'):
                program = root/name
                program.write_text('''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
with Path(os.environ['CALLS']).open('a') as output:
    output.write(Path(sys.argv[0]).name+'\\n')
case = os.environ['DIAG_CASE']
if case == 'native17':
    print('PRIVATE_AUTOSYNC_CANARY')
    print('PRIVATE_AUTOSYNC_CANARY', file=sys.stderr)
    raise SystemExit(17)
if case == 'malformed':
    print('PRIVATE_AUTOSYNC_CANARY')
elif Path(sys.argv[0]).name == 'systemctl':
    print('Result=exit-code\\nExecMainCode=1\\nExecMainStatus=1\\nActiveState=failed\\nSubState=failed')
else:
    print(json.dumps({'MESSAGE':os.environ['JOURNAL_MESSAGE']+' PRIVATE_AUTOSYNC_CANARY'}))
    print(json.dumps({'MESSAGE':'PRIVATE_AUTOSYNC_CANARY'}))
''')
                program.chmod(0o700)
            script = function + r'''
set -euo pipefail
AUTO_SERVICE=fixture.service
due=$1
systemctl_user() { [ "$*" = 'start fixture.service' ] || exit 99; return "$START_RC"; }
timeout() {
  case "$DIAG_CASE" in timeout) return 124 ;; diagnostic17) return 17 ;; esac
  command timeout "$@"
}
[ "$DIAG_CASE" != closed-stderr ] || exec 2>&-
start_auto_sync
'''
            for case in ('good', 'native17', 'diagnostic17', 'timeout', 'malformed', 'success', 'closed-stderr', *categories):
                with self.subTest(case=case):
                    calls.write_text('')
                    due.write_text(json.dumps([canary] if case == 'malformed' else
                        {'lastAttempt':1, 'lastSuccess':0, 'nextRetry':2,
                         'consecutiveFailures':1, 'error':canary}))
                    due.chmod(0o600)
                    result = subprocess.run(['bash', '-c', script, 'fixture', str(due)],
                        env={**os.environ, 'PATH':str(root)+':'+os.environ['PATH'],
                             'CALLS':str(calls), 'DIAG_CASE':case,
                             'JOURNAL_MESSAGE':categories.get(case, 'dependency-unavailable')+
                                 (' dependency-unavailable migration-required transition-required'
                                  if case in categories and case not in ('release-dependency-unavailable',
                                      'release-migration-required', 'release-transition-required') else ''),
                             'START_RC':'0' if case == 'success' else '23'},
                        capture_output=True, text=True, timeout=5)
                    self.assertEqual(result.returncode, 0 if case == 'success' else 23)
                    self.assertNotIn(canary, result.stdout+result.stderr)
                    if case in ('success', 'closed-stderr'):
                        self.assertEqual(calls.read_text(), '')
                        self.assertEqual(result.stdout+result.stderr, '')
                    else:
                        self.assertIn('github_connection_auto_sync_capture exit_code=', result.stderr)
                        if case in ('good', 'native17', 'malformed') or case in categories:
                            line = next(line for line in result.stderr.splitlines()
                                if line.startswith('github_connection_auto_sync_diagnostic='))
                            diagnostic = json.loads(line.split('=', 1)[1])
                            if case == 'malformed':
                                self.assertEqual(diagnostic['peer']['capture'], 'malformed')
                            else:
                                self.assertEqual(diagnostic['peer']['attempted'], True)
                            self.assertEqual(diagnostic['unit']['capture'],
                                {'native17':'native-failure', 'malformed':'malformed'}.get(case, 'ok'))
                            if case == 'good' or case in categories:
                                self.assertEqual(diagnostic['journal']['categories'],
                                                 {case if case in categories else 'release-dependency-unavailable':1})
                                self.assertEqual(diagnostic['journal']['unknown_count'], 1)
                        else:
                            self.assertIn('exit_code='+('124' if case == 'timeout' else '17'), result.stderr)

    def test_user_manager_group_membership_requires_verified_native_snapshot(self):
        source = (Path(__file__).parent/'e2e/connection-sync-guest.sh').read_text()
        program = source.split("<<'PY' >&2", 1)[1].split('\n', 1)[1].split('\nPY\n', 1)[0]
        function = next(node for node in ast.parse(program).body
                        if isinstance(node, ast.FunctionDef) and node.name == 'manager_group')
        uid = os.getuid()
        for case, groups, expected in (
                ('present', '111 222', True), ('absent', '222', False),
                ('unavailable', '', 'unknown'), ('malformed-pid', '', 'unknown'),
                ('malformed-groups', 'PRIVATE_AUTOSYNC_CANARY', 'unknown'),
                ('different-manager', '111', 'unknown')):
            with self.subTest(case=case):
                collect = mock.Mock(return_value=({'capture':'ok'}, b'42\n'))
                if case == 'unavailable':
                    collect.return_value = ({'capture':'unavailable'}, b'')
                elif case == 'malformed-pid':
                    collect.return_value = ({'capture':'ok'}, b'PRIVATE_AUTOSYNC_CANARY')
                elif case == 'different-manager':
                    collect.side_effect = [({'capture':'ok'}, b'42\n'), ({'capture':'ok'}, b'43\n')]
                reader = mock.mock_open(read_data=f'Name:\tsystemd\nUid:\t{uid} {uid} {uid} {uid}\nGroups:\t{groups}\n')
                namespace = {'collect':collect, 'os':os, 'open':reader,
                             'grp':mock.Mock(getgrnam=mock.Mock(return_value=mock.Mock(gr_gid=111)))}
                exec(compile(ast.Module(body=[function], type_ignores=[]), '<fixture>', 'exec'), namespace)
                self.assertEqual(namespace['manager_group'](), expected)
                for call in collect.call_args_list:
                    self.assertEqual(call.args[0], ['systemctl', 'show', f'user@{uid}.service',
                                                   '--property=MainPID', '--value'])
                if case in ('unavailable', 'malformed-pid'):
                    reader.assert_not_called()

    def test_installer_requires_reconciled_platform_and_refreshed_native_api(self):
        source = (Path(__file__).parent/'e2e/connection-sync-guest.sh').read_text()
        start = source.index('fresh() {' if 'fresh() {' in source else 'yard() {')
        wrapper = source[start:source.index('\n}\n', start)+3]
        start = source.index('    # The supported installer activates')
        prepare = source[start:source.index('\n    context\n', start)]
        version_declaration = next((line for line in source.splitlines() if line.startswith('VERSION=')), '')
        package = next(line for line in source.splitlines() if '"$ROOT/dev/package-engine.sh"' in line)
        guard = next(line for line in source.splitlines() if 'case "$runtime" in' in line)
        with tempfile.TemporaryDirectory(prefix='subyard-connection-platform-', dir='/tmp') as temporary:
            root = Path(temporary)
            (root/'dev/e2e').mkdir(parents=True)
            (root/'bin').mkdir()
            platform = root/'dev/e2e/p0-guest.sh'
            platform.write_text('''[ "$1" = fixture-platform ] || exit 99
printf 'platform\\n' >> "$FIXTURE_ROOT/calls"
[ "$FIXTURE_FAILURE" != platform ] || exit 23
touch "$FIXTURE_ROOT/platform"
''')
            driver = root/'bin/driver'
            driver.write_text('''#!/usr/bin/python3
import os, pathlib, sys
root = pathlib.Path(os.environ['FIXTURE_ROOT'])
name = pathlib.Path(sys.argv[0]).name
if name == 'id':
    if sys.argv[1:] == ['-un']: print('dev')
    elif sys.argv[1:] == ['-nG']: print('dev')
    elif sys.argv[1:] == ['-nG', 'dev']:
        print('dev incus-admin' if (root/'platform').exists() else 'dev')
    else: sys.exit(99)
elif name == 'sg':
    assert sys.argv[1:3] == ['incus-admin', '-c']
    os.execve('/bin/bash', ['bash', '-c', sys.argv[3]], dict(os.environ, FIXTURE_ACTIVE='1'))
elif name == 'package-engine.sh':
    with (root/'calls').open('a') as stream: stream.write(name+'\\n')
    (root/'version').write_text(sys.argv[sys.argv.index('--version')+1])
elif name in ('incus', 'subyard-install.sh'):
    with (root/'calls').open('a') as stream: stream.write(name+'\\n')
    if not (root/'platform').exists() or os.environ.get('FIXTURE_ACTIVE') != '1': sys.exit(41)
    if name == 'incus':
        assert sys.argv[1:] == ['query', '/1.0/instances?project=default']
        if os.environ['FIXTURE_FAILURE'] == 'api': sys.exit(37)
    else:
        assert os.environ['YARD_RELEASE_VERSION'] == (root/'version').read_text()
        (root/'installed').touch()
else: sys.exit(99)
''')
            driver.chmod(0o700)
            for name in ('id', 'sg', 'incus', 'subyard-install.sh'):
                (root/'bin'/name).symlink_to(driver)
            (root/'dev/package-engine.sh').symlink_to(driver)
            prepare = prepare.replace('/usr/bin/incus', str(root/'bin/incus'))
            environment = dict(os.environ, FIXTURE_ROOT=str(root), PATH=str(root/'bin')+':/usr/bin:/bin')
            for failure, status, expected in (('', 0, ['platform', 'incus', 'subyard-install.sh']),
                    ('platform', 23, ['platform']), ('api', 37, ['platform', 'incus'])):
                with self.subTest(failure=failure):
                    for name in ('calls', 'platform', 'installed'):
                        (root/name).unlink(missing_ok=True)
                    script = ('set -euo pipefail\nROOT="$FIXTURE_ROOT" STATE="$FIXTURE_ROOT" RELEASE="$FIXTURE_ROOT/bin"\n'
                        'fail() { exit 43; }\n'+version_declaration+'\n'+wrapper+package+'\n'+prepare
                        +'\nruntime="$STATE/data/runtime/releases/$(cat "$FIXTURE_ROOT/version")-fixture/bin/yard"\n'+guard)
                    result = subprocess.run(['bash', '-c', script],
                        env=dict(environment, FIXTURE_FAILURE=failure), capture_output=True, text=True, timeout=5)
                    self.assertEqual(result.returncode, status, result.stderr)
                    self.assertEqual((root/'calls').read_text().splitlines(), ['package-engine.sh']+expected)
                    self.assertEqual((root/'installed').exists(), status == 0)
                    version = (root/'version').read_text()
                    match = re.fullmatch(r'(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?', version)
                    self.assertIsNotNone(match, 'fixture version must be valid SemVer')
                    numbers = tuple(map(int, match.groups()))
                    self.assertTrue(numbers > (0, 11, 0) or (numbers == (0, 11, 0) and '-' not in version))

    def test_fresh_repeat_and_failed_init_ownership(self):
        source = (Path(__file__).parent/'e2e/connection-sync-guest.sh').read_text()
        start = source.index('init() {')
        init = source[start:source.index('\n}\n', start) + 3]
        start = source.index('  cleanup)\n')
        cleanup = 'case cleanup in\n' + source[start:source.index("\n  *) fail 'unknown fixture phase'", start)] + '\nesac\n'
        with tempfile.TemporaryDirectory(prefix='subyard-connection-init-', dir='/tmp') as temporary:
            for failure in (0, 23):
                with self.subTest(failure=failure):
                    calls = Path(temporary)/str(failure)
                    script = init + r'''
set -euo pipefail
PROJECT=project INSTANCE=yard MARKER=fixture
project_state=absent
calls=$1 failure=$2
fail() { printf '%s\n' "$*" >&2; exit 1; }
storage_diagnostics() { :; }
incus() {
  case "$*" in
    'project show project') [ "$project_state" != absent ] ;;
    'project get project user.subyard.e2e') [ "$project_state" != marked ] || printf '%s\n' "$MARKER" ;;
    'project create project -c user.subyard.e2e=fixture') project_state=unmarked ;;
    'profile set default user.subyard.e2e=fixture --project project') : ;;
    'project set project user.subyard.e2e fixture') project_state=marked; printf 'claim\n' >> "$calls" ;;
    'config set yard user.subyard.e2e fixture --project project') printf 'instance-claim\n' >> "$calls" ;;
    *) printf 'unexpected incus: %s\n' "$*" >&2; exit 99 ;;
  esac
}
yard() {
  [ "$*" = 'init --yes' ] || exit 99
  printf 'init %s\n' "$project_state" >> "$calls"
  case "$project_state" in absent) project_state=native ;; marked) ;; *) return 41 ;; esac
  return "$failure"
}
if [ "$failure" = 0 ]; then
  init
  init
else
  rc=0; init || rc=$?
  [ "$rc" = "$failure" ] || exit 99
  printf 'original_exit=%s\n' "$rc"
''' + cleanup + '\nfi\n'
                    result = subprocess.run(['bash', '-c', script, 'fixture', str(calls), str(failure)],
                                            capture_output=True, text=True)
                    log = calls.read_text().splitlines()
                    if failure == 0:
                        self.assertEqual(result.returncode, 0, (result.stdout, result.stderr, log))
                        self.assertEqual(log, ['init absent', 'claim', 'instance-claim',
                                               'init marked', 'claim', 'instance-claim'])
                    else:
                        self.assertEqual(result.returncode, 1, result.stderr)
                        self.assertIn('original_exit=23', result.stdout)
                        self.assertIn('refusing unowned project cleanup', result.stderr)
                        self.assertEqual(log, ['init absent'])


if __name__ == '__main__':
    unittest.main()
