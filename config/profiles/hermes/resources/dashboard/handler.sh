#!/usr/bin/env bash
# Route an already-running guest loopback endpoint to one owner-host Tailscale address.
set -euo pipefail

RESOURCE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUBYARD_ROOT="$(cd "$RESOURCE_DIR/../../../../.." && pwd)"
SCRIPT_DIR="$SUBYARD_ROOT/scripts"
# shellcheck source=scripts/lib/engine-context.sh
. "$SCRIPT_DIR/lib/engine-context.sh"
subyard_require_engine_context
# shellcheck source=scripts/lib/ui.sh
. "$SCRIPT_DIR/lib/ui.sh"
# shellcheck source=scripts/lib/host.sh
. "$SCRIPT_DIR/lib/host.sh"
# shellcheck source=scripts/lib-service.sh
. "$SCRIPT_DIR/lib-service.sh"

HERMES_DEVICE=hermes-dashboard
HERMES_GUEST_PORT=9119
HERMES_OWNERSHIP_KEY=user.subyard.resource.hermes-dashboard
HERMES_OWNERSHIP_VERSION=v1

device_exists() {
  incus config device list "$YARD_INSTANCE_NAME" "${PROJ[@]}" 2>/dev/null |
    grep -qx "$HERMES_DEVICE"
}

device_value() {
  incus config device get "$YARD_INSTANCE_NAME" "$HERMES_DEVICE" "$1" \
    "${PROJ[@]}" 2>/dev/null
}

require_runtime_settings() {
  [ -n "${HERMES_DASHBOARD_ADVERTISE_HOST:-}" ] \
    || die "HERMES_DASHBOARD_ADVERTISE_HOST is required (the owner-host Tailscale hostname or IPv4)"
  [ -n "${HERMES_DASHBOARD_HOST_PORT:-}" ] \
    || die "HERMES_DASHBOARD_HOST_PORT is required (set a unique per-yard port)"
  [[ "$HERMES_DASHBOARD_ADVERTISE_HOST" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]] \
    || die "HERMES_DASHBOARD_ADVERTISE_HOST must be a hostname or IPv4 address without scheme, path or port"
  case "$HERMES_DASHBOARD_HOST_PORT" in
    *[!0-9]*|'') die "HERMES_DASHBOARD_HOST_PORT must be a decimal integer" ;;
  esac
  [ "$HERMES_DASHBOARD_HOST_PORT" -ge 1 ] && [ "$HERMES_DASHBOARD_HOST_PORT" -le 65535 ] \
    || die "HERMES_DASHBOARD_HOST_PORT must be in range 1..65535"
  local reserved
  for reserved in "${SSH_PORT:-}" "${ADB_PROXY_PORT:-}" "${ADB_CONSOLE_PROXY_PORT:-}" \
    "${ORCA_HOST_PORT:-}"; do
    [ -z "$reserved" ] || [ "$HERMES_DASHBOARD_HOST_PORT" != "$reserved" ] \
      || die "HERMES_DASHBOARD_HOST_PORT collides with another owner-host route"
  done
}

resolve_owner_address() {
  local candidate
  local -a tailscale_ips=() resolved_ips=()
  command -v tailscale >/dev/null 2>&1 \
    || die "tailscale CLI is required on the owner host for Hermes route publication"
  mapfile -t tailscale_ips < <(tailscale ip -4 2>/dev/null | sed '/^$/d')
  [ "${#tailscale_ips[@]}" -gt 0 ] || die "the owner host has no active Tailscale IPv4 address"
  mapfile -t resolved_ips < <(
    getent ahostsv4 "$HERMES_DASHBOARD_ADVERTISE_HOST" 2>/dev/null |
      awk '{print $1}' | sort -u
  )
  [ "${#resolved_ips[@]}" -eq 1 ] \
    || die "HERMES_DASHBOARD_ADVERTISE_HOST must have exactly one IPv4 DNS answer"
  candidate="${resolved_ips[0]}"
  printf '%s\n' "${tailscale_ips[@]}" | grep -Fqx "$candidate" \
    || die "HERMES_DASHBOARD_ADVERTISE_HOST is not an active Tailscale IPv4 address"
  ip -4 -brief address show scope global |
    awk '{for (field = 3; field <= NF; field++) {sub(/\/.*/, "", $field); print $field}}' |
    grep -Fqx "$candidate" \
    || die "HERMES_DASHBOARD_ADVERTISE_HOST is not active on the owner host"
  HERMES_OWNER_IP="$candidate"
}

ownership_marker() {
  incus config get "$YARD_INSTANCE_NAME" "$HERMES_OWNERSHIP_KEY" \
    "${PROJ[@]}" 2>/dev/null
}

device_keys() {
  incus config device show "$YARD_INSTANCE_NAME" "${PROJ[@]}" 2>/dev/null |
    awk -v device="$HERMES_DEVICE" '
      $0 == device ":" { inside = 1; next }
      inside && /^[^[:space:]]/ { exit }
      inside && /^  [A-Za-z0-9_.-]+:/ {
        key = $0
        sub(/^  /, "", key)
        sub(/:.*/, "", key)
        print key
      }
    '
}

fingerprint_entries() {
  local entry key value key_output record
  local -a entries=()
  [ "$#" -gt 0 ] || return 1
  key_output="$(printf '%s\n' "$@" | LC_ALL=C sort)" || return 1
  mapfile -t entries <<<"$key_output"
  record="$HERMES_OWNERSHIP_VERSION"$'\n'
  for entry in "${entries[@]}"; do
    key="${entry%%=*}"
    value="${entry#*=}"
    [[ "$key" =~ ^[A-Za-z0-9_.-]+$ ]] || return 1
    [[ "$value" != *$'\n'* ]] || return 1
    record+="$key=$value"$'\n'
  done
  printf '%s' "$record" | sha256sum | awk '{print $1}'
}

device_fingerprint() {
  local key key_output value
  local -a entries=()
  key_output="$(device_keys | LC_ALL=C sort)" || return 1
  [ -n "$key_output" ] || return 1
  while IFS= read -r key; do
    value="$(device_value "$key")" || return 1
    [[ "$value" != *$'\n'* ]] || return 1
    entries+=("$key=$value")
  done <<<"$key_output"
  fingerprint_entries "${entries[@]}"
}

desired_route_fingerprint() {
  fingerprint_entries \
    bind=host \
    "connect=tcp:127.0.0.1:$HERMES_GUEST_PORT" \
    "listen=tcp:$HERMES_OWNER_IP:$HERMES_DASHBOARD_HOST_PORT" \
    type=proxy
}

device_has_exact_shape() {
  local keys
  keys="$(device_keys | LC_ALL=C sort)" || return 1
  [ "$keys" = $'bind\nconnect\nlisten\ntype' ]
}

route_configuration_matches() {
  device_exists &&
    device_has_exact_shape &&
    [ "$(device_value type)" = proxy ] &&
    [ "$(device_value listen)" = "tcp:$HERMES_OWNER_IP:$HERMES_DASHBOARD_HOST_PORT" ] &&
    [ "$(device_value connect)" = "tcp:127.0.0.1:$HERMES_GUEST_PORT" ] &&
    [ "$(device_value bind)" = host ]
}

device_is_authorized() {
  local marker fingerprint
  device_exists || return 1
  marker="$(ownership_marker)" || return 1
  fingerprint="$(device_fingerprint)" || return 1
  [ "$marker" = "$HERMES_OWNERSHIP_VERSION:$fingerprint" ] ||
    [ "$marker" = "$HERMES_OWNERSHIP_VERSION:pending:$fingerprint" ]
}

device_is_active_owned() {
  local marker fingerprint
  device_exists || return 1
  marker="$(ownership_marker)" || return 1
  fingerprint="$(device_fingerprint)" || return 1
  [ "$marker" = "$HERMES_OWNERSHIP_VERSION:$fingerprint" ]
}

route_matches() {
  route_configuration_matches && device_is_active_owned
}

set_route_ownership() {
  local marker="$1" observed
  incus config set "$YARD_INSTANCE_NAME" "$HERMES_OWNERSHIP_KEY" "$marker" \
    "${PROJ[@]}" >/dev/null || return 1
  observed="$(ownership_marker)" || return 1
  [ "$observed" = "$marker" ]
}

clear_route_ownership() {
  local marker
  marker="$(ownership_marker)" || return 1
  [ -n "$marker" ] || return 0
  incus config unset "$YARD_INSTANCE_NAME" "$HERMES_OWNERSHIP_KEY" \
    "${PROJ[@]}" >/dev/null || return 1
  marker="$(ownership_marker)" || return 1
  [ -z "$marker" ]
}

remove_owned_route() {
  if ! device_exists; then
    clear_route_ownership || return 1
    return 0
  fi
  device_is_authorized || die "refusing to remove foreign or unowned device '$HERMES_DEVICE'"
  incus config device remove "$YARD_INSTANCE_NAME" "$HERMES_DEVICE" \
    "${PROJ[@]}" >/dev/null || return 1
  device_exists && return 1
  clear_route_ownership || return 1
}

refuse_port_collision() {
  route_matches && return 0
  if ss -Hltn "sport = :$HERMES_DASHBOARD_HOST_PORT" |
    awk '{print $4}' |
    grep -Eq "^($HERMES_OWNER_IP|0\\.0\\.0\\.0|\\*):$HERMES_DASHBOARD_HOST_PORT$"; then
    die "owner endpoint $HERMES_OWNER_IP:$HERMES_DASHBOARD_HOST_PORT is already in use"
  fi
}

guest_endpoint_ready() {
  local listeners
  listeners="$(yexec ss -Hltn "sport = :$HERMES_GUEST_PORT" 2>/dev/null)" || return 1
  printf '%s\n' "$listeners" | awk -v port="$HERMES_GUEST_PORT" '
    $4 == "127.0.0.1:" port { found = 1; next }
    NF { foreign = 1 }
    END { exit !(found && !foreign) }
  '
}

owner_endpoint_ready() {
  timeout --foreground 3 bash -c 'exec 3<>"/dev/tcp/$1/$2"' _ \
    "$HERMES_OWNER_IP" "$HERMES_DASHBOARD_HOST_PORT" >/dev/null 2>&1
}

wait_owner_endpoint() {
  local _
  for _ in $(seq 1 15); do
    owner_endpoint_ready && return 0
    sleep 1
  done
  return 1
}

ensure_route() {
  local active_marker fingerprint marker pending_marker
  fingerprint="$(desired_route_fingerprint)" || return 1
  active_marker="$HERMES_OWNERSHIP_VERSION:$fingerprint"
  pending_marker="$HERMES_OWNERSHIP_VERSION:pending:$fingerprint"
  if device_exists; then
    marker="$(ownership_marker)" || return 1
    if route_configuration_matches && [ "$marker" = "$active_marker" ]; then
      return 0
    fi
    if route_configuration_matches && [ "$marker" = "$pending_marker" ]; then
      set_route_ownership "$active_marker" || return 1
      return 0
    fi
    device_is_authorized \
      || die "device '$HERMES_DEVICE' exists without matching ownership metadata"
    remove_owned_route || return 1
  else
    clear_route_ownership || return 1
  fi
  set_route_ownership "$pending_marker" || return 1
  if ! incus config device add "$YARD_INSTANCE_NAME" "$HERMES_DEVICE" proxy "${PROJ[@]}" \
    "listen=tcp:$HERMES_OWNER_IP:$HERMES_DASHBOARD_HOST_PORT" \
    "connect=tcp:127.0.0.1:$HERMES_GUEST_PORT" bind=host >/dev/null; then
    if device_exists; then
      device_is_authorized && remove_owned_route || return 1
    else
      clear_route_ownership || return 1
    fi
    return 1
  fi
  route_configuration_matches || return 1
  [ "$(ownership_marker)" = "$pending_marker" ] || return 1
  set_route_ownership "$active_marker" || return 1
}

cmd_up() {
  require_runtime_settings
  resolve_owner_address
  refuse_port_collision
  guest_endpoint_ready \
    || die "guest loopback endpoint 127.0.0.1:$HERMES_GUEST_PORT is not listening"
  if device_exists && ! device_is_authorized; then
    die "device '$HERMES_DEVICE' exists without matching ownership metadata"
  fi
  if route_matches && owner_endpoint_ready; then
    ok "Hermes browser route is already available at $HERMES_DASHBOARD_ADVERTISE_HOST:$HERMES_DASHBOARD_HOST_PORT"
    return 0
  fi
  if ! ensure_route; then
    if device_exists && device_is_authorized && ! remove_owned_route; then
      die "Hermes route publication failed and rollback is incomplete; retry dashboard down"
    fi
    die "Hermes route publication failed"
  fi
  if ! wait_owner_endpoint; then
    remove_owned_route \
      || die "Hermes endpoint failed readiness and rollback is incomplete; retry dashboard down"
    die "Hermes owner endpoint failed readiness; the route was removed"
  fi
  ok "Hermes browser route is available at $HERMES_DASHBOARD_ADVERTISE_HOST:$HERMES_DASHBOARD_HOST_PORT"
}

cmd_down() {
  local changed=0
  if device_exists; then
    device_is_authorized || die "refusing to remove foreign or unowned device '$HERMES_DEVICE'"
    changed=1
  elif [ -n "$(ownership_marker)" ]; then
    changed=1
  fi
  remove_owned_route
  if [ "$changed" -eq 1 ]; then
    ok "Hermes browser route removed"
  else
    ok "Hermes browser route already absent"
  fi
}

cmd_status() {
  require_runtime_settings
  resolve_owner_address
  if guest_endpoint_ready; then
    ok "guest loopback endpoint is listening"
  else
    warn "guest loopback endpoint is not listening"
  fi
  if route_matches; then
    ok "owner route: $(device_value listen) -> $(device_value connect)"
  else
    warn "owner route absent or divergent"
  fi
}

emit_resource_assessment() {
  local action="$1" changed="$2" separator=""
  shift 2
  printf '{"schema":"yard.resource-action-assessment.v1","action":"%s","changed":%s,"consequences":[' \
    "$action" "$changed"
  local consequence
  for consequence in "$@"; do
    printf '%s"%s"' "$separator" "$consequence"
    separator=,
  done
  printf ']}\n'
}

require_no_resource_arguments() {
  local verb="$1"
  shift
  [ "$#" -eq 0 ] || die "'$verb' does not accept additional arguments"
}

require_resource_apply() {
  local expected="$1"
  [ "${SUBYARD_RESOURCE_MODE:-}" = apply ] || die "resource apply mode is required"
  [ "${SUBYARD_RESOURCE_ACTION:-}" = "$expected" ] \
    || die "prepared resource action mismatch (expected '$expected')"
  [ -n "${SUBYARD_OPERATION_ID:-}" ] || die "resource apply operation ID is required"
}

prepare_resource() {
  local verb="$1" changed=false
  shift
  require_no_resource_arguments "$verb" "$@"
  case "$verb" in
    up)
      svc_require_yard_running
      require_runtime_settings
      resolve_owner_address
      if device_exists && ! device_is_authorized; then
        die "device '$HERMES_DEVICE' exists without matching ownership metadata"
      fi
      refuse_port_collision
      guest_endpoint_ready \
        || die "guest loopback endpoint 127.0.0.1:$HERMES_GUEST_PORT is not listening"
      route_matches || changed=true
      if [ "$changed" = false ]; then
        owner_endpoint_ready || changed=true
      fi
      if [ "$changed" = true ]; then
        emit_resource_assessment up true \
          "publish one owner-host Tailscale endpoint to the guest loopback endpoint"
      else
        emit_resource_assessment up false
      fi
      ;;
    down)
      svc_require_yard_running
      if device_exists; then
        device_is_authorized || die "refusing to remove foreign or unowned device '$HERMES_DEVICE'"
        emit_resource_assessment down true "remove the owned owner-host route"
      elif [ -n "$(ownership_marker)" ]; then
        emit_resource_assessment down true "remove stale owner-host route ownership metadata"
      else
        emit_resource_assessment down false
      fi
      ;;
    is-up|status) emit_resource_assessment "$verb" false ;;
    *) die "unknown Hermes dashboard resource verb '$verb'" ;;
  esac
}

cmd_is_up() {
  incus info "$YARD_INSTANCE_NAME" "${PROJ[@]}" >/dev/null 2>&1 || return 1
  require_runtime_settings
  resolve_owner_address
  guest_endpoint_ready && route_matches && owner_endpoint_ready
}

sub="${1:-}"
shift || true
case "${SUBYARD_RESOURCE_MODE:-}" in
  prepare)
    [ -n "$sub" ] || die "resource verb is required"
    prepare_resource "$sub" "$@"
    ;;
  apply)
    case "$sub" in up|is-up|status|down) ;; *) die "unknown Hermes dashboard apply verb '$sub'" ;; esac
    require_no_resource_arguments "$sub" "$@"
    require_resource_apply "$sub"
    if [ "$sub" = is-up ]; then cmd_is_up; exit $?; fi
    svc_require_yard_running
    case "$sub" in
      up) cmd_up ;;
      status) cmd_status ;;
      down) cmd_down ;;
    esac
    ;;
  '')
    case "$sub" in
      is-up) require_no_resource_arguments is-up "$@"; cmd_is_up ;;
      -h|--help|help|'') printf 'Usage: %s dashboard <up|is-up|status|down>\n' "${PROG:-yard}" ;;
      *) die "typed resource dispatcher required for 'yard dashboard $sub'" ;;
    esac
    ;;
  *) die "unknown resource execution mode '${SUBYARD_RESOURCE_MODE:-}'" ;;
esac
