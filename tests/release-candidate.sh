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
python3 -B "$ROOT/tests/helpers/skills-transport-test.py" "$ROOT"
python3 -B "$ROOT/tests/helpers/release-candidate-test.py"
python3 -B "$ROOT/tests/helpers/evidence-oracle-test.py"

# Compatibility controls bind the native runtime and bound owned scratch lifetime
# without changing deliberate interruption or shared fixture ownership.
PYTHONDONTWRITEBYTECODE=1 python3 -B - "$ROOT" <<'PY'
import contextlib
import hashlib
import io
import json
from pathlib import Path
import runpy
import os
import shlex
import shutil
import subprocess
import signal
import sys
import tarfile
import tempfile
from types import SimpleNamespace
from unittest.mock import Mock, patch

root = Path(sys.argv[1])
helper = runpy.run_path(str(root / 'dev/verify-release-upgrades.py'))['history_materialized_fixture']
with tempfile.TemporaryDirectory(prefix='subyard-skill-transport-', dir='/tmp') as temporary:
    base = Path(temporary)
    shims = base / 'commands'
    shims.mkdir(mode=0o700)
    unexpected = base / 'unexpected'
    helper.__globals__['write_observation_incus'](shims, base/'control', unexpected)
    env = {**os.environ, **dict.fromkeys(('SUBYARD_OPERATOR_HOME SUBYARD_CONFIG_DIR SUBYARD_CONFIG_HOME '
           'SUBYARD_HOME STORAGE_PATH HOST_BASE RESTRICTED_DISK_PATHS ACCESS_KIND YARD_KIND '
           'INCUS_BRIDGE SSH_HOST DEV_UID DEV_SUDO FORWARD_SSH_AGENT NESTED_E2E_VMS').split(), ''),
           'SUBYARD_ENGINE_CONTEXT': '1', 'SUBYARD_ENGINE_CONTEXT_SCHEMA': '1', 'DEV_USER': 'dev',
           'YARD_INSTANCE_NAME': 'yard', 'INCUS_PROJECT': 'subyard', 'ENVIRONMENT_PROFILES': '',
           'ALLOWS_CODING_TOOLS': 'true', 'PATH': str(shims) + ':/usr/bin:/bin'}
    result = subprocess.run(['bash', str(root/'scripts/refresh-agent-skills.sh'), 'apply'],
                            env=env, capture_output=True)
    assert result.returncode == 0 and not unexpected.exists(), 'native skill tar pipeline was rejected'
    contracts = helper.__globals__['skills_transport_contract'](root)
    arguments = list(next(command for command, _ in contracts if command[-1] == 'apply'))
    program = arguments[-3]
    payload = subprocess.check_output([sys.executable, '-B', str(root/'config/skills/package.py'), 'archive', str(root)])
    check = next(command for command, _ in contracts if command[-1] == 'check')
    assert subprocess.run([str(shims/'incus'), *check], input=payload, capture_output=True).returncode == 0
    with tarfile.open(fileobj=io.BytesIO(payload)) as archive:
        files = [(member, archive.extractfile(member).read() if member.isfile() else None)
                 for member in archive.getmembers()]
        end = archive.offset
    assert end % 512 == 0 and len(payload[end:]) >= 1024 and not any(payload[end:])
    changed = io.BytesIO()
    with tarfile.open(fileobj=changed, mode='w') as archive:
        altered = False
        for member, content in files:
            if content and not altered:
                content = bytes([content[0] ^ 1]) + content[1:]
                altered = True
            archive.addfile(member, io.BytesIO(content) if content is not None else None)
    assert altered
    variants = [('body', [*arguments[:14], program + '\n:', *arguments[15:]], payload),
                ('mode', [*arguments[:-1], 'unknown'], payload),
                ('identity', [*arguments[:5], 'DEV_USER=foreign', *arguments[6:]], payload),
                ('archive', arguments, changed.getvalue()),
                ('length', [*arguments, 'unexpected'], payload),
                ('truncated', arguments, payload[:-1]),
                ('missing-eof', arguments, payload[:end]),
                ('one-eof-block', arguments, payload[:end] + bytes(512)),
                ('first-eof-byte', arguments, payload[:end] + b'x' + payload[end+1:]),
                ('trailer', arguments, payload[:-1] + b'x'),
                ('appended', arguments, payload + b'x' * 512),
                ('over-limit', arguments, payload.ljust((16 << 20) + 512, b'\0'))]
    for name, argv, data in variants:
        result = subprocess.run([str(shims/'incus'), *argv], input=data, capture_output=True)
        assert result.returncode == 99 and unexpected.exists(), name
        unexpected.unlink()
    # Each admitted archive must come wholly from one package, even when the
    # same guest program is shared by two versions with different skill bytes.
    other = base/'other'
    for relative in ('config/skills', 'config/profiles', 'scripts'):
        shutil.copytree(root/relative, other/relative)
    selected = [other/name for name, value in contracts[0][1].items()
                if value[0] == 'file' and name.startswith('config/skills/')
                and name != 'config/skills/refresh.sh'][:2]
    assert len(selected) == 2
    for path in selected:
        path.write_bytes(path.read_bytes() + b'\n# alternate package\n')
    helper.__globals__['write_observation_incus'](shims, base/'control', unexpected, [root, other])
    alternate = subprocess.check_output([sys.executable, '-B', str(other/'config/skills/package.py'), 'archive', str(other)])
    for data in (payload, alternate):
        assert subprocess.run([str(shims/'incus'), *arguments], input=data, capture_output=True).returncode == 0
    hybrid = io.BytesIO()
    with tarfile.open(fileobj=hybrid, mode='w') as archive:
        for member, content in files:
            if member.name == str(selected[0].relative_to(other)):
                content = selected[0].read_bytes()
                member.size = len(content)
            archive.addfile(member, io.BytesIO(content) if content is not None else None)
    assert subprocess.run([str(shims/'incus'), *arguments], input=hybrid.getvalue(),
                          capture_output=True).returncode == 99, 'mixed package archive was accepted'
print('ok: skill transport drains the native tar pipeline and refuses changed body, arguments and archive')

with tempfile.TemporaryDirectory(prefix='subyard-history-architecture-', dir='/tmp') as temporary:
    base = Path(temporary)
    for arch, foreign in (('amd64', 'arm64'), ('arm64', 'amd64')):
        for order in ('native-first', 'foreign-first'):
            case = base / arch / order
            release = case / 'release'
            release.mkdir(parents=True)
            native = release / f'subyard-1.2.3-linux-{arch}.tar.gz'
            other = release / f'subyard-1.2.3-linux-{foreign}.tar.gz'
            shipped = (root / 'config/agents.env').read_bytes()
            for archive_path, content in ((native, shipped), (other, shipped + b'\n# foreign architecture fixture\n')):
                with tarfile.open(archive_path, 'w:gz') as archive:
                    member = tarfile.TarInfo('config/agents.env')
                    member.size, member.mode = len(content), 0o644
                    archive.addfile(member, io.BytesIO(content))
                    directory = tarfile.TarInfo('config/skills')
                    directory.type, directory.mode = tarfile.DIRTYPE, 0o755
                    archive.addfile(directory)
                    skill = tarfile.TarInfo('config/skills/fixture')
                    skill.mode, skill.size = 0o644, 1
                    archive.addfile(skill, io.BytesIO(b'x'))
                    registry = json.dumps({'migrations': []}).encode()
                    member = tarfile.TarInfo('config/release-transition.json')
                    member.size, member.mode = len(registry), 0o644
                    archive.addfile(member, io.BytesIO(registry))
            state = case / 'fixture'
            config = state / 'config'
            config.mkdir(parents=True)
            fixture = SimpleNamespace(root=state, config=config, runtime=state/'runtime',
                                      journal=config/'journal.json', version='1.2.3', arch=arch,
                                      old_launcher=state/'old/bin/yard',
                                      env={'YARD_RELEASE_BASE_URL': release.as_uri()})
            old_source = fixture.old_launcher.parent.parent / 'config/agents.env'
            old_source.parent.mkdir(parents=True)
            old_source.write_bytes(b'# original released fixture config\n')
            old_source.chmod(0o600)
            # Minimal synthetic completed-head inputs for this archive-selection control.
            for path, value in ((fixture.journal, {'checkpoint': 'complete'}),
                                (old_source.parent/'release-transition.json', {'currentEpochs': {}}),
                                (config/'release-transition/v2/history-checkpoint.json', {'domains': {}})):
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(json.dumps(value))
                path.chmod(0o600)
            def show_source(command):
                assert command == [str(fixture.old_launcher), '-Y', 'default', 'config', 'show', 'AGENT_fixture_CONFIG']
                return 'effective: ' + str(old_source) + '\n'
            fixture.run = show_source
            captured = []
            def installed_skills(*args, **kwargs):
                package = args[3][-1]
                assert (package/'config/skills').stat().st_mode & 0o777 == 0o700
                assert (package/'config/skills/fixture').stat().st_mode & 0o777 == 0o600
            with patch.dict(helper.__globals__, {
                    'write_observation_incus': installed_skills,
                    'set_fixture_observation': lambda *args, **kwargs: captured.append(kwargs['history'])}), \
                 patch.object(Path, 'glob', return_value=iter([native, other] if order == 'native-first' else [other, native])):
                history, source, guest, _ = helper(fixture, case, case/'control')
            assert source.read_bytes() == shipped and guest.read_bytes() == old_source.read_bytes()
            assert history['priorSource'] == str(old_source) and source.read_bytes() != guest.read_bytes()
            expected = '1.2.3-' + hashlib.sha256(native.read_bytes()).hexdigest()[:12]
            assert history['target'] == expected, (arch, history['target'], expected)
            assert captured[0]['target'] == expected
print('ok: migration-history apply control binds the selected architecture in a mixed release inventory')

globals_ = helper.__globals__
with tempfile.TemporaryDirectory(prefix='subyard-bridge-capture-', dir='/tmp') as temporary:
    base = Path(temporary)
    state = base / 'retained-history-legacy'; state.mkdir()
    config = state / 'config'; config.mkdir()
    journal = config / 'journal.json'
    journal.write_text(json.dumps({'checkpoint': 'complete', 'transaction': 'tx',
                                   'goal': {'target': 'source'}}))
    projection = config / 'release-transition/v2/ledger.json'
    projection.parent.mkdir(parents=True); projection.write_text('ledger')
    evidence = base / 'evidence'; evidence.mkdir()
    control = base / 'incus-observation.json'
    marker = state / 'unexpected-host-command'
    argv = state / 'unexpected-host-command.argv.json'
    api = Path(str(control) + '.unexpected')
    outside = base / 'outside'; outside.write_text('outside')
    unsafe = state / 'unexpected-host-command.symlink'; unsafe.symlink_to(outside)
    hardlink = state / 'unexpected-host-command.hardlink'; os.link(outside, hardlink)
    large = state / 'unexpected-host-command.large'; large.write_bytes(b'x' * 65537)
    fixture = SimpleNamespace(root=state, config=config, runtime=state/'runtime', journal=journal,
                              old_launcher=state/'old/bin/yard')
    calls = []
    def run(command):
        calls.append(command)
        if '--check' not in command:
            return ''
        return json.dumps({'outcome': {'status': 'operator-action-required'}, 'blockers': [{}]})
    fixture.run = run
    def observe(*args):
        marker.write_text('rejected')
        argv.write_text('["incus"]')
        api.write_text('api rejection')
        return {}, base/'source', base/'guest', marker
    def reset(path):
        for item in (marker, argv, api): item.unlink()
    stderr = io.StringIO()
    with patch.dict(globals_, {'HISTORY_EVIDENCE': evidence,
                               'history_links': lambda fixture: (('releases/source',), None),
                               'history_materialized_fixture': observe,
                               'set_fixture_observation': reset,
                               'verify_observed_instance_removed': lambda fixture: None}), \
         contextlib.redirect_stderr(stderr):
        try:
            globals_['verify_supported_bridge'](base, 'candidate', base, '0.22.0', 'amd64', base, fixture=fixture)
            raise AssertionError('blocked source inspection passed')
        except RuntimeError as error:
            assert str(error) == 'released direct source is not current before candidate activation'
    captured = list(evidence.glob('retained-history-legacy-observation-*'))
    assert len(captured) == 1 and len(calls) == 2
    assert (captured[0]/marker.name).read_text() == 'rejected'
    assert (captured[0]/argv.name).read_text() == '["incus"]'
    assert (captured[0]/api.name).read_text() == 'api rejection'
    assert sorted(p.name for p in captured[0].iterdir()) == sorted((marker.name, argv.name, api.name))
    assert outside.read_text() == 'outside' and 'bridge observation capture refused:' in stderr.getvalue()
print('ok: failed bridge preserves bounded observation markers before cleanup and retains the original error')

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
    baseline_archive = baseline / f"subyard-{globals_['DIRECT_BASELINE']}-linux-amd64.tar.gz"
    baseline_archive.write_bytes(archive.read_bytes())
    history = base / 'history-baselines'
    history.mkdir()
    history_pins = {'0.19.0': {'amd64': hashlib.sha256(archive.read_bytes()).hexdigest()}}
    history_dir = history / 'baseline-0.19.0'
    history_dir.mkdir()
    history_archive = history_dir / 'subyard-0.19.0-linux-amd64.tar.gz'
    history_archive.write_bytes(archive.read_bytes())
    args = SimpleNamespace(direct_baseline_dir=None, only_migration_history=True,
                           only_direct_019=False, only_fresh_activation_recovery=False,
                           only_legacy_retirement=False, migration_history_baselines=history,
                           legacy_baseline_dir=None)
    with patch.dict(globals_, {'HISTORY_BASELINES': history_pins}):
        assert history_archive in globals_['selected_baseline_archives'](args, release, 'amd64')
        history_archive.write_bytes(b'tampered')
        try:
            globals_['selected_baseline_archives'](args, release, 'amd64')
            raise AssertionError('modified history baseline passed preflight')
        except RuntimeError as error:
            assert 'official baseline checksum' in str(error)
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
                globals_['Fixture'](refused_root, release, 'candidate', baseline, 'amd64', globals_['DIRECT_BASELINE'])
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
