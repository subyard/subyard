#!/usr/bin/env python3
"""Bind the activation fixture to current frozen metadata and authentic predecessors."""
import importlib.util
import shlex
import hashlib
import io
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import tarfile
import unittest
from unittest import mock


GUEST = Path(__file__).parent/'e2e/release-activation-guest.sh'
INSTALLER_SHA = 'eeacda9ded1cc4c095ecc1e0569cf6fd96a737b3af6ab30ce7804833e3d8f1d1'


def source_fixture(name):
    return '. '+shlex.quote(str(GUEST.with_name(name)))+'\n'


spec = importlib.util.spec_from_file_location('connection_diagnostic', GUEST.with_name('connection-diagnostic.py'))
diagnostic = importlib.util.module_from_spec(spec)
spec.loader.exec_module(diagnostic)



class ActivationIdentityTest(unittest.TestCase):
    def test_loss_modes_are_closed_before_candidate_preparation(self):
        controller = GUEST.with_name('release-activation.sh')
        for arguments in (['--native-loss', 'invalid'], ['--native-loss'],
                          ['--native-loss', 'owner', '--native-loss', 'group']):
            with self.subTest(arguments=arguments):
                result = subprocess.run(['bash', str(controller), '--slot', '1', *arguments],
                                        capture_output=True, text=True, timeout=5)
                self.assertEqual(result.returncode, 2)

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
                {'runtime_file': 'subyard-0.21.0-linux-riscv64.tar.gz'},
                {'runtime_sha256': 'invalid'}, {'baseline': '0.21.0'}, {'version': '0.20.0'}):
            with self.subTest(changes=changes):
                self.assertNotEqual(self.identity(**changes).returncode, 0)


class ControllerPrerequisiteTest(unittest.TestCase):
    def test_source_only_operations_have_no_sourcing_effects(self):
        for name in ('release-activation-fixture.sh', 'broker-fixture.sh', 'connection-fixture.sh'):
            result = subprocess.run(['bash', '-c', '''set -euo pipefail
before=$(pwd; set +o; trap -p)
. "$1"
after=$(pwd; set +o; trap -p)
[ "$before" = "$after" ]
[ -z "${READY_STATE+x}" ]
''', 'fixture', str(GUEST.with_name(name))], capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, 0, result.stderr)

    def test_default_broker_disable_uses_scoped_lifecycle_and_preserves_connection(self):
        script = source_fixture('broker-fixture.sh')+r'''
set -euo pipefail
STATE=$1 failure=$2 operation=$3
SUBYARD_KEYS_CONSUMER_ROOT=$STATE UNIT_FILE=$STATE/unit
mkdir -p "$STATE/github"
printf 'protected connection\n' > "$STATE/github/connection.json"
touch "$UNIT_FILE" "$STATE/wiring"
die() { printf '%s\n' "$*" >&2; exit 2; }
yard() {
  printf '%s\n' "$*" >> "$STATE/calls"
  case "$*" in
    'profile disable github --yes')
      [ "$failure" != native ] || return 23
      [ "$failure" = unit ] || rm -- "$UNIT_FILE"
      [ "$failure" = wiring ] || rm -- "$STATE/wiring"
      [ "$failure" != connection ] || printf changed > "$STATE/github/connection.json"
      ;;
    'profile status github --json')
      printf '{"enabled":false,"configured":true,"runtime":"%s","ready":false}\n' "$([ "$failure" = status ] && printf ready || printf removed)"
      ;;
    *) exit 99 ;;
  esac
}
assert_removed() { [ ! -e "$STATE/wiring" ] || die 'wiring survived'; }
guest() { [ "$*" = 'id -u dev' ] || [ "$*" = 'id -g dev' ] || exit 99; printf '1000\n'; }
"$operation"
'''
        with tempfile.TemporaryDirectory(prefix='subyard-broker-disable-', dir='/tmp') as temporary:
            for operation in ('disable_default_broker', 'cleanup_default_broker'):
                for failure in ('none', 'native', 'unit', 'wiring', 'status', 'connection'):
                    case = Path(temporary)/operation/failure
                    result = subprocess.run(['bash', '-c', script, 'fixture', str(case), failure, operation], capture_output=True, text=True, timeout=5)
                    self.assertEqual(result.returncode, 0 if failure == 'none' else 23 if failure == 'native' else 2, result.stderr)
                    self.assertEqual((case/'calls').read_text().splitlines()[0], 'profile disable github --yes')
                    if failure == 'none': self.assertEqual((case/'github/connection.json').read_bytes(), b'protected connection\n')

    def test_guest_cleanup_reports_failure_independently_of_native_exit_three(self):
        cleanup = source_fixture('broker-fixture.sh')
        script = cleanup + r'''
set -euo pipefail
MODE=--cleanup UNIT_FILE='' HERMES_PROJECT=owned PROJECT='' STATE='' CHECKPOINT=/missing
failure=$2
cleanup_hermes() { [ "$failure" != hermes ]; }
cleanup_unit() { [ "$failure" != unit ]; }
broker_guest_cleanup "$1"
'''
        for native in (0,3,23):
            for failure in ('none','hermes','unit'):
                with self.subTest(native=native,failure=failure):
                    result = subprocess.run(['bash','-c',script,'fixture',str(native),failure],
                                            capture_output=True,text=True,timeout=5)
                    cleanup_rc = 0 if failure == 'none' else 3
                    expected = native or cleanup_rc
                    self.assertEqual(result.returncode,expected,result.stderr)
                    self.assertIn(f'original_exit_code={native} cleanup_exit_code={cleanup_rc} final_exit_code={expected}',result.stderr)

    def test_disable_then_scoped_lifecycle_consumes_explicit_retained_hashes(self):
        script = source_fixture('broker-fixture.sh')+r'''
set -euo pipefail
STATE=$1 failure=$2
SUBYARD_KEYS_CONSUMER_ROOT=$STATE UNIT_FILE=$STATE/unit HERMES_UNIT=neighbor.service
mkdir -p "$STATE/github"
printf 'protected connection\n' > "$STATE/github/connection.json"
chmod 0600 "$STATE/github/connection.json"
printf 'operator git identity\n' > "$STATE/gitconfig"
printf 'opaque neighboring data\n' > "$STATE/opaque"
printf 'synthetic github\n' > "$STATE/profiles"
touch "$UNIT_FILE" "$STATE/wiring" "$STATE/running"
connection_before_lifecycle="$(sha256sum "$STATE/github/connection.json" | awk '{print $1}')"
opaque_before="$(sha256sum "$STATE/opaque" | awk '{print $1}')"
die() { printf '%s\n' "$*" >&2; exit 2; }
setting() { [ "$*" = ENVIRONMENT_PROFILES ] || exit 99; cat "$STATE/profiles"; }
assert_removed() { [ ! -e "$UNIT_FILE" ] && [ ! -e "$STATE/wiring" ] || die 'wiring survived'; }
assert_wiring() { [ -e "$UNIT_FILE" ] && [ -e "$STATE/wiring" ] || die 'wiring missing'; }
wait_status() { [ "$*" = '{"configured":true}' ] || exit 99; }
systemctl_user() {
  [ "$*" = 'show neighbor.service -p MainPID --value' ] || exit 99
  if [ "$failure" = neighbor-restart ] && [ -e "$STATE/scoped-disabled" ]; then printf '322\n'; else printf '321\n'; fi
}
hermes_status() {
  if [ "$failure" = neighbor-disconnect ] && [ -e "$STATE/scoped-disabled" ]; then
    printf '{"configured":false}'
  else printf '{"configured":true}'; fi
}
hermes_dev() { [ "$1" = sha256sum ] && [ "$2" = "$STATE/opaque" ] || exit 99; sha256sum "$2"; }
guest() {
  case "$*" in
    'mv /home/dev/.gitconfig /home/dev/.gitconfig-scoped-fixture') mv "$STATE/gitconfig" "$STATE/gitconfig-stash" ;;
    'test ! -e /home/dev/.gitconfig') test ! -e "$STATE/gitconfig" ;;
    'mv /home/dev/.gitconfig-scoped-fixture /home/dev/.gitconfig') mv "$STATE/gitconfig-stash" "$STATE/gitconfig" ;;
    *) exit 99 ;;
  esac
}
yard() {
  printf '%s\n' "$*" >> "$STATE/calls"
  case "$*" in
    'profile disable github --yes')
      rm -f "$UNIT_FILE" "$STATE/wiring" "$STATE/enabled"
      printf 'synthetic\n' > "$STATE/profiles"
      if [ -e "$STATE/scoped" ]; then
        touch "$STATE/scoped-disabled"
        case "$failure" in
          connection) printf changed > "$STATE/github/connection.json" ;;
          neighboring-selection) printf foreign > "$STATE/profiles" ;;
        esac
      fi
      ;;
    'profile enable github --yes')
      if [ ! -e "$STATE/running" ]; then
        [ "$failure" != stopped-accepted ] || return 0
        [ "$failure" != stopped-selection ] || printf foreign > "$STATE/profiles"
        return 17
      fi
      [ "$failure" != native-enable ] || return 23
      [ "$failure" != native-enable-three ] || return 3
      touch "$UNIT_FILE" "$STATE/wiring" "$STATE/enabled"
      printf 'synthetic github\n' > "$STATE/profiles"
      [ "$failure" != git-drift ] || printf changed > "$STATE/gitconfig"
      ;;
    'profile status github --json')
      if [ ! -e "$STATE/running" ]; then
        if [ "$failure" = stopped-status ]; then
          printf '{"configured":true,"runtime":"ready","ready":true}'
        else printf '{"configured":true,"runtime":"unavailable","ready":false}'; fi
      elif [ -e "$STATE/enabled" ]; then
        printf '{"enabled":true,"configured":true,"runtime":"ready","ready":true}'
      else printf '{"enabled":false,"configured":true,"runtime":"removed","ready":false}'; fi
      ;;
    'stop --yes') rm "$STATE/running" ;;
    'profile setup github --yes') : ;;
    'start --yes') touch "$STATE/running"; [ "$failure" != opaque ] || printf changed > "$STATE/opaque" ;;
    *) exit 99 ;;
  esac
}
unset connection_hash
disable_default_broker
[ -z "${connection_hash+x}" ]
[ "$failure" != pre-scoped-connection ] || printf changed > "$STATE/github/connection.json"
touch "$STATE/scoped"
verify_scoped_broker_lifecycle "$connection_before_lifecycle" "$STATE/opaque" "$opaque_before"
[ -z "${connection_hash+x}" ]
cmp -s <(printf 'operator git identity\n') "$STATE/gitconfig"
'''
        with tempfile.TemporaryDirectory(prefix='subyard-scoped-broker-', dir='/tmp') as temporary:
            for failure in ('none', 'native-enable', 'native-enable-three', 'git-drift', 'neighboring-selection',
                            'neighbor-restart', 'neighbor-disconnect', 'connection',
                            'pre-scoped-connection', 'stopped-accepted', 'stopped-selection', 'stopped-status', 'opaque'):
                with self.subTest(failure=failure):
                    case = Path(temporary)/failure
                    result = subprocess.run(['bash', '-c', script, 'fixture', str(case), failure],
                                            capture_output=True, text=True, timeout=5)
                    expected = {'none':0, 'native-enable':23, 'native-enable-three':3}.get(failure, 2)
                    self.assertEqual(result.returncode, expected, result.stderr)
                    if failure == 'none':
                        self.assertEqual((case/'github/connection.json').read_bytes(), b'protected connection\n')
                        self.assertEqual((case/'profiles').read_text(), 'synthetic github\n')
                        calls = (case/'calls').read_text().splitlines()
                        self.assertEqual(calls.count('profile enable github --yes'), 4)
                        self.assertIn('profile setup github --yes', calls)

    def test_empty_python_selection_is_not_green(self):
        result = subprocess.run(['python3','-B',__file__,'-k','__absent_required_case__'],
                                capture_output=True,text=True,timeout=5)
        self.assertNotEqual(result.returncode,0)
        self.assertIn('empty or skipped required fixture selection',result.stderr)

    def test_broker_controller_cleanup_preserves_original_and_reports_release_failure(self):
        cleanup = source_fixture('broker-fixture.sh')
        script = cleanup + r'''
set -euo pipefail
prepared=0 cleanup_failed=0 LEASE_KEEPER_PID='' LOCAL_TEMP=''
declare -A GUEST_DIRS=()
phase_end() { :; }
phase_marker() { :; }
release_lease() { return 7; }
broker_controller_cleanup "$1"
'''
        for native in (0,3,23,124,130,143):
            with self.subTest(native=native):
                result = subprocess.run(['bash','-c',script,'fixture',str(native)],
                                        capture_output=True,text=True,timeout=5)
                self.assertEqual(result.returncode,native or 3,result.stderr)
                self.assertIn('cleanup_exit_code=3',result.stderr)

    def test_prepared_broker_cleanup_at_exit_preserves_state_and_order(self):
        script = '. '+shlex.quote(str(GUEST.parents[5]/'dev/agent-e2e.sh'))+'\n' + source_fixture('broker-fixture.sh') + r'''
set -euo pipefail
main() {
  prepared=1 cleanup_failed=0 bundle="$1/work tree's bundle" bundle_hash="hash with ' quote"
  LOCAL_TEMP="$1/local" LEASE_KEEPER_PID='' rc=0
  scenario="$2" native="$3" calls=0
  GUEST_DIRS=([1]=owned [2]=other)
  mkdir -m 0700 "$LOCAL_TEMP"
  printf 'marker-owned\n' > "$1/.marker"
  phase_end() { printf 'phase:%s\n' "$1" >> "$EVENTS"; }
  phase_marker() { :; }
  cleanup_guest() {
    calls=$((calls + 1))
    printf 'guest:%s:%s\n' "$1" "$2" >> "$EVENTS"
    [ "$(cat "$STATE/.marker")" = marker-owned ] || return 88
    if [ "$scenario" = pre ] && [ "$calls" = 1 ]; then return 9; fi
    if [ "$scenario" = post ] && [ "$calls" = 2 ]; then return 9; fi
    unset 'GUEST_DIRS[$1]'
  }
  run_guest() {
    if [ "${*: -1}" = --verify-reboot ]; then
      printf 'primary:%s\n' "$scenario" >> "$EVENTS"
      return 23
    fi
    printf 'fixture\n' >> "$EVENTS"
    printf '%s\0' "$@" > "$STATE/argv"
    [ "$(stat -c %a "$LOCAL_TEMP/fixture-cleanup.log")" = 600 ] || return 88
    GUEST_DIRS[1]=new-owned
    printf '%s\n' PRIVATE_CLEANUP_CANARY
    [ "$scenario" != fixture ]
  }
  release_lease() { printf 'release\n' >> "$EVENTS"; [ "$scenario" != release ]; }
  trap broker_controller_cleanup EXIT
  case "$scenario" in primary-guest|primary-transport)
    run_guest 1 "$bundle" "$bundle_hash" bash config/profiles/github/tests/e2e/acceptance.sh --verify-reboot ;;
  esac
  exit "$native"
}
STATE=$1 EVENTS=$1/events
main "$@"
'''
        for scenario in ('success', 'pre', 'fixture', 'post', 'release', 'primary-guest', 'primary-transport'):
            for native in ((23,) if scenario.startswith('primary-') else (0, 3, 23, 124, 130, 143)):
                with self.subTest(scenario=scenario, native=native), tempfile.TemporaryDirectory(
                        prefix='github-cleanup-', dir='/tmp') as temporary:
                    root = Path(temporary)
                    result = subprocess.run(['bash', '-c', script, 'fixture', temporary, scenario, str(native)],
                                            capture_output=True, text=True, timeout=5)
                    failed = scenario in ('pre', 'fixture', 'post', 'release')
                    self.assertEqual(result.returncode, native or (3 if failed else 0), result.stderr)
                    self.assertIn(f'original_exit_code={native} cleanup_exit_code={3 if failed else 0} '
                                  f'final_exit_code={native or (3 if failed else 0)}', result.stderr)
                    self.assertNotIn('PRIVATE_CLEANUP_CANARY', result.stdout + result.stderr)
                    self.assertLess(len(result.stdout + result.stderr), 2048)
                    events = (root/'events').read_text().splitlines()
                    if scenario.startswith('primary-'):
                        self.assertEqual(events.pop(0), f'primary:{scenario}')
                    self.assertEqual(events[0:2], [f'phase:{native}', 'guest:1:quiet'])
                    self.assertEqual(events[-1], 'release')
                    if scenario == 'pre':
                        self.assertNotIn('fixture', events)
                        self.assertFalse((root/'argv').exists())
                    else:
                        self.assertEqual(events[2:4], ['fixture', 'guest:1:quiet'])
                        argv = (root/'argv').read_bytes().split(b'\0')[:-1]
                        self.assertEqual([x.decode() for x in argv], ['1', str(root/"work tree's bundle"),
                            "hash with ' quote", 'bash', 'config/profiles/github/tests/e2e/acceptance.sh', '--cleanup'])
                    self.assertIn('guest:2:quiet', events)

    def test_controller_entrypoint_keeps_guest_directory_on_errexit(self):
        with tempfile.TemporaryDirectory(prefix='github-controller-', dir='/tmp') as temporary:
            root = Path(temporary)
            driver = root/'config/profiles/github/tests/e2e/acceptance.sh'
            driver.parent.mkdir(parents=True)
            shutil.copyfile(GUEST.with_name('acceptance.sh'), driver)
            shutil.copyfile(GUEST.with_name('broker-fixture.sh'), driver.with_name('broker-fixture.sh'))
            (root/'tests/helpers').mkdir(parents=True)
            (root/'tests/helpers/release-candidate.sh').write_text('release_candidate_prepare() { return 1; }\n')
            (root/'dev').mkdir()
            (root/'dev/agent-e2e.sh').write_text(r'''
. "$REAL_AGENT_E2E"
set_requested_slot() { :; }
acquire_lease() { :; }
start_lease_keeper() { :; }
build_bundle() { printf 'bundle\n' > "$2"; }
run_guest() {
  GUEST_DIRS[1]="owned-$#"
  printf 'run:%s\n' "${*: -1}" >> "$EVENTS"
  [ "${*: -1}" != --verify-reboot ] || return 23
}
cleanup_guest() {
  printf 'guest:%s:%s\n' "$1" "${GUEST_DIRS[$1]:-lost}" >> "$EVENTS"
  [ "$(cat "$MARKER_FILE")" = marker-owned ] || return 88
  [ -n "${GUEST_DIRS[$1]:-}" ] || return 89
  unset 'GUEST_DIRS[$1]'
}
release_lease() { printf 'release\n' >> "$EVENTS"; }
timeout() {
  case " $* " in
    *'/boot_id '*)
      if [ -e "$BOOT_MARKER" ]; then printf 'after\n'; else printf 'before\n'; touch "$BOOT_MARKER"; fi ;;
    *' -- true '*) return 1 ;;
    *' is-system-running '*) printf 'running\n' ;;
    *' systemctl show '*) printf 'ExecMainStartTimestampMonotonic=1\nActiveState=inactive\nSubState=dead\nResult=success\nExecMainStatus=0\n' ;;
    *) : ;;
  esac
}
''')
            events, marker = root/'events', root/'marker'
            marker.write_text('marker-owned\n')
            env = dict(os.environ, REAL_AGENT_E2E=str(GUEST.parents[5]/'dev/agent-e2e.sh'),
                       EVENTS=str(events), MARKER_FILE=str(marker), BOOT_MARKER=str(root/'boot'), TMPDIR='/tmp')
            env.pop('SUBYARD_E2E_VM', None)
            result = subprocess.run(['bash', str(driver), '--slot', '1'], env=env,
                                    capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 23, result.stderr)
            self.assertIn('original_exit_code=23 cleanup_exit_code=0 final_exit_code=23', result.stderr)
            self.assertNotIn('lost', events.read_text())
            self.assertEqual(events.read_text().splitlines(), [
                'run:--cleanup', 'guest:1:owned-6', 'run:--prepare-reboot', 'guest:1:owned-6',
                'run:--verify-reboot', 'guest:1:owned-6', 'run:--cleanup', 'guest:1:owned-6', 'release'])

    def test_actual_controller_cleanup_preserves_failure_timeout_and_signal(self):
        cleanup = source_fixture('connection-fixture.sh')
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            script = cleanup + r'''
set -euo pipefail
ROOT=$1 LEASE_RUN=fixture
declare -A GUEST_DIRS=([1]=owned)
fixture() { return 7; }
cleanup_on_exit() { exit "$2"; }
cleanup_acceptance "$2"
'''
            for native in (0, 3, 23, 124, 130, 143):
                with self.subTest(native=native):
                    result = subprocess.run(['bash', '-c', script, 'fixture', temporary, str(native)],
                                            capture_output=True, text=True, timeout=5)
                    self.assertEqual(result.returncode, native or 3, result.stderr)

    def test_required_failure_missing_empty_or_skipped_case_stops_allocation(self):
        root = GUEST.parents[5]
        required = ('TestConnectionFixtureIssuerLocal',
                    'TestClientHandlerAndFakeGitHubContractOverUnixSocket',
                    'TestRuntimeStatusReportsLoadedExecutableWithoutCredentials',
                    'TestClientRuntimeStatusRejectsPredecessorAndIncompatibleIdentity')
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            directory = Path(temporary)
            go = directory/'go'
            go.write_text('#!/usr/bin/env python3\nimport os\nprint(os.environ["GO_EVENTS"])\nraise SystemExit(int(os.environ["GO_EXIT"]))\n')
            go.chmod(0o700)
            ssh = directory/'ssh'
            ssh.write_text('#!/bin/sh\ntouch "$ALLOCATION_ATTEMPT"\nexit 99\n')
            ssh.chmod(0o700)
            attempted = directory/'allocation'
            for case in ('failure', 'empty', 'missing', 'skip'):
                events = [] if case == 'empty' else [dict(Test=name,
                    Action='skip' if case == 'skip' and name == required[0] else 'pass')
                    for name in required if case != 'missing' or name != required[0]]
                with self.subTest(case=case):
                    result = subprocess.run(['bash', str(root/'config/profiles/github/tests/e2e/connection-sync.sh'),
                        '--slot', '1'], env=dict(os.environ, PATH=str(directory)+':'+os.environ['PATH'],
                            GO_EVENTS='\n'.join(map(json.dumps, events)),
                            GO_EXIT='23' if case == 'failure' else '0', ALLOCATION_ATTEMPT=str(attempted)),
                        capture_output=True, text=True, timeout=10)
                    self.assertNotEqual(result.returncode, 0, result.stdout+result.stderr)
                    self.assertFalse(attempted.exists(), 'local failure reached the broker')

    def test_installed_worker_mode_dispatches_to_owning_pair_and_preserves_exit(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            directory = Path(temporary)/'config/profiles/github/tests/e2e'
            directory.mkdir(parents=True)
            shutil.copyfile(GUEST.with_name('acceptance.sh'), directory/'acceptance.sh')
            (directory/'connection-sync.sh').write_text('#!/bin/sh\n[ "$*" = "--slot 7" ] || exit 99\nexit 17\n')
            result = subprocess.run(['bash', str(directory/'acceptance.sh'), '--installed-worker', '--slot', '7'],
                                    capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, 17, result.stderr)
            self.assertIn('installed worker admission and real SSH/SOPS peer', result.stdout)


class ActivationTimingTest(unittest.TestCase):
    def test_actual_readiness_and_cleanup_preserve_exit_and_privacy(self):
        functions = source_fixture('release-activation-fixture.sh')+'release_activation_initialize_readiness\n'
        with tempfile.TemporaryDirectory(prefix='subyard-release-activation.', dir='/tmp') as temporary:
            root = Path(temporary)
            canary = 'PRIVATE_READINESS_CANARY'
            malformed_exit = subprocess.run(['jq', '-e', '.outcome.status == "ready"'],
                                            input=canary, capture_output=True, text=True).returncode
            script = functions + r'''
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
trap release_activation_cleanup EXIT
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
        function = source_fixture('connection-fixture.sh')
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
                with mock.patch.object(diagnostic, 'collect', collect), \
                     mock.patch.object(diagnostic, 'open', reader, create=True), \
                     mock.patch.object(diagnostic.grp, 'getgrnam', return_value=mock.Mock(gr_gid=111)):
                    self.assertEqual(diagnostic.manager_group(), expected)
                for call in collect.call_args_list:
                    self.assertEqual(call.args[0], ['systemctl', 'show', f'user@{uid}.service',
                                                   '--property=MainPID', '--value'])
                if case in ('unavailable', 'malformed-pid'):
                    reader.assert_not_called()

    def test_installer_requires_reconciled_platform_and_refreshed_native_api(self):
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
        runtime=root/'data/runtime/releases'/(os.environ['YARD_RELEASE_VERSION']+'-fixture')
        (runtime/'bin').mkdir(parents=True,exist_ok=True)
        (runtime/'bin/yard').write_text('installed launcher')
        import hashlib
        (runtime/'runtime-files.sha256').write_text(hashlib.sha256(b'installed launcher').hexdigest()+'  bin/yard\\n')
        (root/'bin/yard').symlink_to(runtime/'bin/yard')
else: sys.exit(99)
''')
            driver.chmod(0o700)
            for name in ('id', 'sg', 'incus', 'subyard-install.sh'):
                (root/'bin'/name).symlink_to(driver)
            (root/'dev/package-engine.sh').symlink_to(driver)
            environment = dict(os.environ, FIXTURE_ROOT=str(root), PATH=str(root/'bin')+':/usr/bin:/bin')
            for failure, status, expected in (('', 0, ['platform', 'incus', 'subyard-install.sh']),
                    ('platform', 23, ['platform']), ('api', 37, ['platform', 'incus'])):
                with self.subTest(failure=failure):
                    for name in ('calls', 'platform', 'installed', 'bin/yard'):
                        (root/name).unlink(missing_ok=True)
                    script = (source_fixture('connection-fixture.sh')+
                        'set -euo pipefail\nROOT="$FIXTURE_ROOT" STATE="$FIXTURE_ROOT" RELEASE="$FIXTURE_ROOT/bin"\n'
                        'VERSION=0.21.2-github-connection-e2e\n'
                        'fail() { exit 43; }\ncontext() { :; }\n'
                        '"$ROOT/dev/package-engine.sh" --output-dir "$RELEASE" --version "$VERSION" --arch amd64\n'
                        'prepare_connection_runtime "$FIXTURE_ROOT/bin/incus"\n')
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
        init = source_fixture('connection-fixture.sh')
        cleanup = '\ncleanup_connection_fixture\n'
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
    program = unittest.main(exit=False)
    if not program.result.testsRun or program.result.skipped:
        raise SystemExit('empty or skipped required fixture selection')
    raise SystemExit(0 if program.result.wasSuccessful() else 1)
