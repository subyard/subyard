#!/usr/bin/env bash
# Subyard development profile checks with fake package and toolchain commands.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../" && pwd)"
PROFILE="$ROOT/config/profiles/subyard-dev/profile.conf"
HOOK="$ROOT/config/profiles/subyard-dev/provision.sh"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

[ -x "$HOOK" ] || fail "subyard-dev provision hook is not executable"
# shellcheck source=config/profiles/subyard-dev/profile.conf
. "$PROFILE"
[ "$PROFILE_NAME" = subyard-dev ] || fail "profile name drifted"
[ "$GOCACHE" = /srv/cache/go-build ] || fail "Go build cache drifted"
[ "$GOMODCACHE" = /srv/cache/go-mod ] || fail "Go module cache drifted"
for cache in /srv/cache/go-build /srv/cache/go-mod; do
  case " $CACHES " in
    *" $cache "*) ;;
    *) fail "persistent Go cache is not declared: $cache" ;;
  esac
done

tmp="$(mktemp -d /tmp/subyard-dev-provision.XXXXXX)"
adapter_cwd="$tmp/adapter-cwd"
trap 'chmod 0700 "$adapter_cwd" 2>/dev/null || true; rm -rf "$tmp"' EXIT
mkdir -p "$tmp/fake-bin" "$tmp/home" "$adapter_cwd"

# Developer provisioning must not require or change a user manager.
export SUBYARD_DEV_MANAGER_LOG="$tmp/manager-invocations"
for tool in loginctl systemctl; do
  cat > "$tmp/fake-bin/$tool" <<'SH'
#!/bin/sh
: > "${SUBYARD_DEV_MANAGER_LOG:?}"
exit 99
SH
  chmod +x "$tmp/fake-bin/$tool"
done

cat > "$tmp/fake-bin/apt-get" <<'APT'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$SUBYARD_DEV_APT_LOG"
APT
cat > "$tmp/fake-bin/go" <<'GO'
#!/usr/bin/env bash
set -euo pipefail
[ "$(pwd -P)" = "$SUBYARD_DEV_EXPECT_CWD" ] || {
  printf 'go ran from %s, want %s\n' "$(pwd -P)" "$SUBYARD_DEV_EXPECT_CWD" >&2
  exit 91
}
[ "$GOENV" = "$HOME/.config/go/env" ] || exit 98
case "${1:-}" in
  env)
    if [ "${2:-}" = -w ]; then
      printf '%s\n' "${*:3}" >> "$SUBYARD_DEV_GO_LOG"
    else
      printf '%s\n' \
        "$SUBYARD_DEV_EXPECT_GOCACHE" "$SUBYARD_DEV_EXPECT_GOMODCACHE" auto
    fi
    ;;
  version) printf '%s\n' 'go version go1.24.0 linux/amd64' ;;
  *) exit 90 ;;
esac
GO
cat > "$tmp/fake-bin/shellcheck" <<'SHELLCHECK'
#!/usr/bin/env bash
printf '%s\n' 'ShellCheck - shell script analysis tool' 'version: 0.10.0'
SHELLCHECK
cat > "$tmp/fake-bin/rustup" <<'RUSTUP'
#!/usr/bin/env bash
set -euo pipefail
[ "$(pwd -P)" = "$SUBYARD_DEV_EXPECT_CWD" ] || exit 92
[ "$CARGO_HOME" = "$HOME/.cargo" ] && [ "$RUSTUP_HOME" = "$HOME/.rustup" ] || exit 93
case "$*" in
  --version) printf 'rustup fixture\n' ;;
  'toolchain install stable --profile minimal --component rustfmt --component clippy --target x86_64-unknown-linux-musl --target aarch64-unknown-linux-musl')
    [ "${SUBYARD_DEV_TEST_INSTALL_FAIL:-0}" != 1 ] || exit 70
    printf '%s\n' "$*" >> "$SUBYARD_DEV_RUSTUP_LOG" ;;
  'run stable cargo --version') printf 'cargo fixture\n' ;;
  'run stable rustc --version') printf 'rustc fixture\n' ;;
  'target list --installed --toolchain stable')
    printf 'x86_64-unknown-linux-musl\n'
    [ "${SUBYARD_DEV_TEST_MISSING_TARGET:-0}" = 1 ] || printf 'aarch64-unknown-linux-musl\n' ;;
  'component list --installed --toolchain stable') printf 'rustfmt-x86_64-unknown-linux-gnu\nclippy-x86_64-unknown-linux-gnu\n' ;;
  *) exit 94 ;;
esac
RUSTUP
for tool in jq rg readelf; do
  printf '#!/bin/sh\nexit 0\n' > "$tmp/fake-bin/$tool"
done
cat > "$tmp/fake-bin/curl" <<'CURL'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' download >> "$SUBYARD_DEV_CURL_LOG"
while [ "$#" -gt 0 ]; do
  case "$1" in -o) output="$2"; shift 2 ;; *) shift ;; esac
done
case "$output" in
  *.sha256)
    if [ "${SUBYARD_DEV_TEST_BAD_CHECKSUM:-0}" = 1 ]; then
      printf '%064d  rustup-init\n' 0 > "$output"
    else
      (cd "$(dirname "$output")" && sha256sum rustup-init) > "$output"
    fi ;;
  *)
    cat > "$output" <<'INIT'
#!/usr/bin/env bash
set -euo pipefail
[ "$CARGO_HOME" = "$HOME/.cargo" ] && [ "$RUSTUP_HOME" = "$HOME/.rustup" ] || exit 95
[ "$(id -un)" = "$DEV_USER" ] || exit 96
[ "$*" = '-y --profile minimal --default-toolchain none' ] || exit 97
printf '%s\n' installed >> "$SUBYARD_DEV_INIT_LOG"
install -m 0755 "$SUBYARD_DEV_RUSTUP_FIXTURE" "$CARGO_HOME/bin/rustup"
printf 'export PATH="$HOME/.cargo/bin:$PATH"\n' > "$HOME/.profile"
INIT
  ;;
esac
CURL
chmod +x "$tmp"/fake-bin/*
install -d -m 0700 "$tmp/home/.cargo" "$tmp/home/.cargo/bin"
cp "$tmp/fake-bin/rustup" "$tmp/home/.cargo/bin/rustup"

export SUBYARD_DEV_APT_LOG="$tmp/apt.log"
export SUBYARD_DEV_GO_LOG="$tmp/go.log"
export SUBYARD_DEV_RUSTUP_LOG="$tmp/rustup.log"
export SUBYARD_DEV_CURL_LOG="$tmp/curl.log"
export SUBYARD_DEV_INIT_LOG="$tmp/init.log"
export SUBYARD_DEV_RUSTUP_FIXTURE="$tmp/fake-bin/rustup"
test_cache="$tmp/cache/go-build"
test_mod_cache="$tmp/cache/go-mod"
common_env=(
  PATH="$tmp/fake-bin:$PATH"
  CARGO_HOME="$tmp/forbidden-cargo-home"
  RUSTUP_HOME="$tmp/forbidden-rustup-home"
  SUBYARD_DEV_TEST_ALLOW_NON_ROOT=1
  DEV_USER="$(id -un)"
  DEV_GROUP="$(id -gn)"
  SUBYARD_DEV_HOME="$tmp/home"
  GOCACHE="$test_cache"
  GOMODCACHE="$test_mod_cache"
  SUBYARD_CARGO_BUILD_ROOT="$tmp/cache/cargo-build/subyard"
  SUBYARD_NPM_PUBLIC_CACHE="$tmp/cache/npm-public"
  SUBYARD_DEV_SYSTEM_ROOT="$tmp/system"
  SUBYARD_DEV_EXPECT_CWD="$tmp/home"
  SUBYARD_DEV_EXPECT_GOCACHE="$test_cache"
  SUBYARD_DEV_EXPECT_GOMODCACHE="$test_mod_cache"
)

run_hook() {
  chmod 0700 "$adapter_cwd"
  (
    cd "$adapter_cwd"
    chmod 000 .
    env "${common_env[@]}" bash "$HOOK" --rust >/dev/null
  )
  chmod 0700 "$adapter_cwd"
}

run_hook
[ -d "$test_cache" ] || fail "Go build cache was not created"
[ -d "$test_mod_cache" ] || fail "Go module cache was not created"
[ -d "$tmp/home/.config/go" ] || fail "Go user config directory was not created"
# An agent shell sources the system profile while retaining its isolated XDG.
env -i PATH="$PATH" XDG_CONFIG_HOME="$tmp/agent-config" bash --noprofile --norc -c \
  '. "$1"; [ "$GOCACHE" = "$2" ] && [ "$GOMODCACHE" = "$3" ] && [ "$XDG_CONFIG_HOME" = "$4" ] && [ "$CARGO_HOME" = "$5/.cargo" ] && [ "$RUSTUP_HOME" = "$5/.rustup" ]' \
  sh "$tmp/system/etc/profile.d/subyard-dev.sh" "$test_cache" "$test_mod_cache" "$tmp/agent-config" "$tmp/home" \
  || fail "fresh isolated agent shell did not inherit absolute cache paths"

grep -Fxq 'update -qq' "$SUBYARD_DEV_APT_LOG" || fail "apt metadata was not refreshed"
grep -Fxq 'install --no-remove -y -qq golang-go shellcheck jq ripgrep binutils curl ca-certificates' "$SUBYARD_DEV_APT_LOG" \
  || fail "bootstrap dependencies were not installed"
grep -Fxq "GOCACHE=$test_cache GOMODCACHE=$test_mod_cache GOTOOLCHAIN=auto" \
  "$SUBYARD_DEV_GO_LOG" || fail "persistent Go environment was not configured"

[ -d "$tmp/home/.cargo" ] && [ -d "$tmp/home/.rustup" ] || fail "normal dev Rust homes missing"
[ ! -e "$tmp/forbidden-cargo-home" ] && [ ! -e "$tmp/forbidden-rustup-home" ] || fail "caller Rust homes were used"
[ "$(wc -l < "$SUBYARD_DEV_RUSTUP_LOG")" -eq 1 ] || fail "normal Rust toolchain was not installed"

# Re-provisioning converges to the same settings.
run_hook
[ "$(wc -l < "$SUBYARD_DEV_GO_LOG")" -eq 2 ] \
  || fail "Go environment was not re-applied exactly once per provision"

# Ordinary Go preparation/checks neither create Rust homes nor install Rust.
mv "$tmp/home/.cargo" "$tmp/saved-cargo"
mv "$tmp/home/.rustup" "$tmp/saved-rustup"
rust_before="$(wc -l < "$SUBYARD_DEV_RUSTUP_LOG")"
env "${common_env[@]}" SUBYARD_DEV_TEST_MISSING_TARGET=1 bash "$HOOK" >/dev/null
env "${common_env[@]}" SUBYARD_DEV_TEST_MISSING_TARGET=1 bash "$HOOK" --check >/dev/null \
  || fail "ordinary Go readiness unexpectedly required Rust"
[ "$(wc -l < "$SUBYARD_DEV_RUSTUP_LOG")" -eq "$rust_before" ] || fail "ordinary Go provision installed Rust"
[ ! -e "$tmp/home/.cargo" ] && [ ! -e "$tmp/home/.rustup" ] || fail "ordinary Go provision created Rust homes"
mv "$tmp/saved-cargo" "$tmp/home/.cargo"
mv "$tmp/saved-rustup" "$tmp/home/.rustup"
rustup_lines="$(wc -l < "$SUBYARD_DEV_RUSTUP_LOG")"
apt_lines="$(wc -l < "$SUBYARD_DEV_APT_LOG")"
go_lines="$(wc -l < "$SUBYARD_DEV_GO_LOG")"
env "${common_env[@]}" bash "$HOOK" --rust --check >/dev/null \
  || fail "converged provision check failed"
[ "$(wc -l < "$SUBYARD_DEV_APT_LOG")" -eq "$apt_lines" ] \
  || fail "provision check mutated apt state"
[ "$(wc -l < "$SUBYARD_DEV_GO_LOG")" -eq "$go_lines" ] \
  || fail "provision check rewrote Go settings"

[ "$(wc -l < "$SUBYARD_DEV_RUSTUP_LOG")" -eq "$rustup_lines" ] || fail "check downloaded a Rust toolchain"
set +e
env "${common_env[@]}" SUBYARD_DEV_TEST_MISSING_TARGET=1 bash "$HOOK" --rust --check >/dev/null
check_status=$?
set -e
[ "$check_status" -eq 10 ] || fail "missing musl target check returned $check_status, want 10"

rm -rf -- "$test_mod_cache"
set +e
env "${common_env[@]}" bash "$HOOK" --rust --check >/dev/null
check_status=$?
set -e
[ "$check_status" -eq 10 ] || fail "drifted provision check returned $check_status, want 10"

set +e
env "${common_env[@]}" SUBYARD_DEV_TEST_INSTALL_FAIL=1 bash "$HOOK" --rust >/dev/null
install_status=$?
set -e
[ "$install_status" -eq 70 ] || fail "failed Rust installation returned $install_status, want 70"

# An existing usable Rustup is reused without downloads. A broken/missing
# user installation is bootstrapped without removing a distro compiler.
[ ! -e "$SUBYARD_DEV_CURL_LOG" ] || fail "existing Rustup was downloaded again"
printf '#!/bin/sh\nprintf distro-preserved\n' > "$tmp/fake-bin/rustc"
chmod 0755 "$tmp/fake-bin/rustc"
distro_before="$(sha256sum "$tmp/fake-bin/rustc")"
printf '#!/bin/sh\nexit 127\n' > "$tmp/home/.cargo/bin/rustup"
chmod 0755 "$tmp/home/.cargo/bin/rustup"
run_hook
[ "$(sha256sum "$tmp/fake-bin/rustc")" = "$distro_before" ] || fail "distro Rust was replaced"
[ "$(wc -l < "$SUBYARD_DEV_CURL_LOG")" -eq 2 ] || fail "Rustup bootstrap did not verify its download"
[ "$(wc -l < "$SUBYARD_DEV_INIT_LOG")" -eq 1 ] || fail "Rustup init did not run as developer"
grep -Fq '.cargo/bin' "$tmp/home/.profile" || fail "Rustup init omitted persistent user PATH"

printf '#!/bin/sh\nexit 127\n' > "$tmp/home/.cargo/bin/rustup"
set +e
env "${common_env[@]}" SUBYARD_DEV_TEST_BAD_CHECKSUM=1 bash "$HOOK" --rust >"$tmp/bad-checksum.out" 2>&1
checksum_status=$?
set -e
[ "$checksum_status" -ne 0 ] || fail "unverified Rustup installer was accepted"
[ "$(wc -l < "$SUBYARD_DEV_INIT_LOG")" -eq 1 ] || fail "corrupt Rustup download executed"

if [ "$(id -u)" -ne 0 ]; then
  if env PATH="$tmp/fake-bin:$PATH" DEV_USER="$(id -un)" \
    SUBYARD_DEV_HOME="$tmp/home" GOCACHE="$test_cache" GOMODCACHE="$test_mod_cache" \
    bash "$HOOK" --rust >"$tmp/non-root.out" 2>&1; then
    fail "non-root provision unexpectedly succeeded"
  fi
  grep -Fq 'must run as root' "$tmp/non-root.out" || fail "root requirement is unclear"
fi

[ ! -e "$SUBYARD_DEV_MANAGER_LOG" ] || fail "provision invoked a user manager command"
printf 'ok: subyard-dev provision\n'
