#!/usr/bin/env bash
# 01-install-incus.sh — Phase 1: install Incus, grant the operator incus-admin,
# init a dir pool under $HOME/.subyard. A named socket ACL activates the approved
# operator immediately while the parent retains its exact plan. Self-elevates via sudo.
# QEMU and conntrack are installed only for VM yards.
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
# This refusal must precede sudo's deliberately restricted environment handoff.
[ -z "${INCUS_SOCKET:-}${INCUS_DIR:-}${SUBYARD_INCUS_SOCKET:-}" ] \
  || subyard_context_error "Incus installation requires the default local server endpoint"
# shellcheck source=scripts/lib/ui.sh
. "$SCRIPT_DIR/lib/ui.sh"
# shellcheck source=scripts/lib-power.sh
. "$SCRIPT_DIR/lib-power.sh"
# shellcheck source=scripts/lib/host.sh
. "$SCRIPT_DIR/lib/host.sh"

USE_ZABBLY=auto; UPGRADE_ONLY=0; APPROVED_OPERATOR_UID=
for _a in "$@"; do
  case "$_a" in
    --operator-uid=*) APPROVED_OPERATOR_UID="${_a#--operator-uid=}" ;;
    --zabbly)       USE_ZABBLY=1 ;;
    --upgrade-only) UPGRADE_ONLY=1 ;;
  esac
done; unset _a
# Nested Docker (project-env boxes) needs the CVE-2025-52881 AppArmor fix in Incus 6.0.6 LTS.
MIN_INCUS_VER="${MIN_INCUS_VER:-6.0.6}"
RECOMMENDED_INCUS_VER=6.0.7

# Grant incus-admin to the real operator (SUDO_USER under sudo), not root.
OPERATOR_USER="${SUBYARD_USER:-${SUDO_USER:-${USER:-root}}}"
if [ "$OPERATOR_USER" = root ]; then
  warn "operator user resolved to 'root' (running as root without sudo?); set SUBYARD_USER=<you> to grant your own account"
fi
OPERATOR_HOME="$(getent passwd "$OPERATOR_USER" | cut -d: -f6)"
[ -n "$OPERATOR_HOME" ] || die "cannot resolve home dir for user '$OPERATOR_USER'"
OPERATOR_GROUP="$(id -gn "$OPERATOR_USER")"
operator_uid="$(id -u "$OPERATOR_USER")"
# Dependency-only upgrades do not create installation-owned Incus state.
# Full installation still requires its captured authority before host operations.
if [ "$UPGRADE_ONLY" != 1 ]; then
  : "${SUBYARD_INSTALLATION_CONTEXT:?}"
fi
[ -z "$APPROVED_OPERATOR_UID" ] || [ "$operator_uid" = "$APPROVED_OPERATOR_UID" ] \
  || die "operator numeric identity differs from the approved installer"

# $SUBYARD_HOME is already resolved under the real operator by explicit context loading (it reads
# the same SUDO_USER), so it points at the operator's home even though this script self-elevates.
STORAGE_POOL="${STORAGE_POOL:-default}"
INCUS_BRIDGE="${INCUS_BRIDGE:-incusbr0}"

_iver()    { incus --version 2>/dev/null || echo '?'; }
_irecent() { local v; v="$(_iver)"; [ "$v" != '?' ] && command -v dpkg >/dev/null 2>&1 \
               && dpkg --compare-versions "$v" ge "$MIN_INCUS_VER"; }

# Pure selection: callers supply observations; planning never changes the host.
plan_incus_source() { # <installed version or empty> <selector> <distro candidate or empty>
  local installed="$1" selector="$2" candidate="$3"
  if [ -n "$installed" ]; then
    if [ "$installed" != '?' ] && command -v dpkg >/dev/null 2>&1 \
      && dpkg --compare-versions "$installed" ge "$MIN_INCUS_VER"; then
      printf 'none\n'
    else
      printf 'zabbly-upgrade\n'
    fi
  elif [ "$selector" = auto ] && [ -n "$candidate" ] && [ "$candidate" != '(none)' ] \
    && command -v dpkg >/dev/null 2>&1 \
    && dpkg --compare-versions "$candidate" ge "$MIN_INCUS_VER"; then
    printf 'distro\n'
  else
    printf 'zabbly\n'
  fi
}

_iserver() (
  local version client_state
  # A pre-consent read must not activate a dormant daemon through its socket.
  command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet incus.service || return 1
  if [ "${1:-}" = --pre-consent ]; then
    # Even a version query initializes the client. Keep its config/cache private
    # and disposable until consent; local: needs no caller configuration.
    client_state="$(mktemp -d /tmp/subyard-incus-plan.XXXXXXXX)" || return
    trap 'rm -rf -- "$client_state"' EXIT
    export INCUS_CONF="$client_state/config" INCUS_CACHE="$client_state/cache"
  fi
  version="$(LC_ALL=C incus version local: 2>/dev/null \
    | awk '/^Server version:/ && !seen {print $3; seen=1}')" || return
  [ -n "$version" ] && dpkg --validate-version "$version" >/dev/null 2>&1 || return 1
  printf '%s\n' "$version"
)

incus_server_below_minimum() { # <known server version or empty>
  [ -n "$1" ] && dpkg --compare-versions "$1" lt "$MIN_INCUS_VER"
}

installed_version='' candidate=''
if command -v incus >/dev/null 2>&1; then
  installed_version="$(_iver)"
  [ -n "$installed_version" ] || installed_version='?'
elif [ "$USE_ZABBLY" = auto ] && command -v apt-cache >/dev/null 2>&1 \
  && command -v dpkg >/dev/null 2>&1; then
  # Read all input: under pipefail, an early awk exit can fail apt-cache with SIGPIPE (exit 141).
  candidate="$(apt-cache policy incus 2>/dev/null | awk '/Candidate:/ && !seen {print $2; seen=1}')"
fi
incus_source="$(plan_incus_source "$installed_version" "$USE_ZABBLY" "$candidate")"
restart_plan=none
restart_notes=()
if [ "$incus_source" = zabbly-upgrade ]; then
  restart_plan=upgrade
  restart_notes+=("upgrade incus from the Zabbly LTS-6.0 repository and restart the Incus daemon (below the supported minimum $MIN_INCUS_VER)")
elif [ "$incus_source" = none ]; then
  server_version="$(_iserver --pre-consent)" || server_version=''
  if incus_server_below_minimum "$server_version"; then
    restart_plan=required
    restart_notes+=("restart the Incus daemon to finish the required upgrade")
  elif [ -z "$server_version" ] && [ "$(id -u)" -ne 0 ] \
    && command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet incus.service; then
    # Root re-entry may be able to inspect a daemon this caller cannot read.
    restart_plan=conditional
    restart_notes+=("restart the Incus daemon if it still runs a version below $MIN_INCUS_VER")
  fi
fi
if [ "$restart_plan" != none ] && command -v systemctl >/dev/null 2>&1 \
  && systemctl is-active --quiet NetworkManager; then
  restart_notes+=("mark Incus devices unmanaged in NetworkManager before the change")
fi

prepare_operator_data_home() {
  subyard_home_validate_root "$SUBYARD_HOME" \
    || die "Subyard data home must be an absolute, non-broad directory"
  SUBYARD_HOME="$SUBYARD_VALIDATED_HOME"

  local operator_uid actual_owner
  operator_uid="$(id -u "$OPERATOR_USER")"
  if [ "$(id -u)" -ne 0 ] && [ ! -e "$SUBYARD_HOME" ]; then
    install -d -m 0700 -- "$SUBYARD_HOME" \
      || die "could not create operator data home: $SUBYARD_HOME"
  fi
  [ -d "$SUBYARD_HOME" ] \
    || die "operator data home must be created by the operator before root installation: $SUBYARD_HOME"
  actual_owner="$(stat -c '%u' -- "$SUBYARD_HOME")"
  [ "$actual_owner" = "$operator_uid" ] \
    || die "operator data home must be owned by the operator before root installation: $SUBYARD_HOME"
}

if [ "$UPGRADE_ONLY" = 1 ]; then
  announce "Subyard — upgrade Incus to >= $MIN_INCUS_VER (Zabbly LTS-6.0)" \
    "Add the Zabbly LTS-6.0 apt repo (keyring + source), if not already present." \
    "apt-get install the newer 'incus' package." \
    "${restart_notes[@]}" \
    "Reversible: delete /etc/apt/sources.list.d/zabbly-incus-lts-6.0.sources and downgrade."
  proceed_or_die
  root_reason="adding an apt repo and upgrading the incus package needs root"
else
  vm_qemu_note=()
  if [ "${YARD_KIND:-container}" = vm ]; then
    vm_qemu_note=("Install qemu-system-x86 and conntrack for VM yards if missing (apt).")
  fi
  announce "Subyard Phase 1 — install & initialize Incus" \
    "Install the 'incus' package if missing (apt)." \
    "${restart_notes[@]}" \
    "${vm_qemu_note[@]}" \
    "Add user '$OPERATOR_USER' to group 'incus-admin' — this grants Incus access ≈ root on this host." \
    "Activate captured-parent access on the current default socket; standalone callers need a fresh group session." \
    "Create the storage pool directory: $STORAGE_PATH" \
    "Run 'incus admin init': dir pool '$STORAGE_POOL' + bridge '$INCUS_BRIDGE' (only if not already initialized)."
  proceed_or_die
  prepare_operator_data_home
  root_reason="the steps above install packages, edit group membership, and initialize Incus"
fi
require_root "$root_reason"
# Package installation can restart incusd too. Protect host networking before
# any package/repository changes with the static section, then list and pin the
# Incus bridges after init (step 5).
nm_unmanaged_guard_early

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
# Many distros package an Incus older than MIN_INCUS_VER, which breaks nested Docker.
# Auto-selection uses the Zabbly LTS-6.0 repository when the installed version or
# distro candidate cannot meet the floor; --zabbly forces it. Every path verifies it.
echo "Dependency: incus"
apply_incus_source() { # <plan>
  local plan="$1" incus_ready install_error server_version restart=0
  if [ "$plan" = none ]; then
    incus_ready="incus present ($(_iver)) >= $MIN_INCUS_VER"
    # Finish only our required upgrade. A supported daemon may intentionally
    # lag the installed package; its restart belongs to the operator.
    if [ "$restart_plan" = required ] || [ "$restart_plan" = conditional ]; then
      server_version="$(_iserver)" || server_version=''
      if incus_server_below_minimum "$server_version"; then restart=1; fi
    fi
  else
    install_error="incus install failed (the distro repos may carry it; Zabbly LTS-6.0 is the >= $MIN_INCUS_VER source)"
    if [ "$plan" = zabbly-upgrade ]; then
      warn "incus $(_iver) < $MIN_INCUS_VER — upgrading from the Zabbly LTS-6.0 repo"
      install_error="incus upgrade failed"
    else
      command -v apt-get >/dev/null 2>&1 \
        || die "no apt-get; install Incus manually (linuxcontainers.org/incus) and re-run"
      if [ "$plan" = zabbly ]; then
        warn "missing dependency: incus — installing from the Zabbly LTS-6.0 repo"
      else
        warn "missing dependency: incus — installing (sudo apt-get install incus)"
      fi
    fi
    if [ "$plan" = distro ]; then
      info "apt-get update"
      apt-get update -qq || die "apt-get update failed"
    else
      add_zabbly_lts_repo || die "could not set up the Zabbly LTS-6.0 repo"
    fi
    if [ "$plan" = zabbly-upgrade ]; then
      info "apt-get install incus (Zabbly)"
    else
      info "installing incus"
    fi
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends incus \
      || die "$install_error"
    if [ "$plan" = zabbly-upgrade ]; then
      restart=1
      incus_ready="incus upgraded ($(_iver))"
    else
      incus_ready="incus installed ($(_iver))"
    fi
  fi
  if [ "$restart" = 1 ]; then
    systemctl try-restart incus.service || die "incus restart failed"
  fi
  _irecent \
    || die "incus $(_iver) < $MIN_INCUS_VER after installation/upgrade — check 'apt-cache policy incus'"
  ok "$incus_ready"
}

apply_incus_source "$incus_source"
if dpkg --compare-versions "$(_iver)" lt "$RECOMMENDED_INCUS_VER"; then
  info "incus $(_iver) is supported; $RECOMMENDED_INCUS_VER is recommended"
fi

# --upgrade-only: ensuring the version is all this run does — group/storage/init are 'yard init's job.
if [ "$UPGRADE_ONLY" = 1 ]; then
  echo
  ok "Incus is $(_iver)."
  exit 0
fi

if [ "${YARD_KIND:-container}" = vm ]; then
  echo "Dependency: QEMU for VM yards"
  if command -v qemu-system-x86_64 >/dev/null 2>&1; then
    ok "qemu-system-x86_64 present"
  else
    command -v apt-get >/dev/null 2>&1 \
      || die "no apt-get; install qemu-system-x86 for the VM and re-run"
    info "installing qemu-system-x86 for the VM"
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends qemu-system-x86 \
      || die "qemu-system-x86 install failed"
    command -v qemu-system-x86_64 >/dev/null 2>&1 \
      || die "qemu-system-x86_64 is unavailable after qemu-system-x86 installation"
    ok "qemu-system-x86_64 installed"
  fi
  echo "Dependency: conntrack for VM public UDP recovery"
  if command -v conntrack >/dev/null 2>&1; then
    ok "conntrack present"
  else
    command -v apt-get >/dev/null 2>&1 \
      || die "no apt-get; install conntrack for the VM and re-run"
    info "installing conntrack for VM public UDP recovery"
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends conntrack \
      || die "conntrack install failed"
    command -v conntrack >/dev/null 2>&1 \
      || die "conntrack is unavailable after installation"
    ok "conntrack installed"
  fi
fi

# --- 2. grant operator access to the Incus socket ---------------------------
echo "Socket access (incus-admin → operator only):"
if ! getent group incus-admin >/dev/null 2>&1; then
  warn "group 'incus-admin' missing; creating it"
  groupadd --system incus-admin
fi
if id -nG "$OPERATOR_USER" 2>/dev/null | tr ' ' '\n' | grep -x >/dev/null incus-admin; then
  ok "$OPERATOR_USER already in incus-admin"
else
  usermod -aG incus-admin "$OPERATOR_USER"
  ok "added $OPERATOR_USER to incus-admin (re-login required to take effect)"
fi

# --- 3. storage dir ----------------------------------------------------------
prepare_operator_data_home
echo "Storage:"
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
operator_uid="$(id -u "$OPERATOR_USER")"
context_preseed="$(python3 -c 'import json,sys; print(json.dumps(sys.argv[1]))' "${SUBYARD_INSTALLATION_CONTEXT:?}")"
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
      "user.subyard.operator_uid=$operator_uid" "user.subyard.data_home=$SUBYARD_HOME" \
      "user.subyard.config_home=$SUBYARD_CONFIG_HOME" "user.subyard.context=${SUBYARD_INSTALLATION_CONTEXT:?}" \
      --project default 2>&1)"; then
      case "$out" in
        *"cannot open log"*)
          warn "dnsmasq cannot reach syslog (nested/AppArmor host) — retrying with a file log"
          incus network create "$INCUS_BRIDGE" ipv4.address=auto ipv6.address=none \
      "user.subyard.operator_uid=$operator_uid" "user.subyard.data_home=$SUBYARD_HOME" \
      "user.subyard.config_home=$SUBYARD_CONFIG_HOME" "user.subyard.context=${SUBYARD_INSTALLATION_CONTEXT:?}" \
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
      user.subyard.operator_uid: "$operator_uid"
      user.subyard.data_home: "$SUBYARD_HOME"
      user.subyard.config_home: "$SUBYARD_CONFIG_HOME"
      user.subyard.context: $context_preseed
networks:
  - name: $INCUS_BRIDGE
    type: bridge
    config:
      user.subyard.operator_uid: "$operator_uid"
      user.subyard.data_home: "$SUBYARD_HOME"
      user.subyard.config_home: "$SUBYARD_CONFIG_HOME"
      user.subyard.context: $context_preseed
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

# Grant only this approved actor on this socket inode. The native helper pins
# root-owned ancestors/socket, preserves modes and unrelated ACLs, and verifies
# readback. No default ACL or service hook makes this grant survive replacement.
[ "$(id -u "$OPERATOR_USER")" = "$operator_uid" ] \
  || die "operator numeric identity changed during installation"
if [ "$operator_uid" != 0 ] && [ -n "${SUBYARD_DISPATCHER_PATH:-}" ]; then
  if [ ! -x /usr/bin/setfacl ] || [ ! -x /usr/bin/getfacl ]; then
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends acl \
      || die "POSIX ACL tools could not be installed"
  fi
  case "${SUBYARD_DISPATCHER_PATH:-}" in
    /*) [ -x "$SUBYARD_DISPATCHER_PATH" ] || die "native operator access helper is unavailable" ;;
    *) die "native operator access helper is unavailable" ;;
  esac
  "$SUBYARD_DISPATCHER_PATH" _incus-operator-access "$operator_uid" \
    || die "native operator socket access could not be established"
fi

# --- summary -----------------------------------------------------------------
echo
ok "Phase 1 done."
cat <<MSG

Next:
  - Captured native parents receive current socket access; verify: incus list
  - Standalone callers must open a fresh incus-admin session
  - Re-login to activate incus-admin in other sessions. Socket recreation removes
    the named grant; removing group membership alone does not revoke that grant.
  - Phase 1 cont.: scripts/02-create-project.sh (project 'subyard' + restricted config)
MSG
