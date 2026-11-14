#!/usr/bin/env python3
"""Exercise the real handler and guest program against an isolated guest tree."""
import importlib.util
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import unittest

PACKAGE = Path(__file__).resolve().parents[1]
HELPERS = ('paseo', 'paseo-check', 'paseo-pair', 'paseo-rotate-identity', 'paseo-sync-projects')
DIAGNOSTIC_LIMIT = 440
GUEST_DIRECTORIES = ('etc/systemd/system', 'var/lib/subyard', 'usr/local/bin', 'opt/subyard', 'srv/agents')

# Runs the exact guest program with only its filesystem root replaced.
INCUS_SHIM = r'''#!/usr/bin/env python3
import os, sys
arguments = sys.argv[1:]
separator = arguments.index('--')
if arguments[:2] != ['exec', os.environ['YARD_INSTANCE_NAME']] or arguments[2:separator] != ['--project', os.environ['INCUS_PROJECT']]:
    sys.exit(90)
if os.path.exists(os.environ['FIXTURE_INCUS_DOWN']):
    sys.exit(1)
command = arguments[separator + 1:]
if command[:3] != ['python3', '-B', '-c']:
    sys.exit(91)
program = command[3].replace("ROOT = ''", 'ROOT = %r' % os.environ['FIXTURE_ROOT'], 1)
if 'FIXTURE_MOUNTINFO' in os.environ:
    program = program.replace("open('/proc/self/mountinfo', 'rb')", "open(os.environ['FIXTURE_MOUNTINFO'], 'rb')")
program = program.replace('def remove(path):', "def remove(path):\n    if path == os.environ.get('FIXTURE_REMOVE_FAIL'): raise OSError('injected removal failure')")
os.execv(sys.executable, [sys.executable, '-B', '-c', program, *command[4:]])
'''
SYSTEMCTL_SHIM = r'''#!/bin/sh
case "$*" in
  show*)
    [ -z "$FIXTURE_SYSTEMCTL_SHOW_FAILS" ] || exit 1
    if [ "$FIXTURE_LOAD_STATE" = not-found ]; then load=not-found fragment=
    elif [ -n "$FIXTURE_LOADED_FRAGMENT" ]; then load=loaded fragment=$FIXTURE_LOADED_FRAGMENT
    elif [ -e "$FIXTURE_ROOT/etc/systemd/system/paseo.service" ]; then load=loaded fragment=/etc/systemd/system/paseo.service
    else load=not-found fragment=
    fi
    active=${FIXTURE_ACTIVE_STATE:-inactive}
    [ ! -e "$FIXTURE_SYSTEMCTL_LOG.stopped" ] || active=inactive
    printf 'ActiveState=%s\nFragmentPath=%s\nLoadState=%s\n' "$active" "$fragment" "$load"
    exit 0 ;;
esac
printf '%s\n' "$*" >>"$FIXTURE_SYSTEMCTL_LOG"
case "$*" in
  "$FIXTURE_SYSTEMCTL_FAIL") exit 1 ;;
esac
case "$*" in 'stop paseo.service') : > "$FIXTURE_SYSTEMCTL_LOG.stopped" ;; esac
exit 0
'''


def payload_source():
    # The payload sources shared retained-lease helpers; test both together.
    return ((PACKAGE/'tests/e2e/owner.sh').read_text()
            + (PACKAGE.parents[2]/'dev/e2e/retired-integration-fixture.sh').read_text())


class FixtureCreationTest(unittest.TestCase):
    def test_stopped_path_requires_disclosure_and_power_restoration(self):
        source = payload_source()
        functions = ''.join(name+'() {'+source.split(name+'() {', 1)[1].split('\n}\n', 1)[0]+'\n}\n'
                            for name in ('report_failure', 'check_json', 'run_checked', 'stop_owned_solo', 'assert_stopped_yard_retires')
                            if name+'() {' in source)
        for scenario, error in (('healthy', None), ('restore-fails', 'did not restore stopped power'),
                                ('no-disclosure', 'destructive retirement disclosure'),
                                ('foreign-project', 'refusing unmarked project'),
                                ('uuid-changed', 'solo instance identity changed'),
                                ('stop-fails', 'native_exit=23')):
            with self.subTest(scenario=scenario), tempfile.TemporaryDirectory(dir='/tmp') as temporary:
                script = r'''
set -euo pipefail
STATE_ROOT=$1 ROOT=$2 scenario=$3 MARKER=fixture
printf '%s\n' "$MARKER" > "$STATE_ROOT/.solo-armed"
printf 'running\n' > "$STATE_ROOT/intent"
printf 'RUNNING\n' > "$STATE_ROOT/power"
: > "$STATE_ROOT/proof"
printf '0\n' > "$STATE_ROOT/boot"
die() { printf '%s\n' "$1" >&2; exit 1; }
yard_power() { cat "$STATE_ROOT/power"; }
retired_proof() { cat "$STATE_ROOT/proof"; }
last_boot() { cat "$STATE_ROOT/boot"; }
assert_state_root() { [ -d "$STATE_ROOT" ]; }
assert_project_owned() {
  [ "$(incus project get "$PROJECT" user.p0-power-systemd)" = "$MARKER" ] || die "refusing unmarked project $PROJECT"
}
incus() {
  case "$*" in
    'config get yard-solo user.subyard.disk_tmp --project subyard-solo') printf 'v1\n' ;;
    'config get yard-solo volatile.uuid --project subyard-solo')
      if [ "$scenario" = uuid-changed ] && [ -e "$STATE_ROOT/native-stopped" ]; then printf 'replaced\n'
      else printf 'solo-uuid\n'; fi ;;
    'config get yard-solo user.subyard.managed --project subyard-solo') printf 'true\n' ;;
    'project get subyard-solo user.p0-power-systemd')
      if [ "$scenario" = foreign-project ]; then printf 'foreign\n'; else printf '%s\n' "$MARKER"; fi ;;
    'stop yard-solo --project subyard-solo')
      [ "$scenario" != stop-fails ] || return 23
      touch "$STATE_ROOT/native-stopped"
      printf 'STOPPED\n' > "$STATE_ROOT/power" ;;
    'config set yard-solo user.subyard.desired_power stopped --project subyard-solo') printf 'stopped\n' > "$STATE_ROOT/intent" ;;
    *) die "unexpected native operation: $*" ;;
  esac
}
guest_yard() { cat >/dev/null; }
operator_yard() {
  local proof
  proof=$(retired_proof)
  case "$*" in
    '-Y solo integration status --json')
      if [ -z "$proof" ]; then
        printf '%s\n' '{"observed":"stopped","detail":"run: yard -Y solo init"}'
      else printf '%s\n' '{"observed":"stopped","detail":""}'; fi ;;
    '-Y solo start'|'-Y solo start --yes')
      touch "$STATE_ROOT/started"
      printf 'running\n' > "$STATE_ROOT/intent"
      printf 'RUNNING\n' > "$STATE_ROOT/power"
      printf '%s\n' "$(( $(last_boot) + 1 ))" > "$STATE_ROOT/boot"
      [ -n "$proof" ] || printf 'Warning: retired integration paseo may still be installed in yard solo; run: yard migrate\n' ;;
    '-Y solo stop --yes')
      if [ ! -f "$STATE_ROOT/runtime-ready" ]; then
        printf '%s\n' '{"status":"migration-required","code":"transition-required","action":"run yard migrate"}'
        return 1
      fi
      printf 'STOPPED\n' > "$STATE_ROOT/power"
      printf 'stopped\n' > "$STATE_ROOT/intent" ;;
    '-Y solo init')
      [ "$(yard_power)" = STOPPED ] && [ "$(cat "$STATE_ROOT/intent")" = stopped ] || die 'init did not capture stopped intent'
      printf 'temporarily start this stopped yard to retire integration paseo\n'
      [ "$scenario" = no-disclosure ] || printf 'chats, schedules, loops and uploads\n'
      return 1 ;;
    '-Y solo init --yes')
      if [ -z "$proof" ]; then
        printf 'true\n' > "$STATE_ROOT/proof"
        printf '%s\n' "$(( $(last_boot) + 1 ))" > "$STATE_ROOT/boot"
        if [ "$scenario" = restore-fails ] || [ "$(cat "$STATE_ROOT/intent")" != stopped ]; then
          printf 'RUNNING\n' > "$STATE_ROOT/power"
        fi
      fi ;;
    'migrate --yes') touch "$STATE_ROOT/runtime-ready" ;;
    'migrate --check --json')
      [ -f "$STATE_ROOT/runtime-ready" ] || { [ "$(yard_power)" = STOPPED ] && [ "$(cat "$STATE_ROOT/intent")" = stopped ]; } || die 'runtime is pending'
      printf '%s\n' '{"outcome":{"status":"ready","code":"ready","reachedGoal":true},"blockers":[]}' ;;
    *) die "unexpected operation: $*" ;;
  esac
}
'''
                script += functions+"run_checked solo-stop.log 'stop owned solo' stop_owned_solo solo-uuid\nassert_stopped_yard_retires solo-uuid\n"
                result = subprocess.run(['bash', '-c', script, 'fixture', temporary,
                                         str(PACKAGE.parents[2]), scenario], stdin=subprocess.DEVNULL,
                                        capture_output=True, text=True, timeout=10)
                if error:
                    self.assertNotEqual(result.returncode, 0, result.stdout)
                    self.assertIn(error, result.stderr)
                    if scenario in ('foreign-project', 'stop-fails'):
                        self.assertEqual((Path(temporary)/'power').read_text().strip(), 'RUNNING')
                        self.assertEqual((Path(temporary)/'intent').read_text().strip(), 'running')
                else:
                    self.assertEqual(result.returncode, 0, result.stdout+result.stderr)

    def test_candidate_update_preserves_arguments_and_native_exit(self):
        source = payload_source()
        function = 'update_candidate() {'+source.split('update_candidate() {', 1)[1].split('\n}\n', 1)[0]+'\n}\n'
        for arguments in (['--check', '--json'], ['--yes']):
            for native in (0, 23):
                with self.subTest(arguments=arguments, native=native), tempfile.TemporaryDirectory(dir='/tmp') as temporary:
                    script = 'set -euo pipefail\nSTATE_ROOT=$1 OPERATOR_HOME=/operator RELEASE_ROOT=/release CANDIDATE_VERSION=candidate\n'
                    script += 'operator_env() { printf "%s\\n" "$@" > "$STATE_ROOT/calls"; return '+str(native)+'; }\n'
                    script += function+'update_candidate '+shlex.join(arguments)+'\n'
                    result = subprocess.run(['bash', '-c', script, 'fixture', temporary], capture_output=True, text=True)
                    self.assertEqual(result.returncode, native, result.stderr)
                    command = ['env', 'YARD_RELEASE_BASE_URL=file:///release', '/operator/.local/bin/yard',
                               'update', '--version', 'candidate', *arguments]
                    self.assertEqual((Path(temporary)/'calls').read_text().splitlines(), command)

    def test_failure_capture_names_native_and_json_failures(self):
        source = payload_source()
        functions = ''.join(name+'() {'+source.split(name+'() {', 1)[1].split('\n}\n', 1)[0]+'\n}\n'
                            for name in ('report_failure', 'check_json', 'run_checked'))
        for code in (0, 23):
            with self.subTest(native=code), tempfile.TemporaryDirectory(dir='/tmp') as temporary:
                script = 'set -euo pipefail\nSTATE_ROOT=$1\n'+functions
                body = 'printf %s ' + shlex.quote('{"native":true}') + '; exit "$1"'
                script += 'run_checked native.json ' + shlex.join(['solo operation', 'sh', '-c', body, 'fixture', str(code)]) + '\n'
                result = subprocess.run(['bash', '-c', script, 'fixture', temporary, str(code)], capture_output=True, text=True)
                self.assertEqual(result.returncode, code, result.stderr)
                if code:
                    self.assertIn('solo operation native_exit=23', result.stderr)
                    self.assertIn('{"native":true}', result.stderr)
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            path = Path(temporary)/'ready.json'
            path.write_text('{"outcome":{"status":"recovering"}}\n')
            script = 'set -euo pipefail\n'+functions+'check_json '+shlex.join([str(path), 'final readiness failed', '.outcome.status == "ready"'])+'\n'
            result = subprocess.run(['bash', '-c', script, 'fixture', str(path)], capture_output=True, text=True)
            self.assertEqual(result.returncode, 1)
            self.assertIn('final readiness failed', result.stderr)
            self.assertIn(path.read_text().strip(), result.stderr)

    def test_image_is_resolvable_in_baseline_created_projects(self):
        source = payload_source()
        seed = source.split('seed_settings() {', 1)[1].split('\n}\n', 1)[0]
        for names in (('default', 'solo', 'foreign'),):
            with self.subTest(names=names), tempfile.TemporaryDirectory(dir='/tmp') as temporary:
                home = Path(temporary)
                (home/'config').mkdir(mode=0o700)
                (home/'config/config.env').write_text('AGENTS=none\n')
                (home/'config/config.env').chmod(0o600)
                script = 'set -euo pipefail\nOPERATOR_HOME=$1 CONFIG_HOME=$1/config STATE_ROOT=$1\n'
                script += 'operator_env() { "$@"; }\nYARDS=('+shlex.join(names)+')\n'
                script += 'seed_settings() {'+seed+'\n}\nseed_settings\n'
                result = subprocess.run(['bash', '-c', script, 'fixture', temporary],
                                        capture_output=True, text=True, timeout=5)
                self.assertEqual(result.returncode, 0, result.stderr)
                for name in names:
                    self.assertTrue((home/'config/yards'/name/'config.env').exists())
                    settings = (home/'config/config.env').read_text().splitlines()
                    self.assertIn('BASE_IMAGE=images:debian/13/cloud', settings)
                    self.assertIn('BASE_IMAGE_FALLBACK=images:debian/13/cloud', settings)

    def test_native_init_precedes_cleanup_claim(self):
        source = payload_source()
        create = source.split('create_yards() {', 1)[1].split('\n}\n', 1)[0]
        for names in (('default', 'solo', 'foreign'),):
            for failure in (0, 23):
                with self.subTest(names=names, failure=failure), tempfile.TemporaryDirectory(dir='/tmp') as temporary:
                    script = r'''
set -euo pipefail
STATE_ROOT=$1 PROJECT_MUTATION_ARMED=$1/default-armed MARKER=fixture
failure=$2
project_of() { printf 'subyard-%s' "$1"; }
die() { printf '%s\n' "$*" >&2; exit 1; }
incus() {
  case "$1 $2" in
    'project show') return 1 ;;
    'project set')
      [ "$3" = "$initialized" ] && [ "$4" = user.p0-power-systemd ] && [ "$5" = "$MARKER" ]
      printf 'claim %s\n' "$3" >> "$STATE_ROOT/calls" ;;
    *) exit 99 ;;
  esac
}
operator_yard() {
  [ "$1" = -Y ] && [ "$4" = --yes ]
  case "$3" in
    init)
      printf 'init %s\n' "$2" >> "$STATE_ROOT/calls"
      [ "$failure" = 0 ] || return "$failure"
      initialized=$(project_of "$2") ;;
    start) printf 'start %s\n' "$2" >> "$STATE_ROOT/calls" ;;
    *) exit 99 ;;
  esac
}
'''
                    script += '\nYARDS=('+shlex.join(names)+')\ncreate_yards() {'+create+'\n}\ncreate_yards\n'
                    result = subprocess.run(['bash', '-c', script, 'fixture', temporary, str(failure)],
                                            capture_output=True, text=True, timeout=5)
                    self.assertEqual(result.returncode, failure, result.stderr)
                    expected = ['init default'] if failure else [
                        line for name in names for line in (f'init {name}', f'claim subyard-{name}', f'start {name}')]
                    self.assertEqual((Path(temporary)/'calls').read_text().splitlines(), expected)


class RetirementTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(dir='/tmp')
        self.addCleanup(self.temporary.cleanup)
        self.base = Path(self.temporary.name)
        self.root, self.bin = self.base/'guest', self.base/'bin'
        self.bin.mkdir(mode=0o700)
        for directory in GUEST_DIRECTORIES:
            (self.root/directory).mkdir(parents=True, mode=0o755)
        for name, content in (('incus', INCUS_SHIM), ('systemctl', SYSTEMCTL_SHIM)):
            self.write(self.bin/name, content.encode(), 0o755)
        self.unit = self.root/'etc/systemd/system/paseo.service'
        self.state = self.root/'srv/agents/paseo'
        self.log = self.base/'systemctl.log'
        self.environment = dict(PATH=f'{self.bin}:{os.environ["PATH"]}', INCUS_PROJECT='subyard', YARD_INSTANCE_NAME='yard',
                                DEV_USER='synthetic', SUBYARD_INTEGRATION_SOURCE_BINDING='source', FIXTURE_ROOT=str(self.root),
                                FIXTURE_INCUS_DOWN=str(self.base/'incus-down'), FIXTURE_SYSTEMCTL_LOG=str(self.log),
                                FIXTURE_SYSTEMCTL_FAIL='', FIXTURE_SYSTEMCTL_SHOW_FAILS='', FIXTURE_LOADED_FRAGMENT='')

    def write(self, path, data=b'foreign content', mode=0o644):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)
        path.chmod(mode)

    def run_handler(self, *arguments):
        return subprocess.run([sys.executable, '-B', str(PACKAGE/'retire.py'), *arguments], env=self.environment,
                              capture_output=True, text=True, timeout=30)

    def observe(self):
        result = self.run_handler('observe')
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(result.stdout)

    def apply(self, observed=None):
        observed = observed or self.observe()
        return self.run_handler('apply', 'op-retire', observed['actual'], observed['desired'])

    def install(self, worktree=False):
        for prefix in ('etc', 'run', 'usr/lib', 'lib'):
            path = self.root/prefix/'systemd/system/paseo.service'
            self.write(path)
            self.write(Path(str(path)+'.d')/'foreign.conf')
        for helper in HELPERS:
            self.write(self.root/'usr/local/bin'/helper)
        self.write(self.root/'opt/subyard/paseo/unknown/data')
        self.write(self.root/'var/lib/subyard/paseo-ownership', b'invalid receipt')
        self.write(self.state/'chat/data')
        if worktree:
            self.write(self.state/'worktrees/local/unpushed', b'unpushed work')

    def assert_removed(self):
        for prefix in ('etc', 'run', 'usr/lib', 'lib'):
            self.assertFalse(os.path.lexists(self.root/prefix/'systemd/system/paseo.service'))
            self.assertFalse(os.path.lexists(self.root/prefix/'systemd/system/paseo.service.d'))
        for helper in HELPERS:
            self.assertFalse(os.path.lexists(self.root/'usr/local/bin'/helper))
        for path in ('opt/subyard/paseo', 'var/lib/subyard/paseo-ownership'):
            self.assertFalse(os.path.lexists(self.root/path))

    def assert_failure(self, result, phase, reason):
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assertEqual(result.stderr.strip().splitlines()[-1], f'subyard-lifecycle-diagnostic-v1 {phase} {reason}')

    def test_nothing_installed(self):
        self.assertEqual(self.observe(), dict(state='absent', actual='', desired=''))

    def test_foreign_installation_is_removed_but_only_worktrees_remain(self):
        self.install(worktree=True)
        self.environment['FIXTURE_ACTIVE_STATE'] = 'active'
        observed = self.observe()
        self.assertEqual(observed, self.observe())
        result = self.apply(observed)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assert_removed()
        self.assertEqual(list(self.state.iterdir()), [self.state/'worktrees'])
        self.assertEqual((self.state/'worktrees/local/unpushed').read_bytes(), b'unpushed work')
        notice = json.loads(result.stdout)['notice']
        self.assertEqual(notice.count('/srv/agents/paseo/worktrees'), 1)
        self.assertLessEqual(len(notice), DIAGNOSTIC_LIMIT)
        before = self.log.read_bytes(), (self.state/'worktrees/local/unpushed').stat()
        self.assertEqual(self.observe()['state'], 'absent')
        self.assertEqual(self.observe()['notice'], notice)
        self.assertEqual(before, (self.log.read_bytes(), (self.state/'worktrees/local/unpushed').stat()))
        self.assertEqual(self.log.read_text().splitlines(), ['stop paseo.service', 'disable paseo.service', 'daemon-reload'])

    def test_running_service_without_a_unit_is_stopped(self):
        self.environment['FIXTURE_ACTIVE_STATE'] = 'active'
        self.assertEqual(self.observe()['state'], 'stale')
        result = self.apply()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.log.read_text().splitlines(), ['stop paseo.service', 'daemon-reload'])
        self.assertEqual(self.observe()['state'], 'absent')

    def test_empty_or_missing_worktrees_remove_all_state(self):
        for empty in (False, True):
            with self.subTest(empty=empty):
                self.install()
                if empty:
                    (self.state/'worktrees').mkdir()
                result = self.apply()
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assert_removed()
                self.assertFalse(self.state.exists())
                self.assertNotIn('notice', self.observe())

    def test_leaf_symlinks_are_unlinked_without_touching_their_targets(self):
        outside = self.base/'outside'
        self.write(outside/'sentinel', b'keep')
        for relative in ('etc/systemd/system/paseo.service', 'usr/local/bin/paseo', 'opt/subyard/paseo',
                         'srv/agents/paseo', 'srv/agents/paseo/worktrees'):
            with self.subTest(path=relative):
                path = self.root/relative
                path.parent.mkdir(parents=True, exist_ok=True)
                path.symlink_to(outside, target_is_directory=True)
                result = self.apply()
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertFalse(os.path.lexists(path))
                self.assertEqual((outside/'sentinel').read_bytes(), b'keep')

    def test_symlinked_ancestor_refuses_before_effects(self):
        outside = self.base/'outside'
        self.write(outside/'paseo/sentinel', b'keep')
        (self.root/'srv/agents').rmdir()
        (self.root/'srv/agents').symlink_to(outside, target_is_directory=True)
        self.write(self.unit)
        self.assert_failure(self.run_handler('observe'), 'inspect', 'native-unavailable')
        self.assertTrue(self.unit.exists())
        self.assertEqual((outside/'paseo/sentinel').read_bytes(), b'keep')

    def test_standard_lib_alias_is_the_same_fixed_unit_path(self):
        (self.root/'lib').symlink_to('usr/lib', target_is_directory=True)
        self.write(self.root/'usr/lib/systemd/system/paseo.service')
        result = self.apply()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((self.root/'lib').is_symlink())
        self.assertFalse((self.root/'usr/lib/systemd/system/paseo.service').exists())

    def test_mounts_refuse_without_deleting_other_paths(self):
        self.install()
        mountinfo = self.base/'mountinfo'
        self.environment['FIXTURE_MOUNTINFO'] = str(mountinfo)
        for relative in ('srv/agents/paseo', 'srv/agents/paseo/chat', 'opt/subyard/paseo/unknown'):
            with self.subTest(path=relative):
                mountinfo.write_text(f'1 2 0:1 / {self.root/relative} rw - ext4 /dev/fixture rw\n')
                self.assert_failure(self.apply(), 'apply', 'operation-failed')
                self.assertTrue(self.unit.exists())
                self.assertTrue((self.state/'chat/data').exists())

    def test_real_service_failure_is_reported_before_removal(self):
        self.install()
        self.environment['FIXTURE_ACTIVE_STATE'] = 'active'
        self.environment['FIXTURE_SYSTEMCTL_FAIL'] = 'stop paseo.service'
        self.assert_failure(self.apply(), 'apply', 'operation-failed')
        self.assertTrue(self.unit.exists())

    def test_partial_removal_retries_without_a_receipt(self):
        self.install(worktree=True)
        self.environment['FIXTURE_REMOVE_FAIL'] = '/opt/subyard/paseo'
        self.assert_failure(self.apply(), 'apply', 'operation-failed')
        self.assertFalse(self.unit.exists())
        del self.environment['FIXTURE_REMOVE_FAIL']
        result = self.apply()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assert_removed()
        self.assertEqual((self.state/'worktrees/local/unpushed').read_bytes(), b'unpushed work')

    def test_changed_observation_refuses(self):
        self.write(self.unit)
        observed = self.observe()
        self.write(self.root/'usr/local/bin/paseo')
        self.assert_failure(self.apply(observed), 'apply', 'observation-changed')
        self.assertTrue(self.unit.exists())
        self.assertFalse(self.log.exists())

    def test_unavailable_guest_and_service_reads_report_failure(self):
        self.environment['FIXTURE_SYSTEMCTL_SHOW_FAILS'] = '1'
        self.assert_failure(self.run_handler('observe'), 'inspect', 'native-unavailable')
        self.environment['FIXTURE_SYSTEMCTL_SHOW_FAILS'] = ''
        (self.base/'incus-down').touch()
        self.assert_failure(self.run_handler('observe'), 'inspect', 'native-unavailable')

    def test_release_observation_fixture_matches_empty_guest(self):
        spec = importlib.util.spec_from_file_location('release_observation', PACKAGE/'tests/release-observation.py')
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        for arguments, expected in module.read_only_commands(PACKAGE):
            result = subprocess.run(['incus', *arguments], env=self.environment, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout, expected)
            # Check the actual host handler against the same exact-command
            # table used by compatibility, not just the guest program in isolation.
            shim = ("#!/usr/bin/env python3\nimport sys\n"
                    f"expected = {arguments!r}\n"
                    "if sys.argv[1:] != expected: sys.exit(99)\n"
                    f"sys.stdout.write({expected!r})\n")
            self.write(self.bin/'incus', shim.encode(), 0o755)
            observed = self.run_handler('observe')
            self.assertEqual(observed.returncode, 0, observed.stderr)
            self.assertEqual(json.loads(observed.stdout), {'state': 'absent', 'actual': '', 'desired': ''})
            for refused in (arguments + ['dev'], arguments[:-1] + ['apply'], arguments[:1] + ['foreign'] + arguments[2:]):
                result = subprocess.run(['incus', *refused], env=self.environment, capture_output=True, text=True)
                self.assertEqual(result.returncode, 99)


if __name__ == '__main__':
    unittest.main()
