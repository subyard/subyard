#!/usr/bin/env bash
# Hermetic checks for the pinned native Codex provision hook.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOOK="$ROOT/config/agents/codex/provision.sh"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "missing test dependency: $1"; }
for command in cc sha256sum tar; do need "$command"; done
[ -x "$HOOK" ] || fail "Codex provision hook is not executable"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
releases="$tmp/releases"
fake_bin="$tmp/fake-bin"
curl_log="$tmp/curl.log"
mkdir -p "$releases" "$fake_bin" "$tmp/work" "$tmp/home/.codex"
printf '{"tokens":"yard-local-sentinel"}\n' > "$tmp/home/.codex/auth.json"
auth_before="$(sha256sum "$tmp/home/.codex/auth.json" | cut -d' ' -f1)"

make_fixture() { # <release-version> <amd64|arm64> [binary-version]
  local release_version="$1" arch="$2" binary_version="${3:-$1}"
  local target build out
  case "$arch" in
    amd64) target=x86_64-unknown-linux-musl ;;
    arm64) target=aarch64-unknown-linux-musl ;;
    *) fail "unsupported fixture architecture: $arch" ;;
  esac
  build="$tmp/build-${release_version}-${arch}-${binary_version}"
  out="$releases/rust-v${release_version}/codex-${target}.tar.gz"
  mkdir -p "$build" "$(dirname "$out")"
  cat > "$build/main.c" <<EOF
#include <stdio.h>
#include <string.h>
int main(int argc, char **argv) {
  if (argc == 2 && strcmp(argv[1], "--version") == 0) {
    puts("codex-cli ${binary_version}");
    return 0;
  }
  if (argc == 3 && strcmp(argv[1], "app-server") == 0 && strcmp(argv[2], "--help") == 0) {
    puts("Codex app server help");
    return 0;
  }
  return 64;
}
EOF
  cc -O2 -o "$build/codex-$target" "$build/main.c"
  chmod 0644 "$build/codex-$target"
  tar -czf "$out" -C "$build" "codex-$target"
  sha256sum "$out" | cut -d' ' -f1
}

sha_147_amd64="$(make_fixture 0.147.0 amd64)"
sha_147_arm64="$(make_fixture 0.147.0 arm64)"
sha_148_amd64="$(make_fixture 0.148.0 amd64)"
sha_148_arm64="$(make_fixture 0.148.0 arm64)"
sha_bad_version="$(make_fixture 9.9.9 amd64 9.9.8)"
mkdir -p "$releases/rust-v8.8.8"
printf 'not a tar archive\n' \
  > "$releases/rust-v8.8.8/codex-x86_64-unknown-linux-musl.tar.gz"
sha_bad_archive="$(sha256sum "$releases/rust-v8.8.8/codex-x86_64-unknown-linux-musl.tar.gz" | cut -d' ' -f1)"

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
printf '%s\n' "$url" >> "$CODEX_TEST_CURL_LOG"
case "$url" in file://*) cp -- "${url#file://}" "$out" ;; *) exit 90 ;; esac
CURL
chmod +x "$fake_bin/curl"
original_path="$PATH"
export CODEX_TEST_CURL_LOG="$curl_log"

fetch_count() {
  if [ -f "$curl_log" ]; then wc -l < "$curl_log"; else printf '0\n'; fi
}

run_hook() { # <version> <arch> <root> <amd64-sha> <arm64-sha>
  local install_root="$3"
  env PATH="$fake_bin:$original_path" HOME="$tmp/home" TMPDIR="$tmp/work" \
    CODEX_TEST_ALLOW_NON_ROOT=1 \
    DEV_USER="$(id -un)" \
    CODEX_VERSION="$1" \
    CODEX_ARCH="$2" \
    CODEX_INSTALL_ROOT="$install_root/opt/subyard/codex" \
    CODEX_BIN_LINK="$install_root/usr/local/bin/codex" \
    CODEX_CHECK_PATH="$install_root/usr/local/bin/codex-check" \
    CODEX_RELEASE_BASE_URL="file://$releases" \
    CODEX_SHA256_AMD64="$4" \
    CODEX_SHA256_ARM64="$5" \
    bash "$HOOK"
}

root="$tmp/install"
runtime="$root/opt/subyard/codex/codex"
link="$root/usr/local/bin/codex"
check="$root/usr/local/bin/codex-check"
run_hook 0.147.0 amd64 "$root" "$sha_147_amd64" "$sha_147_arm64" >/dev/null
[ "$(fetch_count)" -eq 1 ] || fail "fresh install did not fetch exactly once"
[ -f "$runtime" ] && [ ! -L "$runtime" ] && [ -x "$runtime" ] \
  || fail "managed runtime is not an executable regular file"
[ "$(stat -c '%a' "$runtime")" = 755 ] || fail "runtime mode is not 0755"
[ -L "$link" ] || fail "canonical Codex command is not a symlink"
[ "$(readlink "$link")" = "$runtime" ] || fail "canonical Codex command points outside the managed runtime"
[ -x "$check" ] || fail "Codex package check was not installed"
[ "$("$link" --version)" = 'codex-cli 0.147.0' ] || fail "fresh install reports the wrong version"
"$link" app-server --help >/dev/null || fail "fresh install has no app-server command"
CODEX_CHECK_DEV_USER="$(id -un)" "$check" >/dev/null || fail "package check rejected a valid install"
case "$(head -n 1 "$curl_log")" in
  */rust-v0.147.0/codex-x86_64-unknown-linux-musl.tar.gz) ;;
  *) fail "amd64 did not map to the x86_64 musl artifact" ;;
esac
if [ "$(id -u)" -eq 0 ]; then
  [ "$(stat -c '%u:%g' "$runtime")" = 0:0 ] || fail "runtime is not root-owned"
fi
[ "$(sha256sum "$tmp/home/.codex/auth.json" | cut -d' ' -f1)" = "$auth_before" ] \
  || fail "provision changed yard-local Codex authorization"

# Exact reruns do not fetch; a pin bump and rollback both converge atomically.
run_hook 0.147.0 amd64 "$root" "$sha_147_amd64" "$sha_147_arm64" >/dev/null
[ "$(fetch_count)" -eq 1 ] || fail "same-version rerun fetched the release"
run_hook 0.148.0 amd64 "$root" "$sha_148_amd64" "$sha_148_arm64" >/dev/null
[ "$(fetch_count)" -eq 2 ] || fail "upgrade did not fetch exactly once"
[ "$("$link" --version)" = 'codex-cli 0.148.0' ] || fail "upgrade did not publish the new version"
run_hook 0.147.0 amd64 "$root" "$sha_147_amd64" "$sha_147_arm64" >/dev/null
[ "$(fetch_count)" -eq 3 ] || fail "rollback did not fetch exactly once"
[ "$("$link" --version)" = 'codex-cli 0.147.0' ] || fail "rollback did not restore the pinned version"

# Artifact failures preserve the previous working runtime and authorization.
before="$(sha256sum "$runtime" | cut -d' ' -f1)"
zeros="$(printf '0%.0s' {1..64})"
if run_hook 0.148.0 amd64 "$root" "$zeros" "$sha_148_arm64" >/dev/null 2>"$tmp/checksum.err"; then
  fail "checksum mismatch unexpectedly succeeded"
fi
[ "$(sha256sum "$runtime" | cut -d' ' -f1)" = "$before" ] || fail "checksum failure replaced the runtime"
if run_hook 9.9.9 amd64 "$root" "$sha_bad_version" "$sha_147_arm64" >/dev/null 2>"$tmp/version.err"; then
  fail "staged version mismatch unexpectedly succeeded"
fi
[ "$(sha256sum "$runtime" | cut -d' ' -f1)" = "$before" ] || fail "version failure replaced the runtime"
if run_hook 8.8.8 amd64 "$root" "$sha_bad_archive" "$sha_147_arm64" >/dev/null 2>"$tmp/archive.err"; then
  fail "corrupt archive unexpectedly succeeded"
fi
[ "$(sha256sum "$runtime" | cut -d' ' -f1)" = "$before" ] || fail "archive failure replaced the runtime"
[ "$(sha256sum "$tmp/home/.codex/auth.json" | cut -d' ' -f1)" = "$auth_before" ] \
  || fail "failed upgrades changed yard-local Codex authorization"
if compgen -G "$root/opt/subyard/codex/.codex.*" >/dev/null; then
  fail "failed install left a staged runtime"
fi

# Existing commands outside the Subyard-owned runtime fail closed.
conflict="$tmp/conflict"
mkdir -p "$conflict/usr/local/bin"
printf '#!/bin/sh\nexit 0\n' > "$conflict/usr/local/bin/codex"
chmod +x "$conflict/usr/local/bin/codex"
count="$(fetch_count)"
if run_hook 0.147.0 amd64 "$conflict" "$sha_147_amd64" "$sha_147_arm64" >/dev/null 2>"$tmp/conflict.err"; then
  fail "conflicting canonical command unexpectedly succeeded"
fi
[ "$(fetch_count)" -eq "$count" ] || fail "command conflict fetched before failing"
[ ! -e "$conflict/opt/subyard/codex/codex" ] || fail "command conflict mutated the managed runtime"

# Verify arm64 mapping and fail-fast tuple validation.
arm_root="$tmp/arm-install"
run_hook 0.147.0 arm64 "$arm_root" "$sha_147_amd64" "$sha_147_arm64" >/dev/null
tail -n 1 "$curl_log" | grep -Fq '/rust-v0.147.0/codex-aarch64-unknown-linux-musl.tar.gz' \
  || fail "arm64 did not map to the aarch64 musl artifact"
count="$(fetch_count)"
if run_hook 0.147.0 s390x "$tmp/unsupported" "$sha_147_amd64" "$sha_147_arm64" >/dev/null 2>&1; then
  fail "unsupported architecture unexpectedly succeeded"
fi
if run_hook latest amd64 "$tmp/latest" "$sha_147_amd64" "$sha_147_arm64" >/dev/null 2>&1; then
  fail "mutable version unexpectedly succeeded"
fi
if run_hook 0.147.0 amd64 "$tmp/incomplete" "$sha_147_amd64" '' >/dev/null 2>&1; then
  fail "incomplete checksum tuple unexpectedly succeeded"
fi
[ "$(fetch_count)" -eq "$count" ] || fail "invalid metadata fetched before failing"

# Production invocation is root-only.
nonroot_err="$tmp/nonroot.err"
if [ "$(id -u)" -eq 0 ]; then
  need setpriv
  if env -u CODEX_TEST_ALLOW_NON_ROOT \
    CODEX_VERSION=0.147.0 CODEX_SHA256_AMD64="$sha_147_amd64" \
    CODEX_SHA256_ARM64="$sha_147_arm64" \
    setpriv --reuid=65534 --regid=65534 --clear-groups bash -s < "$HOOK" \
      >/dev/null 2>"$nonroot_err"; then
    fail "non-root provision unexpectedly succeeded"
  fi
else
  if env -u CODEX_TEST_ALLOW_NON_ROOT \
    CODEX_VERSION=0.147.0 CODEX_SHA256_AMD64="$sha_147_amd64" \
    CODEX_SHA256_ARM64="$sha_147_arm64" \
    bash "$HOOK" >/dev/null 2>"$nonroot_err"; then
    fail "non-root provision unexpectedly succeeded"
  fi
fi
grep -Fq 'must run as root' "$nonroot_err" || fail "non-root refusal is unclear"

printf 'ok: native Codex agent provision\n'
