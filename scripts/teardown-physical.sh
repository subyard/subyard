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

OPERATOR_USER="${SUBYARD_USER:-${SUDO_USER:-${USER:-root}}}"
OPERATOR_HOME="$(getent passwd "$OPERATOR_USER" | cut -d: -f6)"
[ -n "$OPERATOR_HOME" ] || OPERATOR_HOME="$HOME"
OPERATOR_GROUP="$(id -gn "$OPERATOR_USER" 2>/dev/null || printf '%s\n' "$OPERATOR_USER")"

INCUS_PROJECT="${INCUS_PROJECT:-subyard}"
INSTANCE_NAME="${INSTANCE_NAME:-yard}"
SRV_POOL="${SRV_POOL:-default}"
SRV_VOLUME="${SRV_VOLUME:-yard-srv}"
STORAGE_POOL="${STORAGE_POOL:-$SRV_POOL}"
BRIDGE="${INCUS_BRIDGE:-${INCUS_NETWORK:-incusbr0}}"

YARD_SNIP="subyard${YARD_NAME:+-$YARD_NAME}.config"
YARD_STATE_DIR="${SUBYARD_STATE_DIR:-$SUBYARD_CONFIG_HOME/projects}"
YARD_SPACE_CACHE="$SUBYARD_HOME/space${YARD_NAME:+-$YARD_NAME}.cache"

KEEP_DATA="${SUBYARD_TEARDOWN_KEEP_DATA:-}"
case "$KEEP_DATA" in 0 | 1) ;; *) die "prepared teardown mode is required" ;; esac
KEEP_SHARED="${SUBYARD_TEARDOWN_KEEP_SHARED:-}"
case "$KEEP_SHARED" in 0 | 1) ;; *) die "prepared shared-infrastructure mode is required" ;; esac
subyard_home_validate_root "$SUBYARD_HOME" \
  || die "refusing unsafe Subyard data root: $SUBYARD_HOME"
SUBYARD_HOME="$SUBYARD_VALIDATED_HOME"
require_root "removing the NetworkManager guard, ufw rules, and the Incus storage data needs root"

if [ "$KEEP_DATA" = 0 ] && command -v incus >/dev/null 2>&1 && ! incus info >/dev/null 2>&1; then
  die "incus is installed but its daemon is unreachable — start or repair incusd before teardown (a down daemon must not be treated as 'nothing to remove'; see incident #33)"
fi

have_incus=0
if command -v incus >/dev/null 2>&1 && incus info >/dev/null 2>&1; then have_incus=1; fi
PROJ=(--project "$INCUS_PROJECT")
bridge_gone=0; pool_gone=0
[ "$have_incus" = 1 ] || { bridge_gone=1; pool_gone=1; }

echo "Instance:"
if [ "$have_incus" = 1 ] && incus project show "$INCUS_PROJECT" >/dev/null 2>&1; then
  while IFS= read -r inst; do
    [ -n "$inst" ] || continue
    if incus delete -f "$inst" "${PROJ[@]}"; then ok "deleted instance '$inst'"; else warn "could not delete instance '$inst'"; fi
  done < <(incus list "${PROJ[@]}" -c n -f csv 2>/dev/null)
  ok "no instances left in '$INCUS_PROJECT'"
else
  ok "Incus not reachable or project '$INCUS_PROJECT' absent — nothing to delete"
fi

if [ "$KEEP_DATA" = 0 ]; then
  echo "Project + volume:"
  if [ "$have_incus" = 1 ] && incus project show "$INCUS_PROJECT" >/dev/null 2>&1; then
    if incus storage volume show "$SRV_POOL" "$SRV_VOLUME" "${PROJ[@]}" >/dev/null 2>&1; then
      incus storage volume delete "$SRV_POOL" "$SRV_VOLUME" "${PROJ[@]}" >/dev/null 2>&1 \
        && ok "deleted volume '$SRV_VOLUME'" || warn "could not delete volume '$SRV_VOLUME'"
    else
      ok "volume '$SRV_VOLUME' absent"
    fi
    if incus_project_has_isolated_images "$INCUS_PROJECT"; then
      while IFS= read -r fp; do
        [ -n "$fp" ] || continue
        incus image delete "$fp" "${PROJ[@]}" >/dev/null 2>&1 \
          && ok "deleted cached image ${fp:0:12}" || true
      done < <(incus image list "${PROJ[@]}" -f csv -c f 2>/dev/null)
    else
      ok "kept images shared from the default project"
    fi
    while IFS= read -r prof; do
      [ -n "$prof" ] && [ "$prof" != default ] || continue
      incus profile delete "$prof" "${PROJ[@]}" >/dev/null 2>&1 && ok "deleted profile '$prof'" || true
    done < <(incus profile list "${PROJ[@]}" -f csv -c n 2>/dev/null)
    incus profile device remove default eth0 "${PROJ[@]}" >/dev/null 2>&1 || true
    incus profile device remove default root "${PROJ[@]}" >/dev/null 2>&1 || true
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
      incus_remove_default_profile_device_if_matches eth0 network "$BRIDGE" \
        >/dev/null 2>&1 || true
      incus_remove_default_profile_device_if_matches root pool "$STORAGE_POOL" \
        >/dev/null 2>&1 || true
      if ! incus network show "$BRIDGE" --project default >/dev/null 2>&1; then
        bridge_gone=1; ok "bridge '$BRIDGE' absent"
      elif incus network delete "$BRIDGE" --project default >/dev/null 2>&1; then
        bridge_gone=1; ok "deleted bridge '$BRIDGE'"
      else
        warn "bridge '$BRIDGE' not deleted (still in use) — keeping the NetworkManager guard"
      fi
      if ! incus storage show "$STORAGE_POOL" --project default >/dev/null 2>&1; then
        pool_gone=1; ok "storage pool '$STORAGE_POOL' absent"
      elif incus storage delete "$STORAGE_POOL" --project default >/dev/null 2>&1; then
        pool_gone=1; ok "deleted storage pool '$STORAGE_POOL'"
      else
        warn "pool '$STORAGE_POOL' not deleted (still in use) — keeping its data dir to avoid an orphan"
      fi
    fi
  fi
fi

echo "Boot power reconciler:"
"$SCRIPT_DIR/install-power-reconciler.sh" --remove-if-unused --yes

echo "Host config:"
if command -v ufw >/dev/null 2>&1 && [ "$bridge_gone" = 1 ]; then
  ufw delete allow in on "$BRIDGE" to any port 67 proto udp >/dev/null 2>&1 || true
  ufw delete allow in on "$BRIDGE" to any port 53 >/dev/null 2>&1 || true
  ufw route delete allow in on "$BRIDGE" >/dev/null 2>&1 || true
  ufw route delete allow out on "$BRIDGE" >/dev/null 2>&1 || true
  ufw_rules_set_probe_access disable \
    || warn "could not restore root-only ownership on /etc/ufw/user.rules"
  ok "removed Subyard ufw rules for '$BRIDGE' (if any)"
elif command -v ufw >/dev/null 2>&1; then
  ok "kept ufw rules for '$BRIDGE' (bridge still exists — shared with other yards)"
fi
snip="$OPERATOR_HOME/.ssh/$YARD_SNIP"; cfg="$OPERATOR_HOME/.ssh/config"
rm -f "$snip" && ok "removed ~/.ssh/$YARD_SNIP (if present)"
if [ -f "$cfg" ] && grep -qxF "Include $YARD_SNIP" "$cfg"; then
  grep -vxF "Include $YARD_SNIP" "$cfg" > "$cfg.tmp" && mv -f "$cfg.tmp" "$cfg"
  chown "$OPERATOR_USER:$OPERATOR_GROUP" "$cfg" 2>/dev/null || true
  ok "removed 'Include $YARD_SNIP' from ~/.ssh/config"
fi
known="$SUBYARD_HOME/ssh/known_hosts"
if [ -f "$known" ] && [ -n "${SSH_PORT:-}" ]; then
  ssh_known_host_remove "$known" "[127.0.0.1]:$SSH_PORT" "$OPERATOR_USER" "$OPERATOR_GROUP" \
    || die "could not clear this yard's SSH host-key entry"
  ok "cleared this yard's host-key entry ([127.0.0.1]:$SSH_PORT) from known_hosts"
fi
state_cleanup="$(subyard_state_remove_canonical \
  "$YARD_STATE_DIR" "$SUBYARD_CONFIG_HOME" "${YARD_NAME:-}")" \
  || die "refusing unsafe yard state path: $YARD_STATE_DIR"
case "$state_cleanup" in
  removed) ok "removed yard state $YARD_STATE_DIR" ;;
  absent) ok "yard state already absent: $YARD_STATE_DIR" ;;
  preserved) warn "kept non-canonical yard state path: $YARD_STATE_DIR" ;;
  *) die "unexpected yard state cleanup result" ;;
esac
if [ -n "${YARD_NAME:-}" ]; then
  rmdir "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME" 2>/dev/null || true
  rmdir "$SUBYARD_CONFIG_HOME/yards" 2>/dev/null || true
else
  rmdir --ignore-fail-on-non-empty "$SUBYARD_CONFIG_HOME" 2>/dev/null || true
fi
rm -f "$YARD_SPACE_CACHE" "$YARD_SPACE_CACHE.lock" "$YARD_SPACE_CACHE.tmp" 2>/dev/null || true
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

bridge_link_gone=0
ip link show "$BRIDGE" >/dev/null 2>&1 || bridge_link_gone=1
if [ "$KEEP_DATA" = 0 ] && [ "$bridge_gone" = 1 ] && [ "$bridge_link_gone" = 1 ]; then
  echo "NetworkManager guard:"
  rm -f /etc/NetworkManager/conf.d/zz-subyard-unmanaged.conf 2>/dev/null || true
  rm -f /etc/NetworkManager/conf.d/99-subyard-unmanaged.conf 2>/dev/null || true
  if command -v nmcli >/dev/null 2>&1 && systemctl is-active --quiet NetworkManager 2>/dev/null; then
    systemctl reload NetworkManager 2>/dev/null || nmcli general reload 2>/dev/null || true
    ok "removed NetworkManager guard + reloaded NM"
  else
    ok "removed NetworkManager guard file (NM not active)"
  fi
elif [ "$KEEP_DATA" = 0 ]; then
  echo "NetworkManager guard:"
  warn "kept the NetworkManager guard — bridge '$BRIDGE' still present (removing it now could let NM hijack the route)"
fi

echo
if [ "$KEEP_DATA" = 1 ]; then ok "Subyard teardown done (data kept)."; else ok "Subyard teardown done."; fi
cat <<MSG

Verify the host is clean (use sudo — plain 'incus' fails without the incus-admin group):
  sudo incus list --all-projects      # expect an empty table
  sudo incus network list             # no '$BRIDGE'
  ip route show default               # only your real gateway — no veth/$BRIDGE default
  ip -br link show | grep -iE 'veth|$BRIDGE' || echo 'no incus interfaces (good)'

Rebuild any time:   yard init
To ALSO remove Incus itself (DANGEROUS — only if nothing else on this host uses it):
  sudo apt-get remove --purge incus incus-client     # all instances must be gone first
MSG
