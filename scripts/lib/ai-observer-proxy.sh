#!/usr/bin/env bash
# One owned loopback route for the selected AI Observer integration. VM guests
# use the existing SSH access path because Incus cannot proxy their loopback.

subyard_ai_observer_proxy() {
  local selected="$1" snapshot marker device port current_port desired
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

  # A pending marker permits recovery after interruption between marker and add.
  current_port="${marker#v1:}"
  current_port="${current_port#pending:}"
  if [ "$device" != null ]; then
    if ! [[ "$marker" =~ ^v1:(pending:)?[1-9][0-9]{3,4}$ ]] ||
      ! jq -e --arg listen "tcp:127.0.0.1:$current_port" '
        . == {type:"proxy",bind:"host",listen:$listen,connect:"tcp:127.0.0.1:8080"}
      ' <<<"$device" >/dev/null; then
      printf 'AI Observer: refusing to replace foreign or divergent ai-observer device\n' >&2
      return 1
    fi
    if [ "$selected" = 1 ] && [ "$current_port" = "$port" ]; then
      if [ "$marker" != "v1:$port" ]; then
        incus config set "$YARD_INSTANCE_NAME" "$key" "v1:$port" "${PROJ[@]}" || return 1
      fi
      return 0
    fi
    incus config device remove "$YARD_INSTANCE_NAME" ai-observer "${PROJ[@]}" >/dev/null || return 1
  fi
  if [ "$selected" = 0 ]; then
    [ -z "$marker" ] || incus config unset "$YARD_INSTANCE_NAME" "$key" "${PROJ[@]}" || return 1
    return 0
  fi
  desired="v1:pending:$port"
  incus config set "$YARD_INSTANCE_NAME" "$key" "$desired" "${PROJ[@]}" || return 1
  incus config device add "$YARD_INSTANCE_NAME" ai-observer proxy "${PROJ[@]}" \
    "listen=tcp:127.0.0.1:$port" connect=tcp:127.0.0.1:8080 bind=host >/dev/null || {
      printf 'AI Observer: cannot publish dashboard; check AI_OBSERVER_HOST_PORT for a host port collision\n' >&2
      return 1
    }
  incus config set "$YARD_INSTANCE_NAME" "$key" "v1:$port" "${PROJ[@]}" || return 1
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
