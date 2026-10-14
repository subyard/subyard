#!/usr/bin/env bash
# Host-free candidate identity, owned bootstrap and publication-boundary checks.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
tmp="$(mktemp -d /tmp/subyard-openlit-candidate.XXXXXX)"
trap 'rm -rf -- "$tmp"' EXIT
# shellcheck source=config/profiles/openlit/tests/e2e/candidate.sh
. "$ROOT/config/profiles/openlit/tests/e2e/candidate.sh"
root="$tmp/source"
state="$tmp/state"
install -d -m 0700 "$root/dev" "$root/tests/helpers" "$state"
printf 'openlit-profile-e2e-v1\n' > "$state/.marker"
chmod 0600 "$state/.marker"
cp "$ROOT/tests/helpers/release-candidate.sh" "$root/tests/helpers/"
export OPENLIT_CANDIDATE_TEST_ROOT="$tmp" OPENLIT_CANDIDATE_TEST_GATE_FAIL=''
openlit_candidate_command() { "$@"; }
openlit_candidate_platform() {
  printf '%s\n' "$3" >> "$OPENLIT_CANDIDATE_TEST_ROOT/platform"
}
cat > "$tmp/installer" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$YARD_RELEASE_BASE_URL" "$YARD_RUNTIME_ROOT" "$YARD_RELEASE_CACHE" \
  "$YARD_BIN_DIR" "$YARD_SHELL_RC" "$YARD_LOGIN_RC" "$SUBYARD_CONFIG_HOME" "$SUBYARD_HOME" "$@" \
  >> "$OPENLIT_CANDIDATE_TEST_ROOT/bootstrap"
test -z "${YARD_ENGINE_PATH:-}"
mkdir -p "$YARD_RUNTIME_ROOT/current/bin"
cp "$OPENLIT_CANDIDATE_TEST_ROOT/launcher" "$YARD_RUNTIME_ROOT/current/bin/yard"
chmod 0755 "$YARD_RUNTIME_ROOT/current/bin/yard"
SH
cat > "$tmp/launcher" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
test -z "${YARD_ENGINE_PATH:-}"
printf '%s\n' "$*" >> "$OPENLIT_CANDIDATE_TEST_ROOT/gates"
case "$1" in
  migrate)
    test "$*" = 'migrate --check --json'
    if [ "$OPENLIT_CANDIDATE_TEST_GATE_FAIL" = migration ]; then
      printf '{"outcome":{"status":"blocked"}}\n'
    else
      printf '{"outcome":{"status":"ready"}}\n'
    fi ;;
  _publication-contracts)
    test "$OPENLIT_CANDIDATE_TEST_GATE_FAIL" != publication
    printf 'synthetic-publication-contracts\n' ;;
  *) exit 2 ;;
esac
SH
chmod 0755 "$tmp/installer" "$tmp/launcher"
cat > "$root/dev/package-engine.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$@" >> "$OPENLIT_CANDIDATE_TEST_ROOT/built"
if [ "${OPENLIT_CANDIDATE_TEST_BUILD_FAIL:-0}" != 0 ]; then exit 17; fi
test "$1" = --version && test "$2" = 0.1.0-openlit.e2e && test "$3" = --output-dir
mkdir -p "$4"
cp "$OPENLIT_CANDIDATE_TEST_ROOT/installer" "$4/subyard-install.sh"
SH
chmod 0755 "$root/dev/package-engine.sh"
expect_failure() {
  local expected="$1" rc=0
  openlit_candidate_install "$root" "$state" > "$tmp/output" 2> "$tmp/error" || rc=$?
  [ "$rc" -eq "$expected" ] || { printf 'unexpected OpenLIT candidate exit=%s expected=%s\n' "$rc" "$expected" >&2; return 1; }
  [ ! -s "$tmp/output" ]
}
# An unmarked root is refused before packaging/platform/installer writes.
printf 'foreign\n' > "$state/.marker"
expect_failure 2
[ ! -e "$tmp/built" ] && [ ! -e "$tmp/platform" ] && [ ! -e "$tmp/bootstrap" ]
printf 'openlit-profile-e2e-v1\n' > "$state/.marker"
ln -s "$tmp/foreign" "$state/runtime"
expect_failure 2
[ ! -e "$tmp/built" ]
rm "$state/runtime"
install -d -m 0700 "$state/runtime"
ln -s "$tmp/foreign" "$state/runtime/current"
expect_failure 2
[ ! -e "$tmp/built" ]
rm "$state/runtime/current"
# No metadata means package the source. Packaging failure cannot start setup.
export OPENLIT_CANDIDATE_TEST_BUILD_FAIL=1
expect_failure 17
[ -f "$tmp/built" ] && [ ! -e "$tmp/platform" ]
unset OPENLIT_CANDIDATE_TEST_BUILD_FAIL
rm "$tmp/built"
# A supplied invalid candidate never silently falls back to rebuilding.
install -d -m 0700 "$root/.subyard-acceptance/release"
printf '{}' > "$root/.subyard-acceptance/candidate.json"
expect_failure 2
[ ! -e "$tmp/built" ] && [ ! -e "$tmp/platform" ]
case "$(uname -s)/$(uname -m)" in
  Linux/x86_64) arch=amd64; foreign_arch=arm64 ;;
  Linux/aarch64|Linux/arm64) arch=arm64; foreign_arch=amd64 ;;
  *) printf 'unsupported test platform\n' >&2; exit 1 ;;
esac
prepare_candidate() {
  python3 - "$root/.subyard-acceptance" "$1" <<'PY'
import hashlib, io, json, pathlib, sys, tarfile
root, name = pathlib.Path(sys.argv[1]), sys.argv[2]
files = {'bin/yard': b'#!/bin/sh\n', 'bin/yard-engine': b'#!/bin/sh\n'}
files['runtime-files.sha256'] = ''.join(hashlib.sha256(v).hexdigest()+'  '+k+'\n' for k,v in files.items()).encode()
archive = root/'release'/name
with tarfile.open(archive, 'w:gz') as bundle:
    for path, data in files.items():
        info = tarfile.TarInfo(path)
        info.size, info.mode = len(data), 0o755 if path.startswith('bin/') else 0o644
        bundle.addfile(info, io.BytesIO(data))
(root/'candidate.json').write_text(json.dumps({'version': '1.2.3', 'runtime_file': name, 'runtime_sha256': hashlib.sha256(archive.read_bytes()).hexdigest()}))
PY
}
cp "$tmp/installer" "$root/.subyard-acceptance/release/subyard-install.sh"
for name in "subyard-9.9.9-linux-$arch.tar.gz" "subyard-1.2.3-linux-$foreign_arch.tar.gz"; do
  prepare_candidate "$name"
  expect_failure 2
  [ ! -e "$tmp/platform" ] && [ ! -e "$tmp/bootstrap" ] && [ ! -e "$tmp/built" ]
done
prepare_candidate "subyard-1.2.3-linux-$arch.tar.gz"
export YARD_ENGINE_PATH="$tmp/must-not-use-engine"
openlit_candidate_install "$root" "$state" > "$tmp/output" 2> "$tmp/error"
[ "$(cat "$tmp/output")" = "$state/runtime/current/bin/yard" ]
[ "$(cat "$tmp/platform")" = "$root/.subyard-acceptance/runtime" ]
[ ! -e "$tmp/built" ]
printf '%s\n' "file://$root/.subyard-acceptance/release" "$state/runtime" "$state/releases" \
  "$state/bin" "$state/bashrc" "$state/profile" "$state/config" "$state/data" --version 1.2.3 --yes > "$tmp/expected-bootstrap"
cmp "$tmp/expected-bootstrap" "$tmp/bootstrap"
printf '%s\n' 'migrate --check --json' _publication-contracts > "$tmp/expected-gates"
cmp "$tmp/expected-gates" "$tmp/gates"
# A blocked release boundary must stop before the publication gate/yard creation.
: > "$tmp/gates"
OPENLIT_CANDIDATE_TEST_GATE_FAIL=migration
expect_failure 1
[ "$(cat "$tmp/gates")" = 'migrate --check --json' ]
# The exact originally failing native publication reader is an independent gate.
: > "$tmp/gates"
OPENLIT_CANDIDATE_TEST_GATE_FAIL=publication
expect_failure 1
cmp "$tmp/expected-gates" "$tmp/gates"
OPENLIT_CANDIDATE_TEST_GATE_FAIL=''
rm "$root/.subyard-acceptance/candidate.json"
: > "$tmp/platform"
openlit_candidate_install "$root" "$state" > "$tmp/output" 2> "$tmp/error"
[ -f "$tmp/built" ] && [ "$(cat "$tmp/platform")" = "$root" ]
[ "$(cat "$tmp/output")" = "$state/runtime/current/bin/yard" ]
printf 'ok: OpenLIT candidate owns bootstrap paths and rejects invalid frozen identities\n'
printf 'ok: OpenLIT source packaging and supported migration/publication preflight precede yard actions\n'
