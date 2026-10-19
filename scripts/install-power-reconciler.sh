#!/usr/bin/env bash
# install-power-reconciler.sh — install/remove the root-owned host boot power reconciler.
# Internal lifecycle helper; `yard init` installs it and the last `yard teardown` removes it.
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

LIBEXEC_DIR="${SUBYARD_POWER_LIBEXEC_DIR:-/usr/local/libexec/subyard}"
RECONCILER_PATH="${SUBYARD_POWER_RECONCILER_PATH:-$LIBEXEC_DIR/yard-boot-reconcile}"
ENGINE_SOURCE="${SUBYARD_POWER_ENGINE_SOURCE:-}"
UNIT_PATH="${SUBYARD_POWER_UNIT_PATH:-/etc/systemd/system/subyard-power-reconcile.service}"
UNIT_NAME="$(basename "$UNIT_PATH")"
TEMPLATE="$SCRIPT_DIR/../config/systemd/subyard-power-reconcile.service.in"

action=install
for arg in "$@"; do
  case "$arg" in
    --remove-if-unused) action=remove-if-unused ;;
    --native-fence) action=native-fence ;;
    -y|--yes) ;;
    -*) die "unknown option '$arg'" ;;
  esac
done

if [ "$action" = install ]; then
  announce "Subyard — install guarded yard boot reconciliation" \
    "Install a root-owned systemd service that restores only yards with desired=running after Incus/network guards are ready." \
    "Keep Incus boot.autostart=false; validate NetworkManager and host default routes before and after every yard start."
else
  announce "Subyard — remove unused yard boot reconciliation" \
    "Disable and remove the host boot reconciler only when no managed local yard remains."
fi
proceed_or_die
require_root "installing a host systemd unit and root-owned reconciler needs root"
# shellcheck source=scripts/lib/native-install-custody.sh
. "$SCRIPT_DIR/lib/native-install-custody.sh"
subyard_install_custody host-power "$LIBEXEC_DIR" "$RECONCILER_PATH" "$UNIT_PATH"
if [ "$action" = native-fence ]; then
  [ -n "${SUBYARD_NATIVE_SUCCESSOR:-}" ] || die 'native fence successor is required'
  subyard_install_custody_verify host-power "$LIBEXEC_DIR"
  exit 0
fi

if [ "$action" = remove-if-unused ]; then
  # shellcheck source=scripts/lib/teardown-services.sh
  . "$SCRIPT_DIR/lib/teardown-services.sh"
  teardown_capture_producer power
  [ -n "$SUBYARD_TEARDOWN_SERVICES" ] || die "producer inventory is unavailable"
  [ -x "$RECONCILER_PATH" ] || { [ ! -e "$UNIT_PATH" ] || die "boot reconciler ownership is unavailable"; exit 0; }
  if "$RECONCILER_PATH" _power-reconcile has-managed; then
    ok "managed yards remain — keeping $UNIT_NAME"
    exit 0
  else
    rc=$?
    if [ "$rc" -ne 1 ]; then
      warn "managed-yard inventory failed — retaining $UNIT_NAME fail-closed"
      exit 0
    fi
  fi
  teardown_stop_services
  rmdir "$LIBEXEC_DIR" 2>/dev/null || true
  ok "removed unused $UNIT_NAME"
  exit 0
fi

[ -r "$TEMPLATE" ] || die "systemd template missing: $TEMPLATE"
[ -n "$ENGINE_SOURCE" ] || die "SUBYARD_POWER_ENGINE_SOURCE is required"
[ -f "$ENGINE_SOURCE" ] && [ -x "$ENGINE_SOURCE" ] \
  || die "power engine source must be an executable regular file: $ENGINE_SOURCE"
# Carry the pinned candidate through the operator credential boundary using the
# query's own descriptors; another process's /proc directory may be inaccessible.
exec {publication_root_fd}<"$SCRIPT_DIR/.."
exec {publication_engine_fd}<"$ENGINE_SOURCE"
publication_engine="/proc/self/fd/$publication_engine_fd"
[ -f "$publication_engine" ] && [ -x "$publication_engine" ] \
  || die 'opened power engine must be an executable regular file'
if [ -n "${SUBYARD_NATIVE_ENGINE_SHA256:-}" ]; then
  [[ "$SUBYARD_NATIVE_ENGINE_SHA256" =~ ^[a-f0-9]{64}$ ]] \
    && [ "$(sha256sum "$publication_engine" | cut -d ' ' -f1)" = "$SUBYARD_NATIVE_ENGINE_SHA256" ] \
    || die 'prepared host engine changed before installation'
fi
# Read the protected epoch as its approved operator before copying any runtime.
publication_args=(env "SUBYARD_REPOSITORY_ROOT=/proc/self/fd/$publication_root_fd" "SUBYARD_CONFIG_HOME=${SUBYARD_CONFIG_HOME:?}")
if [ "$(id -u)" = 0 ] && [ "${SUBYARD_USER:-root}" != root ]; then
  publication_args=(runuser -u "$SUBYARD_USER" -- "${publication_args[@]}")
fi
publication_contracts="$("${publication_args[@]}" "$publication_engine" _publication-contracts)"
exec {publication_root_fd}<&-
[[ "$publication_contracts" =~ ^[A-Za-z0-9+/]+={0,2}$ ]] || die 'invalid publication contract snapshot'
install -d -o root -g root -m 0755 "$LIBEXEC_DIR" "$(dirname "$UNIT_PATH")"
install -o root -g root -m 0755 "$publication_engine" "$RECONCILER_PATH"
exec {publication_engine_fd}<&-
# Upgrades can reach this helper without running the new host-network stage.
# Managed starts and teardown must work before the next host reboot.
"$RECONCILER_PATH" _network-lock ensure \
  || die "could not initialize the host network policy lock"

tmp="$(mktemp)"
subyard_result=0
trap 'subyard_result=$?; rm -f "$tmp"; subyard_prepared_installed_finish "$subyard_result"' EXIT
sed "s|@SUBYARD_POWER_RECONCILER@|$RECONCILER_PATH|g" "$TEMPLATE" > "$tmp"
# Snapshot owning declarations into the root-owned unit beside its copied engine.
printf '\n[Service]\nEnvironment=SUBYARD_PUBLICATION_CONTRACTS=%s\n' "$publication_contracts" >> "$tmp"
if [ ! -f "$UNIT_PATH" ] || ! cmp -s "$tmp" "$UNIT_PATH"; then
  install -o root -g root -m 0644 "$tmp" "$UNIT_PATH"
  ok "installed $UNIT_PATH"
else
  ok "$UNIT_NAME already current"
fi
systemctl daemon-reload
if ! systemctl is-enabled --quiet "$UNIT_NAME"; then
  systemctl enable "$UNIT_NAME" >/dev/null
fi
ok "$UNIT_NAME enabled (it runs on boot; not started during setup)"
subyard_install_custody_verify host-power "$LIBEXEC_DIR"
