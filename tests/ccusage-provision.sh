#!/usr/bin/env bash
# Hermetic checks for the pinned native ccusage provision hook.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOOK="$ROOT/config/agents/ccusage/provision.sh"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "missing test dependency: $1"; }
for command in cc jq sha256sum tar; do need "$command"; done
[ -x "$HOOK" ] || fail "ccusage provision hook is not executable"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
registry="$tmp/registry"
fake_bin="$tmp/fake-bin"
curl_log="$tmp/curl.log"
mkdir -p "$registry" "$fake_bin" "$tmp/install" "$tmp/work"

# Host-native fixtures keep both package-selection paths executable in one test run.
make_fixture() { # <artifact-version> <metadata-version> <x64|arm64> [binary-version]
  local artifact_version="$1" metadata_version="$2" arch="$3"
  local binary_version="${4:-$metadata_version}"
  local package="ccusage-linux-$arch"
  local build="$tmp/build-${artifact_version}-${metadata_version}-${binary_version}-${arch}"
  local out="$registry/@ccusage/$package/-/$package-${artifact_version}.tgz"
  mkdir -p "$build/package/bin" "$(dirname "$out")"
  cat > "$build/main.c" <<EOF
#include <stdio.h>
#include <string.h>
int main(int argc, char **argv) {
  if (argc == 2 && strcmp(argv[1], "--version") == 0) {
    puts("ccusage ${binary_version}");
    return 0;
  }
  return 64;
}
EOF
  cc -O2 -o "$build/package/bin/ccusage" "$build/main.c"
  chmod 0644 "$build/package/bin/ccusage"
  cat > "$build/package/package.json" <<EOF
{"name":"@ccusage/$package","version":"$metadata_version","os":["linux"],"cpu":["$arch"]}
EOF
  tar -czf "$out" -C "$build" package
  sha256sum "$out" | cut -d' ' -f1
}

sha_123_amd64="$(make_fixture 1.2.3 1.2.3 x64)"
sha_123_arm64="$(make_fixture 1.2.3 1.2.3 arm64)"
sha_124_amd64="$(make_fixture 1.2.4 1.2.4 x64)"
sha_124_arm64="$(make_fixture 1.2.4 1.2.4 arm64)"
sha_bad_metadata="$(make_fixture 9.9.9 9.9.8 x64)"
sha_bad_binary="$(make_fixture 8.8.8 8.8.8 x64 8.8.7)"

cat > "$fake_bin/curl" <<'CURL'
#!/usr/bin/env bash
set -euo pipefail
out='' url=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output|-o) out="$2"; shift 2 ;;
    --output=*) out="${1#*=}"; shift ;;
    --*) shift ;;
    *) url="$1"; shift ;;
  esac
done
[ -n "$out" ] && [ -n "$url" ]
printf '%s\n' "$url" >> "$CCUSAGE_TEST_CURL_LOG"
case "$url" in file://*) cp -- "${url#file://}" "$out" ;; *) exit 90 ;; esac
CURL
chmod +x "$fake_bin/curl"
original_path="$PATH"
export CCUSAGE_TEST_CURL_LOG="$curl_log"

fetch_count() {
  if [ -f "$curl_log" ]; then wc -l < "$curl_log"; else printf '0\n'; fi
}

run_hook() { # <version> <arch> <dest> <amd64-sha> <arm64-sha>
  env PATH="$fake_bin:$original_path" \
    TMPDIR="$tmp/work" \
    CCUSAGE_TEST_ALLOW_NON_ROOT=1 \
    CCUSAGE_VERSION="$1" \
    CCUSAGE_ARCH="$2" \
    CCUSAGE_INSTALL_PATH="$3" \
    CCUSAGE_REGISTRY_URL="file://$registry" \
    CCUSAGE_SHA256_AMD64="$4" \
    CCUSAGE_SHA256_ARM64="$5" \
    bash "$HOOK" "${6:-}"
}

dest="$tmp/install/ccusage"
run_hook 1.2.3 amd64 "$dest" "$sha_123_amd64" "$sha_123_arm64" >/dev/null
[ "$(fetch_count)" -eq 1 ] || fail "fresh install did not fetch exactly once"
if [ ! -f "$dest" ] || [ -L "$dest" ] || [ ! -x "$dest" ]; then
  fail "fresh install is not executable regular file"
fi
[ "$(stat -c '%a' "$dest")" = 755 ] || fail "fresh install mode is not 0755"
[ "$("$dest" --version)" = 'ccusage 1.2.3' ] || fail "fresh install reports the wrong version"
mapfile -t urls < "$curl_log"
case "${urls[0]}" in
  */@ccusage/ccusage-linux-x64/-/ccusage-linux-x64-1.2.3.tgz) ;;
  *) fail "amd64 did not map to the x64 package" ;;
esac
if [ "$(id -u)" -eq 0 ]; then
  [ "$(stat -c '%u:%g' "$dest")" = 0:0 ] || fail "root install is not root-owned"
fi

# Verify idempotence and metadata drift repair.
run_hook 1.2.3 amd64 "$dest" "$sha_123_amd64" "$sha_123_arm64" >/dev/null
[ "$(fetch_count)" -eq 1 ] || fail "exact rerun fetched the package"
chmod 0777 "$dest"
run_hook 1.2.3 amd64 "$dest" "$sha_123_amd64" "$sha_123_arm64" >/dev/null
[ "$(fetch_count)" -eq 2 ] || fail "mode drift was not replaced from the verified artifact"
[ "$(stat -c '%a' "$dest")" = 755 ] || fail "mode-only repair did not restore 0755"

# A pin bump, a non-executable file, a legacy symlink, and an executable wrapper all converge.
run_hook 1.2.4 amd64 "$dest" "$sha_124_amd64" "$sha_124_arm64" >/dev/null
[ "$(fetch_count)" -eq 3 ] || fail "version drift did not fetch the new pin"
[ "$("$dest" --version)" = 'ccusage 1.2.4' ] || fail "version drift was not repaired"

printf 'broken\n' > "$dest"
chmod 0644 "$dest"
run_hook 1.2.4 amd64 "$dest" "$sha_124_amd64" "$sha_124_arm64" >/dev/null
[ "$(fetch_count)" -eq 4 ] || fail "non-executable file was not replaced"

cat > "$tmp/npm-target" <<'WRAPPER'
#!/usr/bin/env bash
printf 'ccusage 1.2.4\n'
WRAPPER
chmod +x "$tmp/npm-target"
rm -f "$dest"
ln -s "$tmp/npm-target" "$dest"
run_hook 1.2.4 amd64 "$dest" "$sha_124_amd64" "$sha_124_arm64" >/dev/null
[ "$(fetch_count)" -eq 5 ] || fail "legacy npm symlink was not replaced"
if [ ! -f "$dest" ] || [ -L "$dest" ]; then fail "legacy npm symlink survived migration"; fi

cat > "$dest" <<'WRAPPER'
#!/usr/bin/env bash
printf 'ccusage 1.2.4\n'
WRAPPER
chmod +x "$dest"
run_hook 1.2.4 amd64 "$dest" "$sha_124_amd64" "$sha_124_arm64" >/dev/null
[ "$(fetch_count)" -eq 6 ] || fail "executable npm wrapper was mistaken for the native binary"
[ "$(LC_ALL=C od -An -tx1 -N4 "$dest" | tr -d '[:space:]')" = 7f454c46 ] \
  || fail "wrapper migration did not install an ELF binary"

# Artifact failures leave the old working binary untouched.
before="$(sha256sum "$dest" | cut -d' ' -f1)"
if run_hook 1.2.3 amd64 "$dest" "$(printf '0%.0s' {1..64})" "$sha_123_arm64" >/dev/null 2>"$tmp/checksum.err"; then
  fail "checksum mismatch unexpectedly succeeded"
fi
[ "$(fetch_count)" -eq 7 ] || fail "checksum test did not fetch its fixture"
[ "$(sha256sum "$dest" | cut -d' ' -f1)" = "$before" ] || fail "checksum failure replaced the old binary"
[ "$("$dest" --version)" = 'ccusage 1.2.4' ] || fail "checksum failure broke the old binary"

if run_hook 9.9.9 amd64 "$dest" "$sha_bad_metadata" "$sha_123_arm64" >/dev/null 2>"$tmp/metadata.err"; then
  fail "package-version mismatch unexpectedly succeeded"
fi
[ "$(fetch_count)" -eq 8 ] || fail "metadata test did not fetch its fixture"
[ "$(sha256sum "$dest" | cut -d' ' -f1)" = "$before" ] || fail "metadata failure replaced the old binary"

if run_hook 8.8.8 amd64 "$dest" "$sha_bad_binary" "$sha_123_arm64" >/dev/null 2>"$tmp/binary.err"; then
  fail "staged-version mismatch unexpectedly succeeded"
fi
[ "$(fetch_count)" -eq 9 ] || fail "staged-version test did not fetch its fixture"
[ "$(sha256sum "$dest" | cut -d' ' -f1)" = "$before" ] || fail "staged failure replaced the old binary"
if compgen -G "$tmp/install/.ccusage.*" >/dev/null; then fail "staged failure left a temporary file"; fi

if run_hook 7.7.7 amd64 "$dest" "$(printf '1%.0s' {1..64})" "$sha_123_arm64" >/dev/null 2>"$tmp/download.err"; then
  fail "missing artifact unexpectedly succeeded"
fi
[ "$(fetch_count)" -eq 10 ] || fail "download failure did not reach the fetcher"
[ "$(sha256sum "$dest" | cut -d' ' -f1)" = "$before" ] || fail "download failure replaced the old binary"
if compgen -G "$tmp/work/*" >/dev/null; then fail "provision left a temporary work directory"; fi

# Verify architecture mapping and tuple validation.
arm_dest="$tmp/install/ccusage-arm64"
run_hook 1.2.3 arm64 "$arm_dest" "$sha_123_amd64" "$sha_123_arm64" >/dev/null
[ "$(fetch_count)" -eq 11 ] || fail "arm64 install did not fetch exactly once"
mapfile -t urls < "$curl_log"
case "${urls[10]}" in
  */@ccusage/ccusage-linux-arm64/-/ccusage-linux-arm64-1.2.3.tgz) ;;
  *) fail "arm64 did not map to the arm64 package" ;;
esac

count="$(fetch_count)"
if run_hook 1.2.3 s390x "$tmp/install/unsupported" "$sha_123_amd64" "$sha_123_arm64" >/dev/null 2>&1; then
  fail "unsupported architecture unexpectedly succeeded"
fi
[ "$(fetch_count)" -eq "$count" ] || fail "unsupported architecture fetched before failing"
if run_hook latest amd64 "$tmp/install/latest" "$sha_123_amd64" "$sha_123_arm64" >/dev/null 2>&1; then
  fail "latest version unexpectedly succeeded"
fi
if run_hook 1.2.3 amd64 "$tmp/install/incomplete" "$sha_123_amd64" '' >/dev/null 2>&1; then
  fail "incomplete checksum tuple unexpectedly succeeded"
fi
[ "$(fetch_count)" -eq "$count" ] || fail "invalid metadata fetched before failing"

# Production invocation is root-only. Exercise the refusal as the current user or a dropped uid.
nonroot_err="$tmp/nonroot.err"
if [ "$(id -u)" -eq 0 ]; then
  need setpriv
  if env -u CCUSAGE_TEST_ALLOW_NON_ROOT \
    CCUSAGE_VERSION=1.2.3 CCUSAGE_SHA256_AMD64="$sha_123_amd64" \
    CCUSAGE_SHA256_ARM64="$sha_123_arm64" \
    setpriv --reuid=65534 --regid=65534 --clear-groups bash -s < "$HOOK" \
      >/dev/null 2>"$nonroot_err"; then
    fail "non-root provision unexpectedly succeeded"
  fi
else
  if env -u CCUSAGE_TEST_ALLOW_NON_ROOT \
    CCUSAGE_VERSION=1.2.3 CCUSAGE_SHA256_AMD64="$sha_123_amd64" \
    CCUSAGE_SHA256_ARM64="$sha_123_arm64" \
    bash "$HOOK" >/dev/null 2>"$nonroot_err"; then
    fail "non-root provision unexpectedly succeeded"
  fi
fi
grep -Fq 'must run as root' "$nonroot_err" || fail "non-root refusal is unclear"

# A version string cannot authorize a new pin or bytes replaced after publication.
receipt_dest="$tmp/install/receipt-case"
run_hook 1.2.3 amd64 "$receipt_dest" "$sha_123_amd64" "$sha_123_arm64" >/dev/null
count="$(fetch_count)"
run_hook 1.2.3 amd64 "$receipt_dest" "$sha_123_amd64" "$sha_123_arm64" --check
[ "$(fetch_count)" = "$count" ] || fail 'read-only receipt check fetched an archive'
# Published predecessors have a valid binary without the newer trusted receipt.
legacy_binary="$(sha256sum "$receipt_dest" | cut -d' ' -f1)"
rm "$receipt_dest.subyard-runtime.json"
if run_hook 1.2.3 amd64 "$receipt_dest" "$sha_123_amd64" "$sha_123_arm64" --check; then fail 'legacy binary without receipt reported current'; fi
[ "$(fetch_count)" = "$count" ] && [ ! -e "$receipt_dest.subyard-runtime.json" ] \
  && [ "$(sha256sum "$receipt_dest" | cut -d' ' -f1)" = "$legacy_binary" ] \
  || fail 'legacy readiness check downloaded or changed installed state'
run_hook 1.2.3 amd64 "$receipt_dest" "$sha_123_amd64" "$sha_123_arm64" >/dev/null
[ "$(fetch_count)" -eq "$((count + 1))" ] || fail 'legacy receipt repair trusted the version instead of a verified archive'
run_hook 1.2.3 amd64 "$receipt_dest" "$sha_123_amd64" "$sha_123_arm64" --check
count="$(fetch_count)"
printf 'replaced bytes' >> "$receipt_dest"
if run_hook 1.2.3 amd64 "$receipt_dest" "$sha_123_amd64" "$sha_123_arm64" --check; then fail 'version-only check accepted replaced binary'; fi
run_hook 1.2.3 amd64 "$receipt_dest" "$sha_123_amd64" "$sha_123_arm64" >/dev/null
[ "$(fetch_count)" -eq "$((count + 1))" ] || fail 'replaced ccusage did not reinstall from verified archive'
receipt_before="$(sha256sum "$receipt_dest.subyard-runtime.json" | cut -d' ' -f1)"
wrong_pin="$(printf '0%.0s' {1..64})"
if run_hook 1.2.3 amd64 "$receipt_dest" "$wrong_pin" "$sha_123_arm64" --check; then fail 'same-version changed archive pin accepted'; fi
if run_hook 1.2.3 amd64 "$receipt_dest" "$wrong_pin" "$sha_123_arm64" > "$tmp/receipt-pin.err" 2>&1; then fail 'unchecked same-version binary installed'; fi
[ "$(sha256sum "$receipt_dest.subyard-runtime.json" | cut -d' ' -f1)" = "$receipt_before" ] || fail 'failed archive verification changed trusted receipt'
run_hook 1.2.3 amd64 "$receipt_dest" "$sha_123_amd64" "$sha_123_arm64" --check

printf 'ok: native ccusage provision\n'
