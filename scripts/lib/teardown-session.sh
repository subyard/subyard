#!/usr/bin/env bash
# Teardown permits unfinished/unreachable guests, but protects proven SSH sessions.
# shellcheck source=scripts/lib/ssh-listener.sh
. "$SCRIPT_DIR/lib/ssh-listener.sh"

teardown_ssh_state() {
  ssh_listener_command exec "$SSH_LISTENER_INSTANCE" "${SSH_LISTENER_PROJECT[@]}" -- \
    sh -s -- check-ssh < "$SCRIPT_DIR/vscode-remote-maintenance.sh" 2>/dev/null || printf 'unavailable\n'
}

teardown_session_guard() {
  [ "${have_incus:-0}" = 1 ] || return 0
  local instance="$1" capture_rc=0 result paused=""
  [ "$(timeout 5s incus list "$instance" "${PROJ[@]}" -f csv -c s 2>/dev/null)" = RUNNING ] || return 0
  ssh_listener_capture "$instance" 5s || capture_rc=$?
  case "$capture_rc" in
    0) ;;
    1) return 0 ;;
    2) die "cannot safely fence new SSH sessions before teardown: ssh.service KillMode must be process" ;;
    *) die "could not inspect the yard SSH listener before teardown" ;;
  esac
  TEARDOWN_SESSION_PROFILES=""
  teardown_session_restore() {
    ssh_listener_running || return 0
    ssh_listener_restore || true
    [ -z "$TEARDOWN_SESSION_PROFILES" ] || "$SCRIPT_DIR/profile-services.sh" --resume "$TEARDOWN_SESSION_PROFILES" || true
  }
  trap teardown_session_restore EXIT
  ssh_listener_fence || die "could not fence new SSH sessions before teardown"
  paused="$("$SCRIPT_DIR/profile-services.sh" --pause)" || die "could not pause profile owner services before teardown"
  TEARDOWN_SESSION_PROFILES="$paused"
  # The same all-user SSH probe works before the developer account exists.
  result="$(teardown_ssh_state)"
  if [ -n "$paused" ]; then
    for _ in {1..10}; do
      [ "$result" = active ] || break
      sleep 0.2
      result="$(teardown_ssh_state)"
    done
  fi
  [ "$result" != active ] || die "an SSH session is still connected to the yard — close remote windows and shells, then retry teardown"
}
