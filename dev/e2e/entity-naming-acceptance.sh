#!/usr/bin/env bash
# Acquire one bounded two-VM lease and run the real-OpenSSH HostID lifecycle fixture.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
# shellcheck source=dev/agent-e2e.sh
. "$ROOT/dev/agent-e2e.sh"

LOCAL_TEMP="$(mktemp -d "${TMPDIR:-/tmp}/subyard-agent-e2e.XXXXXX")"
LEASE_PURPOSE=entity-naming-lifecycle
cleanup_acceptance() {
  local rc=$? cleanup_failed=0
  trap - EXIT INT TERM
  set +e
  [ -z "${GUEST_DIRS[1]:-}" ] || cleanup_guest 1 >/dev/null 2>&1 || cleanup_failed=1
  if [ -n "${LEASE_KEEPER_PID:-}" ]; then
    kill "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
    wait "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
  fi
  release_lease >/dev/null 2>&1 || cleanup_failed=1
  case "$LOCAL_TEMP" in /tmp/subyard-agent-e2e.*|"${TMPDIR:-/tmp}"/subyard-agent-e2e.*)
    find "$LOCAL_TEMP" -depth -delete >/dev/null 2>&1 || cleanup_failed=1 ;;
  esac
  [ "$cleanup_failed" = 0 ] || rc=3
  exit "$rc"
}
trap cleanup_acceptance EXIT INT TERM

acquire_lease
start_lease_keeper
bundle="$LOCAL_TEMP/worktree.tar.gz"
build_bundle "$ROOT" "$bundle"
bundle_hash="$(sha256sum "$bundle" | awk '{print $1}')"
prepare_guest 1 "$bundle" "$bundle_hash"
guest_root="${GUEST_DIRS[1]}"
guest 1 chown -R dev:dev "$guest_root"

guest 1 dd "of=$guest_root/peer-key" status=none < "$GUEST_IDENTITY"
guest 1 dd "of=$guest_root/peer-known-hosts" status=none < "$GUEST_KNOWN_HOSTS"
peer_config="$LOCAL_TEMP/peer-config"
cat > "$peer_config" <<EOF
Host peer-admin
    HostName ${VM_IP[2]}
    Port 22
    User root
    IdentityFile $guest_root/peer-key
    IdentitiesOnly yes
    BatchMode yes
    StrictHostKeyChecking yes
    HostKeyAlias e2e-vm-2
    UserKnownHostsFile $guest_root/peer-known-hosts
    GlobalKnownHostsFile /dev/null
EOF
chmod 0600 "$peer_config"
guest 1 dd "of=$guest_root/peer-config" status=none < "$peer_config"
guest 1 chown dev:dev \
  "$guest_root/peer-key" "$guest_root/peer-known-hosts" "$guest_root/peer-config"
guest 1 chmod 0600 \
  "$guest_root/peer-key" "$guest_root/peer-known-hosts" "$guest_root/peer-config"

lifecycle=(bash)
[ -z "${SUBYARD_ENTITY_TRACE:-}" ] || lifecycle+=(-x)
lifecycle+=("$guest_root/src/dev/e2e/entity-naming-lifecycle.sh")
guest 1 /usr/sbin/runuser -u dev -- \
  env HOME=/home/dev USER=dev LOGNAME=dev \
  SUBYARD_E2E_RUN_ID="$LEASE_RUN" SUBYARD_E2E_VM=1 \
  SUBYARD_ENTITY_PEER_CONFIG="$guest_root/peer-config" \
  SUBYARD_ENTITY_PEER_ALIAS=peer-admin SUBYARD_ENTITY_PEER_IP="${VM_IP[2]}" \
  "${lifecycle[@]}"
