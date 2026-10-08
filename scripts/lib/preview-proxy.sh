#!/usr/bin/env bash
# Publish preview on an active Tailscale or private primary owner IPv4.
# shellcheck source=scripts/lib/ai-observer-proxy.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/ai-observer-proxy.sh"

subyard_preview_private_address() {
  local address="$1" first second third fourth octet
  [[ "$address" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || return 1
  IFS=. read -r first second third fourth <<<"$address"
  for octet in "$first" "$second" "$third" "$fourth"; do
    [[ "$octet" != 0?* ]] && [ "$octet" -le 255 ] || return 1
  done
  [ "$first" = 10 ] ||
    { [ "$first" = 172 ] && [ "$second" -ge 16 ] && [ "$second" -le 31 ]; } ||
    { [ "$first" = 192 ] && [ "$second" = 168 ]; }
}

subyard_preview_address() {
  subyard_ai_observer_tail_address "$1" || subyard_preview_private_address "$1"
}

subyard_preview_route() {
  local snapshot marker device local_device host="$1" port="${WEB_PREVIEW_HOST_PORT:-}" desired guest=""
  local current_host="" current_port="" current_guest="" version value expected key=user.subyard.preview_proxy
  if [ "$host" != 127.0.0.1 ] && { ! subyard_preview_address "$host" ||
    ! ip -j -4 address show up | jq -e --arg address "$host" \
      'any(.[].addr_info[]; .local == $address)' >/dev/null; }; then
    printf 'Preview: prepared owner address is invalid or no longer active; retry init\n' >&2
    return 1
  fi
  snapshot="$(incus query "/1.0/instances/$YARD_INSTANCE_NAME?project=$INCUS_PROJECT")" || return 1
  marker="$(jq -r --arg key "$key" '.config[$key] // ""' <<<"$snapshot")" || return 1
  local_device="$(jq -c '.devices["subyard-preview"] // null' <<<"$snapshot")" || return 1
  device="$(jq -c '(.expanded_devices // .devices)["subyard-preview"] // null' <<<"$snapshot")" || return 1
  if [ -n "$marker" ]; then
    version="${marker%%:*}"
    value="${marker#*:}"
    value="${value#pending:}"
    IFS=: read -r current_host current_port current_guest <<<"$value"
    if ! { { [ "$version" = v1 ] && [ -z "$current_guest" ] && [ "$value" = "$current_host:$current_port" ]; } ||
      { [ "$version" = v2 ] && subyard_preview_private_address "$current_guest" && [ "$value" = "$current_host:$current_port:$current_guest" ]; }; } ||
      ! subyard_preview_address "$current_host" ||
      ! [[ "$current_port" =~ ^[1-9][0-9]{3,4}$ ]] ||
      [ "$current_port" -lt 1024 ] || [ "$current_port" -gt 65535 ]; then
      printf 'Preview: invalid route ownership receipt\n' >&2
      return 1
    fi
  fi
  expected="$(jq -cn --arg host "$current_host" --arg port "$current_port" --arg guest "$current_guest" '
    {type:"proxy",bind:"host",listen:("tcp:"+$host+":"+$port),connect:"tcp:127.0.0.1:8765"}
    | if $guest != "" then . + {nat:"true",connect:("tcp:"+$guest+":8765")} else . end')" || return 1
  if [ "$device" != null ] || [ "$local_device" != null ]; then
    if [ -z "$marker" ] ||
      ! jq -en --argjson local "$local_device" --argjson effective "$device" --argjson expected "$expected" \
        '$local == $effective and $effective == $expected' >/dev/null; then
      printf 'Preview: refusing to replace a foreign or divergent subyard-preview device\n' >&2
      return 1
    fi
  fi
  if [ "$host" = 127.0.0.1 ]; then
    if [ "$device" != null ]; then
      incus config device remove "$YARD_INSTANCE_NAME" subyard-preview "${PROJ[@]}" >/dev/null || return 1
    fi
    [ -z "$marker" ] || incus config unset "$YARD_INSTANCE_NAME" "$key" "${PROJ[@]}" >/dev/null || return 1
    return 0
  fi
  if ! [[ "$port" =~ ^[1-9][0-9]{3,4}$ ]] || [ "$port" -lt 1024 ] || [ "$port" -gt 65535 ]; then
    printf 'Preview: WEB_PREVIEW_HOST_PORT must be within 1024..65535\n' >&2
    return 1
  fi
  desired="v1:$host:$port"
  local -a proxy_args=(connect=tcp:127.0.0.1:8765 bind=host)
  if [ "${YARD_KIND:-container}" = vm ]; then
    guest="$(jq -r '(.expanded_devices // .devices).eth0["ipv4.address"] // ""' <<<"$snapshot")" || return 1
    subyard_preview_private_address "$guest" || {
      printf 'Preview: VM has no private primary IPv4 pin; rerun init\n' >&2; return 1;
    }
    if [ "${VM_PIN_IPV4:-0}" = 1 ] && ! jq -e '.devices.eth0 == null' <<<"$snapshot" >/dev/null; then
      printf 'Preview: pinned VM must use its profile-owned primary NIC\n' >&2
      return 1
    fi
    desired="v2:$host:$port:$guest"
    proxy_args=("connect=tcp:$guest:8765" bind=host nat=true)
  fi
  if [ "$device" != null ] && { [ "$current_host" != "$host" ] || [ "$current_port" != "$port" ] || [ "$current_guest" != "$guest" ]; }; then
    incus config device remove "$YARD_INSTANCE_NAME" subyard-preview "${PROJ[@]}" >/dev/null || return 1
    device=null
  fi
  if [ "$device" = null ]; then
    if [ -n "$guest" ]; then
      local listeners address
      listeners="$(ss -H -ltn "sport = :$port")" || return 1
      while read -r address; do
        case "$address" in "$host:$port" | "0.0.0.0:$port" | "*:$port" | "[::]:$port")
          printf 'Preview: cannot publish endpoint; check WEB_PREVIEW_HOST_PORT for a host port collision\n' >&2
          return 1 ;;
        esac
      done < <(awk '{print $4}' <<<"$listeners")
    fi
    marker="${desired%%:*}:pending:${desired#*:}"
    incus config set "$YARD_INSTANCE_NAME" "$key" "$marker" "${PROJ[@]}" >/dev/null || return 1
    incus config device add "$YARD_INSTANCE_NAME" subyard-preview proxy "${PROJ[@]}" \
      "listen=tcp:$host:$port" "${proxy_args[@]}" >/dev/null || {
        printf 'Preview: cannot publish endpoint; check WEB_PREVIEW_HOST_PORT for a host port collision\n' >&2
        return 1
      }
  fi
  [ "$marker" = "$desired" ] ||
    incus config set "$YARD_INSTANCE_NAME" "$key" "$desired" "${PROJ[@]}" >/dev/null || return 1
}
