#!/usr/bin/env bash
# Image choice is bounded and preserves old requests when omitted.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
# shellcheck source=config/profiles/test-vms-client/skills/subyard-test-vms/client.sh
. "$ROOT/config/profiles/test-vms-client/skills/subyard-test-vms/client.sh"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
LEASE_REQUESTED_SLOT=slot-001 LEASE_YARD=yard LEASE_PROJECT=project LEASE_RUN=run LEASE_PURPOSE=tests
base='acquire-v3 subyard-pair client fingerprint yard project run tests ssh-ed25519 blob slot-001'
[ "$(lease_acquire_request client fingerprint ssh-ed25519 blob)" = "$base" ] || fail 'legacy request changed'
for image in debian-13 ubuntu-24.04; do
  IMAGE_REQUESTED=''
  allocation_option --image "$image"
  [ "$(lease_acquire_request client fingerprint ssh-ed25519 blob)" = "$base 2 - $image" ] || fail 'image not forwarded with default disk'
  VM_COUNT_REQUESTED=1
  resolve_vm_count
  DISK_REQUESTED=50GiB
  [ "$(lease_acquire_request client fingerprint ssh-ed25519 blob)" = "$base 1 50GiB $image" ] || fail 'image changed count or disk'
  IMAGE_REQUESTED='' DISK_REQUESTED='' VM_COUNT_REQUESTED=''
  resolve_vm_count
done
for image in images:ubuntu/24.04/cloud ubuntu-26.04 ''; do
 if (allocation_option --image "$image") >/dev/null 2>&1; then fail 'unallowlisted image accepted'; fi
done
IMAGE_REQUESTED=debian-13
if (allocation_option --image ubuntu-24.04) >/dev/null 2>&1; then fail 'duplicate image accepted'; fi
printf 'ok: image allowlist and compatible lease request forwarding\n'

# Exercise the NM controller's real option parser without acquiring a VM.
fixture="$(mktemp -d /tmp/subyard-nm-image.XXXXXX)"
trap 'rm -rf -- "$fixture"' EXIT
mkdir -p "$fixture/dev"
cat > "$fixture/dev/agent-e2e.sh" <<'STUB'
. "$NM_IMAGE_CLIENT"
build_bundle() { : > "$2"; }
cleanup_on_exit() { :; }
acquire_lease() {
  printf '%s\n' "${IMAGE_REQUESTED:-debian-13}" > "$NM_IMAGE_CAPTURE"
  exit 0
}
STUB
export NM_IMAGE_CLIENT="$ROOT/config/profiles/test-vms-client/skills/subyard-test-vms/client.sh"
export NM_IMAGE_CAPTURE="$fixture/image"
controller="$(sed -n '/^controller_main() {/,/^}/p' "$ROOT/dev/e2e/nm-admin-uplink.sh")"
for image in default debian-13 ubuntu-24.04; do
  (
    eval "$controller"
    ROOT="$fixture"
    export TMPDIR="$fixture"
    if [ "$image" = default ]; then
      controller_main --slot 1 --lane fresh
    else
      controller_main --slot 1 --lane upgrade --image "$image"
    fi
  )
  expected="$image"
  [ "$expected" != default ] || expected=debian-13
  [ "$(cat "$NM_IMAGE_CAPTURE")" = "$expected" ] || fail 'NM guard controller lost image choice'
done
if (
  eval "$controller"
  ROOT="$fixture"
    export TMPDIR="$fixture"
  controller_main --slot 1 --lane fresh --image unsupported
) >/dev/null 2>&1; then fail 'NM guard controller accepted an unsupported image'; fi
printf 'ok: NM guard fresh/upgrade controller forwards image selection\n'
