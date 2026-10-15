#!/usr/bin/env bash
# Physical teardown boundary; Go owns parsing and policy.
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
# shellcheck source=scripts/lib/ssh-config.sh
. "$SCRIPT_DIR/lib/ssh-config.sh"

# The same physical owner handles the aggregate full-cleanup phases. Parsing,
# exact inventory and the single authorization stay at the prepared boundary.
case "${1:-apply}" in
  apply) [ "$#" = 0 ] || shift ;;
  --yes) ;;
  prepare-lock)
    require_root "initializing the approved host coordination lock requires root"
    exec "${SUBYARD_DISPATCHER_PATH:?}" _network-lock ensure ;;
  check-sessions)
    python3 "$SCRIPT_DIR/lib/teardown-plan.py" guard-all "$INCUS_PROJECT"
    PROJ=(--project "$INCUS_PROJECT"); have_incus=1
    # shellcheck source=scripts/lib/teardown-session.sh
    . "$SCRIPT_DIR/lib/teardown-session.sh"
    teardown_session_guard "$YARD_INSTANCE_NAME"
    exit 0 ;;
  check-profiles)
    require_root "conditionally inspecting approved private owner metadata requires root"
    exec python3 -B "$SCRIPT_DIR/lib/teardown-profile-checks.py" ;;
  remove-shared)
    require_root "retiring approved shared host resources requires root"
    removed_bridges="$(python3 -B "$SCRIPT_DIR/lib/teardown-shared.py")" || exit "$?"
    while IFS= read -r removed_bridge; do
      [ -n "$removed_bridge" ] || continue
      ufw_yard_rules_remove "$removed_bridge" || die "approved bridge firewall cleanup failed"
    done <<< "$removed_bridges"
    nm_unmanaged_guard_reconcile --yes
    exit 0 ;;
  remove-files)
    if python3 - "$SUBYARD_TEARDOWN_PLAN" <<'PY_NEEDS_ROOT'
import json,os,sys
with open(sys.argv[1],encoding="utf-8") as handle: plan=json.load(handle)
raise SystemExit(0 if any(tree.get("Opaque") or any(entry["UID"] != os.getuid() for entry in tree["Entries"]) for tree in plan["Trees"]) else 1)
PY_NEEDS_ROOT
    then require_root "removing approved private installation roots requires root"; fi
    exec python3 -B "$SCRIPT_DIR/lib/teardown-files.py" ;;
  stop-services)
    # shellcheck source=scripts/lib/teardown-services.sh
    . "$SCRIPT_DIR/lib/teardown-services.sh"
    teardown_stop_services
    exit 0 ;;
  *) die "invalid prepared teardown phase" ;;
esac

OPERATOR_USER="${SUBYARD_USER:-${SUDO_USER:-${USER:-root}}}"
OPERATOR_GROUP="$(id -gn "$OPERATOR_USER" 2>/dev/null || printf '%s\n' "$OPERATOR_USER")"

INCUS_PROJECT="${INCUS_PROJECT:-subyard}"
YARD_INSTANCE_NAME="${YARD_INSTANCE_NAME:-yard}"
SRV_POOL="${SRV_POOL:-default}"
SRV_VOLUME="${SRV_VOLUME:-yard-srv}"
STORAGE_POOL="${STORAGE_POOL:-$SRV_POOL}"
BRIDGE="${INCUS_BRIDGE:-${INCUS_NETWORK:-incusbr0}}"

YARD_SNIP="$(ssh_yard_snippet_name "${YARD_NAME:-}")"
YARD_STATE_DIR="${SUBYARD_STATE_DIR:-$SUBYARD_CONFIG_HOME/projects}"
YARD_SPACE_CACHE="$SUBYARD_HOME/space${YARD_NAME:+-$YARD_NAME}.cache"

KEEP_DATA="${SUBYARD_TEARDOWN_KEEP_DATA:-}"
case "$KEEP_DATA" in 0 | 1) ;; *) die "prepared teardown mode is required" ;; esac
KEEP_SHARED="${SUBYARD_TEARDOWN_KEEP_SHARED:-}"
case "$KEEP_SHARED" in 0 | 1) ;; *) die "prepared shared-infrastructure mode is required" ;; esac
subyard_home_validate_root "$SUBYARD_HOME" \
  || die "refusing unsafe Subyard data root: $SUBYARD_HOME"
SUBYARD_HOME="$SUBYARD_VALIDATED_HOME"
[ "$INCUS_PROJECT" != default ] || die "teardown requires a dedicated project"
TEARDOWN_GUARD="$SCRIPT_DIR/lib/teardown-plan.py"
python3 "$TEARDOWN_GUARD" validate || die "prepared exact teardown inventory is required"
approved_names() { python3 "$TEARDOWN_GUARD" list "$1"; }
guard_resource() { python3 "$TEARDOWN_GUARD" guard "$1" "$2" "$INCUS_PROJECT" "${3:-}" || die "plan_stale: teardown resource changed after confirmation"; }
require_root "removing the NetworkManager guard, ufw rules, and the Incus storage data needs root"

if command -v incus >/dev/null 2>&1 && ! incus info >/dev/null 2>&1; then
  die "incus is installed but its daemon is unreachable — start or repair incusd before teardown (a down daemon must not be treated as 'nothing to remove'; see incident #33)"
fi

have_incus=0
if command -v incus >/dev/null 2>&1 && incus info >/dev/null 2>&1; then have_incus=1; fi
PROJ=(--project "$INCUS_PROJECT")
bridge_gone=0; pool_gone=0
[ "$have_incus" = 1 ] || { bridge_gone=1; pool_gone=1; }

python3 "$TEARDOWN_GUARD" guard-all "$INCUS_PROJECT" || die "plan_stale: teardown inventory changed before apply"
python3 "$TEARDOWN_GUARD" guard-artifacts || exit 75
# shellcheck source=scripts/lib/teardown-session.sh
. "$SCRIPT_DIR/lib/teardown-session.sh"
teardown_session_guard "$YARD_INSTANCE_NAME"
if [ "${SUBYARD_TEARDOWN_FULL:-0}" != 1 ]; then
  # shellcheck source=scripts/lib/teardown-services.sh
  . "$SCRIPT_DIR/lib/teardown-services.sh"
  teardown_stop_services
fi
bash "$SCRIPT_DIR/profile-services.sh" --remove

echo "Instance:"
if [ "$have_incus" = 1 ] && incus project show "$INCUS_PROJECT" >/dev/null 2>&1; then
  while IFS= read -r inst; do
    [ -n "$inst" ] || continue
    guard_resource instance "$inst"
    if incus delete -f "$inst" "${PROJ[@]}"; then ok "deleted instance '$inst'"; else warn "could not delete instance '$inst'"; fi
  done < <(approved_names instance)
  ok "no instances left in '$INCUS_PROJECT'"
else
  ok "Incus not reachable or project '$INCUS_PROJECT' absent — nothing to delete"
fi

if [ "$KEEP_DATA" = 0 ]; then
  echo "Project + volume:"
  if [ "$have_incus" = 1 ] && incus project show "$INCUS_PROJECT" >/dev/null 2>&1; then
    while IFS=$'\t' read -r approved_pool approved_volume; do
      [ -n "$approved_volume" ] || continue
      guard_resource volume "$approved_volume" "$approved_pool"
      incus storage volume delete "$approved_pool" "$approved_volume" "${PROJ[@]}" >/dev/null 2>&1 \
        && ok "deleted approved volume '$approved_volume'" || die "approved volume removal failed"
    done < <(python3 "$TEARDOWN_GUARD" volumes)
    if incus_project_has_isolated_images "$INCUS_PROJECT"; then
      while IFS= read -r fp; do
        [ -n "$fp" ] || continue
        guard_resource image "$fp"
        incus image delete "$fp" "${PROJ[@]}" >/dev/null 2>&1 \
          && ok "deleted cached image ${fp:0:12}" || true
      done < <(approved_names image)
    else
      ok "kept images shared from the default project"
    fi
    while IFS= read -r prof; do
      [ -n "$prof" ] && [ "$prof" != default ] || continue
      guard_resource profile "$prof"
      incus profile delete "$prof" "${PROJ[@]}" >/dev/null 2>&1 && ok "deleted profile '$prof'" || true
    done < <(approved_names profile)
    if approved_names profile | grep -qx default; then
    guard_resource profile default
    incus profile device remove default eth0 "${PROJ[@]}" >/dev/null 2>&1 || true
    incus profile device remove default root "${PROJ[@]}" >/dev/null 2>&1 || true
    fi
    guard_resource project "$INCUS_PROJECT"
    if incus project delete "$INCUS_PROJECT" >/dev/null 2>&1; then
      ok "deleted project '$INCUS_PROJECT'"
    else
      warn "could not delete project '$INCUS_PROJECT' (inspect: sudo incus project show $INCUS_PROJECT)"
    fi
  else
    ok "project '$INCUS_PROJECT' absent"
  fi

  echo "Bridge + storage pool:"
  if [ "$have_incus" = 1 ]; then
    others="$(incus list --all-projects -c n -f csv 2>/dev/null | grep -v '^$' || true)"
    if [ "$KEEP_SHARED" = 1 ]; then
      warn "other registered local yards exist — keeping shared bridge '$BRIDGE' and pool '$STORAGE_POOL'"
    elif [ -n "$others" ]; then
      warn "other Incus instances exist — keeping shared bridge '$BRIDGE' and pool '$STORAGE_POOL':"
      printf '%s\n' "$others" | sed 's/^/      - /'
    else
      removed_bridges="$(python3 -B "$SCRIPT_DIR/lib/teardown-shared.py" --selected)" || exit "$?"
      while IFS= read -r removed_bridge; do
        [ "$removed_bridge" != "$BRIDGE" ] || bridge_gone=1
      done <<< "$removed_bridges"
      incus storage show "$STORAGE_POOL" --project default >/dev/null 2>&1 || pool_gone=1
    fi
  fi
fi

if command -v ufw >/dev/null 2>&1 && [ "$bridge_gone" = 1 ]; then
  ufw_yard_rules_remove "$BRIDGE" || die "approved bridge firewall cleanup failed"
  ok "removed Subyard ufw rules for '$BRIDGE' (if any)"
elif command -v ufw >/dev/null 2>&1; then
  ok "kept ufw rules for '$BRIDGE' (bridge still exists — shared with other yards)"
fi
sshdir="$SUBYARD_OPERATOR_HOME/.ssh"
snip="$sshdir/$YARD_SNIP"; cfg="$sshdir/config"
# A verified runtime may be pinned through a root process's /proc/PID/fd.
# Send the already-loaded helper; the operator cannot reopen that source path.
# shellcheck disable=SC2016
{
  declare -f ssh_config_remove_exact &&
    printf '%s\n' 'set -e' "python3 -B - remove-artifact \"\$1\" <<'SUBYARD_TEARDOWN_GUARD_CODE'" &&
    cat "$TEARDOWN_GUARD" &&
    printf '%s\n' 'SUBYARD_TEARDOWN_GUARD_CODE' 'ssh_config_remove_exact "$2" "$3"'
} | sudo -n -u "$OPERATOR_USER" -- env "SUBYARD_TEARDOWN_ARTIFACTS=$SUBYARD_TEARDOWN_ARTIFACTS" \
  "SUBYARD_TEARDOWN_PLAN=${SUBYARD_TEARDOWN_PLAN:-}" "SUBYARD_TEARDOWN_PLAN_SHA256=${SUBYARD_TEARDOWN_PLAN_SHA256:-}" bash -s -- "$snip" "$cfg" "Include $YARD_SNIP" || {
  cleanup_status=$?
  [ "$cleanup_status" != 75 ] || exit 75
  die "could not remove this yard's operator SSH config"
}
ok "removed ~/.ssh/$YARD_SNIP (if present)"
ok "removed 'Include $YARD_SNIP' from ~/.ssh/config (if present)"
known="$SUBYARD_HOME/ssh/known_hosts"
if [ "${SUBYARD_TEARDOWN_FULL:-0}" != 1 ] && [ -f "$known" ] && [ -n "${SSH_PORT:-}" ]; then
  ssh_known_host_remove "$known" "[127.0.0.1]:$SSH_PORT" "$OPERATOR_USER" "$OPERATOR_GROUP" \
    || die "could not clear this yard's SSH host-key entry"
  ok "cleared this yard's host-key entry ([127.0.0.1]:$SSH_PORT) from known_hosts"
fi
state_cleanup="$(subyard_state_remove_canonical \
  "$YARD_STATE_DIR" "$SUBYARD_CONFIG_HOME" "${YARD_NAME:-}")" \
  || { state_status=$?; [ "$state_status" != 75 ] || exit 75; die "refusing unsafe yard state path: $YARD_STATE_DIR"; }
case "$state_cleanup" in
  removed) ok "removed yard state $YARD_STATE_DIR" ;;
  absent) ok "yard state already absent: $YARD_STATE_DIR" ;;
  preserved) warn "kept non-canonical yard state path: $YARD_STATE_DIR" ;;
  *) die "unexpected yard state cleanup result" ;;
esac
if [ "${SUBYARD_TEARDOWN_RESET_CONFIG:-0}" = 1 ]; then
  : # The Go configuration boundary retains its captured directory until physical verification.
elif [ -n "${YARD_NAME:-}" ]; then
  rmdir "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME" 2>/dev/null || true
  rmdir "$SUBYARD_CONFIG_HOME/yards" 2>/dev/null || true
else
  rmdir --ignore-fail-on-non-empty "$SUBYARD_CONFIG_HOME" 2>/dev/null || true
fi
for cache in "$YARD_SPACE_CACHE" "$YARD_SPACE_CACHE.lock" "$YARD_SPACE_CACHE.tmp"; do
  python3 "$TEARDOWN_GUARD" remove-artifact "$cache" || exit 75
done
if [ "$KEEP_DATA" = 1 ]; then
  ok "kept data: $STORAGE_PATH (and $SUBYARD_HOME)"
elif [ "$pool_gone" = 1 ]; then
  data_home_removed="$(subyard_home_remove_if_empty "$SUBYARD_HOME")" \
    || die "refusing to remove unsafe Subyard data root: $SUBYARD_HOME"
  case "$data_home_removed" in
    removed) ok "removed empty Subyard data root $SUBYARD_HOME" ;;
    absent) ok "Subyard data root already absent: $SUBYARD_HOME" ;;
    retained) ok "kept non-empty Subyard data root $SUBYARD_HOME; teardown only removes selected-yard state" ;;
    *) die "unexpected Subyard data-root cleanup result" ;;
  esac
else
  warn "kept $STORAGE_PATH and shared $SUBYARD_HOME/{ssh,logs} — another yard still uses the pool"
fi

if [ "$KEEP_DATA" = 0 ]; then
  echo "NetworkManager guard:"
  nm_unmanaged_guard_reconcile \
    || warn "kept the NetworkManager guard because surviving Incus bridges could not be verified"
fi

echo
trap - EXIT
if [ "${SUBYARD_TEARDOWN_RESET_CONFIG:-0}" = 1 ]; then
  ok "Yard runtime cleanup applied; verification and local settings cleanup follow."
elif [ "$KEEP_DATA" = 1 ]; then
  ok "Subyard teardown done (data kept)."
else
  ok "Subyard teardown done."
fi
