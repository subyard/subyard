#!/usr/bin/env bash
# A bounded current-worktree GitHub connection acceptance in one disposable pair.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
# shellcheck source=dev/agent-e2e.sh
. "$ROOT/dev/agent-e2e.sh"
VM_COUNT_REQUESTED=2
LEASE_PURPOSE=github-connection-sync
usage() { printf 'Usage: config/profiles/github/tests/e2e/connection-sync.sh --slot N\n'; }
while [ "$#" -gt 0 ]; do
  case "$1" in
    --slot)
      [ "$#" -ge 2 ] && [ -z "$LEASE_REQUESTED_SLOT" ] || die '--slot requires one explicit slot'
      set_requested_slot "$2" --slot; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done
[ -n "$LEASE_REQUESTED_SLOT" ] || die '--slot N is required'
# These process/fixture and production filesystem checks never acquire a lease.
bash "$ROOT/config/profiles/github/tests/run.sh" --preflight
LOCAL_TEMP="$(mktemp -d /tmp/subyard-agent-e2e.XXXXXX)"
fixture_path=config/profiles/github/tests/e2e/connection-sync-guest.sh
fixture() {
  local vm="$1" started=$SECONDS rc=0 mode="$2" budget=1800; shift
  [ "$mode" != cleanup ] || budget=180
  printf 'E2E_BUDGET phase=fixture/connection-%s budget_seconds=%s grace_seconds=60\n' "$mode" "$budget" >&2
  printf 'E2E_PHASE phase=fixture/connection-%s state=start duration_seconds=0 exit_code=0\n' "$mode" >&2
  guest "$vm" /usr/sbin/runuser -u dev -- env HOME=/home/dev USER=dev LOGNAME=dev \
    SUBYARD_E2E_VM="$vm" SUBYARD_E2E_RUN_ID="$LEASE_RUN" \
    timeout --foreground --signal=TERM --kill-after=60 "$budget" bash "${GUEST_DIRS[$vm]}/src/$fixture_path" "$@" </dev/null || rc=$?
  printf 'E2E_PHASE phase=fixture/connection-%s state=end duration_seconds=%s exit_code=%s\n' "$mode" "$((SECONDS - started))" "$rc" >&2
  return "$rc"
}
cleanup_acceptance() {
  local rc=${1:-$?} vm failed=0 cleanup_rc cleanup_log
  trap - EXIT INT TERM
  set +e
  for vm in 1 2; do
    [ -n "${GUEST_DIRS[$vm]:-}" ] || continue
    mkdir -p "$ROOT/.build/github-connection-sync"
    cleanup_log="$ROOT/.build/github-connection-sync/$LEASE_RUN-cleanup-vm$vm.log"
    fixture "$vm" cleanup >"$cleanup_log" 2>&1
    cleanup_rc=$?
    if [ "$cleanup_rc" != 0 ]; then
      failed=1
      printf 'github-connection-e2e: VM%s cleanup exit=%s; evidence=%s\n' "$vm" "$cleanup_rc" "$cleanup_log" >&2
    fi
  done
  [ "$failed" = 0 ] || printf 'github-connection-e2e: fixture phase exit=%s; cleanup also failed (exit=3)\n' "$rc" >&2
  [ "$failed" = 0 ] || [ "$rc" != 0 ] || rc=3
  cleanup_on_exit '' "$rc"
}
trap cleanup_acceptance EXIT
trap 'cleanup_acceptance 130' INT
trap 'cleanup_acceptance 143' TERM
phase_start allocation
acquire_lease
phase_end 0
start_lease_keeper
bundle="$LOCAL_TEMP/worktree.tar.gz"
build_bundle "$ROOT" "$bundle"
bundle_hash="$(sha256sum "$bundle" | cut -d' ' -f1)"
printf 'GitHub connection source_bundle_sha256=%s base=%s\n' "$bundle_hash" "$BASE_FINGERPRINT"
for vm in 1 2; do
  prepare_guest "$vm" "$bundle" "$bundle_hash"
  guest "$vm" chown -R dev:dev "${GUEST_DIRS[$vm]}" </dev/null
done
fixture 1 package
assets="$LOCAL_TEMP/runtime-assets.tar.gz"
guest 1 tar -C "${GUEST_DIRS[1]}/src/.build/github-connection-release" -czf - . </dev/null >"$assets"
assets_hash="$(sha256sum "$assets" | cut -d' ' -f1)"
guest 2 dd "of=${GUEST_DIRS[2]}/runtime-assets.tar.gz" status=none <"$assets"
[ "$(guest 2 sha256sum "${GUEST_DIRS[2]}/runtime-assets.tar.gz" </dev/null | cut -d' ' -f1)" = "$assets_hash" ] \
  || die 'runtime transport checksum mismatch'
guest 2 mkdir -p "${GUEST_DIRS[2]}/src/.build/github-connection-release" </dev/null
guest 2 tar -xzf "${GUEST_DIRS[2]}/runtime-assets.tar.gz" -C "${GUEST_DIRS[2]}/src/.build/github-connection-release" </dev/null
guest 2 chown -R dev:dev "${GUEST_DIRS[2]}/src/.build" </dev/null
printf 'GitHub connection runtime_assets_sha256=%s\n' "$assets_hash"
state="/tmp/subyard-github-connection-e2e-$LEASE_RUN"
for vm in 1 2; do
  fixture "$vm" prepare
  fixture "$vm" public >"$LOCAL_TEMP/public-$vm"
done
for vm in 1 2; do
  peer=$((3 - vm))
  guest "$vm" dd "of=$state/peer-public" status=none <"$LOCAL_TEMP/public-$peer"
  guest "$vm" chown dev:dev "$state/peer-public" </dev/null
  fixture "$vm" authorize "${VM_IP[$peer]}"
done
fixture 1 migrate
fixture 2 init
fixture 1 sync
transfer_public() {
  guest 1 cat "$state/app-public.pem" </dev/null >"$LOCAL_TEMP/app-public.pem"
  guest 2 dd "of=$state/app-public.pem" status=none <"$LOCAL_TEMP/app-public.pem"
  guest 2 chown dev:dev "$state/app-public.pem" </dev/null
}
transfer_public
fixture 2 verify 123456 42
fixture 1 repeat
fixture 1 sync
fixture 2 repeat
fixture 2 verify 123456 42
fixture 1 update
fixture 1 sync
fixture 2 verify 654321 84
fixture 1 rotate
fixture 1 auto-sync
transfer_public
fixture 2 verify 654321 84
fixture 1 revoke
fixture 1 sync
fixture 2 absent
fixture 1 local-only
fixture 1 sync
fixture 2 absent
for vm in 1 2; do fixture "$vm" cleanup; done
ok 'GitHub connection migration, real SSH/SOPS sync, issuer, repeat init, rotation, automatic sync, revoke and local-only passed'
