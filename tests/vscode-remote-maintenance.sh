#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

export HOME="$TMP/home" SUBYARD_PROC_ROOT="$TMP/proc"
mkdir -p "$HOME/.vscode-server" "$SUBYARD_PROC_ROOT"
out="$(sh "$ROOT/scripts/vscode-remote-maintenance.sh" check-active)"
[ "$out" = idle ] || fail "idle VS Code state was $out"

mkdir "$SUBYARD_PROC_ROOT/123"
printf '%s\0%s\0' "$HOME/.vscode-server/server" '--type=extensionHost' \
  > "$SUBYARD_PROC_ROOT/123/cmdline"
: > "$SUBYARD_PROC_ROOT/123/comm"
out="$(sh "$ROOT/scripts/vscode-remote-maintenance.sh" check-active)"
[ "$out" = active ] || fail "active VS Code state was $out"

out="$(VSCODE_USER=missing-user sh "$ROOT/scripts/vscode-remote-maintenance.sh" check-active)"
[ "$out" = unknown ] || fail "missing VS Code user was reported as $out"
out="$(VSCODE_USER=missing-user sh "$ROOT/scripts/vscode-remote-maintenance.sh" check-ssh)"
[ "$out" = idle ] || fail "SSH-only probe included a background editor or required a user"
for process in sshd sshd-session; do
  printf '%s\n' "$process" > "$SUBYARD_PROC_ROOT/123/comm"
  out="$(VSCODE_USER=missing-user sh "$ROOT/scripts/vscode-remote-maintenance.sh" check-ssh)"
  [ "$out" = active ] || fail "SSH-only probe missed $process without the developer account"
  out="$(VSCODE_USER=missing-user sh "$ROOT/scripts/vscode-remote-maintenance.sh" check-active)"
  [ "$out" = unknown ] || fail "SSH-only action changed check-active's missing-user protocol"
done
if sh "$ROOT/scripts/vscode-remote-maintenance.sh" sync >/dev/null 2>&1; then
  fail "retired extension-sync action is still accepted"
fi

printf 'ok: VS Code session probe distinguishes idle, active and unknown states\n'
