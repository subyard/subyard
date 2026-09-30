#!/usr/bin/env bash
# Publish container preview only on the owner's active Tailscale IPv4.
# shellcheck source=scripts/lib/ai-observer-proxy.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/ai-observer-proxy.sh"

subyard_preview_endpoint() {
  local snapshot marker device local_device host=127.0.0.1 port="${WEB_PREVIEW_HOST_PORT:-}" desired
  local current_host current_port key=user.subyard.preview_proxy
  snapshot="$(incus query "/1.0/instances/$YARD_INSTANCE_NAME?project=$INCUS_PROJECT")" || return 1
  marker="$(jq -r --arg key "$key" '.config[$key] // ""' <<<"$snapshot")" || return 1
  local_device="$(jq -c '.devices["subyard-preview"] // null' <<<"$snapshot")" || return 1
  device="$(jq -c '(.expanded_devices // .devices)["subyard-preview"] // null' <<<"$snapshot")" || return 1
  [ "${YARD_KIND:-container}" = vm ] || host="$(subyard_ai_observer_host)" || return 1
  if [ -n "$marker" ]; then
    current_host="${marker#v1:}"
    current_host="${current_host#pending:}"
    current_port="${current_host##*:}"
    current_host="${current_host%:*}"
    if [[ "$marker" != v1:* ]] ||
      ! subyard_ai_observer_tail_address "$current_host" ||
      ! [[ "$current_port" =~ ^[1-9][0-9]{3,4}$ ]] ||
      [ "$current_port" -lt 1024 ] || [ "$current_port" -gt 65535 ]; then
      printf 'Preview: invalid route ownership receipt\n' >&2
      return 1
    fi
  fi
  if [ "$device" != null ] || [ "$local_device" != null ]; then
    if [ -z "$marker" ] ||
      ! jq -en --argjson local "$local_device" --argjson effective "$device" '$local == $effective' >/dev/null ||
      ! jq -e --arg listen "tcp:$current_host:$current_port" '
        . == {type:"proxy",bind:"host",listen:$listen,connect:"tcp:127.0.0.1:8765"}
      ' <<<"$device" >/dev/null; then
      printf 'Preview: refusing to replace a foreign or divergent subyard-preview device\n' >&2
      return 1
    fi
  fi
  if [ "$host" = 127.0.0.1 ]; then
    if [ "$device" != null ]; then
      incus config device remove "$YARD_INSTANCE_NAME" subyard-preview "${PROJ[@]}" >/dev/null || return 1
    fi
    [ -z "$marker" ] || incus config unset "$YARD_INSTANCE_NAME" "$key" "${PROJ[@]}" >/dev/null || return 1
    printf '{"version":1,"host":"127.0.0.1","port":8765}\n'
    return 0
  fi
  if ! [[ "$port" =~ ^[1-9][0-9]{3,4}$ ]] || [ "$port" -lt 1024 ] || [ "$port" -gt 65535 ]; then
    printf 'Preview: WEB_PREVIEW_HOST_PORT must be within 1024..65535\n' >&2
    return 1
  fi
  desired="v1:$host:$port"
  if [ "$device" != null ] && { [ "$current_host" != "$host" ] || [ "$current_port" != "$port" ]; }; then
    incus config device remove "$YARD_INSTANCE_NAME" subyard-preview "${PROJ[@]}" >/dev/null || return 1
    device=null
  fi
  if [ "$device" = null ]; then
    marker="v1:pending:$host:$port"
    incus config set "$YARD_INSTANCE_NAME" "$key" "$marker" "${PROJ[@]}" >/dev/null || return 1
    incus config device add "$YARD_INSTANCE_NAME" subyard-preview proxy "${PROJ[@]}" \
      "listen=tcp:$host:$port" connect=tcp:127.0.0.1:8765 bind=host >/dev/null || {
        printf 'Preview: cannot publish endpoint; check WEB_PREVIEW_HOST_PORT for a host port collision\n' >&2
        return 1
      }
  fi
  [ "$marker" = "$desired" ] ||
    incus config set "$YARD_INSTANCE_NAME" "$key" "$desired" "${PROJ[@]}" >/dev/null || return 1
  printf '{"version":1,"host":"%s","port":%s}\n' "$host" "$port"
}
