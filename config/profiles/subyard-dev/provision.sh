#!/usr/bin/env bash
# subyard-provision-check-v1
# Install the L1 development toolchain for Subyard. Debian Go is only the bootstrap; the repository
# go.mod selects and downloads the exact compiler into the persistent module cache.
set -euo pipefail

check_only=0
with_rust=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --check) check_only=1 ;;
    --rust) with_rust=1 ;;
    *) printf 'subyard-dev provision: unknown argument %s\n' "$1" >&2; exit 2 ;;
  esac
  shift
done

if [ "$(id -u)" -ne 0 ] && [ "${SUBYARD_DEV_TEST_ALLOW_NON_ROOT:-0}" != 1 ]; then
  printf 'subyard-dev provision: must run as root\n' >&2
  exit 1
fi

DEV_USER="${DEV_USER:-dev}"
DEV_GROUP="${DEV_GROUP:-$(id -gn "$DEV_USER")}"
DEV_HOME="${SUBYARD_DEV_HOME:-$(getent passwd "$DEV_USER" | cut -d: -f6)}"
DEV_HOME="${DEV_HOME:-/home/$DEV_USER}"
GOCACHE="${GOCACHE:-/srv/cache/go-build}"
GOMODCACHE="${GOMODCACHE:-/srv/cache/go-mod}"
SUBYARD_CARGO_BUILD_ROOT="${SUBYARD_CARGO_BUILD_ROOT:-/srv/cache/cargo-build/subyard}"
SUBYARD_NPM_PUBLIC_CACHE="${SUBYARD_NPM_PUBLIC_CACHE:-/srv/cache/npm-public}"
SYSTEM_ROOT="${SUBYARD_DEV_SYSTEM_ROOT:-}"

# PAM launches, login shells (including IDE/agent command shells), and future
# user-manager processes get the same absolute paths despite isolated app XDG.
cache_environment() {
  printf '%s\n' "GOCACHE=$GOCACHE" "GOMODCACHE=$GOMODCACHE" \
    "CARGO_HOME=$DEV_HOME/.cargo" "RUSTUP_HOME=$DEV_HOME/.rustup" \
    "SUBYARD_CARGO_BUILD_ROOT=$SUBYARD_CARGO_BUILD_ROOT" \
    "SUBYARD_NPM_PUBLIC_CACHE=$SUBYARD_NPM_PUBLIC_CACHE"
}
check_environment() {
  local temporary status=0
  temporary="$(mktemp /tmp/subyard-dev-environment.XXXXXX)"
  cache_environment > "$temporary"
  cmp -s "$temporary" "$SYSTEM_ROOT/etc/environment.d/60-subyard-dev.conf" || status=1
  while IFS= read -r line; do
    grep -Fxq "$line" "$SYSTEM_ROOT/etc/environment" 2>/dev/null || status=1
    grep -Fxq "export $line" "$SYSTEM_ROOT/etc/profile.d/subyard-dev.sh" 2>/dev/null || status=1
  done < "$temporary"
  rm -f -- "$temporary"
  return "$status"
}


run_as_dev() {
  if [ "$(id -un)" = "$DEV_USER" ]; then
    (
      cd "$DEV_HOME"
      HOME="$DEV_HOME" "$@"
    )
  else
    runuser -u "$DEV_USER" -- env HOME="$DEV_HOME" \
      sh -c 'cd "$HOME" && exec "$@"' sh "$@"
  fi
}

run_go_as_dev() {
  run_as_dev env -u GOCACHE -u GOMODCACHE -u GOTOOLCHAIN GOENV="$DEV_HOME/.config/go/env" go "$@"
}

run_rustup_as_dev() {
  run_as_dev env -u RUSTUP_TOOLCHAIN CARGO_HOME="$DEV_HOME/.cargo" \
    RUSTUP_HOME="$DEV_HOME/.rustup" PATH="$DEV_HOME/.cargo/bin:$PATH" "$RUSTUP_PROGRAM" "$@"
}

# Ignore a root caller's temporary Cargo PATH. Reuse only the developer's
# normal installation or distro proxies, without replacing distro Rust.
RUSTUP_PROGRAM=""
if [ "$with_rust" = 1 ]; then
  RUSTUP_PROGRAM="$(run_as_dev env PATH="$DEV_HOME/.cargo/bin:/usr/bin:/bin" sh -c 'command -v rustup' || true)"
fi

bootstrap_rustup() (
  case "$(uname -m)" in x86_64) triple=x86_64-unknown-linux-gnu ;; aarch64) triple=aarch64-unknown-linux-gnu ;; *) exit 2 ;; esac
  tool="$(run_as_dev mktemp -d "$DEV_HOME/.cargo/.rustup-init.XXXXXX")"
  trap 'run_as_dev rm -rf -- "$tool"' EXIT
  run_as_dev curl --fail --location --retry 2 --connect-timeout 15 --max-time 180 \
    "https://static.rust-lang.org/rustup/dist/$triple/rustup-init" -o "$tool/rustup-init"
  run_as_dev curl --fail --location --retry 2 --connect-timeout 15 --max-time 180 \
    "https://static.rust-lang.org/rustup/dist/$triple/rustup-init.sha256" -o "$tool/rustup-init.sha256"
  run_as_dev sh -c 'cd "$1" && sha256sum --check rustup-init.sha256' verifier "$tool"
  run_as_dev chmod 0700 "$tool/rustup-init"
  # The standard user installer establishes persistent Cargo PATH. Stable is
  # installed explicitly below; preserve any existing default toolchain.
  run_as_dev env CARGO_HOME="$DEV_HOME/.cargo" RUSTUP_HOME="$DEV_HOME/.rustup" \
    timeout 600 "$tool/rustup-init" -y --profile minimal --default-toolchain none
)

if [ "$check_only" -eq 1 ]; then
  changed=0
  command -v go >/dev/null 2>&1 || changed=1
  command -v shellcheck >/dev/null 2>&1 || changed=1
  for tool in jq rg readelf; do
    command -v "$tool" >/dev/null 2>&1 || changed=1
  done
  if [ "$with_rust" = 1 ]; then
    [ -n "$RUSTUP_PROGRAM" ] && run_rustup_as_dev --version >/dev/null 2>&1 || changed=1
  fi
  group_id="$(getent group "$DEV_GROUP" | cut -d: -f3)"
  expected_owner="$(id -u "$DEV_USER"):$group_id"
  directories=("$GOCACHE" "$GOMODCACHE" "$SUBYARD_CARGO_BUILD_ROOT" "$SUBYARD_NPM_PUBLIC_CACHE" "$DEV_HOME/.config/go")
  [ "$with_rust" = 0 ] || directories+=("$DEV_HOME/.cargo" "$DEV_HOME/.rustup")
  for directory in "${directories[@]}"; do
    if [ ! -d "$directory" ] || [ -L "$directory" ] \
      || [ "$(stat -c '%u:%g' "$directory" 2>/dev/null)" != "$expected_owner" ]; then
      changed=1
    fi
  done
  check_environment || changed=1
  if [ "$changed" -eq 0 ]; then
    mapfile -t go_env < <(run_go_as_dev env GOCACHE GOMODCACHE GOTOOLCHAIN)
    [ "${go_env[0]-}" = "$GOCACHE" ] || changed=1
    [ "${go_env[1]-}" = "$GOMODCACHE" ] || changed=1
    [ "${go_env[2]-}" = auto ] || changed=1
    [ "${#go_env[@]}" -eq 3 ] || changed=1
  fi
  if [ "$changed" -eq 0 ] && [ "$with_rust" = 1 ]; then
    # Explicit installed-toolchain queries stay local; proxy resolution can
    # otherwise install a missing toolchain during a readiness check.
    run_rustup_as_dev run stable cargo --version >/dev/null 2>&1 || changed=1
    run_rustup_as_dev run stable rustc --version >/dev/null 2>&1 || changed=1
    targets="$(run_rustup_as_dev target list --installed --toolchain stable)" || changed=1
    for target in x86_64-unknown-linux-musl aarch64-unknown-linux-musl; do
      grep -Fxq "$target" <<< "$targets" || changed=1
    done
    components="$(run_rustup_as_dev component list --installed --toolchain stable)" || changed=1
    for component in rustfmt clippy; do
      grep -Eq "^$component(-|$)" <<< "$components" || changed=1
    done
  fi
  [ "$changed" -eq 0 ] && exit 0
  exit 10
fi

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install --no-remove -y -qq golang-go shellcheck jq ripgrep binutils curl ca-certificates

install -d -o "$DEV_USER" -g "$DEV_GROUP" "$GOCACHE" "$GOMODCACHE" "$DEV_HOME/.config/go"
install -d -o "$DEV_USER" -g "$DEV_GROUP" "$SUBYARD_CARGO_BUILD_ROOT" "$SUBYARD_NPM_PUBLIC_CACHE"
install -d -m 0755 "$SYSTEM_ROOT/etc/profile.d" "$SYSTEM_ROOT/etc/environment.d"
# Preserve other profiles' PAM settings; replace only our marked block.
[ ! -L "$SYSTEM_ROOT/etc/environment" ] || { printf 'subyard-dev provision: environment must not be a symlink\n' >&2; exit 1; }
touch "$SYSTEM_ROOT/etc/environment"
sed -i '/^# >>> subyard-dev >>>$/,/^# <<< subyard-dev <<<$/d' "$SYSTEM_ROOT/etc/environment"
{
  printf '# >>> subyard-dev >>>\n'
  cache_environment
  printf '# <<< subyard-dev <<<\n'
} >> "$SYSTEM_ROOT/etc/environment"
cache_environment > "$SYSTEM_ROOT/etc/environment.d/60-subyard-dev.conf"
cache_environment | sed 's/^/export /' > "$SYSTEM_ROOT/etc/profile.d/subyard-dev.sh"
chmod 0644 "$SYSTEM_ROOT/etc/environment" "$SYSTEM_ROOT/etc/environment.d/60-subyard-dev.conf" "$SYSTEM_ROOT/etc/profile.d/subyard-dev.sh"

if [ "$with_rust" = 1 ]; then
for directory in "$DEV_HOME/.cargo" "$DEV_HOME/.rustup"; do
  [ ! -L "$directory" ] || { printf 'subyard-dev provision: Rust home must not be a symlink\n' >&2; exit 1; }
  install -d -m 0700 -o "$DEV_USER" -g "$DEV_GROUP" "$directory"
done
if [ -z "$RUSTUP_PROGRAM" ] || ! run_rustup_as_dev --version >/dev/null 2>&1; then
  bootstrap_rustup
  RUSTUP_PROGRAM="$DEV_HOME/.cargo/bin/rustup"
fi
run_rustup_as_dev toolchain install stable --profile minimal --component rustfmt \
  --component clippy --target x86_64-unknown-linux-musl --target aarch64-unknown-linux-musl

fi

run_go_as_dev env -w GOCACHE="$GOCACHE" GOMODCACHE="$GOMODCACHE" GOTOOLCHAIN=auto
run_go_as_dev env GOCACHE GOMODCACHE GOTOOLCHAIN
printf 'subyard-dev provision OK: %s; shellcheck %s\n' \
  "$(run_go_as_dev version)" \
  "$(shellcheck --version | awk '/^version:/ { print $2; exit }')"
