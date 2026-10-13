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
exit 17
SH
chmod 0755 "$root/dev/package-engine.sh"
export ANDROID_CANDIDATE_TEST_BUILD_MARKER="$tmp/built"
rc=0
android_candidate_install "$root" "$tmp/state" > "$tmp/output" 2> "$tmp/error" || rc=$?
[ "$rc" -eq 17 ]
[ -f "$tmp/built" ]
[ ! -s "$tmp/output" ]
[ ! -e "$tmp/state/runtime" ]
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
