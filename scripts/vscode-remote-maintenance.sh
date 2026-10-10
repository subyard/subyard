#!/bin/sh
# Report whether VS Code or another SSH session is active inside a yard.
set -eu
PROC_ROOT="${SUBYARD_PROC_ROOT:-/proc}"

target_identity() {
  if [ -n "${VSCODE_USER:-}" ]; then
    TARGET_UID="$(id -u "$VSCODE_USER" 2>/dev/null)" || return 1
    TARGET_HOME="$(getent passwd "$VSCODE_USER" 2>/dev/null | cut -d: -f6)"
  else
    TARGET_UID="$(id -u)"
    TARGET_HOME="${HOME:-}"
  fi
  [ -n "$TARGET_HOME" ]
}

root_active() { # <server-root>
  root="$1"
  for proc in "$PROC_ROOT"/[0-9]*; do
    [ -r "$proc/cmdline" ] || continue
    [ "$(stat -c %u "$proc" 2>/dev/null || true)" = "$TARGET_UID" ] || continue
    cmd="$(tr '\000' '\n' 2>/dev/null < "$proc/cmdline" || true)"
    case "$cmd" in
      *"$root/"*)
        case "$cmd" in
          *--type=extensionHost* | *command-shell*) return 0 ;;
        esac
        ;;
    esac
  done
  return 1
}

any_active() {
  root_active "$TARGET_HOME/.vscode-server" \
    || root_active "$TARGET_HOME/.vscode-server-insiders"
}

ssh_session_active() {
  for proc in "$PROC_ROOT"/[0-9]*; do
    comm="$(tr -d '\n' 2>/dev/null < "$proc/comm" || true)"
    case "$comm" in sshd | sshd-session) return 0 ;; esac
  done
  return 1
}

check_active() {
  if ! target_identity; then
    printf 'unknown\n'
  elif ssh_session_active || any_active; then
    printf 'active\n'
  else
    printf 'idle\n'
  fi
}

case "${1:-}" in
  check-active) check_active ;;
  check-ssh) if ssh_session_active; then printf 'active\n'; else printf 'idle\n'; fi ;;
  *) printf 'usage: check-active\n' >&2; exit 2 ;;
esac
