#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d /tmp/subyard-release-candidate.XXXXXX)"
trap 'rm -rf "$TMP"' EXIT
. "$ROOT/tests/helpers/release-candidate.sh"
root="$TMP/root"
mkdir -p "$root/.subyard-acceptance/release"
! release_candidate_prepare "$root" >/dev/null
archive="$root/.subyard-acceptance/release/runtime.tar.gz"
python3 - "$archive" <<'PY'
import hashlib, io, tarfile, sys
files={"bin/yard":b"#!/bin/sh\n", "bin/yard-engine":b"#!/bin/sh\n"}
files["runtime-files.sha256"]=''.join(hashlib.sha256(v).hexdigest()+"  "+k+"\n" for k,v in files.items()).encode()
with tarfile.open(sys.argv[1], "w:gz") as archive:
    for name, value in files.items():
        member=tarfile.TarInfo(name); member.size=len(value); member.mode=0o755 if name.startswith("bin/") else 0o644
        archive.addfile(member, io.BytesIO(value))
PY
hash="$(sha256sum "$archive" | awk '{print $1}')"
printf '{"version":"test","runtime_file":"runtime.tar.gz","runtime_sha256":"%s"}\n' "$hash" > "$root/.subyard-acceptance/candidate.json"
[ "$(release_candidate_prepare "$root")" = "$root/.subyard-acceptance/runtime/bin/yard-engine" ]
# Fresh profile fixtures must use the standalone installer, isolated entrypoints
# and an unselected readiness query instead of executing the extracted engine.
case "$(uname -m)" in x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) exit 1 ;; esac
runtime_file="subyard-1.2.3-linux-$arch.tar.gz"
cp "$archive" "$root/.subyard-acceptance/release/$runtime_file"
printf '{"version":"1.2.3","runtime_file":"%s","runtime_sha256":"%s"}\n' \
  "$runtime_file" "$hash" > "$root/.subyard-acceptance/candidate.json"
export SUBYARD_CONFIG_HOME="$TMP/config" SUBYARD_HOME="$TMP/data"
export TEST_CANDIDATE_STATE="$TMP/state" TEST_CANDIDATE_READY=ready
cat > "$root/.subyard-acceptance/release/subyard-install.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[ "${TEST_NATIVE_OWNER_REQUIRED:-0}" != 1 ] || [ "${TEST_NATIVE_OWNER:-}" = 1 ]
[ -z "${TEST_NATIVE_CALLS:-}" ] || printf 'install owner=%s\n' "${TEST_NATIVE_OWNER:-0}" >> "$TEST_NATIVE_CALLS"
[ "$*" = '--version 1.2.3 --yes' ]
[ "$YARD_RUNTIME_ROOT" = "$TEST_CANDIDATE_STATE/runtime" ]
[ "$YARD_RELEASE_CACHE" = "$TEST_CANDIDATE_STATE/releases" ]
[ "$YARD_BIN_DIR" = "$TEST_CANDIDATE_STATE/bin" ]
[ "$YARD_SHELL_RC" = "$TEST_CANDIDATE_STATE/bashrc" ]
[ "$YARD_LOGIN_RC" = "$TEST_CANDIDATE_STATE/profile" ]
[ "$SUBYARD_CONFIG_HOME" = "${TEST_CANDIDATE_STATE%/*}/config" ]
[ "$SUBYARD_HOME" = "${TEST_CANDIDATE_STATE%/*}/data" ]
install -d "$YARD_BIN_DIR"
cat > "$YARD_BIN_DIR/yard" <<'YARD'
#!/usr/bin/env bash
set -euo pipefail
[ "${TEST_NATIVE_OWNER_REQUIRED:-0}" != 1 ] || [ "${TEST_NATIVE_OWNER:-}" = 1 ]
[ "$*" = 'migrate --check --json' ]
[ "$YARD_RUNTIME_ROOT" = "$TEST_CANDIDATE_STATE/runtime" ]
printf '{"outcome":{"status":"%s"}}\n' "$TEST_CANDIDATE_READY"
YARD
chmod 0755 "$YARD_BIN_DIR/yard"
SH
[ "$(release_candidate_install "$root" "$TEST_CANDIDATE_STATE")" = "$TEST_CANDIDATE_STATE/bin/yard" ]
TEST_CANDIDATE_READY=operator-action-required
export TEST_CANDIDATE_READY
if release_candidate_install "$root" "$TEST_CANDIDATE_STATE" > "$TMP/not-ready-launcher"; then
  printf 'FAIL: release fixture continued without migration readiness\n' >&2; exit 1
fi
[ ! -s "$TMP/not-ready-launcher" ]

# Platform children cannot refresh their parent's groups. Exercise the storage
# fixture's actual bootstrap boundary with group-sensitive native consumers.
(
  source_root="$ROOT"
  mkdir -p "$root/tests/helpers" "$TMP/native-bin"
  cp "$source_root/tests/helpers/release-candidate.sh" "$root/tests/helpers/"
  python3 - "$root/.subyard-acceptance/release/$runtime_file" <<'PY'
import hashlib,io,sys,tarfile
files={"bin/yard":b"#!/bin/sh\n", "bin/yard-engine":b'''#!/bin/sh
set -eu
[ "${TEST_NATIVE_OWNER:-}" = 1 ]
[ "$*" = "config status --all-local" ]
printf 'preflight owner=%s\n' "$TEST_NATIVE_OWNER" >> "$TEST_NATIVE_CALLS"
exit "${TEST_PREFLIGHT_EXIT:-0}"
'''}
files['runtime-files.sha256']=''.join(hashlib.sha256(v).hexdigest()+'  '+k+'\n' for k,v in files.items()).encode()
with tarfile.open(sys.argv[1],'w:gz') as archive:
    for name,value in files.items():
        member=tarfile.TarInfo(name);member.size=len(value);member.mode=0o755 if name.startswith('bin/') else 0o644
        archive.addfile(member,io.BytesIO(value))
PY
  hash="$(sha256sum "$root/.subyard-acceptance/release/$runtime_file" | awk '{print $1}')"
  printf '{"version":"1.2.3","runtime_file":"%s","runtime_sha256":"%s"}\n' \
    "$runtime_file" "$hash" > "$root/.subyard-acceptance/candidate.json"
  cat > "$TMP/native-bin/sg" <<'SH'
#!/bin/sh
set -eu
[ "$1:$2" = incus-admin:-c ]
export TEST_NATIVE_OWNER=1
exec sh -c "$3"
SH
  chmod 0755 "$TMP/native-bin/sg"
  export PATH="$TMP/native-bin:$PATH" TEST_NATIVE_OWNER_REQUIRED=1 TEST_CANDIDATE_READY=ready
  export TEST_NATIVE_CALLS="$TMP/native-calls"
  unset TEST_NATIVE_OWNER
  : > "$TEST_NATIVE_CALLS"
  if release_candidate_install "$root" "$TEST_CANDIDATE_STATE" >/dev/null 2>&1; then
    printf 'FAIL: stale parent execution unexpectedly reached readiness\n' >&2; exit 1
  fi
  [ ! -s "$TEST_NATIVE_CALLS" ]
  eval "$(sed -n '/^native_owner() {/,/^}/p' "$source_root/dev/e2e/disk-tmp-acceptance.sh")"
  eval "$(sed -n '/^install_candidate() {/,/^}/p' "$source_root/dev/e2e/disk-tmp-acceptance.sh")"
  (
    python3() { return 29; }
    rc=0
    native_owner true || rc=$?
    [ "$rc" = 29 ] || { printf 'FAIL: failed argument quoting reached the group command\n' >&2; exit 1; }
  )
  ROOT="$root" STATE="$TMP"
  TEST_CANDIDATE_STATE="$STATE/candidate"
  [ "$(install_candidate)" = "$STATE/candidate/bin/yard" ]
  [ "$(cat "$TEST_NATIVE_CALLS")" = $'preflight owner=1\ninstall owner=1' ]
  : > "$TEST_NATIVE_CALLS"
  export TEST_PREFLIGHT_EXIT=23
  rc=0
  install_candidate >/dev/null 2>&1 || rc=$?
  [ "$rc" = 23 ] && [ "$(cat "$TEST_NATIVE_CALLS")" = 'preflight owner=1' ] \
    || { printf 'FAIL: failed native prerequisite allowed installation or lost its exit\n' >&2; exit 1; }
)
printf 'ok: disk-storage bootstrap probes and installs in the same fresh native owner context\n'

# A container restart changes its Incus init PID; a VM reboot changes its guest
# boot ID. Execute the owning fixture's identity and restart checks for both.
(
  script="$ROOT/dev/e2e/disk-tmp-acceptance.sh"
  eval "$(sed -n '/^boot_id() {/p' "$script")"
  eval "$(sed -n '/^restart_identity() {/,/^}/p' "$script")"
  eval "$(sed -n '/^restart_yard() {/,/^}/p' "$script")"
  export instance=yard-fixture project=subyard-fixture
  export YARD_RUNTIME_ROOT="$TMP/restart-runtime"
  mkdir -p "$YARD_RUNTIME_ROOT/current/scripts"
  cp "$ROOT/scripts/guest-disk-tmp.sh" "$YARD_RUNTIME_ROOT/current/scripts/"
  identity="$TMP/restart-identity" calls="$TMP/restart-calls" noop=0
  die() { printf 'fixture restart check: %s\n' "$*" >&2; exit 1; }
  guest() {
    if [ "$*" = 'sh -s -- wait-boot' ]; then
      cat >/dev/null
      printf 'boot-ready\n' >> "$calls"
      return 0
    fi
    [ "$kind:$*" = 'vm:cat /proc/sys/kernel/random/boot_id' ] || return 90
    printf 'boot-%s\n' "$(cat "$identity")"
  }
  incus() {
    [ "$kind:$*" = 'container:query /1.0/instances/yard-fixture/state?project=subyard-fixture' ] || return 90
    printf '{"pid":%s}\n' "$(cat "$identity")"
  }
  yard() {
    printf '%s\n' "$*" >> "$calls"
    case "$*" in
      'stop --yes') ;;
      'start --yes') [ "$noop" = 1 ] || printf '%s\n' "$(($(cat "$identity") + 1))" > "$identity" ;;
      *) return 90 ;;
    esac
  }
  for kind in container vm; do
    printf '100\n' > "$identity"
    : > "$calls"
    restart_yard
    [ "$(cat "$identity")" = 101 ] && [ "$(cat "$calls")" = $'stop --yes\nstart --yes\nboot-ready' ]
    if (noop=1; restart_yard) >/dev/null 2>&1; then
      printf 'FAIL: %s restart accepted unchanged native identity\n' "$kind" >&2; exit 1
    fi
  done
)
printf 'ok: disk-storage restart checks use container PID and VM guest boot ID\n'
printf '{"version":"test","runtime_file":"runtime.tar.gz","runtime_sha256":"%064d"}\n' 0 > "$root/.subyard-acceptance/candidate.json"
! release_candidate_prepare "$root" >/dev/null
python3 - "$archive" <<'PY'
import io, tarfile, sys
with tarfile.open(sys.argv[1], "w:gz") as archive:
    member=tarfile.TarInfo("../escape"); member.size=1; archive.addfile(member, io.BytesIO(b"x"))
PY
hash="$(sha256sum "$archive" | awk '{print $1}')"
printf '{"version":"test","runtime_file":"runtime.tar.gz","runtime_sha256":"%s"}\n' "$hash" > "$root/.subyard-acceptance/candidate.json"
! release_candidate_prepare "$root" >/dev/null
printf 'ok: release candidate helper validates packaged runtime\n'
printf 'ok: fresh release fixture installs isolated entrypoints and requires unselected migration readiness\n'

# Compatibility controls bind the native runtime and bound owned scratch lifetime
# without changing deliberate interruption or shared fixture ownership.
PYTHONDONTWRITEBYTECODE=1 python3 -B - "$ROOT" <<'PY'
import contextlib
import copy
import hashlib
import io
import json
import os
from pathlib import Path
import runpy
import signal
import subprocess
import sys
import tarfile
import tempfile
from types import SimpleNamespace
from unittest.mock import Mock, patch

root = Path(sys.argv[1])
helper = runpy.run_path(str(root / 'dev/verify-release-upgrades.py'))['history_materialized_fixture']
with tempfile.TemporaryDirectory(prefix='subyard-history-architecture-', dir='/tmp') as temporary:
    base = Path(temporary)
    for arch, foreign in (('amd64', 'arm64'), ('arm64', 'amd64')):
        for order in ('native-first', 'foreign-first'):
            case = base / arch / order
            release = case / 'release'
            release.mkdir(parents=True)
            native = release / f'subyard-1.2.3-linux-{arch}.tar.gz'
            other = release / f'subyard-1.2.3-linux-{foreign}.tar.gz'
            native.write_bytes(b'native runtime identity')
            other.write_bytes(b'foreign runtime identity')
            state = case / 'fixture'
            config = state / 'config'
            config.mkdir(parents=True)
            fixture = SimpleNamespace(root=state, config=config, runtime=state/'runtime',
                                      journal=config/'journal.json', version='1.2.3', arch=arch,
                                      old_launcher=state/'old/bin/yard',
                                      env={'YARD_RELEASE_BASE_URL': release.as_uri()})
            captured = []
            with patch.dict(helper.__globals__, {
                    'write_observation_incus': lambda *args, **kwargs: None,
                    'set_fixture_observation': lambda *args, **kwargs: captured.append(kwargs['history'])}), \
                 patch.object(Path, 'glob', return_value=iter([native, other] if order == 'native-first' else [other, native])):
                history, _, _, _ = helper(fixture, case, case/'control')
            expected = '1.2.3-' + hashlib.sha256(native.read_bytes()).hexdigest()[:12]
            assert history['target'] == expected, (arch, history['target'], expected)
            assert captured[0]['target'] == expected
print('ok: migration-history apply control binds the selected architecture in a mixed release inventory')

globals_ = helper.__globals__
with tempfile.TemporaryDirectory(prefix='subyard-history-skills-', dir='/tmp') as temporary:
    base = Path(temporary)
    skills = globals_['skill_fixture'](root)
    control = base / 'control'
    control.write_text('{"instances":[]}')
    globals_['write_observation_incus'](base, control, base/'unexpected', skills=skills)
    program = (root/'scripts/refresh-agent-skills.sh').read_text().split("' -- \"$mode\"\n", 1)[1].rsplit('\nPY', 1)[0]
    for name, directory_mode, regular_mode, executable_mode in (('normalized-source', 0o755, 0o644, 0o755),
                                                                ('private-installed', 0o700, 0o600, 0o700)):
        package = base / name
        for relative, (kind, mode, _) in skills['archive'].items():
            path = package / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            if kind == 'directory': path.mkdir(exist_ok=True)
            else: path.write_bytes((root / relative).read_bytes())
            path.chmod(directory_mode if kind == 'directory' else executable_mode if mode & 0o111 else regular_mode)
        script = package / 'scripts/refresh-agent-skills.sh'
        script.parent.mkdir(parents=True)
        script.write_bytes((root / 'scripts/refresh-agent-skills.sh').read_bytes())
        script.chmod(executable_mode)
        assert globals_['skill_fixture'](package) == skills, name
    archive = subprocess.run([sys.executable, '-c', program, str(package)], check=True, capture_output=True).stdout
    command = [str(base/'incus'), *skills['command']]
    assert subprocess.run(command, input=archive, capture_output=True).returncode == 0
    with tarfile.open(fileobj=io.BytesIO(archive)) as source:
        members = [(member, source.extractfile(member).read() if member.isfile() else None) for member in source]
    for change in ('bytes', 'mode', 'extra', 'duplicate', 'command'):
        altered = io.BytesIO()
        with tarfile.open(fileobj=altered, mode='w') as target:
            for member, data in members:
                member = copy.copy(member)
                if member.name == 'config/skills/deliver.py':
                    if change == 'bytes': data = b'x' * len(data)
                    if change == 'mode': member.mode ^= 0o020
                target.addfile(member, io.BytesIO(data) if data is not None else None)
            if change in ('extra', 'duplicate'):
                member = tarfile.TarInfo('unknown' if change == 'extra' else 'config/skills/deliver.py')
                target.addfile(member, io.BytesIO(b''))
        argv = command[:-1] + ['unknown'] if change == 'command' else command
        assert subprocess.run(argv, input=altered.getvalue(), capture_output=True).returncode == 99, change
print('ok: migration-history skill delivery normalizes source modes and accepts only exact package commands and archive identity')

with tempfile.TemporaryDirectory(prefix='subyard-history-staging-', dir='/tmp') as temporary:
    for prefix in (b'', b'published '):
        for change in ('valid', 'inode', 'mode', 'prefix', 'published', 'published-inode', 'published-mode',
                       'published-mtime', 'unknown', 'collision', 'leftover'):
            if not prefix and change == 'prefix': continue
            case = Path(tempfile.mkdtemp(dir=temporary))
            evidence = case/'evidence'
            evidence.mkdir(mode=0o700)
            captured = evidence/'first.ledger.captured.json'
            staged = evidence/'.first.ledger.applied.json.pending'
            final = evidence/'first.ledger.applied.json'
            captured.write_bytes(b'captured\n'); captured.chmod(0o600)
            staged.write_bytes(prefix); staged.chmod(0o600)
            if change == 'unknown':
                unknown = evidence/'.unknown.pending'
                unknown.write_bytes(b''); unknown.chmod(0o600)
            if change == 'collision':
                final.write_bytes(b'published result\n'); final.chmod(0o600)
            try:
                published, pending = globals_['history_cut_snapshot'](case, {'steps': [{'id': 'first.ledger'}]})
                staged.write_bytes(b'published result\n')
                staged.rename(final)
                if change == 'inode':
                    replacement = evidence/'replacement'
                    replacement.write_bytes(final.read_bytes()); replacement.chmod(0o600)
                    replacement.replace(final)
                if change == 'mode': final.chmod(0o640)
                if change == 'prefix': final.write_bytes(b'changed prefix\n')
                if change == 'published': captured.write_bytes(b'changed published evidence\n')
                if change == 'published-inode':
                    replacement = evidence/'replacement'
                    replacement.write_bytes(captured.read_bytes()); replacement.chmod(0o600)
                    replacement.replace(captured)
                if change == 'published-mode': captured.chmod(0o640)
                if change == 'published-mtime':
                    info = captured.stat()
                    os.utime(captured, ns=(info.st_atime_ns, info.st_mtime_ns + 1_000_000_000))
                if change == 'leftover':
                    staged.write_bytes(b''); staged.chmod(0o600)
                globals_['history_preserves_cut_evidence'](case, published, pending)
            except RuntimeError:
                assert change != 'valid', (prefix, change)
            else:
                assert change == 'valid', (prefix, change)
print('ok: migration-history cuts preserve published evidence and only complete exact staged prefixes')

verify = globals_['verify']
with tempfile.TemporaryDirectory(prefix='subyard-compat-scratch-', dir='/tmp') as temporary:
    base = Path(temporary)
    owned = base / 'owned'
    owned.mkdir()
    sentinels = [base / 'outside', owned / 'incus.socket', owned / 'incus-observation.json',
                 owned / 'empty-incus', owned / 'baseline-assets/payload',
                 owned / 'compatible-power-release/payload', owned / 'compatible-power-source/payload']
    for sentinel in sentinels:
        sentinel.parent.mkdir(exist_ok=True)
        sentinel.write_text('retained')
    previous = []
    completed = []
    fail_final = False
    interrupted_result = -signal.SIGKILL

    class ScratchFixture:
        def __init__(self, path, *args):
            assert all(not prior.exists() for prior in previous), previous
            assert path.parent == owned
            self.root = path
            self.root.mkdir()
            (path / 'bulk').write_bytes(b'payload')
            self.runtime = path / 'runtime'
            self.runtime.mkdir()
            self.initial = 'initial'
            (self.runtime / 'current').symlink_to(self.initial)
            self.journal = path / 'journal.json'
            self.journal.write_text(json.dumps({'checkpoint': 'incomplete', 'transaction': 'authorized'}))
            self.old_launcher = path / 'old/bin/yard'
            self.env = {}
            self.checks = 0
            self.completions = 0
            previous.append(path)

        def alive(self):
            assert (self.root / 'bulk').read_bytes() == b'payload'
            assert all(sentinel.read_text() == 'retained' for sentinel in sentinels)

        def check(self):
            self.alive()
            self.checks += 1
            return {'outcome': {'status': 'migration-required' if self.checks == 1 else 'recovering',
                                'target': 'candidate'}}

        def update(self, *args, **kwargs):
            self.alive()

        def complete(self):
            self.alive()
            self.completions += 1
            if fail_final and self.completions == 2:
                raise RuntimeError('workload final assertion')
            completed.append(self.root)

    def consume(fixture):
        fixture.alive()

    def interrupted(command, env, timeout):
        fixture_root = Path(command[command.index('--marker') + 1]).parent
        assert (fixture_root / 'bulk').is_file()
        (fixture_root / 'activation-observed.json').write_text('observed')
        return SimpleNamespace(returncode=interrupted_result, stdout='', stderr='')

    with patch.dict(globals_, {'Fixture': ScratchFixture,
            'compatible_power_release': lambda *args: (owned / 'compatible-power-release', 'bridge'),
            'verify_checkpoint_refusal': consume, 'verify_completed_activation_drift': consume,
            'verify_power_rollback_refusal': consume, 'verify_epoch_one_publication_gate': consume,
            'run_process': interrupted}):
        for baseline_version in ('first', 'second'):
            verify(base, 'candidate', base, 'amd64', owned, baseline_version)
            assert all(not path.exists() for path in previous)
        assert len(completed) == 10
        assert all(sentinel.read_text() == 'retained' for sentinel in sentinels)

        fail_final = True
        with patch.object(globals_['shutil'], 'rmtree', side_effect=AssertionError('cleanup hid workload failure')):
            try:
                verify(base, 'candidate', base, 'amd64', owned, 'failed')
                raise AssertionError('workload failure passed')
            except RuntimeError as error:
                assert str(error) == 'workload final assertion'
        assert (previous[-1] / 'bulk').is_file()
        previous.clear()
        fail_final = False
        with patch.object(globals_['shutil'], 'rmtree', side_effect=OSError('cleanup refused')):
            try:
                verify(base, 'candidate', base, 'amd64', owned, 'cleanup-failed')
                raise AssertionError('cleanup failure passed')
            except OSError as error:
                assert str(error) == 'cleanup refused'
        assert (previous[-1] / 'bulk').is_file()
        previous.clear()
        interrupted_result = 0
        try:
            verify(base, 'candidate', base, 'amd64', owned, 'not-interrupted')
            raise AssertionError('missing deliberate SIGKILL passed')
        except RuntimeError as error:
            assert 'did not interrupt the real update' in str(error)
        assert (previous[-1] / 'bulk').is_file()

    release = base / 'release'
    release.mkdir()
    archive = release / 'subyard-candidate-linux-amd64.tar.gz'
    with tarfile.open(archive, 'w:gz') as output:
        member = tarfile.TarInfo('bin/yard')
        member.size = 32
        output.addfile(member, io.BytesIO(b'x' * 32))
    assert globals_['archive_footprint'](archive) == (32, 2)
    baseline = base / 'baseline'
    baseline.mkdir()
    baseline_archive = baseline / 'subyard-0.19.0-linux-amd64.tar.gz'
    baseline_archive.write_bytes(archive.read_bytes())
    process = Mock(side_effect=AssertionError('subprocess started before capacity refusal'))
    for resource, available in (
            ('bytes', SimpleNamespace(f_bavail=15, f_frsize=4, f_files=100, f_favail=100)),
            ('inodes', SimpleNamespace(f_bavail=100, f_frsize=4, f_files=100, f_favail=3))):
        with patch.dict(globals_, {'run_process': process}), \
             patch.dict(globals_['DIRECT_BASELINE_SHA256'], {'amd64': hashlib.sha256(archive.read_bytes()).hexdigest()}), \
             patch.object(globals_['subprocess'], 'Popen', process), \
             patch.object(globals_['os'], 'statvfs', return_value=available) as statvfs, \
             patch.object(sys, 'argv', ['verify', '--release-dir', str(release), '--version', 'candidate',
                                        '--only-direct-019', '--direct-baseline-dir', str(baseline)]), \
             patch.object(globals_['platform'], 'machine', return_value='x86_64'), \
             patch.object(globals_['platform'], 'system', return_value='Linux'):
            try:
                globals_['main']()
                raise AssertionError('known capacity shortage passed')
            except RuntimeError as error:
                assert 'insufficient /tmp ' + resource in str(error)
            statvfs.assert_called_once_with('/tmp')
            statvfs.reset_mock()
            refused_root = base / 'refused-fixture'
            try:
                globals_['Fixture'](refused_root, release, 'candidate', baseline, 'amd64', '0.19.0')
                raise AssertionError('fixture install ignored capacity shortage')
            except RuntimeError as error:
                assert 'insufficient /tmp ' + resource in str(error)
            assert not refused_root.exists()
            statvfs.assert_called_once_with('/tmp')
        process.assert_not_called()
    unknown = io.StringIO()
    with contextlib.redirect_stdout(unknown), \
         patch.object(globals_['os'], 'statvfs', return_value=SimpleNamespace(
             f_bavail=0, f_frsize=4096, f_files=0, f_favail=0)):
        globals_['check_scratch_capacity'](base / 'unavailable', [None])
    assert 'UNKNOWN: 2 unavailable archive(s)' in unknown.getvalue()
    assert 'UNKNOWN inodes' in unknown.getvalue()
    assert 'passing this floor does not guarantee capacity' in unknown.getvalue()
    assert 'PASS' not in unknown.getvalue()
    with patch.object(globals_['os'], 'statvfs', return_value=SimpleNamespace(
            f_bavail=8, f_frsize=4, f_files=0, f_favail=0)):
        globals_['check_scratch_capacity'](archive)
print('ok: compatibility scratch cleanup follows final consumers and refuses known capacity shortages before subprocesses')
PY
