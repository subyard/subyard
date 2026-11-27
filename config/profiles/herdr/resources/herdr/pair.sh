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
  if [ "$#" -ge 1 ] && [ "$1" = --from-host ]; then
    shift
    pair_operation=host
    pair_selection=(list)
    if [ "$#" -eq 1 ] && [ "$1" = --all ]; then
      pair_selection=(all)
    elif [ "$#" -gt 0 ]; then
      pair_selection=(select)
      for pair_value in "$@"; do
        [[ "$pair_value" =~ ^SHA256:[A-Za-z0-9+/]{43}$ ]] ||
          svc_usage_error 'use pair --from-host [--all | SHA256:FINGERPRINT...]'
        pair_selection+=("$pair_value")
      done
      pair_value=''
    fi
    return
  fi
  svc_usage_error 'use pair --key PUBKEY [--name LABEL], --from-host [--all | SHA256:FINGERPRINT...], --list or --revoke FINGERPRINT'
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

# with_pair_lock runs a key operation under the lock init uses for the owner SSH key.
with_pair_lock() (
  # Init uses this same lock while it installs the owner SSH key.
  path="$SUBYARD_CONFIG_HOME/generated/integration-locks/${YARD_NAME:-default}.lock"
  if [ ! -L "$path" ] && [ -f "$path" ] && [ "$(stat -c '%u:%h' "$path")" = "$(id -u):1" ]; then
    chmod 0600 "$path" 2>/dev/null || true
  fi
  [ ! -L "$path" ] && [ -f "$path" ] && [ "$(stat -c '%u:%a:%h' "$path")" = "$(id -u):600:1" ] ||
    die 'Herdr could not verify the yard operation lock; reconcile SSH with yard init'
  exec {pair_lock}< "$path"
  flock -w 15 "$pair_lock" || die 'another yard operation is reconciling SSH'
  "$@"
)

write_key() { with_pair_lock key_operation apply "$1"; }

# host_pair_report runs the boundary check: the host helper collects the refusals found
# here with its own findings and returns the key report.
host_pair_report() {
  local owner='' list state=unknown report='' refusal=''
  local -a refusals=()
  mapfile -t refusals < <(allowed_refusals)
  if [ "$YARD_KIND" = container ] && [ "$NESTED_E2E_VMS" != 1 ]; then
    if ! yard_running; then
      refusals+=("yard is not running — start it: $(yard_cmd_hint) start")
    elif [ "${pair_selection[0]}" != list ] && ! route_ready; then
      refusals+=('Herdr SSH ingress is not ready. Run yard herdr up.')
    fi
  fi
  owner=$(owner_fingerprint 2>/dev/null) || owner=''
  # The guest helper's bounded list only refines the preview; its key operation decides at apply.
  if [ -n "$owner" ] && yard_running && yexec test -x "${helper:?}" &&
    list=$(as_dev "$helper" keys apply list "$owner" 2>/dev/null); then
    state=known
  fi
  local -a arguments=()
  for refusal in "${refusals[@]}"; do arguments+=(--refusal "$refusal"); done
  arguments+=("${SUBYARD_OPERATOR_HOME:-}" "$owner" "$state" "${pair_selection[@]}")
  if [ ! -x "${native:?}" ]; then
    refusals+=('Herdr profile helper is missing from the built runtime')
  elif [ "$state" = known ]; then
    report=$(printf '%s\n' "$list" | "$native" host-keys "${arguments[@]}") || report=''
  else
    report=$("$native" host-keys "${arguments[@]}" < /dev/null) || report=''
  fi
  if [ -x "$native" ] && ! refusal=$(jq -er .refusal <<< "$report" 2>/dev/null); then
    refusals+=('could not classify the owner account authorized_keys')
    report=''
  fi
  if [ -z "$report" ]; then die "$(printf '%s\n' "${refusals[@]}")"; fi
  [ -z "$refusal" ] || die "$refusal"
  printf '%s\n' "$report"
}

host_pair_assessment() {
  local report
  report=$(host_pair_report)
  jq -c --arg list "${pair_selection[0]}" '
    [.selected[] | select(.status != "granted")] as $new
    | if $list == "list" then {action:"pair-list",changed:false,consequences:[]}
      elif ($new | length) == 0 then {action:"pair-existing",changed:false,consequences:[]}
      else {action:"pair-add",changed:true,consequences:[$new[] |
        "authorize Herdr public key fingerprint:\(.digest) (\(.label)) from the host account for the development user"]}
      end
    | {schema:"yard.resource-action-assessment.v1"} + .' <<< "$report"
}

host_pair_list() {
  local report
  report=$(host_pair_report)
  jq -r '(.keys[] | "\(.fingerprint)  \(.label)  \(.status)"), (.unrecognized[] | "line \(.)  unrecognized")' <<< "$report"
  if jq -e 'any(.keys[]; .status == "new")' <<< "$report" >/dev/null; then
    printf 'Pair new keys with %s herdr pair --from-host --all or --from-host SHA256:FINGERPRINT...\n' "${PROG:-yard}"
  fi
}

# host_pair_grant re-reads the selection. The engine refreshes the assessment after
# confirmation and refuses changed consequences, so only the owner account itself can
# change the host file in the remaining window, and that account administers Incus.
host_pair_grant() {
  local report operation=add key label fingerprint status
  report=$(host_pair_report)
  if [ "$SUBYARD_RESOURCE_ACTION" = pair-existing ]; then
    operation=existing
  elif ! jq -e 'any(.selected[]; .status != "granted")' <<< "$report" >/dev/null; then
    die 'the Herdr grant changed; retry pairing'
  fi
  while IFS=$'\t' read -r -u 3 fingerprint label status key; do
    pair_value="$key"
    pair_label="$label"
    key_operation apply "$operation" >/dev/null
    if [ "$status" = granted ]; then
      printf 'Already paired %s  %s\n' "$fingerprint" "$label"
    else
      printf 'Paired %s  %s\n' "$fingerprint" "$label"
    fi
  done 3< <(jq -r '.selected[] | [.fingerprint, .label, .status, .key] | @tsv' <<< "$report")
}

pair_assessment() {
  local local_action result changed fingerprint consequence=''
  if [ "$pair_operation" = host ]; then host_pair_assessment; return; fi
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
  case "$SUBYARD_RESOURCE_ACTION:$pair_operation" in
    pair-list:host) host_pair_list; return ;;
    pair-add:host|pair-existing:host)
      with_pair_lock host_pair_grant
      pair_endpoint
      return ;;
  esac
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
  pair_endpoint
}

pair_endpoint() {
  local fingerprint
  fingerprint=$(yexec ssh-keygen -E sha256 -lf /etc/ssh/ssh_host_ed25519_key.pub | awk 'NR == 1 {print $2}')
  printf 'User: %s\nHost: %s\nPort: %s\nEd25519 host key: %s\n' "$DEV_USER" "$HERDR_ADVERTISE_HOST" "$HERDR_HOST_PORT" "$fingerprint"
  printf 'hc machine add %s@%s:%s\n' "$DEV_USER" "$HERDR_ADVERTISE_HOST" "$HERDR_HOST_PORT"
}
