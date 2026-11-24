#!/usr/bin/env bash

parse_pair() {
  pair_label=client
  pair_value=''
  if [ "$#" -eq 1 ] && [ "$1" = --list ]; then pair_operation=list; return; fi
  if [ "$#" -eq 2 ] && [ "$1" = --revoke ]; then pair_operation=revoke; pair_label="$2"; return; fi
  if { [ "$#" -eq 2 ] || [ "$#" -eq 4 ]; } && [ "$1" = --key ]; then
    if [ "$#" -eq 4 ]; then
      [ "$3" = --name ] || svc_usage_error 'expected --name after the public key'
      pair_label="$4"
    fi
    pair_operation=add
    pair_value="$2"
    return
  fi
  svc_usage_error 'use pair --key PUBKEY [--name LABEL], --list or --revoke FINGERPRINT'
}

owner_fingerprint() {
  ssh-keygen -E sha256 -lf "$SUBYARD_HOME/ssh/id_ed25519.pub" | awk 'NR == 1 {print $2}'
}

key_operation() {
  local phase="$1" operation="$2" owner
  owner=$(owner_fingerprint) || die 'could not read the managed owner public key fingerprint'
  if [ "$operation" = list ]; then
    as_dev "${helper:?}" keys "$phase" list "$owner"
  else
    printf '%s' "$pair_value" | as_dev "${helper:?}" keys "$phase" "$operation" "$owner" "$pair_label"
  fi
}

write_key() (
  # Init uses this same lock while it installs the owner SSH key.
  path="$SUBYARD_CONFIG_HOME/generated/integration-locks/${YARD_NAME:-default}.lock"
  if [ ! -L "$path" ] && [ -f "$path" ] && [ "$(stat -c '%u:%h' "$path")" = "$(id -u):1" ]; then
    chmod 0600 "$path" 2>/dev/null || true
  fi
  [ ! -L "$path" ] && [ -f "$path" ] && [ "$(stat -c '%u:%a:%h' "$path")" = "$(id -u):600:1" ] ||
    die 'Herdr could not verify the yard operation lock; reconcile SSH with yard init'
  exec {pair_lock}< "$path"
  flock -w 15 "$pair_lock" || die 'another yard operation is reconciling SSH'
  key_operation apply "$1"
)

pair_assessment() {
  local local_action result changed fingerprint consequence=''
  allowed
  svc_require_yard_running
  if [ "$pair_operation" = add ]; then
    route_ready || die 'Herdr SSH ingress is not ready. Run yard herdr up.'
  fi
  if [ "$pair_operation" = list ]; then
    local_action=pair-list
    changed=false
  else
    result=$(key_operation prepare "$pair_operation")
    fingerprint=$(jq -er .digest <<< "$result")
    changed=$(jq -r .changed <<< "$result")
    local_action="pair-$pair_operation"
    if [ "$pair_operation" = add ]; then
      if [ "$changed" = false ]; then local_action=pair-existing; fi
      consequence="authorize Herdr public key fingerprint:$fingerprint for the development user"
    else
      consequence="revoke Herdr public key fingerprint:$fingerprint for future logins"
    fi
  fi
  jq -cn --arg action "$local_action" --argjson changed "$changed" --arg consequence "$consequence" \
    '{schema:"yard.resource-action-assessment.v1",action:$action,changed:$changed,
     consequences:(if $changed then [$consequence] else [] end)}'
}

pair_apply() {
  allowed
  svc_require_yard_running
  case "$SUBYARD_RESOURCE_ACTION:$pair_operation" in
    pair-list:list) key_operation apply list; return ;;
    pair-add:add|pair-existing:add)
      route_ready || die 'Herdr SSH ingress is not ready. Run yard herdr up.'
      if [ "$SUBYARD_RESOURCE_ACTION" = pair-existing ]; then
        key_operation apply existing >/dev/null
      else
        write_key add >/dev/null
      fi ;;
    pair-revoke:revoke) write_key revoke >/dev/null; return ;;
    *) die 'invalid assessed Herdr key operation' ;;
  esac
  local fingerprint
  fingerprint=$(yexec ssh-keygen -E sha256 -lf /etc/ssh/ssh_host_ed25519_key.pub | awk 'NR == 1 {print $2}')
  printf 'User: %s\nHost: %s\nPort: %s\nEd25519 host key: %s\n' "$DEV_USER" "$HERDR_ADVERTISE_HOST" "$HERDR_HOST_PORT" "$fingerprint"
  printf 'hc machine add %s@%s:%s\n' "$DEV_USER" "$HERDR_ADVERTISE_HOST" "$HERDR_HOST_PORT"
}
