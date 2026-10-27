#!/usr/bin/env python3
"""Exercise actual Hermes capture and cleanup with disposable transport."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import time

SOURCE = Path(__file__).resolve().parent / 'e2e'
DIAGNOSTIC = b'controlled broker failure: connection refused; token=private-fixture\n'


def check_repeat_init_diagnostic():
    source = (SOURCE/'hermes-profile.sh').read_text()
    wait = source.split('incus restart "$instance" --project "$project" --timeout 120\n', 1)[1]
    wait = wait.split('\n[ "$(state_signature)"', 1)[0]
    route = source.split('if yard "$YARD" config set HERMES_DASHBOARD_ADVERTISE_HOST', 1)[1]
    route = 'if yard "$YARD" config set HERMES_DASHBOARD_ADVERTISE_HOST' + route.split('\ncat >', 1)[0]
    start = source.index('yard "$YARD" start --yes\n')
    repeat = source[start:source.index("\nprintf '  [ .. ] provisioning only", start)]
    with tempfile.TemporaryDirectory(prefix='subyard-hermes-readiness-', dir='/tmp') as temporary:
        root = Path(temporary)
        engine = root/'yard'
        engine.write_text('''#!/usr/bin/python3
import json, os, pathlib, sys, time
root = pathlib.Path(__file__).parent
args = sys.argv[1:]
with (root/'calls').open('a') as output: output.write(json.dumps(args)+'\\n')
assert args[:2] == ['-Y', 'fixture']
if args[2:] == ['profile', 'status', 'github', '--json']:
    if os.environ['READINESS_MODE'] == 'timeout': time.sleep(2)
    print(json.dumps(dict(runtime='ready' if len((root/'calls').read_text().splitlines()) > 1 else 'unavailable', configured=False, ready=False)))
    raise SystemExit()
assert args[2:] == ['migrate', '--check', '--json']
(root/'called').touch()
print('PRIVATE_SENTINEL', file=sys.stderr)
if os.environ['READINESS_MODE'] == 'timeout' and not os.environ.get('WAIT_ONLY'): time.sleep(2)
print((root/'response').read_text())
raise SystemExit(17 if os.environ['READINESS_MODE'] == 'native-failure' else 0)
''')
        engine.chmod(0o700)
        bound = root/'timeout'
        bound.write_text('''#!/bin/sh
test "$1 $2" = '--signal=TERM --kill-after=2' || exit 99
case "$3" in 1|2|3|4|5|15) ;; *) exit 99 ;; esac
shift 3
exec /usr/bin/timeout --signal=TERM --kill-after=0.1 0.1 "$@"
''')
        bound.chmod(0o700)
        report = {'schemaVersion': 1, 'outcome': {'status': 'migration-required',
            'code': 'transition-required', 'reachedGoal': False, 'message': 'PRIVATE_SENTINEL'},
            'plan': 'PRIVATE_SENTINEL', 'consequences': ['PRIVATE_SENTINEL'],
            'decisions': [{'resource': 'activation.github-runtime', 'scope': 'activation',
                          'decision': 'canonicalize', 'result': 'converged'},
                         {'resource': 'activation.PRIVATE_SENTINEL', 'scope': 'activation',
                          'decision': 'canonicalize', 'result': 'PRIVATE_SENTINEL'}],
            'blockers': [{'code': 'dependency-unavailable', 'message': 'PRIVATE_SENTINEL'}]}
        script = '''set -euo pipefail
. "$1"
YARD=fixture YARD_BIN="$2"
yard() {
  case "$*" in
    'fixture start --yes') return 0 ;;
    'fixture init --yes') return "$REPEAT_EXIT" ;;
    *) exit 99 ;;
  esac
}
''' + repeat
        for mode, original, native, capture in (('report', 23, 0, 0), ('invalid', 23, 0, 2),
                ('native-failure', 23, 17, 0), ('timeout', 23, 124, 2), ('success', 0, 0, 0)):
            (root/'called').unlink(missing_ok=True)
            (root/'response').write_text('PRIVATE_SENTINEL' if mode == 'invalid' else json.dumps(report))
            result = subprocess.run(['bash', '-c', script, 'fixture', str(SOURCE/'failure-capture.sh'), str(engine)],
                env=dict(os.environ, PATH=str(root)+':'+os.environ['PATH'], READINESS_MODE=mode,
                         REPEAT_EXIT=str(original)), capture_output=True, text=True, timeout=5)
            assert result.returncode == original, 'repeat-init failure was replaced'
            assert 'PRIVATE_SENTINEL' not in result.stdout+result.stderr, 'private inspection bytes escaped'
            assert (root/'called').exists() == bool(original), 'inspection ran outside repeat-init failure'
            if not original:
                assert result.stdout == '' and result.stderr == ''
                continue
            expected = f'HERMES_RELEASE_DIAGNOSTIC original_exit_code={original} native_exit_code={native} capture_exit_code={capture}'
            assert expected in result.stdout, 'inspection native/capture exits were conflated'
            if capture == 0:
                line, = [line for line in result.stdout.splitlines() if line.startswith('HERMES_RELEASE_READINESS=')]
                value = json.loads(line.split('=', 1)[1])
                assert value['changed_activation_owners'] == ['activation.github-runtime']
                assert value['other_changed_decisions'] == 1
                assert value['blocker_codes'] == {'dependency-unavailable': 1}

        (root/'response').write_text(json.dumps(report))
        script = '''set -euo pipefail
. "$1"
YARD=fixture YARD_BIN="$2" TAILSCALE_IP=100.127.254.20 dashboard_port=19119
sleep() { SECONDS=$((SECONDS + 10)); }
yard() { printf '%s\\n' "$*" >>"$YARD_BIN.route-calls"; return 23; }
'''
        for mode in ('reconnect', 'timeout', 'route-failure'):
            (root/'calls').unlink(missing_ok=True)
            started = time.monotonic()
            result = subprocess.run(['bash', '-c', script + (route if mode == 'route-failure' else wait),
                'fixture', str(SOURCE/'failure-capture.sh'), str(engine)], capture_output=True, text=True,
                env=dict(os.environ, PATH=str(root)+':'+os.environ['PATH'], READINESS_MODE=mode, WAIT_ONLY='1'), timeout=7)
            assert result.returncode == {'reconnect': 0, 'timeout': 1, 'route-failure': 23}[mode]
            assert time.monotonic() - started < 5, 'reconnect wait was not bounded'
            assert 'PRIVATE_SENTINEL' not in result.stdout+result.stderr
            calls = [json.loads(line)[2:] for line in (root/'calls').read_text().splitlines()]
            if mode == 'reconnect':
                assert calls == [['profile', 'status', 'github', '--json']]*2
                assert not result.stdout and not result.stderr
            else:
                assert calls[-1] == ['migrate', '--check', '--json']
                assert '"changed_activation_owners": ["activation.github-runtime"]' in result.stdout
            route_calls = root/'yard.route-calls'
            if mode == 'route-failure':
                assert route_calls.read_text().splitlines() == [
                    'fixture config set HERMES_DASHBOARD_ADVERTISE_HOST 100.127.254.20 --scope yard --yes']
            else:
                assert not route_calls.exists(), 'runtime wait performed a mutation'


def check(original=23, cleanup=0, fault=''):
    with tempfile.TemporaryDirectory(prefix='subyard-hermes-capture-', dir='/tmp') as temporary:
        root = Path(temporary)
        target = root / 'config/profiles/hermes/tests/e2e'
        target.mkdir(parents=True)
        for name in ('acceptance.sh', 'failure-capture.sh'):
            shutil.copyfile(SOURCE / name, target / name)
        (root / 'dev').mkdir()
        (root / 'bin').mkdir()
        for name in ('systemctl', 'journalctl', 'incus'):
            command = root / 'bin' / name
            command.write_text('#!/bin/sh\ncat "$FIXTURE_DIAGNOSTIC"\n'
                               '[ "$FIXTURE_FAULT" != probe ] || exit 17\n')
            command.chmod(0o700)
        diagnostic = root / 'diagnostic'
        diagnostic.write_bytes(DIAGNOSTIC)
        if fault == 'probe-size':
            diagnostic.write_bytes(b'x' * 65537)
        yard_bin = root / 'bin/yard'
        yard_bin.write_text('#!/bin/sh\nprintf "effective: controlled\\n"\n')
        yard_bin.chmod(0o700)
        transport = root / 'bin/ssh'
        transport.write_text('''#!/usr/bin/env bash
set -euo pipefail
test "$1 $2 $3 $4 $5" = "-F $FIXTURE_ROOT/client-config -T e2e-vm-1 --"
directory="$FIXTURE_ROOT/worktree/src/.build/hermes-failure"
case "$FIXTURE_FAULT" in
  transport) exit 19 ;;
  stalled-transport)
    trap 'touch "$FIXTURE_ROOT/transport-stopped"; exit 0' TERM
    while :; do sleep 1; done ;;
  symlink) mv "$directory/journal" "$directory/original"; ln -s original "$directory/journal" ;;
  mode) chmod 0644 "$directory/journal" ;;
  size) head -c 65537 /dev/zero > "$directory/journal" ;;
esac
exec bash -c "$6"
''')
        transport.chmod(0o700)
        guest_cleanup = (SOURCE / 'hermes-profile.sh').read_text().split('\ncleanup() {\n', 1)[1].split('\n}\n', 1)[0]
        (root / 'guest-cleanup.sh').write_text('cleanup() {\n' + guest_cleanup + '\n}\n')
        (root / 'guest.sh').write_text('''set -euo pipefail
ROOT="$FIXTURE_ROOT/worktree/src"
. "$ROOT/config/profiles/hermes/tests/e2e/failure-capture.sh"
YARD=hermes-controlled
YARD_BIN="$FIXTURE_ROOT/bin/yard"
STATE="$FIXTURE_ROOT/runtime"
TAILSCALE_IP=''
SUBYARD_CONFIG_HOME="$STATE/config"
mkdir -p "$SUBYARD_CONFIG_HOME/yards/$YARD"
touch "$SUBYARD_CONFIG_HOME/yards/$YARD/config.env"
setting() { printf '%s\\n' controlled; }
yard() { return "$FIXTURE_CLEANUP"; }
cleanup_fixture_pool() { find "$STATE" -depth -delete; return "$FIXTURE_CLEANUP"; }
. "$FIXTURE_ROOT/guest-cleanup.sh"
trap cleanup EXIT
exit "$FIXTURE_ORIGINAL"
''')
        quote_function = (SOURCE.parents[4] / 'dev/agent-e2e.sh').read_text().split('\nquote_ssh_command() {\n', 1)[1].split('\n}\n', 1)[0]
        (root / 'dev/agent-e2e.sh').write_text('''LEASE_KEEPER_PID=''
CLIENT_CONFIG="$FIXTURE_ROOT/client-config"
declare -A GUEST_DIRS=()
set_requested_slot() { test "$1" = 7; }
phase_start() { :; }
phase_end() { :; }
phase_marker() { :; }
acquire_lease() {
  test "$VM_COUNT_REQUESTED:$VM_COUNT:$LEASE_PURPOSE" = 1:1:hermes-profile
}
start_lease_keeper() { :; }
build_bundle() { printf 'controlled frozen bundle' > "$2"; }
run_guest() {
  test "$1" = 1
  test "$4 $5" = 'bash config/profiles/hermes/tests/e2e/hermes-profile.sh'
  GUEST_DIRS[1]="$FIXTURE_ROOT/worktree"
  mkdir -p "${GUEST_DIRS[1]}/src/config/profiles/hermes/tests/e2e"
  cp "$FIXTURE_ROOT/config/profiles/hermes/tests/e2e/failure-capture.sh" \
    "${GUEST_DIRS[1]}/src/config/profiles/hermes/tests/e2e/"
  bash "$FIXTURE_ROOT/guest.sh"
}
cleanup_guest() {
  test "$1" = 1
  test ! -e "$FIXTURE_ROOT/runtime"
  find "$FIXTURE_ROOT/worktree" -depth -delete
  return "$FIXTURE_CLEANUP"
}
release_lease() { touch "$FIXTURE_ROOT/released"; return "$FIXTURE_CLEANUP"; }
''' + 'quote_ssh_command() {\n' + quote_function + '\n}\n')
        environment = dict(os.environ, PATH=str(root / 'bin') + ':' + os.environ['PATH'],
                           FIXTURE_ROOT=str(root), FIXTURE_DIAGNOSTIC=str(diagnostic),
                           FIXTURE_ORIGINAL=str(original), FIXTURE_CLEANUP=str(cleanup),
                           FIXTURE_FAULT=fault)
        command = ['bash', str(target / 'acceptance.sh')]
        missing = subprocess.run(command, env=environment, capture_output=True, timeout=10)
        assert missing.returncode == 2 and not (root / 'released').exists()
        started = time.monotonic()
        result = subprocess.run(command + ['--slot', '7'], env=environment, capture_output=True, timeout=27)
        if fault == 'stalled-transport':
            assert 19 <= time.monotonic() - started < 25, 'local capture deadline was not enforced'
            assert (root / 'transport-stopped').exists(), 'stalled transport was not terminated'
        assert result.returncode == (original or (3 if cleanup else 0)), 'original failure was lost'
        assert DIAGNOSTIC.strip() not in result.stdout + result.stderr, 'private bytes reached terminal'
        assert (root / 'released').exists() and not (root / 'worktree').exists(), 'cleanup did not run'
        output, = (root / '.build/test-runs').iterdir()
        assert output.stat().st_mode & 0o777 == 0o700
        assert all(path.stat().st_mode & 0o777 == 0o600 for path in output.iterdir())
        receipt = (output / 'result').read_text()
        assert f'original_exit_code={original}\n' in receipt
        assert f'cleanup_exit_code={3 if cleanup else 0}\n' in receipt
        assert f'capture_exit_code={1 if fault else 0}\n' in receipt
        archive_path = output / 'failure.tar'
        if original and not fault:
            assert archive_path.stat().st_size <= 220000
            with tarfile.open(archive_path) as archive:
                assert archive.extractfile('journal').read() == DIAGNOSTIC
                assert archive.extractfile('unit').read() == DIAGNOSTIC
                assert archive.extractfile('client').read() == DIAGNOSTIC
                assert f'original_exit_code={original}\n'.encode() in archive.extractfile('result').read()
        elif not original:
            assert not archive_path.exists()


check_repeat_init_diagnostic()
for mask in (0o002, 0o077):
    previous = os.umask(mask)
    try:
        check()
        check(cleanup=7)
        for failure in ('transport', 'symlink', 'mode', 'size', 'probe', 'probe-size'):
            check(cleanup=7, fault=failure)
        check(original=0)
    finally:
        os.umask(previous)
check(cleanup=7, fault='stalled-transport')
print('ok: Hermes original diagnostics survive cleanup; failures, bounds and private modes preserved')
