#!/usr/bin/env bash
# Subyard development profile checks with fake package and toolchain commands.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
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

tmp="$(mktemp -d)"
adapter_cwd="$tmp/adapter-cwd"
trap 'chmod 0700 "$adapter_cwd" 2>/dev/null || true; rm -rf "$tmp"' EXIT
mkdir -p "$tmp/fake-bin" "$tmp/home" "$adapter_cwd"

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
chmod +x "$tmp/fake-bin/apt-get" "$tmp/fake-bin/go" "$tmp/fake-bin/shellcheck"

export SUBYARD_DEV_APT_LOG="$tmp/apt.log"
export SUBYARD_DEV_GO_LOG="$tmp/go.log"
test_cache="$tmp/cache/go-build"
test_mod_cache="$tmp/cache/go-mod"
common_env=(
  PATH="$tmp/fake-bin:$PATH"
  SUBYARD_DEV_TEST_ALLOW_NON_ROOT=1
  DEV_USER="$(id -un)"
  DEV_GROUP="$(id -gn)"
  SUBYARD_DEV_HOME="$tmp/home"
  GOCACHE="$test_cache"
  GOMODCACHE="$test_mod_cache"
  SUBYARD_DEV_EXPECT_CWD="$tmp/home"
  SUBYARD_DEV_EXPECT_GOCACHE="$test_cache"
  SUBYARD_DEV_EXPECT_GOMODCACHE="$test_mod_cache"
)

run_hook() {
  chmod 0700 "$adapter_cwd"
  (
    cd "$adapter_cwd"
    chmod 000 .
    env "${common_env[@]}" bash "$HOOK" >/dev/null
  )
  chmod 0700 "$adapter_cwd"
}

run_hook
[ -d "$test_cache" ] || fail "Go build cache was not created"
[ -d "$test_mod_cache" ] || fail "Go module cache was not created"
[ -d "$tmp/home/.config/go" ] || fail "Go user config directory was not created"
grep -Fxq 'update -qq' "$SUBYARD_DEV_APT_LOG" || fail "apt metadata was not refreshed"
grep -Fxq 'install -y -qq golang-go shellcheck' "$SUBYARD_DEV_APT_LOG" \
  || fail "bootstrap dependencies were not installed"
grep -Fxq "GOCACHE=$test_cache GOMODCACHE=$test_mod_cache GOTOOLCHAIN=auto" \
  "$SUBYARD_DEV_GO_LOG" || fail "persistent Go environment was not configured"

# Re-provisioning converges to the same settings.
run_hook
[ "$(wc -l < "$SUBYARD_DEV_GO_LOG")" -eq 2 ] \
  || fail "Go environment was not re-applied exactly once per provision"

apt_lines="$(wc -l < "$SUBYARD_DEV_APT_LOG")"
go_lines="$(wc -l < "$SUBYARD_DEV_GO_LOG")"
env "${common_env[@]}" bash "$HOOK" --check >/dev/null \
  || fail "converged provision check failed"
[ "$(wc -l < "$SUBYARD_DEV_APT_LOG")" -eq "$apt_lines" ] \
  || fail "provision check mutated apt state"
[ "$(wc -l < "$SUBYARD_DEV_GO_LOG")" -eq "$go_lines" ] \
  || fail "provision check rewrote Go settings"

rm -rf -- "$test_mod_cache"
set +e
env "${common_env[@]}" bash "$HOOK" --check >/dev/null
check_status=$?
set -e
[ "$check_status" -eq 10 ] || fail "drifted provision check returned $check_status, want 10"

if [ "$(id -u)" -ne 0 ]; then
  if env PATH="$tmp/fake-bin:$PATH" DEV_USER="$(id -un)" \
    SUBYARD_DEV_HOME="$tmp/home" GOCACHE="$test_cache" GOMODCACHE="$test_mod_cache" \
    bash "$HOOK" >"$tmp/non-root.out" 2>&1; then
    fail "non-root provision unexpectedly succeeded"
  fi
  grep -Fq 'must run as root' "$tmp/non-root.out" || fail "root requirement is unclear"
fi

printf 'ok: subyard-dev provision\n'
