#!/usr/bin/env bash
# One disposable pair owns the local/controller and remote-owner preview paths.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
# shellcheck source=dev/agent-e2e.sh
. "$ROOT/dev/agent-e2e.sh"

usage() { printf 'Usage: dev/e2e/preview-acceptance.sh --slot N [--remote-only] [--tailnet] [--canonical] [--kind container|vm]\n'; }
slot_seen=0
remote_only=0
tailnet=0
canonical=0
kind=container
kind_seen=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --slot)
      [ "$#" -ge 2 ] || die '--slot needs a number from 1 to 999'
      [ "$slot_seen" = 0 ] || die '--slot may be specified only once'
      set_requested_slot "$2" --slot
      slot_seen=1
      shift 2 ;;
    --kind)
      [ "$#" -ge 2 ] || die '--kind needs container or vm'
      [ "$kind_seen" = 0 ] || die '--kind may be specified only once'
      case "$2" in container|vm) kind="$2" ;; *) die '--kind needs container or vm' ;; esac
      kind_seen=1
      shift 2 ;;
    --remote-only) remote_only=1; shift ;;
    --tailnet) tailnet=1; shift ;;
    --canonical) canonical=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die 'unknown argument' ;;
  esac
done
[ -n "$LEASE_REQUESTED_SLOT" ] || die '--slot N is required'
LOCAL_TEMP="$(mktemp -d "${TMPDIR:-/tmp}/subyard-agent-e2e.XXXXXX")"
# Used by the sourced lease helpers.
# shellcheck disable=SC2034
LEASE_PURPOSE=preview-lifecycle

payload() {
  local vm="$1" phase="$2"; shift 2
  guest "$vm" /usr/sbin/runuser -u dev -- \
    env HOME=/home/dev USER=dev LOGNAME=dev \
    SUBYARD_E2E_RUN_ID="$LEASE_RUN" SUBYARD_E2E_VM="$vm" \
    SUBYARD_E2E_TYPE=subyard-pair "$@" \
    SUBYARD_PREVIEW_TAILNET="$tailnet" \
    SUBYARD_PREVIEW_KIND="$kind" \
    SUBYARD_PREVIEW_CANONICAL="$canonical" \
    bash -c 'cd "$1"; shift; exec bash "$@"' subyard \
    "${GUEST_DIRS[$vm]}/src" "${GUEST_DIRS[$vm]}/src/dev/e2e/preview-lifecycle.sh" "$phase"
}
cleanup_acceptance() {
  local rc=$? cleanup_failed=0 vm
  trap - EXIT INT TERM
  set +e
  if [ -n "${GUEST_DIRS[2]:-}" ]; then
    payload 2 owner-cleanup >/dev/null 2>&1 || cleanup_failed=1
  fi
  for vm in "${!GUEST_DIRS[@]}"; do
    cleanup_guest "$vm" >/dev/null 2>&1 || cleanup_failed=1
  done
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
for vm in 1 2; do
  prepare_guest "$vm" "$bundle" "$bundle_hash"
  guest "$vm" chown -R dev:dev "${GUEST_DIRS[$vm]}"
done
guest_root="${GUEST_DIRS[1]}"
guest 1 dd "of=$guest_root/peer-key" status=none < "$GUEST_IDENTITY"
guest 1 dd "of=$guest_root/peer-known-hosts" status=none < "$GUEST_KNOWN_HOSTS"
cat > "$LOCAL_TEMP/peer-config" <<EOF
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
    ConnectTimeout 5
    LogLevel ERROR
EOF
chmod 0600 "$LOCAL_TEMP/peer-config"
guest 1 dd "of=$guest_root/peer-config" status=none < "$LOCAL_TEMP/peer-config"
guest 1 chown dev:dev "$guest_root/peer-key" "$guest_root/peer-known-hosts" "$guest_root/peer-config"
guest 1 chmod 0600 "$guest_root/peer-key" "$guest_root/peer-known-hosts" "$guest_root/peer-config"

printf 'preview acceptance: source SHA-256 %s\n' "$bundle_hash"
payload 2 owner-setup SUBYARD_PREVIEW_PEER_IP="${VM_IP[1]}"
payload 1 controller \
  SUBYARD_PREVIEW_REMOTE_ONLY="$remote_only" \
  SUBYARD_PREVIEW_PEER_CONFIG="$guest_root/peer-config" \
  SUBYARD_PREVIEW_PEER_IP="${VM_IP[2]}"
if [ "$tailnet" = 1 ]; then
  printf 'ok: synthetic owner Tailnet route, fallback and peer HTTP verified\n'
fi
if [ "$remote_only" = 1 ]; then
  printf 'ok: remote ProxyJump preview lifecycle verified\n'
else
  printf 'ok: local, named and remote ProxyJump preview lifecycle verified\n'
fi
