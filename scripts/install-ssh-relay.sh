#!/usr/bin/env bash
# Install or remove the root-owned loopback TCP relay used for VM SSH access.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/runtime.sh
. "$SCRIPT_DIR/lib/runtime.sh"
# shellcheck source=scripts/lib/engine-context.sh
. "$SCRIPT_DIR/lib/engine-context.sh"
subyard_require_engine_context
# shellcheck source=scripts/lib/ui.sh
. "$SCRIPT_DIR/lib/ui.sh"
# shellcheck source=scripts/lib/host.sh
. "$SCRIPT_DIR/lib/host.sh"

action="${1:-}"
port="${2:-}"
target="${3:-}"
case "$action" in --ensure | --remove) ;; *) die "usage: install-ssh-relay.sh --ensure PORT IPV4 | --remove PORT" ;; esac
case "$port" in '' | *[!0-9]*) die "SSH relay port must be numeric" ;; esac
[ "$port" -ge 1 ] && [ "$port" -le 65535 ] || die "SSH relay port is out of range"
if [ "$action" = --ensure ]; then
  [[ "$target" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || die "SSH relay target must be an IPv4 address"
  IFS=. read -r a b c d <<<"$target"
  for octet in "$a" "$b" "$c" "$d"; do
    [ "$octet" -le 255 ] || die "SSH relay target must be an IPv4 address"
  done
else
  [ -z "$target" ] || die "--remove does not accept a target"
fi

require_root "installing the VM SSH loopback relay needs a root-owned systemd socket"

unit="subyard-ssh-relay-$port"
socket_path="/etc/systemd/system/$unit.socket"
service_path="/etc/systemd/system/$unit.service"
if [ "$action" = --remove ]; then
  systemctl disable --now "$unit.socket" >/dev/null 2>&1 || true
  systemctl stop "$unit.service" >/dev/null 2>&1 || true
  rm -f -- "$socket_path" "$service_path"
  systemctl daemon-reload
  ok "removed VM SSH relay on 127.0.0.1:$port (if present)"
  exit 0
fi

proxyd=""
for candidate in /usr/lib/systemd/systemd-socket-proxyd /lib/systemd/systemd-socket-proxyd; do
  [ -x "$candidate" ] && { proxyd="$candidate"; break; }
done
[ -n "$proxyd" ] || die "systemd-socket-proxyd is required for VM SSH access"

socket_temp="$(mktemp "/etc/systemd/system/.$unit.socket.XXXXXX")"
service_temp="$(mktemp "/etc/systemd/system/.$unit.service.XXXXXX")"
socket_backup=""
service_backup=""
trap 'rm -f -- "$socket_temp" "$service_temp" ${socket_backup:+"$socket_backup"} ${service_backup:+"$service_backup"}' EXIT
cat >"$socket_temp" <<EOF
[Unit]
Description=Subyard VM SSH loopback relay on port $port

[Socket]
ListenStream=127.0.0.1:$port
Accept=no

[Install]
WantedBy=sockets.target
EOF
cat >"$service_temp" <<EOF
[Unit]
Description=Subyard VM SSH relay to $target:22
After=network-online.target

[Service]
ExecStart=$proxyd $target:22
NoNewPrivileges=yes
PrivateTmp=yes
ProtectHome=yes
ProtectSystem=strict
PrivateDevices=yes
RestrictAddressFamilies=AF_INET AF_INET6
EOF
chmod 0644 "$socket_temp" "$service_temp"

for current in "$socket_path" "$service_path"; do
  if [ -e "$current" ] || [ -L "$current" ]; then
    [ -f "$current" ] && [ ! -L "$current" ] \
      || die "refusing unsafe SSH relay unit path: $current"
    [ "$(stat -c %u "$current")" = 0 ] && [ "$(stat -c %a "$current")" = 644 ] \
      || die "SSH relay unit must be root-owned mode 0644: $current"
  fi
done

changed=0
cmp -s "$socket_temp" "$socket_path" || changed=1
cmp -s "$service_temp" "$service_path" || changed=1
if [ "$changed" = 1 ]; then
  socket_backup="$(mktemp "/etc/systemd/system/.$unit.socket.backup.XXXXXX")"
  service_backup="$(mktemp "/etc/systemd/system/.$unit.service.backup.XXXXXX")"
  socket_existed=0
  service_existed=0
  [ ! -e "$socket_path" ] || { cp -a -- "$socket_path" "$socket_backup"; socket_existed=1; }
  [ ! -e "$service_path" ] || { cp -a -- "$service_path" "$service_backup"; service_existed=1; }
  was_enabled=0
  was_active=0
  systemctl is-enabled --quiet "$unit.socket" 2>/dev/null && was_enabled=1
  systemctl is-active --quiet "$unit.socket" 2>/dev/null && was_active=1
  systemctl stop "$unit.socket" "$unit.service" >/dev/null 2>&1 || true
  if ! install -o root -g root -m 0644 "$socket_temp" "$socket_path" \
    || ! install -o root -g root -m 0644 "$service_temp" "$service_path" \
    || ! systemctl daemon-reload \
    || ! systemctl enable --now "$unit.socket" >/dev/null \
    || ! systemctl is-active --quiet "$unit.socket"; then
    systemctl disable --now "$unit.socket" >/dev/null 2>&1 || true
    if [ "$socket_existed" = 1 ]; then
      install -o root -g root -m 0644 "$socket_backup" "$socket_path"
    else
      rm -f -- "$socket_path"
    fi
    if [ "$service_existed" = 1 ]; then
      install -o root -g root -m 0644 "$service_backup" "$service_path"
    else
      rm -f -- "$service_path"
    fi
    systemctl daemon-reload || true
    [ "$was_enabled" = 0 ] || systemctl enable "$unit.socket" >/dev/null 2>&1 || true
    [ "$was_active" = 0 ] || systemctl start "$unit.socket" >/dev/null 2>&1 || true
    rm -f -- "$socket_backup" "$service_backup"
    die "could not publish the VM SSH relay; previous relay state restored"
  fi
  rm -f -- "$socket_backup" "$service_backup"
else
  systemctl enable --now "$unit.socket" >/dev/null
fi
systemctl is-active --quiet "$unit.socket" \
  || die "VM SSH relay socket did not become active"
ok "VM SSH relay active on 127.0.0.1:$port -> $target:22"
