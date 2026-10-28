#!/usr/bin/env bash
# Candidate preparation must fail closed before Android lifecycle actions.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
tmp="$(mktemp -d /tmp/subyard-android-candidate.XXXXXX)"
trap 'rm -rf -- "$tmp"' EXIT
. "$ROOT/config/profiles/android/tests/e2e/android-candidate.sh"
root="$tmp/root"
mkdir -p "$root/dev" "$root/tests/helpers" "$root/.subyard-acceptance/release"
cp "$ROOT/tests/helpers/release-candidate.sh" "$root/tests/helpers/release-candidate.sh"
cat > "$root/dev/package-engine.sh" <<'SH'
#!/usr/bin/env bash
touch "$ANDROID_CANDIDATE_TEST_BUILD_MARKER"
while [ "$#" -gt 0 ]; do
  if [ "$1" = --version ]; then printf '%s\n' "$2" > "$ANDROID_CANDIDATE_TEST_VERSION"; break; fi
  shift
done
exit 17
SH
chmod 0755 "$root/dev/package-engine.sh"
export ANDROID_CANDIDATE_TEST_BUILD_MARKER="$tmp/built"
export ANDROID_CANDIDATE_TEST_VERSION="$tmp/fallback-version"
rc=0
android_candidate_install "$root" "$tmp/state" > "$tmp/output" 2> "$tmp/error" || rc=$?
[ "$rc" -eq 17 ]
[ -f "$tmp/built" ]
[ ! -s "$tmp/output" ]
[ ! -e "$tmp/state/runtime" ]
# Execute the product source-version policy against the helper's selected version.
cat > "$tmp/source-version_test.go" <<'GO'
package releaseruntime
import ("os"; "testing")
func TestAndroidFixtureSourceVersion(t *testing.T) {
    if supportedSourceVersion("0.1.0-android.e2e") { t.Fatal("retired fixture version was admitted") }
    if !supportedSourceVersion(os.Getenv("ANDROID_CANDIDATE_FIXTURE_VERSION")) { t.Fatal("Android fixture version is below the supported source floor") }
}
GO
python3 - "$ROOT" "$tmp" <<'PY'
import json, pathlib, sys
root, temporary = map(pathlib.Path, sys.argv[1:])
(temporary/'overlay.json').write_text(json.dumps({'Replace': {
    str(root/'internal/adapters/releaseruntime/android_fixture_source_version_test.go'): str(temporary/'source-version_test.go')}}))
PY
chmod 0600 "$tmp/source-version_test.go" "$tmp/overlay.json"
(cd "$ROOT" && ANDROID_CANDIDATE_FIXTURE_VERSION="$(cat "$tmp/fallback-version")" \
  go test -overlay "$tmp/overlay.json" ./internal/adapters/releaseruntime -run '^TestAndroidFixtureSourceVersion$' -count=1 -v)
rm "$tmp/built"
# A supplied invalid candidate must never silently rebuild from source.
printf '{}' > "$root/.subyard-acceptance/candidate.json"
rc=0
android_candidate_install "$root" "$tmp/state" > "$tmp/output" 2> "$tmp/error" || rc=$?
[ "$rc" -eq 2 ]
[ ! -e "$tmp/built" ]
[ ! -s "$tmp/output" ]
[ ! -e "$tmp/state/runtime" ]
export ANDROID_CANDIDATE_TEST_PLATFORM_MARKER="$tmp/platform"
export ANDROID_CANDIDATE_TEST_BOOTSTRAP_MARKER="$tmp/bootstrap"
android_candidate_platform() { touch "$ANDROID_CANDIDATE_TEST_PLATFORM_MARKER"; }
android_candidate_command() { "$@"; }
case "$(uname -s)/$(uname -m)" in
  Linux/x86_64) arch=amd64; foreign_arch=arm64 ;;
  Linux/aarch64|Linux/arm64) arch=arm64; foreign_arch=amd64 ;;
  *) printf 'unsupported test platform\n' >&2; exit 1 ;;
esac
release="$root/.subyard-acceptance/release"
cat > "$release/subyard-install.sh" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$YARD_RELEASE_BASE_URL" "$YARD_RUNTIME_ROOT" "$YARD_RELEASE_CACHE" "$@" \
  > "$ANDROID_CANDIDATE_TEST_BOOTSTRAP_MARKER"
SH
prepare_candidate() {
  python3 - "$root/.subyard-acceptance" "$1" <<'PY'
import hashlib, io, json, pathlib, sys, tarfile
root, name = pathlib.Path(sys.argv[1]), sys.argv[2]
files = {"bin/yard": b"#!/bin/sh\n", "bin/yard-engine": b"#!/bin/sh\n"}
files["runtime-files.sha256"] = ''.join(hashlib.sha256(v).hexdigest() + "  " + k + "\n"
                                       for k, v in files.items()).encode()
archive = root / 'release' / name
with tarfile.open(archive, 'w:gz') as bundle:
    for path, data in files.items():
        entry = tarfile.TarInfo(path)
        entry.size, entry.mode = len(data), 0o755 if path.startswith('bin/') else 0o644
        bundle.addfile(entry, io.BytesIO(data))
(root / 'candidate.json').write_text(json.dumps(dict(version='1.2.3', runtime_file=name,
    runtime_sha256=hashlib.sha256(archive.read_bytes()).hexdigest())))
PY
}
# A checksummed archive must be the exact archive selected by host bootstrap.
for name in "subyard-9.9.9-linux-$arch.tar.gz" "subyard-1.2.3-linux-$foreign_arch.tar.gz"; do
  prepare_candidate "$name"
  rc=0
  android_candidate_install "$root" "$tmp/state" > "$tmp/output" 2> "$tmp/error" || rc=$?
  [ "$rc" -eq 2 ]
  [ ! -s "$tmp/output" ]
  [ ! -e "$tmp/platform" ]
  [ ! -e "$tmp/bootstrap" ]
  [ ! -e "$tmp/built" ]
done
prepare_candidate "subyard-1.2.3-linux-$arch.tar.gz"
android_candidate_install "$root" "$tmp/state" > "$tmp/output" 2> "$tmp/error"
[ "$(cat "$tmp/output")" = "$tmp/state/runtime/current/bin/yard" ]
[ -f "$tmp/platform" ]
[ ! -e "$tmp/built" ]
printf '%s\n' "file://$release" "$tmp/state/runtime" "$tmp/state/releases" \
  --version 1.2.3 --yes > "$tmp/expected-bootstrap"
cmp "$tmp/expected-bootstrap" "$tmp/bootstrap"
printf 'ok: Android candidate build errors and invalid supplied assets fail closed\n'
printf 'ok: Android supplied candidate binds host archive and bootstrap identity\n'

# Exercise the real controller context across installation command substitution,
# later yard commands and its actual telemetry callback dispatch.
cat > "$tmp/migration-context_test.go" <<'GO'
package cli
import (
    "context"
    "os"
    "path/filepath"
    "strings"
    "testing"
    "github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
    "github.com/Subyard/Subyard/internal/config"
)
func TestAndroidFixtureMigrationContext(t *testing.T) {
    root := os.Getenv("ANDROID_CANDIDATE_TEST_REPOSITORY")
    catalog, err := config.LoadCatalog(root)
    if err != nil { t.Fatal(err) }
    inherited := map[string]string{}
    for _, entry := range os.Environ() {
        name, value, _ := strings.Cut(entry, "=")
        inherited[name] = value
    }
    program := CLI{catalog: catalog, baseEnv: inherited, options: Options{RepositoryRoot: root}}
    operation := program.releaseActivationOperation()
    if _, present := operation.env["SUBYARD_KEYS_SYSTEMD_DIR"]; present {
        t.Fatal("activation retained a transient setting")
    }
    loaded, err := config.Load(config.LoadOptions{Catalog: &catalog, RepositoryRoot: root,
        OperatorHome: operation.env["SUBYARD_OPERATOR_HOME"], YardName: "default", Environment: operation.env})
    if err != nil { t.Fatal(err) }
    if loaded.Environment["SUBYARD_KEYS_SYSTEMD_DIR"] != filepath.Join(inherited["SUBYARD_OPERATOR_HOME"], ".config/systemd/user") {
        t.Fatal("activation changed the operator-owned unit defaults")
    }
    runtime := reconcileruntime.Runtime{RepositoryRoot: root, Yard: loaded.Context,
        Environment: environmentList(operation.env, structuredCommandContext(loaded))}
    presence, err := runtime.InstalledRuntimePresence(context.Background(), "key-tools-runtime")
    if os.Getenv("ANDROID_CANDIDATE_TEST_EXPECT_FOREIGN") == "1" {
        if err == nil || !strings.Contains(err.Error(), "credential timer files are not owned") {
            t.Fatal("activation did not refuse the prior installation's units")
        }
    } else if err != nil || presence.KeyTimer {
        t.Fatal("activation did not retain the fixture's isolated unit defaults")
    }
}
GO
python3 - "$ROOT" "$tmp" <<'PY'
import json, pathlib, sys
root, temporary = map(pathlib.Path, sys.argv[1:])
(temporary/'migration-overlay.json').write_text(json.dumps({'Replace': {
    str(root/'internal/cli/android_fixture_context_test.go'): str(temporary/'migration-context_test.go')}}))
PY
chmod 0600 "$tmp/migration-context_test.go" "$tmp/migration-overlay.json"
(cd "$ROOT" && go test -c -overlay "$tmp/migration-overlay.json" -o "$tmp/migration-probe" ./internal/cli)
python3 -B - "$ROOT" "$tmp" "$root" <<'PY'
import hashlib, json, os, pathlib, subprocess, sys
repository, temporary, candidate = map(pathlib.Path, sys.argv[1:])
home = temporary/'shared-home'
units = home/'.config/systemd/user'
for path in (home, home/'.config', home/'.config/systemd', units):
    path.mkdir(); path.chmod(0o700)
for name in ('prior', 'next'):
    launcher = temporary/name/'runtime/current/bin/yard'
    launcher.parent.mkdir(parents=True)
    launcher.write_text('#!/bin/sh\nexit 0\n'); launcher.chmod(0o755)
prior = temporary/'prior/runtime/current/bin/yard'
(units/'subyard-keys-sync.service').write_text((repository/'config/systemd/subyard-keys-sync.service.in').read_text().replace('@YARD_BIN@', str(prior)))
(units/'subyard-keys-sync.timer').write_bytes((repository/'config/systemd/subyard-keys-sync.timer.in').read_bytes())
(units/'.subyard-keys-sync.json').write_text(json.dumps({'schema': 1, 'yard': str(prior)}, indent=2)+'\n')
for path in units.iterdir(): path.chmod(0o600 if path.name.startswith('.') else 0o644)
def shared_snapshot():
    return {str(path.relative_to(home)): (path.stat().st_mode, hashlib.sha256(path.read_bytes()).hexdigest())
            for path in home.rglob('*') if path.is_file()}
before = shared_snapshot()
env = dict(PATH=os.environ['PATH'], HOME=str(home), SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE='1')
migration_probe = temporary/'migration-probe'
result = subprocess.run([str(migration_probe), '-test.run=^TestAndroidFixtureMigrationContext$'],
    env={**env, 'SUBYARD_OPERATOR_HOME': str(home), 'SUBYARD_CONFIG_HOME': str(temporary/'before-config'),
         'SUBYARD_HOME': str(temporary/'before-data'), 'SUBYARD_KEYS_SYSTEMD_DIR': str(temporary/'transient-units'),
         'YARD_RUNTIME_ROOT': str(temporary/'next/runtime'), 'ANDROID_CANDIDATE_TEST_REPOSITORY': str(repository),
         'ANDROID_CANDIDATE_TEST_EXPECT_FOREIGN': '1'}, capture_output=True, text=True)
assert result.returncode == 0, 'real activation reset did not expose the prior global units'
print('ok: real activation reset drops the transient unit setting and refuses prior global units', flush=True)
proof = temporary/'unit-context.jsonl'
probe = temporary/'context-probe'
probe.write_text('''#!/usr/bin/python3
import json, os, pathlib, subprocess, sys
state = pathlib.Path(os.environ['SUBYARD_HOME']).parent
operator = pathlib.Path(os.environ['SUBYARD_OPERATOR_HOME'])
units = operator/'.config/systemd/user'
assert operator == state/'operator-home' and operator.is_dir() and not operator.is_symlink()
assert operator.stat().st_mode & 0o777 == 0o700 and operator.stat().st_uid == os.getuid()
assert 'SUBYARD_KEYS_SYSTEMD_DIR' not in os.environ
assert os.environ['HOME'] == os.environ['ANDROID_CANDIDATE_TEST_HOME'] and pathlib.Path(os.environ['HOME']) != operator
assert os.environ['STORAGE_PATH'] == str(pathlib.Path(os.environ['HOME'])/'.cache/subyard-e2e-platform/incus/incus/storage')
assert os.environ['SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE'] == '1'
operation = next((name for name in ('install','migrate','init','callback') if name in sys.argv[1:]), 'callback')
if operation in ('install', 'migrate'):
    result = subprocess.run([os.environ['ANDROID_CANDIDATE_TEST_MIGRATION_PROBE'], '-test.run=^TestAndroidFixtureMigrationContext$'], capture_output=True)
    assert result.returncode == 0, 'real activation inspection refused isolated fixture defaults'
with open(os.environ['ANDROID_CANDIDATE_TEST_PROOF'], 'a') as stream:
    stream.write(json.dumps(dict(operation=operation, units=str(units)))+'\\n')
if operation == 'migrate': print('{"outcome":{"status":"ready"}}')
'''); probe.chmod(0o755)
installer = candidate/'.subyard-acceptance/release/subyard-install.sh'
installer.write_text('''#!/usr/bin/env bash
set -euo pipefail
install -d -m 0700 "$YARD_RUNTIME_ROOT/current/bin"
install -m 0755 "$ANDROID_CANDIDATE_TEST_PROBE" "$YARD_RUNTIME_ROOT/current/bin/yard"
"$YARD_RUNTIME_ROOT/current/bin/yard" install
'''); installer.chmod(0o755)
controller = (repository/'config/profiles/android/tests/e2e/android-pool-runtime.sh').read_text()
start = controller.index('state="$(mktemp -d /var/tmp/subyard-android-pool-runtime.')
context = controller[start:controller.index('\nincus() {', start)]
start = controller.index('  if [ -n "${SUBYARD_E2E_PROFILE_EXERCISE:-}" ]; then')
callback = controller[start:controller.index('\n  fi', start)+len('\n  fi')]
script = '''set -euo pipefail
. "$1/tests/helpers/test-context.sh"
setup_test_context "$2/context"
. "$1/config/profiles/android/tests/e2e/android-candidate.sh"
'''
# Only the native temporary-directory leaf is redirected into this test's /tmp root.
script += '''mktemp() {
  [ "$*" = '-d /var/tmp/subyard-android-pool-runtime.XXXXXX' ]
  local created
  created="$(command mktemp -d "$ANDROID_CANDIDATE_TEST_TEMP/state.XXXXXX")"
  chmod 0700 "$created"
  printf '%s\\n' "$created"
}
''' + context + '''
export YARD_RUNTIME_ROOT="$state/runtime"
android_candidate_platform() { :; }
android_candidate_command() { "$@"; }
YARD_BIN="$(android_candidate_install "$3" "$state")"
android_candidate_command "$YARD_BIN" migrate --check --json | jq -e '.outcome.status == "ready"' >/dev/null
android_candidate_command "$YARD_BIN" -Y "$YARD_NAME" init --yes
android_phase_begin() { :; }
android_phase_end() { :; }
root="$1"; project=fixture; instance=fixture
export SUBYARD_E2E_PROFILE_EXERCISE="$ANDROID_CANDIDATE_TEST_PROBE" SUBYARD_TELEMETRY_NATIVE_STATE="$state/native"
''' + callback
result = subprocess.run(['bash', '-c', script, 'controller', str(repository), str(temporary), str(candidate)],
    env={**env, 'ANDROID_CANDIDATE_TEST_TEMP': str(temporary), 'ANDROID_CANDIDATE_TEST_HOME': str(home),
         'SUBYARD_KEYS_SYSTEMD_DIR': str(units),
         'ANDROID_CANDIDATE_TEST_PROOF': str(proof), 'ANDROID_CANDIDATE_TEST_PROBE': str(probe),
         'ANDROID_CANDIDATE_TEST_MIGRATION_PROBE': str(migration_probe),
         'ANDROID_CANDIDATE_TEST_REPOSITORY': str(repository)}, capture_output=True, text=True)
assert result.returncode == 0, 'controller context failed'
rows = [json.loads(line) for line in proof.read_text().splitlines()]
assert [row['operation'] for row in rows] == ['install', 'migrate', 'init', 'callback'], rows
assert len({row['units'] for row in rows}) == 1 and pathlib.Path(rows[0]['units']) != units
assert shared_snapshot() == before, 'prior installation credential unit bytes or modes changed'
print('ok: Android private operator home survives real activation reset, install, migrate, init and callback dispatch')
PY
