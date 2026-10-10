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
# shellcheck source=scripts/lib/ssh-listener.sh
. "$SCRIPT_DIR/lib/ssh-listener.sh"
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

PROFILE_RESTORE_NEEDED=0
paused_profiles=""

restore_ssh_listener_on_exit() {
  ssh_listener_restore || warn "could not restore the yard SSH listener after cancelling stop"
  if [ "$PROFILE_RESTORE_NEEDED" = 1 ]; then
    "$SCRIPT_DIR/profile-services.sh" --resume "$paused_profiles" || warn 'could not resume profile owner services'
  fi
}
trap restore_ssh_listener_on_exit EXIT

case "$action" in
  start)
    power_nm_prepare_reader || die "$POWER_ERROR"
    info "starting $YARD_INSTANCE_NAME"
    power_start_guarded "$INCUS_PROJECT" "$YARD_INSTANCE_NAME" "$BRIDGE" || die "$POWER_ERROR"
    info "waiting for $YARD_INSTANCE_NAME agent"
    incus_wait_instance_agent "$INCUS_PROJECT" "$YARD_INSTANCE_NAME" || {
      power_guest_start_failed "$INCUS_PROJECT" "$YARD_INSTANCE_NAME" \
        "instance '$YARD_INSTANCE_NAME' agent did not become ready" || die "$POWER_ERROR"
    }
    # Cloud-image seeding can replace the QEMU process scoped at first start.
    if [ -n "${VM_CPU_WEIGHT:-}" ]; then
      power_start_guarded "$INCUS_PROJECT" "$YARD_INSTANCE_NAME" "$BRIDGE" || die "$POWER_ERROR"
    fi
    ;;
  stop)
    cur="$(state)"
    if [ "$cur" = RUNNING ]; then
      if [ "$force" = 0 ] && [ "$reconcile" = 0 ]; then
        quiesce_rc=0
        ssh_listener_capture "$YARD_INSTANCE_NAME" || quiesce_rc=$?
        if [ "$quiesce_rc" = 0 ]; then
          ssh_listener_fence || quiesce_rc=4
        fi
        case "$quiesce_rc" in
          0) ;;
          2) die "cannot safely pause new SSH connections: ssh.service KillMode is not 'process'; use '$(yard_cmd_hint) stop --force' only for emergency shutdown" ;;
          *) die "could not pause new SSH connections before checking VS Code; retry, or use '$(yard_cmd_hint) stop --force' for emergency shutdown" ;;
        esac
        paused_profiles="$("$SCRIPT_DIR/profile-services.sh" --pause)" \
          || die 'could not pause profile owner services before checking SSH sessions'
        [ -z "$paused_profiles" ] || PROFILE_RESTORE_NEEDED=1
        vcstate="$(vscode_remote_state)"
        # Remote sshd children can briefly outlive the closed transport.
        if [ "$PROFILE_RESTORE_NEEDED" = 1 ]; then
          for _ in {1..10}; do
            [ "$vcstate" = active ] || break
            sleep 0.2
            vcstate="$(vscode_remote_state)"
          done
        fi
        case "$vcstate" in
          active)
            ssh_listener_restore || warn "could not restore the yard SSH listener after cancelling stop"
            die "VS Code Remote-SSH or another SSH session is still connected to '$SSH_HOST' — close every remote window (File > Close Remote Connection) and shell, then retry; use '$(yard_cmd_hint) stop --force' only for emergency shutdown"
            ;;
          unknown)
            ssh_listener_restore || warn "could not restore the yard SSH listener after cancelling stop"
            die "could not verify that VS Code Remote-SSH is idle — retry, or use '$(yard_cmd_hint) stop --force' for emergency shutdown"
            ;;
        esac
      elif [ "$reconcile" = 0 ]; then
        warn "--force bypasses the active SSH / VS Code session guard"
      fi
      info "stopping $YARD_INSTANCE_NAME"
      if ! power_stop_instance "$INCUS_PROJECT" "$YARD_INSTANCE_NAME"; then
        ssh_listener_restore || warn "could not restore the yard SSH listener after cancelling stop"
        die "could not stop $YARD_INSTANCE_NAME"
      fi
      SSH_LISTENER_RESTORE_NEEDED=0
    else
      info "$YARD_INSTANCE_NAME already stopped (${cur:-unknown})"
    fi
    ;;
  *)
    die "unknown action '$action' (expected: start | stop)"
    ;;
esac
