#!/usr/bin/env bash
# Print what a direct controller's shared helpers emit when staging (stage) or cleaning up
# (cleanup [FAILED_VM]) both VMs of a lease, with a synthetic guest transport.
set -euo pipefail
ROOT="$1" mode="$2" failed="${3:-}"
# shellcheck source=dev/agent-e2e.sh
. "$ROOT/dev/agent-e2e.sh"
# shellcheck disable=SC2317
guest() {
  local vm="$1"
  shift
  case "$1" in
    findmnt) printf 'ext4\n' ;;
    mktemp) printf '/var/tmp/subyard-worktree.vm%s\n' "$vm" ;;
    dd) cat >/dev/null ;;
    sha256sum) printf '%s  worktree.tar.gz\n' "$HASH" ;;
    mkdir | tar) ;;
    sudo) [ "$vm" != "$failed" ] || return 3 ;;
    *) return 97 ;;
  esac
}
bundle="$(mktemp)"
HASH="$(sha256sum "$bundle" | cut -d' ' -f1)"
for vm in 1 2; do
  if [ "$mode" = stage ]; then
    prepare_guest "$vm" "$bundle" "$HASH"
  else
    GUEST_DIRS[$vm]="/var/tmp/subyard-worktree.vm$vm"
    cleanup_guest "$vm" || true
  fi
done 2>&1
rm -f "$bundle"
