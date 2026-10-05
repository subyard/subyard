#!/usr/bin/env bash
# Published stable configuration sync over protected SSH in one disposable pair lease.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
# shellcheck source=dev/agent-e2e.sh
. "$ROOT/dev/agent-e2e.sh"

usage() {
  printf 'Usage: dev/e2e/config-sync-acceptance.sh --slot N\n'
}
while [ "$#" -gt 0 ]; do
  case "$1" in
    --slot)
      [ "$#" -ge 2 ] || die '--slot needs a number from 1 to 999'
      [ -z "$LEASE_REQUESTED_SLOT" ] || die '--slot may be specified only once'
      set_requested_slot "$2" --slot
      shift 2
      ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument '$1'" ;;
  esac
done
[ -n "$LEASE_REQUESTED_SLOT" ] || die '--slot N is required'
for command in git rg tar sha256sum ssh ssh-keygen; do
  command -v "$command" >/dev/null || die "$command is required"
done

LOCAL_TEMP="$(mktemp -d "${TMPDIR:-/tmp}/subyard-agent-e2e.XXXXXX")"
LEASE_PURPOSE=published-config-sync
STATE=/var/lib/subyard-config-sync-e2e
REMOTE_ROOT=/srv/subyard-config-sync-e2e
fixture() {
  local vm="$1"; shift
  guest "$vm" env SUBYARD_E2E_VM="$vm" SUBYARD_E2E_RUN_ID="$LEASE_RUN" \
    bash "${GUEST_DIRS[$vm]}/src/dev/e2e/config-sync-two-host.sh" "$@" </dev/null
}
cleanup_acceptance() {
  local rc=$? vm cleanup_failed=0
  trap - EXIT INT TERM
  set +e
  for vm in 1 2; do
    if [ -n "${GUEST_DIRS[$vm]:-}" ]; then
      fixture "$vm" published-cleanup >/dev/null 2>&1 || cleanup_failed=1
    fi
  done
  [ "$cleanup_failed" = 0 ] || rc=3
  # The shared wrapper owns heartbeat, guest-worktree deletion and lease release.
  (exit "$rc")
  cleanup_on_exit
}
trap cleanup_acceptance EXIT INT TERM

acquire_lease
start_lease_keeper
bundle="$LOCAL_TEMP/worktree.tar.gz"
build_bundle "$ROOT" "$bundle"
bundle_hash="$(sha256sum "$bundle" | cut -d' ' -f1)"
printf 'config-sync controller source bundle sha256=%s\n' "$bundle_hash"
for vm in 1 2; do
  prepare_guest "$vm" "$bundle" "$bundle_hash"
  for command in git ssh ssh-keygen curl jq sha256sum systemctl; do
    guest "$vm" sh -c 'command -v "$1" >/dev/null' _ "$command" </dev/null
  done
  guest "$vm" test -x /usr/sbin/sshd </dev/null
  host="host-$([ "$vm" = 1 ] && printf a || printf b)"
  fixture "$vm" published-prepare "$host"
  guest "$vm" cat "$STATE/release-identity" </dev/null >"$LOCAL_TEMP/release-$vm"
  ssh-keygen -q -t ed25519 -N '' -C subyard-config-sync-synthetic -f "$LOCAL_TEMP/$host-key"
  chmod 0600 "$LOCAL_TEMP/$host-key"
  guest "$vm" dd "of=$STATE/$host/client-key" status=none <"$LOCAL_TEMP/$host-key"
  guest "$vm" chmod 0600 "$STATE/$host/client-key" </dev/null
  guest 1 dd "of=$STATE/$host.pub" status=none <"$LOCAL_TEMP/$host-key.pub"
  guest 1 chmod 0600 "$STATE/$host.pub" </dev/null
done
cmp -s "$LOCAL_TEMP/release-1" "$LOCAL_TEMP/release-2" \
  || die 'hosts installed different published runtime artifacts or provenance'
printf 'evidence: published_release_identity=%s\n' "$(cat "$LOCAL_TEMP/release-1")"
fixture 1 published-server
# Pin the public host key through the already authenticated lease transport.
guest 1 cat "$REMOTE_ROOT/server-key.pub" </dev/null >"$LOCAL_TEMP/server-key.pub"
awk '{print "sync-private " $1 " " $2}' "$LOCAL_TEMP/server-key.pub" >"$LOCAL_TEMP/known-hosts"
chmod 0600 "$LOCAL_TEMP/known-hosts"
for vm in 1 2; do
  host="host-$([ "$vm" = 1 ] && printf a || printf b)"
  guest "$vm" dd "of=$STATE/$host/known-hosts" status=none <"$LOCAL_TEMP/known-hosts"
  fixture "$vm" published-auth "$host" "${VM_IP[1]}"
done
fixture 1 published-connect host-a
fixture 2 published-connect host-b
fixture 1 published-change host-a images:debian/12
fixture 2 published-pull host-b images:debian/12
fixture 1 published-verify host-a images:debian/12
fixture 2 published-change host-b images:debian/13
fixture 1 published-pull host-a images:debian/13
fixture 2 published-verify host-b images:debian/13
fixture 1 published-fail-closed host-a
fixture 2 published-verify host-b images:debian/13
for vm in 1 2; do fixture "$vm" published-cleanup; done
ok 'published stable SSH sync, bidirectional live convergence and fail-closed recovery passed'
