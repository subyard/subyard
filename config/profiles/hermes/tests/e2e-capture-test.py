#!/usr/bin/env python3
"""Exercise actual Hermes capture and cleanup with disposable transport."""
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import time

SOURCE = Path(__file__).resolve().parent / 'e2e'
DIAGNOSTIC = b'controlled broker failure: connection refused; token=private-fixture\n'


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
