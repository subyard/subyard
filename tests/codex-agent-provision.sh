#!/usr/bin/env bash
# Hermetic checks for current official Codex packages and verified atomic upgrades.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOOK="$ROOT/config/agents/codex/provision.sh"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "missing test dependency: $1"; }
for command in cc jq sha256sum tar; do need "$command"; done
[ -x "$HOOK" ] || fail "Codex provision hook is not executable"

tmp="$(mktemp -d /tmp/subyard-codex-provision.XXXXXX)"
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
  out="$releases/rust-v${release_version}/codex-package-${target}.tar.gz"
  mkdir -p "$build/bin" "$build/codex-path" "$(dirname "$out")"
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
  cc -O2 -o "$build/bin/codex" "$build/main.c"
  rm "$build/main.c"
  # Upstream ships read-only libraries; releases must still be removable.
  printf 'helper\n' > "$build/codex-path/rg"
  chmod 0555 "$build/codex-path/rg"
  tar -czf "$out" -C "$build" bin codex-path
  sha256sum "$out" | cut -d' ' -f1
}

sha_147_amd64="$(make_fixture 0.147.0 amd64)"
sha_147_arm64="$(make_fixture 0.147.0 arm64)"
sha_148_amd64="$(make_fixture 0.148.0 amd64)"
sha_148_arm64="$(make_fixture 0.148.0 arm64)"
sha_149_amd64="$(make_fixture 0.149.0 amd64)"
sha_bad_version="$(make_fixture 9.9.9 amd64 9.9.8)"
sha_bad_readiness="$(make_fixture 9.9.7 amd64 9.9.7 1)"
make_bad_archive() { # <release-version> <file-writer command...>
  local out="$releases/rust-v$1/codex-package-x86_64-unknown-linux-musl.tar.gz"
  shift
  mkdir -p "$(dirname "$out")"
  "$@" "$out"
  sha256sum "$out" | cut -d' ' -f1
}
not_tar() { printf 'not a tar archive\n' > "$1"; }
traversal_tar() { python3 - "$1" <<'PY'
import io, sys, tarfile
with tarfile.open(sys.argv[1], 'w:gz') as archive:
    info = tarfile.TarInfo('../outside')
    info.size = 1
    archive.addfile(info, io.BytesIO(b'x'))
PY
}
symlink_tar() { python3 - "$1" <<'PY'
import sys, tarfile
with tarfile.open(sys.argv[1], 'w:gz') as archive:
    info = tarfile.TarInfo('bin')
    info.type = tarfile.SYMTYPE
    info.linkname = '/etc'
    archive.addfile(info)
PY
}
no_binary_tar() { tar -czf "$1" -C "$tmp/work" --files-from /dev/null; }
sha_bad_archive="$(make_bad_archive 8.8.8 not_tar)"
sha_traversal="$(make_bad_archive 8.8.7 traversal_tar)"
sha_symlink="$(make_bad_archive 8.8.6 symlink_tar)"
sha_no_binary="$(make_bad_archive 8.8.5 no_binary_tar)"

cat > "$fake_bin/curl" <<'CURL'
#!/usr/bin/env bash
set -euo pipefail
out='' url='' retries='' all_errors='' retry_time=''
args=("$@")
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output|-o) out="$2"; shift 2 ;;
    --output=*) out="${1#*=}"; shift ;;
    --retry) retries="$2"; shift 2 ;;
    --retry-all-errors) all_errors=1; shift ;;
    --retry-max-time) retry_time="$2"; shift 2 ;;
    --header|-H|--proto|--proto-redir|--connect-timeout|--max-time|--speed-limit|--speed-time|--continue-at) shift 2 ;;
    --*) shift ;;
    *) url="$1"; shift ;;
  esac
done
[ -n "$out" ] && [ -n "$url" ]
printf '%s\n' "$url" >> "$CODEX_TEST_CURL_LOG"
case "$url" in
  https://api.github.com/repos/openai/codex/releases/latest)
    # Metadata still outlasts a short 5xx outage with its own bounded retries.
    [ "$retries" = 6 ] && [ "$all_errors" = 1 ] && [ "$retry_time" -ge 600 ]
    cp -- "$CODEX_TEST_RELEASES/latest.json" "$out" ;;
  https://github.com/openai/codex/releases/download/*)
    bash "$SUBYARD_TEST_ARTIFACT_CURL" \
      "$CODEX_TEST_RELEASES/${url#https://github.com/openai/codex/releases/download/}" "${args[@]}" ;;
  *) exit 90 ;;
esac
CURL
chmod +x "$fake_bin/curl"
cat > "$fake_bin/bwrap" <<'BWRAP'
#!/usr/bin/env bash
[ -f "$CODEX_TEST_BWRAP_READY" ] || exit 1
[ "$*" = --version ] || exit 2
printf 'bubblewrap 0.11.0\n'
BWRAP
cat > "$fake_bin/apt-get" <<'APT'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$CODEX_TEST_APT_LOG"
[ "${CODEX_TEST_APT_FAIL:-0}" = 0 ] || exit 1
case "$*" in
  'update -qq') ;;
  'install -y -qq --no-install-recommends bubblewrap') touch "$CODEX_TEST_BWRAP_READY" ;;
  *) exit 90 ;;
esac
APT
chmod +x "$fake_bin/bwrap" "$fake_bin/apt-get"
original_path="$PATH"
export PATH="$fake_bin:$original_path"
export CODEX_TEST_CURL_LOG="$curl_log" CODEX_TEST_RELEASES="$releases"
export SUBYARD_TEST_ARTIFACT_CURL="$ROOT/tests/helpers/artifact-curl.sh" SUBYARD_TEST_TRANSFER_LOG="$tmp/transfer.log"
export CODEX_TEST_BWRAP_READY="$tmp/bwrap-ready" CODEX_TEST_APT_LOG="$tmp/apt.log"
touch "$CODEX_TEST_BWRAP_READY"

fetch_count() {
  if [ -f "$curl_log" ]; then wc -l < "$curl_log"; else printf '0\n'; fi
}

set_latest() { # <version> <amd64-sha> <arm64-sha>
  jq -n --arg version "$1" --arg amd64 "$2" --arg arm64 "$3" '
    def asset($target; $sha):
      ("codex-package-" + $target + "-unknown-linux-musl.tar.gz") as $name |
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
    "$@" bash -euo pipefail -s < "$HOOK"
}

root="$tmp/install"
release_root="$root/opt/subyard/codex/releases"
runtime="$release_root/0.147.0/bin/codex"
link="$root/usr/local/bin/codex"
runtime_link="$root/opt/subyard/codex/codex"
check="$root/usr/local/bin/codex-policy-check"
requirements="$root/etc/codex/requirements.toml"
set_latest 0.147.0 "$sha_147_amd64" "$sha_147_arm64"
run_hook amd64 "$root" >/dev/null
[ "$(fetch_count)" -eq 2 ] || fail "fresh install did not fetch release metadata and artifact"
[ -f "$runtime" ] && [ ! -L "$runtime" ] && [ -x "$runtime" ] \
  || fail "managed runtime is not an executable regular file"
[ "$(stat -c '%a' "$runtime")" = 755 ] || fail "runtime mode is not 0755"
[ -L "$link" ] || fail "canonical Codex command is not a symlink"
[ "$(readlink "$link")" = "$runtime_link" ] || fail "canonical Codex command left the path a retained hook expects"
[ "$(readlink "$runtime_link")" = releases/0.147.0/bin/codex ] || fail "the runtime link does not name the release"
[ -x "$check" ] || fail "Codex package check was not installed"
[ -x "$release_root/0.147.0/codex-path/rg" ] || fail "the package was not installed whole"
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
  */rust-v0.147.0/codex-package-x86_64-unknown-linux-musl.tar.gz) ;;
  *) fail "amd64 did not map to the x86_64 musl artifact" ;;
esac
if [ "$(id -u)" -eq 0 ]; then
  [ "$(stat -c '%u:%g' "$runtime")" = 0:0 ] || fail "runtime is not root-owned"
fi
[ "$(sha256sum "$tmp/home/.codex/auth.json" | cut -d' ' -f1)" = "$auth_before" ] \
  || fail "provision changed yard-local Codex authorization"

# Each rerun resolves current metadata; only a newer release fetches an artifact.
cp "$check" "$tmp/initial-check"
rm "$CODEX_TEST_BWRAP_READY"
if CODEX_CHECK_DEV_USER="$(id -un)" "$check" >/dev/null 2>&1; then
  fail "missing bubblewrap dependency passed readiness"
fi
run_hook amd64 "$root" >/dev/null
[ -f "$CODEX_TEST_BWRAP_READY" ] || fail "current native install did not repair bubblewrap"
[ "$(wc -l < "$CODEX_TEST_APT_LOG")" -eq 2 ] || fail "dependency repair did not install only bubblewrap"
CODEX_CHECK_DEV_USER="$(id -un)" "$check" >/dev/null || fail "repaired sandbox dependency failed readiness"
[ "$(fetch_count)" -eq 3 ] || fail "same-version rerun did not only resolve metadata"
set_latest 0.148.0 "$sha_148_amd64" "$sha_148_arm64"
run_hook amd64 "$root" >/dev/null
[ "$(fetch_count)" -eq 5 ] || fail "upgrade did not fetch metadata and artifact"
[ "$("$link" --version)" = 'codex-cli 0.148.0' ] || fail "upgrade did not publish the current version"
[ "$(readlink "$runtime_link")" = releases/0.148.0/bin/codex ] || fail "upgrade did not move the runtime link"
[ -x "$release_root/0.147.0/bin/codex" ] || fail "upgrade removed the release it replaced"
runtime="$release_root/0.148.0/bin/codex"
CODEX_CHECK_DEV_USER="$(id -un)" "$tmp/initial-check" >/dev/null \
  || fail "readiness check rejected an updated working runtime"
set_latest 0.147.0 "$sha_147_amd64" "$sha_147_arm64"
run_hook amd64 "$root" >/dev/null
[ "$(fetch_count)" -eq 6 ] || fail "older release metadata downloaded an artifact"
[ "$("$link" --version)" = 'codex-cli 0.148.0' ] || fail "older release metadata downgraded a working runtime"

# Artifact failures preserve the previous working runtime and authorization.
before="$(sha256sum "$runtime" | cut -d' ' -f1)"
rm "$CODEX_TEST_BWRAP_READY"
count="$(fetch_count)"
if run_hook amd64 "$root" CODEX_TEST_APT_FAIL=1 >/dev/null 2>"$tmp/dependency.err"; then
  fail "failed sandbox dependency installation unexpectedly succeeded"
fi
[ "$(fetch_count)" -eq "$count" ] || fail "failed dependency repair fetched a release"
[ "$(sha256sum "$runtime" | cut -d' ' -f1)" = "$before" ] || fail "dependency failure replaced the runtime"
touch "$CODEX_TEST_BWRAP_READY"
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
for bad in "8.8.7 $sha_traversal" "8.8.6 $sha_symlink" "8.8.5 $sha_no_binary"; do
  set_latest "${bad% *}" "${bad#* }" "$sha_147_arm64"
  if run_hook amd64 "$root" >/dev/null 2>"$tmp/archive.err"; then
    fail "unsafe archive unexpectedly succeeded: ${bad% *}"
  fi
  [ "$(realpath "$link")" = "$runtime" ] || fail "unsafe archive moved the command: ${bad% *}"
  [ ! -e "$release_root/${bad% *}" ] || fail "unsafe archive was published: ${bad% *}"
done
[ ! -e "$root/opt/subyard/codex/outside" ] && [ ! -e "$release_root/outside" ] \
  || fail "an archive member escaped its stage"
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
if compgen -G "$release_root/.stage.*" >/dev/null; then
  fail "failed install left a staged package"
fi

# Only the published release, the one it replaced and any release a live process runs remain.
mkdir -p "$release_root/.stage.crashed" "$release_root/0.100.0/bin" "$release_root/0.110.0/bin"
cp "$(command -v sleep)" "$release_root/0.110.0/bin/held"
"$release_root/0.110.0/bin/held" 60 & held_pid=$!
set_latest 0.149.0 "$sha_149_amd64" "$sha_148_arm64"
run_hook amd64 "$root" >/dev/null
[ "$(cd "$release_root" && find . -mindepth 1 -maxdepth 1 | LC_ALL=C sort | tr '\n' ' ')" = './0.110.0 ./0.148.0 ./0.149.0 ' ] \
  || fail "pruning kept the wrong releases or removed a live one"
kill "$held_pid"; wait "$held_pid" 2>/dev/null || true
runtime="$release_root/0.149.0/bin/codex"
before="$(sha256sum "$runtime" | cut -d' ' -f1)"

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
[ ! -e "$conflict/opt/subyard/codex/releases" ] || fail "command conflict mutated the managed runtime"
rm "$conflict/usr/local/bin/codex"
for foreign_target in /usr/bin/true "$conflict/opt/subyard/codex/releases/0.1.0/bin/codex"; do
  ln -s "$foreign_target" "$conflict/usr/local/bin/codex"
  if run_hook amd64 "$conflict" >/dev/null 2>"$tmp/conflict.err"; then
    fail "foreign command link was accepted: $foreign_target"
  fi
  [ "$(readlink "$conflict/usr/local/bin/codex")" = "$foreign_target" ] || fail "foreign command link was moved"
  rm "$conflict/usr/local/bin/codex"
done
conflict_runtime="$conflict/opt/subyard/codex/codex"
mkdir -p "$(dirname "$conflict_runtime")"
for foreign_target in /usr/bin/true releases/current/bin/codex releases/0.1.0/codex; do
  ln -s "$foreign_target" "$conflict_runtime"
  if run_hook amd64 "$conflict" >/dev/null 2>"$tmp/conflict.err"; then
    fail "foreign runtime link was accepted: $foreign_target"
  fi
  [ "$(readlink "$conflict_runtime")" = "$foreign_target" ] || fail "foreign runtime link was moved"
  rm "$conflict_runtime"
done
mkdir "$conflict_runtime"
if run_hook amd64 "$conflict" >/dev/null 2>"$tmp/conflict.err"; then fail "a directory at the runtime path was accepted"; fi
rmdir "$conflict_runtime"

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
# Preflight reports every refusal in order and changes nothing.
mkdir -p "$policy_conflict/usr/local/bin"
printf '#!/bin/sh\n' > "$policy_conflict/usr/local/bin/codex"
printf '#!/bin/sh\n' > "$policy_conflict/usr/local/bin/codex-policy-check"
if run_hook s390x "$policy_conflict" >/dev/null 2>"$tmp/preflight.err"; then fail "preflight refusals were accepted"; fi
sed -e 's/^Codex provision: //' -e "s|$policy_conflict||" -e 's/ — .*//' "$tmp/preflight.err" > "$tmp/preflight.actual"
cat > "$tmp/preflight.expected" <<'REFUSALS'
unsupported architecture 's390x' (supported: amd64, arm64)
/usr/local/bin/codex already exists outside the Subyard-managed runtime
/usr/local/bin/codex-policy-check is managed by another install
/etc/codex/requirements.toml is managed by another policy
REFUSALS
diff "$tmp/preflight.expected" "$tmp/preflight.actual" >&2 || fail "preflight refusals are incomplete or out of order"
[ ! -e "$policy_conflict/opt" ] || fail "preflight refusals changed the install root"

# Verify arm64 mapping and fail-fast architecture validation.
arm_root="$tmp/arm-install"
set_latest 0.147.0 "$sha_147_amd64" "$sha_147_arm64"
run_hook arm64 "$arm_root" >/dev/null
tail -n 1 "$curl_log" | grep -Fq '/rust-v0.147.0/codex-package-aarch64-unknown-linux-musl.tar.gz' \
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

# A fresh install also reconciles its OS sandbox dependency.
rm "$CODEX_TEST_BWRAP_READY"
apt_before="$(wc -l < "$CODEX_TEST_APT_LOG")"
fresh_sandbox="$tmp/fresh-sandbox"
run_hook amd64 "$fresh_sandbox" >/dev/null
[ -f "$CODEX_TEST_BWRAP_READY" ] || fail "fresh native install left bubblewrap unavailable"
[ "$(wc -l < "$CODEX_TEST_APT_LOG")" -eq "$((apt_before + 2))" ] || fail "fresh sandbox dependency was not installed"
run_hook amd64 "$fresh_sandbox" >/dev/null
[ "$(wc -l < "$CODEX_TEST_APT_LOG")" -eq "$((apt_before + 2))" ] || fail "ready sandbox dependency was reinstalled"

# A bare binary from earlier provisioning is replaced by the link. A running client keeps its image,
# and even stale metadata cannot strand the yard on it.
bare="$tmp/bare"
bare_runtime="$bare/opt/subyard/codex/codex"
bare_link="$bare/usr/local/bin/codex"
bare_check="$bare/usr/local/bin/codex-policy-check"
bare_requirements="$bare/etc/codex/requirements.toml"
seed_bare() {
  rm -rf "$bare"
  mkdir -p "$(dirname "$bare_runtime")" "$(dirname "$bare_link")" "$(dirname "$bare_requirements")"
  cp "$release_root/0.148.0/bin/codex" "$bare_runtime"
  ln -s "$bare_runtime" "$bare_link"
  printf '#!/usr/bin/env bash\n# Managed by Subyard Codex provision.\nexit 0\n' > "$bare_check"
  chmod 0755 "$bare_check"
  printf '# Managed by Subyard Codex provision.\nallowed_approval_policies = ["on-request"]\n' > "$bare_requirements"
}
seed_bare
exec 9< "$bare_runtime"
set_latest 0.149.0 "$sha_149_amd64" "$sha_148_arm64"
run_hook amd64 "$bare" >/dev/null
[ "$(readlink "$bare_link")" = "$bare_runtime" ] || fail "the command left the runtime path"
[ "$(readlink "$bare_runtime")" = releases/0.149.0/bin/codex ] || fail "bare layout did not move"
[ "$(od -An -tx1 -N4 <&9 | tr -d '[:space:]')" = 7f454c46 ] || fail "open bare image was truncated"
exec 9<&-
CODEX_CHECK_DEV_USER="$(id -un)" "$bare_check" >/dev/null || fail "converged bare layout failed readiness"
cmp "$bare_requirements" "$requirements" || fail "bare layout kept its old policy"
# Forward again after a rollback: a bare binary replaced the link and the release stayed behind.
rm "$bare_runtime"
cp "$release_root/0.148.0/bin/codex" "$bare_runtime"
if CODEX_CHECK_DEV_USER="$(id -un)" "$bare_check" >/dev/null 2>"$tmp/bare.err"; then
  fail "a command on the bare binary passed readiness"
fi
grep -Fq 'run yard init' "$tmp/bare.err" || fail "bare layout readiness lacks its repair"
run_hook amd64 "$bare" >/dev/null
[ "$("$bare_link" --version)" = 'codex-cli 0.149.0' ] || fail "the forward update stayed on the bare binary"
compgen -G "$bare/opt/subyard/codex/releases/.stage.*" >/dev/null && fail "the forward update left the old tree aside"
seed_bare
set_latest 0.147.0 "$sha_147_amd64" "$sha_147_arm64"
run_hook amd64 "$bare" >/dev/null
[ "$("$bare_link" --version)" = 'codex-cli 0.147.0' ] || fail "stale metadata stranded the bare layout"
# A same-version reinstall that cannot publish puts the old tree back, with the command still working.
chmod 0775 "$bare/opt/subyard/codex/releases/0.147.0/bin/codex"
mkdir -p "$tmp/failing-mv"
cat > "$tmp/failing-mv/mv" <<'MV'
#!/usr/bin/env bash
case "$3" in */.stage.aside-*) ;; */.stage.*) exit 1 ;; esac
exec /usr/bin/mv "$@"
MV
chmod +x "$tmp/failing-mv/mv"
if run_hook amd64 "$bare" PATH="$tmp/failing-mv:$fake_bin:$original_path" >/dev/null 2>"$tmp/reinstall.err"; then
  fail "a failed reinstall reported success"
fi
grep -Fq 'could not be published' "$tmp/reinstall.err" || fail "a failed reinstall reported something else"
[ "$("$bare_link" --version)" = 'codex-cli 0.147.0' ] || fail "a failed reinstall broke the command"
[ "$(stat -c '%a' "$bare/opt/subyard/codex/releases/0.147.0/bin/codex")" = 775 ] || fail "a failed reinstall did not restore the old tree"
run_hook amd64 "$bare" >/dev/null
[ "$(stat -c '%a' "$bare/opt/subyard/codex/releases/0.147.0/bin/codex")" = 755 ] || fail "the reinstall did not replace the tree"
compgen -G "$bare/opt/subyard/codex/releases/.stage.*" >/dev/null && fail "the reinstall left the old tree aside"

# Only the standalone installer's exact link is released, and the daemon's home state is upstream's.
set_latest 0.149.0 "$sha_149_amd64" "$sha_148_arm64"
run_hook amd64 "$root" >/dev/null
python3 - "$requirements" <<'PY'
import pathlib, sys, tomllib
assert tomllib.loads(pathlib.Path(sys.argv[1]).read_text())['check_for_update_on_startup'] is False
PY
dev_home="$tmp/dev-home"
shadow="$dev_home/.local/bin/codex"
standalone="$dev_home/.codex/packages/standalone"
run_dev_hook() { run_hook amd64 "$root" CODEX_DEV_HOME="$dev_home" >/dev/null; }
dev_check() { CODEX_CHECK_DEV_USER="$(id -un)" "$check"; }
mkdir -p "$dev_home/.local/bin" "$standalone/releases/0.100.0/bin"
printf 'old\n' > "$standalone/releases/0.100.0/bin/codex"
ln -s releases/0.100.0 "$standalone/current"
printf '0.100.0\n' > "$standalone/auto-update-version"
printf 'keep\n' > "$dev_home/.codex/packages/unknown"
packages_before="$(cd "$dev_home/.codex/packages" && find . -printf '%y %p %l\n' | sort)"
run_dev_hook
[ ! -e "$dev_home/.codex/app-server-daemon" ] || fail "provisioning created daemon settings"

# Independent refusals are all reported, in order. Findings about a missing file are skipped.
mv "$requirements" "$requirements.saved"
rm -f "$link"
ln -s "$standalone/current/bin/codex" "$shadow"
if dev_check >"$tmp/refusals.out" 2>"$tmp/refusals.err"; then fail "readiness passed with refusals"; fi
grep -Fq ' ready' "$tmp/refusals.out" && fail "refused readiness still reported ready"
sed -e 's/^codex-policy-check: //' -e 's/;.*//' "$tmp/refusals.err" > "$tmp/refusals.actual"
cat > "$tmp/refusals.expected" <<'REFUSALS'
managed approval requirements are missing
canonical command does not point to a managed Codex release
a standalone self-installed link shadows the managed runtime
REFUSALS
diff "$tmp/refusals.expected" "$tmp/refusals.actual" >&2 || fail "refusals are incomplete or out of order"
# A missing runtime behind a good link is its own refusal, and a missing policy alone still fails.
ln -s "$runtime_link" "$link"
mv "$runtime" "$runtime.saved"
if dev_check >/dev/null 2>"$tmp/refusals.err"; then fail "a missing runtime passed readiness"; fi
grep -Fxq 'codex-policy-check: managed runtime is missing' "$tmp/refusals.err" || fail "a missing runtime reported something else"
mv "$runtime.saved" "$runtime"
rm -f "$shadow"
if dev_check >/dev/null 2>"$tmp/refusals.err"; then fail "a missing policy passed readiness"; fi
[ "$(cat "$tmp/refusals.err")" = 'codex-policy-check: managed approval requirements are missing' ] \
  || fail "a missing policy reported something else"
# A refusal does not hide a conflict.
printf '#!/bin/sh\n' > "$shadow"
if dev_check >"$tmp/refusals.out" 2>/dev/null; then fail "a missing policy passed beside a conflict"; fi
grep -Fq 'Conflict:' "$tmp/refusals.out" || fail "a refusal hid a conflict"
rm -f "$shadow"
mv "$requirements.saved" "$requirements"
run_dev_hook

metadata_count="$(fetch_count)"
ln -s "$standalone/current/bin/codex" "$shadow"
if dev_check >"$tmp/shadow.out" 2>&1; then fail "standalone shadow passed readiness"; fi
grep -Fq 'run yard init' "$tmp/shadow.out" || fail "standalone shadow readiness lacks its repair"
run_dev_hook
{ [ ! -e "$shadow" ] && [ ! -L "$shadow" ]; } || fail "current provisioning kept the standalone shadow"
[ "$(fetch_count)" -eq "$((metadata_count + 1))" ] || fail "shadow repair downloaded an artifact"
[ "$packages_before" = "$(cd "$dev_home/.codex/packages" && find . -printf '%y %p %l\n' | sort)" ] \
  || fail "shadow repair changed standalone packages"
dev_check >/dev/null || fail "released shadow did not restore readiness"
assert_foreign_shadow() {
  run_dev_hook
  { [ -e "$shadow" ] || [ -L "$shadow" ]; } || fail "foreign shadow removed: $1"
  dev_check >"$tmp/foreign.out" 2>&1 || fail "foreign shadow failed readiness: $1"
  grep -Fq 'Conflict:' "$tmp/foreign.out" || fail "foreign shadow not reported: $1"
  rm -f -- "$shadow"
}
printf '#!/bin/sh\n' > "$shadow"; assert_foreign_shadow 'regular file'
ln -s /usr/bin/true "$shadow"; assert_foreign_shadow 'other program'
if [ "$(id -u)" -ne 0 ]; then
  ln -s "$standalone/current/bin/codex" "$shadow"
  chmod 0500 "$(dirname "$shadow")"
  run_dev_hook
  dev_check >"$tmp/foreign.out" 2>&1 || fail "unremovable standalone link failed readiness"
  grep -Fq 'Conflict:' "$tmp/foreign.out" || fail "unremovable standalone link was not reported"
  chmod 0700 "$(dirname "$shadow")"
  rm -f -- "$shadow"
fi
ln -s "$standalone/releases/0.100.0/bin/codex" "$shadow"; assert_foreign_shadow 'pinned release'
ln -s ".codex/packages/standalone/current/bin/codex" "$shadow"; assert_foreign_shadow 'relative target'
# Hermetic callers without an explicit fixture home must never reach the caller's own home.
mkdir -p "$tmp/home/.local/bin"
ln -s "$tmp/home/.codex/packages/standalone/current/bin/codex" "$tmp/home/.local/bin/codex"
run_hook amd64 "$root" >/dev/null
[ -L "$tmp/home/.local/bin/codex" ] || fail "hook released a link without an explicit fixture home"

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

# Exercise resumable upgrades through the same stdin delivery used by Incus.
transport_root="$tmp/transport"
set_latest 0.147.0 "$sha_147_amd64" "$sha_147_arm64"
run_hook amd64 "$transport_root" >/dev/null
transport_link="$transport_root/usr/local/bin/codex"
set_latest 0.148.0 "$sha_148_amd64" "$sha_148_arm64"
for scenario in corrupt ignore-range range-refused exhausted resume; do
  : > "$SUBYARD_TEST_TRANSFER_LOG"
  if SUBYARD_TEST_TRANSFER_SCENARIO="$scenario" run_hook amd64 "$transport_root" >"$tmp/transfer.out" 2>"$tmp/transfer.err"; then
    [ "$scenario" = resume ] || fail "unsafe transfer accepted: $scenario"
    [ "$("$transport_link" --version)" = 'codex-cli 0.148.0' ] || fail 'resumed upgrade failed'
  else
    [ "$scenario" != resume ] || fail 'stalled download did not resume'
    [ "$("$transport_link" --version)" = 'codex-cli 0.147.0' ] || fail "failed transfer replaced runtime: $scenario"
    case "$scenario" in
      corrupt|ignore-range) grep -Fq 'checksum mismatch' "$tmp/transfer.err" || fail 'missing checksum diagnostic' ;;
      *) grep -Fq 'release artifact download failed' "$tmp/transfer.err" || fail 'missing download diagnostic' ;;
    esac
  fi
  expected=$'0\n64'
  case "$scenario" in range-refused|exhausted) expected=$'0\n64\n64\n64' ;; esac
  [ "$(cat "$SUBYARD_TEST_TRANSFER_LOG")" = "$expected" ] || fail "wrong resume offsets or retry count: $scenario"
done
printf 'ok: native Codex agent provision, stall resume and fail-closed partial downloads\n'
