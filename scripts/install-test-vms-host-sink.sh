#!/usr/bin/env bash
# Install the root-owned physical-host sink for bounded test-vms broker records.
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
# shellcheck source=scripts/lib/native-installed-stage.sh
. "$SCRIPT_DIR/lib/native-installed-stage.sh"

LIBEXEC_DIR="${SUBYARD_TEST_VMS_SINK_LIBEXEC_DIR:-/usr/local/libexec/subyard}"
SINK_PATH="${SUBYARD_TEST_VMS_SINK_PATH:-$LIBEXEC_DIR/test-vms-host-sink}"
SERVICE_PATH="${SUBYARD_TEST_VMS_SINK_SERVICE_PATH:-/etc/systemd/system/subyard-test-vms-host-sink.service}"
TIMER_PATH="${SUBYARD_TEST_VMS_SINK_TIMER_PATH:-/etc/systemd/system/subyard-test-vms-host-sink.timer}"
ENGINE_SOURCE="${SUBYARD_TEST_VMS_SINK_ENGINE_SOURCE:-}"
SERVICE_TEMPLATE="$SCRIPT_DIR/../config/systemd/subyard-test-vms-host-sink.service.in"
TIMER_TEMPLATE="$SCRIPT_DIR/../config/systemd/subyard-test-vms-host-sink.timer.in"

for argument in "$@"; do
  case "$argument" in
    -y|--yes) ;;
    -*) die "unknown option '$argument'" ;;
  esac
done

announce "Subyard — install test VM broker host sink" \
  "Install a root-owned bounded spool collector and one-minute timer on this physical owner host." \
  "Write only validated broker events and incidents below $SUBYARD_HOME/logs; do not mount that log root into a yard."
proceed_or_die
require_root "the physical-host broker sink and systemd timer require root"
# shellcheck source=scripts/lib/native-install-custody.sh
. "$SCRIPT_DIR/lib/native-install-custody.sh"
subyard_install_custody test-vms-host-sink "$LIBEXEC_DIR" "$SINK_PATH" "$SERVICE_PATH" "$TIMER_PATH"

[ -n "$ENGINE_SOURCE" ] \
  && [ -f "$ENGINE_SOURCE" ] && [ -x "$ENGINE_SOURCE" ] \
  || die "SUBYARD_TEST_VMS_SINK_ENGINE_SOURCE must be an executable regular file"
if [ -L "$ENGINE_SOURCE" ]; then
  [[ "$ENGINE_SOURCE" =~ ^/proc/[0-9]+/fd/[0-9]+$ ]] && [[ "${SUBYARD_NATIVE_ENGINE_SHA256:-}" =~ ^[a-f0-9]{64}$ ]] \
    || die 'host sink requires its exact prepared engine descriptor'
fi
exec {sink_engine_fd}<"$ENGINE_SOURCE"
sink_engine="/proc/self/fd/$sink_engine_fd"
if [ -n "${SUBYARD_NATIVE_ENGINE_SHA256:-}" ]; then
  [[ "$SUBYARD_NATIVE_ENGINE_SHA256" =~ ^[a-f0-9]{64}$ ]] \
    && [ "$(sha256sum "$sink_engine" | cut -d ' ' -f1)" = "$SUBYARD_NATIVE_ENGINE_SHA256" ] \
    || die 'prepared host sink engine changed before installation'
fi
[ -r "$SERVICE_TEMPLATE" ] && [ -r "$TIMER_TEMPLATE" ] \
  || die "test-vms host sink systemd templates are missing"
case "$SUBYARD_HOME" in
  /*) ;;
  *) die "SUBYARD_HOME must be absolute" ;;
esac
case "$SUBYARD_HOME" in
  *$'\n'*|*'"'*|*'%'*|*'\'*) die "SUBYARD_HOME cannot be represented safely in the systemd unit" ;;
esac
for system_path in "$SINK_PATH" "$SERVICE_PATH" "$TIMER_PATH"; do
  case "$system_path" in
    /*) ;;
    *) die "test-vms host sink paths must be absolute" ;;
  esac
  case "$system_path" in
    *[[:space:]%\"\\]*) die "test-vms host sink path cannot be represented safely: $system_path" ;;
  esac
done

operator_user="${SUBYARD_USER:-${SUDO_USER:-}}"
[ -n "$operator_user" ] || operator_user="$(stat -c '%U' "$SUBYARD_HOME")"
operator_gid="$(id -g "$operator_user")" \
  || die "cannot resolve the Subyard operator group"

install -d -o root -g root -m 0755 "$LIBEXEC_DIR" \
  "$(dirname "$SERVICE_PATH")" "$(dirname "$TIMER_PATH")"
candidate="$(mktemp "$LIBEXEC_DIR/.test-vms-host-sink.XXXXXX")"
subyard_result=0
trap 'subyard_result=$?; rm -f -- "$candidate"; subyard_prepared_installed_finish "$subyard_result"' EXIT
install -o root -g root -m 0755 "$sink_engine" "$candidate"
exec {sink_engine_fd}<&-
mv -f -- "$candidate" "$SINK_PATH"
trap - EXIT

service_candidate="$(mktemp)"
timer_candidate="$(mktemp)"
trap 'subyard_result=$?; rm -f -- "$service_candidate" "$timer_candidate"; subyard_prepared_installed_finish "$subyard_result"' EXIT
sink_replacement="${SINK_PATH//\\/\\\\}"
sink_replacement="${sink_replacement//&/\\&}"
sink_replacement="${sink_replacement//|/\\|}"
home_replacement="${SUBYARD_HOME//\\/\\\\}"
home_replacement="${home_replacement//&/\\&}"
home_replacement="${home_replacement//|/\\|}"
sed \
  -e "s|@SUBYARD_TEST_VMS_HOST_SINK@|$sink_replacement|g" \
  -e "s|@SUBYARD_HOME@|$home_replacement|g" \
  -e "s|@SUBYARD_OPERATOR_GID@|$operator_gid|g" \
  "$SERVICE_TEMPLATE" > "$service_candidate"
cp "$TIMER_TEMPLATE" "$timer_candidate"

install -o root -g root -m 0644 "$service_candidate" "$SERVICE_PATH"
install -o root -g root -m 0644 "$timer_candidate" "$TIMER_PATH"
systemctl daemon-reload
systemctl enable --now "$(basename "$TIMER_PATH")"
ok "test VM broker host sink is installed and its timer is active"
subyard_install_custody_verify test-vms-host-sink "$LIBEXEC_DIR"
