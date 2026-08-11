#!/usr/bin/env bash
# Apply Go-prepared yard-level profile mounts, capabilities and devices.
# Usage: scripts/09-yard-extras.sh [--check] [--yes]
#   --check  read-only: exit 0 only when live extras match the profile union exactly.
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
SHIFT_MODE="${SHIFT_MODE:-shift}"
DEV_UID="${DEV_UID:-1000}"
PROJ=(--project "$INCUS_PROJECT")

check_only=0
for arg in "$@"; do
  case "$arg" in
    --check) check_only=1 ;;
    -y | --yes) ;;
    -h | --help)
      printf 'Usage: %s [--check] [--yes]\n' "${PROG:-yard extras}"
      exit 0 ;;
    *) die "unknown option '$arg'" ;;
  esac
done

device_exists() { incus config device list "$YARD_INSTANCE_NAME" "${PROJ[@]}" 2>/dev/null | grep -qx "$1"; }
dev_get() { incus config device get "$YARD_INSTANCE_NAME" "$1" "$2" "${PROJ[@]}" 2>/dev/null || true; }
cfg_get() { incus config get "$YARD_INSTANCE_NAME" "$1" "${PROJ[@]}" 2>/dev/null || true; }
case "$SHIFT_MODE" in shift) SHIFT_OPT="shift=true" ;; *) SHIFT_OPT="" ;; esac

read -r -a u_mounts <<<"${SUBYARD_EXTRAS_MOUNTS:-}"
read -r -a u_caps <<<"${SUBYARD_EXTRAS_CAPABILITIES:-}"
read -r -a u_devs <<<"${SUBYARD_EXTRAS_DEVICES:-}"

# Resolve the desired device/capability set once. Both --check and reconcile consume this state,
# so profile interpretation has a single owner and cannot drift from init's convergence probe.
need_fuse=0
need_rootless=0
want_devices=""
want_device() { case " $want_devices " in *" $1 "*) ;; *) want_devices+=" $1" ;; esac; }
for entry in ${u_mounts[@]+"${u_mounts[@]}"}; do
  IFS=: read -r mn mp _access _mode <<<"$entry"
  [ -n "$mn" ] && [ -n "$mp" ] && want_device "yx-$mn"
done
for c in ${u_caps[@]+"${u_caps[@]}"}; do
  case "$c" in
    nesting | rootless-docker) need_rootless=1; need_fuse=1 ;;
    fuse) need_fuse=1 ;;
  esac
done
for d in ${u_devs[@]+"${u_devs[@]}"}; do
  case "$d" in
    kvm) [ ! -e /dev/kvm ] || want_device yx-dev-kvm ;;
    fuse) need_fuse=1 ;;
    gpu)
      for node in /dev/dri/renderD*; do
        [ -e "$node" ] && want_device "yx-dev-dri-${node##*/}"
      done ;;
  esac
done
[ "$need_fuse" = 0 ] || [ ! -e /dev/fuse ] || want_device yx-dev-fuse

unix_char_matches() { # <device-name> <host-source>
  [ "$(dev_get "$1" type)" = unix-char ] \
    && [ "$(dev_get "$1" source)" = "$2" ] \
    && [ "$(dev_get "$1" path)" = "$2" ]
}

extras_converged() {
  command -v incus >/dev/null 2>&1 \
    && incus info >/dev/null 2>&1 \
    && incus info "$YARD_INSTANCE_NAME" "${PROJ[@]}" >/dev/null 2>&1 \
    || return 1

  local listed devices entry name path access _mode dev actual_readonly want_readonly shift want_shift node
  listed="$(incus config device list "$YARD_INSTANCE_NAME" "${PROJ[@]}" 2>/dev/null)" || return 1
  devices=" $(printf '%s\n' "$listed" | tr '\n' ' ') "

  for entry in ${u_mounts[@]+"${u_mounts[@]}"}; do
    IFS=: read -r name path access _mode <<<"$entry"
    [ -n "$name" ] && [ -n "$path" ] || continue
    dev="yx-$name"
    case "$devices" in *" $dev "*) ;; *) return 1 ;; esac
    [ "$(dev_get "$dev" type)" = disk ] \
      && [ "$(dev_get "$dev" source)" = "$HOST_BASE/$name" ] \
      && [ "$(dev_get "$dev" path)" = "$path" ] \
      || return 1
    actual_readonly="$(dev_get "$dev" readonly)"; [ "$actual_readonly" = true ] || actual_readonly=false
    want_readonly=false; [ "$access" = ro ] && want_readonly=true
    [ "$actual_readonly" = "$want_readonly" ] || return 1
    shift="$(dev_get "$dev" shift)"; [ "$shift" = true ] || shift=false
    want_shift=false; [ "$SHIFT_MODE" = shift ] && want_shift=true
    [ "$shift" = "$want_shift" ] || return 1
  done

  if [ "$need_fuse" = 1 ] && [ -e /dev/fuse ]; then
    case "$devices" in *' yx-dev-fuse '*) ;; *) return 1 ;; esac
    unix_char_matches yx-dev-fuse /dev/fuse || return 1
  fi
  for d in ${u_devs[@]+"${u_devs[@]}"}; do
    case "$d" in
      kvm)
        [ ! -e /dev/kvm ] && continue
        case "$devices" in *' yx-dev-kvm '*) ;; *) return 1 ;; esac
        unix_char_matches yx-dev-kvm /dev/kvm || return 1 ;;
      gpu)
        for node in /dev/dri/renderD*; do
          [ -e "$node" ] || continue
          dev="yx-dev-dri-${node##*/}"
          case "$devices" in *" $dev "*) ;; *) return 1 ;; esac
          unix_char_matches "$dev" "$node" || return 1
        done ;;
    esac
  done

  if [ "$need_rootless" = 1 ]; then
    [ "$(cfg_get security.idmap.size)" = 1000000 ] \
      && [ "$(cfg_get security.syscalls.intercept.mknod)" = true ] \
      && [ "$(cfg_get security.syscalls.intercept.setxattr)" = true ] \
      || return 1
  else
    [ -z "$(cfg_get security.idmap.size)" ] \
      && [ -z "$(cfg_get security.syscalls.intercept.mknod)" ] \
      && [ -z "$(cfg_get security.syscalls.intercept.setxattr)" ] \
      || return 1
  fi

  while IFS= read -r dev; do
    case "$dev" in yx-*) ;; *) continue ;; esac
    case " $want_devices " in *" $dev "*) ;; *) return 1 ;; esac
  done <<<"$listed"
  return 0
}

if [ "$check_only" = 1 ]; then
  extras_converged
  exit
fi

# --- announce → confirm ------------------------------------------------------
summary=()
[ "${#u_mounts[@]}" -gt 0 ] && summary+=("Mounts onto the yard: ${u_mounts[*]}")
[ "${#u_caps[@]}"   -gt 0 ] && summary+=("Capabilities: ${u_caps[*]} (may need a yard restart)")
[ "${#u_devs[@]}"   -gt 0 ] && summary+=("Devices: ${u_devs[*]}")
[ "${#summary[@]}" -gt 0 ] || summary+=("No extras requested; remove stale extras owned by Subyard.")
announce "Subyard Phase 2b — yard extras requested by projects ($YARD_INSTANCE_NAME)" \
  "Reconcile the UNION of YARD_* across in-yard projects to the shared yard." \
  "${summary[@]}" \
  "Detach stale yx-* devices and clear capabilities no longer requested. The host is untouched."
proceed_or_die

incus_preflight
incus info "$YARD_INSTANCE_NAME" "${PROJ[@]}" >/dev/null 2>&1 \
  || die "instance '$YARD_INSTANCE_NAME' missing — run '$(yard_cmd_hint) init' first"

# --- 1. mounts (yx-* devices, host dirs under HOST_BASE) ---------------------
if [ "${#u_mounts[@]}" -gt 0 ]; then
  echo "Yard extra mounts:"
fi
for entry in ${u_mounts[@]+"${u_mounts[@]}"}; do
  IFS=: read -r mn mp mro mmode <<<"$entry"
  [ -n "$mn" ] && [ -n "$mp" ] || { warn "bad YARD_MOUNTS entry '$entry' — skipping"; continue; }
  dev="yx-$mn"; src="$HOST_BASE/$mn"
  host_sudo install -d -m "${mmode:-0755}" -o "$DEV_UID" -g "$DEV_UID" "$src"
  want_ro=0; [ "$mro" = ro ] && want_ro=1
  want_shift=false; [ "$SHIFT_MODE" = shift ] && want_shift=true
  if device_exists "$dev"; then
    cur_ro=0; [ "$(dev_get "$dev" readonly)" = true ] && cur_ro=1
    cur_shift=false; [ "$(dev_get "$dev" shift)" = true ] && cur_shift=true
    if [ "$(dev_get "$dev" type)" = disk ] \
      && [ "$(dev_get "$dev" source)" = "$src" ] \
      && [ "$(dev_get "$dev" path)" = "$mp" ] \
      && [ "$cur_ro" = "$want_ro" ] \
      && [ "$cur_shift" = "$want_shift" ]; then
      ok "$dev → $mp (${mro:-rw}) unchanged"; continue
    fi
    warn "$dev drifted — re-attaching"
    incus config device remove "$YARD_INSTANCE_NAME" "$dev" "${PROJ[@]}" >/dev/null
  fi
  opts=(source="$src" path="$mp")
  [ "$want_ro" = 1 ] && opts+=(readonly=true)
  [ -n "$SHIFT_OPT" ] && opts+=("$SHIFT_OPT")
  incus config device add "$YARD_INSTANCE_NAME" "$dev" disk "${PROJ[@]}" "${opts[@]}" >/dev/null
  ok "$dev → $mp (${mro:-rw})"
done

# --- 2. capabilities (instance config; some need a restart) ------------------
restart_needed=0
set_cfg() {  # <key> <value> — set if it differs; flag restart
  local k="$1" v="$2"
  [ "$(cfg_get "$k")" = "$v" ] && { ok "$k=$v already"; return; }
  incus config set "$YARD_INSTANCE_NAME" "$k" "$v" "${PROJ[@]}"
  ok "set $k=$v"; restart_needed=1
}
unset_cfg() { # <key> — clear an extras-owned key if it is still present
  local k="$1"
  [ -n "$(cfg_get "$k")" ] || { ok "$k already unset"; return; }
  incus config unset "$YARD_INSTANCE_NAME" "$k" "${PROJ[@]}"
  ok "unset $k"; restart_needed=1
}
for c in ${u_caps[@]+"${u_caps[@]}"}; do
  case "$c" in
    nesting | rootless-docker | fuse) ;;
    *) warn "unknown YARD_CAP '$c' — skipping" ;;
  esac
done
if [ "$need_rootless" = 1 ]; then
  # security.nesting is a core container invariant owned by 03-create-subyard.sh. Extras only own
  # the additional rootless-container keys below, so removing the last cap cannot undo core setup.
  set_cfg security.idmap.size 1000000
  set_cfg security.syscalls.intercept.mknod true
  set_cfg security.syscalls.intercept.setxattr true
else
  unset_cfg security.idmap.size
  unset_cfg security.syscalls.intercept.mknod
  unset_cfg security.syscalls.intercept.setxattr
fi

# --- 3. devices (/dev passthrough as unix-char) ------------------------------
ensure_unix_char() {  # <device-name> <host-source>
  local name="$1" source="$2" err
  [ -e "$source" ] || { warn "$source absent on host — skipping $name"; return; }
  if device_exists "$name"; then
    if [ "$(dev_get "$name" type)" = unix-char ] \
      && [ "$(dev_get "$name" source)" = "$source" ] \
      && [ "$(dev_get "$name" path)" = "$source" ]; then
      ok "$name → $source unchanged"
      return
    fi
    warn "$name drifted — re-attaching"
    incus config device remove "$YARD_INSTANCE_NAME" "$name" "${PROJ[@]}" >/dev/null
  fi
  # Nested hosts reject the mode property on unix-char devices — retry without it
  # (in-yard perms then follow the source node; consumers may need the device group).
  if ! err="$(incus config device add "$YARD_INSTANCE_NAME" "$name" unix-char "${PROJ[@]}" \
        source="$source" path="$source" mode=0666 2>&1 >/dev/null)"; then
    case "$err" in
      *"nested container"*)
        incus config device add "$YARD_INSTANCE_NAME" "$name" unix-char "${PROJ[@]}" \
          source="$source" path="$source" >/dev/null
        warn "nested host: $name attached without an explicit mode" ;;
      *) printf '%s\n' "$err" >&2; die "could not attach device '$name'" ;;
    esac
  fi
  ok "$name → $source"
}
# 'gpu' → pass the host GPU RENDER NODE(s) as unix-char (Mesa headless GLES for -gpu host). The incus
# 'gpu' device type is forbidden by the restricted project; unix-char is allowed, and a render node is
# all a headless renderer needs. renderD* numbering isn't fixed (renderD128 = first; more per extra GPU),
# so pass every render node the host has.
ensure_gpu() {
  local node found=0
  for node in /dev/dri/renderD*; do
    [ -e "$node" ] || continue
    found=1; ensure_unix_char "yx-dev-dri-${node##*/}" "$node"
  done
  [ "$found" = 1 ] || warn "no /dev/dri/renderD* on host — GPU profile requirement unmet (skipping)"
}
[ "$need_fuse" = 1 ] && ensure_unix_char yx-dev-fuse /dev/fuse
for d in ${u_devs[@]+"${u_devs[@]}"}; do
  case "$d" in
    kvm)  ensure_unix_char yx-dev-kvm  /dev/kvm ;;
    fuse) : ;; # already covered by need_fuse
    gpu)  ensure_gpu ;;
    *)    warn "unknown YARD_DEVICE '$d' — skipping" ;;
  esac
done

# One cleanup pass for every extras-owned device. Doing this after mounts and passthrough devices
# are reconciled avoids detaching valid yx-dev-* entries just to add them again.
mapfile -t attached_devices < <(incus config device list "$YARD_INSTANCE_NAME" "${PROJ[@]}" 2>/dev/null)
for d in "${attached_devices[@]}"; do
  case "$d" in yx-*) ;; *) continue ;; esac
  case " $want_devices " in *" $d "*) continue ;; esac
  warn "detaching '$d' — no project requests it"
  incus config device remove "$YARD_INSTANCE_NAME" "$d" "${PROJ[@]}" >/dev/null
done

# --- summary -----------------------------------------------------------------
echo
ok "Yard extras reconciled."
if [ "$restart_needed" = 1 ]; then
  warn "Capabilities changed — restart the yard to apply them (the host's network guard runs on the way up):"
  printf '    %s%s down && %s up%s\n' "$C_HEAD" "${PROG:-yard}" "${PROG:-yard}" "$C_OFF" >&2
fi
