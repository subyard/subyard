#!/usr/bin/env bash
# 05-mount-host-paths.sh — Phase 2: create $HOST_BASE (/srv/subyard) and reconcile the
# yard's host mounts to the declarative HOST_MOUNTS list (config/host.env): attach
# missing, re-attach drifted, detach de-listed host-* mounts. Root; idempotent; a
# re-run applies config changes. Host exposes ONLY $HOST_BASE. SHIFT_MODE=shift|acl.
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
# shellcheck source=scripts/lib/host.sh
. "$SCRIPT_DIR/lib/host.sh"

INCUS_PROJECT="${INCUS_PROJECT:-subyard}"
YARD_INSTANCE_NAME="${YARD_INSTANCE_NAME:-yard}"
YARD_KIND="${YARD_KIND:-container}"
SHIFT_MODE="${SHIFT_MODE:-shift}"
DEV_USER="${DEV_USER:-dev}"
DEV_UID="${DEV_UID:-1000}"
# HOST_BASE and the declarative HOST_MOUNTS list come from config/host.env (loaded
# above). Lines: "<name>:<yard-path>:<ro|rw>:<dir-mode>".

PROJ=(--project "$INCUS_PROJECT")
device_exists() { incus config device list "$YARD_INSTANCE_NAME" "${PROJ[@]}" 2>/dev/null | grep -qx "$1"; }
dev_get() { incus config device get "$YARD_INSTANCE_NAME" "$1" "$2" "${PROJ[@]}" 2>/dev/null || true; }

# Parse HOST_MOUNTS once into parallel arrays; build the announce summary too.
m_name=(); m_path=(); m_ro=(); m_mode=(); mount_summary=()
while IFS=: read -r _n _p _ro _mode; do
  [ -n "$_n" ] || continue
  m_name+=("$_n"); m_path+=("$_p"); m_ro+=("$_ro"); m_mode+=("${_mode:-0755}")
  mount_summary+=("$_p (${_ro:-rw}) ← $HOST_BASE/$_n")
done < <(printf '%s\n' "$HOST_MOUNTS" | sed 's/[[:space:]]//g')

# --- announce → confirm → sudo → checks → work -------------------------------
announce "Subyard Phase 2 — host mounts ($YARD_INSTANCE_NAME)" \
  "Create the narrow host area under $HOST_BASE (+ a host-side backups dir)." \
  "Reconcile host mounts → yard to HOST_MOUNTS: ${mount_summary[*]}." \
  "Detach any host-* mount no longer listed (the yard is rebuildable; the host is untouched)." \
  "Own shared dirs by uid $DEV_UID and mount with '$SHIFT_MODE' so they map 1:1 to '$DEV_USER' in the yard." \
  "The host exposes ONLY $HOST_BASE — no \$HOME/.ssh/etc."
proceed_or_die
require_root "the steps above create directories under /srv and attach host mounts to the yard"

incus_preflight
incus info "$YARD_INSTANCE_NAME" "${PROJ[@]}" >/dev/null 2>&1 \
  || die "instance '$YARD_INSTANCE_NAME' missing — run scripts/03-create-subyard.sh first"

# shift vs acl
case "$SHIFT_MODE" in
  shift) SHIFT_OPT="shift=true" ;;
  acl)   SHIFT_OPT=""; warn "SHIFT_MODE=acl — mounts added without shift; apply POSIX ACLs separately" ;;
  *)     die "invalid SHIFT_MODE='$SHIFT_MODE' (expected: shift|acl)" ;;
esac
if [ "$YARD_KIND" = vm ]; then
  warn "vm mode uses virtiofs — 'shift' is not applicable; review host-file ownership and permissions before use"
fi

# --- 1. create the narrow host area ------------------------------------------
echo "Host directories:"
# Shared dirs are owned by DEV_UID so the idmapped ('shift') mount maps them to the
# yard's 'dev' (host uid == container uid under shift). install -d also fixes an
# existing dir's owner/mode, so this self-heals dirs left root-owned by an older run.
for i in "${!m_name[@]}"; do
  install -d -m "${m_mode[$i]}" -o "$DEV_UID" -g "$DEV_UID" "$HOST_BASE/${m_name[$i]}"
  ok "$HOST_BASE/${m_name[$i]} (${m_mode[$i]}, owner $DEV_UID)"
done
# 'backups' is a host-side backup target, never mounted into the yard — keep it root.
install -d -m 0755 "$HOST_BASE/backups"
ok "$HOST_BASE/backups (0755, root)"

# --- 2. reconcile host-mount devices to HOST_MOUNTS --------------------------
echo "Host mounts → yard:"
# Add a missing mount, or re-attach one whose source/path/readonly drifted from config.
reconcile_mount() {
  local name="$1" path="$2" ro="$3"
  local src="$HOST_BASE/$name"
  local want_ro=0; [ "$ro" = ro ] && want_ro=1
  if device_exists "$name"; then
    local cur_ro=0; [ "$(dev_get "$name" readonly)" = true ] && cur_ro=1
    if [ "$(dev_get "$name" source)" = "$src" ] && [ "$(dev_get "$name" path)" = "$path" ] \
       && [ "$cur_ro" = "$want_ro" ]; then
      ok "$name → $path (${ro:-rw}) unchanged"; return
    fi
    warn "$name drifted from config — re-attaching"
    incus config device remove "$YARD_INSTANCE_NAME" "$name" "${PROJ[@]}" >/dev/null
  fi
  local opts=(source="$src" path="$path")
  [ "$want_ro" = 1 ] && opts+=(readonly=true)
  [ -n "$SHIFT_OPT" ] && opts+=("$SHIFT_OPT")
  local err
  if ! err="$(incus config device add "$YARD_INSTANCE_NAME" "$name" disk "${PROJ[@]}" "${opts[@]}" 2>&1 >/dev/null)"; then
    # Kernels/hosts without idmapped-mount support (e.g. a nested host) reject shift
    # at ATTACH time with this exact cause — point at the documented fallback instead
    # of leaving a bare incus error.
    case "$err" in
      *idmapping*)
        printf '%s\n' "$err" >&2
        die "this host cannot idmap-shift mounts — set SHIFT_MODE=acl (yard env or environment) and re-run" ;;
      *) printf '%s\n' "$err" >&2; die "could not attach host mount '$name'" ;;
    esac
  fi
  ok "$name → $path (${ro:-rw})"
}
for i in "${!m_name[@]}"; do
  reconcile_mount "${m_name[$i]}" "${m_path[$i]}" "${m_ro[$i]}"
done
# Detach host-* mounts that are no longer in HOST_MOUNTS (yard is rebuildable; the
# host is never touched). Scoped to the 'host-*' naming so project workspace devices
# are never affected.
wanted=" ${m_name[*]} "
while IFS= read -r dev; do
  case "$dev" in host-*) ;; *) continue ;; esac
  case "$wanted" in *" $dev "*) continue ;; esac
  warn "detaching '$dev' — no longer in HOST_MOUNTS (source $(dev_get "$dev" source))"
  incus config device remove "$YARD_INSTANCE_NAME" "$dev" "${PROJ[@]}" >/dev/null
done < <(incus config device list "$YARD_INSTANCE_NAME" "${PROJ[@]}" 2>/dev/null)
# 'backups' stays host-side only (backup target) — not mounted into the yard.

# --- summary -----------------------------------------------------------------
echo
ok "Phase 2 (host mounts) done."
cat <<MSG

Verify:
  incus exec $YARD_INSTANCE_NAME "${PROJ[@]}" -- ls -la /mnt/host
  incus exec $YARD_INSTANCE_NAME "${PROJ[@]}" -- touch /mnt/host/secrets/x  # must FAIL (read-only)

Next:
  - Phase 3 provisioning: packages, user '$DEV_USER', Docker, SSH, then the kvm gid fix.
MSG
