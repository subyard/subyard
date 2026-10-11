#!/usr/bin/env bash
# Restrict publication to one active Tailscale IPv4; absence retains loopback access.
subyard_owner_tail_address() {
  local address="$1" _first second third fourth
  [[ "$address" =~ ^100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.([0-9]{1,3})\.([0-9]{1,3})$ ]] || return 1
  IFS=. read -r _first second third fourth <<<"$address"
  [ "$second" -ge 64 ] && [ "$second" -le 127 ] &&
    [ "$third" -le 255 ] && [ "$fourth" -le 255 ] &&
    [[ "$third" != 0?* && "$fourth" != 0?* ]]
}

subyard_owner_host() {
  local address
  if address="$(timeout 2 tailscale ip -4 2>/dev/null)" &&
    subyard_owner_tail_address "$address" &&
    ip -j -4 address show up | jq -e --arg address "$address" \
      'any(.[].addr_info[]; .local == $address)' >/dev/null; then
    printf '%s\n' "$address"
  else
    printf '127.0.0.1\n'
  fi
}

