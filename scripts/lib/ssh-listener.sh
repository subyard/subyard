#!/usr/bin/env bash
# Capture and fence only the SSH listener; established sessions survive.
# Callers own unavailable-guest policy, profile services and EXIT recovery.
# shellcheck disable=SC2016
SSH_LISTENER_RESTORE_NEEDED=0

ssh_listener_command() {
  if [ -n "$SSH_LISTENER_DEADLINE" ]; then
    timeout "$SSH_LISTENER_DEADLINE" incus "$@"
  else
    incus "$@"
  fi
}

ssh_listener_capture() {
  # 1: unavailable, 2: unsupported KillMode, 3: malformed observation.
  SSH_LISTENER_INSTANCE="$1"
  SSH_LISTENER_PROJECT=("${PROJ[@]}")
  SSH_LISTENER_DEADLINE="${2:-}"
  SSH_LISTENER_RESTORE_NEEDED=0
  local snapshot rest
  snapshot="$(ssh_listener_command exec "$SSH_LISTENER_INSTANCE" "${SSH_LISTENER_PROJECT[@]}" -- sh -eu -c '
    service=0; socket=0
    if systemctl is-active --quiet ssh; then service=1; fi
    if systemctl is-active --quiet ssh.socket; then socket=1; fi
    if [ "$service" = 1 ] && [ "$(systemctl show ssh --property=KillMode --value)" != process ]; then
      printf "unsupported\n"; exit 0
    fi
    printf "snapshot:%s:%s\n" "$service" "$socket"
  ' 2>/dev/null)" || return 1
  case "$snapshot" in
    snapshot:[01]:[01])
      rest="${snapshot#snapshot:}"
      SSH_LISTENER_SERVICE="${rest%%:*}"
      SSH_LISTENER_SOCKET="${rest##*:}"
      ;;
    unsupported) return 2 ;;
    *) return 3 ;;
  esac
}

ssh_listener_fence() {
  # Arm recovery before stopping either listener, including partial failure.
  SSH_LISTENER_RESTORE_NEEDED=1
  ssh_listener_command exec "$SSH_LISTENER_INSTANCE" "${SSH_LISTENER_PROJECT[@]}" \
    --env SERVICE="$SSH_LISTENER_SERVICE" --env SOCKET="$SSH_LISTENER_SOCKET" -- sh -eu -c '
      [ "$SOCKET" = 0 ] || systemctl stop ssh.socket
      [ "$SERVICE" = 0 ] || systemctl stop ssh
    ' >/dev/null 2>&1
}

ssh_listener_running() {
  [ "$(ssh_listener_command list "$SSH_LISTENER_INSTANCE" "${SSH_LISTENER_PROJECT[@]}" -f csv -c s 2>/dev/null)" = RUNNING ]
}

ssh_listener_restore() {
  [ "$SSH_LISTENER_RESTORE_NEEDED" = 1 ] || return 0
  if ! ssh_listener_running || [ "$SSH_LISTENER_SERVICE:$SSH_LISTENER_SOCKET" = 0:0 ]; then
    SSH_LISTENER_RESTORE_NEEDED=0
    return 0
  fi
  ssh_listener_command exec "$SSH_LISTENER_INSTANCE" "${SSH_LISTENER_PROJECT[@]}" \
    --env SERVICE="$SSH_LISTENER_SERVICE" --env SOCKET="$SSH_LISTENER_SOCKET" -- sh -eu -c '
      [ "$SOCKET" = 0 ] || systemctl start ssh.socket
      [ "$SERVICE" = 0 ] || systemctl start ssh
    ' >/dev/null 2>&1 || return 1
  SSH_LISTENER_RESTORE_NEEDED=0
}
