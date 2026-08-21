#!/usr/bin/env bash
# subyard-provision-check-v1
# Install only the generic OS prerequisites for an independently managed Hermes Agent.
set -euo pipefail

check_only=0
case "${1:-}" in
  --check) check_only=1; shift ;;
  '') ;;
  *) printf 'hermes provision: unknown argument %s\n' "$1" >&2; exit 2 ;;
esac
[ "$#" -eq 0 ] || { printf 'hermes provision: unexpected argument\n' >&2; exit 2; }

die() { printf 'hermes provision: %s\n' "$*" >&2; exit 1; }
if [ "$(id -u)" -ne 0 ] && [ "${HERMES_TEST_ALLOW_NON_ROOT:-0}" != 1 ]; then
  die 'must run as root'
fi

packages=(
  build-essential
  ca-certificates
  curl
  git
  libffi-dev
  python3-dev
  xz-utils
)

packages_ready() {
  local package
  for package in "${packages[@]}"; do
    [ "$(dpkg-query -W -f='${Status}' "$package" 2>/dev/null)" = 'install ok installed' ] \
      || return 1
  done
}

if [ "$check_only" -eq 1 ]; then
  packages_ready && exit 0
  exit 10
fi

if packages_ready; then
  printf 'hermes provision OK: generic OS prerequisites are already installed\n'
  exit 0
fi

export DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a
apt-get update -qq
apt-get install -y -qq "${packages[@]}"

packages_ready || die 'generic OS prerequisites are incomplete after package installation'
printf 'hermes provision OK: generic OS prerequisites are installed\n'
