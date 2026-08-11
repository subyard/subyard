#!/usr/bin/env bash
# Physical start/stop boundary. Go owns parsing, desired state and the transaction.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/engine-context.sh
. "$SCRIPT_DIR/lib/engine-context.sh"
subyard_require_engine_context
# shellcheck source=scripts/lib/ui.sh
. "$SCRIPT_DIR/lib/ui.sh"
# shellcheck source=scripts/lib-power.sh
. "$SCRIPT_DIR/lib-power.sh"
# shellcheck source=scripts/lib/host.sh
. "$SCRIPT_DIR/lib/host.sh"
INCUS_PROJECT="${INCUS_PROJECT:-subyard}"
YARD_INSTANCE_NAME="${YARD_INSTANCE_NAME:-yard}"
DEV_USER="${DEV_USER:-dev}"
SSH_HOST="${SSH_HOST:-yard}"
PROJ=(--project "$INCUS_PROJECT")

action="${1:-}"; shift || true
force=0 reconcile=0
for a in "$@"; do
  case "$a" in
    --force) force=1 ;;
    --reconcile) reconcile=1 ;;
    -*) die "unknown option '$a'" ;;
    *) ;;
  esac
done
[ "$force" = 0 ] || [ "$action" = stop ] || die "--force is only valid with stop"
[ "$reconcile" = 0 ] || case "$action" in start | stop) ;; *) die "--reconcile needs start or stop" ;; esac

incus_preflight "$action"
incus info "$YARD_INSTANCE_NAME" "${PROJ[@]}" >/dev/null 2>&1 \
  || die "instance '$YARD_INSTANCE_NAME' missing — run '$(yard_cmd_hint) init' first"

state() { incus list "$YARD_INSTANCE_NAME" "${PROJ[@]}" -f csv -c s 2>/dev/null; }
BRIDGE="${INCUS_BRIDGE:-${INCUS_NETWORK:-incusbr0}}"

# A container stop cannot gracefully close desktop windows or SSH shells. Refuse to cross an active
# session after closing the SSH listener to new connections.
vscode_remote_state() {
  local result
  if result="$(incus exec "$YARD_INSTANCE_NAME" "${PROJ[@]}" --env VSCODE_USER="$DEV_USER" -- \
      sh -s -- check-active < "$SCRIPT_DIR/vscode-remote-maintenance.sh" 2>/dev/null)"; then
    result="${result##*$'\n'}"
    case "$result" in idle | active | unknown) printf '%s\n' "$result" ;; *) printf 'unknown\n' ;; esac
  else
    printf 'unknown\n'
  fi
}

SSH_SERVICE_WAS_ACTIVE=0
SSH_SOCKET_WAS_ACTIVE=0
SSH_RESTORE_NEEDED=0

# Stop only the SSH listener, never established sessions. With Debian's KillMode=process the
# per-session sshd children survive, so the activity probe can see them while no new Remote-SSH
# window can enter between the probe and `incus stop`.
ssh_listener_quiesce() {
  local result rest
  if ! result="$(incus exec "$YARD_INSTANCE_NAME" "${PROJ[@]}" -- sh -eu -c '
    service=0; socket=0
    if systemctl is-active --quiet ssh; then service=1; fi
    if systemctl is-active --quiet ssh.socket; then socket=1; fi
    if [ "$service" = 1 ] && [ "$(systemctl show ssh --property=KillMode --value)" != process ]; then
      printf "unsupported\n"
      exit 0
    fi
    printf "snapshot:%s:%s\n" "$service" "$socket"
  ' 2>/dev/null)"; then
    return 1
  fi
  case "$result" in
    snapshot:[01]:[01])
      rest="${result#snapshot:}"
      SSH_SERVICE_WAS_ACTIVE="${rest%%:*}"
      SSH_SOCKET_WAS_ACTIVE="${rest##*:}"
      SSH_RESTORE_NEEDED=1
      ;;
    unsupported) return 2 ;;
    *) return 1 ;;
  esac
  if ! incus exec "$YARD_INSTANCE_NAME" "${PROJ[@]}" \
      --env SERVICE="$SSH_SERVICE_WAS_ACTIVE" --env SOCKET="$SSH_SOCKET_WAS_ACTIVE" -- \
      sh -eu -c '
        [ "$SOCKET" = 0 ] || systemctl stop ssh.socket
        [ "$SERVICE" = 0 ] || systemctl stop ssh
      ' >/dev/null 2>&1; then
    ssh_listener_restore
    return 1
  fi
  return 0
}

ssh_listener_restore() {
  if [ "$SSH_SERVICE_WAS_ACTIVE" = 0 ] && [ "$SSH_SOCKET_WAS_ACTIVE" = 0 ]; then
    SSH_RESTORE_NEEDED=0
    return 0
  fi
  if ! incus exec "$YARD_INSTANCE_NAME" "${PROJ[@]}" \
      --env SERVICE="$SSH_SERVICE_WAS_ACTIVE" --env SOCKET="$SSH_SOCKET_WAS_ACTIVE" -- \
      sh -eu -c '
        [ "$SOCKET" = 0 ] || systemctl start ssh.socket
        [ "$SERVICE" = 0 ] || systemctl start ssh
      ' >/dev/null 2>&1; then
    warn "could not restore the yard SSH listener after cancelling stop"
    return 0
  fi
  SSH_SERVICE_WAS_ACTIVE=0
  SSH_SOCKET_WAS_ACTIVE=0
  SSH_RESTORE_NEEDED=0
}

restore_ssh_listener_on_exit() {
  [ "$SSH_RESTORE_NEEDED" = 0 ] || ssh_listener_restore
}
trap restore_ssh_listener_on_exit EXIT

case "$action" in
  start)
    power_nm_prepare_reader || die "$POWER_ERROR"
    info "starting $YARD_INSTANCE_NAME"
    power_start_guarded "$INCUS_PROJECT" "$YARD_INSTANCE_NAME" "$BRIDGE" || die "$POWER_ERROR"
    info "waiting for $YARD_INSTANCE_NAME agent"
    incus_wait_instance_agent "$INCUS_PROJECT" "$YARD_INSTANCE_NAME" \
      || die "instance '$YARD_INSTANCE_NAME' agent did not become ready"
    ;;
  stop)
    cur="$(state)"
    if [ "$cur" = RUNNING ]; then
      if [ "$force" = 0 ] && [ "$reconcile" = 0 ]; then
        quiesce_rc=0
        ssh_listener_quiesce || quiesce_rc=$?
        case "$quiesce_rc" in
          0) ;;
          2) die "cannot safely pause new SSH connections: ssh.service KillMode is not 'process'; use '$(yard_cmd_hint) stop --force' only for emergency shutdown" ;;
          *) die "could not pause new SSH connections before checking VS Code; retry, or use '$(yard_cmd_hint) stop --force' for emergency shutdown" ;;
        esac
        vcstate="$(vscode_remote_state)"
        case "$vcstate" in
          active)
            ssh_listener_restore
            die "VS Code Remote-SSH or another SSH session is still connected to '$SSH_HOST' — close every remote window (File > Close Remote Connection) and shell, then retry; use '$(yard_cmd_hint) stop --force' only for emergency shutdown"
            ;;
          unknown)
            ssh_listener_restore
            die "could not verify that VS Code Remote-SSH is idle — retry, or use '$(yard_cmd_hint) stop --force' for emergency shutdown"
            ;;
        esac
      elif [ "$reconcile" = 0 ]; then
        warn "--force bypasses the active SSH / VS Code session guard"
      fi
      info "stopping $YARD_INSTANCE_NAME"
      if ! power_stop_instance "$INCUS_PROJECT" "$YARD_INSTANCE_NAME"; then
        ssh_listener_restore
        die "could not stop $YARD_INSTANCE_NAME"
      fi
      SSH_RESTORE_NEEDED=0
    else
      info "$YARD_INSTANCE_NAME already stopped (${cur:-unknown})"
    fi
    ;;
  *)
    die "unknown action '$action' (expected: start | stop)"
    ;;
esac
