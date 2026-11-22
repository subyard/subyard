#!/usr/bin/env bash
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
profile="$(cd "$here/../.." && pwd)"
root="$(cd "$profile/../../.." && pwd)"
# shellcheck source=scripts/lib/engine-context.sh
. "$root/scripts/lib/engine-context.sh"
subyard_require_engine_context
# shellcheck source=scripts/lib/ui.sh
. "$root/scripts/lib/ui.sh"
# shellcheck source=scripts/lib/host.sh
. "$root/scripts/lib/host.sh"
# shellcheck source=scripts/lib-service.sh
. "$root/scripts/lib-service.sh"
# shellcheck source=config/profiles/herdr/release.env
. "$profile/release.env"

PROJ=(--project "$INCUS_PROJECT")
helper=/usr/local/libexec/subyard/herdr-profile
login=/usr/local/libexec/subyard/herdr-login
unit=subyard-herdr-core.service
native="$profile/bin/herdr-profile"
[ -x "$native" ] || native="$root/.build/profiles/herdr/bin/herdr-profile"
yexec() { incus exec "$YARD_INSTANCE_NAME" "${PROJ[@]}" -- "$@"; }
as_dev() { yexec runuser -u "$DEV_USER" -- "$@"; }
yard_running() {
  command -v incus >/dev/null &&
    [ "$(power_state "$INCUS_PROJECT" "$YARD_INSTANCE_NAME")" = RUNNING ]
}
allowed() {
  [ "$YARD_KIND" = container ] || die 'Herdr supports container yards only'
  [ "$NESTED_E2E_VMS" != 1 ] || die 'Herdr is unavailable in nested E2E mode'
}
observe() {
  if yexec test -x "$helper"; then
    as_dev "$helper" observe
  else
    printf 'null\n'
  fi
}
running() {
  local result
  result=$(observe) || return 1
  jq -e '. != null' <<< "$result" >/dev/null
}
enabled() { yexec systemctl is-enabled --quiet "$unit"; }
core_release() {
  local arch digest
  arch=$(yexec uname -m)
  case "$arch" in
    x86_64) digest="$HERDR_X86_64_SHA256" ;;
    aarch64) digest="$HERDR_AARCH64_SHA256" ;;
    *) die 'Herdr Core has no release for this guest architecture' ;;
  esac
  as_dev bash -s -- "$HERDR_VERSION" "$HERDR_GENERATION" "$digest" "$1" < "$here/install-core.sh"
}
digest() {
  [ -x "$native" ] || die 'Herdr profile helper is missing from the built runtime'
  sha256sum "$native" "$here/login.sh" "$here/handler.sh" "$here/install-core.sh" "$profile/release.env" |
    awk '{print $1}' | sha256sum | cut -d ' ' -f1
}
guest_file() {
  yexec sh -eu -c '
    path="$1"; mode="$2"
    [ ! -L "$path" ] && { [ ! -e "$path" ] || [ -f "$path" ]; }
    stage=$(mktemp "${path}.XXXXXX")
    trap '\''rm -f -- "$stage"'\'' EXIT
    cat > "$stage"
    chmod "$mode" "$stage"
    cmp -s "$stage" "$path" || mv -fT -- "$stage" "$path"
  ' herdr "$1" "$2"
}
install_assets() {
  [ -x "$native" ] || die 'Herdr profile helper is missing from the built runtime'
  yexec install -d -m 0755 /usr/local/libexec/subyard
  guest_file "$helper" 0755 < "$native"
  guest_file "$login" 0755 < "$here/login.sh"
  guest_file "/etc/systemd/system/$unit" 0644 <<EOF
[Unit]
Description=Start Herdr Core in the developer login environment
Wants=user@$DEV_UID.service
After=user@$DEV_UID.service

[Service]
Type=oneshot
User=$DEV_USER
ExecStart=$login server start

[Install]
WantedBy=multi-user.target
EOF
  yexec systemctl daemon-reload
}
wait_core() {
  local old="${1:-0}" result attempt
  for ((attempt=0; attempt<20; attempt++)); do
    result=$(observe) || result=null
    if jq -e --argjson old "$old" '. != null and .PID != $old' <<< "$result" >/dev/null; then return 0; fi
    sleep 0.5
  done
  die 'Herdr Core did not become ready'
}
up() {
  install_assets
  core_release minimum
  yexec loginctl enable-linger "$DEV_USER"
  yexec systemctl enable --quiet "$unit"
  if ! running; then yexec systemctl start "$unit"; fi
  wait_core
  digest | guest_file "$helper.sha256" 0644
}
up_ready() {
  yard_running && enabled && core_release check && running &&
    [ "$(yexec loginctl show-user "$DEV_USER" -p Linger --value)" = yes ] &&
    [ "$(yexec cat "$helper.sha256" 2>/dev/null)" = "$(digest)" ]
}

runtime() {
  local mode="${1:-}" state=absent actual='' desired='' installed=false
  [ "$#" -gt 0 ] || die 'Herdr runtime mode is required'
  shift
  if [ "$YARD_KIND" = container ] && [ "$NESTED_E2E_VMS" != 1 ] && yard_running; then
    if yexec test -x "$helper"; then
      installed=true
      desired=$(digest)
      actual=$(yexec cat "$helper.sha256" 2>/dev/null) || actual=$(printf '' | sha256sum | cut -d ' ' -f1)
      state=stale
      if [ "$actual" = "$desired" ]; then
        if ! enabled || core_release check; then state=current; fi
      fi
    fi
  elif [ "$YARD_KIND" = container ] && [ "$NESTED_E2E_VMS" != 1 ]; then
    state=deferred
  fi
  if [ "$mode" = apply ]; then
    [ "$#" -eq 3 ] && [ "$installed" = true ] && [ "$actual" = "$2" ] && [ "$desired" = "$3" ] || die 'Herdr runtime plan is stale'
    if enabled; then
      up
    else
      install_assets
      printf '%s\n' "$desired" | guest_file "$helper.sha256" 0644
    fi
    actual="$desired"
    state=current
  elif [ "$mode" != observe ] || [ "$#" -ne 0 ]; then
    die 'invalid Herdr runtime observation arguments'
  fi
  jq -cn --arg state "$state" --arg actual "$actual" --arg desired "$desired" \
    '{state:$state} + (if $actual == "" then {} else {actual:$actual,desired:$desired} end)'
}

usage() {
  cat <<EOF
Usage: ${PROG:-yard} herdr <up|status|is-up|update|restart|logs|down>
  update [--force]  Upgrade to the latest compatible release, or restore the pin
  logs [--follow]   Read Herdr logs
  down              Stop Core and its running terminals
EOF
}

action="${1:-}"
if [ "$action" = _runtime ]; then shift; runtime "$@"; exit; fi
case "$action" in ''|--help|-h|help) usage; exit ;; esac
shift
case "$action:$*" in
  update:--force|logs:--follow|up:|status:|is-up:|update:|restart:|logs:|down:) ;;
  *) svc_usage_error 'invalid Herdr command or arguments' ;;
esac

assessment() {
  local mode="$1" ready=false consequence desired target old result count
  target="incus:$INCUS_PROJECT/$YARD_INSTANCE_NAME/herdr"
  case "$action" in
    status|is-up|logs)
      jq -cn --arg action "$action" '{schema:"yard.resource-action-assessment.v1",action:$action,changed:false,consequences:[]}'
      return ;;
    up)
      allowed
      if up_ready; then ready=true; fi
      desired='core ready and kick enabled'
      consequence='install or refresh Herdr Core and enable its startup kick' ;;
    update|restart)
      allowed
      svc_require_yard_running
      running || die "Herdr Core is stopped; run '${PROG:-yard} herdr up'"
      if [ "$mode" = verify ]; then ready=true; fi
      desired='core ready'
      if [ "$action" = restart ]; then
        consequence='restart Herdr Core in a clean login environment, keeping its terminals'
      elif [ "$#" -eq 2 ]; then
        consequence='restore the pinned Herdr Core release, keeping its terminals'
      else
        consequence='update to the latest compatible Herdr Core, keeping its terminals'
      fi ;;
    down)
      svc_require_yard_running
      result=$(observe)
      if [ "$result" = null ] && ! enabled; then ready=true; fi
      desired='core stopped and kick disabled'
      count=$(jq -r '.Live // 0' <<< "$result")
      consequence="stop all Herdr terminals, including arrivals; observed live terminals: $count"
      if [ -n "${SUBYARD_RESOURCE_STEPS:-}" ]; then
        old=$(jq -er '.[0].consequence' <<< "$SUBYARD_RESOURCE_STEPS")
        consequence="$old"
      fi ;;
  esac
  jq -cn --arg action "$action" --arg target "$target" --arg desired "$desired" \
    --arg consequence "$consequence" --argjson ready "$ready" '
    {schema:"yard.resource-action-assessment.v2",action:$action,changed:($ready|not),
     consequences:(if $ready then [] else [$consequence] end),steps:[{id:"core",target:$target,
     observed:(if $ready then $desired else "unknown" end),desired:$desired,
     decision:(if $ready then "skip" else "conditional" end),
     preconditions:["selected yard and Herdr runtime"],verify:$desired,consequence:$consequence}]}'
}

case "${SUBYARD_RESOURCE_MODE:-}" in
  prepare|verify) assessment "$SUBYARD_RESOURCE_MODE" "$@" ;;
  apply)
    [ "${SUBYARD_RESOURCE_ACTION:-}" = "$action" ] && [ -n "${SUBYARD_OPERATION_ID:-}" ] || die 'typed resource apply required'
    case "$action" in
      up) allowed; svc_require_yard_running; up ;;
      is-up) yard_running && running ;;
      status)
        if ! yard_running; then printf 'Herdr: yard is stopped or unavailable\n'; exit; fi
        if ! yexec test -x "$helper"; then printf 'Herdr: not installed\n'; exit; fi
        printf 'Minimum Core release: %s\n' "$HERDR_VERSION"
        as_dev env "SUBYARD_HERDR_MINIMUM=$HERDR_VERSION" "$helper" status ;;
      logs) svc_require_yard_running; yexec journalctl --no-pager -n 100 -u "$unit" + "_UID=$DEV_UID" "_COMM=herdr-core" "$@" ;;
      update)
        allowed
        running || die 'Herdr Core must be running to update'
        if [ "$#" -eq 1 ]; then core_release force; else core_release latest; fi
        wait_core ;;
      restart)
        allowed
        old=$(observe | jq -er '.PID')
        as_dev "$login" server restart --fresh
        wait_core "$old" ;;
      down)
        if enabled; then yexec systemctl disable --now "$unit"; fi
        if running; then as_dev "$helper" stop; fi
        if running; then die 'Herdr Core is still running or was started by another client'; fi ;;
    esac ;;
  '') [ "$action" = is-up ] && yard_running && running ;;
  *) die 'unsupported Herdr execution mode' ;;
esac
