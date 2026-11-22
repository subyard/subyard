#!/usr/bin/env bash
# Share a container yard's Herdr Core over tailnet SSH.
# Usage: yard herdr <up|status|is-up|pair|sync|update|restart|logs|down>
#   pair --key PUBKEY [--name LABEL] | --list | --revoke FINGERPRINT
#   sync [--restore]  Reconcile project spaces, optionally restoring closed ones
#   update [--force]  Upgrade or restore the pinned release
#   logs [--follow]   Read Herdr logs
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
project_hook=/usr/local/libexec/subyard/projects-changed.d/herdr
login=/usr/local/libexec/subyard/herdr-login
unit=subyard-herdr-core.service
native="$profile/bin/herdr-profile"
[ -x "$native" ] || native="$root/.build/profiles/herdr/bin/herdr-profile"
yexec() { incus exec "$YARD_INSTANCE_NAME" "${PROJ[@]}" -- "$@"; }
as_dev() { yexec runuser -u "$DEV_USER" -- "$@"; }
guest_ssh() { cat "$root/scripts/lib/sshd.sh" "$profile/guest-ssh.sh" | yexec sh -eu -s -- "$1" "$DEV_USER"; }
# shellcheck source=config/profiles/herdr/resources/herdr/route.sh
. "$here/route.sh"
# shellcheck source=config/profiles/herdr/resources/herdr/pair.sh
. "$here/pair.sh"
yard_running() {
  command -v incus >/dev/null &&
    [ "$(power_state "$INCUS_PROJECT" "$YARD_INSTANCE_NAME")" = RUNNING ]
}
allowed() {
  local -a refusals=()
  [ "$YARD_KIND" = container ] || refusals+=('Herdr supports container yards only')
  [ "$NESTED_E2E_VMS" != 1 ] || refusals+=('Herdr is unavailable in nested E2E mode')
  [ "${#refusals[@]}" -eq 0 ] || die "$(printf '%s\n' "${refusals[@]}")"
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
  as_dev bash -s -- "$HERDR_VERSION" "$HERDR_GENERATION" "$digest" "$1" "$login" < "$here/install-core.sh"
}
digest() {
  [ -x "$native" ] || die 'Herdr profile helper is missing from the built runtime'
  sha256sum "$native" "$here/login.sh" "$here/handler.sh" "$here/install-core.sh" \
    "$here/projects-changed.sh" "$profile/release.env" "$profile/guest-ssh.sh" |
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
    if ! cmp -s "$stage" "$path" || [ "$(stat -c "%u:%a" "$path")" != "0:${mode#0}" ]; then
      mv -fT -- "$stage" "$path"
    fi
  ' herdr "$1" "$2"
}
install_assets() {
  [ -x "$native" ] || die 'Herdr profile helper is missing from the built runtime'
  yexec install -d -m 0755 /usr/local/libexec/subyard/projects-changed.d
  guest_file "$helper" 0755 < "$native"
  guest_file "$login" 0755 < "$here/login.sh"
  guest_file "$project_hook" 0755 < "$here/projects-changed.sh"
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
  local old="${1:-0}" pid attempt
  for ((attempt=0; attempt<20; attempt++)); do
    if pid=$(core_pid) && [ "$pid" != "$old" ]; then return 0; fi
    sleep 0.5
  done
  if [ "$old" = 0 ]; then die 'Herdr Core did not become ready'; fi
  die "Herdr Core did not become ready with a new PID after $old"
}
core_pid() {
  observe | jq -er '.PID | select(type == "number" and . > 0 and . == floor)'
}
up() {
  install_assets
  core_release minimum
  yexec loginctl enable-linger "$DEV_USER"
  yexec systemctl enable --quiet "$unit"
  if ! running; then yexec systemctl start "$unit"; fi
  wait_core
  as_dev "$helper" sync
  digest | guest_file "$helper.sha256" 0644
}
up_ready() {
  yard_running && enabled && core_release check && running &&
    [ "$(yexec loginctl show-user "$DEV_USER" -p Linger --value)" = yes ] &&
    [ "$(yexec cat "$helper.sha256" 2>/dev/null)" = "$(digest)" ] &&
    yexec test -x "$project_hook" && yexec cmp -s - "$project_hook" < "$here/projects-changed.sh" &&
    as_dev "$helper" registration-ready 2>/dev/null
}

runtime() {
  local mode="${1:-}" state=absent actual='' desired='' installed=false
  [ "$#" -gt 0 ] || die 'Herdr runtime mode is required'
  shift
  if [ "$YARD_KIND" = container ] && [ "$NESTED_E2E_VMS" != 1 ] && yard_running; then
    if yexec test -x "$helper"; then
      installed=true
      desired=$(digest)
      actual=$(yexec cat "$helper.sha256" 2>/dev/null) || actual=''
      # A missing or corrupted record is stale: apply reinstalls the assets and rewrites it.
      [[ "$actual" =~ ^[a-f0-9]{64}$ ]] || actual=$(printf '' | sha256sum | cut -d ' ' -f1)
      state=stale
      if [ "$actual" = "$desired" ]; then state=current; fi
    fi
  elif [ "$YARD_KIND" = container ] && [ "$NESTED_E2E_VMS" != 1 ]; then
    state=deferred
  fi
  if [ "$mode" = apply ]; then
    [ "$#" -eq 3 ] && [ "$installed" = true ] && [ "$actual" = "$2" ] && [ "$desired" = "$3" ] || die 'Herdr runtime plan is stale'
    install_assets >&2
    printf '%s\n' "$desired" | guest_file "$helper.sha256" 0644 >&2
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
Usage: ${PROG:-yard} herdr <up|status|is-up|pair|sync|update|restart|logs|down>
  pair --key PUBKEY [--name LABEL] | --list | --revoke FINGERPRINT
  sync [--restore]  Reconcile project spaces, optionally restoring closed ones
  update [--force]  Upgrade to the latest compatible release, or restore the pin
  logs [--follow]   Read Herdr logs
  down              Stop Core and its running terminals
EOF
}

action="${1:-}"
if [ "$action" = _runtime ]; then shift; runtime "$@"; exit; fi
case "$action" in ''|--help|-h|help) usage; exit ;; esac
shift
if [ "$action" = pair ]; then
  parse_pair "$@"
else
  case "$action:$#:${1:-}" in
    update:1:--force|logs:1:--follow|sync:1:--restore|up:0:|status:0:|is-up:0:|sync:0:|update:0:|restart:0:|logs:0:|down:0:) ;;
    *) svc_usage_error 'invalid Herdr command or arguments' ;;
  esac
fi

assessment() {
  local mode="$1" ready=false consequence desired target old result count observed=unknown installed version
  target="incus:$INCUS_PROJECT/$YARD_INSTANCE_NAME/herdr"
  case "$action" in
    status|is-up|logs)
      jq -cn --arg action "$action" '{schema:"yard.resource-action-assessment.v1",action:$action,changed:false,consequences:[]}'
      return ;;
    pair) pair_assessment; return ;;
    sync)
      allowed
      svc_require_yard_running
      jq -cn '{schema:"yard.resource-action-assessment.v1",action:"sync",changed:true,consequences:[]}'
      return ;;
    up)
      allowed
      if yard_running && route_ready && up_ready; then ready=true; fi
      desired='core ready, kick enabled and proxy published'
      consequence='install or refresh Herdr Core and publish its tailnet proxy with password and keyboard-interactive SSH disabled' ;;
    update)
      allowed
      svc_require_yard_running
      running || die "Herdr Core is stopped; run '${PROG:-yard} herdr up'"
      desired='core is running the installed release'
      if [ "$mode" = verify ]; then
        installed=$(core_release installed)
        version=$(observe | jq -er '.Version')
        if [ "$installed" = "$version" ]; then ready=true; fi
      fi
      if [ "$#" -eq 2 ]; then
        consequence='restore the pinned Herdr Core release, keeping its terminals'
      else
        consequence='update to the latest compatible Herdr Core, keeping its terminals'
      fi ;;
    restart)
      allowed
      svc_require_yard_running
      result=$(core_pid) || die "Herdr Core is stopped or its PID is unavailable; run '${PROG:-yard} herdr up'"
      old="$result"
      if [ -n "${SUBYARD_RESOURCE_STEPS:-}" ]; then
        old=$(jq -er '.[0].desired | capture("^running-after:(?<pid>[1-9][0-9]*)$").pid' <<< "$SUBYARD_RESOURCE_STEPS") ||
          die 'Herdr approved Core PID is unavailable'
      fi
      if [ "$mode" = verify ] && [ "$result" != "$old" ]; then ready=true; fi
      desired="running-after:$old"
      observed="running:$result"
      consequence='restart Herdr Core in a clean login environment, keeping its terminals' ;;
    down)
      svc_require_yard_running
      result=$(observe) || result=unknown
      if [ "$result" = null ] && ! enabled && route_absent && guest_ssh absent; then ready=true; fi
      desired='core stopped, kick disabled, proxy and SSH drop-in absent'
      count=unavailable
      if [ "$result" != unknown ]; then count=$(jq -r '.Live // 0' <<< "$result"); fi
      consequence="remove the Herdr proxy and SSH drop-in and stop all Herdr terminals, including arrivals. Observed live terminals: $count"
      if [ -n "${SUBYARD_RESOURCE_STEPS:-}" ]; then
        old=$(jq -er '.[0].consequence' <<< "$SUBYARD_RESOURCE_STEPS")
        consequence="$old"
      fi ;;
  esac
  jq -cn --arg action "$action" --arg target "$target" --arg desired "$desired" \
    --arg consequence "$consequence" --arg observed "$observed" --argjson ready "$ready" '
    {schema:"yard.resource-action-assessment.v2",action:$action,changed:($ready|not),
     consequences:(if $ready then [] else [$consequence] end),steps:[{id:"core",target:$target,
     observed:(if $ready then $desired else $observed end),desired:$desired,
     decision:(if $ready then "skip" elif $observed == "unknown" then "conditional" else "apply" end),
     preconditions:["selected yard and Herdr runtime"],verify:$desired,consequence:$consequence}]}'
}

case "${SUBYARD_RESOURCE_MODE:-}" in
  prepare|verify) assessment "$SUBYARD_RESOURCE_MODE" "$@" ;;
  apply)
    [ -n "${SUBYARD_OPERATION_ID:-}" ] || die 'typed resource apply required'
    if [ "$action" = pair ]; then pair_apply; exit; fi
    [ "${SUBYARD_RESOURCE_ACTION:-}" = "$action" ] || die 'typed resource apply action mismatch'
    case "$action" in
      up)
        allowed
        svc_require_yard_running
        up
        guest_ssh ensure
        if ! route_ready; then ensure_route; fi ;;
      is-up) yard_running && running && route_ready ;;
      status)
        if ! yard_running; then printf 'Herdr: yard is stopped or unavailable\n'; exit; fi
        if ! yexec test -x "$helper"; then printf 'Herdr: not installed\n'; exit; fi
        printf 'Minimum Core release: %s\n' "$HERDR_VERSION"
        if route_ready 2>/dev/null; then
          printf 'SSH endpoint: %s@%s:%s\n' "$DEV_USER" "$HERDR_ADVERTISE_HOST" "$HERDR_HOST_PORT"
        else
          printf 'SSH endpoint: unpublished or unavailable\n'
        fi
        as_dev env "SUBYARD_HERDR_MINIMUM=$HERDR_VERSION" "$helper" status ;;
      sync) allowed; svc_require_yard_running; as_dev "$helper" sync "$@" ;;
      logs) svc_require_yard_running; yexec journalctl --no-pager -n 100 "_SYSTEMD_UNIT=$unit" + "_UID=$DEV_UID" "_COMM=herdr-core" "$@" ;;
      update)
        allowed
        old=$(core_pid) || die 'Herdr Core must be running to update'
        if [ "$#" -eq 1 ]; then changed=$(core_release force); else changed=$(core_release latest); fi
        case "$changed" in
          true) wait_core "$old" ;;
          false)
            # A Core still running an older binary than the installed release needs the handover.
            if [ "$(observe | jq -er '.Version')" != "$(core_release installed)" ]; then
              as_dev "$login" server restart
              wait_core "$old"
            fi ;;
          *) die 'Herdr Core update did not report whether it replaced the release' ;;
        esac ;;
      restart)
        allowed
        old=$(core_pid) || die 'Herdr Core must be running to restart'
        as_dev "$login" server restart --fresh
        wait_core "$old" ;;
      down)
        remove_route "$(route_state)"
        if enabled; then yexec systemctl disable --now "$unit"; fi
        if yexec test -x "$helper"; then
          as_dev "$helper" stop || die 'Herdr ingress is closed, but Core shutdown could not be verified'
        fi
        result=$(observe) || die 'Herdr ingress is closed, but Core status could not be verified'
        if [ "$result" != null ]; then die 'Herdr Core is still running or was started by another client'; fi
        guest_ssh remove ;;
    esac ;;
  '') [ "$action" = is-up ] && yard_running && running && route_ready ;;
  *) die 'unsupported Herdr execution mode' ;;
esac
