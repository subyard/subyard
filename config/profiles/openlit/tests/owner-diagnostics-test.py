#!/usr/bin/env python3
"""Exercise owner cleanup diagnostics without provisioning or changing its exits."""
from pathlib import Path
import hashlib
import io
import os
import json
import re
import runpy
import shlex
import subprocess
import sys
import tarfile
import tempfile
import traceback
import unittest
from unittest import mock

OWNER = Path(__file__).parent/'e2e/owner.sh'


class OwnerDiagnosticsTest(unittest.TestCase):
    def test_late_claude_dependency_is_private_checked_and_prepared_before_mutation(self):
        source = OWNER.read_text()
        helpers = source[source.index('\nclaude_pin() {'):source.index('\nretained_data() {')]
        guard = source[source.index('# Acquire the late-only dependency'):source.index('\ntoken=')]
        cleanup = source[source.index('\ncleanup() {'):source.index('\nensure_owner_incus\n')]
        late = source[source.index('\nclaude_payload=()'):source.index('\ntar -C "$ROOT/config/profiles/openlit/tests/e2e" -cf - native-signals.py "${claude_payload[@]}"')]
        for arch, platform, digest in (
                ('amd64', 'linux-x64', '3920489a5109cff5786a1a392c25277408ff22bc796d5edb9c16a60e5a1718f0'),
                ('arm64', 'linux-arm64', 'e4daf793d1e74fb0d9874dd09e98690bbfd7be515f78a87fd05b9e2b4bb33b03')):
            result = subprocess.run(['bash', '-ec', helpers+'\nclaude_pin '+arch], capture_output=True, text=True, timeout=5)
            self.assertEqual((result.returncode, result.stdout), (0, platform+' '+digest+'\n'))
        payload = b'#!/bin/sh\nexit 0\n'
        digest = hashlib.sha256(payload).hexdigest()
        cases = [('success', '', '', 0), ('download', '', '', 92), ('corrupt', '', '', 1),
                 ('bad-archive', '', '', 1), ('member', '', '', 2), ('extraction', '', '', 7),
                 ('architecture', '', '', 2), ('tamper', '', '', 1), ('missing', '', '', 2), ('loss-late', '', 'owner', 0)]
        cases += [('skip', mode, '', 0) for mode in ('auth-only', 'teardown-only', 'network-only', 'dependency-only', 'pull-diagnostic')]
        cases += [('skip', '', mode, 0) for mode in ('owner', 'helper', 'group')]
        for scenario, mode, loss, expected in cases:
            with self.subTest(scenario=scenario, mode=mode, loss=loss), tempfile.TemporaryDirectory(prefix='openlit-late-cli-', dir='/tmp') as directory:
                root = Path(directory); state = root/'state'; state.mkdir(mode=0o700)
                (state/'.marker').write_text('openlit-profile-e2e-v1\n')
                (root/'bin').mkdir(); (root/'installed').mkdir()
                if scenario == 'bad-archive':
                    (root/'fixture.tgz').write_bytes(b'not an archive')
                else:
                    with tarfile.open(root/'fixture.tgz', 'w:gz') as archive:
                        member = tarfile.TarInfo('package/claude')
                        if scenario == 'member':
                            member.type = tarfile.SYMTYPE; member.linkname = 'private'
                            archive.addfile(member)
                        else:
                            content = b'corrupt' if scenario == 'corrupt' else payload
                            member.size = len(content)
                            archive.addfile(member, io.BytesIO(content))
                (root/'bin/dpkg').write_text('#!/bin/sh\nprintf "%s\\n" "${CLAUDE_FIXTURE_ARCH:-amd64}"\n')
                (root/'bin/python3').write_text('#!/bin/sh\nif [ "$1" = - ] && [ "$CLAUDE_FIXTURE_SCENARIO" = extraction ]; then exit 7; fi\nexec /usr/bin/python3 "$@"\n')
                (root/'bin/curl').write_text('''#!/usr/bin/python3
import os,pathlib,sys
root=pathlib.Path(os.environ['CLAUDE_FIXTURE_ROOT']); args=sys.argv[1:]
assert args[:-1]==['--proto','=https','--proto-redir','=https','--tlsv1.2','--fail','--silent','--show-error','--location','--connect-timeout','10','--max-time','180','https://registry.npmjs.org/@anthropic-ai/claude-code-linux-x64/-/claude-code-linux-x64-2.1.287.tgz','-o']
with (root/'calls').open('a') as out: out.write('late\\n' if (root/'mutation').exists() else 'early\\n')
if os.environ['CLAUDE_FIXTURE_SCENARIO']=='download': sys.exit(92)
pathlib.Path(args[-1]).write_bytes((root/'fixture.tgz').read_bytes())
''')
                (root/'bin/install').write_text('''#!/bin/bash
set -euo pipefail
if [ "${1:-}" = -m ] && [ "${2:-}" = 0755 ]; then
  [ "$3 $4 $5 $6 $8" = '-o dev -g dev /home/dev/.local/bin/claude' ]
  printf 'install\\n' >> "$CLAUDE_FIXTURE_ROOT/events"
  exec /usr/bin/install -m 0755 "$7" "$CLAUDE_FIXTURE_ROOT/installed/claude"
fi
exec /usr/bin/install "$@"
''')
                for path in (root/'bin').iterdir(): path.chmod(0o700)
                (root/'native-signals.py').write_text('import os,pathlib,sys\nassert sys.argv[1:]==["--agents","claude"]\npathlib.Path(os.environ["CLAUDE_FIXTURE_ROOT"],"signals").write_text("claude")\n')
                setup = ('set -euo pipefail\nSTATE='+shlex.quote(str(state))+'\n'
                    'YARD_NAME= secondary_name= occupied_pid= tunnel_pid= synthetic_address= synthetic_address_active= kind=container\n'
                    'auth_only='+shlex.quote(mode)+' native_loss='+shlex.quote(loss)+'\n'
                    'sudo() { [ "$1" = -n ]; shift; "$@"; }\n')
                fixture_cleanup = cleanup.replace('/var/tmp/subyard-openlit.*', shlex.quote(str(state)))
                script = setup+helpers+'\nclaude_pin() { printf "linux-x64 '+digest+'\\n"; }\n'+fixture_cleanup+'\n'+guard+'\n'
                script += 'if [ -f "$STATE/late-claude/claude" ]; then [ "$(stat -c %a "$STATE/late-claude")" = 700 ]; [ "$(stat -c %a "$STATE/late-claude/claude")" = 600 ]; [ "$(stat -c %a "$STATE/late-claude/architecture")" = 600 ]; fi\n'
                script += 'test ! -e "$CLAUDE_FIXTURE_ROOT/installed/claude"\nprintf mutation > "$CLAUDE_FIXTURE_ROOT/mutation"\n'
                if scenario == 'tamper': script += 'printf changed > "$STATE/late-claude/claude"\n'
                if scenario == 'missing': script += 'rm "$STATE/late-claude/claude"\n'
                if scenario != 'skip':
                    if scenario == 'architecture': script += 'export CLAUDE_FIXTURE_ARCH=arm64\n'
                    script += late+'\ntar -C "$CLAUDE_FIXTURE_ROOT" -cf - native-signals.py "${claude_payload[@]}" | bash -euo pipefail -c "$late_claude_script"\n'
                result = subprocess.run(['bash', '-c', script], capture_output=True, text=True, timeout=10,
                    env={**os.environ, 'PATH': str(root/'bin')+':/usr/bin:/bin', 'CLAUDE_FIXTURE_ROOT': str(root), 'CLAUDE_FIXTURE_SCENARIO': scenario})
                self.assertEqual(result.returncode, expected, result.stderr)
                self.assertFalse(state.exists(), 'owned cleanup did not remove staged dependency')
                self.assertEqual((root/'mutation').exists(), scenario not in ('download', 'corrupt', 'bad-archive', 'member', 'extraction'))
                calls = (root/'calls').read_text().splitlines() if (root/'calls').exists() else []
                self.assertEqual(calls, [] if scenario=='skip' else ['late' if loss else 'early'])
                if scenario in ('success', 'loss-late'):
                    self.assertEqual((root/'installed/claude').read_bytes(), payload)
                    self.assertEqual((root/'installed/claude').stat().st_mode & 0o777, 0o755)
                    self.assertEqual((root/'signals').read_text(), 'claude')
                else:
                    self.assertFalse((root/'installed/claude').exists())
                    self.assertFalse((root/'signals').exists())

    def test_runtime_refresh_comparisons_project_only_closed_predicates(self):
        failure = runpy.run_path(str(OWNER.with_name('runtime-refresh.py')))['observation_failure']
        expected = {'state': 'current', 'actual': 'approved', 'desired': 'approved'}
        for phase, observation in (('baseline', {'state': 'PRIVATE-SENTINEL', 'actual': 'private', 'desired': 'approved'}),
                                   ('baseline', {'state': 'stale', 'actual': 'PRIVATE-SENTINEL', 'desired': 'approved'}),
                                   ('post-apply', {**expected, 'actual': 'PRIVATE-SENTINEL'}),
                                   ('post-apply', {**expected, 'PRIVATE-SENTINEL': 'private'})):
            with self.subTest(phase=phase):
                message = failure(phase, observation, expected if phase == 'post-apply' else None)
                self.assertNotIn('PRIVATE-SENTINEL', message)
                details = json.loads(message.split(': ', 1)[1])
                self.assertEqual(details['phase'], phase)
                if observation['state'] == 'PRIVATE-SENTINEL':
                    self.assertEqual(details['state'], 'unknown')
                self.assertEqual(details['category'], 'not-current' if phase == 'baseline' else 'observation-mismatch')
                if phase == 'post-apply':
                    self.assertEqual(details['state_matches_returned'], True)
                    self.assertEqual(details['desired_matches_returned'], True)
                    self.assertFalse(details['actual_matches_returned'] and details['keys_match_returned'])


    def test_migration_update_timing_preserves_native_arguments_output_and_exit(self):
        source = OWNER.with_name('migration.sh').read_text()
        update = source[source.index('\nmigration_update() {'):source.index('\ndriver() {')]
        cases = [('normal-offline-update', status, False) for status in (0, 7, 124, 143)]
        cases += [('fixed-point-update', 0, False), ('fixed-point-update', 143, True)]
        for phase, status, closed in cases:
            with self.subTest(phase=phase, status=status, closed=closed):
                setup = ('set -euo pipefail\n'
                         'candidate() { [ "$*" = "update --version fixture --offline --yes" ]; '
                         'printf "native-output\\n"; return '+str(status)+'; }\n')
                result = subprocess.run(['bash', '-c', setup+update+'\n'
                    +('exec 2>&-\n' if closed else '')
                    +'migration_update '+phase+' --version fixture --offline --yes'],
                    capture_output=True, text=True, timeout=5)
                self.assertEqual(result.returncode, status, result.stderr)
                self.assertEqual(result.stdout, 'native-output\n')
                if closed:
                    self.assertEqual(result.stderr, '')
                else:
                    self.assertRegex(result.stderr,
                        r'^E2E_PHASE phase=fixture/openlit-migration-'+phase
                        +r' state=start duration_seconds=0 exit_code=0\n'
                        +r'E2E_PHASE phase=fixture/openlit-migration-'+phase
                        +r' state=end duration_seconds=[0-9]+ exit_code='+str(status)+r'\n$')

    def test_migration_cleanup_preserves_workload_exit_and_reports_separate_failures(self):
        source = OWNER.with_name('migration.sh').read_text()
        cleanup = source[source.index('\ncleanup() {'):source.index('\nSTATE="$(mktemp')]
        with tempfile.TemporaryDirectory(prefix='openlit-migration-cleanup-', dir='/tmp') as directory:
            root = Path(directory)
            config = root/'config/yards/fixture/config.env'
            config.parent.mkdir(parents=True)
            config.write_text('')
            config.chmod(0o600)
            for status, driver_exit, yard_exit, closed_stderr in (
                    (0, 0, 0, False), (0, 7, 0, False), (0, 0, 124, False),
                    (7, 7, 124, False), (124, 0, 0, False), (143, 0, 0, False),
                    (7, 7, 124, True)):
                with self.subTest(status=status, driver_exit=driver_exit, yard_exit=yard_exit,
                                  closed_stderr=closed_stderr):
                    setup = ('set -euo pipefail\nSTATE='+shlex.quote(directory)+'\n'
                        'SUBYARD_CONFIG_HOME="$STATE/config" YARD_NAME=fixture SOURCE=\'\'\n'
                        'driver_pid=fixture driver_input=\'\'\n'
                        'wait() { return '+str(driver_exit)+'; }\n'
                        'yard() { return '+str(yard_exit)+'; }\n')
                    result = subprocess.run(['bash', '-c', setup+cleanup+'\n'
                        +('exec 2>&-\n' if closed_stderr else '')+'exit '+str(status)],
                        capture_output=True, text=True, timeout=5)
                    expected = status or (3 if driver_exit or yard_exit else 0)
                    self.assertEqual(result.returncode, expected, result.stderr)
                    self.assertEqual(result.stdout, '')
                    if closed_stderr:
                        self.assertEqual(result.stderr, '')
                    else:
                        for operation, native_exit in (('driver', driver_exit), ('yard-teardown', yard_exit)):
                            self.assertIn('openlit_migration_cleanup='+operation+' result='
                                +('failed' if native_exit else 'passed')+' exit_code='+str(native_exit)+'\n',
                                result.stderr)

    def test_runtime_refresh_failure_retains_closed_original_result(self):
        command = runpy.run_path(str(OWNER.with_name('runtime-refresh.py')))['command']
        sentinel = 'PRIVATE-SENTINEL'
        arguments = ['fixture-child', sentinel]
        stages = ('guard', 'image-inventory', 'image-pull', 'assets', 'native-disable',
                  'compose-start', 'account-bootstrap', 'unit-start', 'native-enable', 'publication')
        for index, stage in enumerate(stages):
            phase = 'refresh' if index % 2 else 'observe'
            returncode = (0, 1, 7, 124, 137, -15)[index % 6]
            with self.subTest(phase=phase, returncode=returncode, stage=stage):
                result = subprocess.CompletedProcess(arguments, returncode,
                    stdout=sentinel.encode(), stderr=('openlit: guest stage='+stage+'\n'+sentinel).encode())
                with mock.patch.object(subprocess, 'run', return_value=result) as native, \
                        mock.patch('time.monotonic', side_effect=(10, 10.25)):
                    with self.assertRaises(RuntimeError) as failure:
                        command(arguments, success=returncode != 0, phase=phase)
                details = json.loads(str(failure.exception).split(': ', 1)[1])
                self.assertEqual(details, {'phase': phase, 'expected': 'success' if returncode else 'refusal',
                    'returncode': returncode, 'category': 'exit' if returncode else 'unexpected-success',
                    'elapsed_ms': 250, 'guest_stage': stage, 'compose_reason': 'unknown'})
                self.assertNotIn(sentinel, str(failure.exception))
                native.assert_called_once_with(arguments, capture_output=True, timeout=115)


    def test_runtime_refresh_stage_projection_refuses_private_or_unknown_values(self):
        command = runpy.run_path(str(OWNER.with_name('runtime-refresh.py')))['command']
        for stderr, stage, reason in (
                (b'openlit: guest stage=PRIVATE-SENTINEL\n', 'unknown', 'unknown'),
                (b'openlit: guest stage=guard PRIVATE-SENTINEL\n', 'unknown', 'unknown'),
                (b'openlit: guest stage=guard\nopenlit: guest stage=publication\n', 'publication', 'unknown'),
                (b'openlit: guest stage=publication\n'+b'PRIVATE-SENTINEL'*5000, 'publication', 'unknown'),
                (b'openlit: guest stage=compose-start\ncontainer PRIVATE-SENTINEL is unhealthy\n', 'compose-start', 'unhealthy'),
                (b'openlit: guest stage=compose-start\ntimed out waiting for PRIVATE-SENTINEL\n', 'compose-start', 'wait-deadline'),
                (b'openlit: guest stage=compose-start\ndependency failed to start: PRIVATE-SENTINEL\n', 'compose-start', 'dependency-start')):
            with self.subTest(stage=stage, reason=reason), mock.patch.object(subprocess, 'run',
                    return_value=subprocess.CompletedProcess([], 1, stdout=b'PRIVATE-SENTINEL', stderr=stderr)):
                with self.assertRaises(RuntimeError) as failure:
                    command(['PRIVATE-SENTINEL'], phase='refresh')
                details = json.loads(str(failure.exception).split(': ', 1)[1])
                self.assertEqual((details['guest_stage'], details['compose_reason']), (stage, reason))
                self.assertNotIn('PRIVATE-SENTINEL', str(failure.exception))

    def test_runtime_refresh_parent_timeout_sanitizes_rendered_chained_traceback(self):
        command = runpy.run_path(str(OWNER.with_name('runtime-refresh.py')))['command']
        for success in (True, False):
            error = subprocess.TimeoutExpired(['PRIVATE-SENTINEL'], 115,
                output=b'PRIVATE-SENTINEL', stderr=b'openlit: guest stage=publication\nPRIVATE-SENTINEL')
            arguments = error.cmd
            with self.subTest(success=success), mock.patch.object(subprocess, 'run', side_effect=error) as native:
                try:
                    command(arguments, success=success, phase='refresh')
                except RuntimeError as failure:
                    rendered = ''.join(traceback.format_exception(failure))
                    details = json.loads(str(failure).split(': ', 1)[1])
                    self.assertNotIn('PRIVATE-SENTINEL', rendered)
                    self.assertNotIn('TimeoutExpired', rendered)
                    self.assertEqual(details['category'], 'parent-timeout')
                    self.assertIsNone(details['returncode'])
                    self.assertEqual(details['guest_stage'], 'publication')
                else:
                    self.fail('parent timeout was accepted as an expected refusal')
                native.assert_called_once_with(['PRIVATE-SENTINEL'], capture_output=True, timeout=115)


    def test_restored_url_assertion_drains_output_and_preserves_producer_failure(self):
        source = OWNER.read_text()
        yard = source[source.index('\nyard() {'):source.index('\nretained_data() {')]
        assertion = next(line for line in source.splitlines() if line.startswith('yard status |')
                         and 'exact dashboard restoration' in line)
        for status in (0, 7):
            with self.subTest(status=status):
                setup = '''set -euo pipefail
YARD_NAME=fixture private_origin=http://100.64.1.2:22222
die() { exit 2; }
candidate() {
  python3 -c 'import sys; print("http://100.64.1.2:22222/"); sys.stdout.write("x"*1048576+"\\n")' || return $?
  return '''+str(status)+'''
}
'''
                result = subprocess.run(['bash', '-c', setup+yard+'\n'+assertion],
                                        capture_output=True, text=True, timeout=5)
                self.assertEqual(result.returncode, 0 if status == 0 else 2, result.stderr)
                self.assertEqual(result.stdout, '')

    def test_cleanup_keeps_owner_address_until_teardown_and_preserves_workload_exit(self):
        source = OWNER.read_text()
        cleanup = source[source.index('\ncleanup() {'):source.index('\nensure_owner_incus\n')]
        with tempfile.TemporaryDirectory(prefix='openlit-cleanup-', dir='/tmp') as directory:
            root = Path(directory)
            config = root/'config/yards/fixture/config.env'
            config.parent.mkdir(parents=True)
            config.write_text('')
            config.chmod(0o600)
            setup = '''set -euo pipefail
STATE='''+shlex.quote(directory)+'''
SUBYARD_CONFIG_HOME="$STATE/config" YARD_NAME=fixture
ROOT='''+shlex.quote(str(OWNER.resolve().parents[5]))+'''
secondary_name='' occupied_pid='' tunnel_pid='' auth_only='' native_loss='' kind=container
synthetic_address=100.64.1.2 synthetic_address_active=1
diagnostic_images=()
incus() { :; }
yard() { [ "$synthetic_address_active" = 1 ]; }
sudo() { synthetic_address_active=''; return 5; }
'''
            for status in (0, 7):
                with self.subTest(status=status):
                    result = subprocess.run(['bash', '-c', setup+cleanup+'\nexit '+str(status)],
                                            capture_output=True, text=True, timeout=5)
                    self.assertEqual(result.returncode, 3 if status == 0 else status, result.stderr)
                    self.assertIn('primary-teardown result=passed', result.stderr)

    def test_operation_timing_preserves_output_and_native_exit(self):
        source = OWNER.read_text()
        yard = source[source.index('\nyard() {'):source.index('\nretained_data() {')]
        for status, closed_stderr in ((0, False), (7, False), (124, False), (143, False), (143, True)):
            with self.subTest(status=status, closed_stderr=closed_stderr):
                setup = ('set -euo pipefail\nYARD_NAME=fixture\n'
                         'candidate() { printf "native-output\\n"; return '+str(status)+'; }\n')
                result = subprocess.run(['bash', '-c', setup+yard+'\n'
                                         +('exec 2>&-\n' if closed_stderr else '')+'yard init --yes'],
                                        capture_output=True, text=True, timeout=5)
                self.assertEqual(result.returncode, status, result.stderr)
                self.assertEqual(result.stdout, 'native-output\n')
                if not closed_stderr:
                    self.assertRegex(result.stderr, r'^E2E_PHASE phase=fixture/openlit-init state=start duration_seconds=0 exit_code=0\n'
                                     r'E2E_PHASE phase=fixture/openlit-init state=end duration_seconds=[0-9]+ exit_code='+str(status)+r'\n$')

    def test_cleanup_distinguishes_exit_and_signals_without_replacing_status(self):
        source = OWNER.read_text()
        # Execute the actual cleanup and its trap registrations with no owned
        # resources. The full owner entrypoint requires an allocated VM.
        cleanup = source[source.index('\ncleanup() {'):source.index('\nensure_owner_incus\n')]
        setup = '''set -euo pipefail
STATE='' YARD_NAME='' secondary_name='' occupied_pid='' tunnel_pid=''
synthetic_address='' synthetic_address_active='' auth_only='' native_loss='' kind=container
SECONDS=17
'''
        for command, trigger, status in (
                ('exit 0', 'EXIT', 0),
                ('exit 7', 'EXIT', 7),
                ('kill -TERM $$', 'TERM', 0),
                ('kill -INT $$', 'INT', 0),
                ('bash -c \'kill -TERM "$PPID"; exit 23\'', 'TERM', 23),
                ('bash -c \'kill -INT "$PPID"; exit 23\'', 'INT', 23)):
            with self.subTest(trigger=trigger):
                result = subprocess.run(['bash', '-c', setup+cleanup+'\n'+command],
                                        capture_output=True, text=True, timeout=5)
                self.assertEqual(result.returncode, status, result.stderr)
                self.assertEqual(result.stdout, '')
                match = re.fullmatch(
                    r'openlit_e2e_cleanup_entry trigger='+trigger
                    +r' elapsed_seconds=([0-9]+) exit_code='+str(status)+r'\n', result.stderr)
                self.assertIsNotNone(match, result.stderr)
                self.assertGreaterEqual(int(match[1]), 17)

    def test_closed_stderr_preserves_failure_and_owned_child_cleanup(self):
        source = OWNER.read_text()
        cleanup = source[source.index('\ncleanup() {'):source.index('\nensure_owner_incus\n')]
        setup = '''set -euo pipefail
STATE='' YARD_NAME='' secondary_name='' occupied_pid=fixture tunnel_pid=''
synthetic_address='' synthetic_address_active='' auth_only='' native_loss='' kind=container
kill() { printf 'kill-owned-child\\n'; }
wait() { printf 'wait-owned-child\\n'; }
'''
        result = subprocess.run(['bash', '-c', setup+cleanup+'\nexec 2>&-\nexit 7'],
                                capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 7)
        self.assertEqual(result.stdout, 'kill-owned-child\nwait-owned-child\n')
        self.assertEqual(result.stderr, '')


if __name__ == '__main__':
    unittest.main()
