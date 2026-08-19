#!/usr/bin/env bash
# 01-install-incus.sh — Phase 1: install Incus, grant the operator incus-admin,
# init a dir pool under $HOME/.subyard. Idempotent. Self-elevates via sudo.
# Only `incus` is installed here (qemu is lazy in vm mode).
# Env: SUBYARD_USER, SUBYARD_HOME, STORAGE_POOL, STORAGE_PATH, INCUS_BRIDGE, MIN_INCUS_VER.
# Flags: -y; --zabbly (install/upgrade incus from the Zabbly LTS-6.0 repo, for nested Docker);
#        --upgrade-only (only ensure incus >= MIN_INCUS_VER, skip group/storage/init).
# Flags survive the sudo re-exec via SUBYARD_SCRIPT_ARGV (sudo scrubs env, not argv).
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

USE_ZABBLY=auto; UPGRADE_ONLY=0
for _a in "$@"; do
  case "$_a" in
    --zabbly)       USE_ZABBLY=1 ;;
    --upgrade-only) UPGRADE_ONLY=1 ;;
  esac
done; unset _a
# Nested Docker (project-env boxes) needs the CVE-2025-52881 AppArmor fix in Incus 6.0.6 LTS.
MIN_INCUS_VER="${MIN_INCUS_VER:-6.0.6}"

# Grant incus-admin to the real operator (SUDO_USER under sudo), not root.
OPERATOR_USER="${SUBYARD_USER:-${SUDO_USER:-${USER:-root}}}"
if [ "$OPERATOR_USER" = root ]; then
  warn "operator user resolved to 'root' (running as root without sudo?); set SUBYARD_USER=<you> to grant your own account"
fi
OPERATOR_HOME="$(getent passwd "$OPERATOR_USER" | cut -d: -f6)"
[ -n "$OPERATOR_HOME" ] || die "cannot resolve home dir for user '$OPERATOR_USER'"
OPERATOR_GROUP="$(id -gn "$OPERATOR_USER")"

# $SUBYARD_HOME is already resolved under the real operator by explicit context loading (it reads
# the same SUDO_USER), so it points at the operator's home even though this script self-elevates.
STORAGE_POOL="${STORAGE_POOL:-default}"
INCUS_BRIDGE="${INCUS_BRIDGE:-incusbr0}"

if [ "$UPGRADE_ONLY" = 1 ]; then
  announce "Subyard — upgrade Incus to >= $MIN_INCUS_VER (Zabbly LTS-6.0)" \
    "Add the Zabbly LTS-6.0 apt repo (keyring + source), if not already present." \
    "apt-get install the newer 'incus' package and restart the daemon." \
    "Reversible: delete /etc/apt/sources.list.d/zabbly-incus-lts-6.0.sources and downgrade."
  proceed_or_die
  require_root "adding an apt repo and upgrading the incus package needs root"
  # Guard host networking BEFORE the daemon restart in the upgrade below: restarting
  # incusd with NetworkManager unguarded is how a veth got hijacked and killed the
  # host's internet once. The install path runs this guard too (step 5 below).
  nm_unmanaged_guard "$INCUS_BRIDGE"
else
  announce "Subyard Phase 1 — install & initialize Incus" \
    "Install the 'incus' package if missing (apt)." \
    "Add user '$OPERATOR_USER' to group 'incus-admin' — this grants Incus access ≈ root on this host." \
    "Create the storage pool directory: $STORAGE_PATH" \
    "Run 'incus admin init': dir pool '$STORAGE_POOL' + bridge '$INCUS_BRIDGE' (only if not already initialized)."
  proceed_or_die
  require_root "the steps above install packages, edit group membership, and initialize Incus"
fi

# Elevated Incus commands use root's HOME. Older Subyard runs could preserve the
# operator's HOME across sudo and leave the Incus client directory root-owned,
# preventing the operator from using Incus after group activation. Repair only
# root-owned nodes in that operator-owned client tree.
operator_incus_config="$OPERATOR_HOME/.config/incus"
if [ "$OPERATOR_USER" != root ] && [ -e "$operator_incus_config" ]; then
  if [ -L "$operator_incus_config" ]; then
    warn "not repairing symlinked Incus client config at $operator_incus_config"
  elif [ -d "$operator_incus_config" ] \
    && [ -n "$(find "$operator_incus_config" -xdev -uid 0 -print -quit)" ]; then
    find "$operator_incus_config" -xdev -uid 0 \
      -exec chown -h "$OPERATOR_USER:$OPERATOR_GROUP" {} +
    ok "repaired operator ownership of $operator_incus_config"
  fi
fi

# --- 1. ensure incus, recent enough for nested Docker ------------------------
# Many distros still package an Incus older than MIN_INCUS_VER, so nested Docker (project-env
# boxes) fails. With --zabbly (set by 'yard init' after a y/N prompt) we add the Zabbly LTS-6.0
# repo and install/upgrade from there; otherwise we use the distro package and warn about the floor.
echo "Dependency: incus"
_iver()    { incus --version 2>/dev/null || echo '?'; }
_irecent() { local v; v="$(_iver)"; [ "$v" != '?' ] && command -v dpkg >/dev/null 2>&1 \
               && dpkg --compare-versions "$v" ge "$MIN_INCUS_VER"; }

if [ "$USE_ZABBLY" = auto ]; then
  USE_ZABBLY=0
  if command -v incus >/dev/null 2>&1; then
    _irecent || USE_ZABBLY=1
  elif ! command -v apt-cache >/dev/null 2>&1 || ! command -v dpkg >/dev/null 2>&1; then
    USE_ZABBLY=1
  else
    candidate="$(apt-cache policy incus 2>/dev/null | awk '/Candidate:/{print $2; exit}')"
    [ -n "$candidate" ] && [ "$candidate" != '(none)' ] \
      && dpkg --compare-versions "$candidate" ge "$MIN_INCUS_VER" \
      || USE_ZABBLY=1
  fi
fi

if command -v incus >/dev/null 2>&1; then
  if _irecent; then
    ok "incus present ($(_iver)) >= $MIN_INCUS_VER"
  elif [ "$USE_ZABBLY" = 1 ]; then
    warn "incus $(_iver) < $MIN_INCUS_VER — upgrading from the Zabbly LTS-6.0 repo"
    add_zabbly_lts_repo || die "could not set up the Zabbly LTS-6.0 repo"
    info "apt-get install incus (Zabbly)"
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends incus \
      || die "incus upgrade failed"
    systemctl try-restart incus.service 2>/dev/null || true
    if _irecent; then ok "incus upgraded ($(_iver))"
    else warn "incus is still $(_iver) after upgrade — check 'apt-cache policy incus'"; fi
  else
    warn "incus $(_iver) < $MIN_INCUS_VER — nested Docker (project-env boxes) will fail (re-run with --zabbly to upgrade)"
  fi
else
  command -v apt-get >/dev/null 2>&1 \
    || die "no apt-get; install Incus manually (linuxcontainers.org/incus) and re-run"
  if [ "$USE_ZABBLY" = 1 ]; then
    warn "missing dependency: incus — installing from the Zabbly LTS-6.0 repo"
    add_zabbly_lts_repo || die "could not set up the Zabbly LTS-6.0 repo"
  else
    warn "missing dependency: incus — installing (sudo apt-get install incus)"
    info "apt-get update"
    apt-get update -qq
  fi
  info "installing incus"
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends incus \
    || die "incus install failed (the distro repos may carry it; Zabbly LTS-6.0 is the >= $MIN_INCUS_VER source)"
  ok "incus installed ($(_iver))"
  if ! _irecent; then
    if [ "$USE_ZABBLY" = 1 ]; then
      die "installed incus $(_iver) < $MIN_INCUS_VER even after adding the Zabbly repo — check 'apt-cache policy incus'"
    else
      warn "installed incus $(_iver) < $MIN_INCUS_VER — nested Docker will fail until you upgrade (re-run with --zabbly)"
    fi
  fi
fi

# --upgrade-only: ensuring the version is all this run does — group/storage/init are 'yard init's job.
if [ "$UPGRADE_ONLY" = 1 ]; then
  echo
  ok "Incus is $(_iver)."
  exit 0
fi

# --- 2. grant operator access to the Incus socket ---------------------------
echo "Socket access (incus-admin → operator only):"
if ! getent group incus-admin >/dev/null 2>&1; then
  warn "group 'incus-admin' missing; creating it"
  groupadd --system incus-admin
fi
if id -nG "$OPERATOR_USER" 2>/dev/null | tr ' ' '\n' | grep -qx incus-admin; then
  ok "$OPERATOR_USER already in incus-admin"
else
  usermod -aG incus-admin "$OPERATOR_USER"
  ok "added $OPERATOR_USER to incus-admin (re-login required to take effect)"
fi

# --- 3. storage dir ----------------------------------------------------------
# Keep $SUBYARD_HOME operator-owned (chown even if it pre-exists — Incus may have shifted
# it to nobody:nogroup in a prior run, which locks operator steps out of their own state).
echo "Storage:"
install -d "$SUBYARD_HOME"
chown "$OPERATOR_USER:$OPERATOR_GROUP" "$SUBYARD_HOME"
if [ ! -d "$STORAGE_PATH" ]; then
  install -d -o "$OPERATOR_USER" -g "$OPERATOR_GROUP" "$STORAGE_PATH"
  ok "created $STORAGE_PATH"
else
  ok "$STORAGE_PATH exists"
fi

# --- 4. initialize Incus (idempotent) ----------------------------------------
# Reconcile the pool and the bridge INDEPENDENTLY: a host can arrive half-initialized
# (e.g. a pool but no bridge from an earlier manual `incus admin init`), and gating on
# the pool alone would skip the bridge forever — the yard's nic then fails at create.
echo "Init:"
if incus storage show "$STORAGE_POOL" --project default >/dev/null 2>&1; then
  ok "storage pool '$STORAGE_POOL' present"
  if incus network show "$INCUS_BRIDGE" --project default >/dev/null 2>&1; then
    ok "bridge '$INCUS_BRIDGE' present — Incus already initialized"
  else
    info "pool exists but bridge '$INCUS_BRIDGE' is missing — creating it"
    # Nested/AppArmor-stacked hosts (e.g. a yard inside a yard): dnsmasq may be denied the
    # syslog unix socket ("cannot open log : Permission denied") — retry logging to a file
    # under the network's own state dir, which the generated profile does allow.
    if ! out="$(incus network create "$INCUS_BRIDGE" ipv4.address=auto ipv6.address=none \
      --project default 2>&1)"; then
      case "$out" in
        *"cannot open log"*)
          warn "dnsmasq cannot reach syslog (nested/AppArmor host) — retrying with a file log"
          incus network create "$INCUS_BRIDGE" ipv4.address=auto ipv6.address=none \
            raw.dnsmasq="log-facility=/var/lib/incus/networks/$INCUS_BRIDGE/dnsmasq.log" \
            --project default
          ;;
        *) printf '%s\n' "$out" >&2; die "could not create bridge '$INCUS_BRIDGE'" ;;
      esac
    fi
    ok "created bridge '$INCUS_BRIDGE'"
  fi
else
  info "running incus admin init (pool '$STORAGE_POOL' → $STORAGE_PATH)"
  incus admin init --preseed <<EOF
storage_pools:
  - name: $STORAGE_POOL
    driver: dir
    config:
      source: $STORAGE_PATH
networks:
  - name: $INCUS_BRIDGE
    type: bridge
    config:
      ipv4.address: auto
      ipv6.address: none
profiles:
  - name: default
    devices:
      root:
        path: /
        pool: $STORAGE_POOL
        type: disk
      eth0:
        name: eth0
        network: $INCUS_BRIDGE
        type: nic
EOF
  ok "Incus initialized"
fi

# --- 5. keep NetworkManager off Incus bridge/veths (host-internet guard) ------
# Set up BEFORE any instance/veth exists, so NM never grabs a yard veth.
echo "Host networking (NetworkManager guard):"
nm_unmanaged_guard "$INCUS_BRIDGE"

# --- summary -----------------------------------------------------------------
echo
ok "Phase 1 done."
cat <<MSG

Next:
  - Re-login (or run 'newgrp incus-admin') so $OPERATOR_USER can use 'incus'
    without sudo, then verify:  incus list
  - Phase 1 cont.: scripts/02-create-project.sh (project 'subyard' + restricted config)
MSG
