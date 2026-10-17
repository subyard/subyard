#!/usr/bin/env bash
# 02-create-project.sh — Phase 1: create the restricted Incus project.
# Operator (incus-admin, no sudo). Idempotent.
# Config: config/incus.project.env — INCUS_PROJECT, RESTRICTED_DISK_PATHS (see config/host.env).
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
RESTRICTED_DISK_PATHS="${RESTRICTED_DISK_PATHS:-}"
ROOT_POOL="${ROOT_POOL:-${SRV_POOL:-default}}"
INCUS_NETWORK="${INCUS_NETWORK:-${INCUS_BRIDGE:-incusbr0}}"

# --- preconditions -----------------------------------------------------------
incus_preflight

announce_confirm "Subyard Phase 1 — create restricted Incus project" \
  "Create Incus project '$INCUS_PROJECT' (if absent)." \
  "Apply the restricted policy: nesting allow, host disks/unix-char/proxy allowed (disk sources kept under '$RESTRICTED_DISK_PATHS' by tooling, not Incus policy — needed for idmapped 'shift' mounts)." \
  "Reversible: 'incus project delete $INCUS_PROJECT' removes it."

# --- 1. create project (idempotent) ------------------------------------------
echo "Project:"
if incus project show "$INCUS_PROJECT" >/dev/null 2>&1; then
  ok "project '$INCUS_PROJECT' exists"
else
  env INCUS_PROJECT=default incus project create "$INCUS_PROJECT" </dev/null >/dev/null
  ok "created project '$INCUS_PROJECT'"
fi

# --- 2. apply restricted.* policy --------------------------------------------
# restricted=true keeps sensitive features off; re-enable only what the yard needs.
echo "Restricted policy:"
set_key() {
  incus project set "$INCUS_PROJECT" "$1" "$2"
  ok "$1=$2"
}
set_key restricted true
set_key restricted.containers.nesting allow
set_key restricted.containers.privilege unprivileged
# Device-cgroup BPF interception is required only when this trusted test yard hosts
# a nested Incus VM lab. Keep the project-level permission explicitly blocked for
# every normal yard; allowing it broadens the L0/L1 syscall boundary.
if [ "${NESTED_E2E_VMS:-0}" = 1 ]; then
  set_key restricted.containers.interception allow
else
  set_key restricted.containers.interception block
fi
set_key restricted.devices.disk allow
# No Incus source-path allowlist: a restricted disk path forbids `shift` (idmapped
# mounts), which the host mounts need. Tooling keeps sources under $RESTRICTED_DISK_PATHS.
if incus project get "$INCUS_PROJECT" restricted.devices.disk.paths 2>/dev/null | grep -q .; then
  incus project unset "$INCUS_PROJECT" restricted.devices.disk.paths
  ok "restricted.devices.disk.paths unset (was set; blocks shift)"
else
  ok "restricted.devices.disk.paths empty (any source; shift works)"
fi
set_key restricted.devices.unix-char allow
set_key restricted.devices.proxy allow

# --- 3. seed the project's default profile (root disk + nic) -----------------
# restricted.devices.nic defaults to "managed", so a managed bridge ('$INCUS_NETWORK') is allowed.
echo "Default profile (root + nic):"
prof_device_exists() {
  incus profile device list default --project "$INCUS_PROJECT" 2>/dev/null | grep -qx "$1"
}
if prof_device_exists root; then
  ok "root disk already on default profile"
else
  incus profile device add default root disk pool="$ROOT_POOL" path=/ --project "$INCUS_PROJECT" >/dev/null
  ok "added root disk (pool '$ROOT_POOL')"
fi
if prof_device_exists eth0; then
  ok "eth0 nic already on default profile"
else
  incus profile device add default eth0 nic network="$INCUS_NETWORK" --project "$INCUS_PROJECT" >/dev/null
  ok "added eth0 nic (network '$INCUS_NETWORK')"
fi

# --- summary -----------------------------------------------------------------
echo
ok "Phase 1 step 2 done."
cat <<MSG

Verify:
  incus project list
  incus project show $INCUS_PROJECT   # expect the restricted.* keys above

Next:
  - Phase 2: scripts/03-create-subyard.sh (instance + /srv volume + /dev/kvm + host mounts)
MSG
