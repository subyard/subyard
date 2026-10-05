#!/usr/bin/env bash
# subyard-provision-check-v1
# Prepare the dedicated environment without installing or publishing a VPN.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
destination=/usr/local/lib/subyard-amnezia
unit=/etc/systemd/system/subyard-amnezia.service
die() { printf 'amnezia provision: %s\n' "$*" >&2; exit 1; }
case "${1:-}" in ''|--check) ;; *) die 'expected --check or no arguments' ;; esac
[ "$#" -le 1 ] || die 'unexpected arguments'
[ "$(id -u)" = 0 ] || die 'must run inside the dedicated VM as root'
[ "${EXCLUSIVE_ENVIRONMENT_PROFILE:-}" = amnezia ] && [ "${YARD_KIND:-}" = vm ] \
  || die 'requires the selected dedicated amnezia VM preset'
[ "$(uname -m)" = x86_64 ] || die 'the native Amnezia application supports amd64 only'
packages=(python3 nftables iproute2 iptables)
ready() {
  local name
  for name in "${packages[@]}"; do
    [ "$(dpkg-query -W -f='${Status}' "$name" 2>/dev/null)" = 'install ok installed' ] || return 1
  done
  [ -d "$destination" ] && [ ! -L "$destination" ] \
    && [ "$(stat -c '%u:%g:%a' "$destination")" = '0:0:755' ] || return 1
  [ -f "$destination/runtime.py" ] && [ ! -L "$destination/runtime.py" ] \
    && [ "$(stat -c '%u:%g:%a' "$destination/runtime.py")" = '0:0:644' ] \
    && cmp -s "$root/runtime.py" "$destination/runtime.py" || return 1
  [ -f "$unit" ] && [ ! -L "$unit" ] \
    && [ "$(stat -c '%u:%g:%a' "$unit")" = '0:0:644' ] \
    && cmp -s "$root/subyard-amnezia.service" "$unit" || return 1
  python3 "$destination/runtime.py" observe 2>/dev/null | python3 -c 'import json,sys; sys.exit(0 if json.load(sys.stdin)["ready"] else 1)' 2>/dev/null
}
if [ "${1:-}" = --check ]; then ready && exit 0; exit 10; fi
python3 "$root/runtime.py" check-state
command -v docker >/dev/null || die 'core yard provisioning must install Docker first'
if ! ready; then
  export DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a
  apt-get update -qq
  apt-get install -y -qq "${packages[@]}"
  [ ! -L "$destination" ] || die 'runtime directory must not be a symlink'
  install -d -o 0 -g 0 -m 0755 "$destination"
  [ ! -L "$destination/runtime.py" ] || die 'runtime file must not be a symlink'
  install -o 0 -g 0 -m 0644 "$root/runtime.py" "$destination/runtime.py"
  [ ! -L "$unit" ] || die 'service unit must not be a symlink'
  install -o 0 -g 0 -m 0644 "$root/subyard-amnezia.service" "$unit"
  systemctl daemon-reload
fi
python3 "$destination/runtime.py" provision
ready || die 'runtime provisioning did not converge'
printf 'amnezia provision OK: dedicated environment ready; install and manage VPN in the AmneziaVPN app\n'
