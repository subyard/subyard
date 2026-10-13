#!/usr/bin/env bash
# Owned AI Observer route and pending-recovery receipts.
# shellcheck source=scripts/lib/owner-address.sh
. "${SUBYARD_REPOSITORY_ROOT:?}/scripts/lib/owner-address.sh"

subyard_ai_observer_proxy_owned() {
  local marker="$1" device="$2" version value host=127.0.0.1 port
  version="${marker%%:*}"; value="${marker#*:}"; value="${value#pending:}"; port="$value"
  if [ "$version" = v2 ]; then host="${value%:*}"; port="${value##*:}"; fi
  { [ "$version" = v1 ] || { [ "$version" = v2 ] && subyard_owner_tail_address "$host"; }; } &&
    [[ "$port" =~ ^[1-9][0-9]{3,4}$ ]] && [ "$port" -ge 1024 ] && [ "$port" -le 65535 ] &&
    jq -e --arg listen "tcp:$host:$port" '
      . == {type:"proxy",bind:"host",listen:$listen,connect:"tcp:127.0.0.1:8080"}
    ' <<<"$device" >/dev/null
}

subyard_ai_observer_proxy_cancel() {
  local marker="$1" device="$2" deferred="$3" snapshot current value
  local key=user.subyard.ai_observer_proxy deferred_key=user.subyard.deferred_publication.ai-observer
  if [ "$device" != null ]; then
    subyard_ai_observer_proxy_owned "$marker" "$device" || {
      printf 'AI Observer: refusing to remove foreign or divergent ai-observer device\n' >&2; return 1;
    }
  fi
  if [ -n "$deferred" ]; then
    value="$(jq -ce --arg marker "$marker" '
      select(keys == ["device","ownership","version"] and .version == 1 and .ownership == $marker) | .device
    ' <<<"$deferred")" || return 1
    subyard_ai_observer_proxy_owned "$marker" "$value" || return 1
  fi
  # Incus metadata updates invalidate any pending restore's ETag. Retain the
  # receipt until a fresh observation proves no just-restored device remains.
  current="$(incus config get "$YARD_INSTANCE_NAME" "$deferred_key" "${PROJ[@]}")" || return 1
  { [ -z "$current" ] || [ "$current" = "$deferred" ]; } || return 1
  [ -z "$current" ] || incus config unset "$YARD_INSTANCE_NAME" "$deferred_key" "${PROJ[@]}" >/dev/null || return 1
  snapshot="$(incus query "/1.0/instances/$YARD_INSTANCE_NAME?project=$INCUS_PROJECT")" || return 1
  [ "$(jq -r --arg key "$key" '.config[$key] // ""' <<<"$snapshot")" = "$marker" ] || return 1
  [ -z "$(jq -r --arg key "$deferred_key" '.config[$key] // ""' <<<"$snapshot")" ] || return 1
  device="$(jq -c '.devices["ai-observer"] // null' <<<"$snapshot")" || return 1
  if [ "$device" != null ]; then
    subyard_ai_observer_proxy_owned "$marker" "$device" || {
      printf 'AI Observer: refusing to remove foreign or divergent ai-observer device\n' >&2; return 1;
    }
    incus config device remove "$YARD_INSTANCE_NAME" ai-observer "${PROJ[@]}" >/dev/null || return 1
  fi
  snapshot="$(incus query "/1.0/instances/$YARD_INSTANCE_NAME?project=$INCUS_PROJECT")" || return 1
  jq -e '.devices["ai-observer"] == null' <<<"$snapshot" >/dev/null || return 1
  [ "$(jq -r --arg key "$key" '.config[$key] // ""' <<<"$snapshot")" = "$marker" ] || return 1
  [ -z "$marker" ] || incus config unset "$YARD_INSTANCE_NAME" "$key" "${PROJ[@]}" || return 1
}

subyard_ai_observer_proxy() {
  local selected="$1" snapshot marker device port current_port current_host desired host value version
  local key=user.subyard.ai_observer_proxy
  snapshot="$(incus query "/1.0/instances/$YARD_INSTANCE_NAME?project=$INCUS_PROJECT")" || return 1
  marker="$(jq -r --arg key "$key" '.config[$key] // ""' <<<"$snapshot")" || return 1
  device="$(jq -c '.devices["ai-observer"] // null' <<<"$snapshot")" || return 1
  [ "${YARD_KIND:-container}" != vm ] || selected=0

  if [ "$selected" = 0 ]; then
    local cancel_deferred
    cancel_deferred="$(jq -r '.config["user.subyard.deferred_publication.ai-observer"] // ""' <<<"$snapshot")" || return 1
    if [ -z "$marker" ] && [ -z "$cancel_deferred" ]; then return 0; fi
    subyard_ai_observer_proxy_cancel "$marker" "$device" "$cancel_deferred"
    return
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
  [ "$selected" != 1 ] || host="$(subyard_owner_host)" || return 1
  local saved_route
  saved_route="$(jq -r '.config["user.subyard.deferred_publication.ai-observer"] // ""' <<<"$snapshot")" || return 1
  if [ "$selected" = 1 ] && [ -n "$saved_route" ]; then
    host="$(jq -r '.device.listen | split(":")[1]' <<<"$saved_route")" || return 1
  fi
  if [ "$selected" = 1 ] && [ -n "${AI_OBSERVER_FRONTEND_URL:-}" ] &&
    [ "$AI_OBSERVER_FRONTEND_URL" != "http://$host:$port" ]; then
    printf 'AI Observer: prepared dashboard origin changed; retry reconciliation\n' >&2
    return 1
  fi
  local deferred_key=user.subyard.deferred_publication.ai-observer deferred
  deferred="$(jq -r --arg key "$deferred_key" '.config[$key] // ""' <<<"$snapshot")" || return 1
  if [ "$selected" = 1 ] && [ -n "$deferred" ]; then
    subyard_owner_tail_address "$host" || return 1
    { [ "$marker" = "v2:$host:$port" ] || [ "$marker" = "v2:pending:$host:$port" ]; } || return 1
    jq -e --arg marker "$marker" --arg listen "tcp:$host:$port" '.version == 1 and .ownership == $marker and .device == {type:"proxy",bind:"host",listen:$listen,connect:"tcp:127.0.0.1:8080"}' <<<"$deferred" >/dev/null || return 1
    return 0
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
    if ! subyard_ai_observer_proxy_owned "$marker" "$device"; then
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
