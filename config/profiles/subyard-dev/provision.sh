#!/usr/bin/env bash
# subyard-provision-check-v1
# Install the L1 development toolchain for Subyard. Debian Go is only the bootstrap; the repository
# go.mod selects and downloads the exact compiler into the persistent module cache.
set -euo pipefail

check_only=0
case "${1:-}" in
  --check) check_only=1; shift ;;
  "") ;;
  *) printf 'subyard-dev provision: unknown argument %s\n' "$1" >&2; exit 2 ;;
esac
[ "$#" -eq 0 ] || { printf 'subyard-dev provision: unexpected argument\n' >&2; exit 2; }

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
  run_as_dev env -u GOCACHE -u GOMODCACHE -u GOTOOLCHAIN go "$@"
}

if [ "$check_only" -eq 1 ]; then
  changed=0
  command -v go >/dev/null 2>&1 || changed=1
  command -v shellcheck >/dev/null 2>&1 || changed=1
  group_id="$(getent group "$DEV_GROUP" | cut -d: -f3)"
  expected_owner="$(id -u "$DEV_USER"):$group_id"
  for directory in "$GOCACHE" "$GOMODCACHE" "$DEV_HOME/.config/go"; do
    if [ ! -d "$directory" ] || [ -L "$directory" ] \
      || [ "$(stat -c '%u:%g' "$directory" 2>/dev/null)" != "$expected_owner" ]; then
      changed=1
    fi
  done
  if [ "$changed" -eq 0 ]; then
    mapfile -t go_env < <(run_go_as_dev env GOCACHE GOMODCACHE GOTOOLCHAIN)
    [ "${go_env[0]-}" = "$GOCACHE" ] || changed=1
    [ "${go_env[1]-}" = "$GOMODCACHE" ] || changed=1
    [ "${go_env[2]-}" = auto ] || changed=1
    [ "${#go_env[@]}" -eq 3 ] || changed=1
  fi
  [ "$changed" -eq 0 ] && exit 0
  exit 10
fi

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq golang-go shellcheck

install -d -o "$DEV_USER" -g "$DEV_GROUP" "$GOCACHE" "$GOMODCACHE" "$DEV_HOME/.config/go"

run_go_as_dev env -w GOCACHE="$GOCACHE" GOMODCACHE="$GOMODCACHE" GOTOOLCHAIN=auto
run_go_as_dev env GOCACHE GOMODCACHE GOTOOLCHAIN
printf 'subyard-dev provision OK: %s; shellcheck %s\n' \
  "$(run_go_as_dev version)" "$(shellcheck --version | awk '/^version:/ { print $2; exit }')"
