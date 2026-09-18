#!/usr/bin/env bash
# Hermetic checks for current native Codex releases and verified atomic upgrades.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOOK="$ROOT/config/agents/codex/provision.sh"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "missing test dependency: $1"; }
for command in cc jq sha256sum tar; do need "$command"; done
[ -x "$HOOK" ] || fail "Codex provision hook is not executable"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
releases="$tmp/releases"
fake_bin="$tmp/fake-bin"
curl_log="$tmp/curl.log"
mkdir -p "$releases" "$fake_bin" "$tmp/work" "$tmp/home/.codex"
printf '{"tokens":"yard-local-sentinel"}\n' > "$tmp/home/.codex/auth.json"
auth_before="$(sha256sum "$tmp/home/.codex/auth.json" | cut -d' ' -f1)"

make_fixture() { # <release-version> <amd64|arm64> [binary-version] [readiness-exit]
  local release_version="$1" arch="$2" binary_version="${3:-$1}" readiness="${4:-0}"
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
    return ${readiness};
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
sha_149_amd64="$(make_fixture 0.149.0 amd64)"
sha_bad_version="$(make_fixture 9.9.9 amd64 9.9.8)"
sha_bad_readiness="$(make_fixture 9.9.7 amd64 9.9.7 1)"
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
    --header|-H|--proto|--proto-redir|--connect-timeout|--max-time|--retry) shift 2 ;;
    --*) shift ;;
    *) url="$1"; shift ;;
  esac
done
[ -n "$out" ] && [ -n "$url" ]
printf '%s\n' "$url" >> "$CODEX_TEST_CURL_LOG"
case "$url" in
  https://api.github.com/repos/openai/codex/releases/latest)
    cp -- "$CODEX_TEST_RELEASES/latest.json" "$out" ;;
  https://github.com/openai/codex/releases/download/*)
    cp -- "$CODEX_TEST_RELEASES/${url#https://github.com/openai/codex/releases/download/}" "$out" ;;
  *) exit 90 ;;
esac
CURL
chmod +x "$fake_bin/curl"
original_path="$PATH"
export CODEX_TEST_CURL_LOG="$curl_log" CODEX_TEST_RELEASES="$releases"

fetch_count() {
  if [ -f "$curl_log" ]; then wc -l < "$curl_log"; else printf '0\n'; fi
}

set_latest() { # <version> <amd64-sha> <arm64-sha>
  jq -n --arg version "$1" --arg amd64 "$2" --arg arm64 "$3" '
    def asset($target; $sha):
      ("codex-" + $target + "-unknown-linux-musl.tar.gz") as $name |
      {name: $name, state: "uploaded", digest: ("sha256:" + $sha),
       browser_download_url: ("https://github.com/openai/codex/releases/download/rust-v" + $version + "/" + $name)};
    {tag_name: ("rust-v" + $version), draft: false, prerelease: false,
     assets: [asset("x86_64"; $amd64), asset("aarch64"; $arm64)]}
  ' > "$releases/latest.json"
}

run_hook() { # <arch> <root> [environment assignments...]
  local arch="$1" install_root="$2"
  shift 2
  env -u CODEX_VERSION -u CODEX_SHA256_AMD64 -u CODEX_SHA256_ARM64 \
    PATH="$fake_bin:$original_path" HOME="$tmp/home" TMPDIR="$tmp/work" \
    CODEX_TEST_ALLOW_NON_ROOT=1 \
    DEV_USER="$(id -un)" \
    CODEX_ARCH="$arch" \
    CODEX_INSTALL_ROOT="$install_root/opt/subyard/codex" \
    CODEX_BIN_LINK="$install_root/usr/local/bin/codex" \
    CODEX_CHECK_PATH="$install_root/usr/local/bin/codex-policy-check" \
    CODEX_REQUIREMENTS_PATH="$install_root/etc/codex/requirements.toml" \
    "$@" bash "$HOOK"
}

root="$tmp/install"
runtime="$root/opt/subyard/codex/codex"
link="$root/usr/local/bin/codex"
check="$root/usr/local/bin/codex-policy-check"
requirements="$root/etc/codex/requirements.toml"
set_latest 0.147.0 "$sha_147_amd64" "$sha_147_arm64"
run_hook amd64 "$root" >/dev/null
[ "$(fetch_count)" -eq 2 ] || fail "fresh install did not fetch release metadata and artifact"
[ -f "$runtime" ] && [ ! -L "$runtime" ] && [ -x "$runtime" ] \
  || fail "managed runtime is not an executable regular file"
[ "$(stat -c '%a' "$runtime")" = 755 ] || fail "runtime mode is not 0755"
[ -L "$link" ] || fail "canonical Codex command is not a symlink"
[ "$(readlink "$link")" = "$runtime" ] || fail "canonical Codex command points outside the managed runtime"
[ -x "$check" ] || fail "Codex package check was not installed"
[ "$("$link" --version)" = 'codex-cli 0.147.0' ] || fail "fresh install reports the wrong version"
"$link" app-server --help >/dev/null || fail "fresh install has no app-server command"
CODEX_CHECK_DEV_USER="$(id -un)" "$check" >/dev/null || fail "package check rejected a valid install"
python3 - "$requirements" <<'PY'
import pathlib, sys, tomllib
policy = tomllib.loads(pathlib.Path(sys.argv[1]).read_text())
assert policy['allowed_approval_policies'] == ['on-request']
assert policy['allowed_approvals_reviewers'] == ['user']
rule, = policy['rules']['prefix_rules']
assert rule['pattern'] == [{'token': 'git'}, {'any_of': ['commit', 'push']}]
assert rule['decision'] == 'prompt'
PY
case "$(tail -n 1 "$curl_log")" in
  */rust-v0.147.0/codex-x86_64-unknown-linux-musl.tar.gz) ;;
  *) fail "amd64 did not map to the x86_64 musl artifact" ;;
esac
if [ "$(id -u)" -eq 0 ]; then
  [ "$(stat -c '%u:%g' "$runtime")" = 0:0 ] || fail "runtime is not root-owned"
fi
[ "$(sha256sum "$tmp/home/.codex/auth.json" | cut -d' ' -f1)" = "$auth_before" ] \
  || fail "provision changed yard-local Codex authorization"

# Each rerun resolves current metadata; only a newer release fetches an artifact.
cp "$check" "$tmp/initial-check"
run_hook amd64 "$root" >/dev/null
[ "$(fetch_count)" -eq 3 ] || fail "same-version rerun did not only resolve metadata"
set_latest 0.148.0 "$sha_148_amd64" "$sha_148_arm64"
run_hook amd64 "$root" >/dev/null
[ "$(fetch_count)" -eq 5 ] || fail "upgrade did not fetch metadata and artifact"
[ "$("$link" --version)" = 'codex-cli 0.148.0' ] || fail "upgrade did not publish the current version"
CODEX_CHECK_DEV_USER="$(id -un)" "$tmp/initial-check" >/dev/null \
  || fail "readiness check rejected an updated working runtime"
set_latest 0.147.0 "$sha_147_amd64" "$sha_147_arm64"
run_hook amd64 "$root" >/dev/null
[ "$(fetch_count)" -eq 6 ] || fail "older release metadata downloaded an artifact"
[ "$("$link" --version)" = 'codex-cli 0.148.0' ] || fail "older release metadata downgraded a working runtime"

# Artifact failures preserve the previous working runtime and authorization.
before="$(sha256sum "$runtime" | cut -d' ' -f1)"
zeros="$(printf '0%.0s' {1..64})"
set_latest 0.149.0 "$zeros" "$sha_148_arm64"
if run_hook amd64 "$root" >/dev/null 2>"$tmp/checksum.err"; then
  fail "checksum mismatch unexpectedly succeeded"
fi
[ "$(sha256sum "$runtime" | cut -d' ' -f1)" = "$before" ] || fail "checksum failure replaced the runtime"
set_latest 9.9.9 "$sha_bad_version" "$sha_147_arm64"
if run_hook amd64 "$root" >/dev/null 2>"$tmp/version.err"; then
  fail "staged version mismatch unexpectedly succeeded"
fi
[ "$(sha256sum "$runtime" | cut -d' ' -f1)" = "$before" ] || fail "version failure replaced the runtime"
set_latest 8.8.8 "$sha_bad_archive" "$sha_147_arm64"
if run_hook amd64 "$root" >/dev/null 2>"$tmp/archive.err"; then
  fail "corrupt archive unexpectedly succeeded"
fi
[ "$(sha256sum "$runtime" | cut -d' ' -f1)" = "$before" ] || fail "archive failure replaced the runtime"
set_latest 9.9.7 "$sha_bad_readiness" "$sha_147_arm64"
if run_hook amd64 "$root" >/dev/null 2>"$tmp/readiness.err"; then
  fail "staged app-server failure unexpectedly succeeded"
fi
[ "$(sha256sum "$runtime" | cut -d' ' -f1)" = "$before" ] || fail "readiness failure replaced the runtime"

# Missing, ambiguous or untrusted release metadata fails before any artifact download.
set_latest 0.149.0 "$sha_149_amd64" "$sha_148_arm64"
cp "$releases/latest.json" "$tmp/valid-metadata.json"
for mutation in \
  '.draft = true' \
  '.prerelease = true' \
  '.tag_name = "rust-v0.149.0-alpha.1"' \
  'del(.draft)' \
  '.assets = []' \
  '.assets += [.assets[0]]' \
  '.assets[0].digest = null' \
  '.assets[0].digest = "sha256:invalid"' \
  '.assets[0].digest = "sha512:0123456789"' \
  '.assets[0].browser_download_url = "https://example.invalid/codex.tar.gz"' \
  '.assets[0].state = "new"'; do
  jq "$mutation" "$tmp/valid-metadata.json" > "$releases/latest.json"
  count="$(fetch_count)"
  if run_hook amd64 "$root" >/dev/null 2>"$tmp/metadata.err"; then
    fail "invalid release metadata unexpectedly succeeded: $mutation"
  fi
  [ "$(fetch_count)" -eq "$((count + 1))" ] || fail "invalid metadata fetched an artifact: $mutation"
  [ "$(sha256sum "$runtime" | cut -d' ' -f1)" = "$before" ] || fail "invalid metadata replaced the runtime"
done
printf '{invalid json\n' > "$releases/latest.json"
if run_hook amd64 "$root" >/dev/null 2>"$tmp/json.err"; then
  fail "malformed release metadata unexpectedly succeeded"
fi
mv "$releases/latest.json" "$releases/unavailable.json"
if run_hook amd64 "$root" >/dev/null 2>"$tmp/unavailable.err"; then
  fail "unavailable release metadata unexpectedly succeeded"
fi
[ "$(sha256sum "$runtime" | cut -d' ' -f1)" = "$before" ] || fail "metadata failure replaced the runtime"
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
if run_hook amd64 "$conflict" >/dev/null 2>"$tmp/conflict.err"; then
  fail "conflicting canonical command unexpectedly succeeded"
fi
[ "$(fetch_count)" -eq "$count" ] || fail "command conflict fetched before failing"
[ ! -e "$conflict/opt/subyard/codex/codex" ] || fail "command conflict mutated the managed runtime"

# Existing operator-managed requirements must never be overwritten.
policy_conflict="$tmp/policy-conflict"
mkdir -p "$policy_conflict/etc/codex"
printf 'allowed_approval_policies = ["never"]\n' > "$policy_conflict/etc/codex/requirements.toml"
cp "$policy_conflict/etc/codex/requirements.toml" "$tmp/operator-policy"
if run_hook amd64 "$policy_conflict" >/dev/null 2>&1; then
  fail "unmanaged requirements were overwritten"
fi
cmp "$policy_conflict/etc/codex/requirements.toml" "$tmp/operator-policy" \
  || fail "operator policy changed on refusal"
[ "$(fetch_count)" -eq "$count" ] || fail "policy conflict fetched before failing"

# Verify arm64 mapping and fail-fast architecture validation.
arm_root="$tmp/arm-install"
set_latest 0.147.0 "$sha_147_amd64" "$sha_147_arm64"
run_hook arm64 "$arm_root" >/dev/null
tail -n 1 "$curl_log" | grep -Fq '/rust-v0.147.0/codex-aarch64-unknown-linux-musl.tar.gz' \
  || fail "arm64 did not map to the aarch64 musl artifact"
count="$(fetch_count)"
if run_hook s390x "$tmp/unsupported" >/dev/null 2>&1; then
  fail "unsupported architecture unexpectedly succeeded"
fi
[ "$(fetch_count)" -eq "$count" ] || fail "invalid architecture fetched before failing"

# Obsolete process-environment pins cannot hold a fresh installation back.
set_latest 0.148.0 "$sha_148_amd64" "$sha_148_arm64"
for stale_version in 0.147.0 latest; do
  legacy_root="$tmp/legacy-$stale_version"
  run_hook amd64 "$legacy_root" CODEX_VERSION="$stale_version" \
    CODEX_SHA256_AMD64=not-a-checksum CODEX_SHA256_ARM64= >/dev/null
  [ "$("$legacy_root/usr/local/bin/codex" --version)" = 'codex-cli 0.148.0' ] \
    || fail "obsolete environment pins overrode the current release"
done

# Missing, modified and writable requirements fail readiness and reconcile on rerun.
cp "$requirements" "$tmp/expected-requirements"
for drift in missing policy permissions directory; do
  case "$drift" in
    missing) rm "$requirements" ;;
    policy) printf '\nallowed_extra = true\n' >> "$requirements" ;;
    permissions) chmod 0666 "$requirements" ;;
    directory) chmod 0777 "$(dirname "$requirements")" ;;
  esac
  if CODEX_CHECK_DEV_USER="$(id -un)" "$check" >/dev/null 2>&1; then
    fail "$drift requirements drift passed readiness"
  fi
  run_hook amd64 "$root" >/dev/null
  cmp "$requirements" "$tmp/expected-requirements" || fail "requirements were not repaired"
done

# Production invocation is root-only.
nonroot_err="$tmp/nonroot.err"
if [ "$(id -u)" -eq 0 ]; then
  need setpriv
  if env -u CODEX_TEST_ALLOW_NON_ROOT \
    setpriv --reuid=65534 --regid=65534 --clear-groups bash -s < "$HOOK" \
      >/dev/null 2>"$nonroot_err"; then
    fail "non-root provision unexpectedly succeeded"
  fi
else
  if env -u CODEX_TEST_ALLOW_NON_ROOT \
    bash "$HOOK" >/dev/null 2>"$nonroot_err"; then
    fail "non-root provision unexpectedly succeeded"
  fi
fi
grep -Fq 'must run as root' "$nonroot_err" || fail "non-root refusal is unclear"

printf 'ok: native Codex agent provision\n'
