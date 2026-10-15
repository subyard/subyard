#!/usr/bin/env bash
# install-keys-auto-sync.sh — install the operator-owned persistent 6-hour credential sync timer.
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

REPO="$(cd "$SCRIPT_DIR/.." && pwd)"
UNIT_DIR="${SUBYARD_KEYS_SYSTEMD_DIR:-$SUBYARD_OPERATOR_HOME/.config/systemd/user}"
SERVICE="$UNIT_DIR/subyard-keys-sync.service"
TIMER="$UNIT_DIR/subyard-keys-sync.timer"
RECEIPT="$UNIT_DIR/.subyard-keys-sync.json"
SERVICE_TEMPLATE="$REPO/config/systemd/subyard-keys-sync.service.in"
TIMER_TEMPLATE="$REPO/config/systemd/subyard-keys-sync.timer.in"
SKIP_ENABLE="${SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE:-0}"
RUNTIME_ROOT="${YARD_RUNTIME_ROOT:-$SUBYARD_HOME/runtime}"
YARD_BIN="$RUNTIME_ROOT/current/bin/yard"
DEVELOPMENT_YARD_BIN="$REPO/.build/yard"
if [ ! -x "$YARD_BIN" ] && [ -f "$DEVELOPMENT_YARD_BIN" ] \
  && [ ! -L "$DEVELOPMENT_YARD_BIN" ] && [ -x "$DEVELOPMENT_YARD_BIN" ]; then
  YARD_BIN="$DEVELOPMENT_YARD_BIN"
fi
if [ "$SKIP_ENABLE" = 1 ] && [ ! -x "$YARD_BIN" ]; then
  YARD_BIN="$REPO/bin/yard"
fi
[ -x "$YARD_BIN" ] \
  || die "yard runtime is missing — run the Subyard release installer (or build the development candidate)"

render_service() {
  sed -e "s|@YARD_BIN@|$YARD_BIN|g" "$SERVICE_TEMPLATE"
}

render_receipt() {
  jq -n --arg yard "$YARD_BIN" '{schema:1,yard:$yard}'
}

publish_receipt() {
  local temporary
  temporary="$(mktemp "$UNIT_DIR/.keys-sync-receipt.XXXXXX")"
  render_receipt > "$temporary"
  chmod 0600 "$temporary"
  mv -f -- "$temporary" "$RECEIPT"
}

receipt_current() {
  [ -f "$RECEIPT" ] && [ ! -L "$RECEIPT" ] \
    && [ "$(stat -c '%a:%u' "$RECEIPT")" = "600:$(id -u)" ] \
    && cmp -s <(render_receipt) "$RECEIPT"
}

units_owned() {
  # A protected receipt records a prior explicit install. Without one, only
  # the exact shipped legacy recipes establish ownership; names alone do not.
  [ -e "$SERVICE" ] || [ -e "$TIMER" ] || return 1
  [ ! -L "$UNIT_DIR" ] && [ ! -L "$SERVICE" ] && [ ! -L "$TIMER" ] && [ ! -L "$RECEIPT" ] || return 1
  local path
  for path in "$SERVICE" "$TIMER"; do
    if [ -e "$path" ]; then
      [ -f "$path" ] && [ "$(stat -c '%a:%u' "$path")" = "644:$(id -u)" ] || return 1
    fi
  done
  if [ -e "$RECEIPT" ]; then
    receipt_current
  else
    { [ ! -e "$SERVICE" ] || cmp -s <(render_service) "$SERVICE"; } \
      && { [ ! -e "$TIMER" ] || cmp -s "$TIMER_TEMPLATE" "$TIMER"; }
  fi
}

user_systemctl() {
  local runtime_dir bus_address
  runtime_dir="${XDG_RUNTIME_DIR:-${SUBYARD_KEYS_RUNTIME_DIR:-/run/user/$(id -u)}}"
  bus_address="${DBUS_SESSION_BUS_ADDRESS:-unix:path=$runtime_dir/bus}"
  XDG_RUNTIME_DIR="$runtime_dir" DBUS_SESSION_BUS_ADDRESS="$bus_address" systemctl --user "$@"
}

wait_for_user_manager() {
  local attempts=0
  while [ "$attempts" -lt 50 ]; do
    user_systemctl show-environment >/dev/null 2>&1 && return 0
    attempts=$((attempts + 1))
    sleep 0.1
  done
  return 1
}

ensure_user_systemd_manager() {
  local uid user unit
  wait_for_user_manager && return 0
  uid="$(id -u)"
  user="$(id -un)"
  unit="user@$uid.service"
  if [ "$uid" -eq 0 ]; then
    systemctl start "$unit" || die "could not start the user systemd manager for $user"
  elif command -v sudo >/dev/null 2>&1; then
    host_sudo systemctl start "$unit" || die "could not start the user systemd manager for $user"
  else
    die "the user systemd manager is unavailable and sudo is missing"
  fi
  wait_for_user_manager || die "the user systemd manager for $user has no runtime bus"
}

units_current() {
  [ ! -L "$UNIT_DIR" ] && [ ! -L "$SERVICE" ] && [ ! -L "$TIMER" ] \
    && [ -r "$SERVICE" ] && [ -r "$TIMER" ] \
    && [ "$(stat -c '%a:%u' "$SERVICE")" = "644:$(id -u)" ] \
    && [ "$(stat -c '%a:%u' "$TIMER")" = "644:$(id -u)" ] \
    && cmp -s <(render_service) "$SERVICE" && cmp -s "$TIMER_TEMPLATE" "$TIMER"
}

timer_enabled() {
  [ "$SKIP_ENABLE" = 1 ] && return 0
  user_systemctl is-enabled --quiet subyard-keys-sync.timer 2>/dev/null || return 1
  command -v loginctl >/dev/null 2>&1 || return 1
  [ "$(loginctl show-user "$(id -un)" -p Linger --value 2>/dev/null || true)" = yes ] || return 1
}

units_loaded_current() {
  [ "$SKIP_ENABLE" = 1 ] && return 0
  # A stopped manager loads these files on its next explicit start.
  user_systemctl show-environment >/dev/null 2>&1 || return 0
  [ "$(user_systemctl show subyard-keys-sync.service --property=NeedDaemonReload --value)" = no ] \
    && [ "$(user_systemctl show subyard-keys-sync.timer --property=NeedDaemonReload --value)" = no ]
}

case "${1:-}" in
  --check) units_current && timer_enabled; exit ;;
  --check-owned) units_owned; exit ;;
  --check-refresh) receipt_current && units_owned && units_current && units_loaded_current; exit ;;
  --refresh)
    # Release activation updates an existing timer without enabling or starting it.
    [ -e "$SERVICE" ] || [ -e "$TIMER" ] || die "credential timer is not installed"
    [ ! -L "$UNIT_DIR" ] && [ ! -L "$SERVICE" ] && [ ! -L "$TIMER" ] || die "unsafe credential timer managed path"
    units_owned || die "credential timer files are not owned by Subyard"
    if units_current && units_loaded_current; then
      [ -e "$RECEIPT" ] || publish_receipt
      exit 0
    fi
    if ! units_current; then
      install -d -m 700 "$UNIT_DIR"
      render_service > "$SERVICE.tmp"; chmod 0644 "$SERVICE.tmp"; mv -f "$SERVICE.tmp" "$SERVICE"
      install -m 0644 "$TIMER_TEMPLATE" "$TIMER.tmp"; mv -f "$TIMER.tmp" "$TIMER"
    fi
    [ -e "$RECEIPT" ] || publish_receipt
    # A stopped user manager loads the refreshed files when explicitly resumed.
    if user_systemctl show-environment >/dev/null 2>&1; then user_systemctl daemon-reload; fi
    exit ;;
  -h|--help)
    cat <<EOF
Usage: install-keys-auto-sync.sh [--check] [-y]

Install and enable a persistent operator-owned timer that runs encrypted credential sync every 6 hours.
EOF
    exit 0 ;;
esac

if units_current && timer_enabled; then
  ok "encrypted credential auto-sync timer already installed"
  exit 0
fi

announce "Enable automatic encrypted credential synchronization" \
  "Install user units under $UNIT_DIR." \
  "Attempt synchronization every 6 hours with a persistent boot catch-up and randomized delay." \
  "Enable systemd lingering for $(id -un), if needed, so the timer continues after logout." \
  "Only peers approved by 'yard keys trust' participate; no recipient is enrolled automatically."
proceed_or_die

install -d -m 700 "$UNIT_DIR"
render_service > "$SERVICE.tmp"; chmod 0644 "$SERVICE.tmp"; mv -f "$SERVICE.tmp" "$SERVICE"
install -m 0644 "$TIMER_TEMPLATE" "$TIMER.tmp"; mv -f "$TIMER.tmp" "$TIMER"
publish_receipt

if [ "$SKIP_ENABLE" = 1 ]; then
  ok "installed credential auto-sync unit files (enable skipped by test override)"
  exit 0
fi
command -v systemctl >/dev/null 2>&1 || die "systemctl is required for the automatic credential sync timer"
command -v loginctl >/dev/null 2>&1 || die "loginctl is required to keep automatic credential sync running after logout"
linger="$(loginctl show-user "$(id -un)" -p Linger --value 2>/dev/null || true)"
if [ "$linger" != yes ]; then
  if [ "$(id -u)" -eq 0 ]; then
    loginctl enable-linger "$(id -un)" || die "could not enable systemd lingering"
  elif command -v sudo >/dev/null 2>&1; then
    host_sudo loginctl enable-linger "$(id -un)" || die "could not enable systemd lingering"
  else
    die "systemd lingering is disabled and sudo is unavailable; the 24-hour sync bound cannot be installed"
  fi
fi
# Linger may be enabled while user@UID.service is still stopped.
ensure_user_systemd_manager
user_systemctl daemon-reload || die "could not reload the user systemd manager"
user_systemctl enable --now subyard-keys-sync.timer \
  || die "could not enable subyard-keys-sync.timer"
ok "enabled subyard-keys-sync.timer (6-hour interval, persistent catch-up)"
