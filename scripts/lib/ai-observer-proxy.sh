#!/usr/bin/env bash
# One owned Tailscale or loopback route for the selected AI Observer integration. VM guests
# use the existing SSH access path because Incus cannot proxy their loopback.

# Restrict publication to one active Tailscale IPv4; absence retains loopback access.
subyard_ai_observer_tail_address() {
  local address="$1" _first second third fourth
  [[ "$address" =~ ^100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.([0-9]{1,3})\.([0-9]{1,3})$ ]] || return 1
  IFS=. read -r _first second third fourth <<<"$address"
  [ "$second" -ge 64 ] && [ "$second" -le 127 ] &&
    [ "$third" -le 255 ] && [ "$fourth" -le 255 ] &&
    [[ "$third" != 0?* && "$fourth" != 0?* ]]
}

subyard_ai_observer_host() {
  local address
  if address="$(timeout 2 tailscale ip -4 2>/dev/null)" &&
    subyard_ai_observer_tail_address "$address" &&
    ip -j -4 address show up | jq -e --arg address "$address" \
      'any(.[].addr_info[]; .local == $address)' >/dev/null; then
    printf '%s\n' "$address"
  else
    printf '127.0.0.1\n'
  fi
}

subyard_ai_observer_proxy() {
  local selected="$1" snapshot marker device port current_port current_host desired host value version
  local key=user.subyard.ai_observer_proxy
  snapshot="$(incus query "/1.0/instances/$YARD_INSTANCE_NAME?project=$INCUS_PROJECT")" || return 1
  marker="$(jq -r --arg key "$key" '.config[$key] // ""' <<<"$snapshot")" || return 1
  device="$(jq -c '.devices["ai-observer"] // null' <<<"$snapshot")" || return 1
  [ "${YARD_KIND:-container}" != vm ] || selected=0

  if [ "$selected" = 0 ] && [ -z "$marker" ]; then
    return 0
  fi
  port="${AI_OBSERVER_HOST_PORT:-}"
  if [ "$selected" = 1 ] && ! [[ "$port" =~ ^[1-9][0-9]{3,4}$ ]] ; then
    printf 'AI Observer: AI_OBSERVER_HOST_PORT must be an unprivileged port\n' >&2
    return 1
  fi
  if [ "$selected" = 1 ] && { [ "$port" -lt 1024 ] || [ "$port" -gt 65535 ]; }; then
    printf 'AI Observer: AI_OBSERVER_HOST_PORT is outside 1024..65535\n' >&2
    return 1
  fi
  host=127.0.0.1
  [ "$selected" != 1 ] || host="$(subyard_ai_observer_host)" || return 1
  if [ "$selected" = 1 ] && [ -n "${AI_OBSERVER_FRONTEND_URL:-}" ] &&
    [ "$AI_OBSERVER_FRONTEND_URL" != "http://$host:$port" ]; then
    printf 'AI Observer: prepared dashboard origin changed; retry reconciliation\n' >&2
    return 1
  fi
  desired="v1:$port"
  [ "$host" = 127.0.0.1 ] || desired="v2:$host:$port"

  # Accept exact old loopback receipts and pending receipts for interrupted recovery.
  version="${marker%%:*}"
  value="${marker#*:}"
  value="${value#pending:}"
  current_host=127.0.0.1
  current_port="$value"
  if [ "$version" = v2 ]; then
    current_host="${value%:*}"
    current_port="${value##*:}"
  fi
  if [ "$device" != null ]; then
    if ! { [ "$version" = v1 ] || { [ "$version" = v2 ] && subyard_ai_observer_tail_address "$current_host"; }; } ||
      ! [[ "$current_port" =~ ^[1-9][0-9]{3,4}$ ]] ||
      [ "$current_port" -lt 1024 ] || [ "$current_port" -gt 65535 ] ||
      ! jq -e --arg listen "tcp:$current_host:$current_port" '
        . == {type:"proxy",bind:"host",listen:$listen,connect:"tcp:127.0.0.1:8080"}
      ' <<<"$device" >/dev/null; then
      printf 'AI Observer: refusing to replace foreign or divergent ai-observer device\n' >&2
      return 1
    fi
    if [ "$selected" = 1 ] && [ "$current_port" = "$port" ] && [ "$current_host" = "$host" ]; then
      if [ "$marker" != "$desired" ]; then
        incus config set "$YARD_INSTANCE_NAME" "$key" "$desired" "${PROJ[@]}" || return 1
      fi
      return 0
    fi
    incus config device remove "$YARD_INSTANCE_NAME" ai-observer "${PROJ[@]}" >/dev/null || return 1
  fi
  if [ "$selected" = 0 ]; then
    [ -z "$marker" ] || incus config unset "$YARD_INSTANCE_NAME" "$key" "${PROJ[@]}" || return 1
    return 0
  fi
  incus config set "$YARD_INSTANCE_NAME" "$key" "${desired%%:*}:pending:${desired#*:}" "${PROJ[@]}" || return 1
  incus config device add "$YARD_INSTANCE_NAME" ai-observer proxy "${PROJ[@]}" \
    "listen=tcp:$host:$port" connect=tcp:127.0.0.1:8080 bind=host >/dev/null || {
      printf 'AI Observer: cannot publish dashboard; check AI_OBSERVER_HOST_PORT for a host port collision\n' >&2
      return 1
    }
  incus config set "$YARD_INSTANCE_NAME" "$key" "$desired" "${PROJ[@]}" || return 1
}

# Record the guest-side provision identity without asking Incus to unset a
# missing volatile key (Incus 6.0 rejects that otherwise harmless operation).
subyard_ai_observer_provision_marker() {
  local selected="${1:-}" context="${2:-}" current
  local key=user.subyard.ai_observer_provision
  case "$selected" in 0|1) ;; *) return 1 ;; esac
  if [ "$selected" = 1 ] && [[ ! "$context" =~ ^[0-9a-f]{64}$ ]]; then
    return 1
  fi
  current="$(incus config get "$YARD_INSTANCE_NAME" "$key" "${PROJ[@]}")" || return 1
  if [ "$selected" = 1 ]; then
    [ "$current" = "$context" ] \
      || incus config set "$YARD_INSTANCE_NAME" "$key" "$context" "${PROJ[@]}" \
      || return 1
  else
    [ -z "$current" ] \
      || incus config unset "$YARD_INSTANCE_NAME" "$key" "${PROJ[@]}" \
      || return 1
  fi
}
