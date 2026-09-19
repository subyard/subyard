#!/usr/bin/env bash
# Physical convergence of the selected GitHub profile and its owner-side user service.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/engine-context.sh
. "$SCRIPT_DIR/lib/engine-context.sh"
subyard_require_engine_context
# shellcheck source=scripts/lib/ui.sh
. "$SCRIPT_DIR/lib/ui.sh"
# shellcheck source=scripts/lib/host.sh
. "$SCRIPT_DIR/lib/host.sh"

mode=apply
case "${1:-}" in --check) mode=check ;; --remove) mode=remove ;; --pause) mode=pause ;; --resume) mode=resume ;; --yes|'') ;; *) die 'invalid GitHub broker lifecycle argument' ;; esac
root="$(cd "$SCRIPT_DIR/.." && pwd)"
yard="${SUBYARD_YARD:-default}"
case "$yard" in ''|*[!a-zA-Z0-9_-]*) die 'invalid GitHub broker yard' ;; esac
unit="subyard-github-$yard.service"
unit_dir="$SUBYARD_OPERATOR_HOME/.config/systemd/user"
unit_file="$unit_dir/$unit"
state_dir="$SUBYARD_HOME/github-broker"
runtime_file="$state_dir/$yard.json"
broker_engine="$state_dir/$yard-engine"
app_config="$SUBYARD_CONFIG_HOME/github-app.json"
engine="${SUBYARD_DISPATCHER_PATH:-}"
profile_dir="$root/config/profiles/github"
guest_engine=/usr/local/libexec/subyard/github-client
marker='# Managed by Subyard GitHub broker'
enabled="${SUBYARD_GITHUB_ENABLED:-0}"
[ "$mode" != remove ] || enabled=0

operator="${SUBYARD_USER:-$(id -un)}"
operator_uid="$(id -u "$operator")"
user_systemctl() {
  local runtime_dir="/run/user/$operator_uid"
  if [ "$(id -u)" = "$operator_uid" ]; then
    XDG_RUNTIME_DIR="$runtime_dir" DBUS_SESSION_BUS_ADDRESS="unix:path=$runtime_dir/bus" systemctl --user "$@"
  else
    runuser -u "$operator" -- env XDG_RUNTIME_DIR="$runtime_dir" DBUS_SESSION_BUS_ADDRESS="unix:path=$runtime_dir/bus" systemctl --user "$@"
  fi
}
unit_quote() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g; s/%/%%/g'; }
render_runtime() {
  jq -n --arg app "$app_config" --argjson port "$SSH_PORT" --arg dev "$DEV_USER" \
    --arg key "${SUBYARD_KEYS_CONSUMER_ROOT:-$SUBYARD_CONFIG_HOME/generated}/github/github-app.pem" \
    --arg identity "$SUBYARD_HOME/ssh/id_ed25519" --arg known "$SUBYARD_HOME/ssh/known_hosts" \
    '{app_config:$app,key_file:$key,ssh_port:$port,developer:$dev,identity_file:$identity,known_hosts_file:$known}'
}
render_unit() {
  printf '%s\n' "$marker"
  printf '# engine: %s\n' "$(sha256sum "$engine" | cut -d' ' -f1)"
  cat <<UNIT
[Unit]
Description=Subyard GitHub token broker ($yard)
StartLimitIntervalSec=0
[Service]
ExecStart="$(unit_quote "$broker_engine")" _github-broker "$(unit_quote "$runtime_file")"
Restart=always
RestartSec=5
TimeoutStopSec=10
UMask=0077
LimitCORE=0
MemoryMax=128M
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=read-only
[Install]
WantedBy=default.target
UNIT
}
owned_service() {
  [ ! -L "$unit_file" ] && [ -f "$unit_file" ] && [ "$(head -n 1 "$unit_file")" = "$marker" ]
}
# Normal stop temporarily disconnects only our transport before checking user sessions.
if [ "$mode" = pause ] || [ "$mode" = resume ]; then
  owned_service || exit 0
  if [ "$mode" = resume ]; then
    user_systemctl start "$unit"
  else
    state="$(user_systemctl show "$unit" --property=ActiveState --value)"
    case "$state" in
      active|activating|reloading)
        user_systemctl stop "$unit"
        printf 'paused\n'
        ;;
    esac
  fi
  exit 0
fi
guest_hook() {
  tar -C "$profile_dir" -cf - . | incus exec "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" \
    --env DEV_USER="$DEV_USER" -- bash -euo pipefail -c '
      bundle="$(mktemp -d /tmp/subyard-github.XXXXXX)"
      trap '\''rm -rf -- "$bundle"'\'' EXIT
      tar -xf - -C "$bundle"
      bash "$bundle/provision.sh" "$@"
    ' subyard "$@"
}

guest_status() {
  incus exec "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" \
    -- runuser -u "$DEV_USER" -- /usr/local/bin/subyard-github status
}

if [ "$enabled" != 1 ]; then
  if [ "$mode" = check ]; then
    [ ! -e "$unit_file" ] && [ ! -L "$unit_file" ] && [ ! -e "$runtime_file" ] && [ ! -e "$broker_engine" ] || exit 1
    if incus exec "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" -- getent passwd "$DEV_USER" >/dev/null 2>&1; then
      guest_hook --check-removed >/dev/null || exit 1
    fi
    exit 0
  fi
  if [ -e "$unit_file" ] || [ -L "$unit_file" ]; then
    owned_service || die 'refusing to remove unmanaged GitHub broker service'
    user_systemctl disable --now "$unit" >/dev/null
    rm -- "$unit_file"
    user_systemctl daemon-reload
  fi
  # The host authorization is already gone; guest cleanup needs a running instance.
  if incus exec "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" -- true 2>/dev/null; then
    if incus exec "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" -- getent passwd "$DEV_USER" >/dev/null 2>&1; then
      guest_hook --remove
    fi
    incus exec "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" -- rm -f -- "$guest_engine"
  fi
  [ ! -L "$runtime_file" ] || die 'unsafe GitHub broker runtime file'
  [ ! -e "$runtime_file" ] || rm -- "$runtime_file"
  [ ! -L "$broker_engine" ] || die 'unsafe GitHub broker engine path'
  [ ! -e "$broker_engine" ] || rm -- "$broker_engine"
  exit
fi

[ -x "$engine" ] && [ -f "$engine" ] || die 'native GitHub broker engine is missing'
[ ! -L "$state_dir" ] && [ ! -L "$broker_engine" ] && [ ! -L "$runtime_file" ] && [ ! -L "$unit_file" ] || die 'unsafe GitHub broker managed path'
if [ -e "$unit_file" ]; then owned_service || die 'refusing to overwrite unmanaged GitHub broker service'; fi

if [ "$mode" = check ]; then
  [ -f "$runtime_file" ] && cmp -s <(render_runtime) "$runtime_file" || exit 1
  owned_service && cmp -s <(render_unit) "$unit_file" || exit 1
  user_systemctl is-enabled --quiet "$unit" 2>/dev/null || exit 1
  [ "$(loginctl show-user "$(id -un)" -p Linger --value 2>/dev/null)" = yes ] || exit 1
  cmp -s "$engine" "$broker_engine" || exit 1
  [ "${SUBYARD_GITHUB_STOPPED:-0}" != 1 ] || exit 0
  user_systemctl is-active --quiet "$unit" 2>/dev/null || exit 1
  wanted="$(sha256sum "$engine" | cut -d' ' -f1)"
  actual="$(incus exec "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" -- sha256sum "$guest_engine" 2>/dev/null | cut -d' ' -f1)" || exit 1
  [ "$actual" = "$wanted" ] || exit 1
  guest_hook --check >/dev/null || exit 1
  guest_status >/dev/null 2>&1 || exit 1
  exit
fi

install -d -m 0700 "$state_dir" "$unit_dir"
install -m 0700 "$engine" "$broker_engine.tmp"
mv -f -- "$broker_engine.tmp" "$broker_engine"
render_runtime > "$runtime_file.tmp"
chmod 0600 "$runtime_file.tmp"
mv -f -- "$runtime_file.tmp" "$runtime_file"
# Stream the engine through stdin, not through a guest-writable destination path on the host.
incus exec "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" -- sh -eu -c '
  install -d -m 0755 /usr/local/libexec/subyard
  temp="$(mktemp /usr/local/libexec/subyard/.github-client.XXXXXX)"
  trap '\''rm -f -- "$temp"'\'' EXIT
  cat > "$temp"
  chmod 0755 "$temp"
  mv -f -- "$temp" /usr/local/libexec/subyard/github-client
' < "$engine"
guest_hook
render_unit > "$unit_file.tmp"
chmod 0644 "$unit_file.tmp"
mv -f -- "$unit_file.tmp" "$unit_file"
if [ "$(loginctl show-user "$(id -un)" -p Linger --value 2>/dev/null || true)" != yes ]; then
  host_sudo loginctl enable-linger "$(id -un)"
fi
if ! user_systemctl show-environment >/dev/null 2>&1; then
  host_sudo systemctl start "user@$(id -u).service"
fi
user_systemctl daemon-reload
# The caller's HOME may differ from the running user manager's unit search path.
user_systemctl enable "$unit_file" >/dev/null
user_systemctl restart "$unit"
ready=0
for _ in {1..30}; do
  if guest_status >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
[ "$ready" = 1 ] || die "GitHub broker did not connect; inspect systemctl --user status $unit"
printf 'GitHub broker profile configured; App settings stay in owner-side github-app.json\n'
