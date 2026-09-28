#!/usr/bin/env bash
# subyard-provision-check-v1
# Install a pinned runtime without starting or publishing the VPN.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
destination=/usr/local/lib/subyard-amnezia
unit=/etc/systemd/system/subyard-amnezia.service
# shellcheck source=release.env
. "$root/release.env"
die() { printf 'amnezia provision: %s\n' "$*" >&2; exit 1; }
case "${1:-}" in ''|--check) ;; *) die 'expected --check or no arguments' ;; esac
[ "$#" -le 1 ] || die 'unexpected arguments'
[ "$(id -u)" = 0 ] || die 'must run inside the dedicated VM as root'
[ "${EXCLUSIVE_ENVIRONMENT_PROFILE:-}" = amnezia ] && [ "${YARD_KIND:-}" = vm ] \
  || die 'requires the selected dedicated amnezia VM preset'
[ "$(uname -m)" = x86_64 ] || die 'the pinned Amnezia runtime supports amd64 only'
packages=(python3 nftables iproute2 iptables)
ready() {
  local name
  for name in "${packages[@]}"; do
    [ "$(dpkg-query -W -f='${Status}' "$name" 2>/dev/null)" = 'install ok installed' ] || return 1
  done
  [ -d "$destination" ] && [ ! -L "$destination" ] \
    && [ "$(stat -c '%u:%g:%a' "$destination")" = '0:0:755' ] || return 1
  for name in runtime.py container.sh release.env; do
    [ -f "$destination/$name" ] && [ ! -L "$destination/$name" ] \
      && [ "$(stat -c '%u:%g:%a' "$destination/$name")" = '0:0:644' ] \
      && cmp -s "$root/$name" "$destination/$name" || return 1
  done
  [ -f "$unit" ] && [ ! -L "$unit" ] \
    && [ "$(stat -c '%u:%g:%a' "$unit")" = '0:0:644' ] \
    && cmp -s "$root/subyard-amnezia.service" "$unit" || return 1
  docker image inspect "$AMNEZIA_IMAGE" >/dev/null 2>&1
}
if [ "${1:-}" = --check ]; then ready && exit 0; exit 10; fi
command -v docker >/dev/null || die 'core yard provisioning must install Docker first'
if ! ready; then
  export DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a
  apt-get update -qq
  apt-get install -y -qq "${packages[@]}"
  docker image inspect "$AMNEZIA_IMAGE" >/dev/null 2>&1 || docker pull "$AMNEZIA_IMAGE"
  [ ! -L "$destination" ] || die 'runtime directory must not be a symlink'
  install -d -o 0 -g 0 -m 0755 "$destination"
  for name in runtime.py container.sh release.env; do
    [ ! -L "$destination/$name" ] || die 'runtime file must not be a symlink'
    install -o 0 -g 0 -m 0644 "$root/$name" "$destination/$name"
  done
  [ ! -L "$unit" ] || die 'service unit must not be a symlink'
  install -o 0 -g 0 -m 0644 "$root/subyard-amnezia.service" "$unit"
  systemctl daemon-reload
fi
ready || die 'runtime provisioning did not converge'
printf 'amnezia provision OK: pinned runtime available; VPN enablement is unchanged\n'
