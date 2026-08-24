#!/usr/bin/env bash
# 03-create-subyard.sh — Phase 2: create the yard instance, pass /dev/kvm, attach /srv volume.
# Operator (incus-admin, no sudo). Idempotent.
# Config: config/incus.project.env + config/subyard.env + config/host.env.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/runtime.sh
. "$SCRIPT_DIR/lib/runtime.sh"
# shellcheck source=scripts/lib/engine-context.sh
. "$SCRIPT_DIR/lib/engine-context.sh"
subyard_require_engine_context
# shellcheck source=scripts/lib/ui.sh
. "$SCRIPT_DIR/lib/ui.sh"
# shellcheck source=scripts/lib-power.sh
. "$SCRIPT_DIR/lib-power.sh"
# shellcheck source=scripts/lib/host.sh
. "$SCRIPT_DIR/lib/host.sh"

INCUS_PROJECT="${INCUS_PROJECT:-subyard}"
YARD_INSTANCE_NAME="${YARD_INSTANCE_NAME:-yard}"
YARD_KIND="${YARD_KIND:-container}"
YARD_IMAGE="${YARD_IMAGE:-images:debian/13}"
YARD_IMAGE_FALLBACK="${YARD_IMAGE_FALLBACK:-images:ubuntu/24.04}"
SRV_POOL="${SRV_POOL:-default}"
SRV_VOLUME="${SRV_VOLUME:-yard-srv}"
DEV_USER="${DEV_USER:-dev}"
BRIDGE="${INCUS_BRIDGE:-${INCUS_NETWORK:-incusbr0}}"
YARD_LABEL="${YARD_NAME:-default}"
desired_power="${SUBYARD_POWER_DESIRED:-}"
case "$desired_power" in running | stopped) ;; *) die "prepared desired power is required" ;; esac

PROJ=(--project "$INCUS_PROJECT")
device_exists() { incus config device list "$YARD_INSTANCE_NAME" "${PROJ[@]}" 2>/dev/null | grep -qx "$1"; }
device_get() { incus config device get "$YARD_INSTANCE_NAME" "$1" "$2" "${PROJ[@]}" 2>/dev/null || true; }
instance_get() { incus config get "$YARD_INSTANCE_NAME" "$1" "${PROJ[@]}" 2>/dev/null || true; }
incus_apparmor_disabled() {
  local service_environment
  command -v systemctl >/dev/null 2>&1 || return 1
  service_environment="$(systemctl show incus.service -p Environment --value 2>/dev/null)" \
    || return 1
  case " $service_environment " in
    *' INCUS_SECURITY_APPARMOR=false '*) return 0 ;;
  esac
  return 1
}

reconcile_e2e_route_mount() {
  # Do not mount below /run: systemd mounts its tmpfs there during container
  # boot and would hide an Incus disk device attached before startup.
  local source="$SUBYARD_HOME/e2e/routes" target=/var/lib/subyard/e2e-routes
  install -d -m 0755 "$source"
  if device_exists subyard-e2e-routes; then
    if [ "$(device_get subyard-e2e-routes type)" = disk ] \
      && [ "$(device_get subyard-e2e-routes source)" = "$source" ] \
      && [ "$(device_get subyard-e2e-routes path)" = "$target" ] \
      && [ "$(device_get subyard-e2e-routes readonly)" = true ]; then
      ok "shared E2E routes → $target unchanged"
      return
    fi
    incus config device remove "$YARD_INSTANCE_NAME" subyard-e2e-routes "${PROJ[@]}" >/dev/null
  fi
  incus config device add "$YARD_INSTANCE_NAME" subyard-e2e-routes disk "${PROJ[@]}" \
    source="$source" path="$target" readonly=true
  ok "shared E2E routes → $target (read-only)"
}

# --- preconditions -----------------------------------------------------------
incus_preflight
incus project show "$INCUS_PROJECT" >/dev/null 2>&1 \
  || die "project '$INCUS_PROJECT' missing — run scripts/02-create-project.sh first"

announce_confirm "Subyard Phase 2 — create yard instance" \
  "Create Incus instance '$YARD_INSTANCE_NAME' ($YARD_KIND) from $YARD_IMAGE (fallback $YARD_IMAGE_FALLBACK)." \
  "Pass /dev/kvm through (container) and attach a persistent '$SRV_VOLUME' volume at /srv." \
  "Reversible: 'incus delete -f $YARD_INSTANCE_NAME ${PROJ[*]}' removes it."
power_nm_prepare_reader || die "$POWER_ERROR"

# --- 1. create instance (idempotent) -----------------------------------------
echo "Instance:"
LAUNCH_FLAGS=()
if [ "$YARD_KIND" = vm ]; then
  LAUNCH_FLAGS+=(--vm)
  # qemu-system only for vm mode — installed lazily.
  if ! dpkg -s qemu-system-x86 >/dev/null 2>&1 && ! command -v qemu-system-x86_64 >/dev/null 2>&1; then
    die "vm mode needs qemu — install it and re-run: sudo apt-get install qemu-system-x86"
  fi
else
  LAUNCH_FLAGS+=(-c security.nesting=true)
  if [ "${NESTED_E2E_VMS:-0}" = 1 ]; then
    LAUNCH_FLAGS+=(
      -c security.syscalls.intercept.bpf=true
      -c security.syscalls.intercept.bpf.devices=true
    )
  fi
fi
[ -n "${LIMITS_CPU:-}" ]    && LAUNCH_FLAGS+=(-c "limits.cpu=$LIMITS_CPU")
[ -n "${LIMITS_MEMORY:-}" ] && LAUNCH_FLAGS+=(-c "limits.memory=$LIMITS_MEMORY")

if incus info "$YARD_INSTANCE_NAME" "${PROJ[@]}" >/dev/null 2>&1; then
  ok "instance '$YARD_INSTANCE_NAME' exists"
  if [ "$YARD_KIND" = container ] \
    && [ "$(incus config get "$YARD_INSTANCE_NAME" security.nesting "${PROJ[@]}" 2>/dev/null || true)" != true ]; then
    incus config set "$YARD_INSTANCE_NAME" security.nesting true "${PROJ[@]}"
    ok "reconciled security.nesting=true"
  fi
else
  LAUNCH_FLAGS+=(
    -c boot.autostart=false
    -c "$POWER_KEY_MANAGED=true"
    -c "$POWER_KEY_NAME=$YARD_LABEL"
    -c "$POWER_KEY_BRIDGE=$BRIDGE"
    -c "$POWER_KEY_DESIRED=$desired_power"
    -c "$POWER_KEY_INITIALIZED=false"
  )
  info "creating $YARD_INSTANCE_NAME from $YARD_IMAGE"
  if err="$(incus init "$YARD_IMAGE" "$YARD_INSTANCE_NAME" "${PROJ[@]}" "${LAUNCH_FLAGS[@]}" 2>&1)"; then
    ok "created $YARD_INSTANCE_NAME"
  elif incus info "$YARD_INSTANCE_NAME" "${PROJ[@]}" >/dev/null 2>&1; then
    warn "instance '$YARD_INSTANCE_NAME' was created with an initialization warning:"
    printf '%s\n' "$err" >&2
  elif printf '%s' "$err" | grep -qiE 'image|not found|no such|remote'; then
    # Only the base image looks missing — try the fallback. Other failures (e.g. a
    # missing root device) would just repeat, so surface them instead of retrying.
    warn "create from $YARD_IMAGE failed (image unavailable); trying fallback $YARD_IMAGE_FALLBACK"
    incus init "$YARD_IMAGE_FALLBACK" "$YARD_INSTANCE_NAME" "${PROJ[@]}" "${LAUNCH_FLAGS[@]}" \
      || die "instance creation failed (check image remotes and YARD_KIND)"
    ok "created $YARD_INSTANCE_NAME (fallback image)"
  else
    printf '%s\n' "$err" >&2
    die "instance creation failed"
  fi
fi

# Every yard receives the same non-secret route/host-key registry. The test-vms backend writes it
# on the owner host; no project enrollment or checkout-local artifact is involved.
reconcile_e2e_route_mount

# When the Incus daemon deliberately has AppArmor disabled, an unprivileged yard still sees the
# host kernel's AppArmor indicator but cannot read its securityfs profile inventory. Docker then
# mistakes that partial visibility for usable AppArmor and refuses to start any container. Hide
# only the indicator in that compatibility mode; on ordinary AppArmor-enabled hosts Docker keeps
# its normal docker-default confinement.
docker_apparmor_device=subyard-docker-apparmor
docker_apparmor_required=0
if [ "$YARD_KIND" = container ] && incus_apparmor_disabled; then
  docker_apparmor_required=1
fi
docker_apparmor_drift=0
if [ "$docker_apparmor_required" = 1 ]; then
  device_exists "$docker_apparmor_device" \
    && [ "$(device_get "$docker_apparmor_device" type)" = disk ] \
    && [ "$(device_get "$docker_apparmor_device" source)" = /dev/null ] \
    && [ "$(device_get "$docker_apparmor_device" path)" = /sys/module/apparmor/parameters/enabled ] \
    && [ "$(device_get "$docker_apparmor_device" readonly)" = true ] \
    || docker_apparmor_drift=1
elif device_exists "$docker_apparmor_device"; then
  docker_apparmor_drift=1
fi

if [ "$docker_apparmor_drift" = 1 ] \
  && [ "$(power_state "$INCUS_PROJECT" "$YARD_INSTANCE_NAME")" = RUNNING ]; then
  warn "Docker AppArmor compatibility changed — a guarded yard restart is required"
  "$SCRIPT_DIR/lifecycle-guard.sh" stop --reconcile \
    || die "could not safely stop the yard; close active SSH/VS Code sessions and re-run '$(yard_cmd_hint) init'"
fi
if [ "$docker_apparmor_required" = 1 ]; then
  if device_exists "$docker_apparmor_device" && [ "$docker_apparmor_drift" = 1 ]; then
    incus config device remove "$YARD_INSTANCE_NAME" "$docker_apparmor_device" "${PROJ[@]}" >/dev/null
  fi
  if ! device_exists "$docker_apparmor_device"; then
    incus config device add "$YARD_INSTANCE_NAME" "$docker_apparmor_device" disk "${PROJ[@]}" \
      source=/dev/null path=/sys/module/apparmor/parameters/enabled readonly=true >/dev/null
    ok "Docker sees AppArmor as disabled with the Incus daemon"
  fi
elif device_exists "$docker_apparmor_device"; then
  incus config device remove "$YARD_INSTANCE_NAME" "$docker_apparmor_device" "${PROJ[@]}" >/dev/null
  ok "restored Docker AppArmor detection"
fi

# --- 2. trusted nested-VM capability (container only, opt-in) ----------------
# These settings belong to the L0/L1 boundary and must be present before the
# container starts. Existing running yards are stopped through the normal
# VS Code/SSH safety guard before a non-live setting is changed.
ensure_char_device() { # <name> <source>
  local name="$1" source="$2" err
  [ -c "$source" ] || die "NESTED_E2E_VMS=1 requires character device $source on the L0 host"
  if device_exists "$name"; then
    if [ "$(device_get "$name" type)" = unix-char ] \
      && [ "$(device_get "$name" source)" = "$source" ] \
      && [ "$(device_get "$name" path)" = "$source" ]; then
      ok "$name → $source unchanged"
      return
    fi
    incus config device remove "$YARD_INSTANCE_NAME" "$name" "${PROJ[@]}" >/dev/null
  fi
  if ! err="$(incus config device add "$YARD_INSTANCE_NAME" "$name" unix-char "${PROJ[@]}" \
        source="$source" path="$source" mode=0660 2>&1 >/dev/null)"; then
    case "$err" in
      *"nested container"*)
        incus config device add "$YARD_INSTANCE_NAME" "$name" unix-char "${PROJ[@]}" \
          source="$source" path="$source" >/dev/null
        warn "nested host: $name attached without an explicit mode" ;;
      *) printf '%s\n' "$err" >&2; die "could not attach $source for nested E2E VMs" ;;
    esac
  fi
  ok "$name → $source"
}

nested_drift=0
if [ "${NESTED_E2E_VMS:-0}" = 1 ]; then
  [ "$(instance_get security.syscalls.intercept.bpf)" = true ] || nested_drift=1
  [ "$(instance_get security.syscalls.intercept.bpf.devices)" = true ] || nested_drift=1
  for pair in 'kvm:/dev/kvm' 'e2e-vsock:/dev/vsock' 'e2e-vhost-vsock:/dev/vhost-vsock' 'e2e-tun:/dev/net/tun'; do
    name="${pair%%:*}"; source="${pair#*:}"
    device_exists "$name" \
      && [ "$(device_get "$name" type)" = unix-char ] \
      && [ "$(device_get "$name" source)" = "$source" ] \
      && [ "$(device_get "$name" path)" = "$source" ] \
      || nested_drift=1
  done
else
  [ -z "$(instance_get security.syscalls.intercept.bpf)" ] || nested_drift=1
  [ -z "$(instance_get security.syscalls.intercept.bpf.devices)" ] || nested_drift=1
  device_exists e2e-vsock && nested_drift=1
  device_exists e2e-vhost-vsock && nested_drift=1
  device_exists e2e-tun && nested_drift=1
fi

if [ "$nested_drift" = 1 ] \
  && [ "$(power_state "$INCUS_PROJECT" "$YARD_INSTANCE_NAME")" = RUNNING ]; then
  warn "nested E2E VM boundary changed — a guarded yard restart is required"
  "$SCRIPT_DIR/lifecycle-guard.sh" stop --reconcile \
    || die "could not safely stop the yard; close active SSH/VS Code sessions and re-run '$(yard_cmd_hint) init'"
fi

if [ "${NESTED_E2E_VMS:-0}" = 1 ]; then
  incus config set "$YARD_INSTANCE_NAME" security.syscalls.intercept.bpf true "${PROJ[@]}"
  incus config set "$YARD_INSTANCE_NAME" security.syscalls.intercept.bpf.devices true "${PROJ[@]}"
  ensure_char_device kvm /dev/kvm
  ensure_char_device e2e-vsock /dev/vsock
  ensure_char_device e2e-vhost-vsock /dev/vhost-vsock
  ensure_char_device e2e-tun /dev/net/tun
else
  incus config unset "$YARD_INSTANCE_NAME" security.syscalls.intercept.bpf "${PROJ[@]}" 2>/dev/null || true
  incus config unset "$YARD_INSTANCE_NAME" security.syscalls.intercept.bpf.devices "${PROJ[@]}" 2>/dev/null || true
  for name in e2e-vsock e2e-vhost-vsock e2e-tun; do
    device_exists "$name" || continue
    incus config device remove "$YARD_INSTANCE_NAME" "$name" "${PROJ[@]}" >/dev/null
    ok "removed disabled nested-VM device '$name'"
  done
fi

# --- 3. /dev/kvm passthrough (container only) --------------------------------
echo "KVM:"
if [ "$YARD_KIND" = vm ]; then
  ok "vm mode uses nested virtualization — no unix-char passthrough"
elif device_exists kvm; then
  ok "kvm device already attached"
elif [ -e /dev/kvm ]; then
  # Nested hosts (this host is itself a container) reject the mode property on unix-char
  # devices — retry without it.
  if ! err="$(incus config device add "$YARD_INSTANCE_NAME" kvm unix-char "${PROJ[@]}" \
        source=/dev/kvm path=/dev/kvm mode=0660 2>&1 >/dev/null)"; then
    case "$err" in
      *"nested container"*)
        incus config device add "$YARD_INSTANCE_NAME" kvm unix-char "${PROJ[@]}" \
          source=/dev/kvm path=/dev/kvm >/dev/null
        warn "nested host: /dev/kvm attached without an explicit mode" ;;
      *) printf '%s\n' "$err" >&2; die "could not attach /dev/kvm" ;;
    esac
  fi
  ok "added /dev/kvm passthrough (gid fix deferred to Phase 3, after group 'kvm' exists)"
else
  warn "/dev/kvm absent on host — emulator won't be hardware-accelerated; skipping passthrough"
fi

# --- 4. persistent /srv volume (idempotent) ----------------------------------
echo "Storage (/srv):"
if incus storage volume show "$SRV_POOL" "$SRV_VOLUME" "${PROJ[@]}" >/dev/null 2>&1; then
  ok "volume '$SRV_VOLUME' exists"
else
  incus storage volume create "$SRV_POOL" "$SRV_VOLUME" "${PROJ[@]}" >/dev/null
  ok "created volume '$SRV_VOLUME' on pool '$SRV_POOL'"
fi
srv_drifted=0
if device_exists srv; then
  [ "$(incus config device get "$YARD_INSTANCE_NAME" srv pool "${PROJ[@]}" 2>/dev/null || true)" = "$SRV_POOL" ] \
    && [ "$(incus config device get "$YARD_INSTANCE_NAME" srv source "${PROJ[@]}" 2>/dev/null || true)" = "$SRV_VOLUME" ] \
    && [ "$(incus config device get "$YARD_INSTANCE_NAME" srv path "${PROJ[@]}" 2>/dev/null || true)" = /srv ] \
    || srv_drifted=1
  if [ "$srv_drifted" = 0 ]; then
    ok "srv device already attached"
  else
    warn "srv device drifted — re-attaching to '$SRV_VOLUME' at /srv"
    incus config device remove "$YARD_INSTANCE_NAME" srv "${PROJ[@]}" >/dev/null
  fi
fi
if ! device_exists srv; then
  incus config device add "$YARD_INSTANCE_NAME" srv disk "${PROJ[@]}" \
    pool="$SRV_POOL" source="$SRV_VOLUME" path=/srv >/dev/null
  ok "attached '$SRV_VOLUME' at /srv"
fi

# Attach boot devices before the first start. Incus 6.0 cannot hot-add virtiofs to a running VM
# when its first PCI function is already occupied by the balloon device.
state="$(power_state "$INCUS_PROJECT" "$YARD_INSTANCE_NAME")"
[ "$state" = RUNNING ] || info "starting $YARD_INSTANCE_NAME temporarily (was: ${state:-unknown})"
power_start_guarded "$INCUS_PROJECT" "$YARD_INSTANCE_NAME" "$BRIDGE" || die "$POWER_ERROR"

# --- summary -----------------------------------------------------------------
echo
ok "Phase 2 (instance) done."
cat <<MSG

Verify:
  incus list "${PROJ[@]}"                  # $YARD_INSTANCE_NAME is RUNNING
  incus exec $YARD_INSTANCE_NAME "${PROJ[@]}" -- ls -l /dev/kvm   # present (container)

Next:
  - scripts/05-mount-host-paths.sh   (host dirs under $HOST_BASE + /mnt/host/* mounts)
  - Phase 3 provisioning (packages, user '$DEV_USER', Docker, SSH) + kvm gid fix
MSG
